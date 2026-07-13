# Expected-Node Baseline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Opt-in `--expected-nodes a,b,c` declares the node names you expect; `clusterhealth.Assess` flags each declared name with no `Node` object as `✗ node <name> expected but absent from the cluster` → verdict Degraded, with a daemon gauge `kubeagent_nodes_expected_absent`.

**Architecture:** `clusterhealth.Assess` gains an `expected []string` param; it diffs the cleaned declared list against the present node names and appends an issue (+ increments `NodesExpectedAbsent`) for each absent name. `scan.Evaluate` passes `Options.ExpectedNodes`; `main.go`/`watch` split a comma-separated flag/env. No new collector, no new RBAC.

**Tech Stack:** Go 1.26, `k8s.io/api/core/v1`. Tests use fake nodes + client-go fake clientset.

## Global Constraints

- Read-only; **opt-in** (empty declared list → check off); not wired into `--explain`.
- An expected-absent node is a `clusterhealth` node issue → verdict `Degraded`. "Absent" = **no `Node` object** with that name; a node that exists but is `NotReady` is **present** (health flagged elsewhere). **Missing only** — never flag unexpected/extra nodes.
- Declared list is cleaned once (trim whitespace, drop blanks, dedup, sort); matching is exact/case-sensitive.
- Exact names: flag `--expected-nodes`; env `KUBEAGENT_EXPECTED_NODES`; gauge `kubeagent_nodes_expected_absent`; JSON field `nodesExpectedAbsent`; issue string `expected but absent from the cluster`.
- No new RBAC (nodes already read). Commits carry no `Co-Authored-By: Claude` trailer. TDD. `export PATH=$PATH:/usr/local/go/bin`.

---

### Task 1: `clusterhealth` expected-node check + `Assess` signature

**Files:**
- Modify: `internal/clusterhealth/clusterhealth.go` (import `sort`; `ClusterHealth.NodesExpectedAbsent`; `Assess` signature + expected-absent loop; `cleanExpected` helper)
- Modify: `internal/scan/scan.go:104` (keep the build green with a minimal `nil` expected — the real value comes in Task 2)
- Test: `internal/clusterhealth/clusterhealth_test.go` (update existing `Assess` calls to the new arity; add new tests)

**Interfaces:**
- Produces: `func Assess(nodes []corev1.Node, hb Heartbeat, expected []string, workloads []inventory.Workload) ClusterHealth`; `ClusterHealth.NodesExpectedAbsent int` (`json:"nodesExpectedAbsent,omitempty"`).

- [ ] **Step 1: Write the failing tests**

In `internal/clusterhealth/clusterhealth_test.go`, first **update every existing `Assess(...)` call** to insert `nil` as the new 3rd argument (before the `workloads` argument). There are calls of the form `Assess(nodes, Heartbeat{}, workloads)` → `Assess(nodes, Heartbeat{}, nil, workloads)` and `Assess([]corev1.Node{...}, hb, nil)` → `Assess([]corev1.Node{...}, hb, nil, nil)`. Every call gains one `nil` in the 3rd position.

Then add these tests (helpers `hbReadyNode(name)` and `notReadyNode(name, reason, message)` already exist in the file):

```go
func TestAssess_ExpectedNodeAbsentDegrades(t *testing.T) {
	nodes := []corev1.Node{hbReadyNode("nova-worker-1")}
	ch := Assess(nodes, Heartbeat{}, []string{"nova-worker-1", "nova-worker-2"}, nil)
	if ch.Verdict != "Degraded" || ch.NodesExpectedAbsent != 1 {
		t.Fatalf("an absent expected node must degrade + count: %+v", ch)
	}
	if len(ch.NodeIssues) != 1 || !strings.Contains(ch.NodeIssues[0], "nova-worker-2 expected but absent from the cluster") {
		t.Errorf("want absent issue for nova-worker-2, got %+v", ch.NodeIssues)
	}
}

func TestAssess_ExpectedNodesAllPresentClean(t *testing.T) {
	nodes := []corev1.Node{hbReadyNode("a"), hbReadyNode("b")}
	ch := Assess(nodes, Heartbeat{}, []string{"a", "b"}, nil)
	if ch.Verdict != "Healthy" || ch.NodesExpectedAbsent != 0 {
		t.Errorf("all expected present -> Healthy: %+v", ch)
	}
}

func TestAssess_ExpectedNotReadyCountsAsPresent(t *testing.T) {
	// A node that exists but is NotReady is present for this check (its health is
	// flagged separately); it must NOT produce an "expected but absent" issue.
	nodes := []corev1.Node{notReadyNode("w1", "KubeletNotReady", "down")}
	ch := Assess(nodes, Heartbeat{}, []string{"w1"}, nil)
	if ch.NodesExpectedAbsent != 0 {
		t.Errorf("a NotReady-but-present node must not be flagged absent: %+v", ch)
	}
	for _, iss := range ch.NodeIssues {
		if strings.Contains(iss, "expected but absent") {
			t.Errorf("no absent issue expected for a present node: %q", iss)
		}
	}
}

func TestAssess_ExpectedEmptyDisabled(t *testing.T) {
	ch := Assess([]corev1.Node{hbReadyNode("a")}, Heartbeat{}, nil, nil)
	if ch.NodesExpectedAbsent != 0 || ch.Verdict != "Healthy" {
		t.Errorf("no expected list -> check disabled: %+v", ch)
	}
	ch2 := Assess([]corev1.Node{hbReadyNode("a")}, Heartbeat{}, []string{" ", ""}, nil)
	if ch2.NodesExpectedAbsent != 0 {
		t.Errorf("blank-only expected list -> disabled: %+v", ch2)
	}
}

func TestAssess_ExpectedCleaningAndOrder(t *testing.T) {
	// Trim + dedup: " zeta " and "alpha"/"alpha" collapse; absent names sort.
	nodes := []corev1.Node{hbReadyNode("present")}
	ch := Assess(nodes, Heartbeat{}, []string{" zeta ", "alpha", "alpha", "present"}, nil)
	if ch.NodesExpectedAbsent != 2 {
		t.Fatalf("want 2 absent (alpha, zeta) after trim+dedup: %+v", ch)
	}
	if !strings.Contains(ch.NodeIssues[0], "alpha") || !strings.Contains(ch.NodeIssues[1], "zeta") {
		t.Errorf("absent issues must be sorted (alpha before zeta): %+v", ch.NodeIssues)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/clusterhealth/`
Expected: FAIL to compile — `Assess` takes 3 args / no `NodesExpectedAbsent`.

- [ ] **Step 3: Implement**

In `internal/clusterhealth/clusterhealth.go`, add `"sort"` to the imports. Add the field to `ClusterHealth` (after `NodesStaleHeartbeat`):

```go
	NodesExpectedAbsent int      `json:"nodesExpectedAbsent,omitempty"`
```

Replace `Assess` with (adds the `expected` param, builds a `present` set in the node loop, and appends absent issues before the workloads loop):

```go
func Assess(nodes []corev1.Node, hb Heartbeat, expected []string, workloads []inventory.Workload) ClusterHealth {
	ch := ClusterHealth{NodesTotal: len(nodes)}
	leaseByNode := make(map[string]coordinationv1.Lease, len(hb.Leases))
	for _, l := range hb.Leases {
		leaseByNode[l.Name] = l
	}
	present := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		present[n.Name] = true
		ready, issues := nodeHealth(n)
		if ready {
			ch.NodesReady++
			if hb.Threshold > 0 {
				if iss, stale := staleHeartbeat(leaseByNode, n.Name, hb.Now, hb.Threshold); stale {
					issues = append(issues, iss)
					ch.NodesStaleHeartbeat++
				}
			}
		}
		for _, iss := range issues {
			ch.NodeIssues = append(ch.NodeIssues, n.Name+" "+iss)
		}
	}
	for _, name := range cleanExpected(expected) {
		if !present[name] {
			ch.NodeIssues = append(ch.NodeIssues, name+" expected but absent from the cluster")
			ch.NodesExpectedAbsent++
		}
	}
	for _, w := range workloads {
		if w.Namespace == systemNamespace && w.Flagged() {
			if w.Kind == "Job" || w.Kind == "CronJob" {
				ch.SystemIssues = append(ch.SystemIssues,
					fmt.Sprintf("%s/%s %s", w.Namespace, w.Name, w.Status))
			} else {
				ch.SystemIssues = append(ch.SystemIssues,
					fmt.Sprintf("%s/%s %d/%d %s", w.Namespace, w.Name, w.Ready, w.Desired, w.Status))
			}
		}
	}
	if len(ch.NodeIssues) == 0 && len(ch.SystemIssues) == 0 {
		ch.Verdict = "Healthy"
	} else {
		ch.Verdict = "Degraded"
	}
	return ch
}

// cleanExpected trims, drops blanks, dedups, and sorts the declared expected
// node names.
func cleanExpected(expected []string) []string {
	seen := make(map[string]bool, len(expected))
	var out []string
	for _, name := range expected {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
```

Also update the `Assess` doc comment's first sentence to mention the expected-node check, e.g. append to the existing comment: "…or a declared expected node is absent from the cluster."

In `internal/scan/scan.go:104`, add a minimal `nil` 3rd arg to keep the build green (the real value comes in Task 2):

```go
	health := clusterhealth.Assess(nodes, clusterhealth.Heartbeat{Leases: leases, Now: time.Now(), Threshold: opts.NodeHeartbeatThreshold}, nil, workloads)
```

- [ ] **Step 4: Run to verify they pass**

Run: `go build ./... && go test ./internal/clusterhealth/`
Expected: PASS (new + updated tests; `go build` green because scan.go passes `nil`).

- [ ] **Step 5: Commit**

```bash
git add internal/clusterhealth/clusterhealth.go internal/clusterhealth/clusterhealth_test.go internal/scan/scan.go
git commit -m "feat(clusterhealth): flag declared expected nodes that are absent"
```

---

### Task 2: Wire `ExpectedNodes` into `scan` + `--expected-nodes`

**Files:**
- Modify: `internal/scan/scan.go` (`Options.ExpectedNodes`; pass real value to `Assess`)
- Modify: `main.go` (flag + usage string + `splitCSV` helper + `scan.Options` literal)
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `clusterhealth.Assess` expected param, `ClusterHealth.NodesExpectedAbsent` (Task 1).
- Produces: `scan.Options.ExpectedNodes []string`.

- [ ] **Step 1: Write the failing test**

Add to `internal/scan/scan_test.go` (imports `corev1`, `metav1`, `fake`, `context` already present from the heartbeat test):

```go
func TestEvaluate_ExpectedNodeAbsentDegrades(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
	)
	res, err := Evaluate(context.Background(), client, Options{ExpectedNodes: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Health.Verdict != "Degraded" || res.Health.NodesExpectedAbsent != 1 {
		t.Errorf("declared node b absent must degrade the verdict: %+v", res.Health)
	}

	off, _ := Evaluate(context.Background(), client, Options{})
	if off.Health.NodesExpectedAbsent != 0 {
		t.Errorf("no expected list must leave the count 0: %+v", off.Health)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ -run TestEvaluate_ExpectedNode`
Expected: FAIL — `Options` has no `ExpectedNodes` / verdict not Degraded (not wired).

- [ ] **Step 3: Wire `scan.go`**

In `internal/scan/scan.go`, add to `Options` (after `NodeHeartbeatThreshold`):

```go
	ExpectedNodes []string
```

Change the `clusterhealth.Assess(...)` call (line ~104) to pass `opts.ExpectedNodes` instead of `nil`:

```go
	health := clusterhealth.Assess(nodes, clusterhealth.Heartbeat{Leases: leases, Now: time.Now(), Threshold: opts.NodeHeartbeatThreshold}, opts.ExpectedNodes, workloads)
```

- [ ] **Step 4: Wire `main.go`**

Add the flag next to the other scan flags (after `node-heartbeat-threshold`, ~line 75):

```go
	expectedNodes := fs.String("expected-nodes", "", "names of nodes expected in the cluster; a declared name with no Node object is flagged Degraded (comma-separated)")
```

Add `[--expected-nodes a,b,…]` to the scan usage string (~line 60), after `[--node-heartbeat-threshold dur]`.

Add to the `scan.Options{...}` literal (~line 99):

```go
		ExpectedNodes:          splitCSV(*expectedNodes),
```

Add the `splitCSV` helper next to `envOr` (~line 219):

```go
// splitCSV splits a comma-separated list into a slice, returning nil for empty.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
```

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/scan/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/scan/scan.go main.go
git commit -m "feat(scan): opt expected-node baseline with --expected-nodes"
```

---

### Task 3: Daemon gauge `kubeagent_nodes_expected_absent`

**Files:**
- Modify: `internal/watch/watch.go` (`Config.ExpectedNodes`; pass into `scan.Options`)
- Modify: `internal/watch/metrics.go` (`metrics` field; `update`; `render`)
- Modify: `main.go` (`watch.Config` literal — env via `splitCSV`)
- Test: `internal/watch/metrics_test.go`

**Interfaces:**
- Consumes: `scan.Options.ExpectedNodes` (Task 2); `Result.Health.NodesExpectedAbsent` (Task 1).

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, set `NodesExpectedAbsent` on the `sampleResult()` `Health` (e.g. `NodesExpectedAbsent: 1`), and add to the `want` list in `TestMetrics_RenderReflectsResult`:

```go
		"kubeagent_nodes_expected_absent 1",
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult`
Expected: FAIL — gauge missing (and compile error until the field exists).

- [ ] **Step 3: Implement**

In `internal/watch/watch.go`, add to `Config` (after `NodeHeartbeatThreshold`):

```go
	ExpectedNodes []string
```

and add it to the `scan.Options{...}` construction (line ~91):

```go
		ExpectedNodes: cfg.ExpectedNodes,
```

In `internal/watch/metrics.go`, add to the `metrics` struct (after `nodesStaleHeartbeat int`):

```go
	nodesExpectedAbsent int
```

In `update`, after `m.nodesStaleHeartbeat = res.Health.NodesStaleHeartbeat`, add:

```go
	m.nodesExpectedAbsent = res.Health.NodesExpectedAbsent
```

In `render`, after the `kubeagent_nodes_stale_heartbeat` gauge line, add:

```go
	gauge("kubeagent_nodes_expected_absent", "Declared expected nodes that are absent from the cluster", float64(m.nodesExpectedAbsent))
```

- [ ] **Step 4: Wire `main.go` daemon env**

In `main.go`, in the `watch.Config{...}` literal (after `NodeHeartbeatThreshold: envDur(...)`, ~line 216), add:

```go
		ExpectedNodes:          splitCSV(envOr("KUBEAGENT_EXPECTED_NODES", "")),
```

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/watch/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/watch/watch.go internal/watch/metrics.go internal/watch/metrics_test.go main.go
git commit -m "feat(watch): expose kubeagent_nodes_expected_absent gauge"
```

---

### Task 4: Docs

**Files:**
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `website/docs/features/diagnostics.md`
- Modify: `website/docs/features/watch-mode.md`
- Modify: `website/docs/roadmap.md`
- Modify: `README.md`

**Interfaces:** none. Exact names: flag `--expected-nodes`; env `KUBEAGENT_EXPECTED_NODES`; gauge `kubeagent_nodes_expected_absent`; JSON `nodesExpectedAbsent`.

- [ ] **Step 1: CHANGELOG**

Add a new `## [Unreleased]` section above the current top entry with:

```markdown
## [Unreleased]

### Added

- **Expected-node baseline.** Opt-in `scan --expected-nodes nova-worker-1,…`
  declares the node names you expect; kubeagent flags each declared node that has
  **no `Node` object** in the cluster — `node nova-worker-2 expected but absent
  from the cluster` — catching a node that never registered or dropped out. It
  degrades the cluster verdict, and the watch daemon exposes
  `kubeagent_nodes_expected_absent` (set `KUBEAGENT_EXPECTED_NODES`). A node that
  exists but is `NotReady` counts as present (its health is flagged elsewhere);
  extra/unexpected nodes are not flagged. Read-only; no new RBAC; best on
  clusters with stable node names.
```

- [ ] **Step 2: diagnostics.md**

Run: `grep -nE '^### ' website/docs/features/diagnostics.md | head`

Add a subsection (matching the heading level/style) after the node-heartbeat material:

```markdown
### Expected-node baseline

`scan --expected-nodes nova-worker-1,nova-worker-2,…` declares the node names you
expect. kubeagent flags each declared node that has **no `Node` object** in the
cluster — `✗ node nova-worker-2 expected but absent from the cluster` — which
catches a kubelet that never registered its node, or a node that dropped out of
the cluster entirely. It degrades the cluster verdict. A node that exists but is
`NotReady` counts as **present** (its health is flagged by the NotReady /
heartbeat checks); unexpected/extra nodes are never flagged. It is opt-in (off
until you declare a list) and best on clusters with **stable** node names —
autoscaled clusters whose node names churn would false-positive. The count is
also exposed in JSON as `nodesExpectedAbsent`.
```

- [ ] **Step 3: watch-mode.md**

Run: `grep -nE 'kubeagent_|KUBEAGENT_' website/docs/features/watch-mode.md | head`

Add `kubeagent_nodes_expected_absent` to the documented metrics (neighbouring format), described as "Number of declared expected nodes that are absent from the cluster", and document the `KUBEAGENT_EXPECTED_NODES` env (comma-separated node names) alongside the other `KUBEAGENT_*` env vars.

- [ ] **Step 4: roadmap.md**

Run: `grep -nE 'Shipped|Version history' website/docs/roadmap.md | head`

Add a bullet to the "Shipped" list (before the `!!! info "Version history"` block), matching the existing style:

```markdown
- **Expected-node baseline** — opt-in `scan --expected-nodes` flags a declared
  node that is absent from the cluster (never registered or dropped out), and
  the daemon exposes `kubeagent_nodes_expected_absent`. See
  [Failure diagnostics](features/diagnostics.md).
```

- [ ] **Step 5: README**

Run: `grep -nE 'node-heartbeat|disk-usage|expected|detect' README.md | head`

Add a one-line mention of the expected-node-baseline check alongside the existing feature list, matching the surrounding style.

- [ ] **Step 6: Verify the docs build**

Run:

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: exit 0, "Documentation built", no page `WARNING` lines (the Material for MkDocs team banner is cosmetic — ignore it). If the venv is missing, recreate: `python3 -m venv <path> && <path>/bin/pip install -r requirements.txt`.

- [ ] **Step 7: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md website/docs/features/watch-mode.md website/docs/roadmap.md README.md
git commit -m "docs: document expected-node baseline + its daemon gauge"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Manual smoke (declare a name that isn't a real node):

```bash
go build -o kubeagent . && ./kubeagent scan --expected-nodes does-not-exist | sed -n '/^Cluster:/,/^$/p'
./kubeagent scan --expected-nodes does-not-exist --output json | grep -o '"nodesExpectedAbsent"'
```

Expected: a `✗ node does-not-exist expected but absent from the cluster` line and a `Degraded` verdict; `nodesExpectedAbsent` in JSON.
