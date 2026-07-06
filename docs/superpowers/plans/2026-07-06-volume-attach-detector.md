# Volume-attach (Multi-Attach) detector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Detect a pod stuck at container creation because a volume cannot be attached (a `FailedAttachVolume` event — most often a Multi-Attach error), emitting a `VolumeAttachError` finding in the CLI scan, JSON, `--explain`, and the daemon's metrics.

**Architecture:** A pure `VolumeAttachDetector` reads the pod's events via the existing `PodFacts.Events` field; `collect.VolumeAttachEvents` does one cheap field-selected List; `FactsFrom` gains an events argument and correlates them; `scan.Evaluate` wires it in for both CLI and daemon; the daemon RBAC gains `events` read.

**Tech Stack:** Go 1.26. `corev1.Event` and field selectors are already available (no new module dependency).

## Global Constraints

- **READ-ONLY.** One extra field-selected `List` of events; no writes, no LLM. The daemon gets no events informer (avoids event churn) — events are List-ed per `scan.Evaluate` reconcile.
- Detectors stay pure `Detect(PodFacts) *Finding`.
- The finding surfaces via existing paths (report/JSON/`--explain`/`kubeagent_findings{}`) with no changes to those.
- Go at `/usr/local/go/bin` (`export PATH=$PATH:/usr/local/go/bin`).

---

### Task 1: the `VolumeAttachDetector` (pure) + tests

**Files:**
- Create: `internal/diagnose/volumeattach.go`, `internal/diagnose/volumeattach_test.go`

**Interfaces:**
- Produces: `diagnose.VolumeAttachDetector` (implements `Detector`); reads `PodFacts.Events` (already exists).

- [ ] **Step 1: Write the failing tests — create `internal/diagnose/volumeattach_test.go`**

```go
package diagnose

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podCreating returns a not-Ready pod stuck at container creation.
func podCreating(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}},
		},
	}
}

func attachEvent(ns, podName, msg string) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: podName + ".ev"},
		Reason:         "FailedAttachVolume",
		Type:           "Warning",
		Message:        msg,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: podName},
		LastTimestamp:  metav1.Now(),
	}
}

func TestVolumeAttach_MultiAttachOnStuckPod(t *testing.T) {
	ev := attachEvent("shop", "db-0", `Multi-Attach error for volume "pvc-1" Volume is already exclusively attached to one node`)
	f := VolumeAttachDetector{}.Detect(PodFacts{Pod: podCreating("shop", "db-0"), Events: []corev1.Event{ev}})
	if f == nil || f.Issue != "VolumeAttachError" {
		t.Fatalf("want VolumeAttachError, got %+v", f)
	}
	if !strings.Contains(f.Reason, "Multi-Attach") {
		t.Errorf("reason should name Multi-Attach: %q", f.Reason)
	}
	if !strings.Contains(f.Evidence, "pvc-1") {
		t.Errorf("evidence should carry the event message: %q", f.Evidence)
	}
}

func TestVolumeAttach_GenericAttachFailure(t *testing.T) {
	ev := attachEvent("shop", "db-0", `AttachVolume.Attach failed for volume "pvc-2": timed out waiting for external-attacher`)
	f := VolumeAttachDetector{}.Detect(PodFacts{Pod: podCreating("shop", "db-0"), Events: []corev1.Event{ev}})
	if f == nil {
		t.Fatal("want a finding for a generic attach failure")
	}
	if strings.Contains(f.Reason, "Multi-Attach") {
		t.Errorf("non-Multi-Attach message should use the generic reason: %q", f.Reason)
	}
}

func TestVolumeAttach_ReadyPodIgnored(t *testing.T) {
	pod := podCreating("shop", "db-0")
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	ev := attachEvent("shop", "db-0", "Multi-Attach error ...")
	if f := (VolumeAttachDetector{}).Detect(PodFacts{Pod: pod, Events: []corev1.Event{ev}}); f != nil {
		t.Errorf("a Ready/Running pod must not be flagged, got %+v", f)
	}
}

func TestVolumeAttach_CrashLoopingPodWithStaleEventIgnored(t *testing.T) {
	// Volume attached; the pod is now CrashLoopBackOff (past creation) but a stale
	// FailedAttachVolume event is still within its TTL — must NOT flag VolumeAttachError.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-0"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}},
		},
	}
	ev := attachEvent("shop", "db-0", "Multi-Attach error ...")
	if f := (VolumeAttachDetector{}).Detect(PodFacts{Pod: pod, Events: []corev1.Event{ev}}); f != nil {
		t.Errorf("crashlooping pod (past volume setup) must not be flagged, got %+v", f)
	}
}

func TestVolumeAttach_NoEvent(t *testing.T) {
	if f := (VolumeAttachDetector{}).Detect(PodFacts{Pod: podCreating("shop", "db-0")}); f != nil {
		t.Errorf("no event -> no finding, got %+v", f)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/diagnose/ -run TestVolumeAttach`
Expected: build failure — `VolumeAttachDetector` undefined.

- [ ] **Step 3: Implement — create `internal/diagnose/volumeattach.go`**

```go
package diagnose

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// VolumeAttachDetector flags a pod stuck at container creation because a volume
// cannot be attached (a FailedAttachVolume event) — most often a Multi-Attach
// error (a ReadWriteOnce volume still attached to another node).
type VolumeAttachDetector struct{}

func (d VolumeAttachDetector) Detect(facts PodFacts) *Finding {
	if podReady(facts.Pod) || !stuckCreating(facts.Pod) {
		return nil
	}
	ev := newestAttachEvent(facts.Events)
	if ev == nil {
		return nil
	}
	reason := "a volume cannot be attached to the pod's node"
	if strings.Contains(ev.Message, "Multi-Attach") {
		reason = "the volume is attached to another node (Multi-Attach) — the pod cannot mount it"
	}
	return &Finding{
		Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
		Issue:    "VolumeAttachError",
		Reason:   reason,
		Evidence: ev.Message,
	}
}

// podReady reports whether the pod has a Ready condition with status True.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// stuckCreating reports whether the pod is still at container creation: a
// container Waiting with reason ContainerCreating, or no container statuses yet.
// This excludes pods that progressed past volume setup (Running, CrashLoopBackOff,
// ImagePullBackOff), so a stale attach event cannot cause a false positive.
func stuckCreating(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason == "ContainerCreating" {
			return true
		}
	}
	return false
}

// newestAttachEvent returns the most recent FailedAttachVolume event (by
// LastTimestamp), or nil.
func newestAttachEvent(events []corev1.Event) *corev1.Event {
	var matches []corev1.Event
	for _, e := range events {
		if e.Reason == "FailedAttachVolume" {
			matches = append(matches, e)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].LastTimestamp.After(matches[j].LastTimestamp.Time)
	})
	return &matches[0]
}
```

- [ ] **Step 4: Run tests + build**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/diagnose/ -v && go build ./... && go vet ./internal/diagnose/ && gofmt -l internal/diagnose/`
Expected: all diagnose tests pass (new + existing), build ok, vet clean, gofmt clean.

- [ ] **Step 5: Commit**

```bash
git add internal/diagnose/
git commit -m "feat(diagnose): VolumeAttachDetector for FailedAttachVolume/Multi-Attach"
```

---

### Task 2: collect events + FactsFrom correlation + scan wiring + daemon RBAC

**Files:**
- Modify: `internal/collect/collect.go`, `internal/collect/collect_test.go`, `internal/scan/scan.go`, `deploy/rbac.yaml`
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `diagnose.VolumeAttachDetector` (Task 1).
- Produces: `collect.VolumeAttachEvents(...)`; `collect.FactsFrom(pods, events)` (signature change).

- [ ] **Step 1: Add `VolumeAttachEvents` and change `FactsFrom` in `internal/collect/collect.go`**

Add the collector (after `AllPods`, say):

```go
// VolumeAttachEvents lists FailedAttachVolume Warning events in the namespace
// (empty = all), read-only. Attach failures are rare, so this field-selected
// List is cheap.
func VolumeAttachEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=FailedAttachVolume"})
	if err != nil {
		return nil, fmt.Errorf("listing volume-attach events: %w", err)
	}
	return events.Items, nil
}
```

Replace `FactsFrom` with the events-correlating version:

```go
// FactsFrom wraps each pod in a diagnose.PodFacts, attaching any of the given
// events that reference that pod (by InvolvedObject). Pods with no matching
// events get an empty slice, so status-only detectors are unaffected.
func FactsFrom(pods []corev1.Pod, events []corev1.Event) []diagnose.PodFacts {
	byPod := make(map[string][]corev1.Event)
	for _, e := range events {
		if e.InvolvedObject.Kind == "Pod" {
			key := e.InvolvedObject.Namespace + "/" + e.InvolvedObject.Name
			byPod[key] = append(byPod[key], e)
		}
	}
	facts := make([]diagnose.PodFacts, 0, len(pods))
	for i := range pods {
		pod := pods[i] // take this element's address for PodFacts
		facts = append(facts, diagnose.PodFacts{Pod: &pod, Events: byPod[pod.Namespace+"/"+pod.Name]})
	}
	return facts
}
```

- [ ] **Step 2: Update the `FactsFrom` caller in `internal/collect/collect_test.go` and add a correlation test**

Change the existing `FactsFrom(pods)` call (line ~82) to `FactsFrom(pods, nil)`. Then add:

```go
func TestFactsFrom_CorrelatesEvents(t *testing.T) {
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p2"}},
	}
	events := []corev1.Event{
		{Reason: "FailedAttachVolume", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "a", Name: "p1"}},
		{Reason: "FailedAttachVolume", InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: "n1"}}, // non-pod -> ignored
	}
	facts := FactsFrom(pods, events)
	if len(facts[0].Events) != 1 {
		t.Errorf("p1 should have 1 correlated event, got %d", len(facts[0].Events))
	}
	if len(facts[1].Events) != 0 {
		t.Errorf("p2 should have no events, got %d", len(facts[1].Events))
	}
}
```

(Ensure `corev1` and `metav1` are imported in the test file; the existing `FactsFrom` test already uses pods, so add `metav1` if missing.)

- [ ] **Step 3: Wire into `internal/scan/scan.go`**

Add `VolumeAttachDetector` to the detector slice and collect+pass events. Change:

```go
	detectors := []diagnose.Detector{
		diagnose.CrashLoopDetector{},
		diagnose.ImagePullDetector{},
		diagnose.OOMKilledDetector{},
		diagnose.PendingDetector{},
	}
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods))
```

to:

```go
	detectors := []diagnose.Detector{
		diagnose.CrashLoopDetector{},
		diagnose.ImagePullDetector{},
		diagnose.OOMKilledDetector{},
		diagnose.PendingDetector{},
		diagnose.VolumeAttachDetector{},
	}
	attachEvents, _ := collect.VolumeAttachEvents(ctx, client, opts.Namespace)
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods, attachEvents))
```

- [ ] **Step 4: Add the `events` read permission in `deploy/rbac.yaml`**

Change the core-group rule:

```yaml
  - apiGroups: [""]
    resources: [pods, nodes, services, configmaps]
    verbs: [get, list, watch]
```

to:

```yaml
  - apiGroups: [""]
    resources: [pods, nodes, services, configmaps, events]
    verbs: [get, list, watch]
```

(Still only read verbs — the RBAC stays strictly read-only.)

- [ ] **Step 5: Add a `scan.Evaluate` integration test in `internal/scan/scan_test.go`**

```go
func TestEvaluate_FlagsVolumeAttachError(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-0"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "db",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}},
		},
	}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "db-0.ev"},
		Reason:         "FailedAttachVolume",
		Type:           "Warning",
		Message:        `Multi-Attach error for volume "pvc-9"`,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "db-0"},
	}
	cli := fake.NewSimpleClientset(node, pod, ev)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "VolumeAttachError" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a VolumeAttachError finding, got %+v", res.Inventory.Workloads)
	}
}
```

Note: the client-go fake clientset does not apply the `reason=` field selector to List (it returns the seeded event regardless), so `VolumeAttachEvents` returns the event and the finding appears. If, on the installed client-go version, the fake instead filters and drops the event, add a `PrependReactor("list", "events", …)` returning the event so the test exercises the wiring deterministically.

- [ ] **Step 6: Run tests + build + vet + gofmt**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./... && go vet ./... && gofmt -l internal/collect/ internal/scan/`
Expected: all packages pass (including the new correlation + integration tests and every existing test), build ok, vet clean, gofmt clean.

- [ ] **Step 7: Commit**

```bash
git add internal/collect/ internal/scan/ deploy/rbac.yaml
git commit -m "feat(scan,deploy): collect FailedAttachVolume events, correlate, wire VolumeAttachDetector; daemon events RBAC"
```

---

### Task 3: docs

**Files:**
- Modify: `README.md`, `CHANGELOG.md`, `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`

- [ ] **Step 1: `README.md` — add the detector**

Find where the deterministic detectors are listed (CrashLoopBackOff, ImagePullBackOff/ErrImagePull, OOMKilled, Pending/Unschedulable) and add:

```markdown
- **VolumeAttachError** — a pod stuck at container creation because a volume
  cannot be attached (`FailedAttachVolume`), most often a **Multi-Attach** error
  (a ReadWriteOnce volume still attached to another node).
```

- [ ] **Step 2: `CHANGELOG.md` — `[Unreleased] → Added`**

Add a `## [Unreleased]` section (above the latest release) with:

```markdown
## [Unreleased]

### Added

- **Volume-attach detection.** A new `VolumeAttachError` finding flags a pod stuck
  at container creation because a volume cannot be attached (`FailedAttachVolume`
  Warning event) — most often a **Multi-Attach** error (a ReadWriteOnce volume
  still attached to another node). Detected by reading the pod's events (one cheap
  field-selected List; the watch daemon needs no events informer). Read-only; the
  daemon's RBAC gains `events` read.
```

- [ ] **Step 3: `website/docs/features/diagnostics.md` — mention it**

Add `VolumeAttachError` to the diagnostics page's description of what's detected, with a one-line note that it reads `FailedAttachVolume` events and names Multi-Attach specifically.

- [ ] **Step 4: `website/docs/features/watch-mode.md` — metric example**

In the `kubeagent_findings{issue="..."}` row of the metrics table, add `VolumeAttachError` to the listed example issue types.

- [ ] **Step 5: Build + commit**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: build succeeds, all packages `ok` (no code changed in this task).

```bash
git add README.md CHANGELOG.md website/docs/features/diagnostics.md website/docs/features/watch-mode.md
git commit -m "docs: document the VolumeAttachError (Multi-Attach) detector"
```

---

## Self-Review

**Spec coverage:**
- `VolumeAttachDetector` (not-Ready + stuck-creating + FailedAttachVolume event; Multi-Attach vs generic reason) → Task 1. ✓
- `collect.VolumeAttachEvents` (field-selected List) + `FactsFrom(pods, events)` correlation → Task 2. ✓
- `scan.Evaluate` wiring (detector + events) → Task 2. ✓
- Daemon `events` RBAC → Task 2. ✓
- Docs (README, CHANGELOG, diagnostics, watch-mode) → Task 3. ✓
- READ-ONLY / no events informer / no new dep (Global Constraints) → one List; RBAC read-only; `corev1.Event` already available. ✓

**Placeholder scan:** none — complete code in every step.

**Type/name consistency:** `VolumeAttachDetector`, `Issue: "VolumeAttachError"`, `collect.VolumeAttachEvents`, and `FactsFrom(pods, events)` are used identically across tasks and tests. The two `FactsFrom` callers (`scan.go:55`, `collect_test.go:82`) are both updated in Task 2, keeping the build green. The finding flows to report/JSON/`--explain`/`kubeagent_findings{}` unchanged (they iterate findings by issue).
