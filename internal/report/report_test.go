package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/svchealth"
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
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, "", "text", &buf); err != nil {
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
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: ws}, nil, nil, nil, "", "text", &buf); err != nil {
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
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, "", "json", &buf); err != nil {
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
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, "rancher is fine", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `"explanation": "rancher is fine"`) {
		t.Errorf("expected explanation field:\n%s", buf.String())
	}
}

func TestPrintInventory_UnknownFormatErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{}, nil, nil, nil, "", "xml", &buf); err == nil {
		t.Error("expected an error for unknown format")
	}
}

func TestPrintInventory_TextLeadsWithClusterVerdict(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleCluster(), inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, "", "text", &buf); err != nil {
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
	if err := PrintInventory(ch, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, "", "text", &buf); err != nil {
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
	if err := PrintInventory(ch, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, "", "text", &buf); err != nil {
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
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: ws}, nil, nil, nil, "", "text", &buf); err != nil {
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
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: ws}, nil, nil, nil, "", "text", &buf); err != nil {
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
	if err := PrintInventory(sampleCluster(), inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, "", "json", &buf); err != nil {
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
	if err := PrintInventory(ch, res, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "+3 restarted workloads (--include-restarts)") {
		t.Errorf("missing restart hint:\n%s", out)
	}
	if !strings.Contains(out, "+5 CronJobs (--include-cron)") {
		t.Errorf("missing cron hint:\n%s", out)
	}
	if !strings.Contains(out, "+3 restarted workloads (--include-restarts) · +5 CronJobs (--include-cron)") {
		t.Errorf("footer parts should be joined by ' · ':\n%s", out)
	}
}

func TestPrintInventory_FooterShownAndNoAllClearWhenDegraded(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 2, NodesReady: 1, NodeIssues: []string{"n2 NotReady"}}
	res := inventory.Result{HiddenRestarts: 2}
	if err := PrintInventory(ch, res, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "+2 restarted workloads (--include-restarts)") {
		t.Errorf("footer should appear even when degraded:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("must not claim all-clear when degraded:\n%s", out)
	}
}

func TestPrintInventory_AllClearWhenHealthyAndEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 2, NodesReady: 2}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("expected all-clear message:\n%s", buf.String())
	}
}

func TestPrintInventory_NoAllClearWhenDegraded(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 2, NodesReady: 1, NodeIssues: []string{"n2 NotReady"}}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "No issues found") {
		t.Errorf("should not claim all-clear when the cluster is degraded:\n%s", buf.String())
	}
}

func TestPrintInventory_NoFooterWhenNothingHidden(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "--include-") {
		t.Errorf("no footer expected when nothing is hidden:\n%s", buf.String())
	}
}

func sampleSummary() *resources.Summary {
	return &resources.Summary{
		MetricsAvailable: true,
		CPU:              resources.Line{Allocatable: "8.0", Requests: "2.0", Limits: "4.0", Usage: "2.0", RequestsPct: 25, LimitsPct: 50, UsagePct: 25},
		Memory:           resources.Line{Allocatable: "16Gi", Requests: "4Gi", Limits: "8Gi", Usage: "4Gi", RequestsPct: 25, LimitsPct: 50, UsagePct: 25},
	}
}

func TestPrintInventory_TextShowsResourceBlock(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, sampleSummary(), nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Resources (cluster):", "8.0 cores", "req 2.0 (25%)", "lim 4.0 (50%)", "used 2.0 (25%)", "16Gi", "used 4Gi (25%)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestPrintInventory_ResourceBlockPrecedesWorkloads(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{Workloads: sampleWorkloads()}, sampleSummary(), nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if strings.Index(out, "Resources (cluster):") > strings.Index(out, "cattle-system/rancher") {
		t.Errorf("resource block should print before the workload list:\n%s", out)
	}
}

func TestPrintInventory_TextResourceBlockNoMetrics(t *testing.T) {
	var buf bytes.Buffer
	s := sampleSummary()
	s.MetricsAvailable = false
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, s, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "metrics-server unavailable") {
		t.Errorf("expected unavailable note:\n%s", out)
	}
	if strings.Contains(out, "used ") {
		t.Errorf("should not show usage without metrics:\n%s", out)
	}
}

func TestPrintInventory_TextOOMFindingShowsResources(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "cattle-system", Name: "rancher", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{
			Pod: "cattle-system/rancher-x", Issue: "OOMKilled", Reason: "Container exceeded its memory limit and was killed", Evidence: "e",
			Resources: &diagnose.ContainerResources{Container: "rancher", CPURequest: "500m", CPULimit: "3", MemRequest: "1Gi", MemLimit: "4Gi"},
		}},
	}}
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: ws}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "memory req=1Gi limit=4Gi") || !strings.Contains(out, "cpu req=500m limit=3") {
		t.Errorf("expected per-OOM resources line:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesResources(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, sampleSummary(), nil, nil, "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		Resources *resources.Summary `json:"resources"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Resources == nil || got.Resources.CPU.Allocatable != "8.0" || !got.Resources.MetricsAvailable {
		t.Errorf("resources missing/wrong in JSON: %+v", got.Resources)
	}
}

func sampleFacts() *platform.Facts {
	return &platform.Facts{
		CNI: "Cilium", Ingress: "Traefik",
		Storage:     []platform.Storage{{Name: "Hetzner CSI", Default: true}, {Name: "NFS CSI"}},
		KubeVersion: "v1.35", Distro: "RKE2", Runtime: "containerd", Cloud: "Hetzner Cloud",
	}
}

func TestPrintInventory_TextShowsPlatformLine(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, sampleFacts(), nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	want := "Platform: Cilium CNI · Traefik ingress · Hetzner CSI (+NFS CSI) storage · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud"
	if !strings.Contains(out, want) {
		t.Errorf("missing platform line %q:\n%s", want, out)
	}
	// Platform must appear under the verdict (before any workloads / resources).
	if strings.Index(out, "Platform:") < strings.Index(out, "Cluster: Healthy") {
		t.Errorf("platform line should follow the cluster verdict:\n%s", out)
	}
}

func TestPrintInventory_TextOmitsPlatformWhenNilOrEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "Platform:") {
		t.Errorf("no platform line expected for nil facts:\n%s", buf.String())
	}
	var buf2 bytes.Buffer
	if err := PrintInventory(ch, inventory.Result{}, nil, &platform.Facts{}, nil, "", "text", &buf2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf2.String(), "Platform:") {
		t.Errorf("no platform line expected for empty facts:\n%s", buf2.String())
	}
}

func TestPrintInventory_JSONIncludesPlatform(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, sampleFacts(), nil, "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		Platform *platform.Facts `json:"platform"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Platform == nil || got.Platform.CNI != "Cilium" || len(got.Platform.Storage) != 2 {
		t.Errorf("platform missing/wrong in JSON: %+v", got.Platform)
	}
}

func sampleServiceIssues() []svchealth.Issue {
	return []svchealth.Issue{
		{Namespace: "default", Name: "web", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"},
		{Namespace: "default", Name: "api-lb", Type: "LoadBalancer", Problem: "NoExternalAddress", Detail: "no external address", Since: "2026-06-29T00:00:00Z"},
	}
}

func TestPrintInventory_TextShowsServiceIssues(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, sampleServiceIssues(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Service issues:", "default/web", "ClusterIP", "no ready endpoints", "default/api-lb", "LoadBalancer", "no external address", "no external address · "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestPrintInventory_TextNoServiceSectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "Service issues:") {
		t.Errorf("no Service issues section expected when empty:\n%s", buf.String())
	}
}

func TestPrintInventory_ServiceIssuesSuppressAllClear(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, sampleServiceIssues(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "No issues found") {
		t.Errorf("all-clear must not print when there are service issues:\n%s", buf.String())
	}
}

func TestPrintInventory_ServiceSectionFollowsWorkloads(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, sampleServiceIssues(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if strings.Index(out, "cattle-system/rancher") > strings.Index(out, "Service issues:") {
		t.Errorf("workloads should precede the Service issues section:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesServiceIssues(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, sampleServiceIssues(), "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		ServiceIssues []svchealth.Issue `json:"serviceIssues"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.ServiceIssues) != 2 || got.ServiceIssues[0].Name != "web" {
		t.Errorf("serviceIssues missing/wrong in JSON: %+v", got.ServiceIssues)
	}
}

func TestPrintInventory_TextShowsNetworkPolicyHint(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "default", Name: "api", Kind: "Deployment", Desired: 2, Ready: 0, Status: "Degraded",
		NetworkPolicies: []string{"deny-all", "web-allow"},
		Pods:            []inventory.PodRow{{Name: "api-1", Phase: "Running", Ready: "0/1"}},
	}}
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: ws}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "NetworkPolicy: pods selected by deny-all, web-allow") {
		t.Errorf("expected NP hint line:\n%s", buf.String())
	}
}

func TestPrintInventory_TextNoNetworkPolicyHintWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "NetworkPolicy:") {
		t.Errorf("no NP hint expected when the workload has none:\n%s", buf.String())
	}
}
