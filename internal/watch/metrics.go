package watch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imantaba/kubeagent/internal/alert"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/oncall"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/slo"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

// realServiceIssues counts Service issues that need attention, excluding
// intentionally-empty (Expected/parked) ones so alerts don't fire on backends
// that are empty by design.
func realServiceIssues(issues []svchealth.Issue) int {
	n := 0
	for _, is := range issues {
		if !is.Expected {
			n++
		}
	}
	return n
}

// realIngressIssues counts broken Ingress routes, excluding intentionally-empty
// (Expected/parked) ones.
func realIngressIssues(issues []ingresshealth.RouteIssue) int {
	n := 0
	for _, r := range issues {
		if !r.Expected {
			n++
		}
	}
	return n
}

// issueSnapshot is the tracker's state as of the last reconcile. Ages are
// measured from At, so /metrics and /issues never disagree about an issue's age.
type issueSnapshot struct {
	At       time.Time
	Active   []watchstate.Record
	Resolved []watchstate.Record
	Stats    watchstate.Stats
}

// sloSnapshot is the SLO tracker's state as of the last reconcile. Enabled is
// false when --slo-target was not set, in which case no SLO series render at all.
type sloSnapshot struct {
	Enabled    bool
	Target     float64
	Fast, Slow slo.Report
}

// explainSnapshot is the explainer's state as of the last reconcile. Enabled is
// false when --explain was not set, in which case no explain series render at
// all — the absence of the series is how an operator sees the feature is off.
type explainSnapshot struct {
	Enabled bool
	Stats   oncall.Stats
	Latest  []oncall.Explanation
}

// metrics holds the latest evaluation snapshot and renders it as Prometheus text.
// All access is mutex-guarded; the daemon updates it from the reconcile loop and
// the HTTP handler reads it.
type metrics struct {
	mu                    sync.RWMutex
	ready                 bool
	healthy               float64
	nodesReady            int
	nodesTotal            int
	nodesNoReserve        int
	nodesStaleHeartbeat   int
	nodesExpectedAbsent   int
	kubeletUnhealthy      int
	controlPlaneUnhealthy int
	dnsServfailRatio      float64
	pvcsReclaimDelete     int
	flagged               int
	serviceIssues         int
	ingressIssues         int
	pvcPendingIssues      int
	stuckTerminating      int
	pdbBlockingIssues     int
	hpaScalingIssues      int
	webhooksFailing       int
	webhookLatencyRisks   int
	quotaIssues           int
	findings              map[string]int
	lastScanUnix          int64
	scanSeconds           float64
	scansTotal            int64
	scanErrors            int64
	nodeFSRatio           map[string]float64
	volumesOverDisk       int
	certsRan              bool
	certsExpired          int
	certsExpiring         int
	issues                issueSnapshot
	alerts                alert.Stats
	slo                   sloSnapshot
	explain               explainSnapshot
}

func newMetrics() *metrics { return &metrics{findings: map[string]int{}} }

// update records one reconcile. On err != nil only the attempt/error counters and
// timing move; the last good snapshot of gauges is preserved.
func (m *metrics) update(res *scan.Result, dur time.Duration, now time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scansTotal++
	m.scanSeconds = dur.Seconds()
	m.lastScanUnix = now.Unix()
	if err != nil {
		m.scanErrors++
		return
	}
	if res.Health.Verdict == "Healthy" {
		m.healthy = 1
	} else {
		m.healthy = 0
	}
	m.nodesReady = res.Health.NodesReady
	m.nodesTotal = res.Health.NodesTotal
	m.nodesNoReserve = res.NodeReserve.WarnCount
	m.nodesStaleHeartbeat = res.Health.NodesStaleHeartbeat
	m.nodesExpectedAbsent = res.Health.NodesExpectedAbsent
	m.kubeletUnhealthy = len(res.KubeletHealth.Unhealthy)
	m.controlPlaneUnhealthy = 0
	if res.ControlPlane.Status == "unhealthy" {
		m.controlPlaneUnhealthy = 1
	}
	m.dnsServfailRatio = res.DNS.ServfailRatio
	m.pvcsReclaimDelete = res.PVCReclaim.Count
	m.serviceIssues = realServiceIssues(res.ServiceIssues)
	m.ingressIssues = realIngressIssues(res.IngressIssues)
	m.pvcPendingIssues = len(res.PVCIssues)
	m.stuckTerminating = len(res.StuckTerminating)
	m.pdbBlockingIssues = len(res.PDBIssues)
	m.hpaScalingIssues = len(res.HPAIssues)
	m.webhooksFailing = 0
	m.webhookLatencyRisks = 0
	for _, i := range res.WebhookIssues {
		if i.Problem == "high-timeout" {
			m.webhookLatencyRisks++
		} else {
			m.webhooksFailing++
		}
	}
	m.quotaIssues = len(res.QuotaIssues)
	flagged := 0
	findings := map[string]int{}
	for _, w := range res.Inventory.Workloads {
		if w.Flagged() {
			flagged++
		}
		for _, f := range w.Findings {
			findings[f.Issue]++
		}
	}
	m.flagged = flagged
	m.findings = findings
	if len(res.DiskUsage.Nodes) > 0 {
		ratios := make(map[string]float64, len(res.DiskUsage.Nodes))
		for _, n := range res.DiskUsage.Nodes {
			ratios[n.Node] = n.Ratio
		}
		m.nodeFSRatio = ratios
		m.volumesOverDisk = len(res.DiskUsage.Over)
	}
	if res.Certificates != nil {
		m.certsRan = true
		m.certsExpired = len(res.Certificates.Expired)
		m.certsExpiring = len(res.Certificates.Expiring)
	}
}

func (m *metrics) markReady() { m.mu.Lock(); m.ready = true; m.mu.Unlock() }
func (m *metrics) isReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ready
}

// updateIssues records the tracker state for rendering. now becomes the snapshot's
// reference time for every age it reports.
func (m *metrics) updateIssues(tr *watchstate.Tracker, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.issues = issueSnapshot{At: now, Active: tr.Active(), Resolved: tr.RecentlyResolved(), Stats: tr.Stats()}
}

// updateAlerts records the sink's delivery counters for rendering.
func (m *metrics) updateAlerts(s alert.Stats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts = s
}

// updateSLO records the SLO tracker's latest report for rendering.
func (m *metrics) updateSLO(enabled bool, target float64, fast, slow slo.Report) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slo = sloSnapshot{Enabled: enabled, Target: target, Fast: fast, Slow: slow}
}

// updateExplain folds the explainer's state into the served snapshot.
func (m *metrics) updateExplain(enabled bool, s oncall.Stats, latest []oncall.Explanation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.explain = explainSnapshot{Enabled: enabled, Stats: s, Latest: latest}
}

func (m *metrics) render() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var b strings.Builder
	gauge := func(name, help string, v float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
	}
	counter := func(name, help string, v int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	counterF := func(name, help string, v float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %g\n", name, help, name, name, v)
	}
	gauge("kubeagent_cluster_healthy", "1 if the cluster verdict is Healthy, else 0", m.healthy)
	gauge("kubeagent_nodes_ready", "Number of Ready nodes", float64(m.nodesReady))
	gauge("kubeagent_nodes_total", "Total number of nodes", float64(m.nodesTotal))
	gauge("kubeagent_nodes_without_reservations", "Nodes whose kubelet reserves no memory (allocatable == capacity)", float64(m.nodesNoReserve))
	gauge("kubeagent_nodes_stale_heartbeat", "Ready nodes whose kubelet lease is stale (kubelet not heartbeating)", float64(m.nodesStaleHeartbeat))
	gauge("kubeagent_nodes_expected_absent", "Declared expected nodes that are absent from the cluster", float64(m.nodesExpectedAbsent))
	gauge("kubeagent_kubelet_unhealthy", "Nodes whose kubelet /healthz reported unhealthy", float64(m.kubeletUnhealthy))
	gauge("kubeagent_control_plane_unhealthy", "Apiserver /readyz reported the control plane not ready", float64(m.controlPlaneUnhealthy))
	gauge("kubeagent_dns_servfail_ratio", "CoreDNS SERVFAIL+REFUSED response ratio (0 when healthy or not probed)", m.dnsServfailRatio)
	gauge("kubeagent_pvcs_reclaim_delete", "PVCs whose bound PV has reclaimPolicy Delete", float64(m.pvcsReclaimDelete))
	gauge("kubeagent_workloads_flagged", "Number of workloads currently flagged", float64(m.flagged))
	gauge("kubeagent_service_issues", "Number of Service issues", float64(m.serviceIssues))
	gauge("kubeagent_ingress_route_issues", "Ingress routes whose backend Service is missing, has no ready endpoints, or does not expose the referenced port", float64(m.ingressIssues))
	gauge("kubeagent_pvc_pending_issues", "PVCs stuck Pending because provisioning or binding failed", float64(m.pvcPendingIssues))
	gauge("kubeagent_resources_stuck_terminating", "Resources (namespaces, pods, PVCs) wedged in Terminating past the threshold", float64(m.stuckTerminating))
	gauge("kubeagent_pdb_blocking_issues", "PodDisruptionBudgets that will block a node drain", float64(m.pdbBlockingIssues))
	gauge("kubeagent_hpa_scaling_issues", "HorizontalPodAutoscalers that cannot scale as intended", float64(m.hpaScalingIssues))
	gauge("kubeagent_admission_webhooks_failing", "Fail-policy admission webhooks whose backend is missing or has no ready endpoints", float64(m.webhooksFailing))
	gauge("kubeagent_admission_webhook_latency_risks", "Fail-policy admission webhooks with a high timeoutSeconds (a latency landmine)", float64(m.webhookLatencyRisks))
	gauge("kubeagent_resourcequota_issues", "ResourceQuota entries at or over the usage threshold", float64(m.quotaIssues))
	fmt.Fprintf(&b, "# HELP kubeagent_findings Current findings by issue type\n# TYPE kubeagent_findings gauge\n")
	issues := make([]string, 0, len(m.findings))
	for k := range m.findings {
		issues = append(issues, k)
	}
	sort.Strings(issues)
	for _, k := range issues {
		fmt.Fprintf(&b, "kubeagent_findings{issue=%q} %d\n", k, m.findings[k])
	}
	flapping := 0
	for _, r := range m.issues.Active {
		if r.Flapping {
			flapping++
		}
	}
	gauge("kubeagent_issues_active", "Issues currently firing, tracked across reconciles", float64(len(m.issues.Active)))
	gauge("kubeagent_issues_flapping", "Active issues that have crossed the flap threshold", float64(flapping))
	counter("kubeagent_issues_new_total", "Issue firings observed since start", m.issues.Stats.NewTotal)
	counter("kubeagent_issues_resolved_total", "Issue firings that resolved since start", m.issues.Stats.ResolvedTotal)
	counter("kubeagent_issues_flapping_total", "Times an issue crossed the flap threshold since start", m.issues.Stats.FlapTotal)
	counter("kubeagent_issues_dropped_total", "New issues left untracked because the tracker is at capacity", m.issues.Stats.DroppedTotal)
	counterF("kubeagent_issue_resolution_seconds_sum", "Seconds issues spent firing before resolving (MTTR numerator)", m.issues.Stats.ResolutionSecondsSum)
	counter("kubeagent_issue_resolution_seconds_count", "Issue firings that resolved (MTTR denominator)", m.issues.Stats.ResolutionSecondsCount)
	if len(m.issues.Active) > 0 {
		fmt.Fprintf(&b, "# HELP kubeagent_issue_active 1 while this issue instance is firing\n# TYPE kubeagent_issue_active gauge\n")
		for _, r := range m.issues.Active {
			fmt.Fprintf(&b, "kubeagent_issue_active{%s} 1\n", issueLabels(r.Key))
		}
		fmt.Fprintf(&b, "# HELP kubeagent_issue_age_seconds Seconds since this issue instance started firing\n# TYPE kubeagent_issue_age_seconds gauge\n")
		for _, r := range m.issues.Active {
			fmt.Fprintf(&b, "kubeagent_issue_age_seconds{%s} %d\n", issueLabels(r.Key), ageSeconds(r.FiringSince, m.issues.At))
		}
	}
	if len(m.nodeFSRatio) > 0 {
		names := make([]string, 0, len(m.nodeFSRatio))
		for n := range m.nodeFSRatio {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "# HELP kubeagent_node_fs_usage_ratio Node root filesystem used/capacity (0-1)\n# TYPE kubeagent_node_fs_usage_ratio gauge\n")
		for _, n := range names {
			fmt.Fprintf(&b, "kubeagent_node_fs_usage_ratio{node=%q} %g\n", n, m.nodeFSRatio[n])
		}
		gauge("kubeagent_volumes_over_disk_threshold", "Node+PVC volumes at or over the disk-usage threshold", float64(m.volumesOverDisk))
	}
	if m.certsRan {
		gauge("kubeagent_certificates_expired", "TLS certificates already expired (opt-in --certs)", float64(m.certsExpired))
		gauge("kubeagent_certificates_expiring", "TLS certificates expiring within the warn window (opt-in --certs)", float64(m.certsExpiring))
	}
	fmt.Fprintf(&b, "# HELP kubeagent_alerts_sent_total Alert notifications delivered since start\n# TYPE kubeagent_alerts_sent_total counter\n")
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "firing", "ok", m.alerts.FiringOK)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "firing", "failed", m.alerts.FiringFailed)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "resolved", "ok", m.alerts.ResolvedOK)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "resolved", "failed", m.alerts.ResolvedFailed)
	fmt.Fprintf(&b, "# HELP kubeagent_alerts_dropped_total Alert notifications dropped without delivery\n# TYPE kubeagent_alerts_dropped_total counter\n")
	fmt.Fprintf(&b, "kubeagent_alerts_dropped_total{reason=%q} %d\n", "queue_full", m.alerts.DroppedQueueFull)
	fmt.Fprintf(&b, "kubeagent_alerts_dropped_total{reason=%q} %d\n", "retries_exhausted", m.alerts.DroppedRetriesExhausted)
	gauge("kubeagent_alert_last_success_timestamp_seconds", "Unix time of the last successful alert delivery (0 if none)", float64(m.alerts.LastSuccessUnix))
	if m.slo.Enabled {
		labelled := func(name, help string, fast, slow float64) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
			fmt.Fprintf(&b, "%s{window=\"fast\"} %g\n", name, fast)
			fmt.Fprintf(&b, "%s{window=\"slow\"} %g\n", name, slow)
		}
		gauge("kubeagent_slo_target_ratio", "Configured availability SLO as a ratio", m.slo.Target)
		labelled("kubeagent_slo_availability_ratio",
			"Time-weighted fraction of workload-seconds that are not flagged, over the window",
			m.slo.Fast.Availability, m.slo.Slow.Availability)
		labelled("kubeagent_slo_burn_rate",
			"Error-budget consumption multiple over the window (1 = spending exactly at budget)",
			m.slo.Fast.BurnRate, m.slo.Slow.BurnRate)
		labelled("kubeagent_slo_window_coverage_ratio",
			"Fraction of the window carrying samples; below 0.6 the burn alert is suppressed",
			m.slo.Fast.Coverage, m.slo.Slow.Coverage)
		// Clamped at zero: a burn above 1x means the window's budget is already
		// spent, and a negative "remaining" is nonsense on a dashboard.
		remaining := 1 - m.slo.Slow.BurnRate
		if remaining < 0 {
			remaining = 0
		}
		gauge("kubeagent_slo_error_budget_remaining_ratio",
			"Fraction of the error budget left over the slow window, clamped to [0,1]", remaining)
	}
	if m.explain.Enabled {
		counter("kubeagent_explain_allowed_total", "Incident explanations the throttle admitted since start", m.explain.Stats.Allowed)
		counter("kubeagent_explain_throttled_total", "Incident explanations refused by the cooldown or the hourly budget", m.explain.Stats.Throttled)
		counter("kubeagent_explain_failed_total", "Incident explanations whose model call errored or returned no text", m.explain.Stats.Failed)
		counter("kubeagent_explain_dropped_total", "Incident explanations admitted but dropped because the worker queue was full", m.explain.Stats.Dropped)
		gauge("kubeagent_explain_budget_remaining", "Model calls left in the hourly budget", m.explain.Stats.BudgetRemaining)
	}
	gauge("kubeagent_last_scan_timestamp_seconds", "Unix time of the last evaluation", float64(m.lastScanUnix))
	gauge("kubeagent_scan_duration_seconds", "Duration of the last evaluation in seconds", m.scanSeconds)
	counter("kubeagent_scans_total", "Total evaluations run", m.scansTotal)
	counter("kubeagent_scan_errors_total", "Total evaluations that errored", m.scanErrors)
	return b.String()
}

// handler serves /metrics, /issues, /explanations, /healthz, and /readyz.
func (m *metrics) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		io.WriteString(w, m.render())
	})
	mux.HandleFunc("/issues", func(w http.ResponseWriter, _ *http.Request) {
		body, err := m.issuesJSON()
		if err != nil {
			http.Error(w, "encoding issues", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
	mux.HandleFunc("/explanations", func(w http.ResponseWriter, _ *http.Request) {
		body, err := m.explanationsJSON()
		if err != nil {
			http.Error(w, "encoding explanations", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !m.isReady() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok")
	})
	return mux
}

func issueLabels(k watchstate.Key) string {
	return fmt.Sprintf("kind=%q,namespace=%q,name=%q,issue=%q", k.Kind, k.Namespace, k.Name, k.Issue)
}

// ageSeconds is whole seconds from since to at, floored at zero (a snapshot can
// never legitimately predate the firing it describes).
func ageSeconds(since, at time.Time) int64 {
	s := int64(at.Sub(since).Seconds())
	if s < 0 {
		return 0
	}
	return s
}

// issueView is one record as served by /issues. The pointer fields distinguish
// "not applicable" from a legitimate zero: active records carry ageSeconds and
// omit resolution data, resolved records the reverse.
type issueView struct {
	Kind              string `json:"kind"`
	Namespace         string `json:"namespace,omitempty"`
	Name              string `json:"name"`
	Issue             string `json:"issue"`
	FirstSeen         string `json:"firstSeen"`
	FiringSince       string `json:"firingSince"`
	LastSeen          string `json:"lastSeen"`
	Firings           int    `json:"firings"`
	Flapping          bool   `json:"flapping"`
	AgeSeconds        *int64 `json:"ageSeconds,omitempty"`
	ResolvedAt        string `json:"resolvedAt,omitempty"`
	ResolutionSeconds *int64 `json:"resolutionSeconds,omitempty"`
}

type statsView struct {
	NewTotal               int64   `json:"newTotal"`
	ResolvedTotal          int64   `json:"resolvedTotal"`
	FlapTotal              int64   `json:"flapTotal"`
	DroppedTotal           int64   `json:"droppedTotal"`
	ResolutionSecondsSum   float64 `json:"resolutionSecondsSum"`
	ResolutionSecondsCount int64   `json:"resolutionSecondsCount"`
}

type issuesView struct {
	Active   []issueView `json:"active"`
	Resolved []issueView `json:"resolved"`
	Stats    statsView   `json:"stats"`
}

func issueViews(rs []watchstate.Record, at time.Time, resolved bool) []issueView {
	out := make([]issueView, 0, len(rs))
	for _, r := range rs {
		v := issueView{
			Kind:        r.Key.Kind,
			Namespace:   r.Key.Namespace,
			Name:        r.Key.Name,
			Issue:       r.Key.Issue,
			FirstSeen:   rfc3339(r.FirstSeen),
			FiringSince: rfc3339(r.FiringSince),
			LastSeen:    rfc3339(r.LastSeen),
			Firings:     r.Firings,
			Flapping:    r.Flapping,
		}
		if resolved {
			v.ResolvedAt = rfc3339(r.ResolvedAt)
			secs := ageSeconds(r.FiringSince, r.ResolvedAt)
			v.ResolutionSeconds = &secs
		} else {
			secs := ageSeconds(r.FiringSince, at)
			v.AgeSeconds = &secs
		}
		out = append(out, v)
	}
	return out
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// issuesJSON renders the tracked-issue snapshot. Held under the read lock so the
// reconcile loop cannot swap the snapshot mid-encode.
func (m *metrics) issuesJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return json.Marshal(issuesView{
		Active:   issueViews(m.issues.Active, m.issues.At, false),
		Resolved: issueViews(m.issues.Resolved, m.issues.At, true),
		Stats: statsView{
			NewTotal:               m.issues.Stats.NewTotal,
			ResolvedTotal:          m.issues.Stats.ResolvedTotal,
			FlapTotal:              m.issues.Stats.FlapTotal,
			DroppedTotal:           m.issues.Stats.DroppedTotal,
			ResolutionSecondsSum:   m.issues.Stats.ResolutionSecondsSum,
			ResolutionSecondsCount: m.issues.Stats.ResolutionSecondsCount,
		},
	})
}

// explanationView is one record as served by /explanations.
type explanationView struct {
	Kind        string   `json:"kind"`
	Namespace   string   `json:"namespace,omitempty"`
	Name        string   `json:"name"`
	Issues      []string `json:"issues"`
	ExplainedAt string   `json:"explainedAt"`
	Model       string   `json:"model"`
	Text        string   `json:"text"`
}

type explainStatsView struct {
	AllowedTotal    int64   `json:"allowedTotal"`
	ThrottledTotal  int64   `json:"throttledTotal"`
	FailedTotal     int64   `json:"failedTotal"`
	DroppedTotal    int64   `json:"droppedTotal"`
	BudgetRemaining float64 `json:"budgetRemaining"`
}

type explanationsView struct {
	Explanations []explanationView `json:"explanations"`
	Stats        explainStatsView  `json:"stats"`
}

// explanationsJSON renders the latest explanation per object. With --explain off
// the list is empty rather than the endpoint absent, so a probe gets a stable
// shape either way.
func (m *metrics) explanationsJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := explanationsView{
		Explanations: []explanationView{},
		Stats: explainStatsView{
			AllowedTotal:    m.explain.Stats.Allowed,
			ThrottledTotal:  m.explain.Stats.Throttled,
			FailedTotal:     m.explain.Stats.Failed,
			DroppedTotal:    m.explain.Stats.Dropped,
			BudgetRemaining: m.explain.Stats.BudgetRemaining,
		},
	}
	for _, x := range m.explain.Latest {
		issues := x.Issues
		if issues == nil {
			issues = []string{}
		}
		out.Explanations = append(out.Explanations, explanationView{
			Kind:        x.Kind,
			Namespace:   x.Namespace,
			Name:        x.Name,
			Issues:      issues,
			ExplainedAt: x.ExplainedAt.UTC().Format(time.RFC3339),
			Model:       x.Model,
			Text:        x.Text,
		})
	}
	return json.Marshal(out)
}
