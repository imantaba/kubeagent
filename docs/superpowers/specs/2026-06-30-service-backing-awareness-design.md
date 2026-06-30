# kubeagent — Design: service backing awareness

**Status:** approved design (pre-implementation)
**Date:** 2026-06-30

## Goal

Reduce false-positive noise in the "Service issues" section. Today a
selector-based Service with no ready endpoints is always flagged `⚠ … no ready
endpoints`. On a real cluster that fires on Services that are *expected* to have
no endpoints — those backed by a CronJob/Job (no pods between runs) or by a
DaemonSet/Deployment/StatefulSet whose desired count is `0` (e.g. a Windows
node-exporter DaemonSet on an all-Linux cluster).

Observed on the nova cluster:

```text
Service issues:
  ⚠ cattle-monitoring-system/rancher-monitoring-windows-exporter  ClusterIP  no ready endpoints
  ⚠ ekb-js-nightly/clickhouse-sync  ClusterIP  no ready endpoints
  ⚠ ekb-js-staging/clickhouse-sync  ClusterIP  no ready endpoints
```

`windows-exporter` backs a DaemonSet with 0 desired; `clickhouse-sync` backs a
CronJob. Neither is a primary problem.

## Decision (from brainstorming)

- **Annotate in place** — keep these in the same `Service issues` list (nothing
  hidden, so a genuinely-broken cron service is still visible), but tag each with
  its backing workload and the reason it has no endpoints, so it's clear which
  are not primary.
- **Scope of "expected":** annotate when the backing workload is a CronJob/Job,
  **or** a DaemonSet/Deployment/StatefulSet whose desired count is `0`. A
  Deployment/StatefulSet with desired `> 0` and no endpoints stays a **primary**
  issue (the real-outage signal) — even if its labels also coincidentally match a
  job.
- **Detection:** match `service.spec.selector` against each workload's
  pod-template labels. Works even between cron runs (the controller object exists
  when its pods don't) and reads the authoritative per-workload desired count.

## Invariants preserved

- **READ-ONLY.** No new API calls — Deployments, StatefulSets, DaemonSets, Jobs,
  and CronJobs are already collected by `collect.CollectInventory`. Never mutates
  the cluster.
- **No new Go module dependency.** Uses already-present `k8s.io/api` subpackages
  (`apps/v1`, `batch/v1`, `core/v1`, `discovery/v1`).
- **Sequential**, stdlib `flag`. Exit codes unchanged.
- `svchealth` stays **pure**: the caller supplies Services, EndpointSlices, and
  the backend descriptors.
- **No new noise on real issues.** A primary (real-outage) `no ready endpoints`
  issue renders exactly as today. LoadBalancer `NoExternalAddress` is unaffected.

## Architecture

```text
collect (already gathers Deployments/STS/DS/Jobs/CronJobs + Services + slices)
  → svchealth.BackendsFrom(deploys, sts, ds, jobs, cronjobs) []Backend   ← adapter
  → svchealth.Assess(services, slices, backends) []Issue                  ← + classification
  → report (text Detail carries the annotation; JSON gains expected/backing)
  → explain (annotation rides along in Detail; unchanged call)
```

## Component 1 — `Backend` + `BackendsFrom` (`internal/svchealth`)

```go
// Backend describes a workload that may back a Service: its pod-template labels
// and whether it currently wants any pods.
type Backend struct {
	Kind           string            // Deployment | StatefulSet | DaemonSet | Job | CronJob
	Namespace      string
	TemplateLabels map[string]string // pod-template labels the Service selector must be a subset of
	Desired        int               // replicas / DesiredNumberScheduled (ignored when Ephemeral)
	Ephemeral      bool              // true for Job and CronJob
}

// BackendsFrom adapts the already-collected controller slices into Backends.
func BackendsFrom(
	deploys []appsv1.Deployment,
	statefulsets []appsv1.StatefulSet,
	daemonsets []appsv1.DaemonSet,
	jobs []batchv1.Job,
	cronjobs []batchv1.CronJob,
) []Backend
```

Mapping (template labels are the workload's pod-template `metadata.labels`):

| Kind | TemplateLabels source | Desired | Ephemeral |
|------|----------------------|---------|-----------|
| Deployment | `Spec.Template.Labels` | `Spec.Replicas` (nil → 1) | false |
| StatefulSet | `Spec.Template.Labels` | `Spec.Replicas` (nil → 1) | false |
| DaemonSet | `Spec.Template.Labels` | `Status.DesiredNumberScheduled` | false |
| Job | `Spec.Template.Labels` | (unused) | true |
| CronJob | `Spec.JobTemplate.Spec.Template.Labels` | (unused) | true |

ReplicaSets are intentionally **not** included — a ReplicaSet's owning Deployment
already covers the same pods, and RS pod templates carry a `pod-template-hash`
the Service selector won't include.

## Component 2 — classification in `Assess` (`internal/svchealth`)

```go
func Assess(services []corev1.Service, slices []discoveryv1.EndpointSlice, backends []Backend) []Issue
```

For each selector-Service with `readyEndpoints == 0`, classify before emitting the
`NoEndpoints` issue:

1. `matches` = backends where `b.Namespace == svc.Namespace` and
   `selectorMatches(svc.Spec.Selector, b.TemplateLabels)`.
2. If any match has `!Ephemeral && Desired > 0` → **primary**: `Detail = "no
   ready endpoints"`, `Expected = false`, `Backing = ""`. (Real outage wins, even
   if a job also matches.)
3. Else if `len(matches) > 0` → **expected**: pick a representative backing in
   precedence order `CronJob, Job, DaemonSet, Deployment, StatefulSet`; set
   `Expected = true`, `Backing = <kind>`, and `Detail` per the table below.
4. Else (no match) → **primary** plain (orphan / externally-managed endpoints).

`selectorMatches(selector, labels)` returns true when every key/value in
`selector` is present in `labels` (an empty selector never reaches here — the
existing selectorless skip stays).

Detail text for the expected case:

| Representative backing | Detail |
|------------------------|--------|
| CronJob | `no ready endpoints (backs CronJob — expected between runs)` |
| Job | `no ready endpoints (backs Job — expected between runs)` |
| DaemonSet (desired 0) | `no ready endpoints (backs DaemonSet — 0 desired)` |
| Deployment (desired 0) | `no ready endpoints (backs Deployment — scaled to 0)` |
| StatefulSet (desired 0) | `no ready endpoints (backs StatefulSet — scaled to 0)` |

`Issue` gains:

```go
Expected bool   `json:"expected,omitempty"` // true for an expected (annotated) NoEndpoints issue
Backing  string `json:"backing,omitempty"`  // representative backing kind, when classified
```

## Component 3 — surfacing (text + JSON; explain rides along)

- **text:** unchanged rendering — `printServiceIssues` already prints `Detail`,
  which now carries the annotation. Same `⚠` glyph, same line format.
- **json:** the new `Expected` / `Backing` fields appear on each `Issue`
  (`omitempty`).
- **explain:** unchanged call; the annotation is part of `Detail`, so the model
  sees it without any new wiring. Credential-lint isolation is irrelevant here.
- **all-clear:** unchanged — expected issues still count as service issues (they
  are listed), so the all-clear condition is unaffected.

## Component 4 — wiring (`main.go`)

```go
slices, _ := collect.EndpointSlices(context.Background(), client, namespace)
backends := svchealth.BackendsFrom(inputs.Deployments, inputs.StatefulSets, inputs.DaemonSets, inputs.Jobs, inputs.CronJobs)
serviceIssues := svchealth.Assess(svcs, slices, backends)
```

Only the `Assess` call-site changes (one new argument); the report/explain calls
are untouched.

## Testing (TDD)

- `selectorMatches` — subset true; missing key false; mismatched value false;
  empty selector handled by the caller's existing skip.
- `BackendsFrom` — each kind maps to the right `Kind`, `TemplateLabels`,
  `Desired` (Deployment/STS nil replicas → 1; DaemonSet from
  `DesiredNumberScheduled`), and `Ephemeral` (true only for Job/CronJob).
- `Assess` classification table:
  - Deployment desired>0, 0 endpoints → primary, `Expected=false`, `Detail="no
    ready endpoints"`.
  - CronJob-backed → `Expected=true`, `Backing="CronJob"`, Detail contains
    "backs CronJob".
  - Job-backed → `Expected=true`, `Backing="Job"`.
  - DaemonSet desired 0 → `Expected=true`, `Backing="DaemonSet"`, "0 desired".
  - Deployment desired 0 → `Expected=true`, "scaled to 0".
  - StatefulSet desired 0 → `Expected=true`, "scaled to 0".
  - No matching backend → primary plain.
  - Deployment desired>0 AND a coincidental matching CronJob → primary (real
    outage wins).
  - Service with ready endpoints → no issue regardless of backends.
- existing `Assess` tests pass `nil` backends and keep their current expectations
  (every NoEndpoints issue is primary plain — backward compatible).
- report: an expected issue renders the parenthetical Detail; a primary issue is
  unchanged. JSON: `expected`/`backing` present on an expected issue, absent on a
  primary one.

## Out of scope (explicit non-goals)

- Annotating primary (real-outage) issues with their backing workload (the
  workloads section already shows the Deployment; keep real issues plain).
- Moving expected issues to a separate section or muting the glyph (the chosen
  behavior is annotate-in-place).
- Matching via EndpointSlice `targetRef` (fails at zero endpoints) or
  name/namespace heuristics.
- ReplicaSet-level or bare-pod backing correlation.
- Any change to LoadBalancer `NoExternalAddress` detection.
