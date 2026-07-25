# Stateful `watch` (design)

**Status:** approved · **Date:** 2026-07-25 · **Type:** continuous operations (Theme E —
slice 1: the stateful core)

## Problem

The `watch` daemon re-derives the whole health picture on every reconcile and has no
memory of what it already reported. Its only state is `changeLogger`, which hashes the
entire cluster picture into one signature string: it can say *"something changed"* but not
**what became new**, **what cleared**, **how long an issue has been firing**, or **whether
an issue is flapping**. Metrics are point-in-time gauges only — `kubeagent_workloads_flagged 3`
tells you three workloads are unhappy, never that one of them started 20 seconds ago and
another has been broken for six hours.

Everything else in Theme E depends on this. Alerting must know what is *new* (or it pages
on every reconcile); SLO burn-rate needs firing durations; a multi-cluster hub needs a
per-cluster incident list. This slice builds that state.

## Scope decisions (locked)

| Decision | Choice |
|----------|--------|
| State home | **New `internal/watchstate` package** — a pure `Tracker` with an injected clock, unit-testable without a cluster. `watch` stays a thin shell; `changeLogger`/`signature` are deleted |
| Surfaces | **Logs + metrics + `/issues` JSON** — per-transition log lines, additive Prometheus series, and a read-only endpoint, so the alerting slice has both a push and a pull source |
| Persistence | **In-memory only** — no volume, no on-disk schema; the daemon stays strictly read-only. Restart semantics documented explicitly |
| Derived signals | **Full set** — new / resolved / ongoing, age, flapping, MTTR, plus a cardinality cap with a dropped counter |

## Architecture

```
informers/heartbeat ──▶ scan.Evaluate ──▶ issueKeys(res) ──▶ watchstate.Tracker.Observe(keys, now)
                                                                        │
                                                            Delta{New, Resolved, Ongoing, NewlyFlapping}
                                                                        │
                                              ┌─────────────────────────┼─────────────────────────┐
                                        transition logs          metrics series             /issues JSON
```

### 1. `internal/watchstate` — the pure lifecycle tracker

Generic over an issue key. Knows nothing about Kubernetes or `scan.Result`: no I/O, no
goroutines, no wall-clock reads (`now` is a parameter). This is the same "pure core, thin
I/O shell" shape the detectors and `remediate.Plan` already use.

```go
// Key identifies one tracked issue instance. Low-cardinality by construction.
type Key struct {
	Kind      string // "Deployment" | "Service" | "Node" | "PVC" | "Cluster" | …
	Namespace string // "" for cluster-scoped
	Name      string
	Issue     string // "CrashLoopBackOff" | "NoEndpoints" | "KubeletUnhealthy" | …
}

// String renders "Deployment/prod/api:CrashLoopBackOff", or
// "Node/worker-2:KubeletUnhealthy" when Namespace is empty.
func (k Key) String() string
```

```go
// Record is one issue's tracked lifecycle.
type Record struct {
	Key         Key
	FirstSeen   time.Time // first time this key was ever observed
	FiringSince time.Time // start of the CURRENT firing (== FirstSeen on the first one)
	LastSeen    time.Time
	Active      bool
	Firings     int       // inactive→active transitions (>= 1)
	ResolvedAt  time.Time // zero while active
	Flapping    bool      // >= FlapThreshold firings inside FlapWindow
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

type Tracker struct { /* records, per-key firing timestamps, counters */ }

func New(o Options) *Tracker
func (t *Tracker) Observe(keys []Key, now time.Time) Delta
func (t *Tracker) Active() []Record           // sorted by Key.String()
func (t *Tracker) RecentlyResolved() []Record // sorted; within RetainResolved
func (t *Tracker) Stats() Stats

// Stats are monotonic process-lifetime counters (Prometheus counter sources).
type Stats struct {
	NewTotal               int64
	ResolvedTotal          int64
	FlapTotal              int64
	DroppedTotal           int64   // new keys rejected by the cap
	ResolutionSecondsSum   float64 // MTTR numerator
	ResolutionSecondsCount int64   // MTTR denominator
}
```

Locked semantics:

- **New:** a key with no record, or a record with `Active == false`. Both set
  `FiringSince = now`, `Active = true`, `Firings++`, `NewTotal++`; a re-fire keeps the
  original `FirstSeen` and its resolved-firing history.
- **Resolved:** a record that was `Active` and whose key is absent from `keys`. Sets
  `Active = false`, `ResolvedAt = now`, `ResolvedTotal++`, and accumulates
  `ResolvedAt.Sub(FiringSince)` into `ResolutionSecondsSum` / `Count`. MTTR therefore
  measures **the firing that just ended**, not the age since first-ever sighting.
- **Ongoing:** `Active` before and present now; `LastSeen = now`.
- **Flapping:** each firing appends `now` to the key's firing-timestamp list, trimmed to
  entries newer than `now - FlapWindow`. `Flapping = len(trimmed) >= FlapThreshold`. A
  record appears in `NewlyFlapping` (and increments `FlapTotal`) only on the cycle it
  crosses the threshold; once the window ages out it un-flaps and can flap again later.
- **Cardinality cap:** `Observe` sorts `keys` by `Key.String()` and admits them in that
  order. Existing records always keep their slot; a brand-new key while
  `len(records) >= MaxTracked` is dropped (not tracked, not reported) and
  `DroppedTotal++`. Sorting makes which keys get dropped deterministic and testable.
- **Purge:** at the start of each `Observe`, resolved records with
  `now.Sub(ResolvedAt) > RetainResolved` are deleted, freeing cap slots. Purged records
  never reappear in a `Delta`.

`Tracker` is not goroutine-safe; the daemon only touches it from the reconcile loop.
`Active()` / `RecentlyResolved()` / `Stats()` return copies, so the HTTP handler reads a
snapshot the loop cannot mutate mid-render (see §5).

### 2. `internal/watch/issues.go` — mapping the scan result to keys

A pure function, unit-testable with a hand-built `scan.Result`. It lives in `watch` (not
`watchstate`) so the tracker stays free of the large `scan.Result` type.

```go
// issueKeys projects one evaluation into the set of tracked issue instances.
// Deterministic and deduplicated; sorting is the tracker's job.
func issueKeys(res *scan.Result) []watchstate.Key
```

| Source | Kind | Namespace | Name | Issue |
|--------|------|-----------|------|-------|
| `Inventory.Workloads[].Findings` | `w.Kind` | `w.Namespace` | `w.Name` | `f.Issue` |
| flagged workload with **no** findings | `w.Kind` | `w.Namespace` | `w.Name` | `Degraded` |
| `ServiceIssues` (skip `Expected`) | `Service` | `Namespace` | `Name` | `Problem` |
| `IngressIssues` (skip `Expected`) | `Ingress` | `Namespace` | `Ingress` | `Problem` |
| `PVCIssues` | `PVC` | `Namespace` | `Name` | `Reason` |
| `StuckTerminating` | `i.Kind` | `Namespace` | `Name` | `StuckTerminating` |
| `PDBIssues` | `PodDisruptionBudget` | `Namespace` | `Name` | `Category` |
| `HPAIssues` | `HorizontalPodAutoscaler` | `Namespace` | `Name` | `Category` |
| `WebhookIssues` | `i.Kind` | `` | `Config + "/" + Webhook` | `Problem` |
| `QuotaIssues` | `ResourceQuota` | `Namespace` | `Quota + "/" + Resource` | `Severity` |
| `Health.DownNodes` | `Node` | `` | `Name` | `NotReady` when `Reason == "NotReady"`, else `KubeletNotHeartbeating` |
| `KubeletHealth.Unhealthy` | `Node` | `` | `Node` | `KubeletUnhealthy` |
| `ControlPlane.Status == "unhealthy"` | `Cluster` | `` | `control-plane` | `Unhealthy` |
| `DNS.Status == "degraded"` | `Cluster` | `` | `coredns` | `DNSDegraded` |
| `Certificates.Expired` / `.Expiring` (nil-safe) | `Secret` | `Namespace` | `Name` | `CertExpired` / `CertExpiring` |
| `DiskUsage.Over` | `Volume` | `Namespace` | `Node` when `Kind == "node"`, else `Name` | `DiskOverThreshold` |

Deliberate choices in the mapping:

- **Duplicates collapse.** Two broken routes on one Ingress with the same `Problem`
  produce one key; the same holds for any source that can repeat a
  (kind, namespace, name, issue) tuple. This bounds cardinality; the exact route/detail
  stays available in `scan`'s own output.
- **`Expected` issues are excluded**, matching the existing metrics (`realServiceIssues`,
  `realIngressIssues`) — a parked backend is not an incident.
- **Advisory/config reports are excluded**: `NodeReserve`, `PVCReclaim`, and
  `SecurityIssues` describe standing configuration, not incidents that fire and resolve.
  Tracking them would make MTTR meaningless.

### 3. Reconcile-loop wiring

`changeLogger` and `signature` are **deleted** — the `Delta` is the change detector now.

```go
tr := watchstate.New(watchstate.Options{})
reconcile := func() {
	start := time.Now()
	res, err := scan.Evaluate(ctx, client, opts)
	m.update(&res, time.Since(start), time.Now(), err)
	if err != nil {
		log.Printf("kubeagent: evaluation error: %v", err)
		return // NO Observe: a failed evaluation is not "all clear"
	}
	d := tr.Observe(issueKeys(&res), time.Now())
	m.updateIssues(tr)
	logDelta(&res, d)
}
```

**Invariant (tested):** an evaluation error never reaches `Observe`. Otherwise a single
API blip would resolve every issue, then re-fire them all on the next success — inflating
flap counts, corrupting MTTR, and (once alerting lands) paging the on-call for a network
hiccup. The error is logged every failing cycle; `kubeagent_scan_errors_total` already
counts them.

Log output, silent in steady state:

```
kubeagent: NEW Deployment/prod/api:CrashLoopBackOff
kubeagent: RESOLVED Deployment/prod/api:CrashLoopBackOff (fired for 4m12s)
kubeagent: FLAPPING Deployment/prod/api:CrashLoopBackOff (3 firings in 30m)
kubeagent: cluster Degraded (2/3 nodes ready) — 4 issue(s) active, 1 new, 1 resolved
```

`logDelta` prints one line per `New`, `Resolved`, and `NewlyFlapping` record (in
`Key.String()` order), then the summary line — and prints **nothing at all** when the
delta holds only `Ongoing` records. A reconcile that changes nothing stays quiet, as
today.

### 4. Metrics (additive; every existing series unchanged)

Aggregates:

```
kubeagent_issues_active                   gauge    # len(Active())
kubeagent_issues_flapping                 gauge    # active records with Flapping
kubeagent_issues_new_total                counter
kubeagent_issues_resolved_total           counter
kubeagent_issues_flapping_total           counter
kubeagent_issues_dropped_total            counter  # cap rejections
kubeagent_issue_resolution_seconds_sum    counter  # MTTR = sum / count
kubeagent_issue_resolution_seconds_count  counter
```

Per active issue, labels `{kind,namespace,name,issue}`, bounded by `MaxTracked`:

```
kubeagent_issue_active{kind="Deployment",namespace="prod",name="api",issue="CrashLoopBackOff"} 1
kubeagent_issue_age_seconds{kind="Deployment",namespace="prod",name="api",issue="CrashLoopBackOff"} 252
```

`kubeagent_issue_age_seconds` is `now - FiringSince` (current firing, not first-ever
sighting), computed at render time from the snapshot's `renderedAt`. Series are emitted in
`Key.String()` order so `/metrics` output is stable. `metrics` gains an issue snapshot
(records + `Stats`) written by `updateIssues` under the existing mutex — the HTTP handler
never touches the `Tracker` directly.

### 5. `/issues` endpoint

A read-only `GET` on the existing metrics server, `Content-Type: application/json`. Field
names are a contract from this release on:

```json
{
  "active": [
    {"kind":"Deployment","namespace":"prod","name":"api","issue":"CrashLoopBackOff",
     "firstSeen":"2026-07-25T10:00:00Z","firingSince":"2026-07-25T10:12:00Z",
     "lastSeen":"2026-07-25T10:16:12Z","firings":2,"flapping":false,"ageSeconds":252}
  ],
  "resolved": [
    {"kind":"Service","namespace":"prod","name":"api","issue":"NoEndpoints",
     "firstSeen":"2026-07-25T10:00:00Z","firingSince":"2026-07-25T10:00:00Z",
     "lastSeen":"2026-07-25T10:04:00Z","firings":1,"flapping":false,
     "resolvedAt":"2026-07-25T10:04:12Z","resolutionSeconds":252}
  ],
  "stats": {"newTotal":7,"resolvedTotal":3,"flapTotal":1,"droppedTotal":0,
            "resolutionSecondsSum":812,"resolutionSecondsCount":3}
}
```

`active` omits `resolvedAt`/`resolutionSeconds`; `resolved` omits `ageSeconds`. Both
arrays are sorted by `Key.String()` and are `[]` (never `null`) when empty. `ageSeconds`
is derived the same way as the metric — from the snapshot's `renderedAt` minus
`firingSince` — so `/metrics` and `/issues` never disagree about an issue's age.

### 6. Restart semantics

State is in-memory. After a daemon restart the first reconcile reports **everything
currently firing as `NEW`**, ages restart from zero, and the counters reset. This is
documented in `watch-mode.md` so nobody reads the first post-restart burst as a real
incident storm. The trade for that wrinkle: no volume, no on-disk schema to version, no
corruption path, and the daemon stays strictly read-only.

### 7. Configuration

No new flags. `watchstate.Options{}` takes its defaults (500 tracked / 1h retention /
30m flap window / 3 firings). Tuning knobs land in a later slice if an operator asks for
them — a flag that nobody sets is a flag that has to be documented, tested, and supported
forever.

## Global constraints

- **Strictly read-only.** No new API verbs; `watchstate` performs no I/O at all. The
  daemon still never writes and never calls an LLM.
- **Additive only.** Existing metric names, `/healthz`, `/readyz`, and the `scan` output
  are untouched; the **golden snapshot is unchanged** (`scan` is not involved).
- **No new dependency** — stdlib plus the client-go already in the module.
- **Deterministic and clock-injected.** Every `watchstate` behaviour is testable without
  a cluster and without sleeping.
- **No `Co-Authored-By: Claude` trailer.** **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Alert routing (webhook/Slack/PagerDuty — the next slice); persistence across restarts;
SLO burn-rate windows; multi-cluster aggregation; tuning flags; per-issue history beyond
the flap window; a Grafana dashboard; suppression/silencing rules; on-incident
`--explain`.

## Testing

**`watchstate` (fake clock, no cluster, no sleeps):**

- first observation → `New` with `FirstSeen == FiringSince == now`, `Firings == 1`
- same key next cycle → `Ongoing` only, `LastSeen` advances, `NewTotal` unchanged
- key disappears → `Resolved`, `Active` false, `ResolvedAt` set, and
  `ResolutionSecondsSum` gains exactly the firing duration
- re-fire → `New` again with `Firings == 2` and the **original** `FirstSeen` preserved
- flap threshold crossed → the record appears in `NewlyFlapping` **exactly once**, and
  `Flapping` stays true while the window holds
- flap window expiry → `Flapping` returns to false; a later burst can flap again
- cap: `MaxTracked` reached → the deterministic overflow key is absent from `Active()`
  and `DroppedTotal == 1`; an existing record keeps its slot
- purge: a resolved record older than `RetainResolved` leaves `RecentlyResolved()`, frees
  a cap slot, and never reappears in a `Delta`
- `Active()` / `RecentlyResolved()` return sorted copies; mutating the returned slice does
  not affect the tracker

**`issueKeys` (table-driven, hand-built `scan.Result`):** one case per source row in §2,
plus `Expected` suppression (Service and Ingress), duplicate collapse (two same-problem
routes on one Ingress → one key), the flagged-but-findingless `Degraded` case, and a nil
`Certificates` report.

**`watch`:** an evaluation error does **not** call `Observe` (the active set survives, no
`Resolved`, no flap inflation); `logDelta` prints nothing for an `Ongoing`-only delta;
`/issues` returns the documented shape (including `[]` for empty arrays); the new metric
names render with correct values and stable ordering.

**Live gate:** the full chaos suite — the daemon scenario must show a `NEW` line when an
outage is injected and a `RESOLVED` line when it is repaired, with `/issues` listing the
incident while it fires.

## Release

- **Gate:** touches the watch daemon → **FULL CHAOS GATE** (`./chaos/run.sh --recreate`).
- **Version:** minor **v0.54.0 → v0.55.0**.
- **Chart:** **PATCH** — no RBAC, template, or values change.

## Files touched

- **Create:** `internal/watchstate/watchstate.go` (+ `watchstate_test.go`) — `Key`,
  `Record`, `Delta`, `Options`, `Tracker`, `Stats`.
- **Create:** `internal/watch/issues.go` (+ `issues_test.go`) — `issueKeys`.
- **Modify:** `internal/watch/watch.go` (+ test) — tracker wiring, `logDelta`, deletion of
  `changeLogger`/`signature`.
- **Modify:** `internal/watch/metrics.go` (+ test) — the issue snapshot, `updateIssues`,
  the new series, the `/issues` handler.
- **Docs:** `website/docs/features/watch-mode.md` (state, new metrics, `/issues`, restart
  semantics), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md` (Theme E slice 1
  shipped), `docs/go-concepts.md` if the build introduces a new Go concept.
