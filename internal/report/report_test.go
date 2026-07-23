package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/certhealth"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/controlplane"
	"github.com/imantaba/kubeagent/internal/credlint"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/dnshealth"
	"github.com/imantaba/kubeagent/internal/hpahealth"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/pdbhealth"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/pvchealth"
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
	"github.com/imantaba/kubeagent/internal/quotahealth"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/secscan"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/termhealth"
	"github.com/imantaba/kubeagent/internal/webhookhealth"
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
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: sampleWorkloads()}}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "CrashLoopBackOff") || !strings.Contains(out, "Degraded") {
		t.Errorf("expected the finding + Degraded to show:\n%s", out)
	}
	if !strings.Contains(out, "✗ kube-system/coredns") {
		t.Errorf("expected the ✗ flag on the flagged workload header:\n%s", out)
	}
}

func TestPrintInventory_JSONObjectWithWorkloads(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: sampleWorkloads()}}, "json", &buf); err != nil {
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
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: sampleWorkloads()}, Explanation: "rancher is fine"}, "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `"explanation": "rancher is fine"`) {
		t.Errorf("expected explanation field:\n%s", buf.String())
	}
}

func TestPrintInventory_UnknownFormatErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{}, "xml", &buf); err == nil {
		t.Error("expected an error for unknown format")
	}
}

func TestPrintInventory_TextLeadsWithClusterVerdict(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{Cluster: sampleCluster(), Result: inventory.Result{Workloads: sampleWorkloads()}}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Cluster: ch, Result: inventory.Result{Workloads: sampleWorkloads()}}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Cluster: ch, Result: inventory.Result{Workloads: sampleWorkloads()}}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Cluster: sampleCluster(), Result: inventory.Result{Workloads: sampleWorkloads()}}, "json", &buf); err != nil {
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
	if err := PrintInventory(Input{Cluster: ch, Result: res}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Cluster: ch, Result: res}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Cluster: ch}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("expected all-clear message:\n%s", buf.String())
	}
}

func TestPrintInventory_NoAllClearWhenDegraded(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 2, NodesReady: 1, NodeIssues: []string{"n2 NotReady"}}
	if err := PrintInventory(Input{Cluster: ch}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "No issues found") {
		t.Errorf("should not claim all-clear when the cluster is degraded:\n%s", buf.String())
	}
}

func TestPrintInventory_NoFooterWhenNothingHidden(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, Result: inventory.Result{Workloads: sampleWorkloads()}}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Cluster: ch, Resources: sampleSummary()}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Resources (cluster):", "8.0 cores", "req 2.0 (25%)", "lim 4.0 (50%)", "used 2.0 (25%)", "16Gi", "used 4Gi (25%)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestPrintInventory_ResourceBlockFollowsWorkloads(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, Result: inventory.Result{Workloads: sampleWorkloads()}, Resources: sampleSummary()}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	// Resources now render in CONTEXT, which comes after NEEDS ATTENTION (workloads).
	if strings.Index(out, "Resources (cluster):") < strings.Index(out, "cattle-system/rancher") {
		t.Errorf("resource block should print after the workload list (CONTEXT zone):\n%s", out)
	}
}

func TestPrintInventory_TextResourceBlockNoMetrics(t *testing.T) {
	var buf bytes.Buffer
	s := sampleSummary()
	s.MetricsAvailable = false
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, Resources: s}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "text", &buf); err != nil {
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
	if err := PrintInventory(Input{Cluster: ch, Resources: sampleSummary()}, "json", &buf); err != nil {
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
	if err := PrintInventory(Input{Cluster: ch, Platform: sampleFacts()}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	want := "Platform: Cilium CNI · Traefik ingress · Hetzner CSI (+NFS CSI) storage · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud"
	if !strings.Contains(out, want) {
		t.Errorf("missing platform line %q:\n%s", want, out)
	}
	// Platform is reference material — it renders in the CONTEXT zone, after the verdict.
	if strings.Index(out, "Platform:") < strings.Index(out, "Cluster: Healthy") {
		t.Errorf("platform line should follow the cluster verdict:\n%s", out)
	}
}

func TestPrintInventory_TextOmitsPlatformWhenNilOrEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "Platform:") {
		t.Errorf("no platform line expected for nil facts:\n%s", buf.String())
	}
	var buf2 bytes.Buffer
	if err := PrintInventory(Input{Cluster: ch, Platform: &platform.Facts{}}, "text", &buf2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf2.String(), "Platform:") {
		t.Errorf("no platform line expected for empty facts:\n%s", buf2.String())
	}
}

func TestPrintInventory_JSONIncludesPlatform(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, Platform: sampleFacts()}, "json", &buf); err != nil {
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
		{Namespace: "default", Name: "web", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints — 2 matching pods, 0 ready"},
		{Namespace: "default", Name: "api-lb", Type: "LoadBalancer", Problem: "NoExternalAddress", Detail: "no external address", Since: "2026-06-29T00:00:00Z"},
	}
}

func TestPrintInventory_TextShowsServiceIssues(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, ServiceIssues: sampleServiceIssues()}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	// Service issues now render under NEEDS ATTENTION with ✗ glyph; no "Service issues:" header.
	for _, want := range []string{"NEEDS ATTENTION", "default/web", "ClusterIP", "no ready endpoints", "default/api-lb", "LoadBalancer", "no external address", "no external address · "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Service issues:") {
		t.Errorf("old Service issues: header must be absent:\n%s", out)
	}
}

func TestPrintInventory_TextNoServiceSectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "Service issues:") {
		t.Errorf("no Service issues section expected when empty:\n%s", buf.String())
	}
}

func TestPrintInventory_ServiceIssuesSuppressAllClear(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, ServiceIssues: sampleServiceIssues()}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "No issues found") {
		t.Errorf("all-clear must not print when there are service issues:\n%s", buf.String())
	}
}

func TestPrintInventory_ServiceLinesFollowWorkloads(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, Result: inventory.Result{Workloads: sampleWorkloads()}, ServiceIssues: sampleServiceIssues()}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	// Both workloads and real service issues are in NEEDS ATTENTION; workloads print first.
	if strings.Index(out, "cattle-system/rancher") > strings.Index(out, "default/web") {
		t.Errorf("workloads should precede the service issue lines:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesServiceIssues(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, ServiceIssues: sampleServiceIssues()}, "json", &buf); err != nil {
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
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "NetworkPolicy: pods selected by deny-all, web-allow") {
		t.Errorf("expected NP hint line:\n%s", buf.String())
	}
}

func TestPrintInventory_TextNoNetworkPolicyHintWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: sampleWorkloads()}}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "NetworkPolicy:") {
		t.Errorf("no NP hint expected when the workload has none:\n%s", buf.String())
	}
}

func sampleCredWarnings() []credlint.Finding {
	return []credlint.Finding{
		{Namespace: "default", Name: "app-config", Kind: "ConfigMap", Location: "DB_PASSWORD", Pattern: "credential-like name with a literal value"},
		{Namespace: "default", Name: "web", Kind: "Pod", Location: "app/AWS_SECRET", Pattern: "AWS access key"},
	}
}

func TestPrintInventory_TextShowsCredentialWarnings(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, CredentialWarnings: sampleCredWarnings()}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	// Credential warnings now render under NEEDS ATTENTION with ✗ glyph; no standalone header.
	for _, want := range []string{"NEEDS ATTENTION", "default/app-config", "ConfigMap[DB_PASSWORD]", "default/web", "Pod[app/AWS_SECRET]", "AWS access key"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Credential warnings (--lint-secrets):") {
		t.Errorf("old Credential warnings header must be absent:\n%s", out)
	}
}

func TestPrintInventory_TextNoCredentialSectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "Credential warnings") {
		t.Errorf("no credential section expected when empty:\n%s", buf.String())
	}
}

func TestPrintInventory_CredentialWarningsSuppressAllClear(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, CredentialWarnings: sampleCredWarnings()}, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "No issues found") {
		t.Errorf("all-clear must not print when there are credential warnings:\n%s", buf.String())
	}
}

func TestPrintInventory_TextShowsRolloutChange(t *testing.T) {
	wl := inventory.Workload{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff", Reason: "bad image"}},
		Rollout:  &inventory.RolloutChange{Revision: "6", Since: "4d ago", OldImage: "nginx:1.27", NewImage: "nginx:bad"}}
	var buf bytes.Buffer
	result := inventory.Result{Workloads: []inventory.Workload{wl}}
	if err := PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, Result: result}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "changed: rollout to revision 6, 4d ago") {
		t.Errorf("missing rollout-change line:\n%s", out)
	}
	if !strings.Contains(out, "image nginx:1.27 → nginx:bad") {
		t.Errorf("missing image delta:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesCredentialWarnings(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(Input{Cluster: ch, CredentialWarnings: sampleCredWarnings()}, "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		CredentialWarnings []credlint.Finding `json:"credentialWarnings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.CredentialWarnings) != 2 || got.CredentialWarnings[0].Location != "DB_PASSWORD" {
		t.Errorf("credentialWarnings missing/wrong in JSON: %+v", got.CredentialWarnings)
	}
}

func TestPrintInventory_TextShowsNodeReservations(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{
		WarnCount: 1,
		Nodes: []nodereserve.NodeReservation{
			{Name: "w1", Role: "worker", CPUReserved: "0", MemReserved: "0", Warning: true},
			{Name: "w2", Role: "worker", CPUReserved: "200m", MemReserved: "800Mi", Warning: false},
		},
	}
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 2, NodesTotal: 2}
	if err := PrintInventory(Input{Cluster: ch, NodeReserve: rep}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Warning node named in NOTES zone.
	notes := strings.Index(out, "NOTES")
	if notes < 0 || !strings.Contains(out, "reserve no memory") || !strings.Contains(out, "w1") {
		t.Errorf("expected NOTES warning naming w1 in:\n%s", out)
	}
	// CONTEXT shows the per-resource reservation block.
	if !strings.Contains(out, "Kubelet reservations (combined kube+system)") {
		t.Errorf("missing reservations block header in:\n%s", out)
	}
	if !strings.Contains(out, "1 of 2 nodes reserve none") {
		t.Errorf("missing per-resource memory status in:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesNodeReserve(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{WarnCount: 1, Nodes: []nodereserve.NodeReservation{
		{Name: "w1", CPUReserved: "0", MemReserved: "0", Warning: true},
	}}
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}
	if err := PrintInventory(Input{Cluster: ch, NodeReserve: rep}, "json", &buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	nr, ok := got["nodeReserve"].(map[string]any)
	if !ok {
		t.Fatalf("nodeReserve missing/wrong type in: %s", buf.String())
	}
	if nr["warnCount"].(float64) != 1 {
		t.Errorf("want warnCount 1, got %v", nr["warnCount"])
	}
}

func TestPrintInventory_TextShowsPVCReclaim(t *testing.T) {
	var buf bytes.Buffer
	rep := &pvcreclaim.Report{
		Count: 1,
		PVCs: []pvcreclaim.PVCReclaim{
			{Namespace: "shop", Name: "data-0", PV: "pvc-abc123", StorageClass: "standard", Capacity: "8Gi"},
		},
	}
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}
	// Use PVCReclaimFull to get per-PVC rows.
	if err := PrintInventory(Input{Cluster: ch, PVCReclaim: rep, PVCReclaimFull: true}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "shop/data-0") || !strings.Contains(out, "pv pvc-abc123") {
		t.Errorf("missing pvc row in:\n%s", out)
	}
	if !strings.Contains(out, "class standard") || !strings.Contains(out, "8Gi") {
		t.Errorf("missing class/capacity in:\n%s", out)
	}
}

func TestPrintInventory_TextNoPVCReclaimSectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	// Non-nil but empty report — the production path from main.go passes
	// &res.PVCReclaim, so the empty-slice branch must skip the section.
	rep := &pvcreclaim.Report{}
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}
	if err := PrintInventory(Input{Cluster: ch, PVCReclaim: rep}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "PVC") || strings.Contains(out, "--pvc-reclaim") {
		t.Errorf("section must be omitted for empty report, got:\n%s", out)
	}
}

func TestPrintInventory_HeaderAttentionLine(t *testing.T) {
	var buf bytes.Buffer
	ws := sampleWorkloads()
	ws[0].Findings = []diagnose.Finding{{Issue: "ImagePullBackOff", Reason: "bad ref"}}
	svc := []svchealth.Issue{
		{Namespace: "a", Name: "svc1", Type: "ClusterIP", Detail: "no ready endpoints"}, // real
		{Namespace: "b", Name: "svc2", Type: "ClusterIP", Detail: "scaled to 0", Expected: true},
	}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 3, NodesTotal: 3}, Result: inventory.Result{Workloads: ws}, ServiceIssues: svc}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Needs attention:") {
		t.Errorf("missing attention line in:\n%s", out)
	}
	if !strings.Contains(out, "1 workload failing") {
		t.Errorf("missing workload count in:\n%s", out)
	}
	if !strings.Contains(out, "1 service without endpoints") {
		t.Errorf("missing real-service count in:\n%s", out)
	}
}

func TestPrintInventory_ZoneOrderAndGlyphs(t *testing.T) {
	var buf bytes.Buffer
	ws := sampleWorkloads()
	ws[0].Findings = []diagnose.Finding{{Issue: "ImagePullBackOff", Reason: "bad ref"}}
	svc := []svchealth.Issue{
		{Namespace: "a", Name: "real", Type: "ClusterIP", Detail: "no ready endpoints"},
		{Namespace: "b", Name: "expected", Type: "ClusterIP", Detail: "scaled to 0", Expected: true},
	}
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 3, NodesTotal: 3},
		Result:        inventory.Result{Workloads: ws},
		Resources:     sampleSummary(),
		Platform:      sampleFacts(),
		ServiceIssues: svc,
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	na := strings.Index(out, "NEEDS ATTENTION")
	notes := strings.Index(out, "NOTES")
	ctx := strings.Index(out, "CONTEXT")
	if !(na >= 0 && notes > na && ctx > notes) {
		t.Fatalf("zones out of order: NEEDS ATTENTION=%d NOTES=%d CONTEXT=%d\n%s", na, notes, ctx, out)
	}
	// real service under NEEDS ATTENTION (before NOTES), expected under NOTES.
	if i := strings.Index(out, "a/real"); !(i > na && i < notes) {
		t.Errorf("real service not in NEEDS ATTENTION zone:\n%s", out)
	}
	if i := strings.Index(out, "b/expected"); !(i > notes && i < ctx) {
		t.Errorf("expected service not in NOTES zone:\n%s", out)
	}
	// resources + platform live in CONTEXT.
	if i := strings.Index(out, "Resources (cluster):"); i < ctx {
		t.Errorf("resources not in CONTEXT zone:\n%s", out)
	}
	if !strings.Contains(out, "✗ ") {
		t.Errorf("expected ✗ glyph for a problem in:\n%s", out)
	}
}

func TestPrintInventory_NodeReservationsCollapseWhenAllOK(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{WarnCount: 0, Nodes: []nodereserve.NodeReservation{
		{Name: "n1", CPUReserved: "300m", MemReserved: "1Gi"},
		{Name: "n2", CPUReserved: "300m", MemReserved: "1Gi"},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 2, NodesTotal: 2}, NodeReserve: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "n1") || strings.Contains(out, "n2") {
		t.Errorf("all-OK reservations must collapse (no per-node lines):\n%s", out)
	}
	if !strings.Contains(out, "all 2 nodes reserve some") {
		t.Errorf("missing all-OK reservation status in:\n%s", out)
	}
}

func TestPrintInventory_NodeReservationsWarningIsNote(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{WarnCount: 1, Nodes: []nodereserve.NodeReservation{
		{Name: "bad", CPUReserved: "0", MemReserved: "0", Warning: true},
		{Name: "ok", CPUReserved: "300m", MemReserved: "1Gi"},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 2, NodesTotal: 2}, NodeReserve: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	notes := strings.Index(out, "NOTES")
	if notes < 0 || !strings.Contains(out, "reserve no memory") || !strings.Contains(out, "bad") {
		t.Errorf("expected a NOTES warning naming the bad node:\n%s", out)
	}
	if !strings.Contains(out, "memory pressure can destabilize the node") {
		t.Errorf("expected the memory consequence line in:\n%s", out)
	}
}

func TestPrintInventory_PVCReclaimSummaryByDefault(t *testing.T) {
	var buf bytes.Buffer
	rep := &pvcreclaim.Report{Count: 3, PVCs: []pvcreclaim.PVCReclaim{
		{Namespace: "a", Name: "p1", PV: "pv1", StorageClass: "fast", Capacity: "1Gi"},
		{Namespace: "a", Name: "p2", PV: "pv2", StorageClass: "fast", Capacity: "1Gi"},
		{Namespace: "b", Name: "p3", PV: "pv3", StorageClass: "slow", Capacity: "1Gi"},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, PVCReclaim: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "3 PVCs on Delete reclaim") || !strings.Contains(out, "fast ×2") || !strings.Contains(out, "slow ×1") {
		t.Errorf("missing grouped PVC summary:\n%s", out)
	}
	if !strings.Contains(out, "[--pvc-reclaim]") {
		t.Errorf("missing --pvc-reclaim hint:\n%s", out)
	}
	if strings.Contains(out, "pv1") {
		t.Errorf("summary must not list individual PV rows:\n%s", out)
	}
}

func TestPrintInventory_PVCReclaimFullWhenFlagged(t *testing.T) {
	var buf bytes.Buffer
	rep := &pvcreclaim.Report{Count: 1, PVCs: []pvcreclaim.PVCReclaim{
		{Namespace: "a", Name: "p1", PV: "pv1", StorageClass: "fast", Capacity: "1Gi"},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, PVCReclaim: rep, PVCReclaimFull: true}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "a/p1") || !strings.Contains(out, "pv pv1") {
		t.Errorf("full list expected under --pvc-reclaim:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesPVCReclaim(t *testing.T) {
	var buf bytes.Buffer
	rep := &pvcreclaim.Report{Count: 1, PVCs: []pvcreclaim.PVCReclaim{
		{Namespace: "shop", Name: "data-0", PV: "pvc-abc123"},
	}}
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}
	if err := PrintInventory(Input{Cluster: ch, PVCReclaim: rep}, "json", &buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	pr, ok := got["pvcReclaim"].(map[string]any)
	if !ok {
		t.Fatalf("pvcReclaim missing/wrong type in: %s", buf.String())
	}
	if pr["count"].(float64) != 1 {
		t.Errorf("want count 1, got %v", pr["count"])
	}
}

func TestPrintInventory_TextShowsDiskUsageInNeedsAttention(t *testing.T) {
	var buf bytes.Buffer
	rep := &diskusage.Report{Threshold: 0.80, Over: []diskusage.VolumeUsage{
		{Kind: "pvc", Namespace: "clickhouse", Name: "data", UsedBytes: 46 << 30, CapacityBytes: 50 << 30, Ratio: 0.92},
		{Kind: "node", Node: "n1", Name: "n1", UsedBytes: 168 << 30, CapacityBytes: 200 << 30, Ratio: 0.84},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, DiskUsage: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "NEEDS ATTENTION") {
		t.Errorf("disk usage should trip the NEEDS ATTENTION zone:\n%s", out)
	}
	if !strings.Contains(out, "✗ pvc clickhouse/data") || !strings.Contains(out, "92% full") {
		t.Errorf("missing pvc disk line:\n%s", out)
	}
	if !strings.Contains(out, "✗ node n1") || !strings.Contains(out, "84% full") {
		t.Errorf("missing node disk line:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must be suppressed when a disk is over threshold:\n%s", out)
	}
	if !strings.Contains(out, "Needs attention: 2 volumes low on disk") {
		t.Errorf("header attention line should count the over-threshold volumes:\n%s", out)
	}
}

func TestPrintInventory_DiskUsageAbsentWhenNilOrEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, DiskUsage: &diskusage.Report{Threshold: 0.80}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "disk") {
		t.Errorf("no disk lines expected for an empty report:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("empty disk report must not suppress all-clear:\n%s", buf.String())
	}
}

func TestPrintInventory_JSONIncludesDiskUsage(t *testing.T) {
	var buf bytes.Buffer
	rep := &diskusage.Report{Threshold: 0.80, Over: []diskusage.VolumeUsage{
		{Kind: "node", Node: "n1", Name: "n1", UsedBytes: 168 << 30, CapacityBytes: 200 << 30, Ratio: 0.84},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, DiskUsage: rep}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["diskUsage"].(map[string]any); !ok {
		t.Fatalf("diskUsage missing in JSON: %s", buf.String())
	}
}

func TestPrintInventory_JSONOmitsDiskUsageWhenNil(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "diskUsage") {
		t.Errorf("diskUsage must be absent from JSON when the check is off:\n%s", buf.String())
	}
}

func TestPrintInventory_JSONOmitsKubeletHealthWhenNil(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "kubeletHealth") {
		t.Errorf("kubeletHealth must be absent from JSON when the check is off:\n%s", buf.String())
	}
}

func TestPrintInventory_TextShowsFindingEvidence(t *testing.T) {
	var buf bytes.Buffer
	ws := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Pending",
		Findings: []diagnose.Finding{{
			Issue:    "Unschedulable",
			Reason:   "No node can schedule this pod (resources, taints, or affinity)",
			Evidence: "0/5 nodes are available: 3 Insufficient memory, 2 node(s) had untolerated taint",
		}},
	}}
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "⚠ Unschedulable:") {
		t.Errorf("missing the finding line:\n%s", out)
	}
	if !strings.Contains(out, "↳ 0/5 nodes are available: 3 Insufficient memory") {
		t.Errorf("expected the Evidence sub-line with the scheduler message:\n%s", out)
	}
}

func TestPrintInventory_TextOmitsEvidenceWhenEmptyOrDuplicate(t *testing.T) {
	var buf bytes.Buffer
	ws := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{
			{Issue: "CrashLoopBackOff", Reason: "boom", Evidence: ""}, // empty -> no sub-line
			{Issue: "OOMKilled", Reason: "same", Evidence: "same"},    // equals Reason -> no sub-line
		},
	}}
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "↳") {
		t.Errorf("no Evidence sub-line expected for empty/duplicate evidence:\n%s", buf.String())
	}
}

func TestPrintInventory_TextShowsIngressIssues(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		IngressIssues: []ingresshealth.RouteIssue{{
			Namespace: "shop", Ingress: "web", Host: "example.com", Path: "/api",
			Service: "api-svc", Port: "8080", Problem: "NoEndpoints",
			Detail: "backend Service api-svc:8080 has no ready endpoints (likely 502/503)",
		}},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "NEEDS ATTENTION") {
		t.Errorf("a broken ingress route should trip NEEDS ATTENTION:\n%s", out)
	}
	if !strings.Contains(out, "✗ ingress shop/web") || !strings.Contains(out, "example.com/api") || !strings.Contains(out, "likely 502/503") {
		t.Errorf("missing the ingress route line:\n%s", out)
	}
	if !strings.Contains(out, "Needs attention: 1 ingress route broken") {
		t.Errorf("attention line should count the broken route:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must be suppressed:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesIngressIssues(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		IngressIssues: []ingresshealth.RouteIssue{{Namespace: "shop", Ingress: "web", Service: "api-svc", Problem: "NoService", Detail: "backend Service api-svc not found"}},
	}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"ingressIssues"`) || !strings.Contains(buf.String(), `"problem": "NoService"`) {
		t.Errorf("expected ingressIssues in JSON:\n%s", buf.String())
	}
}

func TestPrintInventory_NoIngressLinesWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "ingress") {
		t.Errorf("no ingress lines expected when there are no issues:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("empty ingress issues must not suppress all-clear:\n%s", buf.String())
	}
}

func TestPrintInventory_SecurityDefaultView(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityIssues: []secscan.Finding{
			{Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Profile: "baseline", Check: "Privileged", Detail: `container "app" runs privileged (full host access)`},
			{Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Profile: "restricted", Check: "RunAsRoot", Detail: `container "app" is not guaranteed to run as non-root`},
			{Namespace: "shop", Workload: "web", Kind: "Deployment", Container: "web", Profile: "restricted", Check: "AllowPrivilegeEscalation", Detail: `container "web" allows privilege escalation (allowPrivilegeEscalation not false)`},
			{Namespace: "shop", Workload: "web", Kind: "Deployment", Container: "web", Profile: "restricted", Check: "CapabilitiesNotDropped", Detail: `container "web" does not drop all capabilities`},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "SECURITY") {
		t.Errorf("expected a SECURITY section:\n%s", out)
	}
	if !strings.Contains(out, "1 baseline · 3 restricted hardening gaps · 2 workloads") {
		t.Errorf("missing tier summary header:\n%s", out)
	}
	if !strings.Contains(out, "✗ shop/api  Deployment") || !strings.Contains(out, "[baseline] Privileged") {
		t.Errorf("missing the act-on-these detail block:\n%s", out)
	}
	if strings.Contains(out, "[restricted] RunAsRoot") {
		t.Errorf("restricted findings must be folded into the aggregate, not listed:\n%s", out)
	}
	if strings.Contains(out, "✗ shop/web") {
		t.Errorf("a restricted-only workload must not get a detail block:\n%s", out)
	}
	if !strings.Contains(out, "restricted (hardening gaps, near-universal): 3 across 2 workloads") {
		t.Errorf("missing restricted aggregate:\n%s", out)
	}
	if !strings.Contains(out, "RunAsRoot ×1 · AllowPrivilegeEscalation ×1 · CapabilitiesNotDropped ×1") {
		t.Errorf("missing per-check counts:\n%s", out)
	}
	if !strings.Contains(out, "--security-verbose") {
		t.Errorf("missing the --security-verbose hint:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must be suppressed when there are findings:\n%s", out)
	}
}

func TestPrintInventory_SecurityVerbose(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:         clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityVerbose: true,
		SecurityIssues: []secscan.Finding{
			{Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Profile: "baseline", Check: "Privileged", Detail: "p"},
			{Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Profile: "restricted", Check: "RunAsRoot", Detail: "r"},
			{Namespace: "shop", Workload: "web", Kind: "Deployment", Container: "web", Profile: "restricted", Check: "CapabilitiesNotDropped", Detail: "c"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "[restricted] RunAsRoot") {
		t.Errorf("verbose must list restricted findings:\n%s", out)
	}
	if !strings.Contains(out, "✗ shop/web  Deployment") {
		t.Errorf("verbose must show restricted-only workloads:\n%s", out)
	}
	if strings.Contains(out, "restricted (hardening gaps, near-universal)") {
		t.Errorf("verbose must omit the aggregate block:\n%s", out)
	}
}

func TestPrintInventory_SecurityOnlyRestricted(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityIssues: []secscan.Finding{
			{Namespace: "shop", Workload: "web", Kind: "Deployment", Container: "web", Profile: "restricted", Check: "RunAsRoot", Detail: "r"},
			{Namespace: "shop", Workload: "web", Kind: "Deployment", Container: "web", Profile: "restricted", Check: "CapabilitiesNotDropped", Detail: "c"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "✗ ") {
		t.Errorf("restricted-only findings must produce no detail blocks:\n%s", out)
	}
	if !strings.Contains(out, "restricted (hardening gaps, near-universal): 2 across 1 workload") {
		t.Errorf("missing restricted aggregate for restricted-only input:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must stay suppressed:\n%s", out)
	}
}

func TestPrintInventory_SecurityWorstFirst(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityIssues: []secscan.Finding{
			{Namespace: "ns", Workload: "bbb", Kind: "Deployment", Profile: "baseline", Check: "HostPath", Detail: "mounts hostPath /a"},
			{Namespace: "ns", Workload: "aaa", Kind: "Deployment", Profile: "baseline", Check: "HostPath", Detail: "mounts hostPath /b"},
			{Namespace: "ns", Workload: "aaa", Kind: "Deployment", Profile: "baseline", Check: "HostPath", Detail: "mounts hostPath /c"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Index(out, "ns/aaa") > strings.Index(out, "ns/bbb") {
		t.Errorf("workload with more findings (aaa: 2) must sort before bbb (1):\n%s", out)
	}
	if strings.Contains(out, "restricted (hardening") {
		t.Errorf("no restricted aggregate expected when there are no restricted findings:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesSecurity(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:        clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityIssues: []secscan.Finding{{Namespace: "shop", Workload: "admin", Kind: "Service", Profile: "kubeagent", Check: "ExposedService", Detail: "type LoadBalancer exposes port(s) 80 externally"}},
	}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"securityIssues"`) || !strings.Contains(buf.String(), `"check": "ExposedService"`) {
		t.Errorf("expected securityIssues in JSON:\n%s", buf.String())
	}
}

func TestPrintInventory_NoSecurityWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "SECURITY") {
		t.Errorf("no SECURITY section expected when there are no findings:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("empty security must not suppress the all-clear:\n%s", buf.String())
	}
}

func TestPrintInventory_SecurityExposedServiceTier(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityIssues: []secscan.Finding{
			{Namespace: "shop", Workload: "admin", Kind: "Service", Profile: "kubeagent", Check: "ExposedService", Detail: "type LoadBalancer exposes port(s) 80 externally"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "1 exposed service · 1 workload") {
		t.Errorf("expected the exposed-service tier in the summary header:\n%s", out)
	}
	if !strings.Contains(out, "✗ shop/admin  Service") || !strings.Contains(out, "[kubeagent] ExposedService") {
		t.Errorf("an exposed Service must be shown in full (act-on-these):\n%s", out)
	}
	if strings.Contains(out, "restricted (hardening") {
		t.Errorf("no restricted aggregate expected when there are no restricted findings:\n%s", out)
	}
}

func TestPrintInventory_KubeletHealthUnhealthy(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		KubeletHealth: &nodehealth.Report{Probed: 2, Unhealthy: []nodehealth.Issue{{Node: "worker-2", Detail: "[-]pleg failed"}}},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "KUBELET HEALTH") || !strings.Contains(out, "✗ node worker-2 kubelet /healthz unhealthy: [-]pleg failed") {
		t.Errorf("missing kubelet-health section:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must be suppressed when unhealthy:\n%s", out)
	}
}

func TestPrintInventory_KubeletHealthForbiddenHint(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		KubeletHealth: &nodehealth.Report{Probed: 3, Forbidden: 3},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "needs the nodes/proxy add-on") {
		t.Errorf("missing missing-grant hint:\n%s", buf.String())
	}
}

func TestPrintInventory_KubeletHealthCleanNoSection(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		KubeletHealth: &nodehealth.Report{Probed: 3}, // all ok
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "KUBELET HEALTH") {
		t.Errorf("no section expected when all healthy:\n%s", out)
	}
	if !strings.Contains(out, "No issues found") {
		t.Errorf("all-clear preserved when clean:\n%s", out)
	}
}

func TestPrintInventory_KubeletHealthJSON(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		KubeletHealth: &nodehealth.Report{Probed: 1, Unhealthy: []nodehealth.Issue{{Node: "w", Detail: "d"}}},
	}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"kubeletHealth"`) || !strings.Contains(buf.String(), `"node": "w"`) {
		t.Errorf("expected kubeletHealth in JSON:\n%s", buf.String())
	}
}

func TestPrintInventory_NoEphemeralWarnsAndContext(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{
		WarnCount: 0, EphemeralNone: 1, EphemeralReporting: 2,
		Nodes: []nodereserve.NodeReservation{
			{Name: "diskless", CPUReserved: "200m", MemReserved: "1Gi", EphemeralReserved: "0", NoEphemeral: true},
			{Name: "ok", CPUReserved: "200m", MemReserved: "1Gi", EphemeralReserved: "5Gi"},
		},
	}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 2, NodesTotal: 2}, NodeReserve: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "reserve no ephemeral-storage: diskless") || !strings.Contains(out, "disk pressure can destabilize the node") {
		t.Errorf("expected ephemeral NOTES warning naming diskless in:\n%s", out)
	}
	// WarnCount==0 here, so memory reads "all ... reserve some"; the
	// "reserve none ⚠" status uniquely identifies the ephemeral-storage line.
	if !strings.Contains(out, "1 of 2 nodes reserve none  ⚠") {
		t.Errorf("expected ephemeral CONTEXT line with warn glyph in:\n%s", out)
	}
}

func TestPrintInventory_ReservationsNotReportedEphemeral(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{
		WarnCount: 0, EphemeralReporting: 0,
		Nodes: []nodereserve.NodeReservation{
			{Name: "n1", CPUReserved: "200m", MemReserved: "1Gi", EphemeralReserved: "—"},
		},
	}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, NodeReserve: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "ephemeral-storage not reported") {
		t.Errorf("expected 'ephemeral-storage not reported' in:\n%s", out)
	}
	if strings.Contains(out, "reserve no ephemeral-storage") {
		t.Errorf("must not warn on ephemeral when not reported:\n%s", out)
	}
}

func TestPrintInventory_ShowsLogRootCause(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 1, NodesTotal: 1},
		Result: inventory.Result{Workloads: []inventory.Workload{{
			Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Findings: []diagnose.Finding{{
				Pod: "shop/web-1", Issue: "CrashLoopBackOff", Reason: "Container repeatedly crashes after starting",
				Evidence: `container "web", restartCount=8`, Container: "web",
				LogExcerpt: "panic: runtime error: invalid memory address", LogCause: "application panic (code bug)",
			}},
		}}},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "logs (previous container):") ||
		!strings.Contains(out, "panic: runtime error: invalid memory address") ||
		!strings.Contains(out, "→ application panic (code bug)") {
		t.Errorf("missing log root-cause block:\n%s", out)
	}
}

func TestPrintInventory_UsesInjectedClock(t *testing.T) {
	// A degraded workload that restarted at a fixed instant; Now is 5 days later.
	// The rendered age must be measured from Input.Now, not wall-clock.
	in := Input{
		Now:     time.Date(2020, 1, 6, 0, 0, 0, 0, time.UTC),
		Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 1, NodesTotal: 1},
		Result: inventory.Result{Workloads: []inventory.Workload{{
			Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0,
			Status: "Degraded", Restarts: 8, LastRestart: "2020-01-01T00:00:00Z",
			Findings: []diagnose.Finding{{Pod: "shop/web", Issue: "CrashLoopBackOff", Reason: "keeps crashing", Evidence: "restartCount=8"}},
		}}},
	}
	var buf bytes.Buffer
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "5d ago") {
		t.Errorf("age should be measured from Input.Now (want \"5d ago\"):\n%s", buf.String())
	}
}

func TestPrintInventory_ShowsPVCIssues(t *testing.T) {
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"},
		PVCIssues: []pvchealth.Issue{{
			Namespace: "shop", Name: "data-pvc", Phase: "Pending",
			Reason: "ProvisioningFailed", Detail: `storageclass "fast" not found`, StorageClass: "fast",
		}},
	}
	var buf bytes.Buffer
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `✗ shop/data-pvc  PersistentVolumeClaim  Pending — storageclass "fast" not found`) {
		t.Errorf("PVC issue not rendered in NEEDS ATTENTION:\n%s", out)
	}
	if !strings.Contains(out, "1 PVC failing to provision") {
		t.Errorf("attention summary missing the PVC count:\n%s", out)
	}
}

func TestPrintInventory_ExpectedIngressGoesToNotes(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		IngressIssues: []ingresshealth.RouteIssue{
			{Namespace: "shop", Ingress: "real", Host: "a.io", Path: "/", Service: "api", Port: "80",
				Problem: "NoEndpoints", Detail: "backend Service api:80 has no ready endpoints (likely 502/503)"},
			{Namespace: "shop", Ingress: "parked", Host: "b.io", Path: "/", Service: "web",
				Problem: "NoEndpoints", Expected: true,
				Detail: "backend Service web is intentionally empty (scaled to 0) — route parked"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// The attention line counts only the real route.
	if !strings.Contains(out, "1 ingress route broken") {
		t.Errorf("attention line should count only the real route:\n%s", out)
	}
	// The real route is in NEEDS ATTENTION with the ✗ glyph.
	if !strings.Contains(out, "✗ ingress shop/real") {
		t.Errorf("real route should be under NEEDS ATTENTION:\n%s", out)
	}
	// The parked route is a quiet NOTE with the • glyph, not an attention ✗.
	if !strings.Contains(out, "• ingress shop/parked") {
		t.Errorf("parked route should be a NOTE:\n%s", out)
	}
	if strings.Contains(out, "✗ ingress shop/parked") {
		t.Errorf("parked route must not appear under NEEDS ATTENTION:\n%s", out)
	}
}

func TestPrintInventory_RootCauseLineAndRollup(t *testing.T) {
	ws := []inventory.Workload{
		{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 2, Ready: 0, Status: "Degraded",
			RootCause: "node worker-2 (NotReady)",
			Findings:  []diagnose.Finding{{Issue: "CrashLoopBackOff", Reason: "keeps crashing"}}},
		{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "node worker-2 (NotReady)"},
	}
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		Result: inventory.Result{Workloads: ws}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "↳ likely caused by node worker-2 (NotReady)") {
		t.Errorf("missing root-cause line:\n%s", out)
	}
	if !strings.Contains(out, "(2 ⇐ node worker-2)") {
		t.Errorf("attention line should roll up both workloads under one node:\n%s", out)
	}
}

func TestPrintInventory_RootCauseMultiNodeRollup(t *testing.T) {
	ws := []inventory.Workload{
		{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded", RootCause: "node worker-2 (NotReady)"},
		{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded", RootCause: "node worker-1 (kubelet not heartbeating)"},
	}
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 1, NodesTotal: 3}, Result: inventory.Result{Workloads: ws}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "(2 ⇐ 2 root causes)") {
		t.Errorf("attention line should report 2 root causes:\n%s", buf.String())
	}
}

func TestPrintInventory_RootCauseMixedNodeAndRegistry(t *testing.T) {
	ws := []inventory.Workload{
		{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "node worker-2 (NotReady)"},
		{Namespace: "shop", Name: "frontend", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "registry ghcr.io (2 workloads failing to pull)"},
		{Namespace: "shop", Name: "search", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "registry ghcr.io (2 workloads failing to pull)"},
	}
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		Result: inventory.Result{Workloads: ws}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "(3 ⇐ 2 root causes)") {
		t.Errorf("mixed node+registry causes should roll up as 2 root causes:\n%s", out)
	}
	if !strings.Contains(out, "↳ likely caused by registry ghcr.io (2 workloads failing to pull)") {
		t.Errorf("registry cause line should render via the generic path:\n%s", out)
	}
}

func TestPrintInventory_SingleRegistryCauseNamed(t *testing.T) {
	ws := []inventory.Workload{
		{Namespace: "shop", Name: "frontend", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "registry ghcr.io (2 workloads failing to pull)"},
		{Namespace: "shop", Name: "search", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "registry ghcr.io (2 workloads failing to pull)"},
	}
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 3, NodesTotal: 3},
		Result: inventory.Result{Workloads: ws}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "(2 ⇐ registry ghcr.io)") {
		t.Errorf("single distinct cause should be named:\n%s", buf.String())
	}
}

func TestPrintInventory_CertificatesSection(t *testing.T) {
	rep := &certhealth.Report{Checked: 3, WarnDays: 30,
		Expired: []certhealth.Cert{{Namespace: "shop", Name: "shop-tls", CommonName: "shop.example.com",
			NotAfter: "2026-07-18T00:00:00Z", Days: -3, Ingresses: []string{"shop/storefront (shop.example.com)"}}},
		Expiring: []certhealth.Cert{{Namespace: "infra", Name: "api-tls", CommonName: "api.example.com",
			NotAfter: "2026-08-02T00:00:00Z", Days: 12}},
		Invalid: []certhealth.Invalid{{Namespace: "shop", Name: "bad-tls", Detail: "invalid certificate data"}},
	}
	var buf bytes.Buffer
	if err := PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		Certificates: rep}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"CERTIFICATES  (advisory — public certificate metadata only)",
		"✗ shop/shop-tls  EXPIRED 3d ago  (CN shop.example.com)",
		"— fronts ingress shop/storefront (shop.example.com)",
		"⚠ infra/api-tls  expires in 12d  (CN api.example.com)",
		"⚠ shop/bad-tls  invalid certificate data",
		"· 3 certificates checked (warn window 30d)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPrintInventory_CertificatesForbiddenHint(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		Certificates: &certhealth.Report{WarnDays: 30, Forbidden: true}}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "secrets access denied — apply deploy/rbac-certs.yaml (or Helm certs.enabled=true)") {
		t.Errorf("missing forbidden hint:\n%s", buf.String())
	}
}

func TestPrintInventory_CertificatesAbsentWhenNilOrClean(t *testing.T) {
	var buf bytes.Buffer
	_ = PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}, "text", &buf)
	if strings.Contains(buf.String(), "CERTIFICATES") {
		t.Error("section must be absent when the check did not run")
	}
	buf.Reset()
	_ = PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		Certificates: &certhealth.Report{Checked: 5, WarnDays: 30}}, "text", &buf)
	if strings.Contains(buf.String(), "CERTIFICATES") {
		t.Error("section must be absent when everything is healthy")
	}
}

func TestPrintInventory_ConfidenceTags(t *testing.T) {
	ws := []inventory.Workload{
		{Namespace: "shop", Name: "cache", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "registry ghcr.io (2 workloads failing to pull)",
			Findings: []diagnose.Finding{
				{Issue: "RestartLoop", Reason: "keeps erroring", Confidence: "medium"},
				{Issue: "CrashLoopBackOff", Reason: "repeatedly crashes", Confidence: "high"},
			}},
		{Namespace: "shop", Name: "db", Kind: "StatefulSet", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "node worker-2 (NotReady)",
			Findings:  []diagnose.Finding{{Issue: "VolumeAttachError", Reason: "Multi-Attach", Confidence: "high"}}},
	}
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		Result: inventory.Result{Workloads: ws}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "⚠ RestartLoop [medium]: keeps erroring") {
		t.Errorf("medium finding should be tagged:\n%s", out)
	}
	if !strings.Contains(out, "⚠ CrashLoopBackOff: repeatedly crashes") || strings.Contains(out, "CrashLoopBackOff [high]") {
		t.Errorf("high finding must be unmarked:\n%s", out)
	}
	if !strings.Contains(out, "↳ likely caused by registry ghcr.io (2 workloads failing to pull) [medium]") {
		t.Errorf("registry attribution should be tagged medium:\n%s", out)
	}
	if strings.Contains(out, "node worker-2 (NotReady) [") {
		t.Errorf("node attribution (high) must be unmarked:\n%s", out)
	}
}

func TestPrintInventory_JSONFindingCarriesConfidence(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff", Reason: "x", Confidence: "high"}}}}
	var buf bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"confidence": "high"`) {
		t.Errorf("JSON must carry finding confidence:\n%s", buf.String())
	}
}

func TestPrintInventory_StuckTerminating(t *testing.T) {
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		StuckTerminating: []termhealth.Issue{
			{Kind: "Namespace", Name: "legacy-ns", Age: "3h", Reason: "NamespaceFinalizersRemaining — kubernetes finalizer remains"},
			{Kind: "Pod", Namespace: "shop", Name: "api-7c9d5", Age: "8m", PastGrace: true, Reason: "finalizer example.com/cleanup-hook"},
			{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data", Age: "20m", Reason: "pvc-protection — still mounted by pod shop/db-0"},
		}}
	var buf bytes.Buffer
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"✗ legacy-ns  Namespace  Terminating 3h",
		"⚠ StuckTerminating: NamespaceFinalizersRemaining — kubernetes finalizer remains",
		"✗ shop/api-7c9d5  Pod  Terminating 8m (past grace)",
		"✗ shop/data  PersistentVolumeClaim  Terminating 20m",
		"⚠ StuckTerminating: pvc-protection — still mounted by pod shop/db-0",
		"3 resources stuck terminating",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "legacy-ns  Namespace  Terminating 3h (past grace)") {
		t.Error("(past grace) must appear only on pods")
	}
}

func TestPrintInventory_StuckTerminatingAbsentWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	_ = PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}, "text", &buf)
	if strings.Contains(buf.String(), "StuckTerminating") {
		t.Error("section must be absent when nothing is stuck")
	}
}

func TestPrintInventory_PDBIssues(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"},
		PDBIssues: []pdbhealth.Issue{
			{Namespace: "shop", Name: "api", Rule: "minAvailable: 3", Category: "unsatisfiable",
				Reason: "covers all 3 pods — no voluntary eviction can ever proceed; every node drain will hang"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "✗ shop/api  PodDisruptionBudget  minAvailable: 3") {
		t.Errorf("missing PDB header line:\n%s", out)
	}
	if !strings.Contains(out, "⚠ PDBBlocked: covers all 3 pods") {
		t.Errorf("missing PDBBlocked reason line:\n%s", out)
	}
	if !strings.Contains(out, "1 PodDisruptionBudget blocking drains") {
		t.Errorf("missing attention-line fragment:\n%s", out)
	}
}

func TestPrintInventory_NoPDBSectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "PDBBlocked") {
		t.Errorf("no PDB section expected when empty:\n%s", buf.String())
	}
}

func TestPrintInventory_HPAIssues(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"},
		HPAIssues: []hpahealth.Issue{
			{Namespace: "shop", Name: "api-hpa", Target: "Deployment/api", Category: "metrics",
				Reason: "can't fetch metrics — unable to get resource metric cpu: no metrics returned"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "✗ shop/api-hpa  HorizontalPodAutoscaler  targets Deployment/api") {
		t.Errorf("missing HPA header line:\n%s", out)
	}
	if !strings.Contains(out, "⚠ HPAStuck: can't fetch metrics — unable to get resource metric cpu") {
		t.Errorf("missing HPAStuck reason line:\n%s", out)
	}
	if !strings.Contains(out, "1 HPA can't scale") {
		t.Errorf("missing attention-line fragment:\n%s", out)
	}
}

func TestPrintInventory_NoHPASectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "HPAStuck") {
		t.Errorf("no HPA section expected when empty:\n%s", buf.String())
	}
}

func TestPrintInventory_WebhookIssues(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"},
		WebhookIssues: []webhookhealth.Issue{
			{Kind: "ValidatingWebhookConfiguration", Config: "policy-webhook", Webhook: "validate.policy.io",
				Service: "kube-system/policy-svc", Problem: "no-endpoints",
				Reason: "backend Service kube-system/policy-svc has no ready endpoints — failurePolicy Fail rejects every intercepted create/update"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "✗ policy-webhook  ValidatingWebhookConfiguration  webhook validate.policy.io") {
		t.Errorf("missing webhook header line:\n%s", out)
	}
	if !strings.Contains(out, "⚠ WebhookDown: backend Service kube-system/policy-svc has no ready endpoints") {
		t.Errorf("missing WebhookDown reason line:\n%s", out)
	}
	if !strings.Contains(out, "1 admission webhook failing") {
		t.Errorf("missing attention-line fragment:\n%s", out)
	}
}

func TestPrintInventory_NoWebhookSectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "WebhookDown") {
		t.Errorf("no webhook section expected when empty:\n%s", buf.String())
	}
}

func TestPrintInventory_SuggestLines(t *testing.T) {
	build := func(suggest bool) string {
		var buf bytes.Buffer
		in := Input{
			Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded"},
			Result: inventory.Result{Workloads: []inventory.Workload{{
				Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 2, Ready: 0, Status: "Degraded",
				Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "CrashLoopBackOff", Reason: "keeps crashing", Container: "web"}},
			}}},
			Suggest: suggest,
		}
		if err := PrintInventory(in, "text", &buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	on := build(true)
	if !strings.Contains(on, "↳ next step: starts then crashes — inspect the crash output") {
		t.Errorf("missing next-step line:\n%s", on)
	}
	if !strings.Contains(on, "↳ try: kubectl -n shop logs web-abc -c web --previous") {
		t.Errorf("missing try line:\n%s", on)
	}

	off := build(false)
	if strings.Contains(off, "next step:") || strings.Contains(off, "↳ try:") {
		t.Errorf("no suggest lines expected by default:\n%s", off)
	}
}

func TestPrintQuotaIssues(t *testing.T) {
	in := Input{
		Result: inventory.Result{}, // no workloads
		QuotaIssues: []quotahealth.Issue{
			{Namespace: "shop", Quota: "compute", Resource: "requests.cpu", Used: "4", Hard: "4", Ratio: 1.0, Severity: "exhausted"},
			{Namespace: "web", Quota: "compute", Resource: "pods", Used: "47", Hard: "50", Ratio: 0.94, Severity: "near"},
		},
	}
	var buf bytes.Buffer
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "✗ shop/compute  ResourceQuota  requests.cpu") {
		t.Errorf("missing exhausted quota row:\n%s", out)
	}
	if !strings.Contains(out, "⚠ QuotaExhausted: used 4 / hard 4 (100%)") {
		t.Errorf("missing exhausted detail:\n%s", out)
	}
	if !strings.Contains(out, "✗ web/compute  ResourceQuota  pods") {
		t.Errorf("missing near quota row:\n%s", out)
	}
	if !strings.Contains(out, "⚠ QuotaNearLimit: used 47 / hard 50 (94%)") {
		t.Errorf("missing near detail:\n%s", out)
	}

	// Empty QuotaIssues renders no ResourceQuota rows.
	var buf2 bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}}, "text", &buf2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf2.String(), "ResourceQuota") {
		t.Errorf("empty QuotaIssues should render nothing, got:\n%s", buf2.String())
	}
}

func TestPrintControlPlane(t *testing.T) {
	// unhealthy → section with the failing checks
	unhealthy := &controlplane.Probe{Status: "unhealthy", Failed: []string{"etcd", "poststarthook/x"}}
	var b bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}, ControlPlane: unhealthy}, "text", &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "CONTROL PLANE") || !strings.Contains(out, "control plane not ready") {
		t.Errorf("missing CONTROL PLANE section:\n%s", out)
	}
	if !strings.Contains(out, "2 checks failing: etcd, poststarthook/x") {
		t.Errorf("missing failing-checks line:\n%s", out)
	}

	// unhealthy with no named checks → the generic not-ready line
	var bg bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}, ControlPlane: &controlplane.Probe{Status: "unhealthy"}}, "text", &bg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bg.String(), "apiserver /readyz reported not ready") {
		t.Errorf("empty-Failed unhealthy should print the generic not-ready line:\n%s", bg.String())
	}

	// forbidden → grant hint
	var bf bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}, ControlPlane: &controlplane.Probe{Status: "forbidden"}}, "text", &bf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bf.String(), "/readyz") {
		t.Errorf("forbidden should print a /readyz grant hint:\n%s", bf.String())
	}

	// ok / unreachable / nil → nothing
	for _, p := range []*controlplane.Probe{{Status: "ok"}, {Status: "unreachable"}, nil} {
		var bo bytes.Buffer
		if err := PrintInventory(Input{Result: inventory.Result{}, ControlPlane: p}, "text", &bo); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(bo.String(), "CONTROL PLANE") {
			t.Errorf("probe %+v should render no CONTROL PLANE section:\n%s", p, bo.String())
		}
	}
}

func TestPrintDNSHealth(t *testing.T) {
	degraded := &dnshealth.Report{Status: "degraded", ServfailRatio: 0.123, ErrorResponses: 1234, TotalResponses: 10000, PodsProbed: 2}
	var b bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}, DNS: degraded}, "text", &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "DNS") || !strings.Contains(out, "cluster DNS is failing to resolve") {
		t.Errorf("missing DNS section:\n%s", out)
	}
	if !strings.Contains(out, "12.3%") || !strings.Contains(out, "1234/10000 responses across 2 pods") {
		t.Errorf("missing ratio detail:\n%s", out)
	}

	// forbidden → grant hint
	var bf bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}, DNS: &dnshealth.Report{Status: "forbidden"}}, "text", &bf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bf.String(), "pods/proxy") {
		t.Errorf("forbidden should print a pods/proxy grant hint:\n%s", bf.String())
	}

	// ok / unreachable / nil → nothing
	for _, p := range []*dnshealth.Report{{Status: "ok"}, {Status: "unreachable"}, nil} {
		var bo bytes.Buffer
		if err := PrintInventory(Input{Result: inventory.Result{}, DNS: p}, "text", &bo); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(bo.String(), "cluster DNS") {
			t.Errorf("probe %+v should render no DNS finding:\n%s", p, bo.String())
		}
	}
}
