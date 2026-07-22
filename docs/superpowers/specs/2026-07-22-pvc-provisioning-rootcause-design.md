# PVC provisioning root cause — design

**Status:** approved · **Date:** 2026-07-22 · **Type:** root-cause enrichment (Theme A)

## Problem

kubeagent's Pending-PVC check is **event-driven**: it flags a Pending PVC only
when a `ProvisioningFailed`/`FailedBinding` event is still present, and shows that
event's raw message as the cause. Two gaps follow. First, Kubernetes events expire
(~1h by default), so a PVC that has been stuck for hours loses its event and
silently stops being flagged. Second, the raw event message is not always a clean
cause. This adds a **structural** correlation — PVC → StorageClass → PV — that
names the cause definitively and catches the stuck PVC even with no event, while
never false-positiving a freshly-provisioning one.

## Behavior (approved)

For each **Pending** PVC, the cause is chosen by this precedence (structural
first, then the existing event path):

| Cause (`Reason`) | Predicate | `Detail` |
|---|---|---|
| `MissingStorageClass` | `spec.storageClassName` is set and non-empty and names a StorageClass absent from the collected StorageClasses | `references StorageClass "fast-ssd" which does not exist` |
| `NoMatchingPV` | explicitly-static claim (`spec.storageClassName == ""`) **and** no candidate PV matches (see below) | `no available PersistentVolume matches its request (10Gi, ReadWriteOnce)` |
| `ProvisioningFailed` / `FailedBinding` *(existing)* | no structural cause, but a newest failure event exists for the PVC | the event message (unchanged) |

A PVC is flagged when **any** row yields a cause. The two structural rows fire
**even with no event present**. They are mutually exclusive: `MissingStorageClass`
requires a non-empty class name; `NoMatchingPV` requires the empty string.

A **candidate PV** for `NoMatchingPV` is one where all hold:
`status.phase == Available`; `spec.claimRef == nil` (unbound); `spec.storageClassName
== ""` (a static PV); `spec.capacity[storage] >= pvc.spec.resources.requests[storage]`
(via `resource.Quantity.Cmp`); and the PV's access modes are a **superset** of the
claim's requested access modes. If at least one candidate matches, the PVC is not
flagged by `NoMatchingPV` (binding is simply in progress).

**Deliberately NOT flagged (false-positive guards):**

- A Pending PVC with a valid **dynamic** StorageClass (present in the cluster) and
  no failure event → neither structural case applies (the class exists; the claim
  is not static), so a normally-provisioning PVC is never structurally flagged.
- `spec.storageClassName == nil` **with** a default StorageClass in the cluster →
  dynamic, not flagged.
- `spec.storageClassName == nil` **without** a default → left to the event path
  only (kept out of `NoMatchingPV` to avoid the ambiguous nil-default case).
- A static claim (`""`) that **has** a matching Available PV → not flagged (binding
  in progress).

## Design

### 1. `pvchealth.Assess` — add the structural correlation

```go
// Assess flags each Pending PVC with a provisioning/binding failure, naming the
// cause: a missing StorageClass or no matching PV (structural, event-independent),
// else the newest failure event's message. Pure and read-only.
func Assess(pvcs []corev1.PersistentVolumeClaim, events []corev1.Event, storageClasses []storagev1.StorageClass, pvs []corev1.PersistentVolume) []Issue
```

For each `Pending` PVC:

1. `structuralCause(pvc, storageClasses, pvs)` → `(reason, detail, ok)`:
   - if `pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" &&
     !classExists(*name, storageClasses)` → `("MissingStorageClass",
     fmt.Sprintf("references StorageClass %q which does not exist", *name), true)`.
   - else if `pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == ""
     && !anyMatchingPV(pvc, pvs)` → `("NoMatchingPV", fmt.Sprintf("no available
     PersistentVolume matches its request (%s, %s)", requestSize(pvc),
     modeList(pvc)), true)`.
   - else `("", "", false)`.
2. If structural `ok` → build the Issue with that `Reason`/`Detail` and
   `StorageClass: storageClass(pvc)`.
3. Else fall through to the existing `newestFailureEvent` path (unchanged).
4. Else the PVC is not flagged.

Helpers (all pure): `classExists`, `anyMatchingPV` (the candidate-PV predicate
above), `requestSize` (`pvc.Spec.Resources.Requests[corev1.ResourceStorage].String()`,
or `"?"` when absent), `modeList` (join `pvc.Spec.AccessModes`, e.g.
`"ReadWriteOnce"`). New import: `storagev1 "k8s.io/api/storage/v1"`, `resource`
(`k8s.io/apimachinery/pkg/api/resource`) is reachable via the Quantity's `Cmp`
(no new import needed — Quantity is already `corev1`-adjacent; use the value's
`.Cmp`). Output stays sorted by (Namespace, Name); the `Issue` struct is unchanged.

### 2. `scan.Evaluate` — pass the two collected slices

`pvs` (`collect.PersistentVolumes`) is already collected in `Evaluate`; StorageClasses
are **not** yet collected there (platform-fact detection runs elsewhere), so add a
`collect.StorageClasses` call (the collector and the `storageclasses` RBAC grant
already exist — no new grant), then:

```go
	scs, _ := collect.StorageClasses(ctx, client)
	pvcIssues := pvchealth.Assess(pvcs, pvcEvents, scs, pvs)
```

### 3. `report` — no change

`printPVCIssues` already renders `Phase — Detail`; the structural `Detail` flows
through unchanged. The "N PVCs failing to provision" attention count naturally
includes any newly-caught event-less PVCs. JSON already serializes the fields.

## Global constraints

- **Read-only; always-on; no flag.** No new RBAC, collector, watch gauge, or
  `Result` field — StorageClasses and PVs are already collected. Touches
  `internal/pvchealth` + one line of `internal/scan` → **LIGHTWEIGHT SMOKE** gate.
  **Minor** bump v0.39.0 → **v0.40.0**; **patch** chart bump (no Helm change).
- **Pure & deterministic** — `Assess` reads only the passed objects; no clock, no
  cluster calls. Sorted output.
- **Advisory** — the cluster verdict is unchanged (the PVC section is already
  advisory, like PDB/HPA/service).
- `inventory`, `clusterhealth`, `rootcause` (the workload↔PVC attribution still
  reads `pvcIssues` — enriched Detail flows through it for free), `explain.go`,
  `--fix`, the watch daemon stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Dynamic-provisioner health (whether the CSI/provisioner pod is running — a
separate workload check already flags a down provisioner Deployment); the
`nil`-storageClassName-no-default case in `NoMatchingPV` (ambiguous — left to the
event path); WaitForFirstConsumer classification (already correctly not flagged —
it emits a Normal event, not a failure one); matching PVs by node affinity /
volume mode / selector (size + access modes + static-class is the high-signal
subset); a new JSON field or metric (the cause rides on the existing `Detail`).

## Testing

- **`pvchealth.Assess` (pure, fake objects):**
  - `MissingStorageClass`: a Pending PVC with `storageClassName: "fast-ssd"`, no
    such SC in the list, **and no event** → Issue `Reason "MissingStorageClass"`,
    Detail `references StorageClass "fast-ssd" which does not exist`.
  - `NoMatchingPV` (no candidate): a Pending static PVC (`""`) requesting `10Gi
    RWO`, PVs list empty (or only a bound/too-small/wrong-mode PV), no event →
    `Reason "NoMatchingPV"`, Detail names size + mode.
  - `NoMatchingPV` not flagged: a static PVC with a matching Available unbound
    static PV (capacity ≥ request, modes ⊇) → not flagged.
  - structural precedence over event: a `MissingStorageClass` PVC that also has a
    `ProvisioningFailed` event → structural cause wins (clean Detail, not the raw
    event message).
  - event fallback unchanged: a Pending PVC with a valid dynamic SC + a
    `ProvisioningFailed` event → Detail is the event message, `Reason
    "ProvisioningFailed"`.
  - not flagged: a Pending PVC with a valid dynamic SC and no event (fresh
    provisioning); a `nil`-storageClassName PVC with a default SC present; a Bound
    PVC (not Pending).
  - sorted output (Namespace, Name).
  - candidate-PV predicate units: too-small PV (capacity < request) → no match;
    wrong access mode (PVC wants RWX, PV offers only RWO) → no match; already-bound
    PV (`claimRef` set) → no match; dynamic PV (`storageClassName != ""`) → no
    match for a static claim.
- **`scan` integration:** a fake clientset with a Pending PVC referencing a missing
  StorageClass and **no event** → `Result.PVCIssues` contains it with the
  `MissingStorageClass` structural Detail (proves the wiring passes scs + pvs and
  that flagging no longer requires an event).
- **Golden:** update the `reports-data` fixture Issue to the structural form
  (`Reason: "MissingStorageClass"`, `Detail: references StorageClass "fast-ssd"
  which does not exist`); regenerate. The attention count is unchanged (still one
  PVC).

## Files touched

- **Modify:** `internal/pvchealth/pvchealth.go` (+ test) — `Assess` signature + structural cause + PV-match helpers.
- **Modify:** `internal/scan/scan.go` (+ test) — pass `scs`, `pvs`.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — enriched fixture Detail + regenerate.
- **Docs:** `website/docs/features/diagnostics.md` (the Pending-PVC section), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
