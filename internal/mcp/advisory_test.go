package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// restRouted overrides CoreV1Interface.RESTClient with a working client so
// collect.PodMetrics' raw GET can actually run. fake.NewSimpleClientset's own
// RESTClient() is always a nil *rest.RESTClient (client-go's fake package only
// intercepts typed List/Get/Create calls, never raw REST requests), so calling
// it panics instead of returning the "metrics-server absent" answer
// collect.PodMetrics is documented to give — see the identical workaround in
// internal/advisory/advisory_test.go (fakeClientWithNoMetricsServer). Routing
// RESTClient() at a real httptest server that 404s reproduces the real
// "absent" behavior without touching collect.go or advisory.go.
type restRouted struct {
	corev1client.CoreV1Interface
	rc rest.Interface
}

func (r restRouted) RESTClient() rest.Interface { return r.rc }

type clientWithMetricsRoute struct {
	*fake.Clientset
	core corev1client.CoreV1Interface
}

func (c clientWithMetricsRoute) CoreV1() corev1client.CoreV1Interface { return c.core }

// fakeClientWithNoMetricsServer builds a fake.Clientset whose RESTClient()
// works but 404s, so collect.PodMetrics observes an absent metrics-server the
// same way it would against a real cluster that never installed one. Every
// kubeagent_advisory test that requests the capacity section needs this
// instead of a plain fake.NewSimpleClientset(), or the capacity assessor's
// metrics probe panics instead of degrading.
func fakeClientWithNoMetricsServer(t *testing.T) kubernetes.Interface {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	rc, err := rest.RESTClientFor(&rest.Config{
		Host: srv.URL,
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &corev1.SchemeGroupVersion,
			NegotiatedSerializer: scheme.Codecs,
		},
	})
	if err != nil {
		t.Fatalf("building test REST client: %v", err)
	}

	base := fake.NewSimpleClientset()
	return clientWithMetricsRoute{Clientset: base, core: restRouted{CoreV1Interface: base.CoreV1(), rc: rc}}
}

func callAdvisory(t *testing.T, cs *mcpsdk.ClientSession, args map[string]any) AdvisoryOutput {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "kubeagent_advisory", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	var out AdvisoryOutput
	blob, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return out
}

func TestAdvisory_OnlyTheRequestedSectionsAppear(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fakeClientWithNoMetricsServer(t))

	out := callAdvisory(t, cs, map[string]any{"sections": []any{"capacity"}})

	if len(out.Requested) != 1 || out.Requested[0] != "capacity" {
		t.Errorf("Requested = %v, want [capacity]", out.Requested)
	}
	if _, ok := out.Sections["capacity"]; !ok {
		t.Errorf("Sections = %v, want a capacity entry", out.Sections)
	}
	if _, ok := out.Sections["security"]; ok {
		t.Error("Sections contains security, which was not requested")
	}
}

func TestAdvisory_RequestedSectionsAreDedupedAndSorted(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fakeClientWithNoMetricsServer(t))

	out := callAdvisory(t, cs, map[string]any{"sections": []any{"security", "capacity", "security"}})

	want := []string{"capacity", "security"}
	if len(out.Requested) != len(want) {
		t.Fatalf("Requested = %v, want %v", out.Requested, want)
	}
	for i := range want {
		if out.Requested[i] != want[i] {
			t.Errorf("Requested = %v, want %v", out.Requested, want)
		}
	}
}

func TestAdvisory_SectionsMapIsNeverNull(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_advisory", Arguments: map[string]any{"sections": []any{}},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	blob, _ := json.Marshal(res.StructuredContent)
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if raw["sections"] == nil {
		t.Error("sections is null; an empty section map must marshal as {} so absent and empty stay distinct")
	}
}

func TestAdvisory_UnknownSectionIsRejectedBeforeTheHandlerRuns(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_advisory", Arguments: map[string]any{"sections": []any{"fix"}},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() accepted section \"fix\"; the enum must reject anything outside the five sections")
	}
}

func TestAdvisory_CapacityWithoutMetricsReportsMetricsServerAbsent(t *testing.T) {
	// The fake clientset serves no metrics API. The capacity report is still
	// produced, from requests and limits, so the only honest signal that the
	// headroom is not usage-backed is coverage.metricsServer.
	cs := connect(t, Config{Context: "kind-example"}, fakeClientWithNoMetricsServer(t))

	out := callAdvisory(t, cs, map[string]any{"sections": []any{"capacity"}})

	if out.Coverage == nil {
		t.Fatal("Coverage is nil")
	}
	if out.Coverage.MetricsServer != "absent" {
		t.Errorf("coverage.metricsServer = %q, want %q", out.Coverage.MetricsServer, "absent")
	}
}

func TestAdvisory_MetricsServerStaysNotCheckedWhenCapacityIsNotRequested(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callAdvisory(t, cs, map[string]any{"sections": []any{"security"}})

	if out.Coverage.MetricsServer != "not-checked" {
		t.Errorf("coverage.metricsServer = %q, want %q — nothing queried metrics, so claiming "+
			"\"absent\" would assert an untested fact", out.Coverage.MetricsServer, "not-checked")
	}
}
