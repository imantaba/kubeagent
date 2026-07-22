# CreateContainerConfigError detector — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag a container (main or init) stuck in `Waiting` with reason `CreateContainerConfigError` — a referenced ConfigMap/Secret is missing or a key is absent — so the workload names *why* it can't start instead of showing only as degraded.

**Architecture:** A new pure `ConfigErrorDetector` in `internal/diagnose`, mirroring `imagepull.go`, registered in the `scan.go` detectors slice. It reads pod container statuses that are already collected; the kubelet's Waiting message names the missing object. The finding flows through the existing `diagnose.Run` → inventory → report/JSON/confidence path with no new collector, RBAC, or report change.

**Tech Stack:** Go 1.26, `k8s.io/api/core/v1`. Tests use fake pod objects (via the existing `podWaiting`/`podWithInit` helpers) and the fake clientset.

## Global Constraints

- **READ-ONLY.** The detector reads only the pod's container statuses; no cluster calls, no writes, no LLM.
- **Always-on; no flag.** No new collector, RBAC, watch gauge, `Result` field, or `report` change.
- **Pure & deterministic** — no clock, no state (unlike `RestartLoopDetector`).
- **Only `CreateContainerConfigError`** is flagged (not `CreateContainerError` / `InvalidImageName`). One finding per pod (first match; main containers before init).
- **Confidence `high`** — the issue must NOT be in the `confidence` package's `medium` set, so `confidence.ForIssue("CreateContainerConfigError")` returns `high` with no change to `confidence.go`.
- **v1 uses the standard-library `flag` package only** — no Cobra.
- **No `Co-Authored-By: Claude` trailer** (or any Claude attribution) on any commit. Every commit authored solely by the human.
- **TDD** — write the failing test first, watch it fail, then implement. **gofmt-clean.**
- Exact strings: Issue `CreateContainerConfigError`; Reason `a referenced ConfigMap or Secret is missing, or a required key is absent — the container cannot start` (em dash U+2014); Evidence `container "<name>": <msg>` (main) / `init container "<name>": <msg>` (init).

---

### Task 1: `ConfigErrorDetector` — the detector, registration, and end-to-end wiring

**Files:**
- Create: `internal/diagnose/configerror.go`
- Test: `internal/diagnose/configerror_test.go`
- Modify: `internal/scan/scan.go` (register the detector)
- Test: `internal/scan/scan_test.go` (integration test)
- Test: `internal/confidence/confidence_test.go` (high-classification pin)

**Interfaces:**
- Consumes: `diagnose.Finding`, `diagnose.PodFacts`, the `Detector` interface; the existing test helpers `podWaiting(ns, name, container, reason, message)` and `podWithInit(ns, name, initStatuses ...corev1.ContainerStatus)` (both in the `diagnose` test package).
- Produces: `type ConfigErrorDetector struct{}` implementing `Detect(facts PodFacts) *Finding`.

- [ ] **Step 1: Write the failing detector test**

Create `internal/diagnose/configerror_test.go`:

```go
package diagnose

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestConfigErrorDetector_MainContainer(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("shop", "api", "app", "CreateContainerConfigError", `configmap "app-config" not found`)}
	f := ConfigErrorDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Issue != "CreateContainerConfigError" {
		t.Errorf("Issue = %q", f.Issue)
	}
	if f.Reason != "a referenced ConfigMap or Secret is missing, or a required key is absent — the container cannot start" {
		t.Errorf("Reason = %q", f.Reason)
	}
	if !strings.Contains(f.Evidence, `container "app"`) || !strings.Contains(f.Evidence, "app-config") {
		t.Errorf("Evidence = %q", f.Evidence)
	}
	if f.Container != "app" {
		t.Errorf("Container = %q", f.Container)
	}
}

func TestConfigErrorDetector_InitContainer(t *testing.T) {
	init := corev1.ContainerStatus{
		Name:  "wait-db",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: `secret "db-creds" not found`}},
	}
	f := ConfigErrorDetector{}.Detect(PodFacts{Pod: podWithInit("shop", "api", init)})
	if f == nil {
		t.Fatal("expected a finding for an init container, got nil")
	}
	if !strings.HasPrefix(f.Evidence, `init container "wait-db"`) {
		t.Errorf("Evidence = %q, want it to start with init container", f.Evidence)
	}
}

func TestConfigErrorDetector_MainBeatsInit(t *testing.T) {
	pod := podWaiting("shop", "api", "app", "CreateContainerConfigError", `configmap "main" not found`)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  "wait",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: `configmap "init" not found`}},
	}}
	f := ConfigErrorDetector{}.Detect(PodFacts{Pod: pod})
	if f == nil || f.Container != "app" {
		t.Fatalf("main container must take precedence, got %+v", f)
	}
}

func TestConfigErrorDetector_OtherReason(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("shop", "api", "app", "CrashLoopBackOff", "")}
	if f := (ConfigErrorDetector{}).Detect(facts); f != nil {
		t.Fatalf("a different waiting reason must not fire, got %+v", f)
	}
}

func TestConfigErrorDetector_RunningPod(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Namespace, pod.Name = "shop", "api"
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	if f := (ConfigErrorDetector{}).Detect(PodFacts{Pod: pod}); f != nil {
		t.Fatalf("a running container must not fire, got %+v", f)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/diagnose/ -run TestConfigErrorDetector`
Expected: FAIL — `undefined: ConfigErrorDetector`.

- [ ] **Step 3: Write the detector**

Create `internal/diagnose/configerror.go`:

```go
package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ConfigErrorDetector flags a container stuck in CreateContainerConfigError — a
// referenced ConfigMap/Secret is missing, or a required key is absent, so the
// container cannot start. Scans main and init containers.
type ConfigErrorDetector struct{}

func (d ConfigErrorDetector) Detect(facts PodFacts) *Finding {
	if f := configErrorIn(facts, facts.Pod.Status.ContainerStatuses, "container"); f != nil {
		return f
	}
	return configErrorIn(facts, facts.Pod.Status.InitContainerStatuses, "init container")
}

func configErrorIn(facts PodFacts, statuses []corev1.ContainerStatus, kind string) *Finding {
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && w.Reason == "CreateContainerConfigError" {
			return &Finding{
				Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:     "CreateContainerConfigError",
				Reason:    "a referenced ConfigMap or Secret is missing, or a required key is absent — the container cannot start",
				Evidence:  fmt.Sprintf("%s %q: %s", kind, cs.Name, w.Message),
				Container: cs.Name,
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run detector test to verify it passes**

Run: `go test ./internal/diagnose/ -run TestConfigErrorDetector`
Expected: PASS.

- [ ] **Step 5: Write the confidence pin (failing) and register the detector**

Add `"CreateContainerConfigError"` to the `high` string slice in `internal/confidence/confidence_test.go`'s `TestForIssue` (the slice that asserts each entry returns `"high"`). Run `go test ./internal/confidence/ -run TestForIssue` — it should PASS immediately (the default for any non-medium issue is `high`); this pins that `CreateContainerConfigError` is never accidentally added to the `medium` set.

Then add the integration test to `internal/scan/scan_test.go` (imports `corev1`, `metav1`, the fake clientset are present):

```go
func TestEvaluate_ConfigErrorDetected(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-abc"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: `configmap "app-config" not found`}},
		}}},
	}
	cli := fake.NewSimpleClientset(pod)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "CreateContainerConfigError" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected a CreateContainerConfigError finding, got workloads %+v", res.Inventory.Workloads)
	}
}
```

> Match `res.Inventory.Workloads` to how the scan `Result` exposes workloads — confirm the field name against a neighbouring scan test (e.g. one that inspects `res.Inventory` / `res.Inventory.Workloads`). Use the real accessor.

Run: `go test ./internal/scan/ -run TestEvaluate_ConfigErrorDetected`
Expected: FAIL — the detector isn't registered yet (no finding).

- [ ] **Step 6: Register the detector in `scan.go`**

In `internal/scan/scan.go`, add to the `detectors := []diagnose.Detector{…}` slice (next to the other pod detectors):

```go
		diagnose.ConfigErrorDetector{},
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/diagnose/ ./internal/scan/ ./internal/confidence/`
Expected: PASS. Then `gofmt -l internal/diagnose/configerror.go internal/diagnose/configerror_test.go internal/scan/scan.go internal/scan/scan_test.go internal/confidence/confidence_test.go` prints nothing.

- [ ] **Step 8: Commit**

```bash
git add internal/diagnose/ internal/scan/ internal/confidence/
git commit -m "feat(diagnose): flag CreateContainerConfigError (missing ConfigMap/Secret)"
```

---

### Task 2: Golden snapshot + docs

**Files:**
- Modify: `internal/report/golden_test.go` (add a workload with the finding)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerate)
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** Consumes the rendering behavior; the golden test renders a pre-built `Input`.

- [ ] **Step 1: Add a workload with a CreateContainerConfigError finding to the fixture**

In `internal/report/golden_test.go`, in the `goldenInput` builder's `Result.Workloads` list (where the other failing workloads like `shop/web` CrashLoop and `shop/api` ImagePull are declared), add one workload:

```go
		{Namespace: "shop", Name: "worker", Kind: "Deployment", Desired: 2, Ready: 0, Status: "Degraded",
			Findings: []diagnose.Finding{{Pod: "shop/worker-7c9f-x", Issue: "CreateContainerConfigError",
				Reason:    "a referenced ConfigMap or Secret is missing, or a required key is absent — the container cannot start",
				Evidence:  `container "worker": configmap "worker-config" not found`, Container: "worker"}}},
```

(The Confidence field is left unset — `high` is the unmarked default, so the finding renders with no confidence tag, matching how a real scan renders it.)

- [ ] **Step 2: Confirm the golden test fails (snapshot drift)**

Run: `go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the new workload's two lines are absent from the snapshot, and the `N workloads failing` count changed.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report -run TestGoldenScanOutput -update`
Then inspect: `git diff internal/report/testdata/golden-scan.txt` — it must add the `shop/worker` block (`✗ shop/worker  Deployment  0/2 Degraded` + the `⚠ CreateContainerConfigError:` line + the `↳ container "worker": configmap "worker-config" not found` evidence line) and bump the `N workloads failing` count in the attention line by one. No unrelated lines change.

- [ ] **Step 4: Run the full report suite**

Run: `go test ./internal/report/`
Expected: PASS (the `TestGoldenInputCoversAllSections` guard still holds — the workload count only grew).

- [ ] **Step 5: Update docs**

- `website/docs/features/diagnostics.md`: add `CreateContainerConfigError` to the failure-modes list — a container can't start because a referenced ConfigMap/Secret is missing or a key is absent; the finding surfaces the kubelet message naming the object; covers main and init containers.
- `README.md`: add it to the detector list.
- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`, add a bullet:
  ```
  - **Missing-config detection (`CreateContainerConfigError`).** `scan` now flags a
    container (main or init) that can't start because a referenced ConfigMap or
    Secret is missing, or a required key is absent — naming the object from the
    kubelet message. Previously such a workload showed only as degraded with no
    explaining finding. Read-only (no new flag or metric).
  ```
- `website/docs/roadmap.md`: add it to the Shipped list under the Theme-B "deeper diagnosis" detectors.

- [ ] **Step 6: Verify docs build (only website/ changed)**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (use the venv mkdocs if not on PATH: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, "Documentation built", no WARNING about your pages.

- [ ] **Step 7: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt website/ README.md CHANGELOG.md
git commit -m "test+docs: golden coverage and documentation for CreateContainerConfigError"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the release skill owns this. Touches `internal/diagnose` (new detector) + one line of `internal/scan` — no collect/cluster/watch/RBAC/Helm change → **LIGHTWEIGHT SMOKE** gate (a Kind pod referencing a nonexistent ConfigMap; confirm the finding renders). **Minor** version bump **v0.40.0 → v0.41.0**; **patch** chart bump (no Helm template change — the bump script's default patch is correct; do NOT override to minor). Hold for the user's explicit "run release and push".
