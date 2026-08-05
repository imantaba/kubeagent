# PagerDuty Events API v2 as a fourth alert receiver — design

**Status:** approved, ready for planning
**Theme:** E · Continuous operations — the last named-open item in an otherwise
complete theme
**Ships as:** v1.3.0 (MINOR: a new accepted value for an existing flag, a new
environment variable, no removal and no change of meaning)

## Why now, and why not the answer we gave last time

The 2026-07-25 alerting design deferred PagerDuty explicitly, listing it under
*Out of scope* with the rationale "reachable today via the `alertmanager`
format". That rationale is true and insufficient. It holds only for an operator
who already runs Alertmanager. For anyone else it means: to be paged by
kubeagent, first deploy a Prometheus stack. That is a direct contradiction of the
roadmap's fifth principle — *one fast binary, minimal dependencies; no agent
sprawl, no control plane to babysit*. Paging is the reason a watch daemon exists
at all, and it is the one integration that should not require a second system.

So we build it. Four formats, not three.

The cost is bounded because the shape is already there. `internal/alert` is 422
non-test lines across three files: a `Format` enum with a dispatch, a `Sink` with
one sender goroutine, and a URL resolver. A fourth format is one encoder, one
enum value, one credential, and one branch in the Helm template.

## What this is not

Not a severity model. Not alerting from `scan`. Not acknowledgement handling —
kubeagent never reads PagerDuty's state back, so `event_action: acknowledge` has
no source of truth and is not sent. Not persistence: a daemon restart re-triggers
whatever is still broken, which PagerDuty deduplicates onto the open alert, which
is exactly the self-healing behaviour the existing formats already rely on.

## The upstream contract we are coding against

Corroborated against PagerDuty's Events API v2 reference and against
Prometheus Alertmanager's own PagerDuty notifier, which is the most widely
deployed independent implementation of the same contract:

- Endpoint `https://events.pagerduty.com/v2/enqueue`, `POST`, JSON body.
- Body: `routing_key`, `event_action`, `dedup_key`, `payload{summary, source,
  severity, timestamp, custom_details, class, component, group}`, plus optional
  `client`, `client_url`, `links`, `images`.
- `severity` is one of `critical`, `error`, `warning`, `info`.
- `dedup_key` is capped at 255 characters.
- Alertmanager truncates `summary` to 1024 runes and defaults `severity` to
  `error`.

One fact we could **not** confirm and therefore do not design around: whether a
separate EU service-region endpoint exists. No second host is named anywhere in
the code, the chart, or the docs. The endpoint is overridable instead, which
covers a non-default region, an egress proxy, and the test double with one
mechanism rather than a hardcoded list that would go stale.

Two behaviours we deliberately make no claim about, in code comments or in
documentation, because we could not verify them: exactly how PagerDuty's UI
renders a repeated `trigger` on an open `dedup_key`, and what its response body
contains. Neither is load-bearing — see *Errors* below.

## Decision 1 — where the routing key enters

PagerDuty authenticates with a 32-character integration key in the request
**body**. Every existing format authenticates with the URL itself. This is a new
credential shape, and it lands on the project's standing rules: a credential may
not appear on a command line, in `values.yaml`, in Helm's release history, or in
any log line, metric label, or error message.

**Decision: a new environment variable, `KUBEAGENT_ALERT_ROUTING_KEY`, with no
flag.**

There is no `--alert-webhook` flag today and that is not an oversight — the
comment at `internal/cli/watch.go:142` says a flag "would put it in the pod
spec's args and in `ps` output". The routing key inherits that rule verbatim.

Rejected alternatives:

- *Reuse `KUBEAGENT_ALERT_WEBHOOK` to carry the routing key for this format.*
  One credential channel, no new variable, no chart branch. Rejected because a
  variable named `WEBHOOK` holding a bare key is a name that lies, and because it
  consumes the only slot the endpoint override could occupy — which would leave
  the sink untestable against `httptest` and the format with no possible chaos
  coverage.
- *A `--pagerduty-routing-key` flag.* Most discoverable, and the reason the rule
  exists.

### Validation

`alert.New` requires the routing key to be non-empty when the format is
`pagerduty`, and rejects a value containing whitespace or any non-printable
byte — a pasted multi-line blob is a configuration mistake worth catching at
startup rather than at the first 400. The error names the variable and never
echoes the value.

It is **not** checked against PagerDuty's 32 characters. Pinning an upstream
length is a hostage to fortune, and it would force every fixture to be a 32-char
string that reads like a real key. Fixtures use `not-a-real-routing-key`, which
is both obviously fake and, at 22 characters, only possible because the length
check does not exist. The two decisions reinforce each other.

Setting the routing key under a non-pagerduty format is a stderr warning, not a
silent ignore, matching the existing `--alert-* flags ignored` warning at
`internal/cli/watch.go:151`.

## Decision 2 — the endpoint

`KUBEAGENT_ALERT_WEBHOOK` remains the destination URL for all four formats. For
`pagerduty` it becomes **optional**, defaulting to
`https://events.pagerduty.com/v2/enqueue`.

A URL supplied for this format with an empty path or `/` gets `/v2/enqueue`
appended, the same way `resolveURL` already fills in `/api/v2/alerts` for
Alertmanager. So an operator pointing at a non-default region or a proxy supplies
a host and nothing more.

The default is exposed as `alert.DefaultURL(Format) string` rather than being
buried inside `New`, because `internal/watch` logs the resolved endpoint at
startup (`internal/watch/watch.go:278`) and that line must not print an empty
string when the operator configured no URL.

## Decision 3 — the payload

```json
{
  "routing_key": "<ROUTING_KEY>",
  "event_action": "trigger",
  "dedup_key": "local/Deployment/shop/web",
  "payload": {
    "summary": "local/Deployment/shop/web: ImagePullBackOff",
    "source": "local",
    "severity": "error",
    "timestamp": "2026-08-05T10:04:11Z",
    "custom_details": {
      "cluster": "local",
      "kind": "Deployment",
      "namespace": "shop",
      "name": "web",
      "issues": ["ImagePullBackOff"],
      "reason": "new",
      "flapping": false
    }
  }
}
```

**`dedup_key` is `alertstate.Object.String()`** — `local/Deployment/shop/web`,
or `local/Node/worker-2` for a cluster-scoped object. Three properties earn it
the slot:

1. It is derived from identity, not from state, so it is stable across a daemon
   restart. That is what makes the restart re-trigger land on the open alert
   instead of opening a second one.
2. It is byte-identical to the string the `slack` encoder already sends, so it
   discloses nothing the alert path did not already disclose.
3. It is readable in PagerDuty's incident list, which a hash would not be.

PagerDuty caps it at 255 characters. A Kubernetes name may be 253 characters on
its own, so the cluster/kind/namespace/name concatenation can legally exceed the
cap. A flat truncation would silently merge two distinct objects onto one
incident — one outage swallowing another is exactly the failure the per-object
rollup exists to prevent. So an over-long key keeps a readable 246-character
prefix and appends a `crypto/sha256` suffix (8 hex characters, standard library,
no new dependency). Deterministic, collision-free in practice, still readable at
a glance.

**`severity` is the constant `"error"`.** kubeagent has no severity model on
`diagnose.Finding`; that is its own future slice, and building a second ranking
that lives only inside this encoder would fork it before it exists. `error` is
the same default Alertmanager's notifier picks, and it is the honest middle:
kubeagent knows something is broken, not how badly. When the severity slice
lands, this constant becomes its first consumer.

**`source` is `n.Object.Cluster`.** PagerDuty documents `source` as the location
of the affected system, and the cluster name is the truthful answer to that.

**`timestamp` is `n.FiringSince`**, so the PagerDuty alert is stamped when the
object actually broke rather than when the daemon noticed — and, because
`FiringSince` only ever moves earlier, the stamp survives a failure mode evolving
underneath it.

**`summary` is the object string plus the issue list**, truncated to 1024 runes.
Model-written prose never enters it: `summary` is the line a human reads at 3am
in a push notification, and it must stay scannable and bounded.

**`custom_details` is a Go struct, not a `map[string]any`** — the shape is
documented by the type, and it cannot pick up a stray key. `explanation` is
present only when `n.Text` is non-empty, JSON-encoded as data and never
concatenated into any string, per the standing rule on `Notification.Text`.

### Event action mapping

| Notification | `event_action` | Body |
|---|---|---|
| `StatusFiring`, reason `new` / `changed` / `repeat` | `trigger` | full payload |
| `StatusFiring`, reason `explanation` | `trigger` | full payload plus `custom_details.explanation` |
| `StatusResolved` | `resolve` | `routing_key`, `event_action`, `dedup_key` only |

A **resolve carries no payload**. PagerDuty requires only the three fields, and
it computes the incident duration itself — there is nothing for kubeagent to
add that would not be a second, disagreeing copy.

An **explanation is a `trigger` on the same `dedup_key`**, not a new event kind.
The object is still firing; an explanation is additional detail about one state,
not a transition to another, and `dedup_key` is precisely the mechanism for
attaching more information to an open alert. kubeagent makes no claim about how
PagerDuty surfaces the update — the daemon's `/explanations` endpoint and the
dashboard remain the authoritative place to read explanation prose.

### Re-send cadence

`DefaultRepeat(FormatPagerDuty)` returns **4h**, the same as `json` and `slack`.

No ceiling equivalent to `maxAlertmanagerRepeat` is added. That ceiling exists
because Alertmanager expires an alert `resolve_timeout` after the last POST, so a
slow cadence produces a false recovery. PagerDuty does not expire alerts, so
there is no failure to guard against, and inventing a limit would be a rule with
no reason behind it.

## Decision 4 — errors and privacy

**The sink's delivery path needs no change, and that is the design.**

- PagerDuty returns 202 on success. The existing `status < 300` branch accepts
  it.
- A bad routing key returns 400. The existing `status < 500` branch logs
  `alert for <object> rejected by <scheme://host>: HTTP 400 (not retrying)` and
  stops. Correct behaviour — a wrong credential is not retryable.
- The response body is discarded (`io.Copy(io.Discard, resp.Body)` at
  `internal/alert/sink.go:229`) and stays discarded. We do not read 4xx bodies to
  produce a better message. The gain is a slightly friendlier log line; the risk
  is a general rule that reads and logs arbitrary bytes from an operator-supplied
  endpoint, which is a new untrusted-text ingress in the one package whose whole
  job is not leaking things.
- The routing key travels in the request body only. It is never a URL component,
  so `redact.URL` and `sanitizeErr` already cover every path by which a URL
  reaches a log.
- The startup line at `internal/watch/watch.go:278` prints
  `redact.URL(resolved endpoint)` and never the key.

Read-only toward the cluster is unchanged: alerting adds one egress destination
and issues no cluster call. Separately, and this is a distinct promise, nothing
on this path makes a model call — the encoder receives prose the explain
pipeline already produced and does not produce any.

## Decision 5 — the Helm chart

**Zero new values.** `alerts.format` accepts `pagerduty`; the deployment template
wires the existing `alerts.existingSecret` / `alerts.secretKey` pair to
`KUBEAGENT_ALERT_ROUTING_KEY` instead of `KUBEAGENT_ALERT_WEBHOOK` when the
format is `pagerduty`. The operator changes two lines:

```yaml
alerts:
  enabled: true
  format: pagerduty
  existingSecret: kubeagent-pagerduty
  secretKey: routing-key
```

The Secret still holds the credential and it still never appears in
`values.yaml` or in Helm's release history — the property the existing design
bought and this one inherits without paying again.

`values.yaml` comments change (the format list, and what `existingSecret` holds
for each format) and `templates/deployment.yaml` gains a conditional. Templates
and values both move, so the chart takes a **MINOR** version bump, overriding
`scripts/bump-version.sh`'s patch bump.

## Components

| File | Change |
|---|---|
| `internal/alert/encode.go` | `FormatPagerDuty`; `encodePagerDuty`; `encode` grows a routing-key parameter; `dedupKey`; `validateRoutingKey`; the two payload structs |
| `internal/alert/url.go` | `DefaultURL(Format)`; `resolveURL` fills `/v2/enqueue` |
| `internal/alert/sink.go` | `Config.RoutingKey`; the format switch in `New`; `DefaultRepeat`; pass the key to `encode` |
| `internal/watch/watch.go` | `Config.AlertRoutingKey`; the enable gate in `newAlerter`; the startup log's resolved endpoint |
| `internal/cli/watch.go` | read `KUBEAGENT_ALERT_ROUTING_KEY`; enable on either credential; `--alert-format` help; two warnings |
| `deploy/helm/kubeagent/values.yaml` | comments only |
| `deploy/helm/kubeagent/templates/deployment.yaml` | one conditional on the env var name |
| `deploy/helm/kubeagent/Chart.yaml` | MINOR bump |
| `chaos/run.sh` | scenario 23 |
| `.github/workflows/fuzz.yml` | one new `(package, target)` matrix pair |
| docs | six files, below |

`internal/alert`'s import set gains `crypto/sha256` and `encoding/hex` — both
standard library. `go.mod` and `go.sum` do not change.

## Testing

TDD throughout: failing test first.

**Encoder table** — firing (`new`), firing (`changed`, two issues), `repeat`,
`explanation` with prose, `resolved`, a flapping object, a cluster-scoped object
with no namespace, and an object whose string exceeds 255 characters. Each case
asserts the full marshalled body, so a field that silently changes name fails.

**Escaping and injection** — an issue string and an explanation carrying `"`,
`\`, a newline and a control byte must round-trip through `encoding/json` as
data. The encoder is not a sanitization site; ingress already ran
`safetext.Line`.

**Routing-key validation** — empty, whitespace-bearing, control-byte-bearing,
and valid. Each error is asserted to *not* contain the input value.

**Endpoint resolution** — empty URL takes the default; a bare host gets
`/v2/enqueue`; an explicit path is left alone; a non-pagerduty format is
unaffected.

**Delivery** — against `httptest`: the routing key appears in the received body,
a 202 counts as success, a 400 is not retried, and a captured log buffer contains
neither the key nor more than `scheme://host`.

**Determinism** — the same notification encodes byte-identically twice.

**Fuzz** — a new `internal/alert/fuzz_test.go` carrying `FuzzEncodePagerDuty`,
in the style of the existing targets: arbitrary notification content must never
panic the encoder and must always produce valid JSON whose `dedup_key` is at most
255 characters. `internal/alert` has no fuzz target today, so this is the
package's first, and `.github/workflows/fuzz.yml` enumerates its
`(package, target)` pairs explicitly — the new pair is added there or the target
never runs a real campaign. Seed corpus entries use RFC 2606 domains and RFC 5737
addresses only.

**Race** — `go test -race ./internal/alert ./internal/watch ./internal/cli`.

## Chaos coverage — scenario 23

A new scenario, not a graft onto scenario 12, which already runs a daemon in
`json` format and would need a second receiver, a second port and a second daemon
inside one scenario to carry this.

Break one Deployment in its own namespace, run the daemon with
`--alert-format pagerduty` and `KUBEAGENT_ALERT_ROUTING_KEY` set to the fake key,
with `KUBEAGENT_ALERT_WEBHOOK` pointed at `chaos/alert-receiver.py`. Repair it.
Four assertions:

1. A body with `"event_action": "trigger"` was delivered.
2. `dedup_key` is present and identical across every delivered body for the
   object — the property the whole design rests on.
3. Repair delivered `"event_action": "resolve"` carrying that same `dedup_key`.
4. The routing key appears in **no** line of the daemon's log.

Assertion 4 is the one that could not be written any other way: it is a statement
about a live process's stderr, not about a function's return value.

The count moves **128 → 132** in the four documents that carry it: `CLAUDE.md`,
`chaos/README.md`, `website/docs/compatibility.md`, `website/docs/roadmap.md`.
All four move in the same commit.

Per the project's gate rule — chart templates and the watch daemon both change —
this slice takes the **full chaos gate**, not the lightweight smoke.

## Documentation

- `website/docs/features/watch-mode.md` — the `--alert-format` table row gains
  `pagerduty`; a new subsection under *Payloads* with the body and the field
  rationale; the credential paragraph gains the routing key. **The `### No
  severity` section must be amended**: it currently reads "kubeagent has no
  severity model, so no payload claims one", which stops being true the moment
  this format sends `severity: error`. The amended text keeps the claim about the
  model and states what the PagerDuty constant is and why.
- `website/docs/compatibility.md` — assertion count only. A flag accepting a new
  value is additive under the stable-CLI rules already written there and needs no
  new promise.
- `website/docs/roadmap.md` — Theme E's bullet drops "PagerDuty remains an open
  receiver"; assertion count.
- `deploy/README.md` — the chart snippet for a PagerDuty install.
- `CHANGELOG.md` — under `[Unreleased]`, `### Added`.
- `CLAUDE.md` — assertion count.

## What does not move

Each is checked by a test that already exists, so a violation fails the build
rather than the review:

- `go.mod` and `go.sum`.
- The six versioned JSON documents. An alert body is not one of them: the six are
  `report.ScanReport`, `gate.Verdict`, `rbacprofile.RulesDocument`,
  `rbacprofile.CheckDocument`, `watch.IssuesReport` and
  `watch.ExplanationsReport`. No `schemaVersion` bump, no `internal/jsonschema`
  change, no schema regeneration.
- `internal/report/testdata/golden-scan.txt`. The demo GIF and
  `website/docs/quickstart.md` are not regenerated.
- `internal/rbacprofile`'s `Feature` table and every generated RBAC manifest —
  this reads no new resource kind.
- `internal/alert`'s prohibition on importing `internal/remediate` or
  `internal/explain`.
- `internal/safetext` as the single ingress sanitizer. The encoder escapes for
  JSON, which `encoding/json` does; it does not sanitize.

## Constraints carried into the plan

- Every commit signed off (`git commit -s`); `main` enforces DCO. Verify with
  `bash scripts/dco-check.sh main HEAD`. No AI attribution anywhere — commits,
  code, comments, docs, changelog.
- Work stays on branch `pagerduty-receiver`; never commit to `main` directly.
- No secrets, credentials, private IPs or internal hostnames anywhere, including
  fixtures, seed corpora, chart examples and every doc example. RFC 5737
  addresses, RFC 2606 domains, `<ROUTING_KEY>` and `<WEBHOOK_URL>` placeholders.
  A fixture *named* like a credential is a defect even when its value is fake.
- Nothing kubeagent emits may carry more than `scheme://host`.
- `go test` runs with `-p 2`, never `-short`. CI's `go test -race ./...` stays
  green.
- `ANTHROPIC_API_KEY` is never set, referenced or exported by the chaos harness.
