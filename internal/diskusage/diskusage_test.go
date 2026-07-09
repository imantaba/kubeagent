package diskusage

import "testing"

func gib(n int64) int64 { return n << 30 }

func TestAssess_NodeOverAndUnder(t *testing.T) {
	stats := []NodeSummary{
		{Node: "n1", FSUsed: gib(170), FSCap: gib(200)}, // 85% -> over
		{Node: "n2", FSUsed: gib(50), FSCap: gib(200)},  // 25% -> under
	}
	r := Assess(stats, 0.80)
	if len(r.Over) != 1 || r.Over[0].Kind != "node" || r.Over[0].Name != "n1" {
		t.Fatalf("want only n1 over, got %+v", r.Over)
	}
	if len(r.Nodes) != 2 {
		t.Errorf("want all-node ratios for the metric, got %d", len(r.Nodes))
	}
	if r.Threshold != 0.80 {
		t.Errorf("want threshold echoed, got %v", r.Threshold)
	}
}

func TestAssess_PVCOverAndSkipZeroCap(t *testing.T) {
	stats := []NodeSummary{{
		Node: "n1", FSUsed: gib(1), FSCap: gib(100), // 1% under
		Volumes: []PVCVolume{
			{Namespace: "shop", Name: "data", Used: gib(46), Cap: gib(50)}, // 92% over
			{Namespace: "shop", Name: "cache", Used: gib(1), Cap: gib(50)}, // 2% under
			{Namespace: "shop", Name: "nostat", Used: 0, Cap: 0},           // no capacity -> skipped
		},
	}}
	r := Assess(stats, 0.80)
	if len(r.Over) != 1 || r.Over[0].Kind != "pvc" || r.Over[0].Name != "data" {
		t.Fatalf("want only shop/data over, got %+v", r.Over)
	}
	if r.Over[0].Namespace != "shop" || r.Over[0].CapacityBytes != gib(50) {
		t.Errorf("wrong pvc row: %+v", r.Over[0])
	}
}

func TestAssess_SortsByRatioDesc(t *testing.T) {
	stats := []NodeSummary{{
		Node: "n1", FSUsed: gib(90), FSCap: gib(100), // node 90%
		Volumes: []PVCVolume{{Namespace: "a", Name: "p", Used: gib(95), Cap: gib(100)}}, // pvc 95%
	}}
	r := Assess(stats, 0.80)
	if len(r.Over) != 2 || r.Over[0].Ratio < r.Over[1].Ratio {
		t.Fatalf("want highest ratio first, got %+v", r.Over)
	}
	if r.Over[0].Name != "p" {
		t.Errorf("want pvc p (95%%) first, got %q", r.Over[0].Name)
	}
}

func TestAssess_EmptyWhenNoneOver(t *testing.T) {
	r := Assess([]NodeSummary{{Node: "n1", FSUsed: gib(1), FSCap: gib(100)}}, 0.80)
	if len(r.Over) != 0 {
		t.Errorf("want no over-threshold entries, got %+v", r.Over)
	}
	if len(r.Nodes) != 1 {
		t.Errorf("all-node ratios still populated, got %d", len(r.Nodes))
	}
}
