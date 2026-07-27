package advisory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func pod(ns, name string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func TestFlagNames(t *testing.T) {
	tests := []struct {
		operators, drift bool
		want             string
	}{
		{true, true, "--operators/--drift"},
		{true, false, "--operators"},
		{false, true, "--drift"},
	}
	for _, tt := range tests {
		if got := FlagNames(tt.operators, tt.drift); got != tt.want {
			t.Errorf("FlagNames(%v, %v) = %q, want %q", tt.operators, tt.drift, got, tt.want)
		}
	}
}

func TestClusterPods_ClusterScopeReturnsScopedUnchanged(t *testing.T) {
	client := fake.NewSimpleClientset()
	scoped := []corev1.Pod{pod("a", "one")}

	got, err := ClusterPods(context.Background(), client, "", scoped)
	if err != nil {
		t.Fatalf("ClusterPods() error = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Name != "one" {
		t.Errorf("ClusterPods() = %v, want the scoped slice unchanged", got)
	}
}

func TestClusterPods_NamespacedFetchesEveryNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "one"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "two"}},
	)
	scoped := []corev1.Pod{pod("a", "one")}

	got, err := ClusterPods(context.Background(), client, "a", scoped)
	if err != nil {
		t.Fatalf("ClusterPods() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Errorf("ClusterPods() returned %d pods, want 2 (the whole cluster)", len(got))
	}
}

func TestClusterPods_ListFailureFallsBackToScopedAndReportsWhy(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pods is forbidden")
	})
	scoped := []corev1.Pod{pod("a", "one")}

	got, err := ClusterPods(context.Background(), client, "a", scoped)
	if err == nil {
		t.Fatal("ClusterPods() error = nil, want the list failure reported")
	}
	if len(got) != 1 || got[0].Name != "one" {
		t.Errorf("ClusterPods() = %v, want the scoped slice as the fallback", got)
	}
}

func TestAssess_NothingEnabledDoesNothing(t *testing.T) {
	called := false
	dyn := func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
		called = true
		return nil, nil, errors.New("must not be called")
	}

	got := Assess(context.Background(), fake.NewSimpleClientset(), dyn, Inputs{}, Options{}, time.Now())

	if called {
		t.Error("Assess() built dynamic clients with no advisory section enabled; a default scan must issue no extra API call")
	}
	if got.Operators != nil || got.GitOps != nil || got.Capacity != nil {
		t.Errorf("Assess() = %+v, want all reports nil", got)
	}
	if len(got.Degradations) != 0 {
		t.Errorf("Assess() degradations = %v, want none", got.Degradations)
	}
}

func TestAssess_DynamicClientFailureDegradesBothSectionsOnce(t *testing.T) {
	dyn := func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
		return nil, nil, errors.New("no CRD access")
	}

	got := Assess(context.Background(), fake.NewSimpleClientset(), dyn, Inputs{},
		Options{Operators: true, Drift: true}, time.Now())

	if len(got.Degradations) != 1 {
		t.Fatalf("Assess() degradations = %v, want exactly one", got.Degradations)
	}
	d := got.Degradations[0]
	if d.Subject != "--operators/--drift" {
		t.Errorf("Subject = %q, want %q", d.Subject, "--operators/--drift")
	}
	if len(d.Sections) != 2 || d.Sections[0] != "operators" || d.Sections[1] != "drift" {
		t.Errorf("Sections = %v, want [operators drift]", d.Sections)
	}
	if d.Reason != "no CRD access" {
		t.Errorf("Reason = %q, want %q", d.Reason, "no CRD access")
	}
	if got.Operators != nil || got.GitOps != nil {
		t.Error("Assess() produced a report despite the client failure")
	}
}

// restRouted overrides CoreV1Interface.RESTClient with a working client so
// collect.PodMetrics' raw GET can actually run. fake.NewSimpleClientset's own
// RESTClient() is always a nil *rest.RESTClient (client-go's fake package
// only intercepts typed List/Get/Create calls, never raw REST requests), so
// calling it panics instead of returning the "metrics-server absent" answer
// PodMetrics is documented to give — see TestEvaluate_KubeletHealthOffByDefault
// in internal/scan for the same limitation. Routing RESTClient() at a real
// httptest server that 404s reproduces the real "absent" behavior without
// touching collect.go or advisory.go.
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
// same way it would against a real cluster that never installed one.
func fakeClientWithNoMetricsServer(t *testing.T, objs ...runtime.Object) kubernetes.Interface {
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

	base := fake.NewSimpleClientset(objs...)
	return clientWithMetricsRoute{Clientset: base, core: restRouted{CoreV1Interface: base.CoreV1(), rc: rc}}
}

func TestAssess_CapacityRunsWithoutMetricsAndSaysSo(t *testing.T) {
	// The fake clientset serves no metrics API, which collect.PodMetrics
	// reports as "not available" rather than as an error.
	dyn := func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
		return nil, nil, errors.New("must not be called")
	}

	got := Assess(context.Background(), fakeClientWithNoMetricsServer(t), dyn, Inputs{},
		Options{Capacity: true}, time.Now())

	if got.Capacity == nil {
		t.Fatal("Capacity report is nil; the section still runs from requests and limits without metrics")
	}
	if got.MetricsAvailable {
		t.Error("MetricsAvailable = true with no metrics API; a consumer would read the headroom as usage-backed")
	}
	if len(got.Degradations) != 0 {
		t.Errorf("Degradations = %v, want none — an absent metrics-server is normal, not an error",
			got.Degradations)
	}
}

var _ kubernetes.Interface = (*fake.Clientset)(nil)
