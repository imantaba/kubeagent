package nodehealth

import "testing"

func TestAssess_CollectsUnhealthyAndCounts(t *testing.T) {
	probes := []Probe{
		{Node: "a", Status: "ok"},
		{Node: "b", Status: "unhealthy", Detail: "[-]pleg failed"},
		{Node: "c", Status: "forbidden"},
		{Node: "d", Status: "unreachable"},
	}
	rep := Assess(probes)
	if rep.Probed != 4 || rep.Forbidden != 1 {
		t.Fatalf("counts wrong: %+v", rep)
	}
	if len(rep.Unhealthy) != 1 || rep.Unhealthy[0].Node != "b" || rep.Unhealthy[0].Detail != "[-]pleg failed" {
		t.Errorf("want one unhealthy b, got %+v", rep.Unhealthy)
	}
}

func TestAssess_AllOKEmpty(t *testing.T) {
	rep := Assess([]Probe{{Node: "a", Status: "ok"}, {Node: "b", Status: "ok"}})
	if len(rep.Unhealthy) != 0 || rep.Forbidden != 0 || rep.Probed != 2 {
		t.Errorf("all ok -> no unhealthy: %+v", rep)
	}
}
