# Missing-config / CreateContainerConfigError detector — design

**Status:** approved · **Date:** 2026-07-22 · **Type:** new detector (Theme B)

## Problem

A very common deploy mistake — a Pod referencing a ConfigMap or Secret that
doesn't exist (or a key that's absent), as a volume, `envFrom`, or
`env.valueFrom` — leaves the container stuck in `Waiting` with reason
`CreateContainerConfigError`. No kubeagent detector matches that reason today, so
the workload shows only as degraded (`Ready < Desired`) with **no finding
explaining why**. This adds a detector that names it, surfacing the kubelet's
message (which identifies the missing object).

The two other named Theme-B items are intentionally **not** built: a degraded
CoreDNS is already flagged as a verdict-flipping `SystemIssue` by
`clusterhealth`, and node pressure conditions (`DiskPressure`/`MemoryPressure`/
`PIDPressure`) are already checked in `clusterhealth.nodeHealth` — both would be
redundant.

## Behavior (approved)

A container (main **or** init) stuck in `Waiting` with reason
`CreateContainerConfigError` produces one Finding on its workload, rendered
through the existing generic finding path:

```text
✗ shop/api  Deployment  0/2 Degraded
    ⚠ CreateContainerConfigError: a referenced ConfigMap or Secret is missing, or a required key is absent — the container cannot start
      ↳ container "api": configmap "app-config" not found
```

- **Issue:** `CreateContainerConfigError`.
- **Reason:** `a referenced ConfigMap or Secret is missing, or a required key is absent — the container cannot start`.
- **Evidence:** `container "<name>": <kubelet message>` for a main container, or
  `init container "<name>": <kubelet message>` for an init container — the
  kubelet message names the missing ConfigMap/Secret/key.
- **Container:** the container name.
- **Confidence:** `high` automatically — `CreateContainerConfigError` is a direct
  Kubernetes-asserted state, not in the `confidence` package's `medium` heuristic
  set, so `confidence.ForIssue` returns `high` with no change to that package.

Only `CreateContainerConfigError` is flagged (not `CreateContainerError` or
`InvalidImageName`). The detector returns the **first** matching container
(main containers scanned before init containers), one Finding per pod, matching
the one-finding-per-pod convention of the sibling detectors.

## Design

### 1. `internal/diagnose/configerror.go` — the detector

Mirrors `imagepull.go`:

```go
package diagnose

import "fmt"

// ConfigErrorDetector flags a container stuck in CreateContainerConfigError — a
// referenced ConfigMap/Secret is missing, or a required key is absent, so the
// container cannot start. Scans main and init containers.
type ConfigErrorDetector struct{}

func (d ConfigErrorDetector) Detect(facts PodFacts) *Finding {
	if f := configErrorIn(facts, facts.Pod.Status.ContainerStatuses, "container"); f != nil {
		return f
	}
	return configErrorIn(facts, facts.Pod.Status.InitContainerStatuses, "init container")
}

func configErrorIn(facts PodFacts, statuses []corev1.ContainerStatus, kind string) *Finding {
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && w.Reason == "CreateContainerConfigError" {
			return &Finding{
				Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:     "CreateContainerConfigError",
				Reason:    "a referenced ConfigMap or Secret is missing, or a required key is absent — the container cannot start",
				Evidence:  fmt.Sprintf("%s %q: %s", kind, cs.Name, w.Message),
				Container: cs.Name,
			}
		}
	}
	return nil
}
```

(`corev1` import for the `[]corev1.ContainerStatus` parameter.)

### 2. `scan.Evaluate` — register the detector

Add one line to the `detectors` slice:

```go
		diagnose.ConfigErrorDetector{},
```

The finding then flows through `diagnose.Run` → `inventory` (attaches to the
workload) → `report`/JSON/`--explain`/`confidence.Annotate` with no other change.

### 3. `report` — no change

A `CreateContainerConfigError` Finding renders through the existing
finding-rendering path (Issue + Reason + Evidence), exactly like `ImagePullBackOff`.
Golden: add one workload carrying a `CreateContainerConfigError` finding to the
fixture and regenerate (documents it and guards the render).

## Global constraints

- **Read-only; always-on; no flag.** No new collector, RBAC, watch gauge,
  `Result` field, or report change. Touches `internal/diagnose` (new file) + one
  line of `internal/scan` → **LIGHTWEIGHT SMOKE** gate. **Minor** bump v0.40.0 →
  **v0.41.0**; **patch** chart bump (no Helm change).
- **Pure & deterministic** — the detector reads only the pod's container statuses;
  no clock, no cluster calls (unlike `RestartLoopDetector` which needs `Now`).
- **Advisory-neutral** — a `CreateContainerConfigError` finding makes its workload
  `Flagged()`, exactly like every other pod finding; a kube-system workload with
  this finding contributes to the verdict via the existing `SystemIssues` path
  (correct — a broken system pod is a real problem). No verdict logic is added.
- `confidence`, `inventory`, `clusterhealth`, `rootcause`, `explain.go`, `--fix`,
  the watch daemon stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

`CreateContainerError` (a broader container-creation failure — bad command, hook,
device) and `InvalidImageName` (chosen out in scope); correlating the pod's
ConfigMap/Secret *references* against collected objects to confirm which is
missing (the kubelet message already names it, and it would add sensitive Secret
listing); a new Issue-specific report section (the generic finding path is
enough); `--fix` remediation (read-only diagnosis only).

## Testing

- **`diagnose` (pure, fake pods via the existing `podWaiting` helper + an
  init-container variant):**
  - a main container Waiting with `CreateContainerConfigError` and message
    `configmap "app-config" not found` → Finding `Issue
    "CreateContainerConfigError"`, `Evidence` contains `container "app"` and the
    message, `Container "app"`.
  - an **init** container with `CreateContainerConfigError` (and healthy main
    containers) → Finding whose Evidence starts `init container "…"`.
  - a container Waiting with a different reason (`CrashLoopBackOff`,
    `ContainerCreating`) → no finding (nil).
  - a Running pod / no waiting container → nil.
  - main-container match takes precedence when both a main and an init container
    are in the error state.
- **`scan` integration:** a fake clientset with a Pod whose container is Waiting
  with `CreateContainerConfigError` → `Result` has a workload finding with that
  Issue (proves the detector is registered and runs end-to-end).
- **`confidence`:** `confidence.ForIssue("CreateContainerConfigError") == "high"`
  (a direct-read issue; pins that it is not accidentally in the `medium` set).
- **Golden:** add a workload with a `CreateContainerConfigError` finding to the
  fixture; regenerate; the `N workloads failing` count and attention line update
  by one accordingly.

## Files touched

- **Create:** `internal/diagnose/configerror.go` (+ test).
- **Modify:** `internal/scan/scan.go` (+ test) — register the detector + integration test.
- **Modify:** `internal/confidence/confidence_test.go` — the `high` classification pin.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — a fixture workload + regenerate.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
