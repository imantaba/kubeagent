package capacity

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestNodeLossFitsWhenRemainingNodesHaveRoom(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "8", "32Gi"), node("worker2", "8", "32Gi")}
	pods := []corev1.Pod{
		pod("prod", "a", "worker1", "1", "2Gi"),
		pod("prod", "b", "worker1", "1", "2Gi"),
	}

	rep := Assess(nodes, pods, nil, nil, nil, "")

	nl := rep.Headroom.NodeLoss
	if nl == nil {
		t.Fatal("want a node-loss result")
	}
	if !nl.Fits {
		t.Errorf("want fits, got %+v", nl)
	}
	if nl.Placed != 2 {
		t.Errorf("want 2 pods placed, got %d", nl.Placed)
	}
}

func TestNodeLossReportsBlockerWhenFirstFitCannotPlace(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "8", "32Gi"), node("worker2", "2", "8Gi")}
	pods := []corev1.Pod{ownedBy(pod("prod", "db-0", "worker1", "6", "2Gi"), "StatefulSet", "db")}

	rep := Assess(nodes, pods, nil, nil, nil, "")

	nl := rep.Headroom.NodeLoss
	if nl.Fits {
		t.Fatalf("want a placement failure, got %+v", nl)
	}
	if nl.Node != "worker1" {
		t.Errorf("want the largest node as the victim, got %q", nl.Node)
	}
	if nl.Blocker != "StatefulSet/prod/db" {
		t.Errorf("want the owner named as blocker, got %q", nl.Blocker)
	}
	if nl.BlockerCPU != "6.0" {
		t.Errorf("want the blocker's CPU request, got %q", nl.BlockerCPU)
	}
}

// DaemonSet pods do not rehome when their node goes away, so they must not count
// against the remaining nodes' room.
func TestNodeLossExcludesDaemonSetPods(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "4", "16Gi"), node("worker2", "1", "2Gi")}
	pods := []corev1.Pod{ownedBy(pod("kube-system", "cni-abc", "worker1", "3", "8Gi"), "DaemonSet", "cni")}

	rep := Assess(nodes, pods, nil, nil, nil, "")

	nl := rep.Headroom.NodeLoss
	if !nl.Fits {
		t.Errorf("want fits — the only pod is a DaemonSet pod, got %+v", nl)
	}
	if nl.Placed != 0 {
		t.Errorf("want 0 pods needing placement, got %d", nl.Placed)
	}
}

func TestNodeLossSingleNode(t *testing.T) {
	rep := Assess([]corev1.Node{node("only", "4", "16Gi")}, nil, nil, nil, nil, "")

	nl := rep.Headroom.NodeLoss
	if nl == nil || !nl.SingleNode {
		t.Fatalf("want the single-node case flagged, got %+v", nl)
	}
	if nl.Fits {
		t.Errorf("want Fits false when no arithmetic is possible, got %+v", nl)
	}
}

// Equal-sized nodes must not make the victim depend on slice order.
func TestNodeLossVictimTieBreaksByName(t *testing.T) {
	nodes := []corev1.Node{node("zeta", "4", "16Gi"), node("alpha", "4", "16Gi")}

	rep := Assess(nodes, nil, nil, nil, nil, "")

	if rep.Headroom.NodeLoss.Node != "alpha" {
		t.Errorf("want the alphabetically first node on a tie, got %q", rep.Headroom.NodeLoss.Node)
	}
}

// Decreasing order matters: placing the big pod first is what makes first-fit
// succeed here. A naive in-order pass would fill worker2 with the small pod and
// then fail the big one.
func TestNodeLossPlacesLargestFirst(t *testing.T) {
	nodes := []corev1.Node{
		node("victim", "8", "32Gi"),
		node("roomy", "4", "16Gi"),
		node("tiny", "1", "4Gi"),
	}
	pods := []corev1.Pod{
		pod("prod", "small", "victim", "500m", "1Gi"),
		pod("prod", "big", "victim", "4", "8Gi"),
	}

	rep := Assess(nodes, pods, nil, nil, nil, "")

	nl := rep.Headroom.NodeLoss
	if !nl.Fits || nl.Placed != 2 {
		t.Errorf("want both placed by first-fit-decreasing, got %+v", nl)
	}
}
