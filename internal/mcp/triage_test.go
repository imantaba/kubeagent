package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func callTriage(t *testing.T, cs *mcpsdk.ClientSession, args map[string]any) TriageOutput {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "kubeagent_triage", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	var out TriageOutput
	blob, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("Marshal(structured) error = %v", err)
	}
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return out
}

func crashingPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "api-abc"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "api",
				RestartCount: 7,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff", Message: "back-off restarting failed container",
				}},
			}},
		},
	}
}

func TestTriage_HealthyClusterIsExplicitlyHealthy(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callTriage(t, cs, map[string]any{})

	if out.Verdict != "healthy" {
		t.Errorf("Verdict = %q, want %q", out.Verdict, "healthy")
	}
	if out.Findings == nil {
		t.Error("Findings is null; an empty finding list must marshal as [] so a caller can tell it apart from absent")
	}
	if out.Coverage == nil {
		t.Fatal("Coverage is nil; every result carries the honesty contract")
	}
	if out.Coverage.Context != "kind-example" {
		t.Errorf("coverage.context = %q, want %q", out.Coverage.Context, "kind-example")
	}
	if len(out.Coverage.ChecksRun) == 0 {
		t.Error("coverage.checksRun is empty; a healthy verdict with no declared checks is unfalsifiable")
	}
	if out.Coverage.MetricsServer != "not-checked" {
		t.Errorf("coverage.metricsServer = %q, want %q — triage never queries metrics",
			out.Coverage.MetricsServer, "not-checked")
	}
}

func TestTriage_CrashLoopIsACriticalFinding(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(crashingPod()))

	out := callTriage(t, cs, map[string]any{})

	if out.Verdict != "degraded" {
		t.Fatalf("Verdict = %q, want %q", out.Verdict, "degraded")
	}
	if len(out.Findings) == 0 {
		t.Fatal("Findings is empty on a crash-looping pod")
	}
	f := out.Findings[0]
	if f.Severity != "critical" || f.Reason != "CrashLoopBackOff" {
		t.Errorf("Findings[0] = %+v, want a critical CrashLoopBackOff", f)
	}
	if f.RemediationHint == "" {
		t.Error("RemediationHint is empty; the caller gets the deterministic next step, not an invented one")
	}
}

func TestTriage_NamespaceArgumentIsReflectedInCoverage(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callTriage(t, cs, map[string]any{"namespace": "payments"})

	if out.Coverage.NamespaceScope != "payments" {
		t.Errorf("coverage.namespaceScope = %q, want %q", out.Coverage.NamespaceScope, "payments")
	}
}

func TestTriage_ContextArgumentIsRejectedUnlessSwitchingIsAllowed(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_triage", Arguments: map[string]any{"context": "kind-other"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded; a server started without --allow-context-switch must not be " +
			"talked into another cluster")
	}
}

func TestTriage_SkippedChecksAreDeclaredNotSilent(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callTriage(t, cs, map[string]any{})

	declared := map[string]bool{}
	for _, s := range out.Coverage.ChecksSkipped {
		declared[s.Check] = true
		if s.Why == "" {
			t.Errorf("checksSkipped entry %q has an empty reason", s.Check)
		}
	}
	for _, want := range []string{"credential-lint", "disk-usage", "security", "certificates"} {
		if !declared[want] {
			t.Errorf("coverage.checksSkipped does not mention %q; the CLI reports it and triage does not, "+
				"so its absence must be stated rather than implied clean", want)
		}
	}
}
