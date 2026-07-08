# Node Reservations Check — Design

**Date:** 2026-07-08
**Status:** Approved

## Goal

`kubeagent scan` should surface each node's kubelet resource reservations
(`system-reserved` + `kube-reserved` + `eviction-hard`, observed in aggregate)
and warn when a node reserves **no memory** — a kubelet configuration that lets
OS/kubelet memory pressure destabilise the node.

## Problem & data source

The three kubelet settings (`systemReserved`, `kubeReserved`, `evictionHard`)
are not plain fields on the Node object. Reading them individually requires the
kubelet `/configz` endpoint via the `nodes/proxy` subresource — a broader RBAC
grant than kubeagent's strict `get`/`list`/`watch`.

Instead we observe their **aggregate effect**, which *is* on the Node object:

```
reserved = Node.Status.Capacity − Node.Status.Allocatable
```

`allocatable = capacity − kube-reserved − system-reserved − eviction-hard`, so
the delta is exactly the combined reservation. When `capacity == allocatable`
for a resource, nothing is reserved.

This keeps the check strictly read-only with **no new RBAC** — `collect.Nodes`
already lists full Node objects.

Trade-off (accepted): we show aggregate reserved cpu/mem per node, not the three
named fields separately.

## Warn rule

`WARNING` when **memory** reserved == 0 (allocatable memory == capacity memory).

Rationale: even a default `evictionHard` (`memory.available<100Mi`) reserves some
memory, so `mem reserved == 0` means truly nothing is configured — the dangerous
case where the kubelet/OS can be OOM'd. CPU reservation is commonly left unset
even on well-run clusters, so CPU reserved is **shown but never warned** (avoids
false positives).

## Components

### `internal/nodereserve` (new, pure function)

```go
package nodereserve

type NodeReservation struct {
    Name        string `json:"name"`
    Role        string `json:"role,omitempty"`   // "control-plane"/"worker" from node labels
    CPUReserved string `json:"cpuReserved"`       // "200m", "0"
    MemReserved string `json:"memReserved"`       // "800Mi", "0"
    Warning     bool   `json:"warning"`           // mem reserved == 0
}

type Report struct {
    Nodes     []NodeReservation `json:"nodes"`
    WarnCount int               `json:"warnCount"`
}

func Assess(nodes []corev1.Node) Report
```

- Reserved per resource = `Capacity − Allocatable`; negative deltas clamped to 0.
- `MemReserved == 0` sets `Warning` and increments `WarnCount`.
- `Role` derived from the `node-role.kubernetes.io/*` labels (falls back to
  "worker" when none present).
- CPU/memory formatted with the same helpers style as `internal/resources`.

Unit-testable with fake nodes; no cluster needed.

### `internal/scan` wiring

`scan.Result` gains `NodeReserve nodereserve.Report`. `Evaluate` computes it from
the nodes it already collects — no new API calls.

### `internal/report` (text + JSON)

New "Node reservations" section in the text report:

```
Node reservations
  nova-worker-1  cpu=0    mem=0      WARNING: kubelet reserves no memory
  nova-worker-2  cpu=200m mem=800Mi  OK
```

JSON output serializes `Report` under `nodeReserve`.

### `internal/watch/metrics.go` (daemon)

New gauge `kubeagent_nodes_without_reservations` = `Report.WarnCount`.
HELP: "Nodes whose kubelet reserves no memory (allocatable == capacity)."
Updated per reconcile from the shared `scan.Evaluate` result.

## Scope boundaries

- **Advisory only** — does not flip `cluster_healthy` or the scan exit code. It
  is a warning, not an active failure.
- **Not** wired into `--explain` (LLM) — out of scope for this feature.
- **No RBAC change** — `nodes` `get`/`list` already granted.

## Testing

- `nodereserve` unit tests (fake nodes): mem==cap → warn; mem reserved > 0 → ok;
  cpu 0 / mem > 0 → ok (not warned); negative delta clamps to 0; role parsing.
- Report-rendering test for the new section (warning and OK rows).
- Daemon metric wiring test mirroring existing `watch` metrics tests.

## Docs

- `CHANGELOG.md` `[Unreleased]`.
- Website `features/diagnostics.md` (scan section) and `features/watch-mode.md`
  (new metric).
- `README.md` feature list.
