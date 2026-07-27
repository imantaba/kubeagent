package capacity

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestHeadroomSchedulableSumsIncludedNodesOnly(t *testing.T) {
	nodes := []corev1.Node{
		node("worker1", "4", "16Gi"),
		node("worker2", "4", "16Gi"),
		tainted(node("cp", "8", "32Gi"), corev1.TaintEffectNoSchedule),
	}
	pods := []corev1.Pod{pod("prod", "a", "worker1", "1", "4Gi")}

	rep := Assess(nodes, pods, nil, nil, nil, "")

	h := rep.Headroom
	if h == nil {
		t.Fatal("want a headroom block")
	}
	if h.IncludedNodes != 2 || h.TotalNodes != 3 {
		t.Errorf("want 2 of 3 nodes, got %d of %d", h.IncludedNodes, h.TotalNodes)
	}
	// 8 allocatable cores across the two workers, 1 requested.
	if h.FreeCPU != "7.0" {
		t.Errorf("want FreeCPU 7.0, got %q", h.FreeCPU)
	}
	if h.FreeMemory != "28Gi" {
		t.Errorf("want FreeMemory 28Gi, got %q", h.FreeMemory)
	}
	if len(h.Excluded) != 1 || h.Excluded[0].Node != "cp" {
		t.Errorf("want cp excluded, got %+v", h.Excluded)
	}
}

// largest pod fit must never mix nodes: the memory reported beside the winning CPU
// node is that same node's memory.
func TestHeadroomLargestFitNeverMixesNodes(t *testing.T) {
	nodes := []corev1.Node{node("bigcpu", "16", "8Gi"), node("bigmem", "2", "64Gi")}

	rep := Assess(nodes, nil, nil, nil, nil, "")

	h := rep.Headroom
	if h.LargestCPUFit == nil || h.LargestCPUFit.Node != "bigcpu" {
		t.Fatalf("want bigcpu as the CPU fit, got %+v", h.LargestCPUFit)
	}
	if h.LargestCPUFit.Memory != "8Gi" {
		t.Errorf("want bigcpu's own memory 8Gi beside it, got %q", h.LargestCPUFit.Memory)
	}
	if h.LargestMemFit == nil || h.LargestMemFit.Node != "bigmem" {
		t.Fatalf("want a separate bigmem line, got %+v", h.LargestMemFit)
	}
	if h.LargestMemFit.CPU != "2.0" {
		t.Errorf("want bigmem's own CPU 2.0 beside it, got %q", h.LargestMemFit.CPU)
	}
}

// When one node wins both, there is no second line to print.
func TestHeadroomLargestFitSingleLineWhenSameNode(t *testing.T) {
	nodes := []corev1.Node{node("big", "16", "64Gi"), node("small", "2", "8Gi")}

	rep := Assess(nodes, nil, nil, nil, nil, "")

	if rep.Headroom.LargestMemFit != nil {
		t.Errorf("want no separate memory line when one node wins both, got %+v",
			rep.Headroom.LargestMemFit)
	}
}

func TestHeadroomTightestNodePicksHigherRatio(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "4", "16Gi"), node("worker2", "4", "16Gi")}
	pods := []corev1.Pod{
		pod("prod", "cpuhog", "worker1", "3", "1Gi"),  // 75% CPU, 6% memory
		pod("prod", "memhog", "worker2", "1", "14Gi"), // 25% CPU, 87% memory
	}

	rep := Assess(nodes, pods, nil, nil, nil, "")

	tn := rep.Headroom.TightestNode
	if tn == nil || tn.Node != "worker2" {
		t.Fatalf("want worker2 tightest, got %+v", tn)
	}
	if tn.Resource != "memory" || tn.Pct != 87 {
		t.Errorf("want memory 87%%, got %s %d%%", tn.Resource, tn.Pct)
	}
}

func TestHeadroomNilWhenNoNodesIncluded(t *testing.T) {
	nodes := []corev1.Node{cordoned(node("parked", "4", "16Gi"))}

	rep := Assess(nodes, nil, nil, nil, nil, "")

	h := rep.Headroom
	if h == nil {
		t.Fatal("want a headroom block carrying the exclusion list")
	}
	if h.IncludedNodes != 0 {
		t.Errorf("want 0 included, got %d", h.IncludedNodes)
	}
	if h.LargestCPUFit != nil || h.TightestNode != nil {
		t.Errorf("want no rows with nothing included, got %+v / %+v",
			h.LargestCPUFit, h.TightestNode)
	}
	if len(h.Excluded) != 1 {
		t.Errorf("want the exclusion still reported, got %+v", h.Excluded)
	}
}

// A node reporting zero allocatable must not divide by zero.
func TestHeadroomZeroAllocatableNode(t *testing.T) {
	nodes := []corev1.Node{node("empty", "0", "0")}

	rep := Assess(nodes, nil, nil, nil, nil, "")

	if rep.Headroom.TightestNode == nil || rep.Headroom.TightestNode.Pct != 0 {
		t.Errorf("want 0%% for a zero-allocatable node, got %+v", rep.Headroom.TightestNode)
	}
}

// An over-committed node (summed pod requests exceed allocatable) must clamp free
// capacity at zero, not go negative — a future refactor dropping the max64(0, ...)
// clamp would understate (or, summed with other nodes, silently distort) the
// schedulable figure.
func TestHeadroomOvercommittedNodeClampsFreeAtZero(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "4", "16Gi")}
	// 6 cores / 20Gi requested against a 4-core / 16Gi node: over-committed on both.
	pods := []corev1.Pod{pod("prod", "big", "worker1", "6", "20Gi")}

	rep := Assess(nodes, pods, nil, nil, nil, "")

	h := rep.Headroom
	if h.FreeCPU != "0.0" {
		t.Errorf("want FreeCPU clamped to 0.0, got %q", h.FreeCPU)
	}
	if h.FreeMemory != "0Mi" {
		t.Errorf("want FreeMemory clamped to 0Mi, got %q", h.FreeMemory)
	}
}
