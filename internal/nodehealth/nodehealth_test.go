package nodehealth

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAssess_CollectsUnhealthyAndCounts(t *testing.T) {
	probes := []Probe{
		{Node: "a", Status: "ok"},
		{Node: "b", Status: "unhealthy", Detail: "[-]pleg failed"},
		{Node: "c", Status: "forbidden"},
		{Node: "d", Status: "unreachable"},
	}
	rep := Assess(probes)
	if rep.Probed != 4 || rep.Forbidden != 1 || rep.Unreachable != 1 {
		t.Fatalf("counts wrong: %+v", rep)
	}
	if len(rep.Unhealthy) != 1 || rep.Unhealthy[0].Node != "b" || rep.Unhealthy[0].Detail != "[-]pleg failed" {
		t.Errorf("want one unhealthy b, got %+v", rep.Unhealthy)
	}
}

func TestAssess_AllOKEmpty(t *testing.T) {
	rep := Assess([]Probe{{Node: "a", Status: "ok"}, {Node: "b", Status: "ok"}})
	if len(rep.Unhealthy) != 0 || rep.Forbidden != 0 || rep.Unreachable != 0 || rep.Probed != 2 {
		t.Errorf("all ok -> no unhealthy: %+v", rep)
	}
}

// TestAssess_ThreeOKOneUnreachableTwoForbidden pins the exact fixture WP7's
// brief specifies for the new Unreachable count, independent of the other
// two tests' shapes.
func TestAssess_ThreeOKOneUnreachableTwoForbidden(t *testing.T) {
	probes := []Probe{
		{Node: "a", Status: "ok"},
		{Node: "b", Status: "ok"},
		{Node: "c", Status: "ok"},
		{Node: "d", Status: "unreachable"},
		{Node: "e", Status: "forbidden"},
		{Node: "f", Status: "forbidden"},
	}
	rep := Assess(probes)
	want := Report{Probed: 6, Forbidden: 2, Unreachable: 1}
	if rep.Probed != want.Probed || rep.Forbidden != want.Forbidden || rep.Unreachable != want.Unreachable || len(rep.Unhealthy) != 0 {
		t.Fatalf("Assess() = %+v, want %+v", rep, want)
	}
}

// TestReport_UnreachableOmitemptyInJSON pins the wire shape: a zero
// Unreachable count must not appear in the encoded JSON at all (so an older
// consumer sees an unchanged document), and a non-zero count must.
func TestReport_UnreachableOmitemptyInJSON(t *testing.T) {
	zero := Report{Probed: 2, Forbidden: 0}
	b, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(b), "unreachable") {
		t.Errorf("zero Unreachable must be omitted, got %s", b)
	}

	nonzero := Report{Probed: 3, Unreachable: 1}
	b, err = json.Marshal(nonzero)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(b), `"unreachable":1`) {
		t.Errorf("non-zero Unreachable must be present, got %s", b)
	}
}
