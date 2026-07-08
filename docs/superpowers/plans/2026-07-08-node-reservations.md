# Node Reservations Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `kubeagent scan` shows each node's aggregate kubelet resource reservation (Capacity − Allocatable) and warns when a node reserves no memory; the `watch` daemon exposes the warning count as a Prometheus gauge.

**Architecture:** A new pure package `internal/nodereserve` computes per-node reserved cpu/mem from the Node objects `scan.Evaluate` already collects (no new API call, no new RBAC). `scan.Result` carries the report; `internal/report` renders a text section and a JSON field; `internal/watch/metrics.go` exposes `kubeagent_nodes_without_reservations`.

**Tech Stack:** Go 1.26, `k8s.io/api/core/v1`, `k8s.io/apimachinery/pkg/api/resource`. Tests use fake `corev1.Node` values (no cluster).

## Global Constraints

- **Read-only.** No writes, no LLM, no new API calls. `nodes` `get`/`list` is already granted — **no RBAC change**.
- **Warn rule:** a node warns when reserved **memory** == 0 (allocatable memory == capacity memory). CPU reserved is shown but never warned.
- **Advisory only:** the check must NOT change `clusterhealth` verdict, `kubeagent_cluster_healthy`, or the scan exit code.
- **Not** wired into `--explain`.
- Reserved = `Capacity − Allocatable` per resource; negative deltas clamp to 0.
- Commits: **no `Co-Authored-By: Claude` trailer** (project rule).
- TDD: write the failing test first, watch it fail, then implement.

---

### Task 1: `internal/nodereserve` package

**Files:**
- Create: `internal/nodereserve/nodereserve.go`
- Test: `internal/nodereserve/nodereserve_test.go`

**Interfaces:**
- Consumes: `[]corev1.Node` (each with `Status.Capacity`, `Status.Allocatable`, `ObjectMeta.Labels`).
- Produces:
  - `type NodeReservation struct { Name string; Role string; CPUReserved string; MemReserved string; Warning bool }` (JSON tags below).
  - `type Report struct { Nodes []NodeReservation; WarnCount int }` (JSON tags below).
  - `func Assess(nodes []corev1.Node) Report`.

- [ ] **Step 1: Write the failing test**

Create `internal/nodereserve/nodereserve_test.go`:

```go
package nodereserve

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// node builds a fake node with the given capacity and allocatable cpu/mem.
func node(name, capCPU, capMem, allocCPU, allocMem string, labels map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(capCPU),
				corev1.ResourceMemory: resource.MustParse(capMem),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(allocCPU),
				corev1.ResourceMemory: resource.MustParse(allocMem),
			},
		},
	}
}

func find(r Report, name string) NodeReservation {
	for _, n := range r.Nodes {
		if n.Name == name {
			return n
		}
	}
	return NodeReservation{}
}

func TestAssess_WarnsWhenMemoryReservedZero(t *testing.T) {
	// allocatable mem == capacity mem -> nothing reserved -> warn.
	r := Assess([]corev1.Node{node("w1", "4", "16Gi", "4", "16Gi", nil)})
	n := find(r, "w1")
	if !n.Warning {
		t.Fatalf("want Warning=true when mem reserved is 0, got %+v", n)
	}
	if n.MemReserved != "0" {
		t.Errorf("want MemReserved %q, got %q", "0", n.MemReserved)
	}
	if n.CPUReserved != "0" {
		t.Errorf("want CPUReserved %q, got %q", "0", n.CPUReserved)
	}
	if r.WarnCount != 1 {
		t.Errorf("want WarnCount 1, got %d", r.WarnCount)
	}
}

func TestAssess_OKWhenMemoryReserved(t *testing.T) {
	// 800Mi mem reserved, cpu unset (0) -> not warned.
	r := Assess([]corev1.Node{node("w2", "4", "16Gi", "4", "15584Mi", nil)})
	n := find(r, "w2")
	if n.Warning {
		t.Errorf("want Warning=false when mem is reserved, got %+v", n)
	}
	if n.MemReserved != "800Mi" {
		t.Errorf("want MemReserved %q, got %q", "800Mi", n.MemReserved)
	}
	if n.CPUReserved != "0" {
		t.Errorf("want CPUReserved %q (cpu unset), got %q", "0", n.CPUReserved)
	}
	if r.WarnCount != 0 {
		t.Errorf("want WarnCount 0, got %d", r.WarnCount)
	}
}

func TestAssess_FormatsCPUAndMemReserved(t *testing.T) {
	// 200m cpu, 1Gi mem reserved.
	r := Assess([]corev1.Node{node("w3", "4", "16Gi", "3800m", "15Gi", nil)})
	n := find(r, "w3")
	if n.CPUReserved != "200m" {
		t.Errorf("want CPUReserved %q, got %q", "200m", n.CPUReserved)
	}
	if n.MemReserved != "1Gi" {
		t.Errorf("want MemReserved %q, got %q", "1Gi", n.MemReserved)
	}
	if n.Warning {
		t.Errorf("want Warning=false, got true")
	}
}

func TestAssess_ClampsNegativeDeltaToZero(t *testing.T) {
	// allocatable > capacity (pathological) -> clamp to 0, warn on mem.
	r := Assess([]corev1.Node{node("w4", "4", "16Gi", "5", "17Gi", nil)})
	n := find(r, "w4")
	if n.CPUReserved != "0" || n.MemReserved != "0" {
		t.Errorf("want reserved clamped to 0/0, got cpu=%q mem=%q", n.CPUReserved, n.MemReserved)
	}
	if !n.Warning {
		t.Errorf("want Warning=true when mem reserved clamps to 0")
	}
}

func TestAssess_RoleFromLabels(t *testing.T) {
	cp := node("m1", "4", "16Gi", "4", "16Gi", map[string]string{"node-role.kubernetes.io/control-plane": ""})
	wk := node("w1", "4", "16Gi", "4", "15Gi", nil)
	r := Assess([]corev1.Node{cp, wk})
	if got := find(r, "m1").Role; got != "control-plane" {
		t.Errorf("want Role control-plane, got %q", got)
	}
	if got := find(r, "w1").Role; got != "worker" {
		t.Errorf("want Role worker, got %q", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/nodereserve/`
Expected: FAIL — `undefined: Assess` / `undefined: Report` (package has no implementation yet).

- [ ] **Step 3: Write the implementation**

Create `internal/nodereserve/nodereserve.go`:

```go
// Package nodereserve reports each node's aggregate kubelet resource
// reservation, observed as Capacity - Allocatable (kube-reserved +
// system-reserved + eviction-hard combined). It warns when a node reserves no
// memory, a kubelet configuration that lets OS/kubelet memory pressure
// destabilise the node. Pure: the caller supplies the nodes. Read-only.
package nodereserve

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// NodeReservation is one node's observed reservation. Reserved amounts are
// human-readable strings ("200m", "800Mi", "0"). Warning is set when the node
// reserves no memory.
type NodeReservation struct {
	Name        string `json:"name"`
	Role        string `json:"role,omitempty"`
	CPUReserved string `json:"cpuReserved"`
	MemReserved string `json:"memReserved"`
	Warning     bool   `json:"warning"`
}

// Report is the per-node reservation picture. WarnCount is the number of nodes
// with Warning set.
type Report struct {
	Nodes     []NodeReservation `json:"nodes"`
	WarnCount int               `json:"warnCount"`
}

// Assess computes reserved cpu/memory for each node as Capacity - Allocatable
// (clamped at 0) and flags nodes that reserve no memory.
func Assess(nodes []corev1.Node) Report {
	rep := Report{Nodes: make([]NodeReservation, 0, len(nodes))}
	for _, n := range nodes {
		cpuRes := reserved(n.Status.Capacity[corev1.ResourceCPU], n.Status.Allocatable[corev1.ResourceCPU])
		memRes := reserved(n.Status.Capacity[corev1.ResourceMemory], n.Status.Allocatable[corev1.ResourceMemory])
		warn := memRes.Value() == 0
		if warn {
			rep.WarnCount++
		}
		rep.Nodes = append(rep.Nodes, NodeReservation{
			Name:        n.Name,
			Role:        role(n),
			CPUReserved: fmtCPU(cpuRes),
			MemReserved: fmtMem(memRes),
			Warning:     warn,
		})
	}
	return rep
}

// reserved returns capacity - allocatable, clamped to zero on a negative delta.
func reserved(capacity, allocatable resource.Quantity) resource.Quantity {
	out := capacity.DeepCopy()
	out.Sub(allocatable)
	if out.Sign() < 0 {
		return resource.Quantity{}
	}
	return out
}

// role classifies the node from its node-role labels.
func role(n corev1.Node) string {
	for k := range n.Labels {
		if k == "node-role.kubernetes.io/control-plane" || k == "node-role.kubernetes.io/master" {
			return "control-plane"
		}
	}
	return "worker"
}

// fmtCPU renders reserved cpu as millicores ("200m") or "0".
func fmtCPU(q resource.Quantity) string {
	m := q.MilliValue()
	if m <= 0 {
		return "0"
	}
	return fmt.Sprintf("%dm", m)
}

// fmtMem renders reserved memory in Gi/Mi ("1Gi", "800Mi") or "0".
func fmtMem(q resource.Quantity) string {
	b := q.Value()
	if b <= 0 {
		return "0"
	}
	if b >= 1<<30 {
		return fmt.Sprintf("%.0fGi", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/nodereserve/`
Expected: PASS (all 5 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/nodereserve/
git commit -m "feat(nodereserve): assess per-node kubelet reservation from capacity-allocatable"
```

---

### Task 2: Wire the report into `scan.Result`

**Files:**
- Modify: `internal/scan/scan.go` (imports, `Result` struct ~33-39, `Evaluate` return ~86)

**Interfaces:**
- Consumes: `nodereserve.Assess(nodes) Report` from Task 1; `nodes` already collected at `scan.go:61`.
- Produces: `scan.Result.NodeReserve nodereserve.Report` for Tasks 3 and 4.

- [ ] **Step 1: Add the import**

In `internal/scan/scan.go`, add to the import block (alphabetical among the `internal/...` imports, after `netpolicy`):

```go
	"github.com/imantaba/kubeagent/internal/nodereserve"
```

- [ ] **Step 2: Add the field to `Result`**

In the `Result` struct, add a field after `Nodes`:

```go
type Result struct {
	Inputs        inventory.Inputs
	Nodes         []corev1.Node
	NodeReserve   nodereserve.Report
	Health        clusterhealth.ClusterHealth
	Inventory     inventory.Result
	ServiceIssues []svchealth.Issue
}
```

- [ ] **Step 3: Populate it in `Evaluate`**

Change the final return (currently `scan.go:86`) to include the assessment computed from the already-collected `nodes`:

```go
	return Result{Inputs: inputs, Nodes: nodes, NodeReserve: nodereserve.Assess(nodes), Health: health, Inventory: result, ServiceIssues: serviceIssues}, nil
```

- [ ] **Step 4: Verify the build**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/scan/`
Expected: build succeeds; scan tests PASS (unchanged behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go
git commit -m "feat(scan): carry node reservation report in Result"
```

---

### Task 3: Report — text section, JSON field, caller updates

**Files:**
- Modify: `internal/report/report.go` (import, `inventoryReport` ~19-27, `PrintInventory` ~30, `printInventoryText` ~43, add `printNodeReservations`)
- Modify: `main.go:143` (pass `&res.NodeReserve`)
- Modify: `internal/report/report_test.go` (insert `nil` at the new arg position in every existing `PrintInventory(...)` call; add two new tests)

**Interfaces:**
- Consumes: `nodereserve.Report` / `nodereserve.NodeReservation` from Task 1; `scan.Result.NodeReserve` from Task 2.
- Produces: new `PrintInventory` signature with a `nodeReserve *nodereserve.Report` parameter inserted before `explanation`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go` (the package already imports what the helpers need; add `"github.com/imantaba/kubeagent/internal/nodereserve"` to its imports):

```go
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
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, nil, rep, "", "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Node reservations:") {
		t.Errorf("missing section header in:\n%s", out)
	}
	if !strings.Contains(out, "w1") || !strings.Contains(out, "WARNING") {
		t.Errorf("missing warning row in:\n%s", out)
	}
	if !strings.Contains(out, "w2") || !strings.Contains(out, "cpu=200m mem=800Mi") {
		t.Errorf("missing ok row in:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesNodeReserve(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{WarnCount: 1, Nodes: []nodereserve.NodeReservation{
		{Name: "w1", CPUReserved: "0", MemReserved: "0", Warning: true},
	}}
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, nil, rep, "", "json", &buf); err != nil {
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
```

Note: `report_test.go` already imports `bytes`, `encoding/json`, `strings`, `clusterhealth`, and `inventory` (used by existing tests). Only add the `nodereserve` import.

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run 'NodeReserv|NodeReservations'`
Expected: FAIL to compile — `too many arguments in call to PrintInventory` and `undefined: nodereserve` until wired.

- [ ] **Step 3: Update the JSON struct and signatures**

In `internal/report/report.go`, add the import (after `"github.com/imantaba/kubeagent/internal/inventory"`):

```go
	"github.com/imantaba/kubeagent/internal/nodereserve"
```

Add a field to `inventoryReport` (after `CredentialWarnings`):

```go
	NodeReserve        *nodereserve.Report         `json:"nodeReserve,omitempty"`
```

Change `PrintInventory` to accept `nodeReserve *nodereserve.Report` immediately before `explanation`, and thread it into both branches:

```go
func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, credentialWarnings []credlint.Finding, nodeReserve *nodereserve.Report, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Cluster: cluster, Workloads: result.Workloads, Resources: summary, Platform: facts, ServiceIssues: serviceIssues, CredentialWarnings: credentialWarnings, NodeReserve: nodeReserve, Explanation: explanation})
	case "text":
		return printInventoryText(cluster, result, summary, facts, serviceIssues, credentialWarnings, nodeReserve, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}
```

- [ ] **Step 4: Update `printInventoryText` and add the renderer**

Change the `printInventoryText` signature to accept `nodeReserve *nodereserve.Report` before `explanation`:

```go
func printInventoryText(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, credentialWarnings []credlint.Finding, nodeReserve *nodereserve.Report, explanation string, w io.Writer) error {
```

Immediately after the existing `printResources(summary, w)` block (the `if err := printResources(...); err != nil { return err }` at ~75-77), insert:

```go
	if err := printNodeReservations(nodeReserve, w); err != nil {
		return err
	}
```

Add the new renderer function (place it right after `printResLine`):

```go
// printNodeReservations lists each node's observed kubelet reservation and
// flags nodes that reserve no memory. Nothing is printed when the report is
// nil or empty.
func printNodeReservations(rep *nodereserve.Report, w io.Writer) error {
	if rep == nil || len(rep.Nodes) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Node reservations:"); err != nil {
		return err
	}
	for _, n := range rep.Nodes {
		status := "OK"
		if n.Warning {
			status = "⚠ WARNING: kubelet reserves no memory"
		}
		role := ""
		if n.Role != "" {
			role = " " + n.Role
		}
		if _, err := fmt.Fprintf(w, "  %s%s  cpu=%s mem=%s  %s\n", n.Name, role, n.CPUReserved, n.MemReserved, status); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}
```

- [ ] **Step 5: Update the `main.go` caller**

In `main.go:143`, pass `&res.NodeReserve` in the new position (before `explanation`):

```go
	if err := report.PrintInventory(health, result, &summary, &facts, serviceIssues, credWarnings, &res.NodeReserve, explanation, *output, os.Stdout); err != nil {
```

- [ ] **Step 6: Fix the existing test callers**

`go build ./...` now fails, listing every existing `PrintInventory(...)` call in `internal/report/report_test.go` that passes the old arity. For each such call, insert `nil` in the new parameter position — immediately before the `explanation` argument (the string just before the format string `"text"`/`"json"`). Example:

```go
// before
PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, nil, "", "text", &buf)
// after
PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, nil, nil, "", "text", &buf)
```

Do NOT change the two new tests from Step 1 (they already pass `rep` in that position). After editing, run `go build ./...` and re-fix any remaining callsites the compiler names.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go build ./... && go test ./internal/report/`
Expected: build succeeds; all report tests PASS (existing + two new).

- [ ] **Step 8: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go main.go
git commit -m "feat(report): render node reservations section and nodeReserve JSON field"
```

---

### Task 4: Daemon metric `kubeagent_nodes_without_reservations`

**Files:**
- Modify: `internal/watch/metrics.go` (`metrics` struct ~18-31, `update` ~52-53, `render` gauges ~86-90)
- Modify: `internal/watch/metrics_test.go` (extend `sampleResult` and `TestMetrics_RenderReflectsResult`)

**Interfaces:**
- Consumes: `scan.Result.NodeReserve.WarnCount` from Task 2.
- Produces: gauge line `kubeagent_nodes_without_reservations <n>` in the daemon `/metrics` output.

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, set a warn count on the sample and assert the gauge. Change `sampleResult` to include a `NodeReserve`, and add the expected line to `TestMetrics_RenderReflectsResult`:

```go
func sampleResult() *scan.Result {
	return &scan.Result{
		Health:      clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		NodeReserve: nodereserve.Report{WarnCount: 1},
		Inventory: inventory.Result{Workloads: []inventory.Workload{
			{Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 1,
				Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff"}}},
		}},
	}
}
```

Add `"github.com/imantaba/kubeagent/internal/nodereserve"` to the test's imports, and add to the `want` list in `TestMetrics_RenderReflectsResult`:

```go
		"kubeagent_nodes_without_reservations 1",
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult`
Expected: FAIL — output missing `kubeagent_nodes_without_reservations 1` (and compile error until the field exists).

- [ ] **Step 3: Add the field, update, and gauge**

In `internal/watch/metrics.go`, add a field to the `metrics` struct (after `nodesTotal`):

```go
	nodesNoReserve int
```

In `update`, after `m.nodesTotal = res.Health.NodesTotal`, add:

```go
	m.nodesNoReserve = res.NodeReserve.WarnCount
```

In `render`, after the `kubeagent_nodes_total` gauge line (~88), add:

```go
	gauge("kubeagent_nodes_without_reservations", "Nodes whose kubelet reserves no memory (allocatable == capacity)", float64(m.nodesNoReserve))
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/watch/`
Expected: PASS (the error-path test keeps last-good gauges; the new gauge rides the same snapshot).

- [ ] **Step 5: Commit**

```bash
git add internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): expose kubeagent_nodes_without_reservations gauge"
```

---

### Task 5: Docs — CHANGELOG, website, README

**Files:**
- Modify: `CHANGELOG.md` (`[Unreleased]` section — already present from the Helm change)
- Modify: `website/docs/features/diagnostics.md`
- Modify: `website/docs/features/watch-mode.md`
- Modify: `README.md`

**Interfaces:** none (docs only). Use the exact metric name `kubeagent_nodes_without_reservations` and the section header `Node reservations:`.

- [ ] **Step 1: CHANGELOG**

Under the existing `## [Unreleased]` → `### Added` list in `CHANGELOG.md`, add a bullet:

```markdown
- **Node reservation check.** `scan` now reports each node's aggregate kubelet
  reservation (`Capacity − Allocatable`, i.e. kube-reserved + system-reserved +
  eviction-hard combined) and warns when a node reserves **no memory** —
  a kubelet that can be OOM'd under pressure. Shown as a "Node reservations"
  section (text + JSON `nodeReserve`) and, in the watch daemon, as the gauge
  `kubeagent_nodes_without_reservations`. Read-only; no new RBAC. Advisory only
  (does not change the cluster verdict).
```

- [ ] **Step 2: diagnostics.md**

In `website/docs/features/diagnostics.md`, add a short subsection describing the node reservation check: what it reads (Capacity − Allocatable), the warn rule (memory reserved == 0), that CPU is shown but not warned, and that it needs no extra permissions. Match the surrounding heading level and prose style. Find the current structure first:

Run: `grep -nE '^#|^##|^###' website/docs/features/diagnostics.md | head -30`

Add the subsection after the existing detector/diagnostic descriptions, e.g.:

```markdown
### Node reservations

`scan` shows each node's aggregate kubelet resource reservation, computed as
`Capacity − Allocatable` (the combined effect of `system-reserved`,
`kube-reserved`, and `eviction-hard`). A node is flagged with a **WARNING** when
it reserves no memory (allocatable memory equals capacity) — a kubelet
configuration that lets OS or kubelet memory pressure destabilise the node. CPU
reservation is shown but not warned, since many clusters intentionally leave it
unset. The check reads only the Node objects already listed during a scan, so it
needs no extra permissions, and it is advisory: it never changes the cluster
verdict.
```

- [ ] **Step 3: watch-mode.md**

In `website/docs/features/watch-mode.md`, find the metrics list:

Run: `grep -nE 'kubeagent_|metric' website/docs/features/watch-mode.md | head -30`

Add `kubeagent_nodes_without_reservations` to the documented metrics, in the same format as the neighbouring entries, described as: "Number of nodes whose kubelet reserves no memory (allocatable == capacity)."

- [ ] **Step 4: README**

In `README.md`, find where detectors/checks are listed:

Run: `grep -nE 'RestartLoop|VolumeAttach|OOMKilled|detect|Detect' README.md | head`

Add a one-line mention of the node reservation check alongside the existing detector/feature list, matching the surrounding style (e.g. "node reservation check (warns when a node's kubelet reserves no memory)").

- [ ] **Step 5: Verify the website builds**

Run (venv exists from prior sessions at the scratchpad path; if absent, `python3 -m venv` + `pip install -r website/requirements.txt`):

```bash
cd website && mkdocs build --strict -f mkdocs.yml
```

Expected: "Documentation built" with no strict warnings about the edited pages. (A mkdocs-material 2.0 informational banner is cosmetic.)

- [ ] **Step 6: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md website/docs/features/watch-mode.md README.md
git commit -m "docs: document node reservation check and its daemon metric"
```

---

## Final verification (after all tasks)

Run the full suite and a build:

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Then a manual smoke on any reachable cluster or kind:

```bash
go build -o kubeagent . && ./kubeagent scan --output text | sed -n '/Node reservations:/,/^$/p'
```

Expected: a "Node reservations:" block listing nodes with `cpu=… mem=…` and a WARNING on any node whose kubelet reserves no memory.
