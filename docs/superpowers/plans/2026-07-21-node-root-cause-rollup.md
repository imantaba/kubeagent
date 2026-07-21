# Node-Anchored Root-Cause Rollup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a node is hard-down, attribute the failures of workloads with pods on it to that node — one hedged "↳ likely caused by node X" line per affected workload plus a rollup in the attention line — instead of N disconnected red findings.

**Architecture:** `clusterhealth` exposes a structured list of hard-down nodes (NotReady / kubelet-not-heartbeating), reusing its existing detection. A new pure `internal/rootcause.Annotate` (mirroring `netpolicy`/`rollout`) sets a `Workload.RootCause` string on each flagged workload with a pod on a down node. `scan.Evaluate` wires it after `Prioritize`; `report` renders the attribution line and the attention-line rollup.

**Tech Stack:** Go 1.26, standard library + `k8s.io/api`. No new dependencies, no new API calls.

## Global Constraints

- **READ-ONLY; NO new RBAC / no new collector** — nodes, leases, and pods are already collected.
- **Pure & deterministic** — `rootcause.Annotate` and the `clusterhealth` addition are pure; down nodes are sorted for a stable pick.
- **Always-on** — no flag, no `watch.Config` change; runs in `scan` and the `watch` daemon via `scan.Evaluate`.
- `internal/collect`, `internal/cluster`, `internal/watch`, `explain.go`, RBAC, and Helm stay **unchanged**. `clusterhealth`'s verdict and existing `NodeIssues` stay **unchanged** (the addition is additive). `Assemble`/`Prioritize`/`Flagged` stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** (or any AI attribution) on any commit. **TDD.**
- Set PATH first every task: `export PATH=$PATH:/usr/local/go/bin`.
- Spec: [docs/superpowers/specs/2026-07-21-node-root-cause-rollup-design.md](../specs/2026-07-21-node-root-cause-rollup-design.md).

---

## File Structure

- **Modify** `internal/clusterhealth/clusterhealth.go` (+ test) — `DownNode` type, `DownNodes` field, two appends in `Assess`.
- **Modify** `internal/inventory/inventory.go` — `Workload.RootCause` field.
- **Create** `internal/rootcause/rootcause.go` (+ test) — `Annotate`.
- **Modify** `internal/scan/scan.go` (+ test) — the `rootcause.Annotate` wiring.
- **Modify** `internal/report/report.go` (+ test) — `printWorkload` line + `attentionLine` rollup.
- **Modify** `internal/report/golden_test.go` + `internal/report/testdata/golden-scan.txt` — fixture attribution + snapshot.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.

---

### Task 1: `clusterhealth` — expose hard-down nodes

**Files:**
- Modify: `internal/clusterhealth/clusterhealth.go`
- Test: `internal/clusterhealth/clusterhealth_test.go`

**Interfaces:**
- Produces: `type DownNode struct { Name, Reason string }`; `ClusterHealth.DownNodes []DownNode`. Populated only for NotReady and stale-heartbeat nodes.

- [ ] **Step 1: Write the failing tests**

Add to `internal/clusterhealth/clusterhealth_test.go` (reuse the existing `node`, `notReadyNode`, `hbReadyNode`, `hbLease` helpers):

```go
func TestAssess_DownNodesNotReadyAndStale(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	nodes := []corev1.Node{
		notReadyNode("worker-2", "KubeletNotReady", "runtime down"),
		hbReadyNode("worker-1"),        // Ready, but lease will be stale
		node("worker-3", true, nil, true), // Ready + cordoned — NOT hard-down
		node("worker-4", true, []corev1.NodeConditionType{corev1.NodeMemoryPressure}, false), // Ready + pressure — NOT hard-down
	}
	hb := Heartbeat{Leases: []coordinationv1.Lease{hbLease("worker-1", now.Add(-90*time.Second))}, Now: now, Threshold: 40 * time.Second}
	ch := Assess(nodes, hb, nil, nil)

	got := map[string]string{}
	for _, d := range ch.DownNodes {
		got[d.Name] = d.Reason
	}
	if got["worker-2"] != "NotReady" {
		t.Errorf("worker-2 reason = %q, want NotReady", got["worker-2"])
	}
	if got["worker-1"] != "kubelet not heartbeating" {
		t.Errorf("worker-1 reason = %q, want kubelet not heartbeating", got["worker-1"])
	}
	if _, ok := got["worker-3"]; ok {
		t.Error("a cordoned-but-Ready node must not be a DownNode")
	}
	if _, ok := got["worker-4"]; ok {
		t.Error("a pressured-but-Ready node must not be a DownNode")
	}
	if len(ch.DownNodes) != 2 {
		t.Errorf("want exactly 2 down nodes, got %d: %+v", len(ch.DownNodes), ch.DownNodes)
	}
}

func TestAssess_NoDownNodesWhenHealthy(t *testing.T) {
	ch := Assess([]corev1.Node{node("a", true, nil, false)}, Heartbeat{}, nil, nil)
	if len(ch.DownNodes) != 0 {
		t.Errorf("healthy cluster must have no down nodes, got %+v", ch.DownNodes)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/clusterhealth -run TestAssess_DownNodes`
Expected: FAIL — build error (`DownNodes`/`DownNode` undefined).

- [ ] **Step 3: Implement**

In `internal/clusterhealth/clusterhealth.go`, add the type after `ClusterHealth`:

```go
// DownNode is a node that is effectively down — NotReady, or Ready but its
// kubelet has stopped heartbeating. Used to attribute workload failures to their
// node (internal/rootcause).
type DownNode struct {
	Name   string `json:"name"`
	Reason string `json:"reason"` // "NotReady" | "kubelet not heartbeating"
}
```

Add the field to `ClusterHealth` (after `SystemIssues`):

```go
	DownNodes []DownNode `json:"downNodes,omitempty"`
```

In `Assess`, inside the `for _, n := range nodes` loop, right after `ready, issues := nodeHealth(n)`:

```go
		if !ready {
			ch.DownNodes = append(ch.DownNodes, DownNode{Name: n.Name, Reason: "NotReady"})
		}
```

And inside the existing stale-heartbeat branch, alongside `ch.NodesStaleHeartbeat++`:

```go
			if iss, stale := staleHeartbeat(leaseByNode, n.Name, hb.Now, hb.Threshold); stale {
				issues = append(issues, iss)
				ch.NodesStaleHeartbeat++
				ch.DownNodes = append(ch.DownNodes, DownNode{Name: n.Name, Reason: "kubelet not heartbeating"})
			}
```

Nothing else changes — the verdict and `NodeIssues` are untouched.

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/clusterhealth`
Expected: PASS (new tests + all existing clusterhealth tests).

- [ ] **Step 5: Commit**

```bash
git add internal/clusterhealth/clusterhealth.go internal/clusterhealth/clusterhealth_test.go
git commit -m "feat(clusterhealth): expose hard-down nodes for root-cause attribution"
```

---

### Task 2: `inventory.Workload.RootCause` + `internal/rootcause`

**Files:**
- Modify: `internal/inventory/inventory.go`
- Create: `internal/rootcause/rootcause.go`
- Test: `internal/rootcause/rootcause_test.go`

**Interfaces:**
- Consumes: `inventory.Workload` (`Pods[].Node`, `Flagged()`), `clusterhealth.DownNode` (Task 1).
- Produces: `Workload.RootCause string`; `func Annotate(workloads []inventory.Workload, down []clusterhealth.DownNode)`.

- [ ] **Step 1: Add the `RootCause` field**

In `internal/inventory/inventory.go`, add to the `Workload` struct (after the `Rollout` field, keeping the annotator-set fields together):

```go
	RootCause       string             `json:"rootCause,omitempty"`       // "node X (reason)" — root-cause attribution (hint; set by rootcause.Annotate)
```

Verify it compiles: `export PATH=$PATH:/usr/local/go/bin && go build ./internal/inventory`.

- [ ] **Step 2: Write the failing tests**

Create `internal/rootcause/rootcause_test.go`:

```go
package rootcause

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func wl(ns, name string, ready, desired int, nodes ...string) inventory.Workload {
	w := inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment", Ready: ready, Desired: desired, Status: "Degraded"}
	for i, n := range nodes {
		w.Pods = append(w.Pods, inventory.PodRow{Name: name + "-" + string(rune('a'+i)), Node: n})
	}
	return w
}

func TestAnnotate_AttributesPodOnNotReadyNode(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-2")}
	down := []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}}
	Annotate(ws, down)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Errorf("RootCause = %q, want node worker-2 (NotReady)", ws[0].RootCause)
	}
}

func TestAnnotate_StaleHeartbeatReason(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "web", 0, 1, "worker-1")}
	down := []clusterhealth.DownNode{{Name: "worker-1", Reason: "kubelet not heartbeating"}}
	Annotate(ws, down)
	if ws[0].RootCause != "node worker-1 (kubelet not heartbeating)" {
		t.Errorf("RootCause = %q", ws[0].RootCause)
	}
}

func TestAnnotate_HealthyNodeNoAttribution(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-9")} // not in down
	Annotate(ws, []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}})
	if ws[0].RootCause != "" {
		t.Errorf("workload on a healthy node must not be attributed, got %q", ws[0].RootCause)
	}
}

func TestAnnotate_NotFlaggedSkipped(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 2, 2, "worker-2")} // Ready==Desired, not flagged
	ws[0].Status = "Running"
	Annotate(ws, []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}})
	if ws[0].RootCause != "" {
		t.Errorf("a non-flagged workload must be skipped, got %q", ws[0].RootCause)
	}
}

func TestAnnotate_DeterministicPickSortedByNode(t *testing.T) {
	// Pods on two down nodes; the sorted-first node name wins.
	ws := []inventory.Workload{wl("shop", "api", 0, 3, "worker-5", "worker-2")}
	down := []clusterhealth.DownNode{{Name: "worker-5", Reason: "NotReady"}, {Name: "worker-2", Reason: "NotReady"}}
	Annotate(ws, down)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Errorf("want the sorted-first down node (worker-2), got %q", ws[0].RootCause)
	}
}

func TestAnnotate_EmptyDownNoop(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-2")}
	Annotate(ws, nil)
	if ws[0].RootCause != "" {
		t.Errorf("no down nodes => no attribution, got %q", ws[0].RootCause)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/rootcause`
Expected: FAIL — build error (`Annotate` undefined).

- [ ] **Step 4: Implement**

Create `internal/rootcause/rootcause.go`:

```go
// Package rootcause attributes a flagged workload's failure to a hard-down node
// (NotReady or kubelet-not-heartbeating) when the workload has a pod placed on
// that node — collapsing many disconnected findings toward one root cause. Pure
// and read-only; the caller supplies the workloads and the down-node list.
// Mirrors netpolicy/rollout.Annotate.
package rootcause

import (
	"sort"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate sets w.RootCause on each flagged workload that has a pod on a hard-down
// node. It mutates the slice elements in place. When several down nodes host the
// workload's pods, the node whose name sorts first is chosen (deterministic).
func Annotate(workloads []inventory.Workload, down []clusterhealth.DownNode) {
	if len(down) == 0 {
		return
	}
	reasonByNode := make(map[string]string, len(down))
	names := make([]string, 0, len(down))
	for _, d := range down {
		if _, seen := reasonByNode[d.Name]; !seen {
			names = append(names, d.Name)
		}
		reasonByNode[d.Name] = d.Reason
	}
	sort.Strings(names)
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() {
			continue
		}
		on := podNodes(*w)
		for _, name := range names {
			if on[name] {
				workloads[i].RootCause = "node " + name + " (" + reasonByNode[name] + ")"
				break
			}
		}
	}
}

// podNodes is the set of nodes this workload's pods are placed on.
func podNodes(w inventory.Workload) map[string]bool {
	on := make(map[string]bool, len(w.Pods))
	for _, p := range w.Pods {
		if p.Node != "" {
			on[p.Node] = true
		}
	}
	return on
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/rootcause && go vet ./internal/rootcause`
Expected: PASS, vet clean.

- [ ] **Step 6: Commit**

```bash
git add internal/inventory/inventory.go internal/rootcause/rootcause.go internal/rootcause/rootcause_test.go
git commit -m "feat(rootcause): attribute a flagged workload to its hard-down node"
```

---

### Task 3: Wire `rootcause.Annotate` into `scan.Evaluate`

**Files:**
- Modify: `internal/scan/scan.go`
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `rootcause.Annotate` (Task 2), the existing `health` local (`clusterhealth.Assess(...)`, scan.go:151) and its `DownNodes`, `result.Workloads`.
- Produces: no signature change to `Evaluate`; `result.Inventory.Workloads[i].RootCause` is now populated.

- [ ] **Step 1: Write the failing integration test**

Add to `internal/scan/scan_test.go` (the file already imports `appsv1`, `corev1`, `metav1`, `fake`, and has `p32`):

```go
func TestEvaluate_AttributesRootCauseToNotReadyNode(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady"}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-1", Labels: map[string]string{"app": "api"}},
		Spec:   corev1.PodSpec{NodeName: "worker-2"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "api", Ready: false}}}}
	cli := fake.NewSimpleClientset(node, dep, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		if w.RootCause == "node worker-2 (NotReady)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a workload attributed to node worker-2, got %+v", res.Inventory.Workloads)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan -run TestEvaluate_AttributesRootCause`
Expected: FAIL — no `RootCause` set (wiring absent).

- [ ] **Step 3: Add the import + wiring**

In `internal/scan/scan.go`, add the import (alphabetical among the `internal/...` group — after `rollout` / before `svchealth`, wherever it sorts):

```go
	"github.com/imantaba/kubeagent/internal/rootcause"
```

After the existing annotator block (the `rollout.Annotate(...)` line, scan.go:190), add:

```go
	rootcause.Annotate(result.Workloads, health.DownNodes)
```

(`health` is the `clusterhealth.Assess(...)` result from scan.go:151; it is in scope here.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/scan`
Expected: PASS (new test + existing `Evaluate` tests).

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): run node root-cause attribution after Prioritize"
```

---

### Task 4: `report` — render the attribution + rollup

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `inventory.Workload.RootCause` (Task 2).
- Produces: the `↳ likely caused by …` workload line and the `(M ⇐ node X)` / `(M ⇐ K unhealthy nodes)` attention-line rollup. No signature changes (both functions already take what they need).

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go`:

```go
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
	if !strings.Contains(buf.String(), "(2 ⇐ 2 unhealthy nodes)") {
		t.Errorf("attention line should report 2 unhealthy nodes:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory_RootCause`
Expected: FAIL — the line and rollup are not rendered yet.

- [ ] **Step 3: Implement the workload line**

In `internal/report/report.go`, `printWorkload`, immediately AFTER the header is printed (the `fmt.Fprintln(w, header)` block) and BEFORE the `if wl.Image != ""` block:

```go
	if wl.RootCause != "" {
		if _, err := fmt.Fprintf(w, "    ↳ likely caused by %s\n", wl.RootCause); err != nil {
			return err
		}
	}
```

- [ ] **Step 4: Implement the attention-line rollup**

In `internal/report/report.go`, `attentionLine`, extend the flagged-workload loop and the "failing" clause. Replace the existing:

```go
	failing := 0
	for _, wl := range in.Result.Workloads {
		if wl.Flagged() {
			failing++
		}
	}
	var parts []string
	if failing > 0 {
		parts = append(parts, fmt.Sprintf("%d %s failing", failing, plural(failing, "workload", "workloads")))
	}
```

with:

```go
	failing := 0
	attributed := 0
	var causeNodes []string
	seenNode := map[string]bool{}
	for _, wl := range in.Result.Workloads {
		if wl.Flagged() {
			failing++
		}
		if wl.RootCause != "" {
			attributed++
			n := rootCauseNode(wl.RootCause)
			if !seenNode[n] {
				seenNode[n] = true
				causeNodes = append(causeNodes, n)
			}
		}
	}
	var parts []string
	if failing > 0 {
		s := fmt.Sprintf("%d %s failing", failing, plural(failing, "workload", "workloads"))
		if attributed > 0 {
			if len(causeNodes) == 1 {
				s += fmt.Sprintf(" (%d ⇐ %s)", attributed, causeNodes[0])
			} else {
				s += fmt.Sprintf(" (%d ⇐ %d unhealthy nodes)", attributed, len(causeNodes))
			}
		}
		parts = append(parts, s)
	}
```

Add the helper near `attentionLine`:

```go
// rootCauseNode extracts the "node X" prefix from a RootCause string of the fixed
// form "node X (reason)" for the attention-line rollup.
func rootCauseNode(rc string) string {
	return strings.SplitN(rc, " (", 2)[0]
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory`
Expected: PASS (new tests + existing `TestPrintInventory_*`). `TestGoldenScanOutput` may now fail — that is Task 5.

- [ ] **Step 6: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render node root-cause attribution and rollup"
```

---

### Task 5: Golden fixture + snapshot

**Files:**
- Modify: `internal/report/golden_test.go`
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated)

**Interfaces:** none — exercises the Task 4 rendering through the real text renderer.

Context: the golden fixture's `NodeIssues` already carry `worker-2 NotReady` and `worker-1 kubelet not heartbeating`, and its workloads place pods on those nodes — `web` (worker-1), `api` (worker-1), `billing-worker` (worker-2), `data` (worker-2). `cache`/`checkout`/`orders` are on `worker-3` (cordoned — NOT hard-down) and must NOT be attributed. The golden renders a pre-built `Input` (it does not run the annotator), so set `RootCause` directly on those four fixture workloads.

- [ ] **Step 1: Set `RootCause` on the four affected fixture workloads**

In `internal/report/golden_test.go`, `goldenWorkloads()`, add `RootCause` to the matching entries (keep every other field as-is):
- `web` (Deployment, pod on `worker-1`): `RootCause: "node worker-1 (kubelet not heartbeating)"`
- `api` (Deployment, pod on `worker-1`): `RootCause: "node worker-1 (kubelet not heartbeating)"`
- `billing-worker` (Deployment, pod on `worker-2`): `RootCause: "node worker-2 (NotReady)"`
- `data` (StatefulSet, pod on `worker-2`): `RootCause: "node worker-2 (NotReady)"`

Add the field to each struct literal (e.g. on the `web` entry):

```go
		{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Restarts: 8, LastRestart: r, Image: "busybox:1.36", RootCause: "node worker-1 (kubelet not heartbeating)",
			Pods:     []inventory.PodRow{{Name: "web-5b8-2wplt", Phase: "Running", Ready: "0/1", Restarts: 8, LastRestart: r, Node: "worker-1", IP: "10.244.2.2", Age: "20d", Image: "busybox:1.36"}},
			Findings: []diagnose.Finding{{Pod: "shop/web", Issue: "CrashLoopBackOff", Reason: "Container repeatedly crashes after starting", Evidence: `container "web", restartCount=8`, Container: "web", LogExcerpt: "panic: runtime error: invalid memory address", LogCause: "application panic (code bug)"}}},
```

(Apply the analogous one-field addition to `api`, `billing-worker`, and `data`.)

- [ ] **Step 2: Run the golden test to see it fail (snapshot stale)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput`
Expected: FAIL — output now has four `↳ likely caused by …` lines and a rollup on the attention line.

- [ ] **Step 3: Regenerate the snapshot**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput -update`
Expected: PASS (writes `testdata/golden-scan.txt`).

- [ ] **Step 4: Inspect the regenerated snapshot**

Run: `grep -n "likely caused by\|workloads failing" internal/report/testdata/golden-scan.txt`
Expected: (a) the "workloads failing" clause of the attention line now reads `11 workloads failing (4 ⇐ 2 unhealthy nodes)` (the rollup is a parenthetical on that clause, followed by the existing ` · 2 services without endpoints · …`); (b) four `    ↳ likely caused by node worker-1 (kubelet not heartbeating)` / `node worker-2 (NotReady)` lines under `web`/`api`/`billing-worker`/`data`; (c) NO `↳ likely caused by` under `cache`/`checkout`/`orders`. If any worker-3 workload gained an attribution, STOP — the fixture is wrong.

- [ ] **Step 5: Run the full report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report`
Expected: PASS (`TestGoldenScanOutput` + `TestGoldenInputCoversAllSections`).

- [ ] **Step 6: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover node root-cause attribution in the golden snapshot"
```

---

### Task 6: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs only). `go build`/`go test` stay green.

- [ ] **Step 1: Add a diagnostics subsection**

In `website/docs/features/diagnostics.md`, add this subsection immediately after the `### FailedCreate (controller can't create pods)` subsection (the last of the pod-failure detectors, before `### Node reservations`):

```markdown
### Root-cause attribution (node)

When a node is **hard-down** — `NotReady`, or Ready but its kubelet has stopped
heartbeating (a stale `Lease`) — every workload with a pod on it fails at once.
Instead of leaving those as disconnected findings, `scan` attributes each affected
workload to the node with a hedged `↳ likely caused by node <name> (<reason>)`
line, and rolls the count up on the attention line (`3 workloads failing (3 ⇐ node
worker-2)`). The workload's own findings still show — attribution is additive, and
the wording is deliberately "likely" (correlation, not a hard causation claim).
Read-only, always-on, no new RBAC. Cordoned and node-pressure causes are not yet
attributed.
```

- [ ] **Step 2: README bullet**

In `README.md`, in the detector/feature list, add:

```markdown
- **Root-cause attribution** — when a node is NotReady or its kubelet stops
  heartbeating, workloads with pods on it are attributed to that node ("↳ likely
  caused by node X") instead of N disconnected findings.
```

- [ ] **Step 3: CHANGELOG entry**

In `CHANGELOG.md`, under `## [Unreleased]` → `### Added` (create the headers under a fresh `## [Unreleased]` if the last release consumed them):

```markdown
- **Node-anchored root-cause attribution.** When a node is hard-down (NotReady, or
  its kubelet stops heartbeating), `scan` attributes each workload with a pod on it
  to that node — a hedged "↳ likely caused by node X (reason)" line plus a rollup
  on the attention line — collapsing a wall of disconnected findings toward the one
  real cause. Additive (the workload's own findings still show), read-only,
  always-on, no new RBAC. The first step of the root-cause correlation roadmap.
```

- [ ] **Step 4: Roadmap**

In `website/docs/roadmap.md`, add a bullet to the **Shipped** list (mirroring the existing entries' style):

```markdown
- **Node-anchored root-cause attribution** — a hard-down node (NotReady or
  kubelet-not-heartbeating) becomes the named root cause of the workloads with pods
  on it, instead of N disconnected findings; the first slice of the root-cause
  correlation theme. See [Failure diagnostics](features/diagnostics.md).
```

- [ ] **Step 5: Verify docs build (if mkdocs available) + full suite**

Run: `cd website && mkdocs build --strict -f mkdocs.yml 2>&1 | tail -3; cd ..` (skip with a note if mkdocs is not installed — convenience check, not a gate).
Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all Go packages PASS.

- [ ] **Step 6: Commit**

```bash
git add website/docs/features/diagnostics.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "docs: document node-anchored root-cause attribution"
```

---

## Notes for the executor

- **Release gate (post-merge, not part of these tasks):** touches `clusterhealth`, `inventory`, `rootcause`, `scan`, `report` — **not** `internal/collect`/`cluster`/`watch`/RBAC/Helm — so a **lightweight real-cluster smoke** confirms rendering; the full chaos gate is not required. Version bump is a **minor** (v0.28.2 → **v0.29.0**).
- **No new RBAC / collector:** nodes, leases, and pods are already collected; do not touch `deploy/` or Helm.
- **Deterministic attribution:** when a workload has pods on more than one down node, the node whose name sorts first wins — keep the `sort.Strings(names)` in `rootcause.Annotate`.
- **Honesty:** the wording is "likely caused by" on purpose; do not strengthen it to a definitive causation claim.
