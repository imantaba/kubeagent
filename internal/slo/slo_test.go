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
