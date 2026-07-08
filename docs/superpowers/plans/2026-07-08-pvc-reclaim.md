# PVC Reclaim-Policy Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `kubeagent scan` lists Bound PVCs whose bound PV has `reclaimPolicy: Delete`, and the watch daemon exposes the count as a Prometheus gauge.

**Architecture:** A new pure package `internal/pvcreclaim` correlates PVCs to their bound PVs and flags the Delete-policy ones. Two new read-only `collect` List calls (PVCs namespaced, PVs cluster-scoped) feed it via `scan.Evaluate`. `internal/report` renders a text section + JSON field; `internal/watch/metrics.go` exposes `kubeagent_pvcs_reclaim_delete`. RBAC gains `persistentvolumeclaims` + `persistentvolumes` read.

**Tech Stack:** Go 1.26, `k8s.io/api/core/v1`, `k8s.io/apimachinery/pkg/api/resource`. Tests use fake objects + client-go fake clientset (no cluster).

## Global Constraints

- **Read-only.** Only List/Get calls; no writes, no LLM. New collectors are best-effort — a List error yields an empty report, never fails the scan.
- **RBAC:** add core (`""`) `persistentvolumeclaims` and `persistentvolumes` with verbs `get`/`list`/`watch` to BOTH `deploy/rbac.yaml` and `deploy/helm/kubeagent/templates/clusterrole.yaml`. Still get/list/watch only.
- **Flag rule:** a PVC is listed when `Status.Phase == Bound` AND its bound PV's `Spec.PersistentVolumeReclaimPolicy == Delete`. Presence in the list is the advisory.
- **Advisory only:** must NOT change `clusterhealth` verdict, `kubeagent_cluster_healthy`, or the scan exit code.
- **Not** wired into `--explain`.
- Metric name exactly `kubeagent_pvcs_reclaim_delete`. Text header exactly `PVCs with reclaim policy Delete:`. JSON field `pvcReclaim`.
- Commits: **no `Co-Authored-By: Claude` trailer**.
- TDD: failing test first, watch it fail, implement, pass, commit.

---

### Task 1: `internal/pvcreclaim` package

**Files:**
- Create: `internal/pvcreclaim/pvcreclaim.go`
- Test: `internal/pvcreclaim/pvcreclaim_test.go`

**Interfaces:**
- Consumes: `[]corev1.PersistentVolumeClaim`, `[]corev1.PersistentVolume`.
- Produces:
  - `type PVCReclaim struct { Namespace, Name, PV, StorageClass, Capacity string }` (JSON tags below).
  - `type Report struct { PVCs []PVCReclaim; Count int }` (JSON tags below).
  - `func Assess(pvcs []corev1.PersistentVolumeClaim, pvs []corev1.PersistentVolume) Report`.

- [ ] **Step 1: Write the failing test**

Create `internal/pvcreclaim/pvcreclaim_test.go`:

```go
package pvcreclaim

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pv(name string, policy corev1.PersistentVolumeReclaimPolicy) corev1.PersistentVolume {
	return corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: policy},
	}
}

// pvc builds a PVC. class "" means no storageClassName. cap "" means no status capacity.
func pvc(ns, name, volumeName, class, capacity string, phase corev1.PersistentVolumeClaimPhase) corev1.PersistentVolumeClaim {
	spec := corev1.PersistentVolumeClaimSpec{VolumeName: volumeName}
	if class != "" {
		c := class
		spec.StorageClassName = &c
	}
	st := corev1.PersistentVolumeClaimStatus{Phase: phase}
	if capacity != "" {
		st.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)}
	}
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spec,
		Status:     st,
	}
}

func find(r Report, ns, name string) (PVCReclaim, bool) {
	for _, p := range r.PVCs {
		if p.Namespace == ns && p.Name == name {
			return p, true
		}
	}
	return PVCReclaim{}, false
}

func TestAssess_ListsBoundDeletePVC(t *testing.T) {
	pvs := []corev1.PersistentVolume{pv("pv-a", corev1.PersistentVolumeReclaimDelete)}
	pvcs := []corev1.PersistentVolumeClaim{pvc("shop", "data-0", "pv-a", "standard", "8Gi", corev1.ClaimBound)}
	r := Assess(pvcs, pvs)
	p, ok := find(r, "shop", "data-0")
	if !ok {
		t.Fatalf("expected shop/data-0 listed, got %+v", r)
	}
	if p.PV != "pv-a" || p.StorageClass != "standard" || p.Capacity != "8Gi" {
		t.Errorf("wrong row: %+v", p)
	}
	if r.Count != 1 {
		t.Errorf("want Count 1, got %d", r.Count)
	}
}

func TestAssess_SkipsRetainPVC(t *testing.T) {
	pvs := []corev1.PersistentVolume{pv("pv-r", corev1.PersistentVolumeReclaimRetain)}
	pvcs := []corev1.PersistentVolumeClaim{pvc("shop", "keep", "pv-r", "standard", "8Gi", corev1.ClaimBound)}
	r := Assess(pvcs, pvs)
	if _, ok := find(r, "shop", "keep"); ok {
		t.Errorf("Retain PVC must not be listed")
	}
	if r.Count != 0 {
		t.Errorf("want Count 0, got %d", r.Count)
	}
}

func TestAssess_SkipsUnboundPVC(t *testing.T) {
	pvs := []corev1.PersistentVolume{pv("pv-a", corev1.PersistentVolumeReclaimDelete)}
	// Pending PVC with no volumeName.
	pvcs := []corev1.PersistentVolumeClaim{pvc("shop", "pending", "", "standard", "", corev1.ClaimPending)}
	r := Assess(pvcs, pvs)
	if r.Count != 0 {
		t.Errorf("unbound PVC must not be listed, got %+v", r)
	}
}

func TestAssess_SkipsWhenBoundPVMissing(t *testing.T) {
	// Bound to a PV name that has no matching PV object — defensive.
	pvcs := []corev1.PersistentVolumeClaim{pvc("shop", "orphan", "pv-gone", "standard", "8Gi", corev1.ClaimBound)}
	r := Assess(pvcs, nil)
	if r.Count != 0 {
		t.Errorf("PVC with no resolvable PV must not be listed, got %+v", r)
	}
}

func TestAssess_NoStorageClassStillListed(t *testing.T) {
	pvs := []corev1.PersistentVolume{pv("pv-s", corev1.PersistentVolumeReclaimDelete)}
	pvcs := []corev1.PersistentVolumeClaim{pvc("shop", "static", "pv-s", "", "", corev1.ClaimBound)}
	r := Assess(pvcs, pvs)
	p, ok := find(r, "shop", "static")
	if !ok {
		t.Fatalf("static Delete PVC should be listed")
	}
	if p.StorageClass != "" || p.Capacity != "" {
		t.Errorf("want empty class/capacity, got %+v", p)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/pvcreclaim/`
Expected: FAIL — `undefined: Assess` / `undefined: Report`.

- [ ] **Step 3: Write the implementation**

Create `internal/pvcreclaim/pvcreclaim.go`:

```go
// Package pvcreclaim lists PersistentVolumeClaims whose bound PersistentVolume
// has reclaimPolicy Delete — the data-loss-prone case where deleting the PVC or
// PV destroys the underlying storage. Pure: the caller supplies the PVCs and
// PVs. Read-only.
package pvcreclaim

import (
	corev1 "k8s.io/api/core/v1"
)

// PVCReclaim is one flagged PVC: it is Bound and its PV reclaims with Delete.
type PVCReclaim struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	PV           string `json:"pv"`
	StorageClass string `json:"storageClass,omitempty"`
	Capacity     string `json:"capacity,omitempty"`
}

// Report is the set of Delete-policy PVCs. Count == len(PVCs).
type Report struct {
	PVCs  []PVCReclaim `json:"pvcs"`
	Count int          `json:"count"`
}

// Assess flags each Bound PVC whose bound PV has reclaimPolicy Delete.
func Assess(pvcs []corev1.PersistentVolumeClaim, pvs []corev1.PersistentVolume) Report {
	policy := make(map[string]corev1.PersistentVolumeReclaimPolicy, len(pvs))
	for _, v := range pvs {
		policy[v.Name] = v.Spec.PersistentVolumeReclaimPolicy
	}

	rep := Report{PVCs: make([]PVCReclaim, 0)}
	for _, c := range pvcs {
		if c.Status.Phase != corev1.ClaimBound || c.Spec.VolumeName == "" {
			continue
		}
		p, ok := policy[c.Spec.VolumeName]
		if !ok || p != corev1.PersistentVolumeReclaimDelete {
			continue
		}
		rep.PVCs = append(rep.PVCs, PVCReclaim{
			Namespace:    c.Namespace,
			Name:         c.Name,
			PV:           c.Spec.VolumeName,
			StorageClass: storageClass(c),
			Capacity:     capacity(c),
		})
	}
	rep.Count = len(rep.PVCs)
	return rep
}

func storageClass(c corev1.PersistentVolumeClaim) string {
	if c.Spec.StorageClassName == nil {
		return ""
	}
	return *c.Spec.StorageClassName
}

func capacity(c corev1.PersistentVolumeClaim) string {
	q, ok := c.Status.Capacity[corev1.ResourceStorage]
	if !ok {
		return ""
	}
	return q.String()
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/pvcreclaim/`
Expected: PASS (all 5 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/pvcreclaim/
git commit -m "feat(pvcreclaim): flag Bound PVCs whose PV reclaims with Delete"
```

---

### Task 2: Collect PVCs + PVs, wire into `scan.Result`

**Files:**
- Modify: `internal/collect/collect.go` (add two List helpers after `StorageClasses`, ~145)
- Test: `internal/collect/collect_test.go` (add one fake-clientset test)
- Modify: `internal/scan/scan.go` (import, `Result` struct, `Evaluate`)

**Interfaces:**
- Consumes: `pvcreclaim.Assess(pvcs, pvs) Report` from Task 1.
- Produces:
  - `func PersistentVolumeClaims(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.PersistentVolumeClaim, error)`
  - `func PersistentVolumes(ctx context.Context, client kubernetes.Interface) ([]corev1.PersistentVolume, error)`
  - `scan.Result.PVCReclaim pvcreclaim.Report`

- [ ] **Step 1: Write the failing collect test**

Add to `internal/collect/collect_test.go` (it already imports `context`, `testing`, `corev1`, `metav1`, and `k8s.io/client-go/kubernetes/fake`):

```go
func TestPersistentVolumeClaimsAndVolumes_List(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data-0"}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-a"}},
	)
	pvcs, err := PersistentVolumeClaims(context.Background(), client, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pvcs) != 1 || pvcs[0].Name != "data-0" {
		t.Errorf("want 1 pvc data-0, got %+v", pvcs)
	}
	pvs, err := PersistentVolumes(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(pvs) != 1 || pvs[0].Name != "pv-a" {
		t.Errorf("want 1 pv pv-a, got %+v", pvs)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -run TestPersistentVolumeClaimsAndVolumes_List`
Expected: FAIL to compile — `undefined: PersistentVolumeClaims` / `undefined: PersistentVolumes`.

- [ ] **Step 3: Add the collect helpers**

In `internal/collect/collect.go`, immediately after the `StorageClasses` function (~145), add:

```go
// PersistentVolumeClaims lists PVCs in the namespace (all namespaces when
// empty), read-only.
func PersistentVolumeClaims(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.PersistentVolumeClaim, error) {
	pvcs, err := client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing persistentvolumeclaims: %w", err)
	}
	return pvcs.Items, nil
}

// PersistentVolumes lists all PVs (cluster-scoped, read-only).
func PersistentVolumes(ctx context.Context, client kubernetes.Interface) ([]corev1.PersistentVolume, error) {
	pvs, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing persistentvolumes: %w", err)
	}
	return pvs.Items, nil
}
```

- [ ] **Step 4: Run the collect test to verify it passes**

Run: `go test ./internal/collect/`
Expected: PASS.

- [ ] **Step 5: Wire into `scan.Result` and `Evaluate`**

In `internal/scan/scan.go`, add the import (alphabetical among `internal/...`, after `netpolicy`; note `nodereserve` is already there):

```go
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
```

Add a field to `Result` (after `NodeReserve`):

```go
	PVCReclaim    pvcreclaim.Report
```

In `Evaluate`, after the `serviceIssues` block (before `result := inventory.Prioritize(...)`), add the best-effort collection:

```go
	pvcs, _ := collect.PersistentVolumeClaims(ctx, client, opts.Namespace)
	pvs, _ := collect.PersistentVolumes(ctx, client)
	pvcReclaim := pvcreclaim.Assess(pvcs, pvs)
```

Add `PVCReclaim: pvcReclaim` to the returned `Result` literal:

```go
	return Result{Inputs: inputs, Nodes: nodes, NodeReserve: nodereserve.Assess(nodes), PVCReclaim: pvcReclaim, Health: health, Inventory: result, ServiceIssues: serviceIssues}, nil
```

- [ ] **Step 6: Verify build + scan tests**

Run: `go build ./... && go test ./internal/scan/ ./internal/collect/`
Expected: build succeeds; tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go internal/scan/scan.go
git commit -m "feat(scan): collect PVCs+PVs and carry PVC reclaim report in Result"
```

---

### Task 3: Report — text section, JSON field, caller updates

**Files:**
- Modify: `internal/report/report.go` (import, `inventoryReport`, `PrintInventory`, `printInventoryText`, add `printPVCReclaim`)
- Modify: `main.go:143` (pass `&res.PVCReclaim`)
- Modify: `internal/report/report_test.go` (insert `nil` at the new position in every existing `PrintInventory(...)` call; add two new tests)

**Interfaces:**
- Consumes: `pvcreclaim.Report` / `pvcreclaim.PVCReclaim` from Task 1; `scan.Result.PVCReclaim` from Task 2.
- Produces: new `PrintInventory` parameter `pvcReclaim *pvcreclaim.Report` inserted immediately AFTER `nodeReserve` and before `explanation`.

Context: `PrintInventory` currently is
`func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, credentialWarnings []credlint.Finding, nodeReserve *nodereserve.Report, explanation, format string, w io.Writer) error`.
Every existing `report_test.go` call already passes a `nodeReserve` argument (`nil` or a `*nodereserve.Report`) in that slot. This task adds `pvcReclaim` right after it.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go` (add `"github.com/imantaba/kubeagent/internal/pvcreclaim"` to its imports; it already imports `bytes`, `encoding/json`, `strings`, `clusterhealth`, `inventory`):

```go
func TestPrintInventory_TextShowsPVCReclaim(t *testing.T) {
	var buf bytes.Buffer
	rep := &pvcreclaim.Report{
		Count: 1,
		PVCs: []pvcreclaim.PVCReclaim{
			{Namespace: "shop", Name: "data-0", PV: "pvc-abc123", StorageClass: "standard", Capacity: "8Gi"},
		},
	}
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, nil, nil, rep, "", "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "PVCs with reclaim policy Delete:") {
		t.Errorf("missing section header in:\n%s", out)
	}
	if !strings.Contains(out, "shop/data-0") || !strings.Contains(out, "pv pvc-abc123") {
		t.Errorf("missing pvc row in:\n%s", out)
	}
	if !strings.Contains(out, "class standard") || !strings.Contains(out, "8Gi") {
		t.Errorf("missing class/capacity in:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesPVCReclaim(t *testing.T) {
	var buf bytes.Buffer
	rep := &pvcreclaim.Report{Count: 1, PVCs: []pvcreclaim.PVCReclaim{
		{Namespace: "shop", Name: "data-0", PV: "pvc-abc123"},
	}}
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, nil, nil, rep, "", "json", &buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	pr, ok := got["pvcReclaim"].(map[string]any)
	if !ok {
		t.Fatalf("pvcReclaim missing/wrong type in: %s", buf.String())
	}
	if pr["count"].(float64) != 1 {
		t.Errorf("want count 1, got %v", pr["count"])
	}
}
```

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run PVCReclaim`
Expected: FAIL to compile — `too many arguments in call to PrintInventory` and `undefined: pvcreclaim`.

- [ ] **Step 3: Update the JSON struct and signatures**

In `internal/report/report.go`, add the import (after `"github.com/imantaba/kubeagent/internal/platform"` or wherever alphabetical — group with the other internal imports):

```go
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
```

Add a field to `inventoryReport` (after `NodeReserve`):

```go
	PVCReclaim         *pvcreclaim.Report          `json:"pvcReclaim,omitempty"`
```

Change `PrintInventory` to accept `pvcReclaim *pvcreclaim.Report` immediately after `nodeReserve`, and thread it into both branches:

```go
func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, credentialWarnings []credlint.Finding, nodeReserve *nodereserve.Report, pvcReclaim *pvcreclaim.Report, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Cluster: cluster, Workloads: result.Workloads, Resources: summary, Platform: facts, ServiceIssues: serviceIssues, CredentialWarnings: credentialWarnings, NodeReserve: nodeReserve, PVCReclaim: pvcReclaim, Explanation: explanation})
	case "text":
		return printInventoryText(cluster, result, summary, facts, serviceIssues, credentialWarnings, nodeReserve, pvcReclaim, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}
```

- [ ] **Step 4: Update `printInventoryText` and add the renderer**

Change the `printInventoryText` signature to accept `pvcReclaim *pvcreclaim.Report` after `nodeReserve`:

```go
func printInventoryText(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, credentialWarnings []credlint.Finding, nodeReserve *nodereserve.Report, pvcReclaim *pvcreclaim.Report, explanation string, w io.Writer) error {
```

Immediately after the existing `printNodeReservations(nodeReserve, w)` block (the `if err := printNodeReservations(...); err != nil { return err }`), insert:

```go
	if err := printPVCReclaim(pvcReclaim, w); err != nil {
		return err
	}
```

Add the renderer (place it right after `printNodeReservations`):

```go
// printPVCReclaim lists PVCs whose bound PV reclaims with Delete. Nothing is
// printed when the report is nil or empty.
func printPVCReclaim(rep *pvcreclaim.Report, w io.Writer) error {
	if rep == nil || len(rep.PVCs) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "PVCs with reclaim policy Delete:"); err != nil {
		return err
	}
	for _, p := range rep.PVCs {
		line := fmt.Sprintf("  ⚠ %s/%s  pv %s", p.Namespace, p.Name, p.PV)
		if p.StorageClass != "" {
			line += "  class " + p.StorageClass
		}
		if p.Capacity != "" {
			line += "  " + p.Capacity
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}
```

- [ ] **Step 5: Update the `main.go` caller**

In `main.go:143`, pass `&res.PVCReclaim` immediately after `&res.NodeReserve`:

```go
	if err := report.PrintInventory(health, result, &summary, &facts, serviceIssues, credWarnings, &res.NodeReserve, &res.PVCReclaim, explanation, *output, os.Stdout); err != nil {
```

- [ ] **Step 6: Fix the existing test callers**

`go build ./...` now fails, listing every existing `PrintInventory(...)` call in `internal/report/report_test.go` with the old arity. For each, insert `nil` in the new slot — immediately AFTER the `nodeReserve` argument and before `explanation`. The `nodeReserve` slot is the 7th argument (right after the `credentialWarnings` arg). Example:

```go
// before
PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, nil, nil, "", "text", &buf)
// after (add one more nil for pvcReclaim)
PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, nil, nil, nil, "", "text", &buf)
```

Do NOT change the two new tests from Step 1 (they already pass `nil, rep` in those two slots). After editing, run `go build ./...` and re-fix any remaining callsites the compiler names.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go build ./... && go test ./internal/report/`
Expected: build succeeds; all report tests PASS (existing + two new).

- [ ] **Step 8: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go main.go
git commit -m "feat(report): render PVC reclaim-Delete section and pvcReclaim JSON field"
```

---

### Task 4: Daemon metric `kubeagent_pvcs_reclaim_delete`

**Files:**
- Modify: `internal/watch/metrics.go` (`metrics` struct, `update`, `render`)
- Modify: `internal/watch/metrics_test.go` (extend `sampleResult` + `TestMetrics_RenderReflectsResult`)

**Interfaces:**
- Consumes: `scan.Result.PVCReclaim.Count` from Task 2.
- Produces: gauge line `kubeagent_pvcs_reclaim_delete <n>`.

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, add `"github.com/imantaba/kubeagent/internal/pvcreclaim"` to imports, set the sample count, and assert the gauge. Update `sampleResult`:

```go
func sampleResult() *scan.Result {
	return &scan.Result{
		Health:      clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		NodeReserve: nodereserve.Report{WarnCount: 1},
		PVCReclaim:  pvcreclaim.Report{Count: 2},
		Inventory: inventory.Result{Workloads: []inventory.Workload{
			{Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 1,
				Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff"}}},
		}},
	}
}
```

Add to the `want` list in `TestMetrics_RenderReflectsResult`:

```go
		"kubeagent_pvcs_reclaim_delete 2",
```

(The test file already imports `nodereserve` from the node-reservations feature; add only `pvcreclaim`.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult`
Expected: FAIL — output missing `kubeagent_pvcs_reclaim_delete 2` (and compile error until the field exists).

- [ ] **Step 3: Add the field, update, and gauge**

In `internal/watch/metrics.go`, add a field to the `metrics` struct (after `nodesNoReserve`):

```go
	pvcsReclaimDelete int
```

In `update`, after `m.nodesNoReserve = res.NodeReserve.WarnCount`, add:

```go
	m.pvcsReclaimDelete = res.PVCReclaim.Count
```

In `render`, after the `kubeagent_nodes_without_reservations` gauge line, add:

```go
	gauge("kubeagent_pvcs_reclaim_delete", "PVCs whose bound PV has reclaimPolicy Delete", float64(m.pvcsReclaimDelete))
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/watch/`
Expected: PASS (the error-path test keeps last-good gauges; the new gauge rides the same snapshot).

- [ ] **Step 5: Commit**

```bash
git add internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): expose kubeagent_pvcs_reclaim_delete gauge"
```

---

### Task 5: RBAC — grant PVC + PV read

**Files:**
- Modify: `deploy/rbac.yaml` (core resources list, ~13)
- Modify: `deploy/helm/kubeagent/templates/clusterrole.yaml` (core resources list, ~11)

**Interfaces:** none (manifests). The daemon runs the same `scan.Evaluate` pipeline, so it needs the same read grants the CLI uses.

- [ ] **Step 1: Update `deploy/rbac.yaml`**

Change the core (`""`) rule's resources line to add `persistentvolumeclaims` and `persistentvolumes`:

```yaml
  - apiGroups: [""]
    resources: [pods, nodes, services, configmaps, events, persistentvolumeclaims, persistentvolumes]
    verbs: [get, list, watch]
```

- [ ] **Step 2: Update the Helm ClusterRole**

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, make the same change to the core rule:

```yaml
  - apiGroups: [""]
    resources: [pods, nodes, services, configmaps, events, persistentvolumeclaims, persistentvolumes]
    verbs: [get, list, watch]
```

- [ ] **Step 3: Verify the chart still renders read-only**

Run (helm on PATH — `$HOME/.local/bin` or `/usr/local/bin`):

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent | grep -A3 'apiGroups: \[""\]'
```

Expected: lint clean; the rendered core rule shows `persistentvolumeclaims, persistentvolumes` with verbs `[get, list, watch]` and no write verbs.

- [ ] **Step 4: Confirm no write verbs anywhere in the rendered RBAC**

```bash
helm template x deploy/helm/kubeagent | grep -iE 'create|update|patch|delete|deletecollection' | grep -i verb && echo BAD || echo "read-only OK"
```

Expected: `read-only OK`.

- [ ] **Step 5: Commit**

```bash
git add deploy/rbac.yaml deploy/helm/kubeagent/templates/clusterrole.yaml
git commit -m "feat(rbac): grant read-only persistentvolumeclaims + persistentvolumes"
```

---

### Task 6: Docs — CHANGELOG, website, README

**Files:**
- Modify: `CHANGELOG.md` (add/lead an `## [Unreleased]` → `### Added` bullet)
- Modify: `website/docs/features/diagnostics.md`
- Modify: `website/docs/features/watch-mode.md`
- Modify: `README.md`

**Interfaces:** none. Use exact metric name `kubeagent_pvcs_reclaim_delete` and section header `PVCs with reclaim policy Delete:`.

- [ ] **Step 1: CHANGELOG**

The current top section is `## [0.12.0] - 2026-07-08`. Add a new `## [Unreleased]` section above it with the bullet:

```markdown
## [Unreleased]

### Added

- **PVC reclaim-policy check.** `scan` now lists Bound PersistentVolumeClaims
  whose bound PersistentVolume has `reclaimPolicy: Delete` — the data-loss-prone
  case where deleting the PVC or PV destroys the underlying storage. Shown as a
  "PVCs with reclaim policy Delete" section (text + JSON `pvcReclaim`) and, in
  the watch daemon, as the gauge `kubeagent_pvcs_reclaim_delete`. Reads PVCs and
  their bound PVs (two new read-only RBAC grants); advisory only (does not change
  the cluster verdict).
```

- [ ] **Step 2: diagnostics.md**

First inspect the structure:

Run: `grep -nE '^#|^##|^###' website/docs/features/diagnostics.md | head -40`

Add a subsection after the node-reservations subsection, matching its heading level and style:

```markdown
### PVC reclaim policy

`scan` lists Bound PersistentVolumeClaims whose bound PersistentVolume has
`reclaimPolicy: Delete`. For those volumes, deleting the PVC (or the PV) tells
the provisioner to destroy the underlying storage — so the section is a
data-loss audit: which claims are *not* protected by `Retain`. The reclaim
policy is read from the bound PV (the authoritative value), so only Bound PVCs
appear. `Delete` is the common default for dynamic provisioners, so the list can
be long; it is informational and never changes the cluster verdict. Reading PVCs
and PVs needs only `get`/`list`/`watch`.
```

- [ ] **Step 3: watch-mode.md**

Run: `grep -nE 'kubeagent_|metric' website/docs/features/watch-mode.md | head -30`

Add `kubeagent_pvcs_reclaim_delete` to the documented metrics in the same format as the neighbouring entries, described as: "Number of PVCs whose bound PV has reclaimPolicy Delete."

- [ ] **Step 4: README**

Run: `grep -nE 'node reservation|RestartLoop|detect' README.md | head`

Add a one-line mention alongside the existing detector/check list, matching the surrounding style (e.g. "PVC reclaim-policy check (lists Bound PVCs whose PV reclaims with Delete)").

- [ ] **Step 5: Verify the website builds**

Run (venv from prior sessions; if missing, `python3 -m venv <path> && <path>/bin/pip install -r website/requirements.txt`):

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: "Documentation built" with no strict WARNING lines referencing the edited pages. (The red mkdocs-material 2.0 banner is cosmetic.)

- [ ] **Step 6: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md website/docs/features/watch-mode.md README.md
git commit -m "docs: document PVC reclaim-policy check and its daemon metric"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Then a manual smoke against any cluster with a Delete-policy PVC:

```bash
go build -o kubeagent . && ./kubeagent scan --output text | sed -n '/PVCs with reclaim policy Delete:/,/^$/p'
```

Expected: a "PVCs with reclaim policy Delete:" block listing each Bound PVC whose PV reclaims with Delete (namespace/name, pv, class, capacity).
