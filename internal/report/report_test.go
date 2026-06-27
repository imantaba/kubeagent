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
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, "", "text", &buf); err != nil {
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
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: ws}, "", "text", &buf); err != nil {
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
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		Cluster     clusterhealth.ClusterHealth `json:"cluster"`
		Workloads   []inventory.Workload        `json:"workloads"`
		Explanation string                      `json:"explanation"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not the workloads object: %v", err)
	}
	if len(got.Workloads) != 1 || got.Workloads[0].Name != "rancher" || got.Explanation != "" {
		t.Errorf("workloads object mismatch: %+v", got)
	}
	if got.Cluster.Verdict != "" {
		t.Errorf("expected zero-value cluster verdict, got %q", got.Cluster.Verdict)
	}
}

func TestPrintInventory_JSONIncludesExplanation(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, "rancher is fine", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `"explanation": "rancher is fine"`) {
		t.Errorf("expected explanation field:\n%s", buf.String())
	}
}

func TestPrintInventory_UnknownFormatErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{}, "", "xml", &buf); err == nil {
		t.Error("expected an error for unknown format")
	}
}

func TestPrintInventory_TextLeadsWithClusterVerdict(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleCluster(), inventory.Result{Workloads: sampleWorkloads()}, "", "text", &buf); err != nil {
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
	if err := PrintInventory(ch, inventory.Result{Workloads: sampleWorkloads()}, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Cluster: Healthy — 3/3 nodes Ready") {
		t.Errorf("expected healthy one-liner:\n%s", out)
	}
}

func TestPrintInventory_TextShowsScopeNote(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 3, NodesReady: 3, ScopeNote: "node health only — re-run without -n"}
	if err := PrintInventory(ch, inventory.Result{Workloads: sampleWorkloads()}, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "node health only") {
		t.Errorf("expected the scope note in output:\n%s", buf.String())
	}
}

func TestPrintInventory_TextJobOmitsCountShowsStatus(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "batch", Name: "migrate", Kind: "Job", Status: "Complete",
		Pods: []inventory.PodRow{{Name: "migrate-x", Phase: "Succeeded", Ready: "0/1"}},
	}}
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: ws}, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "batch/migrate") || !strings.Contains(out, "Job") || !strings.Contains(out, "Complete") {
		t.Errorf("expected job header with status:\n%s", out)
	}
	if strings.Contains(out, "0/0") {
		t.Errorf("job header should not show a 0/0 replica count:\n%s", out)
	}
}

func TestPrintInventory_TextCronJobShowsScheduleAndOmitted(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "batch", Name: "backup", Kind: "CronJob", Status: "Idle", Schedule: "0 2 * * *",
		Pods:        []inventory.PodRow{{Name: "backup-1"}, {Name: "backup-2"}, {Name: "backup-3"}},
		PodsOmitted: 5,
	}}
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: ws}, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "0 2 * * *") {
		t.Errorf("expected the cron schedule:\n%s", out)
	}
	if !strings.Contains(out, "+5 more pods") {
		t.Errorf("expected the omitted-pods note:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesCluster(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleCluster(), inventory.Result{Workloads: sampleWorkloads()}, "", "json", &buf); err != nil {
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

func TestPrintInventory_FooterHintListsHidden(t *testing.T) {
	var buf bytes.Buffer
	res := inventory.Result{Workloads: nil, HiddenRestarts: 3, HiddenCron: 5}
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 3, NodesReady: 3}
	if err := PrintInventory(ch, res, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "+3 restarted workloads (--include-restarts)") {
		t.Errorf("missing restart hint:\n%s", out)
	}
	if !strings.Contains(out, "+5 CronJobs (--include-cron)") {
		t.Errorf("missing cron hint:\n%s", out)
	}
}

func TestPrintInventory_AllClearWhenHealthyAndEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 2, NodesReady: 2}
	if err := PrintInventory(ch, inventory.Result{}, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("expected all-clear message:\n%s", buf.String())
	}
}

func TestPrintInventory_NoAllClearWhenDegraded(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 2, NodesReady: 1, NodeIssues: []string{"n2 NotReady"}}
	if err := PrintInventory(ch, inventory.Result{}, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "No issues found") {
		t.Errorf("should not claim all-clear when the cluster is degraded:\n%s", buf.String())
	}
}

func TestPrintInventory_NoFooterWhenNothingHidden(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}, inventory.Result{Workloads: sampleWorkloads()}, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "--include-") {
		t.Errorf("no footer expected when nothing is hidden:\n%s", buf.String())
	}
}
