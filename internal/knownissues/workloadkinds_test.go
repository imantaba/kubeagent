package knownissues

import (
	"sort"
	"testing"
)

// The three names are pinned literally. WorkloadKinds is a claim about what
// kubeagent emits, not a preference, so a name added or removed here must be a
// deliberate edit to this test — which is what forces a reader to go and check
// that scan really did gain or lose a workload pass.
func TestWorkloadKindsIsExactlyTheThreeWorkloadPasses(t *testing.T) {
	want := []string{"FailedCreate", "JobFailed", "RolloutStuck"}
	got := WorkloadKinds()
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("WorkloadKinds() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("WorkloadKinds()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The two lists must not overlap. A kind in both would make the CLI's two
// messages contradict each other: Lookup would find an entry and print it,
// while IsWorkloadKind would say the reference does not cover it.
func TestWorkloadKindsAreNotDocumentedKinds(t *testing.T) {
	for _, k := range WorkloadKinds() {
		if _, ok := Lookup(k); ok {
			t.Errorf("%q is both a documented kind and a workload kind", k)
		}
	}
}

// IsWorkloadKind matches byte for byte, for the same reason Lookup does: the
// caller passes a Finding.Issue value verbatim, and a near miss is a typo.
func TestIsWorkloadKindIsExact(t *testing.T) {
	if !IsWorkloadKind("RolloutStuck") {
		t.Error(`IsWorkloadKind("RolloutStuck") = false, want true`)
	}
	for _, miss := range []string{"rolloutstuck", "RolloutStuck ", "Rollout", "CrashLoopBackOff", ""} {
		if IsWorkloadKind(miss) {
			t.Errorf("IsWorkloadKind(%q) = true, want false", miss)
		}
	}
}

// The returned slice must share nothing with the package's own, on the same
// rule All and Lookup follow: a caller that sorts or truncates what it is
// handed must not rewrite the list for the next caller.
func TestWorkloadKindsReturnsACopy(t *testing.T) {
	first := WorkloadKinds()
	first[0] = "clobbered"
	if second := WorkloadKinds(); second[0] == "clobbered" {
		t.Error("WorkloadKinds returned the package's own slice; a caller can rewrite the list")
	}
}
