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
	opts        Options
	ring        []bucket
	last        time.Time // the last accepted sample's instant; zero before the first
	firingSince time.Time // start of the current breach; zero when not firing
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
