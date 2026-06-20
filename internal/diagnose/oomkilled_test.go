package diagnose

import "testing"

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
