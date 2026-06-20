package diagnose

import "testing"

// stubDetector lets us test Run without any real pod logic.
type stubDetector struct{ result *Finding }

func (s stubDetector) Detect(facts PodFacts) *Finding { return s.result }

func TestRunCollectsFindingsFromMatchingDetectors(t *testing.T) {
	hit := stubDetector{result: &Finding{Pod: "ns/p", Issue: "X"}}
	miss := stubDetector{result: nil}

	facts := []PodFacts{{}, {}} // two pods

	got := Run([]Detector{hit, miss}, facts)

	if len(got) != 2 {
		t.Fatalf("expected 2 findings (the hit detector fires once per pod), got %d", len(got))
	}
	if got[0].Issue != "X" {
		t.Errorf("Issue = %q, want X", got[0].Issue)
	}
}
