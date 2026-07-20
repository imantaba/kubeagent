# FailedCreate ("can't create pods") Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Name the cause on a workload stuck below its desired replicas because its controller cannot *create* pods (ResourceQuota, LimitRange, or admission-webhook denial) — a blind spot today because kubeagent's detectors all operate on existing pods, and such a workload has none.

**Architecture:** A new pure, read-only package `internal/createhealth` exposes `Annotate(workloads, replicaSets, events)` that runs *after* `inventory.Prioritize` (mirroring `netpolicy`/`rollout`). It reads `FailedCreate` Warning events (fetched by a new `collect.FailedCreateEvents` collector), maps each to its workload — resolving a Deployment's ReplicaSet event back to the Deployment via owner references — and appends a `FailedCreate` finding classifying the failure mode. No changes to `report.go`, `inventory`, `explain.go`, `watch`, or RBAC.

**Tech Stack:** Go 1.26, standard library + `k8s.io/api` (`appsv1`, `corev1`), client-go fake clientset for I/O tests. No new dependencies.

## Global Constraints

- **READ-ONLY.** Only `List` calls; no create/update/patch/delete. No new RBAC (the `events` list verb is already granted).
- **No `Co-Authored-By: Claude` trailer** (or any AI attribution) on any commit.
- **TDD** — write the failing test first, watch it fail, then implement.
- **Always-on** — no CLI flag, no `watch.Config` change; runs in both `scan` and the `watch` daemon via the shared `scan.Evaluate`.
- `createhealth.Annotate` is a **pure function** of its inputs; deterministic (newest event per workload by `LastTimestamp`).
- Detectors/annotators are pure functions unit-tested with **fake objects**; I/O packages use the **fake clientset**.
- Set PATH first every task: `export PATH=$PATH:/usr/local/go/bin`.
- Spec: [docs/superpowers/specs/2026-07-20-failedcreate-check-design.md](../specs/2026-07-20-failedcreate-check-design.md).

---

## File Structure

- **Create** `internal/createhealth/createhealth.go` — the `Annotate` package (owner resolution, event→workload mapping, mode classifier).
- **Create** `internal/createhealth/createhealth_test.go` — pure unit tests over hand-built workloads/ReplicaSets/events.
- **Modify** `internal/collect/collect.go` — add `FailedCreateEvents` (mirrors `VolumeAttachEvents`).
- **Modify** `internal/collect/collect_test.go` — test the new collector.
- **Modify** `internal/scan/scan.go` — fetch the events and call `createhealth.Annotate` after `Prioritize`, before `netpolicy.Annotate`.
- **Modify** `internal/scan/scan_test.go` — integration test through `Evaluate`.
- **Modify** `internal/report/golden_test.go` + `internal/report/testdata/golden-scan.txt` — add a FailedCreate workload to the fixture and regenerate the snapshot.
- **Modify** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/quickstart.md` — document the check.

---

### Task 1: `collect.FailedCreateEvents` collector

**Files:**
- Modify: `internal/collect/collect.go` (add after `VolumeAttachEvents`, around line 104)
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Consumes: nothing new (uses the same `kubernetes.Interface`, `metav1.ListOptions`, `corev1.Event` already imported in `collect.go`).
- Produces: `func FailedCreateEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error)` — returns all `reason=FailedCreate` events in the namespace (`""` = all).

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go`:

```go
func TestFailedCreateEvents(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "api-7c9f.ev"},
		Reason:         "FailedCreate",
		Type:           "Warning",
		Message:        `pods "api-7c9f-" is forbidden: exceeded quota: compute`,
		InvolvedObject: corev1.ObjectReference{Kind: "ReplicaSet", Namespace: "shop", Name: "api-7c9f"},
	}
	client := fake.NewSimpleClientset(ev)
	got, err := FailedCreateEvents(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "FailedCreate" {
		t.Fatalf("want one FailedCreate event, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect -run TestFailedCreateEvents`
Expected: FAIL — `undefined: FailedCreateEvents`.

- [ ] **Step 3: Add the collector**

In `internal/collect/collect.go`, immediately after the `VolumeAttachEvents` function:

```go
// FailedCreateEvents lists the controller "FailedCreate" Warning events in the
// namespace ("" = all) — a Deployment's ReplicaSet, a StatefulSet, or a DaemonSet
// reporting that it cannot create pods (quota, LimitRange, admission webhook).
// Read-only; mirrors VolumeAttachEvents. Needs no permission beyond the event
// list scan already performs.
func FailedCreateEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=FailedCreate"})
	if err != nil {
		return nil, fmt.Errorf("listing FailedCreate events: %w", err)
	}
	return events.Items, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect -run TestFailedCreateEvents`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go
git commit -m "feat(collect): list FailedCreate controller events"
```

---

### Task 2: `internal/createhealth` — the Annotate package

**Files:**
- Create: `internal/createhealth/createhealth.go`
- Test: `internal/createhealth/createhealth_test.go`

**Interfaces:**
- Consumes: `inventory.Workload` (fields `Namespace`, `Name`, `Kind`, `Desired`, `Ready`, `Status`, `Findings`; method `Flagged() bool` = `len(Findings)>0 || Ready<Desired || Status=="Failed"`); `diagnose.Finding{Pod, Issue, Reason, Evidence string}`; `appsv1.ReplicaSet`; `corev1.Event`.
- Produces: `func Annotate(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, events []corev1.Event)` — mutates `workloads` in place, appending at most one `FailedCreate` finding per eligible workload.

- [ ] **Step 1: Write the failing test**

Create `internal/createhealth/createhealth_test.go`:

```go
package createhealth

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func boolPtr(b bool) *bool { return &b }

// ownedRS builds a ReplicaSet controlled by the named Deployment.
func ownedRS(ns, name, deploy string) appsv1.ReplicaSet {
	return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: deploy, Controller: boolPtr(true)}},
	}}
}

// fcEvent builds a FailedCreate event on the given involved object.
func fcEvent(kind, ns, name, msg string, secs int64) corev1.Event {
	return corev1.Event{
		Reason:         "FailedCreate",
		Type:           "Warning",
		Message:        msg,
		InvolvedObject: corev1.ObjectReference{Kind: kind, Namespace: ns, Name: name},
		LastTimestamp:  metav1.Unix(secs, 0),
	}
}

const quotaMsg = `pods "api-7c9f-" is forbidden: exceeded quota: compute, requested: requests.cpu=2, used: requests.cpu=4, limited: requests.cpu=4`

func TestAnnotate_DeploymentViaReplicaSet(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 3, Ready: 0, Status: "Degraded"}}
	rss := []appsv1.ReplicaSet{ownedRS("shop", "api-7c9f", "api")}
	evs := []corev1.Event{fcEvent("ReplicaSet", "shop", "api-7c9f", quotaMsg, 100)}

	Annotate(ws, rss, evs)

	if len(ws[0].Findings) != 1 {
		t.Fatalf("want one finding, got %+v", ws[0].Findings)
	}
	f := ws[0].Findings[0]
	if f.Issue != "FailedCreate" {
		t.Errorf("Issue = %q, want FailedCreate", f.Issue)
	}
	if !strings.Contains(f.Reason, "ResourceQuota") {
		t.Errorf("Reason = %q, want it to mention ResourceQuota", f.Reason)
	}
	if f.Evidence != quotaMsg {
		t.Errorf("Evidence = %q, want the raw event message", f.Evidence)
	}
	if f.Pod != "shop/api" {
		t.Errorf("Pod = %q, want the workload identity shop/api", f.Pod)
	}
}

func TestAnnotate_StatefulSetDirect(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "db", Name: "pg", Kind: "StatefulSet", Desired: 2, Ready: 0, Status: "Degraded"}}
	msg := `admission webhook "policy.example.com" denied the request: label required`
	evs := []corev1.Event{fcEvent("StatefulSet", "db", "pg", msg, 100)}

	Annotate(ws, nil, evs)

	if len(ws[0].Findings) != 1 || !strings.Contains(ws[0].Findings[0].Reason, "admission webhook") {
		t.Fatalf("want an admission-webhook FailedCreate finding, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_DaemonSetLimitRange(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "sys", Name: "agent", Kind: "DaemonSet", Desired: 4, Ready: 1, Status: "Degraded"}}
	msg := `pods "agent-" is forbidden: maximum cpu usage per Container is 1, but limit is 2`
	evs := []corev1.Event{fcEvent("DaemonSet", "sys", "agent", msg, 100)}

	Annotate(ws, nil, evs)

	if len(ws[0].Findings) != 1 || !strings.Contains(ws[0].Findings[0].Reason, "LimitRange") {
		t.Fatalf("want a LimitRange FailedCreate finding, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_SkipsWorkloadWithExistingFinding(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 3, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff"}}}}
	rss := []appsv1.ReplicaSet{ownedRS("shop", "api-7c9f", "api")}
	evs := []corev1.Event{fcEvent("ReplicaSet", "shop", "api-7c9f", quotaMsg, 100)}

	Annotate(ws, rss, evs)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Issue != "CrashLoopBackOff" {
		t.Fatalf("must not annotate a workload that already has a finding, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_SkipsHealthyWorkload(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 3, Ready: 3, Status: "Running"}}
	rss := []appsv1.ReplicaSet{ownedRS("shop", "api-7c9f", "api")}
	evs := []corev1.Event{fcEvent("ReplicaSet", "shop", "api-7c9f", quotaMsg, 100)}

	Annotate(ws, rss, evs)

	if len(ws[0].Findings) != 0 {
		t.Fatalf("must not annotate a healthy workload, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_NewestEventWins(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 3, Ready: 0, Status: "Degraded"}}
	rss := []appsv1.ReplicaSet{ownedRS("shop", "api-7c9f", "api")}
	evs := []corev1.Event{
		fcEvent("ReplicaSet", "shop", "api-7c9f", "old: pod creation is failing", 100),
		fcEvent("ReplicaSet", "shop", "api-7c9f", quotaMsg, 200), // newer
	}

	Annotate(ws, rss, evs)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Evidence != quotaMsg {
		t.Fatalf("want the newest event's message, got %+v", ws[0].Findings)
	}
}

func TestClassifyCreateFailure(t *testing.T) {
	cases := map[string]string{
		quotaMsg:                                    "blocked by a ResourceQuota",
		`admission webhook "x" denied the request`:  "rejected by an admission webhook",
		`maximum cpu usage per Container is 1`:       "violates a LimitRange",
		`is forbidden: some other policy`:            "forbidden by admission",
		`internal server error creating pod`:         "pod creation is failing",
	}
	for msg, want := range cases {
		if got := classifyCreateFailure(msg); got != want {
			t.Errorf("classifyCreateFailure(%q) = %q, want %q", msg, got, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/createhealth`
Expected: FAIL — build error (`undefined: Annotate`, `undefined: classifyCreateFailure`).

- [ ] **Step 3: Implement the package**

Create `internal/createhealth/createhealth.go`:

```go
// Package createhealth attaches a "FailedCreate" finding to a workload whose
// controller cannot create pods — a ResourceQuota, LimitRange, or admission
// webhook is rejecting them, so the workload sits below its desired replicas
// with no pods to diagnose. Pure and read-only: the caller supplies the
// assembled+prioritized workloads, the ReplicaSets (to resolve a Deployment's
// ReplicaSet events back to the Deployment), and the FailedCreate events.
// Mirrors netpolicy/rollout.Annotate.
package createhealth

import (
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate appends a "FailedCreate" finding to each flagged workload that has no
// existing finding and whose controller has a FailedCreate event. It mutates the
// slice elements in place. A Deployment's FailedCreate event lands on its
// ReplicaSet, so replicaSets resolves that back to the Deployment; StatefulSet
// and DaemonSet events are matched directly.
func Annotate(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, events []corev1.Event) {
	rsToDeploy := map[string]string{}
	for _, rs := range replicaSets {
		if name, ok := ownedByDeployment(rs); ok {
			rsToDeploy[rs.Namespace+"/"+rs.Name] = name
		}
	}
	byWorkload := map[string]*corev1.Event{}
	for i := range events {
		e := &events[i]
		if e.Reason != "FailedCreate" {
			continue
		}
		key := workloadKeyForEvent(e, rsToDeploy)
		if key == "" {
			continue
		}
		if best, ok := byWorkload[key]; !ok || e.LastTimestamp.Time.After(best.LastTimestamp.Time) {
			byWorkload[key] = e
		}
	}
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() || len(w.Findings) > 0 {
			continue
		}
		e, ok := byWorkload[w.Kind+"/"+w.Namespace+"/"+w.Name]
		if !ok {
			continue
		}
		w.Findings = append(w.Findings, diagnose.Finding{
			Pod:      w.Namespace + "/" + w.Name,
			Issue:    "FailedCreate",
			Reason:   "the controller cannot create pods — " + classifyCreateFailure(e.Message),
			Evidence: e.Message,
		})
	}
}

// ownedByDeployment returns the owning Deployment's name if the ReplicaSet is
// controlled by one.
func ownedByDeployment(rs appsv1.ReplicaSet) (string, bool) {
	for _, o := range rs.OwnerReferences {
		if o.Kind == "Deployment" && o.Controller != nil && *o.Controller {
			return o.Name, true
		}
	}
	return "", false
}

// workloadKeyForEvent maps a FailedCreate event to a workload key
// ("Kind/namespace/name"), resolving a ReplicaSet to its owning Deployment. It
// returns "" for an involved-object kind this check does not track (e.g. a Job,
// which internal/batchhealth owns).
func workloadKeyForEvent(e *corev1.Event, rsToDeploy map[string]string) string {
	io := e.InvolvedObject
	switch io.Kind {
	case "ReplicaSet":
		if dep, ok := rsToDeploy[io.Namespace+"/"+io.Name]; ok {
			return "Deployment/" + io.Namespace + "/" + dep
		}
		return "ReplicaSet/" + io.Namespace + "/" + io.Name
	case "StatefulSet", "DaemonSet":
		return io.Kind + "/" + io.Namespace + "/" + io.Name
	default:
		return ""
	}
}

// classifyCreateFailure names the pod-creation failure mode from the controller's
// FailedCreate event message. Order matters: a quota/LimitRange denial is also
// phrased as "forbidden", so those are matched before the generic forbidden case.
func classifyCreateFailure(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "exceeded quota"):
		return "blocked by a ResourceQuota"
	case strings.Contains(m, "admission webhook"):
		return "rejected by an admission webhook"
	case strings.Contains(m, "limitrange"), strings.Contains(m, "minimum "), strings.Contains(m, "maximum "):
		return "violates a LimitRange"
	case strings.Contains(m, "forbidden"):
		return "forbidden by admission"
	default:
		return "pod creation is failing"
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/createhealth -v`
Expected: PASS (all `TestAnnotate_*` and `TestClassifyCreateFailure`).

- [ ] **Step 5: Commit**

```bash
git add internal/createhealth/createhealth.go internal/createhealth/createhealth_test.go
git commit -m "feat(createhealth): flag workloads whose controller can't create pods"
```

---

### Task 3: Wire `createhealth.Annotate` into `scan.Evaluate`

**Files:**
- Modify: `internal/scan/scan.go` (imports; and between the `podLabels` loop and `netpolicy.Annotate`, around lines 185-186)
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `collect.FailedCreateEvents` (Task 1), `createhealth.Annotate` (Task 2), existing `inputs.ReplicaSets []appsv1.ReplicaSet`, `result.Workloads`.
- Produces: no signature change to `Evaluate`; a `FailedCreate` finding now appears on `result.Inventory.Workloads[i].Findings`.

- [ ] **Step 1: Write the failing integration test**

Add to `internal/scan/scan_test.go` (the file already imports `appsv1`, `corev1`, `metav1`, `fake`, and has the `p32` helper for `*int32`):

```go
func TestEvaluate_FlagsFailedCreate(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(3)}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-7c9f",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", Controller: boolp(true)}}}}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "api-7c9f.ev"},
		Reason:         "FailedCreate",
		Type:           "Warning",
		Message:        `pods "api-7c9f-" is forbidden: exceeded quota: compute`,
		InvolvedObject: corev1.ObjectReference{Kind: "ReplicaSet", Namespace: "shop", Name: "api-7c9f"},
	}
	cli := fake.NewSimpleClientset(node, dep, rs, ev)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "FailedCreate" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a FailedCreate finding, got %+v", res.Inventory.Workloads)
	}
}

```

Note: this reuses the existing `boolp(b bool) *bool` and `p32(i int32) *int32` helpers already defined in `scan_test.go` — do **not** add new pointer helpers.

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan -run TestEvaluate_FlagsFailedCreate`
Expected: FAIL — no `FailedCreate` finding (wiring not present yet).

- [ ] **Step 3: Add the import**

In `internal/scan/scan.go`, add to the import block (with the other `internal/...` imports, keep alphabetical grouping):

```go
	"github.com/imantaba/kubeagent/internal/createhealth"
```

- [ ] **Step 4: Add the fetch + Annotate call**

In `internal/scan/scan.go`, the current block reads:

```go
	netpolicy.Annotate(result.Workloads, podLabels, nps)
	rollout.Annotate(result.Workloads, inputs.ReplicaSets, time.Now())
```

Insert the two new lines immediately **before** `netpolicy.Annotate` so a `FailedCreate` finding claims the workload and `netpolicy` does not add a redundant hint:

```go
	failedCreateEvents, _ := collect.FailedCreateEvents(ctx, client, opts.Namespace)
	createhealth.Annotate(result.Workloads, inputs.ReplicaSets, failedCreateEvents)
	netpolicy.Annotate(result.Workloads, podLabels, nps)
	rollout.Annotate(result.Workloads, inputs.ReplicaSets, time.Now())
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan`
Expected: PASS (including the new test and all existing `Evaluate` tests).

- [ ] **Step 6: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): run the FailedCreate check after Prioritize"
```

---

### Task 4: Golden fixture + snapshot

**Files:**
- Modify: `internal/report/golden_test.go` (the `goldenWorkloads()` slice, after the `nightly-report` CronJob entry, before the closing `}`)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated)

**Interfaces:**
- Consumes: `inventory.Workload`, `diagnose.Finding` (as used throughout `goldenWorkloads()`).
- Produces: the golden snapshot now includes a `FailedCreate` workload, exercising the new finding through the real text renderer.

- [ ] **Step 1: Add a FailedCreate workload to the fixture**

In `internal/report/golden_test.go`, inside `goldenWorkloads()`, add this entry as the last element of the returned slice (after the `nightly-report` CronJob, before the closing `}` at the end of the slice literal):

```go
		{Namespace: "shop", Name: "storefront", Kind: "Deployment", Desired: 3, Ready: 0, Status: "Degraded",
			Findings: []diagnose.Finding{{Pod: "shop/storefront", Issue: "FailedCreate",
				Reason:   "the controller cannot create pods — blocked by a ResourceQuota",
				Evidence: `pods "storefront-7c9f-" is forbidden: exceeded quota: compute, requested: requests.cpu=2, used: requests.cpu=4, limited: requests.cpu=4`}}},
```

- [ ] **Step 2: Run the golden test to see it fail (fixture changed, snapshot stale)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput`
Expected: FAIL — output changed (the new `storefront` block is not yet in `golden-scan.txt`).

- [ ] **Step 3: Regenerate the snapshot**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput -update`
Expected: PASS (writes the new `testdata/golden-scan.txt`).

- [ ] **Step 4: Inspect the regenerated snapshot**

Run: `grep -n -A2 "storefront" internal/report/testdata/golden-scan.txt`
Expected: shows the `storefront  Deployment  0/3 Degraded` line, a `⚠ FailedCreate: the controller cannot create pods — blocked by a ResourceQuota` line, and the `↳ pods "storefront-7c9f-" is forbidden: exceeded quota...` evidence line. Confirm it renders through the generic finding block (no `report.go` change needed).

- [ ] **Step 5: Run the full report suite (guards stay green)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report`
Expected: PASS — `TestGoldenScanOutput` and `TestGoldenInputCoversAllSections` (the distinct-failure-modes count rises to include `FailedCreate`).

- [ ] **Step 6: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover a FailedCreate workload in the golden snapshot"
```

---

### Task 5: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md` (new subsection + Status line)
- Modify: `README.md` (detector bullet + two summary lines)
- Modify: `CHANGELOG.md` (`[Unreleased]` → `### Added`)
- Modify: `website/docs/quickstart.md` (intro detector list + curated example output)

**Interfaces:** none (docs only). No code changes; `go build`/`go test` remain green.

- [ ] **Step 1: Add the diagnostics subsection**

In `website/docs/features/diagnostics.md`, insert a new subsection immediately after the `### Job / CronJob failures` block (which ends before `### Node reservations`):

```markdown
### FailedCreate (controller can't create pods)

A workload can sit below its desired replicas with **no pods at all** when its
controller is being denied pod *creation* — a `ResourceQuota` is exhausted, a
`LimitRange` rejects the pod's resources, or an admission webhook blocks it. The
pod-level detectors see nothing (there is no pod), so the workload would
otherwise show only `0/N Degraded` with no cause. kubeagent reads the
controller's `FailedCreate` events and names the cause on the workload — e.g.
`⚠ FailedCreate: the controller cannot create pods — blocked by a ResourceQuota`,
with the raw admission message as evidence. A Deployment's event lands on its
ReplicaSet and is resolved back to the Deployment; StatefulSets and DaemonSets
are matched directly. Read-only, always-on, no new RBAC.
```

- [ ] **Step 2: Update the diagnostics Status line**

In `website/docs/features/diagnostics.md`, the `## Status` paragraph ends with `...and failed Jobs/CronJobs, in text or JSON.` Change that tail to:

```markdown
...failed Jobs/CronJobs, and controllers that cannot create pods (FailedCreate), in text or JSON.
```

- [ ] **Step 3: Add the README detector bullet**

In `README.md`, in the detector list (after the `- **JobFailed** — ...` bullet), add:

```markdown
- **FailedCreate** — a workload whose controller cannot create pods because a
  ResourceQuota, LimitRange, or admission webhook is rejecting them.
```

- [ ] **Step 4: Update the two README summary lines**

In `README.md`, the summary sentence that ends `...and failed Jobs/CronJobs, in text or JSON.` — change its tail to:

```markdown
...failed Jobs/CronJobs, and pod-creation denials (FailedCreate), in text or JSON.
```

- [ ] **Step 5: Add the CHANGELOG entry**

In `CHANGELOG.md`, under `## [Unreleased]` → `### Added`, add as the first bullet:

```markdown
- **"Can't create pods" (FailedCreate) check.** `scan` now flags a workload stuck below its
  desired replicas because its controller cannot create pods — a `ResourceQuota`,
  `LimitRange`, or admission webhook is rejecting them — naming the cause on the workload
  (e.g. "blocked by a ResourceQuota") with the admission message as evidence. Covers
  Deployments (via their ReplicaSet), StatefulSets, and DaemonSets. Read-only, always-on,
  no new RBAC.
```

- [ ] **Step 6: Update the quickstart intro + curated example**

In `website/docs/quickstart.md`: (a) in the intro sentence listing detectors (around line 5), append `FailedCreate` to the list in the same style. (b) In the curated example scan output block, add a FailedCreate workload after the last `✗ shop/...` entry, matching the surrounding two-space-indented format:

```text
✗ shop/storefront  Deployment  0/3 Degraded
    ⚠ FailedCreate: the controller cannot create pods — blocked by a ResourceQuota
      ↳ pods "storefront-7c9f-" is forbidden: exceeded quota: compute, requested: requests.cpu=2, used: requests.cpu=4, limited: requests.cpu=4
```

- [ ] **Step 7: Verify the docs build (if mkdocs is available)**

Run: `cd website && mkdocs build --strict -f mkdocs.yml 2>&1 | tail -5; cd ..`
Expected: "Documentation built" with no `WARNING` lines about these pages. (If `mkdocs` is not installed, skip — this is a strict-lint convenience, not a gate; note it in the report.)

- [ ] **Step 8: Final build + full test suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 9: Commit**

```bash
git add website/docs/features/diagnostics.md README.md CHANGELOG.md website/docs/quickstart.md
git commit -m "docs: document the FailedCreate pod-creation check"
```

---

## Notes for the executor

- **Release gate (post-merge, not part of these tasks):** this feature touches `internal/collect`, so the release runs the **full chaos gate** (`./chaos/run.sh --recreate`), not the lightweight smoke. Version bump is a **minor** (v0.27.0 → v0.28.0).
- **No new RBAC:** the daemon's `events` list grant already covers `FailedCreateEvents`; do not touch `deploy/` or Helm RBAC.
- **Ordering matters:** `createhealth.Annotate` must run *before* `netpolicy.Annotate` (both guard on `!Flagged() || len(Findings)>0`), so the FailedCreate finding claims the workload first.
