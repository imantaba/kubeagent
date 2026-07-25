// Package watchstate tracks the lifecycle of cluster issues across watch
// reconciles: which issues are new, which resolved, how long each has been
// firing, and which are flapping. Pure and deterministic — no I/O, no
// goroutines, and no wall-clock reads: the caller passes now.
//
// A Tracker is not safe for concurrent use; the daemon touches it only from its
// reconcile loop. The accessors return copies so an HTTP handler can render a
// snapshot the loop cannot mutate underneath it.
package watchstate

import (
	"sort"
	"time"
)

// Key identifies one tracked issue instance. Low-cardinality by construction:
// no timestamps, no free-form detail text.
type Key struct {
	Kind      string // "Deployment" | "Service" | "Node" | "PVC" | "Cluster" | …
	Namespace string // "" for cluster-scoped
	Name      string
	Issue     string // "CrashLoopBackOff" | "NoEndpoints" | "KubeletUnhealthy" | …
}

// String renders "Deployment/prod/api:CrashLoopBackOff", or
// "Node/worker-2:KubeletUnhealthy" when the issue is cluster-scoped.
func (k Key) String() string {
	if k.Namespace == "" {
		return k.Kind + "/" + k.Name + ":" + k.Issue
	}
	return k.Kind + "/" + k.Namespace + "/" + k.Name + ":" + k.Issue
}

// Record is one issue's tracked lifecycle.
type Record struct {
	Key           Key
	FirstSeen     time.Time // first time this key was ever observed
	FiringSince   time.Time // start of the CURRENT firing (== FirstSeen on the first one)
	LastSeen      time.Time
	Active        bool
	Firings       int       // inactive->active transitions (>= 1)
	ResolvedAt    time.Time // zero while active
	Flapping      bool      // >= FlapThreshold firings inside FlapWindow
	RecentFirings int       // firings inside FlapWindow as of the last Observe
}

// Delta is what one Observe changed.
type Delta struct {
	New           []Record // became active this cycle (first firing or a re-fire)
	Resolved      []Record // active last cycle, absent now
	Ongoing       []Record // active before and still active
	NewlyFlapping []Record // crossed the flap threshold on this cycle
}

// Options tunes retention and flap detection. A zero field takes its default.
type Options struct {
	MaxTracked     int           // hard cap on records, default 500
	RetainResolved time.Duration // resolved records stay queryable, default 1h
	FlapWindow     time.Duration // default 30m
	FlapThreshold  int           // default 3
}

const (
	defaultMaxTracked     = 500
	defaultRetainResolved = time.Hour
	defaultFlapWindow     = 30 * time.Minute
	defaultFlapThreshold  = 3
)

// Stats are monotonic process-lifetime counters (the Prometheus counter sources).
type Stats struct {
	NewTotal               int64
	ResolvedTotal          int64
	FlapTotal              int64
	DroppedTotal           int64   // new keys rejected by the cap
	ResolutionSecondsSum   float64 // MTTR numerator
	ResolutionSecondsCount int64   // MTTR denominator
}

// Tracker remembers every issue it has been shown.
type Tracker struct {
	opts    Options
	records map[Key]*Record
	firings map[Key][]time.Time // firing timestamps inside FlapWindow
	stats   Stats
}

// New returns a Tracker; zero-valued Options fields take their defaults.
func New(o Options) *Tracker {
	if o.MaxTracked <= 0 {
		o.MaxTracked = defaultMaxTracked
	}
	if o.RetainResolved <= 0 {
		o.RetainResolved = defaultRetainResolved
	}
	if o.FlapWindow <= 0 {
		o.FlapWindow = defaultFlapWindow
	}
	if o.FlapThreshold <= 0 {
		o.FlapThreshold = defaultFlapThreshold
	}
	return &Tracker{opts: o, records: map[Key]*Record{}, firings: map[Key][]time.Time{}}
}

// Observe folds one evaluation's issue set into the tracker and reports what
// changed. Keys may repeat and arrive in any order; duplicates collapse and the
// returned slices are sorted by Key.String().
func (t *Tracker) Observe(keys []Key, now time.Time) Delta {
	t.purge(now)
	t.refreshFlaps(now)

	seen := make(map[Key]bool, len(keys))
	uniq := make([]Key, 0, len(keys))
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		uniq = append(uniq, k)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].String() < uniq[j].String() })

	var d Delta
	for _, k := range uniq {
		r, ok := t.records[k]
		if !ok {
			if len(t.records) >= t.opts.MaxTracked {
				t.stats.DroppedTotal++
				continue
			}
			r = &Record{Key: k, FirstSeen: now}
			t.records[k] = r
		}
		r.LastSeen = now
		if r.Active {
			d.Ongoing = append(d.Ongoing, *r)
			continue
		}
		wasFlapping := r.Flapping
		r.Active = true
		r.FiringSince = now
		r.ResolvedAt = time.Time{}
		r.Firings++
		t.stats.NewTotal++
		t.recordFiring(r, now)
		if r.Flapping && !wasFlapping {
			t.stats.FlapTotal++
			d.NewlyFlapping = append(d.NewlyFlapping, *r)
		}
		d.New = append(d.New, *r)
	}

	for k, r := range t.records {
		if !r.Active || seen[k] {
			continue
		}
		r.Active = false
		r.ResolvedAt = now
		// Flapping and RecentFirings are deliberately left as they are: they describe
		// the firing history, which resolving does not erase. refreshFlaps clears both
		// once the last firing ages out of the window, so a resolved record can report
		// Flapping for at most FlapWindow after it clears.
		t.stats.ResolvedTotal++
		t.stats.ResolutionSecondsSum += now.Sub(r.FiringSince).Seconds()
		t.stats.ResolutionSecondsCount++
		d.Resolved = append(d.Resolved, *r)
	}
	sortRecords(d.Resolved)
	sortRecords(d.NewlyFlapping)
	sortRecords(d.New)
	sortRecords(d.Ongoing)
	return d
}

// FlapWindow is the window flap detection counts firings over. The daemon reports
// it alongside a FLAPPING log line.
func (t *Tracker) FlapWindow() time.Duration { return t.opts.FlapWindow }

// purge deletes resolved records whose retention window has passed, freeing the
// cap slots they occupied. A purged record never reappears in a Delta.
func (t *Tracker) purge(now time.Time) {
	for k, r := range t.records {
		if !r.Active && !r.ResolvedAt.IsZero() && now.Sub(r.ResolvedAt) > t.opts.RetainResolved {
			delete(t.records, k)
			delete(t.firings, k)
		}
	}
}

// refreshFlaps drops firing timestamps that have aged out of the window and
// recomputes the flap state, so an issue that stopped flapping un-flags itself
// even while it keeps firing.
func (t *Tracker) refreshFlaps(now time.Time) {
	cutoff := now.Add(-t.opts.FlapWindow)
	for k, times := range t.firings {
		kept := make([]time.Time, 0, len(times))
		for _, ts := range times {
			if ts.After(cutoff) {
				kept = append(kept, ts)
			}
		}
		if len(kept) == 0 {
			delete(t.firings, k)
		} else {
			t.firings[k] = kept
		}
		if r, ok := t.records[k]; ok {
			r.RecentFirings = len(kept)
			r.Flapping = len(kept) >= t.opts.FlapThreshold
		}
	}
}

// recordFiring appends this firing to the key's history (trimmed to the window)
// and updates the record's flap state.
func (t *Tracker) recordFiring(r *Record, now time.Time) {
	cutoff := now.Add(-t.opts.FlapWindow)
	times := append(t.firings[r.Key], now)
	kept := make([]time.Time, 0, len(times))
	for _, ts := range times {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	t.firings[r.Key] = kept
	r.RecentFirings = len(kept)
	r.Flapping = len(kept) >= t.opts.FlapThreshold
}

// Active returns the currently-firing records, sorted, as copies.
func (t *Tracker) Active() []Record { return t.snapshot(true) }

// RecentlyResolved returns the resolved records still inside the retention
// window, sorted, as copies.
func (t *Tracker) RecentlyResolved() []Record { return t.snapshot(false) }

// Stats returns the lifetime counters.
func (t *Tracker) Stats() Stats { return t.stats }

func (t *Tracker) snapshot(active bool) []Record {
	out := make([]Record, 0, len(t.records))
	for _, r := range t.records {
		if r.Active == active {
			out = append(out, *r)
		}
	}
	sortRecords(out)
	return out
}

func sortRecords(rs []Record) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Key.String() < rs[j].Key.String() })
}
