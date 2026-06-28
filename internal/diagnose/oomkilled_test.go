package diagnose

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestOOMKilledDetector_FiresOnCurrentState(t *testing.T) {
	facts := PodFacts{Pod: podOOMKilled("default", "cache", "redis", 137, false)}
	f := OOMKilledDetector{}.Detect(facts)
	if f == nil || f.Issue != "OOMKilled" {
		t.Fatalf("expected OOMKilled finding, got %+v", f)
	}
}

func TestOOMKilledDetector_FiresOnLastTerminationState(t *testing.T) {
	facts := PodFacts{Pod: podOOMKilled("default", "cache", "redis", 137, true)}
	if f := (OOMKilledDetector{}).Detect(facts); f == nil {
		t.Fatal("expected OOMKilled finding from LastTerminationState, got nil")
	}
}

func TestOOMKilledDetector_IgnoresCleanExit(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := (OOMKilledDetector{}).Detect(facts); f != nil {
		t.Errorf("expected nil, got %+v", f)
	}
}

func TestOOMKilledDetector_AttachesContainerResources(t *testing.T) {
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("4Gi")},
	}
	facts := PodFacts{Pod: podOOMKilledWithResources("cattle-system", "rancher", "rancher", res)}
	f := OOMKilledDetector{}.Detect(facts)
	if f == nil || f.Resources == nil {
		t.Fatalf("expected finding with resources, got %+v", f)
	}
	r := f.Resources
	if r.Container != "rancher" || r.MemRequest != "1Gi" || r.MemLimit != "4Gi" || r.CPURequest != "500m" || r.CPULimit != "3" {
		t.Errorf("resources = %+v", r)
	}
}

func TestOOMKilledDetector_UnsetLimitRendersUnset(t *testing.T) {
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	facts := PodFacts{Pod: podOOMKilledWithResources("ns", "p", "c", res)}
	f := OOMKilledDetector{}.Detect(facts)
	if f == nil || f.Resources == nil {
		t.Fatal("expected resources")
	}
	if f.Resources.MemLimit != "unset" || f.Resources.CPULimit != "unset" || f.Resources.CPURequest != "unset" {
		t.Errorf("expected unset for missing entries, got %+v", f.Resources)
	}
	if f.Resources.MemRequest != "1Gi" {
		t.Errorf("expected mem request 1Gi, got %+v", f.Resources)
	}
}
