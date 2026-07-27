package capacity

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func usageOf(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
}

func TestSampleAttachesToFlaggedOwner(t *testing.T) {
	pods := []corev1.Pod{pod("staging", "web-1", "worker1", "", "")}
	usage := map[string]corev1.ResourceList{"staging/web-1": usageOf("30m", "64Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, usage, "")

	o := rep.RightSizing.Rules[0].Owners[0]
	if !strings.Contains(o.Observed, "0.0") {
		t.Errorf("want an observed CPU reading, got %q", o.Observed)
	}
	if !rep.RightSizing.MetricsAvailable {
		t.Error("want MetricsAvailable true")
	}
	if rep.RightSizing.PodsReporting != 1 || rep.RightSizing.PodsTotal != 1 {
		t.Errorf("want 1 of 1 reporting, got %d of %d",
			rep.RightSizing.PodsReporting, rep.RightSizing.PodsTotal)
	}
}

// Replicas of one owner are summed, because the row names the workload.
func TestSampleSumsAcrossOwnerReplicas(t *testing.T) {
	pods := []corev1.Pod{
		pod("staging", "web-1", "worker1", "", ""),
		pod("staging", "web-2", "worker1", "", ""),
	}
	pods[0] = ownedBy(pods[0], "StatefulSet", "web")
	pods[1] = ownedBy(pods[1], "StatefulSet", "web")
	usage := map[string]corev1.ResourceList{
		"staging/web-1": usageOf("100m", "64Mi"),
		"staging/web-2": usageOf("400m", "64Mi"),
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, usage, "")

	o := rep.RightSizing.Rules[0].Owners[0]
	if !strings.Contains(o.Observed, "0.5") {
		t.Errorf("want the two replicas summed to 0.5 cores, got %q", o.Observed)
	}
}

// THE core constraint: a healthy workload with a tiny sample must never appear.
func TestSampleNeverSelectsAWorkload(t *testing.T) {
	pods := []corev1.Pod{pod("prod", "thrifty", "worker1", "4", "8Gi")}
	usage := map[string]corev1.ResourceList{"prod/thrifty": usageOf("1m", "8Mi")}

	rep := Assess([]corev1.Node{node("worker1", "8", "32Gi")}, pods, nil, usage, "")

	if rep.RightSizing != nil {
		t.Errorf("want no right-sizing block: a sample must never create a row, got %+v",
			rep.RightSizing)
	}
}

func TestSampleAbsentMetricsStillRendersRules(t *testing.T) {
	pods := []corev1.Pod{pod("staging", "web-1", "worker1", "", "")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	if rep.RightSizing == nil || len(rep.RightSizing.Rules) != 1 {
		t.Fatalf("want the structural rule with no metrics, got %+v", rep.RightSizing)
	}
	if rep.RightSizing.MetricsAvailable {
		t.Error("want MetricsAvailable false")
	}
	if rep.RightSizing.Rules[0].Owners[0].Observed != "" {
		t.Error("want no observed value with no metrics")
	}
}

// Coverage counts the pods the section considered, not every pod in the cluster.
func TestSampleCoverageCountsScopedPods(t *testing.T) {
	pods := []corev1.Pod{
		pod("prod", "a", "worker1", "", ""),
		pod("prod", "b", "worker1", "", ""),
		pod("staging", "c", "worker1", "", ""),
	}
	usage := map[string]corev1.ResourceList{"prod/a": usageOf("10m", "8Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, usage, "prod")

	if rep.RightSizing.PodsTotal != 2 || rep.RightSizing.PodsReporting != 1 {
		t.Errorf("want 1 of 2 in-scope pods reporting, got %d of %d",
			rep.RightSizing.PodsReporting, rep.RightSizing.PodsTotal)
	}
}
