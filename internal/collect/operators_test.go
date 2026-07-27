package collect

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/operators"
)

var certGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
var clusterIssuerGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}

var certAdapter = operators.Adapter{
	Operator: "cert-manager", Group: "cert-manager.io", Version: "v1",
	Resource: "certificates", Kind: "Certificate", Rule: operators.ConditionRule{Type: "Ready"},
}
var clusterIssuerAdapter = operators.Adapter{
	Operator: "cert-manager", Group: "cert-manager.io", Version: "v1",
	Resource: "clusterissuers", Kind: "ClusterIssuer", Rule: operators.ConditionRule{Type: "Ready"},
}

// discoveryFor builds a fake discovery serving the given resource lists.
func discoveryFor(lists ...*metav1.APIResourceList) *discoveryfake.FakeDiscovery {
	return &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: lists}}
}

// certManagerV1 is the resource list a cluster with cert-manager installed serves.
func certManagerV1() *metav1.APIResourceList {
	return &metav1.APIResourceList{
		GroupVersion: "cert-manager.io/v1",
		APIResources: []metav1.APIResource{
			{Name: "certificates", Kind: "Certificate", Namespaced: true},
			{Name: "clusterissuers", Kind: "ClusterIssuer", Namespaced: false},
		},
	}
}

// dynamicFor builds a fake dynamic client that knows the cert-manager list kinds.
func dynamicFor(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			certGVR:          "CertificateList",
			clusterIssuerGVR: "ClusterIssuerList",
			{Group: "longhorn.io", Version: "v1beta1", Resource: "volumes"}: "VolumeList",
		},
		objs...,
	)
}

// certCR builds a cert-manager Certificate object.
func certCR(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True"},
		}},
	}}
}

func TestOperatorResources_AbsentGroupCostsZeroDynamicCalls(t *testing.T) {
	// Discovery is the installation signal. An operator whose group the API
	// server does not serve is skipped entirely: no call, no error, no entry.
	disco := discoveryFor() // nothing installed
	dyn := dynamicFor()

	got := OperatorResources(context.Background(), disco, dyn, operators.Adapters(), "")
	if len(got) != 0 {
		t.Errorf("got %d fetched results, want 0 for a cluster with no operators", len(got))
	}
	if n := len(dyn.Actions()); n != 0 {
		t.Errorf("dynamic client made %d calls, want 0: %v", n, dyn.Actions())
	}
}

func TestOperatorResources_ListsAServedResource(t *testing.T) {
	disco := discoveryFor(certManagerV1())
	dyn := dynamicFor(certCR("shop", "web-tls"), certCR("infra", "api-tls"))

	got := OperatorResources(context.Background(), disco, dyn, []operators.Adapter{certAdapter}, "")
	if len(got) != 1 {
		t.Fatalf("got %d fetched results, want 1", len(got))
	}
	if got[0].APIVersion != "cert-manager.io/v1" {
		t.Errorf("apiVersion = %q, want cert-manager.io/v1", got[0].APIVersion)
	}
	if len(got[0].Items) != 2 {
		t.Errorf("listed %d items, want 2", len(got[0].Items))
	}
	if got[0].Err != nil || got[0].Forbidden {
		t.Errorf("unexpected failure: err=%v forbidden=%v", got[0].Err, got[0].Forbidden)
	}
}

func TestOperatorResources_UninstalledCRDInAServedGroupIsSkipped(t *testing.T) {
	// The group is served but this CRD is not installed — a real shape with
	// Flux, where source-controller can be present without kustomize-controller.
	partial := &metav1.APIResourceList{
		GroupVersion: "cert-manager.io/v1",
		APIResources: []metav1.APIResource{{Name: "clusterissuers", Kind: "ClusterIssuer", Namespaced: false}},
	}
	disco := discoveryFor(partial)
	dyn := dynamicFor()

	got := OperatorResources(context.Background(), disco, dyn, []operators.Adapter{certAdapter}, "")
	if len(got) != 0 {
		t.Errorf("got %d fetched results, want 0 (certificates is not served)", len(got))
	}
}

func TestOperatorResources_NamespaceScopingHonoursTheDiscoveredScope(t *testing.T) {
	// Namespaced resources honour -n; cluster-scoped ones are always listed
	// cluster-wide, the way Nodes already ignores the namespace filter.
	disco := discoveryFor(certManagerV1())
	dyn := dynamicFor(certCR("shop", "web-tls"), certCR("infra", "api-tls"))

	got := OperatorResources(context.Background(), disco, dyn,
		[]operators.Adapter{certAdapter, clusterIssuerAdapter}, "shop")
	if len(got) != 2 {
		t.Fatalf("got %d fetched results, want 2", len(got))
	}
	if len(got[0].Items) != 1 {
		t.Errorf("namespaced list returned %d items, want 1 (scoped to shop)", len(got[0].Items))
	}

	var sawNamespacedList, sawClusterList bool
	for _, a := range dyn.Actions() {
		la, ok := a.(clienttesting.ListAction)
		if !ok {
			continue
		}
		switch la.GetResource().Resource {
		case "certificates":
			sawNamespacedList = true
			if la.GetNamespace() != "shop" {
				t.Errorf("certificates listed in namespace %q, want shop", la.GetNamespace())
			}
		case "clusterissuers":
			sawClusterList = true
			if la.GetNamespace() != "" {
				t.Errorf("clusterissuers listed in namespace %q, want cluster-wide", la.GetNamespace())
			}
		}
	}
	if !sawNamespacedList || !sawClusterList {
		t.Errorf("missing list calls: namespaced=%v cluster=%v", sawNamespacedList, sawClusterList)
	}
}

func TestOperatorResources_ForbiddenIsIsolatedToItsOwnAdapter(t *testing.T) {
	disco := discoveryFor(certManagerV1())
	dyn := dynamicFor(certCR("shop", "web-tls"))
	dyn.PrependReactor("list", "certificates", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "cert-manager.io", Resource: "certificates"}, "", errors.New("denied"))
	})

	got := OperatorResources(context.Background(), disco, dyn,
		[]operators.Adapter{certAdapter, clusterIssuerAdapter}, "")
	if len(got) != 2 {
		t.Fatalf("got %d fetched results, want 2 — one denial must not stop the rest", len(got))
	}
	if !got[0].Forbidden {
		t.Error("certificates: Forbidden = false, want true")
	}
	if got[0].Err != nil {
		t.Errorf("certificates: Err = %v, want nil (a denial is not an error to report)", got[0].Err)
	}
	if got[1].Forbidden || got[1].Err != nil {
		t.Errorf("clusterissuers was affected by the other adapter's denial: %+v", got[1])
	}
}

func TestOperatorResources_OtherListErrorIsRecordedAgainstThatAdapterOnly(t *testing.T) {
	disco := discoveryFor(certManagerV1())
	dyn := dynamicFor()
	dyn.PrependReactor("list", "certificates", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcdserver: request timed out")
	})

	got := OperatorResources(context.Background(), disco, dyn,
		[]operators.Adapter{certAdapter, clusterIssuerAdapter}, "")
	if len(got) != 2 {
		t.Fatalf("got %d fetched results, want 2", len(got))
	}
	if got[0].Err == nil {
		t.Error("certificates: Err = nil, want the list failure recorded")
	}
	if got[1].Err != nil {
		t.Errorf("clusterissuers: Err = %v, want nil", got[1].Err)
	}
}

func TestOperatorResources_FallsBackToTheServedVersion(t *testing.T) {
	// The adapter prefers longhorn.io/v1beta2; this cluster serves v1beta1. The
	// served version is used and recorded — a field path that does not exist
	// there yields unknown, never unhealthy.
	longhorn := operators.Adapter{
		Operator: "Longhorn", Group: "longhorn.io", Version: "v1beta2",
		Resource: "volumes", Kind: "Volume",
		Rule: operators.FieldRule{Path: []string{"status", "robustness"}, Healthy: []string{"healthy"}},
	}
	disco := discoveryFor(&metav1.APIResourceList{
		GroupVersion: "longhorn.io/v1beta1",
		APIResources: []metav1.APIResource{{Name: "volumes", Kind: "Volume", Namespaced: true}},
	})
	dyn := dynamicFor()

	got := OperatorResources(context.Background(), disco, dyn, []operators.Adapter{longhorn}, "")
	if len(got) != 1 {
		t.Fatalf("got %d fetched results, want 1", len(got))
	}
	if got[0].APIVersion != "longhorn.io/v1beta1" {
		t.Errorf("apiVersion = %q, want the version actually served", got[0].APIVersion)
	}
}

func TestOperatorResources_DiscoveryFailureYieldsNothing(t *testing.T) {
	// Discovery is available to every authenticated user, so a failure here
	// means the API server is unreachable — which the base scan already reports.
	disco := discoveryFor()
	disco.Fake.PrependReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	dyn := dynamicFor()
	// ServerGroups on the fake derives from Fake.Resources; with none set it
	// returns an empty group list, which is the same outcome: nothing fetched.
	if got := OperatorResources(context.Background(), disco, dyn, operators.Adapters(), ""); len(got) != 0 {
		t.Errorf("got %d fetched results, want 0", len(got))
	}
}
