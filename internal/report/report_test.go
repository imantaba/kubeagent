package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func sampleWorkloads() []inventory.Workload {
	return []inventory.Workload{{
		Namespace: "cattle-system", Name: "rancher", Kind: "Deployment",
		Desired: 3, Ready: 3, Status: "Running", Restarts: 64, LastRestart: "2026-06-02T08:14:03Z",
		Image: "rancher/rancher:v2.14.1",
		Pods: []inventory.PodRow{
			{Name: "rancher-64smq", Phase: "Running", Ready: "1/1", Restarts: 31, LastRestart: "2026-06-02T08:14:03Z", Node: "nova-worker-3", IP: "10.42.4.41", Age: "36d", Image: "rancher/rancher:v2.14.1"},
		},
	}}
}

func sampleCluster() clusterhealth.ClusterHealth {
	return clusterhealth.ClusterHealth{
		Verdict: "Degraded", NodesTotal: 3, NodesReady: 2,
		NodeIssues:   []string{"nova-worker-2 NotReady"},
		SystemIssues: []string{"kube-system/coredns 1/2 Degraded"},
	}
}

func TestPrintInventory_TextShowsWorkloadAndPods(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, sampleWorkloads(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"cattle-system/rancher", "Deployment", "3/3", "Running", "64", "rancher-64smq", "nova-worker-3"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}

func TestPrintInventory_TextFlagsWorkloadWithFinding(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "kube-system", Name: "coredns", Kind: "Deployment", Desired: 2, Ready: 1, Status: "Degraded",
		Findings: []diagnose.Finding{{Pod: "kube-system/coredns-x", Issue: "CrashLoopBackOff", Reason: "boom", Evidence: "e"}},
	}}
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, ws, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "CrashLoopBackOff") || !strings.Contains(out, "Degraded") {
		t.Errorf("expected the finding + Degraded to show:\n%s", out)
	}
	if !strings.Contains(out, "⚠") {
		t.Errorf("expected the ⚠ flag symbol on a flagged workload:\n%s", out)
	}
}

func TestPrintInventory_JSONObjectWithWorkloads(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, sampleWorkloads(), "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		Workloads   []inventory.Workload `json:"workloads"`
		Explanation string               `json:"explanation"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not the workloads object: %v", err)
	}
	if len(got.Workloads) != 1 || got.Workloads[0].Name != "rancher" || got.Explanation != "" {
		t.Errorf("workloads object mismatch: %+v", got)
	}
}

func TestPrintInventory_JSONIncludesExplanation(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, sampleWorkloads(), "rancher is fine", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `"explanation": "rancher is fine"`) {
		t.Errorf("expected explanation field:\n%s", buf.String())
	}
}

func TestPrintInventory_UnknownFormatErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, nil, "", "xml", &buf); err == nil {
		t.Error("expected an error for unknown format")
	}
}

func TestPrintInventory_TextLeadsWithClusterVerdict(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleCluster(), sampleWorkloads(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Cluster: Degraded", "2/3 nodes Ready", "nova-worker-2 NotReady", "kube-system/coredns 1/2 Degraded"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
	// The verdict must come before the workload inventory.
	if strings.Index(out, "Cluster: Degraded") > strings.Index(out, "cattle-system/rancher") {
		t.Error("cluster verdict should be printed before the inventory")
	}
}

func TestPrintInventory_TextHealthyClusterSingleLine(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 3, NodesReady: 3}
	if err := PrintInventory(ch, sampleWorkloads(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Cluster: Healthy — 3/3 nodes Ready") {
		t.Errorf("expected healthy one-liner:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesCluster(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleCluster(), sampleWorkloads(), "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		Cluster   clusterhealth.ClusterHealth `json:"cluster"`
		Workloads []inventory.Workload        `json:"workloads"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not the expected object: %v", err)
	}
	if got.Cluster.Verdict != "Degraded" || got.Cluster.NodesReady != 2 || len(got.Workloads) != 1 {
		t.Errorf("cluster/workloads mismatch: %+v", got)
	}
}
