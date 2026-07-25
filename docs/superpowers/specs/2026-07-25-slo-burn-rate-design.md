# SLO burn-rate signals — design

Theme E, slice 3. Turns the watch daemon's issue stream into an error-budget
signal an SRE can alert on, without new API calls, new RBAC, or any write.

Prerequisite: v0.56.0 (`internal/watchstate` issue tracking, `internal/alertstate`
per-object rollup, `internal/alert` webhook delivery).

## Problem

The daemon reports what is broken right now (`/issues`, `kubeagent_issues_active`)
and pages when an object breaks (slice 2). Neither answers the question an SRE
actually budgets against: *are we burning through our reliability allowance fast
enough to care?*

A cluster with one Deployment broken for thirty seconds and a cluster with thirty
Deployments broken for a day produce the same `kubeagent_issues_active > 0`
condition. Paging on that is alert fatigue. Paging on burn rate is not: it scales
the response to how much of the budget the current failure is consuming.

The daemon already has everything needed to compute it. `scan.Result.Inventory.Workloads`
lists every workload it evaluated — healthy or not — so both the numerator and the
denominator of an availability SLI are already in hand on every reconcile.

## Decisions

| Decision | Choice |
|----------|--------|
| SLI | Time-weighted workload availability |
| Breakdown | Cluster-wide only (no per-namespace label) |
| Output | Prometheus series **and** a gated alert through the existing sink |
| Storage | Fixed ring of one-minute buckets |
| Alert plumbing | Reuse `alert.Sink`; do **not** reuse `watchstate`/`alertstate` |
| Enablement | Off unless `--slo-target` is set |

### Why workload availability

Each reconcile contributes, weighted by elapsed time:

```
good  += dt * count(workloads with no findings)
total += dt * count(workloads)

SLI       = good / total
burn rate = (1 - SLI) / (1 - target)
```

A workload is "good" when `len(w.Findings) == 0` — the same predicate
`issueKeys` uses to decide whether to track it, so the SLI and the issue tracker
never disagree about what "broken" means.

Three of two hundred workloads broken for an hour against a 99.9% target gives
SLI 98.5%, burn 15× — a fast-burn page. The same three broken on a two-thousand
workload cluster gives burn 1.5× — visible, not paged. The signal scales with
the estimate, which a binary cluster-healthy SLI does not: any single stuck PVC
would pin that at maximum burn forever and the series would be useless on a real
cluster, which always has something minor broken.

The alternative of summing unhealthy object-seconds across *every* tracked kind
was rejected because there is no uniform denominator. `Inventory.Workloads` carries
its population; `ServiceIssues`, `PVCIssues` and the rest report only their issues,
so a ratio built across them would rest on inconsistent bases per kind.

### Why cluster-wide only

`watchstate.Key` is deliberately low-cardinality, and the daemon keeps every series
bounded. A `namespace` label multiplies the burn series by the namespace count.
Operators who want a per-namespace budget can run the daemon with `--namespace`,
which already narrows the whole scope — including the SLI population — to that one
namespace.

### Why reuse the Sink but not the Roller

Synthesizing a pseudo-object (`Kind: "SLO"`, `Name: "error-budget"`) and pushing it
through `watchstate` → `alertstate` would inherit transitions, dedup, repeat and
MTTR for free. It is rejected anyway: `/issues` is documented as a list of per-object
incidents, and a budget breach is not an object. It would appear in
`kubeagent_issues_active`, inflate `kubeagent_issues_new_total`, and land in the
MTTR average — surprising anyone reading those as object counts.

Reuse stops at `alert.Sink`, which is the part worth reusing: bounded queue, three
attempts with backoff, no retry on 4xx, URL redaction, and all three payload
encoders. `internal/slo` returns a pure firing verdict; `internal/watch` maps it to
an `alertstate.Notification` and enqueues it on the same sink the object alerts use.

## Architecture

```
reconcile ─→ scan.Result ─→ slo.Tracker.Observe(good, total, now)
                                    │
                                    ├─→ slo.Tracker.Report(now) ─→ metrics.render()
                                    └─→ slo.Tracker.Verdict(now) ─→ watch: Notification ─→ alert.Sink
```

`internal/slo` is pure and deterministic: no I/O, no goroutines, no wall-clock
reads — the caller passes `now`. Not safe for concurrent use; only the reconcile
loop touches it. Accessors return values, not internal pointers, so the metrics
handler renders a snapshot the loop cannot mutate underneath it.

### `internal/slo` — the accumulator

```go
// Package slo accumulates a time-weighted availability SLI into a fixed ring of
// one-minute buckets and reports multi-window error-budget burn rates.
package slo

// Options configures the tracker. A zero field takes its default.
type Options struct {
    Target       float64       // required, exclusive range (0,1) — e.g. 0.999
    MaxSampleGap time.Duration // cap on one sample's time weight, default 2m
}

// Window names the two burn windows. Both are fixed.
type Window string

const (
    Fast Window = "fast" // 1h
    Slow Window = "slow" // 6h
)

// Report is one window's computed state.
type Report struct {
    Window       Window
    Availability float64 // good/total over the window; 1 when total is 0
    BurnRate     float64 // (1-Availability)/(1-Target); 0 when total is 0
    Coverage     float64 // fraction of the window that has samples, 0..1
}

// Verdict is the alert decision. Firing is true only when every condition holds.
type Verdict struct {
    Firing      bool
    FiringSince time.Time // zero when not firing
    Fast, Slow  Report
}

func New(o Options) *Tracker
func (t *Tracker) Observe(good, total int, now time.Time)
func (t *Tracker) Report(w Window, now time.Time) Report
func (t *Tracker) Verdict(now time.Time) Verdict
func (t *Tracker) Target() float64
```

Fixed constants, not options:

| Constant | Value | Why fixed |
|----------|-------|-----------|
| `bucketWidth` | 1 minute | 360 buckets covers the slow window in ~14 KB (40-byte bucket) |
| `fastWindow` | 1 hour | Google SRE workbook fast-burn window |
| `slowWindow` | 6 hours | workbook slow-burn window |
| `fastBurnThreshold` | 14.4× | workbook pair for a 30-day budget |
| `slowBurnThreshold` | 6× | workbook pair for a 30-day budget |
| `minCoverage` | 0.6 | see "Why coverage gates the alert" |

Making these flags is YAGNI until an operator asks. They are documented so an
operator reading the metrics can reproduce the arithmetic.

### The ring

```go
type bucket struct {
    start       time.Time // bucket boundary this slot currently holds
    good, total float64   // workload-seconds
}
```

`ringSize = slowWindow / bucketWidth` = 360 slots, allocated once. A bucket's slot
is `unixMinute % ringSize`; a slot whose `start` does not match the requested
boundary is stale and reads as empty, which is how the ring self-expires without a
sweep.

**Bucket splitting.** A sample covers `[now-dt, now]` and its weight is apportioned
across every bucket that interval overlaps, proportional to the overlap. Attributing
the whole `dt` to the bucket containing `now` would smear weight past the window
edge and make short windows wrong whenever `dt` approaches `bucketWidth`.

**The `dt` cap.** `dt` is the elapsed time since the previous accepted sample,
clamped to `MaxSampleGap`. `internal/watch` passes 2× the configured heartbeat;
the package's own default when the field is zero is 2 minutes, which matches the
default heartbeat. A daemon blocked for two hours on
an unresponsive API server must not resume and emit one sample asserting the state
held throughout. Clamped-away time is simply never counted: it lowers coverage
rather than inventing history. The first sample after `New` has no predecessor and
contributes no weight — it only establishes the baseline timestamp.

**Errors do not sample.** `applyResult` already returns before `Observe` on
evaluation error, and the SLO tracker hangs off the same call site, inheriting the
invariant. An API blip is neither "all healthy" nor "all broken". The resulting gap
lowers coverage, which is the honest representation.

### Coverage

```
coverage = (seconds of the window carrying any sample) / (window length)
```

A bucket counts as covered when `total > 0`. Partial buckets at the leading edge
count proportionally.

**Why coverage gates the alert.** State is in-memory and resets on restart, exactly
as slice 1 established. A daemon that started five minutes ago has five minutes of
history inside what it calls a one-hour window; any badness in it computes to a
100% burn and would page immediately on every rollout. Requiring 60% coverage of
*both* windows means the slow window must be ~3.6 hours warm before it can page.
Coverage is exposed as its own series so a partially-warm daemon is visibly
warming rather than silently mute.

### Firing condition

```
Firing = fast.BurnRate >= 14.4
      && slow.BurnRate >= 6
      && fast.Coverage >= 0.6
      && slow.Coverage >= 0.6
```

Requiring both windows is what makes the signal multi-window: the fast window
catches a sharp outage quickly, and the slow window refuses to page for a blip
that has already passed. `FiringSince` is set on the transition to firing and
cleared on the transition out, so a breach that persists across reconciles keeps
one start time.

## Configuration

One flag, off by default:

```
--slo-target 99.9
```

Parsed as a **percentage**, matching how an SRE writes an SLO, and converted to a
ratio internally. Unset (or `0`) means no SLO tracking at all: no series rendered,
no verdict computed, no alerts. This mirrors alerting being off unless
`KUBEAGENT_ALERT_WEBHOOK` is set, and keeps v0.56.0 users' behavior unchanged on
upgrade.

Validation, at the same startup point as the alert config so a bad value fails fast
rather than hiding behind a cache sync:

- `--slo-target` outside `(0, 100)` exclusive is a startup error. `100` is rejected
  explicitly: a 100% target makes the error budget zero and the burn rate a division
  by zero.
- A target is meaningful without a webhook: the series still render, they just do
  not page. Alerting and SLO tracking are independent switches.

Helm: `slo.enabled` (default `false`) and `slo.target` (default `99.9`), rendering
the flag only when enabled. No Secret, no RBAC, no new env var.

## Metrics

Rendered only when SLO tracking is on:

| Series | Type | Meaning |
|--------|------|---------|
| `kubeagent_slo_target_ratio` | gauge | the configured target as a ratio |
| `kubeagent_slo_availability_ratio{window}` | gauge | good/total over the window |
| `kubeagent_slo_burn_rate{window}` | gauge | budget consumption multiple |
| `kubeagent_slo_error_budget_remaining_ratio` | gauge | over the slow window; clamped to `[0,1]` |
| `kubeagent_slo_window_coverage_ratio{window}` | gauge | how warm the window is |

`window` takes `fast` or `slow`. Eight series total, fixed cardinality.

`kubeagent_slo_error_budget_remaining_ratio` is `1 - slowBurn` clamped at zero:
a burn rate above 1× means the budget for the window is already spent, and a
negative "remaining" would be nonsense on a dashboard.

Note for the implementer: `metrics.render()`'s `gauge` closure formats with `%g`,
which is correct for every value here (ratios and small multiples). Do not add a
timestamp-valued SLO series without checking that — `%g` renders `1770000000` as
`1.77e+09`.

## Alert payload

The burn alert reuses `alertstate.Notification` and therefore all three encoders
unchanged:

| Field | Value |
|-------|-------|
| `Kind` | `SLO` |
| `Namespace` | `""` |
| `Name` | `error-budget` |
| `Issues` | `["ErrorBudgetBurn"]` — one name, not `FastBurn`: the condition requires *both* windows, so naming it after the fast one would misdescribe what fired |
| `FiringSince` | the verdict's `FiringSince` |
| `Status` | `firing` / `resolved` |

It is constructed in `internal/watch` and enqueued on the existing sink. It never
enters `watchstate` or `alertstate`, so it does not appear in `/issues` or in any
`kubeagent_issues_*` series.

Re-send cadence reuses the configured `--alert-repeat`, so an Alertmanager receiver
gets refreshed before `resolve_timeout` exactly as object alerts do.

## Invariants preserved

1. **Strictly read-only.** No new API calls; the SLI reads a `scan.Result` the
   daemon already computed. No writes, no new RBAC.
2. **No LLM in the watch path.**
3. **Error never samples.** Same call site and same ordering as `tr.Observe`.
4. **In-memory only.** State resets on restart, and coverage makes that visible
   instead of silently producing a wrong burn rate.
5. **Bounded memory.** The ring is allocated once at a fixed size.
6. **Off by default.** No flag, no behavior change.
7. **The alert sink is shared, not forked.** URL redaction, retry, backoff and the
   bounded queue apply unchanged.

## Error handling

| Condition | Behavior |
|-----------|----------|
| `total == 0` (no workloads in scope) | availability 1, burn 0, and the bucket records no weight — so coverage also stays 0 and the verdict can never fire. An empty namespace neither burns budget nor pages. |
| Evaluation error | no sample; coverage drops |
| Clock moves backwards | a sample with `now` before the last accepted sample is ignored |
| Gap longer than `MaxSampleGap` | weight clamped; the excess is uncounted |
| Coverage below the gate | metrics still render; the verdict cannot fire |
| Sink queue full | the burn notification is dropped and counted like any other, via the existing `DroppedQueueFull` counter |

## Testing

`internal/slo` is pure, so it is unit-tested with an injected `now` and no cluster:

- bucket splitting across a boundary, and a sample entirely inside one bucket
- window edges — weight exactly at, just inside, and just outside the cutoff
- ring wraparound: a sample 6h+ later must not read stale slots as current data
- the `dt` cap, including the first-sample-has-no-weight case
- coverage accounting, including partial leading buckets and error gaps
- `total == 0` and the backwards-clock guard
- the firing verdict: each of the four conditions independently withheld must
  prevent firing, and `FiringSince` must hold steady across consecutive firing
  reconciles

Each test must be non-vacuous: for the verdict tests specifically, assert that a
mutant which drops one condition from the conjunction fails the test. The queue
test in slice 2 and the object-count assertion in chaos scenario 12 both shipped
vacuous, so this is called out rather than assumed.

`internal/watch` gets fake-clientset coverage for flag validation (rejecting `0`,
`100`, and negatives at startup, before cache sync) and for the notification being
enqueued on breach.

Chaos gets a scenario, and it deliberately does **not** try to fake a full breach.
Filling a 6h window needs six hours; a scenario that shortened the windows would
need a test-only production flag, and one that claimed a breach after ninety
seconds would be asserting a lie. Instead the scenario asserts the property that
actually matters on a fresh daemon and cannot be unit-tested end to end — **that a
cold daemon does not page**:

- the SLO series render with the configured target once `--slo-target` is set
- breaking a workload moves `kubeagent_slo_availability_ratio` down and
  `kubeagent_slo_burn_rate` up, so the accumulator is wired to real scan data
- `kubeagent_slo_window_coverage_ratio` is well below the gate
- **zero** SLO notifications reach the webhook receiver despite a burn rate far
  above both thresholds

The scenario's recorded expectation states exactly that, so it cannot later be read
as claiming full-window coverage. The threshold arithmetic and the firing
transition are unit-tested with an injected clock, where six hours costs nothing.

## Packaging and docs

- `website/docs/features/watch-mode.md` — an `## SLO burn rate` section: what the
  SLI measures, the fixed constants and why, the coverage gate and the restart
  caveat, and a worked example of the arithmetic.
- `deploy/helm/kubeagent/values.yaml` + `templates/deployment.yaml` — `slo.enabled`,
  `slo.target`.
- `deploy/deployment.yaml` — commented-out example.
- `CHANGELOG.md` under `[Unreleased]`.
- `docs/go-concepts.md` — a ring buffer entry if it is the first in the repo
  (plain everyday example first, then the kubeagent example; no Python comparisons).
- `website/docs/roadmap.md` — mark slice 3 shipped at release time.

## Out of scope

- Per-namespace or per-workload burn series.
- Configurable windows, thresholds, or bucket width.
- Persisting SLI history across restarts.
- A second SLI (latency, request-based). This is availability only.
- PagerDuty as a receiver; it remains open from slice 2.
- Burn-rate-driven remediation. `--fix` stays out of the watch path.
