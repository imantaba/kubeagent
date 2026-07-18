package diagnose

import "testing"

func TestCrashLoopDetector_FiresOnCrashLoopBackOff(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "CrashLoopBackOff", "")}

	f := CrashLoopDetector{}.Detect(facts)

	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Issue != "CrashLoopBackOff" {
		t.Errorf("Issue = %q, want CrashLoopBackOff", f.Issue)
	}
	if f.Pod != "default/web" {
		t.Errorf("Pod = %q, want default/web", f.Pod)
	}
}

func TestCrashLoopDetector_IgnoresOtherWaitingReasons(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := (CrashLoopDetector{}).Detect(facts); f != nil {
		t.Errorf("expected nil for non-crashloop pod, got %+v", f)
	}
}

func TestCrashLoopDetector_SetsContainer(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "CrashLoopBackOff", "")}
	f := CrashLoopDetector{}.Detect(facts)
	if f == nil || f.Container != "app" {
		t.Fatalf("expected Container=\"app\", got %+v", f)
	}
}
