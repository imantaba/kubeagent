# ResourceQuota near-exhaustion check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag a ResourceQuota whose `used/hard` ratio is at or over 0.90, as an advisory NEEDS ATTENTION block — the proactive early-warning complement to the reactive `FailedCreate` detector.

**Architecture:** A new pure Assess package (`internal/quotahealth`) mirroring `pdbhealth`/`hpahealth`, fed by a new namespace-scoped `collect.ResourceQuotas` collector, wired into `scan.Evaluate` as `Result.QuotaIssues`, rendered by `report`, mapped through `main.go`'s `resultInput` seam, exposed as a `watch` gauge, and enabled by adding `resourcequotas` to the core-group RBAC grant.

**Tech Stack:** Go 1.26, `k8s.io/api/core/v1` (`ResourceQuota`, `resource.Quantity`), client-go fake clientset for I/O tests, pure fake-object tests for the assessor.

## Global Constraints

- **Read-only; always-on; no CLI flag.** Threshold is a fixed **0.90** default, overridable by the env var **`KUBEAGENT_QUOTA_THRESHOLD`** (a float; out-of-range or `<=0`/`>1` falls back to 0.90).
- **Coverage:** evaluate **every** `status.hard` entry generically (pods, requests.cpu/memory, limits.*, services, configmaps, secrets, pvc, `count/*`, extended). Same used/hard ratio math for all.
- **Severity:** `exhausted` when `used >= hard` (ratio ≥ 1.0), else `near`. Sort **exhausted-first**, then ratio descending, then (Namespace, Quota, Resource).
- **Guard:** skip any entry whose `hard <= 0` (deliberate zero quota; avoids div-by-zero).
- **Advisory** — appears in NEEDS ATTENTION; does **not** change the cluster verdict.
- **Pure & deterministic** — `Assess` reads only the passed quotas + threshold; no clock, no cluster calls; sorted output.
- **`resultInput` seam:** `main.go`'s `resultInput` MUST copy `QuotaIssues` from `Result` to `Input`, or the section silently never renders.
- Gate: collection/RBAC/watch/Helm-template changes → **FULL CHAOS GATE**. **Minor** bump v0.43.0 → **v0.44.0**; **chart MINOR** bump (clusterrole template changed).
- `confidence`, `inventory`, `clusterhealth`, `rootcause`, `explain.go`, `--fix`, and all existing detectors/Assess checks stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

---

### Task 1: `quotahealth` package (pure assessor)

**Files:**
- Create: `internal/quotahealth/quotahealth.go`
- Test: `internal/quotahealth/quotahealth_test.go`

**Interfaces:**
- Consumes: `corev1.ResourceQuota` (`.Namespace`, `.Name`, `.Status.Hard`/`.Status.Used` which are `corev1.ResourceList = map[corev1.ResourceName]resource.Quantity`).
- Produces: `type Issue struct { Namespace, Quota, Resource, Used, Hard string; Ratio float64; Severity string }` and `func Assess(quotas []corev1.ResourceQuota, threshold float64) []Issue`. Used by Tasks 3 (scan), 4 (report), 6 (watch).

- [ ] **Step 1: Write the failing test**

Create `internal/quotahealth/quotahealth_test.go`:

```go
package quotahealth

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// quota builds a ResourceQuota with the given hard/used maps (quantity strings).
func quota(ns, name string, hard, used map[corev1.ResourceName]string) corev1.ResourceQuota {
	h := corev1.ResourceList{}
	for k, v := range hard {
		h[k] = resource.MustParse(v)
	}
	u := corev1.ResourceList{}
	for k, v := range used {
		u[k] = resource.MustParse(v)
	}
	return corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     corev1.ResourceQuotaStatus{Hard: h, Used: u},
	}
}

func approxEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// findIssue returns the first Issue for a resource, or a zero Issue and false.
func findIssue(issues []Issue, res string) (Issue, bool) {
	for _, is := range issues {
		if is.Resource == res {
			return is, true
		}
	}
	return Issue{}, false
}

func TestAssess_Near(t *testing.T) {
	qs := []corev1.ResourceQuota{quota("shop", "compute",
		map[corev1.ResourceName]string{"pods": "50"},
		map[corev1.ResourceName]string{"pods": "47"})}

	got := Assess(qs, 0.90)

	if len(got) != 1 {
		t.Fatalf("want 1 issue, got %+v", got)
	}
	is := got[0]
	if is.Severity != "near" {
		t.Errorf("Severity = %q, want near", is.Severity)
	}
	if is.Used != "47" || is.Hard != "50" {
		t.Errorf("Used/Hard = %q/%q, want 47/50", is.Used, is.Hard)
	}
	if !approxEq(is.Ratio, 0.94) {
		t.Errorf("Ratio = %v, want ~0.94", is.Ratio)
	}
	if is.Namespace != "shop" || is.Quota != "compute" || is.Resource != "pods" {
		t.Errorf("identity = %s/%s %s, want shop/compute pods", is.Namespace, is.Quota, is.Resource)
	}
}

func TestAssess_Exhausted(t *testing.T) {
	qs := []corev1.ResourceQuota{quota("shop", "compute",
		map[corev1.ResourceName]string{"requests.cpu": "4"},
		map[corev1.ResourceName]string{"requests.cpu": "4"})}

	got := Assess(qs, 0.90)

	if len(got) != 1 || got[0].Severity != "exhausted" {
		t.Fatalf("want one exhausted issue, got %+v", got)
	}
	if !approxEq(got[0].Ratio, 1.0) {
		t.Errorf("Ratio = %v, want 1.0", got[0].Ratio)
	}
}

func TestAssess_SubThresholdNotFlagged(t *testing.T) {
	qs := []corev1.ResourceQuota{quota("shop", "compute",
		map[corev1.ResourceName]string{"pods": "50"},
		map[corev1.ResourceName]string{"pods": "40"})} // 0.80

	if got := Assess(qs, 0.90); len(got) != 0 {
		t.Errorf("want no issue at 0.80 < 0.90, got %+v", got)
	}
}

func TestAssess_ZeroHardSkipped(t *testing.T) {
	qs := []corev1.ResourceQuota{quota("shop", "no-pods",
		map[corev1.ResourceName]string{"pods": "0"},
		map[corev1.ResourceName]string{"pods": "0"})}

	if got := Assess(qs, 0.90); len(got) != 0 {
		t.Errorf("want no issue for hard=0 (no div-by-zero), got %+v", got)
	}
}

func TestAssess_GenericResources(t *testing.T) {
	qs := []corev1.ResourceQuota{quota("shop", "compute",
		map[corev1.ResourceName]string{"requests.cpu": "4", "count/configmaps": "10"},
		map[corev1.ResourceName]string{"requests.cpu": "3800m", "count/configmaps": "9"})}

	got := Assess(qs, 0.90)

	if len(got) != 2 {
		t.Fatalf("want 2 issues (cpu + configmaps), got %+v", got)
	}
	cpu, ok := findIssue(got, "requests.cpu")
	if !ok || cpu.Used != "3800m" || cpu.Hard != "4" || !approxEq(cpu.Ratio, 0.95) {
		t.Errorf("requests.cpu issue = %+v, want used 3800m/hard 4 ~0.95", cpu)
	}
	cm, ok := findIssue(got, "count/configmaps")
	if !ok || cm.Used != "9" || cm.Hard != "10" || !approxEq(cm.Ratio, 0.90) {
		t.Errorf("count/configmaps issue = %+v, want used 9/hard 10 ~0.90", cm)
	}
}

func TestAssess_SortPrecedence(t *testing.T) {
	qs := []corev1.ResourceQuota{
		quota("b-ns", "q", map[corev1.ResourceName]string{"pods": "100"}, map[corev1.ResourceName]string{"pods": "95"}),  // near 0.95
		quota("a-ns", "q", map[corev1.ResourceName]string{"pods": "100"}, map[corev1.ResourceName]string{"pods": "92"}),  // near 0.92
		quota("z-ns", "q", map[corev1.ResourceName]string{"pods": "10"}, map[corev1.ResourceName]string{"pods": "10"}),   // exhausted 1.0
	}

	got := Assess(qs, 0.90)

	if len(got) != 3 {
		t.Fatalf("want 3 issues, got %+v", got)
	}
	if got[0].Severity != "exhausted" || got[0].Namespace != "z-ns" {
		t.Errorf("issue[0] = %+v, want exhausted z-ns first", got[0])
	}
	if got[1].Namespace != "b-ns" || !approxEq(got[1].Ratio, 0.95) {
		t.Errorf("issue[1] = %+v, want near 0.95 (b-ns) before 0.92", got[1])
	}
	if got[2].Namespace != "a-ns" || !approxEq(got[2].Ratio, 0.92) {
		t.Errorf("issue[2] = %+v, want near 0.92 (a-ns) last", got[2])
	}
}

func TestAssess_EmptyReturnsNonNil(t *testing.T) {
	got := Assess(nil, 0.90)
	if got == nil {
		t.Error("want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %+v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/quotahealth/ 2>&1 | head`
Expected: compile failure — `undefined: Assess` / `undefined: Issue`.

- [ ] **Step 3: Write the implementation**

Create `internal/quotahealth/quotahealth.go`:

```go
// Package quotahealth flags ResourceQuota entries whose used/hard ratio is at or
// over a threshold — a namespace near or past a quota limit, before it silently
// blocks object creation. Pure and read-only: the caller supplies the quotas and
// the threshold. Mirrors pdbhealth/hpahealth.
package quotahealth

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// Issue is one ResourceQuota entry at or over the usage threshold.
type Issue struct {
	Namespace string  `json:"namespace"`
	Quota     string  `json:"quota"`    // ResourceQuota object name
	Resource  string  `json:"resource"` // e.g. "pods", "requests.cpu"
	Used      string  `json:"used"`     // Quantity.String()
	Hard      string  `json:"hard"`     // Quantity.String()
	Ratio     float64 `json:"ratio"`
	Severity  string  `json:"severity"` // "exhausted" | "near"
}

// Assess flags each ResourceQuota status.hard entry whose used/hard ratio is
// >= threshold. Entries with hard <= 0 are skipped (a deliberate zero quota, and
// it avoids a divide-by-zero). Output is sorted exhausted-first, then by ratio
// descending, then by (Namespace, Quota, Resource).
func Assess(quotas []corev1.ResourceQuota, threshold float64) []Issue {
	issues := []Issue{}
	for _, q := range quotas {
		for name, hard := range q.Status.Hard {
			hf := hard.AsApproximateFloat64()
			if hf <= 0 {
				continue
			}
			used := q.Status.Used[name]
			ratio := used.AsApproximateFloat64() / hf
			if ratio < threshold {
				continue
			}
			sev := "near"
			if ratio >= 1.0 {
				sev = "exhausted"
			}
			issues = append(issues, Issue{
				Namespace: q.Namespace,
				Quota:     q.Name,
				Resource:  string(name),
				Used:      used.String(),
				Hard:      hard.String(),
				Ratio:     ratio,
				Severity:  sev,
			})
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if ra, rb := sevRank(a.Severity), sevRank(b.Severity); ra != rb {
			return ra < rb
		}
		if a.Ratio != b.Ratio {
			return a.Ratio > b.Ratio
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Quota != b.Quota {
			return a.Quota < b.Quota
		}
		return a.Resource < b.Resource
	})
	return issues
}

func sevRank(s string) int {
	if s == "exhausted" {
		return 0
	}
	return 1
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/quotahealth/ -v 2>&1 | tail -20`
Expected: all tests PASS.

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/quotahealth/
git add internal/quotahealth/
git commit -m "feat(quotahealth): flag ResourceQuota entries at or over the usage threshold"
```

---

### Task 2: `collect.ResourceQuotas` collector + RBAC grant

**Files:**
- Modify: `internal/collect/collect.go` (add the collector next to `PodDisruptionBudgets`, ≈ line 230)
- Test: `internal/collect/collect_test.go` (add `TestResourceQuotas`)
- Modify: `deploy/rbac.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml` (grant `resourcequotas`)

**Interfaces:**
- Produces: `func ResourceQuotas(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.ResourceQuota, error)`. Used by Task 3.

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go` (the imports `corev1`, `metav1`, `fake`, `context`, `testing` already exist there):

```go
func TestResourceQuotas(t *testing.T) {
	q1 := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "compute"}}
	q2 := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "objects"}}
	other := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "compute"}}
	client := fake.NewSimpleClientset(q1, q2, other)

	got, err := ResourceQuotas(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 quotas in shop, got %d", len(got))
	}

	all, err := ResourceQuotas(context.Background(), client, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 quotas across all namespaces, got %d", len(all))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -run TestResourceQuotas 2>&1 | head`
Expected: compile failure — `undefined: ResourceQuotas`.

- [ ] **Step 3: Add the collector**

In `internal/collect/collect.go`, add next to `PodDisruptionBudgets`:

```go
// ResourceQuotas lists ResourceQuotas in the namespace (empty = all namespaces),
// read-only. Used by the ResourceQuota near-exhaustion check. Needs the core-group
// resourcequotas list grant; a forbidden/absent result simply omits the check.
func ResourceQuotas(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.ResourceQuota, error) {
	list, err := client.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing resourcequotas: %w", err)
	}
	return list.Items, nil
}
```

- [ ] **Step 4: Grant `resourcequotas` in RBAC**

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, find the core-group rule
(line ≈ 11):

```yaml
    resources: [pods, nodes, services, configmaps, events, persistentvolumeclaims, persistentvolumes, namespaces]
```

and add `resourcequotas`:

```yaml
    resources: [pods, nodes, services, configmaps, events, persistentvolumeclaims, persistentvolumes, namespaces, resourcequotas]
```

Make the identical edit to the matching core-group rule in `deploy/rbac.yaml` (find the
rule with `apiGroups: [""]` whose `resources:` list contains `pods, nodes, services, …, namespaces`, and append `resourcequotas`).

- [ ] **Step 5: Run the collector test + verify the chart still templates**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
go test ./internal/collect/ 2>&1 | tail -3
helm template x deploy/helm/kubeagent | grep -A2 'resources:.*pods, nodes' | grep -q resourcequotas && echo "grant present in rendered chart"
helm lint deploy/helm/kubeagent 2>&1 | tail -2
```
Expected: collect tests PASS; the rendered ClusterRole shows `resourcequotas`; lint reports 0 failures.

- [ ] **Step 6: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go deploy/rbac.yaml deploy/helm/kubeagent/templates/clusterrole.yaml
git commit -m "feat(collect): list ResourceQuotas + grant resourcequotas in RBAC"
```

---

### Task 3: Wire into `scan.Evaluate`

**Files:**
- Modify: `internal/scan/scan.go` (import, `Options.QuotaThreshold`, collect + assess, `Result.QuotaIssues`)
- Test: `internal/scan/scan_test.go` (add `TestEvaluate_FlagsNearFullQuota`)

**Interfaces:**
- Consumes: `collect.ResourceQuotas` (Task 2), `quotahealth.Assess` + `quotahealth.Issue` (Task 1).
- Produces: `Options.QuotaThreshold float64`; `Result.QuotaIssues []quotahealth.Issue`. Used by Tasks 4, 5, 6.

- [ ] **Step 1: Write the failing integration test**

Add to `internal/scan/scan_test.go` (helpers `fake`, `corev1`, `metav1`, `context` already present):

```go
func TestEvaluate_FlagsNearFullQuota(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	rq := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "compute"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{"pods": resource.MustParse("50")},
			Used: corev1.ResourceList{"pods": resource.MustParse("47")},
		},
	}
	cli := fake.NewSimpleClientset(node, rq)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop", QuotaThreshold: 0.90})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.QuotaIssues) != 1 || res.QuotaIssues[0].Severity != "near" || res.QuotaIssues[0].Resource != "pods" {
		t.Errorf("want one near pods quota issue, got %+v", res.QuotaIssues)
	}
}
```

(Ensure `"k8s.io/apimachinery/pkg/api/resource"` is imported in `scan_test.go`; add it to the import block if absent.)

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ -run TestEvaluate_FlagsNearFullQuota 2>&1 | head`
Expected: compile failure — `res.QuotaIssues` undefined and `Options.QuotaThreshold` undefined.

- [ ] **Step 3: Wire the collector + assessor**

In `internal/scan/scan.go`:

1. Add the import (alphabetical — `quotahealth` sorts between `pvcreclaim` and `remediate`/`report`; place it after `pvcreclaim`):

```go
	"github.com/imantaba/kubeagent/internal/quotahealth"
```

2. Add a field to the `Options` struct (next to `DiskThreshold`):

```go
	QuotaThreshold float64
```

3. Add a field to the `Result` struct (next to `WebhookIssues`):

```go
	QuotaIssues []quotahealth.Issue
```

4. In `Evaluate`, after the existing PVC assessment (near the other Assess calls, ≈ line 220), collect and assess — clamp the threshold defensively so a zero/unset or out-of-range value falls back to 0.90:

```go
	quotaThreshold := opts.QuotaThreshold
	if quotaThreshold <= 0 || quotaThreshold > 1 {
		quotaThreshold = 0.90
	}
	quotas, _ := collect.ResourceQuotas(ctx, client, opts.Namespace)
	quotaIssues := quotahealth.Assess(quotas, quotaThreshold)
```

5. Add `QuotaIssues: quotaIssues` to the returned `Result{…}` literal (next to `WebhookIssues: webhookIssues`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ 2>&1 | tail -5`
Expected: PASS (the new test and all existing scan tests).

- [ ] **Step 5: Commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/scan/
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): collect ResourceQuotas and assess near-exhaustion"
```

---

### Task 4: Report rendering

**Files:**
- Modify: `internal/report/report.go` (`Input.QuotaIssues`, JSON `inventoryReport` field, `printQuotaIssues`, call site, `hasAttention`, summary counter)
- Test: `internal/report/report_test.go` (add `TestPrintQuotaIssues`)

**Interfaces:**
- Consumes: `quotahealth.Issue` (Task 1).
- Produces: `report.Input.QuotaIssues []quotahealth.Issue` (set by Task 5's `resultInput`); the rendered `RESOURCE QUOTA` rows.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go`:

```go
func TestPrintQuotaIssues(t *testing.T) {
	in := Input{
		Result: inventory.Result{}, // no workloads
		QuotaIssues: []quotahealth.Issue{
			{Namespace: "shop", Quota: "compute", Resource: "requests.cpu", Used: "4", Hard: "4", Ratio: 1.0, Severity: "exhausted"},
			{Namespace: "web", Quota: "compute", Resource: "pods", Used: "47", Hard: "50", Ratio: 0.94, Severity: "near"},
		},
	}
	var buf bytes.Buffer
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "✗ shop/compute  ResourceQuota  requests.cpu") {
		t.Errorf("missing exhausted quota row:\n%s", out)
	}
	if !strings.Contains(out, "⚠ QuotaExhausted: used 4 / hard 4 (100%)") {
		t.Errorf("missing exhausted detail:\n%s", out)
	}
	if !strings.Contains(out, "✗ web/compute  ResourceQuota  pods") {
		t.Errorf("missing near quota row:\n%s", out)
	}
	if !strings.Contains(out, "⚠ QuotaNearLimit: used 47 / hard 50 (94%)") {
		t.Errorf("missing near detail:\n%s", out)
	}

	// Empty QuotaIssues renders no ResourceQuota rows.
	var buf2 bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}}, "text", &buf2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf2.String(), "ResourceQuota") {
		t.Errorf("empty QuotaIssues should render nothing, got:\n%s", buf2.String())
	}
}
```

(The render entry point is the exported `PrintInventory(Input, "text", w)`, matching the existing `TestPrintInventory_*` tests. `bytes`, `strings`, and `inventory` are already imported in `report_test.go`; add `"github.com/imantaba/kubeagent/internal/quotahealth"` to its imports.)

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestPrintQuotaIssues 2>&1 | head`
Expected: FAIL — `Input` has no `QuotaIssues` field / rows absent.

- [ ] **Step 3: Add the field, renderer, and wiring**

In `internal/report/report.go`:

1. Add the import `"github.com/imantaba/kubeagent/internal/quotahealth"` (alphabetical).

2. Add to the JSON `inventoryReport` struct (next to `PDBIssues`/`HPAIssues`, ≈ line 50):

```go
	QuotaIssues        []quotahealth.Issue        `json:"quotaIssues,omitempty"`
```

3. Add to the `Input` struct (next to `PDBIssues`/`HPAIssues`, ≈ line 77):

```go
	QuotaIssues        []quotahealth.Issue
```

4. Add to the JSON-build literal that copies `Input` → `inventoryReport` (next to `PDBIssues: in.PDBIssues`, ≈ line 106):

```go
			QuotaIssues:        in.QuotaIssues,
```

5. Add the renderer next to `printPDBIssues`:

```go
// printQuotaIssues lists ResourceQuota entries at or over the usage threshold.
func printQuotaIssues(issues []quotahealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  ResourceQuota  %s\n", is.Namespace, is.Quota, is.Resource); err != nil {
			return err
		}
		label := "QuotaNearLimit"
		if is.Severity == "exhausted" {
			label = "QuotaExhausted"
		}
		pct := int(is.Ratio*100 + 0.5)
		if _, err := fmt.Fprintf(w, "      ⚠ %s: used %s / hard %s (%d%%)\n", label, is.Used, is.Hard, pct); err != nil {
			return err
		}
	}
	return nil
}
```

6. Add the call site in the NEEDS ATTENTION block, immediately after the `printWebhookIssues(in.WebhookIssues, w)` call (≈ line 170):

```go
		if err := printQuotaIssues(in.QuotaIssues, w); err != nil {
			return err
		}
```

7. Add `len(in.QuotaIssues) > 0` to the `hasAttention` expression (≈ line 137), appended to the existing OR chain:

```go
 || len(in.WebhookIssues) > 0 || len(in.QuotaIssues) > 0
```

8. Add a summary-counter clause after the `WebhookIssues` clause (≈ line 330):

```go
	if n := len(in.QuotaIssues); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s near/over quota", n, plural(n, "ResourceQuota", "ResourceQuotas")))
	}
```

- [ ] **Step 4: Run the report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ 2>&1 | tail -5`
Expected: PASS, including the unchanged `TestGoldenScanOutput` (default fixture has no quota issues, so the golden snapshot is unaffected).

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/report/
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render the RESOURCE QUOTA near-exhaustion block"
```

---

### Task 5: `main.go` seam + threshold env

**Files:**
- Modify: `main.go` (`resultInput` maps `QuotaIssues`; `QuotaThreshold` from `KUBEAGENT_QUOTA_THRESHOLD` in both Options construction sites)
- Test: `main_test.go` (add `TestResultInput_MapsQuotaIssues`)

**Interfaces:**
- Consumes: `scan.Result.QuotaIssues` (Task 3), `report.Input.QuotaIssues` (Task 4), `scan.Options.QuotaThreshold` (Task 3).
- Produces: end-to-end rendering of quota issues from a live scan.

- [ ] **Step 1: Write the failing test**

Add to `main_test.go`:

```go
func TestResultInput_MapsQuotaIssues(t *testing.T) {
	res := scan.Result{QuotaIssues: []quotahealth.Issue{
		{Namespace: "shop", Quota: "compute", Resource: "pods", Used: "47", Hard: "50", Ratio: 0.94, Severity: "near"},
	}}
	in := resultInput(res)
	if len(in.QuotaIssues) != 1 || in.QuotaIssues[0].Resource != "pods" {
		t.Errorf("resultInput dropped QuotaIssues: got %+v", in.QuotaIssues)
	}
}
```

(Add `"github.com/imantaba/kubeagent/internal/quotahealth"` and, if not present, `"github.com/imantaba/kubeagent/internal/scan"` to `main_test.go` imports.)

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run TestResultInput_MapsQuotaIssues 2>&1 | head`
Expected: FAIL — `resultInput` does not copy `QuotaIssues` (the field is absent from the returned `Input`).

- [ ] **Step 3: Map the field + wire the env threshold**

In `main.go`:

1. In `resultInput` (≈ line 205), add to the returned `report.Input{…}` literal next to `WebhookIssues: res.WebhookIssues`:

```go
		QuotaIssues:      res.QuotaIssues,
```

2. In BOTH `scan.Options{…}` construction sites (the CLI-flag path ≈ line 113 and the env-driven path ≈ line 253), set the threshold from the env var:

```go
		QuotaThreshold:         envFloat("KUBEAGENT_QUOTA_THRESHOLD", 0.90),
```

(The `envFloat` helper already exists — it is used for `KUBEAGENT_DISK_THRESHOLD`. `scan.Evaluate` clamps an out-of-range value back to 0.90, so no extra validation is needed here.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . 2>&1 | tail -5`
Expected: PASS (the new seam test and all existing `main` tests).

- [ ] **Step 5: Commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w main.go
git add main.go main_test.go
git commit -m "feat(main): map QuotaIssues through resultInput; KUBEAGENT_QUOTA_THRESHOLD env"
```

---

### Task 6: `watch` daemon gauge

**Files:**
- Modify: `internal/watch/metrics.go` (a `quotaIssues` field, its snapshot assignment, and the gauge line)
- Test: `internal/watch/metrics_test.go` (extend the render assertion)

**Interfaces:**
- Consumes: `scan.Result.QuotaIssues` (Task 3).
- Produces: the `kubeagent_resourcequota_issues` gauge.

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, in `TestMetrics_RenderReflectsResult`, add `kubeagent_resourcequota_issues 1` to the set of expected metric substrings, and ensure the test `scan.Result` fixture it builds includes one quota issue. Add to that fixture's `Result`:

```go
		QuotaIssues: []quotahealth.Issue{{Namespace: "shop", Quota: "compute", Resource: "pods", Severity: "near"}},
```

and add `"kubeagent_resourcequota_issues 1"` to the asserted-substrings slice. (Import `"github.com/imantaba/kubeagent/internal/quotahealth"` in the test file.)

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult 2>&1 | tail`
Expected: FAIL — the rendered metrics do not contain `kubeagent_resourcequota_issues 1`.

- [ ] **Step 3: Add the gauge**

In `internal/watch/metrics.go`:

1. Add a struct field next to `webhooksFailing` (in the metrics struct definition):

```go
	quotaIssues int
```

2. In the snapshot function, next to `m.webhooksFailing = len(res.WebhookIssues)` (≈ line 108):

```go
	m.quotaIssues = len(res.QuotaIssues)
```

3. In the render function, next to the `kubeagent_admission_webhooks_failing` gauge line (≈ line 168):

```go
	gauge("kubeagent_resourcequota_issues", "ResourceQuota entries at or over the usage threshold", float64(m.quotaIssues))
```

- [ ] **Step 4: Run the watch suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/watch/
git add internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): expose kubeagent_resourcequota_issues gauge"
```

---

### Task 7: Golden snapshot — a ResourceQuota fixture issue

**Files:**
- Modify: `internal/report/golden_test.go` (add a `QuotaIssues` entry to the pre-built fixture Input)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated via `-update`)

**Interfaces:**
- Consumes: the golden fixture `Input` builder and `quotahealth.Issue` (Task 1); the `printQuotaIssues` renderer (Task 4).

- [ ] **Step 1: Add a fixture quota issue**

In `internal/report/golden_test.go`, locate where the fixture `Input` is assembled (the struct that sets `PDBIssues`, `HPAIssues`, `WebhookIssues`, etc. — search for `WebhookIssues:` in that file). Add a `QuotaIssues` field to that same `Input` literal:

```go
		QuotaIssues: []quotahealth.Issue{
			{Namespace: "shop", Quota: "compute", Resource: "requests.cpu", Used: "4", Hard: "4", Ratio: 1.0, Severity: "exhausted"},
		},
```

(Import `"github.com/imantaba/kubeagent/internal/quotahealth"` in `golden_test.go` if not already present.)

- [ ] **Step 2: Run the golden test to verify it fails (snapshot mismatch)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestGoldenScanOutput 2>&1 | tail`
Expected: FAIL — the rendered output now includes a `✗ shop/compute  ResourceQuota  requests.cpu` block absent from the committed snapshot, plus the summary counter clause.

- [ ] **Step 3: Regenerate the snapshot**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput -update`
Expected: exit 0; `testdata/golden-scan.txt` now shows the `✗ shop/compute  ResourceQuota  requests.cpu` row with `⚠ QuotaExhausted: used 4 / hard 4 (100%)`, and the `Needs attention:` summary line gains a `1 ResourceQuota near/over quota` clause.

- [ ] **Step 4: Run the full report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ 2>&1 | tail -3`
Expected: PASS (`TestGoldenScanOutput` and `TestGoldenInputCoversAllSections`).

- [ ] **Step 5: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover a ResourceQuota issue in the golden scan snapshot"
```

---

### Task 8: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs).

- [ ] **Step 1: Update the docs**

- `website/docs/features/diagnostics.md`: add a section (near the other always-on Assess checks — PDB/HPA/webhook) describing the ResourceQuota near-exhaustion check: flags a ResourceQuota entry whose `used/hard` is ≥ 90% (env `KUBEAGENT_QUOTA_THRESHOLD` to tune); `exhausted` (100%, blocking now) vs `near limit`; every quota resource; read-only, always-on, no flag; the daemon gauge `kubeagent_resourcequota_issues`; the proactive complement to `FailedCreate`. Show an example block:

  ```text
  ✗ shop/compute  ResourceQuota  requests.cpu
      ⚠ QuotaExhausted: used 4 / hard 4 (100%)
  ✗ web/compute  ResourceQuota  pods
      ⚠ QuotaNearLimit: used 47 / hard 50 (94%)
  ```

  (`↳`/`⚠`/`✗` glyphs as elsewhere; follow the neighboring always-on-check style; do not restructure.)

- `README.md`: add the ResourceQuota near-exhaustion check to the always-on DETECTOR/check list (not the flags list); mention the `KUBEAGENT_QUOTA_THRESHOLD` env override.

- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`:

  ```
  - **ResourceQuota near-exhaustion.** `scan` flags a namespace's ResourceQuota
    entry whose usage is at or over 90% of its hard limit (env
    `KUBEAGENT_QUOTA_THRESHOLD` to tune), labelled `exhausted` (blocking new
    objects now) or `near limit` — the proactive complement to the reactive
    `FailedCreate` detector. Read-only, always-on; the daemon exposes
    `kubeagent_resourcequota_issues`. Adds a `resourcequotas` read grant.
  ```

- `website/docs/roadmap.md`: add a Shipped bullet after the `RolloutStuck` entry, tagged **Theme-B** (deeper diagnosis), noting it's the proactive early-warning half of quota diagnosis and links to `features/diagnostics.md`.

- [ ] **Step 2: Verify the docs build**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (venv fallback: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, "Documentation built", no page WARNINGs.

- [ ] **Step 3: Run the whole suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add website/ README.md CHANGELOG.md
git commit -m "docs: document the ResourceQuota near-exhaustion check"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the `release` skill owns this. This feature touches `internal/collect` (new collector), the **RBAC** manifests + Helm `clusterrole.yaml` **template**, and `internal/watch` → **FULL CHAOS GATE** (`./chaos/run.sh --recreate`). **Minor** bump **v0.43.0 → v0.44.0**; **chart MINOR** bump — the clusterrole template changed, so override the bump script's default patch and edit `deploy/helm/kubeagent/Chart.yaml` `version:` to the next minor (the script prints the exact value). Hold for the user's explicit "run release and push".
```
