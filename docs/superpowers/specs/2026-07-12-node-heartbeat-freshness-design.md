# Node Heartbeat Freshness — Design

**Date:** 2026-07-12
**Status:** Approved

## Goal

Catch a kubelet that has gone dark **before** the control plane flips its node to
`NotReady`. Each node renews a `Lease` in `kube-node-lease` roughly every 10s; the
node-monitor only marks the node `NotReady` after `node-monitor-grace-period`
(default ~40s) of missed renewals. Reading lease `renewTime` staleness surfaces
the "kubelet stopped heartbeating" case (crash, hang, cert-expiry, wedged
runtime) earlier and pins the cause.

Read-only; on by default (plain read-only RBAC). A stale-lease node is a
`clusterhealth` node issue — it flips the verdict to `Degraded`, consistent with
`NotReady`/pressure. Adds the daemon gauge `kubeagent_nodes_stale_heartbeat` so
you can alert before a node goes `NotReady`.

## Data source

Node heartbeats live in `coordination.k8s.io/v1` `Lease` objects in the
`kube-node-lease` namespace — one per node, named after the node, with
`spec.renewTime` (a `metav1.MicroTime`) advanced by the kubelet on each renewal.
Leases are **not** currently collected and `leases` is **not** in the RBAC.

## What is flagged

Staleness is `now − renewTime`, where `now` is the scan's clock (the daemon runs
in-cluster and is NTP-synced; a `scan` from a skewed laptop clock is the caveat,
absorbed by the threshold — documented, not code-handled).

For each node in the node list:

- **Ready node, lease stale beyond the threshold** → issue `kubelet not
  heartbeating (lease Ns stale)`. This is the new signal: the kubelet is dark but
  the node-monitor has not yet flipped `Ready`.
- **Ready node, no lease object at all (or a lease whose `renewTime` is nil)** →
  issue `no kubelet lease`. (Rare; a brand-new node that is Ready before its
  lease exists is a brief race — accepted as a low-risk false positive.)
- **NotReady node** → **no** heartbeat issue is added. The existing `NotReady`
  line already flags it (its `NodeReady` message usually already says "Kubelet
  stopped posting node status"); a second finding would be redundant.

A flagged node contributes to `ClusterHealth.NodeIssues` (→ verdict `Degraded`)
and increments `ClusterHealth.NodesStaleHeartbeat`.

Threshold default **40s** (the kube default `node-monitor-grace-period`),
tunable via `--node-heartbeat-threshold` (Go duration) and the
`KUBEAGENT_NODE_HEARTBEAT_THRESHOLD` env var for the daemon.

## Components

### `internal/collect`

New helper, mirroring `Nodes`:

```go
func NodeLeases(ctx context.Context, client kubernetes.Interface) ([]coordinationv1.Lease, error)
```

Lists `client.CoordinationV1().Leases("kube-node-lease").List(...)`. Best-effort
at the call site (a List error yields no leases and never fails the scan).

### `internal/clusterhealth`

`Assess` is extended to also consider node leases against a staleness threshold at
the current time. To keep the signature readable, the heartbeat inputs are grouped:

```go
type Heartbeat struct {
	Leases    []coordinationv1.Lease
	Now       time.Time
	Threshold time.Duration
}

func Assess(nodes []corev1.Node, hb Heartbeat, workloads []inventory.Workload) ClusterHealth
```

- A `map[nodeName]Lease` is built from `hb.Leases`.
- `nodeHealth` (or a helper it calls) receives the node's lease + `hb.Now` +
  `hb.Threshold` and appends the heartbeat issue for a **Ready** node whose lease
  is stale or missing, per the rules above. A `Threshold <= 0` disables the check
  (defensive; production always passes the default).
- `ClusterHealth` gains `NodesStaleHeartbeat int` (`json:"nodesStaleHeartbeat"`,
  omitempty), incremented per flagged node. The existing `NodeIssues` /
  verdict / `NodesReady` logic is otherwise unchanged.

The staleness in the message is the `now − renewTime` duration rounded to the
second — `(now.Sub(renewTime)).Round(time.Second).String()` — giving `48s` /
`3m12s`.

### `internal/scan`

`Options` gains `NodeHeartbeatThreshold time.Duration`. `Evaluate` collects
`leases, _ := collect.NodeLeases(ctx, client)` and calls
`clusterhealth.Assess(nodes, clusterhealth.Heartbeat{Leases: leases, Now:
time.Now(), Threshold: opts.NodeHeartbeatThreshold}, workloads)`. No other
`Result` field changes (the findings ride the existing `Health` field).

### `main.go` / `internal/watch`

- `main.go` (scan): `--node-heartbeat-threshold` flag (Go duration, default
  `40s`), passed into `scan.Options`.
- `internal/watch` (daemon): reads `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD` (default
  `40s`) into its scan options, and `metrics.go` renders the gauge
  `kubeagent_nodes_stale_heartbeat` from `Result.Health.NodesStaleHeartbeat`
  (set in the success path, last-good preserved on error — same pattern as the
  other gauges).

### RBAC

Add a `coordination.k8s.io` rule granting `leases` `get`/`list`/`watch` to BOTH
`deploy/rbac.yaml` and the Helm ClusterRole. On by default (plain read-only).

## Scope boundaries

- Read-only; on by default; not wired into `--explain`.
- Clock-skew is documented, not code-corrected (daemon is in-cluster/synced; the
  generous default threshold absorbs small `scan`-client skew). The
  freshest-lease-as-reference alternative is explicitly out of scope for v1.
- The **expected-node baseline** (nodes that never registered / dropped out of
  the node list) and **proactive kubelet `/healthz` probing** via `nodes/proxy`
  are separate future features — not in this spec.
- No change to the CrashLoop/OOM/etc. detectors or the JSON schema beyond the
  additive `nodesStaleHeartbeat` field.

## Testing

- `clusterhealth` unit tests (fake nodes + fake leases + a fixed `now`):
  - Ready node, fresh lease → no issue, verdict unaffected.
  - Ready node, lease stale beyond threshold → `kubelet not heartbeating` issue,
    verdict `Degraded`, `NodesStaleHeartbeat == 1`.
  - Ready node, no lease → `no kubelet lease` issue.
  - NotReady node with a stale lease → **only** the `NotReady` issue (no
    duplicate heartbeat issue).
  - Threshold boundary: staleness just under the threshold → clean; just over →
    flagged.
  - `Threshold <= 0` → check disabled.
- `collect.NodeLeases` round-trips a fake Lease via the fake clientset.
- `internal/watch/metrics` test asserts `kubeagent_nodes_stale_heartbeat`
  reflects `Health.NodesStaleHeartbeat`.

## Docs

- `CHANGELOG.md` (`## [Unreleased]` → `### Added`).
- `website/docs/features/diagnostics.md`: a node-heartbeat subsection.
- `website/docs/features/watch-mode.md`: document the
  `kubeagent_nodes_stale_heartbeat` gauge and the
  `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD` env.
- `website/docs/roadmap.md`: a Shipped bullet.
- `README.md`: one-line mention.

Exact names to use verbatim: flag `--node-heartbeat-threshold`; env
`KUBEAGENT_NODE_HEARTBEAT_THRESHOLD`; gauge `kubeagent_nodes_stale_heartbeat`;
JSON field `nodesStaleHeartbeat`; issue strings `kubelet not heartbeating (lease
Ns stale)` and `no kubelet lease`.
