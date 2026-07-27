package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

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
