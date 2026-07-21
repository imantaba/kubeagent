# Failed-PVC Root Cause Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Attribute a workload stranded by a broken PersistentVolumeClaim (Pending with `ProvisioningFailed`/`FailedBinding`, already diagnosed by `pvchealth`) to that PVC — `↳ likely caused by PVC reports-data (ProvisioningFailed)` — joining the stuck workload to the storage cause kubeagent already knows.

**Architecture:** A third annotator in `internal/rootcause`, `AnnotatePVC(workloads, podPVCs, issues)`, joins flagged not-yet-attributed workloads to `pvchealth.Issue`s via a pod→PVC mount map built in `scan.Evaluate` from already-collected pods. Threshold is 1 (evidence-backed join, not statistical inference). Annotator order: node → PVC → registry. Zero report changes — rendering and rollups reuse the v0.29/v0.30 machinery.

**Tech Stack:** Go 1.26, standard library. No new dependencies, no new API calls.

## Global Constraints

- **READ-ONLY; NO new RBAC / collector / flag** — pods, PVCs, PVC events already collected; `pvcIssues` already computed (scan.go:176).
- **Pure & deterministic** — issue keys checked in sorted order; fixed strings.
- **Precedence (load-bearing): node → PVC → registry.** `AnnotatePVC` skips workloads with existing `RootCause`; `AnnotateRegistry` runs after it.
- **Threshold 1** — deliberate: the PVC is independently diagnosed broken; do not add a higher threshold.
- **Always-on** — runs in `scan` and the `watch` daemon via `scan.Evaluate`.
- `inventory`, `clusterhealth`, `report.go`, `pvchealth`, `internal/collect`, `internal/watch`, `explain.go`, RBAC, Helm stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** (or any AI attribution) on any commit. **TDD.**
- Glyphs: `↳` U+21B3, `⇐` U+21D0 — copy exactly.
- Set PATH first every task: `export PATH=$PATH:/usr/local/go/bin`.
- Spec: [docs/superpowers/specs/2026-07-21-pvc-root-cause-design.md](../specs/2026-07-21-pvc-root-cause-design.md).

---

## File Structure

- **Modify** `internal/rootcause/rootcause.go` (+ test) — `AnnotatePVC`.
- **Modify** `internal/scan/scan.go` (+ test) — the `podPVCs` map + one call line (ordered node → PVC → registry).
- **Modify** `internal/report/golden_test.go` + `internal/report/testdata/golden-scan.txt` — the `reports` fixture workload + regenerate. `report.go` itself is NOT touched.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.

---

### Task 1: `rootcause.AnnotatePVC`

**Files:**
- Modify: `internal/rootcause/rootcause.go`
- Test: `internal/rootcause/rootcause_test.go`

**Interfaces:**
- Consumes: `inventory.Workload` (`Flagged()`, `RootCause`, `Namespace`, `Pods[].Name`), `pvchealth.Issue{Namespace, Name, Reason}`.
- Produces: `func AnnotatePVC(workloads []inventory.Workload, podPVCs map[string][]string, issues []pvchealth.Issue)` — `podPVCs` keys are `"namespace/podName"`, values are mounted PVC names. RootCause format exactly: `PVC <name> (<Reason>)`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/rootcause/rootcause_test.go` (the file already imports `testing`, `clusterhealth`, `diagnose`, `inventory`; add `"github.com/imantaba/kubeagent/internal/pvchealth"`):

```go
// pvcWL builds a flagged 0/1 Pending Deployment with one named pod (no findings —
// the realistic stuck-on-storage shape, flagged via Ready<Desired).
func pvcWL(ns, name, podName string) inventory.Workload {
	return inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment",
		Ready: 0, Desired: 1, Status: "Pending",
		Pods: []inventory.PodRow{{Name: podName, Phase: "Pending"}}}
}

func TestAnnotatePVC_MountedIssueAttributed(t *testing.T) {
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Phase: "Pending", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "PVC reports-data (ProvisioningFailed)" {
		t.Errorf("RootCause = %q, want PVC reports-data (ProvisioningFailed)", ws[0].RootCause)
	}
}

func TestAnnotatePVC_FailedBindingReason(t *testing.T) {
	ws := []inventory.Workload{pvcWL("db", "pg", "pg-0")}
	podPVCs := map[string][]string{"db/pg-0": {"pg-data"}}
	issues := []pvchealth.Issue{{Namespace: "db", Name: "pg-data", Phase: "Pending", Reason: "FailedBinding"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "PVC pg-data (FailedBinding)" {
		t.Errorf("RootCause = %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_HealthyMountsNotAttributed(t *testing.T) {
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"other-healthy-pvc"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "" {
		t.Errorf("workload mounting only healthy PVCs must not be attributed, got %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_ExistingRootCausePreserved(t *testing.T) {
	w := pvcWL("shop", "reports", "reports-1")
	w.RootCause = "node worker-2 (NotReady)"
	ws := []inventory.Workload{w}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Errorf("node attribution must win, got %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_NotFlaggedSkipped(t *testing.T) {
	w := pvcWL("shop", "reports", "reports-1")
	w.Ready, w.Desired, w.Status = 1, 1, "Running"
	ws := []inventory.Workload{w}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "" {
		t.Errorf("a healthy workload must be skipped, got %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_NamespaceIsolation(t *testing.T) {
	// Same PVC name broken in ANOTHER namespace must not match.
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "other", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "" {
		t.Errorf("an issue in a different namespace must not match, got %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_DeterministicSortedPick(t *testing.T) {
	// Pod mounts two broken PVCs; the sorted-first issue key wins.
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"zeta-data", "alpha-data"}}
	issues := []pvchealth.Issue{
		{Namespace: "shop", Name: "zeta-data", Reason: "ProvisioningFailed"},
		{Namespace: "shop", Name: "alpha-data", Reason: "FailedBinding"},
	}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "PVC alpha-data (FailedBinding)" {
		t.Errorf("sorted-first issue key must win, got %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_BeatsRegistryWhenRunFirst(t *testing.T) {
	// A workload that both mounts a broken PVC AND fails pulls alongside another
	// workload: running AnnotatePVC before AnnotateRegistry (the scan order) must
	// give it the PVC cause and shrink the registry group below threshold.
	stuck := pvcWL("shop", "reports", "reports-1")
	stuck.Image = "ghcr.io/shop/reports:1.0"
	stuck.Findings = []diagnose.Finding{{Pod: "shop/reports", Issue: "ImagePullBackOff", Reason: "Bad image reference or registry authentication"}}
	other := pullWL("web", "ghcr.io/shop/web:3.1", "ImagePullBackOff")
	ws := []inventory.Workload{stuck, other}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	AnnotateRegistry(ws)
	if ws[0].RootCause != "PVC reports-data (ProvisioningFailed)" {
		t.Errorf("PVC cause must win when run first, got %q", ws[0].RootCause)
	}
	if ws[1].RootCause != "" {
		t.Errorf("with the PVC-attributed workload excluded, the registry group is 1 -> no attribution, got %q", ws[1].RootCause)
	}
}

func TestAnnotatePVC_EmptyInputsNoop(t *testing.T) {
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	AnnotatePVC(ws, nil, nil)
	if ws[0].RootCause != "" {
		t.Errorf("empty inputs => no-op, got %q", ws[0].RootCause)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/rootcause`
Expected: FAIL — build error (`AnnotatePVC` undefined).

- [ ] **Step 3: Implement**

In `internal/rootcause/rootcause.go`, add `"github.com/imantaba/kubeagent/internal/pvchealth"` to the import block (with the other internal imports), and append:

```go
// AnnotatePVC sets w.RootCause = "PVC <name> (<reason>)" on each flagged,
// not-yet-attributed workload that has a pod mounting a PersistentVolumeClaim
// pvchealth diagnosed as broken (Pending with a provisioning/binding failure).
// podPVCs maps "namespace/podName" to the PVC names that pod mounts. The
// threshold is a single workload — the PVC is independently diagnosed, so this
// is a join against evidence, not an inference. Pure and deterministic (issue
// keys checked in sorted order). Call after Annotate (nodes win) and before
// AnnotateRegistry (evidence beats statistics).
func AnnotatePVC(workloads []inventory.Workload, podPVCs map[string][]string, issues []pvchealth.Issue) {
	if len(issues) == 0 || len(podPVCs) == 0 {
		return
	}
	reasonByKey := make(map[string]string, len(issues))
	keys := make([]string, 0, len(issues))
	for _, is := range issues {
		key := is.Namespace + "/" + is.Name
		if _, seen := reasonByKey[key]; !seen {
			keys = append(keys, key)
		}
		reasonByKey[key] = is.Reason
	}
	sort.Strings(keys)
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() || w.RootCause != "" {
			continue
		}
		mounted := map[string]bool{}
		for _, p := range w.Pods {
			for _, claim := range podPVCs[w.Namespace+"/"+p.Name] {
				mounted[w.Namespace+"/"+claim] = true
			}
		}
		for _, key := range keys {
			if mounted[key] {
				name := key[strings.IndexByte(key, '/')+1:]
				workloads[i].RootCause = "PVC " + name + " (" + reasonByKey[key] + ")"
				break
			}
		}
	}
}
```

(`sort`, `strings` are already imported from the earlier annotators.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/rootcause -v && go vet ./internal/rootcause`
Expected: PASS — all new `TestAnnotatePVC_*` plus every existing node/registry test; vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/rootcause/rootcause.go internal/rootcause/rootcause_test.go
git commit -m "feat(rootcause): attribute workloads stranded by a broken PVC"
```

---

### Task 2: Wire `AnnotatePVC` into `scan.Evaluate` (node → PVC → registry)

**Files:**
- Modify: `internal/scan/scan.go`
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `rootcause.AnnotatePVC` (Task 1); `inputs.Pods` (for the mount map); the existing `pvcIssues` local (scan.go:176).
- Produces: no signature change; PVC `RootCause` values now flow through `Evaluate`.

- [ ] **Step 1: Write the failing integration test**

Add to `internal/scan/scan_test.go` (mirrors the existing `TestEvaluate_FlagsPendingPVC` fake-object shape — the fake clientset ignores field selectors; `pvchealth.Assess` correlates by InvolvedObject in-code — plus a Deployment whose pod mounts the PVC; reuses `p32`):

```go
func TestEvaluate_AttributesRootCauseToBrokenPVC(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	sc := "fast"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "reports-data"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "reports-data.ev"},
		Reason:         "ProvisioningFailed",
		Type:           "Warning",
		Message:        `storageclass "fast" not found`,
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "reports-data"},
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "reports"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "reports-1",
		Labels: map[string]string{"app": "reports"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "reports", Image: "busybox:1.36"}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "reports-data"}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "reports", Ready: false,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}}}
	cli := fake.NewSimpleClientset(node, pvc, ev, dep, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		if w.RootCause == "PVC reports-data (ProvisioningFailed)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a workload attributed to PVC reports-data, got %+v", res.Inventory.Workloads)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan -run TestEvaluate_AttributesRootCauseToBrokenPVC`
Expected: FAIL — `RootCause` empty (wiring absent).

- [ ] **Step 3: Add the mount map + call line**

In `internal/scan/scan.go`, immediately after the existing `podLabels` loop (scan.go:184-187), add the mount map:

```go
	podPVCs := make(map[string][]string, len(inputs.Pods))
	for _, p := range inputs.Pods {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				key := p.Namespace + "/" + p.Name
				podPVCs[key] = append(podPVCs[key], v.PersistentVolumeClaim.ClaimName)
			}
		}
	}
```

Then change the annotator block so the order is node → PVC → registry:

```go
	rootcause.Annotate(result.Workloads, health.DownNodes)
	rootcause.AnnotatePVC(result.Workloads, podPVCs, pvcIssues)
	rootcause.AnnotateRegistry(result.Workloads)
```

(The `AnnotatePVC` line is inserted BETWEEN the two existing calls. `pvcIssues` is already in scope from scan.go:176.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/scan`
Expected: PASS (new test + all existing `Evaluate` tests, including the registry and node-precedence guards).

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): run PVC root-cause attribution between node and registry"
```

---

### Task 3: Golden fixture + snapshot

**Files:**
- Modify: `internal/report/golden_test.go`
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated)

**Interfaces:** none — exercises the rendering through the real renderer. The golden renders a pre-built `Input` (annotators do not run), so `RootCause` is set directly. The fixture already carries the `shop/reports-data` PVC issue (`PVCIssues`), so this workload joins an existing fixture element — no new PVC entry needed.

- [ ] **Step 1: Add the `reports` workload to the fixture**

In `internal/report/golden_test.go`, `goldenWorkloads()`, insert this entry immediately AFTER the `search` entry (added by the registry feature) — a 0/1 Pending Deployment with NO findings (flagged via Ready<Desired; the realistic stuck-on-storage shape):

```go
		{Namespace: "shop", Name: "reports", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Pending",
			Image: "busybox:1.36", RootCause: "PVC reports-data (ProvisioningFailed)",
			Pods: []inventory.PodRow{{Name: "reports-6c9-fk2vw", Phase: "Pending", Ready: "0/1", Restarts: 0, Node: "worker-3", IP: "", Age: "2h", Image: "busybox:1.36"}}},
```

- [ ] **Step 2: Run the golden test to see it fail (snapshot stale)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput`
Expected: FAIL — the attention line and a new workload block changed.

- [ ] **Step 3: Regenerate**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput -update`
Expected: PASS.

- [ ] **Step 4: Inspect the regenerated snapshot**

Run: `grep -n "workloads failing\|likely caused by" internal/report/testdata/golden-scan.txt`
Expected: (a) the attention clause reads exactly `14 workloads failing (7 ⇐ 4 root causes)` (14=13+1; 7=6+1; 4 = worker-1, worker-2, ghcr.io, PVC reports-data), with the rest of the line — ` · 2 services without endpoints · 1 ingress route broken · 1 PVC failing to provision` — unchanged; (b) SEVEN `↳ likely caused by` lines total: the 6 existing (4 node + 2 registry) unchanged, plus one new `↳ likely caused by PVC reports-data (ProvisioningFailed)` under `✗ shop/reports`; (c) the `reports` block shows the cause line, image line, and pod row (no findings). If any number differs, STOP and recheck.

- [ ] **Step 5: Run the full report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report`
Expected: PASS (`TestGoldenScanOutput` + `TestGoldenInputCoversAllSections` — the new workload has no findings, so the ≥6-distinct-modes guard is unaffected).

- [ ] **Step 6: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover PVC root-cause attribution in the golden snapshot"
```

---

### Task 4: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs only). `go build`/`go test` stay green.

- [ ] **Step 1: Extend the diagnostics subsection**

In `website/docs/features/diagnostics.md`, in the `### Root-cause attribution` subsection, append this paragraph after the registry paragraph:

```markdown
A **broken PersistentVolumeClaim** is joined the same way: when a workload's pod
mounts a PVC that the [Pending-PVC check](#pending-pvc-storage-provisioning) has
diagnosed as failing to provision or bind, the workload is attributed
`↳ likely caused by PVC <name> (ProvisioningFailed)` — connecting a pod stuck in
`Pending`/`ContainerCreating` (which has no pod-level finding of its own) to the
storage cause kubeagent already reports. Because the PVC is independently
diagnosed, a single affected workload is enough — unlike the registry case, this
is a join against evidence, not an inference. Node attribution still takes
precedence.
```

(Verify the `#pending-pvc-storage-provisioning` anchor matches the actual `### Pending PVC (storage provisioning)` heading in this file — mkdocs anchors lowercase the heading and hyphenate; adjust the link if the generated anchor differs.)

- [ ] **Step 2: Extend the README bullet**

In `README.md`, extend the root-cause bullet — change:

```markdown
- **Root-cause attribution** — when a node is NotReady or its kubelet stops
  heartbeating, workloads with pods on it are attributed to that node ("↳ likely
  caused by node X"); when several workloads fail image pulls from the same
  registry, they are attributed to that registry — one shared cause instead of N
  disconnected findings.
```

to:

```markdown
- **Root-cause attribution** — when a node is NotReady or its kubelet stops
  heartbeating, workloads with pods on it are attributed to that node ("↳ likely
  caused by node X"); when several workloads fail image pulls from the same
  registry, they are attributed to that registry; when a workload's pod mounts a
  PVC that cannot provision, it is attributed to that PVC — one shared cause
  instead of N disconnected findings.
```

- [ ] **Step 3: CHANGELOG entry**

In `CHANGELOG.md`, under `## [Unreleased]` → `### Added` (create the headers if the last release consumed them):

```markdown
- **Failed-PVC root-cause attribution.** A workload whose pod mounts a
  PersistentVolumeClaim that cannot provision or bind (the v0.26.0 Pending-PVC
  check) is now attributed to that PVC — "↳ likely caused by PVC reports-data
  (ProvisioningFailed)" — connecting a pod stuck Pending/ContainerCreating, which
  has no pod-level finding of its own, to the storage cause kubeagent already
  reports. One affected workload is enough (the PVC is independently diagnosed);
  node attribution takes precedence. Read-only, always-on, no new RBAC.
```

- [ ] **Step 4: Extend the roadmap Shipped bullet**

In `website/docs/roadmap.md`, change the root-cause Shipped bullet's title and tail to cover all three:

```markdown
- **Root-cause attribution (nodes, registries & PVCs)** — a hard-down node
  (NotReady or kubelet-not-heartbeating) becomes the named root cause of the
  workloads with pods on it; a registry shared by two-plus failing image pulls
  becomes the named root cause of those workloads; and a PVC that cannot
  provision becomes the named root cause of the workloads mounting it; the first
  slices of the root-cause correlation theme. See
  [Failure diagnostics](features/diagnostics.md).
```

- [ ] **Step 5: Verify docs build (if mkdocs available) + full suite**

Run: `cd website && mkdocs build --strict -f mkdocs.yml 2>&1 | tail -3; cd ..` (skip with a note if mkdocs is not installed).
Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 6: Commit**

```bash
git add website/docs/features/diagnostics.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "docs: document failed-PVC root-cause attribution"
```

---

## Notes for the executor

- **Release gate (post-merge):** touches `rootcause`/`scan`/`report tests` only — **not** `internal/collect`/`cluster`/`watch`/RBAC/Helm — so a **lightweight real-cluster smoke** (ekb negative + a Kind cluster with a PVC on a nonexistent StorageClass mounted by a deployment) suffices. Version bump: **minor**, v0.30.0 → **v0.31.0**.
- **Precedence is load-bearing:** `AnnotatePVC` between `Annotate` (nodes) and `AnnotateRegistry` in `scan.Evaluate`; it must skip workloads with an existing `RootCause`.
- **Threshold 1 is deliberate** (evidence-backed join) — do not add a higher threshold; do not strengthen the hedged "likely" wording.
- **In-file anchor check** (Task 4): confirm the `#pending-pvc-storage-provisioning` link resolves against the actual heading before committing docs.
