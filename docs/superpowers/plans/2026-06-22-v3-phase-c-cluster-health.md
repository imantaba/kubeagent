# kubeagent v3 — Phase C Plan: first-line cluster-health verdict

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lead the `scan` report with a one-line cluster-health verdict derived from node health (Ready / Memory·Disk·PID pressure / cordon) plus a `kube-system` workload rollup; surface it in text and in the JSON `cluster` key, and have `--explain` lead with it when the cluster is degraded.

**Architecture:** A new pure `internal/clusterhealth` package exposes `Assess(nodes, workloads) ClusterHealth`. `collect.Nodes` lists cluster-scoped nodes. `report.PrintInventory` and `explain.ExplainInventory` each gain a `ClusterHealth` parameter; `main` lists nodes, assesses, and threads the verdict through.

**Tech Stack:** Go 1.26, `k8s.io/client-go` (CoreV1 nodes + fake clientset), existing `inventory`/`report`/`explain`. No new dependency.

## Global Constraints

- Module path: `github.com/imantaba/kubeagent`; Go 1.26.
- **Read-only** (List only); **sequential** (no goroutines); stdlib `flag`; no new dependency.
- **Node unhealthy** when: `Ready` condition is not `True`; `MemoryPressure`/`DiskPressure`/`PIDPressure` is `True`; or `Spec.Unschedulable` (cordoned).
- **System rollup:** `kube-system` workloads (from the already-assembled inventory) that are `Flagged()`. CNI lives in `kube-system` on most distros; other namespaces are out of scope.
- **Verdict:** `Healthy` only when there are no node issues and no system issues; otherwise `Degraded`.
- **Scope note:** nodes are cluster-scoped and always listed regardless of `-n`; the system rollup only sees `kube-system` workloads when they're in scope (i.e. all-namespaces or `-n kube-system`).
- **Egress:** the `--explain` prompt may include node names in the cluster section (infrastructure identifiers, not secrets) but still must NOT include per-pod IPs/node names from `PodRow`.
- **JSON shape:** `scan` JSON becomes `{"cluster": {…}, "workloads": [...]}` (+ `"explanation"` when present).
- Each task keeps `go build ./...` and `go test ./...` green.

---

## File Structure

- `internal/clusterhealth/clusterhealth.go` — **new.** `ClusterHealth` type + `Assess` + `nodeHealth` helper.
- `internal/clusterhealth/clusterhealth_test.go` — **new.** `nodeHealth` + `Assess` unit tests (fake nodes + workloads).
- `internal/collect/collect.go` — **modify.** Add `Nodes(ctx, client) ([]corev1.Node, error)`.
- `internal/collect/collect_test.go` — **modify.** Fake-clientset test for `Nodes`.
- `internal/report/report.go` — **modify.** `PrintInventory` gains a `clusterhealth.ClusterHealth` first parameter; text prints the verdict first; JSON gains a `cluster` key.
- `internal/report/report_test.go` — **modify.** Update existing calls; add cluster-line + cluster-JSON tests.
- `internal/explain/explain.go` — **modify.** `ExplainInventory` gains a `clusterhealth.ClusterHealth` parameter; `buildInventoryPrompt` leads with the verdict when degraded; skip logic updated.
- `internal/explain/explain_test.go` — **modify.** Update existing calls; add cluster-degraded cases.
- `main.go` / `main_test.go` — **modify (Task 5).** List nodes, assess, thread the verdict.
- `README.md` — **modify (Task 5).** Mention the first-line verdict.

---

## Task 1: `clusterhealth` — Assess

**Files:**
- Create: `internal/clusterhealth/clusterhealth.go`
- Test: `internal/clusterhealth/clusterhealth_test.go`

**Interfaces:**
- Produces: `type ClusterHealth struct { Verdict string; NodesTotal, NodesReady int; NodeIssues, SystemIssues []string }` (with JSON tags); `func Assess(nodes []corev1.Node, workloads []inventory.Workload) ClusterHealth`.
- Consumes: `inventory.Workload` (`Namespace`, `Name`, `Ready`, `Desired`, `Status`, `Flagged()`).

- [ ] **Step 1: Write the failing tests**

Create `internal/clusterhealth/clusterhealth_test.go`:

```go
package clusterhealth

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// node builds a Node with the given Ready status, optional pressure conditions,
// and cordon flag.
func node(name string, ready bool, pressures []corev1.NodeConditionType, cordoned bool) corev1.Node {
	conds := []corev1.NodeCondition{{Type: corev1.NodeReady, Status: condStatus(ready)}}
	for _, p := range pressures {
		conds = append(conds, corev1.NodeCondition{Type: p, Status: corev1.ConditionTrue})
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status:     corev1.NodeStatus{Conditions: conds},
	}
}

func condStatus(b bool) corev1.ConditionStatus {
	if b {
		return corev1.ConditionTrue
	}
	return corev1.ConditionFalse
}

func TestNodeHealth(t *testing.T) {
	ready, issues := nodeHealth(node("n1", true, nil, false))
	if !ready || len(issues) != 0 {
		t.Errorf("healthy node: ready=%v issues=%v", ready, issues)
	}

	ready, issues = nodeHealth(node("n2", false, nil, false))
	if ready {
		t.Error("not-ready node reported ready")
	}
	if len(issues) != 1 || issues[0] != "NotReady" {
		t.Errorf("expected [NotReady], got %v", issues)
	}

	// Ready but under disk pressure: counts as ready, but is an issue.
	ready, issues = nodeHealth(node("n3", true, []corev1.NodeConditionType{corev1.NodeDiskPressure}, false))
	if !ready {
		t.Error("pressured-but-ready node should still be ready")
	}
	if len(issues) != 1 || issues[0] != "DiskPressure" {
		t.Errorf("expected [DiskPressure], got %v", issues)
	}

	ready, issues = nodeHealth(node("n4", true, nil, true))
	if len(issues) != 1 || issues[0] != "SchedulingDisabled" {
		t.Errorf("expected [SchedulingDisabled], got %v", issues)
	}
}

func TestAssess_HealthyClusterAndSystem(t *testing.T) {
	nodes := []corev1.Node{node("a", true, nil, false), node("b", true, nil, false)}
	workloads := []inventory.Workload{
		{Namespace: "kube-system", Name: "coredns", Ready: 2, Desired: 2, Status: "Running"},
		{Namespace: "default", Name: "web", Ready: 1, Desired: 2, Status: "Degraded"}, // not kube-system → ignored
	}
	ch := Assess(nodes, workloads)
	if ch.Verdict != "Healthy" {
		t.Errorf("verdict = %q, want Healthy", ch.Verdict)
	}
	if ch.NodesTotal != 2 || ch.NodesReady != 2 {
		t.Errorf("nodes = %d/%d, want 2/2", ch.NodesReady, ch.NodesTotal)
	}
	if len(ch.NodeIssues) != 0 || len(ch.SystemIssues) != 0 {
		t.Errorf("expected no issues, got node=%v system=%v", ch.NodeIssues, ch.SystemIssues)
	}
}

func TestAssess_DegradedByNodeAndSystem(t *testing.T) {
	nodes := []corev1.Node{
		node("a", true, nil, false),
		node("b", false, nil, false), // NotReady
	}
	workloads := []inventory.Workload{
		{Namespace: "kube-system", Name: "coredns", Ready: 1, Desired: 2, Status: "Degraded"},
	}
	ch := Assess(nodes, workloads)
	if ch.Verdict != "Degraded" {
		t.Errorf("verdict = %q, want Degraded", ch.Verdict)
	}
	if ch.NodesReady != 1 || ch.NodesTotal != 2 {
		t.Errorf("nodes = %d/%d, want 1/2", ch.NodesReady, ch.NodesTotal)
	}
	if len(ch.NodeIssues) != 1 || ch.NodeIssues[0] != "b NotReady" {
		t.Errorf("node issues = %v, want [b NotReady]", ch.NodeIssues)
	}
	if len(ch.SystemIssues) != 1 || ch.SystemIssues[0] != "kube-system/coredns 1/2 Degraded" {
		t.Errorf("system issues = %v", ch.SystemIssues)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/clusterhealth/ 2>&1 | tail -8
```
Expected: FAIL — undefined `nodeHealth` / `Assess` / `ClusterHealth`.

- [ ] **Step 3: Write the implementation**

Create `internal/clusterhealth/clusterhealth.go`:

```go
// Package clusterhealth derives a one-line cluster verdict from node health
// and a kube-system workload rollup.
package clusterhealth

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

const systemNamespace = "kube-system"

// ClusterHealth is the first-line cluster verdict.
type ClusterHealth struct {
	Verdict      string   `json:"verdict"` // Healthy | Degraded
	NodesTotal   int      `json:"nodesTotal"`
	NodesReady   int      `json:"nodesReady"`
	NodeIssues   []string `json:"nodeIssues,omitempty"`
	SystemIssues []string `json:"systemIssues,omitempty"`
}

// Assess computes the verdict from nodes and the assembled workloads. A node is
// unhealthy if not Ready, under Memory/Disk/PID pressure, or cordoned. System
// issues are flagged kube-system workloads. The verdict is Healthy only when
// there are no node and no system issues.
func Assess(nodes []corev1.Node, workloads []inventory.Workload) ClusterHealth {
	ch := ClusterHealth{NodesTotal: len(nodes)}
	for _, n := range nodes {
		ready, issues := nodeHealth(n)
		if ready {
			ch.NodesReady++
		}
		for _, iss := range issues {
			ch.NodeIssues = append(ch.NodeIssues, n.Name+" "+iss)
		}
	}
	for _, w := range workloads {
		if w.Namespace == systemNamespace && w.Flagged() {
			ch.SystemIssues = append(ch.SystemIssues,
				fmt.Sprintf("%s/%s %d/%d %s", w.Namespace, w.Name, w.Ready, w.Desired, w.Status))
		}
	}
	if len(ch.NodeIssues) == 0 && len(ch.SystemIssues) == 0 {
		ch.Verdict = "Healthy"
	} else {
		ch.Verdict = "Degraded"
	}
	return ch
}

// nodeHealth returns whether the node's Ready condition is true and a list of
// its problems ("NotReady", pressure types, "SchedulingDisabled").
func nodeHealth(n corev1.Node) (ready bool, issues []string) {
	for _, c := range n.Status.Conditions {
		switch c.Type {
		case corev1.NodeReady:
			ready = c.Status == corev1.ConditionTrue
		case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
			if c.Status == corev1.ConditionTrue {
				issues = append(issues, string(c.Type))
			}
		}
	}
	if !ready {
		issues = append(issues, "NotReady")
	}
	if n.Spec.Unschedulable {
		issues = append(issues, "SchedulingDisabled")
	}
	return ready, issues
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/clusterhealth/ -v 2>&1 | tail -20
go vet ./internal/clusterhealth/
```
Expected: PASS — all tests; vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/clusterhealth/
git commit -m "feat(clusterhealth): node + kube-system verdict (Assess)" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: `collect.Nodes`

**Files:**
- Modify: `internal/collect/collect.go`
- Modify: `internal/collect/collect_test.go`

**Interfaces:**
- Produces: `func Nodes(ctx context.Context, client kubernetes.Interface) ([]corev1.Node, error)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go`:

```go
func TestNodes_ListsAllNodes(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}},
	)
	nodes, err := Nodes(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -run TestNodes 2>&1 | tail -8
```
Expected: FAIL — undefined `Nodes`.

- [ ] **Step 3: Implement in `internal/collect/collect.go`**

Add (nodes are cluster-scoped — no namespace argument):

```go
// Nodes lists all cluster nodes (read-only). Nodes are cluster-scoped, so this
// is not affected by the scan's namespace filter.
func Nodes(ctx context.Context, client kubernetes.Interface) ([]corev1.Node, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	return nodes.Items, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/collect/ -v 2>&1 | tail -15
go vet ./internal/collect/
go build ./...
```
Expected: PASS — new + existing collect tests; vet clean; module builds.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): list cluster nodes" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: `report` — print the cluster verdict first + JSON `cluster` key

**Files:**
- Modify: `internal/report/report.go`
- Modify: `internal/report/report_test.go`
- Modify: `main.go` (the one `PrintInventory` call — stub the cluster arg to keep the build green)

**Interfaces:**
- Produces: `func PrintInventory(cluster clusterhealth.ClusterHealth, workloads []inventory.Workload, explanation, format string, w io.Writer) error`.
- text: when `cluster.Verdict != ""`, print `Cluster: <Verdict> — <ready>/<total> nodes Ready`, then one `  ⚠ node <issue>` / `  ⚠ system <issue>` line per issue, then a blank line, then the inventory. json: `{"cluster": {…}, "workloads": [...], "explanation"?: "..."}`.

- [ ] **Step 1: Update the failing tests**

In `internal/report/report_test.go`, add the import `"github.com/imantaba/kubeagent/internal/clusterhealth"`. Update every existing `PrintInventory(...)` call to pass a zero cluster first arg (`clusterhealth.ClusterHealth{}` → no cluster line, existing assertions unaffected). For example `PrintInventory(sampleWorkloads(), "", "text", &buf)` becomes `PrintInventory(clusterhealth.ClusterHealth{}, sampleWorkloads(), "", "text", &buf)`. Then add:

```go
func sampleCluster() clusterhealth.ClusterHealth {
	return clusterhealth.ClusterHealth{
		Verdict: "Degraded", NodesTotal: 3, NodesReady: 2,
		NodeIssues:   []string{"nova-worker-2 NotReady"},
		SystemIssues: []string{"kube-system/coredns 1/2 Degraded"},
	}
}

func TestPrintInventory_TextLeadsWithClusterVerdict(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleCluster(), sampleWorkloads(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Cluster: Degraded", "2/3 nodes Ready", "nova-worker-2 NotReady", "kube-system/coredns 1/2 Degraded"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
	// The verdict must come before the workload inventory.
	if strings.Index(out, "Cluster: Degraded") > strings.Index(out, "cattle-system/rancher") {
		t.Error("cluster verdict should be printed before the inventory")
	}
}

func TestPrintInventory_TextHealthyClusterSingleLine(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 3, NodesReady: 3}
	if err := PrintInventory(ch, sampleWorkloads(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Cluster: Healthy — 3/3 nodes Ready") {
		t.Errorf("expected healthy one-liner:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesCluster(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleCluster(), sampleWorkloads(), "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		Cluster   clusterhealth.ClusterHealth `json:"cluster"`
		Workloads []inventory.Workload        `json:"workloads"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not the expected object: %v", err)
	}
	if got.Cluster.Verdict != "Degraded" || got.Cluster.NodesReady != 2 || len(got.Workloads) != 1 {
		t.Errorf("cluster/workloads mismatch: %+v", got)
	}
}
```

- [ ] **Step 2: Run the report tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ 2>&1 | tail -8
```
Expected: FAIL — compile error: `PrintInventory` now called with 5 args but defined with 4.

- [ ] **Step 3: Update `internal/report/report.go`**

Add the import `"github.com/imantaba/kubeagent/internal/clusterhealth"`. Change `inventoryReport` and `PrintInventory`, and add the cluster header to the text path:

```go
type inventoryReport struct {
	Cluster     clusterhealth.ClusterHealth `json:"cluster"`
	Workloads   []inventory.Workload        `json:"workloads"`
	Explanation string                      `json:"explanation,omitempty"`
}

// PrintInventory writes the cluster verdict and grouped workload inventory to w.
func PrintInventory(cluster clusterhealth.ClusterHealth, workloads []inventory.Workload, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Cluster: cluster, Workloads: workloads, Explanation: explanation})
	case "text":
		return printInventoryText(cluster, workloads, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}
```

In `printInventoryText`, change the signature to take `cluster clusterhealth.ClusterHealth` as the first parameter and emit the verdict block at the very top (before the `if len(workloads) == 0` check):

```go
func printInventoryText(cluster clusterhealth.ClusterHealth, workloads []inventory.Workload, explanation string, w io.Writer) error {
	if cluster.Verdict != "" {
		if _, err := fmt.Fprintf(w, "Cluster: %s — %d/%d nodes Ready\n", cluster.Verdict, cluster.NodesReady, cluster.NodesTotal); err != nil {
			return err
		}
		for _, iss := range cluster.NodeIssues {
			if _, err := fmt.Fprintf(w, "  ⚠ node %s\n", iss); err != nil {
				return err
			}
		}
		for _, iss := range cluster.SystemIssues {
			if _, err := fmt.Fprintf(w, "  ⚠ system %s\n", iss); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	// ... the rest of printInventoryText is UNCHANGED (the empty-workloads
	// branch, the per-workload loop, and the explanation block) ...
}
```
(Keep the entire remaining body of `printInventoryText` exactly as it is today, only adding the cluster block above it and the new first parameter.)

- [ ] **Step 4: Keep the build green — update the `main.go` call site**

In `main.go`, change `report.PrintInventory(workloads, explanation, *output, os.Stdout)` to:

```go
	return report.PrintInventory(clusterhealth.ClusterHealth{}, workloads, explanation, *output, os.Stdout)
```
and add the import `"github.com/imantaba/kubeagent/internal/clusterhealth"`. (Task 5 replaces the zero value with the real assessment.)

- [ ] **Step 5: Run the tests + build**

```bash
go build ./...
go test ./internal/report/ -v 2>&1 | tail -25
go vet ./internal/report/
```
Expected: module builds; all report tests pass (updated existing + new cluster tests); vet clean.

- [ ] **Step 6: Commit**

```bash
git add internal/report/ main.go
git commit -m "feat(report): lead with cluster verdict; JSON cluster key" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: `explain` — lead the prompt with the verdict when degraded

**Files:**
- Modify: `internal/explain/explain.go`
- Modify: `internal/explain/explain_test.go`
- Modify: `main.go` (the one `ExplainInventory` call — stub the cluster arg)

**Interfaces:**
- Produces: `func (c *Client) ExplainInventory(ctx context.Context, cluster clusterhealth.ClusterHealth, workloads []inventory.Workload) (string, error)`; `buildInventoryPrompt(cluster clusterhealth.ClusterHealth, workloads []inventory.Workload) string`.
- Skip (return `"", nil`, no API call) only when `cluster.Verdict != "Degraded"` AND `Notable(workloads)` is empty.

- [ ] **Step 1: Update the failing tests**

In `internal/explain/explain_test.go`, add the import `"github.com/imantaba/kubeagent/internal/clusterhealth"`. Update the existing `ExplainInventory` calls to pass a cluster arg: `TestExplainInventory_SkipsWhenNothingNotable` passes `clusterhealth.ClusterHealth{Verdict: "Healthy"}`; `TestExplainInventory_SummarizesNotable`, `TestExplainInventory_WrapsError`, `TestExplainInventory_ErrorsOnEmptyText` pass `clusterhealth.ClusterHealth{Verdict: "Healthy"}` (they rely on a notable workload to trigger the call). Update `TestBuildInventoryPrompt_OnlyStructuredFields` to call `buildInventoryPrompt(clusterhealth.ClusterHealth{}, ws)`. Then add:

```go
func TestExplainInventory_ExplainsDegradedClusterWithNoNotableWorkloads(t *testing.T) {
	f := &fakeSummarizer{reply: "two nodes are NotReady"}
	c := &Client{s: f}
	ch := clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 3, NodesReady: 1, NodeIssues: []string{"n2 NotReady", "n3 NotReady"}}
	got, err := c.ExplainInventory(context.Background(), ch, []inventory.Workload{{Name: "ok", Ready: 1, Desired: 1}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "two nodes are NotReady" || !f.called {
		t.Errorf("expected the degraded cluster to be explained; got %q called=%v", got, f.called)
	}
}

func TestBuildInventoryPrompt_LeadsWithDegradedCluster(t *testing.T) {
	ch := clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 3, NodesReady: 1, NodeIssues: []string{"n2 NotReady"}}
	got := buildInventoryPrompt(ch, nil)
	if !strings.Contains(got, "DEGRADED") || !strings.Contains(got, "n2 NotReady") {
		t.Errorf("prompt should lead with the degraded cluster:\n%s", got)
	}
}
```

- [ ] **Step 2: Run the explain tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/explain/ 2>&1 | tail -8
```
Expected: FAIL — `ExplainInventory`/`buildInventoryPrompt` now take a cluster arg.

- [ ] **Step 3: Update `internal/explain/explain.go`**

Add the import `"github.com/imantaba/kubeagent/internal/clusterhealth"`. Change `ExplainInventory` and `buildInventoryPrompt`:

```go
// ExplainInventory summarizes the cluster verdict (when degraded) and the
// notable workloads. It skips the API call and returns "" when the cluster is
// healthy and nothing is notable.
func (c *Client) ExplainInventory(ctx context.Context, cluster clusterhealth.ClusterHealth, workloads []inventory.Workload) (string, error) {
	notable := Notable(workloads)
	if cluster.Verdict != "Degraded" && len(notable) == 0 {
		return "", nil
	}
	out, err := c.s.summarize(ctx, buildInventoryPrompt(cluster, notable))
	if err != nil {
		return "", fmt.Errorf("explaining workloads: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("explaining workloads: model returned no text")
	}
	return out, nil
}

// buildInventoryPrompt renders the cluster verdict (when degraded) and the
// notable workloads. Only structured fields are sent — never raw pod specs or
// secrets (node names in the cluster section are infrastructure identifiers).
func buildInventoryPrompt(cluster clusterhealth.ClusterHealth, workloads []inventory.Workload) string {
	var b strings.Builder
	if cluster.Verdict == "Degraded" {
		fmt.Fprintf(&b, "Cluster health: DEGRADED — %d/%d nodes Ready.\n", cluster.NodesReady, cluster.NodesTotal)
		for _, iss := range cluster.NodeIssues {
			fmt.Fprintf(&b, "  node %s\n", iss)
		}
		for _, iss := range cluster.SystemIssues {
			fmt.Fprintf(&b, "  system %s\n", iss)
		}
		b.WriteString("\n")
	}
	b.WriteString("These Kubernetes workloads need attention:\n\n")
	for _, w := range workloads {
		fmt.Fprintf(&b, "- %s/%s (%s): %d/%d ready, status %s, %d restarts\n",
			w.Namespace, w.Name, w.Kind, w.Ready, w.Desired, w.Status, w.Restarts)
		for _, f := range w.Findings {
			fmt.Fprintf(&b, "    issue: %s — %s (%s)\n", f.Issue, f.Reason, f.Evidence)
		}
	}
	b.WriteString("\nExplain what is going wrong and suggest concrete next steps.")
	return b.String()
}
```

- [ ] **Step 4: Keep the build green — update the `main.go` call site**

In `main.go`, change the `ExplainInventory` call to pass a stub cluster:

```go
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).ExplainInventory(ctx, clusterhealth.ClusterHealth{}, workloads)
```
(`clusterhealth` is already imported from Task 3. Task 5 replaces the stub with the real assessment.)

- [ ] **Step 5: Run the tests + build**

```bash
go build ./...
go test ./internal/explain/ -v 2>&1 | tail -25
go vet ./internal/explain/
```
Expected: module builds; all explain tests pass (updated existing + 2 new cluster cases); vet clean. The egress guard test still passes (PodRow IP/node never sent).

- [ ] **Step 6: Commit**

```bash
git add internal/explain/ main.go
git commit -m "feat(explain): lead the prompt with the cluster verdict when degraded" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: `main` — list nodes, assess, thread the verdict

**Files:**
- Modify: `main.go`
- Modify: `main_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `collect.Nodes`, `clusterhealth.Assess`, the cluster-aware `report.PrintInventory` / `explain.ExplainInventory`.

- [ ] **Step 1: Wire the assessment in `run` (`main.go`)**

After `workloads := inventory.Assemble(inputs, findings)`, add the node list + assessment, and replace the two stubbed `clusterhealth.ClusterHealth{}` arguments with the real `health` value. The relevant section becomes:

```go
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods))
	workloads := inventory.Assemble(inputs, findings)

	nodes, err := collect.Nodes(context.Background(), client)
	if err != nil {
		return err
	}
	health := clusterhealth.Assess(nodes, workloads)

	var explanation string
	if *explainFlag {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).ExplainInventory(ctx, health, workloads)
		if err != nil {
			return err
		}
	}

	return report.PrintInventory(health, workloads, explanation, *output, os.Stdout)
```
(The local is named `health`, not `cluster`, to avoid shadowing the imported `cluster` package.)

- [ ] **Step 2: Build + full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./...
go test ./... 2>&1
go vet ./...
```
Expected: builds; all packages PASS; vet clean.

- [ ] **Step 3: Confirm the existing arg tests still pass + manual fail-fast**

```bash
go build -o kubeagent .
ANTHROPIC_API_KEY= ./kubeagent scan --explain 2>&1 | head -2
```
Expected: `TestRun_*` pass (Step 2); the manual command still prints the `--explain needs the ANTHROPIC_API_KEY environment variable` error (the key check runs before any cluster/node call).

`main_test.go` needs no change (its `TestRun_*` tests exercise arg validation before any cluster/node call). Verify they still compile/pass — no edits expected.

- [ ] **Step 4: Update `README.md`**

In the Usage section, update the `scan` description comment to mention the verdict. Replace:

```bash
# scan the whole cluster — prints every workload (Deployments, StatefulSets,
# DaemonSets, bare pods) with replica health, restart history, and any problems
./kubeagent scan
```
with:

```bash
# scan the whole cluster — leads with a cluster-health verdict (nodes +
# kube-system), then every workload (Deployments, StatefulSets, DaemonSets,
# bare pods) with replica health, restart history, and any problems
./kubeagent scan
```

- [ ] **Step 5: Commit**

```bash
git add main.go README.md
git commit -m "feat: lead scan with the cluster-health verdict (v3 Phase C)" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-review

- **Spec coverage (Phase C):** node health (Ready/pressure/cordon) → Task 1 `nodeHealth` ✅; kube-system rollup of flagged workloads → Task 1 `Assess` ✅; verdict Healthy-only-when-clean → Task 1 ✅; cluster-scoped node list → Task 2 `collect.Nodes` ✅; first-line text verdict + JSON `cluster` key → Task 3 ✅; `--explain` leads with the verdict when degraded + skip logic (degraded cluster OR notable triggers the call) → Task 4 ✅; main wiring (nodes → Assess → report/explain) → Task 5 ✅; scope note (nodes always cluster-wide; system rollup needs kube-system in scope) documented in Global Constraints ✅.
- **Out of scope (correctly absent):** Jobs/CronJobs (Phase D).
- **Placeholder scan:** none — every step has complete code/commands.
- **Type consistency:** `clusterhealth.ClusterHealth` and `Assess(nodes, workloads)` defined in Task 1 used identically in Tasks 3–5; `PrintInventory(cluster, workloads, explanation, format, w)` (Task 3) matches its Task 5 call; `ExplainInventory(ctx, cluster, workloads)` (Task 4) matches its Task 5 call; `collect.Nodes(ctx, client)` (Task 2) matches its Task 5 call; the `main` local is `health` (not `cluster`) to avoid shadowing the `cluster` package.
- **Green-per-task:** Tasks 1–2 add unused packages/functions; Tasks 3–4 change signatures but stub `main` with `clusterhealth.ClusterHealth{}`; Task 5 wires the real value — every task ends `go build ./...` green.
- **Egress:** `buildInventoryPrompt` still never reads `PodRow` (IP/Node); node names appear only in the cluster section (infrastructure identifiers, not secrets); the existing egress guard test is updated to the new signature and still asserts no PodRow leak.
