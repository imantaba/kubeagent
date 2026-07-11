# Ingress Route Health — Design

**Date:** 2026-07-10
**Status:** Approved

## Goal

Answer "why is this ingress returning 502?" with a root cause. For each Ingress
routing rule, resolve its backend Service and flag the routes whose chain is
broken — the usual causes of a 502/503 from an ingress controller:

- the backend **Service does not exist**,
- the backend Service has **no ready endpoints** (backend pods down/unready),
- the referenced **port is not exposed** by the Service.

Read-only; advisory (does not change the cluster verdict). Adds a benign,
read-only `ingresses` RBAC grant (on by default).

## Data source

Ingress objects are `networking.k8s.io/v1` `Ingress`. They are **not** currently
collected and `ingresses` is **not** in the RBAC (only `ingressclasses` is).
Endpoint readiness is already computed by `svchealth` from the Services +
EndpointSlices the scan already collects.

## Components

### `internal/collect`

New helper, mirroring `Services`/`EndpointSlices`:

```go
func Ingresses(ctx context.Context, client kubernetes.Interface, namespace string) ([]networkingv1.Ingress, error)
```

(`collect.go` already imports `networkingv1` for IngressClasses.)

### `internal/svchealth` (export the endpoint-readiness helper)

Rename the existing unexported `readyEndpoints` to exported `ReadyEndpoints`
(same signature `func ReadyEndpoints(svc corev1.Service, slices []discoveryv1.EndpointSlice) int`)
and update its internal caller. This lets `ingresshealth` reuse the exact same
endpoint-counting logic instead of duplicating it. No behavior change.

### `internal/ingresshealth` (new, pure)

```go
package ingresshealth

type RouteIssue struct {
    Namespace string `json:"namespace"`
    Ingress   string `json:"ingress"`
    Host      string `json:"host,omitempty"`
    Path      string `json:"path,omitempty"`
    Service   string `json:"service"`
    Port      string `json:"port,omitempty"`
    Problem   string `json:"problem"` // "NoService" | "NoEndpoints" | "PortNotExposed"
    Detail    string `json:"detail"`
}

func Assess(ingresses []networkingv1.Ingress, services []corev1.Service, slices []discoveryv1.EndpointSlice) []RouteIssue
```

For each Ingress, walk the default backend (`spec.defaultBackend`) and every
`spec.rules[].http.paths[]`. For each that has a **Service** backend
(`backend.service`; skip `backend.resource`), resolve the Service **in the
Ingress's namespace** (k8s forbids cross-namespace ingress backends) and emit at
most one `RouteIssue`:

- Service not found → `Problem: "NoService"`,
  `Detail: "backend Service <name> not found"`.
- Service found, `ReadyEndpoints(svc, slices) == 0` → `Problem: "NoEndpoints"`,
  `Detail: "backend Service <name>:<port> has no ready endpoints (likely 502/503)"`.
- Service found with ready endpoints, but the referenced port is not in
  `svc.Spec.Ports` → `Problem: "PortNotExposed"`,
  `Detail: "backend Service <name> does not expose port <port>"`.
- Otherwise (Service exists, has endpoints, port matches) → no issue.

Port matching: an `IngressServiceBackend.Port` has either a `Name` (string) or a
`Number` (int32). Match `Name` against `svc.Spec.Ports[].Name`, or `Number`
against `svc.Spec.Ports[].Port`. `Port` string for output is the name when set,
else the number. When the backend specifies no port and the Service has exactly
one port, treat it as matched (no PortNotExposed); NoEndpoints still applies.

`Host`/`Path` come from the rule (empty for the default backend). Pure and
unit-testable with fake Ingresses/Services/EndpointSlices; ordering follows the
input (ingress order, then default backend, then rule/path order).

### `internal/scan`

`Result` gains `IngressIssues []ingresshealth.RouteIssue`. `Evaluate` lists
ingresses (`collect.Ingresses(ctx, client, opts.Namespace)`, best-effort like the
other secondary collectors) and calls
`ingresshealth.Assess(ings, svcs, slices)` using the Services/EndpointSlices it
already gathered for `svchealth`.

### `internal/report` (text + JSON)

- `Input` gains `IngressIssues []ingresshealth.RouteIssue`; `inventoryReport`
  gains `IngressIssues []ingresshealth.RouteIssue` with tag `ingressIssues,omitempty`.
- Render in **NEEDS ATTENTION** with `✗`, after the service issues / disk lines:
  ```
  ✗ ingress shop/web  example.com/api → api-svc:8080  no ready endpoints (likely 502/503)
  ```
  Host+Path shown when present (`<host><path>`); `→ <service>[:<port>]`; then the
  Detail. `hasAttention` becomes true when `len(IngressIssues) > 0`, so a broken
  route trips the zone and suppresses the all-clear.
- The header attention line gains `N ingress route(s) broken`
  (singular/plural via the existing `plural` helper).

### `internal/watch/metrics.go` (daemon)

New gauge `kubeagent_ingress_route_issues` = `len(res.IngressIssues)`.
HELP: "Ingress routes whose backend Service is missing, has no ready endpoints,
or does not expose the referenced port."

### RBAC

Add `ingresses` to the `networking.k8s.io` rule in `deploy/rbac.yaml` and the
Helm ClusterRole (alongside `networkpolicies`, `ingressclasses`), verbs
`get`/`list`/`watch`. On by default — plain read-only, unlike the opt-in
`nodes/proxy` grant.

## Scope boundaries

- **Advisory** — does not change the cluster verdict, `kubeagent_cluster_healthy`,
  or the scan exit code (consistent with Service issues).
- HTTP rules + default backend only; **Service** backends only (Resource backends
  skipped); no TLS-secret or annotation checks (not a 502 cause).
- Overlap with Service issues is intentional and not deduped — the ingress route
  issue names the public-route impact.
- Not wired into `--fix` or `--explain`.

## Testing

- `ingresshealth.Assess` (fake objects): NoService; NoEndpoints; PortNotExposed
  by name and by number; healthy route → no issue; default backend; multiple
  rules/paths; Resource backend skipped; single-port no-name match.
- `collect.Ingresses`: fake clientset returns the seeded Ingress.
- `svchealth`: the rename compiles and existing svchealth tests still pass
  (behavior unchanged).
- `report`: a route issue renders in NEEDS ATTENTION + the attention-line count;
  JSON carries `ingressIssues`; no ingress lines when the slice is empty.
- `watch/metrics`: `kubeagent_ingress_route_issues` reflects the count.
- RBAC render: `ingresses` present, no write verbs.

## Docs

- `CHANGELOG.md` `[Unreleased]`.
- `website/docs/features/diagnostics.md` (+ a mention in `service-health.md` if it
  cross-references), `features/watch-mode.md` (the new gauge), `deploy/README.md`
  RBAC note, `README.md` feature list, and the roadmap Shipped entry.
