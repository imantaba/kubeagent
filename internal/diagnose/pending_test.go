package diagnose

import (
	"strings"
	"testing"
)

func TestPendingDetector_FiresOnUnschedulable(t *testing.T) {
	facts := PodFacts{Pod: podUnschedulable("default", "web", "0/3 nodes are available: insufficient cpu")}

	f := PendingDetector{}.Detect(facts)

	if f == nil || f.Issue != "Unschedulable" {
		t.Fatalf("expected Unschedulable finding, got %+v", f)
	}
	if !strings.Contains(f.Evidence, "insufficient cpu") {
		t.Errorf("Evidence = %q, want the scheduler message", f.Evidence)
	}
}

func TestPendingDetector_IgnoresScheduledPods(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := (PendingDetector{}).Detect(facts); f != nil {
		t.Errorf("expected nil for a non-pending pod, got %+v", f)
	}
}
