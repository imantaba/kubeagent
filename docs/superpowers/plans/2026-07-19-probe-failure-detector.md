# ProbeFailure Detector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `ProbeFailureDetector` that names why a Running-but-not-Ready pod is failing — a readiness, liveness, or startup probe — read from the kubelet's `Unhealthy` events, with the failure reason sanitized so no pod IP or exec-probe output can leak.

**Architecture:** A new pure `Detector` (`internal/diagnose/probefailure.go`) reads the newest `Unhealthy` event from `PodFacts.Events`, guards on "pod not Ready and the probe's container currently Running" (so crash/pull pods are never double-flagged), and returns a `Finding{Issue:"ProbeFailure"}` whose `Reason`/`Evidence` are built only from a fixed per-probe sentence and a coarse reason table (never the raw message). A new `collect.UnhealthyEvents` lists the events; `scan.Evaluate` merges them with the existing attach events and registers the detector. Report, explain, RBAC, and the watch daemon are untouched.

**Tech Stack:** Go 1.26, standard library + `k8s.io/api/core/v1`, client-go fake clientset for I/O tests.

## Global Constraints

- **Read-only; NO new RBAC.** Only a `List` of events (already granted for `FailedAttachVolume`). No writes, no LLM.
- **Core, always-on detector** — runs in both the CLI `scan` and the `watch` daemon via the shared `scan.Evaluate`. No opt-in flag, no scan-only gating, no `watch.Config` change.
- **Privacy by construction:** no pod IP and no exec-probe command output may appear in any `Finding` field. `internal/report/report.go` and `internal/explain/explain.go` MUST NOT be changed by this feature.
- **Complementary to the restart detectors:** `ProbeFailure` may fire alongside `RestartLoop`/`CrashLoopBackOff`. The overlap is prevented only by the guard "the probe's container is currently `Running`" — no cross-detector coupling.
- **Deterministic & pure:** the detector and classifier are pure functions; the newest event is chosen by `LastTimestamp`.
- **Exact names/strings (use verbatim):**
  - Type `ProbeFailureDetector`; `Finding.Issue == "ProbeFailure"`.
  - `collect.UnhealthyEvents(ctx, client, namespace)` with `FieldSelector: "reason=Unhealthy"`.
  - Per-probe `Reason` (em dash `—` = U+2014):
    - readiness → `the readiness probe keeps failing — the pod is kept out of Service endpoints`
    - liveness → `the liveness probe keeps failing — the kubelet restarts the container`
    - startup → `the startup probe keeps failing — the container never finishes starting`
  - `Evidence` form: `container "<name>": <probeType> probe failed` plus ` — <reason>` only when a reason matched; when the container name is empty, drop the `container "<name>": ` prefix.
  - Reason table outputs: `connection refused`, `connection reset`, `unreachable`, `DNS lookup failed`, `timed out`, `HTTP <code>`, `gRPC NOT_SERVING`; empty when unmatched.
- **TDD** — failing test first. **No `Co-Authored-By: Claude` trailer** on any commit.

---

### Task 1: ProbeFailureDetector + probe-message classifier

**Files:**
- Create: `internal/diagnose/probefailure.go`
- Test: `internal/diagnose/probefailure_test.go`

**Interfaces:**
- Consumes: `PodFacts{Pod *corev1.Pod; Events []corev1.Event}`, `Finding` struct, and the existing unexported `podReady(pod *corev1.Pod) bool` helper — all in package `diagnose` (`internal/diagnose/diagnose.go`, `internal/diagnose/volumeattach.go`).
- Produces: `type ProbeFailureDetector struct{}` with `Detect(PodFacts) *Finding` (satisfies `Detector`). Later tasks register it and rely on `Finding.Issue == "ProbeFailure"`.

- [ ] **Step 1: Write the failing tests**

Create `internal/diagnose/probefailure_test.go`:

```go
package diagnose

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pfPod builds a Running-but-not-Ready pod with one Running container.
func pfPod(ns, name, container string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  container,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

// pfEvent builds an Unhealthy probe event targeting a pod's container.
func pfEvent(ns, pod, container, message string) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: pod + ".ev"},
		Reason:         "Unhealthy",
		Type:           "Warning",
		Message:        message,
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: pod, FieldPath: "spec.containers{" + container + "}"},
	}
}

func TestProbeFailureDetector_ReadinessHTTP(t *testing.T) {
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{
		pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
	}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a ProbeFailure finding, got nil")
	}
	if f.Issue != "ProbeFailure" || f.Container != "web" {
		t.Errorf("Issue/Container = %q/%q, want ProbeFailure/web", f.Issue, f.Container)
	}
	if want := `container "web": readiness probe failed — HTTP 503`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
	if !strings.Contains(f.Reason, "readiness probe keeps failing") {
		t.Errorf("Reason = %q, want it to name the readiness probe", f.Reason)
	}
}

func TestProbeFailureDetector_NoPodIPLeak(t *testing.T) {
	msg := `Liveness probe failed: Get "http://10.244.1.5:8080/healthz": dial tcp 10.244.1.5:8080: connect: connection refused`
	facts := PodFacts{Pod: pfPod("shop", "api-1", "api"), Events: []corev1.Event{pfEvent("shop", "api-1", "api", msg)}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding")
	}
	if strings.Contains(f.Evidence, "10.244.1.5") || strings.Contains(f.Reason, "10.244.1.5") {
		t.Errorf("pod IP leaked: Evidence=%q Reason=%q", f.Evidence, f.Reason)
	}
	if want := `container "api": liveness probe failed — connection refused`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

func TestProbeFailureDetector_SkipsWaitingContainer(t *testing.T) {
	pod := pfPod("shop", "web-1", "web")
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	facts := PodFacts{Pod: pod, Events: []corev1.Event{
		pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
	}}
	if f := ProbeFailureDetector{}.Detect(facts); f != nil {
		t.Errorf("a Waiting (CrashLoopBackOff) container must not be flagged, got %+v", f)
	}
}

func TestProbeFailureDetector_SkipsReadyPod(t *testing.T) {
	pod := pfPod("shop", "web-1", "web")
	pod.Status.Conditions[0].Status = corev1.ConditionTrue
	facts := PodFacts{Pod: pod, Events: []corev1.Event{
		pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
	}}
	if f := ProbeFailureDetector{}.Detect(facts); f != nil {
		t.Errorf("a Ready pod must not be flagged, got %+v", f)
	}
}

func TestProbeFailureDetector_FallbackNoFieldPath(t *testing.T) {
	ev := pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503")
	ev.InvolvedObject.FieldPath = ""
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{ev}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("with empty FieldPath but pod Running+notReady, expected a finding")
	}
	if f.Container != "" {
		t.Errorf("Container = %q, want empty", f.Container)
	}
	if want := "readiness probe failed — HTTP 503"; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q (no container prefix)", f.Evidence, want)
	}
}

func TestContainerFromFieldPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"spec.containers{web}", "web"},
		{"spec.initContainers{init}", "init"},
		{"spec.containers{}", ""},
		{"", ""},
		{"spec.containers", ""},
	}
	for _, c := range cases {
		if got := containerFromFieldPath(c.in); got != c.want {
			t.Errorf("containerFromFieldPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClassifyProbe(t *testing.T) {
	cases := []struct{ msg, wantType, wantReason string }{
		{"Readiness probe failed: HTTP probe failed with statuscode: 503", "readiness", "HTTP 503"},
		{"Liveness probe failed: dial tcp 10.0.0.1:8080: connect: connection refused", "liveness", "connection refused"},
		{`Startup probe failed: Get "http://10.0.0.1/": context deadline exceeded`, "startup", "timed out"},
		{"Readiness probe failed: dial tcp: lookup db on 10.96.0.10:53: no such host", "readiness", "DNS lookup failed"},
		{`Liveness probe failed: service unhealthy (responded with "NOT_SERVING")`, "liveness", "gRPC NOT_SERVING"},
		{"Liveness probe failed: cat: /tmp/healthy: No such file or directory", "liveness", ""},
		{"BackOff restarting failed container", "", ""},
	}
	for _, c := range cases {
		gotType, gotReason := classifyProbe(c.msg)
		if gotType != c.wantType || gotReason != c.wantReason {
			t.Errorf("classifyProbe(%q) = (%q,%q), want (%q,%q)", c.msg, gotType, gotReason, c.wantType, c.wantReason)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/diagnose/ -run 'ProbeFailure|ContainerFromFieldPath|ClassifyProbe'`
Expected: FAIL — `undefined: ProbeFailureDetector` / `containerFromFieldPath` / `classifyProbe`.

- [ ] **Step 3: Write the detector**

Create `internal/diagnose/probefailure.go`:

```go
package diagnose

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// ProbeFailureDetector flags a pod that is not Ready because a container's
// readiness, liveness, or startup probe keeps failing (an "Unhealthy" event).
// It is complementary to the restart detectors: a liveness/startup probe that
// restarts a container also trips RestartLoop/CrashLoop; ProbeFailure names the
// probe as the cause. The "container currently Running" guard keeps a
// CrashLoopBackOff/ImagePullBackOff container (which is Waiting) from being
// double-flagged here. To preserve the --explain privacy guarantee, the raw
// probe message (which may carry a pod IP or arbitrary exec-probe output) is
// never stored; Reason and Evidence are built only from fixed strings.
type ProbeFailureDetector struct{}

func (d ProbeFailureDetector) Detect(facts PodFacts) *Finding {
	if podReady(facts.Pod) {
		return nil
	}
	ev := newestUnhealthyEvent(facts.Events)
	if ev == nil {
		return nil
	}
	container := containerFromFieldPath(ev.InvolvedObject.FieldPath)
	if container != "" {
		if !containerRunning(facts.Pod, container) {
			return nil
		}
	} else if facts.Pod.Status.Phase != corev1.PodRunning {
		return nil
	}
	probeType, reason := classifyProbe(ev.Message)
	if probeType == "" {
		return nil
	}
	return &Finding{
		Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
		Issue:     "ProbeFailure",
		Reason:    probeReason(probeType),
		Evidence:  probeEvidence(container, probeType, reason),
		Container: container,
	}
}

// newestUnhealthyEvent returns the most recent Reason=="Unhealthy" event (by
// LastTimestamp), or nil.
func newestUnhealthyEvent(events []corev1.Event) *corev1.Event {
	var matches []corev1.Event
	for _, e := range events {
		if e.Reason == "Unhealthy" {
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

// containerFromFieldPath extracts the container name from an event involvedObject
// FieldPath, e.g. `spec.containers{web}` -> "web"; "" when there are no braces.
func containerFromFieldPath(fp string) string {
	openIdx := strings.IndexByte(fp, '{')
	closeIdx := strings.IndexByte(fp, '}')
	if openIdx < 0 || closeIdx < 0 || closeIdx < openIdx {
		return ""
	}
	return fp[openIdx+1 : closeIdx]
}

// containerRunning reports whether the named container is currently Running.
func containerRunning(pod *corev1.Pod, name string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name {
			return cs.State.Running != nil
		}
	}
	return false
}

// classifyProbe reads the probe type from the message prefix and derives a
// coarse, IP-free failure reason from the tail. The raw message is never
// returned. reason is "" when the tail is unrecognized (e.g. exec output);
// probeType is "" when the message is not a recognized probe failure.
func classifyProbe(message string) (probeType, reason string) {
	switch {
	case strings.HasPrefix(message, "Readiness probe failed"):
		probeType = "readiness"
	case strings.HasPrefix(message, "Liveness probe failed"):
		probeType = "liveness"
	case strings.HasPrefix(message, "Startup probe failed"):
		probeType = "startup"
	default:
		return "", ""
	}
	return probeType, probeReasonTail(message)
}

// probeReasonTail maps a probe message to a coarse, IP-free reason; "" if none match.
func probeReasonTail(message string) string {
	m := strings.ToLower(message)
	switch {
	case strings.Contains(m, "connection refused"):
		return "connection refused"
	case strings.Contains(m, "connection reset"):
		return "connection reset"
	case strings.Contains(m, "no route to host"), strings.Contains(m, "network is unreachable"):
		return "unreachable"
	case strings.Contains(m, "no such host"), strings.Contains(m, "server misbehaving"):
		return "DNS lookup failed"
	case strings.Contains(m, "context deadline exceeded"), strings.Contains(m, "timeout"):
		return "timed out"
	case strings.Contains(m, "statuscode:"):
		if code := httpStatusCode(message); code != "" {
			return "HTTP " + code
		}
		return ""
	case strings.Contains(m, "not_serving"):
		return "gRPC NOT_SERVING"
	default:
		return ""
	}
}

// httpStatusCode extracts the integer following "statuscode: " in an HTTP probe message.
func httpStatusCode(message string) string {
	const marker = "statuscode: "
	i := strings.Index(message, marker)
	if i < 0 {
		return ""
	}
	rest := message[i+len(marker):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	return rest[:j]
}

// probeReason is the static, clean, per-probe-type root cause sentence.
func probeReason(probeType string) string {
	switch probeType {
	case "readiness":
		return "the readiness probe keeps failing — the pod is kept out of Service endpoints"
	case "liveness":
		return "the liveness probe keeps failing — the kubelet restarts the container"
	case "startup":
		return "the startup probe keeps failing — the container never finishes starting"
	default:
		return "a probe keeps failing"
	}
}

// probeEvidence builds the clean, IP-free evidence line; the reason suffix and the
// container prefix are each omitted when empty.
func probeEvidence(container, probeType, reason string) string {
	var e string
	if container != "" {
		e = fmt.Sprintf("container %q: %s probe failed", container, probeType)
	} else {
		e = fmt.Sprintf("%s probe failed", probeType)
	}
	if reason != "" {
		e += " — " + reason
	}
	return e
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/diagnose/ -run 'ProbeFailure|ContainerFromFieldPath|ClassifyProbe' -v`
Expected: PASS (all cases). Then `gofmt -l internal/diagnose/probefailure.go` (must print nothing) and `go vet ./internal/diagnose/`.

- [ ] **Step 5: Commit**

```bash
git add internal/diagnose/probefailure.go internal/diagnose/probefailure_test.go
git commit -m "feat(diagnose): ProbeFailure detector for readiness/liveness/startup probes"
```

---

### Task 2: Collect Unhealthy events and wire the detector into scan.Evaluate

**Files:**
- Modify: `internal/collect/collect.go` (add `UnhealthyEvents`, next to `VolumeAttachEvents` ~line 98)
- Test: `internal/collect/collect_test.go` (add `TestUnhealthyEvents`)
- Modify: `internal/scan/scan.go` (register detector; fetch + merge events, ~lines 91-100)
- Test: `internal/scan/scan_test.go` (add `TestEvaluate_FlagsProbeFailure`)

**Interfaces:**
- Consumes: `ProbeFailureDetector` (Task 1); existing `collect.VolumeAttachEvents`, `collect.FactsFrom`, `diagnose.Run`.
- Produces: `collect.UnhealthyEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error)`.

- [ ] **Step 1: Write the failing collect test**

Add to `internal/collect/collect_test.go`:

```go
func TestUnhealthyEvents(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "web.ev"},
		Reason:         "Unhealthy",
		Type:           "Warning",
		Message:        "Readiness probe failed: HTTP probe failed with statuscode: 503",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "web"},
	}
	client := fake.NewSimpleClientset(ev)
	got, err := UnhealthyEvents(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "Unhealthy" {
		t.Errorf("want 1 Unhealthy event, got %+v", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -run TestUnhealthyEvents`
Expected: FAIL — `undefined: UnhealthyEvents`.

- [ ] **Step 3: Add the collector**

In `internal/collect/collect.go`, immediately after `VolumeAttachEvents` (which ends ~line 106), add:

```go
// UnhealthyEvents lists the kubelet's probe-failure ("Unhealthy") Warning events
// in the namespace ("" = all). Read-only; mirrors VolumeAttachEvents. Needs no
// permission beyond the event list scan already performs.
func UnhealthyEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=Unhealthy"})
	if err != nil {
		return nil, fmt.Errorf("listing probe (Unhealthy) events: %w", err)
	}
	return events.Items, nil
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/collect/ -run TestUnhealthyEvents`
Expected: PASS.

- [ ] **Step 5: Write the failing scan integration test**

Add to `internal/scan/scan_test.go`:

```go
func TestEvaluate_FlagsProbeFailure(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1", Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "web",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "web-1.ev"},
		Reason:         "Unhealthy",
		Type:           "Warning",
		Message:        "Readiness probe failed: HTTP probe failed with statuscode: 503",
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "web-1", FieldPath: "spec.containers{web}"},
	}
	cli := fake.NewSimpleClientset(node, pod, ev)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "ProbeFailure" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a ProbeFailure finding, got %+v", res.Inventory.Workloads)
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/scan/ -run TestEvaluate_FlagsProbeFailure`
Expected: FAIL — no `ProbeFailure` finding (detector not registered, events not fetched).

- [ ] **Step 7: Wire it into `scan.Evaluate`**

In `internal/scan/scan.go`, add `diagnose.ProbeFailureDetector{}` as the last entry of the `detectors` slice:

```go
	detectors := []diagnose.Detector{
		diagnose.CrashLoopDetector{},
		diagnose.ImagePullDetector{},
		diagnose.OOMKilledDetector{},
		diagnose.PendingDetector{},
		diagnose.VolumeAttachDetector{},
		diagnose.RestartLoopDetector{Now: time.Now()},
		diagnose.ProbeFailureDetector{},
	}
```

Then replace the two lines that fetch attach events and run the detectors:

```go
	attachEvents, _ := collect.VolumeAttachEvents(ctx, client, opts.Namespace)
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods, attachEvents))
```

with:

```go
	attachEvents, _ := collect.VolumeAttachEvents(ctx, client, opts.Namespace)
	unhealthyEvents, _ := collect.UnhealthyEvents(ctx, client, opts.Namespace)
	events := append(attachEvents, unhealthyEvents...)
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods, events))
```

(Leave the `--logs` enrichment block that follows `findings := …` unchanged — it still runs before `inventory.Assemble`.)

- [ ] **Step 8: Run the scan + collect + diagnose tests**

Run: `go test ./internal/scan/ ./internal/collect/ ./internal/diagnose/`
Expected: PASS. Then `gofmt -l internal/collect/collect.go internal/scan/scan.go` (nothing) and `go vet ./internal/scan/`.

- [ ] **Step 9: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): collect Unhealthy events and run the ProbeFailure detector"
```

---

### Task 3: Show ProbeFailure in the golden snapshot

**Files:**
- Modify: `internal/report/golden_test.go` (`goldenWorkloads`, ~line 81)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated, not hand-edited)

**Interfaces:**
- Consumes: `Finding.Issue == "ProbeFailure"` with the exact `Reason`/`Evidence` from Task 1.
- Produces: nothing for later tasks.

- [ ] **Step 1: Add a ProbeFailure workload to the fixture**

In `internal/report/golden_test.go`, inside `goldenWorkloads()`'s returned slice, add this element immediately after the `data` StatefulSet entry (keep it the last workload, before the closing `}` of the slice):

```go
		{Namespace: "shop", Name: "checkout", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Image: "checkout:2.1",
			Pods:  []inventory.PodRow{{Name: "checkout-7f9-qk2mn", Phase: "Running", Ready: "0/1", Restarts: 0, Node: "worker-3", IP: "10.244.3.9", Age: "5d", Image: "checkout:2.1"}},
			Findings: []diagnose.Finding{{Pod: "shop/checkout", Issue: "ProbeFailure",
				Reason:   "the readiness probe keeps failing — the pod is kept out of Service endpoints",
				Evidence: `container "checkout": readiness probe failed — HTTP 503`, Container: "checkout"}}},
```

- [ ] **Step 2: Run the golden test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the rendered output now contains the `checkout` / ProbeFailure lines that are not yet in `golden-scan.txt`.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report/ -run TestGoldenScanOutput -update`
Then inspect the diff: `git diff internal/report/testdata/golden-scan.txt` — it must show ONLY the added `checkout` workload block, rendering as:

```text
    ⚠ ProbeFailure: the readiness probe keeps failing — the pod is kept out of Service endpoints
      ↳ container "checkout": readiness probe failed — HTTP 503
```

- [ ] **Step 4: Run the full report suite twice (determinism)**

Run: `go test ./internal/report/ && go test ./internal/report/`
Expected: PASS both times (`TestGoldenInputCoversAllSections` now counts 7 workloads / 7 distinct modes, still ≥ 6).

- [ ] **Step 5: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover ProbeFailure in the golden scan snapshot"
```

---

### Task 4: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md` (new failure-mode subsection; add to the `## Status` list)
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `website/docs/quickstart.md` (failure-mode list in the intro paragraph)
- Modify: `README.md` (detector bullet list)

**Interfaces:** none (docs only).

- [ ] **Step 1: Add the diagnostics subsection**

In `website/docs/features/diagnostics.md`, add a new subsection after the `### RestartLoop` block (before `### Node reservations`):

```markdown
### ProbeFailure

A pod that is **Running but not Ready** because a container's **readiness**,
**liveness**, or **startup** probe keeps failing. `kubeagent` reads the kubelet's
`Unhealthy` events and names the probe, the container, and a plain-language reason
(`HTTP 503`, `connection refused`, `timed out`, `DNS lookup failed`,
`gRPC NOT_SERVING`, …) — for example `container "web": readiness probe failed —
HTTP 503`. It is complementary to `RestartLoop`/`CrashLoopBackOff`: a liveness probe
that restarts a container shows both the pattern and the probe as the cause. To keep
the failure reason safe for `--explain`, the raw probe message is never surfaced — no
pod IP and no `exec`-probe command output ever leaves the local report. Read-only: it
lists `Unhealthy` events (no extra permission beyond the scan's existing event list).
```

Then add `ProbeFailure` to the detector list in the `## Status` paragraph (the sentence listing `CrashLoopBackOff, ImagePullBackOff/ErrImagePull, OOMKilled, Pending/Unschedulable, VolumeAttachError (Multi-Attach), and RestartLoop`) so it reads `…, RestartLoop, and ProbeFailure pods`.

- [ ] **Step 2: Add the CHANGELOG entry**

In `CHANGELOG.md`, under `## [Unreleased]` → `### Added` (create the `### Added` sub-header if the section is empty), add:

```markdown
- **ProbeFailure detector.** `scan` flags a Running-but-not-Ready pod whose readiness,
  liveness, or startup probe keeps failing, reading the kubelet's `Unhealthy` events and
  naming the probe, container, and a plain-language reason (`HTTP 503`, `connection
  refused`, `timed out`, …). Complementary to `RestartLoop`/`CrashLoopBackOff`.
  Read-only, always-on, **no new RBAC**. The raw probe message (which may carry a pod IP
  or `exec` output) is never surfaced, so `--explain` stays privacy-preserving.
```

- [ ] **Step 3: Update the quickstart failure-mode list**

In `website/docs/quickstart.md`, the first paragraph lists the failure modes
(`CrashLoopBackOff, ImagePullBackOff / ErrImagePull, OOMKilled, Pending / Unschedulable,
VolumeAttachError, and silent restart loops`). Add probe failures, e.g. change the tail
to `…, silent restart loops, and failing readiness/liveness/startup probes —`.

- [ ] **Step 4: Update the README detector list**

`README.md` has a bulleted detector list (~lines 34-43, ending with the multi-line
`- **RestartLoop** — …` bullet) and a summary sentence (~line 86). Two edits:

(a) Add a bullet immediately after the `RestartLoop` bullet's last line
(`  the case \`CrashLoopBackOff\` misses.`):

```markdown
- **ProbeFailure** — a Running-but-not-Ready pod whose readiness, liveness, or
  startup probe keeps failing; names the probe, container, and reason.
```

(b) In the summary sentence `…VolumeAttachError (Multi-Attach), and RestartLoop pods,
in text or JSON.` change the tail to `…VolumeAttachError (Multi-Attach), RestartLoop,
and ProbeFailure pods, in text or JSON.`

- [ ] **Step 5: Verify docs build**

Run: `cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml --site-dir /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/site-check`
Expected: exit 0, "Documentation built", no `WARNING` lines about these pages. (The red Material-for-MkDocs banner is cosmetic.)

- [ ] **Step 6: Commit**

```bash
git add website/docs/features/diagnostics.md CHANGELOG.md website/docs/quickstart.md README.md
git commit -m "docs: document the ProbeFailure detector"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && go test ./...
gofmt -l internal/diagnose/probefailure.go internal/collect/collect.go internal/scan/scan.go
go test ./internal/report -run TestGoldenScanOutput   # run twice: deterministic
```

All packages pass; gofmt prints nothing for the touched files; golden is stable. Confirm no `Co-Authored-By` trailer: `git log --format='%(trailers)' main..HEAD` prints nothing.
