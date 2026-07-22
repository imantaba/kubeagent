# Ingress-route root cause — design

**Status:** approved · **Date:** 2026-07-22 · **Type:** root-cause enrichment (Theme A)

## Problem

kubeagent flags a broken ingress route as `backend Service X:80 has no ready
endpoints (likely 502/503)` — it names the Service but not *why* the Service has
no endpoints. The just-shipped Service-no-endpoints root cause added exactly that
"why" to the Service finding; this extends the same explanation up one hop so the
ingress finding is self-contained. An operator debugging a 502 sees the full
chain on the route itself: **Ingress → Service → Pod → Node**.

## Behavior (approved)

For a **broken** `NoEndpoints` ingress route (not a parked one), the existing
`Detail` gains the backend Service's endpoint cause, appended after the
`(likely 502/503)` phrase:

```text
✗ ingress shop/storefront  shop.example.com/  backend Service api:80 has no ready endpoints (likely 502/503) — matching pods on down node worker-2 (NotReady)
✗ ingress shop/web  web.example.com/  backend Service web has no ready endpoints (likely 502/503) — the selector matches no pods
```

The cause suffix is the same taxonomy the Service check uses:
`the selector matches no pods` / `matching pods on down node <name> (<reason>)` /
`matching pods on <N> down nodes` / `<N> matching pod(s), 0 ready`. When the cause
is inconclusive (a Ready backend pod that isn't an endpoint), nothing is appended
— the route keeps the base `(likely 502/503)` text.

**Never enriched:** a **parked** route (`RouteIssue.Expected == true` — the
backend is intentionally empty / scaled to zero); a `NoService` route (already a
root cause — "backend Service X not found"); a `PortNotExposed` route (already a
root cause). Only the broken `NoEndpoints` branch is touched.

## Design

### 1. `svchealth.EndpointCause` — export the shared cause logic

The Service-no-endpoints feature computes the cause in an unexported
`endpointCause(svc corev1.Service, pods []corev1.Pod, down map[string]string) string`.
Export a thin wrapper so both the Service and Ingress views share one
implementation:

```go
// EndpointCause returns the reason a Service has no ready endpoints (the selector
// matches no pods, its matching pods are on a down node, or none are Ready), or ""
// when inconclusive. Pure; used by both the Service and Ingress root-cause views.
func EndpointCause(svc corev1.Service, pods []corev1.Pod, downNodes []clusterhealth.DownNode) string {
	down := make(map[string]string, len(downNodes))
	for _, d := range downNodes {
		down[d.Name] = d.Reason
	}
	return endpointCause(svc, pods, down)
}
```

- `AnnotateEndpointCause` keeps its existing batch path (build the map once, call
  the internal `endpointCause` per issue) — no behavior change, no per-issue map
  rebuild. `EndpointCause` is the single-call entry point for the ingress path.

### 2. `ingresshealth.Assess` / `check` — enrich the broken route

`Assess` gains two parameters (pods + down-node list), threaded to `check`:

```go
func Assess(ingresses []networkingv1.Ingress, services []corev1.Service, slices []discoveryv1.EndpointSlice, backends []svchealth.Backend, pods []corev1.Pod, downNodes []clusterhealth.DownNode) []RouteIssue
```

In `check`, the `NoEndpoints` branch stays as-is for the parked case (returns
early with `Expected = true`); the broken case builds the base detail and appends
the cause:

```go
	if svchealth.ReadyEndpoints(svc, slices) == 0 {
		r.Problem = "NoEndpoints"
		if reason, ok := svchealth.ExpectedEmpty(svc, backends); ok {
			r.Expected = true
			r.Detail = fmt.Sprintf("backend Service %s is intentionally empty (%s) — route parked", be.Name, reason)
			return r, true
		}
		base := fmt.Sprintf("backend Service %s has no ready endpoints (likely 502/503)", be.Name)
		if port != "" {
			base = fmt.Sprintf("backend Service %s:%s has no ready endpoints (likely 502/503)", be.Name, port)
		}
		if cause := svchealth.EndpointCause(svc, pods, downNodes); cause != "" {
			base += " — " + cause
		}
		r.Detail = base
		return r, true
	}
```

New import: `clusterhealth` (for `DownNode`; `svchealth` already imported by
`ingresshealth`, and neither `svchealth` nor `clusterhealth` imports
`ingresshealth`, so no cycle).

### 3. `scan.Evaluate` — pass the two new args

The single caller (`internal/scan/scan.go`) changes from
`ingresshealth.Assess(ings, svcs, slices, backends)` to:

```go
	ingressIssues := ingresshealth.Assess(ings, svcs, slices, backends, inputs.Pods, health.DownNodes)
```

`inputs.Pods` and `health.DownNodes` are already collected/computed above this
line (the Service check on the previous line uses the same two).

### 4. `report` — no change

`printIngressIssues` already renders `RouteIssue.Detail`, and the report already
filters parked routes (`Expected == true`) out of the "real" set
(`splitIngressIssues`). The enriched broken-route text flows through unchanged;
the `N ingress routes broken` count is unaffected (the annotator edits text only,
never `Expected` or the issue set).

## Global constraints

- **Read-only; always-on; no flag.** No new RBAC, collector, watch gauge, or
  `Result` field. Touches `internal/svchealth` (export a wrapper),
  `internal/ingresshealth` (Assess/check), and one line of `internal/scan` →
  **LIGHTWEIGHT SMOKE** gate at release. **Minor** bump v0.38.0 → **v0.39.0**;
  **patch** chart bump (no Helm template change).
- **Pure & deterministic** — `EndpointCause` and the enriched `check` read only
  the passed objects; no clock, no cluster calls. Idempotent.
- **Advisory** — does not change the verdict or the set/count of ingress routes;
  only enriches the broken-route `Detail` text.
- `inventory`, `clusterhealth`, `rootcause`, `explain.go`, `--fix`, and the watch
  daemon stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Enriching `NoService` / `PortNotExposed` routes (already root causes); enriching
parked routes (intentionally empty); explaining *why* the matching pods are not
ready (the workload detector already flags CrashLoop/ImagePull); a new JSON field
or metric (the cause rides on the existing `Detail`); default-backend vs rule
distinction (both go through the same `check`).

## Testing

- **`svchealth.EndpointCause` (pure):** a direct test — a Service whose selector
  matches no pods → `"the selector matches no pods"`; a Service with matching pods
  on a down node → the node phrase; a healthy/inconclusive Service → `""`. (The
  underlying logic is already covered by the `AnnotateEndpointCause` tests; this
  pins the exported entry point and that it builds the down-node map correctly.)
- **`ingresshealth` (pure, fake objects):**
  - broken route, backend Service selector matches no pods → Detail ends
    `… (likely 502/503) — the selector matches no pods`.
  - broken route, matching pods on a down node → `… — matching pods on down node
    worker-2 (NotReady)`.
  - broken route, N matching pods 0 ready → `… — 3 matching pods, 0 ready`.
  - inconclusive (a Ready matching pod) → Detail unchanged (base text only).
  - **parked** route (`ExpectedEmpty` backend) → Detail unchanged
    (`… route parked`), `Expected == true`, never gets a cause suffix.
  - `NoService` and `PortNotExposed` routes → unchanged.
- **`scan` integration:** a fake clientset with an Ingress whose backend Service
  has a selector matching no pods and no endpoints → `Result.IngressIssues` has
  the route with the enriched no-pods Detail (proves the wiring passes pods +
  downNodes).
- **Golden:** update the fixture's broken `storefront` route Detail to the
  enriched form (e.g. `… (likely 502/503) — 3 matching pods, 0 ready`);
  regenerate. The parked/expected ingress route and the broken-route count are
  unchanged.

## Files touched

- **Modify:** `internal/svchealth/svchealth.go` (+ test) — export `EndpointCause`.
- **Modify:** `internal/ingresshealth/ingresshealth.go` (+ test) — `Assess`/`check` params + enrichment.
- **Modify:** `internal/scan/scan.go` (+ test if a scan-level test is added) — pass the two new args.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — enriched route Detail + regenerate.
- **Docs:** `website/docs/features/diagnostics.md` (the ingress-route-health section), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
