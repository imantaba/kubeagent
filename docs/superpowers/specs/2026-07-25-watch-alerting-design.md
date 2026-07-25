# Watch alerting — design

**Date:** 2026-07-25
**Theme:** E (continuous operations), slice 2
**Status:** approved, ready to plan

## Problem

The watch daemon shipped its stateful core in v0.55.0: `internal/watchstate` tracks
every issue keyed on `(Kind, Namespace, Name, Issue)` and reports New / Ongoing /
Resolved / NewlyFlapping deltas; `internal/watch` logs those transitions, exposes ten
Prometheus series, and serves `/issues`. Everything stays in memory, the daemon stays
strictly read-only toward the cluster, and no LLM runs in the watch path.

Nobody gets paged. An operator has to be watching the logs, scraping the metrics, or
polling `/issues` to learn that a workload broke. This slice routes transitions
outbound so a webhook receiver — a generic endpoint, Slack, or Alertmanager — hears
about them.

Two constraints make this more than "POST the delta":

1. **Outbound HTTP is a new capability for this daemon.** The cluster-read invariant
   must survive intact: alerting adds one egress destination, chosen by the operator,
   and still issues no writes to the API server.

2. **The v0.55.0 chaos run exposed a routing hazard.** Because `Issue` is part of the
   tracking key, a workload whose failure mode evolves —
   `Degraded → ErrImagePull → ImagePullBackOff` — logs a `RESOLVED` for each superseded
   mode *while the workload is still broken*. Routing those transitions naively would
   send a recovery notification for a Deployment that is still down.

## Decisions

| Question | Decision |
|---|---|
| What is an alert keyed on? | **Per-object rollup** — `(Kind, Namespace, Name)`, not the issue key |
| Wire formats | **One sink, three encoders**: `json`, `slack`, `alertmanager` |
| Delivery failure | **Bounded queue, bounded retry, periodic re-send** of still-firing alerts |
| Where the URL comes from | **`KUBEAGENT_ALERT_WEBHOOK` env var only**; unset = alerting off |
| Severity | **Omitted.** kubeagent has no severity model; inventing one here is the wrong layer |
| Where rollup state lives | **New pure package** `internal/alertstate` + I/O package `internal/alert` |

### Why per-object rollup

An alert opens when an object acquires its first issue, updates when its issue set
changes, and clears only when the object has **zero** active issues. The evolving
failure above becomes one alert that stays firing across all three modes, with the
current issue list as a payload field. This is also how Alertmanager expects labels to
group, so the `alertmanager` encoder falls out naturally.

### Why no severity

`diagnose.Finding` has no `Severity` field, and the only root-cause attribution in the
codebase is the free-text hint `inventory.Workload.RootCause`. A hand-maintained
issue→severity table in the alerting layer would drift silently every time a detector
is added, and would put a real model in the wrong package. Severity belongs on
`diagnose.Finding`, which is its own slice. The payload carries `kind`, `namespace`,
`name`, and `issues`; the docs show the Alertmanager route matcher and relabel rule an
operator uses to derive severity locally.

## Architecture

One direction, mirroring the `watchstate` / `watch` split that worked in v0.55.0:

```
scan.Evaluate ─▶ issueKeys ─▶ watchstate.Tracker.Observe ─▶ tracker.Active()
                                                                 │
                        internal/alertstate  (PURE — clock injected, no I/O)
                        Roll(active, now) ─▶ []Notification
                                                                 │
                        internal/alert  (I/O)
                        Enqueue ─▶ [buffered chan] ─▶ sender goroutine
                                   encode ─▶ POST (retries) ─▶ metrics
```

`internal/alertstate` imports `internal/watchstate`. Only `internal/watch` imports
`internal/alert`. No cycles.

### `internal/alertstate` — pure rollup

```go
// Object identifies the thing an alert is about.
type Object struct{ Kind, Namespace, Name string }

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
	ReasonRepeat   Reason = "repeat"   // periodic re-send, set unchanged
	ReasonResolved Reason = "resolved" // object has no active issues
)

// Notification is one message to deliver.
type Notification struct {
	Object      Object
	Status      Status
	Issues      []string  // sorted, unique; empty when resolved
	FiringSince time.Time
	ResolvedAt  time.Time // zero unless resolved
	Flapping    bool      // any constituent issue is flapping
	Reason      Reason
}

// Options tunes re-send cadence. A zero Repeat takes the 4h default, following the
// same zero-takes-the-default convention as watchstate.Options.
type Options struct{ Repeat time.Duration }

func New(o Options) *Roller
func (r *Roller) Roll(active []watchstate.Record, now time.Time) []Notification
```

`Roll` groups the active records by object and compares against what was last sent:

| Object state | Active issues | Emitted |
|---|---|---|
| not alerting | ≥ 1 | `ReasonNew`, `StatusFiring` |
| alerting | set differs from last sent | `ReasonChanged`, `StatusFiring` |
| alerting | set identical, `now - lastSent ≥ Repeat` | `ReasonRepeat`, `StatusFiring` |
| alerting | set identical, `now - lastSent < Repeat` | nothing |
| alerting | 0 | `ReasonResolved`, `StatusResolved`, entry deleted |

`FiringSince` is the **earliest** `FiringSince` among the object's constituent records,
so it reports when the object first broke, not when the current failure mode appeared.
`Flapping` is true when any constituent record is flapping. Returned notifications are
sorted by `Kind/Namespace/Name` so output is deterministic.

`Roll` never reads the wall clock and performs no I/O — the caller passes `now`, exactly
like `watchstate.Observe`. The Roller is not safe for concurrent use; the daemon touches
it only from the reconcile loop.

**No separate cap.** `watchstate` already caps tracking at 500 issues, which bounds the
number of distinct objects; a second cap would be dead configuration.

### `internal/alert` — encoders and sender

```go
type Format string

const (
	FormatJSON         Format = "json"
	FormatSlack        Format = "slack"
	FormatAlertmanager Format = "alertmanager"
)

type Config struct {
	URL    string
	Format Format
	Repeat time.Duration // used only for startup validation of the alertmanager format
}

func New(cfg Config, c *http.Client) (*Sink, error) // validates URL and format
func (s *Sink) Start(ctx context.Context)           // launches the sender goroutine
func (s *Sink) Enqueue(n alertstate.Notification)   // non-blocking
func (s *Sink) Close()                              // drains and stops
```

Fixed, non-configurable: queue capacity 64, HTTP timeout 10s, 3 attempts with 1s/2s/4s
backoff. These are implementation constants, not flags — YAGNI until someone needs to
tune them.

`Enqueue` never blocks the reconcile loop. When the queue is full it **drops the oldest**
queued notification (the newest state is the useful one), logs it, and increments
`kubeagent_alerts_dropped_total{reason="queue_full"}`. When all three attempts fail, the
notification is dropped with `reason="retries_exhausted"`. Nothing is retained across
drops, so memory stays bounded.

The three encoders are pure functions of a `Notification`:

```go
func encodeJSON(n alertstate.Notification) ([]byte, error)
func encodeSlack(n alertstate.Notification) ([]byte, error)
func encodeAlertmanager(n alertstate.Notification) ([]byte, error)
```

**`json`** — kubeagent's native payload, one object per POST:

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

A resolved notification carries `"status":"resolved"`, an empty `issues` array, and
`"resolvedAt"`.

**`slack`** — a Slack incoming-webhook body:

```json
{"text": "*FIRING* Deployment/shop/web\nissues: ImagePullBackOff\nfiring since 2026-07-25T10:04:11Z"}
```

Resolved renders `*RESOLVED* Deployment/shop/web (fired for 4m12s)`. A flapping object
appends ` (flapping)`.

**`alertmanager`** — the v2 alerts array:

```json
[{
  "labels": {
    "alertname": "KubeagentIssue",
    "kind": "Deployment",
    "namespace": "shop",
    "name": "web"
  },
  "annotations": {"issues": "ImagePullBackOff", "flapping": "false"},
  "startsAt": "2026-07-25T10:04:11Z"
}]
```

The issue list lives in an **annotation**, not a label: it changes as the failure
evolves, and a changing label value would create a new Alertmanager alert instead of
updating the existing one — reintroducing the very problem the rollup solves.

### Alertmanager cadence

Alertmanager auto-resolves an alert `resolve_timeout` (default 5m) after the last POST
for it. So the re-send interval is protocol-dependent, and `--alert-repeat` defaults
differ by format:

- `json`, `slack`: **4h** (a chatty default here is alert fatigue)
- `alertmanager`: **60s** (well inside the default `resolve_timeout`)

A firing POST omits `endsAt` and lets `resolve_timeout` be the safety net; a resolved
POST sets `endsAt` to `ResolvedAt` so the alert clears immediately rather than five
minutes later. Startup **rejects `--alert-repeat` greater than 4m with the
`alertmanager` format**, since a still-firing alert would expire between re-sends and
then re-fire — the same false-recovery class of bug this slice exists to avoid.

`--alert-repeat` is unset by default, which means "use the format default" (4h, or 60s
for `alertmanager`). Turning periodic re-sends off entirely is not offered: 4h is
already quiet, and an off switch would silently forfeit the drop-recovery this design
depends on.

For the `alertmanager` format, a URL whose path is empty or `/` gets `/api/v2/alerts`
appended; any other path is used verbatim.

## Configuration

The webhook URL is a credential — a Slack incoming-webhook URL is a bearer token in URL
form — so it follows the `KUBEAGENT_EXPLAIN_API_KEY` precedent and comes from the
environment only:

- `KUBEAGENT_ALERT_WEBHOOK` — the destination URL. **Unset = alerting off**, which is
  the default for every existing deployment.
- `--alert-format json|slack|alertmanager` (default `json`)
- `--alert-repeat <duration>` (unset = the format default: `4h`, or `60s` for
  `alertmanager`)

Both flags are accepted only by `watch`, and are ignored with a startup warning if the
env var is unset.

### Redaction

The URL is never logged. `redactURL` reduces it to `scheme://host`, dropping path,
query, and userinfo, and every log line that mentions the destination — the startup
banner and each delivery error — goes through it:

```
kubeagent: alerting enabled (format=slack, repeat=4h, endpoint=https://hooks.slack.com)
kubeagent: alert delivery failed after 3 attempts (endpoint=https://hooks.slack.com): 502 Bad Gateway
```

## Metrics

Three new series on the existing `/metrics` handler:

| Series | Type | Meaning |
|---|---|---|
| `kubeagent_alerts_sent_total{status,outcome}` | counter | `status` = `firing`/`resolved`, `outcome` = `ok`/`failed` |
| `kubeagent_alerts_dropped_total{reason}` | counter | `reason` = `queue_full`/`retries_exhausted` |
| `kubeagent_alert_last_success_timestamp_seconds` | gauge | Unix seconds of the last successful POST; 0 if none yet |

All three exist even when alerting is disabled, reporting zero, so a dashboard does not
break when alerting is switched on.

## Invariants preserved

- **No cluster writes.** The sink's only egress is the operator-configured URL. The
  daemon's Kubernetes verbs remain get/list/watch, and RBAC is unchanged.
- **No LLM in the watch path.**
- **A failed evaluation still alerts nothing.** `applyResult` already returns before
  `Observe` on error; the rollup hangs off `tracker.Active()`, so an API blip produces
  no state change and therefore no notifications — the same invariant, inherited rather
  than re-implemented.
- **Sink failure never affects the reconcile loop.** `Enqueue` is non-blocking and the
  sender runs on its own goroutine.

## Error handling

| Failure | Behaviour |
|---|---|
| `KUBEAGENT_ALERT_WEBHOOK` unset | Alerting disabled; one log line if `--alert-*` flags were passed |
| Malformed URL, or unknown `--alert-format` | Startup error; daemon exits non-zero |
| `--alert-repeat` > 4m with format `alertmanager` | Startup error; daemon exits non-zero |
| Receiver returns 5xx or times out | Retry up to 3 attempts, then drop + counter + redacted log line |
| Receiver returns 4xx | No retry (a client error will not fix itself); drop + counter + redacted log line |
| Queue full | Drop the oldest queued notification + counter |
| Context cancelled | `Close` stops the sender; in-flight POST is abandoned at shutdown |

## Testing

**`internal/alertstate`** — table-driven, fake clock, no cluster:

- new object fires once; unchanged set within `Repeat` emits nothing
- issue set change emits `ReasonChanged` with the new set, `FiringSince` unmoved
- `Repeat` elapsed emits `ReasonRepeat`
- object clears → exactly one `ReasonResolved`, then silence
- **the evolving-failure case**: `Degraded` → `Degraded,ErrImagePull` →
  `ImagePullBackOff` → clear yields exactly one firing-open, two changes, and one
  resolved — never a resolve while any issue is active
- flapping propagates when any constituent record flaps
- multi-object output is sorted and deterministic

**`internal/alert`** — encoders and sender:

- one encoder test per format for both firing and resolved, asserting exact JSON
- `alertmanager` resolved sets `endsAt`; firing omits it; issues live in annotations
- `httptest.Server`: 200 first try; 500 twice then 200; 4xx not retried; timeout
- queue-full drops the oldest and counts it
- `redactURL` strips path, query, and userinfo from a Slack-shaped URL
- URL path handling for the `alertmanager` format

**`internal/watch`** — wiring only:

- a failed evaluation produces no notifications
- alerting disabled by default when the env var is unset

**Chaos** — `scenario_12_watch` grows an alert receiver (a small python
`http.server` handler writing each POST to a file). The scenario asserts that the
bad-image outage produces exactly one firing alert across the whole
`Degraded → ErrImagePull → ImagePullBackOff` walk, one resolved after the repair, and
that the write-verb count in the daemon log stays zero.

## Packaging and docs

Helm gets an alerting block that never carries the URL itself:

```yaml
alerts:
  enabled: false
  format: json          # json | slack | alertmanager
  repeat: 4h
  existingSecret: ""    # Secret holding the webhook URL
  secretKey: webhook-url
```

The template wires `KUBEAGENT_ALERT_WEBHOOK` from `secretKeyRef`. There is deliberately
no `alerts.webhookUrl` value: that would put the credential in `values.yaml` and in
Helm's release history.

Docs to update: an alerting section in `website/docs/features/watch-mode.md` (the three
formats, the cadence rule, the Alertmanager route/relabel recipe for severity, the
redaction guarantee), the `deploy/` README, and the CHANGELOG.

Because this changes chart templates and the watch daemon, the release takes a **minor
chart bump** and the **full chaos gate**.

## Out of scope

- A severity model on `diagnose.Finding` (its own slice)
- PagerDuty Events API v2 as a fourth encoder — reachable today via the `alertmanager`
  format
- Persisting alert state across daemon restarts; a restart re-opens alerts for whatever
  is still broken, which is correct and self-healing
- Per-namespace or per-issue routing rules — that is Alertmanager's job
- Alerting from `scan` (one-shot runs print their findings already)
