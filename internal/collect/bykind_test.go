package collect

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/policy"
)

// dynamicForKinds builds a fake dynamic client that knows the list kinds for
// every kind ByKind can read, so a test can hand it any selectable object.
func dynamicForKinds(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	lists := map[schema.GroupVersionResource]string{}
	for _, kind := range policy.SelectableKinds() {
		gvr, ok := KindGVR(kind)
		if !ok {
			continue
		}
		lists[gvr] = kind + "List"
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objs...)
}

func unstructuredPod(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
	}}
}

func TestByKindListsANamespacedKind(t *testing.T) {
	dyn := dynamicForKinds(unstructuredPod("prod", "web"), unstructuredPod("dev", "api"))

	all, err := ByKind(context.Background(), dyn, "Pod", "")
	if err != nil {
		t.Fatalf("ByKind: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d pods across all namespaces, want 2", len(all))
	}

	scoped, err := ByKind(context.Background(), dyn, "Pod", "prod")
	if err != nil {
		t.Fatalf("ByKind scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].GetName() != "web" {
		t.Fatalf("namespace scoping did not apply: %#v", scoped)
	}
}

// A cluster-scoped kind has no namespace to scope to. Passing one must not
// produce an empty list — that would silently turn "every Node" into "no
// Nodes" whenever the operator ran a namespaced scan.
func TestByKindIgnoresNamespaceOnAClusterScopedKind(t *testing.T) {
	node := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]any{"name": "worker-1"},
	}}
	dyn := dynamicForKinds(node)

	got, err := ByKind(context.Background(), dyn, "Node", "prod")
	if err != nil {
		t.Fatalf("ByKind: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d nodes, want 1 — a namespace must not scope a cluster-scoped kind", len(got))
	}
}

// Secret is not readable through this path at all. policy.Load already refuses
// it, but ByKind is an exported function in a package other code can call, so
// the refusal is enforced here too rather than only one layer up.
func TestByKindRefusesSecret(t *testing.T) {
	dyn := dynamicForKinds()
	got, err := ByKind(context.Background(), dyn, "Secret", "")
	if err == nil {
		t.Fatal("ByKind read Secret; it must refuse")
	}
	if got != nil {
		t.Errorf("ByKind returned objects alongside the refusal: %#v", got)
	}
	for _, action := range dyn.Actions() {
		t.Errorf("ByKind issued an API call for Secret: %#v", action)
	}
}

func TestByKindRefusesAnUnknownKind(t *testing.T) {
	if _, err := ByKind(context.Background(), dynamicForKinds(), "Frobnicator", ""); err == nil {
		t.Fatal("ByKind accepted a kind it has no GVR for")
	}
}

// A refused read must reach the caller as an error, never as an empty list —
// an empty list is indistinguishable from "nothing is wrong", which is exactly
// the silent pass the policy surface must not produce.
func TestByKindSurfacesAForbiddenReadAsAnError(t *testing.T) {
	dyn := dynamicForKinds()
	dyn.PrependReactor("list", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", nil)
	})

	got, err := ByKind(context.Background(), dyn, "NetworkPolicy", "")
	if err == nil {
		t.Fatal("a forbidden list returned no error")
	}
	if got != nil {
		t.Errorf("a forbidden list returned objects: %#v", got)
	}
}

// The GVR table and policy's selectable-kind table must name the same kinds.
// They live in different packages on purpose — policy is pure and holds no
// client — so nothing but this test keeps them from drifting. A kind policy
// can select but collect cannot read would be reported "not evaluated" on
// every cluster, forever.
func TestGVRTableMatchesTheSelectableKinds(t *testing.T) {
	for _, kind := range policy.SelectableKinds() {
		if _, ok := KindGVR(kind); !ok {
			t.Errorf("policy can select %q but collect has no GVR for it", kind)
		}
	}
	for kind := range kindGVRs {
		if !policy.KindSelectable(kind) {
			t.Errorf("collect can read %q but policy cannot select it", kind)
		}
	}
	if len(kindGVRs) != len(policy.SelectableKinds()) {
		t.Errorf("table sizes differ: collect has %d, policy has %d", len(kindGVRs), len(policy.SelectableKinds()))
	}
}

func TestPolicyObjectsReadsEveryKindItIsGiven(t *testing.T) {
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"namespace": "prod", "name": "web"},
	}}
	dyn := dynamicForKinds(unstructuredPod("prod", "web"), dep)

	objs, unreadable := PolicyObjects(context.Background(), dyn,
		[]string{"Deployment", "Pod"}, "", 8)

	if len(unreadable) != 0 {
		t.Errorf("nothing was refused, got %v", unreadable)
	}
	if len(objs["Pod"]) != 1 || len(objs["Deployment"]) != 1 {
		t.Fatalf("got %d pods and %d deployments, want 1 each",
			len(objs["Pod"]), len(objs["Deployment"]))
	}
}

// A refused kind must land in the unreadable set and must NOT appear in the
// object map with an empty list. An empty list evaluates to "no violations",
// which is the silent pass this whole surface exists to avoid.
func TestPolicyObjectsSeparatesARefusedKindFromAnEmptyOne(t *testing.T) {
	dyn := dynamicForKinds()
	dyn.PrependReactor("list", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", nil)
	})

	objs, unreadable := PolicyObjects(context.Background(), dyn,
		[]string{"NetworkPolicy", "Pod"}, "", 8)

	if !unreadable["NetworkPolicy"] {
		t.Error("a refused read was not recorded as unreadable")
	}
	if _, present := objs["NetworkPolicy"]; present {
		t.Error("a refused kind must be absent from the object map, not empty in it")
	}
	if unreadable["Pod"] {
		t.Error("one refusal marked an unrelated kind unreadable")
	}
	if _, present := objs["Pod"]; !present {
		t.Error("an empty but readable kind must be present with an empty list")
	}
}

// The reads overlap, so the result must not depend on which one answered
// first. One worker and eight must produce the same map.
func TestPolicyObjectsIsIndependentOfTheSchedule(t *testing.T) {
	kinds := []string{"Deployment", "NetworkPolicy", "Pod", "Service"}
	objects := []runtime.Object{unstructuredPod("prod", "web"), unstructuredPod("dev", "api")}

	one, unreadableOne := PolicyObjects(context.Background(), dynamicForKinds(objects...), kinds, "", 1)
	many, unreadableMany := PolicyObjects(context.Background(), dynamicForKinds(objects...), kinds, "", 8)

	if len(one) != len(many) || len(unreadableOne) != len(unreadableMany) {
		t.Fatalf("worker count changed the result: %d/%d vs %d/%d",
			len(one), len(unreadableOne), len(many), len(unreadableMany))
	}
	for kind := range one {
		if len(one[kind]) != len(many[kind]) {
			t.Errorf("%s: %d objects with one worker, %d with eight", kind, len(one[kind]), len(many[kind]))
		}
	}
}

func TestPolicyObjectsWithNoKindsReadsNothing(t *testing.T) {
	dyn := dynamicForKinds()
	objs, unreadable := PolicyObjects(context.Background(), dyn, nil, "", 8)
	if len(objs) != 0 || len(unreadable) != 0 {
		t.Errorf("got %d kinds and %d refusals for an empty plan", len(objs), len(unreadable))
	}
	if len(dyn.Actions()) != 0 {
		t.Errorf("an empty plan issued %d API calls", len(dyn.Actions()))
	}
}
