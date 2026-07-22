# Service-no-endpoints root cause — design

**Status:** approved · **Date:** 2026-07-22 · **Type:** root-cause enrichment (Theme A)

## Problem

When kubeagent flags a Service with no ready endpoints, a genuinely broken one
(not intentionally empty) renders only `no ready endpoints` — the operator still
has to work out *why*. Today `svchealth` reasons about **workload template
labels** (its `Backend` view) to tell an *expected*-empty Service (a Job, or a
controller scaled to zero) from a broken one, but it never looks at actual pods,
so it can't explain a broken one. This adds a read-only correlation that, for a
broken `NoEndpoints` Service, names the cause by cross-referencing the selector
against the collected pods and the down-node list.

This is the first Theme-A (root-cause) step for the Service → Pod → Node graph,
reusing the same `selectorMatches` and `DownNodes` machinery the workload
root-cause attribution already uses.

## Behavior (approved)

The broken `NoEndpoints` issue's existing `Detail` is enriched in place, so it
flows through the current NEEDS ATTENTION rendering unchanged:

```text
✗ shop/api  ClusterIP  no ready endpoints — 3 matching pods, 0 ready
✗ shop/web  ClusterIP  no ready endpoints — the selector matches no pods
✗ shop/cache  ClusterIP  no ready endpoints — matching pods on down node worker-2 (NotReady)
```

For each issue with `Problem == "NoEndpoints" && !Expected`, resolve its Service
by (namespace, name), match `spec.selector` against pods in that namespace, and
rewrite `Detail` to `no ready endpoints — <cause>` per the **first** matching
cause:

| Cause | Condition | Detail suffix |
|---|---|---|
| no-pods | the selector matches zero pods in the namespace | `the selector matches no pods` |
| node-down | ≥1 matching pod is scheduled on a node in `downNodes` | `matching pods on down node <name> (<reason>)` — or `matching pods on <N> down nodes` when more than one |
| pods-not-ready | matches N pods, 0 with a true `Ready` condition, none on a down node | `<N> matching pod(s), 0 ready` |

- **Precedence:** no-pods → node-down → pods-not-ready (a down node is the
  higher-value "why" than a generic not-ready, so it wins over pods-not-ready).
- **Left untouched:** an issue where matching pods exist and ≥1 is `Ready` (a
  Ready matching pod that somehow isn't an endpoint — rare/inconclusive) keeps
  the original `no ready endpoints`. **Expected**-empty issues (annotated, or a
  scaled-to-zero / Job backing) are never touched — the annotator skips
  `Expected == true`.
- **Readiness** = the pod's `Ready` **condition** is `True` (matches
  EndpointSlice membership semantics), not merely container readiness.
- **node-down node set** = the distinct `spec.nodeName`s of matching pods that
  appear in `downNodes`; an unscheduled pod (empty `nodeName`) never counts as
  node-down and falls through to pods-not-ready.

## Design

### 1. `svchealth.AnnotateEndpointCause` — the correlation (pure)

```go
// AnnotateEndpointCause enriches the Detail of every broken NoEndpoints issue with
// the reason its Service has no ready endpoints, correlating the Service selector
// against pods and the down-node list. Pure and read-only; mutates the issues in
// place. Expected-empty and non-NoEndpoints issues are left untouched.
func AnnotateEndpointCause(issues []Issue, services []corev1.Service, pods []corev1.Pod, downNodes []clusterhealth.DownNode)
```

- Builds `down` (`map[nodeName]reason`) from `downNodes` and `svcByID`
  (`map["ns/name"]Service`) from `services`.
- For each issue with `Problem == "NoEndpoints" && !Expected`, look up its
  Service; compute the cause via `endpointCause(svc, pods, down)`; if non-empty,
  set `Detail = "no ready endpoints — " + cause`.
- `endpointCause`:
  1. `matching` = pods where `p.Namespace == svc.Namespace &&
     selectorMatches(svc.Spec.Selector, p.Labels)` (reuses the existing
     unexported `selectorMatches`).
  2. `len(matching) == 0` → `"the selector matches no pods"`.
  3. collect distinct `p.Spec.NodeName` of matching pods that are in `down`;
     if any → one node: `"matching pods on down node <name> (<reason>)"`; more
     than one: `fmt.Sprintf("matching pods on %d down nodes", n)`.
  4. count matching pods with a true `Ready` condition; if `0` → `"N matching
     pod, 0 ready"` for N==1 else `"N matching pods, 0 ready"` (inline
     singular/plural — `svchealth` has no `plural` helper and this is the only
     site that needs it).
  5. else `""` (leave original Detail).
- New helper `podReady(p corev1.Pod) bool` — scans `p.Status.Conditions` for
  `Type == corev1.PodReady && Status == corev1.ConditionTrue`.
- New import: `clusterhealth` (for `DownNode`; `clusterhealth` does not import
  `svchealth`, so no cycle).

### 2. `scan.Evaluate` — one wiring line

Immediately after `serviceIssues := svchealth.Assess(...)` **and** once `health`
(hence `health.DownNodes`) is computed — place the call at whichever of those two
points comes later so both are in scope:

```go
	svchealth.AnnotateEndpointCause(serviceIssues, svcs, inputs.Pods, health.DownNodes)
```

`svcs` (`collect.Services`), `inputs.Pods`, and `health.DownNodes` are all
already collected/computed in `Evaluate`. No new collector, no `Result` field
(the enrichment lives inside the existing `serviceIssues`).

### 3. `report` — no change

`printServiceIssues` already renders `Issue.Detail`; the enriched text appears
automatically. JSON already serializes `Detail`. The "N services without
endpoints" attention-line count is unchanged (the annotator edits text, never
`Expected` or the issue set).

## Global constraints

- **Read-only; always-on; no flag.** No new RBAC, collector, watch gauge, or
  `resultInput` seam change — pods, nodes, and services are already collected and
  the enriched `Detail` flows through existing rendering. Touches only
  `internal/svchealth` (pure) + one line of `internal/scan` → **LIGHTWEIGHT
  SMOKE** gate at release (a Kind cluster with a broken Service). **Minor** bump
  v0.37.0 → **v0.38.0**; **patch** chart bump (no Helm template change).
- **Pure & deterministic** — `AnnotateEndpointCause` reads only the passed
  objects; no clock, no cluster calls. Idempotent (re-running yields the same
  Detail).
- **Advisory** — does not change the cluster verdict or which Services are
  flagged; only enriches the explanation text.
- `inventory`, `clusterhealth`, `rootcause`, `explain.go`, `--fix` stay
  **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.**

## Out of scope (YAGNI)

Correlating to *why* the matching pods are not ready (CrashLoop/ImagePull — the
workload detector already flags those separately); evaluating readiness gates or
per-container readiness beyond the `Ready` condition; the port/targetPort
mismatch case (a Service whose pods are ready but on the wrong port — a distinct
future check); mutating `Expected`-empty issues; a new JSON field or watch metric
(the cause rides on the existing `Detail`).

## Testing

- **`svchealth.AnnotateEndpointCause` (pure, fake objects):**
  - no-pods: a broken NoEndpoints issue for `shop/web` + a Service `shop/web`
    with a selector no pod matches → Detail `no ready endpoints — the selector
    matches no pods`.
  - node-down: `shop/cache` with 2 matching pods both on node `worker-2`, and
    `worker-2` in `downNodes` (reason "NotReady") → Detail `… — matching pods on
    down node worker-2 (NotReady)`.
  - node-down multiple: matching pods split across two down nodes → `… — matching
    pods on 2 down nodes`.
  - pods-not-ready: 3 matching pods, all with `Ready=False`, none on a down node
    → `… — 3 matching pods, 0 ready`.
  - untouched: an `Expected == true` NoEndpoints issue keeps its Detail; a
    `NoExternalAddress` issue is unchanged; a broken issue whose Service has a
    Ready matching pod keeps `no ready endpoints`.
  - idempotent: running twice yields the same Detail.
- **`scan` integration:** a fake clientset with a Service (selector `app=web`),
  no matching pods, and no endpoints → `Result.ServiceIssues` contains the
  `shop/web` issue with the enriched no-pods Detail (proves the wiring runs after
  Assess with pods + downNodes).
- **Golden:** update the fixture's broken `NoEndpoints` service Detail to an
  enriched form (e.g. `no ready endpoints — 2 matching pods, 0 ready`) so the
  snapshot demonstrates the feature; regenerate. The Expected-empty service Detail
  and the attention line are unchanged.

## Files touched

- **Modify:** `internal/svchealth/svchealth.go` (+ test) — `AnnotateEndpointCause`, `endpointCause`, `podReady`.
- **Modify:** `internal/scan/scan.go` (+ test) — the one wiring line + integration test.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — enriched broken-service Detail + regenerate.
- **Docs:** `website/docs/features/service-health.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
