# PVC Reclaim-Policy Check — Design

**Date:** 2026-07-08
**Status:** Approved

## Goal

`kubeagent scan` should surface a new section listing PersistentVolumeClaims
whose bound PersistentVolume has `persistentVolumeReclaimPolicy: Delete` — the
data-loss-prone case where deleting the PVC (or PV) destroys the underlying
storage. The watch daemon exposes the count as a Prometheus gauge.

## Data source

Reclaim policy lives on the **PV**, not the PVC:
`PersistentVolume.spec.persistentVolumeReclaimPolicy` (`Retain` / `Delete` /
`Recycle`). A bound PVC references its PV via `PersistentVolumeClaim.spec.volumeName`.

We read the reclaim policy from the **bound PV** — the authoritative value that
actually governs data loss (accurate even for statically-provisioned or
hand-edited PVs, which a StorageClass-based lookup would get wrong).

Consequence: only **Bound** PVCs are considered (an unbound/Pending PVC has no
PV and therefore no reclaim policy to read).

This requires two new read-only List calls — PVCs (namespaced) and PVs
(cluster-scoped) — and correspondingly two new RBAC grants. StorageClasses are
already collected and used only to annotate the class name for display.

## Warn/flag rule

A PVC is listed when it is **Bound** AND its bound PV's
`persistentVolumeReclaimPolicy == Delete`. Presence in the list *is* the
advisory — there is no sub-threshold. `Delete` is the common default for dynamic
provisioners, so the list can be long; it is an audit inventory, not an error.

## Components

### `internal/pvcreclaim` (new, pure function)

```go
package pvcreclaim

type PVCReclaim struct {
    Namespace    string `json:"namespace"`
    Name         string `json:"name"`
    PV           string `json:"pv"`
    StorageClass string `json:"storageClass,omitempty"`
    Capacity     string `json:"capacity,omitempty"`
}

type Report struct {
    PVCs  []PVCReclaim `json:"pvcs"`
    Count int          `json:"count"`
}

func Assess(pvcs []corev1.PersistentVolumeClaim, pvs []corev1.PersistentVolume) Report
```

- Build a map `pvName -> reclaimPolicy` from the PVs.
- For each PVC: if `Status.Phase == Bound` and `Spec.VolumeName != ""` and the
  bound PV's reclaim policy is `Delete`, emit a `PVCReclaim` row.
- `StorageClass` from `pvc.Spec.StorageClassName` (may be nil → empty).
- `Capacity` from `pvc.Status.Capacity[storage]` (human-readable; empty when
  absent).
- `Count = len(PVCs)`.

Unit-testable with fake PVCs + PVs; no cluster needed.

### `internal/collect`

Two new functions, mirroring the existing List helpers:

```go
func PersistentVolumeClaims(ctx, client, namespace) ([]corev1.PersistentVolumeClaim, error)
func PersistentVolumes(ctx, client) ([]corev1.PersistentVolume, error)   // cluster-scoped
```

### `internal/scan`

`scan.Result` gains `PVCReclaim pvcreclaim.Report`. `Evaluate` lists PVCs (scoped
to `opts.Namespace`) and PVs (cluster-scoped) and calls `pvcreclaim.Assess`.
These are best-effort like the other secondary collectors (a List error yields an
empty report, never fails the scan).

### `internal/report` (text + JSON)

New text section:

```
PVCs with reclaim policy Delete:
  ⚠ shop/data-0  pv pvc-abc123  class standard  8Gi
  ⚠ shop/cache   pv pvc-def456  class fast      2Gi
```

JSON output serializes the report under `pvcReclaim`. The section is skipped when
the report is nil or has zero PVCs.

### `internal/watch/metrics.go` (daemon)

New gauge `kubeagent_pvcs_reclaim_delete` = `Report.Count`.
HELP: "PVCs whose bound PV has reclaimPolicy Delete."
Updated per reconcile from the shared `scan.Evaluate` result.

### RBAC

`deploy/rbac.yaml` and `deploy/helm/kubeagent/templates/clusterrole.yaml` gain, in
the core (`""`) API group, `persistentvolumeclaims` and `persistentvolumes` with
verbs `get`/`list`/`watch`. Still strictly read-only.

## Scope boundaries

- **Advisory only** — does not change the cluster verdict, `kubeagent_cluster_healthy`,
  or the scan exit code.
- **Not** wired into `--explain`.
- Only **Bound** PVCs with a `Delete`-policy PV are listed.

## Testing

- `pvcreclaim` unit tests (fake PVCs + PVs): bound + Delete PV → listed with
  correct pv/class/capacity; bound + Retain PV → not listed; unbound/Pending PVC
  → not listed; PVC bound to a PV name with no matching PV object → not listed
  (defensive); count matches.
- Report-rendering test for the new section (a Delete row present; section
  omitted when empty).
- Daemon metric wiring test mirroring the existing `watch` metrics test.

## Docs

- `CHANGELOG.md` `[Unreleased]`.
- Website `features/diagnostics.md` (scan section) and `features/watch-mode.md`
  (new metric).
- `README.md` feature list.
- `deploy/README.md` security notes already state get/list/watch-only; the new
  resources fit that description (no wording change required beyond the RBAC).
