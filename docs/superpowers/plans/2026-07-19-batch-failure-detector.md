# Job / CronJob Failure Check — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Attach a `JobFailed` finding to a failed standalone Job and to a CronJob whose newest run failed, and show a failing CronJob by default.

**Architecture:** A new pure `internal/batchhealth.Annotate(workloads, jobs)` (the `netpolicy`/`rollout` pattern) appends `JobFailed` findings to the already-assembled Job/CronJob workloads, called between `Assemble` and `Prioritize`. `inventory.Prioritize` is adjusted so a *flagged* CronJob is shown by default. No new collection, RBAC, or report changes.

**Tech Stack:** Go 1.26, standard library + `k8s.io/api/batch/v1` + `k8s.io/api/core/v1`, client-go fake clientset.

## Global Constraints

- **Read-only; NO new RBAC and NO new collection.** Jobs/CronJobs are already collected (`inputs.Jobs`, `inputs.CronJobs`); the `batch` list verb is already granted.
- **Core, always-on** — runs in both the CLI `scan` and the `watch` daemon via the shared `scan.Evaluate`. No opt-in flag, no `watch.Config` change.
- `batchhealth.Annotate` is **pure** and deterministic: newest job by `CreationTimestamp`; reads only `Job.Status.Conditions`; no wall-clock.
- `report.go`, `explain.go`, `collect`, `watch`, `inventory.Assemble`, and all deploy/RBAC/Helm files stay **unchanged**. The only existing code touched is the `inventory.Prioritize` CronJob branch and the `scan.Evaluate` wiring.
- **Exact names/strings (verbatim):**
  - Package `internal/batchhealth`; func `Annotate(workloads []inventory.Workload, jobs []batchv1.Job)`; helpers `ownedByCronJob`, `newestJob`, `jobFailedFinding`, `humanReason`.
  - `Finding.Issue == "JobFailed"`. Reason base: standalone → `the Job failed`; CronJob → `the most recent scheduled run failed`; suffix ` — ` + `humanReason(cond.Reason)` when non-empty. `humanReason`: `BackoffLimitExceeded` → `exhausted its retries (BackoffLimitExceeded)`, `DeadlineExceeded` → `hit its deadline (DeadlineExceeded)`, else the raw reason. Em dash `—` = U+2014.
  - Evidence: the condition `Message` (standalone); `fmt.Sprintf("job %q: %s", j.Name, cond.Message)` (CronJob).
- **TDD** — failing test first. **No `Co-Authored-By: Claude` trailer** on any commit.

---

### Task 1: `internal/batchhealth` — the Annotate package

**Files:**
- Create: `internal/batchhealth/batchhealth.go`
- Test: `internal/batchhealth/batchhealth_test.go`

**Interfaces:**
- Consumes: `inventory.Workload` (has `Namespace, Name, Kind string` and `Findings []diagnose.Finding`), `diagnose.Finding` (`Pod, Issue, Reason, Evidence string`).
- Produces: `func Annotate(workloads []inventory.Workload, jobs []batchv1.Job)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/batchhealth/batchhealth_test.go`:

```go
package batchhealth

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

func failedJob(ns, name, reason, message string) batchv1.Job {
	return batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: message},
		}},
	}
}

func cronOwner(name string) []metav1.OwnerReference {
	ctrl := true
	return []metav1.OwnerReference{{Kind: "CronJob", Name: name, Controller: &ctrl}}
}

func TestAnnotate_FailedJob(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "db-migrate", Kind: "Job"}}
	jobs := []batchv1.Job{failedJob("shop", "db-migrate", "BackoffLimitExceeded", "Job has reached the specified backoff limit")}
	Annotate(ws, jobs)
	if len(ws[0].Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(ws[0].Findings))
	}
	f := ws[0].Findings[0]
	if f.Issue != "JobFailed" {
		t.Errorf("Issue = %q, want JobFailed", f.Issue)
	}
	if want := "the Job failed — exhausted its retries (BackoffLimitExceeded)"; f.Reason != want {
		t.Errorf("Reason = %q, want %q", f.Reason, want)
	}
	if f.Evidence != "Job has reached the specified backoff limit" {
		t.Errorf("Evidence = %q", f.Evidence)
	}
}

func TestAnnotate_CompleteJobNotFlagged(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "done", Kind: "Job"}}
	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "done"},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	Annotate(ws, []batchv1.Job{job})
	if len(ws[0].Findings) != 0 {
		t.Errorf("a Complete Job must not be flagged, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_CronJobNewestRunFailed(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "nightly", Kind: "CronJob"}}
	older := failedJob("shop", "nightly-1", "BackoffLimitExceeded", "old failure")
	older.OwnerReferences = cronOwner("nightly")
	older.CreationTimestamp = metav1.Unix(1000, 0)
	newer := failedJob("shop", "nightly-2", "DeadlineExceeded", "Job was active longer than specified deadline")
	newer.OwnerReferences = cronOwner("nightly")
	newer.CreationTimestamp = metav1.Unix(2000, 0)
	Annotate(ws, []batchv1.Job{older, newer})
	if len(ws[0].Findings) != 1 {
		t.Fatalf("want 1 finding on the CronJob, got %d", len(ws[0].Findings))
	}
	f := ws[0].Findings[0]
	if want := "the most recent scheduled run failed — hit its deadline (DeadlineExceeded)"; f.Reason != want {
		t.Errorf("Reason = %q, want %q", f.Reason, want)
	}
	if want := `job "nightly-2": Job was active longer than specified deadline`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

func TestAnnotate_CronJobNewestCompleteOlderFailed(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "nightly", Kind: "CronJob"}}
	older := failedJob("shop", "nightly-1", "BackoffLimitExceeded", "old failure")
	older.OwnerReferences = cronOwner("nightly")
	older.CreationTimestamp = metav1.Unix(1000, 0)
	newer := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly-2", OwnerReferences: cronOwner("nightly"), CreationTimestamp: metav1.Unix(2000, 0)},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	Annotate(ws, []batchv1.Job{older, newer})
	if len(ws[0].Findings) != 0 {
		t.Errorf("newest run Complete -> CronJob not flagged even if an older run failed, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_CronJobNewestRunning(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "nightly", Kind: "CronJob"}}
	running := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly-3", OwnerReferences: cronOwner("nightly"), CreationTimestamp: metav1.Unix(3000, 0)},
		Status:     batchv1.JobStatus{Active: 1},
	}
	Annotate(ws, []batchv1.Job{running})
	if len(ws[0].Findings) != 0 {
		t.Errorf("a Running latest run must not be flagged, got %+v", ws[0].Findings)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/batchhealth/`
Expected: FAIL — `undefined: Annotate`.

- [ ] **Step 3: Write the package**

Create `internal/batchhealth/batchhealth.go`:

```go
// Package batchhealth attaches a "JobFailed" finding to Job/CronJob workloads whose run
// failed. For a "Job" workload it inspects that Job; for a "CronJob" workload it inspects
// the newest owned Job. Pure and read-only: the caller supplies the assembled workloads
// plus the Jobs/CronJobs. Mirrors netpolicy/rollout.Annotate.
package batchhealth

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate appends a "JobFailed" finding to each Job workload whose Job failed, and to
// each CronJob workload whose newest owned Job failed. CronJob→Jobs are derived from the
// Jobs' owner references, so the CronJob objects themselves are not needed.
func Annotate(workloads []inventory.Workload, jobs []batchv1.Job) {
	byKey := make(map[string]*batchv1.Job, len(jobs))
	cronJobJobs := map[string][]*batchv1.Job{}
	for i := range jobs {
		j := &jobs[i]
		byKey[j.Namespace+"/"+j.Name] = j
		if name, ok := ownedByCronJob(*j); ok {
			cronJobJobs[j.Namespace+"/"+name] = append(cronJobJobs[j.Namespace+"/"+name], j)
		}
	}
	for i := range workloads {
		w := &workloads[i]
		wkey := w.Namespace + "/" + w.Name
		switch w.Kind {
		case "Job":
			if j := byKey[wkey]; j != nil {
				if f := jobFailedFinding(*j, wkey, false); f != nil {
					w.Findings = append(w.Findings, *f)
				}
			}
		case "CronJob":
			if latest := newestJob(cronJobJobs[wkey]); latest != nil {
				if f := jobFailedFinding(*latest, wkey, true); f != nil {
					w.Findings = append(w.Findings, *f)
				}
			}
		}
	}
}

// ownedByCronJob returns the owning CronJob's name if the Job is controlled by one.
func ownedByCronJob(j batchv1.Job) (string, bool) {
	for _, o := range j.OwnerReferences {
		if o.Kind == "CronJob" && o.Controller != nil && *o.Controller {
			return o.Name, true
		}
	}
	return "", false
}

// newestJob returns the Job with the greatest CreationTimestamp, or nil.
func newestJob(jobs []*batchv1.Job) *batchv1.Job {
	var best *batchv1.Job
	for _, j := range jobs {
		if best == nil || j.CreationTimestamp.Time.After(best.CreationTimestamp.Time) {
			best = j
		}
	}
	return best
}

// jobFailedFinding returns a JobFailed finding if the Job has a Failed condition, else nil.
// wkey ("ns/name") identifies the workload; fromCronJob tailors the wording.
func jobFailedFinding(j batchv1.Job, wkey string, fromCronJob bool) *diagnose.Finding {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			base, evidence := "the Job failed", c.Message
			if fromCronJob {
				base = "the most recent scheduled run failed"
				evidence = fmt.Sprintf("job %q: %s", j.Name, c.Message)
			}
			reason := base
			if p := humanReason(c.Reason); p != "" {
				reason = base + " — " + p
			}
			return &diagnose.Finding{Pod: wkey, Issue: "JobFailed", Reason: reason, Evidence: evidence}
		}
	}
	return nil
}

// humanReason maps a Job failure reason to a plain-language phrase.
func humanReason(reason string) string {
	switch reason {
	case "BackoffLimitExceeded":
		return "exhausted its retries (BackoffLimitExceeded)"
	case "DeadlineExceeded":
		return "hit its deadline (DeadlineExceeded)"
	default:
		return reason
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/batchhealth/ -v`
Expected: PASS (all 5). Then `gofmt -l internal/batchhealth/batchhealth.go` (nothing) and `go vet ./internal/batchhealth/`.

- [ ] **Step 5: Commit**

```bash
git add internal/batchhealth/batchhealth.go internal/batchhealth/batchhealth_test.go
git commit -m "feat(batchhealth): flag failed Jobs and CronJobs whose latest run failed"
```

---

### Task 2: `Prioritize` shows a failing CronJob by default

**Files:**
- Modify: `internal/inventory/inventory.go` (the `case w.Kind == "CronJob":` branch in `Prioritize`, ~lines 419-425)
- Test: `internal/inventory/inventory_test.go` (add `TestPrioritize_FailingCronJobShownByDefault`; it is `package inventory`, so it can use `priorityProblem`/`priorityCron`)

**Interfaces:** Consumes `Workload`, `Prioritize`, `Opts`, `priorityProblem`, `priorityCron` (all in package `inventory`).

- [ ] **Step 1: Write the failing test**

Add to `internal/inventory/inventory_test.go`:

```go
func TestPrioritize_FailingCronJobShownByDefault(t *testing.T) {
	flagged := Workload{Namespace: "shop", Name: "nightly", Kind: "CronJob", Status: "Idle",
		Findings: []diagnose.Finding{{Issue: "JobFailed", Reason: "the most recent scheduled run failed"}}}
	healthy := Workload{Namespace: "shop", Name: "hourly", Kind: "CronJob", Status: "Idle"}
	res := Prioritize([]Workload{flagged, healthy}, Opts{}) // no IncludeCron
	shown := map[string]int{}
	for _, w := range res.Workloads {
		shown[w.Name] = w.Priority
	}
	if p, ok := shown["nightly"]; !ok || p != priorityProblem {
		t.Errorf("a flagged CronJob must be shown at priorityProblem(%d); shown=%+v", priorityProblem, shown)
	}
	if _, ok := shown["hourly"]; ok {
		t.Errorf("a healthy CronJob must stay hidden without --include-cron")
	}
	if res.HiddenCron != 1 {
		t.Errorf("HiddenCron = %d, want 1 (the healthy CronJob only)", res.HiddenCron)
	}
}
```

(If `inventory_test.go` does not already import `diagnose`, add `"github.com/imantaba/kubeagent/internal/diagnose"`.)

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/inventory/ -run TestPrioritize_FailingCronJobShownByDefault`
Expected: FAIL — the flagged CronJob is currently hidden (`shown["nightly"]` missing) and `HiddenCron` is 2.

- [ ] **Step 3: Change the CronJob branch**

In `internal/inventory/inventory.go`, replace the `case w.Kind == "CronJob":` block in `Prioritize`:

```go
		case w.Kind == "CronJob":
			if opts.IncludeCron {
				w.Priority = priorityCron
				res.Workloads = append(res.Workloads, w)
			} else {
				res.HiddenCron++
			}
```

with:

```go
		case w.Kind == "CronJob":
			switch {
			case w.Flagged():
				w.Priority = priorityProblem
				res.Workloads = append(res.Workloads, w)
			case opts.IncludeCron:
				w.Priority = priorityCron
				res.Workloads = append(res.Workloads, w)
			default:
				res.HiddenCron++
			}
```

- [ ] **Step 4: Run the inventory tests**

Run: `go test ./internal/inventory/`
Expected: PASS (new test + existing). Then `gofmt -l internal/inventory/inventory.go` (nothing) and `go vet ./internal/inventory/`.

- [ ] **Step 5: Commit**

```bash
git add internal/inventory/inventory.go internal/inventory/inventory_test.go
git commit -m "feat(inventory): show a flagged CronJob by default (only healthy ones stay hidden)"
```

---

### Task 3: Wire `batchhealth.Annotate` into scan.Evaluate

**Files:**
- Modify: `internal/scan/scan.go` (import `batchhealth`; one call after `inventory.Assemble` at ~line 141)
- Test: `internal/scan/scan_test.go` (add `TestEvaluate_FlagsFailedJob`; add the `batchv1` import)

**Interfaces:** Consumes `batchhealth.Annotate` (Task 1), the CronJob-visibility change (Task 2).

- [ ] **Step 1: Write the failing scan integration test**

Add to `internal/scan/scan_test.go` (add `batchv1 "k8s.io/api/batch/v1"` to the imports):

```go
func TestEvaluate_FlagsFailedJob(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-migrate"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"},
		}},
	}
	cli := fake.NewSimpleClientset(node, job)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "JobFailed" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a JobFailed finding, got %+v", res.Inventory.Workloads)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/scan/ -run TestEvaluate_FlagsFailedJob`
Expected: FAIL — no `JobFailed` finding (Annotate not called).

- [ ] **Step 3: Wire it in**

In `internal/scan/scan.go`: add the import `"github.com/imantaba/kubeagent/internal/batchhealth"`, and immediately after the line `workloads := inventory.Assemble(inputs, findings)` (~line 141) add:

```go
	batchhealth.Annotate(workloads, inputs.Jobs)
```

- [ ] **Step 4: Run the scan + batchhealth + inventory tests**

Run: `go test ./internal/scan/ ./internal/batchhealth/ ./internal/inventory/`
Expected: PASS. Then `gofmt -l internal/scan/scan.go` (nothing) and `go vet ./internal/scan/`.

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): run batchhealth.Annotate to flag failed Jobs/CronJobs"
```

---

### Task 4: Show failing Job + CronJob in the golden snapshot

**Files:**
- Modify: `internal/report/golden_test.go` (`goldenWorkloads`, ~line 84)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated, not hand-edited)

**Interfaces:** Consumes `Finding.Issue == "JobFailed"` with the exact strings from Task 1.

- [ ] **Step 1: Append a failing Job and a failing CronJob to the fixture**

In `internal/report/golden_test.go`, add these TWO elements as the LAST entries of the slice returned by `goldenWorkloads()` (append at the end so they render last):

```go
		{Namespace: "shop", Name: "db-migrate", Kind: "Job", Desired: 0, Ready: 0, Status: "Failed",
			Findings: []diagnose.Finding{{Pod: "shop/db-migrate", Issue: "JobFailed",
				Reason:   "the Job failed — exhausted its retries (BackoffLimitExceeded)",
				Evidence: "Job has reached the specified backoff limit"}}},
		{Namespace: "shop", Name: "nightly-report", Kind: "CronJob", Desired: 0, Ready: 0, Status: "Idle", Schedule: "0 2 * * *",
			Findings: []diagnose.Finding{{Pod: "shop/nightly-report", Issue: "JobFailed",
				Reason:   "the most recent scheduled run failed — hit its deadline (DeadlineExceeded)",
				Evidence: `job "nightly-report-28901234": Job was active longer than specified deadline`}}},
```

- [ ] **Step 2: Run the golden test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the rendered output now has the `db-migrate` / `nightly-report` blocks and a higher `N workloads failing` count not yet in the golden.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report/ -run TestGoldenScanOutput -update`
Then inspect `git diff internal/report/testdata/golden-scan.txt` — it must show ONLY: (a) two appended workload blocks — `✗ shop/db-migrate  Job  Failed` with a `⚠ JobFailed:` line, and `✗ shop/nightly-report  CronJob  Idle …` with a `⚠ JobFailed:` line — and (b) the `Needs attention:` summary's `N workloads failing` count increasing by 2. If anything ELSE changed, STOP and report it.

- [ ] **Step 4: Run the full report suite twice (determinism)**

Run: `go test ./internal/report/ && go test ./internal/report/`
Expected: PASS both times (`TestGoldenInputCoversAllSections` still passes — more workloads and a new `JobFailed` mode). Then `go test ./...` once.

- [ ] **Step 5: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover Job/CronJob failures in the golden scan snapshot"
```

---

### Task 5: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md` (new subsection)
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `website/docs/quickstart.md` (failure-mode list in the intro paragraph)
- Modify: `README.md` (detector list)

- [ ] **Step 1: diagnostics.md**

Add a subsection after the `### Init container failures` block (before `### Node reservations`):

```markdown
### Job / CronJob failures

`scan` flags a batch workload whose run failed: a standalone **Job** with a `Failed`
condition (`BackoffLimitExceeded` — exhausted its retries; `DeadlineExceeded` — hit its
`activeDeadlineSeconds`), and a **CronJob** whose most-recent scheduled run failed. It
names the cause on the workload — e.g. `⚠ JobFailed: the Job failed — exhausted its
retries (BackoffLimitExceeded)`. A **failing CronJob is shown by default** (previously all
CronJobs were hidden without `--include-cron`; healthy ones still are). Only the *latest*
scheduled run's outcome is considered, so an older, already-superseded failure is not
re-flagged. Read-only; Jobs/CronJobs are already listed, so it needs no extra permission.
```

Then, if the `## Status` sentence in `diagnostics.md` enumerates the detected pod modes
(`…RestartLoop, ProbeFailure, and init-container failures…`), append `, and failed
Jobs/CronJobs` to it; otherwise the subsection above suffices.

- [ ] **Step 2: CHANGELOG.md**

Under `## [Unreleased]` → `### Added` (create the sub-header if empty), add:

```markdown
- **Job / CronJob failure check.** `scan` flags a failed Job (`BackoffLimitExceeded` /
  `DeadlineExceeded`) and a CronJob whose most-recent run failed, naming the cause on the
  workload. A failing CronJob is now shown by default (healthy ones stay hidden behind
  `--include-cron`). Read-only, always-on, no new RBAC.
```

- [ ] **Step 3: quickstart.md**

In the intro paragraph, the failure-mode list ends `…, silent restart loops, failing
readiness/liveness/startup probes, and failing init containers —`. Add batch failures,
e.g. change the tail to `…, failing init containers, and failed Jobs/CronJobs —`.

- [ ] **Step 4: README.md**

Add a `- **JobFailed** — a Job or CronJob whose run failed (exhausted retries or hit its
deadline); a failing CronJob is shown even without \`--include-cron\`.` bullet after the
`InitContainer failures` bullet, and if the README `## Status` summary sentence lists the
detected modes, append `and failed Jobs/CronJobs` there too. (Run `grep -n "InitContainer\|init container\|RestartLoop" README.md` to locate the spots.)

- [ ] **Step 5: Verify docs build**

Run: `cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml --site-dir /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/site-batch`
Expected: exit 0, "Documentation built", no `WARNING` lines about these pages. Then `export PATH=$PATH:/usr/local/go/bin && go build ./...`.

- [ ] **Step 6: Commit**

```bash
git add website/docs/features/diagnostics.md CHANGELOG.md website/docs/quickstart.md README.md
git commit -m "docs: document the Job/CronJob failure check"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && go test ./...
gofmt -l internal/batchhealth/batchhealth.go internal/inventory/inventory.go internal/scan/scan.go internal/report/golden_test.go
go test ./internal/report -run TestGoldenScanOutput   # run twice: deterministic
```

All packages pass; gofmt clean on the touched files; golden stable. Confirm no `Co-Authored-By` trailer: `git log --format='%(trailers)' main..HEAD` prints nothing.
