# Restart-loop detector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `RestartLoopDetector` that flags a currently-Running container which keeps exiting with a non-OOM error and restarting (`RestartCount ≥ 3`, young current run) — the flapping case the point-in-time detectors miss — emitting a durable `RestartLoop` finding.

**Architecture:** A pure detector reading pod status only, with `Now` injected via a struct field; wired into the shared `scan.Evaluate` (CLI + daemon). No new collection, no `FactsFrom` change, no RBAC change.

**Tech Stack:** Go 1.26. No new module dependency.

## Global Constraints

- **READ-ONLY.** Reads existing pod status only; no new List, no writes, no LLM.
- Detector stays pure `Detect(PodFacts) *Finding`; `Now` via a struct field for testable time.
- Finding flows to report/JSON/`--explain`/`kubeagent_findings{}` unchanged.
- Go at `/usr/local/go/bin` (`export PATH=$PATH:/usr/local/go/bin`).

---

### Task 1: `RestartLoopDetector` + scan wiring + tests

**Files:**
- Create: `internal/diagnose/restartloop.go`, `internal/diagnose/restartloop_test.go`
- Modify: `internal/scan/scan.go`, `internal/scan/scan_test.go`

- [ ] **Step 1: Write the failing detector tests — create `internal/diagnose/restartloop_test.go`**

```go
package diagnose

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var rlNow = time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

// flapPod: currently Running (started ranFor ago), with restartCount and a last
// termination of exit/reason that finished finishedAgo ago.
func flapPod(restarts int32, ranFor time.Duration, exit int32, reason string, finishedAgo time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "flapper"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				RestartCount: restarts,
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(rlNow.Add(-ranFor))}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: exit, Reason: reason, FinishedAt: metav1.NewTime(rlNow.Add(-finishedAgo)),
				}},
			}},
		},
	}
}

func TestRestartLoop_FlappingRunningPod(t *testing.T) {
	f := RestartLoopDetector{Now: rlNow}.Detect(PodFacts{Pod: flapPod(3, 20*time.Second, 1, "Error", 25*time.Second)})
	if f == nil || f.Issue != "RestartLoop" {
		t.Fatalf("want RestartLoop, got %+v", f)
	}
	if !strings.Contains(f.Evidence, "3 restarts") || !strings.Contains(f.Evidence, "exit 1") {
		t.Errorf("evidence missing detail: %q", f.Evidence)
	}
}

func TestRestartLoop_RecoveredOldRunIgnored(t *testing.T) {
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: flapPod(9, 30*time.Minute, 1, "Error", 30*time.Minute)}); f != nil {
		t.Errorf("stable run past the window must not flag, got %+v", f)
	}
}

func TestRestartLoop_BelowThresholdIgnored(t *testing.T) {
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: flapPod(2, 20*time.Second, 1, "Error", 25*time.Second)}); f != nil {
		t.Errorf("restarts<3 must not flag, got %+v", f)
	}
}

func TestRestartLoop_OOMKilledIgnored(t *testing.T) {
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: flapPod(5, 20*time.Second, 137, "OOMKilled", 25*time.Second)}); f != nil {
		t.Errorf("OOMKilled is covered elsewhere, got %+v", f)
	}
}

func TestRestartLoop_GracefulExitIgnored(t *testing.T) {
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: flapPod(5, 20*time.Second, 0, "Completed", 25*time.Second)}); f != nil {
		t.Errorf("exit 0 must not flag, got %+v", f)
	}
}

func TestRestartLoop_NotRunningIgnored(t *testing.T) {
	pod := flapPod(5, 20*time.Second, 1, "Error", 25*time.Second)
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: pod}); f != nil {
		t.Errorf("not-Running must not flag (covered by CrashLoop), got %+v", f)
	}
}

func TestRestartLoop_NeverRestartedIgnored(t *testing.T) {
	pod := flapPod(0, 20*time.Second, 0, "", 0)
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: pod}); f != nil {
		t.Errorf("never restarted must not flag, got %+v", f)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/diagnose/ -run TestRestartLoop`
Expected: build failure — `RestartLoopDetector` undefined.

- [ ] **Step 3: Implement — create `internal/diagnose/restartloop.go`**

```go
package diagnose

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	restartThreshold = 3
	restartRecency   = 10 * time.Minute
)

// RestartLoopDetector flags a currently-Running container that keeps exiting with
// a non-OOM error and restarting — a flapping pod the point-in-time detectors
// (CrashLoopBackOff fires only while Waiting; OOMKilled only for memory kills)
// miss. It reads the durable RestartCount + LastTerminationState, so it fires on
// every reconcile in a Running window rather than only during a crash instant.
type RestartLoopDetector struct{ Now time.Time }

func (d RestartLoopDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		run := cs.State.Running
		if run == nil {
			continue // not currently Running — the Waiting/CrashLoopBackOff cases are covered elsewhere
		}
		if d.Now.Sub(run.StartedAt.Time) > restartRecency {
			continue // recovered: has run stably past the window
		}
		if int(cs.RestartCount) < restartThreshold {
			continue
		}
		term := cs.LastTerminationState.Terminated
		if term == nil || term.ExitCode == 0 || term.Reason == "OOMKilled" {
			continue // no prior error termination, a graceful exit, or OOM (OOMKilledDetector covers it)
		}
		age := d.Now.Sub(term.FinishedAt.Time).Truncate(time.Second)
		return &Finding{
			Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
			Issue:    "RestartLoop",
			Reason:   "Container keeps exiting with an error and restarting",
			Evidence: fmt.Sprintf("container %q, %d restarts, last exit %d (%s), %s ago", cs.Name, cs.RestartCount, term.ExitCode, term.Reason, age),
		}
	}
	return nil
}
```

- [ ] **Step 4: Wire into `internal/scan/scan.go`**

In the detector slice (after `diagnose.VolumeAttachDetector{},`), add:

```go
		diagnose.RestartLoopDetector{Now: time.Now()},
```

(`time` is already imported in `scan.go`.)

- [ ] **Step 5: Add a `scan.Evaluate` integration test in `internal/scan/scan_test.go`**

Add `"time"` to the test file's imports, then:

```go
func TestEvaluate_FlagsRestartLoop(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	now := time.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "flapper"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", RestartCount: 4,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-20 * time.Second))}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(now.Add(-25 * time.Second)),
				}},
			}},
		},
	}
	cli := fake.NewSimpleClientset(node, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "RestartLoop" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a RestartLoop finding, got %+v", res.Inventory.Workloads)
	}
}
```

- [ ] **Step 6: Run tests + build + vet + gofmt**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./... && go vet ./... && gofmt -l internal/diagnose/ internal/scan/`
Expected: all packages pass (new detector + integration tests + every existing test), build ok, vet clean, gofmt clean.

- [ ] **Step 7: Commit**

```bash
git add internal/diagnose/ internal/scan/
git commit -m "feat(diagnose): RestartLoop detector for a flapping (running-but-restarting) container"
```

---

### Task 2: docs

**Files:**
- Modify: `README.md`, `CHANGELOG.md`, `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`

- [ ] **Step 1: `README.md` — add to the detector list**

Add `RestartLoop` next to the other detectors (and to the "## Status" sentence that lists them):

```markdown
- **RestartLoop** — a container that keeps exiting with a non-OOM error and
  restarting (≥ 3 restarts, still flapping) even though it is currently Running —
  the case `CrashLoopBackOff` misses.
```

- [ ] **Step 2: `CHANGELOG.md` — `[Unreleased] → Added`**

Add a `## [Unreleased]` section (above the latest release) with:

```markdown
## [Unreleased]

### Added

- **Restart-loop detection.** A new `RestartLoop` finding flags a container that
  keeps exiting with a non-OOM error and restarting (`RestartCount ≥ 3`, current
  run younger than 10 min) even though it is currently `Running` — a flapping pod
  the point-in-time detectors (`CrashLoopBackOff`/`OOMKilled`) miss. Durable
  (reads `RestartCount` + `lastState.Terminated`), so it appears in the scan,
  `--explain`, and `kubeagent_findings{issue="RestartLoop"}`. Read-only.
```

- [ ] **Step 3: `website/docs/features/diagnostics.md` — add it**

Add a `RestartLoop` entry to what's detected (and the Status sentence), noting it catches a flapping pod that reads as Running.

- [ ] **Step 4: `website/docs/features/watch-mode.md` — metric example**

In the `kubeagent_findings{issue="..."}` row, add `RestartLoop` to the example issue types.

- [ ] **Step 5: Build + commit**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: build succeeds, all packages `ok` (no code changed).

```bash
git add README.md CHANGELOG.md website/docs/features/diagnostics.md website/docs/features/watch-mode.md
git commit -m "docs: document the RestartLoop detector"
```

---

## Self-Review

**Spec coverage:**
- `RestartLoopDetector` (Running + young run + `RestartCount ≥ 3` + non-OOM error last-termination; `Now` via struct field) → Task 1. ✓
- Scan wiring (one detector-slice entry) → Task 1. ✓
- Docs (README, CHANGELOG, diagnostics, watch-mode) → Task 2. ✓
- READ-ONLY / pure / no new dep (Global Constraints) → reads pod status only; no List/RBAC/FactsFrom change. ✓

**Placeholder scan:** none — complete code in every step.

**Type/name consistency:** `RestartLoopDetector{Now}`, `Issue: "RestartLoop"`, `restartThreshold = 3`, `restartRecency = 10m` are used identically across the detector, its tests, and the wiring. The scan integration test adds the `"time"` import it needs. The finding flows to report/JSON/`--explain`/`kubeagent_findings{}` unchanged (they iterate findings by issue).
