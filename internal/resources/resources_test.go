package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func node(name, cpu, mem string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		}},
	}
}

func podWith(phase corev1.PodPhase, cpuReq, memReq, cpuLim, memLim string) corev1.Pod {
	return corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpuReq),
					corev1.ResourceMemory: resource.MustParse(memReq),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpuLim),
					corev1.ResourceMemory: resource.MustParse(memLim),
				},
			},
		}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestSummarize_AggregatesAndComputesPercents(t *testing.T) {
	nodes := []corev1.Node{node("n1", "4", "8Gi"), node("n2", "4", "8Gi")} // 8 cores, 16Gi
	pods := []corev1.Pod{
		podWith(corev1.PodRunning, "1", "2Gi", "2", "4Gi"),
		podWith(corev1.PodRunning, "1", "2Gi", "2", "4Gi"),
		podWith(corev1.PodSucceeded, "1", "2Gi", "2", "4Gi"), // terminal -> excluded
	}
	usage := map[string]corev1.ResourceList{
		"n1": {corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		"n2": {corev1.ResourceCPU: resource.MustParse("1500m"), corev1.ResourceMemory: resource.MustParse("3Gi")},
	}
	s := Summarize(nodes, pods, usage)
	if !s.MetricsAvailable {
		t.Fatal("expected MetricsAvailable=true")
	}
	if s.CPU.Allocatable != "8.0" || s.CPU.Requests != "2.0" || s.CPU.RequestsPct != 25 {
		t.Errorf("CPU req = %+v", s.CPU)
	}
	if s.CPU.Limits != "4.0" || s.CPU.LimitsPct != 50 {
		t.Errorf("CPU lim = %+v", s.CPU)
	}
	if s.CPU.Usage != "2.0" || s.CPU.UsagePct != 25 {
		t.Errorf("CPU usage = %+v", s.CPU)
	}
	if s.Memory.Allocatable != "16Gi" || s.Memory.Requests != "4Gi" || s.Memory.RequestsPct != 25 {
		t.Errorf("Mem = %+v", s.Memory)
	}
	if s.Memory.Limits != "8Gi" || s.Memory.LimitsPct != 50 || s.Memory.Usage != "4Gi" || s.Memory.UsagePct != 25 {
		t.Errorf("Mem lim/usage = %+v", s.Memory)
	}
}

func TestSummarize_NoMetrics(t *testing.T) {
	nodes := []corev1.Node{node("n1", "4", "8Gi")}
	pods := []corev1.Pod{podWith(corev1.PodRunning, "1", "2Gi", "2", "4Gi")}
	s := Summarize(nodes, pods, nil)
	if s.MetricsAvailable {
		t.Error("expected MetricsAvailable=false with nil usage")
	}
	if s.CPU.Usage != "" || s.CPU.UsagePct != 0 {
		t.Errorf("expected empty usage, got %+v", s.CPU)
	}
	if s.CPU.Allocatable != "4.0" || s.CPU.RequestsPct != 25 {
		t.Errorf("CPU = %+v", s.CPU)
	}
}

func TestSummarize_ZeroAllocatableNoDivByZero(t *testing.T) {
	s := Summarize(nil, []corev1.Pod{podWith(corev1.PodRunning, "1", "1Gi", "1", "1Gi")}, nil)
	if s.CPU.RequestsPct != 0 || s.Memory.RequestsPct != 0 {
		t.Errorf("expected 0%% with no nodes, got cpu=%d mem=%d", s.CPU.RequestsPct, s.Memory.RequestsPct)
	}
}
