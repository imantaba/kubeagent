# InitContainer Failure Detector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an `InitContainerDetector` that names why a pod is blocked in its init phase — an init container that is crash-looping, can't pull its image, or was OOM-killed — by reading `InitContainerStatuses`, the slice no current detector touches.

**Architecture:** A new pure `Detector` (`internal/diagnose/initcontainer.go`) iterates `pod.Status.InitContainerStatuses`, returns the first failing init container classified by precedence (image-pull → OOMKilled → CrashLoopBackOff), and reports it with a kubectl-style `Init:<reason>` issue and an `(X/N)` position. The existing `containerResources` helper is extended to also search `pod.Spec.InitContainers` so `Init:OOMKilled` carries the init container's limits. The detector is registered in `scan.Evaluate`; report, explain, collect, watch, and RBAC are untouched.

**Tech Stack:** Go 1.26, standard library + `k8s.io/api/core/v1`, client-go fake clientset for the integration test.

## Global Constraints

- **Read-only; NO new RBAC and NO new collection.** `InitContainerStatuses` is already in the Pod objects `collect` fetches. Reads pod status only.
- **Core, always-on** detector — runs in both the CLI `scan` and the `watch` daemon via the shared `scan.Evaluate`. No opt-in flag, no `watch.Config` change.
- Detector is a **pure function** of `PodFacts` (reads `Pod` only); deterministic.
- `report.go`, `explain.go`, `internal/watch`, `internal/collect`, and all deploy/RBAC/Helm files stay **unchanged**. The only existing code touched is `containerResources` (extended) and the `scan.Evaluate` detector slice (one line).
- **Exact names/strings (verbatim):**
  - Type `InitContainerDetector`; helper `initFinding`.
  - Issue strings: `Init:CrashLoopBackOff`, `Init:ImagePullBackOff`, `Init:ErrImagePull`, `Init:OOMKilled` (the image-pull ones are `"Init:" + Waiting.Reason`).
  - Reasons (em dash `—` = U+2014):
    - image-pull → `an init container's image cannot be pulled — the pod cannot start`
    - OOMKilled → `an init container was killed for exceeding its memory limit — the pod cannot start`
    - CrashLoopBackOff → `an init container is crash-looping — the pod cannot start its main containers`
  - Evidence forms: `init container "<name>" (<i+1>/<total>): <Waiting.Message>` (image-pull); `init container "<name>" (<i+1>/<total>), exitCode=<code>` (OOM); `init container "<name>" (<i+1>/<total>), restartCount=<n>` (crash).
  - Precedence: image-pull, then OOMKilled (current OR last termination), then CrashLoopBackOff. Every finding sets `Container: cs.Name`.
- **TDD** — failing test first. **No `Co-Authored-By: Claude` trailer** on any commit.

---

### Task 1: InitContainerDetector + extend containerResources

**Files:**
- Create: `internal/diagnose/initcontainer.go`
- Test: `internal/diagnose/initcontainer_test.go`
- Modify: `internal/diagnose/oomkilled.go` (extend `containerResources`, ~lines 36-49)

**Interfaces:**
- Consumes: `PodFacts`, `Finding`, `ContainerResources`, and the existing `containerResources`/`quantityOrUnset` helpers — all in package `diagnose`.
- Produces: `type InitContainerDetector struct{}` with `Detect(PodFacts) *Finding` (satisfies `Detector`). Task 2 registers it and relies on `Finding.Issue` values `Init:CrashLoopBackOff` etc.

- [ ] **Step 1: Write the failing tests**

Create `internal/diagnose/initcontainer_test.go`:

```go
package diagnose

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podWithInit builds a pod with the given init-container statuses.
func podWithInit(ns, name string, initStatuses ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     corev1.PodStatus{InitContainerStatuses: initStatuses},
	}
}

func TestInitContainerDetector_CrashLoop(t *testing.T) {
	pod := podWithInit("shop", "orders",
		corev1.ContainerStatus{Name: "wait-for-db", RestartCount: 6,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
		corev1.ContainerStatus{Name: "migrate",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
	)
	f := InitContainerDetector{}.Detect(PodFacts{Pod: pod})
	if f == nil {
		t.Fatal("expected an Init:CrashLoopBackOff finding, got nil")
	}
	if f.Issue != "Init:CrashLoopBackOff" || f.Container != "wait-for-db" {
		t.Errorf("Issue/Container = %q/%q, want Init:CrashLoopBackOff/wait-for-db", f.Issue, f.Container)
	}
	if want := `init container "wait-for-db" (1/2), restartCount=6`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

func TestInitContainerDetector_ImagePull(t *testing.T) {
	pod := podWithInit("shop", "orders",
		corev1.ContainerStatus{Name: "ok-step", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		corev1.ContainerStatus{Name: "fetch-config",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: `Back-off pulling image "reg/config:bad": not found`}}},
		corev1.ContainerStatus{Name: "third",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
	)
	f := InitContainerDetector{}.Detect(PodFacts{Pod: pod})
	if f == nil {
		t.Fatal("expected an Init:ImagePullBackOff finding")
	}
	if f.Issue != "Init:ImagePullBackOff" {
		t.Errorf("Issue = %q, want Init:ImagePullBackOff", f.Issue)
	}
	if want := `init container "fetch-config" (2/3): Back-off pulling image "reg/config:bad": not found`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

func TestInitContainerDetector_OOMKilledPrecedenceAndResources(t *testing.T) {
	pod := podWithInit("shop", "orders",
		corev1.ContainerStatus{Name: "loader", RestartCount: 3,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}},
	)
	pod.Spec.InitContainers = []corev1.Container{{Name: "loader", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
	}}}
	f := InitContainerDetector{}.Detect(PodFacts{Pod: pod})
	if f == nil {
		t.Fatal("expected an Init:OOMKilled finding")
	}
	if f.Issue != "Init:OOMKilled" {
		t.Errorf("Issue = %q, want Init:OOMKilled (OOM takes precedence over CrashLoopBackOff)", f.Issue)
	}
	if want := `init container "loader" (1/1), exitCode=137`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
	if f.Resources == nil || f.Resources.MemLimit != "32Mi" {
		t.Errorf("Resources must carry the init container's limits (32Mi), got %+v", f.Resources)
	}
}

func TestInitContainerDetector_SkipsSucceededInits(t *testing.T) {
	// All inits succeeded; the MAIN container is crash-looping. The init detector
	// must stay silent (CrashLoopDetector owns the main container).
	pod := podWithInit("shop", "orders",
		corev1.ContainerStatus{Name: "setup", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
	)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	if f := (InitContainerDetector{}).Detect(PodFacts{Pod: pod}); f != nil {
		t.Errorf("succeeded inits + crashing main must not yield an init finding, got %+v", f)
	}
}

func TestInitContainerDetector_HealthyRunningSidecarNotFlagged(t *testing.T) {
	pod := podWithInit("shop", "orders",
		corev1.ContainerStatus{Name: "logging-sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	)
	if f := (InitContainerDetector{}).Detect(PodFacts{Pod: pod}); f != nil {
		t.Errorf("a healthy Running (native sidecar) init container must not be flagged, got %+v", f)
	}
}

func TestInitContainerDetector_NoInitContainers(t *testing.T) {
	if f := (InitContainerDetector{}).Detect(PodFacts{Pod: podWithInit("shop", "orders")}); f != nil {
		t.Errorf("a pod with no init containers must not be flagged, got %+v", f)
	}
}
```

> Note: if `podWithInit` collides with a helper in `internal/diagnose/helpers_test.go`, rename yours (e.g. `icPod`) and adjust call sites. (`helpers_test.go` currently defines `podWaiting`/`podOOMKilled`/`podOOMKilledWithResources`/`podUnschedulable` — no collision expected.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/diagnose/ -run TestInitContainerDetector`
Expected: FAIL — `undefined: InitContainerDetector`.

- [ ] **Step 3: Extend `containerResources` to also search init containers**

In `internal/diagnose/oomkilled.go`, replace the body of `containerResources` (currently ranging over `pod.Spec.Containers` only) with a version that searches both slices. Iterate the two slices **separately** — never `append(a, b...)`, which can mutate the backing array:

```go
// containerResources finds the named container in the pod spec (main OR init) and
// returns its cpu/memory requests and limits; nil if the container is not in the spec.
func containerResources(pod *corev1.Pod, name string) *ContainerResources {
	for _, list := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, c := range list {
			if c.Name == name {
				return &ContainerResources{
					Container:  name,
					CPURequest: quantityOrUnset(c.Resources.Requests, corev1.ResourceCPU),
					CPULimit:   quantityOrUnset(c.Resources.Limits, corev1.ResourceCPU),
					MemRequest: quantityOrUnset(c.Resources.Requests, corev1.ResourceMemory),
					MemLimit:   quantityOrUnset(c.Resources.Limits, corev1.ResourceMemory),
				}
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Write the detector**

Create `internal/diagnose/initcontainer.go`:

```go
package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// InitContainerDetector flags a pod blocked in its init phase because an init
// container is failing. Init containers run sequentially and block the pod, so at
// most one is actively failing; the detector reports the first failing one. It
// reads pod.Status.InitContainerStatuses — the slice the main-container crash
// detectors do not look at — so there is no overlap: while an init container
// fails, the main containers sit in Waiting/PodInitializing, which no detector
// matches.
type InitContainerDetector struct{}

func (d InitContainerDetector) Detect(facts PodFacts) *Finding {
	statuses := facts.Pod.Status.InitContainerStatuses
	for i, cs := range statuses {
		if f := initFinding(facts.Pod, cs, i, len(statuses)); f != nil {
			return f
		}
	}
	return nil
}

// initFinding classifies one init container's failure, or returns nil if it is not
// in a failing state (succeeded, not yet started, or a healthy running sidecar).
// Precedence: image-pull, then OOMKilled (current or last termination), then
// CrashLoopBackOff.
func initFinding(pod *corev1.Pod, cs corev1.ContainerStatus, idx, total int) *Finding {
	pos := fmt.Sprintf("(%d/%d)", idx+1, total)
	podName := pod.Namespace + "/" + pod.Name

	if w := cs.State.Waiting; w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull") {
		return &Finding{
			Pod:       podName,
			Issue:     "Init:" + w.Reason,
			Reason:    "an init container's image cannot be pulled — the pod cannot start",
			Evidence:  fmt.Sprintf("init container %q %s: %s", cs.Name, pos, w.Message),
			Container: cs.Name,
		}
	}
	for _, term := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
		if term != nil && term.Reason == "OOMKilled" {
			return &Finding{
				Pod:       podName,
				Issue:     "Init:OOMKilled",
				Reason:    "an init container was killed for exceeding its memory limit — the pod cannot start",
				Evidence:  fmt.Sprintf("init container %q %s, exitCode=%d", cs.Name, pos, term.ExitCode),
				Resources: containerResources(pod, cs.Name),
				Container: cs.Name,
			}
		}
	}
	if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
		return &Finding{
			Pod:       podName,
			Issue:     "Init:CrashLoopBackOff",
			Reason:    "an init container is crash-looping — the pod cannot start its main containers",
			Evidence:  fmt.Sprintf("init container %q %s, restartCount=%d", cs.Name, pos, cs.RestartCount),
			Container: cs.Name,
		}
	}
	return nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/diagnose/ -run 'TestInitContainerDetector|OOMKilled' -v`
Expected: PASS for all `TestInitContainerDetector_*` AND the existing `OOMKilled` tests (the `containerResources` change must not regress main-container OOM resources). Then `gofmt -l internal/diagnose/initcontainer.go internal/diagnose/oomkilled.go` (must print nothing) and `go vet ./internal/diagnose/`.

- [ ] **Step 6: Commit**

```bash
git add internal/diagnose/initcontainer.go internal/diagnose/initcontainer_test.go internal/diagnose/oomkilled.go
git commit -m "feat(diagnose): InitContainer detector for Init:CrashLoopBackOff/ImagePull/OOMKilled"
```

---

### Task 2: Register the detector in scan.Evaluate

**Files:**
- Modify: `internal/scan/scan.go` (the `detectors` slice, ~lines 91-98)
- Test: `internal/scan/scan_test.go` (add `TestEvaluate_FlagsInitContainerFailure`)

**Interfaces:**
- Consumes: `InitContainerDetector` (Task 1).
- Produces: nothing for later tasks.

- [ ] **Step 1: Write the failing integration test**

Add to `internal/scan/scan_test.go`:

```go
func TestEvaluate_FlagsInitContainerFailure(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "orders-1", Labels: map[string]string{"app": "orders"}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "wait-for-db", RestartCount: 6,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
			}},
		},
	}
	cli := fake.NewSimpleClientset(node, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	var initFindings, crashFindings int
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			switch f.Issue {
			case "Init:CrashLoopBackOff":
				initFindings++
			case "CrashLoopBackOff":
				crashFindings++
			}
		}
	}
	if initFindings != 1 {
		t.Errorf("expected exactly 1 Init:CrashLoopBackOff finding, got %d (%+v)", initFindings, res.Inventory.Workloads)
	}
	if crashFindings != 0 {
		t.Errorf("main-container CrashLoopBackOff must not fire for an init-blocked pod, got %d", crashFindings)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/scan/ -run TestEvaluate_FlagsInitContainerFailure`
Expected: FAIL — `initFindings` is 0 (detector not registered).

- [ ] **Step 3: Register the detector**

In `internal/scan/scan.go`, add `diagnose.InitContainerDetector{}` to the `detectors` slice, between `RestartLoopDetector` and `ProbeFailureDetector`:

```go
	detectors := []diagnose.Detector{
		diagnose.CrashLoopDetector{},
		diagnose.ImagePullDetector{},
		diagnose.OOMKilledDetector{},
		diagnose.PendingDetector{},
		diagnose.VolumeAttachDetector{},
		diagnose.RestartLoopDetector{Now: time.Now()},
		diagnose.InitContainerDetector{},
		diagnose.ProbeFailureDetector{},
	}
```

- [ ] **Step 4: Run the scan + diagnose tests**

Run: `go test ./internal/scan/ ./internal/diagnose/`
Expected: PASS. Then `gofmt -l internal/scan/scan.go` (nothing) and `go vet ./internal/scan/`.

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): run the InitContainer detector"
```

---

### Task 3: Show an init failure in the golden snapshot

**Files:**
- Modify: `internal/report/golden_test.go` (`goldenWorkloads`, ~line 81)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated, not hand-edited)

**Interfaces:**
- Consumes: `Finding.Issue == "Init:CrashLoopBackOff"` with the exact `Reason`/`Evidence` from Task 1.

- [ ] **Step 1: Add an Init:CrashLoopBackOff workload to the fixture**

In `internal/report/golden_test.go`, inside the slice returned by `goldenWorkloads()`, add this element as the LAST workload (after the `checkout` ProbeFailure entry, before the slice's closing `}`):

```go
		{Namespace: "shop", Name: "orders", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Image: "orders:3.0",
			Pods:  []inventory.PodRow{{Name: "orders-6f9-qk2mn", Phase: "Init:CrashLoopBackOff", Ready: "0/1", Restarts: 0, Node: "worker-3", IP: "10.244.3.11", Age: "4m", Image: "orders:3.0"}},
			Findings: []diagnose.Finding{{Pod: "shop/orders", Issue: "Init:CrashLoopBackOff",
				Reason:   "an init container is crash-looping — the pod cannot start its main containers",
				Evidence: `init container "wait-for-db" (1/2), restartCount=6`, Container: "wait-for-db"}}},
```

- [ ] **Step 2: Run the golden test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the rendered output now has the `orders` / `Init:CrashLoopBackOff` lines not yet in `golden-scan.txt`.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report/ -run TestGoldenScanOutput -update`
Then inspect `git diff internal/report/testdata/golden-scan.txt` — it must show ONLY the added `orders` block (a `⚠ Init:CrashLoopBackOff: …` line + a `↳ init container "wait-for-db" (1/2), restartCount=6` line, plus the workload header/pod row) and the workload-count summary line changing (e.g. `7 workloads` → `8 workloads`). If anything ELSE changed, STOP and report it.

- [ ] **Step 4: Run the full report suite twice (determinism)**

Run: `go test ./internal/report/ && go test ./internal/report/`
Expected: PASS both times (`TestGoldenInputCoversAllSections` now counts 8 workloads / 8 distinct modes, still ≥ 6). Then `go test ./...` once.

- [ ] **Step 5: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover Init:CrashLoopBackOff in the golden scan snapshot"
```

---

### Task 4: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md` (new subsection after `### ProbeFailure`; `## Status` list line 244)
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `website/docs/quickstart.md` (failure-mode list, line 6)
- Modify: `README.md` (detector bullet after the ProbeFailure bullet; `## Status` summary line 88)

- [ ] **Step 1: diagnostics.md**

Add this subsection immediately after the `### ProbeFailure` block's last line (`… no extra permission beyond the scan's existing event list).`) and before `### Node reservations`:

```markdown
### Init container failures

A pod stuck in its **init phase** because an init container is failing —
`Init:CrashLoopBackOff` (crash-looping), `Init:ImagePullBackOff` /
`Init:ErrImagePull` (its image can't be pulled), or `Init:OOMKilled` (killed for
exceeding its memory limit). `kubeagent` reads `Status.InitContainerStatuses` — the
slice the main-container crash detectors don't look at — and names which init
container is failing, its position, and the reason, e.g. `init container
"wait-for-db" (1/2), restartCount=6`. Init containers run sequentially and block the
pod, so at most one is failing; a pod whose inits all succeeded is left to the
main-container detectors (no overlap). Read-only; reads pod status already collected
(no new RBAC).
```

Then change the `## Status` sentence (line 244) from
`…VolumeAttachError (Multi-Attach), RestartLoop, and ProbeFailure pods, in text or JSON.`
to
`…VolumeAttachError (Multi-Attach), RestartLoop, ProbeFailure, and init-container failures, in text or JSON.`

- [ ] **Step 2: CHANGELOG.md**

Under `## [Unreleased]` → `### Added` (create the `### Added` sub-header if Unreleased is empty), add:

```markdown
- **InitContainer failure detector.** `scan` flags a pod blocked in its init phase —
  `Init:CrashLoopBackOff`, `Init:ImagePullBackOff` / `Init:ErrImagePull`, or
  `Init:OOMKilled` — reading `InitContainerStatuses` (which the main-container crash
  detectors don't look at) and naming which init container is failing, its position,
  and why. Read-only, always-on, no new RBAC.
```

- [ ] **Step 3: quickstart.md**

Change the failure-mode list tail (line 6) from
`…silent restart loops, and failing readiness/liveness/startup probes —`
to
`…silent restart loops, failing readiness/liveness/startup probes, and failing init containers —`

- [ ] **Step 4: README.md**

Add this bullet immediately after the `- **ProbeFailure** — …` bullet (which ends `  startup probe keeps failing; names the probe, container, and reason.`) and before `- **Node reservation check** …`:

```markdown
- **InitContainer failures** — a pod stuck in its init phase because an init
  container is crash-looping, can't pull its image, or was OOM-killed; names which
  init container and why.
```

Then change the `## Status` summary sentence (line 88) the same way as diagnostics:
`…VolumeAttachError (Multi-Attach), RestartLoop, ProbeFailure, and init-container failures, in text or JSON.`

- [ ] **Step 5: Verify docs build**

Run: `cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml --site-dir /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/site-ic`
Expected: exit 0, "Documentation built", no `WARNING` lines about these pages. (The red Material-for-MkDocs banner is cosmetic.) Then `export PATH=$PATH:/usr/local/go/bin && go build ./...` (sanity).

- [ ] **Step 6: Commit**

```bash
git add website/docs/features/diagnostics.md CHANGELOG.md website/docs/quickstart.md README.md
git commit -m "docs: document the InitContainer failure detector"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && go test ./...
gofmt -l internal/diagnose/initcontainer.go internal/diagnose/oomkilled.go internal/scan/scan.go internal/report/golden_test.go
go test ./internal/report -run TestGoldenScanOutput   # run twice: deterministic
```

All packages pass; gofmt prints nothing for the touched files; golden is stable. Confirm no `Co-Authored-By` trailer: `git log --format='%(trailers)' main..HEAD` prints nothing.
