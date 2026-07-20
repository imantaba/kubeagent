# Quiet Intentionally-Empty Endpoints Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop kubeagent from alarming on Services/Ingress routes that are empty on purpose — a backend scaled to zero (or between runs), or a Service an operator declares empty via the `kubeagent.io/expected-empty` annotation.

**Architecture:** `svchealth` already marks a NoEndpoints Service `Expected` when a scaled-to-0 backend explains it (rendered as a quiet NOTE). This plan (a) adds a `kubeagent.io/expected-empty` annotation as an explicit operator opt-out, and (b) teaches the `ingresshealth` route check the same "backend intentionally empty" awareness by reusing a new shared `svchealth.ExpectedEmpty` decision — so a route to such a backend becomes a NOTE, not a NEEDS-ATTENTION 502/503.

**Tech Stack:** Go 1.26, standard library + `k8s.io/api` (`corev1`, `networkingv1`, `discoveryv1`). No new dependencies, no new API calls.

## Global Constraints

- **READ-ONLY; NO new RBAC / no new collector.** Services/Ingresses/EndpointSlices are already listed; the annotation rides on Service objects already fetched.
- **Always-on** — no CLI flag, no `watch.Config` change; runs in `scan` and the `watch` daemon via `scan.Evaluate`.
- **Pure & deterministic** — `svchealth.ExpectedEmpty`, `svchealth.Assess`, `ingresshealth.Assess` stay pure functions of their inputs.
- `internal/watch` (incl. the metric gauges), `internal/collect`, `explain.go`, RBAC, and Helm stay **unchanged**. The existing **service-path** backing wording (`classifyBacking`/`backingDetail`) stays **unchanged** — only a new annotation `else if` is added.
- **No `Co-Authored-By: Claude` trailer** (or any AI attribution) on any commit. **TDD.**
- Set PATH first every task: `export PATH=$PATH:/usr/local/go/bin`.
- Spec: [docs/superpowers/specs/2026-07-20-quiet-empty-endpoints-design.md](../specs/2026-07-20-quiet-empty-endpoints-design.md).

---

## File Structure

- **Modify** `internal/svchealth/svchealth.go` — `ExpectedEmptyAnnotation` const, `annotatedExpectedEmpty`, exported `ExpectedEmpty`, `backingReason`, and one `else if` in `Assess`.
- **Modify** `internal/svchealth/svchealth_test.go` — annotation + `ExpectedEmpty` tests.
- **Modify** `internal/ingresshealth/ingresshealth.go` — `RouteIssue.Expected`, `backends` param on `Assess`/`check`, expected branch.
- **Modify** `internal/ingresshealth/ingresshealth_test.go` — expected-route tests.
- **Modify** `internal/scan/scan.go` — pass `backends` to `ingresshealth.Assess` (call-site, same commit as the signature change).
- **Modify** `internal/report/report.go` — `splitIngressIssues`, glyph param on `printIngressIssues`, thread `realIng`/`expectedIng` through `printInventoryText`/`printHeader`/`attentionLine`/`printNotes`.
- **Modify** `internal/report/report_test.go` — split/rendering test.
- **Modify** `internal/report/golden_test.go` + `internal/report/testdata/golden-scan.txt` — fixture + snapshot.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`.

---

### Task 1: `svchealth` — annotation + shared `ExpectedEmpty`

**Files:**
- Modify: `internal/svchealth/svchealth.go`
- Test: `internal/svchealth/svchealth_test.go`

**Interfaces:**
- Consumes: existing `classifyBacking(svc corev1.Service, backends []Backend) (backing, detail string, ok bool)`, `Backend`, `Issue`.
- Produces: `const ExpectedEmptyAnnotation = "kubeagent.io/expected-empty"`; `func ExpectedEmpty(svc corev1.Service, backends []Backend) (reason string, ok bool)` (annotation-first, then scaled-to-0 backend). Later tasks (ingresshealth) call `ExpectedEmpty`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/svchealth/svchealth_test.go`. (The existing `svc(...)` helper does not set annotations, so build the annotated Service inline.)

```go
func TestExpectedEmpty_Annotation(t *testing.T) {
	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg-ro",
			Annotations: map[string]string{ExpectedEmptyAnnotation: "true"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"role": "replica"}},
	}
	// No backend at all, and even with a LIVE backend the annotation must win (absolute override).
	live := []Backend{{Kind: "Deployment", Namespace: "db", TemplateLabels: map[string]string{"role": "replica"}, Desired: 3}}
	for _, backends := range [][]Backend{nil, live} {
		reason, ok := ExpectedEmpty(s, backends)
		if !ok {
			t.Fatalf("annotated Service must be expected-empty (backends=%v)", backends)
		}
		if !strings.Contains(reason, ExpectedEmptyAnnotation) {
			t.Errorf("reason %q should name the annotation", reason)
		}
	}
}

func TestExpectedEmpty_AnnotationSetsIssueExpected(t *testing.T) {
	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg-ro",
			Annotations: map[string]string{ExpectedEmptyAnnotation: "true"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"role": "replica"}},
	}
	got := Assess([]corev1.Service{s}, nil, nil) // no slices -> 0 endpoints
	if len(got) != 1 || !got[0].Expected {
		t.Fatalf("annotated empty Service must yield one Expected issue, got %+v", got)
	}
}

func TestExpectedEmpty_NotAnnotatedNoBacking(t *testing.T) {
	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg-ro"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"role": "replica"}},
	}
	if _, ok := ExpectedEmpty(s, nil); ok {
		t.Error("a Service with no annotation and no backing must not be expected-empty")
	}
}

func TestExpectedEmpty_ScaledToZeroBacking(t *testing.T) {
	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"app": "web"}},
	}
	backends := []Backend{{Kind: "Deployment", Namespace: "shop", TemplateLabels: map[string]string{"app": "web"}, Desired: 0}}
	reason, ok := ExpectedEmpty(s, backends)
	if !ok || !strings.Contains(reason, "scaled to 0") {
		t.Fatalf("scaled-to-0 Deployment backing should be expected with 'scaled to 0', got %q ok=%v", reason, ok)
	}
}
```

If `strings` / `metav1` are not already imported in the test file, add them.

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/svchealth -run TestExpectedEmpty`
Expected: FAIL — build error (`undefined: ExpectedEmpty`, `undefined: ExpectedEmptyAnnotation`).

- [ ] **Step 3: Implement**

In `internal/svchealth/svchealth.go`, add the constant and helpers (place near `classifyBacking`), and confirm `strings` is imported (it is — `selectorMatches` doesn't use it, so add `"strings"` to the import block if absent):

```go
// ExpectedEmptyAnnotation, when set to "true" on a Service, declares that the Service is
// meant to have no ready endpoints (e.g. an operator-managed role-split Service such as a
// CloudNativePG "-ro" service on a single-instance cluster). kubeagent then treats its
// empty endpoints as expected rather than a problem.
const ExpectedEmptyAnnotation = "kubeagent.io/expected-empty"

func annotatedExpectedEmpty(svc corev1.Service) bool {
	return strings.EqualFold(svc.Annotations[ExpectedEmptyAnnotation], "true")
}

// ExpectedEmpty reports whether a Service's lack of ready endpoints is intentional, with a
// short human reason. It is the shared decision reused by ingresshealth. Order: the
// annotation (an absolute operator declaration, honored even over a live backend), then a
// scaled-to-0 / between-runs backend.
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

In `Assess`, extend the NoEndpoints branch with the annotation `else if` (leave the `classifyBacking` path exactly as-is):

```go
		if ReadyEndpoints(s, slices) == 0 {
			is := Issue{
				Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
				Problem: "NoEndpoints", Detail: "no ready endpoints",
			}
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

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/svchealth`
Expected: PASS — the new `TestExpectedEmpty_*` and all existing svchealth tests.

- [ ] **Step 5: Commit**

```bash
git add internal/svchealth/svchealth.go internal/svchealth/svchealth_test.go
git commit -m "feat(svchealth): kubeagent.io/expected-empty annotation + shared ExpectedEmpty"
```

---

### Task 2: `ingresshealth` — reuse `ExpectedEmpty`, wire `backends` through `scan`

**Files:**
- Modify: `internal/ingresshealth/ingresshealth.go`
- Modify: `internal/scan/scan.go` (call-site — same commit, or the build breaks)
- Test: `internal/ingresshealth/ingresshealth_test.go`

**Interfaces:**
- Consumes: `svchealth.ExpectedEmpty` (Task 1), `svchealth.Backend`, `svchealth.ReadyEndpoints`.
- Produces: `RouteIssue.Expected bool`; `func Assess(ingresses []networkingv1.Ingress, services []corev1.Service, slices []discoveryv1.EndpointSlice, backends []svchealth.Backend) []RouteIssue`.

- [ ] **Step 1: Write the failing tests**

Existing `ingresshealth_test.go` calls `Assess(..., svcs, slices)` (3 args) — those call sites must gain a 4th arg (`nil`) in this step so the file compiles. Then add expected-route tests. (The `svc`/`ing` helpers live in the test file; build annotated/back-ended inputs inline.)

```go
func TestAssess_ExpectedEmpty_ScaledToZeroBackend(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "web", 80)} // helper builds a Service with port 80
	svcs[0].Spec.Selector = map[string]string{"app": "web"}
	backends := []svchealth.Backend{{Kind: "Deployment", Namespace: "shop", TemplateLabels: map[string]string{"app": "web"}, Desired: 0}}
	got := Assess([]networkingv1.Ingress{ing("shop", "site", "x.io", "/", "web", 80)}, svcs, nil, backends) // no slices -> 0 ready
	if len(got) != 1 || !got[0].Expected {
		t.Fatalf("route to a scaled-to-0 backend must be Expected, got %+v", got)
	}
	if !strings.Contains(got[0].Detail, "route parked") {
		t.Errorf("expected 'route parked' detail, got %q", got[0].Detail)
	}
}

func TestAssess_ExpectedEmpty_Annotated(t *testing.T) {
	svcs := []corev1.Service{svc("db", "pg-ro", 5432)}
	svcs[0].Spec.Selector = map[string]string{"role": "replica"}
	svcs[0].Annotations = map[string]string{svchealth.ExpectedEmptyAnnotation: "true"}
	got := Assess([]networkingv1.Ingress{ing("db", "pg", "pg.io", "/", "pg-ro", 5432)}, svcs, nil, nil)
	if len(got) != 1 || !got[0].Expected {
		t.Fatalf("route to an annotated Service must be Expected, got %+v", got)
	}
}

func TestAssess_GenuinelyBroken_NotExpected(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	svcs[0].Spec.Selector = map[string]string{"app": "api"}
	backends := []svchealth.Backend{{Kind: "Deployment", Namespace: "shop", TemplateLabels: map[string]string{"app": "api"}, Desired: 3}} // live, 0 ready
	got := Assess([]networkingv1.Ingress{ing("shop", "site", "x.io", "/", "api", 80)}, svcs, nil, backends)
	if len(got) != 1 || got[0].Expected {
		t.Fatalf("route to a live backend with 0 endpoints is a real issue, got %+v", got)
	}
	if !strings.Contains(got[0].Detail, "502/503") {
		t.Errorf("expected '502/503' detail, got %q", got[0].Detail)
	}
}
```

Add `strings` and the `svchealth` import to the test file if absent. Update the existing 3-arg `Assess(...)` calls in this file to pass a 4th `nil` argument.

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/ingresshealth`
Expected: FAIL — build error (`Assess` still 3-arg / `RouteIssue` has no `Expected`).

- [ ] **Step 3: Implement**

In `internal/ingresshealth/ingresshealth.go`:

Add the field to `RouteIssue` (after `Detail`):
```go
	Expected  bool   `json:"expected,omitempty"` // true when the empty backend is intentional (scaled to 0 / annotated)
```

Change `Assess` to take `backends` and pass it to `check`:
```go
func Assess(ingresses []networkingv1.Ingress, services []corev1.Service, slices []discoveryv1.EndpointSlice, backends []svchealth.Backend) []RouteIssue {
```
Update both `check(...)` call sites inside `Assess` to pass `backends` as the final argument.

Change `check`'s signature and its NoEndpoints branch:
```go
func check(ns, ingName, host, path string, be networkingv1.IngressServiceBackend, byKey map[string]corev1.Service, slices []discoveryv1.EndpointSlice, backends []svchealth.Backend) (RouteIssue, bool) {
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
		} else if port != "" {
			r.Detail = fmt.Sprintf("backend Service %s:%s has no ready endpoints (likely 502/503)", be.Name, port)
		} else {
			r.Detail = fmt.Sprintf("backend Service %s has no ready endpoints (likely 502/503)", be.Name)
		}
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
`NoService` and `PortNotExposed` are unchanged.

In `internal/scan/scan.go`, update the call site (currently `ingresshealth.Assess(ings, svcs, slices)`) to pass the `backends` already computed a few lines above:
```go
	ingressIssues := ingresshealth.Assess(ings, svcs, slices, backends)
```

- [ ] **Step 4: Run tests + full build to verify**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/ingresshealth ./internal/scan`
Expected: PASS (build clean, new + existing tests green).

- [ ] **Step 5: Commit**

```bash
git add internal/ingresshealth/ingresshealth.go internal/ingresshealth/ingresshealth_test.go internal/scan/scan.go
git commit -m "feat(ingresshealth): park routes to intentionally-empty backends"
```

---

### Task 3: `report` — split ingress issues (real vs expected)

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `ingresshealth.RouteIssue.Expected` (Task 2).
- Produces: `splitIngressIssues(issues []ingresshealth.RouteIssue) (real, expected []ingresshealth.RouteIssue)`; `printIngressIssues` gains a `glyph string` param.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go`:

```go
func TestPrintInventory_ExpectedIngressGoesToNotes(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		IngressIssues: []ingresshealth.RouteIssue{
			{Namespace: "shop", Ingress: "real", Host: "a.io", Path: "/", Service: "api", Port: "80",
				Problem: "NoEndpoints", Detail: "backend Service api:80 has no ready endpoints (likely 502/503)"},
			{Namespace: "shop", Ingress: "parked", Host: "b.io", Path: "/", Service: "web",
				Problem: "NoEndpoints", Expected: true,
				Detail: "backend Service web is intentionally empty (scaled to 0) — route parked"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// The attention line counts only the real route.
	if !strings.Contains(out, "1 ingress route broken") {
		t.Errorf("attention line should count only the real route:\n%s", out)
	}
	// The real route is in NEEDS ATTENTION with the ✗ glyph.
	if !strings.Contains(out, "✗ ingress shop/real") {
		t.Errorf("real route should be under NEEDS ATTENTION:\n%s", out)
	}
	// The parked route is a quiet NOTE with the • glyph, not an attention ✗.
	if !strings.Contains(out, "• ingress shop/parked") {
		t.Errorf("parked route should be a NOTE:\n%s", out)
	}
	if strings.Contains(out, "✗ ingress shop/parked") {
		t.Errorf("parked route must not appear under NEEDS ATTENTION:\n%s", out)
	}
}
```

Ensure `clusterhealth`, `ingresshealth`, `strings`, `bytes` are imported in the test file (most already are).

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory_ExpectedIngressGoesToNotes`
Expected: FAIL — the parked route currently renders under NEEDS ATTENTION and inflates the count.

- [ ] **Step 3: Implement**

In `internal/report/report.go`:

Add the splitter next to `splitServiceIssues`:
```go
// splitIngressIssues separates real broken routes from expected-empty (parked) ones.
func splitIngressIssues(issues []ingresshealth.RouteIssue) (real, expected []ingresshealth.RouteIssue) {
	for _, r := range issues {
		if r.Expected {
			expected = append(expected, r)
		} else {
			real = append(real, r)
		}
	}
	return real, expected
}
```

Parameterize `printIngressIssues` with a glyph:
```go
// printIngressIssues lists Ingress routes whose backend chain is broken (glyph "  ✗")
// or is intentionally empty (glyph "  •").
func printIngressIssues(issues []ingresshealth.RouteIssue, glyph string, w io.Writer) error {
	for _, r := range issues {
		line := fmt.Sprintf("%s ingress %s/%s", glyph, r.Namespace, r.Ingress)
		if route := r.Host + r.Path; route != "" {
			line += "  " + route
		}
		line += "  " + r.Detail
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
```

In `printInventoryText`, compute the split and thread it through:
```go
	real, expected := splitServiceIssues(in.ServiceIssues)
	realIng, expectedIng := splitIngressIssues(in.IngressIssues)

	if err := printHeader(in, real, realIng, w); err != nil {
		return err
	}

	hasDisk := in.DiskUsage != nil && len(in.DiskUsage.Over) > 0
	hasAttention := len(in.Result.Workloads) > 0 || len(real) > 0 || len(in.CredentialWarnings) > 0 || hasDisk || len(realIng) > 0 || len(in.PVCIssues) > 0
```
Change the NEEDS ATTENTION ingress call to `printIngressIssues(realIng, "  ✗", w)`, and the notes call to `printNotes(in, expected, expectedIng, w)`.

Update `printHeader` and `attentionLine` signatures to take `realIng` and count it:
```go
func printHeader(in Input, real []svchealth.Issue, realIng []ingresshealth.RouteIssue, w io.Writer) error {
	// ... unchanged until the attention line:
	if line := attentionLine(in, real, realIng); line != "" {
```
```go
func attentionLine(in Input, real []svchealth.Issue, realIng []ingresshealth.RouteIssue) string {
	// ... unchanged until the ingress clause:
	if n := len(realIng); n > 0 {
		parts = append(parts, fmt.Sprintf("%d ingress %s broken", n, plural(n, "route", "routes")))
	}
```

Update `printNotes` to render expected ingress after the expected services:
```go
func printNotes(in Input, expected []svchealth.Issue, expectedIng []ingresshealth.RouteIssue, w io.Writer) error {
	// ... after: printServiceIssues(expected, "  •", now, &b)
	if err := printIngressIssues(expectedIng, "  •", &b); err != nil {
		return err
	}
	// ... footerHint etc. unchanged
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory`
Expected: PASS (the new test and existing `TestPrintInventory_*`). The golden test may now fail — that's Task 4.

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render parked ingress routes as quiet notes"
```

---

### Task 4: Golden fixture + snapshot

**Files:**
- Modify: `internal/report/golden_test.go`
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated)

**Interfaces:** none new — exercises the Task 3 rendering through the real text renderer.

- [ ] **Step 1: Add an expected (parked) ingress route to the fixture**

In `internal/report/golden_test.go`, change the `IngressIssues` field of `goldenInput` from the single real route to two — the existing real one plus a new `Expected` one:

```go
		IngressIssues: []ingresshealth.RouteIssue{
			{Namespace: "shop", Ingress: "storefront", Host: "shop.example.com", Path: "/",
				Service: "payments", Port: "80", Problem: "NoEndpoints",
				Detail: "backend Service payments:80 has no ready endpoints (likely 502/503)"},
			{Namespace: "shop", Ingress: "dashboard", Host: "dash.example.com", Path: "/",
				Service: "grafana", Problem: "NoEndpoints", Expected: true,
				Detail: "backend Service grafana is intentionally empty (scaled to 0) — route parked"},
		},
```
(Em dash U+2014 in the parked detail, matching what `ingresshealth` produces.)

- [ ] **Step 2: Run the golden test to see it fail (snapshot stale)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput`
Expected: FAIL — output changed (new NOTES ingress line; attention-line ingress count unchanged at 1).

- [ ] **Step 3: Regenerate the snapshot**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput -update`
Expected: PASS (writes `testdata/golden-scan.txt`).

- [ ] **Step 4: Inspect the regenerated snapshot**

Run: `grep -n "ingress shop/\|route broken" internal/report/testdata/golden-scan.txt`
Expected: (a) the attention line still says `1 ingress route broken` (only the real route counts); (b) `✗ ingress shop/storefront ...` under NEEDS ATTENTION; (c) `• ingress shop/dashboard  dash.example.com/  backend Service grafana is intentionally empty (scaled to 0) — route parked` under NOTES. Confirm no other lines shifted unexpectedly.

- [ ] **Step 5: Run the full report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report`
Expected: PASS (`TestGoldenScanOutput` + `TestGoldenInputCoversAllSections`; `len(in.IngressIssues)` is now 2, still ≥ 1).

- [ ] **Step 6: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover a parked ingress route in the golden snapshot"
```

---

### Task 5: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`

**Interfaces:** none (docs only). `go build`/`go test` stay green.

- [ ] **Step 1: Document the behavior in diagnostics.md**

In `website/docs/features/diagnostics.md`, find the `### Ingress route health` subsection and append, at the end of that subsection, a paragraph followed by a fenced `yaml` block. The paragraph text (verbatim):

> A route whose backend Service is **intentionally empty** — the backing workload is scaled to zero (or a Job/CronJob between runs), or the Service is explicitly annotated `kubeagent.io/expected-empty: "true"` — is treated as **parked**: it moves to the quiet NOTES section instead of NEEDS ATTENTION, so a deliberately-idle app or an operator-managed role-split Service (e.g. a CloudNativePG `-ro` service on a single-instance cluster) does not read as a 502/503 outage. Set the annotation on the **Service** to silence a route (or the bare Service finding) kubeagent cannot infer is empty by design:

Then a fenced code block with info-string `yaml` containing exactly these three lines (a `metadata:` key, an `annotations:` key indented two spaces, and the annotation indented four spaces):

- line 1: `metadata:`
- line 2: `  annotations:`
- line 3: `    kubeagent.io/expected-empty: "true"`

- [ ] **Step 2: Note the annotation in the Service/Ingress area of README.md**

In `README.md`, near where Service/Ingress checks are described, add one line:

```markdown
- A Service (or Ingress route) that is empty on purpose — its backend is scaled to zero, or
  it carries `kubeagent.io/expected-empty: "true"` — is shown as a quiet note, not an alert.
```
(Place it beside the existing Service/Ingress bullet; match the surrounding list style.)

- [ ] **Step 3: Add the CHANGELOG entry**

In `CHANGELOG.md`, under `## [Unreleased]` → `### Added` (create the `### Added` sub-header under a fresh `## [Unreleased]` if the last release consumed it), add:

```markdown
- **Quiet intentionally-empty endpoints.** An Ingress route whose backend Service is empty
  on purpose — the backing workload is scaled to zero (or between runs), or the Service is
  annotated `kubeagent.io/expected-empty: "true"` — is now shown as a parked route in NOTES
  instead of a 502/503 in NEEDS ATTENTION. The annotation also quiets the bare Service
  finding, covering operator-managed role-split Services (e.g. CloudNativePG `-ro` on a
  single-instance cluster) that kubeagent can't infer. Read-only, always-on, no new RBAC.
```

- [ ] **Step 4: Verify docs build (if mkdocs available) + full suite**

Run: `cd website && mkdocs build --strict -f mkdocs.yml 2>&1 | tail -3; cd ..` (skip with a note if mkdocs is not installed — it's a convenience check, not a gate).
Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all Go packages PASS.

- [ ] **Step 5: Commit**

```bash
git add website/docs/features/diagnostics.md README.md CHANGELOG.md
git commit -m "docs: document parked (intentionally-empty) endpoints + annotation"
```

---

## Notes for the executor

- **Release gate (post-merge, not part of these tasks):** this touches `internal/svchealth`, `internal/ingresshealth`, `internal/scan`, `internal/report` — **not** `internal/collect`, `internal/cluster`, RBAC, Helm, or the watch daemon — so a **lightweight real-cluster smoke** confirms rendering; the full chaos gate is not required. Version bump is a **patch** (v0.28.0 → v0.28.1).
- **No new RBAC / collector:** the annotation rides on Service objects already listed; do not touch `deploy/` or Helm.
- **Absolute override:** the `kubeagent.io/expected-empty` annotation marks a Service expected even when a live backend exists — this is intentional (the operator's explicit declaration).
- **Non-goal:** the `kubeagent_service_issues` / `kubeagent_ingress_route_issues` watch gauges keep counting raw totals (unchanged); `internal/watch` is not touched.
