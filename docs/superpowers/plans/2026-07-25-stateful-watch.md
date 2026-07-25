# Stateful `watch` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the `watch` daemon memory — it reports which issues are **new**, which
**resolved**, how long each has been firing, and which are **flapping**, instead of
re-deriving an anonymous health picture every reconcile.

**Architecture:** A new pure package `internal/watchstate` tracks the lifecycle of each
issue instance keyed by `(kind, namespace, name, issue)`; `Observe(keys, now)` returns a
`Delta{New, Resolved, Ongoing, NewlyFlapping}`. A pure `issueKeys(*scan.Result)` in
`internal/watch` projects one evaluation into that key set. The daemon feeds the tracker
from its reconcile loop and surfaces the state three ways: transition log lines, additive
Prometheus series, and a read-only `/issues` JSON endpoint. The old whole-picture
`changeLogger`/`signature`/`describe` trio is deleted — the delta is the change detector
now.

**Tech Stack:** Go 1.26, standard library only (`sort`, `time`, `encoding/json`,
`net/http`), plus the client-go already in the module. No new dependency.

**Spec:** [docs/superpowers/specs/2026-07-25-stateful-watch-design.md](../specs/2026-07-25-stateful-watch-design.md)

## Global Constraints

- **Strictly read-only.** No new API verbs. `watchstate` performs no I/O at all. The daemon still never writes and never calls an LLM.
- **Additive only.** Existing metric names, `/healthz`, `/readyz`, and the `scan` output are untouched. The golden snapshot (`internal/report/testdata/golden-scan.txt`) must not change — `scan` is not involved in this work.
- **No new dependency** — stdlib plus the client-go already in the module.
- **Deterministic and clock-injected.** Every `watchstate` behaviour is testable without a cluster and without sleeping. `watchstate` must never call `time.Now()`.
- **`Tracker` is not goroutine-safe** and is only touched from the reconcile loop. `Active()`, `RecentlyResolved()`, and `Stats()` return copies so the HTTP handler renders a snapshot the loop cannot mutate mid-render.
- **Fixed defaults, no new flags:** `MaxTracked` 500, `RetainResolved` 1h, `FlapWindow` 30m, `FlapThreshold` 3.
- **No `Co-Authored-By: Claude` trailer** on any commit (and no Claude/Claude Code/Anthropic attribution anywhere — commits, code, comments, docs).
- **TDD** — write the failing test first, run it, watch it fail, then implement.
- **gofmt-clean.** Run `gofmt -l .` before committing; fix with `gofmt -w`, never by hand.
- Go lives at `/usr/local/go/bin` — every task starts with `export PATH=$PATH:/usr/local/go/bin`.

## File Structure

| File | Responsibility |
|------|----------------|
| `internal/watchstate/watchstate.go` (create) | `Key`, `Record`, `Delta`, `Options`, `Stats`, `Tracker` — the pure lifecycle tracker. No Kubernetes types, no I/O. |
| `internal/watchstate/watchstate_test.go` (create) | Tracker behaviour with a fake clock: transitions, MTTR, flapping, cap, purge, copy semantics. |
| `internal/watch/issues.go` (create) | `issueKeys(*scan.Result) []watchstate.Key` — the mapping from one evaluation to tracked issue instances. Pure. |
| `internal/watch/issues_test.go` (create) | Table-driven coverage of every issue source, `Expected` suppression, duplicate collapse. |
| `internal/watch/metrics.go` (modify) | Holds the issue snapshot, renders the new series, serves `/issues`. |
| `internal/watch/watch.go` (modify) | Owns the `Tracker`, folds each evaluation in via `applyResult`, logs the delta. Deletes `changeLogger`, `signature`, `describe`. |

---

### Task 1: `watchstate` core — transitions, MTTR, accessors

**Files:**
- Create: `internal/watchstate/watchstate.go`
- Test: `internal/watchstate/watchstate_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `watchstate.Key{Kind, Namespace, Name, Issue string}` with `String() string`;
  `watchstate.Record{Key Key; FirstSeen, FiringSince, LastSeen time.Time; Active bool; Firings int; ResolvedAt time.Time; Flapping bool; RecentFirings int}`;
  `watchstate.Delta{New, Resolved, Ongoing, NewlyFlapping []Record}`;
  `watchstate.Options{MaxTracked int; RetainResolved, FlapWindow time.Duration; FlapThreshold int}`;
  `watchstate.Stats{NewTotal, ResolvedTotal, FlapTotal, DroppedTotal int64; ResolutionSecondsSum float64; ResolutionSecondsCount int64}`;
  `func New(Options) *Tracker`; `func (*Tracker) Observe(keys []Key, now time.Time) Delta`;
  `func (*Tracker) Active() []Record`; `func (*Tracker) RecentlyResolved() []Record`; `func (*Tracker) Stats() Stats`.
  Task 2 adds `Flapping`/`RecentFirings` behaviour, the cap, the purge, and `FlapWindow() time.Duration` — the fields exist from this task but stay zero.

- [ ] **Step 1: Write the failing tests**

Create `internal/watchstate/watchstate_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watchstate/`
Expected: FAIL — the package does not compile yet (`undefined: Key`, `undefined: New`, …).

- [ ] **Step 3: Write the implementation**

Create `internal/watchstate/watchstate.go`:

```go
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
	return &Tracker{opts: o, records: map[Key]*Record{}}
}

// Observe folds one evaluation's issue set into the tracker and reports what
// changed. Keys may repeat and arrive in any order; duplicates collapse and the
// returned slices are sorted by Key.String().
func (t *Tracker) Observe(keys []Key, now time.Time) Delta {
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
			r = &Record{Key: k, FirstSeen: now}
			t.records[k] = r
		}
		r.LastSeen = now
		if r.Active {
			d.Ongoing = append(d.Ongoing, *r)
			continue
		}
		r.Active = true
		r.FiringSince = now
		r.ResolvedAt = time.Time{}
		r.Firings++
		t.stats.NewTotal++
		d.New = append(d.New, *r)
	}

	for k, r := range t.records {
		if !r.Active || seen[k] {
			continue
		}
		r.Active = false
		r.ResolvedAt = now
		t.stats.ResolvedTotal++
		t.stats.ResolutionSecondsSum += now.Sub(r.FiringSince).Seconds()
		t.stats.ResolutionSecondsCount++
		d.Resolved = append(d.Resolved, *r)
	}
	sortRecords(d.Resolved)
	return d
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watchstate/ -v`
Expected: PASS — all seven tests.

- [ ] **Step 5: Check formatting and commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l internal/watchstate/    # must print nothing
git add internal/watchstate/
git commit -m "feat(watchstate): track issue lifecycle transitions and MTTR"
```

---

### Task 2: `watchstate` — flapping, cardinality cap, retention purge

**Files:**
- Modify: `internal/watchstate/watchstate.go`
- Test: `internal/watchstate/watchstate_test.go` (append)

**Interfaces:**
- Consumes: everything Task 1 produced (`Key`, `Record`, `Delta`, `Options`, `Stats`, `Tracker`, `New`, `Observe`, `Active`, `RecentlyResolved`, `Stats`, `sortRecords`, the `t0`/`k`/`api` test helpers).
- Produces: populated `Record.Flapping` / `Record.RecentFirings`, populated `Delta.NewlyFlapping`, `Stats.FlapTotal` / `Stats.DroppedTotal`, and `func (*Tracker) FlapWindow() time.Duration` (used by the daemon's log line in Task 5).
- `Record.RecentFirings` and `FlapWindow()` are additions to the spec's sketched API: the spec's `FLAPPING … (3 firings in 30m)` log line needs both numbers, and deriving them in the daemon would mean re-implementing the window there.

- [ ] **Step 1: Write the failing tests**

Append to `internal/watchstate/watchstate_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watchstate/ -run 'Flap|Cap|Purge' -v`
Expected: FAIL — `tr.FlapWindow undefined`, and the flap/cap/purge assertions fail
(`NewlyFlapping = 0, want 1`; `New = 3, want 2`).

- [ ] **Step 3: Add the firing history, the cap, and the purge**

In `internal/watchstate/watchstate.go`, add a firing-history map to `Tracker` and
initialize it in `New`:

```go
type Tracker struct {
	opts    Options
	records map[Key]*Record
	firings map[Key][]time.Time // firing timestamps inside FlapWindow
	stats   Stats
}
```

```go
	return &Tracker{opts: o, records: map[Key]*Record{}, firings: map[Key][]time.Time{}}
```

Replace `Observe` with the full version (purge and flap refresh first, cap on admission,
flap accounting on each new firing):

```go
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
		t.stats.ResolvedTotal++
		t.stats.ResolutionSecondsSum += now.Sub(r.FiringSince).Seconds()
		t.stats.ResolutionSecondsCount++
		d.Resolved = append(d.Resolved, *r)
	}
	sortRecords(d.Resolved)
	sortRecords(d.NewlyFlapping)
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
```

- [ ] **Step 4: Run the whole package to verify everything passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watchstate/ -v`
Expected: PASS — all twelve tests (Task 1's seven still green).

- [ ] **Step 5: Check formatting and commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l internal/watchstate/    # must print nothing
git add internal/watchstate/
git commit -m "feat(watchstate): flap detection, cardinality cap, retention purge"
```

---

### Task 3: `issueKeys` — project one evaluation into tracked issue keys

**Files:**
- Create: `internal/watch/issues.go`
- Test: `internal/watch/issues_test.go`

**Interfaces:**
- Consumes: `watchstate.Key{Kind, Namespace, Name, Issue string}` from Task 1.
- Produces: `func issueKeys(res *scan.Result) []watchstate.Key` (package-private, used by Task 5).
- Reuses the existing `sampleResult()` fixture from `internal/watch/metrics_test.go` (same package) — do not duplicate it.

- [ ] **Step 1: Write the failing tests**

Create `internal/watch/issues_test.go`:

```go
package watch

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/certhealth"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

// keySet renders the keys as a lookup of "kind/ns/name:issue" strings.
func keySet(keys []watchstate.Key) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k.String()] = true
	}
	return out
}

// TestIssueKeys_CoversEverySource walks the shared sampleResult fixture and
// asserts one key per issue source, so a new detector wired into scan.Result but
// not into issueKeys shows up as a missing key here.
func TestIssueKeys_CoversEverySource(t *testing.T) {
	got := keySet(issueKeys(sampleResult()))
	for _, want := range []string{
		"Deployment/shop/web:CrashLoopBackOff",
		"Service/shop/api-svc:NoEndpoints",
		"Ingress/shop/web:NoEndpoints",
		"PVC/shop/data-pvc:ProvisioningFailed",
		"Namespace/legacy-ns:StuckTerminating",
		"PodDisruptionBudget/shop/api:",
		"HorizontalPodAutoscaler/shop/api-hpa:",
		"ValidatingWebhookConfiguration/policy-webhook/w:no-endpoints",
		"ValidatingWebhookConfiguration/slow-webhook/s.io:high-timeout",
		"ResourceQuota/shop/compute/pods:near",
		"Node/w:KubeletUnhealthy",
		"Cluster/control-plane:Unhealthy",
		"Cluster/coredns:DNSDegraded",
		"Secret/shop/shop-tls:CertExpired",
		"Secret/infra/api-tls:CertExpiring",
		"Volume/n1:DiskOverThreshold",
	} {
		if !got[want] {
			t.Errorf("missing key %q; got %v", want, got)
		}
	}
}

func TestIssueKeys_SkipsExpectedIssues(t *testing.T) {
	got := keySet(issueKeys(sampleResult()))
	for _, unwanted := range []string{
		"Service/shop/parked-svc:NoEndpoints",
		"Ingress/shop/parked:NoEndpoints",
	} {
		if got[unwanted] {
			t.Errorf("expected/parked issue %q must not be tracked", unwanted)
		}
	}
}

func TestIssueKeys_CollapsesDuplicateRoutes(t *testing.T) {
	res := &scan.Result{IngressIssues: []ingresshealth.RouteIssue{
		{Namespace: "shop", Ingress: "web", Host: "a.example", Path: "/x", Problem: "NoEndpoints"},
		{Namespace: "shop", Ingress: "web", Host: "b.example", Path: "/y", Problem: "NoEndpoints"},
	}}
	if got := issueKeys(res); len(got) != 1 {
		t.Errorf("got %d keys, want 1 (same ingress + problem collapses): %v", len(got), got)
	}
}

func TestIssueKeys_FlaggedWithoutFindingsIsDegraded(t *testing.T) {
	res := &scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{
		{Namespace: "shop", Name: "api", Kind: "StatefulSet", Ready: 1, Desired: 3},
		{Namespace: "shop", Name: "ok", Kind: "Deployment", Ready: 2, Desired: 2},
	}}}
	got := keySet(issueKeys(res))
	if !got["StatefulSet/shop/api:Degraded"] {
		t.Errorf("flagged findingless workload must yield Degraded; got %v", got)
	}
	if len(got) != 1 {
		t.Errorf("healthy workload must not be tracked; got %v", got)
	}
}

func TestIssueKeys_WorkloadWithFindingsSkipsDegraded(t *testing.T) {
	res := &scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{
		{Namespace: "shop", Name: "api", Kind: "Deployment", Ready: 0, Desired: 1,
			Findings: []diagnose.Finding{{Issue: "OOMKilled"}, {Issue: "CrashLoopBackOff"}}},
	}}}
	got := keySet(issueKeys(res))
	if got["Deployment/shop/api:Degraded"] {
		t.Errorf("a workload with findings must not also report Degraded; got %v", got)
	}
	if len(got) != 2 {
		t.Errorf("want one key per finding; got %v", got)
	}
}

func TestIssueKeys_DownNodesCarryTheirReason(t *testing.T) {
	res := &scan.Result{Health: clusterhealth.ClusterHealth{DownNodes: []clusterhealth.DownNode{
		{Name: "w1", Reason: "NotReady"},
		{Name: "w2", Reason: "kubelet not heartbeating"},
	}}}
	got := keySet(issueKeys(res))
	if !got["Node/w1:NotReady"] || !got["Node/w2:KubeletNotHeartbeating"] {
		t.Errorf("down nodes = %v, want NotReady and KubeletNotHeartbeating", got)
	}
}

func TestIssueKeys_NilReportsAndHealthyClusterYieldNothing(t *testing.T) {
	res := &scan.Result{} // no Certificates report, no issues anywhere
	if got := issueKeys(res); len(got) != 0 {
		t.Errorf("empty result yielded %v, want no keys", got)
	}
	healthy := &scan.Result{
		Certificates:  &certhealth.Report{Checked: 3},
		ServiceIssues: []svchealth.Issue{{Namespace: "shop", Name: "ok", Problem: "NoEndpoints", Expected: true}},
	}
	if got := issueKeys(healthy); len(got) != 0 {
		t.Errorf("healthy result yielded %v, want no keys", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run IssueKeys`
Expected: FAIL — `undefined: issueKeys`.

- [ ] **Step 3: Write the implementation**

Create `internal/watch/issues.go`:

```go
package watch

import (
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

// issueKeys projects one evaluation into the set of tracked issue instances.
// Pure and deterministic; duplicates collapse, so two broken routes on the same
// Ingress with the same problem yield one key. Sorting is the tracker's job.
//
// Intentionally excluded: intentionally-empty ("Expected") Service and Ingress
// issues, and the advisory config reports (NodeReserve, PVCReclaim,
// SecurityIssues) — those describe standing configuration, not incidents that
// fire and resolve, so tracking them would make MTTR meaningless.
func issueKeys(res *scan.Result) []watchstate.Key {
	seen := map[watchstate.Key]bool{}
	var keys []watchstate.Key
	add := func(kind, namespace, name, issue string) {
		k := watchstate.Key{Kind: kind, Namespace: namespace, Name: name, Issue: issue}
		if seen[k] {
			return
		}
		seen[k] = true
		keys = append(keys, k)
	}

	for _, w := range res.Inventory.Workloads {
		if len(w.Findings) > 0 {
			for _, f := range w.Findings {
				add(w.Kind, w.Namespace, w.Name, f.Issue)
			}
			continue
		}
		if w.Flagged() {
			add(w.Kind, w.Namespace, w.Name, "Degraded")
		}
	}
	for _, i := range res.ServiceIssues {
		if !i.Expected {
			add("Service", i.Namespace, i.Name, i.Problem)
		}
	}
	for _, i := range res.IngressIssues {
		if !i.Expected {
			add("Ingress", i.Namespace, i.Ingress, i.Problem)
		}
	}
	for _, i := range res.PVCIssues {
		add("PVC", i.Namespace, i.Name, i.Reason)
	}
	for _, i := range res.StuckTerminating {
		add(i.Kind, i.Namespace, i.Name, "StuckTerminating")
	}
	for _, i := range res.PDBIssues {
		add("PodDisruptionBudget", i.Namespace, i.Name, i.Category)
	}
	for _, i := range res.HPAIssues {
		add("HorizontalPodAutoscaler", i.Namespace, i.Name, i.Category)
	}
	for _, i := range res.WebhookIssues {
		add(i.Kind, "", i.Config+"/"+i.Webhook, i.Problem)
	}
	for _, i := range res.QuotaIssues {
		add("ResourceQuota", i.Namespace, i.Quota+"/"+i.Resource, i.Severity)
	}
	for _, n := range res.Health.DownNodes {
		issue := "KubeletNotHeartbeating"
		if n.Reason == "NotReady" {
			issue = "NotReady"
		}
		add("Node", "", n.Name, issue)
	}
	for _, i := range res.KubeletHealth.Unhealthy {
		add("Node", "", i.Node, "KubeletUnhealthy")
	}
	if res.ControlPlane.Status == "unhealthy" {
		add("Cluster", "", "control-plane", "Unhealthy")
	}
	if res.DNS.Status == "degraded" {
		add("Cluster", "", "coredns", "DNSDegraded")
	}
	if res.Certificates != nil {
		for _, c := range res.Certificates.Expired {
			add("Secret", c.Namespace, c.Name, "CertExpired")
		}
		for _, c := range res.Certificates.Expiring {
			add("Secret", c.Namespace, c.Name, "CertExpiring")
		}
	}
	for _, v := range res.DiskUsage.Over {
		name := v.Name
		if v.Kind == "node" {
			name = v.Node
		}
		add("Volume", v.Namespace, name, "DiskOverThreshold")
	}
	return keys
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run IssueKeys -v`
Expected: PASS — all seven tests.

- [ ] **Step 5: Check formatting and commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l internal/watch/    # must print nothing
git add internal/watch/issues.go internal/watch/issues_test.go
git commit -m "feat(watch): project an evaluation into tracked issue keys"
```

---

### Task 4: metrics — issue snapshot, new series, `/issues` endpoint

**Files:**
- Modify: `internal/watch/metrics.go` (struct fields near `internal/watch/metrics.go:45-78`, `render` at `:161`, `handler` at `:224`)
- Test: `internal/watch/metrics_test.go` (append)

**Interfaces:**
- Consumes: `watchstate.Record`, `watchstate.Stats`, `(*watchstate.Tracker).Active/RecentlyResolved/Stats` from Tasks 1-2.
- Produces: `func (m *metrics) updateIssues(tr *watchstate.Tracker, now time.Time)` (called by Task 5), the new Prometheus series, and the `/issues` handler.
- The spec sketches the call as `m.updateIssues(tr)`; it takes `now` explicitly here so metric ages and the JSON are testable without the wall clock. Age is measured from the snapshot's timestamp, so `/metrics` and `/issues` never disagree.

- [ ] **Step 1: Write the failing tests**

Append to `internal/watch/metrics_test.go` (add `"encoding/json"` and
`"github.com/imantaba/kubeagent/internal/watchstate"` to the import block):

```go
// trackerWithFixture returns a tracker holding one active issue (60s old at
// `at`) and one resolved issue (fired 30s), for the metrics/JSON assertions.
func trackerWithFixture() (*watchstate.Tracker, time.Time) {
	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	api := watchstate.Key{Kind: "Deployment", Namespace: "prod", Name: "api", Issue: "CrashLoopBackOff"}
	svc := watchstate.Key{Kind: "Service", Namespace: "prod", Name: "api", Issue: "NoEndpoints"}
	tr := watchstate.New(watchstate.Options{})
	tr.Observe([]watchstate.Key{api, svc}, base)
	tr.Observe([]watchstate.Key{api}, base.Add(30*time.Second)) // svc resolves after 30s
	at := base.Add(60 * time.Second)
	tr.Observe([]watchstate.Key{api}, at)
	return tr, at
}

func TestMetrics_RendersIssueSeries(t *testing.T) {
	m := newMetrics()
	tr, at := trackerWithFixture()
	m.updateIssues(tr, at)
	out := m.render()
	for _, want := range []string{
		"kubeagent_issues_active 1",
		"kubeagent_issues_flapping 0",
		"kubeagent_issues_new_total 2",
		"kubeagent_issues_resolved_total 1",
		"kubeagent_issues_flapping_total 0",
		"kubeagent_issues_dropped_total 0",
		"kubeagent_issue_resolution_seconds_sum 30",
		"kubeagent_issue_resolution_seconds_count 1",
		`kubeagent_issue_active{kind="Deployment",namespace="prod",name="api",issue="CrashLoopBackOff"} 1`,
		`kubeagent_issue_age_seconds{kind="Deployment",namespace="prod",name="api",issue="CrashLoopBackOff"} 60`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
	}
	// The resolved issue must not linger as an active series.
	if strings.Contains(out, `kubeagent_issue_active{kind="Service"`) {
		t.Errorf("resolved issue still rendered as active:\n%s", out)
	}
}

func TestMetrics_IssueSeriesAbsentBeforeFirstUpdate(t *testing.T) {
	out := newMetrics().render()
	if strings.Contains(out, "kubeagent_issue_active{") {
		t.Errorf("per-issue series rendered with no issues:\n%s", out)
	}
	if !strings.Contains(out, "kubeagent_issues_active 0") {
		t.Errorf("aggregate gauge must still render as 0:\n%s", out)
	}
}

func TestMetrics_IssuesEndpointShape(t *testing.T) {
	m := newMetrics()
	tr, at := trackerWithFixture()
	m.updateIssues(tr, at)
	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/issues")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /issues: status %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Active []struct {
			Kind        string `json:"kind"`
			Namespace   string `json:"namespace"`
			Name        string `json:"name"`
			Issue       string `json:"issue"`
			FirstSeen   string `json:"firstSeen"`
			FiringSince string `json:"firingSince"`
			LastSeen    string `json:"lastSeen"`
			Firings     int    `json:"firings"`
			Flapping    bool   `json:"flapping"`
			AgeSeconds  *int64 `json:"ageSeconds"`
			ResolvedAt  string `json:"resolvedAt"`
		} `json:"active"`
		Resolved []struct {
			Kind              string `json:"kind"`
			Name              string `json:"name"`
			Issue             string `json:"issue"`
			ResolvedAt        string `json:"resolvedAt"`
			ResolutionSeconds *int64 `json:"resolutionSeconds"`
			AgeSeconds        *int64 `json:"ageSeconds"`
		} `json:"resolved"`
		Stats struct {
			NewTotal               int64   `json:"newTotal"`
			ResolvedTotal          int64   `json:"resolvedTotal"`
			FlapTotal              int64   `json:"flapTotal"`
			DroppedTotal           int64   `json:"droppedTotal"`
			ResolutionSecondsSum   float64 `json:"resolutionSecondsSum"`
			ResolutionSecondsCount int64   `json:"resolutionSecondsCount"`
		} `json:"stats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding /issues: %v", err)
	}
	if len(body.Active) != 1 || len(body.Resolved) != 1 {
		t.Fatalf("active=%d resolved=%d, want 1 and 1", len(body.Active), len(body.Resolved))
	}
	a := body.Active[0]
	if a.Kind != "Deployment" || a.Namespace != "prod" || a.Name != "api" || a.Issue != "CrashLoopBackOff" {
		t.Errorf("active identity = %+v", a)
	}
	if a.FirstSeen != "2026-07-25T10:00:00Z" || a.FiringSince != "2026-07-25T10:00:00Z" || a.LastSeen != "2026-07-25T10:01:00Z" {
		t.Errorf("active timestamps = %+v, want RFC3339 UTC", a)
	}
	if a.AgeSeconds == nil || *a.AgeSeconds != 60 {
		t.Errorf("active ageSeconds = %v, want 60", a.AgeSeconds)
	}
	if a.ResolvedAt != "" {
		t.Errorf("active record must omit resolvedAt, got %q", a.ResolvedAt)
	}
	r := body.Resolved[0]
	if r.ResolvedAt != "2026-07-25T10:00:30Z" {
		t.Errorf("resolvedAt = %q, want 2026-07-25T10:00:30Z", r.ResolvedAt)
	}
	if r.ResolutionSeconds == nil || *r.ResolutionSeconds != 30 {
		t.Errorf("resolutionSeconds = %v, want 30", r.ResolutionSeconds)
	}
	if r.AgeSeconds != nil {
		t.Errorf("resolved record must omit ageSeconds, got %v", r.AgeSeconds)
	}
	if body.Stats.NewTotal != 2 || body.Stats.ResolvedTotal != 1 || body.Stats.ResolutionSecondsSum != 30 {
		t.Errorf("stats = %+v", body.Stats)
	}
}

func TestMetrics_IssuesEndpointEmptyArrays(t *testing.T) {
	srv := httptest.NewServer(newMetrics().handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/issues")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"active":[]`, `"resolved":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("empty tracker must render %s, got %s", want, raw)
		}
	}
}
```

Note: `io` is already imported by `metrics.go` but not by `metrics_test.go` — add `"io"`
to the test file's imports as well.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run 'Issue' -v`
Expected: FAIL — `m.updateIssues undefined`.

- [ ] **Step 3: Add the snapshot, the series, and the endpoint**

In `internal/watch/metrics.go`, add the imports `"encoding/json"` and
`"github.com/imantaba/kubeagent/internal/watchstate"`, then add the snapshot type
above the `metrics` struct:

```go
// issueSnapshot is the tracker's state as of the last reconcile. Ages are
// measured from At, so /metrics and /issues never disagree about an issue's age.
type issueSnapshot struct {
	At       time.Time
	Active   []watchstate.Record
	Resolved []watchstate.Record
	Stats    watchstate.Stats
}
```

Add one field to the `metrics` struct (after `certsExpiring`):

```go
	issues                issueSnapshot
```

Add the updater next to `markReady`:

```go
// updateIssues records the tracker state for rendering. now becomes the snapshot's
// reference time for every age it reports.
func (m *metrics) updateIssues(tr *watchstate.Tracker, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.issues = issueSnapshot{At: now, Active: tr.Active(), Resolved: tr.RecentlyResolved(), Stats: tr.Stats()}
}
```

In `render`, add a float-valued counter helper beside the existing `counter` helper:

```go
	counterF := func(name, help string, v float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %g\n", name, help, name, name, v)
	}
```

Then, immediately after the existing `kubeagent_findings` loop (before the
`m.nodeFSRatio` block), render the issue series:

```go
	flapping := 0
	for _, r := range m.issues.Active {
		if r.Flapping {
			flapping++
		}
	}
	gauge("kubeagent_issues_active", "Issues currently firing, tracked across reconciles", float64(len(m.issues.Active)))
	gauge("kubeagent_issues_flapping", "Active issues that have crossed the flap threshold", float64(flapping))
	counter("kubeagent_issues_new_total", "Issue firings observed since start", m.issues.Stats.NewTotal)
	counter("kubeagent_issues_resolved_total", "Issue firings that resolved since start", m.issues.Stats.ResolvedTotal)
	counter("kubeagent_issues_flapping_total", "Times an issue crossed the flap threshold since start", m.issues.Stats.FlapTotal)
	counter("kubeagent_issues_dropped_total", "New issues left untracked because the tracker is at capacity", m.issues.Stats.DroppedTotal)
	counterF("kubeagent_issue_resolution_seconds_sum", "Seconds issues spent firing before resolving (MTTR numerator)", m.issues.Stats.ResolutionSecondsSum)
	counter("kubeagent_issue_resolution_seconds_count", "Issue firings that resolved (MTTR denominator)", m.issues.Stats.ResolutionSecondsCount)
	if len(m.issues.Active) > 0 {
		fmt.Fprintf(&b, "# HELP kubeagent_issue_active 1 while this issue instance is firing\n# TYPE kubeagent_issue_active gauge\n")
		for _, r := range m.issues.Active {
			fmt.Fprintf(&b, "kubeagent_issue_active{%s} 1\n", issueLabels(r.Key))
		}
		fmt.Fprintf(&b, "# HELP kubeagent_issue_age_seconds Seconds since this issue instance started firing\n# TYPE kubeagent_issue_age_seconds gauge\n")
		for _, r := range m.issues.Active {
			fmt.Fprintf(&b, "kubeagent_issue_age_seconds{%s} %d\n", issueLabels(r.Key), ageSeconds(r.FiringSince, m.issues.At))
		}
	}
```

Add the two helpers and the JSON views at the bottom of the file:

```go
func issueLabels(k watchstate.Key) string {
	return fmt.Sprintf("kind=%q,namespace=%q,name=%q,issue=%q", k.Kind, k.Namespace, k.Name, k.Issue)
}

// ageSeconds is whole seconds from since to at, floored at zero (a snapshot can
// never legitimately predate the firing it describes).
func ageSeconds(since, at time.Time) int64 {
	s := int64(at.Sub(since).Seconds())
	if s < 0 {
		return 0
	}
	return s
}

// issueView is one record as served by /issues. The pointer fields distinguish
// "not applicable" from a legitimate zero: active records carry ageSeconds and
// omit resolution data, resolved records the reverse.
type issueView struct {
	Kind              string `json:"kind"`
	Namespace         string `json:"namespace,omitempty"`
	Name              string `json:"name"`
	Issue             string `json:"issue"`
	FirstSeen         string `json:"firstSeen"`
	FiringSince       string `json:"firingSince"`
	LastSeen          string `json:"lastSeen"`
	Firings           int    `json:"firings"`
	Flapping          bool   `json:"flapping"`
	AgeSeconds        *int64 `json:"ageSeconds,omitempty"`
	ResolvedAt        string `json:"resolvedAt,omitempty"`
	ResolutionSeconds *int64 `json:"resolutionSeconds,omitempty"`
}

type statsView struct {
	NewTotal               int64   `json:"newTotal"`
	ResolvedTotal          int64   `json:"resolvedTotal"`
	FlapTotal              int64   `json:"flapTotal"`
	DroppedTotal           int64   `json:"droppedTotal"`
	ResolutionSecondsSum   float64 `json:"resolutionSecondsSum"`
	ResolutionSecondsCount int64   `json:"resolutionSecondsCount"`
}

type issuesView struct {
	Active   []issueView `json:"active"`
	Resolved []issueView `json:"resolved"`
	Stats    statsView   `json:"stats"`
}

func issueViews(rs []watchstate.Record, at time.Time, resolved bool) []issueView {
	out := make([]issueView, 0, len(rs))
	for _, r := range rs {
		v := issueView{
			Kind:        r.Key.Kind,
			Namespace:   r.Key.Namespace,
			Name:        r.Key.Name,
			Issue:       r.Key.Issue,
			FirstSeen:   rfc3339(r.FirstSeen),
			FiringSince: rfc3339(r.FiringSince),
			LastSeen:    rfc3339(r.LastSeen),
			Firings:     r.Firings,
			Flapping:    r.Flapping,
		}
		if resolved {
			v.ResolvedAt = rfc3339(r.ResolvedAt)
			secs := ageSeconds(r.FiringSince, r.ResolvedAt)
			v.ResolutionSeconds = &secs
		} else {
			secs := ageSeconds(r.FiringSince, at)
			v.AgeSeconds = &secs
		}
		out = append(out, v)
	}
	return out
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// issuesJSON renders the tracked-issue snapshot. Held under the read lock so the
// reconcile loop cannot swap the snapshot mid-encode.
func (m *metrics) issuesJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return json.Marshal(issuesView{
		Active:   issueViews(m.issues.Active, m.issues.At, false),
		Resolved: issueViews(m.issues.Resolved, m.issues.At, true),
		Stats: statsView{
			NewTotal:               m.issues.Stats.NewTotal,
			ResolvedTotal:          m.issues.Stats.ResolvedTotal,
			FlapTotal:              m.issues.Stats.FlapTotal,
			DroppedTotal:           m.issues.Stats.DroppedTotal,
			ResolutionSecondsSum:   m.issues.Stats.ResolutionSecondsSum,
			ResolutionSecondsCount: m.issues.Stats.ResolutionSecondsCount,
		},
	})
}
```

Register the endpoint in `handler`, after the `/metrics` handler:

```go
	mux.HandleFunc("/issues", func(w http.ResponseWriter, _ *http.Request) {
		body, err := m.issuesJSON()
		if err != nil {
			http.Error(w, "encoding issues", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
```

- [ ] **Step 4: Run the package tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -v`
Expected: PASS — the four new tests plus every pre-existing `watch` test
(`TestMetrics_RenderReflectsResult`, `TestMetrics_ReadyzGate`, … must stay green: the
change is additive).

- [ ] **Step 5: Check formatting and commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l internal/watch/    # must print nothing
git add internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): expose tracked issues as metrics and /issues JSON"
```

---

### Task 5: wire the reconcile loop — `applyResult`, `logDelta`, delete the old change detector

**Files:**
- Modify: `internal/watch/watch.go` (reconcile closure at `:100-111`; delete `changeLogger`/`signature`/`describe` at `:148-201`)
- Test: `internal/watch/watch_test.go` (delete the two obsolete tests, add three)

**Interfaces:**
- Consumes: `issueKeys` (Task 3), `(*metrics).updateIssues` (Task 4), `watchstate.New/Observe/Active/Stats/FlapWindow` and `watchstate.Delta`/`Record` (Tasks 1-2).
- Produces: `func applyResult(m *metrics, tr *watchstate.Tracker, res *scan.Result, dur time.Duration, now time.Time, err error)` and `func logDelta(res *scan.Result, d watchstate.Delta, active int, flapWindow time.Duration)`. Both package-private; `applyResult` exists as a named function (rather than inline in the closure) so the evaluation-error invariant is unit-testable.

- [ ] **Step 1: Write the failing tests**

In `internal/watch/watch_test.go`, **delete** `TestChangeLogger_OnlyLogsOnChange`,
`TestSignature_DistinguishesFindingsAndErrors`, and the now-unused `errDummy` / `errStr`
helpers (lines 19-47). Add the imports `"bytes"`, `"errors"`, `"log"`, `"os"`, `"strings"`,
and `"github.com/imantaba/kubeagent/internal/watchstate"`; `clusterhealth` and `scan` are
already imported, and `inventory` becomes unused once the deleted tests are gone — drop it
(the compiler will say so). Then add:

```go
// captureLog redirects the standard logger for the duration of fn.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	fn()
	return buf.String()
}

// TestApplyResult_EvaluationErrorNeverReachesTheTracker pins the core invariant:
// a failed evaluation is not "all clear". If the error path reached Observe, one
// API blip would resolve every issue and re-fire them all on the next success —
// corrupting MTTR, inflating flap counts, and (once alerting lands) paging the
// on-call for a network hiccup.
func TestApplyResult_EvaluationErrorNeverReachesTheTracker(t *testing.T) {
	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

	captureLog(t, func() { applyResult(m, tr, sampleResult(), time.Millisecond, at, nil) })
	before := len(tr.Active())
	if before == 0 {
		t.Fatal("fixture must produce active issues")
	}

	out := captureLog(t, func() {
		applyResult(m, tr, &scan.Result{}, time.Millisecond, at.Add(time.Minute), errors.New("boom"))
	})
	if got := len(tr.Active()); got != before {
		t.Errorf("active issues %d -> %d; an evaluation error must resolve nothing", before, got)
	}
	if s := tr.Stats(); s.ResolvedTotal != 0 {
		t.Errorf("ResolvedTotal = %d, want 0", s.ResolvedTotal)
	}
	if !strings.Contains(out, "evaluation error: boom") {
		t.Errorf("error must be logged, got %q", out)
	}
}

func TestApplyResult_LogsTransitionsAndStaysQuietInSteadyState(t *testing.T) {
	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

	first := captureLog(t, func() { applyResult(m, tr, sampleResult(), time.Millisecond, at, nil) })
	if !strings.Contains(first, "NEW Deployment/shop/web:CrashLoopBackOff") {
		t.Errorf("first sighting must log a NEW line, got %q", first)
	}
	if !strings.Contains(first, "issue(s) active,") {
		t.Errorf("summary line missing from %q", first)
	}

	steady := captureLog(t, func() { applyResult(m, tr, sampleResult(), time.Millisecond, at.Add(time.Minute), nil) })
	if steady != "" {
		t.Errorf("an unchanged reconcile must log nothing, got %q", steady)
	}

	cleared := captureLog(t, func() {
		applyResult(m, tr, &scan.Result{}, time.Millisecond, at.Add(2*time.Minute), nil)
	})
	if !strings.Contains(cleared, "RESOLVED Deployment/shop/web:CrashLoopBackOff (fired for 2m0s)") {
		t.Errorf("clearing must log a RESOLVED line with the firing duration, got %q", cleared)
	}
}

func TestLogDelta_ReportsFlapping(t *testing.T) {
	res := &scan.Result{Health: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3}}
	rec := watchstate.Record{
		Key:           watchstate.Key{Kind: "Deployment", Namespace: "prod", Name: "api", Issue: "CrashLoopBackOff"},
		RecentFirings: 3,
	}
	out := captureLog(t, func() {
		logDelta(res, watchstate.Delta{NewlyFlapping: []watchstate.Record{rec}}, 1, 30*time.Minute)
	})
	if !strings.Contains(out, "FLAPPING Deployment/prod/api:CrashLoopBackOff (3 firings in 30m0s)") {
		t.Errorf("flap line missing from %q", out)
	}
	if !strings.Contains(out, "cluster Degraded (2/3 nodes ready) — 1 issue(s) active, 0 new, 0 resolved") {
		t.Errorf("summary line missing from %q", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run 'ApplyResult|LogDelta' -v`
Expected: FAIL — `undefined: applyResult`, `undefined: logDelta`.

- [ ] **Step 3: Wire the tracker and delete the old change detector**

In `internal/watch/watch.go`, add the import
`"github.com/imantaba/kubeagent/internal/watchstate"` and drop `"sort"` and `"strings"`
(only `signature` used them).

Replace the `var cl changeLogger` / `reconcile` block with:

```go
	tr := watchstate.New(watchstate.Options{})
	reconcile := func() {
		start := time.Now()
		res, err := scan.Evaluate(ctx, client, opts)
		applyResult(m, tr, &res, time.Since(start), time.Now(), err)
	}
```

Delete `changeLogger`, `changed`, `signature`, and `describe` entirely, and put these in
their place:

```go
// applyResult folds one evaluation into the metrics and the issue tracker, and
// logs whatever changed. A failed evaluation never reaches the tracker: an
// evaluation error is not "all clear", and treating it as one would resolve every
// tracked issue, then re-fire them all on the next success.
func applyResult(m *metrics, tr *watchstate.Tracker, res *scan.Result, dur time.Duration, now time.Time, err error) {
	m.update(res, dur, now, err)
	if err != nil {
		log.Printf("kubeagent: evaluation error: %v", err)
		return
	}
	d := tr.Observe(issueKeys(res), now)
	m.updateIssues(tr, now)
	logDelta(res, d, len(tr.Active()), tr.FlapWindow())
}

// logDelta prints one line per transition plus a summary. A reconcile that
// changed nothing prints nothing, so steady state stays quiet.
func logDelta(res *scan.Result, d watchstate.Delta, active int, flapWindow time.Duration) {
	if len(d.New) == 0 && len(d.Resolved) == 0 && len(d.NewlyFlapping) == 0 {
		return
	}
	for _, r := range d.New {
		log.Printf("kubeagent: NEW %s", r.Key)
	}
	for _, r := range d.Resolved {
		log.Printf("kubeagent: RESOLVED %s (fired for %s)", r.Key, r.ResolvedAt.Sub(r.FiringSince).Round(time.Second))
	}
	for _, r := range d.NewlyFlapping {
		log.Printf("kubeagent: FLAPPING %s (%d firings in %s)", r.Key, r.RecentFirings, flapWindow)
	}
	log.Printf("kubeagent: cluster %s (%d/%d nodes ready) — %d issue(s) active, %d new, %d resolved",
		res.Health.Verdict, res.Health.NodesReady, res.Health.NodesTotal, active, len(d.New), len(d.Resolved))
}
```

- [ ] **Step 4: Run the full suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: PASS — every package, including `TestRun_GracefulShutdown` and
`internal/report`'s golden test (unchanged; `scan` output is untouched).

- [ ] **Step 5: Check formatting and commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l .    # must print nothing
git add internal/watch/watch.go internal/watch/watch_test.go
git commit -m "feat(watch): report issue transitions from tracked state"
```

---

### Task 6: documentation

**Files:**
- Modify: `website/docs/features/watch-mode.md`
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `website/docs/roadmap.md`

**Interfaces:**
- Consumes: the shipped behaviour from Tasks 1-5. Every metric name, log line, and JSON
  field documented here must be copied from the code, not from this plan — if they differ,
  the code wins and the discrepancy is a finding to report.

- [ ] **Step 1: Document the state, the metrics, and the endpoint in `website/docs/features/watch-mode.md`**

Add a section covering, in this order:

1. **What the daemon remembers** — issues are tracked per `(kind, namespace, name, issue)`;
   each reconcile reports transitions rather than the whole picture.
2. **Log output** — the four line shapes, copied verbatim from `logDelta`:
   `NEW <key>`, `RESOLVED <key> (fired for <dur>)`,
   `FLAPPING <key> (<n> firings in <window>)`, and the
   `cluster <verdict> (<r>/<t> nodes ready) — <n> issue(s) active, <n> new, <n> resolved`
   summary. State plainly that an unchanged reconcile logs nothing.
3. **New metrics** — a table naming all ten series: `kubeagent_issues_active`,
   `kubeagent_issues_flapping`, `kubeagent_issues_new_total`,
   `kubeagent_issues_resolved_total`, `kubeagent_issues_flapping_total`,
   `kubeagent_issues_dropped_total`, `kubeagent_issue_resolution_seconds_sum`,
   `kubeagent_issue_resolution_seconds_count`, and the two per-issue series
   `kubeagent_issue_active` / `kubeagent_issue_age_seconds` with their
   `{kind,namespace,name,issue}` labels. Note that MTTR is
   `kubeagent_issue_resolution_seconds_sum / kubeagent_issue_resolution_seconds_count`.
4. **`/issues`** — a `curl` example and the JSON shape with every field named
   (`firstSeen`, `firingSince`, `lastSeen`, `firings`, `flapping`, `ageSeconds`,
   `resolvedAt`, `resolutionSeconds`, `stats`), noting that `ageSeconds` appears only on
   active records and `resolvedAt`/`resolutionSeconds` only on resolved ones.
5. **Limits and restart semantics** — fixed defaults (500 tracked issues, 1h resolved
   retention, 30m flap window, 3 firings to flap), `kubeagent_issues_dropped_total` as the
   at-capacity signal, and the explicit statement that state is in-memory: after a restart
   everything currently firing is reported as `NEW` once, ages restart at zero, and the
   counters reset.
6. **Still read-only** — the tracker adds no API calls and no writes.

- [ ] **Step 2: Update the `watch` bullet in `README.md`**

Extend the existing `watch` description to say the daemon tracks issue state across
reconciles — new / resolved / flapping / MTTR — and serves the incident list at `/issues`
alongside `/metrics`, `/healthz`, and `/readyz`.

- [ ] **Step 3: Add the `CHANGELOG.md` entry under `## [Unreleased]` → `### Added`**

One entry naming: stateful `watch`; transition logging (`NEW`/`RESOLVED`/`FLAPPING`) with
steady state silent; the new issue metrics including MTTR and flap counters; the read-only
`/issues` JSON endpoint; and the in-memory restart caveat. No version header — the release
script moves `[Unreleased]` into place.

- [ ] **Step 4: Mark Theme E slice 1 in `website/docs/roadmap.md`**

Update the Theme E line so the stateful-`watch` piece reads as shipped, leaving alerting,
SLO burn-rate, and the multi-cluster hub as the remaining slices.

- [ ] **Step 5: Verify the docs build and commit**

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
(cd website && mkdocs build --strict -f mkdocs.yml)   # "Documentation built", no page warnings
git add website/docs/features/watch-mode.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "docs: document stateful watch state, metrics, and /issues"
```

---

## Release (after the whole-branch review passes)

- **Gate:** touches the watch daemon → **FULL CHAOS GATE**:
  `unset ANTHROPIC_API_KEY && ./chaos/run.sh --recreate`. Beyond the standing scenarios,
  read the daemon output: injecting an outage must produce a `NEW` line and repairing it a
  `RESOLVED` line, with `/issues` listing the incident while it fires.
- **Version:** minor **v0.54.0 → v0.55.0**.
- **Chart:** **PATCH** — no RBAC, template, or values change.
- **Golden snapshot:** unchanged. If `internal/report/golden_test.go` fails, something
  reached `scan` that should not have.
