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

func TestObserve_FlapThresholdCrossedExactlyOnce(t *testing.T) {
	tr := New(Options{FlapWindow: 30 * time.Minute, FlapThreshold: 3})
	// Three firings inside the window: fire, clear, fire, clear, fire.
	tr.Observe([]Key{api}, t0)
	tr.Observe(nil, t0.Add(1*time.Minute))
	tr.Observe([]Key{api}, t0.Add(2*time.Minute))
	tr.Observe(nil, t0.Add(3*time.Minute))
	d := tr.Observe([]Key{api}, t0.Add(4*time.Minute))
	if len(d.NewlyFlapping) != 1 {
		t.Fatalf("NewlyFlapping = %d, want 1 on the crossing cycle", len(d.NewlyFlapping))
	}
	if got := d.NewlyFlapping[0].RecentFirings; got != 3 {
		t.Errorf("RecentFirings = %d, want 3", got)
	}
	if s := tr.Stats(); s.FlapTotal != 1 {
		t.Errorf("FlapTotal = %d, want 1", s.FlapTotal)
	}
	// Staying firing must not re-announce the flap.
	d = tr.Observe([]Key{api}, t0.Add(5*time.Minute))
	if len(d.NewlyFlapping) != 0 {
		t.Errorf("NewlyFlapping = %d on an ongoing cycle, want 0", len(d.NewlyFlapping))
	}
	if !d.Ongoing[0].Flapping {
		t.Error("Flapping must stay true while the window holds")
	}
	if s := tr.Stats(); s.FlapTotal != 1 {
		t.Errorf("FlapTotal = %d, want 1 (not re-counted)", s.FlapTotal)
	}
}

func TestObserve_FlapWindowExpiryUnflags(t *testing.T) {
	tr := New(Options{FlapWindow: 30 * time.Minute, FlapThreshold: 3})
	tr.Observe([]Key{api}, t0)
	tr.Observe(nil, t0.Add(1*time.Minute))
	tr.Observe([]Key{api}, t0.Add(2*time.Minute))
	tr.Observe(nil, t0.Add(3*time.Minute))
	tr.Observe([]Key{api}, t0.Add(4*time.Minute))
	// An hour later the three firings have aged out of the window.
	d := tr.Observe([]Key{api}, t0.Add(64*time.Minute))
	if len(d.Ongoing) != 1 {
		t.Fatalf("delta = %+v, want one Ongoing", d)
	}
	if d.Ongoing[0].Flapping {
		t.Error("Flapping must clear once the window ages out")
	}
	if got := d.Ongoing[0].RecentFirings; got != 0 {
		t.Errorf("RecentFirings = %d, want 0 after the window aged out", got)
	}
}

func TestObserve_CapDropsOverflowKeysDeterministically(t *testing.T) {
	tr := New(Options{MaxTracked: 2})
	a := k("Deployment", "prod", "a", "CrashLoopBackOff")
	b := k("Deployment", "prod", "b", "CrashLoopBackOff")
	c := k("Deployment", "prod", "c", "CrashLoopBackOff")
	d := tr.Observe([]Key{c, b, a}, t0) // sorted admission: a, b, then c overflows
	if len(d.New) != 2 {
		t.Fatalf("New = %d, want 2", len(d.New))
	}
	if d.New[0].Key != a || d.New[1].Key != b {
		t.Errorf("admitted %s and %s, want a and b (sorted order)", d.New[0].Key, d.New[1].Key)
	}
	if s := tr.Stats(); s.DroppedTotal != 1 {
		t.Errorf("DroppedTotal = %d, want 1", s.DroppedTotal)
	}
	// The dropped key is untracked — it must not appear anywhere.
	for _, r := range tr.Active() {
		if r.Key == c {
			t.Error("dropped key must not be tracked")
		}
	}
	// An already-tracked key keeps its slot on the next cycle.
	d = tr.Observe([]Key{a, b}, t0.Add(time.Minute))
	if len(d.Ongoing) != 2 {
		t.Errorf("Ongoing = %d, want 2 (existing records keep their slots)", len(d.Ongoing))
	}
}

func TestObserve_PurgeFreesSlotAfterRetention(t *testing.T) {
	tr := New(Options{MaxTracked: 1, RetainResolved: 10 * time.Minute})
	a := k("Deployment", "prod", "a", "CrashLoopBackOff")
	b := k("Deployment", "prod", "b", "CrashLoopBackOff")
	tr.Observe([]Key{a}, t0)
	tr.Observe(nil, t0.Add(time.Minute)) // a resolves, still retained (and still occupies the slot)
	if d := tr.Observe([]Key{b}, t0.Add(2*time.Minute)); len(d.New) != 0 {
		t.Fatalf("New = %d while the resolved record holds the only slot, want 0", len(d.New))
	}
	// Past the retention window a is purged, freeing the slot for b.
	d := tr.Observe([]Key{b}, t0.Add(20*time.Minute))
	if len(d.New) != 1 || d.New[0].Key != b {
		t.Fatalf("delta = %+v, want b admitted after the purge", d)
	}
	if len(tr.RecentlyResolved()) != 0 {
		t.Errorf("RecentlyResolved = %d, want 0 (a was purged)", len(tr.RecentlyResolved()))
	}
}

func TestFlapWindow_ReportsTheConfiguredWindow(t *testing.T) {
	if got := New(Options{}).FlapWindow(); got != 30*time.Minute {
		t.Errorf("FlapWindow() = %v, want the 30m default", got)
	}
	if got := New(Options{FlapWindow: time.Minute}).FlapWindow(); got != time.Minute {
		t.Errorf("FlapWindow() = %v, want 1m", got)
	}
}
