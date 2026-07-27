package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/audit"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/hpahealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/pdbhealth"
	"github.com/imantaba/kubeagent/internal/quotahealth"
	"github.com/imantaba/kubeagent/internal/remediate"
	"github.com/imantaba/kubeagent/internal/scan"
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
	if err := run(nil); err == nil {
		t.Fatal("expected a usage error with no args")
	}
}

func TestRun_RejectsUnknownSubcommand(t *testing.T) {
	if err := run([]string{"explode"}); err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
}

func TestRun_RejectsBadOutputFormat(t *testing.T) {
	// This must fail on validation BEFORE any cluster connection is attempted.
	if err := run([]string{"scan", "--output", "bogus"}); err == nil {
		t.Fatal("expected an error for a bad --output value")
	}
}

func TestRun_ExplainRequiresAPIKey(t *testing.T) {
	// --explain without a key (and without a local endpoint) must fail fast, before any cluster connection.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := run([]string{"scan", "--explain"})
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
	err := run([]string{"scan", "--explain"})
	if err == nil || !strings.Contains(err.Error(), "KUBEAGENT_EXPLAIN_ENDPOINT") {
		t.Fatalf("want the key-or-endpoint error, got %v", err)
	}
}

func TestRun_ExplainLocalNeedsModel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "http://localhost:11434/v1")
	t.Setenv("KUBEAGENT_MODEL", "")
	err := run([]string{"scan", "--explain"})
	if err == nil || !strings.Contains(err.Error(), "needs --model") {
		t.Fatalf("want the needs-model error, got %v", err)
	}
}

func TestRun_ModelFlagIsRecognized(t *testing.T) {
	// --model must be a known flag: with it set and no API key, the error is
	// the fail-fast key error, NOT "flag provided but not defined".
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := run([]string{"scan", "--explain", "--model", "claude-sonnet-4-6"})
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
	err := run([]string{"scan", "--explain", "--include-cron", "--include-restarts"})
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
	if err := run([]string{"version"}); err != nil {
		t.Errorf("run([version]) returned error: %v", err)
	}
}

func TestRun_LintSecretsFlagAccepted(t *testing.T) {
	// --lint-secrets must be a defined flag: this fails on output-format
	// validation (which happens before any cluster connection), proving the flag
	// parsed rather than erroring with "flag provided but not defined".
	err := run([]string{"scan", "--lint-secrets", "--output", "bogus"})
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
	err := run([]string{"scan", "--kubeconfig", kc})
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
	err := run([]string{"scan", "--kubeconfig", kc})
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
	err := run([]string{"scan", "--fix", "--dry-run", "--yes", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("expected output-format error (flags accepted), got: %v", err)
	}
}

func TestRun_SuggestFlagAccepted(t *testing.T) {
	// --suggest must be a defined flag: this fails on output-format validation
	// (before any cluster call), proving the flag parsed.
	err := run([]string{"scan", "--suggest", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("expected the output-format error (flag accepted), got: %v", err)
	}
}

func TestRun_ControlPlaneHealthFlagAccepted(t *testing.T) {
	err := run([]string{"scan", "--control-plane-health", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flag parsed), got %v", err)
	}
}

func TestRun_DNSHealthFlagAccepted(t *testing.T) {
	err := run([]string{"scan", "--dns-health", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flag parsed), got %v", err)
	}
}

func TestRun_OperatorsFlagAccepted(t *testing.T) {
	// --operators must be a defined flag: this fails on output-format validation
	// (before any cluster or discovery call), proving the flag parsed rather
	// than erroring with "flag provided but not defined".
	err := run([]string{"scan", "--operators", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flag parsed), got %v", err)
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

func TestRunWatch_AlertFlagsAreRecognized(t *testing.T) {
	// --alert-format/--alert-repeat must be defined flags: with a kubeconfig path
	// that fails to load, the error must be the kubeconfig-load error, not "flag
	// provided but not defined", proving the flags parsed.
	dir := t.TempDir()
	bad := filepath.Join(dir, "nonexistent")
	err := runWatch([]string{"--alert-format", "slack", "--alert-repeat", "10m", "--kubeconfig", bad})
	if err == nil {
		t.Fatal("expected a kubeconfig load error")
	}
	if strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("expected --alert-format/--alert-repeat to be recognized flags, got: %v", err)
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
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
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
	if !strings.Contains(stderr, "KUBEAGENT_ALERT_WEBHOOK is not set") {
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
	if !strings.Contains(stderr, "KUBEAGENT_ALERT_WEBHOOK is not set") {
		t.Fatalf("expected the ignored-alert-flags warning on stderr, got: %q", stderr)
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

func TestRun_UsageMentionsWatchAlertFlags(t *testing.T) {
	err := run(nil)
	if err == nil {
		t.Fatal("expected a usage error with no args")
	}
	if !strings.Contains(err.Error(), "[--alert-format json|slack|alertmanager] [--alert-repeat dur]") {
		t.Fatalf("expected the usage string to mention --alert-format and --alert-repeat, got: %v", err)
	}
}

func TestRun_UsageMentionsOperatorsFlag(t *testing.T) {
	err := run(nil)
	if err == nil {
		t.Fatal("expected a usage error with no args")
	}
	if !strings.Contains(err.Error(), "[--certs [--cert-warn-days n]] [--operators] [--logs]") {
		t.Fatalf("expected the usage string to mention --operators between --certs and --logs, got: %v", err)
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
	// error occurred.
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
	// --help is the only way to observe a flag.Float64 default without either
	// starting the daemon (which requires a live target that has since fallen
	// through validation) or reaching into the flag.FlagSet's internals. Go's
	// flag.PrintDefaults appends a literal " (default X)" suffix to a flag's
	// usage line only when the default is NOT the type's zero value; at the
	// zero value it prints just the usage text. So the trailing "\n" right
	// after the usage string is exactly what discriminates "off by default"
	// from "on by default": with a non-zero default (e.g. 50), this same line
	// would instead read '...(0 = SLO tracking off) (default 50)\n'. A future
	// maintainer must not loosen this to a substring/Contains check without
	// the terminator — this branch has already shipped three prefix-matching
	// assertions that passed for the wrong reason.
	t.Setenv("KUBEAGENT_SLO_TARGET", "")
	stderr := captureStderr(t, func() {
		err := runWatch([]string{"--help"})
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("runWatch([--help]) error = %v, want flag.ErrHelp", err)
		}
	})
	if !strings.Contains(stderr, "-slo-target float") {
		t.Fatalf("expected --help output to describe -slo-target as a float flag, got: %q", stderr)
	}
	want := "availability SLO as a percentage, e.g. 99.9 (0 = SLO tracking off)\n"
	if !strings.Contains(stderr, want) {
		t.Fatalf("expected --help output to show --slo-target defaulting to off (no non-zero default suffix), got: %q", stderr)
	}
}

func TestRun_UsageMentionsSLOTarget(t *testing.T) {
	err := run(nil)
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
	err := run([]string{"scan", "--investigate"})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("expected an ANTHROPIC_API_KEY error, got %v", err)
	}
}

func TestRun_InvestigateRejectsLocalOnlyEndpoint(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "http://localhost:11434/v1")
	err := run([]string{"scan", "--investigate"})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("investigate must require an Anthropic key even when a local endpoint is set, got %v", err)
	}
}

func TestRun_InvestigateSupersedesExplain(t *testing.T) {
	// Passing both --investigate and --explain must not produce a flag-parse error
	// or a precondition error — --investigate supersedes --explain silently. The
	// scan fails at cluster-connect (bogus kubeconfig), which is the expected
	// outcome: it proves flags parsed and the investigate branch was selected.
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := run([]string{"scan", "--investigate", "--explain", "--kubeconfig", "/nonexistent/path"})
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
	err := run([]string{"scan", "--rollback"})
	if err == nil || !strings.Contains(err.Error(), "--audit-log") {
		t.Errorf("expected an --audit-log requirement error, got %v", err)
	}
}

func TestRun_RollbackAndFixAreExclusive(t *testing.T) {
	err := run([]string{"scan", "--rollback", "--fix", "--audit-log", "/tmp/x.log"})
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
	err := run(nil)
	if err == nil {
		t.Fatal("want the usage error")
	}
	for _, want := range []string{"--explain-cooldown", "--explain-budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("usage does not mention %s", want)
		}
	}
}

// TestContextListCollectsRepeats pins the flag type: --context is repeatable,
// and each occurrence names one cluster to watch.
func TestContextListCollectsRepeats(t *testing.T) {
	var got contextList
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(&got, "context", "")
	if err := fs.Parse([]string{"--context", "a", "--context", "b"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("contextList = %v, want [a b]", got)
	}
	if err := fs.Parse([]string{"--context", ""}); err == nil {
		t.Error("an empty --context must be rejected")
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
