package diagnose

import (
	"strings"
	"testing"
	"unicode/utf8"
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

// The ingress bound is safetext.MaxLine, applied to the waiting message
// before ImagePullDetector composes it into Evidence. A 1000-rune message
// produces an Evidence of exactly 529 runes: the 17-rune `container "app": `
// prefix plus the 512-rune safetext.MaxLine budget.
func TestImagePullDetector_CapsAThousandRuneMessageAt529RuneEvidence(t *testing.T) {
	msg := strings.Repeat("x", 1000)
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ErrImagePull", msg)}

	f := ImagePullDetector{}.Detect(facts)

	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if n := utf8.RuneCountInString(f.Evidence); n != 529 {
		t.Errorf("Evidence = %d runes, want 529 (17-rune prefix + safetext.MaxLine)", n)
	}
	if !strings.HasSuffix(f.Evidence, "…") {
		t.Errorf("Evidence = %q, want it to end in the safetext ellipsis", f.Evidence)
	}
}

func TestImagePullDetector_IgnoresRunningContainers(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := (ImagePullDetector{}).Detect(facts); f != nil {
		t.Errorf("expected nil, got %+v", f)
	}
}

// R228: the failing container's image is captured on the Finding itself, not
// just implied by the workload's display image — internal/rootcause keys its
// registry-outage grouping on it.
func TestImagePullDetector_SetsImage(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ImagePullBackOff", "")}
	facts.Pod.Status.ContainerStatuses[0].Image = "example.com/app:bad"

	f := ImagePullDetector{}.Detect(facts)

	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Image != "example.com/app:bad" {
		t.Errorf("Image = %q, want %q", f.Image, "example.com/app:bad")
	}
}
