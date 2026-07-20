# Quiet intentionally-empty endpoints — design

**Status:** approved · **Date:** 2026-07-20 · **Type:** behavior change (2 checks)

## Problem

kubeagent flags a Service/Ingress route with no ready endpoints. Two real false
positives surfaced on a live cluster:

- **superset (Gap A):** a Deployment scaled to 0. `svchealth` already marks its Service
  issue `Expected` (a quiet NOTE), but the **`ingresshealth`** route check has no such
  awareness and independently flags the route as a `502/503` in **NEEDS ATTENTION**.
- **paradedb-cluster-ro (Gap B):** a CloudNativePG read-only Service selecting
  `cnpg.io/instanceRole=replica`. Its pods are **operator-managed** (the CNPG `Cluster`
  CR creates pods directly — no Deployment/StatefulSet backing), and on a single-instance
  cluster there are no replicas, so it is empty **by design**. kubeagent can't infer this,
  so it flags a real-looking outage.

## What already exists (do not duplicate)

`svchealth.Assess` calls `classifyBacking(svc, backends)`: when a NoEndpoints Service's
selector matches a Deployment/StatefulSet scaled to 0, a DaemonSet with 0 desired, or a
Job/CronJob (ephemeral, between runs), it sets `Issue.Expected=true` with a `backingDetail`
string. `report.splitServiceIssues` sends `Expected` service issues to the quiet **NOTES**
section (`•`); the attention line counts only the real ones. This machinery is correct and
**stays unchanged** for the Service path.

## Scope

**In:**
- **Gap A** — teach `ingresshealth` the same "backend intentionally empty" awareness by
  reusing `svchealth`'s classification, so a route to a scaled-to-0 / between-runs backend
  becomes a quiet NOTE, not a NEEDS-ATTENTION 502.
- **Gap B** — honor a Service annotation **`kubeagent.io/expected-empty: "true"`** as an
  explicit operator declaration that empty endpoints are intentional. Marks the Service
  issue `Expected`, and (through Gap A) any Ingress route to it. Works for CNPG `-ro` and
  any operator-managed / role-split Service kubeagent can't infer.

**Out of scope (YAGNI):**
- CNPG-specific CRD collection (no new RBAC, no operator coupling).
- Auto "role-subset" heuristics (fragile; the annotation is the honest escape hatch).
- The `kubeagent_service_issues` / `kubeagent_ingress_route_issues` **watch gauges** keep
  counting raw totals (as the service gauge already does today) — `internal/watch` stays
  untouched. A separate follow-up if the gauges should exclude `Expected`.
- Any annotation on the Ingress itself (only the backend **Service** carries it).

## Design

### 1. `svchealth` — the annotation + the shared decision

New exported constant and function; existing `classifyBacking`/`backingDetail` unchanged.

```go
// ExpectedEmptyAnnotation, when set to "true" on a Service, declares that the Service is
// meant to have no ready endpoints (e.g. an operator-managed role-split Service).
const ExpectedEmptyAnnotation = "kubeagent.io/expected-empty"

func annotatedExpectedEmpty(svc corev1.Service) bool {
	return strings.EqualFold(svc.Annotations[ExpectedEmptyAnnotation], "true")
}

// ExpectedEmpty reports whether a Service's lack of ready endpoints is intentional, with a
// short reason. It is the shared decision used by ingresshealth. Order: the annotation
// (an absolute operator declaration), then a scaled-to-0 / between-runs backend.
func ExpectedEmpty(svc corev1.Service, backends []Backend) (reason string, ok bool) {
	if annotatedExpectedEmpty(svc) {
		return "declared via " + ExpectedEmptyAnnotation, true
	}
	if backing, _, ok := classifyBacking(svc, backends); ok {
		return backingReason(backing), true
	}
	return "", false
}

// backingReason is the short ingress-facing phrase for a scaled-to-0 / between-runs backend.
func backingReason(kind string) string {
	switch kind {
	case "Job", "CronJob":
		return "expected between runs"
	case "DaemonSet":
		return "0 desired"
	default: // Deployment, StatefulSet
		return "scaled to 0"
	}
}
```

`svchealth.Assess` — the NoEndpoints branch gains **one** `else if` for the annotation
(the backend path is unchanged, so no existing service-note wording churns):

```go
if ReadyEndpoints(s, slices) == 0 {
	is := Issue{Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
		Problem: "NoEndpoints", Detail: "no ready endpoints"}
	if backing, detail, ok := classifyBacking(s, backends); ok {
		is.Expected = true
		is.Backing = backing
		is.Detail = detail
	} else if annotatedExpectedEmpty(s) {
		is.Expected = true
		is.Detail = "no ready endpoints — declared via " + ExpectedEmptyAnnotation
	}
	out = append(out, is)
}
```

**Absolute-override note:** `classifyBacking` returns `ok=false` when a live backend
(`desired>0`) exists; the annotation `else if` then still fires, so an annotated Service is
`Expected` even with a live backend — the operator's explicit call, as approved.

### 2. `ingresshealth` — reuse the decision

`RouteIssue` gains `Expected bool` (`json:"expected,omitempty"`). `Assess` takes the
already-computed `backends`:

```go
func Assess(ingresses []networkingv1.Ingress, services []corev1.Service, slices []discoveryv1.EndpointSlice, backends []svchealth.Backend) []RouteIssue
```

In `check` (threading `backends` through), the NoEndpoints branch becomes:

```go
if svchealth.ReadyEndpoints(svc, slices) == 0 {
	r.Problem = "NoEndpoints"
	if reason, ok := svchealth.ExpectedEmpty(svc, backends); ok {
		r.Expected = true
		r.Detail = fmt.Sprintf("backend Service %s is intentionally empty (%s) — route parked", be.Name, reason)
	} else if port != "" {
		r.Detail = fmt.Sprintf("backend Service %s:%s has no ready endpoints (likely 502/503)", be.Name, port)
	} else {
		r.Detail = fmt.Sprintf("backend Service %s has no ready endpoints (likely 502/503)", be.Name)
	}
	return r, true
}
```

`NoService` and `PortNotExposed` branches are unchanged (a missing Service or a genuine
port mismatch is always a real problem).

### 3. `scan.Evaluate` wiring

One argument added — `backends` already exists at that point:

```go
backends := svchealth.BackendsFrom(...)          // existing (line ~156)
serviceIssues := svchealth.Assess(svcs, slices, backends)   // existing
ingressIssues := ingresshealth.Assess(ings, svcs, slices, backends)   // + backends
```

### 4. `report` — split ingress issues like services

- `splitIngressIssues(issues []ingresshealth.RouteIssue) (real, expected []ingresshealth.RouteIssue)` — mirrors `splitServiceIssues`.
- `printInventoryText`: compute `realIng, expectedIng := splitIngressIssues(in.IngressIssues)`.
- `hasAttention` uses `len(realIng) > 0` (not `len(in.IngressIssues)`).
- NEEDS ATTENTION renders `printIngressIssues(realIng, "  ✗", w)`.
- `printIngressIssues` gains a `glyph string` parameter (like `printServiceIssues`).
- `attentionLine` counts `realIng`, not `len(in.IngressIssues)` — signature gains `realIng`.
- `printHeader` threads `realIng` to `attentionLine`.
- `printNotes` gains `expectedIng` and renders `printIngressIssues(expectedIng, "  •", &b)` right after the expected services.

Result: the superset route moves from an alarm to
`• ingress superset/superset  infrasuperset.journals.ekb.eg/  backend Service superset is intentionally empty (scaled to 0) — route parked`,
and (once annotated) a route to `paradedb-cluster-ro` reads `(declared via kubeagent.io/expected-empty)`.

## Global constraints

- **Read-only; NO new RBAC / no new collector.** Services/Ingresses/EndpointSlices are
  already listed; the annotation rides on Service objects already fetched. Not
  `internal/collect` → **lightweight real-cluster smoke** gate at release; **patch** bump.
- **Pure & deterministic** — `ExpectedEmpty`/`Assess` are pure functions of their inputs.
- **Always-on** — no flag; runs in `scan` and the `watch` daemon via `scan.Evaluate`.
- `internal/watch`, `internal/collect`, `explain.go`, RBAC, and Helm stay **unchanged**.
  The service-path `backingDetail` wording stays **unchanged** (no service-note churn).
- **No `Co-Authored-By: Claude` trailer** on any commit. TDD.

## Testing

- **`svchealth`** — a NoEndpoints Service annotated `kubeagent.io/expected-empty: "true"` →
  `Expected=true` (both with no backing AND with a live `desired>0` backend, proving the
  override); annotation `"false"`/absent → not expected; existing `classifyBacking` tests
  unchanged. `ExpectedEmpty` returns the right reason per case (annotation, Deployment,
  DaemonSet, CronJob) and `ok=false` for a live-unbacked Service.
- **`ingresshealth`** — route to a scaled-to-0 Deployment backend → `Expected=true`, detail
  contains "route parked"; route to an annotated Service → `Expected=true`; route to a
  genuinely-broken backend (`desired>0`, 0 ready) → `Expected=false`, detail contains
  "502/503"; `NoService` / `PortNotExposed` still real (Expected=false).
- **`report`** — an `Expected` ingress issue renders under NOTES (`•`) and is excluded from
  the attention line and `hasAttention`; a real ingress issue stays in NEEDS ATTENTION and
  counts. `splitIngressIssues` partitions correctly.
- **Golden** — add to the fixture: (a) an Ingress route whose backend is scaled to 0
  (renders under NOTES), keeping at least one real ingress issue in NEEDS ATTENTION; verify
  the attention-line ingress count reflects only the real one. Regenerate `golden-scan.txt`.

## Files touched

- **Modify:** `internal/svchealth/svchealth.go` (+ `svchealth_test.go`) — `ExpectedEmptyAnnotation`, `ExpectedEmpty`, `backingReason`, annotation branch in `Assess`.
- **Modify:** `internal/ingresshealth/ingresshealth.go` (+ `ingresshealth_test.go`) — `Expected` field, `backends` arg, expected branch in `check`.
- **Modify:** `internal/scan/scan.go` (+ `scan_test.go`) — pass `backends` to `ingresshealth.Assess`.
- **Modify:** `internal/report/report.go` (+ `report_test.go`) — `splitIngressIssues`, glyph-parameterized `printIngressIssues`, thread `realIng`/`expectedIng`.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — fixture + snapshot.
- **Docs:** `website/docs/features/diagnostics.md` (Ingress route health subsection + the `kubeagent.io/expected-empty` annotation), `README.md`, `CHANGELOG.md`. **Not** `watch-mode.md` — the metric gauges are unchanged (out of scope).

## Non-goals recap

CNPG CRD collection; role-subset auto-heuristics; watch-gauge filtering; Ingress-level
annotations; any change to `NoService`/`PortNotExposed`, the service-path backing wording,
`internal/watch`, `internal/collect`, or RBAC.
