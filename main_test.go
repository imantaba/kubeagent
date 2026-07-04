package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

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
	// --explain without a key must fail fast, before any cluster connection.
	t.Setenv("ANTHROPIC_API_KEY", "")
	err := run([]string{"scan", "--explain"})
	if err == nil {
		t.Fatal("expected an error when --explain is set without ANTHROPIC_API_KEY")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected error to mention ANTHROPIC_API_KEY, got: %v", err)
	}
}

func TestRun_ModelFlagIsRecognized(t *testing.T) {
	// --model must be a known flag: with it set and no API key, the error is
	// the fail-fast key error, NOT "flag provided but not defined".
	t.Setenv("ANTHROPIC_API_KEY", "")
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

func TestRunFixes_DryRunWritesNothing(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:bad"}}}}
	cli := fake.NewSimpleClientset(d)
	var out bytes.Buffer
	runFixes(context.Background(), cli, fixWorkload(), fixRS(), nil, true /*dryRun*/, false, &out, strings.NewReader(""))
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
	var out bytes.Buffer
	runFixes(context.Background(), cli, fixWorkload(), rss, nil, false, true /*assumeYes*/, &out, strings.NewReader(""))
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
	runFixes(context.Background(), cli, nil, nil, []corev1.Node{*n}, true /*dryRun*/, false, &out, strings.NewReader(""))
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
	var out bytes.Buffer
	runFixes(context.Background(), cli, nil, nil, []corev1.Node{*n}, false, true, &out, strings.NewReader(""))
	got, _ := cli.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if got.Spec.Unschedulable {
		t.Errorf("expected node uncordoned by --yes")
	}
	if !strings.Contains(out.String(), "node/worker-1") {
		t.Errorf("expected the node target in output, got: %s", out.String())
	}
}
