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

func TestPendingDetector_ReasonNamesNoCause(t *testing.T) {
	facts := PodFacts{Pod: podUnschedulable("default", "web", "0/3 nodes are available: insufficient cpu")}

	f := PendingDetector{}.Detect(facts)

	if f == nil {
		t.Fatal("expected a finding")
	}
	// The reason states only what the detector established: nothing scheduled
	// the pod. Which of resources, taints or affinity blocked it is in
	// Evidence, quoted from the scheduler — the reason must not offer a guess
	// the detector never read.
	if want := "No node can schedule this pod"; f.Reason != want {
		t.Errorf("Reason = %q, want %q", f.Reason, want)
	}
}

func TestPendingDetector_IgnoresScheduledPods(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := (PendingDetector{}).Detect(facts); f != nil {
		t.Errorf("expected nil for a non-pending pod, got %+v", f)
	}
}
