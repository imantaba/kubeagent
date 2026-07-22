# Ingress-route root cause — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enrich a broken `NoEndpoints` ingress route's Detail with *why* its backend Service has no ready endpoints (selector matches no pods / matching pods on a down node / pods not ready), reusing the Service check's cause logic across the Ingress → Service → Pod → Node graph.

**Architecture:** Export the existing unexported `svchealth.endpointCause` as `svchealth.EndpointCause`, so one implementation serves both the Service and Ingress views. `ingresshealth.Assess`/`check` gain the pod + down-node inputs and append the cause to the broken (`!Expected`) `NoEndpoints` route Detail. `scan` passes the two already-collected inputs. No report/JSON/watch/RBAC change — the enriched Detail flows through existing rendering.

**Tech Stack:** Go 1.26, `k8s.io/api/core/v1`, `k8s.io/api/networking/v1`, the internal `clusterhealth` (for `DownNode`) and `svchealth` packages. Tests use fake objects.

## Global Constraints

- **READ-ONLY.** Pure correlation over already-collected objects; no cluster calls, no writes, no LLM.
- **Always-on; no flag.** No new RBAC, collector, watch gauge, `Result` field, or `report` change.
- **Advisory** — does not change the verdict or the set/count of ingress routes; only enriches broken-route Detail text.
- **Pure, deterministic, idempotent** — `EndpointCause` and the enriched `check` read only their parameters.
- **v1 uses the standard-library `flag` package only** — no Cobra.
- **No `Co-Authored-By: Claude` trailer** (or any Claude attribution) on any commit. Every commit authored solely by the human.
- **TDD** — write the failing test first, watch it fail, then implement.
- **gofmt-clean** — run `gofmt -w` on changed files before committing.
- Constant: `—` em dash U+2014 in the Detail suffix.
- Only the **broken** `NoEndpoints` route (`RouteIssue.Expected == false`) is enriched — never parked (`Expected`), `NoService`, or `PortNotExposed`.

---

### Task 1: export `svchealth.EndpointCause`

**Files:**
- Modify: `internal/svchealth/svchealth.go` (add the exported wrapper)
- Test: `internal/svchealth/svchealth_test.go` (add direct tests)

**Interfaces:**
- Consumes: the existing unexported `endpointCause(svc corev1.Service, pods []corev1.Pod, down map[string]string) string`.
- Produces: `func EndpointCause(svc corev1.Service, pods []corev1.Pod, downNodes []clusterhealth.DownNode) string`.

- [ ] **Step 1: Write the failing test**

Add to `internal/svchealth/svchealth_test.go` (it already has the `svc(ns, name, type, selector, lbIngress)` and `pod(ns, name, node, labels, ready)` helpers and imports `clusterhealth` from the previous feature):

```go
func TestEndpointCause_NoPods(t *testing.T) {
	s := svc("shop", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)
	if got := EndpointCause(s, nil, nil); got != "the selector matches no pods" {
		t.Fatalf("got %q", got)
	}
}

func TestEndpointCause_NodeDown(t *testing.T) {
	s := svc("shop", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)
	pods := []corev1.Pod{pod("shop", "web-1", "worker-2", map[string]string{"app": "web"}, false)}
	down := []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}}
	if got := EndpointCause(s, pods, down); got != "matching pods on down node worker-2 (NotReady)" {
		t.Fatalf("got %q", got)
	}
}

func TestEndpointCause_Inconclusive(t *testing.T) {
	s := svc("shop", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)
	pods := []corev1.Pod{pod("shop", "web-1", "worker-1", map[string]string{"app": "web"}, true)} // ready
	if got := EndpointCause(s, pods, nil); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/svchealth/ -run TestEndpointCause`
Expected: FAIL — `undefined: EndpointCause`.

- [ ] **Step 3: Write the implementation**

In `internal/svchealth/svchealth.go`, immediately above the existing unexported `endpointCause`, add:

```go
// EndpointCause returns the reason a Service has no ready endpoints (the selector
// matches no pods, its matching pods are on a down node, or none are Ready), or ""
// when inconclusive. Pure; the single-call entry point shared by the Service and
// Ingress root-cause views.
func EndpointCause(svc corev1.Service, pods []corev1.Pod, downNodes []clusterhealth.DownNode) string {
	down := make(map[string]string, len(downNodes))
	for _, d := range downNodes {
		down[d.Name] = d.Reason
	}
	return endpointCause(svc, pods, down)
}
```

Leave `AnnotateEndpointCause` and `endpointCause` unchanged (the batch path keeps building the map once).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/svchealth/`
Expected: PASS (all tests). Then `gofmt -l internal/svchealth/svchealth.go internal/svchealth/svchealth_test.go` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/svchealth/
git commit -m "feat(svchealth): export EndpointCause for cross-graph reuse"
```

---

### Task 2: `ingresshealth` — enrich the broken route

**Files:**
- Modify: `internal/ingresshealth/ingresshealth.go` (`Assess`/`check` params + enrichment; add `clusterhealth` import)
- Test: `internal/ingresshealth/ingresshealth_test.go` (add tests + local helpers)

**Interfaces:**
- Consumes: `svchealth.EndpointCause(svc corev1.Service, pods []corev1.Pod, downNodes []clusterhealth.DownNode) string` (Task 1); the existing `svchealth.ReadyEndpoints`, `svchealth.ExpectedEmpty`.
- Produces: `func Assess(ingresses []networkingv1.Ingress, services []corev1.Service, slices []discoveryv1.EndpointSlice, backends []svchealth.Backend, pods []corev1.Pod, downNodes []clusterhealth.DownNode) []RouteIssue` (two new trailing params).

- [ ] **Step 1: Write the failing test**

Add to `internal/ingresshealth/ingresshealth_test.go` (it imports `corev1`, `networkingv1`, `metav1`; add `discoveryv1 "k8s.io/api/discovery/v1"` and `"github.com/imantaba/kubeagent/internal/clusterhealth"` and `"github.com/imantaba/kubeagent/internal/svchealth"` if not present). Add these local helpers and tests:

```go
// svcSel builds a selector-bearing backend Service (the file's existing svc()
// helper has no selector).
func svcSel(ns, name string, selector map[string]string, port int32) corev1.Service {
	return corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Selector: selector, Ports: []corev1.ServicePort{{Port: port}}},
	}
}

func podLabeled(ns, name, node string, labels map[string]string, ready bool) corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
	p.Spec.NodeName = node
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	return p
}

// ingressTo builds a single-rule Ingress routing host/ to svcName:port.
func ingressTo(ns, name, host, svcName string, port int32) networkingv1.Ingress {
	return networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: "/",
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: svcName, Port: networkingv1.ServiceBackendPort{Number: port},
					}},
				}},
			}},
		}}},
	}
}

func firstDetail(t *testing.T, issues []RouteIssue) string {
	t.Helper()
	if len(issues) != 1 {
		t.Fatalf("want 1 route issue, got %d: %+v", len(issues), issues)
	}
	return issues[0].Detail
}

func TestAssess_BrokenRoute_NoPodsCause(t *testing.T) {
	ing := ingressTo("shop", "web-ing", "web.example.com", "web", 80)
	services := []corev1.Service{svcSel("shop", "web", map[string]string{"app": "web"}, 80)}
	got := firstDetail(t, Assess([]networkingv1.Ingress{ing}, services, nil, nil, nil, nil))
	if got != "backend Service web:80 has no ready endpoints (likely 502/503) — the selector matches no pods" {
		t.Fatalf("detail = %q", got)
	}
}

func TestAssess_BrokenRoute_NodeDownCause(t *testing.T) {
	ing := ingressTo("shop", "api-ing", "api.example.com", "api", 80)
	services := []corev1.Service{svcSel("shop", "api", map[string]string{"app": "api"}, 80)}
	pods := []corev1.Pod{podLabeled("shop", "api-1", "worker-2", map[string]string{"app": "api"}, false)}
	down := []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}}
	got := firstDetail(t, Assess([]networkingv1.Ingress{ing}, services, nil, nil, pods, down))
	if got != "backend Service api:80 has no ready endpoints (likely 502/503) — matching pods on down node worker-2 (NotReady)" {
		t.Fatalf("detail = %q", got)
	}
}

func TestAssess_BrokenRoute_PodsNotReadyCause(t *testing.T) {
	ing := ingressTo("shop", "api-ing", "api.example.com", "api", 80)
	services := []corev1.Service{svcSel("shop", "api", map[string]string{"app": "api"}, 80)}
	pods := []corev1.Pod{
		podLabeled("shop", "api-1", "worker-1", map[string]string{"app": "api"}, false),
		podLabeled("shop", "api-2", "worker-1", map[string]string{"app": "api"}, false),
		podLabeled("shop", "api-3", "worker-1", map[string]string{"app": "api"}, false),
	}
	got := firstDetail(t, Assess([]networkingv1.Ingress{ing}, services, nil, nil, pods, nil))
	if got != "backend Service api:80 has no ready endpoints (likely 502/503) — 3 matching pods, 0 ready" {
		t.Fatalf("detail = %q", got)
	}
}

func TestAssess_BrokenRoute_InconclusiveLeavesBase(t *testing.T) {
	ing := ingressTo("shop", "api-ing", "api.example.com", "api", 80)
	services := []corev1.Service{svcSel("shop", "api", map[string]string{"app": "api"}, 80)}
	pods := []corev1.Pod{podLabeled("shop", "api-1", "worker-1", map[string]string{"app": "api"}, true)} // ready
	got := firstDetail(t, Assess([]networkingv1.Ingress{ing}, services, nil, nil, pods, nil))
	if got != "backend Service api:80 has no ready endpoints (likely 502/503)" {
		t.Fatalf("detail = %q", got)
	}
}

func TestAssess_NoServiceRoute_NotEnriched(t *testing.T) {
	ing := ingressTo("shop", "ghost-ing", "ghost.example.com", "ghost", 80)
	got := firstDetail(t, Assess([]networkingv1.Ingress{ing}, nil, nil, nil, nil, nil))
	if got != "backend Service ghost not found" {
		t.Fatalf("NoService route must not be enriched, got %q", got)
	}
}
```

> Note on `ReadyEndpoints`: with `slices == nil`, `svchealth.ReadyEndpoints(svc, nil) == 0`, so a selector-bearing backend Service with no slices is a broken `NoEndpoints` route. With empty `backends`, `svchealth.ExpectedEmpty` returns `ok == false`, so it is NOT parked. If any pre-existing helper in the file already builds ingresses/services with a selector, prefer it over the locals above — but the assertions must match verbatim.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ingresshealth/ -run TestAssess_Broken`
Expected: FAIL — `too few arguments in call to Assess` (the new params don't exist yet).

- [ ] **Step 3: Write the implementation**

In `internal/ingresshealth/ingresshealth.go`:

1. Add `"github.com/imantaba/kubeagent/internal/clusterhealth"` to the imports (keep the block gofmt-sorted).
2. Change the `Assess` signature to add the two trailing params and thread them to `check`:

```go
func Assess(ingresses []networkingv1.Ingress, services []corev1.Service, slices []discoveryv1.EndpointSlice, backends []svchealth.Backend, pods []corev1.Pod, downNodes []clusterhealth.DownNode) []RouteIssue {
```

At both `check(...)` call sites inside `Assess` (the `DefaultBackend` one and the rule-path one), add `pods, downNodes` as the final arguments.

3. Change the `check` signature and its `NoEndpoints` broken branch:

```go
func check(ns, ingName, host, path string, be networkingv1.IngressServiceBackend, byKey map[string]corev1.Service, slices []discoveryv1.EndpointSlice, backends []svchealth.Backend, pods []corev1.Pod, downNodes []clusterhealth.DownNode) (RouteIssue, bool) {
	port := portString(be.Port)
	r := RouteIssue{Namespace: ns, Ingress: ingName, Host: host, Path: path, Service: be.Name, Port: port}
	svc, ok := byKey[ns+"/"+be.Name]
	if !ok {
		r.Problem = "NoService"
		r.Detail = fmt.Sprintf("backend Service %s not found", be.Name)
		return r, true
	}
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
	if !portMatches(be.Port, svc) {
		r.Problem = "PortNotExposed"
		r.Detail = fmt.Sprintf("backend Service %s does not expose port %s", be.Name, port)
		return r, true
	}
	return RouteIssue{}, false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ingresshealth/`
Expected: PASS. Then `gofmt -l internal/ingresshealth/ingresshealth.go internal/ingresshealth/ingresshealth_test.go` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/ingresshealth/
git commit -m "feat(ingresshealth): explain why a broken route's backend has no endpoints"
```

---

### Task 3: `scan.Evaluate` — pass pods + down-nodes to `ingresshealth.Assess`

**Files:**
- Modify: `internal/scan/scan.go` (update the one `ingresshealth.Assess` call)
- Test: `internal/scan/scan_test.go` (add one integration test)

**Interfaces:**
- Consumes: the new `ingresshealth.Assess` signature (Task 2); the existing locals `ings`, `svcs`, `slices`, `backends`, `inputs.Pods`, `health.DownNodes` (all already in scope — `health` and the Service check are on the lines just above).

- [ ] **Step 1: Write the failing test**

Add to `internal/scan/scan_test.go` (it imports `corev1`, `metav1`, `networkingv1` may need adding — add `networkingv1 "k8s.io/api/networking/v1"` if absent; and the fake clientset):

```go
func TestEvaluate_IngressRouteRootCause(t *testing.T) {
	// A broken ingress route whose backend Service selector matches no pods →
	// the route Detail is enriched with the no-pods cause.
	svcObj := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "web"}, Ports: []corev1.ServicePort{{Port: 80}}},
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-ing"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "web.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: "/",
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80},
					}},
				}},
			}},
		}}},
	}
	cli := fake.NewSimpleClientset(svcObj, ing)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, r := range res.IngressIssues {
		if r.Namespace == "shop" && r.Ingress == "web-ing" {
			found = true
			if r.Detail != "backend Service web:80 has no ready endpoints (likely 502/503) — the selector matches no pods" {
				t.Fatalf("detail = %q", r.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("expected a shop/web-ing route issue, got %+v", res.IngressIssues)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scan/ -run TestEvaluate_IngressRouteRootCause`
Expected: FAIL — first a compile error (`ingresshealth.Assess` now needs 6 args), then, once the call is updated, the assertion proves the enrichment.

- [ ] **Step 3: Write the implementation**

In `internal/scan/scan.go`, change the line
`ingressIssues := ingresshealth.Assess(ings, svcs, slices, backends)` to:

```go
	ingressIssues := ingresshealth.Assess(ings, svcs, slices, backends, inputs.Pods, health.DownNodes)
```

(`inputs.Pods` and `health.DownNodes` are already in scope — the Service check two lines above uses the same two.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scan/`
Expected: PASS. Then `gofmt -l internal/scan/scan.go internal/scan/scan_test.go` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/scan/
git commit -m "feat(scan): pass pods and down-nodes to the ingress route check"
```

---

### Task 4: Golden snapshot + docs

**Files:**
- Modify: `internal/report/golden_test.go` (enrich the broken `storefront` route Detail)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerate)
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** Consumes the rendering behavior; the golden test renders a pre-built `Input`, so this is a fixture-text change.

- [ ] **Step 1: Enrich the fixture's broken-route Detail**

In `internal/report/golden_test.go`, in the `goldenInput` builder's `IngressIssues`, change the `storefront` route Detail from
`"backend Service payments:80 has no ready endpoints (likely 502/503)"` to the enriched form:

```go
				Detail: "backend Service payments:80 has no ready endpoints (likely 502/503) — 3 matching pods, 0 ready"},
```

(the `—` is em dash U+2014). Leave the other (parked/`dashboard`) ingress route unchanged.

- [ ] **Step 2: Confirm the golden test now fails (snapshot drift)**

Run: `go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the rendered `storefront` route line now carries the enriched Detail, which the snapshot lacks.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report -run TestGoldenScanOutput -update`
Then inspect: `git diff internal/report/testdata/golden-scan.txt` — the only change must be the `storefront` route line gaining ` — 3 matching pods, 0 ready`. No other line changes (the "N ingress routes broken" count is unchanged).

- [ ] **Step 4: Run the full report suite**

Run: `go test ./internal/report/`
Expected: PASS (if any report test referenced the old exact storefront Detail string, update it to the enriched string).

- [ ] **Step 5: Update docs**

- `website/docs/features/diagnostics.md`: in the ingress-route-health section, note that a broken route's Detail now names the backend Service's endpoint cause (selector matches no pods / matching pods on a down node / N matching pods, 0 ready) — the same root cause as the Service check, one hop up.
- `README.md`: extend the ingress-route-health bullet to mention the backend root cause.
- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`, add a bullet:
  ```
  - **Ingress-route root cause.** A broken ingress route (`… has no ready
    endpoints (likely 502/503)`) now names *why* its backend Service is empty —
    the selector matches no pods, the matching pods are on a down node, or none
    are Ready — so the 502 is explained on the route itself. Read-only; reuses the
    Service endpoint-cause logic (no new flag or metric).
  ```
- `website/docs/roadmap.md`: add this to the Shipped list (extends the Theme-A root-cause chain to Ingress → Service → Pod → Node).

- [ ] **Step 6: Verify docs build (only website/ changed)**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (use the venv mkdocs if not on PATH: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, "Documentation built", no WARNING about your pages.

- [ ] **Step 7: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt website/ README.md CHANGELOG.md
git commit -m "test+docs: golden coverage and documentation for Ingress-route root cause"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the release skill owns this. Touches `internal/svchealth` (export a wrapper) + `internal/ingresshealth` + one line of `internal/scan` — no collect/cluster/watch/RBAC/Helm change → **LIGHTWEIGHT SMOKE** gate (a Kind cluster with an Ingress whose backend Service has no endpoints; confirm the enriched route Detail renders). **Minor** version bump **v0.38.0 → v0.39.0**; **patch** chart bump (no Helm template change — the bump script's default patch is correct; do NOT override to minor). Hold for the user's explicit "run release and push".
