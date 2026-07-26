package alertstate

import (
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/watchstate"
)

var base = time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

// rec builds an active tracker record for one issue on one object.
func rec(kind, ns, name, issue string, since time.Time) watchstate.Record {
	return watchstate.Record{
		Key:         watchstate.Key{Kind: kind, Namespace: ns, Name: name, Issue: issue},
		FirstSeen:   since,
		FiringSince: since,
		LastSeen:    since,
		Active:      true,
		Firings:     1,
	}
}

// flapping marks a record as flapping.
func flapping(r watchstate.Record) watchstate.Record {
	r.Flapping = true
	r.RecentFirings = 3
	return r
}

func TestRoll_NewObjectFiresOnce(t *testing.T) {
	r := New(Options{Repeat: time.Hour})
	active := []watchstate.Record{rec("Deployment", "shop", "web", "Degraded", base)}

	got := r.Roll(active, base)
	if len(got) != 1 {
		t.Fatalf("first Roll returned %d notifications, want 1: %+v", len(got), got)
	}
	n := got[0]
	if n.Reason != ReasonNew || n.Status != StatusFiring {
		t.Errorf("got reason=%s status=%s, want new/firing", n.Reason, n.Status)
	}
	if n.Object != (Object{Kind: "Deployment", Namespace: "shop", Name: "web"}) {
		t.Errorf("object = %+v", n.Object)
	}
	if len(n.Issues) != 1 || n.Issues[0] != "Degraded" {
		t.Errorf("issues = %v, want [Degraded]", n.Issues)
	}
	if !n.FiringSince.Equal(base) {
		t.Errorf("firingSince = %s, want %s", n.FiringSince, base)
	}

	if got := r.Roll(active, base.Add(time.Minute)); len(got) != 0 {
		t.Errorf("unchanged issue set inside the repeat window emitted %+v, want nothing", got)
	}
}

// TestRoll_EvolvingFailureModeIsOneAlert is the reason this package exists. The
// v0.55.0 chaos run showed a bad image walking Degraded -> ErrImagePull ->
// ImagePullBackOff, with the tracker logging RESOLVED for each superseded mode
// while the Deployment was still broken. Rolled up per object, that must be one
// alert that stays firing until the object is actually healthy.
func TestRoll_EvolvingFailureModeIsOneAlert(t *testing.T) {
	r := New(Options{Repeat: time.Hour})
	obj := Object{Kind: "Deployment", Namespace: "shop", Name: "web"}

	steps := []struct {
		at     time.Time
		active []watchstate.Record
		want   Reason
		issues []string
	}{
		{base, []watchstate.Record{
			rec("Deployment", "shop", "web", "Degraded", base),
		}, ReasonNew, []string{"Degraded"}},
		{base.Add(time.Minute), []watchstate.Record{
			rec("Deployment", "shop", "web", "Degraded", base),
			rec("Deployment", "shop", "web", "ErrImagePull", base.Add(time.Minute)),
		}, ReasonChanged, []string{"Degraded", "ErrImagePull"}},
		{base.Add(2 * time.Minute), []watchstate.Record{
			rec("Deployment", "shop", "web", "ImagePullBackOff", base.Add(2*time.Minute)),
		}, ReasonChanged, []string{"ImagePullBackOff"}},
	}
	for _, s := range steps {
		got := r.Roll(s.active, s.at)
		if len(got) != 1 {
			t.Fatalf("at %s: %d notifications, want 1: %+v", s.at, len(got), got)
		}
		n := got[0]
		if n.Status != StatusFiring {
			t.Fatalf("at %s: status = %s, want firing — an alert must never resolve while the object is broken", s.at, n.Status)
		}
		if n.Reason != s.want {
			t.Errorf("at %s: reason = %s, want %s", s.at, n.Reason, s.want)
		}
		if len(n.Issues) != len(s.issues) {
			t.Fatalf("at %s: issues = %v, want %v", s.at, n.Issues, s.issues)
		}
		for i := range s.issues {
			if n.Issues[i] != s.issues[i] {
				t.Errorf("at %s: issues = %v, want %v", s.at, n.Issues, s.issues)
			}
		}
		if !n.FiringSince.Equal(base) {
			t.Errorf("at %s: firingSince = %s, want %s — it reports when the object broke, not when the current mode appeared", s.at, n.FiringSince, base)
		}
	}

	resolved := r.Roll(nil, base.Add(3*time.Minute))
	if len(resolved) != 1 {
		t.Fatalf("clearing produced %d notifications, want 1: %+v", len(resolved), resolved)
	}
	n := resolved[0]
	if n.Status != StatusResolved || n.Reason != ReasonResolved {
		t.Errorf("got status=%s reason=%s, want resolved/resolved", n.Status, n.Reason)
	}
	if n.Object != obj {
		t.Errorf("object = %+v, want %+v", n.Object, obj)
	}
	if len(n.Issues) != 0 {
		t.Errorf("resolved issues = %v, want empty", n.Issues)
	}
	if !n.ResolvedAt.Equal(base.Add(3*time.Minute)) || !n.FiringSince.Equal(base) {
		t.Errorf("firingSince=%s resolvedAt=%s", n.FiringSince, n.ResolvedAt)
	}

	if got := r.Roll(nil, base.Add(4*time.Minute)); len(got) != 0 {
		t.Errorf("a resolved object emitted %+v on the next Roll, want silence", got)
	}
}

func TestRoll_RepeatAfterInterval(t *testing.T) {
	r := New(Options{Repeat: 30 * time.Minute})
	active := []watchstate.Record{rec("Deployment", "shop", "web", "Degraded", base)}
	r.Roll(active, base)

	if got := r.Roll(active, base.Add(29*time.Minute)); len(got) != 0 {
		t.Fatalf("emitted %+v before the repeat interval elapsed", got)
	}
	got := r.Roll(active, base.Add(30*time.Minute))
	if len(got) != 1 || got[0].Reason != ReasonRepeat || got[0].Status != StatusFiring {
		t.Fatalf("got %+v, want one firing/repeat notification", got)
	}
	if got := r.Roll(active, base.Add(31*time.Minute)); len(got) != 0 {
		t.Errorf("repeat re-armed too early: %+v", got)
	}
}

func TestRoll_ZeroRepeatTakesTheDefault(t *testing.T) {
	r := New(Options{})
	active := []watchstate.Record{rec("Deployment", "shop", "web", "Degraded", base)}
	r.Roll(active, base)

	if got := r.Roll(active, base.Add(3*time.Hour)); len(got) != 0 {
		t.Errorf("emitted %+v after 3h; the default repeat is 4h", got)
	}
	if got := r.Roll(active, base.Add(4*time.Hour)); len(got) != 1 {
		t.Errorf("got %+v after 4h, want one repeat notification", got)
	}
}

func TestRoll_FlappingPropagatesFromAnyIssue(t *testing.T) {
	r := New(Options{Repeat: time.Hour})
	got := r.Roll([]watchstate.Record{
		rec("Deployment", "shop", "web", "Degraded", base),
		flapping(rec("Deployment", "shop", "web", "CrashLoopBackOff", base)),
	}, base)
	if len(got) != 1 || !got[0].Flapping {
		t.Fatalf("got %+v, want a single flapping notification", got)
	}
}

func TestRoll_MultipleObjectsSortedAndIndependent(t *testing.T) {
	r := New(Options{Repeat: time.Hour, Cluster: "local"})
	got := r.Roll([]watchstate.Record{
		rec("Node", "", "worker-2", "KubeletUnhealthy", base),
		rec("Deployment", "shop", "web", "Degraded", base),
		rec("Deployment", "api", "gateway", "Degraded", base),
	}, base)
	want := []string{"local/Deployment/api/gateway", "local/Deployment/shop/web", "local/Node/worker-2"}
	if len(got) != len(want) {
		t.Fatalf("got %d notifications, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Object.String() != w {
			t.Errorf("notification %d is %s, want %s", i, got[i].Object.String(), w)
		}
	}

	// One object clears; the others keep firing and stay quiet.
	next := r.Roll([]watchstate.Record{
		rec("Node", "", "worker-2", "KubeletUnhealthy", base),
		rec("Deployment", "shop", "web", "Degraded", base),
	}, base.Add(time.Minute))
	if len(next) != 1 || next[0].Status != StatusResolved || next[0].Object.String() != "local/Deployment/api/gateway" {
		t.Fatalf("got %+v, want only local/Deployment/api/gateway resolved", next)
	}
}

// TestRoll_ReturnedIssuesDoNotAliasRollerState guards against the caller's
// Notification.Issues sharing backing storage with the Roller's internal
// openAlert.issues. If it did, a downstream consumer (an encoder or sender)
// mutating the returned slice in place would corrupt the roller's stored
// state, and the next Roll would compare against that corrupted state and
// wrongly decide the issue set changed.
func TestRoll_ReturnedIssuesDoNotAliasRollerState(t *testing.T) {
	r := New(Options{Repeat: time.Hour})
	active := []watchstate.Record{rec("Deployment", "shop", "web", "Degraded", base)}

	got := r.Roll(active, base)
	if len(got) != 1 {
		t.Fatalf("first Roll returned %d notifications, want 1: %+v", len(got), got)
	}
	got[0].Issues[0] = "Mutated"

	if next := r.Roll(active, base.Add(time.Minute)); len(next) != 0 {
		t.Errorf("mutating the returned Issues slice leaked into the roller's state, spuriously emitting %+v", next)
	}
}

func TestRoll_DuplicateIssuesCollapse(t *testing.T) {
	r := New(Options{Repeat: time.Hour})
	got := r.Roll([]watchstate.Record{
		rec("Deployment", "shop", "web", "Degraded", base),
		rec("Deployment", "shop", "web", "Degraded", base),
	}, base)
	if len(got) != 1 || len(got[0].Issues) != 1 {
		t.Fatalf("got %+v, want one notification carrying one issue", got)
	}
}

func TestObjectStringNamesTheCluster(t *testing.T) {
	namespaced := Object{Cluster: "prod-eu", Kind: "Deployment", Namespace: "shop", Name: "web"}
	if got, want := namespaced.String(), "prod-eu/Deployment/shop/web"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	clusterScoped := Object{Cluster: "prod-eu", Kind: "Node", Name: "worker-2"}
	if got, want := clusterScoped.String(), "prod-eu/Node/worker-2"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestRollerStampsItsCluster pins the boundary rule: watchstate.Key carries no
// cluster, because each cluster gets its own tracker and roller. The roller is
// what turns a cluster-free key into a cluster-qualified alert, so if it stops
// stamping, two clusters' alerts for the same object name collapse into one.
func TestRollerStampsItsCluster(t *testing.T) {
	r := New(Options{Cluster: "prod-us"})
	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	ns := r.Roll([]watchstate.Record{{
		Key:         watchstate.Key{Kind: "Deployment", Namespace: "shop", Name: "web", Issue: "CrashLoopBackOff"},
		FiringSince: at,
		LastSeen:    at,
	}}, at)
	if len(ns) != 1 {
		t.Fatalf("Roll returned %d notifications, want 1", len(ns))
	}
	if got := ns[0].Object.Cluster; got != "prod-us" {
		t.Errorf("Object.Cluster = %q, want %q", got, "prod-us")
	}
}
