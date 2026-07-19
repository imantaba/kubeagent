# InitContainer failure detector — design

**Status:** approved · **Date:** 2026-07-19 · **Type:** new detector (v1 core)

## Goal

Close a real coverage gap: a pod blocked in its **init phase** because an init
container is failing is currently invisible or mis-diagnosed. Every existing crash
detector (`CrashLoopDetector`, `OOMKilledDetector`, `ImagePullDetector`,
`RestartLoopDetector`) iterates only `pod.Status.ContainerStatuses` — none look at
`pod.Status.InitContainerStatuses`. A new `InitContainerDetector` names which init
container is failing and why, so `Init:CrashLoopBackOff` / `Init:ImagePullBackOff` /
`Init:OOMKilled` pods stop being a silent "the pod just won't start."

## Scope

**In (failure modes only — the approved choice):** the unambiguous init failures,
read from `InitContainerStatuses`:

- **Init:CrashLoopBackOff** — an init container crash-looping.
- **Init:ImagePullBackOff / Init:ErrImagePull** — an init container's image can't be pulled.
- **Init:OOMKilled** — an init container killed for exceeding its memory limit.

Pure and deterministic (no clock), mirroring `CrashLoopDetector`/`ImagePullDetector`.

**Out of scope (YAGNI / deferred):**
- **Time-based "stuck init"** (an init container Running forever waiting for a
  dependency — `Init:0/N`, nothing crashing). Needs a clock + a tunable threshold and
  risks false-positives on legitimately-slow inits (migrations, large downloads).
  Explicitly deferred to a future iteration.
- **Bare `Init:Error`** (an init container Terminated non-zero but *not* yet in
  CrashLoopBackOff, e.g. `restartPolicy: Never`). A failing init on the default
  `restartPolicy: Always` reaches `CrashLoopBackOff`, which we catch; matching the
  transient/`Never` Terminated-Error state is a future extension (mirrors how
  `CrashLoopDetector` catches only the stable `Waiting` state for main containers).
- No new CLI flag — always-on (no extra RBAC or cost).

## Global constraints

- **Read-only; NO new RBAC and NO new collection.** `InitContainerStatuses` is already
  part of the Pod objects `collect` fetches. Only reads pod status.
- **Core, always-on** detector — runs in both the CLI `scan` and the `watch` daemon via
  the shared `scan.Evaluate`. No opt-in flag, no `watch.Config` change.
- Detector is a **pure function** of `PodFacts` (reads `Pod` only; ignores `Events`);
  deterministic.
- `report.go`, `explain.go`, `internal/watch`, and all deploy/RBAC/Helm files stay
  **unchanged**. The only shared code touched is `containerResources` (extended, below).
- **No `Co-Authored-By: Claude` trailer** on any commit. TDD.

## Design

### 1. The detector

`internal/diagnose/initcontainer.go`:

```go
type InitContainerDetector struct{}

func (d InitContainerDetector) Detect(facts PodFacts) *Finding {
    statuses := facts.Pod.Status.InitContainerStatuses
    for i, cs := range statuses {
        if f := initFinding(facts.Pod, cs, i, len(statuses)); f != nil {
            return f
        }
    }
    return nil
}
```

Init containers run **sequentially and block the pod**, so at most one is actively
failing. The loop returns the **first failing** init container: earlier ones succeeded
(`Terminated`, exit 0) and are not a failing state, later ones haven't started
(`Waiting.Reason == "PodInitializing"`) — both fall through `initFinding` to `nil`.

### 2. Classification — `initFinding(pod, cs, idx, total)`

Position `pos := "(idx+1/total)"` (e.g. `(1/2)`), matching kubectl's `Init:1/2`.
Precedence per failing init container:

1. `cs.State.Waiting.Reason` is `ImagePullBackOff` or `ErrImagePull` →
   `Issue: "Init:" + reason`, `Reason: "an init container's image cannot be pulled — the pod cannot start"`,
   `Evidence: init container "<name>" <pos>: <Waiting.Message>`.
2. else if `cs.State.Terminated` **or** `cs.LastTerminationState.Terminated` has
   `Reason == "OOMKilled"` → `Issue: "Init:OOMKilled"`,
   `Reason: "an init container was killed for exceeding its memory limit — the pod cannot start"`,
   `Evidence: init container "<name>" <pos>, exitCode=<code>`, plus
   `Resources: containerResources(pod, cs.Name)`.
3. else if `cs.State.Waiting.Reason == "CrashLoopBackOff"` → `Issue: "Init:CrashLoopBackOff"`,
   `Reason: "an init container is crash-looping — the pod cannot start its main containers"`,
   `Evidence: init container "<name>" <pos>, restartCount=<cs.RestartCount>`.
4. else → `nil`.

Every finding sets `Container: cs.Name`. OOM is checked before CrashLoopBackOff so an
init container currently in `CrashLoopBackOff` whose last exit was `OOMKilled` reports
the more specific `Init:OOMKilled` (with resources), not the generic crash.

### 3. Issue naming

Issue strings mirror kubectl's STATUS column — `Init:CrashLoopBackOff`,
`Init:ImagePullBackOff`, `Init:ErrImagePull`, `Init:OOMKilled` — so they're instantly
recognizable and clearly distinct from a main-container `CrashLoopBackOff`. The report
renders them through the existing generic block (`⚠ <Issue>: <Reason>` / `↳ <Evidence>`),
so **no `report.go` change**.

### 4. No overlap with existing detectors

When an init container is failing, the pod's **main** containers are `Waiting` with
`Reason == "PodInitializing"`. The main crash detectors match only their specific
reasons (`CrashLoopBackOff`/`ImagePullBackOff`/`OOMKilled`), never `PodInitializing`, so
they stay silent — `InitContainerDetector` is the sole finding for an init-blocked pod.
Conversely, once all inits succeed (`Terminated`, exit 0), `InitContainerDetector`
returns `nil` and a crashing **main** container is left to `CrashLoopDetector`. The two
never both fire, and the guard needs no cross-detector coupling.

**Native sidecars** (init containers with `restartPolicy: Always`, k8s ≥ 1.28) that are
*healthily Running* match none of the failing states above, so they are never flagged; a
*crash-looping* sidecar-init is flagged, which is correct.

### 5. The one shared-code change: `containerResources`

`containerResources(pod, name)` (in `oomkilled.go`) currently searches only
`pod.Spec.Containers`. Extend it to also search `pod.Spec.InitContainers` so
`Init:OOMKilled` carries the init container's requests/limits like the main OOM finding.
Iterate the two slices **separately** (never `append(a, b...)`, which can mutate the
backing array):

```go
func containerResources(pod *corev1.Pod, name string) *ContainerResources {
    for _, list := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
        for _, c := range list {
            if c.Name == name {
                return &ContainerResources{ /* unchanged body */ }
            }
        }
    }
    return nil
}
```

This is backward-compatible (main OOM findings unchanged) and the only edit to existing
code besides the detector registration.

### 6. Wiring

Register `diagnose.InitContainerDetector{}` in `scan.Evaluate`'s `detectors` slice
(after `RestartLoopDetector`, before `ProbeFailureDetector`). No other `scan.go` change
— `collect` already returns pods with their `InitContainerStatuses`.

### 7. `--logs` works for free

Because the finding sets `Container` to the init container name, `scan --logs`
opportunistically enriches it: `collect.PreviousLogs(…, container)` fetches the
crash-looping init container's previous logs (`GetLogs` accepts an init container name),
a useful root cause. Non-fatal / no block when there's no prior instance. The M4
per-container de-dup already prevents any double log block. No new code.

### 8. Output examples

```text
✗ shop/orders  Deployment  0/1 Degraded
    ⚠ Init:CrashLoopBackOff: an init container is crash-looping — the pod cannot start its main containers
      ↳ init container "wait-for-db" (1/2), restartCount=6
    orders-6f9-abcde  0/1  Init:CrashLoopBackOff  restarts=0  worker-1  10.244.2.9  3m
```
```text
    ⚠ Init:ImagePullBackOff: an init container's image cannot be pulled — the pod cannot start
      ↳ init container "fetch-config" (2/3): Back-off pulling image "reg/config:bad": not found
```
```text
    ⚠ Init:OOMKilled: an init container was killed for exceeding its memory limit — the pod cannot start
      ↳ init container "loader" (1/1), exitCode=137
      resources: memory req=16Mi limit=32Mi · cpu req=unset limit=unset
```

## Error handling

- No `InitContainerStatuses` (pod has no init containers) → the loop is empty → `nil`.
- Init containers all succeeded → no failing state matched → `nil`.
- A not-yet-started init (`PodInitializing`) or a running healthy sidecar → `nil`.

## Testing

TDD, detector-level and integration (fake pods, no cluster):

- **`initcontainer_test.go`:**
  - Init:CrashLoopBackOff on init container 1 of 2 → Issue `Init:CrashLoopBackOff`,
    Evidence `init container "wait-for-db" (1/2), restartCount=6`, Container set.
  - Init:ImagePullBackOff → Issue `Init:ImagePullBackOff`, Evidence includes the pull
    message and `(2/3)`.
  - Init:OOMKilled (via `LastTerminationState`) → Issue `Init:OOMKilled`, `exitCode`, and
    `Resources` populated (asserts `containerResources` found the init container's limits).
  - **Precedence:** init container `Waiting: CrashLoopBackOff` + `LastTerminationState
    OOMKilled` → `Init:OOMKilled` (not `Init:CrashLoopBackOff`).
  - **Succeeded inits skipped / no overlap:** all inits `Terminated` exit 0, a main
    container `Waiting: CrashLoopBackOff` → `InitContainerDetector` returns `nil`.
  - **Native sidecar Running** (init container `State.Running`, `restartPolicy: Always`)
    → not flagged.
  - First-failing-only: two failing-looking inits, the earlier one is returned.
- **`oomkilled_test.go`** (or the init test): `containerResources` still returns main
  container resources (regression) AND now returns init container resources.
- **`scan` integration test** — `Evaluate` on a fake clientset with a pod whose init
  container is CrashLoopBackOff (main container `PodInitializing`) yields exactly one
  `Init:CrashLoopBackOff` finding and no `CrashLoopBackOff` finding (no overlap).
- **Golden** — add an `Init:CrashLoopBackOff` workload to the golden fixture and
  regenerate `testdata/golden-scan.txt` with `-update`. Adds sample lines only (no report
  format change); README GIF / quickstart example follow the standard golden-change
  protocol at release time.

## Files touched

- **Create:** `internal/diagnose/initcontainer.go`, `internal/diagnose/initcontainer_test.go`
- **Modify:** `internal/diagnose/oomkilled.go` (extend `containerResources`) + its test
- **Modify:** `internal/scan/scan.go` (register the detector) + `scan_test.go`
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt`
- **Docs:** `website/docs/features/diagnostics.md` (new subsection + `## Status` list),
  `CHANGELOG.md` (`### Added`), `website/docs/quickstart.md`, `README.md`.

## Non-goals recap

Time-based stuck-init detection; bare `Init:Error` / `restartPolicy: Never` permanent
termination; any CLI flag; any change to report/explain/watch/RBAC beyond registering the
detector and extending `containerResources`.
