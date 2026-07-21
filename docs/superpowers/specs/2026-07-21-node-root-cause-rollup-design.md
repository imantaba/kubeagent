# Node-anchored root-cause rollup — design

**Status:** approved · **Date:** 2026-07-21 · **Type:** new feature (v0.29 root-cause theme)

## Problem

When a node goes hard-down, every workload with a pod on it lights up
independently — N separate red findings in NEEDS ATTENTION — with nothing tying
them to the one thing that's actually wrong (the node). kubeagent already prints
the node problem at the top (`✗ node worker-2 NotReady…`), but the workload
findings don't say *"this is because of that node."* This is the first, anchoring
slice of the roadmap's **root-cause correlation** theme.

## Approach (approved)

**Attribute in place**: each affected workload keeps its place and its own
findings, and gains one hedged attribution line plus a rollup count in the
attention line. Nothing is hidden or restructured.

**Hard-down nodes only**: a node is a root cause when it is **NotReady** or its
**kubelet is not heartbeating** (stale lease) — the cases that genuinely make
pods on the node unreachable. Cordoned (SchedulingDisabled) and node-pressure
causes are explicitly out of scope for v1 (they reuse this same mechanism later).

## Output

```text
Cluster: Degraded — 4/5 nodes Ready
  ✗ node worker-2 NotReady: KubeletNotReady — container runtime is down
  Needs attention: 3 workloads failing (3 ⇐ node worker-2)

NEEDS ATTENTION
✗ shop/api  Deployment  0/2 Degraded
    ↳ likely caused by node worker-2 (NotReady)
    ⚠ CrashLoopBackOff: ...
```

- Per-workload line, rendered **first** under the workload (cause before
  symptoms): `↳ likely caused by node <name> (<reason>)`, `<reason>` ∈
  {`NotReady`, `kubelet not heartbeating`}.
- Attention-line rollup: `(M ⇐ node <name>)` when a single node is responsible;
  `(M ⇐ K unhealthy nodes)` when more than one.
- Wording is deliberately hedged — **"likely caused by"** — honest correlation,
  not a causation claim.

## Design

### 1. `clusterhealth` — expose the hard-down nodes (structured)

`Assess` already classifies NotReady (`ready` bool) and stale-heartbeat nodes to
build its `NodeIssues` strings. Add a small structured list alongside, reusing
that exact detection — no new logic, no new collection:

```go
// DownNode is a node that is effectively down — NotReady, or Ready but its
// kubelet has stopped heartbeating. Used to attribute workload failures.
type DownNode struct {
	Name   string `json:"name"`
	Reason string `json:"reason"` // "NotReady" | "kubelet not heartbeating"
}
```

Add `DownNodes []DownNode \`json:"downNodes,omitempty"\`` to `ClusterHealth`. In
`Assess`, in the existing per-node loop:
- when `!ready` → append `DownNode{Name: n.Name, Reason: "NotReady"}`;
- in the existing stale-heartbeat branch (Ready node, stale lease) → append
  `DownNode{Name: n.Name, Reason: "kubelet not heartbeating"}`.

Only these two. **Not** cordoned, **not** pressure. This is additive to the JSON
(`omitempty`), and does not change the verdict or the existing `NodeIssues`.

### 2. `internal/rootcause` — the annotator (pure; mirrors `netpolicy`/`rollout`)

```go
// Annotate sets w.RootCause on each flagged workload that has a pod placed on a
// hard-down node. Pure and read-only; deterministic (down nodes sorted by name).
func Annotate(workloads []inventory.Workload, down []clusterhealth.DownNode)
```

Logic:
- Build a `map[nodeName]reason` and a sorted slice of down-node names (deterministic).
- For each workload: skip unless `w.Flagged()`. Collect the set of nodes its pods
  sit on (`w.Pods[].Node`). Walk the sorted down-node names; the **first** one
  that hosts a pod of this workload wins → set
  `workloads[i].RootCause = "node " + name + " (" + reason + ")"`.

Imports `inventory` + `clusterhealth` (no cycle: neither imports `rootcause`).

### 3. `inventory.Workload` — one new field

Add `RootCause string \`json:"rootCause,omitempty"\`` — an annotator-set field, in
the same family as the existing `NetworkPolicies` (netpolicy) and `Rollout`
(rollout) fields. `Assemble`/`Prioritize`/`Flagged` are unchanged.

### 4. `scan.Evaluate` — wiring

After `inventory.Prioritize`, alongside the existing `netpolicy`/`rollout`
annotators, on the post-Prioritize `result.Workloads`, passing the health
struct's down-node list:

```go
rootcause.Annotate(result.Workloads, health.DownNodes)
```

(Ordering is orthogonal to `netpolicy`/`rollout`/`createhealth`: `rootcause` sets
its own `RootCause` field and does not add a `Finding`, so it neither consumes nor
is consumed by their `!Flagged || len(Findings)>0` guards.)

### 5. `report` — render + rollup

- `printWorkload`: if `w.RootCause != ""`, print `    ↳ likely caused by ` +
  `w.RootCause` as the **first** child line, before the findings.
- `attentionLine`: among the flagged workloads, count those with `RootCause` set
  (M). If M > 0, append a parenthetical to the "N workloads failing" part. The
  distinct root-cause node is derived from `RootCause` (format is fixed:
  `strings.SplitN(rc, " (", 2)[0]` → `node worker-2`): one distinct node →
  `(M ⇐ node worker-2)`; more than one → `(M ⇐ K unhealthy nodes)`.
- JSON: the `rootCause` field is emitted per workload (omitempty) and flows to
  `--explain` (node/status text — no IPs, no secrets).

## Global constraints

- **Read-only; NO new RBAC / no new collector** — nodes, leases, and pods are
  already collected. Not `internal/collect`/`cluster`/`watch`/RBAC/Helm →
  **lightweight real-cluster smoke** gate; **minor** bump v0.28.2 → **v0.29.0**.
- **Pure & deterministic** — `rootcause.Annotate` and the `clusterhealth`
  addition are pure functions; down nodes sorted for a stable pick.
- **Always-on** — no flag; runs in `scan` and the `watch` daemon via `scan.Evaluate`.
- `internal/collect`, `internal/watch`, `explain.go` (consumes the field, no
  change needed), RBAC, and Helm stay **unchanged**. `clusterhealth`'s verdict and
  existing `NodeIssues` are unchanged.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.**

## Testing

- **`rootcause` (pure, fake objects):** a flagged workload with a pod on a
  NotReady node → `RootCause == "node <n> (NotReady)"`; a flagged workload with a
  pod on a stale-heartbeat node → reason "kubelet not heartbeating"; a workload
  whose pods are only on healthy nodes → no attribution; a **non-flagged**
  workload with a pod on a down node → skipped; **multiple** down nodes hosting
  the workload → deterministic pick (sorted, first); empty `down` → no-op.
- **`clusterhealth`:** a NotReady node and a stale-heartbeat node populate
  `DownNodes` with the right reasons; a cordoned/pressured-but-Ready node does
  **not**; the verdict and `NodeIssues` are unchanged.
- **`report`:** a workload with `RootCause` renders the `↳ likely caused by …`
  line first; the attention line shows `(M ⇐ node X)` for one node and
  `(M ⇐ K unhealthy nodes)` for several; a workload without `RootCause` is
  unaffected.
- **`scan` integration:** `Evaluate` on a fake clientset with a NotReady node
  hosting a degraded Deployment's pod yields a workload carrying `RootCause`.
- **Golden:** the golden test renders a pre-built `Input` (it does not run the
  annotator), so set `RootCause` **directly** on the fixture workloads whose pods
  sit on a down node — the fixture already has `worker-2 NotReady` and
  `worker-1 kubelet not heartbeating`, and workloads placed on them (e.g. `web` on
  `worker-1`, `billing-worker` on `worker-2`). Regenerate `golden-scan.txt` and
  confirm the `↳ likely caused by …` lines and the attention-line rollup render.
  (This mirrors how the fixture already sets annotator outputs like `Findings`.)

## Files touched

- **Modify:** `internal/clusterhealth/clusterhealth.go` (+ test) — `DownNode`, `DownNodes`, two appends in `Assess`.
- **Create:** `internal/rootcause/rootcause.go` (+ test) — `Annotate`.
- **Modify:** `internal/inventory/inventory.go` — `Workload.RootCause` field.
- **Modify:** `internal/scan/scan.go` (+ test) — the `rootcause.Annotate` wiring.
- **Modify:** `internal/report/report.go` (+ test) — `printWorkload` line + `attentionLine` rollup.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — fixture attribution + snapshot.
- **Docs:** `website/docs/features/diagnostics.md` (root-cause subsection), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md` (move to Shipped / mark the v0.29 milestone underway).

## Non-goals (this release)

Cordoned/pressure node causes; non-node root causes (shared registry, failed
PVC/StorageClass, ResourceQuota); a numeric confidence score; collapsing/hiding
dependents; any change to `Assemble`/`Prioritize`/`Flagged`, `internal/collect`,
`internal/watch`, or RBAC.
