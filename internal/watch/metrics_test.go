package watch

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/alert"
	"github.com/imantaba/kubeagent/internal/certhealth"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/controlplane"
	"github.com/imantaba/kubeagent/internal/dashboard"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/dnshealth"
	"github.com/imantaba/kubeagent/internal/hpahealth"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/jsonschema"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/oncall"
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
		Inventory: inventory.Result{
			Workloads: []inventory.Workload{
				{Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 1,
					Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff"}}},
			},
			// The one workload above is flagged (a Finding), so the census that
			// inventory.Prioritize would have computed alongside it is Good: 0,
			// Total: 1. sampleResult is reused by the SLO tests in slo_test.go and
			// watch_test.go as their "one broken workload" fixture; applyResult now
			// reads Census (not Workloads) to feed slo.Tracker.Observe, so this
			// fixture needs Census set explicitly to keep representing a broken
			// sample rather than an empty (no-data) one.
			Census: inventory.Census{Good: 0, Total: 1},
		},
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
	m := newMetrics([]string{"local"})
	m.update("local", sampleResult(), 150*time.Millisecond, time.Unix(1000, 0), nil)
	out := m.render()
	for _, want := range []string{
		`kubeagent_cluster_healthy{cluster="local"} 0`,
		`kubeagent_nodes_ready{cluster="local"} 2`,
		`kubeagent_nodes_total{cluster="local"} 3`,
		`kubeagent_workloads_flagged{cluster="local"} 1`,
		`kubeagent_findings{cluster="local",issue="CrashLoopBackOff"} 1`,
		`kubeagent_nodes_without_reservations{cluster="local"} 1`,
		`kubeagent_nodes_stale_heartbeat{cluster="local"} 1`,
		`kubeagent_pvcs_reclaim_delete{cluster="local"} 2`,
		`kubeagent_scans_total{cluster="local"} 1`,
		`kubeagent_node_fs_usage_ratio{cluster="local",node="n1"} 0.84`,
		`kubeagent_volumes_over_disk_threshold{cluster="local"} 1`,
		`kubeagent_ingress_route_issues{cluster="local"} 1`,
		`kubeagent_service_issues{cluster="local"} 1`,
		`kubeagent_pvc_pending_issues{cluster="local"} 1`,
		`kubeagent_resources_stuck_terminating{cluster="local"} 1`,
		`kubeagent_pdb_blocking_issues{cluster="local"} 1`,
		`kubeagent_hpa_scaling_issues{cluster="local"} 1`,
		`kubeagent_admission_webhooks_failing{cluster="local"} 1`,
		`kubeagent_admission_webhook_latency_risks{cluster="local"} 1`,
		`kubeagent_resourcequota_issues{cluster="local"} 1`,
		`kubeagent_nodes_expected_absent{cluster="local"} 1`,
		`kubeagent_kubelet_unhealthy{cluster="local"} 1`,
		`kubeagent_control_plane_unhealthy{cluster="local"} 1`,
		`kubeagent_dns_servfail_ratio{cluster="local"} 0.12`,
		`kubeagent_certificates_expired{cluster="local"} 1`,
		`kubeagent_certificates_expiring{cluster="local"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
	}
}

func TestMetrics_UpdateErrorKeepsLastGoodAndCountsError(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.update("local", sampleResult(), time.Millisecond, time.Unix(1000, 0), nil)
	m.update("local", nil, time.Millisecond, time.Unix(1001, 0), errors.New("boom"))
	out := m.render()
	if !strings.Contains(out, `kubeagent_scan_errors_total{cluster="local"} 1`) {
		t.Errorf("expected error counter, got:\n%s", out)
	}
	if !strings.Contains(out, `kubeagent_workloads_flagged{cluster="local"} 1`) {
		t.Errorf("error update must preserve last-good gauges, got:\n%s", out)
	}
}

func TestMetrics_ReadyzGate(t *testing.T) {
	m := newMetrics([]string{"local"})
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	if code := get(t, srv.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz before ready: want 503, got %d", code)
	}
	m.markReady("local")
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
	m := newMetrics([]string{"local"})
	tr, at := trackerWithFixture()
	m.updateIssues("local", tr, at)
	out := m.render()
	for _, want := range []string{
		`kubeagent_issues_active{cluster="local"} 1`,
		`kubeagent_issues_flapping{cluster="local"} 0`,
		`kubeagent_issues_new_total{cluster="local"} 2`,
		`kubeagent_issues_resolved_total{cluster="local"} 1`,
		`kubeagent_issues_flapping_total{cluster="local"} 0`,
		`kubeagent_issues_dropped_total{cluster="local"} 0`,
		`kubeagent_issue_resolution_seconds_sum{cluster="local"} 30`,
		`kubeagent_issue_resolution_seconds_count{cluster="local"} 1`,
		`kubeagent_issue_active{cluster="local",kind="Deployment",namespace="prod",name="api",issue="CrashLoopBackOff"} 1`,
		`kubeagent_issue_age_seconds{cluster="local",kind="Deployment",namespace="prod",name="api",issue="CrashLoopBackOff"} 60`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
	}
	// The resolved issue must not linger as an active series.
	if strings.Contains(out, `kubeagent_issue_active{cluster="local",kind="Service"`) {
		t.Errorf("resolved issue still rendered as active:\n%s", out)
	}
}

func TestMetrics_IssueSeriesAbsentBeforeFirstUpdate(t *testing.T) {
	out := newMetrics([]string{"local"}).render()
	if strings.Contains(out, "kubeagent_issue_active{") {
		t.Errorf("per-issue series rendered with no issues:\n%s", out)
	}
	if !strings.Contains(out, `kubeagent_issues_active{cluster="local"} 0`) {
		t.Errorf("aggregate gauge must still render as 0:\n%s", out)
	}
}

func TestMetrics_IssuesEndpointShape(t *testing.T) {
	m := newMetrics([]string{"local"})
	tr, at := trackerWithFixture()
	m.updateIssues("local", tr, at)
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
	srv := httptest.NewServer(newMetrics([]string{"local"}).handler())
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
	m := newMetrics([]string{"local"})
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
	m := newMetrics([]string{"local"})
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

func TestExplainMetricsRenderWhenEnabled(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.updateExplain(true, oncall.Stats{
		Allowed: 3, Throttled: 30, Failed: 1, Dropped: 2, BudgetRemaining: 17.5,
	}, nil)
	out := m.render()
	for _, want := range []string{
		"kubeagent_explain_allowed_total 3",
		"kubeagent_explain_throttled_total 30",
		"kubeagent_explain_failed_total 1",
		"kubeagent_explain_dropped_total 2",
		"kubeagent_explain_budget_remaining 17.5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q", want)
		}
	}
}

func TestExplainMetricsAbsentWhenDisabled(t *testing.T) {
	m := newMetrics([]string{"local"})
	if strings.Contains(m.render(), "kubeagent_explain_") {
		t.Error("no kubeagent_explain_ series may render when --explain is off")
	}
}

func TestExplanationsEndpointServesTheStore(t *testing.T) {
	m := newMetrics([]string{"local"})
	at := time.Date(2026, 7, 26, 10, 4, 12, 0, time.UTC)
	m.updateExplain(true, oncall.Stats{Allowed: 1, Throttled: 4, Failed: 0, Dropped: 0},
		[]oncall.Explanation{{
			Kind: "Deployment", Namespace: "shop", Name: "web",
			Issues: []string{"ImagePullBackOff"}, ExplainedAt: at,
			Model: "test-model", Text: "the tag is missing",
		}})

	rec := httptest.NewRecorder()
	m.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/explanations", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Explanations []struct {
			Kind        string   `json:"kind"`
			Namespace   string   `json:"namespace"`
			Name        string   `json:"name"`
			Issues      []string `json:"issues"`
			ExplainedAt string   `json:"explainedAt"`
			Model       string   `json:"model"`
			Text        string   `json:"text"`
		} `json:"explanations"`
		Stats struct {
			AllowedTotal   int64 `json:"allowedTotal"`
			ThrottledTotal int64 `json:"throttledTotal"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if len(got.Explanations) != 1 {
		t.Fatalf("got %d explanations, want 1", len(got.Explanations))
	}
	e := got.Explanations[0]
	if e.Kind != "Deployment" || e.Namespace != "shop" || e.Name != "web" {
		t.Errorf("object = %s/%s/%s, want Deployment/shop/web", e.Kind, e.Namespace, e.Name)
	}
	if e.Text != "the tag is missing" || e.Model != "test-model" {
		t.Errorf("text/model = %q/%q", e.Text, e.Model)
	}
	if e.ExplainedAt != "2026-07-26T10:04:12Z" {
		t.Errorf("explainedAt = %q, want RFC3339 UTC", e.ExplainedAt)
	}
	if got.Stats.AllowedTotal != 1 || got.Stats.ThrottledTotal != 4 {
		t.Errorf("stats = %+v", got.Stats)
	}
}

func TestExplanationsEndpointIsEmptyWhenDisabled(t *testing.T) {
	m := newMetrics([]string{"local"})
	rec := httptest.NewRecorder()
	m.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/explanations", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Explanations []interface{} `json:"explanations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Explanations) != 0 {
		t.Errorf("got %d explanations with --explain off, want 0", len(got.Explanations))
	}
}

// TestRenderLabelsEveryPerClusterSeries pins the label contract. It is emitted
// even with one cluster: a label that only appears once a second cluster is
// added would break every dashboard on the day an operator adds their second
// cluster, which is the worst possible moment.
func TestRenderLabelsEveryPerClusterSeries(t *testing.T) {
	m := newMetrics([]string{"prod-us", "prod-eu"})
	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	m.update("prod-eu", sampleResult(), time.Millisecond, at, nil)
	m.update("prod-us", sampleResult(), time.Millisecond, at, nil)

	out := m.render()
	for _, want := range []string{
		`kubeagent_cluster_healthy{cluster="prod-eu"}`,
		`kubeagent_cluster_healthy{cluster="prod-us"}`,
		`kubeagent_nodes_ready{cluster="prod-eu"}`,
		`kubeagent_workloads_flagged{cluster="prod-us"}`,
		`kubeagent_scans_total{cluster="prod-eu"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render() missing %s\n%s", want, out)
		}
	}

	// Clusters render in sorted order, so the output is stable across restarts.
	if strings.Index(out, `kubeagent_cluster_healthy{cluster="prod-eu"}`) >
		strings.Index(out, `kubeagent_cluster_healthy{cluster="prod-us"}`) {
		t.Error("clusters must render in sorted order")
	}
}

// TestRenderEmitsOneHelpPerFamily pins the exposition format. Prometheus rejects
// a scrape that repeats HELP for a family, so the header cannot move inside the
// per-cluster loop.
func TestRenderEmitsOneHelpPerFamily(t *testing.T) {
	m := newMetrics([]string{"a", "b", "c"})
	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	for _, c := range []string{"a", "b", "c"} {
		m.update(c, sampleResult(), time.Millisecond, at, nil)
	}
	if got := strings.Count(m.render(), "# HELP kubeagent_nodes_ready "); got != 1 {
		t.Errorf("HELP for kubeagent_nodes_ready appears %d times, want 1", got)
	}
}

// TestRenderLeavesProcessWideSeriesUnlabelled pins the other half of the
// contract: there is one alert sink and one explanation budget, so labelling
// their counters per cluster would attribute a process-wide number to a cluster
// that did not produce it.
func TestRenderLeavesProcessWideSeriesUnlabelled(t *testing.T) {
	m := newMetrics([]string{"prod-eu", "prod-us"})
	m.updateAlerts(alert.Stats{FiringOK: 3})
	out := m.render()
	if !strings.Contains(out, `kubeagent_alerts_sent_total{status="firing",outcome="ok"} 3`) {
		t.Errorf("alert series must keep exactly its existing labels\n%s", out)
	}
	if strings.Contains(out, `kubeagent_alerts_sent_total{cluster=`) {
		t.Error("alert series must not carry a cluster label")
	}
	if !strings.Contains(out, "kubeagent_clusters_total 2\n") {
		t.Errorf("kubeagent_clusters_total must be unlabelled and equal the target count\n%s", out)
	}
}

// TestClusterUpReportsPerClusterEvaluationOutcome pins the degradation signal:
// one cluster erroring must not disturb the others' readings.
func TestClusterUpReportsPerClusterEvaluationOutcome(t *testing.T) {
	m := newMetrics([]string{"good", "bad"})
	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	m.update("good", sampleResult(), time.Millisecond, at, nil)
	m.update("bad", &scan.Result{}, time.Millisecond, at, errors.New("connection refused"))

	out := m.render()
	if !strings.Contains(out, `kubeagent_cluster_up{cluster="good"} 1`) {
		t.Errorf("healthy cluster must report up=1\n%s", out)
	}
	if !strings.Contains(out, `kubeagent_cluster_up{cluster="bad"} 0`) {
		t.Errorf("erroring cluster must report up=0\n%s", out)
	}
	if !strings.Contains(out, `kubeagent_scan_errors_total{cluster="bad"} 1`) {
		t.Errorf("the error must be counted against its own cluster\n%s", out)
	}
	if !strings.Contains(out, `kubeagent_scan_errors_total{cluster="good"} 0`) {
		t.Errorf("the healthy cluster must not inherit the error\n%s", out)
	}
}

// TestIsReadyWaitsForEveryCluster pins the readiness rule: ready means "every
// target has finished its first reconcile attempt", never "everything is fine".
func TestIsReadyWaitsForEveryCluster(t *testing.T) {
	m := newMetrics([]string{"a", "b"})
	if m.isReady() {
		t.Fatal("must not be ready before any cluster reports")
	}
	m.markReady("a")
	if m.isReady() {
		t.Error("must not be ready with one cluster outstanding")
	}
	m.markReady("b")
	if !m.isReady() {
		t.Error("must be ready once every cluster has reported")
	}
}

// TestIssuesJSONMergesClustersAndNamesEachRecord pins the /issues shape: the
// arrays merge across clusters with each record naming its own, the stats sum,
// and a clusters array reports per-target status so an operator can tell "no
// issues" apart from "that cluster is unreachable".
func TestIssuesJSONMergesClustersAndNamesEachRecord(t *testing.T) {
	m := newMetrics([]string{"prod-eu", "prod-us"})
	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)

	eu := watchstate.New(watchstate.Options{})
	eu.Observe([]watchstate.Key{{Kind: "Deployment", Namespace: "shop", Name: "web", Issue: "CrashLoopBackOff"}}, at)
	m.update("prod-eu", sampleResult(), time.Millisecond, at, nil)
	m.updateIssues("prod-eu", eu, at)

	us := watchstate.New(watchstate.Options{})
	us.Observe([]watchstate.Key{{Kind: "Deployment", Namespace: "shop", Name: "web", Issue: "ImagePullBackOff"}}, at)
	m.update("prod-us", &scan.Result{}, time.Millisecond, at, errors.New("connection refused"))
	m.updateIssues("prod-us", us, at)

	body, err := m.issuesJSON()
	if err != nil {
		t.Fatalf("issuesJSON: %v", err)
	}
	var got struct {
		Clusters []struct {
			Name     string `json:"name"`
			Up       bool   `json:"up"`
			LastScan string `json:"lastScan"`
			Error    string `json:"error"`
		} `json:"clusters"`
		Active []struct {
			Cluster string `json:"cluster"`
			Name    string `json:"name"`
			Issue   string `json:"issue"`
		} `json:"active"`
		Stats struct {
			NewTotal int64 `json:"newTotal"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(got.Clusters) != 2 {
		t.Fatalf("clusters has %d entries, want 2", len(got.Clusters))
	}
	if got.Clusters[0].Name != "prod-eu" || !got.Clusters[0].Up {
		t.Errorf("clusters[0] = %+v, want prod-eu up", got.Clusters[0])
	}
	if got.Clusters[1].Up {
		t.Errorf("prod-us must report up=false")
	}
	if got.Clusters[1].Error == "" {
		t.Error("an unreachable cluster must report why")
	}

	seen := map[string]string{}
	for _, r := range got.Active {
		seen[r.Cluster] = r.Issue
	}
	if seen["prod-eu"] != "CrashLoopBackOff" || seen["prod-us"] != "ImagePullBackOff" {
		t.Errorf("active records = %v, want one per cluster with its own issue", seen)
	}
	if got.Stats.NewTotal != 2 {
		t.Errorf("stats.newTotal = %d, want 2 (summed across clusters)", got.Stats.NewTotal)
	}
}

func TestExplanationsJSONNamesTheCluster(t *testing.T) {
	m := newMetrics([]string{"prod-eu"})
	m.updateExplain(true, oncall.Stats{Allowed: 1}, []oncall.Explanation{{
		Cluster: "prod-eu", Kind: "Deployment", Namespace: "shop", Name: "web",
		Issues: []string{"ImagePullBackOff"}, ExplainedAt: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
		Model: "test-model", Text: "the image tag does not exist",
	}})
	body, err := m.explanationsJSON()
	if err != nil {
		t.Fatalf("explanationsJSON: %v", err)
	}
	if !strings.Contains(string(body), `"cluster":"prod-eu"`) {
		t.Errorf("explanations must name the cluster\n%s", body)
	}
}

func TestIssuesAndExplanationsJSONStampTheSchemaVersion(t *testing.T) {
	m := newMetrics([]string{"local"})
	for name, get := range map[string]func() ([]byte, error){
		"/issues":       m.issuesJSON,
		"/explanations": m.explanationsJSON,
	} {
		raw, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var doc struct {
			SchemaVersion string `json:"schemaVersion"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s is not valid JSON: %v", name, err)
		}
		if doc.SchemaVersion != jsonschema.WatchVersion {
			t.Errorf("%s schemaVersion = %q, want %q", name, doc.SchemaVersion, jsonschema.WatchVersion)
		}
	}
}

// TestDashboardDisabledReturns404 asserts that a daemon without --dashboard
// does not register the path at all. The mux's own not-found handling answers,
// so there is no switched-off page to serve and no handler to get wrong.
func TestDashboardDisabledReturns404(t *testing.T) {
	srv := httptest.NewServer(newMetrics([]string{"local"}).handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatalf("GET /dashboard: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestDashboardEnabledServesHTML covers the enabled path end to end: the
// status, the content type, and that the page names the tracked incident.
func TestDashboardEnabledServesHTML(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.dashboard = true
	m.version = "v1.2.0"
	c := m.clusters["local"]
	c.up = true
	c.lastScanUnix = time.Date(2026, 8, 2, 9, 29, 0, 0, time.UTC).Unix()
	c.issues = issueSnapshot{
		At: time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC),
		Active: []watchstate.Record{{
			Key:         watchstate.Key{Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff"},
			FirstSeen:   time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
			FiringSince: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
			LastSeen:    time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC),
			Firings:     1,
		}},
	}

	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatalf("GET /dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}
	page := string(body)
	for _, want := range []string{"v1.2.0", "example-ns/web", "ImagePullBackOff"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
}

// TestDashboardInputCopies asserts the builder copies rather than aliases.
// The renderer runs after the read lock is released, so an Input holding a
// reference into a snapshot a worker can swap would be a data race that
// happens to produce plausible output most of the time.
func TestDashboardInputCopies(t *testing.T) {
	m := newMetrics([]string{"local"})
	c := m.clusters["local"]
	c.up = true
	c.lastScanUnix = time.Date(2026, 8, 2, 9, 29, 0, 0, time.UTC).Unix()
	c.issues = issueSnapshot{
		At: time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC),
		Active: []watchstate.Record{{
			Key:         watchstate.Key{Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff"},
			FiringSince: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
			Firings:     1,
		}},
	}
	m.explain = explainSnapshot{Enabled: true, Latest: []oncall.Explanation{{
		Cluster: "local", Kind: "Deployment", Namespace: "example-ns", Name: "web",
		Issues: []string{"ImagePullBackOff"}, ExplainedAt: time.Date(2026, 8, 2, 9, 5, 0, 0, time.UTC),
		Model: "example-model", Text: "example text",
	}}}

	in := m.dashboardInput("v1.2.0", time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC))

	// Mutate everything the snapshot owns, including the one slice field an
	// Input reaches into.
	c.issues.Active[0].Key.Name = "mutated"
	c.lastError = "mutated"
	m.explain.Latest[0].Issues[0] = "mutated"

	if in.Active[0].Name != "web" {
		t.Errorf("Active[0].Name = %q — the Input aliases the snapshot's records", in.Active[0].Name)
	}
	if in.Explanations[0].Issues[0] != "ImagePullBackOff" {
		t.Errorf("Explanations[0].Issues[0] = %q — the Input aliases the explanation's issue slice", in.Explanations[0].Issues[0])
	}
}

// TestDashboardRenderFailureIs500 covers the path buffering exists for. A
// template failure mid-execution would otherwise land after the 200 header and
// produce a truncated page; buffering turns it into a clean 500 with no body
// from the renderer.
func TestDashboardRenderFailureIs500(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.dashboard = true

	orig := renderDashboard
	renderDashboard = func(w io.Writer, in dashboard.Input) error {
		io.WriteString(w, "<html>partial")
		return errors.New("synthetic template failure")
	}
	defer func() { renderDashboard = orig }()

	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatalf("GET /dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if strings.Contains(string(body), "partial") {
		t.Error("a partially rendered page reached the client")
	}
}

// TestDashboardConcurrentReadsAndSwaps is the race test. Run under -race it
// asserts that no dashboard request observes a snapshot a worker is writing.
func TestDashboardConcurrentReadsAndSwaps(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.dashboard = true
	m.version = "v1.2.0"

	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			m.mu.Lock()
			c := m.clusters["local"]
			c.up = i%2 == 0
			c.lastScanUnix = int64(1754126000 + i)
			c.lastError = "connection refused"
			c.issues = issueSnapshot{
				At: time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC),
				Active: []watchstate.Record{{
					Key:         watchstate.Key{Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff"},
					FiringSince: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
					Firings:     i,
				}},
			}
			m.mu.Unlock()
		}
	}()

	for i := 0; i < 50; i++ {
		resp, err := http.Get(srv.URL + "/dashboard")
		if err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("GET /dashboard: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			close(done)
			wg.Wait()
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}
	close(done)
	wg.Wait()
}
