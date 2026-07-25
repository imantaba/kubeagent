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
	// math.IsNaN is checked explicitly because every comparison against NaN
	// (including >) is false in Go: a NaN got would otherwise slip past
	// math.Abs(got-want) > 1e-9 and report a false pass.
	if math.IsNaN(got) || math.Abs(got-want) > 1e-9 {
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
	tr.Observe(10, 10, base)                    // baseline only
	tr.Observe(8, 10, base.Add(30*time.Second)) // 30s x (8 good, 10 total)
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
	tr.Observe(1, 1, base.Add(time.Minute))    // 60s of weight at [base, base+1m)
	tr.Observe(1, 1, base.Add(30*time.Second)) // earlier than the last accepted sample: must be ignored

	_, tot := tr.sum(base, base.Add(2*time.Minute))
	approx(t, tot, 60, "the backwards sample itself must add no weight")

	// The guard's real job is protecting the *next* forward sample. If the
	// backwards sample above had leaked through, it would have pulled t.last
	// back to base+30s; this follow-up sample would then compute dt against
	// that corrupted baseline and re-attribute the [base+30s, base+1m)
	// interval that the first Observe already recorded, double-counting 30s
	// of workload-time. An uncorrupted tracker owes this sample only the 30s
	// of genuinely new interval [base+1m, base+90s).
	tr.Observe(1, 1, base.Add(90*time.Second))

	_, tot = tr.sum(base, base.Add(2*time.Minute))
	approx(t, tot, 90, "total after the follow-up sample must be 60s+30s of new weight, not a corrupted double count")
}

func TestObserve_ZeroTotalRecordsNoWeight(t *testing.T) {
	tr := New(Options{Target: 0.999})
	tr.Observe(0, 0, base)
	tr.Observe(0, 0, base.Add(time.Minute))
	_, tot := tr.sum(base, base.Add(2*time.Minute))
	approx(t, tot, 0, "an empty scope accumulates nothing")

	// sum alone can't tell "the guard skipped add" from "add ran and added zero
	// weight": secs*good and secs*total are both zero either way. The
	// observable difference is in the ring itself: add always stamps
	// slot.start = b to claim the bucket for the current lap, even when the
	// weight it adds is zero. A zero-total sample must never reach add, so the
	// bucket for [base, base+1m) must still be untouched.
	slot := tr.slot(base)
	if !slot.start.IsZero() {
		t.Errorf("bucket at %v has start = %v, want the zero value: a zero-total sample must never stamp the ring", base, slot.start)
	}
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

func TestReport_CoverageProratesEdgeBucket(t *testing.T) {
	// fill lays down 30 bucket-aligned minutes of data: [base, base+30m), one
	// full bucket per minute. Query the fast (1h) window at base+29m20s — 20s
	// into the last filled bucket — so the window's trailing edge lands
	// mid-bucket, the way a live reconcile loop's now actually does: it is
	// essentially never minute-aligned.
	//
	// from = now - 1h = base-30m40s, well before any data, so the leading edge
	// touches an empty bucket and contributes nothing under either rule — this
	// test isolates the trailing edge.
	//
	// The 29 buckets [base, base+29m) sit entirely inside [from, now) and count
	// for a full 60s each = 1740s. The last filled bucket, [base+29m,
	// base+30m), is covered only from base+29m to now = base+29m20s: 20s.
	//
	// Prorated (what coverage() must do): (1740 + 20) / 3600 = 1760/3600 ≈
	// 0.48889. Whole-bucket counting (sum()'s cruder rule, wrong here because it
	// would credit the uncovered 40s of that last bucket): 30*60 / 3600 =
	// 1800/3600 = 0.5. The two disagree by a full percentage point, so a
	// regression to whole-bucket counting cannot hide behind float tolerance.
	tr := New(Options{Target: 0.999})
	fill(tr, base, 30*time.Minute, 0, 10)
	now := base.Add(29*time.Minute + 20*time.Second)

	r := tr.Report(Fast, now)
	want := 1760.0 / 3600.0
	approx(t, r.Coverage, want, "coverage must prorate the partial edge bucket, not count it whole like sum() does")
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
		name     string
		build    func() (*Tracker, time.Time)
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
