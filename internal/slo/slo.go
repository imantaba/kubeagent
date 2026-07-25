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
