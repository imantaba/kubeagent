# PodDisruptionBudget-blocked drains — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag PodDisruptionBudgets that will block a node drain — a misconfigured PDB that can never allow an eviction, a stale zero-pod selector, or a PDB blocking evictions on an already-degraded workload — in `scan`'s NEEDS ATTENTION output.

**Architecture:** A new pure leaf package `internal/pdbhealth` classifies each PDB from its own `spec` + `status` (no cluster calls, no clock). A new `collect.PodDisruptionBudgets` collector feeds it. `scan.Evaluate` runs it into `Result.PDBIssues`; `report` renders a two-line NEEDS ATTENTION block and an attention-line fragment; the `watch` daemon exposes a gauge. A new base `policy/poddisruptionbudgets` RBAC grant is added to the manifest and Helm chart.

**Tech Stack:** Go 1.26, standard-library `flag`, client-go (`PolicyV1`), `k8s.io/api/policy/v1`, `k8s.io/apimachinery/pkg/util/intstr`. Tests use client-go's fake clientset and fake objects.

## Global Constraints

- **READ-ONLY.** `List` only; never create/update/patch/delete. No LLM.
- **Always-on; no flag.** One new base RBAC grant (`policy/poddisruptionbudgets`).
- **Advisory to the verdict** — `PDBIssues` never flips Healthy/Degraded.
- **Pure & deterministic** — `pdbhealth.Assess` reads only the PDB objects; output sorted by (Namespace, Name); no clock, no cluster calls.
- **v1 uses the standard-library `flag` package only** — no Cobra.
- **No `Co-Authored-By: Claude` trailer** (or any Claude attribution) on any commit. Every commit authored solely by the human.
- **TDD** — write the failing test first, watch it fail, then implement.
- Constants used in output: `✗` U+2717, `⚠` U+26A0, `—` em dash U+2014.
- The `scan.Result` → `report.Input` mapping MUST go through `resultInput()` in `main.go` (Task 5), not only the `scan.Result` literal — this is the wiring gap that hid the stuck-terminating feature from the CLI in v0.34.0.

---

### Task 1: `pdbhealth.Assess` — the pure classifier

**Files:**
- Create: `internal/pdbhealth/pdbhealth.go`
- Test: `internal/pdbhealth/pdbhealth_test.go`

**Interfaces:**
- Produces: `type Issue struct { Namespace, Name, Rule, Category, Reason string }` (JSON tags `namespace`, `name`, `rule`, `category`, `reason`); `func Assess(pdbs []policyv1.PodDisruptionBudget) []Issue`.

- [ ] **Step 1: Write the failing test**

Create `internal/pdbhealth/pdbhealth_test.go`:

```go
package pdbhealth

import (
	"testing"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// pdb builds a PDB with a minAvailable rule and the given status counts.
func pdb(ns, name string, minAvail int, expected, desired, current, allowed int32) policyv1.PodDisruptionBudget {
	m := intstr.FromInt(minAvail)
	return policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &m},
		Status: policyv1.PodDisruptionBudgetStatus{
			ExpectedPods: expected, DesiredHealthy: desired,
			CurrentHealthy: current, DisruptionsAllowed: allowed,
		},
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

func TestAssess_Unsatisfiable(t *testing.T) {
	// minAvailable 3 covering all 3 pods → can never allow a disruption.
	got := Assess([]policyv1.PodDisruptionBudget{pdb("shop", "api", 3, 3, 3, 3, 0)})
	is, ok := find(got, "api")
	if !ok || is.Category != "unsatisfiable" {
		t.Fatalf("want unsatisfiable, got %+v", got)
	}
	if is.Rule != "minAvailable: 3" {
		t.Errorf("rule = %q, want minAvailable: 3", is.Rule)
	}
	if is.Reason != "covers all 3 pods — no voluntary eviction can ever proceed; every node drain will hang" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_Blocking(t *testing.T) {
	// disruptionsAllowed 0 with only 1/2 guarded pods healthy.
	got := Assess([]policyv1.PodDisruptionBudget{pdb("shop", "cache", 2, 2, 2, 1, 0)})
	is, ok := find(got, "cache")
	if !ok || is.Category != "blocking" {
		t.Fatalf("want blocking, got %+v", got)
	}
	if is.Reason != "blocking evictions with only 1/2 guarded pods healthy" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_Stale(t *testing.T) {
	// selector matches no pods.
	got := Assess([]policyv1.PodDisruptionBudget{pdb("ops", "legacy", 1, 0, 0, 0, 0)})
	is, ok := find(got, "legacy")
	if !ok || is.Category != "stale" {
		t.Fatalf("want stale, got %+v", got)
	}
	if is.Reason != "selector matches no pods (stale?)" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_MaxUnavailableZeroRuleString(t *testing.T) {
	// maxUnavailable 0 on a multi-replica workload → unsatisfiable, rule names maxUnavailable.
	mu := intstr.FromInt(0)
	p := policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "mu"},
		Spec:       policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &mu},
		Status:     policyv1.PodDisruptionBudgetStatus{ExpectedPods: 2, DesiredHealthy: 2, CurrentHealthy: 2, DisruptionsAllowed: 0},
	}
	is, ok := find(Assess([]policyv1.PodDisruptionBudget{p}), "mu")
	if !ok || is.Category != "unsatisfiable" || is.Rule != "maxUnavailable: 0" {
		t.Fatalf("want unsatisfiable maxUnavailable: 0, got %+v", is)
	}
}

func TestAssess_NotFlagged(t *testing.T) {
	cases := []policyv1.PodDisruptionBudget{
		pdb("a", "singleton", 1, 1, 1, 1, 0), // single replica → excluded by expectedPods>1 guard
		pdb("a", "healthy23", 2, 3, 2, 3, 1),  // minAvailable 2 of 3, disruptionsAllowed 1
	}
	if got := Assess(cases); len(got) != 0 {
		t.Fatalf("expected nothing flagged, got %+v", got)
	}
}

func TestAssess_StaleBeatsBlocking(t *testing.T) {
	// expectedPods 0 AND currentHealthy < desiredHealthy → reported as stale (precedence).
	got := Assess([]policyv1.PodDisruptionBudget{pdb("a", "p", 1, 0, 1, 0, 0)})
	if is, _ := find(got, "p"); is.Category != "stale" {
		t.Fatalf("stale must win precedence, got %+v", got)
	}
}

func TestAssess_SortedByNamespaceName(t *testing.T) {
	got := Assess([]policyv1.PodDisruptionBudget{
		pdb("b", "z", 2, 2, 2, 2, 0),
		pdb("a", "y", 2, 2, 2, 2, 0),
		pdb("a", "x", 2, 2, 2, 2, 0),
	})
	if len(got) != 3 || got[0].Name != "x" || got[1].Name != "y" || got[2].Name != "z" {
		t.Fatalf("not sorted by (ns,name): %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/pdbhealth/`
Expected: FAIL — `undefined: Assess` (package has no implementation yet).

- [ ] **Step 3: Write the implementation**

Create `internal/pdbhealth/pdbhealth.go`:

```go
// Package pdbhealth flags PodDisruptionBudgets that will block a node drain — a
// PDB that can never allow a voluntary eviction, one whose selector matches no
// pods, or one blocking evictions on an already-degraded workload. Pure and
// read-only: the caller supplies the PDB objects; every count comes from the
// PDB's own status. Advisory (never affects the cluster verdict).
package pdbhealth

import (
	"fmt"
	"sort"

	policyv1 "k8s.io/api/policy/v1"
)

// Issue is one PodDisruptionBudget that will block a node drain.
type Issue struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Rule      string `json:"rule"`     // "minAvailable: 3" | "maxUnavailable: 0"
	Category  string `json:"category"` // "stale" | "unsatisfiable" | "blocking"
	Reason    string `json:"reason"`
}

// Assess flags PDBs that will block a node drain, sorted by (Namespace, Name).
// A benign PDB (a healthy at-floor budget, a single-replica singleton, or one
// that currently allows a disruption) is not flagged.
func Assess(pdbs []policyv1.PodDisruptionBudget) []Issue {
	var out []Issue
	for _, p := range pdbs {
		rule := ruleString(p)
		if rule == "" {
			continue // neither minAvailable nor maxUnavailable set — nothing to say
		}
		if cat, reason, ok := classify(p); ok {
			out = append(out, Issue{Namespace: p.Namespace, Name: p.Name, Rule: rule, Category: cat, Reason: reason})
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

// ruleString renders the PDB's rule ("minAvailable: 3" / "maxUnavailable: 0").
// Exactly one of the two is set on a valid PDB; if neither is, returns "".
func ruleString(p policyv1.PodDisruptionBudget) string {
	switch {
	case p.Spec.MinAvailable != nil:
		return "minAvailable: " + p.Spec.MinAvailable.String()
	case p.Spec.MaxUnavailable != nil:
		return "maxUnavailable: " + p.Spec.MaxUnavailable.String()
	default:
		return ""
	}
}

// classify returns the first matching category (stale → unsatisfiable → blocking)
// for a PDB, or ok=false when it is benign. All counts come from status.
func classify(p policyv1.PodDisruptionBudget) (category, reason string, ok bool) {
	s := p.Status
	switch {
	case s.ExpectedPods == 0:
		return "stale", "selector matches no pods (stale?)", true
	case s.ExpectedPods > 1 && s.DesiredHealthy >= s.ExpectedPods:
		return "unsatisfiable", fmt.Sprintf("covers all %d pods — no voluntary eviction can ever proceed; every node drain will hang", s.ExpectedPods), true
	case s.DisruptionsAllowed == 0 && s.CurrentHealthy < s.DesiredHealthy:
		return "blocking", fmt.Sprintf("blocking evictions with only %d/%d guarded pods healthy", s.CurrentHealthy, s.DesiredHealthy), true
	default:
		return "", "", false
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pdbhealth/`
Expected: PASS (all tests).

- [ ] **Step 5: Commit**

```bash
git add internal/pdbhealth/
git commit -m "feat(pdbhealth): classify PDBs that block a node drain"
```

---

### Task 2: `collect.PodDisruptionBudgets` — the collector

**Files:**
- Modify: `internal/collect/collect.go` (add function + `policyv1` import)
- Test: `internal/collect/collect_test.go` (add one test)

**Interfaces:**
- Produces: `func PodDisruptionBudgets(ctx context.Context, client kubernetes.Interface, namespace string) ([]policyv1.PodDisruptionBudget, error)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go` (the file already imports `context`, `testing`, `metav1`, and the fake clientset — reuse them; add `policyv1 "k8s.io/api/policy/v1"` to its imports if not present):

```go
func TestPodDisruptionBudgets(t *testing.T) {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"}}
	client := fake.NewSimpleClientset(pdb)
	got, err := PodDisruptionBudgets(context.Background(), client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "api" {
		t.Fatalf("expected the seeded PDB, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/collect/ -run TestPodDisruptionBudgets`
Expected: FAIL — `undefined: PodDisruptionBudgets`.

- [ ] **Step 3: Write the implementation**

Add `policyv1 "k8s.io/api/policy/v1"` to the imports of `internal/collect/collect.go`, then add this function next to `Namespaces`:

```go
// PodDisruptionBudgets lists PDBs in the namespace (empty = all), read-only. Used
// by the PDB-blocked-drains check. Needs the base policy/poddisruptionbudgets list
// grant; a forbidden/absent result simply omits the check.
func PodDisruptionBudgets(ctx context.Context, client kubernetes.Interface, namespace string) ([]policyv1.PodDisruptionBudget, error) {
	list, err := client.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing poddisruptionbudgets: %w", err)
	}
	return list.Items, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/collect/ -run TestPodDisruptionBudgets`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): list PodDisruptionBudgets (read-only)"
```

---

### Task 3: `scan.Evaluate` — wiring + `Result.PDBIssues`

**Files:**
- Modify: `internal/scan/scan.go` (add import, `Result.PDBIssues` field, collect+assess, return literal)
- Test: `internal/scan/scan_test.go` (add two tests)

**Interfaces:**
- Consumes: `pdbhealth.Assess`, `collect.PodDisruptionBudgets` (Tasks 1–2).
- Produces: `Result.PDBIssues []pdbhealth.Issue`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/scan/scan_test.go` (it already imports `apierrors`, `schema`, `k8stesting`, `runtime`, `policyv1` may need adding — add `policyv1 "k8s.io/api/policy/v1"` and `intstr "k8s.io/apimachinery/pkg/util/intstr"` if absent):

```go
func TestEvaluate_FlagsUnsatisfiablePDB(t *testing.T) {
	m := intstr.FromInt(3)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &m},
		Status:     policyv1.PodDisruptionBudgetStatus{ExpectedPods: 3, DesiredHealthy: 3, CurrentHealthy: 3, DisruptionsAllowed: 0},
	}
	cli := fake.NewSimpleClientset(pdb)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.PDBIssues) != 1 || res.PDBIssues[0].Category != "unsatisfiable" {
		t.Fatalf("expected one unsatisfiable PDB issue, got %+v", res.PDBIssues)
	}
}

func TestEvaluate_ForbiddenPDBsStillScans(t *testing.T) {
	cli := fake.NewSimpleClientset()
	cli.Fake.PrependReactor("list", "poddisruptionbudgets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "policy", Resource: "poddisruptionbudgets"}, "", nil)
	})
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("a forbidden PDB list must not error, got %v", err)
	}
	if len(res.PDBIssues) != 0 {
		t.Fatalf("forbidden PDB list must yield no issues, got %+v", res.PDBIssues)
	}
}
```

> Note: match `Evaluate`'s real signature and the `Options`/`Result` type names as they appear in `scan.go` (this file already tests them — mirror the existing calls, e.g. `Evaluate(ctx, cli, Options{...})`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scan/ -run 'TestEvaluate_(FlagsUnsatisfiablePDB|ForbiddenPDBsStillScans)'`
Expected: FAIL — `res.PDBIssues undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/scan/scan.go`:

1. Add the import `"github.com/imantaba/kubeagent/internal/pdbhealth"`.
2. Add a field to the `Result` struct, next to `StuckTerminating`:

```go
	PDBIssues         []pdbhealth.Issue
```

3. After the stuck-terminating wiring (`stuckTerminating := termhealth.Assess(...)`), add:

```go
	pdbs, _ := collect.PodDisruptionBudgets(ctx, client, opts.Namespace) // forbidden/absent → nil, check skipped
	pdbIssues := pdbhealth.Assess(pdbs)
```

4. Add `PDBIssues: pdbIssues,` to the `return Result{…}` literal.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scan/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scan/
git commit -m "feat(scan): assess PodDisruptionBudgets into Result.PDBIssues"
```

---

### Task 4: `report` — NEEDS ATTENTION section + attention line + JSON

**Files:**
- Modify: `internal/report/report.go` (Input field, JSON field, `hasAttention`, `printPDBIssues`, render call site, `attentionLine`)
- Test: `internal/report/report_test.go` (add tests)

**Interfaces:**
- Consumes: `pdbhealth.Issue` (Task 1), `Result.PDBIssues` (Task 3).
- Produces: `report.Input.PDBIssues []pdbhealth.Issue`.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go` (add `"github.com/imantaba/kubeagent/internal/pdbhealth"` to imports):

```go
func TestPrintInventory_PDBIssues(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.Report{Verdict: "Healthy"},
		PDBIssues: []pdbhealth.Issue{
			{Namespace: "shop", Name: "api", Rule: "minAvailable: 3", Category: "unsatisfiable",
				Reason: "covers all 3 pods — no voluntary eviction can ever proceed; every node drain will hang"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "✗ shop/api  PodDisruptionBudget  minAvailable: 3") {
		t.Errorf("missing PDB header line:\n%s", out)
	}
	if !strings.Contains(out, "⚠ PDBBlocked: covers all 3 pods") {
		t.Errorf("missing PDBBlocked reason line:\n%s", out)
	}
	if !strings.Contains(out, "1 PodDisruptionBudget blocking drains") {
		t.Errorf("missing attention-line fragment:\n%s", out)
	}
}

func TestPrintInventory_NoPDBSectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.Report{Verdict: "Healthy"}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "PDBBlocked") {
		t.Errorf("no PDB section expected when empty:\n%s", buf.String())
	}
}
```

> Match `PrintInventory`'s real signature and the `Input`/`clusterhealth.Report` construction to how neighbouring tests in this file build them (mirror an existing `Input{…}` test literal).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/report/ -run TestPrintInventory_PDBIssues`
Expected: FAIL — `unknown field PDBIssues in struct literal`.

- [ ] **Step 3: Write the implementation**

In `internal/report/report.go`:

1. Add `"github.com/imantaba/kubeagent/internal/pdbhealth"` to imports.
2. In the JSON `inventoryReport` struct (where `StuckTerminating` has its json tag), add:

```go
	PDBIssues          []pdbhealth.Issue           `json:"pdbIssues,omitempty"`
```

and set it in the struct literal that copies from `in` (next to `StuckTerminating: in.StuckTerminating,`):

```go
		PDBIssues:          in.PDBIssues,
```

3. In the `Input` struct, add next to `StuckTerminating`:

```go
	PDBIssues          []pdbhealth.Issue
```

4. Extend `hasAttention` (currently ends `… || len(in.StuckTerminating) > 0`):

```go
	hasAttention := len(in.Result.Workloads) > 0 || len(real) > 0 || len(in.CredentialWarnings) > 0 || hasDisk || len(realIng) > 0 || len(in.PVCIssues) > 0 || len(in.StuckTerminating) > 0 || len(in.PDBIssues) > 0
```

5. Add the render call right after `printStuckTerminating`:

```go
		if err := printPDBIssues(in.PDBIssues, w); err != nil {
			return err
		}
```

6. Add the printer next to `printStuckTerminating`:

```go
// printPDBIssues lists PodDisruptionBudgets that will block a node drain.
func printPDBIssues(issues []pdbhealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  PodDisruptionBudget  %s\n", is.Namespace, is.Name, is.Rule); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ PDBBlocked: %s\n", is.Reason); err != nil {
			return err
		}
	}
	return nil
}
```

7. In `attentionLine`, after the `StuckTerminating` fragment block, add:

```go
	if n := len(in.PDBIssues); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s blocking drains", n, plural(n, "PodDisruptionBudget", "PodDisruptionBudgets")))
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/report/ -run TestPrintInventory`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render PDB-blocked-drains section and attention line"
```

---

### Task 5: `main.go` — carry `PDBIssues` through the `resultInput` seam

**Files:**
- Modify: `main.go` (add `PDBIssues: res.PDBIssues` to `resultInput`)
- Test: `main_test.go` (add one seam test)

**Interfaces:**
- Consumes: `scan.Result.PDBIssues` (Task 3), `report.Input.PDBIssues` (Task 4).

- [ ] **Step 1: Write the failing test**

Add to `main_test.go` (next to `TestResultInput_CarriesStuckTerminating`; imports `scan` and `pdbhealth` — add `"github.com/imantaba/kubeagent/internal/pdbhealth"`):

```go
func TestResultInput_CarriesPDBIssues(t *testing.T) {
	// Regression: the scan.Result → report.Input mapping must carry PDBIssues, or
	// the section never renders in the CLI (the stuck-terminating v0.34.0 bug).
	res := scan.Result{PDBIssues: []pdbhealth.Issue{
		{Namespace: "shop", Name: "api", Rule: "minAvailable: 3", Category: "unsatisfiable", Reason: "…"},
	}}
	in := resultInput(res)
	if len(in.PDBIssues) != 1 || in.PDBIssues[0].Name != "api" {
		t.Fatalf("resultInput must carry PDBIssues into report.Input, got %+v", in.PDBIssues)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestResultInput_CarriesPDBIssues`
Expected: FAIL — `in.PDBIssues` is empty (field not mapped).

- [ ] **Step 3: Write the implementation**

In `main.go`, add to the `resultInput` return literal (next to `StuckTerminating: res.StuckTerminating,`):

```go
		PDBIssues:        res.PDBIssues,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestResultInput`
Expected: PASS (both StuckTerminating and PDBIssues seam tests).

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "fix(report): wire PDBIssues through the resultInput seam"
```

---

### Task 6: `watch` — the `kubeagent_pdb_blocking_issues` gauge

**Files:**
- Modify: `internal/watch/metrics.go` (field, update, gauge)
- Test: `internal/watch/metrics_test.go` (add assertion)

**Interfaces:**
- Consumes: `scan.Result.PDBIssues` (Task 3).

- [ ] **Step 1: Write the failing test**

Add to `internal/watch/metrics_test.go` (mirror the existing `kubeagent_resources_stuck_terminating` gauge test — find the test that renders metrics from a sample `scan.Result` and add a PDB issue + assertion). Add to the sample `scan.Result` used there `PDBIssues: []pdbhealth.Issue{{Namespace: "shop", Name: "api"}}` (import `pdbhealth`) and assert:

```go
	if !strings.Contains(out, "kubeagent_pdb_blocking_issues 1") {
		t.Errorf("expected pdb gauge = 1:\n%s", out)
	}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/watch/ -run Metrics`
Expected: FAIL — gauge absent / count 0.

- [ ] **Step 3: Write the implementation**

In `internal/watch/metrics.go`:

1. Add a field next to `stuckTerminating int`:

```go
	pdbBlockingIssues   int
```

2. In the update path (next to `m.stuckTerminating = len(res.StuckTerminating)`):

```go
	m.pdbBlockingIssues = len(res.PDBIssues)
```

3. Add the gauge render (next to the `kubeagent_resources_stuck_terminating` gauge):

```go
	gauge("kubeagent_pdb_blocking_issues", "PodDisruptionBudgets that will block a node drain", float64(m.pdbBlockingIssues))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/watch/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/
git commit -m "feat(watch): expose kubeagent_pdb_blocking_issues gauge"
```

---

### Task 7: RBAC + Helm — the `policy/poddisruptionbudgets` grant

**Files:**
- Modify: `deploy/rbac.yaml` (add rule)
- Modify: `deploy/helm/kubeagent/templates/clusterrole.yaml` (add rule)

**Interfaces:** none (manifests).

- [ ] **Step 1: Add the rule to both files**

In `deploy/rbac.yaml`, after the `coordination.k8s.io` rule block, add:

```yaml
  - apiGroups: ["policy"]
    resources: [poddisruptionbudgets]
    verbs: [get, list, watch]
```

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, after the same `coordination.k8s.io` rule block (before the `{{- if or .Values.diskUsage.enabled … }}` block), add the identical three lines.

- [ ] **Step 2: Verify Helm renders the rule and lints clean**

Run:
```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent | grep -A2 'apiGroups: \["policy"\]'
```
Expected: `1 chart(s) linted, 0 chart(s) failed`; the grep shows the `poddisruptionbudgets` rule.

- [ ] **Step 3: Verify the raw manifest is valid YAML with the rule**

Run: `grep -A2 'policy' deploy/rbac.yaml`
Expected: shows the `poddisruptionbudgets` rule under `apiGroups: ["policy"]`.

- [ ] **Step 4: Commit**

```bash
git add deploy/rbac.yaml deploy/helm/kubeagent/templates/clusterrole.yaml
git commit -m "feat(rbac): grant read-only policy/poddisruptionbudgets"
```

---

### Task 8: Golden snapshot + coverage guard + docs

**Files:**
- Modify: `internal/report/golden_test.go` (add a PDB to `goldenInput`; extend `TestGoldenInputCoversAllSections`)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerate)
- Modify: `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** Consumes everything above.

- [ ] **Step 1: Add a PDB to the golden fixture and extend the coverage guard**

In `internal/report/golden_test.go`, in the `goldenInput` builder, add (next to the `StuckTerminating:` field), importing `pdbhealth`:

```go
		PDBIssues: []pdbhealth.Issue{
			{Namespace: "shop", Name: "api-pdb", Rule: "minAvailable: 3", Category: "unsatisfiable",
				Reason: "covers all 3 pods — no voluntary eviction can ever proceed; every node drain will hang"},
		},
```

Extend the guard in `TestGoldenInputCoversAllSections` — change the trailing condition `|| len(in.StuckTerminating) == 0` to also include `|| len(in.PDBIssues) == 0`.

- [ ] **Step 2: Run the golden test to confirm it fails (snapshot drift)**

Run: `go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the rendered output now contains the PDB block, which the snapshot lacks.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report -run TestGoldenScanOutput -update`
Then inspect the diff: `git diff internal/report/testdata/golden-scan.txt` — it must add the two PDB lines and update the `Needs attention:` line with `· 1 PodDisruptionBudget blocking drains`.

- [ ] **Step 4: Run the full report suite**

Run: `go test ./internal/report/`
Expected: PASS.

- [ ] **Step 5: Update docs**

- `website/docs/features/diagnostics.md`: add a short "PodDisruptionBudget-blocked drains" entry describing the three categories and that it's advisory/read-only.
- `website/docs/features/watch-mode.md`: add the `kubeagent_pdb_blocking_issues` gauge to the metrics list.
- `README.md`: add PDB-blocked drains to the detector list.
- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`, add a bullet:
  ```
  - **PodDisruptionBudget-blocked drains.** `scan` flags a PDB that will block a
    node drain — one that can never allow a voluntary eviction, a stale zero-pod
    selector, or a PDB blocking evictions on an already-degraded workload —
    naming the rule and the guarded-pod counts. Read-only and advisory; the
    daemon exposes `kubeagent_pdb_blocking_issues`. Adds a base
    `policy/poddisruptionbudgets` read grant.
  ```
- `website/docs/roadmap.md`: move PDB-blocked drains from the Theme-B "headed" list into the Shipped list.

- [ ] **Step 6: Verify docs build (only website/ changed)**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (use the venv mkdocs if not on PATH: `/tmp/.../scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, "Documentation built", no WARNING about your pages.

- [ ] **Step 7: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt website/ README.md CHANGELOG.md
git commit -m "test+docs: golden coverage and documentation for PDB-blocked drains"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the release skill owns this. Touches `internal/collect` + RBAC + Helm → **FULL CHAOS GATE** (`./chaos/run.sh --recreate`) plus a targeted live smoke (a PDB with `minAvailable` ≥ replicas that hangs a drain). **Minor** version bump **v0.34.0 → v0.35.0**; **minor** chart bump (Helm template changed). Hold for the user's explicit "run release and push".
