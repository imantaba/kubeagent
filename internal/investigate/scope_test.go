package investigate

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/inventory"
)

func TestScope_SeedsWorkloadPodsAndNodes(t *testing.T) {
	s := NewScope([]inventory.Workload{{
		Kind: "Deployment", Namespace: "shop", Name: "web",
		Pods: []inventory.PodRow{{Name: "web-abc", Node: "node-1"}},
	}})
	if !s.Allowed("deployment", "shop", "web") {
		t.Error("workload should be in scope")
	}
	if !s.Allowed("Pod", "shop", "web-abc") {
		t.Error("pod should be in scope (kind case-insensitive)")
	}
	if !s.Allowed("node", "", "node-1") {
		t.Error("pod's node should be in scope")
	}
	if s.Allowed("deployment", "other", "web") {
		t.Error("unrelated namespace must be denied")
	}
	if !s.HasName("shop", "web-abc") {
		t.Error("HasName should match an in-scope object regardless of kind")
	}
	if s.HasName("other", "web-abc") {
		t.Error("HasName must deny an out-of-scope namespace")
	}
}

func TestScope_GrowsViaAdd(t *testing.T) {
	s := NewScope(nil)
	if s.Allowed("pvc", "shop", "data") {
		t.Fatal("pvc not in scope before Add")
	}
	s.Add("PersistentVolumeClaim", "shop", "data")
	if !s.Allowed("pvc", "shop", "data") {
		t.Error("pvc should be reachable after Add (kind normalized)")
	}
}
