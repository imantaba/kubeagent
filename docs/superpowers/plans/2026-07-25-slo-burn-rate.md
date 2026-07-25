# SLO Burn-Rate Signals Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the watch daemon's per-reconcile workload census into a multi-window error-budget burn-rate signal, exposed as Prometheus series and — gated on window coverage — pushed through the existing alert sink.

**Architecture:** A new pure package `internal/slo` accumulates time-weighted workload availability into a fixed ring of one-minute buckets and reports burn rates over a 1h and a 6h window. `internal/watch` samples it from the reconcile loop at the same call site as the issue tracker, renders its report as metrics, and maps a firing verdict to an `alertstate.Notification` on the sink that slice 2 already built.

**Tech Stack:** Go 1.26, standard library only. No new dependencies. Tests use the standard `testing` package and, for the watch wiring, client-go's fake clientset.

## Global Constraints

Copied verbatim from the spec. Every task's requirements implicitly include these.

- **Strictly read-only.** No new API calls, no writes, no new RBAC. The SLI reads a `scan.Result` the daemon already computed.
- **No LLM in the watch path.**
- **`internal/slo` is pure:** no I/O, no goroutines, no wall-clock reads — the caller passes `now`. Not safe for concurrent use; only the reconcile loop touches it.
- **In-memory only.** State resets on restart. Coverage makes that visible rather than silently producing a wrong burn rate.
- **Bounded memory.** The ring is allocated once at a fixed size.
- **Off by default.** Unset `--slo-target` means no series, no verdict, no alerts. v0.56.0 behavior must be unchanged on upgrade.
- **Reuse `alert.Sink`, never `watchstate`/`alertstate` tracking.** The burn notification must not appear in `/issues` or in any `kubeagent_issues_*` series.
- **Error never samples.** On evaluation error, `applyResult` returns before `tr.Observe`; the SLO sample must sit on the same side of that return.
- **No `Co-Authored-By: Claude` trailer** on any commit, and no Claude/Anthropic attribution anywhere in code, comments, docs, or commit messages.
- **Never log a webhook URL beyond `scheme://host`.**

Fixed constants — exact values, not configurable:

| Constant | Value |
|----------|-------|
| `bucketWidth` | `time.Minute` |
| `fastWindow` | `time.Hour` |
| `slowWindow` | `6 * time.Hour` |
| `fastBurnThreshold` | `14.4` |
| `slowBurnThreshold` | `6.0` |
| `minCoverage` | `0.6` |
| `defaultMaxSampleGap` | `2 * time.Minute` |
| `ringSize` | `int(slowWindow / bucketWidth)` = 360 |

Exact metric names:

```
kubeagent_slo_target_ratio
kubeagent_slo_availability_ratio{window="fast"|"slow"}
kubeagent_slo_burn_rate{window="fast"|"slow"}
kubeagent_slo_error_budget_remaining_ratio
kubeagent_slo_window_coverage_ratio{window="fast"|"slow"}
```

Exact alert payload identity: `Kind: "SLO"`, `Namespace: ""`, `Name: "error-budget"`, `Issues: ["ErrorBudgetBurn"]`.

## File Structure

| File | Responsibility |
|------|----------------|
| `internal/slo/slo.go` (new) | The whole pure package: ring, `Tracker`, `Report`, `Verdict`. One file — the spec's API is ~180 lines and splitting a ring from its only consumer would hurt more than help. |
| `internal/slo/slo_test.go` (new) | Table-driven unit tests with an injected clock. |
| `internal/watch/watch.go` (modify) | `Config.SLOTarget`; construct the tracker; sample it in `applyResult`; map the verdict to a notification. |
| `internal/watch/slo.go` (new) | The verdict→notification bridge and its firing-edge state, kept out of `watch.go` so the reconcile loop stays readable. |
| `internal/watch/metrics.go` (modify) | Hold the latest SLO report; render the five series. |
| `internal/watch/slo_test.go` (new) | Bridge and metrics-rendering tests. |
| `internal/watch/watch_test.go` (modify) | Startup validation of `--slo-target`. |
| `main.go` (modify) | The `--slo-target` flag, percentage→ratio conversion, usage string. |
| `deploy/helm/kubeagent/values.yaml`, `templates/deployment.yaml` (modify) | `slo.enabled`, `slo.target`. |
| `deploy/deployment.yaml` (modify) | Commented-out example. |
| `chaos/run.sh` (modify) | Scenario 13. |
| `website/docs/features/watch-mode.md`, `CHANGELOG.md`, `docs/go-concepts.md` (modify) | Docs. |

---

### Task 1: `internal/slo` — the ring and time-weighted accumulation

**Files:**

- Create: `internal/slo/slo.go`
- Test: `internal/slo/slo_test.go`

**Interfaces:**

- Consumes: nothing (leaf package, stdlib only).
- Produces: `slo.Options{Target float64; MaxSampleGap time.Duration}`, `slo.New(Options) *Tracker`, `(*Tracker).Observe(good, total int, now time.Time)`, `(*Tracker).Target() float64`. Task 2 adds `Report`/`Verdict` to the same type.

- [ ] **Step 1: Write the failing test**

Create `internal/slo/slo_test.go`:

```go
package slo

import (
	"math"
	"testing"
	"time"
)

// base is an arbitrary fixed instant on a bucket boundary. Tests are pure: every
// time comes from base, never from the wall clock.
var base = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func approx(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestObserve_FirstSampleCarriesNoWeight(t *testing.T) {
	tr := New(Options{Target: 0.999})
	tr.Observe(10, 10, base)
	g, tot := tr.sum(base.Add(-time.Hour), base)
	approx(t, g, 0, "good")
	approx(t, tot, 0, "total")
}

func TestObserve_WeightsBySecondsSinceLastSample(t *testing.T) {
	tr := New(Options{Target: 0.999})
	tr.Observe(10, 10, base)                     // baseline only
	tr.Observe(8, 10, base.Add(30*time.Second))  // 30s x (8 good, 10 total)
	g, tot := tr.sum(base, base.Add(time.Minute))
	approx(t, g, 30*8, "good workload-seconds")
	approx(t, tot, 30*10, "total workload-seconds")
}

func TestObserve_SplitsWeightAcrossBucketBoundary(t *testing.T) {
	tr := New(Options{Target: 0.999})
	// baseline 20s before the boundary, next sample 40s after it: the 60s of
	// weight must land 20s in the earlier bucket and 40s in the later one.
	tr.Observe(1, 1, base.Add(-20*time.Second))
	tr.Observe(1, 1, base.Add(40*time.Second))

	early, _ := tr.sum(base.Add(-time.Minute), base)
	late, _ := tr.sum(base, base.Add(time.Minute))
	approx(t, early, 20, "weight in the earlier bucket")
	approx(t, late, 40, "weight in the later bucket")
}

func TestObserve_ClampsGapToMaxSampleGap(t *testing.T) {
	tr := New(Options{Target: 0.999, MaxSampleGap: time.Minute})
	tr.Observe(1, 1, base)
	tr.Observe(1, 1, base.Add(time.Hour)) // a 1h stall must contribute 1m, not 1h
	_, tot := tr.sum(base, base.Add(2*time.Hour))
	approx(t, tot, 60, "clamped total")
}

func TestObserve_IgnoresBackwardsClock(t *testing.T) {
	tr := New(Options{Target: 0.999})
	tr.Observe(1, 1, base)
	tr.Observe(1, 1, base.Add(time.Minute))
	tr.Observe(1, 1, base.Add(30*time.Second)) // earlier than the last accepted sample
	_, tot := tr.sum(base, base.Add(2*time.Minute))
	approx(t, tot, 60, "total unchanged by the backwards sample")
}

func TestObserve_ZeroTotalRecordsNoWeight(t *testing.T) {
	tr := New(Options{Target: 0.999})
	tr.Observe(0, 0, base)
	tr.Observe(0, 0, base.Add(time.Minute))
	_, tot := tr.sum(base, base.Add(2*time.Minute))
	approx(t, tot, 0, "an empty scope accumulates nothing")
}

func TestObserve_RingWrapsWithoutReadingStaleSlots(t *testing.T) {
	tr := New(Options{Target: 0.999, MaxSampleGap: time.Minute})
	tr.Observe(1, 1, base)
	tr.Observe(1, 1, base.Add(time.Minute)) // 60s of weight at base+0..1m

	// Exactly one full lap later, so the new writes land on the SAME ring slots
	// the first two used. Those slots must read as empty for the old timestamps,
	// not as live data from the previous lap.
	late := base.Add(6 * time.Hour)
	tr.Observe(1, 1, late)
	tr.Observe(1, 1, late.Add(time.Minute))

	_, old := tr.sum(base, base.Add(2*time.Minute))
	approx(t, old, 0, "the stale lap must not be readable")
	_, fresh := tr.sum(late, late.Add(2*time.Minute))
	approx(t, fresh, 60, "the current lap must be")
}

func TestNew_DefaultsMaxSampleGap(t *testing.T) {
	tr := New(Options{Target: 0.999})
	if tr.opts.MaxSampleGap != defaultMaxSampleGap {
		t.Errorf("MaxSampleGap = %v, want %v", tr.opts.MaxSampleGap, defaultMaxSampleGap)
	}
}

func TestTarget(t *testing.T) {
	if got := New(Options{Target: 0.995}).Target(); got != 0.995 {
		t.Errorf("Target() = %v, want 0.995", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/slo/ -run Test -v
```

Expected: FAIL to build — `undefined: New`, `undefined: Options`, `undefined: defaultMaxSampleGap`.

- [ ] **Step 3: Write the implementation**

Create `internal/slo/slo.go`:

```go
// Package slo turns a per-reconcile workload census into a time-weighted
// availability SLI and reports multi-window error-budget burn rates.
//
// Pure and deterministic — no I/O, no goroutines, and no wall-clock reads: the
// caller passes now. A Tracker is not safe for concurrent use; the daemon
// touches it only from its reconcile loop.
//
// The SLI is availability over time, not over requests: each sample contributes
// its workload counts weighted by the seconds since the previous sample, so an
// outage costs budget in proportion to how long it lasted and how much of the
// estate it took down.
package slo

import "time"

// Fixed shape. These are constants rather than Options fields on purpose: the
// window/threshold pair comes from the Google SRE workbook's multi-window
// recipe, and letting operators mix their own pair produces alerting that is
// either deafening or silent with no way to tell which. Documented so anyone
// reading the metrics can reproduce the arithmetic.
const (
	bucketWidth = time.Minute
	fastWindow  = time.Hour
	slowWindow  = 6 * time.Hour

	fastBurnThreshold = 14.4
	slowBurnThreshold = 6.0
	minCoverage       = 0.6

	defaultMaxSampleGap = 2 * time.Minute

	ringSize = int(slowWindow / bucketWidth) // 360
)

// Options configures the tracker. A zero field takes its default.
type Options struct {
	// Target is the SLO as a ratio in the exclusive range (0,1) — 0.999 for
	// "99.9% of workload-time healthy". Validated by the caller.
	Target float64
	// MaxSampleGap caps how much time weight a single sample may carry. A daemon
	// blocked on an unresponsive API server must not resume and assert that the
	// cluster's last known state held for the whole stall.
	MaxSampleGap time.Duration
}

// bucket is one minute of accumulated workload-seconds. start records which
// minute the slot currently holds, so a slot from a previous lap around the ring
// reads as empty instead of as live data.
type bucket struct {
	start       time.Time
	good, total float64
}

// Tracker accumulates the SLI into a fixed ring.
type Tracker struct {
	opts Options
	ring []bucket
	last time.Time // the last accepted sample's instant; zero before the first
}

// New returns a Tracker with the ring preallocated. Memory is fixed for the
// process lifetime: 360 buckets, allocated once, never grown.
func New(o Options) *Tracker {
	if o.MaxSampleGap <= 0 {
		o.MaxSampleGap = defaultMaxSampleGap
	}
	return &Tracker{opts: o, ring: make([]bucket, ringSize)}
}

// Target returns the configured SLO ratio.
func (t *Tracker) Target() float64 { return t.opts.Target }

// Observe folds one evaluation's workload census into the ring: good is the
// number of workloads with no findings, total the number evaluated.
//
// The first sample establishes the baseline instant and contributes no weight —
// there is no preceding interval to attribute it to. A sample at or before the
// last accepted one is ignored, so a clock stepping backwards cannot corrupt the
// ring. total == 0 (an empty scope) records nothing, which also keeps coverage
// at zero so an empty namespace can never trip the alert.
func (t *Tracker) Observe(good, total int, now time.Time) {
	if t.last.IsZero() {
		t.last = now
		return
	}
	if !now.After(t.last) {
		return
	}
	dt := now.Sub(t.last)
	if dt > t.opts.MaxSampleGap {
		// Attribute only the cap, ending at now. The clamped-away time is never
		// counted: it lowers coverage rather than inventing history for it.
		dt = t.opts.MaxSampleGap
	}
	t.last = now
	if total <= 0 {
		return
	}
	t.add(now.Add(-dt), now, float64(good), float64(total))
}

// add spreads one interval's weight across every bucket it overlaps, in
// proportion to the overlap. Attributing the whole interval to the bucket
// containing its end would smear weight past a window edge and make the fast
// window wrong whenever a sample gap approaches the bucket width.
func (t *Tracker) add(from, to time.Time, good, total float64) {
	for cur := from; cur.Before(to); {
		b := cur.Truncate(bucketWidth)
		end := b.Add(bucketWidth)
		if end.After(to) {
			end = to
		}
		secs := end.Sub(cur).Seconds()
		slot := t.slot(b)
		if !slot.start.Equal(b) {
			*slot = bucket{start: b}
		}
		slot.good += secs * good
		slot.total += secs * total
		cur = end
	}
}

// slot maps a bucket boundary to its ring position.
func (t *Tracker) slot(b time.Time) *bucket {
	i := b.Unix() / int64(bucketWidth/time.Second) % int64(ringSize)
	if i < 0 {
		i += int64(ringSize)
	}
	return &t.ring[i]
}

// sum totals the workload-seconds recorded in [from, to). Buckets whose start
// does not match the boundary being asked for belong to an earlier lap and read
// as empty.
//
// Edge buckets are counted whole: a window edge falling mid-bucket pulls in that
// bucket's full weight. With one-minute buckets that is at most ~1.7% of the
// one-hour window and ~0.3% of the six-hour one — far below the resolution of
// the 14.4x/6x thresholds this feeds, and not worth the arithmetic to split.
// coverage() does prorate its edges, because a partial bucket at the edge of a
// mostly-empty window is exactly the case the coverage gate has to get right.
func (t *Tracker) sum(from, to time.Time) (good, total float64) {
	for b := from.Truncate(bucketWidth); b.Before(to); b = b.Add(bucketWidth) {
		slot := t.slot(b)
		if !slot.start.Equal(b) {
			continue
		}
		good += slot.good
		total += slot.total
	}
	return good, total
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/slo/ -v
```

Expected: PASS, 9 tests.

- [ ] **Step 5: Verify the ring-wrap test is not vacuous**

The wrap test is the one most likely to pass for the wrong reason. Prove it catches the bug it targets: temporarily delete the `if !slot.start.Equal(b) { continue }` guard in `sum`, re-run, confirm `TestObserve_RingWrapsWithoutReadingStaleSlots` FAILS, then restore the guard.

```bash
go test ./internal/slo/ -run RingWraps -v   # must FAIL with the guard removed
```

Expected with the guard removed: FAIL, `the stale lap must not be readable = 60, want 0`. Restore the guard and confirm PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/slo/
git commit -m "feat(slo): time-weighted availability ring

A fixed ring of 360 one-minute buckets accumulates workload-seconds, split
across bucket boundaries so window edges stay exact. Sample weight is capped so
a stalled daemon cannot resume and assert its last known state held for the
whole stall, and a backwards clock step is ignored rather than corrupting the
ring. Slots carry the minute they hold, so a previous lap reads as empty
without a sweep."
```

---

### Task 2: `internal/slo` — windows, burn rate, coverage, verdict

**Files:**

- Modify: `internal/slo/slo.go`
- Test: `internal/slo/slo_test.go`

**Interfaces:**

- Consumes: `Tracker`, `Options`, `sum`, the constants — all from Task 1.
- Produces: `Window` (`Fast`/`Slow`), `Report{Window, Availability, BurnRate, Coverage}`, `(*Tracker).Report(Window, time.Time) Report`, `Verdict{Firing bool; FiringSince time.Time; Fast, Slow Report}`, `(*Tracker).Verdict(time.Time) Verdict`. Tasks 3–5 consume these.

- [ ] **Step 1: Write the failing test**

Append to `internal/slo/slo_test.go`:

```go
// fill drives the tracker across span with one sample per minute, each reporting
// `bad` of `total` workloads broken. Returns the instant after the last sample.
func fill(tr *Tracker, start time.Time, span time.Duration, bad, total int) time.Time {
	now := start
	tr.Observe(total-bad, total, now) // baseline, no weight
	for elapsed := time.Duration(0); elapsed < span; elapsed += time.Minute {
		now = now.Add(time.Minute)
		tr.Observe(total-bad, total, now)
	}
	return now
}

func TestReport_FullyHealthyWindow(t *testing.T) {
	tr := New(Options{Target: 0.999})
	now := fill(tr, base, 2*time.Hour, 0, 100)
	r := tr.Report(Fast, now)
	approx(t, r.Availability, 1, "availability")
	approx(t, r.BurnRate, 0, "burn rate")
	approx(t, r.Coverage, 1, "coverage")
}

func TestReport_BurnRateScalesWithBreakage(t *testing.T) {
	// 1 of 100 workloads broken = 99% availability. Against a 99.9% target the
	// error budget is 0.1%, so burn = 0.01/0.001 = 10x.
	tr := New(Options{Target: 0.999})
	now := fill(tr, base, 2*time.Hour, 1, 100)
	r := tr.Report(Fast, now)
	approx(t, r.Availability, 0.99, "availability")
	approx(t, r.BurnRate, 10, "burn rate")
}

func TestReport_EmptyWindowIsHealthyNotBurning(t *testing.T) {
	tr := New(Options{Target: 0.999})
	r := tr.Report(Fast, base)
	approx(t, r.Availability, 1, "availability with no data")
	approx(t, r.BurnRate, 0, "burn rate with no data")
	approx(t, r.Coverage, 0, "coverage with no data")
}

func TestReport_CoverageReflectsPartialWindow(t *testing.T) {
	// 18 minutes of samples inside a 60-minute window = 30% coverage.
	tr := New(Options{Target: 0.999})
	now := fill(tr, base, 18*time.Minute, 0, 10)
	r := tr.Report(Fast, now)
	approx(t, r.Coverage, 0.3, "coverage")
}

func TestReport_WindowExcludesDataOlderThanTheWindow(t *testing.T) {
	// Two hours of total breakage, then two hours of perfect health. The fast
	// (1h) window must see only the healthy stretch; the slow (6h) window sees both.
	tr := New(Options{Target: 0.999})
	now := fill(tr, base, 2*time.Hour, 10, 10)
	tr.last = time.Time{} // re-baseline so the gap between phases carries no weight
	now = fill(tr, now, 2*time.Hour, 0, 10)

	fastR := tr.Report(Fast, now)
	approx(t, fastR.Availability, 1, "fast availability")
	slowR := tr.Report(Slow, now)
	approx(t, slowR.Availability, 0.5, "slow availability")
}

func TestVerdict_FiresWhenEveryConditionHolds(t *testing.T) {
	// 5 of 100 broken = 95% availability = 50x burn, well past both thresholds.
	// Six hours of samples gives both windows full coverage.
	tr := New(Options{Target: 0.999})
	now := fill(tr, base, 6*time.Hour, 5, 100)
	v := tr.Verdict(now)
	if !v.Firing {
		t.Fatalf("Verdict.Firing = false, want true (fast=%+v slow=%+v)", v.Fast, v.Slow)
	}
	if !v.FiringSince.Equal(now) {
		t.Errorf("FiringSince = %v, want %v", v.FiringSince, now)
	}
}

func TestVerdict_FiringSinceHoldsAcrossConsecutiveFirings(t *testing.T) {
	tr := New(Options{Target: 0.999})
	now := fill(tr, base, 6*time.Hour, 5, 100)
	first := tr.Verdict(now)
	later := tr.Verdict(now.Add(time.Minute))
	if !later.Firing {
		t.Fatal("second Verdict stopped firing")
	}
	if !later.FiringSince.Equal(first.FiringSince) {
		t.Errorf("FiringSince moved: %v -> %v", first.FiringSince, later.FiringSince)
	}
}

func TestVerdict_ClearsAndResetsFiringSince(t *testing.T) {
	tr := New(Options{Target: 0.999})
	now := fill(tr, base, 6*time.Hour, 5, 100)
	if !tr.Verdict(now).Firing {
		t.Fatal("expected the breach to fire")
	}
	tr.last = time.Time{}
	now = fill(tr, now, 6*time.Hour, 0, 100) // fully healthy for a whole slow window
	v := tr.Verdict(now)
	if v.Firing {
		t.Fatal("Verdict.Firing = true after full recovery, want false")
	}
	if !v.FiringSince.IsZero() {
		t.Errorf("FiringSince = %v after clearing, want zero", v.FiringSince)
	}
}

// TestVerdict_EachConditionIsLoadBearing is the mutation guard. The firing rule
// is a four-way conjunction; a test suite that only checks the all-true case
// would pass against an implementation that dropped any one conjunct. Each case
// below withholds exactly one condition and must not fire.
func TestVerdict_EachConditionIsLoadBearing(t *testing.T) {
	cases := []struct {
		name    string
		build   func() (*Tracker, time.Time)
		withheld string
	}{
		{
			name:     "fast burn below threshold",
			withheld: "fast.BurnRate >= 14.4",
			build: func() (*Tracker, time.Time) {
				// 6h of breakage at 1% bad: slow burn 10x (over 6), fast burn 10x
				// (under 14.4). Only the fast threshold is unmet.
				tr := New(Options{Target: 0.999})
				return tr, fill(tr, base, 6*time.Hour, 1, 100)
			},
		},
		{
			name:     "slow burn below threshold",
			withheld: "slow.BurnRate >= 6",
			build: func() (*Tracker, time.Time) {
				// 6h healthy, then 1h at 2% bad. Fast burn is 0.02/0.001 = 20x,
				// past 14.4. The slow window spreads that one bad hour over six,
				// giving 0.02/6/0.001 = 3.33x, under 6. Only the slow threshold
				// is unmet. (5% bad would give a slow burn of 8.33x and still
				// fire — the margin here is deliberately narrow.)
				tr := New(Options{Target: 0.999})
				now := fill(tr, base, 6*time.Hour, 0, 100)
				tr.last = time.Time{}
				return tr, fill(tr, now, time.Hour, 2, 100)
			},
		},
		{
			name:     "fast coverage below the gate",
			withheld: "fast.Coverage >= 0.6",
			build: func() (*Tracker, time.Time) {
				// 6h of breakage, then a 50-minute silent gap: the fast window is
				// only 10/60 covered while the slow window stays warm.
				tr := New(Options{Target: 0.999})
				now := fill(tr, base, 6*time.Hour, 5, 100)
				return tr, now.Add(50 * time.Minute)
			},
		},
		{
			name:     "slow coverage below the gate",
			withheld: "slow.Coverage >= 0.6",
			build: func() (*Tracker, time.Time) {
				// A freshly started daemon: 1h of breakage is enough for the fast
				// window but only 1/6 of the slow one.
				tr := New(Options{Target: 0.999})
				return tr, fill(tr, base, time.Hour, 5, 100)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, now := c.build()
			v := tr.Verdict(now)
			if v.Firing {
				t.Errorf("Firing = true with %s unmet; the condition is not load-bearing (fast=%+v slow=%+v)",
					c.withheld, v.Fast, v.Slow)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/slo/ -run 'Report|Verdict' -v
```

Expected: FAIL to build — `undefined: Fast`, `undefined: Slow`, `tr.Report undefined`, `tr.Verdict undefined`.

- [ ] **Step 3: Write the implementation**

Append to `internal/slo/slo.go`:

```go
// Window names one of the two fixed burn windows.
type Window string

const (
	Fast Window = "fast" // 1h — catches a sharp outage quickly
	Slow Window = "slow" // 6h — refuses to page for a blip that already passed
)

// duration returns the window's length.
func (w Window) duration() time.Duration {
	if w == Slow {
		return slowWindow
	}
	return fastWindow
}

// threshold returns the burn multiple at which the window is considered hot.
func (w Window) threshold() float64 {
	if w == Slow {
		return slowBurnThreshold
	}
	return fastBurnThreshold
}

// Report is one window's computed state.
type Report struct {
	Window       Window
	Availability float64 // good/total over the window; 1 when the window has no data
	BurnRate     float64 // (1-Availability)/(1-Target); 0 when the window has no data
	Coverage     float64 // fraction of the window carrying samples, 0..1
}

// Report computes the window's state as of now. A window with no data reports
// perfect availability and zero burn: absence of evidence is not evidence of an
// outage, and Coverage is the field that says so.
func (t *Tracker) Report(w Window, now time.Time) Report {
	span := w.duration()
	from := now.Add(-span)
	good, total := t.sum(from, now)
	r := Report{Window: w, Availability: 1, Coverage: t.coverage(from, now)}
	if total <= 0 {
		return r
	}
	r.Availability = good / total
	r.BurnRate = (1 - r.Availability) / (1 - t.opts.Target)
	return r
}

// coverage is the fraction of [from, now) that carries samples. A bucket counts
// when it recorded any weight; the partial buckets at either edge count in
// proportion to how much of them the window covers.
func (t *Tracker) coverage(from, now time.Time) float64 {
	span := now.Sub(from).Seconds()
	if span <= 0 {
		return 0
	}
	var covered float64
	for b := from.Truncate(bucketWidth); b.Before(now); b = b.Add(bucketWidth) {
		slot := t.slot(b)
		if !slot.start.Equal(b) || slot.total <= 0 {
			continue
		}
		lo, hi := b, b.Add(bucketWidth)
		if lo.Before(from) {
			lo = from
		}
		if hi.After(now) {
			hi = now
		}
		covered += hi.Sub(lo).Seconds()
	}
	return covered / span
}

// Verdict is the alert decision as of now.
type Verdict struct {
	Firing      bool
	FiringSince time.Time // zero when not firing
	Fast, Slow  Report
}

// Verdict evaluates the multi-window firing rule and tracks the firing edge.
//
// All four conditions must hold. The two burn thresholds together are what makes
// this multi-window: the fast window alone would page for a blip that has already
// passed, and the slow window alone would take hours to notice a total outage.
//
// The coverage gate exists because state is in-memory and resets on restart. A
// daemon that started five minutes ago has five minutes of history inside what it
// calls a one-hour window, and any badness in it computes to a 100% burn — it
// would page on every rollout of the daemon itself. Requiring 60% of both windows
// means the slow one must be ~3.6h warm before it can page.
//
// Verdict mutates the firing edge, so call it once per reconcile.
func (t *Tracker) Verdict(now time.Time) Verdict {
	v := Verdict{Fast: t.Report(Fast, now), Slow: t.Report(Slow, now)}
	v.Firing = v.Fast.BurnRate >= Fast.threshold() &&
		v.Slow.BurnRate >= Slow.threshold() &&
		v.Fast.Coverage >= minCoverage &&
		v.Slow.Coverage >= minCoverage
	if !v.Firing {
		t.firingSince = time.Time{}
		return v
	}
	if t.firingSince.IsZero() {
		t.firingSince = now
	}
	v.FiringSince = t.firingSince
	return v
}
```

Add the field to the `Tracker` struct in the same file:

```go
type Tracker struct {
	opts        Options
	ring        []bucket
	last        time.Time // the last accepted sample's instant; zero before the first
	firingSince time.Time // start of the current breach; zero when not firing
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/slo/ -v
```

Expected: PASS, all tests including the four `EachConditionIsLoadBearing` subtests.

- [ ] **Step 5: Prove the mutation guard actually guards**

For each of the four conjuncts in `Verdict`, delete it, run the suite, and confirm the matching subtest fails. Then restore it. This is the check that `TestVerdict_EachConditionIsLoadBearing` is doing its job — a suite that passes with a conjunct removed would be worthless.

```bash
# with `v.Fast.BurnRate >= Fast.threshold() &&` removed:
go test ./internal/slo/ -run EachCondition -v
```

Expected for each mutation: FAIL on exactly the corresponding subtest, e.g. `Firing = true with fast.BurnRate >= 14.4 unmet`. All four must fail when their conjunct is removed. Restore the full conjunction and confirm PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/slo/
git commit -m "feat(slo): multi-window burn rate, coverage, and the firing verdict

Report computes availability, burn rate and coverage over the fast (1h) and slow
(6h) windows. Verdict fires only when both windows are hot AND both are at least
60% covered.

The coverage gate is load-bearing rather than defensive: state is in-memory and
resets on restart, so a daemon five minutes old has five minutes of history in
what it calls a one-hour window, and any badness in it computes to a 100% burn.
Without the gate the daemon would page every time it restarted.

The verdict tests include a mutation guard: each of the four conjuncts is
withheld in turn and must prevent firing, so the suite cannot pass against an
implementation that dropped one."
```

---

### Task 3: Metrics rendering

**Files:**

- Modify: `internal/watch/metrics.go`
- Create: `internal/watch/slo_test.go`

**Interfaces:**

- Consumes: `slo.Report`, `slo.Window`, `slo.Fast`, `slo.Slow` (Task 2).
- Produces: `sloSnapshot` and `(*metrics).updateSLO(enabled bool, target float64, fast, slow slo.Report)`. Task 5 calls `updateSLO` from the reconcile loop.

- [ ] **Step 1: Write the failing test**

Create `internal/watch/slo_test.go`:

```go
package watch

import (
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/slo"
)

// sloBase is a fixed instant for the SLO tests. Deliberately not named `base`:
// metrics_test.go already uses that name as a function-local, and a
// package-level `base` would be silently shadowed there.
var sloBase = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func TestRender_OmitsSLOSeriesWhenDisabled(t *testing.T) {
	m := newMetrics()
	out := m.render()
	if strings.Contains(out, "kubeagent_slo_") {
		t.Error("SLO series rendered while SLO tracking is off; --slo-target unset must mean no series")
	}
}

func TestRender_SLOSeries(t *testing.T) {
	m := newMetrics()
	m.updateSLO(true, 0.999,
		slo.Report{Window: slo.Fast, Availability: 0.99, BurnRate: 10, Coverage: 1},
		slo.Report{Window: slo.Slow, Availability: 0.995, BurnRate: 5, Coverage: 0.75},
	)
	out := m.render()
	for _, want := range []string{
		"kubeagent_slo_target_ratio 0.999",
		`kubeagent_slo_availability_ratio{window="fast"} 0.99`,
		`kubeagent_slo_availability_ratio{window="slow"} 0.995`,
		`kubeagent_slo_burn_rate{window="fast"} 10`,
		`kubeagent_slo_burn_rate{window="slow"} 5`,
		`kubeagent_slo_window_coverage_ratio{window="fast"} 1`,
		`kubeagent_slo_window_coverage_ratio{window="slow"} 0.75`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series %q in:\n%s", want, out)
		}
	}
}

func TestRender_ErrorBudgetRemaining(t *testing.T) {
	cases := []struct {
		name     string
		slowBurn float64
		want     string
	}{
		{"quarter spent", 0.25, "kubeagent_slo_error_budget_remaining_ratio 0.75"},
		{"exactly spent", 1, "kubeagent_slo_error_budget_remaining_ratio 0"},
		{"overspent clamps at zero", 12, "kubeagent_slo_error_budget_remaining_ratio 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMetrics()
			m.updateSLO(true, 0.999,
				slo.Report{Window: slo.Fast},
				slo.Report{Window: slo.Slow, BurnRate: c.slowBurn},
			)
			if out := m.render(); !strings.Contains(out, c.want) {
				t.Errorf("missing %q in:\n%s", c.want, out)
			}
		})
	}
}

func TestRender_SLODoesNotTouchIssueSeries(t *testing.T) {
	// The burn signal must never inflate the object-issue gauges. An operator
	// reading kubeagent_issues_active as "how many objects are broken" must not
	// see a budget breach counted there.
	m := newMetrics()
	m.updateSLO(true, 0.999,
		slo.Report{Window: slo.Fast, BurnRate: 50, Coverage: 1},
		slo.Report{Window: slo.Slow, BurnRate: 50, Coverage: 1},
	)
	out := m.render()
	if !strings.Contains(out, "kubeagent_issues_active 0") {
		t.Error("kubeagent_issues_active moved off zero because of an SLO update")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ -run 'TestRender_.*SLO|TestRender_ErrorBudget' -v
```

Expected: FAIL to build — `m.updateSLO undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/watch/metrics.go`, add `"github.com/imantaba/kubeagent/internal/slo"` to the import block.

Add this type next to `issueSnapshot`:

```go
// sloSnapshot is the SLO tracker's state as of the last reconcile. Enabled is
// false when --slo-target was not set, in which case no SLO series render at all.
type sloSnapshot struct {
	Enabled    bool
	Target     float64
	Fast, Slow slo.Report
}
```

Add the field to the `metrics` struct, immediately after the existing `alerts` field:

```go
	slo sloSnapshot
```

Add the updater next to `updateAlerts`:

```go
// updateSLO records the SLO tracker's latest report for rendering.
func (m *metrics) updateSLO(enabled bool, target float64, fast, slow slo.Report) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slo = sloSnapshot{Enabled: enabled, Target: target, Fast: fast, Slow: slow}
}
```

In `render()`, after the existing alert series and before the final `return`, add:

```go
	if m.slo.Enabled {
		labelled := func(name, help string, fast, slow float64) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
			fmt.Fprintf(&b, "%s{window=\"fast\"} %g\n", name, fast)
			fmt.Fprintf(&b, "%s{window=\"slow\"} %g\n", name, slow)
		}
		gauge("kubeagent_slo_target_ratio", "Configured availability SLO as a ratio", m.slo.Target)
		labelled("kubeagent_slo_availability_ratio",
			"Time-weighted fraction of workload-seconds with no findings, over the window",
			m.slo.Fast.Availability, m.slo.Slow.Availability)
		labelled("kubeagent_slo_burn_rate",
			"Error-budget consumption multiple over the window (1 = spending exactly at budget)",
			m.slo.Fast.BurnRate, m.slo.Slow.BurnRate)
		labelled("kubeagent_slo_window_coverage_ratio",
			"Fraction of the window carrying samples; below 0.6 the burn alert is suppressed",
			m.slo.Fast.Coverage, m.slo.Slow.Coverage)
		// Clamped at zero: a burn above 1x means the window's budget is already
		// spent, and a negative "remaining" is nonsense on a dashboard.
		remaining := 1 - m.slo.Slow.BurnRate
		if remaining < 0 {
			remaining = 0
		}
		gauge("kubeagent_slo_error_budget_remaining_ratio",
			"Fraction of the error budget left over the slow window, clamped to [0,1]", remaining)
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ -v
```

Expected: PASS, including the pre-existing watch tests.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/metrics.go internal/watch/slo_test.go
git commit -m "feat(watch): render the SLO burn-rate series

Five series, eight timeseries, fixed cardinality — and none at all unless
--slo-target is set, so upgrading without the flag changes nothing.

kubeagent_slo_error_budget_remaining_ratio clamps at zero: a burn above 1x means
the window's budget is already spent and a negative remaining would be nonsense
on a dashboard."
```

---

### Task 4: The verdict→notification bridge

**Files:**

- Create: `internal/watch/slo.go`
- Test: `internal/watch/slo_test.go` (append)

**Interfaces:**

- Consumes: `slo.Verdict` (Task 2); `alertstate.Notification`, `alertstate.Object`, `alertstate.StatusFiring`, `alertstate.StatusResolved`, `alertstate.ReasonNew`, `alertstate.ReasonRepeat`, `alertstate.ReasonResolved`.
- Produces: `sloNotifier` with `newSLONotifier(repeat time.Duration) *sloNotifier` and `(*sloNotifier).step(v slo.Verdict, now time.Time) (alertstate.Notification, bool)`. Task 5 wires it to the sink.

- [ ] **Step 1: Write the failing test**

Append to `internal/watch/slo_test.go`. The file already has an import block from Task 3 — merge `"github.com/imantaba/kubeagent/internal/alertstate"` into it rather than adding a second block:

```go
func firing(since time.Time) slo.Verdict {
	return slo.Verdict{Firing: true, FiringSince: since}
}

func TestSLONotifier_SilentWhileNotFiring(t *testing.T) {
	n := newSLONotifier(time.Hour)
	if _, ok := n.step(slo.Verdict{}, sloBase); ok {
		t.Error("emitted a notification while the verdict was not firing")
	}
}

func TestSLONotifier_EmitsOnTheFiringEdge(t *testing.T) {
	n := newSLONotifier(time.Hour)
	got, ok := n.step(firing(sloBase), sloBase)
	if !ok {
		t.Fatal("no notification on the firing edge")
	}
	want := alertstate.Notification{
		Object:      alertstate.Object{Kind: "SLO", Name: "error-budget"},
		Status:      alertstate.StatusFiring,
		Issues:      []string{"ErrorBudgetBurn"},
		FiringSince: sloBase,
		Reason:      alertstate.ReasonNew,
	}
	if got.Object != want.Object || got.Status != want.Status || got.Reason != want.Reason {
		t.Errorf("identity = %+v, want %+v", got, want)
	}
	if len(got.Issues) != 1 || got.Issues[0] != "ErrorBudgetBurn" {
		t.Errorf("Issues = %v, want [ErrorBudgetBurn]", got.Issues)
	}
	if !got.FiringSince.Equal(sloBase) {
		t.Errorf("FiringSince = %v, want %v", got.FiringSince, sloBase)
	}
	if got.Object.Namespace != "" {
		t.Errorf("Namespace = %q, want empty (the budget is cluster-scoped)", got.Object.Namespace)
	}
}

func TestSLONotifier_SilentWhileStillFiringInsideRepeat(t *testing.T) {
	n := newSLONotifier(time.Hour)
	n.step(firing(sloBase), sloBase)
	if _, ok := n.step(firing(sloBase), sloBase.Add(59*time.Minute)); ok {
		t.Error("re-sent inside the repeat interval")
	}
}

func TestSLONotifier_RepeatsAfterTheInterval(t *testing.T) {
	n := newSLONotifier(time.Hour)
	n.step(firing(sloBase), sloBase)
	got, ok := n.step(firing(sloBase), sloBase.Add(time.Hour))
	if !ok {
		t.Fatal("no re-send after the repeat interval elapsed")
	}
	if got.Reason != alertstate.ReasonRepeat {
		t.Errorf("Reason = %q, want %q", got.Reason, alertstate.ReasonRepeat)
	}
	if !got.FiringSince.Equal(sloBase) {
		t.Errorf("FiringSince = %v, want the original %v", got.FiringSince, sloBase)
	}
}

func TestSLONotifier_EmitsResolvedOnce(t *testing.T) {
	n := newSLONotifier(time.Hour)
	n.step(firing(sloBase), sloBase)
	clear := sloBase.Add(2 * time.Hour)
	got, ok := n.step(slo.Verdict{}, clear)
	if !ok {
		t.Fatal("no resolved notification when the breach cleared")
	}
	if got.Status != alertstate.StatusResolved || got.Reason != alertstate.ReasonResolved {
		t.Errorf("status/reason = %q/%q, want resolved/resolved", got.Status, got.Reason)
	}
	if len(got.Issues) != 0 {
		t.Errorf("Issues = %v, want empty on resolve", got.Issues)
	}
	if !got.ResolvedAt.Equal(clear) {
		t.Errorf("ResolvedAt = %v, want %v", got.ResolvedAt, clear)
	}
	if _, ok := n.step(slo.Verdict{}, clear.Add(time.Minute)); ok {
		t.Error("emitted a second resolved notification; resolve must fire once")
	}
}

func TestSLONotifier_ReFiresAfterResolving(t *testing.T) {
	n := newSLONotifier(time.Hour)
	n.step(firing(sloBase), sloBase)
	n.step(slo.Verdict{}, sloBase.Add(time.Hour))
	second := sloBase.Add(2 * time.Hour)
	got, ok := n.step(firing(second), second)
	if !ok {
		t.Fatal("no notification on the second firing edge")
	}
	if got.Reason != alertstate.ReasonNew {
		t.Errorf("Reason = %q, want %q on a fresh breach", got.Reason, alertstate.ReasonNew)
	}
	if !got.FiringSince.Equal(second) {
		t.Errorf("FiringSince = %v, want the new breach start %v", got.FiringSince, second)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ -run TestSLONotifier -v
```

Expected: FAIL to build — `undefined: newSLONotifier`.

- [ ] **Step 3: Write the implementation**

Create `internal/watch/slo.go`:

```go
package watch

import (
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/slo"
)

// The burn alert's fixed identity. It is deliberately NOT an object in the
// cluster: a budget breach is a property of the estate over time, not of a
// Deployment. It never enters watchstate or alertstate tracking, so it does not
// appear in /issues or inflate any kubeagent_issues_* series — an operator
// reading those as object counts would be misled if it did.
const (
	sloAlertKind  = "SLO"
	sloAlertName  = "error-budget"
	sloAlertIssue = "ErrorBudgetBurn" // not "FastBurn": firing needs BOTH windows
)

// sloNotifier turns a stream of verdicts into the edge-triggered notifications
// the alert sink expects: one on the firing edge, a periodic re-send while the
// breach persists, and exactly one on the clearing edge.
//
// This is deliberately a separate, much smaller state machine than
// alertstate.Roller. The roller's job is rolling many per-issue records up to
// per-object alerts; there is exactly one error budget, so all that remains is
// the firing edge and the repeat clock.
type sloNotifier struct {
	repeat   time.Duration
	firing   bool
	since    time.Time
	lastSent time.Time
}

// newSLONotifier returns a notifier re-sending a still-firing breach every
// repeat. It shares --alert-repeat with object alerts so an Alertmanager
// receiver refreshes the budget alert before resolve_timeout expires it, exactly
// as it does for object alerts.
func newSLONotifier(repeat time.Duration) *sloNotifier {
	return &sloNotifier{repeat: repeat}
}

// step folds one verdict in and reports the notification to send, if any.
func (n *sloNotifier) step(v slo.Verdict, now time.Time) (alertstate.Notification, bool) {
	switch {
	case v.Firing && !n.firing:
		n.firing, n.since, n.lastSent = true, v.FiringSince, now
		return n.notification(alertstate.StatusFiring, alertstate.ReasonNew, time.Time{}), true

	case v.Firing && now.Sub(n.lastSent) >= n.repeat:
		n.lastSent = now
		return n.notification(alertstate.StatusFiring, alertstate.ReasonRepeat, time.Time{}), true

	case !v.Firing && n.firing:
		n.firing = false
		out := n.notification(alertstate.StatusResolved, alertstate.ReasonResolved, now)
		n.since = time.Time{}
		return out, true
	}
	return alertstate.Notification{}, false
}

// notification builds the payload. Issues is empty on resolve, matching the
// convention alertstate.Notification documents and the encoders rely on.
func (n *sloNotifier) notification(s alertstate.Status, r alertstate.Reason, resolvedAt time.Time) alertstate.Notification {
	out := alertstate.Notification{
		Object:      alertstate.Object{Kind: sloAlertKind, Name: sloAlertName},
		Status:      s,
		FiringSince: n.since,
		ResolvedAt:  resolvedAt,
		Reason:      r,
	}
	if s == alertstate.StatusFiring {
		out.Issues = []string{sloAlertIssue}
	}
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/slo.go internal/watch/slo_test.go
git commit -m "feat(watch): bridge the SLO verdict to an alert notification

An edge-triggered notifier: one alert on the firing edge, a re-send every
--alert-repeat while the breach persists, exactly one on the clearing edge.

It is a separate state machine from alertstate.Roller rather than a reuse of it.
The roller's job is rolling many per-issue records up to per-object alerts, and
there is exactly one error budget — so all that remains is the firing edge and
the repeat clock. Routing the breach through the roller would also land it in
/issues and in kubeagent_issues_active, where a reader counting broken objects
would be misled by it."
```

---

### Task 5: Wire the tracker into the reconcile loop

**Files:**

- Modify: `internal/watch/watch.go`
- Modify: `internal/watch/watch_test.go` (the seven existing `applyResult` call sites, plus the new startup-validation test)
- Test: `internal/watch/slo_test.go` (append)

**Interfaces:**

- Consumes: `slo.New`, `slo.Options`, `(*slo.Tracker).Observe/Report/Verdict/Target` (Tasks 1–2); `(*metrics).updateSLO` (Task 3); `newSLONotifier`, `(*sloNotifier).step` (Task 4); the existing `alerter` with its `sink` field.
- Produces: `Config.SLOTarget float64` (a ratio in `(0,1)`; `0` disables). Task 6 sets it from the flag.

- [ ] **Step 1: Write the failing test**

Append to `internal/watch/slo_test.go`. Merge these into the file's existing import block: `"errors"`, `"github.com/imantaba/kubeagent/internal/diagnose"`, `"github.com/imantaba/kubeagent/internal/inventory"`, `"github.com/imantaba/kubeagent/internal/scan"`, `"github.com/imantaba/kubeagent/internal/watchstate"`. `captureLog` and `sampleResult` already exist in the package's other test files — reuse them, do not redefine.

```go
func TestWorkloadCensus(t *testing.T) {
	res := &scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{
		{Name: "a"},
		{Name: "b", Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff"}}},
		{Name: "c"},
		{Name: "d", Findings: []diagnose.Finding{{Issue: "OOMKilled"}, {Issue: "Degraded"}}},
	}}}
	good, total := workloadCensus(res)
	if good != 2 || total != 4 {
		t.Errorf("census = (%d good, %d total), want (2, 4)", good, total)
	}
}

func TestWorkloadCensus_EmptyScope(t *testing.T) {
	good, total := workloadCensus(&scan.Result{})
	if good != 0 || total != 0 {
		t.Errorf("census = (%d, %d), want (0, 0) for an empty scope", good, total)
	}
}

// TestApplyResult_ErrorDoesNotSample pins the invariant that makes the SLI
// trustworthy: a failed evaluation is neither "all healthy" nor "all broken", so
// it must not become a sample. Without this the first API blip would count as an
// outage of the entire estate.
func TestApplyResult_ErrorDoesNotSample(t *testing.T) {
	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	sloTr, sloN := newSLOTracker(Config{SLOTarget: 0.999, Heartbeat: time.Minute, AlertRepeat: time.Hour})

	// A healthy sample to establish the baseline, then a minute of healthy time.
	captureLog(t, func() {
		applyResult(m, tr, nil, sloTr, sloN, sampleResult(), time.Millisecond, sloBase, nil)
	})
	captureLog(t, func() {
		applyResult(m, tr, nil, sloTr, sloN, sampleResult(), time.Millisecond, sloBase.Add(time.Minute), nil)
	})
	before := sloTr.Report(slo.Fast, sloBase.Add(time.Minute))

	// An hour of nothing but errors.
	for i := 2; i <= 62; i++ {
		captureLog(t, func() {
			applyResult(m, tr, nil, sloTr, sloN, &scan.Result{}, time.Millisecond,
				sloBase.Add(time.Duration(i)*time.Minute), errors.New("boom"))
		})
	}
	after := sloTr.Report(slo.Fast, sloBase.Add(62*time.Minute))

	if after.Availability != 1 {
		t.Errorf("availability = %v after an hour of errors, want 1: errors must not be sampled", after.Availability)
	}
	if after.Coverage >= before.Coverage {
		t.Errorf("coverage %v did not drop below %v; an error gap must reduce coverage, not be invisible",
			after.Coverage, before.Coverage)
	}
}

// TestApplyResult_SLODisabledIsInert proves the nil path: with SLOTarget unset
// the reconcile loop must not panic and must render no SLO series.
func TestApplyResult_SLODisabledIsInert(t *testing.T) {
	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	sloTr, sloN := newSLOTracker(Config{Heartbeat: time.Minute})
	if sloTr != nil || sloN != nil {
		t.Fatal("newSLOTracker returned a tracker with --slo-target unset")
	}
	captureLog(t, func() {
		applyResult(m, tr, nil, sloTr, sloN, sampleResult(), time.Millisecond, sloBase, nil)
	})
	if strings.Contains(m.render(), "kubeagent_slo_") {
		t.Error("SLO series rendered with SLO tracking off")
	}
}

func TestValidateSLOTarget(t *testing.T) {
	cases := []struct {
		target  float64
		wantErr bool
	}{
		{0, false},      // disabled
		{0.999, false},  // typical
		{0.5, false},    // permissive but legal
		{1, true},       // zero error budget: burn rate divides by zero
		{1.5, true},     // nonsense
		{-0.1, true},    // nonsense
	}
	for _, c := range cases {
		err := validateSLOTarget(c.target)
		if (err != nil) != c.wantErr {
			t.Errorf("validateSLOTarget(%v) error = %v, wantErr = %v", c.target, err, c.wantErr)
		}
	}
}
```

Append to `internal/watch/watch_test.go` (the fake-clientset startup test, matching the existing alert-validation test in that file):

```go
func TestRun_RejectsBadSLOTargetBeforeCacheSync(t *testing.T) {
	client := fake.NewSimpleClientset()
	// Block List forever. A config error must surface anyway: if validation ran
	// after WaitForCacheSync, an unresponsive API server would hide it behind
	// what looks like a cluster hang.
	client.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		select {}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Run(ctx, client, Config{
		MetricsAddr: "127.0.0.1:0",
		Heartbeat:   time.Minute,
		Debounce:    time.Second,
		SLOTarget:   1.0,
	})
	if err == nil {
		t.Fatal("Run returned nil for --slo-target 100; want a startup error")
	}
	if !strings.Contains(err.Error(), "slo-target") {
		t.Errorf("error %q does not name the offending flag", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ -run 'WorkloadCensus|ValidateSLOTarget|BadSLOTarget' -v
```

Expected: FAIL to build — `undefined: workloadCensus`, `undefined: validateSLOTarget`, `unknown field SLOTarget`.

- [ ] **Step 3: Write the implementation**

Add to `Config` in `internal/watch/watch.go`, after `AlertRepeat`:

```go
	SLOTarget float64 // availability SLO as a ratio in (0,1); 0 disables SLO tracking
```

Add to `internal/watch/slo.go`:

```go
// workloadCensus counts the workloads the evaluation covered and how many of
// them are clean. "Clean" is len(Findings) == 0 — the same predicate issueKeys
// uses to decide whether to track a workload, so the SLI and the issue tracker
// can never disagree about what "broken" means.
func workloadCensus(res *scan.Result) (good, total int) {
	for _, w := range res.Inventory.Workloads {
		total++
		if len(w.Findings) == 0 {
			good++
		}
	}
	return good, total
}

// validateSLOTarget rejects a target that cannot produce a burn rate. 1.0 is
// rejected explicitly: a 100% target makes the error budget zero, and the burn
// rate would divide by it.
func validateSLOTarget(target float64) error {
	if target == 0 {
		return nil // disabled
	}
	if target <= 0 || target >= 1 {
		return fmt.Errorf("invalid --slo-target: %g%% (must be greater than 0 and less than 100)", target*100)
	}
	return nil
}

// newSLOTracker returns the tracker and its notifier, or nils when SLO tracking
// is off. Like *alerter, the nil case is the switched-off state.
func newSLOTracker(cfg Config) (*slo.Tracker, *sloNotifier) {
	if cfg.SLOTarget == 0 {
		return nil, nil
	}
	gap := 2 * cfg.Heartbeat
	tr := slo.New(slo.Options{Target: cfg.SLOTarget, MaxSampleGap: gap})
	return tr, newSLONotifier(cfg.AlertRepeat)
}
```

Add `"fmt"`, `"log"`, and the `scan`/`slo` imports to `internal/watch/slo.go` as needed.

**Update the seven existing `applyResult` call sites in `internal/watch/watch_test.go`** (lines 51, 58, 76, 84, 90, 292, 329). Each gains two `nil` arguments in the new positions — `applyResult(m, tr, nil, nil, nil, ...)` for the calls that pass no alerter, and `applyResult(m, tr, al, nil, nil, ...)` for the two that do. Passing `nil` for both keeps every pre-existing test asserting exactly what it asserted before: no SLO tracking, no behavior change. Do not adapt any existing assertion to the new signature beyond adding those arguments — if a pre-existing test starts failing, that is a real regression, not a signature problem.

In `Run`, extend the existing fail-fast validation block. It currently starts with the `alertCtx` comment; add the SLO check as the **first** statement of `Run`, above it, and extend that comment's first sentence to cover both:

```go
func Run(ctx context.Context, client kubernetes.Interface, cfg Config) error {
	// Validate every piece of configuration before anything else starts. A bad
	// --slo-target, --alert-format or --alert-repeat must fail fast: once the
	// metrics server is listening and WaitForCacheSync is underway, a
	// reachable-but-unresponsive API server can block that sync forever, hiding
	// the config error behind what looks like a cluster hang.
	if err := validateSLOTarget(cfg.SLOTarget); err != nil {
		return err
	}

	// The sink runs off its own cancellable context, alertCtx, rather than ctx
	// directly: al.sink.Close() blocks on <-s.done, which only closes once the
	// ... (rest of the existing comment and code unchanged)
```

After the existing `tr := watchstate.New(watchstate.Options{})` line, add:

```go
	sloTr, sloN := newSLOTracker(cfg)
	if sloTr != nil {
		log.Printf("kubeagent: SLO burn-rate tracking enabled (target=%g%%, windows 1h/6h, alert suppressed below %d%% window coverage)",
			cfg.SLOTarget*100, 60)
	}
```

Change the `reconcile` closure and `applyResult` signature to carry them:

```go
	reconcile := func() {
		start := time.Now()
		res, err := scan.Evaluate(ctx, client, opts)
		applyResult(m, tr, al, sloTr, sloN, &res, time.Since(start), time.Now(), err)
	}
```

Replace `applyResult` with:

```go
// applyResult folds one evaluation into the metrics, the issue tracker, the SLO
// tracker, and the outbound alerter, and logs whatever changed. A failed
// evaluation never reaches any tracker: an evaluation error is not "all clear",
// and treating it as one would resolve every tracked issue, then re-fire them
// all on the next success — and page the on-call for an API blip. The SLO
// tracker sits on the same side of that return for the same reason: an API error
// is neither "all healthy" nor "all broken", so it must not become a sample. The
// gap shows up as reduced window coverage, which is the honest representation.
func applyResult(m *metrics, tr *watchstate.Tracker, al *alerter, sloTr *slo.Tracker, sloN *sloNotifier, res *scan.Result, dur time.Duration, now time.Time, err error) {
	m.update(res, dur, now, err)
	if err != nil {
		log.Printf("kubeagent: evaluation error: %v", err)
		return
	}
	d := tr.Observe(issueKeys(res), now)
	al.notify(tr, now)
	if sloTr != nil {
		good, total := workloadCensus(res)
		sloTr.Observe(good, total, now)
		v := sloTr.Verdict(now)
		m.updateSLO(true, sloTr.Target(), v.Fast, v.Slow)
		if n, ok := sloN.step(v, now); ok {
			logSLO(n, v)
			al.enqueue(n)
		}
	}
	m.updateIssues(tr, now)
	m.updateAlerts(al.stats())
	logDelta(res, d, len(tr.Active()), tr.FlapWindow())
}
```

Add to `internal/watch/slo.go`:

```go
// logSLO prints the burn transition. It mirrors logDelta's NEW/RESOLVED shape so
// the two alert sources read the same way in the daemon's log.
func logSLO(n alertstate.Notification, v slo.Verdict) {
	if n.Status == alertstate.StatusResolved {
		log.Printf("kubeagent: RESOLVED SLO/error-budget (burn back under threshold; fast=%.1fx slow=%.1fx)",
			v.Fast.BurnRate, v.Slow.BurnRate)
		return
	}
	log.Printf("kubeagent: %s SLO/error-budget:ErrorBudgetBurn (fast=%.1fx slow=%.1fx, coverage fast=%.0f%% slow=%.0f%%)",
		map[alertstate.Reason]string{alertstate.ReasonNew: "NEW", alertstate.ReasonRepeat: "REPEAT"}[n.Reason],
		v.Fast.BurnRate, v.Slow.BurnRate, v.Fast.Coverage*100, v.Slow.Coverage*100)
}
```

Add the `enqueue` passthrough to the `alerter` in `internal/watch/watch.go`, next to `notify`:

```go
// enqueue hands one already-built notification to the sink. The SLO burn alert
// uses this rather than notify: it is not derived from the issue tracker, but it
// shares the sink so retry, backoff, the bounded queue and URL redaction all
// apply to it unchanged.
func (a *alerter) enqueue(n alertstate.Notification) {
	if a == nil {
		return
	}
	a.sink.Enqueue(n)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -race -count=1
```

Expected: PASS across all packages.

- [ ] **Step 5: Verify SLO stays out of the issue path**

```bash
go test ./internal/watch/ -run 'TestRender_SLODoesNotTouchIssueSeries' -v
```

Expected: PASS. Also confirm by inspection that `sloN.step`'s notification reaches only `al.enqueue` and never `tr.Observe` or `al.notify`.

- [ ] **Step 6: Commit**

```bash
git add internal/watch/
git commit -m "feat(watch): sample the SLO tracker from the reconcile loop

The census sits inside applyResult's post-error branch, so an evaluation error
never becomes a sample — the same invariant the issue tracker already relies on,
and for the same reason: an API blip is neither 'all healthy' nor 'all broken'.
The gap surfaces as reduced window coverage instead.

--slo-target is validated as the first statement of Run, ahead of the metrics
server and the cache sync, so a bad value cannot hide behind an unresponsive API
server.

The burn notification reaches the sink through a new alerter.enqueue passthrough
rather than through notify, so it inherits retry, backoff, the bounded queue and
URL redaction without entering the issue tracker."
```

---

### Task 6: The `--slo-target` flag

**Files:**

- Modify: `main.go:63` (usage string), `main.go:303-363` (`runWatch`)

**Interfaces:**

- Consumes: `watch.Config.SLOTarget` (Task 5).
- Produces: the `--slo-target` flag and `KUBEAGENT_SLO_TARGET`.

- [ ] **Step 1: Add the flag**

In `runWatch`, after the `alertRepeat` line:

```go
	sloTarget := fs.Float64("slo-target", envFloat("KUBEAGENT_SLO_TARGET", 0), "availability SLO as a percentage, e.g. 99.9 (0 = SLO tracking off)")
```

- [ ] **Step 2: Convert percentage to ratio and pass it through**

After the `alertURL` block, add:

```go
	// The flag is a percentage because that is how an SRE writes an SLO; the
	// tracker works in ratios. 0 stays 0, which means off.
	sloRatio := *sloTarget / 100
```

Add to the `watch.Config` literal, after `AlertRepeat`:

```go
		SLOTarget:               sloRatio,
```

- [ ] **Step 3: Update the usage string**

In `main.go:63`, inside the `kubeagent watch [...]` section, insert `[--slo-target pct]` immediately after `[--alert-repeat dur]`:

```
| kubeagent watch [--kubeconfig path] [--context name] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur] [--alert-format json|slack|alertmanager] [--alert-repeat dur] [--slo-target pct] | kubeagent version
```

- [ ] **Step 4: Verify by hand**

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o /tmp/kubeagent-slo . && /tmp/kubeagent-slo watch --slo-target 100 --kubeconfig /nonexistent 2>&1 | head -2
```

Expected: the run fails naming `--slo-target`, e.g. `invalid --slo-target: 100% (must be greater than 0 and less than 100)`. Because validation is the first statement of `Run`, this must appear **without** any cluster connection being needed — but note `cluster.NewInClusterOrKubeconfig` runs first in `runWatch`, so with a nonexistent kubeconfig you may see the connection error instead. In that case verify with a real context:

```bash
/tmp/kubeagent-slo watch --slo-target 100 2>&1 | head -2   # in-cluster or with a valid default kubeconfig
```

Then confirm the flag appears in help:

```bash
/tmp/kubeagent-slo watch --help 2>&1 | grep slo-target
```

Expected: `-slo-target float` with the described default.

- [ ] **Step 5: Run the full suite**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .
```

Expected: all pass, `gofmt -l .` prints nothing.

- [ ] **Step 6: Commit**

```bash
git add main.go
git commit -m "feat(watch): add --slo-target

Takes a percentage because that is how an SRE writes an SLO; the tracker works
in ratios. Unset (0) means no SLO tracking at all — no series, no verdict, no
alerts — so upgrading without the flag changes nothing."
```

---

### Task 7: Helm chart and raw manifest

**Files:**

- Modify: `deploy/helm/kubeagent/values.yaml:84-96` (after the `alerts` block)
- Modify: `deploy/helm/kubeagent/templates/deployment.yaml:37-42` (args)
- Modify: `deploy/deployment.yaml`

**Interfaces:**

- Consumes: the `--slo-target` flag (Task 6).
- Produces: `.Values.slo.enabled`, `.Values.slo.target`.

- [ ] **Step 1: Add the values**

Append to `deploy/helm/kubeagent/values.yaml` after the `alerts` block:

```yaml
slo:
  # Track an availability SLO and expose error-budget burn rate as metrics.
  # The SLI is time-weighted workload availability: the fraction of
  # workload-seconds in which a workload had no findings. Needs no extra RBAC —
  # it reads the evaluation the daemon already performs.
  enabled: false
  # Availability target as a percentage. Must be greater than 0 and less than 100.
  target: 99.9
```

- [ ] **Step 2: Render the flag**

In `deploy/helm/kubeagent/templates/deployment.yaml`, after the closing `{{- end }}` of the `alerts.enabled` args block (line 42):

```yaml
            {{- if .Values.slo.enabled }}
            - "--slo-target={{ required "slo.target is required when slo.enabled is true" .Values.slo.target }}"
            {{- end }}
```

Note: `--slo-target` is a numeric flag inside a quoted arg string, so it does **not** take `| quote` the way `secretKeyRef.name` does — the whole `- "--slo-target=99.9"` is already one quoted scalar.

- [ ] **Step 3: Add the commented example to the raw manifest**

In `deploy/deployment.yaml`, in the container `args` list, after the existing watch flags:

```yaml
            # Track an availability SLO and expose burn-rate metrics.
            # - "--slo-target=99.9"
```

- [ ] **Step 4: Verify the rendering**

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
# default: no SLO flag at all
helm template x deploy/helm/kubeagent | grep -c 'slo-target' || echo "0 — correct, off by default"
# enabled: the flag renders with the target
helm template x deploy/helm/kubeagent --set slo.enabled=true | grep 'slo-target'
# custom target
helm template x deploy/helm/kubeagent --set slo.enabled=true --set slo.target=99.95 | grep 'slo-target'
# the required guard
helm template x deploy/helm/kubeagent --set slo.enabled=true --set slo.target=null 2>&1 | grep -o 'slo.target is required[^"]*'
```

Expected, in order: lint passes; `0 — correct, off by default`; `- "--slo-target=99.9"`; `- "--slo-target=99.95"`; `slo.target is required when slo.enabled is true`.

- [ ] **Step 5: Commit**

```bash
git add deploy/
git commit -m "feat(helm): slo.enabled and slo.target

Off by default, so an existing release picks up nothing on upgrade. No Secret and
no RBAC change: the SLI reads the evaluation the daemon already performs."
```

---

### Task 8: Chaos scenario 13

**Files:**

- Modify: `chaos/run.sh` (new `scenario_13_slo`, added to `run_scenarios`)
- Modify: `chaos/README.md` (scenario table row)

**Interfaces:**

- Consumes: the `--slo-target` flag (Task 6), the metrics endpoint, `chaos/alert-receiver.py` (already exists from slice 2).
- Produces: scenario 13.

**What this scenario asserts, and what it deliberately does not.** Filling a 6h window takes six hours. A scenario that shortened the windows would need a test-only production flag; one that claimed a breach after ninety seconds would be asserting a lie. So this scenario asserts the property that a cold daemon actually guarantees and that unit tests cannot reach end to end: **a freshly started daemon does not page, even while burning budget hard.** The threshold arithmetic and the firing transition are unit-tested with an injected clock, where six hours costs nothing. The recorded expectation must say this explicitly so the scenario can never be misread as covering the full-window path.

- [ ] **Step 1: Write the scenario**

Add to `chaos/run.sh`, immediately after `scenario_12_watch`:

```bash
scenario_13_slo() {   # SLO burn rate: series track real breakage, and a cold daemon does NOT page
  log "scenario 13: SLO burn-rate signals (cold daemon must not page)"
  local ns=chaos-slo port=18082 aport=18083 wlog wpid i alerts apid healthy broken
  wlog="$(mktemp)"
  alerts="$(mktemp)"
  python3 chaos/alert-receiver.py "$aport" "$alerts" >/dev/null 2>&1 &
  apid=$!
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=90s >/dev/null 2>&1 || true

  KUBEAGENT_ALERT_WEBHOOK="http://127.0.0.1:$aport" \
  ./kubeagent watch --context "$CTX" -n "$ns" --metrics-addr "127.0.0.1:$port" \
    --heartbeat 10s --debounce 2s --alert-format json --alert-repeat 1h \
    --slo-target 99.9 >"$wlog" 2>&1 &
  wpid=$!
  for i in $(seq 40); do
    curl -sf "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && break
    sleep 1
  done
  sleep 30
  healthy="$(curl -s "http://127.0.0.1:$port/metrics" 2>/dev/null | grep '^kubeagent_slo_' || echo '<unreachable>')"

  # Break the only workload in scope: availability goes to 0, burn rate to the
  # maximum the target allows.
  kubectl --context "$CTX" -n "$ns" patch deploy web --type=strategic \
    -p '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":"100%"}}}}' >/dev/null
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 45
  broken="$(curl -s "http://127.0.0.1:$port/metrics" 2>/dev/null | grep '^kubeagent_slo_' || echo '<unreachable>')"

  kill "$wpid" >/dev/null 2>&1 || true
  wait "$wpid" >/dev/null 2>&1 || true
  kill "$apid" >/dev/null 2>&1 || true
  wait "$apid" >/dev/null 2>&1 || true

  {
    echo '--- SLO series while the workload was healthy ---'
    printf '%s\n' "$healthy"
    echo
    echo '--- SLO series after the workload broke ---'
    printf '%s\n' "$broken"
    echo
    echo '--- SLO notifications delivered to the webhook receiver ---'
    { grep -c '"kind":"SLO"' "$alerts" 2>/dev/null || echo 0; } \
      | sed 's/^/SLO alerts delivered: /'
    echo '(must be 0: the windows are minutes old, far below the 60% coverage gate)'
    echo
    echo '--- object alerts still work in the same daemon ---'
    { grep -c '"kind":"Deployment"' "$alerts" 2>/dev/null || echo 0; } \
      | sed 's/^/Deployment alerts delivered: /'
  } | record "13. SLO burn-rate signals (cold daemon must not page)" \
    "expect: the five kubeagent_slo_* series render with target 0.999; breaking the only workload drives kubeagent_slo_availability_ratio to 0 and kubeagent_slo_burn_rate to its maximum; kubeagent_slo_window_coverage_ratio stays far below the 0.6 gate; and ZERO SLO notifications are delivered despite the burn rate being far past both thresholds — a daemon minutes old must never page on a window it has not filled. This scenario deliberately does NOT cover a real full-window breach: filling the 6h slow window takes six hours, so the threshold arithmetic and the firing transition are unit-tested with an injected clock instead. Object alerts must still fire in the same daemon, proving the suppression is specific to the SLO path and not a dead alert pipe."

  rm -f "$wlog" "$alerts"
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}
```

- [ ] **Step 2: Register it**

In `run_scenarios`, add `13_slo` immediately after `12_watch` (before `01_etcd`, which must stay last):

```bash
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 01_etcd)
```

- [ ] **Step 3: Check the script parses**

```bash
bash -n chaos/run.sh && echo "syntax ok"
```

Expected: `syntax ok`.

- [ ] **Step 4: Document the scenario**

Add a row to the scenario table in `chaos/README.md`, matching the existing row format, describing scenario 13 as "SLO burn-rate series track real breakage; a cold daemon does not page (coverage gate)".

- [ ] **Step 5: Commit**

```bash
git add chaos/
git commit -m "test(chaos): scenario 13 — SLO series, and a cold daemon must not page

Asserts what a fresh daemon actually guarantees and unit tests cannot reach end
to end: the series track real breakage, coverage stays below the gate, and zero
SLO notifications are delivered despite a burn rate far past both thresholds.

It deliberately does not fake a full-window breach. Filling the 6h window takes
six hours; shortening it would need a test-only production flag, and claiming a
breach after ninety seconds would be asserting a lie. The recorded expectation
says so explicitly so the scenario cannot later be read as covering that path.

Object alerts are asserted in the same daemon so a passing 'zero SLO alerts'
cannot come from a dead alert pipe."
```

---

### Task 9: Documentation

**Files:**

- Modify: `website/docs/features/watch-mode.md` (new `## SLO burn rate` section, placed after `## Alerting` and before `## Run it`)
- Modify: `CHANGELOG.md` (under `## [Unreleased]`)
- Modify: `docs/go-concepts.md` (ring buffer entry — note this file is gitignored, so it is edited on disk but will not appear in the commit)

**Interfaces:**

- Consumes: everything from Tasks 1–8.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the watch-mode section**

Insert into `website/docs/features/watch-mode.md`, **after** the `## Alerting` section's last subsection and **before** `## Run it`. Placement matters: the previous slice orphaned two blocks under the wrong heading by inserting an H2 in the middle of another section's subsections. Verify with `grep -n '^## \|^### ' website/docs/features/watch-mode.md` before and after.

The section must cover:

- What the SLI measures: time-weighted workload availability, `good/total` where good is workloads with no findings, weighted by seconds between samples. Same "broken" predicate the issue tracker uses.
- The arithmetic, worked: `burn = (1 - SLI) / (1 - target)`; the 3-of-200-workloads-for-an-hour example from the spec giving 15×.
- The five series with their labels.
- The fixed constants table (1h/6h windows, 14.4×/6× thresholds, 1m buckets, 60% coverage gate) and that they are not configurable.
- **The restart caveat, prominently:** state is in-memory, so after a restart both windows start empty and the coverage gate suppresses alerting until the slow window is ~3.6h warm. `kubeagent_slo_window_coverage_ratio` is how you see that.
- That `--slo-target` is a percentage, off by default, and independent of alerting: the series render with no webhook configured, they just do not page.
- The burn alert's identity (`SLO/error-budget`, issue `ErrorBudgetBurn`) and that it does **not** appear in `/issues` or `kubeagent_issues_*`.
- A Prometheus alerting-rule example for operators who want to alert on the series themselves rather than via the webhook.

- [ ] **Step 2: Add the changelog entry**

Under `## [Unreleased]`, add an `### Added` entry in the style of the v0.56.0 entry: what the SLI measures, the two windows and their thresholds, the coverage gate and why it exists, the five series, that it is off unless `--slo-target` is set, and that the daemon remains strictly read-only with no new RBAC.

- [ ] **Step 3: Add the go-concepts entry**

Check whether `docs/go-concepts.md` already has a ring-buffer entry:

```bash
grep -n -i 'ring buffer\|circular buffer' docs/go-concepts.md
```

If absent, append an entry in the established house style — **a plain everyday example first, then the kubeagent example** — with the `**Simple example:**` and `**kubeagent example:**` labels every prior entry uses. No Python comparisons. One example is enough. Cover: a fixed-size slice indexed modulo its length, why the slot must record which period it holds (so a previous lap reads as empty without a sweep), and the kubeagent case of 360 one-minute buckets covering six hours in fixed memory.

- [ ] **Step 4: Verify heading structure and the docs build**

```bash
grep -n '^## \|^### ' website/docs/features/watch-mode.md
export PATH=$PATH:$HOME/.local/bin
(cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml 2>&1 | grep -E 'WARNING|ERROR|Documentation built')
```

Expected: the new `## SLO burn rate` sits between the last `### ` of `## Alerting` and `## Run it`, with no pre-existing block orphaned under it. mkdocs reports `Documentation built` with no `WARNING` lines about these pages.

- [ ] **Step 5: Full verification**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && go test ./... -race -count=1 && gofmt -l .
```

Expected: all pass, `gofmt -l .` prints nothing.

- [ ] **Step 6: Commit**

```bash
git add website/docs/features/watch-mode.md CHANGELOG.md
git commit -m "docs: SLO burn-rate signals

Covers the SLI definition, the worked burn arithmetic, the five series, and the
fixed window/threshold constants.

The restart caveat gets its own emphasis: state is in-memory, so after a restart
both windows start empty and the coverage gate suppresses alerting until the
slow window is roughly 3.6 hours warm. An operator who does not know that would
read the silence as 'nothing is wrong'."
```

Note: `docs/go-concepts.md` is gitignored, so the entry from Step 3 stays on disk and is intentionally absent from this commit.

---

## Self-Review

**1. Spec coverage.** Every spec section maps to a task: SLI and ring → Tasks 1–2; windows/burn/coverage/verdict → Task 2; metrics → Task 3; alert payload and Sink reuse → Tasks 4–5; the error-never-samples invariant → Task 5; configuration and validation → Tasks 5–6; Helm → Task 7; the chaos decision → Task 8; docs including the go-concepts ring entry → Task 9. The spec's "Out of scope" list adds no tasks by construction.

**2. Placeholders.** None. Every code step carries the actual code; every verification step carries the exact command and its expected output. Task 9's doc steps give a required-content checklist rather than full prose, which is the one deliberate exception — prose is the implementer's to write, and the checklist pins every fact it must contain.

**3. Type consistency.** `slo.Options{Target, MaxSampleGap}`, `slo.Report{Window, Availability, BurnRate, Coverage}`, and `slo.Verdict{Firing, FiringSince, Fast, Slow}` are defined in Tasks 1–2 and used with the same field names in Tasks 3–5. `updateSLO(enabled bool, target float64, fast, slow slo.Report)` is defined in Task 3 and called with that signature in Task 5. `newSLONotifier(repeat time.Duration)` and `step(v slo.Verdict, now time.Time) (alertstate.Notification, bool)` are defined in Task 4 and called with those signatures in Task 5. `Config.SLOTarget` is a **ratio** everywhere inside `internal/watch`; the percentage→ratio conversion happens once, in `main.go` (Task 6). `workloadCensus` and `validateSLOTarget` are defined in Task 5 and tested there.

**4. One thing the implementer must not get wrong.** `Verdict` mutates `firingSince`, so it must be called exactly once per reconcile. Task 5's `applyResult` does that. A reviewer should check no second call sneaks in for metrics.
