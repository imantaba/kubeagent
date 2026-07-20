# "Can't create pods" / FailedCreate check — design

**Status:** approved · **Date:** 2026-07-20 · **Type:** new check (v1 core)

## Goal

Close a real blind spot: every kubeagent detector operates on **pods**, so a workload
stuck below its desired replicas because pod **creation** is being denied — a
ResourceQuota exceeded, a LimitRange violation, or an admission webhook rejection — has
**no pods to diagnose** and shows only `0/N Degraded` with no cause. A new
`createhealth.Annotate` reads the controller's `FailedCreate` events and names the cause
on the workload.

## Scope

**In:** a `Flagged()` workload (`Ready < Desired`) with **no existing finding** whose
controller has a recent `FailedCreate` Warning event. Controllers: **Deployment**
(event lands on its ReplicaSet → resolve to the Deployment), **StatefulSet**, **DaemonSet**
(event on the controller directly).

**Out of scope (YAGNI):**
- Jobs (a quota-blocked Job is rarer; `batchhealth` owns the Job path; a Job never gets a
  run `Failed` condition when its pod can't be created, but this is a deferred edge).
- A bare ReplicaSet with **0 pods** (not seeded as a workload, so nothing to attach to);
  a ReplicaSet *with* pods still resolves.
- `--explain` special-casing — the `FailedCreate` finding flows to `--explain` like any
  other workload finding (its Evidence is k8s admission/quota status text, no IPs/secrets,
  consistent with `ImagePullBackOff`/`VolumeAttachError` messages).
- No CLI flag — always-on.

## Global constraints

- **Read-only; NO new RBAC.** One new event `List` (`reason=FailedCreate`); the `events`
  list verb is already granted.
- **Core, always-on** — runs in both the CLI `scan` and the `watch` daemon via the shared
  `scan.Evaluate`. No opt-in flag, no `watch.Config` change.
- `createhealth.Annotate` is a **pure function** of its inputs; deterministic (newest
  event by `LastTimestamp`).
- `report.go`, `explain.go`, `inventory` (no `Assemble`/`Prioritize` change), `watch`, and
  all deploy/RBAC/Helm files stay **unchanged**. The only existing code touched is the
  `scan.Evaluate` wiring (fetch + one `Annotate` call) and the new `collect.FailedCreateEvents`.
- **No `Co-Authored-By: Claude` trailer** on any commit. TDD.

## Design

### 1. `internal/createhealth` — the Annotate package

Mirrors `netpolicy.Annotate`/`rollout.Annotate` (walk the assembled+prioritized workloads,
mutate in place). Runs **after `Prioritize`**, because a Degraded workload is already
`Flagged()` and shown — no `Prioritize` change is needed.

```go
package createhealth

// Annotate appends a "FailedCreate" finding to each Degraded workload with no existing
// finding whose controller has a FailedCreate event (pod creation denied). Pure/read-only.
func Annotate(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, events []corev1.Event)
```

Logic:
- Build `rsToDeploy["ns/rsName"] = deploymentName` from `replicaSets` (controller ownerRef
  with `Kind == "Deployment"`, via a local `ownedByDeployment` helper).
- Build `byWorkload["Kind/ns/name"] = *newest FailedCreate event`: for each event with
  `Reason == "FailedCreate"`, resolve `workloadKeyForEvent`:
  - `InvolvedObject.Kind == "ReplicaSet"`: if `rsToDeploy` has it → `"Deployment/ns/dep"`,
    else `"ReplicaSet/ns/rsName"`.
  - `InvolvedObject.Kind` in {`StatefulSet`, `DaemonSet`} → `"Kind/ns/name"`.
  - else → skip.
  Keep the newest by `LastTimestamp` per key.
- For each workload: `if !w.Flagged() || len(w.Findings) > 0 { continue }` (the
  `netpolicy` guard — only a flagged workload with nothing else explaining it). Look up
  `byWorkload["w.Kind/w.Namespace/w.Name"]`; if present, append a `Finding`.

`Finding`: `Pod: "ns/name"` (workload identity — appended directly, not via the pod-key
match), `Issue: "FailedCreate"`, `Reason: "the controller cannot create pods — " +
classifyCreateFailure(ev.Message)`, `Evidence: ev.Message`.

`classifyCreateFailure(msg)` (case-insensitive substring, first match):
- `exceeded quota` → `blocked by a ResourceQuota`
- `admission webhook` → `rejected by an admission webhook`
- `limitrange` / `minimum ` / `maximum ` → `violates a LimitRange`
- `forbidden` → `forbidden by admission`
- else → `pod creation is failing`

Imports: `internal/inventory` (Workload), `internal/diagnose` (Finding), `appsv1`,
`corev1`, `strings`. No cycle (inventory doesn't import createhealth).

### 2. `collect.FailedCreateEvents`

```go
func FailedCreateEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=FailedCreate"})
	if err != nil {
		return nil, fmt.Errorf("listing FailedCreate events: %w", err)
	}
	return events.Items, nil
}
```

Mirrors `VolumeAttachEvents`; non-fatal in `Evaluate`. (`Annotate` filters `Reason` itself
and correlates by `InvolvedObject`, so the fake clientset ignoring field selectors is fine.)

### 3. `scan.Evaluate` wiring

After `inventory.Prioritize` and immediately **before** `netpolicy.Annotate` (so a
`FailedCreate` finding claims the workload and netpolicy doesn't add a redundant hint):

```go
failedCreateEvents, _ := collect.FailedCreateEvents(ctx, client, opts.Namespace)
createhealth.Annotate(result.Workloads, inputs.ReplicaSets, failedCreateEvents)
netpolicy.Annotate(result.Workloads, podLabels, nps)   // existing, unchanged
```

No new `Result` field — the finding lives on `result.Workloads[i].Findings`.

### 4. Output example

```text
✗ shop/api  Deployment  0/3 Degraded
    ⚠ FailedCreate: the controller cannot create pods — blocked by a ResourceQuota
      ↳ pods "api-7c9f-" is forbidden: exceeded quota: compute, requested: requests.cpu=2, used: requests.cpu=4, limited: requests.cpu=4
```

### 5. What does not change

- **`report.go`** — the `FailedCreate` finding renders via the existing generic block
  (`⚠ FailedCreate: <Reason>` / `↳ <Evidence>`).
- **`inventory`** — no `Assemble`/`Prioritize`/`Flagged` change (a Degraded workload is
  already flagged and shown; the finding just adds the cause).
- **`explain.go` / `watch` / RBAC / deploy / Helm** — unchanged.

## Error handling

- `FailedCreateEvents` List error → ignored in `Evaluate` (non-fatal, like `VolumeAttachEvents`).
- A workload with an existing finding, or `Ready == Desired`, or no `FailedCreate` event →
  no annotation.
- An event for a bare ReplicaSet with no seeded workload → the key lookup misses → no finding.

## Testing

TDD, unit + integration (fake objects, no cluster):

- **`createhealth_test.go`** (pure `Annotate` over hand-built `[]inventory.Workload` + RS + events):
  - a Degraded Deployment (0/3, no findings) whose ReplicaSet has a `FailedCreate`
    `exceeded quota` event → one `FailedCreate` finding, Reason mentions "ResourceQuota",
    Evidence = the event message.
  - a Degraded StatefulSet with an `admission webhook` event → Reason mentions "admission webhook".
  - a workload that **already has a finding** → skipped (no FailedCreate added).
  - a **healthy** workload (`Ready == Desired`) with a stale FailedCreate event → skipped.
  - `classifyCreateFailure` cases (quota / admission / LimitRange / forbidden / default).
  - newest-event and RS→Deployment resolution (an event on `web-abc` owned by `web`
    attaches to the `web` Deployment; a StatefulSet event attaches directly).
- **`collect` test** — `FailedCreateEvents` via fake clientset returns the seeded event.
- **`scan` integration test** — `Evaluate` on a fake clientset with a 0-replica Deployment
  + its ReplicaSet + a `FailedCreate` event yields a workload carrying a `FailedCreate`
  finding.
- **Golden** — add a Degraded Deployment workload with a `FailedCreate` finding to the
  golden fixture and regenerate `testdata/golden-scan.txt` with `-update`.

## Files touched

- **Create:** `internal/createhealth/createhealth.go`, `internal/createhealth/createhealth_test.go`
- **Modify:** `internal/collect/collect.go` (+ `collect_test.go`) — `FailedCreateEvents`
- **Modify:** `internal/scan/scan.go` (+ `scan_test.go`) — fetch events + the `Annotate` call
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` (fixture + snapshot)
- **Docs:** `website/docs/features/diagnostics.md` (new subsection), `CHANGELOG.md`
  (`### Added`), `website/docs/quickstart.md`, `README.md`.

## Non-goals recap

Jobs; bare-ReplicaSet-0-pods; a CLI flag; `--explain` special-casing; any change to
`report.go`, `inventory`, `explain.go`, `watch`, or RBAC beyond the `Annotate` wiring and
the new event collector.
