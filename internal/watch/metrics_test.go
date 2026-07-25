package watch

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/alert"
	"github.com/imantaba/kubeagent/internal/certhealth"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/controlplane"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/dnshealth"
	"github.com/imantaba/kubeagent/internal/hpahealth"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/pdbhealth"
	"github.com/imantaba/kubeagent/internal/pvchealth"
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
	"github.com/imantaba/kubeagent/internal/quotahealth"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/termhealth"
	"github.com/imantaba/kubeagent/internal/watchstate"
	"github.com/imantaba/kubeagent/internal/webhookhealth"
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
		// One real issue and one intentionally-empty (Expected/parked) issue each: the
		// gauges must count only the real one, so alerts don't fire on parked backends.
		ServiceIssues: []svchealth.Issue{
			{Namespace: "shop", Name: "api-svc", Problem: "NoEndpoints"},
			{Namespace: "shop", Name: "parked-svc", Problem: "NoEndpoints", Expected: true},
		},
		IngressIssues: []ingresshealth.RouteIssue{
			{Namespace: "shop", Ingress: "web", Service: "api-svc", Problem: "NoEndpoints"},
			{Namespace: "shop", Ingress: "parked", Service: "parked-svc", Problem: "NoEndpoints", Expected: true},
		},
		PVCIssues:        []pvchealth.Issue{{Namespace: "shop", Name: "data-pvc", Phase: "Pending", Reason: "ProvisioningFailed"}},
		StuckTerminating: []termhealth.Issue{{Kind: "Namespace", Name: "legacy-ns", Age: "3h", Reason: "NamespaceFinalizersRemaining — x"}},
		PDBIssues:        []pdbhealth.Issue{{Namespace: "shop", Name: "api", Category: "blocking"}},
		HPAIssues:        []hpahealth.Issue{{Namespace: "shop", Name: "api-hpa", Category: "capped"}},
		WebhookIssues: []webhookhealth.Issue{
			{Kind: "ValidatingWebhookConfiguration", Config: "policy-webhook", Webhook: "w", Problem: "no-endpoints", Reason: "backend missing"},
			{Kind: "ValidatingWebhookConfiguration", Config: "slow-webhook", Webhook: "s.io", Problem: "high-timeout", Reason: "timeoutSeconds too high"},
		},
		QuotaIssues:   []quotahealth.Issue{{Namespace: "shop", Quota: "compute", Resource: "pods", Severity: "near"}},
		KubeletHealth: nodehealth.Report{Probed: 2, Unhealthy: []nodehealth.Issue{{Node: "w"}}},
		ControlPlane:  controlplane.Probe{Status: "unhealthy", Failed: []string{"etcd"}},
		DNS:           dnshealth.Report{Status: "degraded", ServfailRatio: 0.12},
		Certificates: &certhealth.Report{WarnDays: 30, Checked: 4,
			Expired:  []certhealth.Cert{{Namespace: "shop", Name: "shop-tls", Days: -3}},
			Expiring: []certhealth.Cert{{Namespace: "infra", Name: "api-tls", Days: 12}},
			Invalid:  []certhealth.Invalid{{Namespace: "infra", Name: "broken-tls", Detail: "invalid certificate data"}}},
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
		"kubeagent_service_issues 1",
		"kubeagent_pvc_pending_issues 1",
		"kubeagent_resources_stuck_terminating 1",
		"kubeagent_pdb_blocking_issues 1",
		"kubeagent_hpa_scaling_issues 1",
		"kubeagent_admission_webhooks_failing 1",
		"kubeagent_admission_webhook_latency_risks 1",
		"kubeagent_resourcequota_issues 1",
		"kubeagent_nodes_expected_absent 1",
		"kubeagent_kubelet_unhealthy 1",
		"kubeagent_control_plane_unhealthy 1",
		"kubeagent_dns_servfail_ratio 0.12",
		"kubeagent_certificates_expired 1",
		"kubeagent_certificates_expiring 1",
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

// trackerWithFixture returns a tracker holding one active issue (60s old at
// `at`) and one resolved issue (fired 30s), for the metrics/JSON assertions.
func trackerWithFixture() (*watchstate.Tracker, time.Time) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	api := watchstate.Key{Kind: "Deployment", Namespace: "prod", Name: "api", Issue: "CrashLoopBackOff"}
	svc := watchstate.Key{Kind: "Service", Namespace: "prod", Name: "api", Issue: "NoEndpoints"}
	tr := watchstate.New(watchstate.Options{})
	tr.Observe([]watchstate.Key{api, svc}, base)
	tr.Observe([]watchstate.Key{api}, base.Add(30*time.Second)) // svc resolves after 30s
	at := base.Add(60 * time.Second)
	tr.Observe([]watchstate.Key{api}, at)
	return tr, at
}

func TestMetrics_RendersIssueSeries(t *testing.T) {
	m := newMetrics()
	tr, at := trackerWithFixture()
	m.updateIssues(tr, at)
	out := m.render()
	for _, want := range []string{
		"kubeagent_issues_active 1",
		"kubeagent_issues_flapping 0",
		"kubeagent_issues_new_total 2",
		"kubeagent_issues_resolved_total 1",
		"kubeagent_issues_flapping_total 0",
		"kubeagent_issues_dropped_total 0",
		"kubeagent_issue_resolution_seconds_sum 30",
		"kubeagent_issue_resolution_seconds_count 1",
		`kubeagent_issue_active{kind="Deployment",namespace="prod",name="api",issue="CrashLoopBackOff"} 1`,
		`kubeagent_issue_age_seconds{kind="Deployment",namespace="prod",name="api",issue="CrashLoopBackOff"} 60`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
	}
	// The resolved issue must not linger as an active series.
	if strings.Contains(out, `kubeagent_issue_active{kind="Service"`) {
		t.Errorf("resolved issue still rendered as active:\n%s", out)
	}
}

func TestMetrics_IssueSeriesAbsentBeforeFirstUpdate(t *testing.T) {
	out := newMetrics().render()
	if strings.Contains(out, "kubeagent_issue_active{") {
		t.Errorf("per-issue series rendered with no issues:\n%s", out)
	}
	if !strings.Contains(out, "kubeagent_issues_active 0") {
		t.Errorf("aggregate gauge must still render as 0:\n%s", out)
	}
}

func TestMetrics_IssuesEndpointShape(t *testing.T) {
	m := newMetrics()
	tr, at := trackerWithFixture()
	m.updateIssues(tr, at)
	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/issues")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /issues: status %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Active []struct {
			Kind        string `json:"kind"`
			Namespace   string `json:"namespace"`
			Name        string `json:"name"`
			Issue       string `json:"issue"`
			FirstSeen   string `json:"firstSeen"`
			FiringSince string `json:"firingSince"`
			LastSeen    string `json:"lastSeen"`
			Firings     int    `json:"firings"`
			Flapping    bool   `json:"flapping"`
			AgeSeconds  *int64 `json:"ageSeconds"`
			ResolvedAt  string `json:"resolvedAt"`
		} `json:"active"`
		Resolved []struct {
			Kind              string `json:"kind"`
			Name              string `json:"name"`
			Issue             string `json:"issue"`
			ResolvedAt        string `json:"resolvedAt"`
			ResolutionSeconds *int64 `json:"resolutionSeconds"`
			AgeSeconds        *int64 `json:"ageSeconds"`
		} `json:"resolved"`
		Stats struct {
			NewTotal               int64   `json:"newTotal"`
			ResolvedTotal          int64   `json:"resolvedTotal"`
			FlapTotal              int64   `json:"flapTotal"`
			DroppedTotal           int64   `json:"droppedTotal"`
			ResolutionSecondsSum   float64 `json:"resolutionSecondsSum"`
			ResolutionSecondsCount int64   `json:"resolutionSecondsCount"`
		} `json:"stats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding /issues: %v", err)
	}
	if len(body.Active) != 1 || len(body.Resolved) != 1 {
		t.Fatalf("active=%d resolved=%d, want 1 and 1", len(body.Active), len(body.Resolved))
	}
	a := body.Active[0]
	if a.Kind != "Deployment" || a.Namespace != "prod" || a.Name != "api" || a.Issue != "CrashLoopBackOff" {
		t.Errorf("active identity = %+v", a)
	}
	if a.FirstSeen != "2026-07-25T10:00:00Z" || a.FiringSince != "2026-07-25T10:00:00Z" || a.LastSeen != "2026-07-25T10:01:00Z" {
		t.Errorf("active timestamps = %+v, want RFC3339 UTC", a)
	}
	if a.AgeSeconds == nil || *a.AgeSeconds != 60 {
		t.Errorf("active ageSeconds = %v, want 60", a.AgeSeconds)
	}
	if a.ResolvedAt != "" {
		t.Errorf("active record must omit resolvedAt, got %q", a.ResolvedAt)
	}
	r := body.Resolved[0]
	if r.ResolvedAt != "2026-07-25T10:00:30Z" {
		t.Errorf("resolvedAt = %q, want 2026-07-25T10:00:30Z", r.ResolvedAt)
	}
	if r.ResolutionSeconds == nil || *r.ResolutionSeconds != 30 {
		t.Errorf("resolutionSeconds = %v, want 30", r.ResolutionSeconds)
	}
	if r.AgeSeconds != nil {
		t.Errorf("resolved record must omit ageSeconds, got %v", r.AgeSeconds)
	}
	if body.Stats.NewTotal != 2 || body.Stats.ResolvedTotal != 1 || body.Stats.ResolutionSecondsSum != 30 {
		t.Errorf("stats = %+v", body.Stats)
	}
}

func TestMetrics_IssuesEndpointEmptyArrays(t *testing.T) {
	srv := httptest.NewServer(newMetrics().handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/issues")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"active":[]`, `"resolved":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("empty tracker must render %s, got %s", want, raw)
		}
	}
}

// TestRender_AlertSeriesAlwaysPresent pins that the three alert series render even
// when alerting is disabled, so a dashboard does not break when it is switched on.
func TestRender_AlertSeriesAlwaysPresent(t *testing.T) {
	m := newMetrics()
	out := m.render()
	for _, want := range []string{
		`kubeagent_alerts_sent_total{status="firing",outcome="ok"} 0`,
		`kubeagent_alerts_sent_total{status="firing",outcome="failed"} 0`,
		`kubeagent_alerts_sent_total{status="resolved",outcome="ok"} 0`,
		`kubeagent_alerts_sent_total{status="resolved",outcome="failed"} 0`,
		`kubeagent_alerts_dropped_total{reason="queue_full"} 0`,
		`kubeagent_alerts_dropped_total{reason="retries_exhausted"} 0`,
		"kubeagent_alert_last_success_timestamp_seconds 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series %q", want)
		}
	}
}

func TestUpdateAlerts_RendersTheCounters(t *testing.T) {
	m := newMetrics()
	m.updateAlerts(alert.Stats{
		FiringOK: 3, FiringFailed: 1, ResolvedOK: 2, ResolvedFailed: 0,
		DroppedQueueFull: 7, DroppedRetriesExhausted: 4, LastSuccessUnix: 1770000000,
	})
	out := m.render()
	for _, want := range []string{
		`kubeagent_alerts_sent_total{status="firing",outcome="ok"} 3`,
		`kubeagent_alerts_sent_total{status="firing",outcome="failed"} 1`,
		`kubeagent_alerts_sent_total{status="resolved",outcome="ok"} 2`,
		`kubeagent_alerts_dropped_total{reason="queue_full"} 7`,
		`kubeagent_alerts_dropped_total{reason="retries_exhausted"} 4`,
		"kubeagent_alert_last_success_timestamp_seconds 1.77e+09",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series %q in:\n%s", want, out)
		}
	}
}
