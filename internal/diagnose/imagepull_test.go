package diagnose

import (
	"strings"
	"testing"
)

func TestImagePullDetector_FiresOnErrImagePull(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ErrImagePull", `rpc error: pull "x:typo" not found`)}

	f := ImagePullDetector{}.Detect(facts)

	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Issue != "ErrImagePull" {
		t.Errorf("Issue = %q, want ErrImagePull", f.Issue)
	}
	if !strings.Contains(f.Evidence, "not found") {
		t.Errorf("Evidence = %q, want it to include the waiting message", f.Evidence)
	}
}

func TestImagePullDetector_FiresOnImagePullBackOff(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ImagePullBackOff", "")}
	if f := (ImagePullDetector{}).Detect(facts); f == nil || f.Issue != "ImagePullBackOff" {
		t.Fatalf("expected ImagePullBackOff finding, got %+v", f)
	}
}

func TestImagePullDetector_IgnoresRunningContainers(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := (ImagePullDetector{}).Detect(facts); f != nil {
		t.Errorf("expected nil, got %+v", f)
	}
}
