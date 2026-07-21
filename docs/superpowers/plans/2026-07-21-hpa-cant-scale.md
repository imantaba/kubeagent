# HPA-can't-scale check — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag a HorizontalPodAutoscaler that is stuck — can't fetch metrics, can't scale (target missing / scale errors), or pinned at `maxReplicas` while demand exceeds the cap — in `scan`'s NEEDS ATTENTION output.

**Architecture:** A new pure leaf package `internal/hpahealth` classifies each HPA from its own `spec` + `status.conditions` (no cluster calls, no clock). A new `collect.HorizontalPodAutoscalers` collector (autoscaling/v2) feeds it. `scan.Evaluate` runs it into `Result.HPAIssues`; `report` renders a two-line NEEDS ATTENTION block and an attention-line fragment; the `watch` daemon exposes a gauge. A new base `autoscaling/horizontalpodautoscalers` RBAC grant is added to the manifest and Helm chart.

**Tech Stack:** Go 1.26, standard-library `flag`, client-go (`AutoscalingV2`), `k8s.io/api/autoscaling/v2`, `k8s.io/api/core/v1` (for `ConditionStatus`). Tests use client-go's fake clientset and fake objects.

## Global Constraints

- **READ-ONLY.** `List` only; never create/update/patch/delete. No LLM.
- **Always-on; no flag.** One new base RBAC grant (`autoscaling/horizontalpodautoscalers`).
- **Advisory to the verdict** — `HPAIssues` never flips Healthy/Degraded.
- **Pure & deterministic** — `hpahealth.Assess` reads only the HPA objects; output sorted by (Namespace, Name); no clock, no cluster calls.
- **v1 uses the standard-library `flag` package only** — no Cobra.
- **No `Co-Authored-By: Claude` trailer** (or any Claude attribution) on any commit. Every commit authored solely by the human.
- **TDD** — write the failing test first, watch it fail, then implement.
- Constants used in output: `✗` U+2717, `⚠` U+26A0, `—` em dash U+2014.
- The `scan.Result` → `report.Input` mapping MUST go through `resultInput()` in `main.go` (Task 5), not only the `scan.Result` literal — this is the wiring gap that hid the stuck-terminating feature from the CLI in v0.34.0.

---

### Task 1: `hpahealth.Assess` — the pure classifier

**Files:**
- Create: `internal/hpahealth/hpahealth.go`
- Test: `internal/hpahealth/hpahealth_test.go`

**Interfaces:**
- Produces: `type Issue struct { Namespace, Name, Target, Category, Reason string }` (JSON tags `namespace`, `name`, `target`, `category`, `reason`); `func Assess(hpas []autoscalingv2.HorizontalPodAutoscaler) []Issue`.

- [ ] **Step 1: Write the failing test**

Create `internal/hpahealth/hpahealth_test.go`:

```go
package hpahealth

import (
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cond(t autoscalingv2.HorizontalPodAutoscalerConditionType, s corev1.ConditionStatus, reason, msg string) autoscalingv2.HorizontalPodAutoscalerCondition {
	return autoscalingv2.HorizontalPodAutoscalerCondition{Type: t, Status: s, Reason: reason, Message: msg}
}

func hpa(ns, name, kind, target string, maxReplicas int32, conds ...autoscalingv2.HorizontalPodAutoscalerCondition) autoscalingv2.HorizontalPodAutoscaler {
	return autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: kind, Name: target},
			MaxReplicas:    maxReplicas,
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{Conditions: conds},
	}
}

func find(issues []Issue, name string) (Issue, bool) {
	for _, i := range issues {
		if i.Name == name {
			return i, true
		}
	}
	return Issue{}, false
}

func TestAssess_Unable(t *testing.T) {
	h := hpa("shop", "worker-hpa", "Deployment", "worker", 5,
		cond(autoscalingv2.AbleToScale, corev1.ConditionFalse, "FailedGetScale", "the scale target Deployment/worker was not found"))
	is, ok := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "worker-hpa")
	if !ok || is.Category != "unable" {
		t.Fatalf("want unable, got %+v", is)
	}
	if is.Target != "Deployment/worker" {
		t.Errorf("target = %q, want Deployment/worker", is.Target)
	}
	if is.Reason != "can't scale — the scale target Deployment/worker was not found" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_Metrics(t *testing.T) {
	// trailing period must be trimmed.
	h := hpa("shop", "api-hpa", "Deployment", "api", 8,
		cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "FailedGetResourceMetric", "unable to get resource metric cpu: no metrics returned."))
	is, ok := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "api-hpa")
	if !ok || is.Category != "metrics" {
		t.Fatalf("want metrics, got %+v", is)
	}
	if is.Reason != "can't fetch metrics — unable to get resource metric cpu: no metrics returned" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_Capped(t *testing.T) {
	h := hpa("ops", "ingest-hpa", "Deployment", "ingest", 10,
		cond(autoscalingv2.ScalingLimited, corev1.ConditionTrue, "TooManyReplicas", "the desired replica count is more than the maximum replica count"))
	is, ok := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "ingest-hpa")
	if !ok || is.Category != "capped" {
		t.Fatalf("want capped, got %+v", is)
	}
	if is.Reason != "pinned at maxReplicas 10 — desired exceeds the cap" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_UnableBeatsMetrics(t *testing.T) {
	h := hpa("a", "both", "Deployment", "x", 3,
		cond(autoscalingv2.AbleToScale, corev1.ConditionFalse, "FailedGetScale", "no scale"),
		cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "FailedGetResourceMetric", "no metric"))
	if is, _ := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "both"); is.Category != "unable" {
		t.Fatalf("unable must win precedence, got %+v", is)
	}
}

func TestAssess_NotFlagged(t *testing.T) {
	cases := []autoscalingv2.HorizontalPodAutoscaler{
		hpa("a", "healthy", "Deployment", "h", 5,
			cond(autoscalingv2.AbleToScale, corev1.ConditionTrue, "ReadyForNewScale", ""),
			cond(autoscalingv2.ScalingActive, corev1.ConditionTrue, "ValidMetricFound", ""),
			cond(autoscalingv2.ScalingLimited, corev1.ConditionFalse, "DesiredWithinRange", "")),
		hpa("a", "atfloor", "Deployment", "f", 5,
			cond(autoscalingv2.ScalingLimited, corev1.ConditionTrue, "TooFewReplicas", "")), // idle at min → benign
		hpa("a", "fresh", "Deployment", "n", 5), // no conditions yet
	}
	if got := Assess(cases); len(got) != 0 {
		t.Fatalf("expected nothing flagged, got %+v", got)
	}
}

func TestAssess_SortedByNamespaceName(t *testing.T) {
	mk := func(ns, name string) autoscalingv2.HorizontalPodAutoscaler {
		return hpa(ns, name, "Deployment", "d", 3,
			cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "FailedGetResourceMetric", "no metric"))
	}
	got := Assess([]autoscalingv2.HorizontalPodAutoscaler{mk("b", "z"), mk("a", "y"), mk("a", "x")})
	if len(got) != 3 || got[0].Name != "x" || got[1].Name != "y" || got[2].Name != "z" {
		t.Fatalf("not sorted by (ns,name): %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/hpahealth/`
Expected: FAIL — `undefined: Assess`.

- [ ] **Step 3: Write the implementation**

Create `internal/hpahealth/hpahealth.go`:

```go
// Package hpahealth flags HorizontalPodAutoscalers that cannot scale as intended —
// one that can't fetch metrics, can't act on its scale target, or is pinned at
// maxReplicas while demand exceeds the cap — and names why. Pure and read-only:
// the caller supplies the HPA objects; every signal comes from the HPA's own spec
// and status conditions. Advisory (never affects the cluster verdict).
package hpahealth

import (
	"fmt"
	"sort"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
)

// Issue is one HorizontalPodAutoscaler that cannot scale as intended.
type Issue struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Target    string `json:"target"`   // "Deployment/api" from spec.scaleTargetRef
	Category  string `json:"category"` // "unable" | "metrics" | "capped"
	Reason    string `json:"reason"`
}

// Assess flags HPAs that cannot scale as intended, sorted by (Namespace, Name).
// A healthy HPA, one limited only at its floor, or a freshly-created HPA with no
// conditions yet, is not flagged.
func Assess(hpas []autoscalingv2.HorizontalPodAutoscaler) []Issue {
	var out []Issue
	for _, h := range hpas {
		if cat, reason, ok := classify(h); ok {
			out = append(out, Issue{
				Namespace: h.Namespace,
				Name:      h.Name,
				Target:    h.Spec.ScaleTargetRef.Kind + "/" + h.Spec.ScaleTargetRef.Name,
				Category:  cat,
				Reason:    reason,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// classify returns the first matching category (unable → metrics → capped) for an
// HPA, or ok=false when it is healthy/benign.
func classify(h autoscalingv2.HorizontalPodAutoscaler) (category, reason string, ok bool) {
	if c := condition(h, autoscalingv2.AbleToScale); c != nil && c.Status == corev1.ConditionFalse {
		return "unable", "can't scale — " + trimMsg(c.Message), true
	}
	if c := condition(h, autoscalingv2.ScalingActive); c != nil && c.Status == corev1.ConditionFalse {
		return "metrics", "can't fetch metrics — " + trimMsg(c.Message), true
	}
	// "TooManyReplicas" is the literal reason the upstream HPA controller sets on
	// ScalingLimited when it clamps the desired count down to maxReplicas.
	if c := condition(h, autoscalingv2.ScalingLimited); c != nil && c.Status == corev1.ConditionTrue && c.Reason == "TooManyReplicas" {
		return "capped", fmt.Sprintf("pinned at maxReplicas %d — desired exceeds the cap", h.Spec.MaxReplicas), true
	}
	return "", "", false
}

// condition returns the HPA's condition of the given type, or nil if absent.
func condition(h autoscalingv2.HorizontalPodAutoscaler, t autoscalingv2.HorizontalPodAutoscalerConditionType) *autoscalingv2.HorizontalPodAutoscalerCondition {
	for i := range h.Status.Conditions {
		if h.Status.Conditions[i].Type == t {
			return &h.Status.Conditions[i]
		}
	}
	return nil
}

// trimMsg drops trailing period/whitespace from a condition message.
func trimMsg(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), ". ")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/hpahealth/`
Expected: PASS (all tests).

- [ ] **Step 5: Commit**

```bash
git add internal/hpahealth/
git commit -m "feat(hpahealth): classify HPAs that cannot scale"
```

---

### Task 2: `collect.HorizontalPodAutoscalers` — the collector

**Files:**
- Modify: `internal/collect/collect.go` (add function + `autoscalingv2` import)
- Test: `internal/collect/collect_test.go` (add one test)

**Interfaces:**
- Produces: `func HorizontalPodAutoscalers(ctx context.Context, client kubernetes.Interface, namespace string) ([]autoscalingv2.HorizontalPodAutoscaler, error)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go` (the file already imports `context`, `testing`, `metav1`, and the fake clientset as `fake`; add `autoscalingv2 "k8s.io/api/autoscaling/v2"` to its imports):

```go
func TestHorizontalPodAutoscalers(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-hpa"}}
	client := fake.NewSimpleClientset(hpa)
	got, err := HorizontalPodAutoscalers(context.Background(), client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "api-hpa" {
		t.Fatalf("expected the seeded HPA, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/collect/ -run TestHorizontalPodAutoscalers`
Expected: FAIL — `undefined: HorizontalPodAutoscalers`.

- [ ] **Step 3: Write the implementation**

Add `autoscalingv2 "k8s.io/api/autoscaling/v2"` to the imports of `internal/collect/collect.go`, then add this function next to `PodDisruptionBudgets`:

```go
// HorizontalPodAutoscalers lists HPAs in the namespace (empty = all), read-only.
// Used by the HPA-can't-scale check. Needs the base autoscaling/horizontalpodautoscalers
// list grant; a forbidden/absent result simply omits the check.
func HorizontalPodAutoscalers(ctx context.Context, client kubernetes.Interface, namespace string) ([]autoscalingv2.HorizontalPodAutoscaler, error) {
	list, err := client.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing horizontalpodautoscalers: %w", err)
	}
	return list.Items, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/collect/ -run TestHorizontalPodAutoscalers`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): list HorizontalPodAutoscalers (read-only)"
```

---

### Task 3: `scan.Evaluate` — wiring + `Result.HPAIssues`

**Files:**
- Modify: `internal/scan/scan.go` (import, `Result.HPAIssues` field, collect+assess, return literal)
- Test: `internal/scan/scan_test.go` (add two tests)

**Interfaces:**
- Consumes: `hpahealth.Assess`, `collect.HorizontalPodAutoscalers` (Tasks 1–2).
- Produces: `Result.HPAIssues []hpahealth.Issue`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/scan/scan_test.go` (it already imports `apierrors`, `schema`, `k8stesting`, `runtime`, `metav1`, `corev1`, and the fake clientset; add `autoscalingv2 "k8s.io/api/autoscaling/v2"` to its imports if absent):

```go
func TestEvaluate_FlagsStuckHPA(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-hpa"},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}, MaxReplicas: 5},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
			{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse, Reason: "FailedGetResourceMetric", Message: "no metrics"}}},
	}
	cli := fake.NewSimpleClientset(hpa)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.HPAIssues) != 1 || res.HPAIssues[0].Category != "metrics" {
		t.Fatalf("expected one metrics HPA issue, got %+v", res.HPAIssues)
	}
}

func TestEvaluate_ForbiddenHPAsStillScans(t *testing.T) {
	cli := fake.NewSimpleClientset()
	cli.Fake.PrependReactor("list", "horizontalpodautoscalers", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"}, "", nil)
	})
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("a forbidden HPA list must not error, got %v", err)
	}
	if len(res.HPAIssues) != 0 {
		t.Fatalf("forbidden HPA list must yield no issues, got %+v", res.HPAIssues)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scan/ -run 'TestEvaluate_(FlagsStuckHPA|ForbiddenHPAsStillScans)'`
Expected: FAIL — `res.HPAIssues undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/scan/scan.go`:

1. Add the import `"github.com/imantaba/kubeagent/internal/hpahealth"`.
2. Add a field to the `Result` struct, next to `PDBIssues []pdbhealth.Issue`:

```go
	HPAIssues         []hpahealth.Issue
```

3. After the PDB wiring (`pdbIssues := pdbhealth.Assess(pdbs)`), add:

```go
	hpas, _ := collect.HorizontalPodAutoscalers(ctx, client, opts.Namespace) // forbidden/absent → nil, check skipped
	hpaIssues := hpahealth.Assess(hpas)
```

4. Add `HPAIssues: hpaIssues,` to the `return Result{…}` literal (next to `PDBIssues: pdbIssues`).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scan/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scan/
git commit -m "feat(scan): assess HorizontalPodAutoscalers into Result.HPAIssues"
```

---

### Task 4: `report` — NEEDS ATTENTION section + attention line + JSON

**Files:**
- Modify: `internal/report/report.go` (Input field, JSON field, `hasAttention`, `printHPAIssues`, render call site, `attentionLine`)
- Test: `internal/report/report_test.go` (add tests)

**Interfaces:**
- Consumes: `hpahealth.Issue` (Task 1), `Result.HPAIssues` (Task 3).
- Produces: `report.Input.HPAIssues []hpahealth.Issue`.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go` (add `"github.com/imantaba/kubeagent/internal/hpahealth"` to imports; mirror how neighbouring tests build `Input{…}` and call `PrintInventory` — note the cluster field type is `clusterhealth.ClusterHealth`):

```go
func TestPrintInventory_HPAIssues(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"},
		HPAIssues: []hpahealth.Issue{
			{Namespace: "shop", Name: "api-hpa", Target: "Deployment/api", Category: "metrics",
				Reason: "can't fetch metrics — unable to get resource metric cpu: no metrics returned"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "✗ shop/api-hpa  HorizontalPodAutoscaler  targets Deployment/api") {
		t.Errorf("missing HPA header line:\n%s", out)
	}
	if !strings.Contains(out, "⚠ HPAStuck: can't fetch metrics — unable to get resource metric cpu") {
		t.Errorf("missing HPAStuck reason line:\n%s", out)
	}
	if !strings.Contains(out, "1 HPA can't scale") {
		t.Errorf("missing attention-line fragment:\n%s", out)
	}
}

func TestPrintInventory_NoHPASectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "HPAStuck") {
		t.Errorf("no HPA section expected when empty:\n%s", buf.String())
	}
}
```

> Match the real `Input{…}` construction to neighbouring tests (e.g. `TestPrintInventory_PDBIssues`) if the cluster field or `PrintInventory` signature differs.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/report/ -run TestPrintInventory_HPAIssues`
Expected: FAIL — `unknown field HPAIssues in struct literal`.

- [ ] **Step 3: Write the implementation**

In `internal/report/report.go` (mirror the existing `PDBIssues` handling — place HPA code right after each PDB site):

1. Add `"github.com/imantaba/kubeagent/internal/hpahealth"` to imports.
2. In the JSON `inventoryReport` struct (where `PDBIssues` has its json tag), add:

```go
	HPAIssues          []hpahealth.Issue           `json:"hpaIssues,omitempty"`
```

and set it in the struct literal that copies from `in` (next to `PDBIssues: in.PDBIssues,`):

```go
		HPAIssues:          in.HPAIssues,
```

3. In the `Input` struct, add next to `PDBIssues []pdbhealth.Issue`:

```go
	HPAIssues          []hpahealth.Issue
```

4. Extend `hasAttention` (currently ends `… || len(in.PDBIssues) > 0`):

```go
	hasAttention := len(in.Result.Workloads) > 0 || len(real) > 0 || len(in.CredentialWarnings) > 0 || hasDisk || len(realIng) > 0 || len(in.PVCIssues) > 0 || len(in.StuckTerminating) > 0 || len(in.PDBIssues) > 0 || len(in.HPAIssues) > 0
```

5. Add the render call right after `printPDBIssues`:

```go
		if err := printHPAIssues(in.HPAIssues, w); err != nil {
			return err
		}
```

6. Add the printer next to `printPDBIssues`:

```go
// printHPAIssues lists HorizontalPodAutoscalers that cannot scale as intended.
func printHPAIssues(issues []hpahealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  HorizontalPodAutoscaler  targets %s\n", is.Namespace, is.Name, is.Target); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ HPAStuck: %s\n", is.Reason); err != nil {
			return err
		}
	}
	return nil
}
```

7. In `attentionLine`, after the `PDBIssues` fragment block, add:

```go
	if n := len(in.HPAIssues); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s can't scale", n, plural(n, "HPA", "HPAs")))
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/report/ -run TestPrintInventory`
Expected: PASS. (The golden test `TestGoldenScanOutput` still passes — the golden fixture has no HPA issues yet; the fixture PDB/golden update is Task 8. Do NOT modify the golden fixture or testdata here.)

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render HPA-can't-scale section and attention line"
```

---

### Task 5: `main.go` — carry `HPAIssues` through the `resultInput` seam

**Files:**
- Modify: `main.go` (add `HPAIssues: res.HPAIssues` to `resultInput`)
- Test: `main_test.go` (add one seam test)

**Interfaces:**
- Consumes: `scan.Result.HPAIssues` (Task 3), `report.Input.HPAIssues` (Task 4).

- [ ] **Step 1: Write the failing test**

Add to `main_test.go` (next to `TestResultInput_CarriesPDBIssues`; add `"github.com/imantaba/kubeagent/internal/hpahealth"` to imports):

```go
func TestResultInput_CarriesHPAIssues(t *testing.T) {
	// Regression: the scan.Result → report.Input mapping must carry HPAIssues, or
	// the section never renders in the CLI (the stuck-terminating v0.34.0 bug).
	res := scan.Result{HPAIssues: []hpahealth.Issue{
		{Namespace: "shop", Name: "api-hpa", Target: "Deployment/api", Category: "metrics", Reason: "…"},
	}}
	in := resultInput(res)
	if len(in.HPAIssues) != 1 || in.HPAIssues[0].Name != "api-hpa" {
		t.Fatalf("resultInput must carry HPAIssues into report.Input, got %+v", in.HPAIssues)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestResultInput_CarriesHPAIssues`
Expected: FAIL — `in.HPAIssues` is empty (field not mapped).

- [ ] **Step 3: Write the implementation**

In `main.go`, add to the `resultInput` return literal (next to `PDBIssues: res.PDBIssues,`):

```go
		HPAIssues:        res.HPAIssues,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestResultInput`
Expected: PASS (StuckTerminating, PDBIssues, and HPAIssues seam tests).

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "fix(report): wire HPAIssues through the resultInput seam"
```

---

### Task 6: `watch` — the `kubeagent_hpa_scaling_issues` gauge

**Files:**
- Modify: `internal/watch/metrics.go` (field, update, gauge)
- Test: `internal/watch/metrics_test.go` (add assertion)

**Interfaces:**
- Consumes: `scan.Result.HPAIssues` (Task 3).

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, in `TestMetrics_RenderReflectsResult`, add `HPAIssues: []hpahealth.Issue{{Namespace: "shop", Name: "api-hpa"}}` to the sample `scan.Result` (add the import `"github.com/imantaba/kubeagent/internal/hpahealth"`) and add to the asserted substrings list:

```go
		"kubeagent_hpa_scaling_issues 1",
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/watch/ -run Metrics`
Expected: FAIL — gauge absent / count 0.

- [ ] **Step 3: Write the implementation**

In `internal/watch/metrics.go`:

1. Add a field next to `pdbBlockingIssues int`:

```go
	hpaScalingIssues    int
```

2. In the update path (next to `m.pdbBlockingIssues = len(res.PDBIssues)`):

```go
	m.hpaScalingIssues = len(res.HPAIssues)
```

3. Add the gauge render (next to the `kubeagent_pdb_blocking_issues` gauge):

```go
	gauge("kubeagent_hpa_scaling_issues", "HorizontalPodAutoscalers that cannot scale as intended", float64(m.hpaScalingIssues))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/watch/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/
git commit -m "feat(watch): expose kubeagent_hpa_scaling_issues gauge"
```

---

### Task 7: RBAC + Helm — the `autoscaling/horizontalpodautoscalers` grant

**Files:**
- Modify: `deploy/rbac.yaml` (add rule)
- Modify: `deploy/helm/kubeagent/templates/clusterrole.yaml` (add rule)

**Interfaces:** none (manifests).

- [ ] **Step 1: Add the rule to both files**

In `deploy/rbac.yaml`, after the `policy` rule block (the one granting `poddisruptionbudgets`, at lines 33-35), add:

```yaml
  - apiGroups: ["autoscaling"]
    resources: [horizontalpodautoscalers]
    verbs: [get, list, watch]
```

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, after the same `policy` rule block (lines 31-33) and BEFORE the `{{- if or .Values.diskUsage.enabled … }}` conditional block, add the identical three lines.

- [ ] **Step 2: Verify Helm renders the rule and lints clean**

Run:
```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent | grep -A2 'apiGroups: \["autoscaling"\]'
```
Expected: `1 chart(s) linted, 0 chart(s) failed`; the grep shows the `horizontalpodautoscalers` rule.

- [ ] **Step 3: Verify the raw manifest has the rule**

Run: `grep -A2 'autoscaling' deploy/rbac.yaml`
Expected: shows the `horizontalpodautoscalers` rule under `apiGroups: ["autoscaling"]`.

- [ ] **Step 4: Commit**

```bash
git add deploy/rbac.yaml deploy/helm/kubeagent/templates/clusterrole.yaml
git commit -m "feat(rbac): grant read-only autoscaling/horizontalpodautoscalers"
```

---

### Task 8: Golden snapshot + coverage guard + docs

**Files:**
- Modify: `internal/report/golden_test.go` (add an HPA to `goldenInput`; extend `TestGoldenInputCoversAllSections`)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerate)
- Modify: `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** Consumes everything above.

- [ ] **Step 1: Add an HPA to the golden fixture and extend the coverage guard**

In `internal/report/golden_test.go`, in the `goldenInput` builder, add (next to the `PDBIssues:` field), importing `hpahealth`:

```go
		HPAIssues: []hpahealth.Issue{
			{Namespace: "shop", Name: "api-hpa", Target: "Deployment/api", Category: "metrics",
				Reason: "can't fetch metrics — unable to get resource metric cpu: no metrics returned"},
		},
```

Extend the guard in `TestGoldenInputCoversAllSections` — change the trailing condition `|| len(in.PDBIssues) == 0` to also include `|| len(in.HPAIssues) == 0`.

- [ ] **Step 2: Run the golden test to confirm it fails (snapshot drift)**

Run: `go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the rendered output now contains the HPA block, which the snapshot lacks.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report -run TestGoldenScanOutput -update`
Then inspect: `git diff internal/report/testdata/golden-scan.txt` — it must add the two HPA lines (`✗ shop/api-hpa  HorizontalPodAutoscaler  targets Deployment/api` and `⚠ HPAStuck: can't fetch metrics …`) and update the `Needs attention:` line with `· 1 HPA can't scale`.

- [ ] **Step 4: Run the full report suite**

Run: `go test ./internal/report/`
Expected: PASS.

- [ ] **Step 5: Update docs**

- `website/docs/features/diagnostics.md`: add an "HPA-can't-scale" entry describing the three categories (unable / metrics / capped) and that it's advisory/read-only.
- `website/docs/features/watch-mode.md`: add the `kubeagent_hpa_scaling_issues` gauge to the metrics list.
- `README.md`: add HPA-can't-scale to the detector list.
- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`, add a bullet:
  ```
  - **HPA-can't-scale detection.** `scan` flags a HorizontalPodAutoscaler that is
    stuck — can't fetch metrics (broken autoscaling), can't scale because its
    target is missing or the scale subresource errors, or is pinned at
    `maxReplicas` while demand exceeds the cap — naming the target and the reason.
    Read-only and advisory; the daemon exposes `kubeagent_hpa_scaling_issues`.
    Adds a base `autoscaling/horizontalpodautoscalers` read grant.
  ```
- `website/docs/roadmap.md`: move HPA-can't-scale from the Theme-B "headed" list into the Shipped list.

- [ ] **Step 6: Verify docs build (only website/ changed)**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (use the venv mkdocs if not on PATH: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, "Documentation built", no WARNING about your pages.

- [ ] **Step 7: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt website/ README.md CHANGELOG.md
git commit -m "test+docs: golden coverage and documentation for HPA-can't-scale"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the release skill owns this. Touches `internal/collect` + RBAC + Helm → **FULL CHAOS GATE** (`./chaos/run.sh --recreate`) plus a targeted live smoke (an HPA whose scale target is missing → `unable`, and/or one pinned at maxReplicas → `capped`). **Minor** version bump **v0.35.0 → v0.36.0**; **minor** chart bump (Helm template changed). Hold for the user's explicit "run release and push".
