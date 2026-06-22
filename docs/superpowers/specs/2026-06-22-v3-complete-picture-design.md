# kubeagent v3 — Design: complete-picture `scan`

**Status:** approved design (pre-implementation)
**Date:** 2026-06-22

## Goal

Turn `kubeagent scan` from a problem-only finder into a **complete cluster
health report**. `scan` lists every workload in scope grouped by its controller,
shows each workload's replica health, restart history, and per-pod detail, and
surfaces detector problems integrated into that view. A workload that is healthy
now but restarted many times in the past (e.g. `rancher`: 3/3 Running, 64
restarts weeks ago, reachable now) is shown — today it produces no output at all.

No new subcommand and no new flag gate this: **`scan` does it all** by default.

This design also absorbs two previously-approved smaller items:
- **Model selection** for `--explain` (was "v2.1") — folded in as Phase A.
- **Termination timestamps** (was "v2.1") — realized by the per-pod
  `LastRestart` column in Phase B.

## Decisions (from brainstorming)

- **View shape:** workload-level health summary (grouped by controller).
- **CLI:** no new subcommand/flag — the default `scan` output becomes the
  complete picture.
- **Detail:** each workload shows a summary line **plus** an indented row per pod.
- **Scope:** everything — Deployments, StatefulSets, DaemonSets, standalone
  pods, Jobs, CronJobs.
- **`--explain`:** summarize **notable items only** (findings, degraded
  workloads, high restarts, failed Jobs) — not the whole inventory.
- **Model:** `--model` flag › `KUBEAGENT_MODEL` env › `claude-opus-4-8`.

## Invariants preserved

- **Read-only against the cluster.** Only `List`/`Get` on all resource types;
  never create/update/patch/delete.
- **Sequential.** Multiple `List` calls run one after another; no goroutines
  (concurrency stays a documented later step).
- **CLI:** standard-library `flag` only — no Cobra.
- **Exit codes:** `0` ran successfully, `1` tool failed.
- **`--explain` stays additive:** the deterministic report works fully offline
  with no API key.
- **Egress (unchanged principle):** with `--explain`, only the structured
  notable items leave the process — never raw pod specs, env vars, or secrets.

## Architecture / pipeline

```text
cluster → collect (pods + all controllers) → inventory (group → Workloads)
        → diagnose (attach findings) → report (grouped) → [--explain: notable]
```

The new **`internal/inventory`** stage is where grouping, health computation, and
restart aggregation live. Detectors are unchanged (still per-pod, same `Finding`
type); their findings are attached to the owning workload during assembly. Each
stage is independently testable with fakes.

## Data model (`internal/inventory`)

```go
// Workload is one controller (or a bare pod) and its aggregated health.
type Workload struct {
    Namespace   string
    Name        string
    Kind        string   // Deployment | StatefulSet | DaemonSet | Job | CronJob | Pod
    Desired     int      // from the controller's .status (0 for bare pods/where N/A)
    Ready       int
    Status      string   // Healthy | Degraded | Failed | Complete | Pending | ...
    Restarts    int      // summed container restartCounts across this workload's pods
    LastRestart string   // RFC3339 (UTC) of the most recent restart, "" if none
    Image       string   // primary container image (first container)
    Pods        []PodRow
    Findings    []diagnose.Finding // detector problems on this workload's pods
}

// PodRow is one pod under a workload.
type PodRow struct {
    Name        string // pod name (or short suffix in text rendering)
    Phase       string // Running | Pending | Succeeded | Failed | ...
    Ready       string // "1/1"
    Restarts    int
    LastRestart string // RFC3339, "" if none
    Node        string
    IP          string
    Age         string // human duration, e.g. "36d"
    Image       string
}
```

## Collection (`internal/collect`)

`collect` gains a function that lists, read-only and namespace-scoped (or all):
Pods, Deployments, ReplicaSets, StatefulSets, DaemonSets, Jobs, CronJobs. The
existing `collect.Cluster(...) []diagnose.PodFacts` is retained for the detector
inputs; the new lists feed the inventory stage. Typed client-go clients:
`AppsV1().{Deployments,ReplicaSets,StatefulSets,DaemonSets}`,
`BatchV1().{Jobs,CronJobs}`, `CoreV1().Pods`.

## Grouping & health rules (`internal/inventory`)

**Owner resolution** via `ownerReferences`:
- Pod → ReplicaSet → Deployment (the ReplicaSet list resolves the middle hop).
- Pod → DaemonSet / StatefulSet / Job directly.
- Job → CronJob (so Jobs group under their CronJob).
- Pod with no controller owner → a **bare-pod** workload (Kind `Pod`).

A **CronJob** has no replicas: it renders with its schedule, `lastScheduleTime`,
and active-Job count as its summary (Desired/Ready left 0), with its Jobs' pods
grouped beneath it.

**Ready/Desired** come from the controller `.status`:
- Deployment/StatefulSet: `.status.Replicas` / `.status.ReadyReplicas`.
- DaemonSet: `.status.DesiredNumberScheduled` / `.status.NumberReady`.
- Job: derived from `.status.Active/Succeeded/Failed` vs `.spec.Completions`.
- Bare pod: Desired/Ready reflect the single pod (1/1 or 0/1).

**Status label:**
- `Healthy` — ready == desired and no findings.
- `Degraded` — ready < desired (and not a Job/CronJob).
- Jobs: `Complete` (succeeded), `Failed`, or `Running` (active).
- A workload with any attached detector finding is **flagged** regardless.

**Restart aggregation:** `Restarts` = sum of container `restartCount` across the
workload's pods; `LastRestart` = max `LastTerminationState.Terminated.FinishedAt`
across those containers (RFC3339 UTC; `""` when none). A small shared helper
formats a `metav1.Time` → RFC3339 or `""`.

**Completed Job/CronJob pods are capped** in the per-pod rows (show a count plus
the most recent few — default 3) to avoid flooding output with finished pods.

## Output (`internal/report`)

Default `scan` output, grouped, **flagged workloads sorted first**:

```text
cattle-system/rancher  Deployment  3/3 Running  · 64 restarts, last 20d ago
    image rancher/rancher:v2.14.1
    …64smq  1/1  Running  31 (20d ago)  nova-worker-3  10.42.4.41   36d
    …6gc4c  1/1  Running   1 (20d ago)  nova-worker-1  10.42.6.223  35d
    …d2th5  1/1  Running  32 (30d ago)  nova-worker-2  10.42.7.133  36d
⚠ kube-system/coredns  Deployment  1/2 Degraded  → CrashLoopBackOff: <reason>
    …
```

- **text:** one workload header + indented pod rows; flagged workloads carry a
  `⚠` and the detector reason; an all-healthy cluster still prints the inventory
  (no more bare "No issues found").
- **json:** an **array of `Workload` objects**. With `--explain`, wrapped as
  `{"workloads": [...], "explanation": "..."}`.

> ⚠️ **Breaking change:** this replaces v2's scan JSON (a bare `findings` array /
> `{findings, explanation}` wrapper). Findings now live nested inside each
> workload's `findings` field. Accepted because `scan` now reports the whole
> picture.

## `--explain` — notable filtering + model selection

A pure `notable(workloads) []Workload` selects only workloads that (a) have a
detector finding, (b) are `Degraded` (ready<desired), (c) exceed a restart-count
threshold (default: ≥ 5 total restarts), or (d) are failed Jobs. `buildPrompt` renders that filtered set, so
the single Claude call stays bounded even on large clusters. If nothing is
notable, the call is skipped (returns `""`, as today).

**Model selection:** a pure `explain.ResolveModel(flagVal, envVal) string`
implements `flag › env (KUBEAGENT_MODEL) › DefaultModel (claude-opus-4-8)`.
`explain.New(model)` takes the resolved model; the adapter passes
`anthropic.Model(model)` to the request. `main` adds `--model` (default empty so
env can win) and resolves precedence. An unknown model is rejected by the API at
call time (no brittle hardcoded allow-list).

## Testing (TDD)

- `inventory` — pure stage: unit-test owner grouping (incl. RS→Deployment hop),
  ready/desired from controller `.status`, status labels, restart sum + max
  `LastRestart`, and Job/CronJob capping — all with fake pods + controller
  objects, no cluster.
- `collect` — the new multi-resource list uses the client-go fake clientset.
- `report` — table tests for the grouped text rendering (flagged-first ordering,
  pod rows, healthy inventory) and the JSON workloads array (+ wrapper with
  explanation).
- `explain` — `notable` filter and `ResolveModel` precedence are pure unit tests;
  the summarizer seam stays faked (no network).
- `main` — `--model`/`--explain` arg handling; `--explain` without
  `ANTHROPIC_API_KEY` still fails fast.

## Phasing (implementation order)

The plan builds in three phases so value lands early and the noisy part is last:

- **Phase A — model selection.** Small and independent: `--model` flag,
  `ResolveModel`, `KUBEAGENT_MODEL` env, `explain.New(model)`. Can land first.
- **Phase B — core inventory.** Deployments, StatefulSets, DaemonSets, and bare
  pods → grouping, health, restart aggregation (incl. per-pod `LastRestart`
  timestamps), grouped text+JSON report, integrated detectors, and notable-only
  `--explain`. This fully delivers the rancher scenario.
- **Phase C — Jobs & CronJobs.** Job/CronJob collection, Job→CronJob grouping,
  Job status, and capping completed-pod rows.

## Out of scope (explicit non-goals)

- Concurrency for the extra `List` calls (stays sequential; a documented later
  step).
- A problems-only view/flag (problems are integrated; can add a filter later if
  wanted).
- Metrics/resource-usage (CPU/memory) — requires metrics-server; not now.
- Streaming the explanation; configurable prompt; multi-call orchestration.
- Watching/auto-refresh — `scan` remains a one-shot read.
