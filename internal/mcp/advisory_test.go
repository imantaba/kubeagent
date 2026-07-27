package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
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

// TestAdvisory_DynamicClientFailureNeverLeaksKubeconfigPath drives the real
// handler with a Config whose Kubeconfig points nowhere, so
// cluster.NewDynamicClients fails the way it would against a typo'd or
// unreadable kubeconfig. internal/cluster's restConfig wraps that failure
// with the literal kubeconfig path and context name; a kubeconfig path names
// a customer, a cluster and an environment, so it must never reach a
// *successful* tool result via Coverage's Partial/ChecksSkipped reasons.
func TestAdvisory_DynamicClientFailureNeverLeaksKubeconfigPath(t *testing.T) {
	const sentinelPath = "/nonexistent/kubeagent-test/secret-cluster.kubeconfig"
	const sentinelContext = "sentinel-context-should-not-leak-via-error-text"

	cs := connect(t, Config{Kubeconfig: sentinelPath, Context: sentinelContext}, fake.NewSimpleClientset())

	out := callAdvisory(t, cs, map[string]any{"sections": []any{"operators", "drift"}})

	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	body := string(blob)

	if strings.Contains(body, sentinelPath) {
		t.Errorf("marshalled AdvisoryOutput leaks the kubeconfig path sentinel:\n%s", body)
	}

	// coverage.context deliberately and separately echoes cfg.Context (see
	// contextLabel and TestTriage_HealthyClusterIsExplicitlyHealthy) — that
	// single, intended occurrence is not this bug. What must not happen is
	// the *discarded dynamic-client error* embedding the same context name a
	// second time the way internal/cluster's wrapped error used to; a count
	// above 1 means it leaked back in through a Partial/ChecksSkipped reason.
	if got := strings.Count(body, sentinelContext); got != 1 {
		t.Errorf("sentinel context name appears %d times in the marshalled output, want exactly 1 "+
			"(only coverage.context):\n%s", got, body)
	}

	var skippedOperators, skippedDrift bool
	for _, sk := range out.Coverage.ChecksSkipped {
		switch sk.Check {
		case "operators":
			skippedOperators = true
		case "drift":
			skippedDrift = true
		}
	}
	if !skippedOperators || !skippedDrift {
		t.Errorf("coverage.checksSkipped = %+v, want both %q and %q recorded as skipped so the fix "+
			"hides the credential without hiding the degradation", out.Coverage.ChecksSkipped, "operators", "drift")
	}
}

// TestAdvisory_ForbiddenCertificatesIsPartialNotClean covers the RBAC-blocked
// certificates section: scan.Evaluate still allocates a Report and sets
// Certificates.Forbidden, so the section "ran" and produced output, but read
// nothing. Coverage must record that as a partial read, not silently report
// certificates as a clean, fully-run check.
func TestAdvisory_ForbiddenCertificatesIsPartialNotClean(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	cli := fake.NewSimpleClientset(node)
	cli.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", nil)
	})

	cs := connect(t, Config{Context: "kind-example"}, cli)

	out := callAdvisory(t, cs, map[string]any{"sections": []any{"certificates"}})

	var ranCertificates bool
	for _, c := range out.Coverage.ChecksRun {
		if c == "certificates" {
			ranCertificates = true
		}
	}
	if !ranCertificates {
		t.Errorf("coverage.checksRun = %v, want it to contain %q", out.Coverage.ChecksRun, "certificates")
	}

	var partialCertificates bool
	for _, p := range out.Coverage.Partial {
		if p.Resource == "certificates" {
			partialCertificates = true
		}
	}
	if !partialCertificates {
		t.Errorf("coverage.partial = %+v, want an entry naming %q — an RBAC-forbidden certificates "+
			"read must not be reported as a clean, fully-run check", out.Coverage.Partial, "certificates")
	}
}

// TestAdvisory_SecuritySectionMarshalsAsEmptyArrayNotNull covers a clean
// cluster: res.SecurityIssues is nil (not []) when the scan finds nothing.
// Every other section is a pointer-to-struct and never hits this; a nil
// slice marshals as JSON null, and a model reading null cannot tell "ran and
// found nothing" from "did not run". This asserts on the marshalled bytes,
// not the Go value, per the project's json-tag-on-the-wrong-struct history.
func TestAdvisory_SecuritySectionMarshalsAsEmptyArrayNotNull(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_advisory", Arguments: map[string]any{"sections": []any{"security"}},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	blob, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(blob), `"security":[]`) {
		t.Errorf("marshalled sections = %s, want the literal `\"security\":[]`, not null, on a clean cluster", blob)
	}
}
