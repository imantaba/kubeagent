package watch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
	"github.com/imantaba/kubeagent/internal/scan"
)

func sampleResult() *scan.Result {
	return &scan.Result{
		Health:      clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3, NodesStaleHeartbeat: 1, NodesExpectedAbsent: 1},
		NodeReserve: nodereserve.Report{WarnCount: 1},
		PVCReclaim:  pvcreclaim.Report{Count: 2},
		Inventory: inventory.Result{Workloads: []inventory.Workload{
			{Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 1,
				Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff"}}},
		}},
		DiskUsage: diskusage.Report{
			Threshold: 0.80,
			Over:      []diskusage.VolumeUsage{{Kind: "node", Node: "n1", Name: "n1", Ratio: 0.84}},
			Nodes:     []diskusage.VolumeUsage{{Kind: "node", Node: "n1", Name: "n1", Ratio: 0.84}},
		},
		IngressIssues: []ingresshealth.RouteIssue{{Namespace: "shop", Ingress: "web", Service: "api-svc", Problem: "NoEndpoints"}},
		KubeletHealth: nodehealth.Report{Probed: 2, Unhealthy: []nodehealth.Issue{{Node: "w"}}},
	}
}

func TestMetrics_RenderReflectsResult(t *testing.T) {
	m := newMetrics()
	m.update(sampleResult(), 150*time.Millisecond, time.Unix(1000, 0), nil)
	out := m.render()
	for _, want := range []string{
		"kubeagent_cluster_healthy 0",
		"kubeagent_nodes_ready 2",
		"kubeagent_nodes_total 3",
		"kubeagent_workloads_flagged 1",
		`kubeagent_findings{issue="CrashLoopBackOff"} 1`,
		"kubeagent_nodes_without_reservations 1",
		"kubeagent_nodes_stale_heartbeat 1",
		"kubeagent_pvcs_reclaim_delete 2",
		"kubeagent_scans_total 1",
		`kubeagent_node_fs_usage_ratio{node="n1"} 0.84`,
		"kubeagent_volumes_over_disk_threshold 1",
		"kubeagent_ingress_route_issues 1",
		"kubeagent_nodes_expected_absent 1",
		"kubeagent_kubelet_unhealthy 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
	}
}

func TestMetrics_UpdateErrorKeepsLastGoodAndCountsError(t *testing.T) {
	m := newMetrics()
	m.update(sampleResult(), time.Millisecond, time.Unix(1000, 0), nil)
	m.update(nil, time.Millisecond, time.Unix(1001, 0), errors.New("boom"))
	out := m.render()
	if !strings.Contains(out, "kubeagent_scan_errors_total 1") {
		t.Errorf("expected error counter, got:\n%s", out)
	}
	if !strings.Contains(out, "kubeagent_workloads_flagged 1") {
		t.Errorf("error update must preserve last-good gauges, got:\n%s", out)
	}
}

func TestMetrics_ReadyzGate(t *testing.T) {
	m := newMetrics()
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	if code := get(t, srv.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz before ready: want 503, got %d", code)
	}
	m.markReady()
	if code := get(t, srv.URL+"/readyz"); code != http.StatusOK {
		t.Errorf("readyz after ready: want 200, got %d", code)
	}
	if code := get(t, srv.URL+"/healthz"); code != http.StatusOK {
		t.Errorf("healthz: want 200, got %d", code)
	}
}

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
