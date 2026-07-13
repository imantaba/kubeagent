# Node Heartbeat Freshness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag a Ready node whose kubelet `Lease` (`kube-node-lease`) is stale beyond a threshold — "kubelet went dark" — as a `clusterhealth` node issue that degrades the verdict, with a daemon gauge `kubeagent_nodes_stale_heartbeat`.

**Architecture:** A new `collect.NodeLeases` lists the node Leases; `clusterhealth.Assess` gains a `Heartbeat{Leases, Now, Threshold}` input and appends a heartbeat issue (and increments `NodesStaleHeartbeat`) for a Ready node whose lease is stale or missing. `scan.Evaluate` collects the leases and passes the threshold; `main.go`/`watch` expose it; RBAC gains read-only `leases`.

**Tech Stack:** Go 1.26, `k8s.io/api/coordination/v1`, `k8s.io/api/core/v1`. Tests use fake objects + client-go fake clientset.

## Global Constraints

- Read-only; **on by default** (plain read-only RBAC); not wired into `--explain`.
- A stale/missing-lease **Ready** node is a `clusterhealth` node issue → verdict `Degraded`. A **NotReady** node gets **no** heartbeat issue (the existing NotReady line covers it — no duplicate).
- Staleness = `now − renewTime`, rendered as `(...).Round(time.Second).String()` (e.g. `48s`, `3m12s`). A node with no lease (or a lease with nil `renewTime`) → issue `no kubelet lease`.
- Threshold default **40s**, `0` disables the check; flag `--node-heartbeat-threshold` (Go duration) + env `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD` for the daemon.
- Exact names: gauge `kubeagent_nodes_stale_heartbeat`; JSON field `nodesStaleHeartbeat`; issue strings `kubelet not heartbeating (lease Ns stale)` and `no kubelet lease`.
- RBAC adds `leases` (`coordination.k8s.io`, `get`/`list`/`watch`) to BOTH `deploy/rbac.yaml` and the Helm ClusterRole.
- Clock-skew is documented, not code-handled. Commits carry no `Co-Authored-By: Claude` trailer. TDD. `export PATH=$PATH:/usr/local/go/bin`.

---

### Task 1: `collect.NodeLeases`

**Files:**
- Modify: `internal/collect/collect.go` (add `coordinationv1` import; add `NodeLeases` after `Nodes`, ~line 81)
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Produces: `func NodeLeases(ctx context.Context, client kubernetes.Interface) ([]coordinationv1.Lease, error)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go` (add imports `coordinationv1 "k8s.io/api/coordination/v1"` and `"time"` if missing):

```go
func TestNodeLeases_List(t *testing.T) {
	rt := metav1.NewMicroTime(time.Now())
	client := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-node-lease", Name: "node-1"},
		Spec:       coordinationv1.LeaseSpec{RenewTime: &rt},
	})
	got, err := NodeLeases(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "node-1" {
		t.Errorf("want 1 lease node-1, got %+v", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -run TestNodeLeases_List`
Expected: FAIL to compile — `undefined: NodeLeases`.

- [ ] **Step 3: Add the collector**

In `internal/collect/collect.go`, add `coordinationv1 "k8s.io/api/coordination/v1"` to the import block, and after `Nodes` add:

```go
// NodeLeases lists node heartbeat Leases in kube-node-lease (one per node), read-only.
func NodeLeases(ctx context.Context, client kubernetes.Interface) ([]coordinationv1.Lease, error) {
	leases, err := client.CoordinationV1().Leases("kube-node-lease").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing node leases: %w", err)
	}
	return leases.Items, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/collect/ -run TestNodeLeases_List`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go
git commit -m "feat(collect): list node heartbeat Leases (kube-node-lease)"
```

---

### Task 2: `clusterhealth` heartbeat check + `Assess` signature

**Files:**
- Modify: `internal/clusterhealth/clusterhealth.go` (imports; `ClusterHealth.NodesStaleHeartbeat`; `Heartbeat` type; `Assess` signature + logic; `staleHeartbeat` helper)
- Modify: `internal/scan/scan.go:102` (keep the build green with a minimal `Heartbeat{}` — real inputs come in Task 3)
- Test: `internal/clusterhealth/clusterhealth_test.go` (update the two `Assess` call sites; add new tests)

**Interfaces:**
- Consumes: `coordinationv1.Lease` (Task 1 shape).
- Produces:
  - `type Heartbeat struct { Leases []coordinationv1.Lease; Now time.Time; Threshold time.Duration }`
  - `func Assess(nodes []corev1.Node, hb Heartbeat, workloads []inventory.Workload) ClusterHealth`
  - `ClusterHealth.NodesStaleHeartbeat int` (`json:"nodesStaleHeartbeat,omitempty"`)

- [ ] **Step 1: Write the failing tests**

Add to `internal/clusterhealth/clusterhealth_test.go` (ensure imports `"time"`, `coordinationv1 "k8s.io/api/coordination/v1"`, `corev1 "k8s.io/api/core/v1"`, `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`, `"strings"` are present):

```go
func hbReadyNode(name string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func hbLease(node string, renew time.Time) coordinationv1.Lease {
	rt := metav1.NewMicroTime(renew)
	return coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-node-lease", Name: node},
		Spec:       coordinationv1.LeaseSpec{RenewTime: &rt},
	}
}

func TestAssess_StaleHeartbeatDegrades(t *testing.T) {
	now := time.Now()
	hb := Heartbeat{Leases: []coordinationv1.Lease{hbLease("w1", now.Add(-90 * time.Second))}, Now: now, Threshold: 40 * time.Second}
	ch := Assess([]corev1.Node{hbReadyNode("w1")}, hb, nil)
	if ch.Verdict != "Degraded" || ch.NodesStaleHeartbeat != 1 {
		t.Fatalf("stale lease must degrade + count: %+v", ch)
	}
	if len(ch.NodeIssues) != 1 || !strings.Contains(ch.NodeIssues[0], "kubelet not heartbeating") {
		t.Errorf("want a heartbeat issue, got %+v", ch.NodeIssues)
	}
}

func TestAssess_FreshHeartbeatClean(t *testing.T) {
	now := time.Now()
	hb := Heartbeat{Leases: []coordinationv1.Lease{hbLease("w1", now.Add(-5 * time.Second))}, Now: now, Threshold: 40 * time.Second}
	ch := Assess([]corev1.Node{hbReadyNode("w1")}, hb, nil)
	if ch.Verdict != "Healthy" || ch.NodesStaleHeartbeat != 0 {
		t.Errorf("fresh lease must stay Healthy: %+v", ch)
	}
}

func TestAssess_MissingLeaseFlagged(t *testing.T) {
	now := time.Now()
	ch := Assess([]corev1.Node{hbReadyNode("w1")}, Heartbeat{Leases: nil, Now: now, Threshold: 40 * time.Second}, nil)
	if ch.NodesStaleHeartbeat != 1 || len(ch.NodeIssues) != 1 || !strings.Contains(ch.NodeIssues[0], "no kubelet lease") {
		t.Errorf("missing lease on a Ready node must flag: %+v", ch)
	}
}

func TestAssess_NotReadyNodeNoDuplicateHeartbeat(t *testing.T) {
	now := time.Now()
	notReady := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "w1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady"}}},
	}
	hb := Heartbeat{Leases: []coordinationv1.Lease{hbLease("w1", now.Add(-90 * time.Second))}, Now: now, Threshold: 40 * time.Second}
	ch := Assess([]corev1.Node{notReady}, hb, nil)
	if ch.NodesStaleHeartbeat != 0 {
		t.Errorf("NotReady node must not add a heartbeat issue: %+v", ch)
	}
	for _, iss := range ch.NodeIssues {
		if strings.Contains(iss, "heartbeating") {
			t.Errorf("no heartbeat issue expected on a NotReady node: %q", iss)
		}
	}
}

func TestAssess_HeartbeatThresholdDisabled(t *testing.T) {
	now := time.Now()
	ch := Assess([]corev1.Node{hbReadyNode("w1")}, Heartbeat{Leases: nil, Now: now, Threshold: 0}, nil)
	if ch.NodesStaleHeartbeat != 0 || ch.Verdict != "Healthy" {
		t.Errorf("threshold 0 disables the check: %+v", ch)
	}
}
```

Also update the **existing** `Assess` call sites in this test file (currently `Assess(nodes, workloads)`) to `Assess(nodes, Heartbeat{}, workloads)`.

- [ ] **Step 2: Run to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/clusterhealth/`
Expected: FAIL to compile — `Assess` takes 2 args / `Heartbeat` undefined / no `NodesStaleHeartbeat`.

- [ ] **Step 3: Implement**

In `internal/clusterhealth/clusterhealth.go`, add `"time"` and `coordinationv1 "k8s.io/api/coordination/v1"` to the imports. Add the field to `ClusterHealth` (after `NodesReady`):

```go
	NodesStaleHeartbeat int `json:"nodesStaleHeartbeat,omitempty"`
```

Add the `Heartbeat` type (above `Assess`):

```go
// Heartbeat carries the node-lease inputs for the kubelet-heartbeat-freshness
// check. A Threshold <= 0 disables the check.
type Heartbeat struct {
	Leases    []coordinationv1.Lease
	Now       time.Time
	Threshold time.Duration
}
```

Replace `Assess` with:

```go
// Assess computes the verdict from nodes and the assembled workloads. A node is
// unhealthy if not Ready, under Memory/Disk/PID pressure, cordoned, or (when the
// heartbeat check is enabled) Ready but its kubelet lease is stale/missing. The
// verdict is Healthy only when there are no node and no system issues.
func Assess(nodes []corev1.Node, hb Heartbeat, workloads []inventory.Workload) ClusterHealth {
	ch := ClusterHealth{NodesTotal: len(nodes)}
	leaseByNode := make(map[string]coordinationv1.Lease, len(hb.Leases))
	for _, l := range hb.Leases {
		leaseByNode[l.Name] = l
	}
	for _, n := range nodes {
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

// staleHeartbeat reports whether a Ready node's kubelet lease is stale (or
// missing/renewTime-less) beyond the threshold, and the issue string to record.
func staleHeartbeat(leaseByNode map[string]coordinationv1.Lease, node string, now time.Time, threshold time.Duration) (string, bool) {
	l, ok := leaseByNode[node]
	if !ok || l.Spec.RenewTime == nil {
		return "no kubelet lease", true
	}
	staleness := now.Sub(l.Spec.RenewTime.Time)
	if staleness > threshold {
		return fmt.Sprintf("kubelet not heartbeating (lease %s stale)", staleness.Round(time.Second)), true
	}
	return "", false
}
```

In `internal/scan/scan.go:102`, change the call to keep the build green (real inputs land in Task 3):

```go
	health := clusterhealth.Assess(nodes, clusterhealth.Heartbeat{}, workloads)
```

- [ ] **Step 4: Run to verify they pass**

Run: `go build ./... && go test ./internal/clusterhealth/`
Expected: PASS (new + existing tests; `go build` green because scan.go now passes `Heartbeat{}`).

- [ ] **Step 5: Commit**

```bash
git add internal/clusterhealth/clusterhealth.go internal/clusterhealth/clusterhealth_test.go internal/scan/scan.go
git commit -m "feat(clusterhealth): flag Ready nodes with a stale kubelet lease"
```

---

### Task 3: Wire leases + threshold into `scan` + `--node-heartbeat-threshold`

**Files:**
- Modify: `internal/scan/scan.go` (`Options.NodeHeartbeatThreshold`; collect leases; pass real `Heartbeat`)
- Modify: `main.go` (flag + usage string + `scan.Options` literal)
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `collect.NodeLeases` (Task 1); `clusterhealth.Heartbeat`, `ClusterHealth.NodesStaleHeartbeat` (Task 2).
- Produces: `scan.Options.NodeHeartbeatThreshold time.Duration`.

- [ ] **Step 1: Write the failing test**

Add to `internal/scan/scan_test.go` (add imports `coordinationv1 "k8s.io/api/coordination/v1"` and `"time"` if missing):

```go
func TestEvaluate_StaleHeartbeatDegrades(t *testing.T) {
	now := time.Now()
	rt := metav1.NewMicroTime(now.Add(-2 * time.Minute))
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-node-lease", Name: "w1"}, Spec: coordinationv1.LeaseSpec{RenewTime: &rt}},
	)
	res, err := Evaluate(context.Background(), client, Options{NodeHeartbeatThreshold: 40 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Health.Verdict != "Degraded" || res.Health.NodesStaleHeartbeat != 1 {
		t.Errorf("a Ready node with a stale lease must degrade the verdict: %+v", res.Health)
	}

	// Threshold 0 disables the check -> same cluster reads Healthy.
	off, _ := Evaluate(context.Background(), client, Options{})
	if off.Health.NodesStaleHeartbeat != 0 {
		t.Errorf("threshold 0 must disable the heartbeat check: %+v", off.Health)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ -run TestEvaluate_StaleHeartbeat`
Expected: FAIL — `Options` has no `NodeHeartbeatThreshold` / verdict not Degraded (leases not wired yet).

- [ ] **Step 3: Wire `scan.go`**

In `internal/scan/scan.go`, add to `Options` (after `DiskThreshold`):

```go
	NodeHeartbeatThreshold time.Duration
```

Replace the `health := clusterhealth.Assess(nodes, clusterhealth.Heartbeat{}, workloads)` line (from Task 2) with:

```go
	leases, _ := collect.NodeLeases(ctx, client)
	health := clusterhealth.Assess(nodes, clusterhealth.Heartbeat{Leases: leases, Now: time.Now(), Threshold: opts.NodeHeartbeatThreshold}, workloads)
```

- [ ] **Step 4: Wire `main.go`**

Add the flag next to the other scan flags (after `disk-threshold`, ~line 74):

```go
	nodeHeartbeatThreshold := fs.Duration("node-heartbeat-threshold", 40*time.Second, "flag a Ready node whose kubelet lease is stale beyond this (0 disables)")
```

Add `[--node-heartbeat-threshold dur]` to the scan usage string (~line 60), after `[--disk-usage [--disk-threshold r]]`.

Add to the `scan.Options{...}` literal (~line 99):

```go
		NodeHeartbeatThreshold: *nodeHeartbeatThreshold,
```

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/scan/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/scan/scan.go main.go
git commit -m "feat(scan): opt node heartbeat freshness with --node-heartbeat-threshold"
```

---

### Task 4: Daemon gauge `kubeagent_nodes_stale_heartbeat`

**Files:**
- Modify: `internal/watch/watch.go` (`Config.NodeHeartbeatThreshold`; pass into `scan.Options`)
- Modify: `internal/watch/metrics.go` (`metrics` field; `update`; `render`)
- Modify: `main.go` (`watch.Config` literal — env)
- Test: `internal/watch/metrics_test.go`

**Interfaces:**
- Consumes: `scan.Options.NodeHeartbeatThreshold` (Task 3); `Result.Health.NodesStaleHeartbeat` (Task 2).

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, extend the `sampleResult()` `scan.Result{...}` — set `NodesStaleHeartbeat` on its `Health` (the result already builds a `clusterhealth.ClusterHealth`; add the field, e.g. `NodesStaleHeartbeat: 1`). Add to the `want` list in `TestMetrics_RenderReflectsResult`:

```go
		"kubeagent_nodes_stale_heartbeat 1",
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult`
Expected: FAIL — gauge missing (and compile error until the metrics field exists).

- [ ] **Step 3: Implement**

In `internal/watch/watch.go`, add to `Config` (after `DiskThreshold`):

```go
	NodeHeartbeatThreshold time.Duration
```

and add it to the `scan.Options{...}` construction (line ~90):

```go
		NodeHeartbeatThreshold: cfg.NodeHeartbeatThreshold,
```

In `internal/watch/metrics.go`, add to the `metrics` struct (after `nodesNoReserve int`):

```go
	nodesStaleHeartbeat int
```

In `update`, after `m.nodesNoReserve = res.NodeReserve.WarnCount`, add:

```go
	m.nodesStaleHeartbeat = res.Health.NodesStaleHeartbeat
```

In `render`, after the `kubeagent_nodes_without_reservations` gauge line, add:

```go
	gauge("kubeagent_nodes_stale_heartbeat", "Ready nodes whose kubelet lease is stale (kubelet not heartbeating)", float64(m.nodesStaleHeartbeat))
```

- [ ] **Step 4: Wire `main.go` daemon env**

In `main.go`, in the `watch.Config{...}` literal (after `DiskThreshold: envFloat(...)`, ~line 213), add:

```go
		NodeHeartbeatThreshold: envDur("KUBEAGENT_NODE_HEARTBEAT_THRESHOLD", 40*time.Second),
```

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/watch/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/watch/watch.go internal/watch/metrics.go internal/watch/metrics_test.go main.go
git commit -m "feat(watch): expose kubeagent_nodes_stale_heartbeat gauge"
```

---

### Task 5: RBAC — grant `leases` read

**Files:**
- Modify: `deploy/rbac.yaml`
- Modify: `deploy/helm/kubeagent/templates/clusterrole.yaml`

**Interfaces:** none (manifests).

- [ ] **Step 1: Update `deploy/rbac.yaml`**

After the `storage.k8s.io` rule (the `storageclasses` block), add:

```yaml
  - apiGroups: ["coordination.k8s.io"]
    resources: [leases]
    verbs: [get, list, watch]
```

- [ ] **Step 2: Update the Helm ClusterRole**

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, after the `storage.k8s.io` (`storageclasses`) rule, add the same block:

```yaml
  - apiGroups: ["coordination.k8s.io"]
    resources: [leases]
    verbs: [get, list, watch]
```

- [ ] **Step 3: Verify the chart renders read-only**

Run:

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent | grep -A2 'coordination.k8s.io'
helm template x deploy/helm/kubeagent | grep -iE 'create|update|patch|delete' | grep -i verb && echo BAD || echo "read-only OK"
```

Expected: lint clean; the rendered `coordination.k8s.io` rule lists `leases` with `[get, list, watch]`; the write-verb check prints `read-only OK`.

- [ ] **Step 4: Commit**

```bash
git add deploy/rbac.yaml deploy/helm/kubeagent/templates/clusterrole.yaml
git commit -m "feat(rbac): grant read-only leases for node heartbeat freshness"
```

---

### Task 6: Docs

**Files:**
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `website/docs/features/diagnostics.md`
- Modify: `website/docs/features/watch-mode.md`
- Modify: `website/docs/roadmap.md`
- Modify: `README.md`

**Interfaces:** none. Exact names: flag `--node-heartbeat-threshold`; env `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD`; gauge `kubeagent_nodes_stale_heartbeat`; JSON `nodesStaleHeartbeat`.

- [ ] **Step 1: CHANGELOG**

Add a new `## [Unreleased]` section above the current top entry with:

```markdown
## [Unreleased]

### Added

- **Node heartbeat freshness.** `scan` reads each node's `Lease`
  (`kube-node-lease`) and flags a **Ready** node whose kubelet has stopped
  heartbeating — `kubelet not heartbeating (lease Ns stale)` — catching a dark
  kubelet in the window *before* the control plane marks the node `NotReady`.
  It degrades the cluster verdict, is tunable via `--node-heartbeat-threshold`
  (default `40s`), and the watch daemon exposes
  `kubeagent_nodes_stale_heartbeat` (set `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD`) so
  you can alert before a node goes down. Reads `leases` (a new read-only RBAC
  grant); on by default.
```

- [ ] **Step 2: diagnostics.md**

Run: `grep -nE '^### ' website/docs/features/diagnostics.md | head`

Add a subsection (matching the heading level/style) after the node-related material:

```markdown
### Node heartbeat freshness

Each node renews a `Lease` in `kube-node-lease` about every 10 seconds; the
control plane only marks a node `NotReady` after ~40 seconds of missed renewals.
`scan` reads those Leases and flags a node that still reads **Ready** but whose
lease has gone stale — `✗ node worker-2 kubelet not heartbeating (lease 48s
stale)` — so a crashed, hung, or partitioned kubelet shows up *before* the node
flips to `NotReady`. It degrades the cluster verdict, and the threshold is
tunable with `--node-heartbeat-threshold` (default `40s`; `0` disables it).
Compares against the scanner's clock, so run it in-cluster (the watch daemon) or
on a clock-synced host.
```

- [ ] **Step 3: watch-mode.md**

Run: `grep -nE 'kubeagent_|KUBEAGENT_' website/docs/features/watch-mode.md | head`

Add `kubeagent_nodes_stale_heartbeat` to the documented metrics (neighbouring format), described as "Number of Ready nodes whose kubelet lease is stale (kubelet not heartbeating)", and document the `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD` env (default `40s`) alongside the other `KUBEAGENT_*` env vars.

- [ ] **Step 4: roadmap.md**

Run: `grep -nE 'Shipped|Version history' website/docs/roadmap.md | head`

Add a bullet to the "Shipped" list (before the `!!! info "Version history"` block), matching the existing style:

```markdown
- **Node heartbeat freshness** — `scan` flags a Ready node whose kubelet `Lease`
  has gone stale (kubelet not heartbeating) before it flips to `NotReady`, and
  the daemon exposes `kubeagent_nodes_stale_heartbeat`. See
  [Failure diagnostics](features/diagnostics.md).
```

- [ ] **Step 5: README**

Run: `grep -nE 'disk-usage|ingress route|security|detect' README.md | head`

Add a one-line mention of the node-heartbeat-freshness check alongside the existing feature list, matching the surrounding style.

- [ ] **Step 6: Verify the docs build**

Run:

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: exit 0, "Documentation built", no page `WARNING` lines (the Material for MkDocs team banner is cosmetic — ignore it). If the venv is missing, recreate: `python3 -m venv <path> && <path>/bin/pip install -r requirements.txt`.

- [ ] **Step 7: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md website/docs/features/watch-mode.md website/docs/roadmap.md README.md
git commit -m "docs: document node heartbeat freshness + its daemon gauge"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Manual smoke (a node whose kubelet stopped — or simulate by not renewing a lease):

```bash
go build -o kubeagent . && ./kubeagent scan | sed -n '/^Cluster:/,/^$/p'
./kubeagent scan --output json | grep -o '"nodesStaleHeartbeat"'
```

Expected: a `✗ node <name> kubelet not heartbeating (lease Ns stale)` line and a `Degraded` verdict when a kubelet is dark; `nodesStaleHeartbeat` in JSON.
