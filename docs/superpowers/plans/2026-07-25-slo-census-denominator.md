# SLO Census Denominator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the watch daemon's SLI count every long-running workload, not just the ones already broken, so availability and window coverage mean what the documentation says they mean.

**Architecture:** `inventory.Prioritize` already walks the full assembled workload set on its way to producing the display-filtered list. Compute the census in that same pass and return it on `inventory.Result`; the watch daemon reads it instead of counting the filtered list.

**Tech Stack:** Go 1.26, standard library only, client-go fake clientset for I/O-level tests.

## Global Constraints

- The watch daemon stays strictly read-only toward the cluster: get/list/watch only, no writes, no LLM, no new RBAC.
- Standard library only — no new dependency; `go.mod` and `go.sum` must not change.
- The percentage-to-ratio conversion stays in exactly one place, `main.go`.
- `validateSLOTarget` stays the first statement of `watch.Run`.
- `Census` is tagged `json:"-"`: `inventory.Result` is serialized by `scan --output json` and snapshotted by the golden test. The golden file must not need regeneration.
- Long-running means every kind except `Job` and `CronJob`. Stated as an exclusion so unknown CRD-owned kinds count.
- Good means `!w.Flagged()`, the same predicate that decides display membership.
- TDD: write the failing test, run it, watch it fail, then implement.
- No Claude, Claude Code, or Anthropic attribution in any commit, comment, or doc.

---

### Task 1: The census type and its computation

**Files:**

- Modify: `internal/inventory/inventory.go` (the `Result` struct at :398-402 and `Prioritize` at :415-442)
- Test: `internal/inventory/inventory_test.go`

**Interfaces:**

- Consumes: `Workload.Flagged()`, unchanged.
- Produces: `inventory.Census{Good, Total int}` and `inventory.Result.Census`, read by Tasks 2 and 3.

- [ ] **Step 1: Write the failing tests**

Append to `internal/inventory/inventory_test.go`:

```go
func TestPrioritize_CensusCountsHiddenHealthyWorkloads(t *testing.T) {
	in := []Workload{
		{Namespace: "a", Name: "crash", Kind: "Deployment", Ready: 0, Desired: 1, Status: "Degraded"},
		{Namespace: "a", Name: "healthy", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running"},
		{Namespace: "a", Name: "also-healthy", Kind: "StatefulSet", Ready: 2, Desired: 2, Status: "Running"},
	}
	res := Prioritize(in, Opts{})
	// Only "crash" is displayed, but all three are long-running workloads.
	if len(res.Workloads) != 1 {
		t.Fatalf("expected 1 displayed workload, got %+v", res.Workloads)
	}
	if res.Census.Total != 3 {
		t.Errorf("Census.Total = %d, want 3 (the healthy majority must be counted)", res.Census.Total)
	}
	if res.Census.Good != 2 {
		t.Errorf("Census.Good = %d, want 2", res.Census.Good)
	}
}

func TestPrioritize_CensusExcludesJobAndCronJob(t *testing.T) {
	in := []Workload{
		{Namespace: "a", Name: "web", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running"},
		{Namespace: "a", Name: "backup", Kind: "CronJob", Status: "Idle"},
		{Namespace: "a", Name: "migrate", Kind: "Job", Status: "Complete"},
		{Namespace: "a", Name: "failed-job", Kind: "Job", Status: "Failed"},
	}
	res := Prioritize(in, Opts{})
	// Neither kind is expected to be continuously up, so neither belongs in an
	// availability figure — not even the failed Job, whose findings never clear.
	if res.Census.Total != 1 {
		t.Errorf("Census.Total = %d, want 1 (only the Deployment)", res.Census.Total)
	}
	if res.Census.Good != 1 {
		t.Errorf("Census.Good = %d, want 1", res.Census.Good)
	}
}

func TestPrioritize_CensusCountsUnderReplicatedAsBad(t *testing.T) {
	// The numerator defect, pinned directly: no Findings, but Ready < Desired.
	// len(Findings)==0 would call this good; Flagged() correctly does not.
	in := []Workload{
		{Namespace: "a", Name: "web", Kind: "Deployment", Ready: 1, Desired: 3, Status: "Degraded"},
	}
	res := Prioritize(in, Opts{})
	if res.Census.Total != 1 {
		t.Fatalf("Census.Total = %d, want 1", res.Census.Total)
	}
	if res.Census.Good != 0 {
		t.Errorf("Census.Good = %d, want 0: an under-replicated workload is not available", res.Census.Good)
	}
}

func TestPrioritize_CensusCountsUnknownKinds(t *testing.T) {
	// Assemble's pod rollup emits an arbitrary owner kind for CRD-owned pods.
	// The exclusion list must let those through.
	in := []Workload{
		{Namespace: "a", Name: "canary", Kind: "Rollout", Ready: 2, Desired: 2, Status: "Running"},
	}
	res := Prioritize(in, Opts{})
	if res.Census.Total != 1 || res.Census.Good != 1 {
		t.Errorf("Census = %+v, want {Good:1 Total:1}: an unknown controller kind is long-running", res.Census)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/inventory -run TestPrioritize_Census -v 2>&1 | tail -20
```

Expected: compile failure — `res.Census undefined (type Result has no field or method Census)`.

- [ ] **Step 3: Add the Census type and field**

In `internal/inventory/inventory.go`, replace the `Result` struct at :398-402 with:

```go
type Result struct {
	Workloads      []Workload
	HiddenRestarts int
	HiddenCron     int
	Census         Census `json:"-"`
}

// Census counts long-running workloads before display filtering: Total is every
// one of them, Good those that are not Flagged(). The watch daemon's SLI reads
// this rather than Workloads, which is filtered for display and omits exactly
// the healthy majority an availability figure needs.
//
// Job and CronJob are excluded: neither is expected to be continuously up. A
// CronJob idle between runs is not unavailable, and a Job that failed once
// carries its findings forever, permanently denting a figure it has no business
// influencing.
//
// json:"-" because inventory.Result is serialized by `scan --output json`, whose
// shape is a documented contract. The census feeds the watch SLI, not the report.
type Census struct {
	Good  int
	Total int
}
```

- [ ] **Step 4: Count in Prioritize**

In `Prioritize`, immediately after `for _, w := range workloads {` and before the existing `switch`, insert:

```go
		if w.Kind != "Job" && w.Kind != "CronJob" {
			res.Census.Total++
			if !w.Flagged() {
				res.Census.Good++
			}
		}
```

Nothing else in the function changes. The census is computed on the full input, before the display filter drops anything.

- [ ] **Step 5: Run the tests and watch them pass**

```bash
go test ./internal/inventory -run TestPrioritize -v 2>&1 | tail -20
```

Expected: all `TestPrioritize_*` pass, including the four new ones and the five that already existed.

- [ ] **Step 6: Mutation checks — run these and report the output**

Each mutation must break at least one test. A mutation that leaves the suite green is a finding to report, not a curiosity to move past.

```bash
# (a) drop Pod from the census as well — must fail a test if Pod is load-bearing
#     Change the condition to: w.Kind != "Job" && w.Kind != "CronJob" && w.Kind != "Rollout"
#     Expected: TestPrioritize_CensusCountsUnknownKinds fails.
# (b) use the wrong good predicate
#     Change `!w.Flagged()` to `len(w.Findings) == 0`
#     Expected: TestPrioritize_CensusCountsUnderReplicatedAsBad fails.
# (c) count after the filter instead of before
#     Move the census block inside the `case w.Flagged():` branch
#     Expected: TestPrioritize_CensusCountsHiddenHealthyWorkloads fails.
```

Apply each, run `go test ./internal/inventory -run TestPrioritize`, record the exact failure, then revert with `git checkout -- internal/inventory/inventory.go` and confirm `git status --short` is clean before the next.

- [ ] **Step 7: Full verification**

```bash
go build ./... && go vet ./... && go test ./... -race -count=1 && gofmt -l .
```

Expected: all pass, `gofmt -l .` prints nothing. In particular `go test ./internal/report` must pass without regenerating the golden file — if it fails, the `json:"-"` tag is missing.

- [ ] **Step 8: Commit**

```bash
git add internal/inventory/inventory.go internal/inventory/inventory_test.go
git commit -m "feat(inventory): count a workload census before display filtering

Prioritize already walks every assembled workload on its way to the filtered
display list. Counting there gives the watch daemon a denominator that includes
the healthy majority, which the filtered list drops by design.

Long-running means everything but Job and CronJob, stated as an exclusion so a
CRD-owned kind counts. Good is !Flagged(), the same predicate that decides
display membership, so the two cannot drift apart again."
```

---

### Task 2: Bind the census to the real pipeline

**Files:**

- Test: `internal/scan/scan_test.go`

**Interfaces:**

- Consumes: `inventory.Census` from Task 1, `scan.Evaluate`.
- Produces: nothing consumed by later tasks.

This is the test whose absence allowed the defect. Every existing test in this feature area hand-builds a `scan.Result` with healthy entries in `Inventory.Workloads` — a shape `scan.Evaluate` never emits. This one drives the real pipeline against the fake clientset.

- [ ] **Step 1: Write the failing test**

Append to `internal/scan/scan_test.go`:

```go
func TestEvaluate_CensusCountsHealthyWorkloadsTheReportHides(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:   appsv1.DeploymentSpec{Replicas: p32(1)},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "web", Ready: true}}}}
	cli := fake.NewSimpleClientset(node, dep, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	// The report shows nothing — that is correct, and it is exactly why the SLI
	// must not count this list.
	if got := len(res.Inventory.Workloads); got != 0 {
		t.Fatalf("healthy cluster should display no workloads, got %+v", res.Inventory.Workloads)
	}
	if res.Inventory.Census.Total == 0 {
		t.Fatal("Census.Total = 0 on a cluster with a running Deployment: the SLI would record nothing and coverage would never leave zero")
	}
	if res.Inventory.Census.Good != res.Inventory.Census.Total {
		t.Errorf("Census = %+v, want Good == Total on a healthy cluster", res.Inventory.Census)
	}
}

func TestEvaluate_CensusDropsGoodWhenAWorkloadBreaks(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:   appsv1.DeploymentSpec{Replicas: p32(1)},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 0}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "web", Ready: false, RestartCount: 8,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}}
	cli := fake.NewSimpleClientset(node, dep, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Inventory.Census.Total == 0 {
		t.Fatal("Census.Total = 0, want the broken Deployment counted")
	}
	if res.Inventory.Census.Good != 0 {
		t.Errorf("Census.Good = %d, want 0: the only workload is crash-looping", res.Inventory.Census.Good)
	}
}
```

- [ ] **Step 2: Run the tests**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/scan -run TestEvaluate_Census -v 2>&1 | tail -30
```

Expected: both pass, because Task 1 supplied the census. If `TestEvaluate_CensusCountsHealthyWorkloadsTheReportHides` fails on `Census.Total == 0`, the census is being computed after the filter — go back to Task 1 Step 4.

If the healthy fixture turns out to be flagged for a reason unrelated to this change (for example the pod-derived Ready count not matching Desired), fix the fixture until `len(res.Inventory.Workloads) == 0` holds, and say so in the report. Do not weaken the census assertions to accommodate a fixture that is not actually healthy.

- [ ] **Step 3: Verify the test is not vacuous**

```bash
# Revert Task 1's census block temporarily and confirm BOTH new tests fail.
```

Comment out the census block added in Task 1 Step 4, run `go test ./internal/scan -run TestEvaluate_Census`, record the exact failure output, then restore it and confirm `git status --short` shows only your intended changes. A test that still passes with the census removed is asserting nothing — report that as a finding.

- [ ] **Step 4: Full verification**

```bash
go build ./... && go vet ./... && go test ./... -race -count=1 && gofmt -l .
```

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan_test.go
git commit -m "test(scan): bind the workload census to the real pipeline

Every test in this feature area hand-builds a scan.Result with healthy entries
in Inventory.Workloads, a shape Evaluate never emits — which is how a census
that read the display-filtered list passed review twice.

These two drive Evaluate against the fake clientset instead: a healthy
Deployment must be absent from the report and present in the census, and a
crash-looping one must take Good to zero."
```

---

### Task 3: The daemon reads the census

**Files:**

- Modify: `internal/watch/slo.go` (delete `workloadCensus` at :106-114)
- Modify: `internal/watch/watch.go` (`applyResult`, the `good, total := workloadCensus(res)` line)
- Test: `internal/watch/slo_test.go`, `internal/watch/watch_test.go`

**Interfaces:**

- Consumes: `inventory.Result.Census` from Task 1.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the failing regression test**

Append to `internal/watch/watch_test.go`. Match the file's existing helpers for building a `scan.Result` and calling `applyResult` — read the neighbouring tests first and follow their construction exactly rather than inventing a new one.

```go
func TestApplyResult_HealthyClusterStillRecordsASample(t *testing.T) {
	// The defect this fixes: on a healthy cluster the display-filtered workload
	// list is empty, so the old census reported total==0, Observe recorded
	// nothing, and window coverage never left zero — the coverage gate could
	// never open and the daemon could never page.
	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	sloTr := slo.New(slo.Options{Target: 0.999, MaxSampleGap: 2 * time.Minute})
	sloN := newSLONotifier(time.Hour)

	var res scan.Result
	res.Health.Verdict = "Healthy"
	res.Inventory.Census = inventory.Census{Good: 5, Total: 5}
	// Workloads deliberately left empty: that is what a healthy cluster looks like.

	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	applyResult(m, tr, nil, sloTr, sloN, &res, 0, t0, nil)
	applyResult(m, tr, nil, sloTr, sloN, &res, 0, t0.Add(30*time.Second), nil)

	got := sloTr.Report(slo.Fast, t0.Add(30*time.Second))
	if got.Coverage <= 0 {
		t.Errorf("Coverage = %v, want > 0: a healthy cluster must accumulate window coverage", got.Coverage)
	}
	if got.Availability != 1 {
		t.Errorf("Availability = %v, want 1 on an all-good census", got.Availability)
	}
}
```

Add whatever imports the file is missing (`github.com/imantaba/kubeagent/internal/inventory`).

- [ ] **Step 2: Run it and watch it fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch -run TestApplyResult_HealthyClusterStillRecordsASample -v 2>&1 | tail -20
```

Expected: FAIL on `Coverage = 0, want > 0`. The old `workloadCensus` counts `res.Inventory.Workloads`, which the test leaves empty, so `Observe` returns before recording anything.

If it passes before the implementation, the test is not reaching the code path — stop and report rather than proceeding.

- [ ] **Step 3: Delete workloadCensus**

Remove this function from `internal/watch/slo.go` entirely (:106-114):

```go
func workloadCensus(res *scan.Result) (good, total int) {
	for _, w := range res.Inventory.Workloads {
		total++
		if len(w.Findings) == 0 {
			good++
		}
	}
	return good, total
}
```

Drop the `scan` import from `slo.go` if nothing else in the file uses it.

- [ ] **Step 4: Read the census in applyResult**

In `internal/watch/watch.go`, inside `applyResult`, replace:

```go
		good, total := workloadCensus(res)
		sloTr.Observe(good, total, now)
```

with:

```go
		c := res.Inventory.Census
		sloTr.Observe(c.Good, c.Total, now)
```

- [ ] **Step 5: Remove the tests that pinned the deleted function**

```bash
grep -n 'workloadCensus' internal/watch/*.go
```

Delete any test that calls `workloadCensus` directly. Those tests asserted the old behaviour against hand-built `Inventory.Workloads` fixtures and are exactly the tests that agreed with the bug. Do not port them — the behaviour they covered now lives in `internal/inventory` (Task 1) and `internal/scan` (Task 2), tested against the real pipeline.

- [ ] **Step 6: Run the tests and watch them pass**

```bash
go test ./internal/watch -count=1 -race 2>&1 | tail -20
```

Expected: pass, including the new regression test.

- [ ] **Step 7: Mutation check — run it and report the output**

```bash
# Change `sloTr.Observe(c.Good, c.Total, now)` to `sloTr.Observe(c.Good, 0, now)`
# Expected: TestApplyResult_HealthyClusterStillRecordsASample fails on Coverage.
```

Apply, run, record the exact failure, revert with `git checkout -- internal/watch/watch.go`, confirm `git status --short` shows only intended changes.

- [ ] **Step 8: Full verification**

```bash
go build ./... && go vet ./... && go test ./... -race -count=1 && gofmt -l .
```

- [ ] **Step 9: Commit**

```bash
git add internal/watch/slo.go internal/watch/watch.go internal/watch/slo_test.go internal/watch/watch_test.go
git commit -m "fix(watch): read the workload census instead of the display list

workloadCensus counted inventory.Result.Workloads, which Prioritize filters for
display: healthy-quiet workloads are always hidden. On a healthy cluster the
list is empty, so total was 0, Observe returned before recording anything, and
window coverage never left zero — the coverage gate could not open and the
daemon could not page. When it did record, good tested len(Findings)==0 while
membership was decided by Flagged(), so an under-replicated workload with no
finding counted as available.

Both follow from reading a list built to answer a different question. The
daemon now reads inventory.Result.Census, counted before the filter."
```

---

### Task 4: Correct the documentation

**Files:**

- Modify: `CHANGELOG.md` (under `## [Unreleased]`)
- Modify: `website/docs/features/watch-mode.md` (the `## SLO burn rate` section)

**Interfaces:**

- Consumes: everything from Tasks 1-3.
- Produces: nothing.

The shipped docs describe the intended behaviour, which the fix now delivers, so most of the text is already correct. Read the section before editing and change only what is actually wrong or missing.

- [ ] **Step 1: Check what the docs currently claim**

```bash
grep -n 'good/total\|no findings\|workload' website/docs/features/watch-mode.md | sed -n '1,40p'
```

The SLI definition must now say: every long-running workload in scope counts, Jobs and CronJobs are excluded, and a workload is good when it is not flagged — meaning no findings *and* not under-replicated *and* not Failed. If the current text says "workloads with no findings", it understates the predicate and must be corrected.

- [ ] **Step 2: Add the exclusion and the good predicate to the SLI definition**

Edit the SLI paragraph so it states both facts. Keep the existing worked example (3 of 200 workloads → 15x) — it is correct and is now actually producible.

- [ ] **Step 3: Add the changelog entry**

Under `## [Unreleased]`, add a `### Fixed` entry. The `### Added` entry for the SLO feature is already there from the previous branch and describes the intended behaviour, so do not rewrite it; add the fix alongside. Cover: what was counted before, why a healthy cluster recorded nothing, the under-replicated numerator bug, and what is counted now.

- [ ] **Step 4: Verify the docs build**

```bash
export PATH=$PATH:$HOME/.local/bin
(cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml 2>&1 | grep -E 'WARNING|ERROR|Documentation built')
```

Expected: `Documentation built`, no `WARNING` lines about these pages.

- [ ] **Step 5: Full verification**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && go test ./... -race -count=1 && gofmt -l .
```

- [ ] **Step 6: Commit**

```bash
git add CHANGELOG.md website/docs/features/watch-mode.md
git commit -m "docs: correct the SLI definition after the census fix

The SLI counts every long-running workload in scope, not only the ones the
report displays, and a workload is good when it is not flagged — which is
broader than having no findings: under-replicated and Failed both count as
unavailable."
```

---

## Self-Review

**1. Spec coverage.** The census type and its computation → Task 1. The binding test against the real pipeline → Task 2. Deleting `workloadCensus` and rewiring `applyResult` → Task 3. The `json:"-"` tag and the golden test → Task 1 Steps 3 and 7. Documentation → Task 4. The spec's "Out of scope" list adds no tasks by construction.

**2. Placeholders.** None. Every code step carries the actual code. Task 3 Step 1 deliberately tells the implementer to match the file's existing `applyResult` test construction rather than pinning a fixture shape this plan cannot see — that is an instruction, not a placeholder, and Step 2 gives the exact expected failure so a wrong construction is caught immediately.

**3. Type consistency.** `Census{Good, Total int}` is defined in Task 1 and read as `res.Inventory.Census.Good` / `.Total` in Tasks 2 and 3. `Prioritize(workloads []Workload, opts Opts) Result` keeps its signature. `slo.Tracker.Observe(good, total int, now time.Time)` is unchanged and still takes two ints.

**4. One thing the implementer must not get wrong.** The census must be counted on the full input, before the `switch` that drops workloads — not inside any of its branches. Task 1 Step 6 mutation (c) exists precisely to prove it. Counting inside a branch reproduces the original defect in a new location, and the display-level tests would not notice.
