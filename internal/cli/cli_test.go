package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/advisory"
	"github.com/imantaba/kubeagent/internal/audit"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/gate"
	"github.com/imantaba/kubeagent/internal/hpahealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/jsonschema"
	"github.com/imantaba/kubeagent/internal/pdbhealth"
	"github.com/imantaba/kubeagent/internal/quotahealth"
	"github.com/imantaba/kubeagent/internal/rbacprofile"
	"github.com/imantaba/kubeagent/internal/remediate"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/rolloutwait"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/schemadoc"
	"github.com/imantaba/kubeagent/internal/termhealth"
	"github.com/imantaba/kubeagent/internal/watch"
	"github.com/imantaba/kubeagent/internal/webhookhealth"
)

func TestResultInput_CarriesStuckTerminating(t *testing.T) {
	// Regression: the scan.Result → report.Input mapping must carry every
	// Result-derived field, StuckTerminating included. This once silently dropped
	// in the inline literal, so the merged feature never rendered in the CLI.
	res := scan.Result{StuckTerminating: []termhealth.Issue{
		{Kind: "Pod", Namespace: "shop", Name: "web", Age: "8m", PastGrace: true, Reason: "finalizer x/y"},
	}}
	in := resultInput(res)
	if len(in.StuckTerminating) != 1 || in.StuckTerminating[0].Name != "web" {
		t.Fatalf("resultInput must carry StuckTerminating into report.Input, got %+v", in.StuckTerminating)
	}
}

func TestResultInput_CarriesPDBIssues(t *testing.T) {
	// Regression: the scan.Result → report.Input mapping must carry PDBIssues, or
	// the section never renders in the CLI (the stuck-terminating v0.34.0 bug).
	res := scan.Result{PDBIssues: []pdbhealth.Issue{
		{Namespace: "shop", Name: "api", Rule: "minAvailable: 3", Category: "unsatisfiable", Reason: "…"},
	}}
	in := resultInput(res)
	if len(in.PDBIssues) != 1 || in.PDBIssues[0].Name != "api" {
		t.Fatalf("resultInput must carry PDBIssues into report.Input, got %+v", in.PDBIssues)
	}
}

func TestResultInput_CarriesHPAIssues(t *testing.T) {
	// Regression: the scan.Result → report.Input mapping must carry HPAIssues, or
	// the section never renders in the CLI (the stuck-terminating v0.34.0 bug).
	res := scan.Result{HPAIssues: []hpahealth.Issue{
		{Namespace: "shop", Name: "api-hpa", Target: "Deployment/api", Category: "metrics", Reason: "…"},
	}}
	in := resultInput(res)
	if len(in.HPAIssues) != 1 || in.HPAIssues[0].Name != "api-hpa" {
		t.Fatalf("resultInput must carry HPAIssues into report.Input, got %+v", in.HPAIssues)
	}
}

func TestResultInput_CarriesWebhookIssues(t *testing.T) {
	// Regression: the scan.Result → report.Input mapping must carry WebhookIssues,
	// or the section never renders in the CLI (the stuck-terminating v0.34.0 bug).
	res := scan.Result{WebhookIssues: []webhookhealth.Issue{
		{Kind: "ValidatingWebhookConfiguration", Config: "policy-webhook", Webhook: "validate.policy.io", Problem: "no-endpoints", Reason: "…"},
	}}
	in := resultInput(res)
	if len(in.WebhookIssues) != 1 || in.WebhookIssues[0].Config != "policy-webhook" {
		t.Fatalf("resultInput must carry WebhookIssues into report.Input, got %+v", in.WebhookIssues)
	}
}

func TestResultInput_MapsQuotaIssues(t *testing.T) {
	res := scan.Result{QuotaIssues: []quotahealth.Issue{
		{Namespace: "shop", Quota: "compute", Resource: "pods", Used: "47", Hard: "50", Ratio: 0.94, Severity: "near"},
	}}
	in := resultInput(res)
	if len(in.QuotaIssues) != 1 || in.QuotaIssues[0].Resource != "pods" {
		t.Errorf("resultInput dropped QuotaIssues: got %+v", in.QuotaIssues)
	}
}

func TestResultInputCarriesBlindSpots(t *testing.T) {
	res := scan.Result{PartialReads: []scan.ReadFailure{{Resource: "nodes/proxy", Reason: "forbidden: get nodes/proxy"}}}
	in := resultInput(res)
	if len(in.Blind) != 1 || in.Blind[0].Resource != "nodes/proxy" {
		t.Errorf("resultInput dropped PartialReads: got %+v", in.Blind)
	}
}

func TestEnvInt_WebhookTimeoutDefault(t *testing.T) {
	t.Setenv("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", "")
	if got := envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15); got != 15 {
		t.Errorf("unset env should default to 15, got %d", got)
	}
	t.Setenv("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", "25")
	if got := envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15); got != 25 {
		t.Errorf("env override should be 25, got %d", got)
	}
}

func TestRun_NoArgsReturnsUsage(t *testing.T) {
	if err := Run(nil); err == nil {
		t.Fatal("expected a usage error with no args")
	}
}

func TestRun_RejectsUnknownSubcommand(t *testing.T) {
	if err := Run([]string{"explode"}); err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
}

func TestRun_RejectsBadOutputFormat(t *testing.T) {
	// This must fail on validation BEFORE any cluster connection is attempted.
	if err := Run([]string{"scan", "--output", "bogus"}); err == nil {
		t.Fatal("expected an error for a bad --output value")
	}
}

func TestRun_ExplainRequiresAPIKey(t *testing.T) {
	// --explain without a key (and without a local endpoint) must fail fast, before any cluster connection.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := Run([]string{"scan", "--explain"})
	if err == nil {
		t.Fatal("expected an error when --explain is set without ANTHROPIC_API_KEY")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected error to mention ANTHROPIC_API_KEY, got: %v", err)
	}
}

func TestRun_ExplainNeedsKeyOrEndpoint(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := Run([]string{"scan", "--explain"})
	if err == nil || !strings.Contains(err.Error(), "KUBEAGENT_EXPLAIN_ENDPOINT") {
		t.Fatalf("want the key-or-endpoint error, got %v", err)
	}
}

func TestRun_ExplainLocalNeedsModel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "http://localhost:11434/v1")
	t.Setenv("KUBEAGENT_MODEL", "")
	err := Run([]string{"scan", "--explain"})
	if err == nil || !strings.Contains(err.Error(), "needs --model") {
		t.Fatalf("want the needs-model error, got %v", err)
	}
}

func TestRun_ModelFlagIsRecognized(t *testing.T) {
	// --model must be a known flag: with it set and no API key, the error is
	// the fail-fast key error, NOT "flag provided but not defined".
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := Run([]string{"scan", "--explain", "--model", "claude-sonnet-4-6"})
	if err == nil {
		t.Fatal("expected the fail-fast API-key error")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected ANTHROPIC_API_KEY error (proves --model parsed), got: %v", err)
	}
}

func TestRun_IncludeFlagsAreRecognized(t *testing.T) {
	// --include-cron / --include-restarts must be known flags: with --explain and
	// no key, the error is the fail-fast key error, not "flag not defined".
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := Run([]string{"scan", "--explain", "--include-cron", "--include-restarts"})
	if err == nil {
		t.Fatal("expected the fail-fast API-key error")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected ANTHROPIC_API_KEY error (proves the flags parsed), got: %v", err)
	}
}

func TestVersionLine(t *testing.T) {
	// In tests the binary isn't ldflags-stamped, so version is the "dev" default.
	if got := versionLine(); got != "kubeagent dev" {
		t.Errorf("versionLine() = %q, want %q", got, "kubeagent dev")
	}
}

func TestRun_Version(t *testing.T) {
	if err := Run([]string{"version"}); err != nil {
		t.Errorf("run([version]) returned error: %v", err)
	}
}

func TestRun_LintSecretsFlagAccepted(t *testing.T) {
	// --lint-secrets must be a defined flag: this fails on output-format
	// validation (which happens before any cluster connection), proving the flag
	// parsed rather than erroring with "flag provided but not defined".
	err := Run([]string{"scan", "--lint-secrets", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("expected the output-format error (flag accepted), got: %v", err)
	}
}

func TestRun_DiagnosesUnreachableAPI(t *testing.T) {
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	// A kubeconfig pointing at a port nothing listens on → loopback connection
	// refused (no external network). Exercises the connectivity diagnosis path.
	cfg := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:1
  name: dead
contexts:
- context:
    cluster: dead
    user: dead
  name: dead
current-context: dead
users:
- name: dead
  user: {}
`
	if err := os.WriteFile(kc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Run([]string{"scan", "--kubeconfig", kc})
	if err == nil {
		t.Fatal("expected an error for an unreachable API server")
	}
	out := err.Error()
	if !strings.Contains(out, "refused") || !strings.Contains(out, "details:") {
		t.Errorf("expected a connection-refused diagnosis with a details line, got: %v", out)
	}
}

func TestRun_NoLintSecrets_NoCredentialSection(t *testing.T) {
	// Without --lint-secrets, kubeagent must never surface a credential section.
	// run() builds its own client from kubeconfig, so the only hermetic full path
	// is the unreachable-API path (loopback refused); assert its output carries no
	// credential wording. Combined with the report-layer "no section when empty"
	// test, this guards the off-by-default guarantee without needing a live cluster.
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	cfg := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:1
  name: dead
contexts:
- context:
    cluster: dead
    user: dead
  name: dead
current-context: dead
users:
- name: dead
  user: {}
`
	if err := os.WriteFile(kc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Run([]string{"scan", "--kubeconfig", kc})
	if err == nil {
		t.Fatal("expected an error for an unreachable API server")
	}
	if strings.Contains(strings.ToLower(err.Error()), "credential") {
		t.Errorf("no credential output expected without --lint-secrets, got: %v", err)
	}
}

func TestRun_FixFlagsAccepted(t *testing.T) {
	// --fix/--dry-run/--yes must be defined flags: this fails on output-format
	// validation (before any cluster call), proving they parsed.
	err := Run([]string{"scan", "--fix", "--dry-run", "--yes", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("expected output-format error (flags accepted), got: %v", err)
	}
}

func TestRun_SuggestFlagAccepted(t *testing.T) {
	// --suggest must be a defined flag: this fails on output-format validation
	// (before any cluster call), proving the flag parsed.
	err := Run([]string{"scan", "--suggest", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("expected the output-format error (flag accepted), got: %v", err)
	}
}

func TestRun_ControlPlaneHealthFlagAccepted(t *testing.T) {
	err := Run([]string{"scan", "--control-plane-health", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flag parsed), got %v", err)
	}
}

func TestRun_DNSHealthFlagAccepted(t *testing.T) {
	err := Run([]string{"scan", "--dns-health", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flag parsed), got %v", err)
	}
}

func TestRun_OperatorsFlagAccepted(t *testing.T) {
	// --operators must be a defined flag: this fails on output-format validation
	// (before any cluster or discovery call), proving the flag parsed rather
	// than erroring with "flag provided but not defined".
	err := Run([]string{"scan", "--operators", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flag parsed), got %v", err)
	}
}

func TestRun_DriftFlagAccepted(t *testing.T) {
	// --drift and --drift-age must be defined flags: this fails on output-format
	// validation (before any cluster or discovery call), proving the flags parsed
	// rather than erroring with "flag provided but not defined".
	err := Run([]string{"scan", "--drift", "--drift-age", "5m", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flags parsed), got %v", err)
	}
}

func TestRun_CapacityFlagAccepted(t *testing.T) {
	// --capacity must be a defined flag: this fails on output-format validation
	// (before any cluster call), proving the flag parsed rather than erroring with
	// "flag provided but not defined".
	err := Run([]string{"scan", "--capacity", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flag parsed), got %v", err)
	}
}

func TestRun_UsageMentionsCapacityFlag(t *testing.T) {
	err := Run(nil)
	if err == nil {
		t.Fatal("expected a usage error with no args")
	}
	if !strings.Contains(err.Error(), "[--drift-age dur] [--capacity] [--policy path (repeatable)] [--logs]") {
		t.Fatalf("expected the usage string to mention --capacity between --drift-age and --logs, got: %v", err)
	}
}

// captureStderr redirects os.Stderr for the duration of f and returns what was
// written to it. runWatch prints its "alert flags ignored" warning directly to
// os.Stderr rather than through an injectable writer, so tests that need to
// observe it must swap the package-level handle.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatalf("closing pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	return string(out)
}

// captureStdout redirects os.Stdout for the duration of f and returns what was
// written to it. The MCP protocol owns stdout on the mcp subcommand's stdio
// transport, so tests that need to prove a code path never writes there must
// swap the package-level handle the same way captureStderr does for stderr.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	f()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatalf("closing pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	return string(out)
}

func TestRunWatch_AlertFlagsAreRecognized(t *testing.T) {
	// --alert-format/--alert-repeat must be defined flags: with a kubeconfig path
	// that fails to load, the error must be the kubeconfig-load error, not
	// pflag's "unknown flag: --x", proving the flags parsed. The old comparison
	// against "flag provided but not defined" was the standard library's wording,
	// not pflag's, so it could never fail here even if one of these flags were
	// dropped from bindWatchFlags — the real signal is the positive assertion
	// below, that this is specifically the kubeconfig-load error.
	dir := t.TempDir()
	bad := filepath.Join(dir, "nonexistent")
	err := runWatch([]string{"--alert-format", "slack", "--alert-repeat", "10m", "--kubeconfig", bad})
	if err == nil {
		t.Fatal("expected a kubeconfig load error")
	}
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("expected --alert-format/--alert-repeat to be recognized flags, got: %v", err)
	}
	if !strings.Contains(err.Error(), "loading kubeconfig") {
		t.Fatalf("expected the kubeconfig-load error (proving --alert-format/--alert-repeat parsed rather than being rejected), got: %v", err)
	}
}

func TestRunWatch_NoAlertWebhookFlag(t *testing.T) {
	// The webhook URL is a credential (a Slack incoming-webhook URL is a bearer
	// token in URL form) and must never be settable via a flag — that would put it
	// in the pod spec's args and in `ps` output. It must only ever come from the
	// KUBEAGENT_ALERT_WEBHOOK environment variable.
	dir := t.TempDir()
	bad := filepath.Join(dir, "nonexistent")
	err := runWatch([]string{"--alert-webhook", "http://example.invalid/hook", "--kubeconfig", bad})
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("expected no --alert-webhook flag to exist, got: %v", err)
	}
}

func TestRunWatch_WarnsWhenAlertFormatSetWithoutWebhook(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	dir := t.TempDir()
	bad := filepath.Join(dir, "nonexistent")
	stderr := captureStderr(t, func() {
		_ = runWatch([]string{"--alert-format", "slack", "--kubeconfig", bad})
	})
	if !strings.Contains(stderr, "neither KUBEAGENT_ALERT_WEBHOOK nor KUBEAGENT_ALERT_ROUTING_KEY") {
		t.Fatalf("expected the ignored-alert-flags warning on stderr, got: %q", stderr)
	}
}

func TestRunWatch_WarnsWhenAlertRepeatSetWithoutWebhook(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	t.Setenv("KUBEAGENT_ALERT_FORMAT", "")
	dir := t.TempDir()
	bad := filepath.Join(dir, "nonexistent")
	stderr := captureStderr(t, func() {
		_ = runWatch([]string{"--alert-repeat", "5m", "--kubeconfig", bad})
	})
	if !strings.Contains(stderr, "neither KUBEAGENT_ALERT_WEBHOOK nor KUBEAGENT_ALERT_ROUTING_KEY") {
		t.Fatalf("expected the ignored-alert-flags warning on stderr, got: %q", stderr)
	}
}

func TestRunWatch_AlertWarningNamesTheKubectlPluginInvocation(t *testing.T) {
	// The one warning kubeagent writes before it has a cluster is still a
	// warning: a kubectl-plugin user must not be told to look for a "kubeagent"
	// that is not on their PATH.
	saved := invokedAs
	invokedAs = "kubectl kubeagent"
	defer func() { invokedAs = saved }()

	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	dir := t.TempDir()
	bad := filepath.Join(dir, "nonexistent")
	stderr := captureStderr(t, func() {
		_ = runWatch([]string{"--alert-format", "slack", "--kubeconfig", bad})
	})
	if !strings.Contains(stderr, "kubectl kubeagent: warning: --alert-* flags ignored") {
		t.Fatalf("expected the warning to name the kubectl-plugin invocation, got: %q", stderr)
	}
}

func TestRunWatch_NoWarningWithDefaultAlertFlags(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	t.Setenv("KUBEAGENT_ALERT_FORMAT", "")
	t.Setenv("KUBEAGENT_ALERT_REPEAT", "")
	dir := t.TempDir()
	bad := filepath.Join(dir, "nonexistent")
	stderr := captureStderr(t, func() {
		_ = runWatch([]string{"--kubeconfig", bad})
	})
	if strings.Contains(stderr, "flags ignored") {
		t.Fatalf("unexpected alert-flags warning with default values: %q", stderr)
	}
}

// The routing key is a credential and inherits the webhook URL's rule: no flag,
// because a flag would put it in the pod spec's args and in `ps` output.
func TestRunWatch_NoRoutingKeyFlagExists(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "nonexistent-kubeconfig")
	err := runWatch([]string{"--alert-routing-key", "not-a-real-routing-key", "--kubeconfig", bad})
	if err == nil {
		t.Fatal("expected no --alert-routing-key flag to exist")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("expected an unknown-flag error, got: %v", err)
	}
}

// A routing key under a format that does not use one is a configuration
// mistake, and a silent ignore is how it stays one.
func TestRunWatch_WarnsWhenTheRoutingKeyIsSetUnderAnotherFormat(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	t.Setenv("KUBEAGENT_ALERT_ROUTING_KEY", "not-a-real-routing-key")
	bad := filepath.Join(t.TempDir(), "nonexistent-kubeconfig")
	stderr := captureStderr(t, func() {
		_ = runWatch([]string{"--alert-format", "json", "--kubeconfig", bad})
	})
	if !strings.Contains(stderr, "KUBEAGENT_ALERT_ROUTING_KEY is set but --alert-format is json") {
		t.Fatalf("expected the wrong-format warning on stderr, got: %q", stderr)
	}
	if strings.Contains(stderr, "not-a-real-routing-key") {
		t.Fatalf("the warning echoes the routing key: %q", stderr)
	}
}

// A routing key with --alert-format pagerduty turns alerting on with no webhook
// URL set, so the "alerting is off" warning must not fire.
func TestRunWatch_RoutingKeyAloneEnablesPagerDuty(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	t.Setenv("KUBEAGENT_ALERT_ROUTING_KEY", "not-a-real-routing-key")
	bad := filepath.Join(t.TempDir(), "nonexistent-kubeconfig")
	stderr := captureStderr(t, func() {
		_ = runWatch([]string{"--alert-format", "pagerduty", "--kubeconfig", bad})
	})
	if strings.Contains(stderr, "alert-* flags ignored") {
		t.Fatalf("unexpected alerting-is-off warning with a routing key set: %q", stderr)
	}
	if strings.Contains(stderr, "not-a-real-routing-key") {
		t.Fatalf("stderr carries the routing key: %q", stderr)
	}
}

// With neither credential set, the existing warning still fires and now names
// both.
func TestRunWatch_WarnsWhenNeitherCredentialIsSet(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	t.Setenv("KUBEAGENT_ALERT_ROUTING_KEY", "")
	bad := filepath.Join(t.TempDir(), "nonexistent-kubeconfig")
	stderr := captureStderr(t, func() {
		_ = runWatch([]string{"--alert-format", "pagerduty", "--kubeconfig", bad})
	})
	if !strings.Contains(stderr, "--alert-* flags ignored") {
		t.Fatalf("expected the ignored-alert-flags warning on stderr, got: %q", stderr)
	}
	if !strings.Contains(stderr, "KUBEAGENT_ALERT_ROUTING_KEY") {
		t.Fatalf("the warning does not name the pagerduty credential: %q", stderr)
	}
}

// The env var must reach watch.Config, not merely fail to warn.
func TestRunWatch_RoutingKeyReachesTheConfig(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	t.Setenv("KUBEAGENT_ALERT_ROUTING_KEY", "not-a-real-routing-key")
	kc := deadKubeconfigPath(t)
	cfg, err := captureWatchConfig(t, []string{"--alert-format", "pagerduty", "--kubeconfig", kc})
	if err != nil {
		t.Fatalf("captureWatchConfig: %v", err)
	}
	if cfg.AlertRoutingKey != "not-a-real-routing-key" {
		t.Errorf("AlertRoutingKey = %q, want the value from the environment", cfg.AlertRoutingKey)
	}
	if cfg.AlertURL != "" {
		t.Errorf("AlertURL = %q, want empty — pagerduty defaults its endpoint in the sink", cfg.AlertURL)
	}
	// 4h is DefaultRepeat's answer for every format but alertmanager.
	if cfg.AlertRepeat != 4*time.Hour {
		t.Errorf("AlertRepeat = %s, want 4h0m0s", cfg.AlertRepeat)
	}
}

func TestRun_UsageMentionsWatchAlertFlags(t *testing.T) {
	err := Run(nil)
	if err == nil {
		t.Fatal("expected a usage error with no args")
	}
	if !strings.Contains(err.Error(), "[--alert-format json|slack|alertmanager|pagerduty] [--alert-repeat dur]") {
		t.Fatalf("expected the usage string to mention --alert-format and --alert-repeat, got: %v", err)
	}
}

func TestRun_UsageMentionsOperatorsFlag(t *testing.T) {
	err := Run(nil)
	if err == nil {
		t.Fatal("expected a usage error with no args")
	}
	if !strings.Contains(err.Error(), "[--certs [--cert-warn-days n]] [--operators] [--drift] [--drift-age dur]") {
		t.Fatalf("expected the usage string to mention --operators between --certs and --drift-age, got: %v", err)
	}
}

func TestRun_UsageMentionsDriftFlag(t *testing.T) {
	err := Run(nil)
	if err == nil {
		t.Fatal("expected a usage error with no args")
	}
	if !strings.Contains(err.Error(), "[--operators] [--drift] [--drift-age dur] [--capacity]") {
		t.Fatalf("expected the usage string to mention --drift/--drift-age between --operators and --capacity, got: %v", err)
	}
}

// deadKubeconfigPath writes a syntactically valid kubeconfig pointing at a
// server nothing listens on (https://127.0.0.1:1) and returns its path.
// cluster.NewInClusterOrKubeconfig only builds a clientset from this — no
// network I/O happens — so runWatch proceeds past it into watch.Run without
// ever opening a socket, which is what lets these tests reach
// validateSLOTarget deterministically instead of failing at cluster-connect.
func deadKubeconfigPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	cfg := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:1
  name: dead
contexts:
- context:
    cluster: dead
    user: dead
  name: dead
current-context: dead
users:
- name: dead
  user: {}
`
	if err := os.WriteFile(kc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return kc
}

// runWatchBounded calls runWatch in a goroutine and waits up to timeout for it
// to return, failing the test with a diagnostic instead of blocking forever if
// it doesn't. Tests built on the dead kubeconfig expect runWatch to fail fast,
// inside validateSLOTarget, before watch.Run does anything else. But 0 is
// validateSLOTarget's explicit "off" value, so any regression that makes an
// SLO target silently collapse to 0 — say, --slo-target's default quietly
// losing its envFloat wiring — makes validation pass and lets Run fall through
// into the real daemon: binding the metrics port and blocking on
// WaitForCacheSync against a server nothing answers, forever, since nothing in
// these tests can cancel runWatch's signal.NotifyContext. Bounding the wait
// turns that failure mode into a fast, clear test failure instead of a
// multi-minute hang that also leaves a port bound.
//
// Contract: callers MUST pass an ephemeral --metrics-addr (127.0.0.1:0). On
// timeout, the abandoned goroutine keeps running for the lifetime of the test
// binary — including, if it got past validation, holding whatever address it
// was given open for the rest of the run. Passing the real default (:8080)
// would leak a bound listener that a later test binding the same address would
// then fail to acquire, surfacing as an unrelated "address already in use"
// error layered on top of that test's actual failure.
func runWatchBounded(t *testing.T, args []string, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- runWatch(args) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatalf("runWatch(%v) did not return within %s: it likely fell through into the real daemon instead of failing validation fast", args, timeout)
		return nil // unreachable
	}
}

func TestRunWatch_SLOTargetIsAPercentageNotARatio(t *testing.T) {
	// The load-bearing test. --slo-target takes a percentage (99.9), but the
	// tracker works in ratios (0.999), so runWatch must divide by 100 before
	// handing the value to watch.Run. validateSLOTarget's error message
	// multiplies the ratio back by 100 to report a percentage, so this is the
	// only test that can catch a missing/wrong conversion: without the /100,
	// a target of 100 would reach validation as the ratio 100, and the
	// message would read "10000%", not "100%". Do NOT weaken this assertion
	// to a substring like strings.Contains(err.Error(), "invalid --slo-target")
	// — that would pass whether or not the conversion happened.
	kc := deadKubeconfigPath(t)
	err := runWatchBounded(t, []string{"--slo-target", "100", "--kubeconfig", kc, "--metrics-addr", "127.0.0.1:0"}, 3*time.Second)
	if err == nil {
		t.Fatal("expected a validation error for --slo-target 100")
	}
	want := "invalid --slo-target: 100% (must be greater than 0 and less than 100)"
	if err.Error() != want {
		t.Fatalf("error = %q, want exactly %q", err.Error(), want)
	}
}

func TestRunWatch_SLOTargetIsRecognized(t *testing.T) {
	// A valid --slo-target must parse and must not fail validation. This
	// deliberately does NOT use the dead kubeconfig: with a valid target,
	// watch.Run proceeds past validateSLOTarget, binds the metrics address,
	// and blocks on informers until its context is cancelled — and runWatch
	// builds that context from signal.NotifyContext, which no test can
	// cancel. So, as TestRunWatch_AlertFlagsAreRecognized does, this uses a
	// nonexistent kubeconfig path to fail at cluster-connect, before Run is
	// ever reached, and only asserts that the flag parsed and no validation
	// error occurred. As in TestRunWatch_AlertFlagsAreRecognized, the
	// "flag provided but not defined" comparison below is the standard
	// library's wording, not pflag's "unknown flag: --x" — it could never fail
	// here even if --slo-target were dropped, so the positive assertion that
	// this is specifically the kubeconfig-load error is what actually proves
	// the flag parsed.
	dir := t.TempDir()
	bad := filepath.Join(dir, "nonexistent")
	err := runWatch([]string{"--slo-target", "99.9", "--kubeconfig", bad})
	if err == nil {
		t.Fatal("expected a kubeconfig load error")
	}
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("expected --slo-target to be a recognized flag, got: %v", err)
	}
	if strings.Contains(err.Error(), "invalid --slo-target") {
		t.Fatalf("expected no validation error for --slo-target 99.9, got: %v", err)
	}
	if !strings.Contains(err.Error(), "loading kubeconfig") {
		t.Fatalf("expected the kubeconfig-load error (proving --slo-target parsed rather than being rejected), got: %v", err)
	}
}

func TestRunWatch_SLOTargetFromEnv(t *testing.T) {
	// Proves KUBEAGENT_SLO_TARGET feeds the same flag default via envFloat.
	// Same exact-message assertion as TestRunWatch_SLOTargetIsAPercentageNotARatio,
	// this time with no flag given at all.
	t.Setenv("KUBEAGENT_SLO_TARGET", "100")
	kc := deadKubeconfigPath(t)
	err := runWatchBounded(t, []string{"--kubeconfig", kc, "--metrics-addr", "127.0.0.1:0"}, 3*time.Second)
	if err == nil {
		t.Fatal("expected a validation error for KUBEAGENT_SLO_TARGET=100")
	}
	want := "invalid --slo-target: 100% (must be greater than 0 and less than 100)"
	if err.Error() != want {
		t.Fatalf("error = %q, want exactly %q", err.Error(), want)
	}
}

func TestRunWatch_SLOTargetDefaultsToOff(t *testing.T) {
	// Guards the commit's promise that upgrading without --slo-target changes
	// nothing: the flag's default must be the literal zero value, 0, which
	// validateSLOTarget treats as "disabled". A default of, say, 50 would still
	// be a valid target — it would pass validation and silently turn SLO
	// tracking on for every operator who upgrades without touching the flag.
	//
	// Under the standard library this scraped --help's stderr output, because
	// that was the only way to observe a registered flag.Float64 default
	// without starting the daemon. Under Cobra/pflag, a throwaway command
	// built by parseWatchFlags never has --help registered (Cobra only wires
	// it up inside Execute, which parseWatchFlags never calls), so --help
	// falls through to pflag's own fallback instead of exercising the
	// registered default. Reading the default straight off the registered
	// flag is direct and does not depend on either flag package's --help
	// formatting.
	t.Setenv("KUBEAGENT_SLO_TARGET", "")
	var o watchOptions
	cmd := &cobra.Command{Use: "watch", SilenceErrors: true, SilenceUsage: true}
	bindWatchFlags(cmd, &o)
	f := cmd.Flags().Lookup("slo-target")
	if f == nil {
		t.Fatal("no --slo-target flag registered")
	}
	if f.DefValue != "0" {
		t.Fatalf("--slo-target default = %q, want %q (SLO tracking off)", f.DefValue, "0")
	}
}

func TestRun_UsageMentionsSLOTarget(t *testing.T) {
	err := Run(nil)
	if err == nil {
		t.Fatal("expected a usage error with no args")
	}
	if !strings.Contains(err.Error(), "[--alert-repeat dur] [--slo-target pct]") {
		t.Fatalf("expected the usage string to mention --alert-repeat immediately followed by --slo-target, got: %v", err)
	}
}

// captureWatchConfig swaps watchRun for a stub that records the watch.Config
// runWatch built and returns nil immediately, instead of starting the daemon.
// It restores the real watch.Run with defer, so a failing test can't leak the
// stub into another one — no test in this file runs in parallel, and none may
// be added around this helper, since the seam is a single package-level
// variable shared by the whole test binary.
//
// runWatch only reaches watchRun after cluster.NewInClusterOrKubeconfig
// succeeds, so callers must pass args whose --kubeconfig builds a clientset
// without erroring; deadKubeconfigPath(t) does that without any network I/O.
func captureWatchConfig(t *testing.T, args []string) (watch.Config, error) {
	t.Helper()
	var captured watch.Config
	orig := watchRun
	watchRun = func(_ context.Context, _ []watch.Target, cfg watch.Config) error {
		captured = cfg
		return nil
	}
	defer func() { watchRun = orig }()
	err := runWatch(args)
	return captured, err
}

func TestRunWatch_DefaultSLOTargetReachesConfigAsZero(t *testing.T) {
	// This is the test that must catch the reviewer's post-parse-override
	// mutation: inserting "if *sloTarget == 0 { *sloTarget = 50 }" right after
	// fs.Parse leaves the registered flag default and the --help text at 0 —
	// so TestRunWatch_SLOTargetDefaultsToOff keeps passing — while silently
	// turning SLO tracking on for every operator who upgrades without passing
	// --slo-target. 0 is validateSLOTarget's sentinel meaning "SLO tracking
	// off", not a placeholder a future default should fill in; the value that
	// actually reaches watch.Config is the only thing that can catch a mutation
	// downstream of parsing, so this asserts on that, not on a proxy for it.
	t.Setenv("KUBEAGENT_SLO_TARGET", "")
	kc := deadKubeconfigPath(t)
	cfg, err := captureWatchConfig(t, []string{"--kubeconfig", kc, "--metrics-addr", "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("runWatch returned an unexpected error: %v", err)
	}
	if cfg.SLOTarget != 0 {
		t.Fatalf("cfg.SLOTarget = %v, want exactly 0 (SLO tracking off by default)", cfg.SLOTarget)
	}
}

func TestRunWatch_ValidSLOTargetReachesConfigAsARatio(t *testing.T) {
	// The first test that observes a *valid* --slo-target reaching
	// watch.Config at all: every other SLO test in this file only exercises
	// the rejection path (an out-of-range target failing validateSLOTarget).
	//
	// This used to compare with a tolerance instead of ==, because plain
	// float64 division (*sloTarget / 100) is not guaranteed to be
	// bit-identical to the literal 0.999. runWatch now rounds the ratio to 8
	// decimal places specifically to land on the value an operator typed, so
	// exact equality is the stronger assertion here: it fails the instant
	// that rounding is dropped, where a tolerance check would silently keep
	// passing.
	kc := deadKubeconfigPath(t)
	cfg, err := captureWatchConfig(t, []string{"--slo-target", "99.9", "--kubeconfig", kc, "--metrics-addr", "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("runWatch returned an unexpected error: %v", err)
	}
	if cfg.SLOTarget != 0.999 {
		t.Fatalf("cfg.SLOTarget = %v, want exactly 0.999", cfg.SLOTarget)
	}
}

func TestRunWatch_SLOTargetRoundsToExactRatio(t *testing.T) {
	// kubeagent_slo_target_ratio is rendered with %g, which prints the
	// shortest string that round-trips the double — so whatever
	// *sloTarget/100 actually produces in float64 is what an operator
	// scraping the daemon sees. Plain division puts several of the most
	// common SLO targets one bit off the value the operator typed (e.g.
	// 99.9/100 -> 0.9990000000000001), so runWatch rounds the ratio to 8
	// decimal places. This exercises that rounding for four targets whose
	// unrounded division is not bit-identical to the intended ratio, through
	// the real watch.Config runWatch builds — not a proxy computation — so a
	// regression in the rounding itself, not just its presence, gets caught.
	//
	// The 99.9994 row pins the 8-decimal-place constant itself: a mutant that
	// coarsens the rounding (e.g. 1e8 -> 1e5) still rounds 99.9, 99.99,
	// 99.999, and 99.95 correctly, so none of the first four rows can catch
	// it. 99.9994's ratio (0.999994) needs all 6 of its decimal digits kept,
	// and — unlike 99.9999 below — rounding it at 5 places (0.99999) doesn't
	// reach the 1.0 boundary, so the boundary guard has nothing to correct:
	// a coarser rounding constant is exposed directly, uncorrected.
	//
	// 99.9999 -> 0.999999 documents the comment's own six-nines claim, but by
	// itself does NOT pin the constant the way 99.9994 does: 99.9999's ratio
	// sits close enough to 1.0 that rounding it at *any* coarser scale that
	// still overshoots 1.0 (5 places included) trips the boundary guard,
	// which then substitutes the exact quotient — which is exactly what this
	// row expects. So this row mainly documents the comment's example and
	// exercises the guard on a value one order of magnitude short of the
	// fallback boundary; 99.9994 is what actually catches a coarsened
	// constant.
	//
	// 99.9999999 -> 0.999999999 exercises the fallback path itself: at 8
	// places this rounds up to exactly 1.0, outside the valid ratio range, so
	// runWatch must take the exact (unrounded) quotient instead.
	cases := []struct {
		target string
		want   float64
	}{
		{"99.9", 0.999},
		{"99.99", 0.9999},
		{"99.999", 0.99999},
		{"99.95", 0.9995},
		{"99.9994", 0.999994},
		{"99.9999", 0.999999},
		{"99.9999999", 0.999999999},
	}
	for _, c := range cases {
		kc := deadKubeconfigPath(t)
		cfg, err := captureWatchConfig(t, []string{"--slo-target", c.target, "--kubeconfig", kc, "--metrics-addr", "127.0.0.1:0"})
		if err != nil {
			t.Fatalf("runWatch(--slo-target %s) returned an unexpected error: %v", c.target, err)
		}
		if cfg.SLOTarget != c.want {
			t.Errorf("--slo-target %s: cfg.SLOTarget = %v, want exactly %v", c.target, cfg.SLOTarget, c.want)
		}
	}
}

func TestRunWatch_TinyNonzeroSLOTargetIsNotLaunderedToOff(t *testing.T) {
	// The mirror image of TestRunWatch_DefaultSLOTargetReachesConfigAsZero: 0
	// is validateSLOTarget's explicit "SLO tracking off" sentinel, but a tiny
	// nonzero percentage must never collapse into that sentinel merely
	// because rounding the ratio to 8 decimal places lands on 0. Without the
	// boundary guard, --slo-target 0.0000001 divides to roughly 1e-9, rounds
	// to exactly 0 at 8 decimal places, and reaches watch.Config
	// indistinguishable from "the operator never set --slo-target" — silently
	// turning SLO tracking off instead of turning it on at a (nonsensically
	// strict, but explicitly requested) near-zero error budget.
	//
	// validateSLOTarget itself would accept this target: it is > 0 and < 1,
	// so it takes neither the "== 0, disabled" branch nor either rejection
	// branch. The right observable behavior is therefore that it reaches
	// watch.Config as the exact, unrounded quotient — not that it gets
	// rejected.
	//
	// want is computed here the same way runWatch computes it: runtime
	// float64 division of a float64 variable, not a source constant
	// expression. Go evaluates a constant expression like 0.0000001/100 at
	// effectively infinite precision and rounds only once at the end, which
	// is not always bit-identical to two chained IEEE 754 float64 operations
	// (parsing the flag, then dividing by 100 at runtime) — a hardcoded
	// literal would risk asserting the wrong bits.
	var target float64 = 0.0000001
	want := target / 100
	if want == 0 {
		t.Fatal("test bug: want computed as exactly 0, this test could not detect laundering to the sentinel")
	}

	kc := deadKubeconfigPath(t)
	cfg, err := captureWatchConfig(t, []string{"--slo-target", "0.0000001", "--kubeconfig", kc, "--metrics-addr", "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("runWatch(--slo-target 0.0000001) returned an unexpected error: %v", err)
	}
	if cfg.SLOTarget == 0 {
		t.Fatal("cfg.SLOTarget = 0, want nonzero: a tiny nonzero --slo-target must not be laundered into the \"SLO tracking off\" sentinel")
	}
	if cfg.SLOTarget != want {
		t.Fatalf("cfg.SLOTarget = %v, want exactly %v (the exact, unrounded quotient)", cfg.SLOTarget, want)
	}
}

func TestRunWatch_NegativeSLOTargetIsRejected(t *testing.T) {
	// An ordinary negative --slo-target: -5/100 is -0.05, and
	// math.Round(-0.05*1e8)/1e8 lands back on exactly -0.05 with no rounding
	// artifact at all, so the boundary guard's condition — comparing that
	// rounded value against the exact quotient — evaluates false and takes no
	// corrective action here. This is the baseline the tiny-negative test
	// below contrasts with: dropping the guard's zero-side clause, or the
	// whole guard, changes nothing about this case (verified in the mutation
	// checks recorded in the task report), because sloRatio never needed
	// correcting in the first place. This test exists to nail down, through
	// the real unstubbed watch.Run, that an everyday negative percentage is
	// rejected end-to-end — the tiny-negative test isolates the laundering
	// the guard actually exists to prevent.
	kc := deadKubeconfigPath(t)
	err := runWatchBounded(t, []string{"--slo-target", "-5", "--kubeconfig", kc, "--metrics-addr", "127.0.0.1:0"}, 3*time.Second)
	if err == nil {
		t.Fatal("expected a validation error for --slo-target -5")
	}
	want := "invalid --slo-target: -5% (must be greater than 0 and less than 100)"
	if err.Error() != want {
		t.Fatalf("error = %q, want exactly %q", err.Error(), want)
	}
}

func TestRunWatch_TinyNegativeSLOTargetIsNotLaunderedToOff(t *testing.T) {
	// The negative mirror of TestRunWatch_TinyNonzeroSLOTargetIsNotLaunderedToOff,
	// and the subtlest case the boundary guard has to get right: -0.0 == 0.0
	// in Go, so a negative target whose rounded ratio lands on negative zero
	// is, without the guard's (sloRatio == 0) != (exact == 0) clause,
	// indistinguishable from validateSLOTarget's "0 means SLO tracking off"
	// sentinel — laundering a value that must be *rejected* into one that is
	// silently *accepted*.
	//
	// math.Round(exact*1e8)/1e8 rounds to (negative) zero exactly when
	// |exact*1e8| < 0.5, i.e. |exact| < 0.5e-8, i.e. |target/100| < 0.5e-8,
	// i.e. |target| < 0.5e-6. -0.0000001 (-1e-7) sits inside that band: exact
	// = -1e-7/100 is about -1e-9, so exact*1e8 is about -0.1, which
	// math.Round sends to negative zero (Round rounds half away from zero,
	// and preserves sign on underflow to zero — confirmed with
	// math.Signbit in a standalone check before writing this test, the same
	// way the task's reviewer verified it independently). Without the zero-
	// side clause, that -0 compares equal to 0 and reaches validateSLOTarget
	// as the "off" sentinel instead of the negative value the operator typed.
	//
	// This is asserted on runWatch's returned error, not on cfg.SLOTarget via
	// captureWatchConfig: that helper stubs out watchRun itself, so it never
	// calls the real watch.Run and never reaches validateSLOTarget — it can
	// only show which ratio runWatch computed, not whether that ratio gets
	// accepted or rejected, and rejection is the property this guard exists
	// for. So this goes through runWatchBounded against the dead kubeconfig
	// instead, exercising the real, unstubbed watch.Run, whose first
	// statement is validateSLOTarget. If the zero-side clause (or the whole
	// guard) were removed, this negative target would launder to the ratio
	// 0, validateSLOTarget would accept it as "disabled", and Run would fall
	// through into binding the metrics address and blocking on
	// WaitForCacheSync against a server nothing answers — forever.
	// runWatchBounded's timeout is exactly what turns that hang into a clean,
	// fast test failure instead of wedging the test binary.
	//
	// want is computed here the same way runWatch and validateSLOTarget
	// compute it — runtime float64 division and multiplication, not a copied
	// literal — for the same reason TestRunWatch_TinyNonzeroSLOTargetIsNotLaunderedToOff
	// gives: a hand-typed scientific-notation literal risks pinning the wrong
	// bits.
	var target float64 = -0.0000001
	exact := target / 100
	if exact == 0 {
		t.Fatal("test bug: exact computed as exactly 0, this test could not distinguish rejection from laundering")
	}
	want := fmt.Sprintf("invalid --slo-target: %g%% (must be greater than 0 and less than 100)", exact*100)

	kc := deadKubeconfigPath(t)
	err := runWatchBounded(t, []string{"--slo-target", "-0.0000001", "--kubeconfig", kc, "--metrics-addr", "127.0.0.1:0"}, 3*time.Second)
	if err == nil {
		t.Fatal("expected a validation error for --slo-target -0.0000001, got none: the negative target may have been laundered into the \"SLO tracking off\" sentinel")
	}
	if err.Error() != want {
		t.Fatalf("error = %q, want exactly %q", err.Error(), want)
	}
}

func fixWorkload() []inventory.Workload {
	return []inventory.Workload{{Namespace: "shop", Name: "web", Kind: "Deployment",
		Desired: 1, Ready: 0, // degraded, so RolloutUndo is proposed under the Ready < Desired gate
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff"}}}}
}
func fixRS() []appsv1.ReplicaSet {
	mk := func(name, rev, img string) appsv1.ReplicaSet {
		r := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
		r.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: img}}}}
		return r
	}
	return []appsv1.ReplicaSet{mk("web-1", "1", "nginx:1.27"), mk("web-2", "2", "nginx:bad")}
}

func TestRun_InvestigateNeedsAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := Run([]string{"scan", "--investigate"})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("expected an ANTHROPIC_API_KEY error, got %v", err)
	}
}

func TestRun_InvestigateRejectsLocalOnlyEndpoint(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "http://localhost:11434/v1")
	err := Run([]string{"scan", "--investigate"})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("investigate must require an Anthropic key even when a local endpoint is set, got %v", err)
	}
}

func TestRun_InvestigateSupersedesExplain(t *testing.T) {
	// Passing both --investigate and --explain must not produce a flag-parse error
	// or a precondition error — --investigate supersedes --explain silently. The
	// scan fails at cluster-connect (bogus kubeconfig), which is the expected
	// outcome: it proves flags parsed and the investigate branch was selected.
	//
	// The "flag provided but not defined" comparison below is the standard
	// library's wording, not pflag's "unknown flag: --x" — it could never fail
	// here even if --investigate or --explain were dropped, so the positive
	// assertion that this is specifically the kubeconfig-load error is what
	// actually proves both flags parsed.
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := Run([]string{"scan", "--investigate", "--explain", "--kubeconfig", "/nonexistent/path"})
	if err == nil {
		t.Fatal("expected a cluster-connect error for a nonexistent kubeconfig")
	}
	msg := err.Error()
	if strings.Contains(msg, "flag provided but not defined") {
		t.Errorf("got flag-parse error; both flags must be defined: %v", err)
	}
	if strings.Contains(msg, "ANTHROPIC_API_KEY") || strings.Contains(msg, "KUBEAGENT_EXPLAIN_ENDPOINT") {
		t.Errorf("got a precondition error; should have reached cluster-connect: %v", err)
	}
	if !strings.Contains(msg, "loading kubeconfig") {
		t.Errorf("expected the kubeconfig-load error (proving --investigate/--explain parsed rather than being rejected), got: %v", err)
	}
}

// allowFix makes the fake clientset's SelfSubjectAccessReview return Allowed:true so
// tests that exercise the write path can reach the actual write.
func allowFix(cli *fake.Clientset) {
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true},
		}, nil
	})
}

// denyFix makes the fake clientset's SelfSubjectAccessReview return Allowed:false so
// tests can exercise the preflight-denied path.
func denyFix(cli *fake.Clientset) {
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: false},
		}, nil
	})
}

func TestRunFixes_PrintsWillChangeBlock(t *testing.T) {
	actions := []remediate.Action{{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		Target: "shop/web (Deployment)", Summary: "roll back to the previous revision",
		Reason:            "newest rollout cannot pull its image; a prior revision (1) exists",
		KubectlEquivalent: "kubectl -n shop rollout undo deployment/web",
		Changes: []remediate.Change{
			{Field: "revision", From: "2", To: "1"},
			{Field: "image (c)", From: "nginx:broken", To: "nginx:1.27"},
			{Field: "1 other template field changed"},
		},
		CurrentRevision: 2, TargetRevision: 1,
	}}
	var out bytes.Buffer
	runFixes(context.Background(), fake.NewSimpleClientset(), actions, true /*dryRun*/, false, &out, strings.NewReader(""), nil)
	s := out.String()
	for _, want := range []string{
		"will change:",
		"revision: 2 → 1",
		"image (c): nginx:broken → nginx:1.27",
		"1 other template field changed",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestRunFixes_DryRunWritesNothing(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:bad"}}}}
	cli := fake.NewSimpleClientset(d)
	var out bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), cli, actions, true /*dryRun*/, false, &out, strings.NewReader(""), nil)
	for _, a := range cli.Actions() {
		if a.GetVerb() == "update" {
			t.Fatalf("dry-run must not write; saw %s", a.GetVerb())
		}
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("expected a dry-run notice, got: %s", out.String())
	}
}

func TestRunFixes_YesApplies(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:bad"}}}}
	rss := fixRS()
	cli := fake.NewSimpleClientset(d, &rss[0], &rss[1])
	allowFix(cli)
	var out bytes.Buffer
	actions := remediate.Plan(fixWorkload(), rss, nil)
	runFixes(context.Background(), cli, actions, false, true /*assumeYes*/, &out, strings.NewReader(""), nil)
	got, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if got.Spec.Template.Spec.Containers[0].Image != "nginx:1.27" {
		t.Errorf("expected rollback to nginx:1.27, got %q", got.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestRunFixes_DryRunUncordonWritesNothing(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
	n.Spec.Unschedulable = true
	cli := fake.NewSimpleClientset(n)
	var out bytes.Buffer
	actions := remediate.Plan(nil, nil, []corev1.Node{*n})
	runFixes(context.Background(), cli, actions, true /*dryRun*/, false, &out, strings.NewReader(""), nil)
	for _, a := range cli.Actions() {
		if a.GetVerb() == "update" {
			t.Fatalf("dry-run must not write a node; saw update")
		}
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("expected a dry-run notice, got: %s", out.String())
	}
}

func TestRunFixes_UncordonYesApplies(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
	n.Spec.Unschedulable = true
	cli := fake.NewSimpleClientset(n)
	allowFix(cli)
	var out bytes.Buffer
	actions := remediate.Plan(nil, nil, []corev1.Node{*n})
	runFixes(context.Background(), cli, actions, false, true, &out, strings.NewReader(""), nil)
	got, _ := cli.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if got.Spec.Unschedulable {
		t.Errorf("expected node uncordoned by --yes")
	}
	if !strings.Contains(out.String(), "node/worker-1") {
		t.Errorf("expected the node target in output, got: %s", out.String())
	}
}

func rsFor(ns, name, owner, rev, image string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: owner}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": owner, "pod-template-hash": "h" + rev}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
		}},
	}
}

func TestRunRollback_UndoesLastAppliedFix(t *testing.T) {
	// The cluster is where the fix left it: rev 4 (nginx:1.27); rev 5 is pre-fix.
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "4"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.27"}}}}
	r4 := rsFor("shop", "web-4", "web", "4", "nginx:1.27")
	r5 := rsFor("shop", "web-5", "web", "5", "nginx:2.0")
	cli := fake.NewSimpleClientset(d, r4, r5)
	allowFix(cli)

	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(`{"time":"2026-07-24T08:00:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied","fromRevision":5,"toRevision":4}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, auditBuf bytes.Buffer
	if err := runRollback(context.Background(), cli, p, false, true /*yes*/, &out, strings.NewReader(""), audit.NewWriter(&auditBuf)); err != nil {
		t.Fatal(err)
	}
	got, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if img := got.Spec.Template.Spec.Containers[0].Image; img != "nginx:2.0" {
		t.Errorf("image = %q, want the pre-fix nginx:2.0", img)
	}
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "rollback" {
		t.Fatalf("want one rollback record, got %+v", recs)
	}
}

func TestRunRollback_NothingToRollBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(`{"time":"2026-07-24T08:00:00Z","kind":"Uncordon","name":"w1","target":"node/w1","disposition":"declined"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := fake.NewSimpleClientset()
	var out bytes.Buffer
	if err := runRollback(context.Background(), cli, p, false, true, &out, strings.NewReader(""), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to roll back") {
		t.Errorf("expected the nothing-to-roll-back message, got: %s", out.String())
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("no write may happen when there is nothing to roll back")
		}
	}
}

func TestRunRollback_PreV054RecordRefuses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(`{"time":"2026-07-24T08:00:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := fake.NewSimpleClientset()
	var out bytes.Buffer
	if err := runRollback(context.Background(), cli, p, false, true, &out, strings.NewReader(""), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "v0.54") {
		t.Errorf("expected the version refusal, got: %s", out.String())
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("a pre-v0.54 record must not produce a write")
		}
	}
}

func TestRunRollback_DryRunWritesNothing(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "4"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.27"}}}}
	cli := fake.NewSimpleClientset(d)
	allowFix(cli)
	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(`{"time":"2026-07-24T08:00:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied","fromRevision":5,"toRevision":4}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, auditBuf bytes.Buffer
	if err := runRollback(context.Background(), cli, p, true /*dryRun*/, false, &out, strings.NewReader(""), audit.NewWriter(&auditBuf)); err != nil {
		t.Fatal(err)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("dry-run must not write")
		}
	}
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "dry-run" {
		t.Fatalf("want one dry-run record, got %+v", recs)
	}
}

func TestRunRollback_DeclinedWritesNothing(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "4"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.27"}}}}
	cli := fake.NewSimpleClientset(d)
	allowFix(cli)
	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(`{"time":"2026-07-24T06:30:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied","fromRevision":5,"toRevision":4}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, auditBuf bytes.Buffer
	if err := runRollback(context.Background(), cli, p, false /*dryRun*/, false /*assumeYes*/, &out, strings.NewReader("n\n"), audit.NewWriter(&auditBuf)); err != nil {
		t.Fatal(err)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("declining must not write")
		}
	}
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "declined" {
		t.Fatalf("want one declined record, got %+v", recs)
	}
}

func TestRun_RollbackNeedsAuditLog(t *testing.T) {
	err := Run([]string{"scan", "--rollback"})
	if err == nil || !strings.Contains(err.Error(), "--audit-log") {
		t.Errorf("expected an --audit-log requirement error, got %v", err)
	}
}

func TestRun_RollbackAndFixAreExclusive(t *testing.T) {
	err := Run([]string{"scan", "--rollback", "--fix", "--audit-log", "/tmp/x.log"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected a mutual-exclusion error, got %v", err)
	}
}

func auditLines(t *testing.T, s string) []audit.Record {
	t.Helper()
	var recs []audit.Record
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		var r audit.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("audit line not JSON: %v (%q)", err, line)
		}
		recs = append(recs, r)
	}
	return recs
}

func TestRunFixes_AuditRecordsDryRun(t *testing.T) {
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), fake.NewSimpleClientset(), actions, true /*dryRun*/, false, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "dry-run" {
		t.Fatalf("want one dry-run record, got %+v", recs)
	}
}

func TestRunFixes_AuditRecordsDeclined(t *testing.T) {
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), fake.NewSimpleClientset(), actions, false, false, &out, strings.NewReader("n\n"), audit.NewWriter(&auditBuf))
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "declined" {
		t.Fatalf("want one declined record, got %+v", recs)
	}
}

func TestRunFixes_AuditRecordsApplied(t *testing.T) {
	// Mirror TestRunFixes_YesApplies' live-appliable fixtures exactly.
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:bad"}}}}
	rss := fixRS()
	cli := fake.NewSimpleClientset(d, &rss[0], &rss[1])
	allowFix(cli)
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), rss, nil)
	runFixes(context.Background(), cli, actions, false, true /*yes*/, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "applied" {
		t.Fatalf("want one applied record, got %+v", recs)
	}
}

func TestRunFixes_NilAuditWriterLogsNothing(t *testing.T) {
	var out bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), fake.NewSimpleClientset(), actions, true, false, &out, strings.NewReader(""), nil)
	// no panic, no audit output; the human output still rendered
	if !strings.Contains(out.String(), "Proposed fix") {
		t.Error("human output should still render with a nil audit writer")
	}
}

func TestRunFixes_AuditRecordsRefused(t *testing.T) {
	// Drift scenario: action previewed at CurrentRevision=2 but cluster is at rev 3,
	// so remediate.Apply returns Refused=true → audit disposition must be "refused".
	cur := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "3"}}}
	cur.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:still-broken"}}}}
	r1 := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1",
		Annotations:     map[string]string{"deployment.kubernetes.io/revision": "1"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
	r1.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.27"}}}}
	r2 := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-2",
		Annotations:     map[string]string{"deployment.kubernetes.io/revision": "2"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
	r2.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:broken"}}}}
	r3 := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-3",
		Annotations:     map[string]string{"deployment.kubernetes.io/revision": "3"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
	r3.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:still-broken"}}}}
	cli := fake.NewSimpleClientset(cur, &r1, &r2, &r3)

	// Action was previewed at CurrentRevision=2, but the cluster has advanced to rev 3 → drift.
	actions := []remediate.Action{{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		Target: "shop/web (Deployment)", Summary: "roll back", Reason: "r",
		KubectlEquivalent: "kubectl -n shop rollout undo deployment/web",
		CurrentRevision:   2, TargetRevision: 1,
		Changes: []remediate.Change{{Field: "revision", From: "2", To: "1"}},
	}}
	var out, auditBuf bytes.Buffer
	runFixes(context.Background(), cli, actions, false, true /*assumeYes*/, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "refused" {
		t.Fatalf("want one refused record, got %+v", recs)
	}
}

func TestRunFixes_AuditRecordsError(t *testing.T) {
	// Inject a failing reactor so the Deployment update errors → audit disposition "error".
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:bad"}}}}
	rss := fixRS()
	cli := fake.NewSimpleClientset(d, &rss[0], &rss[1])
	allowFix(cli)
	cli.PrependReactor("update", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("update boom")
	})
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), rss, nil)
	runFixes(context.Background(), cli, actions, false, true /*assumeYes*/, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "error" {
		t.Fatalf("want one error record, got %+v", recs)
	}
}

func TestRunFixes_AuditRecordsPreflight(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:bad"}}}}
	rss := fixRS()
	cli := fake.NewSimpleClientset(d, &rss[0], &rss[1])
	denyFix(cli) // reaches the gate, then denied
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), rss, nil)
	runFixes(context.Background(), cli, actions, false, true /*yes*/, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "preflight" {
		t.Fatalf("want one preflight record, got %+v", recs)
	}
	if !strings.Contains(out.String(), "no write attempted") {
		t.Errorf("expected the preflight skip line, got: %s", out.String())
	}
}

func TestRunFixes_DryRunReportsPermissionAllowed(t *testing.T) {
	cli := fake.NewSimpleClientset()
	allowFix(cli)
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), cli, actions, true /*dryRun*/, false, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	if !strings.Contains(out.String(), "you have permission") {
		t.Errorf("dry-run should report permission, got: %s", out.String())
	}
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "dry-run" {
		t.Fatalf("dry-run disposition expected, got %+v", recs)
	}
}

func TestRunFixes_DryRunReportsPermissionDenied(t *testing.T) {
	cli := fake.NewSimpleClientset()
	denyFix(cli)
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), cli, actions, true /*dryRun*/, false, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	if !strings.Contains(out.String(), "would be blocked") {
		t.Errorf("dry-run should report the block, got: %s", out.String())
	}
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "dry-run" {
		t.Fatalf("dry-run disposition expected on the denied path, got %+v", recs)
	}
}

func TestRunWatchWiresExplainConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "<PLACEHOLDER>")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	t.Setenv("KUBEAGENT_MODEL", "")

	var got watch.Config
	orig := watchRun
	watchRun = func(_ context.Context, _ []watch.Target, cfg watch.Config) error {
		got = cfg
		return nil
	}
	defer func() { watchRun = orig }()

	// The dead kubeconfig keeps this hermetic: runWatch builds a real clientset
	// before it reaches the stubbed watchRun, so without one the test passes only
	// on a machine that happens to have ~/.kube/config.
	args := []string{"--kubeconfig", deadKubeconfigPath(t),
		"--explain", "--explain-cooldown", "30m", "--explain-budget", "5", "--model", "test-model"}
	if err := runWatch(args); err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	if !got.Explain {
		t.Error("Explain must be true")
	}
	if got.ExplainCooldown != 30*time.Minute {
		t.Errorf("cooldown = %s, want 30m", got.ExplainCooldown)
	}
	if got.ExplainBudget != 5 {
		t.Errorf("budget = %d, want 5", got.ExplainBudget)
	}
	if got.ExplainModel != "test-model" {
		t.Errorf("model = %q, want test-model", got.ExplainModel)
	}
}

func TestRunWatchDefaultsExplainOff(t *testing.T) {
	t.Setenv("KUBEAGENT_EXPLAIN", "")
	t.Setenv("KUBEAGENT_EXPLAIN_COOLDOWN", "")
	t.Setenv("KUBEAGENT_EXPLAIN_BUDGET", "")

	var got watch.Config
	orig := watchRun
	watchRun = func(_ context.Context, _ []watch.Target, cfg watch.Config) error {
		got = cfg
		return nil
	}
	defer func() { watchRun = orig }()

	if err := runWatch([]string{"--kubeconfig", deadKubeconfigPath(t)}); err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	if got.Explain {
		t.Error("--explain must be off by default")
	}
	if got.ExplainCooldown != time.Hour {
		t.Errorf("default cooldown = %s, want 1h", got.ExplainCooldown)
	}
	if got.ExplainBudget != 20 {
		t.Errorf("default budget = %d, want 20", got.ExplainBudget)
	}
}

// A config error must surface before the daemon starts, not after the metrics
// server is listening and a cache sync is underway.
func TestRunWatchExplainWithoutCredentialsFailsFast(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")

	orig := watchRun
	watchRun = func(context.Context, []watch.Target, watch.Config) error {
		t.Fatal("the daemon must not start without credentials")
		return nil
	}
	defer func() { watchRun = orig }()

	err := runWatch([]string{"--explain"})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error = %q, want it to name the missing credential", err)
	}
}

func TestRunWatchLocalEndpointNeedsAModelName(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "http://127.0.0.1:11434/v1")
	t.Setenv("KUBEAGENT_MODEL", "")

	orig := watchRun
	watchRun = func(context.Context, []watch.Target, watch.Config) error {
		t.Fatal("the daemon must not start without a model name")
		return nil
	}
	defer func() { watchRun = orig }()

	err := runWatch([]string{"--explain"})
	if err == nil {
		t.Fatal("want an error naming --model, got nil")
	}
	if !strings.Contains(err.Error(), "--model") {
		t.Errorf("error = %q, want it to name the missing --model flag", err)
	}
}

func TestUsageMentionsTheExplainFlags(t *testing.T) {
	err := Run(nil)
	if err == nil {
		t.Fatal("want the usage error")
	}
	for _, want := range []string{"--explain-cooldown", "--explain-budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("usage does not mention %s", want)
		}
	}
}

// TestBuildTargetsNaming pins the three naming rules: no --context means one
// default target named by --cluster-name; each --context names its own target;
// and --include-local adds the default target alongside them.
func TestBuildTargetsNaming(t *testing.T) {
	kc := multiContextKubeconfigPath(t)
	t.Setenv("KUBECONFIG", kc)

	tests := []struct {
		name         string
		contexts     []string
		includeLocal bool
		want         []string
	}{
		{"no contexts", nil, false, []string{"local"}},
		{"one context", []string{"alpha"}, false, []string{"alpha"}},
		{"two contexts", []string{"alpha", "beta"}, false, []string{"alpha", "beta"}},
		{"include local", []string{"alpha"}, true, []string{"local", "alpha"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			targets, err := buildTargets(kc, "local", tc.contexts, tc.includeLocal)
			if err != nil {
				t.Fatalf("buildTargets: %v", err)
			}
			var got []string
			for _, tg := range targets {
				got = append(got, tg.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("target names = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildTargetsRejectsAnUnknownContext pins the fail-fast rule. Building a
// client contacts no API server, so a failure here is a misspelled context, and
// silently watching fewer clusters than the operator asked for is the outcome
// this prevents.
func TestBuildTargetsRejectsAnUnknownContext(t *testing.T) {
	kc := multiContextKubeconfigPath(t)
	t.Setenv("KUBECONFIG", kc)
	if _, err := buildTargets(kc, "local", []string{"nope"}, false); err == nil {
		t.Fatal("buildTargets accepted a context that is not in the kubeconfig")
	}
}

// multiContextKubeconfigPath writes a kubeconfig with two contexts pointing at a
// closed loopback port. Every server here is unreachable on purpose: building a
// clientset performs no network I/O, so these tests stay hermetic.
func multiContextKubeconfigPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := `apiVersion: v1
kind: Config
current-context: alpha
clusters:
  - name: alpha
    cluster:
      server: https://127.0.0.1:1
  - name: beta
    cluster:
      server: https://127.0.0.1:2
contexts:
  - name: alpha
    context: {cluster: alpha, user: alpha}
  - name: beta
    context: {cluster: beta, user: beta}
users:
  - name: alpha
    user: {token: <PLACEHOLDER>}
  - name: beta
    user: {token: <PLACEHOLDER>}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

func TestUsage_MentionsTheMCPSubcommand(t *testing.T) {
	err := Run([]string{"kubeagent"})
	if err == nil {
		t.Fatal("run() with no subcommand error = nil, want the usage error")
	}
	if !strings.Contains(err.Error(), "kubeagent mcp") {
		t.Errorf("usage = %q, want it to list the mcp subcommand", err)
	}
}

// TestRunMCP_StdoutStaysEmptyOnFailurePaths pins down stdout purity: the MCP
// protocol owns stdout on the stdio transport, so a single stray write from
// any reachable failure path in runMCP corrupts the protocol stream for
// every caller. This is exercised over the failure paths reachable without a
// cluster: an undefined flag, and a connection failure against a nonexistent
// kubeconfig. (-h used to be a third row here; see TestMCPHelpGoesToStdout for
// why it moved out.)
//
// parseMCPFlags builds a throwaway *cobra.Command and calls its pflag.FlagSet
// directly — it never goes through Cobra's own Execute()/ExecuteC(), which is
// the only layer that ever copies a flag-parsing error to a real,
// process-level stderr. pflag's FlagSet.Parse, constructed by Cobra with
// ContinueOnError, returns a parse error without writing anywhere in that
// case (unlike the standard library's flag package, which always printed via
// failf regardless of ErrorHandling). So neither case here writes to os.Stderr
// any more; both are checked via the returned error instead, the same way the
// connection failure already was.
func TestRunMCP_StdoutStaysEmptyOnFailurePaths(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "mcp-purity-nonexistent-kubeconfig")

	tests := []struct {
		name string
		args []string
		// wantStderr is true when the flag package itself is expected to have
		// written directly to os.Stderr for this case.
		wantStderr bool
	}{
		{name: "undefined flag", args: []string{"--bogus"}, wantStderr: false},
		{name: "connection failure", args: []string{"--kubeconfig", bad}, wantStderr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr string
			var err error
			stdout := captureStdout(t, func() {
				stderr = captureStderr(t, func() {
					err = runMCP(tt.args)
				})
			})

			if stdout != "" {
				t.Errorf("stdout = %q, want empty: the MCP protocol owns stdout, and any write here corrupts the stream", stdout)
			}
			if err == nil {
				t.Fatal("expected runMCP to return an error")
			}
			if tt.wantStderr {
				if stderr == "" {
					t.Error("stderr = \"\" (empty), want a non-empty diagnostic — an empty result here means " +
						"this test captured the wrong stream, or the diagnostic was silenced")
				}
			} else if err.Error() == "" {
				t.Error("err.Error() = \"\" (empty), want a non-empty diagnostic message — an empty result " +
					"here would mean the connection failure was silenced entirely")
			}
		})
	}
}

// TestMCPHelpGoesToStdout documents a deliberate change. Under the standard
// library, `mcp -h` wrote usage to stderr; Cobra writes help to stdout. That
// is safe here even though the MCP protocol owns stdout: --help returns
// before Serve is ever called, so there is no stream to corrupt, and a client
// speaking protocol never passes --help. The stdout-purity invariant for the
// paths that DO fail is unchanged and still asserted next door.
func TestMCPHelpGoesToStdout(t *testing.T) {
	var err error
	stdout := captureStdout(t, func() {
		stderr := captureStderr(t, func() { err = Run([]string{"mcp", "--help"}) })
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})
	if err != nil {
		t.Fatalf("Run([mcp --help]) = %v, want nil", err)
	}
	if stdout == "" {
		t.Error("stdout is empty, want the help text")
	}
}

func TestRunMCP_FlagsAreRecognized(t *testing.T) {
	// --kubeconfig/--context/--allow-context-switch/--logs must all be defined
	// flags: with a kubeconfig path that fails to load, the error must be the
	// cluster-connection error, not "flag provided but not defined", proving
	// all four flags parsed rather than being rejected. Same idiom as
	// TestRunWatch_AlertFlagsAreRecognized.
	dir := t.TempDir()
	bad := filepath.Join(dir, "mcp-flags-nonexistent-kubeconfig")
	err := runMCP([]string{
		"--kubeconfig", bad,
		"--context", "some-context",
		"--allow-context-switch",
		"--logs",
	})
	if err == nil {
		t.Fatal("expected a cluster-connection error")
	}
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("expected all four flags to be recognized, got: %v", err)
	}
	if !strings.Contains(err.Error(), "connecting to the cluster") {
		t.Fatalf("expected a cluster-connection error, got: %v", err)
	}
}

func TestRun_DispatchesMCPWithFlagsIntact(t *testing.T) {
	// Inside run(), args[0] is the subcommand token ("mcp") itself, so the
	// dispatch must slice args[1:] before handing off to runMCP — slicing
	// args[2:] instead would silently drop the first flag the caller passed
	// (an empty flag set parses without error, so nothing would complain).
	// This drives run() end-to-end with a single flag and asserts that flag's
	// value survived the slice, by checking the resulting error names the
	// exact nonexistent kubeconfig path that was passed.
	dir := t.TempDir()
	bad := filepath.Join(dir, "mcp-dispatch-nonexistent-kubeconfig")
	err := Run([]string{"mcp", "--kubeconfig", bad})
	if err == nil {
		t.Fatal("expected a cluster-connection error")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Fatalf("expected the error to name the nonexistent kubeconfig path %q, got: %v", bad, err)
	}
}

func TestEnvDuration(t *testing.T) {
	const key = "KUBEAGENT_TEST_DRIFT_AGE"
	tests := []struct {
		name string
		set  string
		want time.Duration
	}{
		{"unset falls back", "", time.Hour},
		{"parses minutes", "30m", 30 * time.Minute},
		{"parses hours", "36h", 36 * time.Hour},
		{"garbage falls back", "soon", time.Hour},
		{"bare number falls back", "60", time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set == "" {
				os.Unsetenv(key)
			} else {
				t.Setenv(key, tt.set)
			}
			if got := envDuration(key, time.Hour); got != tt.want {
				t.Errorf("envDuration(%q) = %v, want %v", tt.set, got, tt.want)
			}
		})
	}
}

// invocationName is a pure function of argv[0] so the kubectl-plugin spelling
// can be tested without launching a process under that name.
func TestInvocationName(t *testing.T) {
	tests := []struct {
		argv0 string
		want  string
	}{
		{"/home/u/.krew/bin/kubectl-kubeagent", "kubectl kubeagent"},
		{"kubectl-kubeagent", "kubectl kubeagent"},
		{"./kubeagent", "kubeagent"},
		{"/usr/local/bin/kubeagent", "kubeagent"},
		// kubectl-kubeagent as a DIRECTORY component must not match. A naive
		// strings.Contains passes every other row and fails this one.
		{"/opt/kubectl-kubeagent/kubeagent", "kubeagent"},
		{"", "kubeagent"},
		{"kubectl-kubeagent-extra", "kubeagent"},
	}
	for _, tt := range tests {
		if got := invocationName(tt.argv0); got != tt.want {
			t.Errorf("invocationName(%q) = %q, want %q", tt.argv0, got, tt.want)
		}
	}
}

func TestRun_UsageNamesThePlainBinaryByDefault(t *testing.T) {
	// The test binary's argv[0] basename is never "kubectl-kubeagent", so the
	// default spelling under `go test` is the plain one.
	if invokedAs != "kubeagent" {
		t.Fatalf("invokedAs = %q under go test, want %q", invokedAs, "kubeagent")
	}
	err := Run(nil)
	if err == nil {
		t.Fatal("run(nil) = nil, want the usage error")
	}
	if !strings.Contains(err.Error(), "usage: kubeagent scan") {
		t.Errorf("usage = %q, want it to start by naming `kubeagent scan`", err)
	}
}

func TestRun_UsageNamesTheKubectlPluginInvocation(t *testing.T) {
	saved := invokedAs
	invokedAs = "kubectl kubeagent"
	defer func() { invokedAs = saved }()

	err := Run(nil)
	if err == nil {
		t.Fatal("run(nil) = nil, want the usage error")
	}
	got := err.Error()
	// Every subcommand the usage lists must be named the way the user would
	// type it, not just the first one.
	for _, want := range []string{
		"usage: kubectl kubeagent scan",
		"| kubectl kubeagent watch",
		"| kubectl kubeagent mcp",
		"| kubectl kubeagent version",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("usage = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "usage: kubeagent scan") {
		t.Errorf("usage = %q, still tells a kubectl-plugin user to run the bare binary", got)
	}
}

func TestWarnf_NamesThePlainBinaryByDefault(t *testing.T) {
	saved := invokedAs
	invokedAs = "kubeagent"
	defer func() { invokedAs = saved }()

	var buf bytes.Buffer
	warnf(&buf, "metrics unavailable: %v", errors.New("boom"))

	want := "kubeagent: warning: metrics unavailable: boom\n"
	if got := buf.String(); got != want {
		t.Errorf("warnf wrote %q, want %q", got, want)
	}
}

func TestWarnf_NamesTheKubectlPluginInvocation(t *testing.T) {
	saved := invokedAs
	invokedAs = "kubectl kubeagent"
	defer func() { invokedAs = saved }()

	var buf bytes.Buffer
	warnf(&buf, "metrics unavailable: %v", errors.New("boom"))

	// A kubectl-plugin user who trips a warning must not be told the bare
	// binary's name — it is not on their PATH.
	want := "kubectl kubeagent: warning: metrics unavailable: boom\n"
	if got := buf.String(); got != want {
		t.Errorf("warnf wrote %q, want %q", got, want)
	}
}

func TestExitErrorCarriesItsCode(t *testing.T) {
	err := &exitError{code: 2, msg: "could not tell"}
	if err.Error() != "could not tell" {
		t.Errorf("Error() = %q, want \"could not tell\"", err.Error())
	}
	var ee *exitError
	if !errors.As(error(err), &ee) {
		t.Fatal("errors.As failed to unwrap an exitError")
	}
	if ee.code != 2 {
		t.Errorf("code = %d, want 2", ee.code)
	}
}

func TestExitCodeForNilIsZero(t *testing.T) {
	if got := exitCodeFor(nil); got != 0 {
		t.Errorf("exitCodeFor(nil) = %d, want 0", got)
	}
}

func TestExitCodeForPlainErrorIsOne(t *testing.T) {
	if got := exitCodeFor(errors.New("boom")); got != 1 {
		t.Errorf("exitCodeFor(plain error) = %d, want 1 — existing subcommands must not change behavior", got)
	}
}

func TestExitCodeForExitErrorIsItsCode(t *testing.T) {
	if got := exitCodeFor(&exitError{code: 3, msg: ""}); got != 3 {
		t.Errorf("exitCodeFor = %d, want 3", got)
	}
}

func TestGateRejectsUnknownFailOn(t *testing.T) {
	err := Run([]string{"gate", "--fail-on", "fatal"})
	if err == nil {
		t.Fatal("want an error for an unknown --fail-on level, got nil")
	}
	if got := exitCodeFor(err); got != 4 {
		t.Errorf("exit code = %d, want 4 (usage)", got)
	}
	if !strings.Contains(err.Error(), "fatal") {
		t.Errorf("error %q does not name the rejected value", err.Error())
	}
}

func TestGateRejectsUnknownOutputFormat(t *testing.T) {
	err := Run([]string{"gate", "--output", "yaml"})
	if err == nil {
		t.Fatal("want an error for an unknown --output format, got nil")
	}
	if got := exitCodeFor(err); got != 4 {
		t.Errorf("exit code = %d, want 4 (usage)", got)
	}
}

func TestGateRejectsUnknownFlag(t *testing.T) {
	err := Run([]string{"gate", "--nonexistent"})
	if err == nil {
		t.Fatal("want an error for an unknown flag, got nil")
	}
	if got := exitCodeFor(err); got != 4 {
		t.Errorf("exit code = %d, want 4 (usage)", got)
	}
}

func TestGateRejectsUnsupportedWaitForKind(t *testing.T) {
	err := Run([]string{"gate", "--wait-for", "pod/api", "-n", "prod"})
	if err == nil {
		t.Fatal("want an error for an unsupported --wait-for kind, got nil")
	}
	if got := exitCodeFor(err); got != 4 {
		t.Errorf("exit code = %d, want 4 (usage)", got)
	}
}

func TestGateRejectsWaitForWithoutNamespace(t *testing.T) {
	err := Run([]string{"gate", "--wait-for", "deployment/api"})
	if err == nil {
		t.Fatal("want an error when --wait-for has no namespace, got nil")
	}
	if got := exitCodeFor(err); got != 4 {
		t.Errorf("exit code = %d, want 4 (usage)", got)
	}
}

func TestUsageMentionsGate(t *testing.T) {
	err := Run([]string{"nonsense"})
	if err == nil {
		t.Fatal("want a usage error, got nil")
	}
	if !strings.Contains(err.Error(), "gate") {
		t.Errorf("usage text does not mention the gate subcommand: %s", err.Error())
	}
}

func TestGateScopeOptionsMapToTheRightFields(t *testing.T) {
	tgt, err := rolloutwait.ParseTarget("deployment/api", "prod")
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	v := gate.Decide(scan.Result{}, scopeTo(gate.Options{}, tgt))
	if v.Scope != "Deployment/api in prod" {
		t.Errorf("Scope = %q, want \"Deployment/api in prod\" — a swapped name/namespace would show here", v.Scope)
	}
}

func TestGateScanOptionsIncludeTheEnvTunableThresholds(t *testing.T) {
	// scan.Evaluate clamps an out-of-range or zero threshold back to its own
	// default, so asserting against that clamped value would pass whether or
	// not the env var ever reached scan.Options — the same trap the defaults
	// set for runGate. Using values that differ from both the zero value and
	// the documented default is the only way to catch that.
	t.Setenv("KUBEAGENT_QUOTA_THRESHOLD", "0.75")
	t.Setenv("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", "30")

	opts := gateScanOptions("prod")

	if opts.Namespace != "prod" {
		t.Errorf("Namespace = %q, want %q", opts.Namespace, "prod")
	}
	if opts.QuotaThreshold != 0.75 {
		t.Errorf("QuotaThreshold = %v, want 0.75 (from KUBEAGENT_QUOTA_THRESHOLD)", opts.QuotaThreshold)
	}
	if opts.WebhookTimeoutThreshold != 30 {
		t.Errorf("WebhookTimeoutThreshold = %v, want 30 (from KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS)", opts.WebhookTimeoutThreshold)
	}
}

func TestRunGateRejectsANonPositivePollInterval(t *testing.T) {
	for _, arg := range []string{"--poll-interval=0", "--poll-interval=-1s"} {
		err := runGate([]string{arg, "--kubeconfig", "/nonexistent-for-this-test"})
		if err == nil {
			t.Fatalf("%s: want a usage error, got nil", arg)
		}
		if got := exitCodeFor(err); got != gate.CodeUsage {
			t.Errorf("%s: exit code = %d, want %d (usage)", arg, got, gate.CodeUsage)
		}
		// The code alone cannot prove the ordering: an unreadable kubeconfig
		// also exits 4. Only the message distinguishes "rejected the flag" from
		// "tried to connect and failed".
		if !strings.Contains(err.Error(), "poll-interval") {
			t.Errorf("%s: error %q should name the flag — validation must run before cluster.NewClient",
				arg, err.Error())
		}
	}
}

func TestRunGateNeverExitsOneWithoutAVerdict(t *testing.T) {
	// An unusable kubeconfig is bad input, not a finding: nothing was attempted
	// against any cluster, so this must be usage (4) and never fail (1).
	err := runGate([]string{"--kubeconfig", "/nonexistent-for-this-test"})
	if err == nil {
		t.Fatal("want an error for an unreadable kubeconfig, got nil")
	}
	if got := exitCodeFor(err); got != gate.CodeUsage {
		t.Errorf("exit code = %d, want %d (usage); %d would claim kubeagent looked and found problems",
			got, gate.CodeUsage, gate.CodeFail)
	}
	// The error still names what failed — this is stderr, the operator's
	// channel, not the verdict document.
	if !strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("error %q should say what could not be loaded", err.Error())
	}
}

func TestScanAcceptsTheHTMLOutputFormat(t *testing.T) {
	// An unknown format is rejected before any cluster connection, so the error
	// text is reachable without a cluster. "html" must not be rejected there.
	err := Run([]string{"scan", "--output", "html", "--kubeconfig", filepath.Join(t.TempDir(), "nope")})
	if err != nil && strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("--output html was rejected as an unknown format: %v", err)
	}
}

func TestScanRejectsAnUnknownOutputFormatAndNamesHTML(t *testing.T) {
	err := Run([]string{"scan", "--output", "bogus"})
	if err == nil {
		t.Fatal("want an error for an unknown --output format, got nil")
	}
	if !strings.Contains(err.Error(), "want text, json or html") {
		t.Errorf("the rejection must name every accepted format, got: %v", err)
	}
}

func TestUsageMentionsTheHTMLOutputFormat(t *testing.T) {
	err := Run([]string{"bogus-subcommand"})
	if err == nil {
		t.Fatal("want a usage error, got nil")
	}
	if !strings.Contains(err.Error(), "--output text|json|html") {
		t.Errorf("usage must advertise the html format on scan, got: %v", err)
	}
}

// TestRenderScanRoutesHTMLWithEveryFieldPlumbed is the regression guard the
// helper exists for: it drives the exact call runScan makes, so a field that
// silently never reaches htmlreport.Input fails here rather than shipping.
func TestRenderScanRoutesHTMLWithEveryFieldPlumbed(t *testing.T) {
	res := scan.Result{
		PartialReads: []scan.ReadFailure{{Resource: "horizontalpodautoscalers", Reason: "forbidden"}},
		Inventory: inventory.Result{Workloads: []inventory.Workload{{
			Namespace: "shop", Kind: "Deployment", Name: "web", Desired: 1, Ready: 0,
			Status: "Degraded", Image: "busybox:1.36",
			Findings: []diagnose.Finding{{
				Pod: "shop/web", Issue: "CrashLoopBackOff", Reason: "container repeatedly crashes",
			}},
		}}},
	}
	in := resultInput(res)
	in.Now = time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)

	var buf bytes.Buffer
	if err := renderScan(&buf, "html", in, res, "shop"); err != nil {
		t.Fatalf("renderScan html: %v", err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "<!doctype html>") {
		t.Errorf("renderScan did not produce an HTML document, got: %.40q", got)
	}
	// Namespace reached htmlreport.Input.
	if !strings.Contains(got, "namespace shop") {
		t.Error("the -n value did not reach the document header")
	}
	// findings.Flatten reached htmlreport.Input.
	if !strings.Contains(got, "CrashLoopBackOff") {
		t.Error("the flattened findings did not reach the document")
	}
	// scan.Result.PartialReads reached htmlreport.Input.
	if !strings.Contains(got, "horizontalpodautoscalers") {
		t.Error("the partial reads did not reach the blind-spots block")
	}
	// report.Input reached htmlreport.Input.
	if !strings.Contains(got, "busybox:1.36") {
		t.Error("the report.Input workloads did not reach the inventory section")
	}
}

// TestRenderScanLeavesTextAndJSONOnTheOldPath: the new branch must not change
// what the two shipped formats emit.
func TestRenderScanLeavesTextAndJSONOnTheOldPath(t *testing.T) {
	res := scan.Result{}
	in := resultInput(res)
	in.Now = time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)

	for _, format := range []string{"text", "json"} {
		var viaHelper, viaReport bytes.Buffer
		if err := renderScan(&viaHelper, format, in, res, ""); err != nil {
			t.Fatalf("renderScan %s: %v", format, err)
		}
		if err := report.PrintInventory(in, format, &viaReport); err != nil {
			t.Fatalf("PrintInventory %s: %v", format, err)
		}
		if viaHelper.String() != viaReport.String() {
			t.Errorf("renderScan changed the %s output", format)
		}
	}
}

func TestRun_UsageMentionsTUI(t *testing.T) {
	err := Run([]string{})
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "tui") {
		t.Errorf("usage does not mention the tui subcommand: %q", err.Error())
	}
}

func TestRunTUI_RejectsUnknownFlag(t *testing.T) {
	err := Run([]string{"tui", "--bogus"})
	if err == nil {
		t.Fatal("no error for an unknown flag")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error does not name the bad flag: %q", err.Error())
	}
}

// --output is deliberately absent: a TUI seizes the terminal and is not
// redirectable, so it is not an output format.
func TestRunTUI_RejectsOutputFlag(t *testing.T) {
	err := Run([]string{"tui", "--output", "json"})
	if err == nil {
		t.Fatal("no error for --output")
	}
	if !strings.Contains(err.Error(), "output") {
		t.Errorf("error does not name the flag: %q", err.Error())
	}
}

// The TUI must never make an LLM call, so it must not accept the flags that
// would ask for one.
func TestRunTUI_RejectsExplainAndInvestigate(t *testing.T) {
	for _, flag := range []string{"--explain", "--investigate"} {
		if err := Run([]string{"tui", flag}); err == nil {
			t.Errorf("%s was accepted", flag)
		}
	}
}

// tuiScanOptions must produce the same defaults gateScanOptions does, so the
// TUI browses exactly what a bare scan reports.
//
// reflect.DeepEqual, not !=: scan.Options carries an ExpectedNodes []string,
// which makes the struct non-comparable with the operator the brief specifies.
// DeepEqual still compares the whole value, not a field or two.
func TestTUIScanOptions_MatchesGateDefaults(t *testing.T) {
	got := tuiScanOptions("shop")
	want := gateScanOptions("shop")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tuiScanOptions = %+v, want %+v", got, want)
	}
}

func TestSelectedRulesResolvesAProfile(t *testing.T) {
	rules, err := selectedRules("scan", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 10 {
		t.Fatalf("scan profile resolved to %d rules, want 10", len(rules))
	}
}

func TestSelectedRulesPrefersExplicitFeatures(t *testing.T) {
	rules, err := selectedRules("scan", "core, certs")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rules {
		for _, res := range r.Resources {
			if res == "secrets" {
				found = true
			}
		}
	}
	if !found {
		t.Error("--features core,certs did not include the secrets grant")
	}
}

func TestSelectedRulesRejectsAnUnknownProfile(t *testing.T) {
	if _, err := selectedRules("everything", ""); err == nil {
		t.Fatal("selectedRules accepted an unknown profile")
	}
}

func TestSelectedFeaturesResolvesAProfile(t *testing.T) {
	features, err := selectedFeatures("scan", "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range features {
		names = append(names, f.Name)
	}
	want := []string{"core"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("scan profile resolved to %v, want %v", names, want)
	}
}

func TestSelectedFeaturesPrefersExplicitFeatures(t *testing.T) {
	// The "scan" profile alone resolves to just ["core"]; naming --features
	// explicitly must win and bring in "certs" too.
	features, err := selectedFeatures("scan", "core, certs")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range features {
		names = append(names, f.Name)
	}
	want := []string{"core", "certs"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("selectedFeatures(\"scan\", \"core, certs\") = %v, want %v", names, want)
	}
}

func TestSelectedFeaturesRejectsAnUnknownProfile(t *testing.T) {
	if _, err := selectedFeatures("everything", ""); err == nil {
		t.Fatal("selectedFeatures accepted an unknown profile")
	}
}

func TestAdvisoryBlindSpotsNamesEachDegradedSubject(t *testing.T) {
	got := advisoryBlindSpots([]advisory.Degradation{
		{Sections: []string{"drift"}, Subject: "argoproj.io/applications", Reason: "forbidden"},
		{Sections: []string{"operators"}, Subject: "longhorn.io/volumes", Reason: "the server could not find the requested resource"},
	})
	if len(got) != 1 {
		t.Fatalf("got %d blind spots, want only the forbidden one: %+v", len(got), got)
	}
	if got[0].Resource != "argoproj.io/applications" {
		t.Errorf("Resource = %q", got[0].Resource)
	}
	if !strings.Contains(got[0].Reason, "forbidden") {
		t.Errorf("Reason = %q, want it to contain \"forbidden\"", got[0].Reason)
	}
}

func TestSelectedFeaturesRejectsAnUnknownFeature(t *testing.T) {
	if _, err := selectedFeatures("scan", "bogus"); err == nil {
		t.Fatal("selectedFeatures accepted an unknown feature name")
	}
}

func TestRunRBACPrintJSONIsAVersionedObject(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runRBACPrint([]string{"--output", "json"}); err != nil {
			t.Fatalf("runRBACPrint: %v", err)
		}
	})
	var doc rbacprofile.RulesDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not a RulesDocument: %v\n%s", err, out)
	}
	if doc.SchemaVersion != jsonschema.RBACVersion {
		t.Errorf("schemaVersion = %q, want %q", doc.SchemaVersion, jsonschema.RBACVersion)
	}
	if doc.RoleName != "kubeagent" {
		t.Errorf("roleName = %q, want the --role-name default", doc.RoleName)
	}
	if len(doc.Rules) == 0 {
		t.Error("rules is empty; the scan profile resolves to at least one rule")
	}
}

func TestRunSchema_ListsEveryDocument(t *testing.T) {
	var out bytes.Buffer
	if err := runSchema(nil, &out); err != nil {
		t.Fatalf("runSchema: %v", err)
	}
	for _, d := range schemadoc.Documents {
		if !strings.Contains(out.String(), d.Name) {
			t.Errorf("listing does not mention %q:\n%s", d.Name, out.String())
		}
	}
	if !strings.Contains(out.String(), invokedAs+" schema") {
		t.Errorf("listing does not show how to print one:\n%s", out.String())
	}
}

func TestRunSchema_PrintsAValidDocument(t *testing.T) {
	var out bytes.Buffer
	if err := runSchema([]string{"scan"}, &out); err != nil {
		t.Fatalf("runSchema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got, _ := doc["$id"].(string); !strings.HasSuffix(got, "scan-v1.json") {
		t.Errorf("$id = %q, want it to end in scan-v1.json", got)
	}
}

// repoPath resolves a repository-relative path from this package's directory.
// Mirrors internal/schemadoc/schemadoc_test.go's helper of the same name:
// internal/cli sits at the same depth below the repo root.
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join("..", "..", rel)
}

// What the binary prints must be what the binary's types are: the committed
// file and the runtime output come from one code path, and this proves it.
func TestRunSchema_MatchesTheCommittedFile(t *testing.T) {
	var out bytes.Buffer
	if err := runSchema([]string{"gate"}, &out); err != nil {
		t.Fatalf("runSchema: %v", err)
	}
	want, err := os.ReadFile(repoPath(t, filepath.Join("website", "docs", "schemas", "gate-v1.json")))
	if err != nil {
		t.Fatalf("read the committed file: %v", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Error("`schema gate` does not match website/docs/schemas/gate-v1.json")
	}
}

func TestRunSchema_UnknownNameNamesTheValidOnes(t *testing.T) {
	err := runSchema([]string{"nope"}, io.Discard)
	if err == nil {
		t.Fatal("want an error for an unknown document name")
	}
	if !strings.Contains(err.Error(), "scan") {
		t.Errorf("error %q does not name the valid documents", err)
	}
}

func TestRunSchema_RejectsExtraArguments(t *testing.T) {
	if err := runSchema([]string{"scan", "gate"}, io.Discard); err == nil {
		t.Fatal("want a usage error for two document names")
	}
}

// The command reads Go types: no kubeconfig, no cluster, no LLM call.
func TestRun_SchemaNeedsNoKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
	if err := Run([]string{"schema", "scan"}); err != nil {
		t.Errorf("schema must not need a cluster: %v", err)
	}
}

func TestRun_UsageMentionsSchemaCommand(t *testing.T) {
	err := Run(nil)
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Errorf("usage does not mention the schema command: %v", err)
	}
}

func TestParseScanFlagsCarriesEveryValue(t *testing.T) {
	opts, err := parseScanFlags([]string{
		"--kubeconfig", "/nonexistent/kubeconfig",
		"--context", "example-context",
		"--output", "json",
		"--namespace", "example-ns",
		"--disk-usage", "--disk-threshold", "0.42",
		"--cert-warn-days", "7",
		"--drift-age", "30m",
		"--node-heartbeat-threshold", "90s",
		"--expected-nodes", "node-a,node-b",
	})
	if err != nil {
		t.Fatalf("parseScanFlags: %v", err)
	}
	if opts.kubeconfig != "/nonexistent/kubeconfig" {
		t.Errorf("kubeconfig = %q, want /nonexistent/kubeconfig", opts.kubeconfig)
	}
	if opts.contextName != "example-context" {
		t.Errorf("contextName = %q, want example-context", opts.contextName)
	}
	if opts.output != "json" {
		t.Errorf("output = %q, want json", opts.output)
	}
	if opts.namespace != "example-ns" {
		t.Errorf("namespace = %q, want example-ns", opts.namespace)
	}
	if !opts.diskUsage {
		t.Error("diskUsage = false, want true")
	}
	if opts.diskThreshold != 0.42 {
		t.Errorf("diskThreshold = %v, want 0.42", opts.diskThreshold)
	}
	if opts.certWarnDays != 7 {
		t.Errorf("certWarnDays = %d, want 7", opts.certWarnDays)
	}
	if opts.driftAge != 30*time.Minute {
		t.Errorf("driftAge = %v, want 30m", opts.driftAge)
	}
	if opts.nodeHeartbeatThreshold != 90*time.Second {
		t.Errorf("nodeHeartbeatThreshold = %v, want 90s", opts.nodeHeartbeatThreshold)
	}
	if got := strings.Join(splitCSV(opts.expectedNodes), "|"); got != "node-a|node-b" {
		t.Errorf("expectedNodes = %q, want node-a|node-b", got)
	}
}

func TestParseScanFlagsDefaults(t *testing.T) {
	opts, err := parseScanFlags(nil)
	if err != nil {
		t.Fatalf("parseScanFlags: %v", err)
	}
	if opts.output != "text" {
		t.Errorf("output = %q, want text", opts.output)
	}
	if opts.diskThreshold != 0.80 {
		t.Errorf("diskThreshold = %v, want 0.80", opts.diskThreshold)
	}
	if opts.certWarnDays != 30 {
		t.Errorf("certWarnDays = %d, want 30", opts.certWarnDays)
	}
	if opts.driftAge != time.Hour {
		t.Errorf("driftAge = %v, want 1h", opts.driftAge)
	}
	if opts.nodeHeartbeatThreshold != 40*time.Second {
		t.Errorf("nodeHeartbeatThreshold = %v, want 40s", opts.nodeHeartbeatThreshold)
	}
}

func TestParseWatchFlagsCarriesEveryValue(t *testing.T) {
	// Fourteen of watch's flags default from the environment, reading the
	// thirteen keys below — --namespace and -n both read KUBEAGENT_NAMESPACE.
	// Clear them: a developer's shell must not decide whether this passes.
	for _, k := range []string{
		"KUBEAGENT_CLUSTER_NAME", "KUBEAGENT_INCLUDE_LOCAL", "KUBEAGENT_METRICS_ADDR",
		"KUBEAGENT_HEARTBEAT", "KUBEAGENT_DEBOUNCE", "KUBEAGENT_ALERT_FORMAT",
		"KUBEAGENT_ALERT_REPEAT", "KUBEAGENT_SLO_TARGET", "KUBEAGENT_NAMESPACE",
		"KUBEAGENT_EXPLAIN", "KUBEAGENT_EXPLAIN_COOLDOWN", "KUBEAGENT_EXPLAIN_BUDGET",
		"KUBEAGENT_DASHBOARD",
	} {
		t.Setenv(k, "")
	}
	o, err := parseWatchFlags([]string{
		"--context", "ctx-a", "--context", "ctx-b",
		"--cluster-name", "example-cluster",
		"--include-local",
		"--metrics-addr", "192.0.2.10:9090",
		"--heartbeat", "30s",
		"--debounce", "5s",
		"--alert-format", "slack",
		"--alert-repeat", "2h",
		"--slo-target", "99.9",
		"--explain-cooldown", "15m",
		"--explain-budget", "7",
		"--namespace", "example-ns",
		"--dashboard",
	})
	if err != nil {
		t.Fatalf("parseWatchFlags: %v", err)
	}
	if got := []string(o.contexts); !slices.Equal(got, []string{"ctx-a", "ctx-b"}) {
		t.Errorf("contexts = %v, want [ctx-a ctx-b]", got)
	}
	if o.clusterName != "example-cluster" {
		t.Errorf("clusterName = %q, want example-cluster", o.clusterName)
	}
	if !o.includeLocal {
		t.Error("includeLocal = false, want true")
	}
	if o.metricsAddr != "192.0.2.10:9090" {
		t.Errorf("metricsAddr = %q, want 192.0.2.10:9090", o.metricsAddr)
	}
	if o.heartbeat != 30*time.Second {
		t.Errorf("heartbeat = %v, want 30s", o.heartbeat)
	}
	if o.debounce != 5*time.Second {
		t.Errorf("debounce = %v, want 5s", o.debounce)
	}
	if o.alertFormat != "slack" {
		t.Errorf("alertFormat = %q, want slack", o.alertFormat)
	}
	if o.alertRepeat != 2*time.Hour {
		t.Errorf("alertRepeat = %v, want 2h", o.alertRepeat)
	}
	if o.sloTarget != 99.9 {
		t.Errorf("sloTarget = %v, want 99.9", o.sloTarget)
	}
	if o.explainCooldown != 15*time.Minute {
		t.Errorf("explainCooldown = %v, want 15m", o.explainCooldown)
	}
	if o.explainBudget != 7 {
		t.Errorf("explainBudget = %d, want 7", o.explainBudget)
	}
	if o.namespace != "example-ns" {
		t.Errorf("namespace = %q, want example-ns", o.namespace)
	}
	if !o.dashboard {
		t.Error("dashboard = false, want true")
	}
}

// TestWatchDashboardDefaultsFromEnvironment pins KUBEAGENT_DASHBOARD to the
// same envBool contract every other watch toggle uses.
func TestWatchDashboardDefaultsFromEnvironment(t *testing.T) {
	t.Setenv("KUBEAGENT_DASHBOARD", "true")
	o, err := parseWatchFlags(nil)
	if err != nil {
		t.Fatalf("parseWatchFlags: %v", err)
	}
	if !o.dashboard {
		t.Error("dashboard = false with KUBEAGENT_DASHBOARD=true")
	}

	t.Setenv("KUBEAGENT_DASHBOARD", "")
	o, err = parseWatchFlags(nil)
	if err != nil {
		t.Fatalf("parseWatchFlags: %v", err)
	}
	if o.dashboard {
		t.Error("dashboard = true with KUBEAGENT_DASHBOARD unset")
	}
}

// TestNormalizeAcceptsSingleDashDashboard keeps the single-dash long-flag shim
// covering the new flag. Command lines written against v0.72 and earlier use
// this spelling, and Normalize is why they keep working.
func TestNormalizeAcceptsSingleDashDashboard(t *testing.T) {
	t.Setenv("KUBEAGENT_DASHBOARD", "")
	o, err := parseWatchFlags([]string{"-dashboard"})
	if err != nil {
		t.Fatalf("parseWatchFlags: %v", err)
	}
	if !o.dashboard {
		t.Error("-dashboard did not set the flag")
	}
}

func TestParseWatchFlagsRejectsEmptyContext(t *testing.T) {
	_, err := parseWatchFlags([]string{"--context", ""})
	if err == nil {
		t.Fatal("parseWatchFlags([--context \"\"]) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "--context cannot be empty") {
		t.Errorf("error = %q, want it to contain %q", err, "--context cannot be empty")
	}
}

func TestParseGateFlagsCarriesEveryValue(t *testing.T) {
	o, err := parseGateFlags([]string{
		"--kubeconfig", "/nonexistent/kubeconfig",
		"--context", "example-context",
		"--output", "sarif",
		"--fail-on", "warning",
		"--wait-for", "deployment/example-api",
		"--timeout", "90s",
		"--poll-interval", "3s",
		"--allow-partial-read", "leases",
		"--allow-partial-read", "events",
		"--namespace", "example-ns",
	})
	if err != nil {
		t.Fatalf("parseGateFlags: %v", err)
	}
	if o.output != "sarif" {
		t.Errorf("output = %q, want sarif", o.output)
	}
	if o.failOn != "warning" {
		t.Errorf("failOn = %q, want warning", o.failOn)
	}
	if o.waitFor != "deployment/example-api" {
		t.Errorf("waitFor = %q, want deployment/example-api", o.waitFor)
	}
	if o.timeout != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", o.timeout)
	}
	if o.pollInterval != 3*time.Second {
		t.Errorf("pollInterval = %v, want 3s", o.pollInterval)
	}
	if got := []string(o.allowPartialRead); !slices.Equal(got, []string{"leases", "events"}) {
		t.Errorf("allowPartialRead = %v, want [leases events]", got)
	}
	if o.namespace != "example-ns" {
		t.Errorf("namespace = %q, want example-ns", o.namespace)
	}
}

func TestParseGateFlagsDefaults(t *testing.T) {
	o, err := parseGateFlags(nil)
	if err != nil {
		t.Fatalf("parseGateFlags: %v", err)
	}
	if o.output != "text" {
		t.Errorf("output = %q, want text", o.output)
	}
	if o.failOn != "critical" {
		t.Errorf("failOn = %q, want critical", o.failOn)
	}
	if o.timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", o.timeout)
	}
	if o.pollInterval != 2*time.Second {
		t.Errorf("pollInterval = %v, want 2s", o.pollInterval)
	}
}

func TestParseSmallCommandFlags(t *testing.T) {
	m, err := parseMCPFlags([]string{
		"--kubeconfig", "/nonexistent/kubeconfig",
		"--context", "example-context",
		"--allow-context-switch", "--logs",
	})
	if err != nil {
		t.Fatalf("parseMCPFlags: %v", err)
	}
	if !m.allowContextSwitch || !m.logs || m.contextName != "example-context" {
		t.Errorf("mcpOptions = %+v, want all three set", m)
	}

	u, err := parseTUIFlags([]string{"-n", "example-ns", "--context", "example-context"})
	if err != nil {
		t.Fatalf("parseTUIFlags: %v", err)
	}
	if u.namespace != "example-ns" || u.contextName != "example-context" {
		t.Errorf("tuiOptions = %+v, want namespace and context set", u)
	}

	p, err := parseRBACPrintFlags([]string{
		"--profile", "watch", "--features", "core,certs",
		"--role-name", "example-role", "--output", "json",
	})
	if err != nil {
		t.Fatalf("parseRBACPrintFlags: %v", err)
	}
	if p.profile != "watch" || p.features != "core,certs" || p.roleName != "example-role" || p.output != "json" {
		t.Errorf("rbacPrintOptions = %+v, want all four set", p)
	}

	c, err := parseRBACCheckFlags([]string{"--profile", "scan", "--output", "json"})
	if err != nil {
		t.Fatalf("parseRBACCheckFlags: %v", err)
	}
	if c.profile != "scan" || c.output != "json" {
		t.Errorf("rbacCheckOptions = %+v, want profile scan and output json", c)
	}
}

func TestSmallCommandFlagDefaults(t *testing.T) {
	p, err := parseRBACPrintFlags(nil)
	if err != nil {
		t.Fatalf("parseRBACPrintFlags: %v", err)
	}
	if p.profile != "scan" || p.roleName != "kubeagent" || p.output != "yaml" {
		t.Errorf("rbac print defaults = %+v, want profile scan, role-name kubeagent, output yaml", p)
	}
	c, err := parseRBACCheckFlags(nil)
	if err != nil {
		t.Fatalf("parseRBACCheckFlags: %v", err)
	}
	if c.profile != "full" || c.output != "text" {
		t.Errorf("rbac check defaults = %+v, want profile full, output text", c)
	}
}

func TestRootCommandTreeIsSilent(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if !c.SilenceErrors {
			t.Errorf("%s: SilenceErrors = false, want true", c.CommandPath())
		}
		if !c.SilenceUsage {
			t.Errorf("%s: SilenceUsage = false, want true", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRootCommand())
}

func TestRootCommandNamesTheInvokedSpelling(t *testing.T) {
	old := invokedAs
	invokedAs = "kubectl kubeagent"
	defer func() { invokedAs = old }()

	root := newRootCommand()
	if got := root.CommandPath(); got != "kubectl kubeagent" {
		t.Errorf("root.CommandPath() = %q, want %q", got, "kubectl kubeagent")
	}
	for _, sub := range root.Commands() {
		if sub.Name() != "mcp" {
			continue
		}
		if got := sub.CommandPath(); got != "kubectl kubeagent mcp" {
			t.Errorf("mcp.CommandPath() = %q, want %q", got, "kubectl kubeagent mcp")
		}
	}
}

// TestSubcommandHelpExitsZero pins --help's exit status for every command:
// version, schema, mcp, tui, scan, watch, gate and rbac. Under Cobra, --help
// is a normal registered bool flag; Command.execute checks it, ExecuteC
// intercepts the resulting flag.ErrHelp, writes real help to stdout, and
// returns nil, which exitCodeFor maps to 0.
//
// gate is the notable change: before its migration, runGate wrapped every
// parse error, including flag.ErrHelp, in &exitError{code: gate.CodeUsage},
// so `gate --help` exited 4, not 0. Cobra's own --help handling now runs
// before newGateCommand's RunE (and before its SetFlagErrorFunc, which only
// sees flag *errors*, not the help request), so `gate --help` exits 0 like
// every other command — a human running it by hand sees help and a clean
// exit; CI never runs it with --help.
func TestSubcommandHelpExitsZero(t *testing.T) {
	for _, args := range [][]string{{"version", "--help"}, {"schema", "--help"}, {"mcp", "--help"}, {"tui", "--help"}, {"scan", "--help"}, {"watch", "--help"}, {"gate", "--help"}, {"rbac", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			err := Run(args)
			if err != nil {
				t.Errorf("Run(%v) = %v, want nil", args, err)
			}
			if got := exitCodeFor(err); got != 0 {
				t.Errorf("exit code = %d, want 0", got)
			}
		})
	}
}

// TestRootHelpKeepsTheUsageError pins the one help path that is NOT exit 0.
// `kubeagent --help` has always fallen through to the usage error — stderr,
// exit 1 — because the standard-library dispatch only recognised a
// subcommand as the first argument. That is arguably a wart, but exit codes
// are frozen for this migration, so it stays. Fix it in its own change,
// where it is visible as its own diff.
func TestRootHelpKeepsTheUsageError(t *testing.T) {
	err := Run([]string{"--help"})
	if err == nil {
		t.Fatal("Run([--help]) = nil, want the usage error")
	}
	if !strings.Contains(err.Error(), "usage: ") {
		t.Errorf("error = %q, want the usage error", err)
	}
	if got := exitCodeFor(err); got != 1 {
		t.Errorf("exit code = %d, want 1", got)
	}
}
