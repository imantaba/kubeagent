package policypack_test

import (
	"testing"
)

// TestSecurityPackKindDistribution pins the pack's scope decision: the full
// rule set runs against Deployment, and only the two host-escape claims spill
// over to the kinds where they bite as hard. It is not a cross-product, and it
// deliberately never selects Pod or Job — a controller-owned pod repeats its
// workload's violation once per replica, and a report naming one Deployment is
// actionable in a way that a report naming forty pods is not.
func TestSecurityPackKindDistribution(t *testing.T) {
	byKind := map[string]int{}
	for _, r := range loadPack(t, "security") {
		byKind[r.Match.Kind]++
	}

	want := map[string]int{
		"Deployment":  18,
		"StatefulSet": 2,
		"DaemonSet":   2,
		"CronJob":     1,
	}
	for kind, n := range want {
		if byKind[kind] != n {
			t.Errorf("%d rules select %s, want %d", byKind[kind], kind, n)
		}
	}
	for kind := range byKind {
		if _, ok := want[kind]; !ok {
			t.Errorf("the pack selects %s, which is not one of the four workload kinds it is scoped to", kind)
		}
	}

	total := 0
	for _, n := range byKind {
		total += n
	}
	if total != 23 {
		t.Errorf("the pack holds %d rules, want 23", total)
	}
}
