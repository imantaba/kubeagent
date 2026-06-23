package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

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

func TestPrintInventory_TextShowsWorkloadAndPods(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleWorkloads(), "", "text", &buf); err != nil {
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
	if err := PrintInventory(ws, "", "text", &buf); err != nil {
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
	if err := PrintInventory(sampleWorkloads(), "", "json", &buf); err != nil {
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
	if err := PrintInventory(sampleWorkloads(), "rancher is fine", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `"explanation": "rancher is fine"`) {
		t.Errorf("expected explanation field:\n%s", buf.String())
	}
}

func TestPrintInventory_UnknownFormatErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(nil, "", "xml", &buf); err == nil {
		t.Error("expected an error for unknown format")
	}
}
