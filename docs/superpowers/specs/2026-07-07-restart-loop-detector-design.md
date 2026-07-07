# kubeagent — Design: restart-loop detector

**Status:** approved design (pre-implementation)
**Date:** 2026-07-07

## Goal

Catch a pod that runs, exits with an error, restarts, and recovers — repeatedly
— even though it is *currently Running*. Today the point-in-time detectors miss
it: `CrashLoopBackOff` only fires while the container is `Waiting`, `OOMKilled`
only covers memory kills, and a healthy-now-but-restarted workload is hidden
behind the opt-in `--include-restarts`. So a genuinely flapping pod reads as
Healthy and produces no finding.

## Motivation (proven live on hetzner-nova)

A `flapper` pod (runs ~50s, `exit 1`, restart, repeat) accumulated 3 restarts
while `scan` reported **"No issues found. ✅"** and the daemon's
`kubeagent_findings{}` never produced a series — `workloads_flagged` only blinked
to 1 for the split-second the pod was `0/1` mid-restart. A Prometheus alert
could never fire. This detector closes that gap.

## Decision (from brainstorming)

- **Sensitivity: balanced** — flag a container with `RestartCount ≥ 3`, a non-OOM
  error last-termination, and a *young current run* (still flapping). Ignores a
  one-off restart (rollout SIGTERM, node reboot) and a pod that recovered and has
  since run stably.
- **Stateless detector** reading pod status (Approach A) — works identically in
  the CLI scan and the read-only watch daemon, no new state, no new data
  collection, no RBAC change. (Stateful restart-rate tracking is a deferred
  future refinement.)
- **Focused on the currently-Running case** — zero overlap with
  `CrashLoopBackOff` (which fires only while `Waiting`), so no double findings.

## Invariants / constraints (unchanged)

- **READ-ONLY.** Reads only existing pod status; no new List, no writes, no LLM.
- Detectors stay pure `Detect(PodFacts) *Finding`. `Now` is injected via a struct
  field so the detector is fully unit-testable.
- No new Go module dependency.
- The finding flows to report/JSON/`--explain`/`kubeagent_findings{}` unchanged.

## Component 1 — the detector (`internal/diagnose/restartloop.go`)

```go
type RestartLoopDetector struct{ Now time.Time }
func (d RestartLoopDetector) Detect(facts PodFacts) *Finding
```

Constants: `restartThreshold = 3`, `restartRecency = 10 * time.Minute`.

Fires for a container that is **all of**:
1. currently **Running** (`cs.State.Running != nil`) — the case `CrashLoopBackOff`
   misses; also guarantees no overlap with the CrashLoop detector.
2. its **current run is young**: `d.Now.Sub(cs.State.Running.StartedAt.Time) ≤
   restartRecency`. A pod that recovered and has run stably longer than the window
   is not flagged.
3. **`cs.RestartCount ≥ restartThreshold`**.
4. `cs.LastTerminationState.Terminated` is a **non-OOM error**: it is non-nil,
   `ExitCode != 0`, and `Reason != "OOMKilled"` (OOM is covered by the
   OOMKilledDetector).

Emits:
- `Issue: "RestartLoop"`
- `Reason: "Container keeps exiting with an error and restarting"`
- `Evidence:` e.g. `container "app", 3 restarts, last exit 1 (Error), 40s ago`
  (restart count, last exit code + reason, and how long ago the last termination
  finished, from `LastTerminationState.Terminated.FinishedAt`).

Pure: reads only `facts.Pod` status and `d.Now`.

## Component 2 — wire into `scan.Evaluate` (`internal/scan`)

Add one entry to the detector slice:

```go
diagnose.RestartLoopDetector{Now: time.Now()},
```

No new collection, no `FactsFrom` change, no RBAC change (pods are already read).
Both the CLI `scan` and the `watch` daemon inherit it; the finding appears in the
text report, JSON, `--explain`, and `kubeagent_findings{issue="RestartLoop"}`
with no changes to those paths.

## Testing (TDD)

- **Detector** (`restartloop_test.go`, fixed `Now`):
  - Running + young run + `RestartCount ≥ 3` + non-OOM error last-termination →
    a `RestartLoop` finding (evidence carries the count, exit code, and age).
  - Running but the current run is **old** (> 10 min) → nil (recovered).
  - `RestartCount < 3` → nil.
  - last-termination `OOMKilled` → nil (covered elsewhere).
  - last-termination graceful (`ExitCode == 0`) → nil.
  - never restarted (no `LastTerminationState.Terminated`) → nil.
  - not currently Running (a `Waiting` container) → nil (this detector targets the
    Running-but-flapping gap).
- **`scan.Evaluate` integration** (fake clientset): a flapper-like pod (Running,
  young, 3 restarts, exit-1 last termination) → the workload carries a
  `RestartLoop` finding.
- **Live validation:** re-run the nova flapper scenario → the deployed-style
  daemon now emits `kubeagent_findings{issue="RestartLoop"}` and the scan shows
  the finding on every reconcile in a Running window.

## Out of scope (explicit non-goals)

- Stateful restart-rate / delta tracking across reconciles (Approach B).
- Flagging benign single restarts (rollouts, node reboots, graceful exits).
- The actively-backing-off case (already covered by `CrashLoopBackOff`).
- Any `--fix` remediation for restart loops.
