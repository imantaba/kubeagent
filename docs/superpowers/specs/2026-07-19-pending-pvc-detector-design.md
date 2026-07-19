# Pending-PVC / storage-provisioning check — design

**Status:** approved · **Date:** 2026-07-19 · **Type:** new advisory check (v1 core)

## Goal

Complete kubeagent's storage-diagnosis story. It already names attach-time failures
(`VolumeAttachError` / Multi-Attach); the missing half is **provision-time**: a
PersistentVolumeClaim stuck `Pending` because provisioning or binding failed. Today such
a PVC — and the pod that can't start because of it — is unexplained. A new
`pvchealth.Assess` flags the Pending PVC and names the cause from its
`ProvisioningFailed` / `FailedBinding` events (e.g. `storageclass "fast" not found`,
`no persistent volumes available for this claim and no storage class is set`, a
CSI-driver or quota failure).

## Scope

**In:** PVCs in phase `Pending` that carry a `ProvisioningFailed` or `FailedBinding`
Warning event. The event message is the cause. Rendered in **NEEDS ATTENTION** as its
own issue list (like `svchealth`/`ingresshealth`).

**Why event-based (the false-positive guard):** mirrors `VolumeAttachDetector` (which
keys off `FailedAttachVolume` events) — a Pending PVC is flagged **only if it has a
failure event**. This structurally skips the normal `WaitForFirstConsumer` state (a PVC
Pending merely because no pod consumes it yet emits only a *Normal* `WaitForFirstConsumer`
event, never `ProvisioningFailed`/`FailedBinding`), so no `volumeBindingMode` inspection
is needed and there are no false positives.

**Out of scope (YAGNI):**
- Spec-based cause inference (reconstructing "StorageClass not found" from the
  StorageClass objects). The event already names it, more accurately.
- Including PVC issues in the `--explain` API prompt (kept out for v1 — the deterministic
  text/JSON output carries them; `explain.go` is unchanged).
- Any `--fix` interaction; any change to `pvcreclaim` (the separate reclaim-policy
  advisory).
- No new CLI flag — always-on.

## Global constraints

- **Read-only; NO new RBAC.** Lists PVCs (already listed by `pvcreclaim`) and events
  (already listed by `VolumeAttachEvents`). One new event `List`, same `events` verb.
- **Core, always-on** — runs in both the CLI `scan` and the `watch` daemon via the shared
  `scan.Evaluate`. No opt-in flag, no `watch.Config` change.
- `Assess` is a **pure function** of its inputs; deterministic (newest event by
  `LastTimestamp`; issues sorted by namespace/name).
- **Advisory / P2:** appears in **NEEDS ATTENTION** but does NOT change the P1 cluster
  verdict (nodes + kube-system), consistent with `svchealth`/`ingresshealth`.
- `explain.go`, `pvcreclaim`, and all deploy/RBAC/Helm files stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. TDD.

## Design

### 1. `internal/pvchealth` — the Assess package

```go
package pvchealth

// Issue is one PVC stuck Pending because provisioning/binding failed.
type Issue struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Phase        string `json:"phase"`                  // "Pending"
	Reason       string `json:"reason"`                 // "ProvisioningFailed" | "FailedBinding"
	Detail       string `json:"detail"`                 // the event message (the cause)
	StorageClass string `json:"storageClass,omitempty"`
}

// Assess flags each Pending PVC that has a provisioning/binding failure event.
func Assess(pvcs []corev1.PersistentVolumeClaim, events []corev1.Event) []Issue
```

Logic:
- For each `pvc` with `pvc.Status.Phase == corev1.ClaimPending`:
  - `ev := newestFailureEvent(events, pvc.Namespace, pvc.Name)` — the newest event (by
    `LastTimestamp`) whose `InvolvedObject.Kind == "PersistentVolumeClaim"`, namespace and
    name match the PVC, and `Reason` is `ProvisioningFailed` or `FailedBinding`.
  - if `ev != nil`: append `Issue{Namespace, Name, Phase: "Pending", Reason: ev.Reason,
    Detail: ev.Message, StorageClass: storageClass(pvc)}`.
- Sort the result by `(Namespace, Name)` for determinism. `storageClass(pvc)` returns
  `*pvc.Spec.StorageClassName` or `""` (same helper shape as `pvcreclaim`).

`Bound`/`Lost` PVCs, and Pending PVCs with no failure event (WaitForFirstConsumer waiting,
just-created), yield no issue. The correlation is by `InvolvedObject` — robust even if the
caller passes a broader event set (e.g. the fake clientset ignoring the field selector).

### 2. `collect.PVCEvents` — the collector

```go
// PVCEvents lists Warning/Normal events involving PersistentVolumeClaims in the
// namespace (""=all). Read-only; Assess filters to the failure reasons.
func PVCEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=PersistentVolumeClaim"})
	if err != nil {
		return nil, fmt.Errorf("listing PVC events: %w", err)
	}
	return events.Items, nil
}
```

Uses the `involvedObject.kind` field selector (a supported event field) so one `List`
returns all PVC events; `Assess` filters the reasons. Non-fatal in `Evaluate` (like
`VolumeAttachEvents`).

### 3. `scan.Evaluate` wiring

`PersistentVolumeClaims` is already collected (for `pvcreclaim`). Add the event fetch and
the assess call, and a `Result.PVCIssues` field:

```go
pvcs, _ := collect.PersistentVolumeClaims(ctx, client, opts.Namespace) // existing
pvs, _ := collect.PersistentVolumes(ctx, client)                       // existing
pvcReclaim := pvcreclaim.Assess(pvcs, pvs)                             // existing
pvcEvents, _ := collect.PVCEvents(ctx, client, opts.Namespace)        // NEW
pvcIssues := pvchealth.Assess(pvcs, pvcEvents)                        // NEW
```

`scan.Result` gains `PVCIssues []pvchealth.Issue`.

### 4. Report rendering (parallels `svchealth`/`ingresshealth`)

- `report.Input` gains `PVCIssues []pvchealth.Issue`; the JSON `inventoryReport` struct
  gains `PVCIssues []pvchealth.Issue \`json:"pvcIssues,omitempty"\`` and copies it in the
  `"json"` case.
- `main.go` (which builds `report.Input` from `scan.Result`) copies `PVCIssues: result.PVCIssues`.
- New `printPVCIssues(issues []pvchealth.Issue, w io.Writer) error`, called in the
  **NEEDS ATTENTION** block (after `printIngressIssues`):
  ```
    ✗ shop/data-pvc  PersistentVolumeClaim  Pending — storageclass "fast" not found
  ```
  Format: `"  ✗ %s/%s  PersistentVolumeClaim  %s — %s\n"` with (Namespace, Name, Phase, Detail).
- `hasAttention` (report.go ~109) gains `|| len(in.PVCIssues) > 0`.
- `attentionLine` (report.go ~216) appends `"%d %s failing to provision"` (plural
  `PVC`/`PVCs`) when `len(pvcIssues) > 0`, so the `Needs attention:` summary reads e.g.
  `… · 1 PVC failing to provision`. (Pass the PVC issues into `attentionLine`, alongside
  the existing workloads/services/ingress inputs.)

### 5. What does not change

- **`explain.go`** — unchanged. PVC issues appear in text + JSON only, not the `--explain`
  prompt (v1 scope).
- **`pvcreclaim`** — untouched (separate reclaim-policy advisory in NOTES).
- **`watch` / `watch.Config`** — unchanged; the daemon inherits the check via
  `scan.Evaluate` and stays read-only.
- **RBAC / deploy / Helm** — unchanged (PVC + event `list` already granted).
- The **P1 cluster verdict** — unchanged; PVC issues are P2/advisory in NEEDS ATTENTION.

### 6. Output example

```text
Cluster: Healthy — 3/3 nodes Ready
  Needs attention: 1 PVC failing to provision

NEEDS ATTENTION
  ✗ shop/data-pvc  PersistentVolumeClaim  Pending — storageclass "fast" not found
```

## Error handling

- `PVCEvents` List error → ignored in `Evaluate` (non-fatal, like `VolumeAttachEvents`);
  `Assess` then sees no events and returns no issues.
- A Pending PVC with no failure event → no issue (WaitForFirstConsumer / just-created).
- Empty PVC list → empty issue slice (never nil-panics; `Assess` returns `[]Issue{}`).

## Testing

TDD, unit + integration (fake objects/clientset, no cluster):

- **`pvchealth_test.go`** (pure `Assess`):
  - Pending PVC + `ProvisioningFailed` event → one Issue, `Reason:"ProvisioningFailed"`,
    `Detail` = the event message, `StorageClass` set.
  - Pending PVC + `FailedBinding` event → one Issue, `Reason:"FailedBinding"`.
  - **Bound PVC** (even with a stale failure event) → no issue.
  - **Pending PVC with only a Normal `WaitForFirstConsumer` event** → no issue (the
    false-positive guard).
  - Event correlation: a failure event for PVC-A must not attach to PVC-B (match by
    `InvolvedObject` name/namespace); newest-by-`LastTimestamp` wins.
  - Deterministic ordering (two issues sorted by namespace/name).
- **`collect` test** — `PVCEvents` via fake clientset returns the seeded PVC event.
- **`scan` integration test** — `Evaluate` on a fake clientset with a Pending PVC + its
  `ProvisioningFailed` event yields exactly one `Result.PVCIssues` entry; a Bound PVC
  yields none.
- **`report` test** — `printPVCIssues` (or `PrintInventory`) renders the
  `✗ …/… PersistentVolumeClaim Pending — …` line under NEEDS ATTENTION, and the
  `Needs attention:` summary includes the PVC count.
- **Golden** — add a Pending-PVC issue to the golden fixture (`PVCIssues`) and regenerate
  `testdata/golden-scan.txt` with `-update`. Report GIF / quickstart follow the standard
  golden-change protocol at release time.

## Files touched

- **Create:** `internal/pvchealth/pvchealth.go`, `internal/pvchealth/pvchealth_test.go`
- **Modify:** `internal/collect/collect.go` (+ `collect_test.go`) — `PVCEvents`
- **Modify:** `internal/scan/scan.go` (+ `scan_test.go`) — fetch events, assess, `Result.PVCIssues`
- **Modify:** `internal/report/report.go` (+ `report_test.go`/`golden_test.go`) — `Input`/`inventoryReport`
  fields, `printPVCIssues`, `hasAttention`, `attentionLine`, golden fixture + snapshot
- **Modify:** `main.go` — map `result.PVCIssues` into `report.Input`
- **Docs:** `website/docs/features/diagnostics.md` (new subsection + `## Status` list),
  `CHANGELOG.md` (`### Added`), `website/docs/quickstart.md`, `README.md`.

## Non-goals recap

Spec-based cause inference; `--explain` inclusion; `--fix`; any CLI flag; any change to
`pvcreclaim`, `explain.go`, `watch`, or RBAC beyond the read-only PVC-event `List`.
