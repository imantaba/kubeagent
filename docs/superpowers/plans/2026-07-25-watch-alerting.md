# Watch Alerting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route watch-daemon issue transitions to an operator-configured webhook (generic JSON, Slack, or Alertmanager) as one alert per broken object, so an operator gets paged without polling `/issues`.

**Architecture:** A new pure package `internal/alertstate` rolls `watchstate`'s per-issue records up to one alert per `(Kind, Namespace, Name)` and decides what to send (new / changed / repeat / resolved), with the clock injected. A new I/O package `internal/alert` owns three pure encoders plus a bounded-queue sender goroutine that POSTs with retries and keeps delivery counters. `internal/watch` wires them together behind a nil-able `*alerter`; alerting is off unless `KUBEAGENT_ALERT_WEBHOOK` is set.

**Tech Stack:** Go 1.26, standard library only (`net/http`, `encoding/json`, `net/url`, `sync`, `time`). No new dependencies. Tests use `net/http/httptest` and table-driven fixtures — no cluster.

## Global Constraints

- Design source of truth: `docs/superpowers/specs/2026-07-25-watch-alerting-design.md`. Where this plan and the spec disagree, stop and ask.
- The watch daemon stays **strictly read-only toward the cluster**: get/list/watch only, no writes, no RBAC changes. The alert sink's only egress is the operator-configured URL.
- **No LLM anywhere in the watch path.**
- **The webhook URL is a credential and must never be logged**, not in the startup banner, not in an error, not in a wrapped `*url.Error`. Only `scheme://host` may appear.
- The URL comes from the environment variable `KUBEAGENT_ALERT_WEBHOOK` only. There is **no `--alert-webhook` flag**. Unset = alerting off, which stays the default for every existing deployment.
- **No severity field or label** in any payload. kubeagent has no severity model; do not invent one.
- `internal/alertstate` is **pure**: no I/O, no goroutines, and no wall-clock reads — the caller passes `now`. It must never call `time.Now()`.
- A failed evaluation must never reach the tracker or produce a notification (the existing `applyResult` invariant).
- TDD: write the failing test, run it, watch it fail, then implement.
- Every commit must leave `go build ./...`, `go test ./...`, and `gofmt -l .` clean. `gofmt -l .` must print nothing.
- Go lives at `/usr/local/go/bin`: `export PATH=$PATH:/usr/local/go/bin`.
- **No `Co-Authored-By: Claude` trailer** and no Claude/AI attribution in any commit message, code comment, or doc.
- Fixed implementation constants, not flags: queue capacity 64, HTTP timeout 10s, 3 attempts, 1s/2s backoff between them.
- Format defaults for the re-send interval: `4h` for `json` and `slack`, `60s` for `alertmanager`. `--alert-repeat` greater than `4m` with the `alertmanager` format is a startup error.

## File Structure

| File | Responsibility |
|------|----------------|
| `internal/alertstate/alertstate.go` (create) | `Object`, `Status`, `Reason`, `Notification`, `Options`, `Roller`, `Roll`. Pure rollup and transition decisions. |
| `internal/alertstate/alertstate_test.go` (create) | Table tests on a fake clock, including the evolving-failure case. |
| `internal/alert/encode.go` (create) | The three pure encoders and the format type. |
| `internal/alert/encode_test.go` (create) | Exact-JSON assertions per format, firing and resolved. |
| `internal/alert/url.go` (create) | `RedactURL`, `resolveURL`, `sanitizeErr` — everything that keeps the credential out of logs. |
| `internal/alert/url_test.go` (create) | Redaction and URL-resolution tests. |
| `internal/alert/sink.go` (create) | `Config`, `Stats`, `Sink`: validation, bounded queue, sender goroutine, retry, counters. |
| `internal/alert/sink_test.go` (create) | `httptest` delivery, retry, 4xx, queue-full, stats. |
| `internal/watch/watch.go` (modify) | `Config` alert fields, the nil-able `alerter`, sink lifecycle, `applyResult` wiring. |
| `internal/watch/metrics.go` (modify) | `updateAlerts` + three new rendered series. |
| `main.go` (modify) | `--alert-format` / `--alert-repeat` flags, env read, usage string. |
| `deploy/helm/kubeagent/values.yaml`, `templates/deployment.yaml` (modify) | `alerts.*` block wired via `secretKeyRef`. |
| `deploy/deployment.yaml`, `deploy/README.md` (modify) | Commented example + docs for the raw manifest. |
| `chaos/alert-receiver.py` (create), `chaos/run.sh` (modify) | Receiver for the chaos gate; scenario 12 asserts one firing across the failure-mode walk. |
| `website/docs/features/watch-mode.md`, `website/docs/roadmap.md`, `docs/go-concepts.md`, `CHANGELOG.md` (modify) | Documentation. |

---

### Task 1: `internal/alertstate` — pure per-object rollup

**Files:**

- Create: `internal/alertstate/alertstate.go`
- Test: `internal/alertstate/alertstate_test.go`

**Interfaces:**

- Consumes: `watchstate.Record` and `watchstate.Key` from `github.com/imantaba/kubeagent/internal/watchstate` (fields used: `Key.Kind`, `Key.Namespace`, `Key.Name`, `Key.Issue`, `Record.FiringSince`, `Record.Flapping`).
- Produces, for Tasks 2–4:
  - `type Object struct{ Kind, Namespace, Name string }` with `func (o Object) String() string`
  - `type Status string` with `StatusFiring Status = "firing"`, `StatusResolved Status = "resolved"`
  - `type Reason string` with `ReasonNew = "new"`, `ReasonChanged = "changed"`, `ReasonRepeat = "repeat"`, `ReasonResolved = "resolved"`
  - `type Notification struct { Object Object; Status Status; Issues []string; FiringSince time.Time; ResolvedAt time.Time; Flapping bool; Reason Reason }`
  - `type Options struct{ Repeat time.Duration }`
  - `func New(o Options) *Roller`
  - `func (r *Roller) Roll(active []watchstate.Record, now time.Time) []Notification`

- [ ] **Step 1: Write the failing test**

Create `internal/alertstate/alertstate_test.go`:

```go
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
	r := New(Options{Repeat: time.Hour})
	got := r.Roll([]watchstate.Record{
		rec("Node", "", "worker-2", "KubeletUnhealthy", base),
		rec("Deployment", "shop", "web", "Degraded", base),
		rec("Deployment", "api", "gateway", "Degraded", base),
	}, base)
	want := []string{"Deployment/api/gateway", "Deployment/shop/web", "Node/worker-2"}
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
	if len(next) != 1 || next[0].Status != StatusResolved || next[0].Object.String() != "Deployment/api/gateway" {
		t.Fatalf("got %+v, want only Deployment/api/gateway resolved", next)
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
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/alertstate/
```

Expected: FAIL — the package does not compile (`undefined: New`, `undefined: Options`, …).

- [ ] **Step 3: Write the implementation**

Create `internal/alertstate/alertstate.go`:

```go
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
		Issues:      g.issues,
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
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/alertstate/ -v
```

Expected: PASS for all seven tests.

- [ ] **Step 5: Verify formatting and the whole suite**

```bash
gofmt -l . && go build ./... && go test ./...
```

Expected: `gofmt -l .` prints nothing; build and all tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/alertstate/
git commit -m "feat(alertstate): roll per-issue tracking up to one alert per object"
```

---

### Task 2: `internal/alert` — encoders and URL safety

**Files:**

- Create: `internal/alert/encode.go`, `internal/alert/url.go`
- Test: `internal/alert/encode_test.go`, `internal/alert/url_test.go`

**Interfaces:**

- Consumes: `alertstate.Notification`, `alertstate.StatusFiring`, `alertstate.StatusResolved` from Task 1.
- Produces, for Tasks 3–5:
  - `type Format string` with `FormatJSON Format = "json"`, `FormatSlack Format = "slack"`, `FormatAlertmanager Format = "alertmanager"`
  - `func encode(f Format, n alertstate.Notification) ([]byte, error)`
  - `func RedactURL(raw string) string` — exported; the daemon logs it
  - `func resolveURL(raw string, f Format) (string, error)`
  - `func sanitizeErr(err error) error`

- [ ] **Step 1: Write the failing encoder test**

Create `internal/alert/encode_test.go`:

```go
package alert

import (
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

var (
	at      = time.Date(2026, 7, 25, 10, 4, 11, 0, time.UTC)
	cleared = time.Date(2026, 7, 25, 10, 8, 23, 0, time.UTC)

	firingNotif = alertstate.Notification{
		Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonChanged,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: at,
	}
	resolvedNotif = alertstate.Notification{
		Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusResolved,
		Reason:      alertstate.ReasonResolved,
		FiringSince: at,
		ResolvedAt:  cleared,
	}
)

func TestEncode(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		notif  alertstate.Notification
		want   string
	}{
		{
			name:   "json firing",
			format: FormatJSON,
			notif:  firingNotif,
			want:   `{"status":"firing","reason":"changed","kind":"Deployment","namespace":"shop","name":"web","issues":["ImagePullBackOff"],"firingSince":"2026-07-25T10:04:11Z","flapping":false}`,
		},
		{
			name:   "json resolved carries an empty issue list, never null",
			format: FormatJSON,
			notif:  resolvedNotif,
			want:   `{"status":"resolved","reason":"resolved","kind":"Deployment","namespace":"shop","name":"web","issues":[],"firingSince":"2026-07-25T10:04:11Z","resolvedAt":"2026-07-25T10:08:23Z","flapping":false}`,
		},
		{
			name:   "slack firing",
			format: FormatSlack,
			notif:  firingNotif,
			want:   `{"text":"*FIRING* Deployment/shop/web\nissues: ImagePullBackOff\nfiring since 2026-07-25T10:04:11Z"}`,
		},
		{
			name:   "slack resolved reports the duration",
			format: FormatSlack,
			notif:  resolvedNotif,
			want:   `{"text":"*RESOLVED* Deployment/shop/web (fired for 4m12s)"}`,
		},
		{
			name:   "alertmanager firing omits endsAt and keeps issues in annotations",
			format: FormatAlertmanager,
			notif:  firingNotif,
			want:   `[{"labels":{"alertname":"KubeagentIssue","kind":"Deployment","name":"web","namespace":"shop"},"annotations":{"flapping":"false","issues":"ImagePullBackOff"},"startsAt":"2026-07-25T10:04:11Z"}]`,
		},
		{
			name:   "alertmanager resolved sets endsAt",
			format: FormatAlertmanager,
			notif:  resolvedNotif,
			want:   `[{"labels":{"alertname":"KubeagentIssue","kind":"Deployment","name":"web","namespace":"shop"},"annotations":{"flapping":"false","issues":""},"startsAt":"2026-07-25T10:04:11Z","endsAt":"2026-07-25T10:08:23Z"}]`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encode(tc.format, tc.notif)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("encode =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

func TestEncode_SlackFlaggingAndClusterScope(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Kind: "Node", Name: "worker-2"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"KubeletUnhealthy", "NotReady"},
		FiringSince: at,
		Flapping:    true,
	}
	got, err := encode(FormatSlack, n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"text":"*FIRING* Node/worker-2\nissues: KubeletUnhealthy, NotReady\nfiring since 2026-07-25T10:04:11Z (flapping)"}`
	if string(got) != want {
		t.Errorf("encode =\n  %s\nwant\n  %s", got, want)
	}
}

func TestEncode_ClusterScopedAlertmanagerOmitsNamespaceLabel(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Kind: "Node", Name: "worker-2"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"KubeletUnhealthy"},
		FiringSince: at,
	}
	got, err := encode(FormatAlertmanager, n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `[{"labels":{"alertname":"KubeagentIssue","kind":"Node","name":"worker-2"},"annotations":{"flapping":"false","issues":"KubeletUnhealthy"},"startsAt":"2026-07-25T10:04:11Z"}]`
	if string(got) != want {
		t.Errorf("encode =\n  %s\nwant\n  %s", got, want)
	}
}

func TestEncode_UnknownFormatErrors(t *testing.T) {
	if _, err := encode(Format("teletype"), firingNotif); err == nil {
		t.Fatal("encode with an unknown format must error")
	}
}
```

- [ ] **Step 2: Write the failing URL-safety test**

Create `internal/alert/url_test.go`:

```go
package alert

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// The webhook URL is a credential: a Slack incoming-webhook URL is a bearer token
// in URL form. Nothing but scheme://host may ever reach a log line.
const slackish = "https://hooks.slack.example/services/T00000000/B00000000/abcdefghijklmnopqrstuvwx"

func TestRedactURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{slackish, "https://hooks.slack.example"},
		{"http://alertmanager.monitoring:9093/api/v2/alerts", "http://alertmanager.monitoring:9093"},
		{"https://user:secret@example.test/hook?token=abc", "https://example.test"},
		{"not a url at all", "(redacted)"},
		{"", "(redacted)"},
	}
	for _, tc := range tests {
		if got := RedactURL(tc.in); got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveURL(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		format Format
		want   string
	}{
		{"json passes through", "https://example.test/hook", FormatJSON, "https://example.test/hook"},
		{"alertmanager bare host gains the v2 path", "http://alertmanager:9093", FormatAlertmanager, "http://alertmanager:9093/api/v2/alerts"},
		{"alertmanager root path gains the v2 path", "http://alertmanager:9093/", FormatAlertmanager, "http://alertmanager:9093/api/v2/alerts"},
		{"alertmanager explicit path is respected", "http://alertmanager:9093/custom/alerts", FormatAlertmanager, "http://alertmanager:9093/custom/alerts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveURL(tc.in, tc.format)
			if err != nil {
				t.Fatalf("resolveURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveURL_ErrorsNeverEchoTheURL(t *testing.T) {
	bad := []string{"", "://nope", "ftp://example.test/hook", "/no-scheme"}
	for _, in := range bad {
		_, err := resolveURL(in, FormatJSON)
		if err == nil {
			t.Errorf("resolveURL(%q) must error", in)
			continue
		}
		if in != "" && strings.Contains(err.Error(), in) {
			t.Errorf("resolveURL(%q) error echoes the URL: %v", in, err)
		}
	}
	_, err := resolveURL(slackish+"\x7f", FormatJSON)
	if err != nil && strings.Contains(err.Error(), "abcdefghijklmnopqrstuvwx") {
		t.Errorf("error leaked the webhook token: %v", err)
	}
}

func TestSanitizeErr_StripsTheURLNetHTTPEmbeds(t *testing.T) {
	inner := errors.New("connection refused")
	err := sanitizeErr(&url.Error{Op: "Post", URL: slackish, Err: inner})
	if strings.Contains(err.Error(), "slack") {
		t.Errorf("sanitizeErr left the URL in place: %v", err)
	}
	if !errors.Is(err, inner) {
		t.Errorf("sanitizeErr dropped the underlying error: %v", err)
	}
	other := errors.New("plain")
	if got := sanitizeErr(other); got != other {
		t.Errorf("sanitizeErr changed a non-url.Error: %v", got)
	}
}
```

- [ ] **Step 3: Run both tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/alert/
```

Expected: FAIL — the package does not compile (`undefined: encode`, `undefined: RedactURL`, …).

- [ ] **Step 4: Write `internal/alert/url.go`**

```go
// Package alert delivers watch notifications to an operator-configured HTTP
// endpoint. It is the daemon's only egress besides the Kubernetes API and it
// never writes to the cluster.
//
// The destination URL is treated as a credential throughout: a Slack incoming-
// webhook URL is a bearer token in URL form, so nothing in this package logs or
// returns more than scheme://host.
package alert

import (
	"errors"
	"net/url"
)

// RedactURL reduces a webhook URL to scheme://host, dropping the path, query, and
// userinfo. Every log line that names the destination goes through it.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "(redacted)"
	}
	return u.Scheme + "://" + u.Host
}

// resolveURL validates the destination and, for the alertmanager format, fills in
// the v2 alerts path when the URL carries none. Its errors never echo the input:
// url.Parse's own error text embeds the URL, so it is deliberately not wrapped.
func resolveURL(raw string, f Format) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("alert webhook URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("alert webhook URL must use http or https")
	}
	if u.Host == "" {
		return "", errors.New("alert webhook URL has no host")
	}
	if f == FormatAlertmanager && (u.Path == "" || u.Path == "/") {
		u.Path = "/api/v2/alerts"
	}
	return u.String(), nil
}

// sanitizeErr strips the URL that net/http embeds in *url.Error, so a webhook
// credential can never reach a log line through a delivery failure.
func sanitizeErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
```

- [ ] **Step 5: Write `internal/alert/encode.go`**

```go
package alert

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

// Format is the wire format the receiver expects.
type Format string

const (
	FormatJSON         Format = "json"
	FormatSlack        Format = "slack"
	FormatAlertmanager Format = "alertmanager"
)

// encode renders one notification in the configured format.
func encode(f Format, n alertstate.Notification) ([]byte, error) {
	switch f {
	case FormatJSON:
		return encodeJSON(n)
	case FormatSlack:
		return encodeSlack(n)
	case FormatAlertmanager:
		return encodeAlertmanager(n)
	default:
		return nil, fmt.Errorf("unknown alert format %q (want json, slack, or alertmanager)", f)
	}
}

// jsonPayload is kubeagent's native alert body.
type jsonPayload struct {
	Status      string   `json:"status"`
	Reason      string   `json:"reason"`
	Kind        string   `json:"kind"`
	Namespace   string   `json:"namespace,omitempty"`
	Name        string   `json:"name"`
	Issues      []string `json:"issues"`
	FiringSince string   `json:"firingSince"`
	ResolvedAt  string   `json:"resolvedAt,omitempty"`
	Flapping    bool     `json:"flapping"`
}

func encodeJSON(n alertstate.Notification) ([]byte, error) {
	issues := n.Issues
	if issues == nil {
		issues = []string{}
	}
	p := jsonPayload{
		Status:      string(n.Status),
		Reason:      string(n.Reason),
		Kind:        n.Object.Kind,
		Namespace:   n.Object.Namespace,
		Name:        n.Object.Name,
		Issues:      issues,
		FiringSince: n.FiringSince.UTC().Format(time.RFC3339),
		Flapping:    n.Flapping,
	}
	if !n.ResolvedAt.IsZero() {
		p.ResolvedAt = n.ResolvedAt.UTC().Format(time.RFC3339)
	}
	return json.Marshal(p)
}

func encodeSlack(n alertstate.Notification) ([]byte, error) {
	var text string
	if n.Status == alertstate.StatusResolved {
		text = fmt.Sprintf("*RESOLVED* %s (fired for %s)", n.Object, n.ResolvedAt.Sub(n.FiringSince).Round(time.Second))
	} else {
		text = fmt.Sprintf("*FIRING* %s\nissues: %s\nfiring since %s",
			n.Object, strings.Join(n.Issues, ", "), n.FiringSince.UTC().Format(time.RFC3339))
		if n.Flapping {
			text += " (flapping)"
		}
	}
	return json.Marshal(struct {
		Text string `json:"text"`
	}{text})
}

// amAlert is one entry of Alertmanager's POST /api/v2/alerts array.
type amAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt,omitempty"`
}

// encodeAlertmanager keeps the issue list in an annotation rather than a label:
// the set changes as a failure evolves, and a changing label value would create a
// second Alertmanager alert instead of updating the open one — reintroducing the
// false-recovery problem the per-object rollup exists to prevent.
func encodeAlertmanager(n alertstate.Notification) ([]byte, error) {
	labels := map[string]string{
		"alertname": "KubeagentIssue",
		"kind":      n.Object.Kind,
		"name":      n.Object.Name,
	}
	if n.Object.Namespace != "" {
		labels["namespace"] = n.Object.Namespace
	}
	a := amAlert{
		Labels: labels,
		Annotations: map[string]string{
			"issues":   strings.Join(n.Issues, ","),
			"flapping": fmt.Sprintf("%t", n.Flapping),
		},
		StartsAt: n.FiringSince.UTC().Format(time.RFC3339),
	}
	if n.Status == alertstate.StatusResolved {
		a.EndsAt = n.ResolvedAt.UTC().Format(time.RFC3339)
	}
	return json.Marshal([]amAlert{a})
}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
go test ./internal/alert/ -v
```

Expected: PASS. (`encoding/json` sorts map keys, which is why the expected label and annotation JSON is alphabetical.)

- [ ] **Step 7: Verify formatting and the whole suite**

```bash
gofmt -l . && go build ./... && go test ./...
```

Expected: `gofmt -l .` prints nothing; build and all tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/alert/
git commit -m "feat(alert): add json, slack, and alertmanager encoders with URL redaction"
```

---

### Task 3: `internal/alert` — bounded-queue sender

**Files:**

- Create: `internal/alert/sink.go`
- Test: `internal/alert/sink_test.go`

**Interfaces:**

- Consumes: `encode`, `resolveURL`, `RedactURL`, `sanitizeErr`, `Format` and its constants from Task 2; `alertstate.Notification` and `alertstate.StatusResolved` from Task 1.
- Produces, for Tasks 4–5:
  - `type Config struct { URL string; Format Format; Repeat time.Duration }`
  - `type Stats struct { FiringOK, FiringFailed, ResolvedOK, ResolvedFailed, DroppedQueueFull, DroppedRetriesExhausted, LastSuccessUnix int64 }`
  - `func New(cfg Config, c *http.Client) (*Sink, error)`
  - `func DefaultRepeat(f Format) time.Duration` — `time.Minute` for `FormatAlertmanager`, `4 * time.Hour` otherwise
  - `func (s *Sink) Start(ctx context.Context)`
  - `func (s *Sink) Enqueue(n alertstate.Notification)`
  - `func (s *Sink) Stats() Stats`
  - `func (s *Sink) Close()`

- [ ] **Step 1: Write the failing test**

Create `internal/alert/sink_test.go`:

```go
package alert

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSink_DeliversAndCounts(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := New(Config{URL: srv.URL, Format: FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	s.Enqueue(firingNotif)
	s.Enqueue(resolvedNotif)
	waitFor(t, "two deliveries", func() bool {
		st := s.Stats()
		return st.FiringOK == 1 && st.ResolvedOK == 1
	})
	cancel()
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"status":"firing"`) {
		t.Fatalf("bodies = %v", bodies)
	}
	if st := s.Stats(); st.LastSuccessUnix == 0 {
		t.Error("LastSuccessUnix must be set after a successful delivery")
	}
}

func TestSink_RetriesServerErrorsThenSucceeds(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := New(Config{URL: srv.URL, Format: FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.backoffBase = time.Millisecond // keep the test fast
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	s.Enqueue(firingNotif)
	waitFor(t, "delivery after retries", func() bool { return s.Stats().FiringOK == 1 })
	cancel()
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Errorf("server saw %d calls, want 3", calls)
	}
	if st := s.Stats(); st.DroppedRetriesExhausted != 0 {
		t.Errorf("DroppedRetriesExhausted = %d, want 0", st.DroppedRetriesExhausted)
	}
}

func TestSink_GivesUpAfterThreeAttempts(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	s, err := New(Config{URL: srv.URL, Format: FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.backoffBase = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	s.Enqueue(firingNotif)
	waitFor(t, "the sink to give up", func() bool { return s.Stats().DroppedRetriesExhausted == 1 })
	cancel()
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Errorf("server saw %d calls, want 3", calls)
	}
	if st := s.Stats(); st.FiringFailed != 1 || st.FiringOK != 0 {
		t.Errorf("stats = %+v, want one failed firing", st)
	}
}

func TestSink_ClientErrorIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s, err := New(Config{URL: srv.URL, Format: FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.backoffBase = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	s.Enqueue(firingNotif)
	waitFor(t, "the failure to be counted", func() bool { return s.Stats().FiringFailed == 1 })
	cancel()
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("server saw %d calls, want 1 — a 4xx will not fix itself", calls)
	}
}

func TestSink_QueueFullDropsAndCounts(t *testing.T) {
	// No Start: nothing drains the queue, so it fills at exactly queueSize.
	s, err := New(Config{URL: "https://example.test/hook", Format: FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < queueSize+5; i++ {
		s.Enqueue(firingNotif)
	}
	if got := s.Stats().DroppedQueueFull; got != 5 {
		t.Errorf("DroppedQueueFull = %d, want 5", got)
	}
	if got := len(s.queue); got != queueSize {
		t.Errorf("queue depth = %d, want %d", got, queueSize)
	}
}

func TestNew_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"good json", Config{URL: "https://example.test/hook", Format: FormatJSON, Repeat: time.Hour}, false},
		{"unknown format", Config{URL: "https://example.test/hook", Format: "teletype", Repeat: time.Hour}, true},
		{"bad url", Config{URL: "nope", Format: FormatJSON, Repeat: time.Hour}, true},
		{"alertmanager within the cadence limit", Config{URL: "http://am:9093", Format: FormatAlertmanager, Repeat: time.Minute}, false},
		{"alertmanager repeat too long", Config{URL: "http://am:9093", Format: FormatAlertmanager, Repeat: 4 * time.Hour}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, nil)
			if tc.wantErr != (err != nil) {
				t.Fatalf("New error = %v, wantErr %v", err, tc.wantErr)
			}
			// "webhook" legitimately appears in these messages; what must never
			// appear is the URL itself.
			if err != nil && tc.cfg.URL != "" && strings.Contains(err.Error(), tc.cfg.URL) {
				t.Errorf("validation error echoes the URL: %v", err)
			}
		})
	}
}

func TestDefaultRepeat(t *testing.T) {
	if got := DefaultRepeat(FormatAlertmanager); got != time.Minute {
		t.Errorf("DefaultRepeat(alertmanager) = %s, want 1m0s", got)
	}
	for _, f := range []Format{FormatJSON, FormatSlack} {
		if got := DefaultRepeat(f); got != 4*time.Hour {
			t.Errorf("DefaultRepeat(%s) = %s, want 4h0m0s", f, got)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/alert/ -run 'TestSink|TestNew_|TestDefaultRepeat'
```

Expected: FAIL — `undefined: New`, `undefined: queueSize`, `undefined: DefaultRepeat`.

- [ ] **Step 3: Write the implementation**

Create `internal/alert/sink.go`:

```go
package alert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

const (
	queueSize      = 64               // bounded: a slow receiver can never grow memory
	attempts       = 3                // then the notification is dropped
	httpTimeout    = 10 * time.Second // per attempt
	defaultBackoff = time.Second      // 1s, then 2s

	// maxAlertmanagerRepeat keeps the re-send interval inside Alertmanager's
	// default resolve_timeout (5m). A longer interval would let a still-firing
	// alert expire and then re-fire — a false recovery.
	maxAlertmanagerRepeat = 4 * time.Minute
)

// Config configures the sink. Repeat is used only to validate the cadence against
// the chosen format; the re-send itself is driven by alertstate.
type Config struct {
	URL    string
	Format Format
	Repeat time.Duration
}

// Stats are monotonic process-lifetime delivery counters.
type Stats struct {
	FiringOK                int64
	FiringFailed            int64
	ResolvedOK              int64
	ResolvedFailed          int64
	DroppedQueueFull        int64
	DroppedRetriesExhausted int64
	LastSuccessUnix         int64
}

// Sink POSTs notifications to one endpoint from a single background goroutine.
// Enqueue never blocks the caller, so a hung receiver cannot stall the daemon's
// reconcile loop.
type Sink struct {
	url         string
	format      Format
	client      *http.Client
	queue       chan alertstate.Notification
	done        chan struct{}
	backoffBase time.Duration

	mu    sync.Mutex
	stats Stats
}

// DefaultRepeat is the re-send interval for a format when the operator did not
// choose one. Alertmanager needs a short cadence because it expires an alert
// resolve_timeout after the last POST; json and slack are notification channels
// where a chatty default is alert fatigue.
func DefaultRepeat(f Format) time.Duration {
	if f == FormatAlertmanager {
		return time.Minute
	}
	return 4 * time.Hour
}

// New validates the configuration and returns a sink. Pass a nil client for the
// default (a 10s per-attempt timeout). Errors never echo the URL.
func New(cfg Config, c *http.Client) (*Sink, error) {
	switch cfg.Format {
	case FormatJSON, FormatSlack, FormatAlertmanager:
	default:
		return nil, fmt.Errorf("unknown alert format %q (want json, slack, or alertmanager)", cfg.Format)
	}
	if cfg.Format == FormatAlertmanager && cfg.Repeat > maxAlertmanagerRepeat {
		return nil, fmt.Errorf("alert repeat interval %s exceeds %s, the safe maximum for the alertmanager format (Alertmanager expires an alert resolve_timeout — 5m by default — after the last POST)", cfg.Repeat, maxAlertmanagerRepeat)
	}
	u, err := resolveURL(cfg.URL, cfg.Format)
	if err != nil {
		return nil, err
	}
	if c == nil {
		c = &http.Client{Timeout: httpTimeout}
	}
	return &Sink{
		url:         u,
		format:      cfg.Format,
		client:      c,
		queue:       make(chan alertstate.Notification, queueSize),
		done:        make(chan struct{}),
		backoffBase: defaultBackoff,
	}, nil
}

// Start launches the sender goroutine, which runs until ctx is cancelled.
func (s *Sink) Start(ctx context.Context) {
	go func() {
		defer close(s.done)
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-s.queue:
				s.deliver(ctx, n)
			}
		}
	}()
}

// Close waits for the sender goroutine to exit. The caller cancels the context
// passed to Start first.
func (s *Sink) Close() { <-s.done }

// Enqueue hands a notification to the sender without blocking. When the queue is
// full the oldest queued notification is dropped: the newest state is the useful
// one, and unbounded buffering is not an option in a long-lived daemon.
func (s *Sink) Enqueue(n alertstate.Notification) {
	select {
	case s.queue <- n:
		return
	default:
	}
	select {
	case dropped := <-s.queue:
		s.countDrop(dropped, "oldest")
	default:
	}
	select {
	case s.queue <- n:
	default:
		s.countDrop(n, "newest")
	}
}

func (s *Sink) countDrop(n alertstate.Notification, which string) {
	s.mu.Lock()
	s.stats.DroppedQueueFull++
	s.mu.Unlock()
	log.Printf("kubeagent: alert queue full, dropped the %s notification for %s", which, n.Object)
}

// Stats returns the delivery counters.
func (s *Sink) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// deliver encodes and POSTs one notification, retrying server-side failures.
func (s *Sink) deliver(ctx context.Context, n alertstate.Notification) {
	body, err := encode(s.format, n)
	if err != nil {
		log.Printf("kubeagent: encoding alert for %s: %v", n.Object, err)
		s.record(n, false)
		return
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		status, err := s.post(ctx, body)
		switch {
		case err == nil && status < 300:
			s.record(n, true)
			return
		case err == nil && status < 500:
			log.Printf("kubeagent: alert for %s rejected by %s: HTTP %d (not retrying)", n.Object, RedactURL(s.url), status)
			s.record(n, false)
			return
		case err != nil:
			log.Printf("kubeagent: alert delivery to %s failed (attempt %d/%d): %v", RedactURL(s.url), attempt, attempts, err)
		default:
			log.Printf("kubeagent: alert delivery to %s failed (attempt %d/%d): HTTP %d", RedactURL(s.url), attempt, attempts, status)
		}
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			s.record(n, false)
			return
		case <-time.After(s.backoffBase * time.Duration(1<<(attempt-1))):
		}
	}
	s.record(n, false)
	s.mu.Lock()
	s.stats.DroppedRetriesExhausted++
	s.mu.Unlock()
	log.Printf("kubeagent: dropping alert for %s after %d failed attempts", n.Object, attempts)
}

// post sends one attempt and returns the status code. Transport errors are
// sanitized: net/http embeds the full URL in *url.Error.
func (s *Sink) post(ctx context.Context, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("building the alert request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, sanitizeErr(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// record folds one delivery outcome into the counters.
func (s *Sink) record(n alertstate.Notification, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case n.Status == alertstate.StatusResolved && ok:
		s.stats.ResolvedOK++
	case n.Status == alertstate.StatusResolved:
		s.stats.ResolvedFailed++
	case ok:
		s.stats.FiringOK++
	default:
		s.stats.FiringFailed++
	}
	if ok {
		s.stats.LastSuccessUnix = time.Now().Unix()
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/alert/ -v -race
```

Expected: PASS, with no race warnings. (`-race` matters here: the sender goroutine writes the counters while the test reads them.)

- [ ] **Step 5: Verify formatting and the whole suite**

```bash
gofmt -l . && go build ./... && go test ./...
```

Expected: `gofmt -l .` prints nothing; build and all tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/alert/
git commit -m "feat(alert): add the bounded-queue sender with retries and delivery counters"
```

---

### Task 4: Wire alerting into the watch daemon

**Files:**

- Modify: `internal/watch/watch.go`, `internal/watch/metrics.go`
- Test: `internal/watch/watch_test.go` (extend), `internal/watch/metrics_test.go` (extend)

**Interfaces:**

- Consumes: `alertstate.New`, `alertstate.Options`, `alertstate.Roller`, `alertstate.Notification` (Task 1); `alert.New`, `alert.Config`, `alert.Format`, `alert.Stats`, `alert.RedactURL`, `alert.DefaultRepeat`, `alert.Sink` (Tasks 2–3).
- Produces, for Task 5: three new `watch.Config` fields — `AlertURL string`, `AlertFormat string`, `AlertRepeat time.Duration`.
- Changed signature: `applyResult(m *metrics, tr *watchstate.Tracker, al *alerter, res *scan.Result, dur time.Duration, now time.Time, err error)` — the existing two call sites in `internal/watch/watch_test.go` must pass `nil` for `al`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/watch/watch_test.go`:

```go
// TestAlerter_NilIsDisabled pins that alerting is off by default: a nil *alerter
// is the "no webhook configured" case and must be inert, not a panic.
func TestAlerter_NilIsDisabled(t *testing.T) {
	var al *alerter
	tr := watchstate.New(watchstate.Options{})
	tr.Observe([]watchstate.Key{{Kind: "Deployment", Namespace: "shop", Name: "web", Issue: "Degraded"}}, time.Now())
	al.notify(tr, time.Now()) // must not panic
	if got := al.stats(); got != (alert.Stats{}) {
		t.Errorf("nil alerter stats = %+v, want the zero value", got)
	}
}

// TestApplyResult_EvaluationErrorSendsNoAlert extends the tracker invariant to the
// outbound path: one API blip must never page the on-call.
func TestApplyResult_EvaluationErrorSendsNoAlert(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink, err := alert.New(alert.Config{URL: srv.URL, Format: alert.FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("alert.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink.Start(ctx)
	al := &alerter{roller: alertstate.New(alertstate.Options{Repeat: time.Hour}), sink: sink}

	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

	captureLog(t, func() {
		applyResult(m, tr, al, &scan.Result{}, time.Millisecond, at, errors.New("boom"))
	})
	cancel()
	sink.Close()

	mu.Lock()
	defer mu.Unlock()
	if posts != 0 {
		t.Errorf("a failed evaluation produced %d alert POSTs, want 0", posts)
	}
}

// TestApplyResult_AlertsOnRealFindings is the happy path: a successful evaluation
// with findings reaches the receiver.
func TestApplyResult_AlertsOnRealFindings(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink, err := alert.New(alert.Config{URL: srv.URL, Format: alert.FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("alert.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink.Start(ctx)
	al := &alerter{roller: alertstate.New(alertstate.Options{Repeat: time.Hour}), sink: sink}

	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	captureLog(t, func() { applyResult(m, tr, al, sampleResult(), time.Millisecond, at, nil) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sink.Stats().FiringOK > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	sink.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no alert reached the receiver")
	}
	if !strings.Contains(bodies[0], `"status":"firing"`) {
		t.Errorf("first body = %s, want a firing notification", bodies[0])
	}
}
```

Add the imports this needs to the existing `import` block of `internal/watch/watch_test.go`: `"io"`, `"net/http/httptest"`, `"sync"`, `"github.com/imantaba/kubeagent/internal/alert"`, `"github.com/imantaba/kubeagent/internal/alertstate"`. `context`, `errors`, `net/http`, `strings`, and `time` are already imported.

Append to `internal/watch/metrics_test.go`:

```go
// TestRender_AlertSeriesAlwaysPresent pins that the three alert series render even
// when alerting is disabled, so a dashboard does not break when it is switched on.
func TestRender_AlertSeriesAlwaysPresent(t *testing.T) {
	m := newMetrics()
	out := m.render()
	for _, want := range []string{
		`kubeagent_alerts_sent_total{status="firing",outcome="ok"} 0`,
		`kubeagent_alerts_sent_total{status="firing",outcome="failed"} 0`,
		`kubeagent_alerts_sent_total{status="resolved",outcome="ok"} 0`,
		`kubeagent_alerts_sent_total{status="resolved",outcome="failed"} 0`,
		`kubeagent_alerts_dropped_total{reason="queue_full"} 0`,
		`kubeagent_alerts_dropped_total{reason="retries_exhausted"} 0`,
		"kubeagent_alert_last_success_timestamp_seconds 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series %q", want)
		}
	}
}

func TestUpdateAlerts_RendersTheCounters(t *testing.T) {
	m := newMetrics()
	m.updateAlerts(alert.Stats{
		FiringOK: 3, FiringFailed: 1, ResolvedOK: 2, ResolvedFailed: 0,
		DroppedQueueFull: 7, DroppedRetriesExhausted: 4, LastSuccessUnix: 1770000000,
	})
	out := m.render()
	for _, want := range []string{
		`kubeagent_alerts_sent_total{status="firing",outcome="ok"} 3`,
		`kubeagent_alerts_sent_total{status="firing",outcome="failed"} 1`,
		`kubeagent_alerts_sent_total{status="resolved",outcome="ok"} 2`,
		`kubeagent_alerts_dropped_total{reason="queue_full"} 7`,
		`kubeagent_alerts_dropped_total{reason="retries_exhausted"} 4`,
		"kubeagent_alert_last_success_timestamp_seconds 1.77e+09",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series %q in:\n%s", want, out)
		}
	}
}
```

Add `"github.com/imantaba/kubeagent/internal/alert"` to that file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/
```

Expected: FAIL — `undefined: alerter`, `undefined: updateAlerts`, plus a signature error on the existing `applyResult` calls once `alerter` exists.

- [ ] **Step 3: Add the alert fields and the alerter to `internal/watch/watch.go`**

Add to the `import` block:

```go
	"github.com/imantaba/kubeagent/internal/alert"
	"github.com/imantaba/kubeagent/internal/alertstate"
```

Add three fields at the end of `Config`:

```go
	AlertURL                string        // empty disables alerting entirely
	AlertFormat             string        // "json" | "slack" | "alertmanager"
	AlertRepeat             time.Duration // re-send interval for still-firing alerts
```

Add the `alerter` type after `scopeLabel`:

```go
// alerter routes tracker state to the outbound sink. A nil *alerter means no
// webhook is configured, which is the default: every method is a no-op, so the
// reconcile loop needs no conditional.
type alerter struct {
	roller *alertstate.Roller
	sink   *alert.Sink
}

// notify rolls the tracker's active issues up to per-object alerts and hands the
// resulting notifications to the sink. Enqueue never blocks.
func (a *alerter) notify(tr *watchstate.Tracker, now time.Time) {
	if a == nil {
		return
	}
	for _, n := range a.roller.Roll(tr.Active(), now) {
		a.sink.Enqueue(n)
	}
}

// stats returns the sink's delivery counters, or the zero value when alerting is off.
func (a *alerter) stats() alert.Stats {
	if a == nil {
		return alert.Stats{}
	}
	return a.sink.Stats()
}

// newAlerter builds the alerter from the config, returning nil when no webhook is
// configured. The URL is a credential: only scheme://host is ever logged.
func newAlerter(ctx context.Context, cfg Config) (*alerter, error) {
	if cfg.AlertURL == "" {
		return nil, nil
	}
	format := alert.Format(cfg.AlertFormat)
	sink, err := alert.New(alert.Config{URL: cfg.AlertURL, Format: format, Repeat: cfg.AlertRepeat}, nil)
	if err != nil {
		return nil, err
	}
	sink.Start(ctx)
	log.Printf("kubeagent: alerting enabled (format=%s, repeat=%s, endpoint=%s)", format, cfg.AlertRepeat, alert.RedactURL(cfg.AlertURL))
	return &alerter{roller: alertstate.New(alertstate.Options{Repeat: cfg.AlertRepeat}), sink: sink}, nil
}
```

- [ ] **Step 4: Wire it into `Run` and `applyResult`**

In `Run`, immediately after `opts := scan.Options{…}` and before `tr := watchstate.New(…)`:

```go
	al, err := newAlerter(ctx, cfg)
	if err != nil {
		return err
	}
	if al != nil {
		defer al.sink.Close()
	}
```

Change the `reconcile` closure to pass the alerter:

```go
	reconcile := func() {
		start := time.Now()
		res, err := scan.Evaluate(ctx, client, opts)
		applyResult(m, tr, al, &res, time.Since(start), time.Now(), err)
	}
```

Replace `applyResult` with:

```go
// applyResult folds one evaluation into the metrics, the issue tracker, and the
// outbound alerter, and logs whatever changed. A failed evaluation never reaches
// the tracker: an evaluation error is not "all clear", and treating it as one
// would resolve every tracked issue, then re-fire them all on the next success —
// and, now that alerting exists, page the on-call for an API blip.
func applyResult(m *metrics, tr *watchstate.Tracker, al *alerter, res *scan.Result, dur time.Duration, now time.Time, err error) {
	m.update(res, dur, now, err)
	if err != nil {
		log.Printf("kubeagent: evaluation error: %v", err)
		return
	}
	d := tr.Observe(issueKeys(res), now)
	al.notify(tr, now)
	m.updateIssues(tr, now)
	m.updateAlerts(al.stats())
	logDelta(res, d, len(tr.Active()), tr.FlapWindow())
}
```

Update the two existing `applyResult(m, tr, …)` calls in `internal/watch/watch_test.go` to pass `nil` as the third argument.

- [ ] **Step 5: Render the alert metrics in `internal/watch/metrics.go`**

Add `"github.com/imantaba/kubeagent/internal/alert"` to the imports, add a field to the `metrics` struct after `issues issueSnapshot`:

```go
	alerts                alert.Stats
```

Add the setter next to `updateIssues`:

```go
// updateAlerts records the sink's delivery counters for rendering.
func (m *metrics) updateAlerts(s alert.Stats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts = s
}
```

In `render`, immediately before the `gauge("kubeagent_last_scan_timestamp_seconds", …)` line:

```go
	fmt.Fprintf(&b, "# HELP kubeagent_alerts_sent_total Alert notifications delivered since start\n# TYPE kubeagent_alerts_sent_total counter\n")
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "firing", "ok", m.alerts.FiringOK)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "firing", "failed", m.alerts.FiringFailed)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "resolved", "ok", m.alerts.ResolvedOK)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "resolved", "failed", m.alerts.ResolvedFailed)
	fmt.Fprintf(&b, "# HELP kubeagent_alerts_dropped_total Alert notifications dropped without delivery\n# TYPE kubeagent_alerts_dropped_total counter\n")
	fmt.Fprintf(&b, "kubeagent_alerts_dropped_total{reason=%q} %d\n", "queue_full", m.alerts.DroppedQueueFull)
	fmt.Fprintf(&b, "kubeagent_alerts_dropped_total{reason=%q} %d\n", "retries_exhausted", m.alerts.DroppedRetriesExhausted)
	gauge("kubeagent_alert_last_success_timestamp_seconds", "Unix time of the last successful alert delivery (0 if none)", float64(m.alerts.LastSuccessUnix))
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
go test ./internal/watch/ -v -race
```

Expected: PASS for the whole package, including the pre-existing tests.

- [ ] **Step 7: Verify formatting and the whole suite**

```bash
gofmt -l . && go build ./... && go test ./...
```

Expected: `gofmt -l .` prints nothing; build and all tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/watch/
git commit -m "feat(watch): route per-object alerts to the outbound sink"
```

---

### Task 5: CLI flags and environment wiring

**Files:**

- Modify: `main.go` (the `runWatch` function around line 302, and the usage string at line 62)

**Interfaces:**

- Consumes: `watch.Config.AlertURL/AlertFormat/AlertRepeat` (Task 4); `alert.DefaultRepeat` (Task 3).
- Produces: no new exported API — this is the user-facing surface.

- [ ] **Step 1: Add the flags and env wiring**

In `runWatch`, after the `includeRestarts` flag declaration:

```go
	alertFormat := fs.String("alert-format", envOr("KUBEAGENT_ALERT_FORMAT", "json"), "alert payload format: json, slack, or alertmanager")
	alertRepeat := fs.Duration("alert-repeat", envDur("KUBEAGENT_ALERT_REPEAT", 0), "re-send interval for still-firing alerts (0 = the format default: 4h, or 60s for alertmanager)")
```

After `fs.Parse` succeeds, resolve the webhook and the effective cadence:

```go
	// The webhook URL is a credential (a Slack incoming-webhook URL is a bearer
	// token in URL form), so it comes from the environment only — never a flag,
	// which would put it in the pod spec's args and in `ps` output.
	alertURL := os.Getenv("KUBEAGENT_ALERT_WEBHOOK")
	repeat := *alertRepeat
	if repeat == 0 {
		repeat = alert.DefaultRepeat(alert.Format(*alertFormat))
	}
	if alertURL == "" && (*alertFormat != "json" || *alertRepeat != 0) {
		fmt.Fprintln(os.Stderr, "kubeagent: --alert-* flags ignored: KUBEAGENT_ALERT_WEBHOOK is not set, so alerting is off")
	}
```

Add the three fields to the `watch.Config` literal:

```go
		AlertURL:                alertURL,
		AlertFormat:             *alertFormat,
		AlertRepeat:             repeat,
```

Add the import `"github.com/imantaba/kubeagent/internal/alert"` to `main.go`.

- [ ] **Step 2: Update the usage string**

In the `usage:` error string in `run` (the long `fmt.Errorf` listing both subcommands), replace the watch clause

```
kubeagent watch [--kubeconfig path] [--context name] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur]
```

with

```
kubeagent watch [--kubeconfig path] [--context name] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur] [--alert-format json|slack|alertmanager] [--alert-repeat dur]
```

- [ ] **Step 3: Verify the flags are wired and alerting stays off by default**

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o kubeagent . && ./kubeagent watch --help 2>&1 | grep -E 'alert-(format|repeat)'
```

Expected: both flags listed, and **no** `--alert-webhook` flag anywhere in the output.

```bash
./kubeagent watch --alert-format slack --kubeconfig /nonexistent 2>&1 | head -2
```

Expected: the "flags ignored: KUBEAGENT_ALERT_WEBHOOK is not set" warning appears before the kubeconfig failure.

```bash
KUBEAGENT_ALERT_WEBHOOK=http://127.0.0.1:1/hook ./kubeagent watch --alert-format alertmanager --alert-repeat 4h --kubeconfig /nonexistent 2>&1 | head -3
```

Expected: an error mentioning the 4m maximum for the alertmanager format, with **no** URL in the message.

- [ ] **Step 4: Run the full suite**

```bash
gofmt -l . && go build ./... && go test ./...
```

Expected: `gofmt -l .` prints nothing; build and all tests pass.

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "feat(cli): add --alert-format and --alert-repeat to watch"
```

---

### Task 6: Packaging — Helm chart and the raw manifest

**Files:**

- Modify: `deploy/helm/kubeagent/values.yaml`, `deploy/helm/kubeagent/templates/deployment.yaml`, `deploy/deployment.yaml`, `deploy/README.md`

**Interfaces:**

- Consumes: the env var `KUBEAGENT_ALERT_WEBHOOK` and the flags `--alert-format` / `--alert-repeat` from Task 5.
- Produces: the `alerts.*` values block used by the docs in Task 8.

- [ ] **Step 1: Add the values block**

Append to `deploy/helm/kubeagent/values.yaml`:

```yaml
alerts:
  # Route watch transitions to an outbound webhook (one alert per broken object).
  # The URL is a credential, so it is never set here: point existingSecret at a
  # Secret that already holds it. The daemon stays read-only toward the cluster.
  enabled: false
  # json | slack | alertmanager
  format: json
  # Re-send interval for still-firing alerts. Empty = the format default
  # (4h, or 60s for alertmanager, which expires alerts after resolve_timeout).
  repeat: ""
  # Name of an existing Secret holding the webhook URL. Required when enabled.
  existingSecret: ""
  secretKey: webhook-url
```

- [ ] **Step 2: Wire the args and env in the deployment template**

In `deploy/helm/kubeagent/templates/deployment.yaml`, inside `args:` after the `--namespace` block:

```yaml
            {{- if .Values.alerts.enabled }}
            - "--alert-format={{ .Values.alerts.format }}"
            {{- if .Values.alerts.repeat }}
            - "--alert-repeat={{ .Values.alerts.repeat }}"
            {{- end }}
            {{- end }}
```

Inside `env:` after the `certs` block:

```yaml
            {{- if .Values.alerts.enabled }}
            - name: KUBEAGENT_ALERT_WEBHOOK
              valueFrom:
                secretKeyRef:
                  name: {{ required "alerts.existingSecret is required when alerts.enabled is true" .Values.alerts.existingSecret }}
                  key: {{ .Values.alerts.secretKey }}
            {{- end }}
```

- [ ] **Step 3: Verify the rendered chart**

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent | grep -c 'alert'
```

Expected: lint passes with no failures; the grep count is `0` — alerting is off by default and must add nothing to the rendered output.

```bash
helm template x deploy/helm/kubeagent \
  --set alerts.enabled=true --set alerts.format=slack \
  --set alerts.repeat=30m --set alerts.existingSecret=kubeagent-alerts \
  | grep -A 5 -E 'alert-format|KUBEAGENT_ALERT_WEBHOOK'
```

Expected: `--alert-format=slack`, `--alert-repeat=30m`, and a `KUBEAGENT_ALERT_WEBHOOK` env entry sourced from `secretKeyRef` name `kubeagent-alerts`, key `webhook-url`.

```bash
helm template x deploy/helm/kubeagent --set alerts.enabled=true 2>&1 | tail -2
```

Expected: the render fails with `alerts.existingSecret is required when alerts.enabled is true`.

- [ ] **Step 4: Document the raw manifest path**

In `deploy/deployment.yaml`, add this commented block at the end of the container's `env:` list (keep it commented — alerting is opt-in):

```yaml
            # Alerting (opt-in). Create the Secret first:
            #   kubectl -n kubeagent create secret generic kubeagent-alerts \
            #     --from-literal=webhook-url=<WEBHOOK_URL>
            # then uncomment this block and add --alert-format=<format> to args.
            # - name: KUBEAGENT_ALERT_WEBHOOK
            #   valueFrom:
            #     secretKeyRef:
            #       name: kubeagent-alerts
            #       key: webhook-url
```

Add an "Alerting" section to `deploy/README.md` between `## Crash log root-cause (opt-in)` and `## Security notes`:

````markdown
## Alerting (opt-in)

The daemon can POST one alert per broken object to a webhook — generic JSON, a
Slack incoming webhook, or Alertmanager's `/api/v2/alerts`. It stays read-only
toward the cluster; the webhook is its only other egress.

The URL is a credential, so it is read from `KUBEAGENT_ALERT_WEBHOOK` and never
passed as a flag:

```bash
kubectl -n kubeagent create secret generic kubeagent-alerts \
  --from-literal=webhook-url=<WEBHOOK_URL>

helm upgrade --install kubeagent deploy/helm/kubeagent \
  --set alerts.enabled=true \
  --set alerts.format=slack \
  --set alerts.existingSecret=kubeagent-alerts
```

Only `scheme://host` is ever logged. See the
[watch mode docs](https://k8sproject.top/features/watch-mode/) for the payload
shapes and the Alertmanager cadence rule.
````

- [ ] **Step 5: Verify the manifest still applies cleanly**

```bash
kubectl apply --dry-run=client -f deploy/deployment.yaml
```

Expected: `deployment.apps/kubeagent configured (dry run)` or `created (dry run)` — no YAML error from the commented block.

- [ ] **Step 6: Commit**

```bash
git add deploy/
git commit -m "feat(deploy): add the opt-in alerts block to the chart and manifest"
```

---

### Task 7: Chaos scenario — prove one alert survives an evolving failure

**Files:**

- Create: `chaos/alert-receiver.py`
- Modify: `chaos/run.sh` (`scenario_12_watch`, lines 270–321), `chaos/README.md`

**Interfaces:**

- Consumes: the built `./kubeagent` binary with `--alert-format` (Task 5) and `KUBEAGENT_ALERT_WEBHOOK` (Task 5).
- Produces: alert evidence in the chaos results report.

- [ ] **Step 1: Write the receiver**

Create `chaos/alert-receiver.py`:

```python
#!/usr/bin/env python3
"""Minimal alert receiver for the chaos harness.

Usage: alert-receiver.py PORT OUTFILE
Appends each POSTed body to OUTFILE, one JSON document per line, and answers 200.
"""
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        with open(sys.argv[2], "ab") as f:
            f.write(body + b"\n")
        self.send_response(200)
        self.end_headers()

    def log_message(self, *_args):
        pass  # keep the harness log readable


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
```

Make it executable:

```bash
chmod +x chaos/alert-receiver.py
```

- [ ] **Step 2: Extend the scenario**

In `chaos/run.sh`, change the `scenario_12_watch` local declaration line (line 272) to add the receiver's variables:

```bash
  local ns=chaos-watch port=18080 aport=18081 wlog wpid i firing after alerts apid
```

After the `wlog="$(mktemp)"` line, add:

```bash
  alerts="$(mktemp)"
  # A local receiver proves the alert path end to end. The daemon's only egress
  # besides the API server is this URL.
  python3 chaos/alert-receiver.py "$aport" "$alerts" >/dev/null 2>&1 &
  apid=$!
```

Change the daemon launch to enable alerting (the URL is a loopback address, not a credential, but it still travels by environment):

```bash
  KUBEAGENT_ALERT_WEBHOOK="http://127.0.0.1:$aport" \
  ./kubeagent watch --context "$CTX" -n "$ns" --metrics-addr "127.0.0.1:$port" \
    --heartbeat 10s --debounce 2s --alert-format json --alert-repeat 1h >"$wlog" 2>&1 &
  wpid=$!
```

After the existing `kill "$wpid"` / `wait "$wpid"` lines, stop the receiver:

```bash
  kill "$apid" >/dev/null 2>&1 || true
  wait "$apid" >/dev/null 2>&1 || true
```

Inside the `{ … } | record` block, before the write-path check, add:

```bash
    echo '--- alerts delivered to the webhook receiver ---'
    { grep -o '"status":"[a-z]*","reason":"[a-z]*"' "$alerts" || echo '<no alerts delivered>'; }
    echo
    printf 'firing notifications: %s\n' "$(grep -c '"status":"firing"' "$alerts" 2>/dev/null || echo 0)"
    printf 'resolved notifications: %s\n' "$(grep -c '"status":"resolved"' "$alerts" 2>/dev/null || echo 0)"
    printf 'distinct objects alerted: %s\n' "$(grep -o '"name":"[^"]*"' "$alerts" 2>/dev/null | sort -u | wc -l)"
    echo
    echo '--- webhook URL redaction check (only scheme://host may appear) ---'
    { grep -c '127.0.0.1:'"$aport" "$wlog" || true; } | sed 's/^/log lines naming the endpoint host: /'
    echo
```

Change the `record` expectation string to:

```bash
"expect: one NEW line naming Deployment/$ns/web, one RESOLVED line with the firing duration, the incident listed under /issues while firing and under resolved afterwards, and exactly one resolved alert delivered — the firing alert must survive the whole Degraded -> ErrImagePull -> ImagePullBackOff walk"
```

Add the receiver cleanup next to `rm -f "$wlog"`:

```bash
  rm -f "$wlog" "$alerts"
```

- [ ] **Step 3: Add `python3` to the preflight**

`scenario_12_watch` now needs `python3`. In `chaos/run.sh`'s `preflight`, extend the tool list:

```bash
  for b in docker kind kubectl helm go curl python3; do
```

- [ ] **Step 4: Update the chaos README**

`chaos/README.md` line 57 is scenario 12's four-column row. Replace it with:

```markdown
| 12 | Stateful watch daemon | run `kubeagent watch` on a loopback metrics address with alerting pointed at a local receiver, then inject and repair the bad-image outage | one `NEW` transition line, one `RESOLVED` line with the firing duration, the incident on `/issues` (active while firing, under `resolved` afterwards), and exactly one resolved alert delivered — the firing alert survives the whole failure-mode walk |
```

- [ ] **Step 5: Verify the scenario's shell is valid**

```bash
bash -n chaos/run.sh
python3 -c "import ast,sys; ast.parse(open('chaos/alert-receiver.py').read())"
```

Expected: both silent (exit 0).

Smoke-test the receiver alone:

```bash
OUT=$(mktemp); python3 chaos/alert-receiver.py 18099 "$OUT" & sleep 1
curl -s -X POST -d '{"status":"firing"}' http://127.0.0.1:18099/ -o /dev/null -w '%{http_code}\n'
cat "$OUT"; kill %1
```

Expected: `200`, then `{"status":"firing"}` in the file.

- [ ] **Step 6: Commit**

```bash
git add chaos/
git commit -m "test(chaos): assert one alert survives the evolving failure in scenario 12"
```

---

### Task 8: Documentation

**Files:**

- Modify: `website/docs/features/watch-mode.md`, `website/docs/roadmap.md`, `docs/go-concepts.md`, `CHANGELOG.md`

**Interfaces:**

- Consumes: everything from Tasks 1–7.
- Produces: the shipped documentation.

- [ ] **Step 1: Document alerting in the watch-mode page**

Add an `## Alerting` section to `website/docs/features/watch-mode.md`, placed after the `/issues` section and before the Known-limitation paragraph:

````markdown
## Alerting

The daemon can push transitions to a webhook instead of waiting to be scraped. It
stays read-only toward the cluster: alerting adds exactly one egress destination,
the URL you configure.

**One alert per object, not per issue.** An alert opens when an object acquires
its first issue, updates when its issue set changes, and clears only when the
object has no active issues at all. This is deliberate: because tracking is keyed
on the issue, a workload whose failure mode evolves —
`Degraded` → `ErrImagePull` → `ImagePullBackOff` — logs a `RESOLVED` for each
superseded mode while it is still broken. Rolled up per object, that is one alert
that stays firing until the Deployment actually recovers.

Enable it by setting the URL in the environment:

```bash
export KUBEAGENT_ALERT_WEBHOOK=<WEBHOOK_URL>
kubeagent watch --alert-format slack
```

There is no `--alert-webhook` flag on purpose: a Slack incoming-webhook URL is a
bearer token in URL form, and a flag would put it in the pod spec's args and in
`ps` output. Only `scheme://host` is ever logged.

| Flag | Default | Meaning |
|------|---------|---------|
| `--alert-format` | `json` | `json`, `slack`, or `alertmanager` |
| `--alert-repeat` | `4h`, or `60s` for `alertmanager` | Re-send interval for still-firing alerts |

The re-send is what makes a dropped notification self-healing: a receiver that was
down when the alert opened learns about it on the next repeat.

### Payloads

`json` — kubeagent's native body:

```json
{
  "status": "firing",
  "reason": "changed",
  "kind": "Deployment",
  "namespace": "shop",
  "name": "web",
  "issues": ["ImagePullBackOff"],
  "firingSince": "2026-07-25T10:04:11Z",
  "flapping": false
}
```

`reason` is `new`, `changed`, `repeat`, or `resolved`. A resolved body carries an
empty `issues` array and a `resolvedAt`.

`slack` — an incoming-webhook body: `*FIRING* Deployment/shop/web` with the issue
list and firing time, or `*RESOLVED* Deployment/shop/web (fired for 4m12s)`.

`alertmanager` — a `POST /api/v2/alerts` array. Labels are `alertname`
(`KubeagentIssue`), `kind`, `namespace`, and `name`; the issue list is an
**annotation**, because a label that changes as the failure evolves would create a
second alert instead of updating the open one. A bare host URL gets
`/api/v2/alerts` appended.

Alertmanager expires an alert `resolve_timeout` (5m by default) after the last
POST, so the re-send interval must stay under it — `--alert-repeat` above `4m`
with this format is a startup error.

### No severity

kubeagent has no severity model, so no payload claims one. Route on what is
actually known, and derive severity in Alertmanager if you want it:

```yaml
route:
  routes:
    - matchers: [alertname="KubeagentIssue", issues=~".*CrashLoopBackOff.*"]
      receiver: pager
```

### Delivery

Sends run on their own goroutine behind a 64-slot queue, so a hung receiver never
stalls the reconcile loop. Each POST gets three attempts (1s then 2s backoff);
a 4xx is not retried. Drops are counted, never silent:

| Series | Meaning |
|--------|---------|
| `kubeagent_alerts_sent_total{status,outcome}` | Deliveries by `firing`/`resolved` and `ok`/`failed` |
| `kubeagent_alerts_dropped_total{reason}` | `queue_full` or `retries_exhausted` |
| `kubeagent_alert_last_success_timestamp_seconds` | Last successful delivery (0 if none) |
````

- [ ] **Step 2: Update the roadmap**

Two places in `website/docs/roadmap.md` name alerting as future work; both must move it to shipped, or they will contradict each other.

First, in the shipped **Stateful `watch`** bullet (around line 282), replace:

```markdown
  defaults; no new flags or RBAC. Alerting integrations, SLO burn-rate signals,
  rate-limited on-incident `--explain`, and the multi-cluster hub are the
  remaining Theme E slices. See
```

with:

```markdown
  defaults; no new flags or RBAC. Slice 2 adds **alerting**: one webhook alert
  per broken object (`json` / `slack` / `alertmanager`), off unless
  `KUBEAGENT_ALERT_WEBHOOK` is set, with the daemon still read-only toward the
  cluster. SLO burn-rate signals, rate-limited on-incident `--explain`, and the
  multi-cluster hub are the remaining Theme E slices. See
```

Second, in the **E · Continuous operations** theme bullet (around line 342), the phrase "alerting integrations (Slack / PagerDuty / webhook)" now describes shipped work — reword that clause to mark alerting as shipped and leave PagerDuty as the remaining receiver, keeping the rest of the sentence intact.

- [ ] **Step 3: Add the Go concept entries**

Append two entries to `docs/go-concepts.md` in the established style (plain everyday example first, then the kubeagent example). Nothing else in this feature is a new concept — goroutines and channels are already covered by the watch daemon entry.

The first is **methods on a nil pointer**:

````markdown
## Methods on a nil pointer

A method with a pointer receiver can be called on a `nil` pointer. The call is not
a panic — only dereferencing the pointer would be. So a type can define its own
"switched off" state instead of making every caller check.

```go
type Logger struct{ prefix string }

func (l *Logger) Log(msg string) {
	if l == nil {
		return // logging is off
	}
	fmt.Println(l.prefix, msg)
}

var l *Logger // nil
l.Log("hello") // fine: prints nothing
```

In kubeagent, `internal/watch`'s `alerter` is nil when no webhook is configured —
which is the default. `alerter.notify` returns immediately on a nil receiver, so
the reconcile loop calls `al.notify(tr, now)` unconditionally and there is no
`if alerting enabled` branch to forget.
````

The second is **`errors.As`**:

````markdown
## errors.As — unwrapping to a specific error type

`errors.Is` asks "is this error that value?". `errors.As` asks "is there an error
of this *type* anywhere in the chain?" and, if so, assigns it to your variable so
you can read its fields.

```go
var pathErr *os.PathError
if errors.As(err, &pathErr) {
	fmt.Println("the failing path was", pathErr.Path)
}
```

kubeagent uses it for safety rather than detail. `net/http` wraps transport
failures in a `*url.Error` whose message includes the full request URL — and the
alert webhook URL is a credential. `internal/alert`'s `sanitizeErr` uses
`errors.As` to reach inside and return only the underlying error, so the URL
cannot reach a log line.
````

- [ ] **Step 4: Add the changelog entry**

Under `## [Unreleased]` in `CHANGELOG.md`, add to `### Added`:

```markdown
- `watch` alerting: the daemon can POST one alert per broken object to a webhook
  (`json`, `slack`, or `alertmanager` format), keyed on the object rather than the
  issue so an evolving failure never reports a recovery while the workload is
  still broken. The URL comes from `KUBEAGENT_ALERT_WEBHOOK` and is never logged
  beyond `scheme://host`; `--alert-format` and `--alert-repeat` tune the rest.
  Delivery is a bounded queue with three attempts and counted drops
  (`kubeagent_alerts_sent_total`, `kubeagent_alerts_dropped_total`,
  `kubeagent_alert_last_success_timestamp_seconds`). Alerting is off unless the
  environment variable is set, and the daemon remains strictly read-only toward
  the cluster.
```

- [ ] **Step 5: Verify the docs build**

```bash
(cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml)
```

Expected: "Documentation built", exit 0, and no `WARNING` lines naming
`features/watch-mode.md` or `roadmap.md`. The red Material 2.0 banner is cosmetic.

- [ ] **Step 6: Confirm no credential-shaped strings landed in the docs**

```bash
grep -rn 'hooks.slack.com/services' website/ deploy/ docs/ CHANGELOG.md || echo clean
```

Expected: `clean` — examples use `<WEBHOOK_URL>` placeholders.

- [ ] **Step 7: Commit**

```bash
git add website/ docs/ CHANGELOG.md
git commit -m "docs: document watch alerting"
```

---

## Release notes for the controller (not a task)

This branch changes the watch daemon **and** the Helm chart templates, so at
release time:

- the pre-release gate is the **full chaos gate**: `unset ANTHROPIC_API_KEY && ./chaos/run.sh --recreate`
- the chart takes a **minor** `version:` bump, not the script's default patch bump
- `scripts/bump-version.sh vX.Y.Z` still owns every other version reference

## Self-Review

**Spec coverage:**

| Spec section | Task |
|---|---|
| Per-object rollup, the four reasons, `FiringSince` semantics, flapping | 1 |
| Three encoders, exact payload shapes, annotations-not-labels | 2 |
| Redaction, URL resolution, `sanitizeErr` | 2 |
| Bounded queue, 3 attempts, drop counters, `Stats`, validation, `DefaultRepeat` | 3 |
| Alertmanager cadence rule (`> 4m` rejected, `endsAt` on resolve only) | 2 (encoder), 3 (validation) |
| `watch.Config` fields, nil-able alerter, error-never-alerts invariant | 4 |
| Three metric series, present when disabled | 4 |
| Env-only URL, both flags, format-default resolution, usage string | 5 |
| Helm `alerts.*` with `secretKeyRef` and required `existingSecret` | 6 |
| Chaos scenario asserting one alert across the failure-mode walk | 7 |
| watch-mode docs, roadmap, go-concepts, CHANGELOG | 8 |
| Out-of-scope items (severity model, PagerDuty encoder, persistence, routing rules) | no task, by design |

**Placeholder scan:** none — every code step carries complete code, and every
verification step carries the exact command and its expected output.

**Type consistency:** `alertstate.Notification` field names are used identically in
Tasks 1–4; `Format` and its three constants are defined in Task 2 and consumed
unchanged in Tasks 3–5; `alert.Stats` field names match between Task 3's
definition, Task 4's rendering, and Task 4's test; `applyResult`'s new signature is
stated in Task 4's Interfaces block and its two existing call sites are explicitly
updated in Step 4.
