# PVC provisioning root cause — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag a Pending PVC by a structural PVC → StorageClass → PV correlation (missing StorageClass, or no matching PV for a static claim) even when no `ProvisioningFailed` event is present, naming a clean cause and catching PVCs whose events have expired.

**Architecture:** Extend `pvchealth.Assess` to take the collected StorageClasses + PVs and compute a structural cause per Pending PVC; the structural cause takes precedence over the existing event message. `scan.Evaluate` collects StorageClasses and passes them plus PVs. No report change — the enriched `Detail` flows through `printPVCIssues`.

**Tech Stack:** Go 1.26, `k8s.io/api/core/v1`, `k8s.io/api/storage/v1`, `k8s.io/apimachinery/pkg/api/resource` (in tests, for `resource.MustParse`). Tests use fake objects.

## Global Constraints

- **READ-ONLY.** Pure correlation over already-collected objects; no cluster calls in `pvchealth`, no writes, no LLM.
- **Always-on; no flag.** No new RBAC (storageclasses + persistentvolumes are already granted and collected), no new collector, no watch gauge, no `Result` field, no `report` change.
- **Advisory** — the cluster verdict is unchanged; only the PVC finding's cause text (and which event-less PVCs get flagged) changes.
- **Pure, deterministic** — `Assess` reads only its parameters; sorted by (Namespace, Name).
- **No false positives** — a fresh Pending PVC with a valid **dynamic** StorageClass (present in the cluster) and no failure event is NEVER structurally flagged; `nil` storageClassName is left to the event path.
- **v1 uses the standard-library `flag` package only** — no Cobra.
- **No `Co-Authored-By: Claude` trailer** (or any Claude attribution) on any commit. Every commit authored solely by the human.
- **TDD** — write the failing test first, watch it fail, then implement. **gofmt-clean.**
- Constants: em dash is not used here; strings use straight ASCII (`references StorageClass "X" which does not exist`, `no available PersistentVolume matches its request (10Gi, ReadWriteOnce)`).
- Cause precedence: `MissingStorageClass` → `NoMatchingPV` → existing event path.

---

### Task 1: `pvchealth.Assess` — structural cause (missing SC + no matching PV)

**Files:**
- Modify: `internal/pvchealth/pvchealth.go` (Assess signature + `structuralCause` + helpers; add imports)
- Test: `internal/pvchealth/pvchealth_test.go` (add tests + helpers; update existing `Assess` callers)

**Interfaces:**
- Produces: `func Assess(pvcs []corev1.PersistentVolumeClaim, events []corev1.Event, storageClasses []storagev1.StorageClass, pvs []corev1.PersistentVolume) []Issue` (two new trailing params). The `Issue` struct is unchanged; `Reason` gains the values `"MissingStorageClass"` and `"NoMatchingPV"`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/pvchealth/pvchealth_test.go`. It already imports `corev1`, `metav1` and has `pendingPVC(ns, name, sc string)` (sets `StorageClassName: &sc`, Phase Pending) and `pvcEvent(ns, name, reason, message)`. Add imports `storagev1 "k8s.io/api/storage/v1"` and `"k8s.io/apimachinery/pkg/api/resource"`, and these helpers + tests:

```go
func scClass(name string) storagev1.StorageClass {
	return storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// staticPVC is a Pending PVC with storageClassName "" (explicit static) + a size + modes.
func staticPVC(ns, name, size string, modes ...corev1.PersistentVolumeAccessMode) corev1.PersistentVolumeClaim {
	empty := ""
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &empty,
			AccessModes:      modes,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
}

// availPV is an Available, unbound PV with a storage class, size, and modes.
func availPV(name, size, sc string, modes ...corev1.PersistentVolumeAccessMode) corev1.PersistentVolume {
	return corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: sc,
			AccessModes:      modes,
			Capacity:         corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
	}
}

func onlyIssue(t *testing.T, issues []Issue) Issue {
	t.Helper()
	if len(issues) != 1 {
		t.Fatalf("want 1 issue, got %d: %+v", len(issues), issues)
	}
	return issues[0]
}

func TestAssess_MissingStorageClass_NoEvent(t *testing.T) {
	pvc := pendingPVC("shop", "data", "fast-ssd")
	got := onlyIssue(t, Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, nil))
	if got.Reason != "MissingStorageClass" {
		t.Fatalf("reason = %q", got.Reason)
	}
	if got.Detail != `references StorageClass "fast-ssd" which does not exist` {
		t.Fatalf("detail = %q", got.Detail)
	}
	if got.StorageClass != "fast-ssd" {
		t.Errorf("storageClass = %q", got.StorageClass)
	}
}

func TestAssess_MissingStorageClass_PresentSCNotFlagged(t *testing.T) {
	pvc := pendingPVC("shop", "data", "fast-ssd")
	scs := []storagev1.StorageClass{scClass("fast-ssd")}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, scs, nil); len(got) != 0 {
		t.Fatalf("a present dynamic SC with no event must not be flagged, got %+v", got)
	}
}

func TestAssess_StructuralBeatsEvent(t *testing.T) {
	pvc := pendingPVC("shop", "data", "fast-ssd")
	events := []corev1.Event{pvcEvent("shop", "data", "ProvisioningFailed", "some raw provisioner error")}
	got := onlyIssue(t, Assess([]corev1.PersistentVolumeClaim{pvc}, events, nil, nil))
	if got.Reason != "MissingStorageClass" || got.Detail != `references StorageClass "fast-ssd" which does not exist` {
		t.Fatalf("structural cause must win over the event, got %+v", got)
	}
}

func TestAssess_EventFallback_ValidDynamicSC(t *testing.T) {
	pvc := pendingPVC("shop", "data", "standard")
	scs := []storagev1.StorageClass{scClass("standard")}
	events := []corev1.Event{pvcEvent("shop", "data", "ProvisioningFailed", "quota exceeded")}
	got := onlyIssue(t, Assess([]corev1.PersistentVolumeClaim{pvc}, events, scs, nil))
	if got.Reason != "ProvisioningFailed" || got.Detail != "quota exceeded" {
		t.Fatalf("valid dynamic SC must fall through to the event, got %+v", got)
	}
}

func TestAssess_ValidDynamicSC_NoEvent_NotFlagged(t *testing.T) {
	pvc := pendingPVC("shop", "data", "standard")
	scs := []storagev1.StorageClass{scClass("standard")}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, scs, nil); len(got) != 0 {
		t.Fatalf("a normally-provisioning PVC must not be flagged, got %+v", got)
	}
}

func TestAssess_NoMatchingPV_Empty(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteOnce)
	got := onlyIssue(t, Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, nil))
	if got.Reason != "NoMatchingPV" {
		t.Fatalf("reason = %q", got.Reason)
	}
	if got.Detail != "no available PersistentVolume matches its request (10Gi, ReadWriteOnce)" {
		t.Fatalf("detail = %q", got.Detail)
	}
}

func TestAssess_NoMatchingPV_MatchingPVNotFlagged(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteOnce)
	pvs := []corev1.PersistentVolume{availPV("pv-1", "20Gi", "", corev1.ReadWriteOnce)}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, pvs); len(got) != 0 {
		t.Fatalf("a matching Available static PV must satisfy the claim, got %+v", got)
	}
}

func TestAssess_NoMatchingPV_TooSmall(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteOnce)
	pvs := []corev1.PersistentVolume{availPV("pv-1", "5Gi", "", corev1.ReadWriteOnce)}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, pvs); len(got) != 1 || got[0].Reason != "NoMatchingPV" {
		t.Fatalf("a too-small PV must not satisfy the claim, got %+v", got)
	}
}

func TestAssess_NoMatchingPV_WrongMode(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteMany)
	pvs := []corev1.PersistentVolume{availPV("pv-1", "20Gi", "", corev1.ReadWriteOnce)}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, pvs); len(got) != 1 || got[0].Reason != "NoMatchingPV" {
		t.Fatalf("a PV lacking the requested access mode must not satisfy, got %+v", got)
	}
}

func TestAssess_NoMatchingPV_BoundPVNotCandidate(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteOnce)
	pv := availPV("pv-1", "20Gi", "", corev1.ReadWriteOnce)
	pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: "other", Name: "someone-else"}
	pv.Status.Phase = corev1.VolumeBound
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, []corev1.PersistentVolume{pv}); len(got) != 1 || got[0].Reason != "NoMatchingPV" {
		t.Fatalf("a bound PV must not be a candidate, got %+v", got)
	}
}

func TestAssess_NoMatchingPV_DynamicPVNotCandidate(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteOnce)
	pvs := []corev1.PersistentVolume{availPV("pv-1", "20Gi", "standard", corev1.ReadWriteOnce)} // dynamic class
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, pvs); len(got) != 1 || got[0].Reason != "NoMatchingPV" {
		t.Fatalf("a dynamic-class PV must not satisfy a static claim, got %+v", got)
	}
}
```

Update the file's **existing** `Assess(pvcs, events)` call sites (e.g. `TestAssess_ProvisioningFailed`) to pass `nil, nil` for the two new trailing args.

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/pvchealth/`
Expected: FAIL — `too many arguments in call to Assess` / new tests undefined behavior.

- [ ] **Step 3: Write the implementation**

Rewrite `internal/pvchealth/pvchealth.go`'s imports and `Assess`, and add the helpers. Imports become:

```go
import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)
```

`Assess`:

```go
// Assess flags each Pending PVC that cannot provision or bind, naming the cause:
// a missing StorageClass or no matching PV (structural, event-independent), else
// the newest ProvisioningFailed/FailedBinding event's message. Pure and read-only.
func Assess(pvcs []corev1.PersistentVolumeClaim, events []corev1.Event, storageClasses []storagev1.StorageClass, pvs []corev1.PersistentVolume) []Issue {
	issues := make([]Issue, 0)
	for _, c := range pvcs {
		if c.Status.Phase != corev1.ClaimPending {
			continue
		}
		if reason, detail, ok := structuralCause(c, storageClasses, pvs); ok {
			issues = append(issues, Issue{
				Namespace: c.Namespace, Name: c.Name, Phase: "Pending",
				Reason: reason, Detail: detail, StorageClass: storageClass(c),
			})
			continue
		}
		ev := newestFailureEvent(events, c.Namespace, c.Name)
		if ev == nil {
			continue
		}
		issues = append(issues, Issue{
			Namespace: c.Namespace, Name: c.Name, Phase: "Pending",
			Reason: ev.Reason, Detail: ev.Message, StorageClass: storageClass(c),
		})
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Namespace != issues[j].Namespace {
			return issues[i].Namespace < issues[j].Namespace
		}
		return issues[i].Name < issues[j].Name
	})
	return issues
}

// structuralCause returns a definitive provisioning cause derived from the cluster
// graph (missing StorageClass, or no matching PV for a static claim), or ok=false
// when none applies (leaving the PVC to the event path).
func structuralCause(c corev1.PersistentVolumeClaim, storageClasses []storagev1.StorageClass, pvs []corev1.PersistentVolume) (reason, detail string, ok bool) {
	sc := c.Spec.StorageClassName
	switch {
	case sc != nil && *sc != "":
		if !classExists(*sc, storageClasses) {
			return "MissingStorageClass", fmt.Sprintf("references StorageClass %q which does not exist", *sc), true
		}
		return "", "", false
	case sc != nil && *sc == "":
		if !anyMatchingPV(c, pvs) {
			return "NoMatchingPV", fmt.Sprintf("no available PersistentVolume matches its request (%s, %s)", requestSize(c), modeList(c)), true
		}
		return "", "", false
	default: // sc == nil (default SC / ambiguous) — leave to the event path
		return "", "", false
	}
}

func classExists(name string, scs []storagev1.StorageClass) bool {
	for _, s := range scs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// anyMatchingPV reports whether some Available, unbound, static PV can satisfy the
// claim's size and access modes.
func anyMatchingPV(c corev1.PersistentVolumeClaim, pvs []corev1.PersistentVolume) bool {
	req := c.Spec.Resources.Requests[corev1.ResourceStorage]
	for _, pv := range pvs {
		if pv.Status.Phase != corev1.VolumeAvailable || pv.Spec.ClaimRef != nil {
			continue
		}
		if pv.Spec.StorageClassName != "" {
			continue // a dynamic-class PV is not a candidate for a static claim
		}
		pvCap := pv.Spec.Capacity[corev1.ResourceStorage]
		if pvCap.Cmp(req) < 0 {
			continue
		}
		if !modesSatisfied(c.Spec.AccessModes, pv.Spec.AccessModes) {
			continue
		}
		return true
	}
	return false
}

func modesSatisfied(want, have []corev1.PersistentVolumeAccessMode) bool {
	set := make(map[corev1.PersistentVolumeAccessMode]bool, len(have))
	for _, m := range have {
		set[m] = true
	}
	for _, m := range want {
		if !set[m] {
			return false
		}
	}
	return true
}

func requestSize(c corev1.PersistentVolumeClaim) string {
	if q, ok := c.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		return q.String()
	}
	return "?"
}

func modeList(c corev1.PersistentVolumeClaim) string {
	parts := make([]string, 0, len(c.Spec.AccessModes))
	for _, m := range c.Spec.AccessModes {
		parts = append(parts, string(m))
	}
	if len(parts) == 0 {
		return "?"
	}
	return strings.Join(parts, ",")
}
```

Leave `newestFailureEvent` and `storageClass` unchanged.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pvchealth/`
Expected: PASS (all tests). Then `gofmt -l internal/pvchealth/pvchealth.go internal/pvchealth/pvchealth_test.go` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/pvchealth/
git commit -m "feat(pvchealth): structural provisioning cause (missing StorageClass / no matching PV)"
```

---

### Task 2: `scan.Evaluate` — collect StorageClasses and pass them + PVs

**Files:**
- Modify: `internal/scan/scan.go` (collect StorageClasses; update the `pvchealth.Assess` call)
- Test: `internal/scan/scan_test.go` (add one integration test)

**Interfaces:**
- Consumes: `pvchealth.Assess(pvcs, events, storageClasses, pvs)` (Task 1); the existing local `pvs` (`collect.PersistentVolumes`, already collected at the `pvs, _ :=` line); `collect.StorageClasses(ctx, client)` (existing collector, cluster-scoped).

- [ ] **Step 1: Write the failing test**

Add to `internal/scan/scan_test.go` (imports `corev1`, `metav1`, the fake clientset):

```go
func TestEvaluate_PVCMissingStorageClass_NoEvent(t *testing.T) {
	// A Pending PVC referencing a StorageClass that does not exist, with NO event,
	// is flagged structurally (proves the wiring passes StorageClasses + PVs and
	// that flagging no longer requires an event).
	sc := "fast-ssd"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	cli := fake.NewSimpleClientset(pvc)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, is := range res.PVCIssues {
		if is.Namespace == "shop" && is.Name == "data" {
			found = true
			if is.Reason != "MissingStorageClass" || is.Detail != `references StorageClass "fast-ssd" which does not exist` {
				t.Fatalf("issue = %+v", is)
			}
		}
	}
	if !found {
		t.Fatalf("expected a shop/data PVC issue, got %+v", res.PVCIssues)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scan/ -run TestEvaluate_PVCMissingStorageClass_NoEvent`
Expected: FAIL — first a compile error (`pvchealth.Assess` now needs 4 args), then the assertion once wired.

- [ ] **Step 3: Write the implementation**

In `internal/scan/scan.go`, just before the `pvcIssues := pvchealth.Assess(...)` line, collect StorageClasses (the collector exists; `storageclasses` is already in the RBAC grant), then update the call:

```go
	scs, _ := collect.StorageClasses(ctx, client)
	pvcIssues := pvchealth.Assess(pvcs, pvcEvents, scs, pvs)
```

(`pvs` is already collected on the `pvs, _ := collect.PersistentVolumes(...)` line above. The StorageClasses list error is intentionally ignored — a restricted kubeconfig without the grant simply skips the structural StorageClass check, consistent with the sibling advisory collectors.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scan/`
Expected: PASS. Then `gofmt -l internal/scan/scan.go internal/scan/scan_test.go` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/scan/
git commit -m "feat(scan): collect StorageClasses and pass them to the PVC check"
```

---

### Task 3: Golden snapshot + docs

**Files:**
- Modify: `internal/report/golden_test.go` (update the `reports-data` PVC fixture)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerate)
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** Consumes the rendering behavior; the golden test renders a pre-built `Input`, so this is a fixture-text change.

- [ ] **Step 1: Update the fixture's PVC Detail to the structural form**

In `internal/report/golden_test.go`, the `PVCIssues` fixture currently is:

```go
		PVCIssues: []pvchealth.Issue{{
			Namespace: "shop", Name: "reports-data", Phase: "Pending",
			Reason: "ProvisioningFailed", Detail: `storageclass "fast-ssd" not found`, StorageClass: "fast-ssd",
		}},
```

Change it to the cleaner structural form:

```go
		PVCIssues: []pvchealth.Issue{{
			Namespace: "shop", Name: "reports-data", Phase: "Pending",
			Reason: "MissingStorageClass", Detail: `references StorageClass "fast-ssd" which does not exist`, StorageClass: "fast-ssd",
		}},
```

- [ ] **Step 2: Confirm the golden test now fails (snapshot drift)**

Run: `go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the rendered `reports-data` line now carries the new Detail.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report -run TestGoldenScanOutput -update`
Then inspect: `git diff internal/report/testdata/golden-scan.txt` — the only change must be the `reports-data` PVC line becoming `  ✗ shop/reports-data  PersistentVolumeClaim  Pending — references StorageClass "fast-ssd" which does not exist`. No other line changes (the "1 PVC failing to provision" count is unchanged).

- [ ] **Step 4: Run the full report suite**

Run: `go test ./internal/report/`
Expected: PASS.

- [ ] **Step 5: Update docs**

- `website/docs/features/diagnostics.md`: in the Pending-PVC section, note that the check now names a structural cause — `references StorageClass "X" which does not exist`, or `no available PersistentVolume matches its request (size, modes)` for a static claim — derived by correlating the PVC against the cluster's StorageClasses and PVs, and that these fire even when no `ProvisioningFailed` event is present (catching a long-stuck PVC whose event expired). Keep it accurate: the example strings must byte-match the code output.
- `README.md`: extend the Pending-PVC bullet to mention the structural StorageClass/PV root cause.
- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`, add a bullet:
  ```
  - **PVC provisioning root cause.** The Pending-PVC check now names *why* a claim
    is stuck by correlating it against the cluster's StorageClasses and PVs — it
    references a StorageClass that does not exist, or (for a static claim) no
    available PersistentVolume matches its size and access modes — and flags these
    even when no `ProvisioningFailed` event is present (catching a PVC whose event
    has expired). Read-only; reuses collected objects (no new flag or metric).
  ```
- `website/docs/roadmap.md`: add this to the Shipped list (completes the Theme-A root-cause chain with PVC → StorageClass → PV).

- [ ] **Step 6: Verify docs build (only website/ changed)**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (use the venv mkdocs if not on PATH: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, "Documentation built", no WARNING about your pages.

- [ ] **Step 7: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt website/ README.md CHANGELOG.md
git commit -m "test+docs: golden coverage and documentation for PVC provisioning root cause"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the release skill owns this. Touches `internal/pvchealth` + one collect-call + one line of `internal/scan` — no change to `internal/collect`, RBAC, Helm, or watch (StorageClasses/PVs are already collected and granted) → **LIGHTWEIGHT SMOKE** gate (a Kind cluster with a PVC referencing a nonexistent StorageClass; confirm the structural Detail renders with no event). **Minor** version bump **v0.39.0 → v0.40.0**; **patch** chart bump (no Helm template change — the bump script's default patch is correct; do NOT override to minor). Hold for the user's explicit "run release and push".
