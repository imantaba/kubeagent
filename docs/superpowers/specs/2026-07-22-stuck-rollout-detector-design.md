# Stuck-rollout detector (`RolloutStuck`) — design

**Status:** approved · **Date:** 2026-07-22 · **Type:** new detector (Theme B, deeper diagnosis)

## Problem

When you push a new version and the new ReplicaSet's pods never become available
— a bad image, a failing probe, or a `ResourceQuota`/RBAC block on pod creation —
the Deployment's rollout wedges. Kubernetes records this on the Deployment's
`status.conditions`: `Progressing` flips to `status=False` with
`reason=ProgressDeadlineExceeded` once `spec.progressDeadlineSeconds` elapses, and
/ or a `ReplicaFailure` condition (`status=True`) appears naming why pods can't be
created. kubeagent shows the workload as degraded (`Ready < Desired`) but has **no
detector that names the wedged rollout**. The `rollout` package only annotates
*what changed* (revision, image delta) — never *that the rollout is stuck*. This
adds a detector that names it.

## Behavior (approved)

A **flagged Deployment** with **no existing finding** whose status conditions show
a stuck rollout produces one Finding, rendered through the existing generic
finding path:

```text
✗ shop/api  Deployment  2/3 Degraded
    ⚠ RolloutStuck: the Deployment's rollout cannot complete — the new pods are not becoming available
      ↳ Progressing (ProgressDeadlineExceeded): ReplicaSet "api-7f9c" has timed out progressing.
```

- **Issue:** `RolloutStuck`.
- **Reason:** `the Deployment's rollout cannot complete — the new pods are not becoming available`.
- **Evidence:** `<condition type> (<reason>): <message>` for `Progressing`, or
  `ReplicaFailure: <message>` (a `ReplicaFailure` condition carries no distinct
  reason worth repeating) — the condition message names the concrete blocker.
- **Confidence:** `high` automatically — both conditions are direct
  Kubernetes-asserted state, not in the `confidence` package's `medium` heuristic
  set, so `confidence.ForIssue` returns `high` with no change to that package.

**Condition precedence (deterministic).** For a Deployment matching both, check
`ReplicaFailure` (`status=True`) **first** — it names the concrete create failure
(quota/RBAC), the more actionable message — then `Progressing`
(`status=False`, `reason=ProgressDeadlineExceeded`). The first match wins; one
finding per workload.

**Deliberately NOT flagged (false-positive guards):**

- A **paused** Deployment (`Progressing` reason `DeploymentPaused`) — intentional,
  not stuck.
- A Deployment **mid-rollout within its deadline** (`Progressing` `status=True`,
  reason `ReplicaSetUpdated`) — progressing normally.
- A **healthy** Deployment (`Progressing` `status=True`, reason
  `NewReplicaSetAvailable`) — the workload is not flagged anyway.
- A Deployment **already carrying a finding** — a pod-level detector
  (ImagePullBackOff, CrashLoopBackOff, ProbeFailure) or `createhealth`'s
  `FailedCreate` already names the cause; `RolloutStuck` would be redundant.
  Because `createhealth.Annotate` runs first and both gate on "no existing
  finding", a quota-blocked rollout with a lingering `FailedCreate` event is
  named by `createhealth` and skipped here — complementary, never doubled.
- A workload that is **not a Deployment** — StatefulSets and DaemonSets have no
  `ProgressDeadlineExceeded`/`ReplicaFailure` condition; their stuck-update
  detection needs different heuristics and is out of scope (a later slice).

## Design

### 1. `internal/rollouthealth` — the detector (new package)

Mirrors `internal/createhealth`: a pure, read-only workload-level annotator.

```go
// Package rollouthealth attaches a "RolloutStuck" finding to a flagged Deployment
// whose rollout has wedged — the new ReplicaSet's pods are not becoming available,
// so the Deployment's status carries a ReplicaFailure condition or a
// Progressing=False/ProgressDeadlineExceeded condition. Pure and read-only: the
// caller supplies the assembled+prioritized workloads and the Deployments (for
// their status conditions). Mirrors createhealth.Annotate.
package rollouthealth

import (
	appsv1 "k8s.io/api/apps/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate appends a "RolloutStuck" finding to each flagged Deployment workload
// that has no existing finding and whose Deployment status shows a stuck rollout.
// It mutates the slice elements in place.
func Annotate(workloads []inventory.Workload, deployments []appsv1.Deployment)
```

- Build a map `namespace/name → *appsv1.Deployment`.
- For each `w` in `workloads`: skip unless `w.Flagged() && w.Kind == "Deployment"
  && len(w.Findings) == 0`; look up its Deployment; call
  `stuckCondition(dep) → (evidence string, ok bool)`; if `ok`, append
  `diagnose.Finding{Pod: w.Namespace + "/" + w.Name, Issue: "RolloutStuck",
  Reason: "the Deployment's rollout cannot complete — the new pods are not
  becoming available", Evidence: evidence}`.
- `stuckCondition(dep appsv1.Deployment) (string, bool)` (pure helper):
  - iterate `dep.Status.Conditions`; if a `ReplicaFailure` condition has
    `Status == corev1.ConditionTrue` → return
    `fmt.Sprintf("ReplicaFailure: %s", c.Message), true`.
  - else if a `Progressing` condition has `Status == corev1.ConditionFalse &&
    Reason == "ProgressDeadlineExceeded"` → return
    `fmt.Sprintf("Progressing (ProgressDeadlineExceeded): %s", c.Message), true`.
  - else `"", false`.
  - (Imports `corev1 "k8s.io/api/core/v1"` for the condition-status constants and
    `fmt`. The condition **type** constants `appsv1.DeploymentReplicaFailure` and
    `appsv1.DeploymentProgressing` are used rather than raw strings.)

### 2. `scan.Evaluate` — wire the annotator

`inputs.Deployments` is already collected (used by `svchealth.BackendsFrom`). Add
one line immediately after the `createhealth.Annotate(...)` call (so `createhealth`
wins the "no existing finding" gate for a lingering `FailedCreate` event):

```go
	rollouthealth.Annotate(result.Workloads, inputs.Deployments)
```

### 3. `internal/remediation` — a `RolloutStuck` next step (correctness)

A `RolloutStuck` finding's `Pod` is the **Deployment** (`ns/name`), like
`FailedCreate`. Without a dedicated case, `remediation.For` would hit the default
and emit `kubectl -n <ns> describe pod <deployment-name>` — a wrong command. Add:

```go
	case "RolloutStuck":
		return Suggestion{"the rollout is wedged — inspect the new ReplicaSet's pods and events", describeDeployCmd(ns, pod)}
```

with a new read-only helper:

```go
func describeDeployCmd(ns, deploy string) string {
	return fmt.Sprintf("kubectl -n %s describe deployment %s", ns, deploy)
}
```

### 4. `report` — no change

A `RolloutStuck` finding renders through the existing finding path (Issue + Reason
+ Evidence), exactly like `FailedCreate`/`CreateContainerConfigError`. Golden: add
one Deployment workload carrying a `RolloutStuck` finding to the fixture and
regenerate (documents it and guards the render).

## Global constraints

- **Read-only; always-on; no flag.** No new collector, RBAC, watch gauge, or
  `Result` field — Deployments are already collected. Touches
  `internal/rollouthealth` (new) + one line of `internal/scan` + a
  `internal/remediation` case → **LIGHTWEIGHT SMOKE** gate. **Minor** bump
  v0.42.0 → **v0.43.0**; **patch** chart bump (no Helm change).
- **Pure & deterministic** — the detector reads only the passed workloads and
  Deployments; no clock, no cluster calls, no model.
- **Advisory-neutral** — a `RolloutStuck` finding makes its workload `Flagged()`
  like every other finding (the workload was already flagged); it adds no verdict
  logic.
- **Zero false positives** — gated on `Flagged()` + Deployment + no-existing-
  finding + a specific failure condition; paused / progressing / healthy
  Deployments are never flagged.
- `confidence`, `inventory`, `clusterhealth`, `rootcause`, the `rollout`
  annotator, `explain.go`, `--fix`, the watch daemon stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

StatefulSet/DaemonSet stuck-update detection (no such condition — needs
heuristic `updatedReplicas`/`numberUnavailable` signals; a later slice); a
`--fix`-style rollout restart/undo (read-only diagnosis only — `--fix` already
has `RolloutUndo` behind its guard rails); correlating the stuck rollout down to
the specific failing new-RS pod (the pod-level detectors already flag that pod
when present; this names the rollout when they don't); a new report section or
daemon gauge (the finding rides the existing workload-failing path); a JSON field
beyond the existing `Finding` serialization.

## Testing

- **`rollouthealth.Annotate` (pure, fake objects):**
  - a flagged Deployment workload (no finding) whose Deployment has
    `Progressing` `status=False`, `reason=ProgressDeadlineExceeded` → Finding
    `Issue "RolloutStuck"`, Evidence starts `Progressing (ProgressDeadlineExceeded):`
    and contains the condition message.
  - a flagged Deployment with a `ReplicaFailure` `status=True` condition →
    Finding whose Evidence starts `ReplicaFailure:` and contains the message.
  - precedence: a Deployment with **both** conditions → `ReplicaFailure` wins.
  - not flagged: a paused Deployment (`Progressing` reason `DeploymentPaused`); a
    progressing-within-deadline Deployment (`Progressing` `status=True`); a
    Deployment workload that **already has a finding** (existing pod finding →
    skipped); a **StatefulSet** workload with a look-alike condition (wrong Kind →
    skipped); an **unflagged** Deployment.
  - a Deployment workload with no matching Deployment in the slice → skipped (no
    panic).
- **`scan` integration:** a fake clientset with a Deployment reporting
  `ProgressDeadlineExceeded` and below desired replicas, and no pods → the
  workload has a `RolloutStuck` finding (proves the annotator is wired and reads
  `inputs.Deployments`).
- **`remediation`:** `For(Finding{Issue: "RolloutStuck", Pod: "shop/api"})` →
  command `kubectl -n shop describe deployment api`, next step contains
  `the rollout is wedged`; add `RolloutStuck` to `TestFor_CommandsAreNeverMutating`.
- **`confidence`:** `confidence.ForIssue("RolloutStuck") == "high"` (pins it is
  not accidentally in the `medium` set).
- **Golden:** add a Deployment workload with a `RolloutStuck` finding to the
  fixture; regenerate; the `N workloads failing` count and attention line update
  by one accordingly.

## Files touched

- **Create:** `internal/rollouthealth/rollouthealth.go` (+ test).
- **Modify:** `internal/scan/scan.go` (+ test) — wire the annotator + integration test.
- **Modify:** `internal/remediation/remediation.go` (+ test) — `RolloutStuck` case + `describeDeployCmd`.
- **Modify:** `internal/confidence/confidence_test.go` — the `high` classification pin.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — a fixture workload + regenerate.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
