# Pending-PVC / storage-provisioning check — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag a PersistentVolumeClaim stuck `Pending` because provisioning/binding failed, naming the cause from its `ProvisioningFailed`/`FailedBinding` events, and render it in NEEDS ATTENTION.

**Architecture:** A new pure `internal/pvchealth` package (`Assess(pvcs, events) []Issue`) mirrors `svchealth`/`ingresshealth`. A new `collect.PVCEvents` lists PVC events; `scan.Evaluate` assesses them into `Result.PVCIssues`; `report` renders them in NEEDS ATTENTION and counts them in the `Needs attention:` summary. Event-based (like `VolumeAttachError`), so the normal `WaitForFirstConsumer` state — which emits no failure event — is never flagged.

**Tech Stack:** Go 1.26, standard library + `k8s.io/api/core/v1`, client-go fake clientset.

## Global Constraints

- **Read-only; NO new RBAC.** Lists PVCs (already listed by `pvcreclaim`) and events (already listed by `VolumeAttachEvents`) — one new event `List`, same `events` verb.
- **Core, always-on** — runs in both the CLI `scan` and the `watch` daemon via the shared `scan.Evaluate`. No opt-in flag, no `watch.Config` change.
- `Assess` is **pure** and deterministic: newest event by `LastTimestamp`; issues sorted by `(Namespace, Name)`.
- **Advisory / P2:** rendered in NEEDS ATTENTION; does NOT change the P1 cluster verdict.
- `explain.go`, `pvcreclaim`, `watch`, and all deploy/RBAC/Helm files stay **unchanged**. PVC issues are NOT added to the `--explain` prompt (v1).
- **Exact names/strings (verbatim):**
  - Package `internal/pvchealth`; type `pvchealth.Issue`; func `pvchealth.Assess`; helper `newestFailureEvent`.
  - Failure reasons matched: `ProvisioningFailed`, `FailedBinding`. `Issue.Phase` == `"Pending"`.
  - `collect.PVCEvents` with `FieldSelector: "involvedObject.kind=PersistentVolumeClaim"`.
  - `scan.Result.PVCIssues []pvchealth.Issue`; JSON field `pvcIssues,omitempty`.
  - Report line: `"  ✗ %s/%s  PersistentVolumeClaim  %s — %s\n"` (Namespace, Name, Phase, Detail).
  - Summary part: `"%d %s failing to provision"` with `plural(n, "PVC", "PVCs")`.
- **TDD** — failing test first. **No `Co-Authored-By: Claude` trailer** on any commit.

---

### Task 1: `internal/pvchealth` — the Assess package

**Files:**
- Create: `internal/pvchealth/pvchealth.go`
- Test: `internal/pvchealth/pvchealth_test.go`

**Interfaces:**
- Produces: `type Issue struct{ Namespace, Name, Phase, Reason, Detail, StorageClass string }` and `func Assess(pvcs []corev1.PersistentVolumeClaim, events []corev1.Event) []Issue`. Later tasks consume both.

- [ ] **Step 1: Write the failing tests**

Create `internal/pvchealth/pvchealth_test.go`:

```go
package pvchealth

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pendingPVC(ns, name, sc string) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
}

func pvcEvent(ns, name, reason, message string) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: name + ".ev"},
		Reason:         reason,
		Type:           "Warning",
		Message:        message,
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: ns, Name: name},
	}
}

func TestAssess_ProvisioningFailed(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{pendingPVC("shop", "data-pvc", "fast")}
	events := []corev1.Event{pvcEvent("shop", "data-pvc", "ProvisioningFailed", `storageclass "fast" not found`)}
	got := Assess(pvcs, events)
	if len(got) != 1 {
		t.Fatalf("want 1 issue, got %d", len(got))
	}
	if got[0].Reason != "ProvisioningFailed" || got[0].Detail != `storageclass "fast" not found` {
		t.Errorf("Reason/Detail = %q/%q", got[0].Reason, got[0].Detail)
	}
	if got[0].Phase != "Pending" || got[0].StorageClass != "fast" {
		t.Errorf("Phase/StorageClass = %q/%q", got[0].Phase, got[0].StorageClass)
	}
}

func TestAssess_FailedBinding(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{pendingPVC("shop", "legacy", "")}
	events := []corev1.Event{pvcEvent("shop", "legacy", "FailedBinding", "no persistent volumes available for this claim and no storage class is set")}
	got := Assess(pvcs, events)
	if len(got) != 1 || got[0].Reason != "FailedBinding" {
		t.Fatalf("want 1 FailedBinding issue, got %+v", got)
	}
}

func TestAssess_BoundPVCSkipped(t *testing.T) {
	pvc := pendingPVC("shop", "data-pvc", "fast")
	pvc.Status.Phase = corev1.ClaimBound
	events := []corev1.Event{pvcEvent("shop", "data-pvc", "ProvisioningFailed", "stale")}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, events); len(got) != 0 {
		t.Errorf("a Bound PVC must not be flagged, got %+v", got)
	}
}

func TestAssess_WaitForFirstConsumerSkipped(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{pendingPVC("shop", "data-pvc", "local-path")}
	ev := pvcEvent("shop", "data-pvc", "WaitForFirstConsumer", "waiting for first consumer to be created before binding")
	ev.Type = "Normal"
	if got := Assess(pvcs, []corev1.Event{ev}); len(got) != 0 {
		t.Errorf("a WaitForFirstConsumer Pending PVC must not be flagged, got %+v", got)
	}
}

func TestAssess_CorrelationAndOrder(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{
		pendingPVC("shop", "b-pvc", "fast"),
		pendingPVC("shop", "a-pvc", "fast"),
	}
	events := []corev1.Event{
		pvcEvent("shop", "a-pvc", "ProvisioningFailed", "a failed"),
		pvcEvent("shop", "b-pvc", "ProvisioningFailed", "b failed"),
		pvcEvent("other", "a-pvc", "ProvisioningFailed", "wrong namespace"),
	}
	got := Assess(pvcs, events)
	if len(got) != 2 {
		t.Fatalf("want 2 issues, got %d: %+v", len(got), got)
	}
	if got[0].Name != "a-pvc" || got[1].Name != "b-pvc" {
		t.Errorf("issues must be sorted by name, got %q then %q", got[0].Name, got[1].Name)
	}
	if got[0].Detail != "a failed" {
		t.Errorf("a-pvc correlated to the wrong event: %q", got[0].Detail)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/pvchealth/`
Expected: FAIL — `undefined: Assess`.

- [ ] **Step 3: Write the package**

Create `internal/pvchealth/pvchealth.go`:

```go
// Package pvchealth flags PersistentVolumeClaims stuck Pending because provisioning
// or binding failed, reading the PVC's ProvisioningFailed/FailedBinding events. Pure
// and read-only: the caller supplies the PVCs and events. Event-based, like the
// attach-time VolumeAttachError check — a Pending PVC with no failure event (Bound, or
// WaitForFirstConsumer waiting for a pod) is never flagged.
package pvchealth

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// Issue is one PVC stuck Pending because provisioning/binding failed.
type Issue struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Phase        string `json:"phase"`                  // "Pending"
	Reason       string `json:"reason"`                 // "ProvisioningFailed" | "FailedBinding"
	Detail       string `json:"detail"`                 // the event message (the cause)
	StorageClass string `json:"storageClass,omitempty"`
}

// Assess flags each Pending PVC that has a provisioning/binding failure event.
func Assess(pvcs []corev1.PersistentVolumeClaim, events []corev1.Event) []Issue {
	issues := make([]Issue, 0)
	for _, c := range pvcs {
		if c.Status.Phase != corev1.ClaimPending {
			continue
		}
		ev := newestFailureEvent(events, c.Namespace, c.Name)
		if ev == nil {
			continue
		}
		issues = append(issues, Issue{
			Namespace:    c.Namespace,
			Name:         c.Name,
			Phase:        "Pending",
			Reason:       ev.Reason,
			Detail:       ev.Message,
			StorageClass: storageClass(c),
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

// newestFailureEvent returns the most recent ProvisioningFailed/FailedBinding event
// (by LastTimestamp) for the named PVC, or nil.
func newestFailureEvent(events []corev1.Event, namespace, name string) *corev1.Event {
	var best *corev1.Event
	for i := range events {
		e := &events[i]
		if e.InvolvedObject.Kind != "PersistentVolumeClaim" ||
			e.InvolvedObject.Namespace != namespace || e.InvolvedObject.Name != name {
			continue
		}
		if e.Reason != "ProvisioningFailed" && e.Reason != "FailedBinding" {
			continue
		}
		if best == nil || e.LastTimestamp.After(best.LastTimestamp.Time) {
			best = e
		}
	}
	return best
}

func storageClass(c corev1.PersistentVolumeClaim) string {
	if c.Spec.StorageClassName == nil {
		return ""
	}
	return *c.Spec.StorageClassName
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/pvchealth/ -v`
Expected: PASS (all 5). Then `gofmt -l internal/pvchealth/pvchealth.go` (nothing) and `go vet ./internal/pvchealth/`.

- [ ] **Step 5: Commit**

```bash
git add internal/pvchealth/pvchealth.go internal/pvchealth/pvchealth_test.go
git commit -m "feat(pvchealth): flag Pending PVCs with provisioning/binding failures"
```

---

### Task 2: `collect.PVCEvents` + wire into scan.Evaluate

**Files:**
- Modify: `internal/collect/collect.go` (add `PVCEvents`, near `VolumeAttachEvents`)
- Test: `internal/collect/collect_test.go` (add `TestPVCEvents`)
- Modify: `internal/scan/scan.go` (import `pvchealth`; `Result.PVCIssues`; fetch+assess)
- Test: `internal/scan/scan_test.go` (add `TestEvaluate_FlagsPendingPVC`)

**Interfaces:**
- Consumes: `pvchealth.Assess`, `pvchealth.Issue` (Task 1); existing `collect.PersistentVolumeClaims`.
- Produces: `collect.PVCEvents(ctx, client, namespace) ([]corev1.Event, error)`; `scan.Result.PVCIssues []pvchealth.Issue`.

- [ ] **Step 1: Write the failing collect test**

Add to `internal/collect/collect_test.go`:

```go
func TestPVCEvents(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "data-pvc.ev"},
		Reason:         "ProvisioningFailed",
		Type:           "Warning",
		Message:        `storageclass "fast" not found`,
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data-pvc"},
	}
	client := fake.NewSimpleClientset(ev)
	got, err := PVCEvents(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "ProvisioningFailed" {
		t.Errorf("want 1 PVC event, got %+v", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/collect/ -run TestPVCEvents`
Expected: FAIL — `undefined: PVCEvents`.

- [ ] **Step 3: Add the collector**

In `internal/collect/collect.go`, after `UnhealthyEvents`, add:

```go
// PVCEvents lists events involving PersistentVolumeClaims in the namespace (""=all).
// Read-only; pvchealth.Assess filters to the provisioning/binding failure reasons.
func PVCEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=PersistentVolumeClaim"})
	if err != nil {
		return nil, fmt.Errorf("listing PVC events: %w", err)
	}
	return events.Items, nil
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/collect/ -run TestPVCEvents`
Expected: PASS.

- [ ] **Step 5: Write the failing scan integration test**

Add to `internal/scan/scan_test.go`:

```go
func TestEvaluate_FlagsPendingPVC(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	sc := "fast"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data-pvc"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "data-pvc.ev"},
		Reason:         "ProvisioningFailed",
		Type:           "Warning",
		Message:        `storageclass "fast" not found`,
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data-pvc"},
	}
	cli := fake.NewSimpleClientset(node, pvc, ev)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.PVCIssues) != 1 || res.PVCIssues[0].Name != "data-pvc" {
		t.Errorf("expected 1 PVCIssue for data-pvc, got %+v", res.PVCIssues)
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/scan/ -run TestEvaluate_FlagsPendingPVC`
Expected: FAIL — `res.PVCIssues` undefined (field not yet added) / compile error.

- [ ] **Step 7: Wire into `scan.Evaluate`**

In `internal/scan/scan.go`:
1. Add the import `"github.com/imantaba/kubeagent/internal/pvchealth"` (with the other internal imports).
2. Add `PVCIssues []pvchealth.Issue` to the `Result` struct (after `IngressIssues`).
3. In `Evaluate`, right after the existing `pvcReclaim := pvcreclaim.Assess(pvcs, pvs)` line, add:

```go
	pvcEvents, _ := collect.PVCEvents(ctx, client, opts.Namespace)
	pvcIssues := pvchealth.Assess(pvcs, pvcEvents)
```

4. Add `PVCIssues: pvcIssues,` to the `return Result{...}` literal.

- [ ] **Step 8: Run the scan + collect tests**

Run: `go test ./internal/scan/ ./internal/collect/ ./internal/pvchealth/`
Expected: PASS. Then `gofmt -l internal/collect/collect.go internal/scan/scan.go` (nothing) and `go vet ./internal/scan/`.

- [ ] **Step 9: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): collect PVC events and assess Pending-PVC provisioning failures"
```

---

### Task 3: Render PVC issues in the report

**Files:**
- Modify: `internal/report/report.go` (import; `inventoryReport` + `Input` fields; JSON encode; `hasAttention`; NEEDS ATTENTION block; `attentionLine`; new `printPVCIssues`)
- Modify: `main.go` (map `res.PVCIssues` into `report.Input`)
- Test: `internal/report/report_test.go` (add `TestPrintInventory_ShowsPVCIssues`)

**Interfaces:**
- Consumes: `pvchealth.Issue`, `scan.Result.PVCIssues` (Tasks 1–2).

- [ ] **Step 1: Write the failing report test**

Add to `internal/report/report_test.go` (add the `pvchealth` import):

```go
func TestPrintInventory_ShowsPVCIssues(t *testing.T) {
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"},
		PVCIssues: []pvchealth.Issue{{
			Namespace: "shop", Name: "data-pvc", Phase: "Pending",
			Reason: "ProvisioningFailed", Detail: `storageclass "fast" not found`, StorageClass: "fast",
		}},
	}
	var buf bytes.Buffer
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `✗ shop/data-pvc  PersistentVolumeClaim  Pending — storageclass "fast" not found`) {
		t.Errorf("PVC issue not rendered in NEEDS ATTENTION:\n%s", out)
	}
	if !strings.Contains(out, "1 PVC failing to provision") {
		t.Errorf("attention summary missing the PVC count:\n%s", out)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestPrintInventory_ShowsPVCIssues`
Expected: FAIL — `Input` has no field `PVCIssues` (compile error).

- [ ] **Step 3: Add the fields, rendering, and summary count**

In `internal/report/report.go`:

1. Add the import `"github.com/imantaba/kubeagent/internal/pvchealth"`.
2. In the `inventoryReport` struct, after the `IngressIssues` field, add:

```go
	PVCIssues          []pvchealth.Issue           `json:"pvcIssues,omitempty"`
```

3. In the `"json"` case of `PrintInventory`, after `IngressIssues: in.IngressIssues,`, add:

```go
			PVCIssues:          in.PVCIssues,
```

4. In the `Input` struct, after the `IngressIssues` field, add:

```go
	PVCIssues          []pvchealth.Issue
```

5. Extend `hasAttention` to include PVC issues:

```go
	hasAttention := len(in.Result.Workloads) > 0 || len(real) > 0 || len(in.CredentialWarnings) > 0 || hasDisk || len(in.IngressIssues) > 0 || len(in.PVCIssues) > 0
```

6. In the NEEDS ATTENTION block, right after the `printIngressIssues(in.IngressIssues, w)` call, add:

```go
		if err := printPVCIssues(in.PVCIssues, w); err != nil {
			return err
		}
```

7. In `attentionLine`, after the ingress-routes `parts` append, add:

```go
	if n := len(in.PVCIssues); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s failing to provision", n, plural(n, "PVC", "PVCs")))
	}
```

8. Add the new function next to `printIngressIssues`:

```go
// printPVCIssues lists PersistentVolumeClaims stuck Pending because provisioning failed.
func printPVCIssues(issues []pvchealth.Issue, w io.Writer) error {
	for _, iss := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  PersistentVolumeClaim  %s — %s\n", iss.Namespace, iss.Name, iss.Phase, iss.Detail); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Wire `main.go`**

In `main.go`'s `report.Input{...}` literal, after `IngressIssues: res.IngressIssues,`, add:

```go
		PVCIssues:          res.PVCIssues,
```

- [ ] **Step 5: Run the report test + build**

Run: `go test ./internal/report/ -run TestPrintInventory_ShowsPVCIssues -v` (PASS), then `go build ./...`, `gofmt -l internal/report/report.go main.go` (nothing), `go vet ./internal/report/`.

- [ ] **Step 6: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go main.go
git commit -m "feat(report): render Pending-PVC provisioning failures in NEEDS ATTENTION"
```

---

### Task 4: Show a PVC issue in the golden snapshot

**Files:**
- Modify: `internal/report/golden_test.go` (`goldenInput` literal; the coverage assertion)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated, not hand-edited)

**Interfaces:** Consumes `pvchealth.Issue` (Task 1).

- [ ] **Step 1: Add a PVC issue to the fixture + guard it**

In `internal/report/golden_test.go`:

1. In the `Input{...}` literal returned by `goldenInput`, add this field (anywhere in the literal, e.g. right after the `Result:` line):

```go
		PVCIssues: []pvchealth.Issue{{
			Namespace: "shop", Name: "reports-data", Phase: "Pending",
			Reason: "ProvisioningFailed", Detail: `storageclass "fast-ssd" not found`, StorageClass: "fast-ssd",
		}},
```

2. Add the `pvchealth` import to `golden_test.go`.

3. In `TestGoldenInputCoversAllSections`, extend the completeness assertion to require a PVC issue — add `|| len(in.PVCIssues) == 0` to the existing `if` condition so the golden stays comprehensive.

- [ ] **Step 2: Run the golden test to verify it fails**

Run: `go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the rendered output now has the `reports-data` PVC line and the `PVC failing to provision` summary count not yet in `golden-scan.txt`.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report/ -run TestGoldenScanOutput -update`
Then inspect `git diff internal/report/testdata/golden-scan.txt` — it must show ONLY: (a) a new `  ✗ shop/reports-data  PersistentVolumeClaim  Pending — storageclass "fast-ssd" not found` line in NEEDS ATTENTION, and (b) the `Needs attention:` summary line gaining `· 1 PVC failing to provision`. If anything else changed, STOP and report it.

- [ ] **Step 4: Run the full report suite twice (determinism)**

Run: `go test ./internal/report/ && go test ./internal/report/`
Expected: PASS both times. Then `go test ./...` once.

- [ ] **Step 5: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover a Pending-PVC issue in the golden scan snapshot"
```

---

### Task 5: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md` (new subsection; `## Status` list)
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `website/docs/quickstart.md` (intro paragraph — the advisory-checks sentence)
- Modify: `README.md` (detector/checks list)

- [ ] **Step 1: diagnostics.md**

Add a subsection after the `### Ingress route health` block (before `### Node heartbeat freshness`):

```markdown
### Pending PVC (storage provisioning)

`scan` flags a PersistentVolumeClaim stuck **Pending** because provisioning or binding
failed, reading the PVC's `ProvisioningFailed` / `FailedBinding` events and naming the
cause — e.g. `✗ shop/data-pvc  PersistentVolumeClaim  Pending — storageclass "fast" not
found`. It is the provision-time complement to `VolumeAttachError` (attach-time). Like
that check it is **event-based**, so a PVC that is merely Pending under
`WaitForFirstConsumer` (waiting for a pod to consume it) — which emits no failure event
— is never flagged. It appears in **NEEDS ATTENTION** and JSON `pvcIssues` but is advisory
(it does not change the cluster verdict). Read-only; listing PVCs and events needs no
extra permission.
```

Then add Pending PVCs to the advisory-checks list in the `## Status`-area prose if one enumerates them (search `diagnostics.md` for the sentence listing `broken Ingress routes, Services with no endpoints`-style checks and add `PVCs stuck provisioning`; if no such single sentence exists, skip — the subsection above suffices).

- [ ] **Step 2: CHANGELOG.md**

Under `## [Unreleased]` → `### Added` (create the sub-header if empty), add:

```markdown
- **Pending-PVC provisioning check.** `scan` flags a PersistentVolumeClaim stuck
  `Pending` because provisioning/binding failed (`ProvisioningFailed` / `FailedBinding`
  events), naming the cause and rendering it in NEEDS ATTENTION (and JSON `pvcIssues`).
  Event-based like `VolumeAttachError`, so the normal `WaitForFirstConsumer` state is
  never flagged. Read-only, always-on, no new RBAC; advisory (does not change the verdict).
```

- [ ] **Step 3: quickstart.md**

In the intro paragraph, the advisory-checks sentence lists `broken Ingress routes, Services with no endpoints, credentials stored in the clear, PVCs on a `Delete` reclaim policy, …`. Add PVC provisioning to it, e.g. change `…PVCs on a \`Delete\` reclaim policy,` to `…PVCs on a \`Delete\` reclaim policy, PVCs stuck provisioning,`.

- [ ] **Step 4: README.md**

In the `## Status`-area summary sentence and/or the advisory-checks bullet list, add a mention of the Pending-PVC provisioning check in the same style as the neighbouring `Ingress route health` / `Service endpoints` entries. (Run `grep -n "Ingress\|no endpoints\|reclaim" README.md` to locate the right spot.)

- [ ] **Step 5: Verify docs build**

Run: `cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml --site-dir /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/site-pvc`
Expected: exit 0, "Documentation built", no `WARNING` lines about these pages. Then `export PATH=$PATH:/usr/local/go/bin && go build ./...`.

- [ ] **Step 6: Commit**

```bash
git add website/docs/features/diagnostics.md CHANGELOG.md website/docs/quickstart.md README.md
git commit -m "docs: document the Pending-PVC provisioning check"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && go test ./...
gofmt -l internal/pvchealth/pvchealth.go internal/collect/collect.go internal/scan/scan.go internal/report/report.go main.go
go test ./internal/report -run TestGoldenScanOutput   # run twice: deterministic
```

All packages pass; gofmt clean on the touched files; golden stable. Confirm no `Co-Authored-By` trailer: `git log --format='%(trailers)' main..HEAD` prints nothing.
