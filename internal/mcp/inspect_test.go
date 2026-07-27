package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func p32(n int32) *int32 { return &n }

func callInspect(t *testing.T, cs *mcpsdk.ClientSession, args map[string]any) InspectOutput {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "kubeagent_inspect", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	var out InspectOutput
	blob, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return out
}

func TestInspect_PodReturnsItsFindingsAndEvents(t *testing.T) {
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "payments", Name: "e1"},
		InvolvedObject: corev1.ObjectReference{Namespace: "payments", Name: "api-abc"},
		Reason:         "BackOff", Message: "back-off restarting failed container", Type: "Warning", Count: 5,
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(crashingPod(), event))

	out := callInspect(t, cs, map[string]any{"kind": "pod", "namespace": "payments", "name": "api-abc"})

	if !out.Found {
		t.Fatal("Found = false, want the pod to be found")
	}
	if len(out.Findings) == 0 {
		t.Error("Findings is empty on a crash-looping pod")
	}
	if len(out.Events) == 0 {
		t.Fatal("Events is empty; inspect exists to surface the events triage summarises away")
	}
	if out.Events[0].Reason != "BackOff" || out.Events[0].Count != 5 {
		t.Errorf("Events[0] = %+v, want the BackOff event with count 5", out.Events[0])
	}
	if out.Events[0].Age == "" {
		t.Error("Events[0].Age is empty; a model cannot judge relevance without it")
	}
}

func TestInspect_MissingObjectIsNotFoundNotAnError(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callInspect(t, cs, map[string]any{"kind": "deployment", "namespace": "payments", "name": "ghost"})

	if out.Found {
		t.Error("Found = true for an object that does not exist")
	}
	if out.Findings == nil || out.Events == nil || out.Pods == nil {
		t.Error("a not-found result must still carry empty lists, so absent and empty stay distinguishable")
	}
	if out.Coverage == nil {
		t.Error("Coverage is nil; every result carries the honesty contract")
	}
}

func TestInspect_UnknownKindIsRejectedBeforeTheHandlerRuns(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "kubeagent_inspect",
		Arguments: map[string]any{"kind": "secret", "namespace": "payments", "name": "creds"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded for kind=secret; the published schema's enum must reject it")
	}
}

func TestInspect_MissingRequiredArgumentIsRejected(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_inspect", Arguments: map[string]any{"kind": "pod"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded without a name; name is required")
	}
	text := firstText(res)
	if !strings.Contains(text, "name") {
		t.Errorf("error text = %q, want it to name the missing property", text)
	}
}

// firstText returns the first text block of a tool result, for asserting on
// SDK-generated validation messages.
func firstText(res *mcpsdk.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// TestInspect_FlaggedWorkloadWithoutPerPodFindingStillReportsAFinding covers a
// Deployment stuck at 0/3 ready with no crash-looping pod behind it (no pods
// at all, in this fixture): the workload has no per-pod diagnose.Finding, but
// it is Flagged() (Ready < Desired). Mirroring view.go's findingsFromResult,
// inspect must still project it through fromWorkload rather than reporting
// "findings: []" for an object triage would flag.
func TestInspect_FlaggedWorkloadWithoutPerPodFindingStillReportsAFinding(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "worker"},
		Spec:       appsv1.DeploymentSpec{Replicas: p32(3)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 0},
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(dep))

	out := callInspect(t, cs, map[string]any{"kind": "deployment", "namespace": "payments", "name": "worker"})

	if !out.Found {
		t.Fatal("Found = false, want the deployment to be found")
	}
	if out.Ready != 0 || out.Desired != 3 {
		t.Fatalf("Ready/Desired = %d/%d, want 0/3", out.Ready, out.Desired)
	}
	if len(out.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one workload-level finding for a flagged "+
			"deployment with no per-pod finding", out.Findings)
	}
	f := out.Findings[0]
	if f.Severity != "warning" || f.Kind != "Deployment" || f.Namespace != "payments" || f.Name != "worker" {
		t.Errorf("Findings[0] = %+v, want a warning on Deployment payments/worker", f)
	}
	if f.Reason != out.Status {
		t.Errorf("Findings[0].Reason = %q, want the workload's status %q", f.Reason, out.Status)
	}
}

// TestInspect_EventsAreSortedMostRecentFirstWithATotalTiebreak feeds events to
// the fake clientset out of chronological order, including a same-timestamp
// pair, and asserts the returned order is most-recent-first with ties broken
// on Reason (then Message, then Count) rather than left to list order.
func TestInspect_EventsAreSortedMostRecentFirstWithATotalTiebreak(t *testing.T) {
	newest := fixedNow.Add(-1 * time.Minute)
	oldest := fixedNow.Add(-30 * time.Minute)
	tied := fixedNow.Add(-10 * time.Minute)

	mk := func(name, reason, message string, count int32, ts time.Time) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "ops", Name: name},
			InvolvedObject: corev1.ObjectReference{Namespace: "ops", Name: "worker"},
			Reason:         reason, Message: message, Type: "Warning", Count: count,
			LastTimestamp: metav1.NewTime(ts),
		}
	}

	oldE := mk("e-old", "Oldest", "z-message", 1, oldest)
	newE := mk("e-new", "Newest", "a-message", 9, newest)
	tiedB := mk("e-tied-b", "Bravo", "second", 2, tied)
	tiedA := mk("e-tied-a", "Alpha", "first", 5, tied)

	cs := connect(t, Config{Context: "kind-example"},
		fake.NewSimpleClientset(oldE, tiedB, newE, tiedA))

	out := callInspect(t, cs, map[string]any{"kind": "pod", "namespace": "ops", "name": "worker"})

	if len(out.Events) != 4 {
		t.Fatalf("Events = %+v, want 4", out.Events)
	}
	want := []string{"Newest", "Alpha", "Bravo", "Oldest"}
	for i, w := range want {
		if out.Events[i].Reason != w {
			t.Errorf("Events[%d].Reason = %q, want %q (full order: %+v)", i, out.Events[i].Reason, w, out.Events)
		}
	}
}

// TestInspect_EventWithNoTimestampsHasUnknownAge covers an event that carries
// none of LastTimestamp, EventTime or FirstTimestamp. inventory.HumanAge would
// render the zero time.Time as a multi-century age; Age must instead be the
// literal "unknown", never a computed (and nonsensical) duration.
func TestInspect_EventWithNoTimestampsHasUnknownAge(t *testing.T) {
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "ops", Name: "e-no-ts"},
		InvolvedObject: corev1.ObjectReference{Namespace: "ops", Name: "worker"},
		Reason:         "Scheduled", Message: "no timestamps at all", Type: "Normal", Count: 1,
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(event))

	out := callInspect(t, cs, map[string]any{"kind": "pod", "namespace": "ops", "name": "worker"})

	if len(out.Events) != 1 {
		t.Fatalf("Events = %+v, want 1", out.Events)
	}
	if out.Events[0].Age != "unknown" {
		t.Errorf("Age = %q, want %q for an event carrying none of the three timestamps",
			out.Events[0].Age, "unknown")
	}
}

// TestInspect_EmptyNamespaceIsRejectedBeforeTheHandlerRuns asserts that
// namespace: "" fails SDK schema validation (a validation failure is a
// CallToolResult{IsError: true}, not a JSON-RPC error — only an unknown tool
// name is that) and, via a reactor on the fake clientset, that the handler
// never runs: an empty namespace can never match a real workload, so letting
// it through would only buy a pointless cluster-wide scan.
func TestInspect_EmptyNamespaceIsRejectedBeforeTheHandlerRuns(t *testing.T) {
	cli := fake.NewSimpleClientset()
	called := false
	cli.PrependReactor("*", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		called = true
		return false, nil, nil
	})
	cs := connect(t, Config{Context: "kind-example"}, cli)

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "kubeagent_inspect",
		Arguments: map[string]any{"kind": "pod", "namespace": "", "name": "api-abc"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded with namespace=\"\"; MinLength must reject an empty namespace " +
			"before the handler runs")
	}
	if called {
		t.Error("the fake clientset was called; validation must reject namespace=\"\" before the " +
			"handler ever runs")
	}
}
