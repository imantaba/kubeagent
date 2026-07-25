// Package alertstate rolls the watch daemon's per-issue tracking up to one alert
// per object and decides which notifications to send. An object's alert opens
// when it acquires its first issue, updates when its issue set changes, and
// clears only when the object has no active issues at all — so a workload whose
// failure mode evolves (Degraded -> ErrImagePull -> ImagePullBackOff) never
// reports a recovery while it is still broken.
//
// Pure and deterministic — no I/O, no goroutines, and no wall-clock reads: the
// caller passes now. A Roller is not safe for concurrent use; the daemon touches
// it only from its reconcile loop.
package alertstate

import (
	"sort"
	"time"

	"github.com/imantaba/kubeagent/internal/watchstate"
)

// Object identifies the thing an alert is about. Namespace is empty for
// cluster-scoped objects.
type Object struct {
	Kind      string
	Namespace string
	Name      string
}

// String renders "Deployment/shop/web", or "Node/worker-2" when cluster-scoped.
func (o Object) String() string {
	if o.Namespace == "" {
		return o.Kind + "/" + o.Name
	}
	return o.Kind + "/" + o.Namespace + "/" + o.Name
}

// Status is what the receiver is being told.
type Status string

const (
	StatusFiring   Status = "firing"
	StatusResolved Status = "resolved"
)

// Reason is why this notification was emitted.
type Reason string

const (
	ReasonNew      Reason = "new"      // object acquired its first issue
	ReasonChanged  Reason = "changed"  // issue set changed while firing
	ReasonRepeat   Reason = "repeat"   // periodic re-send, issue set unchanged
	ReasonResolved Reason = "resolved" // object has no active issues
)

// Notification is one message to deliver.
type Notification struct {
	Object      Object
	Status      Status
	Issues      []string  // sorted and unique; empty when resolved
	FiringSince time.Time // when the OBJECT first broke, not the current failure mode
	ResolvedAt  time.Time // zero unless resolved
	Flapping    bool      // any constituent issue is flapping
	Reason      Reason
}

// Options tunes the re-send cadence. A zero Repeat takes the default, following
// the same convention as watchstate.Options.
type Options struct {
	Repeat time.Duration
}

const defaultRepeat = 4 * time.Hour

// openAlert is one object's currently-firing alert.
type openAlert struct {
	firingSince time.Time
	issues      []string
	lastSent    time.Time
}

// Roller remembers which objects have an open alert and what was last sent.
type Roller struct {
	opts Options
	open map[Object]*openAlert
}

// New returns a Roller; a zero Repeat takes the default.
func New(o Options) *Roller {
	if o.Repeat <= 0 {
		o.Repeat = defaultRepeat
	}
	return &Roller{opts: o, open: map[Object]*openAlert{}}
}

// group is one object's view of the active records in a single Roll.
type group struct {
	issues      []string
	firingSince time.Time
	flapping    bool
}

// Roll folds the tracker's active records into per-object alerts and returns the
// notifications to deliver, sorted by object.
func (r *Roller) Roll(active []watchstate.Record, now time.Time) []Notification {
	groups := map[Object]*group{}
	for _, rec := range active {
		o := Object{Kind: rec.Key.Kind, Namespace: rec.Key.Namespace, Name: rec.Key.Name}
		g, ok := groups[o]
		if !ok {
			g = &group{firingSince: rec.FiringSince}
			groups[o] = g
		}
		g.issues = append(g.issues, rec.Key.Issue)
		if rec.FiringSince.Before(g.firingSince) {
			g.firingSince = rec.FiringSince
		}
		if rec.Flapping {
			g.flapping = true
		}
	}

	var out []Notification
	for o, g := range groups {
		g.issues = uniqueSorted(g.issues)
		a, open := r.open[o]
		if !open {
			r.open[o] = &openAlert{firingSince: g.firingSince, issues: g.issues, lastSent: now}
			out = append(out, firing(o, r.open[o], g, ReasonNew))
			continue
		}
		// An object's firing start only ever moves earlier: the issue that opened
		// the alert can resolve while the object stays broken.
		if g.firingSince.Before(a.firingSince) {
			a.firingSince = g.firingSince
		}
		switch {
		case !sameIssues(a.issues, g.issues):
			a.issues = g.issues
			a.lastSent = now
			out = append(out, firing(o, a, g, ReasonChanged))
		case now.Sub(a.lastSent) >= r.opts.Repeat:
			a.lastSent = now
			out = append(out, firing(o, a, g, ReasonRepeat))
		}
	}

	for o, a := range r.open {
		if _, still := groups[o]; still {
			continue
		}
		out = append(out, Notification{
			Object:      o,
			Status:      StatusResolved,
			Reason:      ReasonResolved,
			FiringSince: a.firingSince,
			ResolvedAt:  now,
		})
		delete(r.open, o)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Object.String() < out[j].Object.String() })
	return out
}

// firing builds a firing notification from an open alert and this cycle's group.
func firing(o Object, a *openAlert, g *group, reason Reason) Notification {
	return Notification{
		Object:      o,
		Status:      StatusFiring,
		Reason:      reason,
		Issues:      append([]string(nil), g.issues...), // copy: caller must not alias openAlert.issues
		FiringSince: a.firingSince,
		Flapping:    g.flapping,
	}
}

// uniqueSorted sorts and de-duplicates, returning a fresh slice.
func uniqueSorted(in []string) []string {
	sort.Strings(in)
	out := make([]string, 0, len(in))
	for i, s := range in {
		if i > 0 && s == in[i-1] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// sameIssues compares two sorted, de-duplicated issue lists.
func sameIssues(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
