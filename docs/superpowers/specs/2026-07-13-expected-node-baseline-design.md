# Expected-Node Baseline — Design

**Date:** 2026-07-13
**Status:** Approved

## Goal

Detect a node that **should** be in the cluster but isn't — the one node-failure
class kubeagent can't see today: a kubelet that never registered its node, or a
node that dropped out of the cluster entirely. kubeagent is stateless per scan,
so it needs a declared expectation to diff against. The user declares the node
names they expect; kubeagent flags each declared name that has **no `Node`
object** in the cluster.

Read-only; **opt-in** (off until a list is declared). An expected-absent node is
a `clusterhealth` node issue — it degrades the verdict, alongside
NotReady/pressure/stale-heartbeat. Adds the daemon gauge
`kubeagent_nodes_expected_absent`. No new RBAC (nodes are already read).

## What is flagged

For each declared expected name that is **absent** (no `Node` object with that
name in the collected node list) → node issue `expected but absent from the
cluster`. This single check covers both:

- a node that **never registered** (declared, never appeared), and
- a node that **dropped out** (was there, now deleted).

Semantics:

- **"Absent" means the Node object does not exist.** A node that exists but is
  `NotReady` is **present** for this check — its health is flagged by the
  NotReady / stale-heartbeat checks. This check is purely about node-object
  presence.
- **Missing only.** A node present in the cluster but *not* in the declared list
  is **not** flagged (unexpected/rogue-node detection is out of scope).
- The declared list is cleaned once: each name is trimmed of surrounding
  whitespace, blanks are dropped, and duplicates are collapsed. Matching is
  exact (case-sensitive — Kubernetes node names are). An empty cleaned list
  disables the check.
- Absent names are reported in sorted order for stable output/tests.

## Components

### `internal/clusterhealth`

`Assess` gains an `expected []string` parameter (the same integration point as
the heartbeat inputs). New signature:

```go
func Assess(nodes []corev1.Node, hb Heartbeat, expected []string, workloads []inventory.Workload) ClusterHealth
```

- Build `present` = set of `node.Name` for the collected nodes.
- Clean `expected` (trim / drop-blank / dedup) into a sorted unique list; for
  each name not in `present`, append `name + " expected but absent from the
  cluster"` to `ch.NodeIssues` and increment `ch.NodesExpectedAbsent`.
- `ClusterHealth` gains `NodesExpectedAbsent int`
  (`json:"nodesExpectedAbsent,omitempty"`).
- The existing per-node loop, system-issue loop, and Healthy/Degraded decision
  are unchanged; because expected-absent entries land in `NodeIssues`, they
  degrade the verdict through the existing logic.
- The cleaning helper (trim/blank/dedup/sort) lives here and is unit-tested;
  callers may pass a raw comma-split slice.

The `NodeIssues` entries render as `✗ node <entry>` (existing `printHeader`
format), so an absent node reads `✗ node nova-worker-2 expected but absent from
the cluster`.

### `internal/scan`

`Options` gains `ExpectedNodes []string`. `Evaluate` passes it through:

```go
health := clusterhealth.Assess(nodes, clusterhealth.Heartbeat{...}, opts.ExpectedNodes, workloads)
```

No new collector and no `Result`/JSON-schema change beyond the additive
`nodesExpectedAbsent` field on `ClusterHealth`.

### `main.go` / `internal/watch`

- `main.go` (scan): `--expected-nodes` — a comma-separated string flag ("names of
  nodes expected in the cluster; a declared node with no Node object is flagged
  (comma-separated)"). Split on `,` into `Options.ExpectedNodes` (raw; clusterhealth
  cleans). Add `[--expected-nodes a,b,…]` to the scan usage string.
- `internal/watch` (daemon): `Config.ExpectedNodes []string` from
  `KUBEAGENT_EXPECTED_NODES` (comma-split), passed into the daemon's
  `scan.Options`. `metrics.go` renders `kubeagent_nodes_expected_absent` from
  `Result.Health.NodesExpectedAbsent` in the success path (last-good preserved on
  error — same pattern as the other gauges).

### RBAC

None. Nodes are already granted (`""`/`nodes` `get`/`list`/`watch`).

## Scope boundaries

- Read-only; opt-in; not wired into `--explain`.
- Flags **missing** declared nodes only; never flags unexpected/extra nodes.
- The name-list model targets clusters with **stable** node names (self-hosted /
  bare-metal / RKE2). Autoscaled clusters whose node names churn would
  false-positive — documented; a count-based variant is a possible future
  feature, out of scope here.
- Signature ripple: `Assess` adds `expected []string`; the heartbeat-era test
  calls (`Assess(nodes, Heartbeat{}, workloads)`) and the scan call site update
  to the new 4-arg form (`… Heartbeat{}, nil, …`). No behavior change when
  `expected` is nil/empty.

## Testing

- `clusterhealth` unit tests (fake nodes + expected list):
  - declared node present → no issue, verdict unaffected.
  - declared node absent → `expected but absent` issue, verdict `Degraded`,
    `NodesExpectedAbsent == 1`.
  - declared node exists but `NotReady` → **not** flagged by this check (present);
    only the NotReady issue appears.
  - empty / whitespace-only list → check disabled (no issues).
  - cleaning: `[" a ", "", "a", "b"]` with only `a` present → one absent issue for
    `b` (trim + dedup), not two.
  - multiple absent → issues in sorted name order.
- `collect`/`scan`: a `scan.Evaluate` test (fake clientset with node `a` present,
  `Options{ExpectedNodes: ["a","b"]}`) → `Health.NodesExpectedAbsent == 1`,
  verdict `Degraded`; and `Options{}` (no list) → count 0.
- `internal/watch/metrics` test asserts `kubeagent_nodes_expected_absent`
  reflects `Health.NodesExpectedAbsent`.

## Docs

- `CHANGELOG.md` (`## [Unreleased]` → `### Added`).
- `website/docs/features/diagnostics.md`: an "Expected-node baseline" subsection.
- `website/docs/features/watch-mode.md`: the `kubeagent_nodes_expected_absent`
  gauge and the `KUBEAGENT_EXPECTED_NODES` env.
- `website/docs/roadmap.md`: a Shipped bullet.
- `README.md`: one-line mention.

Exact names to use verbatim: flag `--expected-nodes`; env
`KUBEAGENT_EXPECTED_NODES`; gauge `kubeagent_nodes_expected_absent`; JSON field
`nodesExpectedAbsent`; issue string `expected but absent from the cluster`.
