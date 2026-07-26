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

// clusterSnapshot is one cluster's evaluation state as of its last reconcile.
// Every field here was a field on metrics before the daemon learned to watch
// more than one cluster; the split is what keeps two workers from clobbering
// each other's readings.
type clusterSnapshot struct {
	up                    bool
	lastError             string
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
	slo                   sloSnapshot
}

// metrics holds one snapshot per watched cluster plus the process-wide alert and
// explanation state, and renders the lot as Prometheus text. All access is
// mutex-guarded; each cluster's worker updates its own snapshot and the HTTP
// handler reads them all.
type metrics struct {
	mu       sync.RWMutex
	names    []string // configured target names, sorted; fixed for the process lifetime
	clusters map[string]*clusterSnapshot
	pending  map[string]bool // clusters yet to finish a first reconcile attempt
	alerts   alert.Stats     // process-wide: one sink
	explain  explainSnapshot // process-wide: one budget
}

// newMetrics pre-creates a snapshot per cluster so kubeagent_cluster_up renders
// 0 for a cluster that has not reported yet, rather than the series being absent
// — an absent series and a down cluster look identical on a dashboard, and they
// are not the same thing.
func newMetrics(names []string) *metrics {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	m := &metrics{
		names:    sorted,
		clusters: make(map[string]*clusterSnapshot, len(sorted)),
		pending:  make(map[string]bool, len(sorted)),
	}
	for _, n := range sorted {
		m.clusters[n] = &clusterSnapshot{findings: map[string]int{}}
		m.pending[n] = true
	}
	return m
}

// snapshot returns the named cluster's snapshot, creating one if the caller
// names a cluster newMetrics did not know about. That cannot happen with a
// validated target list, but a nil map entry would panic under the write lock
// and take the whole daemon down, which is a worse failure than an extra series.
func (m *metrics) snapshot(cluster string) *clusterSnapshot {
	c, ok := m.clusters[cluster]
	if !ok {
		c = &clusterSnapshot{findings: map[string]int{}}
		m.clusters[cluster] = c
		m.names = append(m.names, cluster)
		sort.Strings(m.names)
	}
	return c
}

// update records one reconcile for one cluster. On err != nil only the
// attempt/error counters, the timing and the up flag move; the last good
// snapshot of that cluster's gauges is preserved, and no other cluster is
// touched.
func (m *metrics) update(cluster string, res *scan.Result, dur time.Duration, now time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.snapshot(cluster)
	c.scansTotal++
	c.scanSeconds = dur.Seconds()
	c.lastScanUnix = now.Unix()
	if err != nil {
		c.scanErrors++
		c.up = false
		c.lastError = err.Error()
		return
	}
	c.up = true
	c.lastError = ""
	if res.Health.Verdict == "Healthy" {
		c.healthy = 1
	} else {
		c.healthy = 0
	}
	c.nodesReady = res.Health.NodesReady
	c.nodesTotal = res.Health.NodesTotal
	c.nodesNoReserve = res.NodeReserve.WarnCount
	c.nodesStaleHeartbeat = res.Health.NodesStaleHeartbeat
	c.nodesExpectedAbsent = res.Health.NodesExpectedAbsent
	c.kubeletUnhealthy = len(res.KubeletHealth.Unhealthy)
	c.controlPlaneUnhealthy = 0
	if res.ControlPlane.Status == "unhealthy" {
		c.controlPlaneUnhealthy = 1
	}
	c.dnsServfailRatio = res.DNS.ServfailRatio
	c.pvcsReclaimDelete = res.PVCReclaim.Count
	c.serviceIssues = realServiceIssues(res.ServiceIssues)
	c.ingressIssues = realIngressIssues(res.IngressIssues)
	c.pvcPendingIssues = len(res.PVCIssues)
	c.stuckTerminating = len(res.StuckTerminating)
	c.pdbBlockingIssues = len(res.PDBIssues)
	c.hpaScalingIssues = len(res.HPAIssues)
	c.webhooksFailing = 0
	c.webhookLatencyRisks = 0
	for _, i := range res.WebhookIssues {
		if i.Problem == "high-timeout" {
			c.webhookLatencyRisks++
		} else {
			c.webhooksFailing++
		}
	}
	c.quotaIssues = len(res.QuotaIssues)
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
	c.flagged = flagged
	c.findings = findings
	if len(res.DiskUsage.Nodes) > 0 {
		ratios := make(map[string]float64, len(res.DiskUsage.Nodes))
		for _, n := range res.DiskUsage.Nodes {
			ratios[n.Node] = n.Ratio
		}
		c.nodeFSRatio = ratios
		c.volumesOverDisk = len(res.DiskUsage.Over)
	}
	if res.Certificates != nil {
		c.certsRan = true
		c.certsExpired = len(res.Certificates.Expired)
		c.certsExpiring = len(res.Certificates.Expiring)
	}
}

// markReady records that this cluster has finished its first reconcile attempt.
func (m *metrics) markReady(cluster string) {
	m.mu.Lock()
	delete(m.pending, cluster)
	m.mu.Unlock()
}

// isReady reports whether every configured cluster has finished a first
// reconcile attempt — success or failure. Readiness answers "can this process
// serve?", not "is everything fine": tying it to cluster health would let one
// unreachable remote cluster pull the pod out of its Service endpoints, stopping
// Prometheus from scraping it, and so blind the operator to the clusters that
// are working.
func (m *metrics) isReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.pending) == 0
}

// updateIssues records one cluster's tracker state for rendering. now becomes
// that snapshot's reference time for every age it reports.
func (m *metrics) updateIssues(cluster string, tr *watchstate.Tracker, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot(cluster).issues = issueSnapshot{At: now, Active: tr.Active(), Resolved: tr.RecentlyResolved(), Stats: tr.Stats()}
}

// updateAlerts records the sink's delivery counters for rendering.
func (m *metrics) updateAlerts(s alert.Stats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts = s
}

// updateSLO records one cluster's latest SLO report. Each cluster burns its own
// error budget: an availability SLO computed across clusters would be meaningless.
func (m *metrics) updateSLO(cluster string, enabled bool, target float64, fast, slow slo.Report) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot(cluster).slo = sloSnapshot{Enabled: enabled, Target: target, Fast: fast, Slow: slow}
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

	// A per-cluster family renders HELP and TYPE once, then one sample line per
	// cluster. Prometheus rejects a repeated HELP for the same family, so the
	// header cannot move inside the loop.
	gauge := func(name, help string, get func(*clusterSnapshot) float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, n := range m.names {
			fmt.Fprintf(&b, "%s{cluster=%q} %g\n", name, n, get(m.clusters[n]))
		}
	}
	counter := func(name, help string, get func(*clusterSnapshot) int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		for _, n := range m.names {
			fmt.Fprintf(&b, "%s{cluster=%q} %d\n", name, n, get(m.clusters[n]))
		}
	}
	// A process-wide family carries no cluster label: there is one alert sink
	// and one explanation budget, so attributing their counters to a cluster
	// would be false.
	plainGauge := func(name, help string, v float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
	}
	plainCounter := func(name, help string, v int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	bit := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}

	plainGauge("kubeagent_clusters_total", "Clusters this daemon is configured to watch", float64(len(m.names)))
	gauge("kubeagent_cluster_up", "1 if the last evaluation of this cluster succeeded, else 0", func(c *clusterSnapshot) float64 { return bit(c.up) })
	gauge("kubeagent_cluster_healthy", "1 if the cluster verdict is Healthy, else 0", func(c *clusterSnapshot) float64 { return c.healthy })
	gauge("kubeagent_nodes_ready", "Number of Ready nodes", func(c *clusterSnapshot) float64 { return float64(c.nodesReady) })
	gauge("kubeagent_nodes_total", "Total number of nodes", func(c *clusterSnapshot) float64 { return float64(c.nodesTotal) })
	gauge("kubeagent_nodes_without_reservations", "Nodes whose kubelet reserves no memory (allocatable == capacity)", func(c *clusterSnapshot) float64 { return float64(c.nodesNoReserve) })
	gauge("kubeagent_nodes_stale_heartbeat", "Ready nodes whose kubelet lease is stale (kubelet not heartbeating)", func(c *clusterSnapshot) float64 { return float64(c.nodesStaleHeartbeat) })
	gauge("kubeagent_nodes_expected_absent", "Declared expected nodes that are absent from the cluster", func(c *clusterSnapshot) float64 { return float64(c.nodesExpectedAbsent) })
	gauge("kubeagent_kubelet_unhealthy", "Nodes whose kubelet /healthz reported unhealthy", func(c *clusterSnapshot) float64 { return float64(c.kubeletUnhealthy) })
	gauge("kubeagent_control_plane_unhealthy", "Apiserver /readyz reported the control plane not ready", func(c *clusterSnapshot) float64 { return float64(c.controlPlaneUnhealthy) })
	gauge("kubeagent_dns_servfail_ratio", "CoreDNS SERVFAIL+REFUSED response ratio (0 when healthy or not probed)", func(c *clusterSnapshot) float64 { return c.dnsServfailRatio })
	gauge("kubeagent_pvcs_reclaim_delete", "PVCs whose bound PV has reclaimPolicy Delete", func(c *clusterSnapshot) float64 { return float64(c.pvcsReclaimDelete) })
	gauge("kubeagent_workloads_flagged", "Number of workloads currently flagged", func(c *clusterSnapshot) float64 { return float64(c.flagged) })
	gauge("kubeagent_service_issues", "Number of Service issues", func(c *clusterSnapshot) float64 { return float64(c.serviceIssues) })
	gauge("kubeagent_ingress_route_issues", "Ingress routes whose backend Service is missing, has no ready endpoints, or does not expose the referenced port", func(c *clusterSnapshot) float64 { return float64(c.ingressIssues) })
	gauge("kubeagent_pvc_pending_issues", "PVCs stuck Pending because provisioning or binding failed", func(c *clusterSnapshot) float64 { return float64(c.pvcPendingIssues) })
	gauge("kubeagent_resources_stuck_terminating", "Resources (namespaces, pods, PVCs) wedged in Terminating past the threshold", func(c *clusterSnapshot) float64 { return float64(c.stuckTerminating) })
	gauge("kubeagent_pdb_blocking_issues", "PodDisruptionBudgets that will block a node drain", func(c *clusterSnapshot) float64 { return float64(c.pdbBlockingIssues) })
	gauge("kubeagent_hpa_scaling_issues", "HorizontalPodAutoscalers that cannot scale as intended", func(c *clusterSnapshot) float64 { return float64(c.hpaScalingIssues) })
	gauge("kubeagent_admission_webhooks_failing", "Fail-policy admission webhooks whose backend is missing or has no ready endpoints", func(c *clusterSnapshot) float64 { return float64(c.webhooksFailing) })
	gauge("kubeagent_admission_webhook_latency_risks", "Fail-policy admission webhooks with a high timeoutSeconds (a latency landmine)", func(c *clusterSnapshot) float64 { return float64(c.webhookLatencyRisks) })
	gauge("kubeagent_resourcequota_issues", "ResourceQuota entries at or over the usage threshold", func(c *clusterSnapshot) float64 { return float64(c.quotaIssues) })

	fmt.Fprintf(&b, "# HELP kubeagent_findings Current findings by issue type\n# TYPE kubeagent_findings gauge\n")
	for _, n := range m.names {
		c := m.clusters[n]
		issues := make([]string, 0, len(c.findings))
		for k := range c.findings {
			issues = append(issues, k)
		}
		sort.Strings(issues)
		for _, k := range issues {
			fmt.Fprintf(&b, "kubeagent_findings{cluster=%q,issue=%q} %d\n", n, k, c.findings[k])
		}
	}

	gauge("kubeagent_issues_active", "Issues currently firing, tracked across reconciles", func(c *clusterSnapshot) float64 { return float64(len(c.issues.Active)) })
	gauge("kubeagent_issues_flapping", "Active issues that have crossed the flap threshold", func(c *clusterSnapshot) float64 {
		n := 0
		for _, r := range c.issues.Active {
			if r.Flapping {
				n++
			}
		}
		return float64(n)
	})
	counter("kubeagent_issues_new_total", "Issue firings observed since start", func(c *clusterSnapshot) int64 { return c.issues.Stats.NewTotal })
	counter("kubeagent_issues_resolved_total", "Issue firings that resolved since start", func(c *clusterSnapshot) int64 { return c.issues.Stats.ResolvedTotal })
	counter("kubeagent_issues_flapping_total", "Times an issue crossed the flap threshold since start", func(c *clusterSnapshot) int64 { return c.issues.Stats.FlapTotal })
	counter("kubeagent_issues_dropped_total", "New issues left untracked because the tracker is at capacity", func(c *clusterSnapshot) int64 { return c.issues.Stats.DroppedTotal })
	// Rendered directly rather than through the gauge helper: this metric stays
	// a counter (a running sum of seconds, never decreasing), even though its
	// value is a float rather than an integer count.
	fmt.Fprintf(&b, "# HELP kubeagent_issue_resolution_seconds_sum Seconds issues spent firing before resolving (MTTR numerator)\n# TYPE kubeagent_issue_resolution_seconds_sum counter\n")
	for _, n := range m.names {
		fmt.Fprintf(&b, "kubeagent_issue_resolution_seconds_sum{cluster=%q} %g\n", n, m.clusters[n].issues.Stats.ResolutionSecondsSum)
	}
	counter("kubeagent_issue_resolution_seconds_count", "Issue firings that resolved (MTTR denominator)", func(c *clusterSnapshot) int64 { return c.issues.Stats.ResolutionSecondsCount })

	anyActive := false
	for _, n := range m.names {
		if len(m.clusters[n].issues.Active) > 0 {
			anyActive = true
			break
		}
	}
	if anyActive {
		fmt.Fprintf(&b, "# HELP kubeagent_issue_active 1 while this issue instance is firing\n# TYPE kubeagent_issue_active gauge\n")
		for _, n := range m.names {
			for _, r := range m.clusters[n].issues.Active {
				fmt.Fprintf(&b, "kubeagent_issue_active{%s} 1\n", issueLabels(n, r.Key))
			}
		}
		fmt.Fprintf(&b, "# HELP kubeagent_issue_age_seconds Seconds since this issue instance started firing\n# TYPE kubeagent_issue_age_seconds gauge\n")
		for _, n := range m.names {
			c := m.clusters[n]
			for _, r := range c.issues.Active {
				fmt.Fprintf(&b, "kubeagent_issue_age_seconds{%s} %d\n", issueLabels(n, r.Key), ageSeconds(r.FiringSince, c.issues.At))
			}
		}
	}

	anyFS := false
	for _, n := range m.names {
		if len(m.clusters[n].nodeFSRatio) > 0 {
			anyFS = true
			break
		}
	}
	if anyFS {
		fmt.Fprintf(&b, "# HELP kubeagent_node_fs_usage_ratio Node root filesystem used/capacity (0-1)\n# TYPE kubeagent_node_fs_usage_ratio gauge\n")
		for _, n := range m.names {
			c := m.clusters[n]
			nodes := make([]string, 0, len(c.nodeFSRatio))
			for k := range c.nodeFSRatio {
				nodes = append(nodes, k)
			}
			sort.Strings(nodes)
			for _, node := range nodes {
				fmt.Fprintf(&b, "kubeagent_node_fs_usage_ratio{cluster=%q,node=%q} %g\n", n, node, c.nodeFSRatio[node])
			}
		}
		gauge("kubeagent_volumes_over_disk_threshold", "Node+PVC volumes at or over the disk-usage threshold", func(c *clusterSnapshot) float64 { return float64(c.volumesOverDisk) })
	}

	anyCerts := false
	for _, n := range m.names {
		if m.clusters[n].certsRan {
			anyCerts = true
			break
		}
	}
	if anyCerts {
		certGauge := func(name, help string, get func(*clusterSnapshot) float64) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
			for _, n := range m.names {
				c := m.clusters[n]
				if !c.certsRan {
					continue
				}
				fmt.Fprintf(&b, "%s{cluster=%q} %g\n", name, n, get(c))
			}
		}
		certGauge("kubeagent_certificates_expired", "TLS certificates already expired (opt-in --certs)", func(c *clusterSnapshot) float64 { return float64(c.certsExpired) })
		certGauge("kubeagent_certificates_expiring", "TLS certificates expiring within the warn window (opt-in --certs)", func(c *clusterSnapshot) float64 { return float64(c.certsExpiring) })
	}

	fmt.Fprintf(&b, "# HELP kubeagent_alerts_sent_total Alert notifications delivered since start\n# TYPE kubeagent_alerts_sent_total counter\n")
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "firing", "ok", m.alerts.FiringOK)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "firing", "failed", m.alerts.FiringFailed)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "resolved", "ok", m.alerts.ResolvedOK)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "resolved", "failed", m.alerts.ResolvedFailed)
	fmt.Fprintf(&b, "# HELP kubeagent_alerts_dropped_total Alert notifications dropped without delivery\n# TYPE kubeagent_alerts_dropped_total counter\n")
	fmt.Fprintf(&b, "kubeagent_alerts_dropped_total{reason=%q} %d\n", "queue_full", m.alerts.DroppedQueueFull)
	fmt.Fprintf(&b, "kubeagent_alerts_dropped_total{reason=%q} %d\n", "retries_exhausted", m.alerts.DroppedRetriesExhausted)
	plainGauge("kubeagent_alert_last_success_timestamp_seconds", "Unix time of the last successful alert delivery (0 if none)", float64(m.alerts.LastSuccessUnix))

	anySLO := false
	for _, n := range m.names {
		if m.clusters[n].slo.Enabled {
			anySLO = true
			break
		}
	}
	if anySLO {
		sloGauge := func(name, help string, get func(*clusterSnapshot) float64) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
			for _, n := range m.names {
				c := m.clusters[n]
				if !c.slo.Enabled {
					continue
				}
				fmt.Fprintf(&b, "%s{cluster=%q} %g\n", name, n, get(c))
			}
		}
		sloWindowed := func(name, help string, get func(*clusterSnapshot) (float64, float64)) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
			for _, n := range m.names {
				c := m.clusters[n]
				if !c.slo.Enabled {
					continue
				}
				fast, slow := get(c)
				fmt.Fprintf(&b, "%s{cluster=%q,window=\"fast\"} %g\n", name, n, fast)
				fmt.Fprintf(&b, "%s{cluster=%q,window=\"slow\"} %g\n", name, n, slow)
			}
		}
		sloGauge("kubeagent_slo_target_ratio", "Configured availability SLO as a ratio", func(c *clusterSnapshot) float64 { return c.slo.Target })
		sloWindowed("kubeagent_slo_availability_ratio",
			"Time-weighted fraction of workload-seconds that are not flagged, over the window",
			func(c *clusterSnapshot) (float64, float64) { return c.slo.Fast.Availability, c.slo.Slow.Availability })
		sloWindowed("kubeagent_slo_burn_rate",
			"Error-budget consumption multiple over the window (1 = spending exactly at budget)",
			func(c *clusterSnapshot) (float64, float64) { return c.slo.Fast.BurnRate, c.slo.Slow.BurnRate })
		sloWindowed("kubeagent_slo_window_coverage_ratio",
			"Fraction of the window carrying samples; below 0.6 the burn alert is suppressed",
			func(c *clusterSnapshot) (float64, float64) { return c.slo.Fast.Coverage, c.slo.Slow.Coverage })
		// Clamped at zero: a burn above 1x means the window's budget is already
		// spent, and a negative "remaining" is nonsense on a dashboard.
		sloGauge("kubeagent_slo_error_budget_remaining_ratio",
			"Fraction of the error budget left over the slow window, clamped to [0,1]",
			func(c *clusterSnapshot) float64 {
				remaining := 1 - c.slo.Slow.BurnRate
				if remaining < 0 {
					remaining = 0
				}
				return remaining
			})
	}

	if m.explain.Enabled {
		plainCounter("kubeagent_explain_allowed_total", "Incident explanations the throttle admitted since start", m.explain.Stats.Allowed)
		plainCounter("kubeagent_explain_throttled_total", "Incident explanations refused by the cooldown or the hourly budget", m.explain.Stats.Throttled)
		plainCounter("kubeagent_explain_failed_total", "Incident explanations whose model call errored or returned no text", m.explain.Stats.Failed)
		plainCounter("kubeagent_explain_dropped_total", "Incident explanations admitted but dropped because the worker queue was full", m.explain.Stats.Dropped)
		plainGauge("kubeagent_explain_budget_remaining", "Model calls left in the hourly budget", m.explain.Stats.BudgetRemaining)
	}

	gauge("kubeagent_last_scan_timestamp_seconds", "Unix time of the last evaluation", func(c *clusterSnapshot) float64 { return float64(c.lastScanUnix) })
	gauge("kubeagent_scan_duration_seconds", "Duration of the last evaluation in seconds", func(c *clusterSnapshot) float64 { return c.scanSeconds })
	counter("kubeagent_scans_total", "Total evaluations run", func(c *clusterSnapshot) int64 { return c.scansTotal })
	counter("kubeagent_scan_errors_total", "Total evaluations that errored", func(c *clusterSnapshot) int64 { return c.scanErrors })
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

func issueLabels(cluster string, k watchstate.Key) string {
	return fmt.Sprintf("cluster=%q,kind=%q,namespace=%q,name=%q,issue=%q", cluster, k.Kind, k.Namespace, k.Name, k.Issue)
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
//
// Issue state is now kept per cluster (clusterSnapshot.issues), but this
// endpoint's shape is not: Task 5 is the one that gives /issues a per-record
// "cluster" field and a cluster roster. Until then, merge every configured
// cluster's records and stats into the same flat view the endpoint has always
// served, so today's single-cluster callers see byte-identical output.
func (m *metrics) issuesJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	view := issuesView{Active: []issueView{}, Resolved: []issueView{}}
	for _, n := range m.names {
		c := m.clusters[n]
		view.Active = append(view.Active, issueViews(c.issues.Active, c.issues.At, false)...)
		view.Resolved = append(view.Resolved, issueViews(c.issues.Resolved, c.issues.At, true)...)
		view.Stats.NewTotal += c.issues.Stats.NewTotal
		view.Stats.ResolvedTotal += c.issues.Stats.ResolvedTotal
		view.Stats.FlapTotal += c.issues.Stats.FlapTotal
		view.Stats.DroppedTotal += c.issues.Stats.DroppedTotal
		view.Stats.ResolutionSecondsSum += c.issues.Stats.ResolutionSecondsSum
		view.Stats.ResolutionSecondsCount += c.issues.Stats.ResolutionSecondsCount
	}
	return json.Marshal(view)
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
