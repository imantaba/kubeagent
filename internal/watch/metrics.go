package watch

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imantaba/kubeagent/internal/scan"
)

// metrics holds the latest evaluation snapshot and renders it as Prometheus text.
// All access is mutex-guarded; the daemon updates it from the reconcile loop and
// the HTTP handler reads it.
type metrics struct {
	mu                  sync.RWMutex
	ready               bool
	healthy             float64
	nodesReady          int
	nodesTotal          int
	nodesNoReserve      int
	nodesStaleHeartbeat int
	nodesExpectedAbsent int
	kubeletUnhealthy    int
	pvcsReclaimDelete   int
	flagged             int
	serviceIssues       int
	ingressIssues       int
	findings            map[string]int
	lastScanUnix        int64
	scanSeconds         float64
	scansTotal          int64
	scanErrors          int64
	nodeFSRatio         map[string]float64
	volumesOverDisk     int
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
	m.pvcsReclaimDelete = res.PVCReclaim.Count
	m.serviceIssues = len(res.ServiceIssues)
	m.ingressIssues = len(res.IngressIssues)
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
}

func (m *metrics) markReady() { m.mu.Lock(); m.ready = true; m.mu.Unlock() }
func (m *metrics) isReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ready
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
	gauge("kubeagent_cluster_healthy", "1 if the cluster verdict is Healthy, else 0", m.healthy)
	gauge("kubeagent_nodes_ready", "Number of Ready nodes", float64(m.nodesReady))
	gauge("kubeagent_nodes_total", "Total number of nodes", float64(m.nodesTotal))
	gauge("kubeagent_nodes_without_reservations", "Nodes whose kubelet reserves no memory (allocatable == capacity)", float64(m.nodesNoReserve))
	gauge("kubeagent_nodes_stale_heartbeat", "Ready nodes whose kubelet lease is stale (kubelet not heartbeating)", float64(m.nodesStaleHeartbeat))
	gauge("kubeagent_nodes_expected_absent", "Declared expected nodes that are absent from the cluster", float64(m.nodesExpectedAbsent))
	gauge("kubeagent_kubelet_unhealthy", "Nodes whose kubelet /healthz reported unhealthy", float64(m.kubeletUnhealthy))
	gauge("kubeagent_pvcs_reclaim_delete", "PVCs whose bound PV has reclaimPolicy Delete", float64(m.pvcsReclaimDelete))
	gauge("kubeagent_workloads_flagged", "Number of workloads currently flagged", float64(m.flagged))
	gauge("kubeagent_service_issues", "Number of Service issues", float64(m.serviceIssues))
	gauge("kubeagent_ingress_route_issues", "Ingress routes whose backend Service is missing, has no ready endpoints, or does not expose the referenced port", float64(m.ingressIssues))
	fmt.Fprintf(&b, "# HELP kubeagent_findings Current findings by issue type\n# TYPE kubeagent_findings gauge\n")
	issues := make([]string, 0, len(m.findings))
	for k := range m.findings {
		issues = append(issues, k)
	}
	sort.Strings(issues)
	for _, k := range issues {
		fmt.Fprintf(&b, "kubeagent_findings{issue=%q} %d\n", k, m.findings[k])
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
	gauge("kubeagent_last_scan_timestamp_seconds", "Unix time of the last evaluation", float64(m.lastScanUnix))
	gauge("kubeagent_scan_duration_seconds", "Duration of the last evaluation in seconds", m.scanSeconds)
	counter("kubeagent_scans_total", "Total evaluations run", m.scansTotal)
	counter("kubeagent_scan_errors_total", "Total evaluations that errored", m.scanErrors)
	return b.String()
}

// handler serves /metrics, /healthz, and /readyz.
func (m *metrics) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		io.WriteString(w, m.render())
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
