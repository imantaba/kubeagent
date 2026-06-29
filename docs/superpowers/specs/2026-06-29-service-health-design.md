# kubeagent — Design: Service / LoadBalancer health (gap-feature A)

**Status:** approved design (pre-implementation)
**Date:** 2026-06-29

## Goal

Detect Service-level failures the workload/pod scan misses (chaos test gap #6 and
the common "selector typo / all backends down" blackhole):

1. A selector-based Service routing to **zero ready endpoints**.
2. A **LoadBalancer Service with no external address**.

Surface them in a flat "Service issues" section (text + JSON) and in `--explain`.

## Decisions (from brainstorming)

- **Both checks:** no-endpoints **and** LB-no-address.
- **Flat section, no P1 elevation:** all flagged Services (any namespace in scope)
  list in one section with the namespace shown; the cluster-health verdict logic
  is unchanged. (kube-system elevation is a possible later refinement.)
- **Endpoint source:** EndpointSlices (`discovery.k8s.io/v1`), matched to a
  Service by the `kubernetes.io/service-name` label.
- **Param handling:** `serviceIssues` is added as one more parameter to
  `PrintInventory`/`ExplainInventory`, consistent with how `Summary`/`Facts` were
  added. The deferred `report.Input` struct refactor is **out of scope here** and
  will be done as its own behavior-preserving step before Feature B.

## Invariants preserved

- **READ-ONLY:** only new List calls (Services, EndpointSlices). Never mutate.
- **No new Go module dependency.** `k8s.io/api/discovery/v1` is a subpackage of
  the already-required `k8s.io/api`.
- **Sequential**, stdlib `flag`, exit codes unchanged.
- **Namespace scope:** Services are namespaced, so they honor the scan's `-n`
  scope (like workloads) — NOT forced cluster-wide.
- **`--explain` egress:** only Service namespace/name/type/problem strings — never
  pod IPs, endpoint IPs, raw specs, or secrets.
- **Best-effort:** List failures in `main` are non-fatal (empty input → no issues).

## Architecture

```text
collect (+Services, +EndpointSlices in scope)
      → svchealth.Assess(services, slices)  ← new pure step
      → report / explain  (alongside workloads, summary, facts)
```

## Component 1 — `internal/svchealth` (pure)

```go
// Issue is one Service-level problem.
type Issue struct {
    Namespace string `json:"namespace"`
    Name      string `json:"name"`
    Type      string `json:"type"`            // ClusterIP | NodePort | LoadBalancer
    Problem   string `json:"problem"`         // "NoEndpoints" | "NoExternalAddress"
    Detail    string `json:"detail"`          // human one-liner, e.g. "no ready endpoints"
    Since     string `json:"since,omitempty"` // RFC3339 service creationTimestamp (LB age)
}

// Assess flags Service-level problems. One Issue per (service, problem); a
// LoadBalancer with no address AND no endpoints yields two Issues. Result sorted
// by (Namespace, Name, Problem).
func Assess(services []corev1.Service, slices []discoveryv1.EndpointSlice) []Issue
```

Rules, per Service:
- `Type == ExternalName` → skip entirely (no endpoints/LB concept).
- `Type == LoadBalancer` && `len(Status.LoadBalancer.Ingress) == 0` →
  `NoExternalAddress`, Detail `"no external address"`, `Since` = the Service's
  `CreationTimestamp` (RFC3339 UTC) so the report can show its age.
- If `len(Spec.Selector) == 0` → skip the endpoints check (selectorless /
  manually-managed, e.g. `kubernetes.default`).
- Else count **ready** endpoints across the Service's EndpointSlices and emit
  `NoEndpoints` (Detail `"no ready endpoints"`) when the count is 0.

Helper `readyEndpoints(svc, slices) int`:
- A slice belongs to the Service when `slice.Namespace == svc.Namespace` and
  `slice.Labels["kubernetes.io/service-name"] == svc.Name`
  (use the `discoveryv1.LabelServiceName` constant).
- An endpoint counts as ready when `ep.Conditions.Ready == nil || *ep.Conditions.Ready`
  (kube-proxy treats nil as ready).
- Each ready endpoint contributes `len(ep.Addresses)` (usually 1).

Sorting uses `sort.Slice` by Namespace, then Name, then Problem.

## Component 2 — collection (`internal/collect`)

```go
func Services(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Service, error)
func EndpointSlices(ctx context.Context, client kubernetes.Interface, namespace string) ([]discoveryv1.EndpointSlice, error)
```

- `Services` → `client.CoreV1().Services(namespace).List(...)`, wraps error.
- `EndpointSlices` → `client.DiscoveryV1().EndpointSlices(namespace).List(...)`,
  wraps error.
- Both take the scan's `namespace` (empty = all namespaces).

## Component 3 — surfacing

### text (`internal/report`)

A new section prints after the workloads list and before the footer/explanation,
only when there are issues:

```text
Service issues:
  ⚠ default/web     ClusterIP     no ready endpoints
  ⚠ default/api-lb  LoadBalancer  no external address · 6m ago
```

- One line per Issue: `  ⚠ <ns>/<name>  <Type>  <Detail>`.
- When `Since` is set (the `NoExternalAddress` case), append
  `" · " + inventory.HumanSince(Since, time.Now())` — which yields e.g.
  `· 6m ago` — so the operator can tell a freshly-provisioning LB from a
  long-stuck one.
- The **all-clear** line (`No issues found. ✅`) now prints only when the cluster
  is Healthy AND the filtered workload list is empty AND there are no service
  issues.

### json (`internal/report`)

`inventoryReport` gains `ServiceIssues []svchealth.Issue`
(`json:"serviceIssues,omitempty"`).

### explain (`internal/explain`)

`buildInventoryPrompt` emits, when there are issues:

```text
Service issues:
  - default/web (ClusterIP): no ready endpoints
  - default/api-lb (LoadBalancer): no external address
```

`ExplainInventory` takes the `[]svchealth.Issue` as a new parameter. The
healthy-and-empty skip is widened: skip the API call only when the cluster is
Healthy AND there are no workloads AND no service issues.

## Component 4 — wiring (`main.go`)

After the existing collect/assemble steps:

```go
svcs, _ := collect.Services(context.Background(), client, namespace)
slices, _ := collect.EndpointSlices(context.Background(), client, namespace)
serviceIssues := svchealth.Assess(svcs, slices)
```

`serviceIssues` is passed to `report.PrintInventory` and
`explain.ExplainInventory`.

## Testing (TDD)

- `svchealth.Assess` — table tests: selector Service with 0 ready endpoints →
  NoEndpoints; with ready endpoints → none; ExternalName → skipped; selectorless
  → skipped; LoadBalancer with no ingress → NoExternalAddress with `Since`;
  LoadBalancer with ingress → none; a LB with no address AND no endpoints → two
  issues; nil `Conditions.Ready` counts as ready; sorting order.
- `collect` — `Services` and `EndpointSlices` via the fake clientset (objects
  across namespaces; namespace scoping).
- `report` — Service-issues section renders (incl. the LB age line); JSON carries
  `serviceIssues`; the all-clear line is suppressed when only service issues
  exist; no section when empty.
- `explain` — prompt includes the Service-issues block when present and omits it
  when empty; egress guard (no endpoint IPs / pod IPs leak).
- `main` — wiring stays green; List failures are non-fatal.

## Out of scope (explicit non-goals)

- Elevating kube-system Service issues to the P1 cluster verdict (later refinement).
- Inspecting Service ports / targetPort mismatches, session affinity, or
  Ingress objects.
- Distinguishing "LB provisioning" from "LB failed" beyond showing the age.
- The `report.Input` struct refactor (separate, behavior-preserving step before
  Feature B).
- Selectorless Services with manually-managed Endpoints (skipped to avoid false
  positives).
