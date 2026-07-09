# Node & PVC Disk-Usage Check — Design

**Date:** 2026-07-09
**Status:** Approved

## Goal

Give kubeagent an **early** warning that a node's root filesystem or a
PersistentVolumeClaim is running out of space — before the kubelet's
`DiskPressure` eviction signal (which kubeagent already surfaces, but only at the
~85%+ eviction threshold). Opt-in, read-only.

## Data source

The kubelet `/stats/summary` endpoint, reached read-only through the API server's
`nodes/proxy` subresource: `GET /api/v1/nodes/<node>/proxy/stats/summary`. This
is the same raw-GET pattern as `collect.NodeMetrics` (`RESTClient().Get().
AbsPath(...).DoRaw(ctx)`), and we hand-parse only the fields we need into a small
local struct — **no new dependency** (we do not import `k8s.io/kubelet`).

Fields read:
- `.node.fs.usedBytes` / `.node.fs.capacityBytes` — node root filesystem.
- `.pods[].volume[]` entries that carry a `.pvcRef` — each gives
  `usedBytes` / `capacityBytes` and the PVC `name`/`namespace`.

One GET per node. Best-effort: a node whose kubelet is unreachable or whose
`nodes/proxy` GET is forbidden is skipped (the scan still succeeds).

`imagefs` is **not** measured separately (it is often the same filesystem as
nodefs; when separate, DiskPressure still catches its eviction case).

## Opt-in gating (never on by default)

`nodes/proxy` is a broader grant than kubeagent's strict `get`/`list`/`watch`
(it can proxy arbitrary kubelet endpoints), so the check is **off by default** and
adds no cost or RBAC unless enabled.

- **CLI:** `--disk-usage` turns it on; `--disk-threshold=0.80` (float, 0–1) tunes
  the warn ratio (default `0.80`).
- **Daemon:** env `KUBEAGENT_DISK_USAGE=true` enables it; `KUBEAGENT_DISK_THRESHOLD`
  (default `0.80`) tunes it.
- `scan.Options` gains `DiskUsage bool` and `DiskThreshold float64`. When
  `DiskUsage` is false, `Evaluate` makes **no** per-node proxy calls.

## Warn rule

A volume is reported when `usedBytes / capacityBytes >= threshold` (default 0.80).
`capacityBytes == 0` volumes are skipped (avoids divide-by-zero and unsized
volumes). This is an **early warning below** the DiskPressure eviction point — the
two are complementary.

## Components

### `internal/collect`

```go
// NodeStats fetches the kubelet summary for one node via nodes/proxy (read-only).
// A forbidden or unreachable node yields (zero, false, nil) — non-fatal.
func NodeStats(ctx context.Context, client kubernetes.Interface, node string) (diskusage.NodeSummary, bool, error)
```

Parses the summary JSON with a minimal local struct (mirroring `parseNodeMetrics`).
`NodeSummary` is defined in `internal/diskusage` (below) so both packages share it
without a cycle: `collect` imports `diskusage` types.

### `internal/diskusage` (new, pure)

```go
package diskusage

// NodeSummary is the slice of kubelet /stats/summary that we consume.
type NodeSummary struct {
    Node    string          // node name (filled by the caller)
    FSUsed  int64
    FSCap   int64
    Volumes []PVCVolume     // volumes with a pvcRef
}
type PVCVolume struct {
    Namespace string
    Name      string
    Used      int64
    Cap       int64
}

type VolumeUsage struct {
    Kind          string  `json:"kind"`      // "node" | "pvc"
    Node          string  `json:"node,omitempty"`
    Namespace     string  `json:"namespace,omitempty"`
    Name          string  `json:"name"`
    UsedBytes     int64   `json:"usedBytes"`
    CapacityBytes int64   `json:"capacityBytes"`
    Ratio         float64 `json:"ratio"`
}
type Report struct {
    Over      []VolumeUsage `json:"over"`      // ratio >= threshold, highest first
    Threshold float64       `json:"threshold"`
}

func Assess(stats []NodeSummary, threshold float64) Report
```

`Assess` builds a `VolumeUsage` for each node fs and each PVC volume whose ratio
`>= threshold` (skipping `Cap == 0`), sorts `Over` by ratio descending (then
name ascending for ties), and sets `Threshold`. Pure; unit-testable with fake
`NodeSummary` slices.

### `internal/scan`

`Result` gains `DiskUsage diskusage.Report`. When `opts.DiskUsage`, `Evaluate`
iterates the already-collected `nodes`, calls `collect.NodeStats` per node
(best-effort), and calls `diskusage.Assess(summaries, opts.DiskThreshold)`. When
off, `Result.DiskUsage` is the zero `Report` and no proxy call is made.

### `internal/report` (text + JSON)

Over-threshold volumes render in the **NEEDS ATTENTION** zone with `✗` (impending
failure), only when the report is non-empty (i.e. the check ran and found
something). Format:

```
✗ node 195.43.22.161  disk 84% full (168Gi/200Gi)
✗ pvc clickhouse/data-ekb-clickhouse-shard0-0  92% full (46Gi/50Gi)
```

Percent is `round(ratio*100)`; sizes reuse a Gi/Mi formatter. JSON serializes the
report under `diskUsage` (omitempty; present with the full `over` list when the
check ran). Advisory only — it does **not** change the cluster verdict,
`kubeagent_cluster_healthy`, or the scan exit code, and is not sent to `--explain`.

### `internal/watch/metrics.go` (daemon)

When disk usage is enabled, expose:
- `kubeagent_node_fs_usage_ratio{node="…"}` — one gauge per node (bounded), 0–1.
- `kubeagent_volumes_over_disk_threshold` — count of node+PVC volumes at/over the
  threshold.

No per-PVC gauge (avoids unbounded label cardinality). These appear only when the
daemon has disk usage enabled.

### CLI / daemon wiring (`main.go`, `internal/watch`)

- `main.go` scan flag set: `--disk-usage` (bool) and `--disk-threshold` (float,
  default 0.80); pass into `scan.Options` and expose the flag in the usage string.
- `internal/watch`: read `KUBEAGENT_DISK_USAGE` / `KUBEAGENT_DISK_THRESHOLD` into
  the watch `Config`, thread into `scan.Options`, and feed the metrics.

### RBAC

- `deploy/rbac.yaml` and the Helm ClusterRole stay **unchanged** (get/list/watch).
- New `deploy/rbac-diskusage.yaml`: a ClusterRole (or additive rule) granting
  `nodes/proxy` `get`, bound to the kubeagent ServiceAccount — applied only by
  operators who enable the daemon disk check.
- Helm: a `diskUsage.enabled: false` value that, when true, (a) adds the
  `nodes/proxy` rule to the rendered ClusterRole and (b) sets
  `KUBEAGENT_DISK_USAGE=true` on the Deployment.

## Testing

- `diskusage.Assess`: node fs over/under threshold; PVC over/under; ratio math and
  percentage; `Cap == 0` skipped; sort order (ratio desc, name asc); empty input.
- `collect.NodeStats`: fake `RESTClient` / RoundTripper returning canned
  `/stats/summary` JSON → correct `NodeSummary`; a 403/again yields
  `(zero, false, nil)`.
- `report`: NEEDS ATTENTION disk lines when the report has entries; nothing when
  empty; JSON `diskUsage` present when the check ran; the section never appears
  when disk usage was off.
- `watch/metrics`: the node fs ratio gauge and the over-threshold count render
  when enabled.
- `main.go` / `watch`: `--disk-usage` / `--disk-threshold` and the env vars parse
  and thread through.

## Scope boundaries

- **Opt-in only** — zero cost, zero extra RBAC when off.
- **Advisory** — no verdict/exit-code change; not in `--explain`.
- **No `--fix`** — disk-full has no safe automatic remediation.
- nodefs + PVCs only (no separate imagefs line); no per-PVC daemon gauge.

## Docs

- `CHANGELOG.md` `[Unreleased]`.
- Website `features/diagnostics.md` (the check + `--disk-usage`/`--disk-threshold`)
  and `features/watch-mode.md` (the new gauges + how to enable in the daemon).
- `deploy/README.md`: document the `nodes/proxy` add-on and the Helm
  `diskUsage.enabled` value.
- `README.md` feature list.
