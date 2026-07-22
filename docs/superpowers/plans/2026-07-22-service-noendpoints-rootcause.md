# Service-no-endpoints root cause — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** For a broken `NoEndpoints` Service, enrich its `Detail` with the root cause — selector matches no pods, matching pods on a down node, or matching pods not ready — by correlating the selector against the collected pods and down-node list.

**Architecture:** A new pure function `svchealth.AnnotateEndpointCause` post-processes the issues from `svchealth.Assess`, rewriting the `Detail` of each broken (`!Expected`) `NoEndpoints` issue in place. It reuses the existing unexported `selectorMatches` and adds only a `clusterhealth.DownNode` data dependency (no cycle). `scan.Evaluate` calls it with one line after `Assess`. No report/JSON/watch/RBAC change — the enriched text flows through the existing rendering.

**Tech Stack:** Go 1.26, `k8s.io/api/core/v1`, the internal `clusterhealth` package (for `DownNode`). Tests use fake objects (no clientset for the pure function; fake clientset for the scan integration test).

## Global Constraints

- **READ-ONLY.** Pure correlation over already-collected objects; no cluster calls, no writes, no LLM.
- **Always-on; no flag.** No new RBAC, collector, watch gauge, `Result` field, `report` change, or `resultInput` seam change.
- **Advisory** — does not change the verdict or which Services are flagged; only enriches `Detail` text.
- **Pure & deterministic & idempotent** — `AnnotateEndpointCause` reads only the passed objects; re-running yields the same Detail.
- **v1 uses the standard-library `flag` package only** — no Cobra.
- **No `Co-Authored-By: Claude` trailer** (or any Claude attribution) on any commit. Every commit authored solely by the human.
- **TDD** — write the failing test first, watch it fail, then implement.
- **gofmt-clean** — run `gofmt -w` on changed files before committing.
- Constant: `—` em dash U+2014 in the Detail suffix.
- Precedence of causes: **no-pods → node-down → pods-not-ready**, first match wins.

---

### Task 1: `svchealth.AnnotateEndpointCause` — the correlation

**Files:**
- Modify: `internal/svchealth/svchealth.go` (add `AnnotateEndpointCause`, `endpointCause`, `podReady`; add `fmt` + `clusterhealth` imports if absent)
- Test: `internal/svchealth/svchealth_test.go` (add tests + pod/downNode helpers)

**Interfaces:**
- Consumes: the existing unexported `selectorMatches(selector, labels map[string]string) bool`; `clusterhealth.DownNode{Name, Reason string}`.
- Produces: `func AnnotateEndpointCause(issues []Issue, services []corev1.Service, pods []corev1.Pod, downNodes []clusterhealth.DownNode)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/svchealth/svchealth_test.go` (it already imports `corev1`, `discoveryv1`, `metav1`, and has a `svc(ns, name, type, selector, lbIngress)` helper; add `"github.com/imantaba/kubeagent/internal/clusterhealth"` to its imports):

```go
func pod(ns, name, node string, labels map[string]string, ready bool) corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
	p.Spec.NodeName = node
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	return p
}

func brokenNoEndpoints(ns, name string) Issue {
	return Issue{Namespace: ns, Name: name, Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"}
}

func TestAnnotateEndpointCause_NoPods(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "web")}
	services := []corev1.Service{svc("shop", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	AnnotateEndpointCause(issues, services, nil, nil)
	if issues[0].Detail != "no ready endpoints — the selector matches no pods" {
		t.Fatalf("detail = %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_NodeDownSingle(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "cache")}
	services := []corev1.Service{svc("shop", "cache", corev1.ServiceTypeClusterIP, map[string]string{"app": "cache"}, 0)}
	pods := []corev1.Pod{
		pod("shop", "cache-1", "worker-2", map[string]string{"app": "cache"}, false),
		pod("shop", "cache-2", "worker-2", map[string]string{"app": "cache"}, false),
	}
	down := []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}}
	AnnotateEndpointCause(issues, services, pods, down)
	if issues[0].Detail != "no ready endpoints — matching pods on down node worker-2 (NotReady)" {
		t.Fatalf("detail = %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_NodeDownMultiple(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "cache")}
	services := []corev1.Service{svc("shop", "cache", corev1.ServiceTypeClusterIP, map[string]string{"app": "cache"}, 0)}
	pods := []corev1.Pod{
		pod("shop", "cache-1", "worker-2", map[string]string{"app": "cache"}, false),
		pod("shop", "cache-2", "worker-3", map[string]string{"app": "cache"}, false),
	}
	down := []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}, {Name: "worker-3", Reason: "NotReady"}}
	AnnotateEndpointCause(issues, services, pods, down)
	if issues[0].Detail != "no ready endpoints — matching pods on 2 down nodes" {
		t.Fatalf("detail = %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_PodsNotReady(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "api")}
	services := []corev1.Service{svc("shop", "api", corev1.ServiceTypeClusterIP, map[string]string{"app": "api"}, 0)}
	pods := []corev1.Pod{
		pod("shop", "api-1", "worker-1", map[string]string{"app": "api"}, false),
		pod("shop", "api-2", "worker-1", map[string]string{"app": "api"}, false),
		pod("shop", "api-3", "worker-1", map[string]string{"app": "api"}, false),
	}
	AnnotateEndpointCause(issues, services, pods, nil) // worker-1 not down
	if issues[0].Detail != "no ready endpoints — 3 matching pods, 0 ready" {
		t.Fatalf("detail = %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_SinglePodNounSingular(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "api")}
	services := []corev1.Service{svc("shop", "api", corev1.ServiceTypeClusterIP, map[string]string{"app": "api"}, 0)}
	pods := []corev1.Pod{pod("shop", "api-1", "worker-1", map[string]string{"app": "api"}, false)}
	AnnotateEndpointCause(issues, services, pods, nil)
	if issues[0].Detail != "no ready endpoints — 1 matching pod, 0 ready" {
		t.Fatalf("detail = %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_LeavesExpectedAndReadyAndOtherProblems(t *testing.T) {
	services := []corev1.Service{
		svc("shop", "sched-zero", corev1.ServiceTypeClusterIP, map[string]string{"app": "sz"}, 0),
		svc("shop", "healthy", corev1.ServiceTypeClusterIP, map[string]string{"app": "h"}, 0),
	}
	pods := []corev1.Pod{pod("shop", "h-1", "worker-1", map[string]string{"app": "h"}, true)} // ready
	issues := []Issue{
		{Namespace: "shop", Name: "sched-zero", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints — declared via …", Expected: true},
		{Namespace: "shop", Name: "healthy", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"}, // has a ready matching pod → inconclusive, leave
		{Namespace: "shop", Name: "lb", Type: "LoadBalancer", Problem: "NoExternalAddress", Detail: "no external address"},
	}
	AnnotateEndpointCause(issues, services, pods, nil)
	if issues[0].Detail != "no ready endpoints — declared via …" {
		t.Errorf("expected-empty must be untouched, got %q", issues[0].Detail)
	}
	if issues[1].Detail != "no ready endpoints" {
		t.Errorf("a ready matching pod is inconclusive; leave detail, got %q", issues[1].Detail)
	}
	if issues[2].Detail != "no external address" {
		t.Errorf("NoExternalAddress must be untouched, got %q", issues[2].Detail)
	}
}

func TestAnnotateEndpointCause_Idempotent(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "web")}
	services := []corev1.Service{svc("shop", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	AnnotateEndpointCause(issues, services, nil, nil)
	first := issues[0].Detail
	AnnotateEndpointCause(issues, services, nil, nil)
	if issues[0].Detail != first {
		t.Fatalf("not idempotent: %q -> %q", first, issues[0].Detail)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/svchealth/`
Expected: FAIL — `undefined: AnnotateEndpointCause`.

- [ ] **Step 3: Write the implementation**

In `internal/svchealth/svchealth.go`, add `"fmt"` and `"github.com/imantaba/kubeagent/internal/clusterhealth"` to the imports (keep the block gofmt-sorted), then add:

```go
// AnnotateEndpointCause enriches the Detail of every broken NoEndpoints issue with
// the reason its Service has no ready endpoints, correlating the Service selector
// against pods and the down-node list. Pure and read-only; mutates the issues in
// place. Expected-empty and non-NoEndpoints issues are left untouched.
func AnnotateEndpointCause(issues []Issue, services []corev1.Service, pods []corev1.Pod, downNodes []clusterhealth.DownNode) {
	down := make(map[string]string, len(downNodes))
	for _, d := range downNodes {
		down[d.Name] = d.Reason
	}
	svcByID := make(map[string]corev1.Service, len(services))
	for _, s := range services {
		svcByID[s.Namespace+"/"+s.Name] = s
	}
	for i := range issues {
		if issues[i].Problem != "NoEndpoints" || issues[i].Expected {
			continue
		}
		svc, ok := svcByID[issues[i].Namespace+"/"+issues[i].Name]
		if !ok {
			continue
		}
		if cause := endpointCause(svc, pods, down); cause != "" {
			issues[i].Detail = "no ready endpoints — " + cause
		}
	}
}

// endpointCause returns the reason a Service has no ready endpoints (no-pods →
// node-down → pods-not-ready, first match), or "" when inconclusive.
func endpointCause(svc corev1.Service, pods []corev1.Pod, down map[string]string) string {
	var matching []corev1.Pod
	for _, p := range pods {
		if p.Namespace == svc.Namespace && selectorMatches(svc.Spec.Selector, p.Labels) {
			matching = append(matching, p)
		}
	}
	if len(matching) == 0 {
		return "the selector matches no pods"
	}

	var downHit []string
	seen := map[string]bool{}
	for _, p := range matching {
		n := p.Spec.NodeName
		if n == "" || seen[n] {
			continue
		}
		if reason, isDown := down[n]; isDown {
			seen[n] = true
			downHit = append(downHit, n+" ("+reason+")")
		}
	}
	if len(downHit) == 1 {
		return "matching pods on down node " + downHit[0]
	}
	if len(downHit) > 1 {
		return fmt.Sprintf("matching pods on %d down nodes", len(downHit))
	}

	ready := 0
	for _, p := range matching {
		if podReady(p) {
			ready++
		}
	}
	if ready == 0 {
		noun := "pods"
		if len(matching) == 1 {
			noun = "pod"
		}
		return fmt.Sprintf("%d matching %s, 0 ready", len(matching), noun)
	}
	return ""
}

// podReady reports whether the pod's Ready condition is true (endpoint-membership
// semantics).
func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/svchealth/`
Expected: PASS (all tests). Then `gofmt -l internal/svchealth/svchealth.go internal/svchealth/svchealth_test.go` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/svchealth/
git commit -m "feat(svchealth): explain why a broken Service has no ready endpoints"
```

---

### Task 2: `scan.Evaluate` — wire the annotator

**Files:**
- Modify: `internal/scan/scan.go` (one line after `svchealth.Assess`)
- Test: `internal/scan/scan_test.go` (add one integration test)

**Interfaces:**
- Consumes: `svchealth.AnnotateEndpointCause` (Task 1); the existing locals `serviceIssues`, `svcs`, `inputs.Pods`, `health.DownNodes` (all already in scope — `health` is computed just above `serviceIssues`).

- [ ] **Step 1: Write the failing test**

Add to `internal/scan/scan_test.go` (it already imports `corev1`, `metav1`, and the fake clientset):

```go
func TestEvaluate_ServiceNoEndpointsRootCause(t *testing.T) {
	// A selector-based Service with no matching pods and no endpoints → the
	// service issue's Detail is enriched with the no-pods cause.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"app": "web"}},
	}
	cli := fake.NewSimpleClientset(svc)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, is := range res.ServiceIssues {
		if is.Namespace == "shop" && is.Name == "web" {
			found = true
			if is.Detail != "no ready endpoints — the selector matches no pods" {
				t.Fatalf("detail = %q", is.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("expected a shop/web service issue, got %+v", res.ServiceIssues)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scan/ -run TestEvaluate_ServiceNoEndpointsRootCause`
Expected: FAIL — the Detail is the un-enriched `no ready endpoints` (annotator not wired yet).

- [ ] **Step 3: Write the implementation**

In `internal/scan/scan.go`, immediately after the line `serviceIssues := svchealth.Assess(svcs, slices, backends)`, add:

```go
	svchealth.AnnotateEndpointCause(serviceIssues, svcs, inputs.Pods, health.DownNodes)
```

(`health` is already computed just above this line, so `health.DownNodes` is in scope.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scan/`
Expected: PASS. Then `gofmt -l internal/scan/scan.go internal/scan/scan_test.go` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/scan/
git commit -m "feat(scan): enrich broken NoEndpoints Services with their root cause"
```

---

### Task 3: Golden snapshot + docs

**Files:**
- Modify: `internal/report/report_test.go` (update `sampleServiceIssues()` broken-service Detail)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerate)
- Modify: `website/docs/features/service-health.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** Consumes the rendering behavior; the golden test renders a pre-built `Input`, so this is a fixture-text change (the annotator runs in `scan`, not in the golden render path).

- [ ] **Step 1: Update the fixture's broken-service Detail**

In `internal/report/report_test.go`, in `sampleServiceIssues()`, change the `default/web` issue's Detail from `"no ready endpoints"` to the enriched form so the snapshot demonstrates the feature:

```go
func sampleServiceIssues() []svchealth.Issue {
	return []svchealth.Issue{
		{Namespace: "default", Name: "web", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints — 2 matching pods, 0 ready"},
		{Namespace: "default", Name: "api-lb", Type: "LoadBalancer", Problem: "NoExternalAddress", Detail: "no external address", Since: "2026-06-29T00:00:00Z"},
	}
}
```

- [ ] **Step 2: Confirm the golden test now fails (snapshot drift)**

Run: `go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the rendered `default/web` line now carries the enriched Detail, which the snapshot lacks.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report -run TestGoldenScanOutput -update`
Then inspect: `git diff internal/report/testdata/golden-scan.txt` — the only change must be the `default/web` line becoming `  ✗ default/web  ClusterIP  no ready endpoints — 2 matching pods, 0 ready`. No other lines change (the attention-line count is unchanged).

- [ ] **Step 4: Run the full report suite**

Run: `go test ./internal/report/`
Expected: PASS (any report test that referenced the old Detail string, if present, now matches the enriched string — update it if needed; the substring `no ready endpoints` is still present).

- [ ] **Step 5: Update docs**

- `website/docs/features/service-health.md`: add a short "Why a Service has no endpoints" note — for a broken (not expected-empty) Service, the Detail names the cause: the selector matches no pods, matching pods on a down node, or N matching pods with 0 ready. Read-only correlation over pods + nodes.
- `README.md`: add a phrase to the service-health bullet noting the broken-Service root cause (selector-matches-no-pods / on-a-down-node / pods-not-ready).
- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`, add a bullet:
  ```
  - **Service-no-endpoints root cause.** For a broken Service with no ready
    endpoints, `scan` now names *why* — the selector matches no pods, the matching
    pods are on a down node, or they exist but none are Ready — by correlating the
    selector against the collected pods and node health. Read-only; enriches the
    existing service finding (no new flag or metric).
  ```
- `website/docs/roadmap.md`: add this to the Shipped list (it's the first Theme-A / root-cause step for the Service → Pod → Node graph).

- [ ] **Step 6: Verify docs build (only website/ changed)**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (use the venv mkdocs if not on PATH: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, "Documentation built", no WARNING about your pages.

- [ ] **Step 7: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/report/report_test.go internal/report/testdata/golden-scan.txt website/ README.md CHANGELOG.md
git commit -m "test+docs: golden coverage and documentation for Service-no-endpoints root cause"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the release skill owns this. Touches only `internal/svchealth` (pure) + one line of `internal/scan` — no collect/cluster/watch/RBAC/Helm change → **LIGHTWEIGHT SMOKE** gate (a Kind cluster with a broken selector-based Service; confirm the enriched Detail renders). **Minor** version bump **v0.37.0 → v0.38.0**; **patch** chart bump (no Helm template change — the bump script's default patch is correct; do NOT override to minor). Hold for the user's explicit "run release and push".
