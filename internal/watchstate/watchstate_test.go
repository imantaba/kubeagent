package watchstate

import (
	"testing"
	"time"
)

// t0 is a fixed base clock: every test does its own arithmetic from here, so
// nothing depends on the wall clock.
var t0 = time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

func k(kind, ns, name, issue string) Key {
	return Key{Kind: kind, Namespace: ns, Name: name, Issue: issue}
}

var api = k("Deployment", "prod", "api", "CrashLoopBackOff")

func TestKeyString(t *testing.T) {
	if got, want := api.String(), "Deployment/prod/api:CrashLoopBackOff"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	node := k("Node", "", "worker-2", "KubeletUnhealthy")
	if got, want := node.String(), "Node/worker-2:KubeletUnhealthy"; got != want {
		t.Errorf("cluster-scoped String() = %q, want %q", got, want)
	}
}

func TestObserve_FirstSightingIsNew(t *testing.T) {
	tr := New(Options{})
	d := tr.Observe([]Key{api}, t0)
	if len(d.New) != 1 || len(d.Ongoing) != 0 || len(d.Resolved) != 0 {
		t.Fatalf("delta = %+v, want exactly one New", d)
	}
	r := d.New[0]
	if !r.Active || r.Firings != 1 {
		t.Errorf("record = %+v, want Active with Firings 1", r)
	}
	if !r.FirstSeen.Equal(t0) || !r.FiringSince.Equal(t0) || !r.LastSeen.Equal(t0) {
		t.Errorf("timestamps = %+v, want all t0", r)
	}
	if s := tr.Stats(); s.NewTotal != 1 {
		t.Errorf("NewTotal = %d, want 1", s.NewTotal)
	}
}

func TestObserve_SecondSightingIsOngoing(t *testing.T) {
	tr := New(Options{})
	tr.Observe([]Key{api}, t0)
	d := tr.Observe([]Key{api}, t0.Add(time.Minute))
	if len(d.New) != 0 || len(d.Resolved) != 0 || len(d.Ongoing) != 1 {
		t.Fatalf("delta = %+v, want exactly one Ongoing", d)
	}
	if got := d.Ongoing[0].LastSeen; !got.Equal(t0.Add(time.Minute)) {
		t.Errorf("LastSeen = %v, want t0+1m", got)
	}
	if !d.Ongoing[0].FiringSince.Equal(t0) {
		t.Errorf("FiringSince moved: %v, want t0", d.Ongoing[0].FiringSince)
	}
	if s := tr.Stats(); s.NewTotal != 1 {
		t.Errorf("NewTotal = %d, want 1 (no second firing)", s.NewTotal)
	}
}

func TestObserve_DisappearanceResolvesAndRecordsMTTR(t *testing.T) {
	tr := New(Options{})
	tr.Observe([]Key{api}, t0)
	d := tr.Observe(nil, t0.Add(4*time.Minute+12*time.Second))
	if len(d.Resolved) != 1 || len(d.New) != 0 || len(d.Ongoing) != 0 {
		t.Fatalf("delta = %+v, want exactly one Resolved", d)
	}
	r := d.Resolved[0]
	if r.Active {
		t.Error("resolved record must not be Active")
	}
	if !r.ResolvedAt.Equal(t0.Add(4*time.Minute + 12*time.Second)) {
		t.Errorf("ResolvedAt = %v, want t0+4m12s", r.ResolvedAt)
	}
	s := tr.Stats()
	if s.ResolvedTotal != 1 || s.ResolutionSecondsCount != 1 || s.ResolutionSecondsSum != 252 {
		t.Errorf("stats = %+v, want ResolvedTotal 1 and 252s over 1 firing", s)
	}
	if len(tr.Active()) != 0 || len(tr.RecentlyResolved()) != 1 {
		t.Errorf("Active=%d RecentlyResolved=%d, want 0 and 1", len(tr.Active()), len(tr.RecentlyResolved()))
	}
}

func TestObserve_RefireKeepsFirstSeenAndCountsSecondFiring(t *testing.T) {
	tr := New(Options{})
	tr.Observe([]Key{api}, t0)
	tr.Observe(nil, t0.Add(time.Minute))
	d := tr.Observe([]Key{api}, t0.Add(2*time.Minute))
	if len(d.New) != 1 {
		t.Fatalf("delta = %+v, want the re-fire reported as New", d)
	}
	r := d.New[0]
	if r.Firings != 2 {
		t.Errorf("Firings = %d, want 2", r.Firings)
	}
	if !r.FirstSeen.Equal(t0) {
		t.Errorf("FirstSeen = %v, want the original t0", r.FirstSeen)
	}
	if !r.FiringSince.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("FiringSince = %v, want t0+2m", r.FiringSince)
	}
	if !r.ResolvedAt.IsZero() {
		t.Errorf("ResolvedAt = %v, want cleared on re-fire", r.ResolvedAt)
	}
}

func TestObserve_DuplicateKeysCollapseAndDeltasAreSorted(t *testing.T) {
	tr := New(Options{})
	web := k("Deployment", "prod", "web", "ImagePullBackOff")
	d := tr.Observe([]Key{web, api, api}, t0)
	if len(d.New) != 2 {
		t.Fatalf("New = %d, want 2 (duplicate collapsed)", len(d.New))
	}
	if d.New[0].Key != api || d.New[1].Key != web {
		t.Errorf("New order = %s, %s; want sorted by Key.String()", d.New[0].Key, d.New[1].Key)
	}
}

func TestAccessors_ReturnSortedCopies(t *testing.T) {
	tr := New(Options{})
	web := k("Deployment", "prod", "web", "ImagePullBackOff")
	tr.Observe([]Key{web, api}, t0)
	got := tr.Active()
	if len(got) != 2 || got[0].Key != api || got[1].Key != web {
		t.Fatalf("Active() = %+v, want sorted [api, web]", got)
	}
	got[0].Firings = 99
	if again := tr.Active(); again[0].Firings != 1 {
		t.Errorf("Active() returned a live reference: Firings = %d, want 1", again[0].Firings)
	}
}
