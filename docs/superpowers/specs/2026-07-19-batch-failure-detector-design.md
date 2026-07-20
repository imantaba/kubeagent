# Job / CronJob failure check — design

**Status:** approved · **Date:** 2026-07-19 · **Type:** new check (v1 core)

## Goal

Extend kubeagent from pod failures to the **batch-workload class** it under-serves. A
Job that exhausted its retries (`BackoffLimitExceeded`) or hit its deadline
(`DeadlineExceeded`), or a **CronJob whose most-recent scheduled run failed**, is
currently silent — CronJobs are hidden by default and a failed Job shows a `Failed`
status with no named cause. A new `batchhealth.Annotate` attaches a `JobFailed` finding
naming the failure, and `Prioritize` is adjusted so a *failing* CronJob is shown by
default.

## Scope

**In:**
- A **standalone Job** with a `Failed` condition (`Type==JobFailed, Status==True`) →
  `JobFailed` finding on the Job workload, naming the reason (`BackoffLimitExceeded` /
  `DeadlineExceeded` / other) and the condition message.
- A **CronJob** whose **newest owned Job** (by `CreationTimestamp`) has a `Failed`
  condition → `JobFailed` finding on the CronJob workload, and that CronJob is shown by
  default (see §3).

**Out of scope (YAGNI):**
- Suspended CronJobs (`Spec.Suspend`) — often an intentional pause; false-positive risk.
- "Never run" / stale-last-success — clock-based and fuzzy.
- Flagging on an *older* historical failed run when the newest run is Running/Complete —
  only the latest run's outcome is considered (avoids noise from GC-retained history).
- No CLI flag — always-on. (A failed run whose Job was already GC'd — e.g.
  `failedJobsHistoryLimit: 0` — is not visible to a read-only scan and is not flagged;
  this is an accepted limitation, not a goal.)

## Global constraints

- **Read-only; NO new RBAC and NO new collection.** Jobs/CronJobs are already collected
  (`inputs.Jobs`, `inputs.CronJobs`) and the `batch` list verb is already granted.
- **Core, always-on** — runs in both the CLI `scan` and the `watch` daemon via the shared
  `scan.Evaluate`. No opt-in flag, no `watch.Config` change.
- `batchhealth.Annotate` is a **pure function** of its inputs; deterministic (newest job
  by `CreationTimestamp`; reads only `Job.Status.Conditions`; no wall-clock).
- `report.go`, `explain.go`, `collect`, `watch`, and all deploy/RBAC/Helm files stay
  **unchanged**. The only existing code touched is `inventory.Prioritize` (the CronJob
  branch) and the `scan.Evaluate` wiring (one `Annotate` call).
- **No `Co-Authored-By: Claude` trailer** on any commit. TDD.

## Design

### 1. `internal/batchhealth` — the Annotate package

Mirrors `netpolicy.Annotate` / `rollout.Annotate` (walk the assembled workloads, mutate
in place):

```go
package batchhealth

// Annotate appends a "JobFailed" finding to each Job/CronJob workload that has failed.
// For a "Job" workload it inspects that Job; for a "CronJob" workload it inspects the
// newest owned Job. Pure and read-only.
func Annotate(workloads []inventory.Workload, jobs []batchv1.Job, cronjobs []batchv1.CronJob)
```

Logic:
- Index the Jobs: `jobByKey["ns/name"] = *Job`; and bucket CronJob-owned Jobs:
  `cronJobJobs["ns/cronjobName"] = append(..., *Job)` for each Job whose controller
  ownerRef is a `CronJob`.
- For each workload:
  - `Kind == "Job"`: look up `jobByKey["ns/name"]`; if found and it has a `Failed`
    condition, append `jobFailedFinding(job, fromCronJob=false)`.
  - `Kind == "CronJob"`: `latest := newestJob(cronJobJobs["ns/name"])`; if non-nil and it
    has a `Failed` condition, append `jobFailedFinding(latest, fromCronJob=true)`.
- `newestJob` returns the Job with the greatest `CreationTimestamp` (nil for empty).
- Helpers `ownedByCronJob(job) (name string, ok bool)` (controller ownerRef, Kind
  `CronJob`) and `humanReason(reason string) string` map:
  `BackoffLimitExceeded → "exhausted its retries (BackoffLimitExceeded)"`,
  `DeadlineExceeded → "hit its deadline (DeadlineExceeded)"`, else the raw reason.

`jobFailedFinding(j, fromCronJob)` builds a `diagnose.Finding`:
- `Pod`: the workload's `"ns/name"` (identity only — batchhealth appends directly to
  `workloads[i].Findings`, not via the pod-key match, so this is metadata).
- `Issue`: `"JobFailed"`.
- `Reason`: `"the Job failed — " + humanReason(cond.Reason)` for a standalone Job;
  `"the most recent scheduled run failed — " + humanReason(cond.Reason)` for a CronJob.
- `Evidence`: the condition `Message` for a Job; `fmt.Sprintf("job %q: %s", j.Name,
  cond.Message)` for a CronJob (names which run failed).

Import note: `batchhealth` imports `internal/inventory` (for `Workload`), `internal/diagnose`
(for `Finding`), `batchv1`, `corev1`. No cycle — `inventory` does not import `batchhealth`
(same as `netpolicy`/`rollout`).

### 2. `scan.Evaluate` wiring

Insert one call **between `Assemble` and `Prioritize`** (so the finding flags the workload
before filtering):

```go
workloads := inventory.Assemble(inputs, findings)
batchhealth.Annotate(workloads, inputs.Jobs, inputs.CronJobs) // NEW
// … existing health/service/etc. assessment …
result := inventory.Prioritize(workloads, inventory.Opts{IncludeRestarts: …, IncludeCron: …})
```

(`netpolicy.Annotate`/`rollout.Annotate` remain after `Prioritize`, unchanged.)

### 3. The one existing-code change: `Prioritize` shows a *failing* CronJob

Today `Prioritize` hides **all** CronJobs without `--include-cron`, even flagged ones.
Change the CronJob branch so a **flagged** CronJob is shown at problem priority; a healthy
CronJob still needs the flag:

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

A CronJob is `Flagged()` only via a finding (its `Ready`/`Desired` are 0/0 and its status
is never literally `"Failed"`), so this surfaces exactly the failing ones — and as a bonus
fixes a latent gap where a CronJob with a crash-looping pod (a pod finding rolled up to it)
was also hidden. `HiddenCron` no longer counts a shown-because-flagged CronJob. (This
change is covered by the `inventory` Prioritize test, not the golden — `goldenInput`
renders pre-built workloads and does not call `Prioritize`.)

### 4. What does not change

- **`report.go`** — the `JobFailed` finding renders through the existing generic block
  (`⚠ JobFailed: <Reason>` / `↳ <Evidence>`). No new field, no format change.
- **`explain.go`** — unchanged. As an ordinary workload finding, `JobFailed` flows into
  the `--explain` prompt like `CrashLoopBackOff` does; its Reason/Evidence are k8s status
  text (no IPs/secrets), so this is correct and needs no special handling.
- **`collect`, `watch`, RBAC/deploy/Helm** — unchanged (Jobs/CronJobs already listed).
- **`inventory.Assemble`** — unchanged (it keeps seeding Job/CronJob workloads and their
  status; batchhealth adds the diagnostic finding separately).

### 5. Output example

```text
✗ shop/db-migrate  Job  Failed
    ⚠ JobFailed: the Job failed — exhausted its retries (BackoffLimitExceeded)
      ↳ Job has reached the specified backoff limit

✗ shop/nightly-report  CronJob  Idle  · schedule 0 2 * * *
    ⚠ JobFailed: the most recent scheduled run failed — hit its deadline (DeadlineExceeded)
      ↳ job "nightly-report-28901234": Job was active longer than specified deadline
```

## Error handling

- A Job/CronJob workload whose Job(s) are absent from `inputs` (unusual) → no finding.
- A CronJob with no owned Jobs collected (all GC'd, or never run) → `newestJob` nil → no
  finding.
- A Job whose newest condition is `Complete`/none → no finding.

## Testing

TDD, unit + integration (fake objects, no cluster):

- **`batchhealth_test.go`** (pure `Annotate` over hand-built `[]inventory.Workload` + Jobs/CronJobs):
  - a `"Job"` workload + a Job with a `JobFailed`/`BackoffLimitExceeded` condition → one
    `JobFailed` finding, Reason mentions "the Job failed", Evidence = the message.
  - a `"Job"` workload + a `Complete` Job → no finding.
  - a `"CronJob"` workload + owned Jobs where the **newest** has a `JobFailed` condition →
    finding, Reason mentions "most recent scheduled run", Evidence names the failed Job.
  - a `"CronJob"` workload where the newest owned Job is `Complete` but an **older** one
    Failed → **no** finding (latest-only).
  - a `"CronJob"` workload where the newest owned Job is Running → no finding.
  - `DeadlineExceeded` reason → "hit its deadline (DeadlineExceeded)".
- **`inventory` (Prioritize) test** — a flagged CronJob (one `Finding`) is included at
  `priorityProblem` even with `IncludeCron:false`; a healthy CronJob is still hidden
  (`HiddenCron` incremented) without the flag, and shown at `priorityCron` with it.
- **`scan` integration test** — `Evaluate` on a fake clientset with a failed standalone
  Job yields a workload carrying a `JobFailed` finding; a Complete Job yields none.
- **Golden** — add a failing standalone Job workload **and** a failing CronJob workload
  (each with a hand-built `JobFailed` finding) to the golden fixture and regenerate
  `testdata/golden-scan.txt` with `-update`. (goldenInput renders pre-built workloads, so
  this covers the finding rendering; the Prioritize visibility change is covered by the
  inventory test.)

## Files touched

- **Create:** `internal/batchhealth/batchhealth.go`, `internal/batchhealth/batchhealth_test.go`
- **Modify:** `internal/inventory/inventory.go` (`Prioritize` CronJob branch) + `inventory_test.go`
- **Modify:** `internal/scan/scan.go` (the `batchhealth.Annotate` call) + `scan_test.go`
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` (fixture + snapshot)
- **Docs:** `website/docs/features/diagnostics.md` (new subsection), `CHANGELOG.md`
  (`### Added`), `website/docs/quickstart.md` (failure-mode list), `README.md`.

## Non-goals recap

Suspended/never-run/stale CronJobs; older-historical-run flagging; a CLI flag; any change
to `report.go`, `explain.go`, `collect`, `watch`, `inventory.Assemble`, or RBAC beyond the
`Prioritize` CronJob-visibility tweak and the `Annotate` wiring.
