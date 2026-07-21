# Stuck-Terminating / Finalizer-Deadlock Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag resources wedged in Terminating past a 2-minute threshold — a Namespace stuck on a finalizer/condition, a Pod stuck past grace, a PVC held by `pvc-protection` — naming the actual blocker, in an always-on read-only check.

**Architecture:** A pure `internal/termhealth.Assess(namespaces, pods, pvcs, threshold, now)` returns `[]Issue`; a new `collect.Namespaces` feeds it (pods + PVCs already collected). `scan.Evaluate` wires it (graceful if namespaces list is forbidden) into `Result.StuckTerminating`; the report renders a NEEDS ATTENTION section, the watch daemon a gauge; the base RBAC gains a `namespaces` read grant. Mirrors the Pending-PVC (`pvchealth`) check end to end.

**Tech Stack:** Go 1.26 standard library + `k8s.io/api`. No new dependencies.

## Global Constraints

- **Read-only; always-on; no flag.** One new base RBAC grant (`namespaces` list).
- **Pure & deterministic** — `termhealth.Assess` takes injected `now` + threshold; sorted output; no `time.Now`/`rand`.
- **Advisory to the verdict** — stuck-terminating never flips Healthy/Degraded (consistent with the PVC/service checks). Do not touch `clusterhealth`.
- **Graceful degradation:** a forbidden/failed `namespaces` list must NOT fail the scan — the pod/PVC checks still run.
- `inventory`, `clusterhealth`, `explain.go`, `--fix` stay **unchanged**. No auto-finalizer-removal.
- Threshold is a **fixed `2 * time.Minute`** passed from `scan` — no CLI flag.
- **No `Co-Authored-By: Claude` trailer** (or any AI attribution) on any commit. **TDD.**
- Glyphs `✗` / `⚠` / `—` (U+2014) already in report.go — reuse, never substitute ASCII.
- Set PATH first every task: `export PATH=$PATH:/usr/local/go/bin`.
- Release gate (controller-owned, post-merge): touches `internal/collect` + RBAC + Helm → **FULL CHAOS GATE**; **minor** bump v0.33.0 → **v0.34.0**.
- Spec: [docs/superpowers/specs/2026-07-21-stuck-terminating-design.md](../specs/2026-07-21-stuck-terminating-design.md).

---

## File Structure

- **Create** `internal/termhealth/termhealth.go` (+ test) — `Issue`, `Assess`, helpers.
- **Modify** `internal/collect/collect.go` (+ test) — `Namespaces`.
- **Modify** `internal/scan/scan.go` (+ test) — wiring + `Result.StuckTerminating`.
- **Modify** `internal/report/report.go` (+ test) — `Input.StuckTerminating`, JSON, `printStuckTerminating`, `hasAttention`, `attentionLine`.
- **Modify** `internal/watch/metrics.go` (+ test) — the gauge.
- **Modify** `deploy/rbac.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml` — `namespaces`.
- **Modify** `internal/report/golden_test.go` + `testdata/golden-scan.txt`; docs (`diagnostics.md`, `watch-mode.md`, `README.md`, `CHANGELOG.md`, `roadmap.md`).

---

### Task 1: `internal/termhealth` — pure assessment

**Files:**
- Create: `internal/termhealth/termhealth.go`
- Test: `internal/termhealth/termhealth_test.go`

**Interfaces:**
- Produces (later tasks depend on these exactly):

```go
type Issue struct {
	Kind      string `json:"kind"`                // "Namespace" | "Pod" | "PersistentVolumeClaim"
	Namespace string `json:"namespace,omitempty"` // empty for cluster-scoped Namespace
	Name      string `json:"name"`
	Age       string `json:"age"`                 // compact time since deletionTimestamp, e.g. "3h", "8m"
	PastGrace bool   `json:"pastGrace,omitempty"` // pods only
	Reason    string `json:"reason"`
}

func Assess(namespaces []corev1.Namespace, pods []corev1.Pod, pvcs []corev1.PersistentVolumeClaim, threshold time.Duration, now time.Time) []Issue
```

- [ ] **Step 1: Write the failing tests**

Create `internal/termhealth/termhealth_test.go`:

```go
package termhealth

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var now = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

func delTime(ago time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(-ago)); return &t }

func TestAssess_StuckNamespaceNamesCondition(t *testing.T) {
	ns := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-ns", DeletionTimestamp: delTime(3 * time.Hour)},
		Status: corev1.NamespaceStatus{Conditions: []corev1.NamespaceCondition{
			{Type: "NamespaceFinalizersRemaining", Status: corev1.ConditionTrue, Message: "Some content in the namespace has finalizers remaining: kubernetes."}}},
	}
	got := Assess([]corev1.Namespace{ns}, nil, nil, 2*time.Minute, now)
	if len(got) != 1 || got[0].Kind != "Namespace" || got[0].Namespace != "" || got[0].Name != "legacy-ns" {
		t.Fatalf("want one Namespace issue, got %+v", got)
	}
	if got[0].Age != "3h" {
		t.Errorf("Age = %q, want 3h", got[0].Age)
	}
	if !contains(got[0].Reason, "NamespaceFinalizersRemaining") || !contains(got[0].Reason, "finalizers remaining") {
		t.Errorf("Reason = %q, want it to name the condition", got[0].Reason)
	}
}

func TestAssess_PodPastGraceWithFinalizer(t *testing.T) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-7c9d5",
		DeletionTimestamp: delTime(8 * time.Minute), Finalizers: []string{"example.com/cleanup-hook"}}}
	got := Assess(nil, []corev1.Pod{pod}, nil, 2*time.Minute, now)
	if len(got) != 1 || got[0].Kind != "Pod" || !got[0].PastGrace {
		t.Fatalf("want one PastGrace Pod issue, got %+v", got)
	}
	if got[0].Namespace != "shop" || got[0].Age != "8m" || !contains(got[0].Reason, "example.com/cleanup-hook") {
		t.Errorf("issue = %+v", got[0])
	}
}

func TestAssess_PodPastGraceNoFinalizer(t *testing.T) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "orphan", DeletionTimestamp: delTime(10 * time.Minute)}}
	got := Assess(nil, []corev1.Pod{pod}, nil, 2*time.Minute, now)
	if len(got) != 1 || !contains(got[0].Reason, "deletion not confirmed") {
		t.Fatalf("want the node/kubelet reason, got %+v", got)
	}
}

func TestAssess_PVCProtectionNamesMountingPod(t *testing.T) {
	pvc := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data",
		DeletionTimestamp: delTime(20 * time.Minute), Finalizers: []string{"kubernetes.io/pvc-protection"}}}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-0"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}}}
	got := Assess(nil, []corev1.Pod{pod}, []corev1.PersistentVolumeClaim{pvc}, 2*time.Minute, now)
	if len(got) != 1 || got[0].Kind != "PersistentVolumeClaim" || !contains(got[0].Reason, "still mounted by pod shop/db-0") {
		t.Fatalf("want the mounting-pod reason, got %+v", got)
	}
}

func TestAssess_BelowThresholdAndNoDeletionSkipped(t *testing.T) {
	recent := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "recent", DeletionTimestamp: delTime(30 * time.Second)}}
	alive := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "alive"}}
	if got := Assess(nil, []corev1.Pod{recent, alive}, nil, 2*time.Minute, now); len(got) != 0 {
		t.Errorf("a <threshold deletion and a non-deleting pod must not be flagged, got %+v", got)
	}
}

func TestAssess_SortedByKindNamespaceName(t *testing.T) {
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "z-ns", DeletionTimestamp: delTime(1 * time.Hour)}}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p", DeletionTimestamp: delTime(1 * time.Hour)}}
	got := Assess([]corev1.Namespace{ns}, []corev1.Pod{pod}, nil, 2*time.Minute, now)
	if len(got) != 2 || got[0].Kind != "Namespace" || got[1].Kind != "Pod" {
		t.Errorf("want Namespace before Pod (sorted by Kind), got %+v", got)
	}
}

func contains(s, sub string) bool { return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/termhealth`
Expected: FAIL — build error (package undefined).

- [ ] **Step 3: Implement**

Create `internal/termhealth/termhealth.go`:

```go
// Package termhealth flags resources wedged in Terminating — a Namespace stuck on
// a finalizer or a downstream condition, a Pod stuck past its grace period, a PVC
// held by pvc-protection — and names the blocker. Pure and read-only: the caller
// supplies the namespaces, pods, PVCs, threshold, and clock. Advisory (never
// affects the cluster verdict).
package termhealth

import (
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Issue is one resource stuck Terminating past the threshold.
type Issue struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Age       string `json:"age"`
	PastGrace bool   `json:"pastGrace,omitempty"`
	Reason    string `json:"reason"`
}

// nsConditionOrder lists the blocking namespace conditions in the order to report.
var nsConditionOrder = []corev1.NamespaceConditionType{
	"NamespaceDeletionContentFailure", "NamespaceContentRemaining", "NamespaceFinalizersRemaining",
}

// Assess flags every resource whose deletion has been pending longer than
// threshold, sorted by (Kind, Namespace, Name).
func Assess(namespaces []corev1.Namespace, pods []corev1.Pod, pvcs []corev1.PersistentVolumeClaim, threshold time.Duration, now time.Time) []Issue {
	var out []Issue
	for _, ns := range namespaces {
		if age, ok := stuckFor(ns.DeletionTimestamp, threshold, now); ok {
			out = append(out, Issue{Kind: "Namespace", Name: ns.Name, Age: age, Reason: nsReason(ns)})
		}
	}
	for _, p := range pods {
		if age, ok := stuckFor(p.DeletionTimestamp, threshold, now); ok {
			out = append(out, Issue{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, Age: age, PastGrace: true, Reason: podReason(p)})
		}
	}
	for _, c := range pvcs {
		if age, ok := stuckFor(c.DeletionTimestamp, threshold, now); ok {
			out = append(out, Issue{Kind: "PersistentVolumeClaim", Namespace: c.Namespace, Name: c.Name, Age: age, Reason: pvcReason(c, pods)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// stuckFor reports the compact age and whether dt is set and older than threshold.
func stuckFor(dt *metav1.Time, threshold time.Duration, now time.Time) (string, bool) {
	if dt == nil {
		return "", false
	}
	d := now.Sub(dt.Time)
	if d <= threshold {
		return "", false
	}
	return compactDur(d), true
}

// compactDur renders a duration as the largest whole unit: Nd / Nh / Nm (min "1m").
func compactDur(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d >= time.Minute:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return "1m"
	}
}

func podReason(p corev1.Pod) string {
	if len(p.Finalizers) > 0 {
		return "finalizer " + strings.Join(p.Finalizers, ", ")
	}
	return "deletion not confirmed (node gone or kubelet not reporting)"
}

func pvcReason(c corev1.PersistentVolumeClaim, pods []corev1.Pod) string {
	hasProtection := false
	for _, f := range c.Finalizers {
		if f == "kubernetes.io/pvc-protection" {
			hasProtection = true
		}
	}
	if hasProtection {
		if mp := mountingPod(c, pods); mp != "" {
			return "pvc-protection — still mounted by pod " + mp
		}
		return "pvc-protection"
	}
	if len(c.Finalizers) > 0 {
		return "finalizer " + strings.Join(c.Finalizers, ", ")
	}
	return "deletion pending"
}

// mountingPod returns "ns/name" of the first same-namespace pod mounting the PVC.
func mountingPod(c corev1.PersistentVolumeClaim, pods []corev1.Pod) string {
	for _, p := range pods {
		if p.Namespace != c.Namespace {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == c.Name {
				return p.Namespace + "/" + p.Name
			}
		}
	}
	return ""
}

func nsReason(ns corev1.Namespace) string {
	byType := map[corev1.NamespaceConditionType]corev1.NamespaceCondition{}
	for _, c := range ns.Status.Conditions {
		byType[c.Type] = c
	}
	for _, t := range nsConditionOrder {
		if c, ok := byType[t]; ok {
			return string(t) + " — " + trimMsg(c.Message)
		}
	}
	if len(ns.Spec.Finalizers) > 0 {
		fs := make([]string, len(ns.Spec.Finalizers))
		for i, f := range ns.Spec.Finalizers {
			fs[i] = string(f)
		}
		return "finalizers " + strings.Join(fs, ", ")
	}
	return "deletion pending"
}

func trimMsg(s string) string { return strings.TrimRight(strings.TrimSpace(s), ".") }
```

(The imports and types above are complete — `metav1.Time`, `strconv`, `corev1` are all imported as shown; nothing to substitute.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/termhealth -v && go vet ./internal/termhealth`
Expected: PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/termhealth/termhealth.go internal/termhealth/termhealth_test.go
git commit -m "feat(termhealth): flag resources stuck terminating and name the blocker"
```

---

### Task 2: `collect.Namespaces`

**Files:**
- Modify: `internal/collect/collect.go`
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Produces: `func Namespaces(ctx context.Context, client kubernetes.Interface) ([]corev1.Namespace, error)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go`:

```go
func TestNamespaces(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "legacy-ns"}}
	client := fake.NewSimpleClientset(ns)
	got, err := Namespaces(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "legacy-ns" {
		t.Fatalf("want the seeded namespace, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect -run TestNamespaces`
Expected: FAIL — `undefined: Namespaces`.

- [ ] **Step 3: Implement**

Add to `internal/collect/collect.go`:

```go
// Namespaces lists all namespaces (cluster-scoped; read-only) for the
// stuck-terminating check. Needs the base `namespaces` list grant; a forbidden
// list is handled gracefully by the caller (namespace checks are skipped).
func Namespaces(ctx context.Context, client kubernetes.Interface) ([]corev1.Namespace, error) {
	list, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	return list.Items, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go
git commit -m "feat(collect): list namespaces for the stuck-terminating check"
```

---

### Task 3: `scan.Evaluate` — wiring + `Result.StuckTerminating`

**Files:**
- Modify: `internal/scan/scan.go`
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `collect.Namespaces` (Task 2), `termhealth.Assess` (Task 1), `inputs.Pods`, the `pvcs` local (scan.go:192).
- Produces: `Result.StuckTerminating []termhealth.Issue`.

- [ ] **Step 1: Write the failing integration tests**

Add to `internal/scan/scan_test.go` (imports `runtime`, `schema`, `apierrors`, `k8stesting` already present from the certs feature — reuse; else add):

```go
func TestEvaluate_FlagsStuckTerminatingNamespace(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dt := metav1.NewTime(time.Now().Add(-3 * time.Hour))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "legacy-ns", DeletionTimestamp: &dt},
		Status: corev1.NamespaceStatus{Conditions: []corev1.NamespaceCondition{
			{Type: "NamespaceFinalizersRemaining", Status: corev1.ConditionTrue, Message: "finalizers remaining: kubernetes"}}}}
	cli := fake.NewSimpleClientset(node, ns)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range res.StuckTerminating {
		if is.Kind == "Namespace" && is.Name == "legacy-ns" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a stuck-terminating namespace, got %+v", res.StuckTerminating)
	}
}

func TestEvaluate_ForbiddenNamespacesStillScansPods(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "stuck", DeletionTimestamp: &dt,
		Finalizers: []string{"example.com/hook"}}}
	cli := fake.NewSimpleClientset(node, pod)
	cli.Fake.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", nil)
	})
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("a forbidden namespaces list must not fail the scan: %v", err)
	}
	found := false
	for _, is := range res.StuckTerminating {
		if is.Kind == "Pod" && is.Name == "stuck" {
			found = true
		}
	}
	if !found {
		t.Errorf("pod checks must still run when namespaces is forbidden, got %+v", res.StuckTerminating)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan -run TestEvaluate_FlagsStuckTerminating`
Expected: FAIL — build error (`Result.StuckTerminating` undefined).

- [ ] **Step 3: Implement**

In `internal/scan/scan.go`:
- Add import `"github.com/imantaba/kubeagent/internal/termhealth"`.
- Add `StuckTerminating []termhealth.Issue` to `Result` (after `Certificates`).
- After the `pvcs` local is available (scan.go:192, `pvcs, _ := collect.PersistentVolumeClaims(...)`), add:

```go
	namespaces, _ := collect.Namespaces(ctx, client) // forbidden/absent → nil, namespace checks skipped
	stuckTerminating := termhealth.Assess(namespaces, inputs.Pods, pvcs, 2*time.Minute, time.Now())
```
- Add `StuckTerminating: stuckTerminating,` to the `Result{...}` return literal.

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/scan`
Expected: PASS (both new tests + all existing).

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): assess stuck-terminating resources (graceful without namespaces)"
```

---

### Task 4: `report` — the STUCK-TERMINATING rendering

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `termhealth.Issue` (Task 1).
- Produces: `Input.StuckTerminating`, JSON `stuckTerminating,omitempty`, `printStuckTerminating`, attention-line clause.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go` (import `termhealth`):

```go
func TestPrintInventory_StuckTerminating(t *testing.T) {
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		StuckTerminating: []termhealth.Issue{
			{Kind: "Namespace", Name: "legacy-ns", Age: "3h", Reason: "NamespaceFinalizersRemaining — kubernetes finalizer remains"},
			{Kind: "Pod", Namespace: "shop", Name: "api-7c9d5", Age: "8m", PastGrace: true, Reason: "finalizer example.com/cleanup-hook"},
			{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data", Age: "20m", Reason: "pvc-protection — still mounted by pod shop/db-0"},
		}}
	var buf bytes.Buffer
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"✗ legacy-ns  Namespace  Terminating 3h",
		"⚠ StuckTerminating: NamespaceFinalizersRemaining — kubernetes finalizer remains",
		"✗ shop/api-7c9d5  Pod  Terminating 8m (past grace)",
		"✗ shop/data  PersistentVolumeClaim  Terminating 20m",
		"⚠ StuckTerminating: pvc-protection — still mounted by pod shop/db-0",
		"3 resources stuck terminating",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "legacy-ns  Namespace  Terminating 3h (past grace)") {
		t.Error("(past grace) must appear only on pods")
	}
}

func TestPrintInventory_StuckTerminatingAbsentWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	_ = PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}, "text", &buf)
	if strings.Contains(buf.String(), "StuckTerminating") {
		t.Error("section must be absent when nothing is stuck")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory_StuckTerminating`
Expected: FAIL — build error (`Input.StuckTerminating` undefined).

- [ ] **Step 3: Implement**

In `internal/report/report.go`:
- Add `StuckTerminating []termhealth.Issue` to `Input` (after `KubeletHealth`/`Certificates`) and to the JSON `inventoryReport` struct as `StuckTerminating []termhealth.Issue \`json:"stuckTerminating,omitempty"\`` populated from `in.StuckTerminating`. Import `"github.com/imantaba/kubeagent/internal/termhealth"`.
- In `printInventoryText`, add `len(in.StuckTerminating) > 0` to the `hasAttention` expression, and call `printStuckTerminating(in.StuckTerminating, w)` right after `printPVCIssues(in.PVCIssues, w)` in the NEEDS ATTENTION block.
- Add the printer:

```go
// printStuckTerminating lists resources wedged in Terminating past the threshold.
func printStuckTerminating(issues []termhealth.Issue, w io.Writer) error {
	for _, is := range issues {
		id := is.Name
		if is.Namespace != "" {
			id = is.Namespace + "/" + is.Name
		}
		grace := ""
		if is.PastGrace {
			grace = " (past grace)"
		}
		if _, err := fmt.Fprintf(w, "  ✗ %s  %s  Terminating %s%s\n", id, is.Kind, is.Age, grace); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ StuckTerminating: %s\n", is.Reason); err != nil {
			return err
		}
	}
	return nil
}
```
- In `attentionLine`, after the PVCIssues clause, add:

```go
	if n := len(in.StuckTerminating); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s stuck terminating", n, plural(n, "resource", "resources")))
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory`
Expected: PASS (new + existing; the golden is unaffected — its fixture has no StuckTerminating yet).

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render the STUCK-TERMINATING findings"
```

---

### Task 5: `watch` gauge

**Files:**
- Modify: `internal/watch/metrics.go`
- Test: `internal/watch/metrics_test.go`

**Interfaces:** consumes `Result.StuckTerminating`; produces `kubeagent_resources_stuck_terminating`.

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`: add `termhealth` to imports; add to `sampleResult()`:
```go
		StuckTerminating: []termhealth.Issue{{Kind: "Namespace", Name: "legacy-ns", Age: "3h", Reason: "NamespaceFinalizersRemaining — x"}},
```
and add to the `TestMetrics_RenderReflectsResult` want-list:
```go
		"kubeagent_resources_stuck_terminating 1",
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch -run TestMetrics_RenderReflectsResult`
Expected: FAIL — gauge line missing.

- [ ] **Step 3: Implement**

In `internal/watch/metrics.go`: add a field `stuckTerminating int` to `metrics`; in `update()` success path add `m.stuckTerminating = len(res.StuckTerminating)`; in `render()`, after the `kubeagent_pvc_pending_issues` gauge, add:
```go
	gauge("kubeagent_resources_stuck_terminating", "Resources (namespaces, pods, PVCs) wedged in Terminating past the threshold", float64(m.stuckTerminating))
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): kubeagent_resources_stuck_terminating gauge"
```

---

### Task 6: RBAC + Helm — the `namespaces` grant

**Files:**
- Modify: `deploy/rbac.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml`

- [ ] **Step 1: Add `namespaces` to the core read rule**

In both `deploy/rbac.yaml` (line 13) and `deploy/helm/kubeagent/templates/clusterrole.yaml` (line 11), change the core resources list from:

```yaml
    resources: [pods, nodes, services, configmaps, events, persistentvolumeclaims, persistentvolumes]
```

to:

```yaml
    resources: [pods, nodes, services, configmaps, events, persistentvolumeclaims, persistentvolumes, namespaces]
```

(verbs unchanged — `[get, list, watch]`). This is the ONLY change; the grant is in the base role, not an opt-in add-on.

- [ ] **Step 2: Verify**

Run: `export PATH=$PATH:$HOME/.local/bin:/usr/local/bin && helm lint deploy/helm/kubeagent && helm template x deploy/helm/kubeagent | grep -m1 -A1 'resources: \[pods'`
Expected: lint clean; the printed core rule includes `namespaces`. Also `kubectl --context ekb apply --dry-run=client -f deploy/rbac.yaml` if a cluster is reachable (read-only client validation), else note skipped.

- [ ] **Step 3: Commit**

```bash
git add deploy/rbac.yaml deploy/helm/kubeagent/templates/clusterrole.yaml
git commit -m "feat(deploy): grant namespaces read for the stuck-terminating check"
```

---

### Task 7: Golden + documentation

**Files:**
- Modify: `internal/report/golden_test.go` + `internal/report/testdata/golden-scan.txt`
- Modify: `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

- [ ] **Step 1: Golden fixture** — in `goldenInput`, after `Certificates`, add:

```go
		StuckTerminating: []termhealth.Issue{
			{Kind: "Namespace", Name: "legacy-ns", Age: "3h", Reason: "NamespaceFinalizersRemaining — some content has finalizers remaining: kubernetes"},
			{Kind: "Pod", Namespace: "shop", Name: "api-7c9d5-x2v", Age: "8m", PastGrace: true, Reason: "finalizer example.com/cleanup-hook"}},
```
(import `termhealth`), and extend `TestGoldenInputCoversAllSections`'s guard with `|| len(in.StuckTerminating) == 0`.

- [ ] **Step 2: Regenerate + inspect** — run the golden without `-update` (FAIL, stale), then with `-update` (PASS). Then `grep -n "Terminating\|stuck terminating" internal/report/testdata/golden-scan.txt` — confirm the `✗ legacy-ns  Namespace  Terminating 3h` + its `⚠ StuckTerminating:` line, the `✗ shop/api-7c9d5-x2v  Pod  Terminating 8m (past grace)` block, and the attention line gained `2 resources stuck terminating`. Run `go test ./internal/report` (full PASS).

- [ ] **Step 3: diagnostics.md** — add after the certificate-expiry subsection:

```markdown
### Stuck-terminating resources

`scan` flags a resource wedged in `Terminating` — deletion pending longer than
two minutes — and names the blocker: a **Namespace** stuck on a finalizer or a
downstream condition (`NamespaceFinalizersRemaining` / `NamespaceContentRemaining`
/ `NamespaceDeletionContentFailure`, message shown as-is), a **Pod** stuck past
its grace period (its finalizers, or "deletion not confirmed" when the node is
gone), or a **PVC** held by `pvc-protection` (cross-referenced to a pod still
mounting it). Read-only and advisory — it never removes a finalizer (that is a
`--fix` concern) and never changes the cluster verdict. The daemon exposes
`kubeagent_resources_stuck_terminating`.
```

- [ ] **Step 4: watch-mode.md** — add a `kubeagent_resources_stuck_terminating` gauge row to the metrics table.

- [ ] **Step 5: README + CHANGELOG + roadmap** —
README (detector list): `- **Stuck-terminating** — a Namespace, Pod, or PVC wedged in Terminating past two minutes, with the blocking finalizer/condition named.`
CHANGELOG `### Added`:
```markdown
- **Stuck-terminating / finalizer-deadlock check.** `scan` flags a Namespace, Pod,
  or PVC wedged in `Terminating` past two minutes and names the blocker — a
  namespace finalizer/condition, a pod's finalizers (or "deletion not confirmed"
  when the node is gone), or `pvc-protection` cross-referenced to the pod still
  mounting the PVC. Read-only and advisory (never removes a finalizer, never
  changes the verdict); the daemon exposes `kubeagent_resources_stuck_terminating`.
  Adds a base `namespaces` read grant.
```
roadmap Shipped bullet:
```markdown
- **Stuck-terminating detection** — flags namespaces/pods/PVCs wedged in
  Terminating past two minutes and names the blocking finalizer or condition. See
  [Failure diagnostics](features/diagnostics.md).
```

- [ ] **Step 6: Verify + commit**

Run: `cd website && mkdocs build --strict -f mkdocs.yml 2>&1 | tail -3; cd ..` (skip-with-note if mkdocs absent).
Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...` (all PASS).

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt website/docs/features/diagnostics.md website/docs/features/watch-mode.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "test+docs: golden coverage and documentation for stuck-terminating"
```

---

## Notes for the executor

- **Release gate (post-merge, controller-owned):** FULL CHAOS GATE (touches `internal/collect` + RBAC + Helm) + a targeted live smoke (a namespace wedged `Terminating` via a fake finalizer, and a pod deleted with a lingering finalizer → confirm both render; confirm a healthy cluster shows nothing). Minor bump → **v0.34.0**.
- **Graceful namespaces is load-bearing:** the `namespaces, _ := collect.Namespaces(...)` error is intentionally dropped — the forbidden-namespaces test (Task 3) is the guard. Never make a namespaces list error fatal.
- **Advisory invariant:** nothing here touches `clusterhealth` or the verdict. The section is in NEEDS ATTENTION (like PVC issues) but the verdict stays node/system-based.
