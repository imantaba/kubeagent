package capacity

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestClassifyNodesIncludesHealthyNodes(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "4", "16Gi"), node("worker2", "4", "16Gi")}
	pods := []corev1.Pod{pod("prod", "a", "worker1", "1", "2Gi")}

	included, excluded := classifyNodes(nodes, pods)

	if len(included) != 2 {
		t.Fatalf("want 2 included nodes, got %d", len(included))
	}
	if len(excluded) != 0 {
		t.Fatalf("want no exclusions, got %+v", excluded)
	}
	if included[0].name != "worker1" || included[1].name != "worker2" {
		t.Fatalf("want included sorted by name, got %s, %s", included[0].name, included[1].name)
	}
	if included[0].cpuReq != 1000 {
		t.Errorf("want worker1 cpuReq 1000m, got %d", included[0].cpuReq)
	}
	if included[1].cpuReq != 0 {
		t.Errorf("want worker2 cpuReq 0, got %d", included[1].cpuReq)
	}
}

func TestClassifyNodesExclusionReasons(t *testing.T) {
	nodes := []corev1.Node{
		node("ok", "4", "16Gi"),
		notReady(node("down", "4", "16Gi")),
		cordoned(node("parked", "4", "16Gi")),
		tainted(node("cp", "4", "16Gi"), corev1.TaintEffectNoSchedule),
		tainted(node("drained", "4", "16Gi"), corev1.TaintEffectNoExecute),
	}

	included, excluded := classifyNodes(nodes, nil)

	if len(included) != 1 || included[0].name != "ok" {
		t.Fatalf("want only ok included, got %+v", included)
	}
	want := map[string]string{
		"down":    "NotReady",
		"parked":  "cordoned",
		"cp":      "NoSchedule taint",
		"drained": "NoExecute taint",
	}
	if len(excluded) != len(want) {
		t.Fatalf("want %d exclusions, got %+v", len(want), excluded)
	}
	for _, e := range excluded {
		if want[e.Node] != e.Reason {
			t.Errorf("node %s: want reason %q, got %q", e.Node, want[e.Node], e.Reason)
		}
	}
}

// A node that is simultaneously NotReady, cordoned and tainted reports exactly one
// reason, the first in the documented order, so the output cannot vary by map order.
func TestClassifyNodesReportsFirstReasonOnly(t *testing.T) {
	n := tainted(cordoned(notReady(node("triple", "4", "16Gi"))), corev1.TaintEffectNoSchedule)

	_, excluded := classifyNodes([]corev1.Node{n}, nil)

	if len(excluded) != 1 {
		t.Fatalf("want exactly 1 exclusion, got %+v", excluded)
	}
	if excluded[0].Reason != "NotReady" {
		t.Errorf("want first reason NotReady, got %q", excluded[0].Reason)
	}
}

// Terminal pods reserve nothing — the same rule internal/resources already applies.
func TestClassifyNodesSkipsTerminalPods(t *testing.T) {
	done := pod("prod", "done", "worker1", "2", "4Gi")
	done.Status.Phase = corev1.PodSucceeded
	failed := pod("prod", "failed", "worker1", "2", "4Gi")
	failed.Status.Phase = corev1.PodFailed

	included, _ := classifyNodes([]corev1.Node{node("worker1", "4", "16Gi")},
		[]corev1.Pod{done, failed})

	if included[0].cpuReq != 0 {
		t.Errorf("want terminal pods to reserve nothing, got cpuReq %d", included[0].cpuReq)
	}
}

// A pod scheduled onto an excluded node must not be counted against any included
// node — its requests belong to capacity the section has already disclaimed.
func TestClassifyNodesIgnoresPodsOnExcludedNodes(t *testing.T) {
	nodes := []corev1.Node{
		node("worker1", "4", "16Gi"),
		tainted(node("cp", "4", "16Gi"), corev1.TaintEffectNoSchedule),
	}
	pods := []corev1.Pod{pod("kube-system", "apiserver", "cp", "2", "4Gi")}

	included, _ := classifyNodes(nodes, pods)

	if len(included) != 1 || included[0].cpuReq != 0 {
		t.Fatalf("want the cp pod ignored, got %+v", included)
	}
}
