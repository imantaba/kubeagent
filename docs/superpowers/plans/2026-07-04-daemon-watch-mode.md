# Daemon watch mode (Phase 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run kubeagent in-cluster as a read-only daemon (`kubeagent watch`) that watches a single cluster via informers, runs the existing deterministic diagnosis on change (debounced) plus a heartbeat, and exposes structured logs + hand-rolled Prometheus `/metrics`.

**Architecture:** Extract the scan orchestration into a reusable `internal/scan.Evaluate`; add a new `internal/watch` package (metrics + control loop); add an in-cluster client constructor; wire a `watch` subcommand; ship read-only `deploy/` manifests.

**Tech Stack:** Go 1.26; informers/listers from the existing `k8s.io/client-go`; hand-rolled Prometheus text on stdlib `net/http`. **No new module dependency.**

## Global Constraints

- **READ-ONLY, absolute.** Daemon makes only get/list/watch calls; no writes (no Events, no `--fix`), no LLM/Anthropic calls anywhere in the daemon.
- **No new Go module dependency.** Informers come from client-go; metrics/HTTP are hand-rolled/stdlib.
- The `internal/scan.Evaluate` extraction must be **behavior-preserving** for the CLI `scan` (existing `main`/report tests stay green).
- Concurrency (informers, ticker, HTTP server) is expected here — this retires the "v1 sequential" rule (documented in Task 6).
- Go at `/usr/local/go/bin` (`export PATH=$PATH:/usr/local/go/bin`).

---

### Task 1: extract `internal/scan.Evaluate` (shared pipeline)

**Files:**
- Create: `internal/scan/scan.go`, `internal/scan/scan_test.go`
- Modify: `main.go`

**Interfaces:**
- Produces: `scan.Options`, `scan.Result`, `scan.Evaluate(ctx, client, opts) (Result, error)` — used by both the CLI and the daemon.

- [ ] **Step 1: Write the failing test — create `internal/scan/scan_test.go`**

```go
package scan

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEvaluate_HealthyClusterNoFlags(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	cli := fake.NewSimpleClientset(node)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Health.Verdict != "Healthy" {
		t.Errorf("want Healthy, got %q", res.Health.Verdict)
	}
	if got := len(res.Inventory.Workloads); got != 0 {
		t.Errorf("want no workloads, got %d", got)
	}
}

func TestEvaluate_FlagsCrashLoopingWorkload(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1",
		Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "web", Ready: false, RestartCount: 8,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}}
	cli := fake.NewSimpleClientset(node, dep, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "CrashLoopBackOff" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a CrashLoopBackOff finding, got %+v", res.Inventory.Workloads)
	}
}

func p32(i int32) *int32 { return &i }
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/`
Expected: build failure — package `scan` / `Evaluate` does not exist yet.

- [ ] **Step 3: Implement — create `internal/scan/scan.go`**

This lifts the collection+diagnosis+assembly currently inline in `main.go run()` (the block from `collect.CollectInventory` through `rollout.Annotate`, plus service health). It does NOT include the CLI-only extras (resource summary, platform facts, credential lint, `--explain`, `--fix`, printing) — those stay in `main.go` and compose around the returned `Result`.

```go
// Package scan runs kubeagent's deterministic evaluation of a cluster — collect,
// diagnose, assemble/prioritize, annotate, assess health and service health —
// and returns the structured result. It is shared by the CLI `scan` command and
// the `watch` daemon. Read-only: only List/Get calls, no writes, no LLM.
package scan

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/netpolicy"
	"github.com/imantaba/kubeagent/internal/rollout"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

// Options controls the evaluation scope.
type Options struct {
	Namespace       string
	IncludeCron     bool
	IncludeRestarts bool
}

// Result is the structured health picture. Inputs and Nodes are exposed so the
// CLI can compose its extra views (resource summary, platform facts, credential
// lint, --fix) without re-collecting.
type Result struct {
	Inputs        collect.Inventory
	Nodes         []corev1.Node
	Health        clusterhealth.ClusterHealth
	Inventory     inventory.Result
	ServiceIssues []svchealth.Issue
}

// Evaluate performs the read-only evaluation. The returned error is the raw
// collection error (callers may wrap it with connectivity.Diagnose).
func Evaluate(ctx context.Context, client kubernetes.Interface, opts Options) (Result, error) {
	inputs, err := collect.CollectInventory(ctx, client, opts.Namespace)
	if err != nil {
		return Result{}, err
	}

	detectors := []diagnose.Detector{
		diagnose.CrashLoopDetector{},
		diagnose.ImagePullDetector{},
		diagnose.OOMKilledDetector{},
		diagnose.PendingDetector{},
	}
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods))
	workloads := inventory.Assemble(inputs, findings)

	nodes, err := collect.Nodes(ctx, client)
	if err != nil {
		return Result{}, err
	}
	health := clusterhealth.Assess(nodes, workloads)
	health.ScopeNote = clusterhealth.NamespaceScopeNote(opts.Namespace)

	svcs, _ := collect.Services(ctx, client, opts.Namespace)
	slices, _ := collect.EndpointSlices(ctx, client, opts.Namespace)
	backends := svchealth.BackendsFrom(inputs.Deployments, inputs.StatefulSets, inputs.DaemonSets, inputs.Jobs, inputs.CronJobs)
	serviceIssues := svchealth.Assess(svcs, slices, backends)

	result := inventory.Prioritize(workloads, inventory.Opts{
		IncludeRestarts: opts.IncludeRestarts,
		IncludeCron:     opts.IncludeCron,
	})

	nps, _ := collect.NetworkPolicies(ctx, client, opts.Namespace)
	podLabels := make(map[string]map[string]string, len(inputs.Pods))
	for _, p := range inputs.Pods {
		podLabels[p.Namespace+"/"+p.Name] = p.Labels
	}
	netpolicy.Annotate(result.Workloads, podLabels, nps)
	rollout.Annotate(result.Workloads, inputs.ReplicaSets, time.Now())

	return Result{Inputs: inputs, Nodes: nodes, Health: health, Inventory: result, ServiceIssues: serviceIssues}, nil
}
```

- [ ] **Step 4: Refactor `main.go run()` to call `scan.Evaluate`**

Replace the inline block (from `inputs, err := collect.CollectInventory(...)` through `rollout.Annotate(result.Workloads, ...)`, i.e. the current lines 92–149) with a call to `scan.Evaluate`, then rebuild the CLI extras from the returned `Result`. The exact replacement:

```go
	res, err := scan.Evaluate(context.Background(), client, scan.Options{
		Namespace:       namespace,
		IncludeCron:     *includeCron,
		IncludeRestarts: *includeRestarts,
	})
	if err != nil {
		if diag, ok := connectivity.Diagnose(err); ok {
			return fmt.Errorf("%s\ndetails: %w", diag, err)
		}
		return err
	}
	health := res.Health
	result := res.Inventory
	serviceIssues := res.ServiceIssues
	nodes := res.Nodes

	usage, _, metricsErr := collect.NodeMetrics(context.Background(), client)
	if metricsErr != nil {
		fmt.Fprintf(os.Stderr, "kubeagent: warning: metrics unavailable: %v\n", metricsErr)
	}
	resourcePods := res.Inputs.Pods
	if namespace != "" {
		if all, perr := collect.AllPods(context.Background(), client); perr == nil {
			resourcePods = all
		}
	}
	summary := resources.Summarize(nodes, resourcePods, usage)

	scs, _ := collect.StorageClasses(context.Background(), client)
	ics, _ := collect.IngressClasses(context.Background(), client)
	sysDS, _ := collect.SystemDaemonSets(context.Background(), client)
	facts := platform.Detect(nodes, sysDS, scs, ics)
```

The subsequent `--explain`, credential-lint, `report.PrintInventory`, and `--fix` blocks are unchanged EXCEPT that credential lint now reads `res.Inputs.Pods` and `--fix` reads `res.Inputs.ReplicaSets`:
- In the credlint block: `credWarnings = credlint.Scan(cms, res.Inputs.Pods)`.
- In the fix block: `runFixes(context.Background(), client, result.Workloads, res.Inputs.ReplicaSets, nodes, *dryRun, *assumeYes, os.Stdout, os.Stdin)`.

Add `"github.com/imantaba/kubeagent/internal/scan"` to the imports (gofmt-alphabetical position: after `resources`? no — `scan` sorts after `rollout`/`report`/`resources` and before `svchealth`; place it there). Remove now-unused imports only if they become unused (they should NOT — `collect`, `diagnose`, `inventory`, `netpolicy`, `rollout`, `clusterhealth`, `svchealth` are still referenced by remaining code or by `scan`; verify with goimports/build).

- [ ] **Step 5: Verify build + tests (behavior-preserving)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./... && go vet ./... && gofmt -l internal/scan/ main.go`
Expected: all packages pass (the existing `main`/report tests confirm `scan` output is unchanged), new `scan` tests pass, vet clean, gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/scan/ main.go
git commit -m "refactor(scan): extract shared Evaluate pipeline (used by CLI and daemon)"
```

---

### Task 2: metrics + health endpoints (`internal/watch/metrics.go`)

**Files:**
- Create: `internal/watch/metrics.go`, `internal/watch/metrics_test.go`

**Interfaces:**
- Produces: `newMetrics()`, `(*metrics).update(res *scan.Result, dur, now, err)`, `(*metrics).markReady()`, `(*metrics).handler() http.Handler`, `(*metrics).render() string`.

- [ ] **Step 1: Write the failing test — create `internal/watch/metrics_test.go`**

```go
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
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/scan"
)

func sampleResult() *scan.Result {
	return &scan.Result{
		Health: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		Inventory: inventory.Result{Workloads: []inventory.Workload{
			{Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 1,
				Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff"}}},
		}},
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
		"kubeagent_scans_total 1",
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/`
Expected: build failure — package `watch` / `newMetrics` does not exist yet.

- [ ] **Step 3: Implement — create `internal/watch/metrics.go`**

```go
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
	mu            sync.RWMutex
	ready         bool
	healthy       float64
	nodesReady    int
	nodesTotal    int
	flagged       int
	serviceIssues int
	findings      map[string]int
	lastScanUnix  int64
	scanSeconds   float64
	scansTotal    int64
	scanErrors    int64
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
	m.serviceIssues = len(res.ServiceIssues)
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
	gauge("kubeagent_workloads_flagged", "Number of workloads currently flagged", float64(m.flagged))
	gauge("kubeagent_service_issues", "Number of Service issues", float64(m.serviceIssues))
	fmt.Fprintf(&b, "# HELP kubeagent_findings Current findings by issue type\n# TYPE kubeagent_findings gauge\n")
	issues := make([]string, 0, len(m.findings))
	for k := range m.findings {
		issues = append(issues, k)
	}
	sort.Strings(issues)
	for _, k := range issues {
		fmt.Fprintf(&b, "kubeagent_findings{issue=%q} %d\n", k, m.findings[k])
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
```

- [ ] **Step 4: Run tests + build**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -v && go build ./... && go vet ./internal/watch/ && gofmt -l internal/watch/`
Expected: all metrics tests PASS, build ok, vet clean, gofmt clean.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): hand-rolled Prometheus metrics + health/readiness endpoints"
```

---

### Task 3: the daemon control loop (`internal/watch/watch.go`)

**Files:**
- Create: `internal/watch/watch.go`, `internal/watch/watch_test.go`

**Interfaces:**
- Consumes: `scan.Evaluate` (Task 1), `metrics` (Task 2).
- Produces: `watch.Config`, `watch.Run(ctx, client, cfg) error`; internal `debouncer` and `changeLogger` (unit-tested).

- [ ] **Step 1: Write the failing tests — create `internal/watch/watch_test.go`**

```go
package watch

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/scan"
)

func TestChangeLogger_OnlyLogsOnChange(t *testing.T) {
	healthy := &scan.Result{Health: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 3, NodesTotal: 3}}
	degraded := &scan.Result{Health: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		Inventory: inventory.Result{Workloads: []inventory.Workload{{Namespace: "s", Name: "w", Ready: 0, Desired: 1}}}}

	var cl changeLogger
	if !cl.changed(healthy, nil) {
		t.Error("first observation should count as a change")
	}
	if cl.changed(healthy, nil) {
		t.Error("identical observation should NOT count as a change")
	}
	if !cl.changed(degraded, nil) {
		t.Error("verdict flip should count as a change")
	}
}

func TestSignature_DistinguishesFindingsAndErrors(t *testing.T) {
	a := &scan.Result{Health: clusterhealth.ClusterHealth{Verdict: "Healthy"}}
	if signature(a, nil) == signature(a, errDummy) {
		t.Error("error vs no-error must produce different signatures")
	}
}
```

Add at the bottom of the test file:

```go
var errDummy = errStr("x")

type errStr string

func (e errStr) Error() string { return string(e) }
```

- [ ] **Step 2: Run to verify failure**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run 'TestChangeLogger|TestSignature'`
Expected: FAIL — `changeLogger` / `signature` undefined.

- [ ] **Step 3: Implement — create `internal/watch/watch.go`**

```go
// Package watch runs kubeagent as an in-cluster, read-only daemon: it watches the
// cluster via informers, re-runs the deterministic evaluation on change (debounced)
// and on a heartbeat, and surfaces the result as structured logs and Prometheus
// metrics. No writes, no LLM.
package watch

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/imantaba/kubeagent/internal/scan"
)

// Config configures the daemon.
type Config struct {
	Namespace       string
	MetricsAddr     string
	Heartbeat       time.Duration
	Debounce        time.Duration
	IncludeCron     bool
	IncludeRestarts bool
}

// Run starts the metrics server and the informer-driven control loop, blocking
// until ctx is cancelled.
func Run(ctx context.Context, client kubernetes.Interface, cfg Config) error {
	m := newMetrics()

	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: m.handler()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("kubeagent: metrics server error: %v", err)
		}
	}()

	var factory informers.SharedInformerFactory
	if cfg.Namespace != "" {
		factory = informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(cfg.Namespace))
	} else {
		factory = informers.NewSharedInformerFactory(client, 0)
	}
	trigger := make(chan struct{}, 1)
	enqueue := func() {
		select {
		case trigger <- struct{}{}:
		default: // already pending
		}
	}
	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { enqueue() },
		UpdateFunc: func(interface{}, interface{}) { enqueue() },
		DeleteFunc: func(interface{}) { enqueue() },
	}
	for _, inf := range []cache.SharedIndexInformer{
		factory.Core().V1().Pods().Informer(),
		factory.Apps().V1().Deployments().Informer(),
		factory.Apps().V1().ReplicaSets().Informer(),
		factory.Core().V1().Nodes().Informer(),
		factory.Core().V1().Services().Informer(),
		factory.Discovery().V1().EndpointSlices().Informer(),
	} {
		if _, err := inf.AddEventHandler(h); err != nil {
			return fmt.Errorf("adding informer handler: %w", err)
		}
	}
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	log.Printf("kubeagent: watching cluster (namespace=%q, heartbeat=%s); metrics on %s", scopeLabel(cfg.Namespace), cfg.Heartbeat, cfg.MetricsAddr)

	opts := scan.Options{Namespace: cfg.Namespace, IncludeCron: cfg.IncludeCron, IncludeRestarts: cfg.IncludeRestarts}
	var cl changeLogger
	reconcile := func() {
		start := time.Now()
		res, err := scan.Evaluate(ctx, client, opts)
		m.update(&res, time.Since(start), time.Now(), err)
		if cl.changed(&res, err) {
			log.Printf("kubeagent: %s", describe(&res, err))
		}
	}
	reconcile() // initial snapshot
	m.markReady()

	heartbeat := time.NewTicker(cfg.Heartbeat)
	defer heartbeat.Stop()
	debounce := time.NewTimer(cfg.Debounce)
	debounce.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = srv.Shutdown(shutCtx)
			cancel()
			log.Printf("kubeagent: shutting down")
			return nil
		case <-trigger:
			if !pending {
				pending = true
				debounce.Reset(cfg.Debounce)
			}
		case <-debounce.C:
			pending = false
			reconcile()
		case <-heartbeat.C:
			reconcile()
		}
	}
}

func scopeLabel(ns string) string {
	if ns == "" {
		return "all"
	}
	return ns
}

// changeLogger suppresses steady-state log spam: it logs only when the health
// picture changes vs the previous reconcile.
type changeLogger struct {
	prev   string
	inited bool
}

func (c *changeLogger) changed(res *scan.Result, err error) bool {
	sig := signature(res, err)
	if c.inited && sig == c.prev {
		return false
	}
	c.inited = true
	c.prev = sig
	return true
}

// signature is a compact, stable fingerprint of the evaluation outcome.
func signature(res *scan.Result, err error) string {
	if err != nil {
		return "err:" + err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%d/%d|", res.Health.Verdict, res.Health.NodesReady, res.Health.NodesTotal)
	var flagged []string
	for _, w := range res.Inventory.Workloads {
		if w.Flagged() {
			issues := make([]string, 0, len(w.Findings))
			for _, f := range w.Findings {
				issues = append(issues, f.Issue)
			}
			sort.Strings(issues)
			flagged = append(flagged, w.Namespace+"/"+w.Name+":"+strings.Join(issues, ","))
		}
	}
	sort.Strings(flagged)
	b.WriteString(strings.Join(flagged, ";"))
	return b.String()
}

// describe is a one-line human summary for the log.
func describe(res *scan.Result, err error) string {
	if err != nil {
		return "evaluation error: " + err.Error()
	}
	n := 0
	for _, w := range res.Inventory.Workloads {
		if w.Flagged() {
			n++
		}
	}
	return fmt.Sprintf("cluster %s (%d/%d nodes ready) — %d workload(s) flagged, %d service issue(s)",
		res.Health.Verdict, res.Health.NodesReady, res.Health.NodesTotal, n, len(res.ServiceIssues))
}
```

- [ ] **Step 4: Run tests + build**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -v && go build ./... && go vet ./internal/watch/ && gofmt -l internal/watch/`
Expected: all watch tests PASS (metrics + changeLogger/signature), build ok, vet clean, gofmt clean.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/watch.go internal/watch/watch_test.go
git commit -m "feat(watch): informer-driven, debounced read-only control loop"
```

---

### Task 4: `watch` subcommand + in-cluster client

**Files:**
- Modify: `main.go`, `internal/cluster/client.go`
- Test: `internal/cluster/client_test.go`

- [ ] **Step 1: Add the in-cluster client constructor to `internal/cluster/client.go`**

Add (and add `"k8s.io/client-go/rest"` to the imports):

```go
// NewInClusterOrKubeconfig builds a clientset from the in-cluster service-account
// when running inside a pod; otherwise it falls back to NewClient(kubeconfig,
// context) for local development.
func NewInClusterOrKubeconfig(kubeconfigPath, contextName string) (*kubernetes.Clientset, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(cfg)
	} else if err != rest.ErrNotInCluster {
		return nil, fmt.Errorf("loading in-cluster config: %w", err)
	}
	return NewClient(kubeconfigPath, contextName)
}
```

- [ ] **Step 2: Test the fallback — add `internal/cluster/client_test.go`**

```go
package cluster

import (
	"os"
	"testing"
)

// Outside a pod (no service-account env), NewInClusterOrKubeconfig must fall back
// to the kubeconfig path — here, a bogus path yields a load error, proving it took
// the kubeconfig branch rather than the in-cluster one.
func TestNewInClusterOrKubeconfig_FallsBackToKubeconfig(t *testing.T) {
	os.Unsetenv("KUBERNETES_SERVICE_HOST")
	if _, err := NewInClusterOrKubeconfig("/no/such/kubeconfig", ""); err == nil {
		t.Fatal("expected a kubeconfig load error in the fallback path, got nil")
	}
}
```

- [ ] **Step 3: Wire the `watch` subcommand in `main.go`**

In `run()`, before the `scan` handling, add a `watch` branch. After the `version` check:

```go
	if len(args) > 0 && args[0] == "watch" {
		return runWatch(args[1:])
	}
```

Update the usage string to include `watch`. Then add the `runWatch` function (and imports `"github.com/imantaba/kubeagent/internal/watch"`, `"os/signal"`, `"syscall"`, `"time"` — `time` already imported):

```go
func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig for local dev (ignored in-cluster)")
	contextName := fs.String("context", "", "kubeconfig context for local dev")
	metricsAddr := fs.String("metrics-addr", envOr("KUBEAGENT_METRICS_ADDR", ":8080"), "address for /metrics, /healthz, /readyz")
	heartbeat := fs.Duration("heartbeat", envDur("KUBEAGENT_HEARTBEAT", 60*time.Second), "safety-net full re-evaluation interval")
	debounce := fs.Duration("debounce", envDur("KUBEAGENT_DEBOUNCE", 2*time.Second), "coalescing window for change events")
	includeCron := fs.Bool("include-cron", false, "include CronJobs in the evaluation")
	includeRestarts := fs.Bool("include-restarts", false, "include workloads that are healthy now but have restarted")
	var namespace string
	fs.StringVar(&namespace, "namespace", envOr("KUBEAGENT_NAMESPACE", ""), "namespace to watch (default: all)")
	fs.StringVar(&namespace, "n", envOr("KUBEAGENT_NAMESPACE", ""), "namespace to watch (shorthand)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := cluster.NewInClusterOrKubeconfig(*kubeconfig, *contextName)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return watch.Run(ctx, client, watch.Config{
		Namespace:       namespace,
		MetricsAddr:     *metricsAddr,
		Heartbeat:       *heartbeat,
		Debounce:        *debounce,
		IncludeCron:     *includeCron,
		IncludeRestarts: *includeRestarts,
	})
}

// envOr returns the env var value if set, else def.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDur parses a duration env var, falling back to def on empty/invalid.
func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
```

- [ ] **Step 4: Verify build + tests + a smoke run**

Run: `export PATH=$PATH:/usr/local/go/bin && go build -o kubeagent . && go test ./... && go vet ./... && gofmt -l main.go internal/cluster/`
Expected: build ok, all tests pass, vet clean, gofmt clean.

Then a no-cluster smoke check (must fail cleanly, not panic):
Run: `KUBECONFIG=/dev/null ./kubeagent watch --kubeconfig /no/such/file 2>&1 | head -2`
Expected: a clean error about loading the kubeconfig (proves flag parsing + client construction path), not a panic.

- [ ] **Step 5: Commit**

```bash
git add main.go internal/cluster/
git commit -m "feat(watch): kubeagent watch subcommand + in-cluster client"
```

---

### Task 5: deploy manifests (`deploy/`)

**Files:**
- Create: `deploy/rbac.yaml`, `deploy/deployment.yaml`, `deploy/service.yaml`, `deploy/README.md`

- [ ] **Step 1: `deploy/rbac.yaml` — read-only RBAC**

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kubeagent
  namespace: kubeagent
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeagent-readonly
rules:
  - apiGroups: [""]
    resources: [pods, nodes, services, configmaps]
    verbs: [get, list, watch]
  - apiGroups: ["apps"]
    resources: [deployments, replicasets, statefulsets, daemonsets]
    verbs: [get, list, watch]
  - apiGroups: ["batch"]
    resources: [jobs, cronjobs]
    verbs: [get, list, watch]
  - apiGroups: ["discovery.k8s.io"]
    resources: [endpointslices]
    verbs: [get, list, watch]
  - apiGroups: ["networking.k8s.io"]
    resources: [networkpolicies, ingressclasses]
    verbs: [get, list, watch]
  - apiGroups: ["storage.k8s.io"]
    resources: [storageclasses]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeagent-readonly
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeagent-readonly
subjects:
  - kind: ServiceAccount
    name: kubeagent
    namespace: kubeagent
```

(The verbs are get/list/watch ONLY — no create/update/patch/delete anywhere.)

- [ ] **Step 2: `deploy/deployment.yaml`**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kubeagent
  namespace: kubeagent
  labels: { app: kubeagent }
spec:
  replicas: 1
  selector:
    matchLabels: { app: kubeagent }
  template:
    metadata:
      labels: { app: kubeagent }
    spec:
      serviceAccountName: kubeagent
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
      containers:
        - name: kubeagent
          image: ghcr.io/imantaba/kubeagent:latest # replace with your published image
          args: ["watch", "--metrics-addr=:8080"]
          ports:
            - name: metrics
              containerPort: 8080
          readinessProbe:
            httpGet: { path: /readyz, port: metrics }
            initialDelaySeconds: 5
          livenessProbe:
            httpGet: { path: /healthz, port: metrics }
            initialDelaySeconds: 10
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          resources:
            requests: { cpu: 50m, memory: 64Mi }
            limits: { cpu: 250m, memory: 256Mi }
```

- [ ] **Step 3: `deploy/service.yaml`**

```yaml
apiVersion: v1
kind: Service
metadata:
  name: kubeagent-metrics
  namespace: kubeagent
  labels: { app: kubeagent }
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "8080"
    prometheus.io/path: "/metrics"
spec:
  selector: { app: kubeagent }
  ports:
    - name: metrics
      port: 8080
      targetPort: metrics
```

- [ ] **Step 4: `deploy/README.md`**

Write a short guide: create the `kubeagent` namespace, `kubectl apply -f deploy/`, build/push your own image (the release tarball is a binary; a container image build is the operator's step for now), then `kubectl -n kubeagent port-forward svc/kubeagent-metrics 8080:8080` and `curl localhost:8080/metrics`. Note the daemon is strictly read-only (RBAC has only get/list/watch) and makes no LLM calls.

- [ ] **Step 5: Commit**

```bash
git add deploy/
git commit -m "feat(watch): read-only deploy manifests (RBAC, Deployment, metrics Service)"
```

---

### Task 6: docs + invariant update

**Files:**
- Modify: `README.md`, `CHANGELOG.md`, `CLAUDE.md`, `website/docs/roadmap.md`

- [ ] **Step 1: `CLAUDE.md` — retire the sequential invariant for the daemon**

Under "Invariants (do not break)", change the v1-sequential bullet to note the daemon exception:

```markdown
- v1 CLI (`scan`) is **sequential** — no goroutines. The `watch` daemon
  (`internal/watch`) is the documented exception: it runs informers, a heartbeat
  ticker, and an HTTP server concurrently. It remains **strictly read-only**
  (get/list/watch only; no writes, no LLM).
```

- [ ] **Step 2: `CHANGELOG.md` — `[Unreleased] → Added`**

Add a new `## [Unreleased]` section (above `## [0.8.0]`) with:

```markdown
## [Unreleased]

### Added

- **Daemon watch mode (`kubeagent watch`).** Run kubeagent in-cluster as a
  strictly read-only daemon: it watches the cluster via informers, re-runs the
  deterministic diagnosis on change (debounced) plus a heartbeat, and exposes the
  result as structured logs and hand-rolled Prometheus `/metrics` (with `/healthz`
  and `/readyz`). No cluster writes, no LLM calls, no new dependency. Read-only
  RBAC and Deployment manifests are in `deploy/`. (Multi-cluster, Kubernetes
  Events, `--explain`, and autonomous remediation are planned for later phases.)
```

- [ ] **Step 3: `README.md` — a Watch mode section**

Add a short section (after the Remediation section) describing `kubeagent watch`: what it does, that it's read-only, the flags (`--metrics-addr`, `--heartbeat`, `--debounce`, `--namespace`), the metrics exposed, and a pointer to `deploy/`.

- [ ] **Step 4: `website/docs/roadmap.md` — note it under Shipped**

Add a bullet:

```markdown
- **Daemon watch mode** — `kubeagent watch` runs in-cluster (read-only) and
  exposes continuous cluster-health diagnosis as Prometheus metrics + structured
  logs; see `deploy/`. First phase of a daemon roadmap (multi-cluster, on-incident
  `--explain`, and guarded autonomous remediation to follow).
```

- [ ] **Step 5: Build + commit**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: build succeeds, all packages `ok` (no code changed in this task).

```bash
git add README.md CHANGELOG.md CLAUDE.md website/docs/roadmap.md
git commit -m "docs: document kubeagent watch daemon mode; note the daemon concurrency exception"
```

---

## Self-Review

**Spec coverage:**
- `internal/scan.Evaluate` extraction (shared by CLI + daemon), behavior-preserving → Task 1. ✓
- Hand-rolled metrics + `/metrics`/`/healthz`/`/readyz` → Task 2. ✓
- Informer-driven, debounced, heartbeat control loop + change-gated logging + graceful shutdown → Task 3. ✓
- `watch` subcommand + in-cluster client (kubeconfig fallback) → Task 4. ✓
- Read-only `deploy/` manifests → Task 5. ✓
- Docs + CLAUDE.md invariant update → Task 6. ✓
- READ-ONLY / no-LLM / no-new-dep / concurrency-documented (Global Constraints) → RBAC is get/list/watch only; no writes or Anthropic calls in `internal/watch`/`internal/scan`; informers/HTTP from stdlib+client-go; CLAUDE.md updated. ✓

**Placeholder scan:** none — complete code/YAML in every step. (`deploy/deployment.yaml` image is a documented operator-replaceable placeholder, not a plan gap.)

**Type/name consistency:** `scan.Options{Namespace,IncludeCron,IncludeRestarts}`, `scan.Result{Inputs,Nodes,Health,Inventory,ServiceIssues}`, `scan.Evaluate`, `watch.Config`, `watch.Run`, `newMetrics/update/markReady/handler/render`, `changeLogger.changed`, `signature`, `describe`, and `cluster.NewInClusterOrKubeconfig` are used identically across tasks and tests. Metrics names in the Task-2 test match those in `render()`. The `main.go` refactor references `res.Inputs.Pods`/`res.Inputs.ReplicaSets`/`res.Nodes`, matching the `scan.Result` fields.
