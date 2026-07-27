package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/capacity"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
)

func sampleCapacity() *capacity.Report {
	return &capacity.Report{
		Headroom: &capacity.Headroom{
			IncludedNodes: 3, TotalNodes: 5,
			FreeCPU: "5.9", FreeMemory: "108Gi",
			LargestCPUFit: &capacity.NodeFit{Node: "worker1", CPU: "2.4", Memory: "42Gi"},
			TightestNode:  &capacity.TightNode{Node: "worker2", Resource: "CPU", Pct: 92},
			NodeLoss: &capacity.NodeLoss{
				Node: "worker1", Fits: false, Placed: 4,
				Blocker: "StatefulSet/prod/db", BlockerCPU: "2.1",
			},
			Excluded: []capacity.NodeExclusion{
				{Node: "control-plane-1", Reason: "NoSchedule taint"},
				{Node: "worker3", Reason: "cordoned"},
			},
		},
		RightSizing: &capacity.RightSizing{
			MetricsAvailable: true, PodsReporting: 14, PodsTotal: 16,
			Rules: []capacity.Rule{
				{Name: capacity.RuleNoRequests, Owners: []capacity.Owner{
					{Kind: "Deployment", Namespace: "staging", Name: "web",
						Observed: "0.0 cores", BestEffort: true},
				}},
				{Name: capacity.RuleNeverSchedulable, Owners: []capacity.Owner{
					{Kind: "Job", Namespace: "batch", Name: "trainer",
						Detail: "req 40.0 cores > largest node (16.0 cores)"},
				}, Truncated: 3},
			},
		},
	}
}

func TestPrintCapacity(t *testing.T) {
	var buf bytes.Buffer
	if err := printCapacity(sampleCapacity(), &buf); err != nil {
		t.Fatalf("printCapacity: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"CAPACITY  (advisory",
		"  Headroom",
		"5.9 cores, 108Gi free across 3 of 5 nodes",
		"worker1  2.4 cores, 42Gi",
		"worker2  92% of CPU requested",
		"may not fit — first-fit could not place StatefulSet/prod/db (2.1 cores)",
		"control-plane-1  (NoSchedule taint)",
		"worker3  (cordoned)",
		"Right-sizing  (metrics-server: 14 of 16 pods reporting)",
		"no requests set",
		"Deployment/staging/web",
		"BestEffort: first evicted under pressure",
		"never schedulable",
		"… +3 more",
		"one sample per pod, ~30s average — not a peak, not a history",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

// FFD failure licenses "may not fit" only. A flat "does not fit" would claim the
// heuristic proved something it cannot.
func TestPrintCapacityNeverSaysDoesNotFit(t *testing.T) {
	var buf bytes.Buffer
	if err := printCapacity(sampleCapacity(), &buf); err != nil {
		t.Fatalf("printCapacity: %v", err)
	}
	if strings.Contains(buf.String(), "does not fit") {
		t.Error(`rendered "does not fit"; first-fit failure only licenses "may not fit"`)
	}
}

func TestPrintCapacityFitsAndSingleNode(t *testing.T) {
	rep := &capacity.Report{Headroom: &capacity.Headroom{
		IncludedNodes: 2, TotalNodes: 2, FreeCPU: "4.0", FreeMemory: "16Gi",
		NodeLoss: &capacity.NodeLoss{Node: "worker1", Fits: true, Placed: 7},
	}}
	var buf bytes.Buffer
	if err := printCapacity(rep, &buf); err != nil {
		t.Fatalf("printCapacity: %v", err)
	}
	if !strings.Contains(buf.String(), "fits — first-fit placed all 7 pods") {
		t.Errorf("want the fits wording, got:\n%s", buf.String())
	}

	single := &capacity.Report{Headroom: &capacity.Headroom{
		IncludedNodes: 1, TotalNodes: 1, FreeCPU: "4.0", FreeMemory: "16Gi",
		NodeLoss: &capacity.NodeLoss{Node: "only", SingleNode: true},
	}}
	buf.Reset()
	if err := printCapacity(single, &buf); err != nil {
		t.Fatalf("printCapacity: %v", err)
	}
	if !strings.Contains(buf.String(), "single node — no node-loss arithmetic possible") {
		t.Errorf("want the single-node wording, got:\n%s", buf.String())
	}
}

func TestPrintCapacityAbsentMetrics(t *testing.T) {
	rep := &capacity.Report{RightSizing: &capacity.RightSizing{
		MetricsAvailable: false, PodsTotal: 9,
		Rules: []capacity.Rule{{Name: capacity.RuleNoRequests,
			Owners: []capacity.Owner{{Kind: "Pod", Namespace: "default", Name: "loose"}}}},
	}}
	var buf bytes.Buffer
	if err := printCapacity(rep, &buf); err != nil {
		t.Fatalf("printCapacity: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "metrics-server unavailable — structural rules only") {
		t.Errorf("want the unavailable line, got:\n%s", out)
	}
	if !strings.Contains(out, "Pod/default/loose") {
		t.Error("want the structural row to render anyway")
	}
	if strings.Contains(out, "not a peak") {
		t.Error("want no sample footer when there is no sample")
	}
}

func TestPrintCapacitySkipsEmpty(t *testing.T) {
	for name, rep := range map[string]*capacity.Report{
		"nil":   nil,
		"empty": {},
	} {
		var buf bytes.Buffer
		if err := printCapacity(rep, &buf); err != nil {
			t.Fatalf("%s: printCapacity: %v", name, err)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: want no output, got %q", name, buf.String())
		}
	}
}

func TestPrintInventoryJSONIncludesCapacity(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:  clusterhealth.ClusterHealth{Verdict: "Healthy"},
		Capacity: sampleCapacity(),
	}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatalf("PrintInventory: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	raw, ok := got["capacity"]
	if !ok {
		t.Fatalf("want a capacity key in --output json, got keys %v", keysOf(got))
	}
	if !strings.Contains(string(raw), `"headroom"`) {
		t.Errorf("want the headroom block encoded, got %s", raw)
	}
}

func TestPrintInventoryJSONOmitsCapacityWhenNil(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"}}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatalf("PrintInventory: %v", err)
	}
	if strings.Contains(buf.String(), `"capacity"`) {
		t.Error("a default scan's JSON must be unchanged")
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
