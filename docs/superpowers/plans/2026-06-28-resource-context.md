# Resource Context Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enrich kubeagent's report and `--explain` with resource context — per-container requests/limits on OOMKilled findings, plus a cluster-wide CPU/memory summary (allocatable / reserved / limits / live usage).

**Architecture:** A new pure package `internal/resources` aggregates node allocatable, pod requests/limits, and optional metrics-server usage into a `Summary`. `internal/collect` gains a best-effort metrics fetch (raw REST to `metrics.k8s.io`) and an all-namespaces pod list. The OOMKilled detector attaches the killed container's resources to its `Finding`. `report` and `explain` render the summary and per-finding resources. Everything is threaded as a `*resources.Summary` (nil when unavailable).

**Tech Stack:** Go 1.26, client-go, `k8s.io/apimachinery/pkg/api/resource` (already vendored), stdlib `flag`, `encoding/json`.

## Global Constraints

- **READ-ONLY:** only List/Get plus one raw `GET` on `metrics.k8s.io`. Never create/update/patch/delete.
- **No new Go module dependency.** Use the existing client's raw REST and `apimachinery`'s `resource.Quantity`.
- **Sequential** — no goroutines. Stdlib `flag` only. Exit codes `0`/`1` unchanged.
- **`--explain` egress:** send only cluster resource **totals** and the OOM container's resource **quantities**. Never pod IPs, per-node names, raw specs, env, or secrets.
- **Offline/optional metrics:** a missing or forbidden metrics-server is non-fatal; usage shows as unavailable.
- **TDD:** failing test first, watch it fail, implement, watch it pass, commit. `export PATH=$PATH:/usr/local/go/bin` before any `go` command.
- **Scope (YAGNI):** cluster totals only (no per-node), OOMKilled-only resource attachment, CPU + memory only.

---

### Task 1: `internal/resources` — cluster resource aggregation (pure)

**Files:**
- Create: `internal/resources/resources.go`
- Test: `internal/resources/resources_test.go`

**Interfaces:**
- Produces:
  - `type Line struct { Allocatable, Requests, Limits, Usage string; RequestsPct, LimitsPct, UsagePct int }`
  - `type Summary struct { CPU, Memory Line; MetricsAvailable bool }`
  - `func Summarize(nodes []corev1.Node, pods []corev1.Pod, usage map[string]corev1.ResourceList) Summary`

- [ ] **Step 1: Write the failing test**

Create `internal/resources/resources_test.go`:

```go
package resources

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func node(name, cpu, mem string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		}},
	}
}

func podWith(phase corev1.PodPhase, cpuReq, memReq, cpuLim, memLim string) corev1.Pod {
	return corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpuReq),
					corev1.ResourceMemory: resource.MustParse(memReq),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpuLim),
					corev1.ResourceMemory: resource.MustParse(memLim),
				},
			},
		}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestSummarize_AggregatesAndComputesPercents(t *testing.T) {
	nodes := []corev1.Node{node("n1", "4", "8Gi"), node("n2", "4", "8Gi")} // 8 cores, 16Gi
	pods := []corev1.Pod{
		podWith(corev1.PodRunning, "1", "2Gi", "2", "4Gi"),
		podWith(corev1.PodRunning, "1", "2Gi", "2", "4Gi"),
		podWith(corev1.PodSucceeded, "1", "2Gi", "2", "4Gi"), // terminal -> excluded
	}
	usage := map[string]corev1.ResourceList{
		"n1": {corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		"n2": {corev1.ResourceCPU: resource.MustParse("1500m"), corev1.ResourceMemory: resource.MustParse("3Gi")},
	}
	s := Summarize(nodes, pods, usage)
	if !s.MetricsAvailable {
		t.Fatal("expected MetricsAvailable=true")
	}
	if s.CPU.Allocatable != "8.0" || s.CPU.Requests != "2.0" || s.CPU.RequestsPct != 25 {
		t.Errorf("CPU req = %+v", s.CPU)
	}
	if s.CPU.Limits != "4.0" || s.CPU.LimitsPct != 50 {
		t.Errorf("CPU lim = %+v", s.CPU)
	}
	if s.CPU.Usage != "2.0" || s.CPU.UsagePct != 25 {
		t.Errorf("CPU usage = %+v", s.CPU)
	}
	if s.Memory.Allocatable != "16Gi" || s.Memory.Requests != "4Gi" || s.Memory.RequestsPct != 25 {
		t.Errorf("Mem = %+v", s.Memory)
	}
	if s.Memory.Limits != "8Gi" || s.Memory.LimitsPct != 50 || s.Memory.Usage != "4Gi" || s.Memory.UsagePct != 25 {
		t.Errorf("Mem lim/usage = %+v", s.Memory)
	}
}

func TestSummarize_NoMetrics(t *testing.T) {
	nodes := []corev1.Node{node("n1", "4", "8Gi")}
	pods := []corev1.Pod{podWith(corev1.PodRunning, "1", "2Gi", "2", "4Gi")}
	s := Summarize(nodes, pods, nil)
	if s.MetricsAvailable {
		t.Error("expected MetricsAvailable=false with nil usage")
	}
	if s.CPU.Usage != "" || s.CPU.UsagePct != 0 {
		t.Errorf("expected empty usage, got %+v", s.CPU)
	}
	if s.CPU.Allocatable != "4.0" || s.CPU.RequestsPct != 25 {
		t.Errorf("CPU = %+v", s.CPU)
	}
}

func TestSummarize_ZeroAllocatableNoDivByZero(t *testing.T) {
	s := Summarize(nil, []corev1.Pod{podWith(corev1.PodRunning, "1", "1Gi", "1", "1Gi")}, nil)
	if s.CPU.RequestsPct != 0 || s.Memory.RequestsPct != 0 {
		t.Errorf("expected 0%% with no nodes, got cpu=%d mem=%d", s.CPU.RequestsPct, s.Memory.RequestsPct)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/resources/`
Expected: FAIL — build error, `undefined: Summarize` / package has no non-test files.

- [ ] **Step 3: Write minimal implementation**

Create `internal/resources/resources.go`:

```go
// Package resources computes a cluster-wide CPU and memory summary: allocatable
// capacity, reserved (pod requests) and limits, and optional live usage from
// metrics-server. It is pure — the caller supplies nodes, pods, and usage.
package resources

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Line is one resource's cluster accounting. Quantities are human-readable
// strings; percentages are integers of allocatable (0 when allocatable is 0).
type Line struct {
	Allocatable string `json:"allocatable"`
	Requests    string `json:"requests"`
	Limits      string `json:"limits"`
	Usage       string `json:"usage,omitempty"` // "" when metrics unavailable
	RequestsPct int    `json:"requestsPct"`
	LimitsPct   int    `json:"limitsPct"`
	UsagePct    int    `json:"usagePct,omitempty"`
}

// Summary is the cluster-wide CPU and memory picture.
type Summary struct {
	CPU              Line `json:"cpu"`
	Memory           Line `json:"memory"`
	MetricsAvailable bool `json:"metricsAvailable"`
}

// Summarize aggregates node allocatable, pod requests/limits (over non-terminal
// pods only — terminal pods reserve nothing), and optional per-node usage into a
// cluster Summary. nil/empty usage yields MetricsAvailable=false.
func Summarize(nodes []corev1.Node, pods []corev1.Pod, usage map[string]corev1.ResourceList) Summary {
	var cpuAlloc, memAlloc, cpuReq, cpuLim, memReq, memLim, cpuUse, memUse resource.Quantity
	for _, n := range nodes {
		cpuAlloc.Add(n.Status.Allocatable[corev1.ResourceCPU])
		memAlloc.Add(n.Status.Allocatable[corev1.ResourceMemory])
	}
	for _, p := range pods {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			cpuReq.Add(c.Resources.Requests[corev1.ResourceCPU])
			cpuLim.Add(c.Resources.Limits[corev1.ResourceCPU])
			memReq.Add(c.Resources.Requests[corev1.ResourceMemory])
			memLim.Add(c.Resources.Limits[corev1.ResourceMemory])
		}
	}
	available := len(usage) > 0
	for _, u := range usage {
		cpuUse.Add(u[corev1.ResourceCPU])
		memUse.Add(u[corev1.ResourceMemory])
	}
	return Summary{
		MetricsAvailable: available,
		CPU:              cpuLine(cpuAlloc, cpuReq, cpuLim, cpuUse, available),
		Memory:           memLine(memAlloc, memReq, memLim, memUse, available),
	}
}

func cpuLine(alloc, req, lim, use resource.Quantity, available bool) Line {
	a := alloc.MilliValue()
	l := Line{
		Allocatable: formatCPU(alloc),
		Requests:    formatCPU(req),
		Limits:      formatCPU(lim),
		RequestsPct: pct(req.MilliValue(), a),
		LimitsPct:   pct(lim.MilliValue(), a),
	}
	if available {
		l.Usage = formatCPU(use)
		l.UsagePct = pct(use.MilliValue(), a)
	}
	return l
}

func memLine(alloc, req, lim, use resource.Quantity, available bool) Line {
	a := alloc.Value()
	l := Line{
		Allocatable: formatMem(alloc),
		Requests:    formatMem(req),
		Limits:      formatMem(lim),
		RequestsPct: pct(req.Value(), a),
		LimitsPct:   pct(lim.Value(), a),
	}
	if available {
		l.Usage = formatMem(use)
		l.UsagePct = pct(use.Value(), a)
	}
	return l
}

func pct(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	return int(part * 100 / whole)
}

// formatCPU renders a quantity as cores with one decimal, e.g. "8.0".
func formatCPU(q resource.Quantity) string {
	return fmt.Sprintf("%.1f", float64(q.MilliValue())/1000)
}

// formatMem renders a quantity in Gi (or Mi below 1Gi), rounded, e.g. "16Gi".
func formatMem(q resource.Quantity) string {
	b := q.Value()
	if b >= 1<<30 {
		return fmt.Sprintf("%.0fGi", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/resources/ -v`
Expected: PASS (all three tests).

- [ ] **Step 5: Commit**

```bash
git add internal/resources/
git commit -m "feat(resources): cluster CPU/memory summary (allocatable/reserved/limits/usage)"
```

---

### Task 2: `internal/collect` — metrics fetch + all-namespaces pods

**Files:**
- Modify: `internal/collect/collect.go`
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Consumes: `kubernetes.Interface`, `corev1`, `resource`, `metav1`.
- Produces:
  - `func parseNodeMetrics(data []byte) (map[string]corev1.ResourceList, error)` (unexported, unit-tested)
  - `func NodeMetrics(ctx context.Context, client kubernetes.Interface) (map[string]corev1.ResourceList, bool, error)`
  - `func AllPods(ctx context.Context, client kubernetes.Interface) ([]corev1.Pod, error)`

- [ ] **Step 1: Write the failing test**

Append to `internal/collect/collect_test.go` (it already constructs a fake clientset and imports `context`, `testing`, `corev1`, `metav1`, and `k8s.io/client-go/kubernetes/fake`; add `"k8s.io/apimachinery/pkg/api/resource"` to its imports):

```go
func TestParseNodeMetrics(t *testing.T) {
	data := []byte(`{"items":[
	  {"metadata":{"name":"n1"},"usage":{"cpu":"531m","memory":"27711Mi"}},
	  {"metadata":{"name":"n2"},"usage":{"cpu":"1046m","memory":"21927Mi"}}
	]}`)
	got, err := parseNodeMetrics(data)
	if err != nil {
		t.Fatalf("parseNodeMetrics: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(got))
	}
	if cpu := got["n1"][corev1.ResourceCPU]; cpu.MilliValue() != 531 {
		t.Errorf("n1 cpu = %d milli, want 531", cpu.MilliValue())
	}
	if mem := got["n2"][corev1.ResourceMemory]; mem.Value() != 21927*(1<<20) {
		t.Errorf("n2 mem = %d bytes", mem.Value())
	}
}

func TestParseNodeMetrics_Malformed(t *testing.T) {
	if _, err := parseNodeMetrics([]byte("not json")); err == nil {
		t.Error("expected error on malformed input")
	}
}

func TestAllPods_ListsAcrossNamespaces(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "p2"}},
	)
	pods, err := AllPods(context.Background(), client)
	if err != nil {
		t.Fatalf("AllPods: %v", err)
	}
	if len(pods) != 2 {
		t.Errorf("want 2 pods across namespaces, got %d", len(pods))
	}
}
```

(If `collect_test.go` uses `fake.NewClientset` rather than `fake.NewSimpleClientset`, match whatever that file already uses.)

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/`
Expected: FAIL — `undefined: parseNodeMetrics`, `undefined: AllPods`.

- [ ] **Step 3: Write minimal implementation**

In `internal/collect/collect.go`, add `"context"` (already present), `"encoding/json"`, and `"k8s.io/apimachinery/pkg/api/resource"` to imports, then append:

```go
// AllPods lists pods across all namespaces (read-only). Used for the cluster
// resource summary when the scan itself is namespace-scoped.
func AllPods(ctx context.Context, client kubernetes.Interface) ([]corev1.Pod, error) {
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing all pods: %w", err)
	}
	return pods.Items, nil
}

// NodeMetrics reads live per-node usage from metrics-server via a raw GET on the
// metrics API. available is false (and err nil) when metrics-server is absent or
// forbidden, so a scan still succeeds without it.
func NodeMetrics(ctx context.Context, client kubernetes.Interface) (map[string]corev1.ResourceList, bool, error) {
	data, err := client.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").DoRaw(ctx)
	if err != nil {
		return nil, false, nil // metrics-server absent/forbidden — non-fatal
	}
	usage, err := parseNodeMetrics(data)
	if err != nil {
		return nil, false, err
	}
	return usage, len(usage) > 0, nil
}

// parseNodeMetrics decodes a metrics.k8s.io NodeMetricsList body into per-node
// resource quantities keyed by node name.
func parseNodeMetrics(data []byte) (map[string]corev1.ResourceList, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage map[string]string `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parsing node metrics: %w", err)
	}
	out := make(map[string]corev1.ResourceList, len(list.Items))
	for _, it := range list.Items {
		rl := corev1.ResourceList{}
		for k, v := range it.Usage {
			q, err := resource.ParseQuantity(v)
			if err != nil {
				return nil, fmt.Errorf("parsing usage %q for node %s: %w", v, it.Metadata.Name, err)
			}
			rl[corev1.ResourceName(k)] = q
		}
		out[it.Metadata.Name] = rl
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -v`
Expected: PASS. (`NodeMetrics`'s raw-REST path is verified live in Task 6, not unit-tested — the fake clientset's RESTClient cannot model the metrics API.)

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): best-effort node metrics + all-namespaces pod list"
```

---

### Task 3: `internal/diagnose` — OOMKilled attaches container resources

**Files:**
- Modify: `internal/diagnose/diagnose.go` (add `ContainerResources`, add `Finding.Resources`)
- Modify: `internal/diagnose/oomkilled.go` (populate it)
- Modify: `internal/diagnose/helpers_test.go` (add a helper)
- Test: `internal/diagnose/oomkilled_test.go`

**Interfaces:**
- Produces:
  - `type ContainerResources struct { Container, CPURequest, CPULimit, MemRequest, MemLimit string }`
  - `Finding.Resources *ContainerResources` (`json:"resources,omitempty"`)

- [ ] **Step 1: Write the failing test**

Add to `internal/diagnose/helpers_test.go` (add `"k8s.io/apimachinery/pkg/api/resource"` to its imports):

```go
// podOOMKilledWithResources is an OOMKilled pod whose spec declares the killed
// container with the given requests/limits.
func podOOMKilledWithResources(ns, name, container string, res corev1.ResourceRequirements) *corev1.Pod {
	p := podOOMKilled(ns, name, container, 137, false)
	p.Spec.Containers = []corev1.Container{{Name: container, Resources: res}}
	return p
}
```

Add to `internal/diagnose/oomkilled_test.go` (add imports `corev1 "k8s.io/api/core/v1"` and `"k8s.io/apimachinery/pkg/api/resource"`):

```go
func TestOOMKilledDetector_AttachesContainerResources(t *testing.T) {
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("4Gi")},
	}
	facts := PodFacts{Pod: podOOMKilledWithResources("cattle-system", "rancher", "rancher", res)}
	f := OOMKilledDetector{}.Detect(facts)
	if f == nil || f.Resources == nil {
		t.Fatalf("expected finding with resources, got %+v", f)
	}
	r := f.Resources
	if r.Container != "rancher" || r.MemRequest != "1Gi" || r.MemLimit != "4Gi" || r.CPURequest != "500m" || r.CPULimit != "3" {
		t.Errorf("resources = %+v", r)
	}
}

func TestOOMKilledDetector_UnsetLimitRendersUnset(t *testing.T) {
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	facts := PodFacts{Pod: podOOMKilledWithResources("ns", "p", "c", res)}
	f := OOMKilledDetector{}.Detect(facts)
	if f == nil || f.Resources == nil {
		t.Fatal("expected resources")
	}
	if f.Resources.MemLimit != "unset" || f.Resources.CPULimit != "unset" || f.Resources.CPURequest != "unset" {
		t.Errorf("expected unset for missing entries, got %+v", f.Resources)
	}
	if f.Resources.MemRequest != "1Gi" {
		t.Errorf("expected mem request 1Gi, got %+v", f.Resources)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/diagnose/`
Expected: FAIL — `f.Resources undefined` / `undefined: podOOMKilledWithResources`.

- [ ] **Step 3: Write minimal implementation**

In `internal/diagnose/diagnose.go`, add the type and the `Finding` field:

```go
// ContainerResources is one container's requests/limits, as human-readable
// quantity strings ("500m", "4Gi"). A missing request or limit is "unset".
type ContainerResources struct {
	Container  string `json:"container"`
	CPURequest string `json:"cpuRequest"`
	CPULimit   string `json:"cpuLimit"`
	MemRequest string `json:"memRequest"`
	MemLimit   string `json:"memLimit"`
}
```

and add to the `Finding` struct (after `Evidence`):

```go
	Resources *ContainerResources `json:"resources,omitempty"` // set by OOMKilled
```

In `internal/diagnose/oomkilled.go`, set `Resources` in the returned finding and add the helpers (the file already imports `fmt` and `corev1`):

```go
				if term != nil && term.Reason == "OOMKilled" {
					return &Finding{
						Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
						Issue:     "OOMKilled",
						Reason:    "Container exceeded its memory limit and was killed",
						Evidence:  fmt.Sprintf("container %q, exitCode=%d", cs.Name, term.ExitCode),
						Resources: containerResources(facts.Pod, cs.Name),
					}
				}
```

```go
// containerResources finds the named container in the pod spec and returns its
// cpu/memory requests and limits; nil if the container is not in the spec.
func containerResources(pod *corev1.Pod, name string) *ContainerResources {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return &ContainerResources{
				Container:  name,
				CPURequest: quantityOrUnset(c.Resources.Requests, corev1.ResourceCPU),
				CPULimit:   quantityOrUnset(c.Resources.Limits, corev1.ResourceCPU),
				MemRequest: quantityOrUnset(c.Resources.Requests, corev1.ResourceMemory),
				MemLimit:   quantityOrUnset(c.Resources.Limits, corev1.ResourceMemory),
			}
		}
	}
	return nil
}

func quantityOrUnset(rl corev1.ResourceList, n corev1.ResourceName) string {
	if q, ok := rl[n]; ok {
		return q.String()
	}
	return "unset"
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/diagnose/ -v`
Expected: PASS (new tests plus the existing OOMKilled tests).

- [ ] **Step 5: Commit**

```bash
git add internal/diagnose/
git commit -m "feat(diagnose): attach killed container's requests/limits to OOMKilled finding"
```

---

### Task 4: `internal/report` — render resource block, per-OOM line, JSON field

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `resources.Summary`, `diagnose.ContainerResources`.
- Produces (changed signature):
  `func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, explanation, format string, w io.Writer) error`

- [ ] **Step 1: Write the failing test**

In `internal/report/report_test.go`, add `"github.com/imantaba/kubeagent/internal/resources"` to imports and add:

```go
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
	if err := PrintInventory(ch, inventory.Result{}, sampleSummary(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Resources (cluster):", "8.0 cores", "req 2.0 (25%)", "lim 4.0 (50%)", "used 2.0 (25%)", "16Gi", "used 4Gi (25%)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestPrintInventory_TextResourceBlockNoMetrics(t *testing.T) {
	var buf bytes.Buffer
	s := sampleSummary()
	s.MetricsAvailable = false
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, s, "", "text", &buf); err != nil {
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
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: ws}, nil, "", "text", &buf); err != nil {
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
	if err := PrintInventory(ch, inventory.Result{}, sampleSummary(), "", "json", &buf); err != nil {
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
```

Then update **every existing `PrintInventory(...)` call** in this file to pass the new `summary` argument in 3rd position. The existing calls have the shape `PrintInventory(<cluster>, <result>, <explanation>, <format>, &buf)`; insert `nil` as the new third argument so they become `PrintInventory(<cluster>, <result>, nil, <explanation>, <format>, &buf)`. (15 existing call sites — all pass `nil`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/`
Expected: FAIL — too many arguments / `undefined: resources` until the signature and rendering exist.

- [ ] **Step 3: Write minimal implementation**

In `internal/report/report.go`: add `"github.com/imantaba/kubeagent/internal/resources"` to imports; add `Resources` to the JSON struct; change the signature; thread `summary` through; render the block and the per-finding line.

Add to `inventoryReport`:

```go
	Resources   *resources.Summary          `json:"resources,omitempty"`
```

Change `PrintInventory` and the JSON encode:

```go
func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Cluster: cluster, Workloads: result.Workloads, Resources: summary, Explanation: explanation})
	case "text":
		return printInventoryText(cluster, result, summary, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}
```

Change `printInventoryText`'s signature to accept `summary *resources.Summary` and call `printResources` right after the cluster-verdict block (after the `fmt.Fprintln(w)` that ends that block, before the workloads loop):

```go
func printInventoryText(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, explanation string, w io.Writer) error {
	// ... existing cluster-verdict block unchanged, ending with fmt.Fprintln(w) ...

	if err := printResources(summary, w); err != nil {
		return err
	}

	// ... existing workloads / all-clear / footer / explanation blocks unchanged ...
}
```

Add the rendering helpers:

```go
func printResources(s *resources.Summary, w io.Writer) error {
	if s == nil {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Resources (cluster):"); err != nil {
		return err
	}
	if err := printResLine(w, "CPU   ", s.CPU, "cores", s.MetricsAvailable); err != nil {
		return err
	}
	if err := printResLine(w, "Memory", s.Memory, "", s.MetricsAvailable); err != nil {
		return err
	}
	if !s.MetricsAvailable {
		if _, err := fmt.Fprintln(w, "  (usage: metrics-server unavailable)"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

func printResLine(w io.Writer, label string, l resources.Line, unit string, metrics bool) error {
	alloc := l.Allocatable
	if unit != "" {
		alloc += " " + unit
	}
	line := fmt.Sprintf("  %s  %s · req %s (%d%%) · lim %s (%d%%)",
		label, alloc, l.Requests, l.RequestsPct, l.Limits, l.LimitsPct)
	if metrics {
		line += fmt.Sprintf(" · used %s (%d%%)", l.Usage, l.UsagePct)
	}
	_, err := fmt.Fprintln(w, line)
	return err
}
```

In `printWorkload`, render the per-finding resource sub-line inside the findings loop:

```go
	for _, f := range wl.Findings {
		if _, err := fmt.Fprintf(w, "    ⚠ %s: %s\n", f.Issue, f.Reason); err != nil {
			return err
		}
		if f.Resources != nil {
			r := f.Resources
			if _, err := fmt.Fprintf(w, "      resources: memory req=%s limit=%s · cpu req=%s limit=%s\n",
				r.MemRequest, r.MemLimit, r.CPURequest, r.CPULimit); err != nil {
				return err
			}
		}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -v`
Expected: PASS (new + existing tests).

- [ ] **Step 5: Commit**

```bash
git add internal/report/
git commit -m "feat(report): cluster resource block, per-OOM resources, JSON resources field"
```

---

### Task 5: `internal/explain` — resources in the prompt

**Files:**
- Modify: `internal/explain/explain.go`
- Test: `internal/explain/explain_test.go`

**Interfaces:**
- Consumes: `resources.Summary`, `diagnose` findings.
- Produces (changed signatures):
  - `func (c *Client) ExplainInventory(ctx, cluster clusterhealth.ClusterHealth, summary *resources.Summary, workloads []inventory.Workload) (string, error)`
  - `func buildInventoryPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, workloads []inventory.Workload) string`

- [ ] **Step 1: Write the failing test**

In `internal/explain/explain_test.go`, add `"github.com/imantaba/kubeagent/internal/resources"` to imports, update **every existing** `ExplainInventory(ctx, <cluster>, <workloads>)` call to `ExplainInventory(ctx, <cluster>, nil, <workloads>)` and every `buildInventoryPrompt(<cluster>, <workloads>)` to `buildInventoryPrompt(<cluster>, nil, <workloads>)`, then add:

```go
func TestBuildInventoryPrompt_IncludesClusterResources(t *testing.T) {
	s := &resources.Summary{
		MetricsAvailable: true,
		CPU:              resources.Line{Allocatable: "8.0", Requests: "2.0", Limits: "4.0", Usage: "2.0", RequestsPct: 25, LimitsPct: 50, UsagePct: 25},
		Memory:           resources.Line{Allocatable: "16Gi", Requests: "4Gi", Limits: "8Gi", Usage: "4Gi", RequestsPct: 25, LimitsPct: 50, UsagePct: 25},
	}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, s, nil)
	for _, want := range []string{"Cluster resources:", "allocatable 8.0 cores", "requests 2.0 (25%)", "usage 2.0 (25%)", "allocatable 16Gi"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestBuildInventoryPrompt_IncludesOOMContainerResources(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "cattle-system", Name: "rancher", Kind: "Deployment", Ready: 0, Desired: 1, Status: "Degraded",
		Findings: []diagnose.Finding{{
			Pod: "cattle-system/rancher-x", Issue: "OOMKilled", Reason: "killed", Evidence: "e",
			Resources: &diagnose.ContainerResources{Container: "rancher", CPURequest: "500m", CPULimit: "3", MemRequest: "1Gi", MemLimit: "4Gi"},
		}},
	}}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, ws)
	if !strings.Contains(got, "container resources: memory req=1Gi limit=4Gi, cpu req=500m limit=3") {
		t.Errorf("prompt missing OOM container resources:\n%s", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/`
Expected: FAIL — wrong arg count / `undefined: resources`.

- [ ] **Step 3: Write minimal implementation**

In `internal/explain/explain.go`: add `"github.com/imantaba/kubeagent/internal/resources"` to imports; change `ExplainInventory` to take `summary *resources.Summary` and forward it; change `buildInventoryPrompt` to take and render it.

```go
func (c *Client) ExplainInventory(ctx context.Context, cluster clusterhealth.ClusterHealth, summary *resources.Summary, workloads []inventory.Workload) (string, error) {
	if cluster.Verdict != "Degraded" && len(workloads) == 0 {
		return "", nil
	}
	out, err := c.s.summarize(ctx, buildInventoryPrompt(cluster, summary, workloads))
	// ... rest unchanged ...
}
```

In `buildInventoryPrompt`, change the signature and add the resources section after the cluster-verdict block (before the workloads section), and the per-finding container line inside the findings loop:

```go
func buildInventoryPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, workloads []inventory.Workload) string {
	var b strings.Builder
	// ... existing cluster degraded block unchanged ...

	if summary != nil {
		b.WriteString("Cluster resources:\n")
		writeResLine(&b, "CPU", summary.CPU, "cores", summary.MetricsAvailable)
		writeResLine(&b, "Memory", summary.Memory, "", summary.MetricsAvailable)
		b.WriteString("\n")
	}

	if len(workloads) > 0 {
		b.WriteString("These Kubernetes workloads need attention:\n\n")
		for _, w := range workloads {
			fmt.Fprintf(&b, "- %s/%s (%s): %d/%d ready, status %s, %d restarts\n",
				w.Namespace, w.Name, w.Kind, w.Ready, w.Desired, w.Status, w.Restarts)
			for _, f := range w.Findings {
				fmt.Fprintf(&b, "    issue: %s — %s (%s)\n", f.Issue, f.Reason, f.Evidence)
				if f.Resources != nil {
					r := f.Resources
					fmt.Fprintf(&b, "      container resources: memory req=%s limit=%s, cpu req=%s limit=%s\n",
						r.MemRequest, r.MemLimit, r.CPURequest, r.CPULimit)
				}
			}
		}
	}
	b.WriteString("\nExplain what is going wrong and suggest concrete next steps.")
	return b.String()
}

func writeResLine(b *strings.Builder, label string, l resources.Line, unit string, metrics bool) {
	alloc := l.Allocatable
	if unit != "" {
		alloc += " " + unit
	}
	fmt.Fprintf(b, "  %s: allocatable %s, requests %s (%d%%), limits %s (%d%%)",
		label, alloc, l.Requests, l.RequestsPct, l.Limits, l.LimitsPct)
	if metrics {
		fmt.Fprintf(b, ", usage %s (%d%%)", l.Usage, l.UsagePct)
	}
	b.WriteString("\n")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ -v`
Expected: PASS. The existing `TestBuildInventoryPrompt_OnlyStructuredFields` (now passing `nil` summary) still guards against pod IP / node-name egress.

- [ ] **Step 5: Commit**

```bash
git add internal/explain/
git commit -m "feat(explain): include cluster resources + OOM container resources in the prompt"
```

---

### Task 6: wire it in `main.go`, document, verify live

**Files:**
- Modify: `main.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `collect.NodeMetrics`, `collect.AllPods`, `resources.Summarize`, the new `report.PrintInventory` / `explain.ExplainInventory` signatures.

- [ ] **Step 1: Wire the pipeline in `main.go`**

Add `"github.com/imantaba/kubeagent/internal/resources"` to imports. After the `clusterhealth.Assess` / `ScopeNote` lines and before `inventory.Prioritize`, insert:

```go
	usage, _, _ := collect.NodeMetrics(context.Background(), client) // best-effort; nil when no metrics-server
	resourcePods := inputs.Pods
	if namespace != "" {
		if all, perr := collect.AllPods(context.Background(), client); perr == nil {
			resourcePods = all
		}
	}
	summary := resources.Summarize(nodes, resourcePods, usage)
```

Update the two consumers to pass `&summary`:

```go
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).ExplainInventory(ctx, health, &summary, result.Workloads)
```

```go
	return report.PrintInventory(health, result, &summary, explanation, *output, os.Stdout)
```

- [ ] **Step 2: Build, vet, and run the whole suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go vet ./... && go test ./... && go build -o /tmp/kubeagent .`
Expected: all packages `ok`, build succeeds.

- [ ] **Step 3: Verify live against the cluster**

Run: `/tmp/kubeagent scan -n cattle-system`
Expected: a `Resources (cluster):` block appears under the verdict with CPU/Memory `req/lim/used` lines (metrics-server is present on `hetzner-nova`).

Run: `/tmp/kubeagent scan -o json | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps(d["resources"], indent=2))'`
Expected: a `resources` object with `cpu`, `memory`, and `metricsAvailable: true`.

(If a workload is currently OOMKilled, confirm its finding shows the `resources: memory req=… limit=…` sub-line. If none is OOMKilling right now, the per-finding rendering is already covered by the Task 4/5 unit tests.)

- [ ] **Step 4: Document in `README.md`**

In the scan/usage section, add a short note (place it near the existing flag/output documentation):

```markdown
### Resource context

`scan` prints a compact cluster resource summary (CPU and memory: allocatable,
reserved/requests, limits, and — when metrics-server is installed — live usage),
and annotates each OOMKilled finding with the killed container's requests and
limits. This context is also sent to `--explain` so the model can judge whether
to raise a limit or scale out. Live usage is best-effort: without metrics-server
the summary still shows allocatable/reserved/limits and notes usage as
unavailable. Reading usage is read-only (a single GET on the metrics API).
```

- [ ] **Step 5: Commit**

```bash
git add main.go README.md
git commit -m "feat: wire cluster resource summary into scan + explain; document"
```

---

## Self-Review

**Spec coverage:**
- OOM finding shows cpu+mem requests+limits → Task 3 (+ rendering in Tasks 4/5). ✓
- Cluster summary: allocatable/reserved/limits/usage → Task 1 (+ Task 2 metrics). ✓
- Live usage via raw REST, no new dependency, graceful → Task 2. ✓
- Reserved excludes terminal pods → Task 1 (`Summarize`). ✓
- Namespace-scope accuracy (all-namespaces pods when `-n`) → Task 2 `AllPods` + Task 6 wiring. ✓
- Surfacing: always in text + JSON + explain; per-OOM inline → Tasks 4, 5, 6. ✓
- Egress: cluster totals + container quantities only → Task 5 (existing egress test retained). ✓
- Docs → Task 6. ✓

**Placeholder scan:** none — every step has concrete code/commands.

**Type consistency:** `resources.Summary`/`Line`, `diagnose.ContainerResources`, the `*resources.Summary` parameter, and the `PrintInventory` / `ExplainInventory` / `buildInventoryPrompt` signatures are used identically across Tasks 1–6.
