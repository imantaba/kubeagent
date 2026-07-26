# Rate-limited on-incident `--explain` for the watch daemon

**Theme E, slice 4.** Status: design approved, ready for planning.

## Goal

When the watch daemon sees an object break, send a plain-English explanation of
*why* it broke as a follow-up to the page — bounded by an explicit call budget,
opt-in, and without the daemon reading anything from the cluster it was not
already reading.

This is the first slice to put an LLM call in the watch path. Everything below is
shaped by that: the read-only-toward-the-cluster invariant and the cost story are
the crux, not the prompt.

## Background

The daemon today is strictly read-only and LLM-free. It runs informers, re-runs
the deterministic evaluation on change and on a heartbeat, tracks issues
(`internal/watchstate`), rolls them up per object (`internal/alertstate`), and
delivers webhooks through a bounded retrying sink (`internal/alert`). Slice 3
added SLO burn-rate tracking (`internal/slo`) and, with it, the proven seam this
slice reuses: `alerter.enqueue` hands one already-built notification to the sink,
bypassing the issue tracker, so retry, backoff, the bounded queue and URL
redaction all apply unchanged.

`internal/explain` already holds the one-shot Claude call behind an unexported
`summarizer` interface, with an OpenAI-compatible local-endpoint backend
selectable via `NewFromConfig`. That interface is what makes the new code
testable without a network.

## Decisions

Four decisions fix the behavior; everything else follows from them.

1. **Destination: follow-up notification plus an endpoint.** The object alert
   fires immediately and LLM-free, exactly as today. The explanation is enqueued
   separately, seconds later, referencing the same object — so it lands under the
   original page. The latest explanation per object is also served over HTTP.
   Nothing in the alert path waits on the model, and the well-tested notify path
   is untouched. Cost: two messages per incident.

2. **Trigger: per object, on clean→flagged.** One call per object when it first
   goes bad, covering all of that object's findings at once. Matches the
   granularity of both the alert rollup and the notification. A Deployment that
   is simultaneously `ImagePullBackOff` and `Degraded` gets one coherent
   explanation, not two partial ones.

3. **Rate limit: per-object cooldown plus a global hourly budget.** Two guards
   for two distinct overspend modes — a flapping object re-transitioning, and a
   mass outage flipping many distinct objects at once. Over budget, the call is
   skipped rather than queued: a stale explanation is worse than none.

4. **Prompt input: the object plus cluster context the daemon already holds.**
   The flagged object's record, the other currently-flagged workloads, the
   cluster facts, and the SLO state. Zero new cluster reads, zero new RBAC —
   every field is already in the in-memory `scan.Result`. This lets the model say
   "web is one of 12 workloads failing to pull from registry Y" instead of
   re-deriving a root cause kubeagent had already attributed.

Rejected: per-issue triggering (fragments one broken workload into several calls
that each see half the picture); severity-gating (silent for ordinary workload
breakage, which is most of what on-call sees); storm coalescing (needs a second
prompt and notification shape, and has no hard ceiling on a steady trickle);
fetching pod logs and Events (needs new RBAC on a daemon whose pitch is a minimal
read-only role, and application logs routinely carry bearer tokens, connection
strings and customer data that no redaction pass reliably removes).

## Architecture

Two new units, split so the interesting logic is pure.

### `internal/oncall` (new package)

**`Throttle`** — pure, no I/O, fake-clock testable.

- `Allow(key, now) bool`: per-object cooldown **then** global token bucket.
- Exposes allowed / throttled counters and remaining budget.

**`Explainer`** — owns the throttle, a bounded job channel, one worker goroutine,
and a latest-per-object store.

- `Consider(delta, res, now)` runs on the reconcile goroutine and never blocks.
- `Start(ctx)`, `Close()`, `Latest()`, `Stats()`.

`Explainer` never receives a `kubernetes.Interface`. It takes the
already-computed `scan.Result` and nothing else. This is a type signature, not a
convention: a future contributor cannot add a cluster read to this package
without changing its constructor and tripping review.

### `internal/explain` (extend)

- `BuildIncidentPrompt(obj, res)` and `ExplainIncident(ctx, …)`.

All prompt-shaping knowledge stays in one package; `oncall` knows nothing about
models.

### `internal/watch` (wire)

- `Config` gains `Explain`, `ExplainModel`, `ExplainEndpoint`,
  `ExplainCooldown`, `ExplainBudget`.
- `applyResult` gains one line, `ex.Consider(d, res, now)`, placed after
  `tr.Observe` so the delta exists.
- New route `/explanations` beside `/issues`; new `kubeagent_explain_*` metrics.

### Data flow

```text
reconcile → applyResult → tr.Observe → Delta.New
                            ↓ dedupe by object
                        Throttle.Allow  ──reject──> throttled_total++
                            ↓ accept
                        bounded chan ──full──> dropped_total++
                            ↓
                     [worker goroutine]
                        explain.ExplainIncident (HTTP, per-call timeout)
                            ↓                    ↓ err
                  al.enqueue(follow-up)     failed_total++
                  latest[obj] = text
                            ↓
                        /explanations
```

The object alert still fires from `al.notify` on the reconcile goroutine,
LLM-free, before any of this.

### Why `Delta.New` is the right signal

`issueKeys` already emits a synthetic `"Degraded"` issue for a workload that is
`Flagged()` with no findings, so `watchstate.Delta.New`, deduped by
(Kind, Namespace, Name), *is* the object-level clean→flagged signal. No new
transition machinery.

The one subtlety: `Delta.New` also fires when an already-broken object acquires
an *additional* issue, which is not literally clean→flagged. The cooldown
absorbs this — a second finding two minutes later is inside the object's
cooldown, so no second call — and if a genuinely new failure mode appears an hour
later, that escalation deserves a fresh explanation anyway. So the coarser
predicate plus the cooldown produces the intended behavior with less machinery
than tracking the transition explicitly.

Non-workload issue kinds (`Service`, `Ingress`, `PVC`, `PodDisruptionBudget`, …)
are objects too and are treated uniformly: the key is object identity, and the
prompt carries the object's issue text plus cluster context.

## Invariants and egress

**Read-only toward the cluster.** Zero new informers, zero new RBAC verbs, no
change to the chart's Role. Enforced structurally, as described above.

**Package doc amendment.** `internal/watch`'s package comment currently ends
"No writes, no LLM." It becomes:

> No writes. The LLM is opt-in, off by default, and sees only findings the daemon
> has already collected — it triggers no additional cluster reads and needs no
> additional RBAC.

"Strictly read-only toward the cluster" stays literally true; the sentence that
stops being true gets corrected rather than left to rot.

**What leaves the cluster.** Structured fields only, the same discipline
`BuildInventoryPrompt` already enforces: kind, namespace, name, status,
ready/desired, restart counts, finding issue strings, correlation hints, cluster
facts, SLO state. Never pod specs, environment variables, ConfigMap or Secret
contents, registry credentials, or logs. Enforced by the prompt builder taking
typed values, not by a redaction pass over free text.

**Model output is untrusted text.** It is JSON-encoded into the notification
payload, never concatenated into Slack markup. A workload named to look like
markup, or a model emitting control characters, must not be able to alter message
structure. The existing `alert` formatters encode via `encoding/json`; the
requirement is that the follow-up path does not bypass that.

**The webhook URL stays a credential.** Follow-up notifications ride the existing
sink — same retry, same bounded queue, same `alert.RedactURL`. No log line,
error, metric label, results file, rendered manifest, or doc example may carry
more than `scheme://host`.

**Opt-in and fail-fast.** Off by default. `watch --explain` without
`ANTHROPIC_API_KEY` (or `KUBEAGENT_EXPLAIN_ENDPOINT`) is a startup error,
validated alongside `--slo-target` *before* the metrics server binds — a config
error must not hide behind a cache sync that never completes.

**`/explanations` exposure.** Same sensitivity class as the existing `/issues` —
namespace and workload names, failure descriptions — on the same unauthenticated
metrics port. No new exposure class, but the docs must say plainly that enabling
`--explain` puts model-written incident prose on that port.

## Throttle semantics

**Check order.** Cooldown first (free), budget second (consumes a token).
Reversed, a cooldown-blocked object would burn budget it never spends.

**Per-object cooldown.** `map[objectKey]time.Time`, stamped only on *allowed*
calls. A budget-denied object gets no stamp — it was never explained, so it stays
eligible. Entries are pruned once older than the cooldown, bounding the map at
roughly `budget-rate × cooldown` ≈ 20 entries at defaults.

**Global budget: token bucket, continuous refill.** Capacity 20, refill 20/hour.
Capacity-as-burst is deliberate: a genuine mass outage should get its 20
explanations immediately, then drip at one per three minutes. A fixed hourly
window would instead have a boundary at which spend doubles.

**Budget is consumed at attempt, not at success.** A model endpoint returning 500
must not grant unlimited retries.

**Two distinct drop counters.** `throttled_total` means policy said no (cooldown
or budget). `dropped_total` means the worker was busy and the bounded queue (size
8) was full. Different causes and different operator responses; collapsing them
would hide a slow endpoint behind a rate-limit reading.

## Configuration

Flags on `watch`:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--explain` | `false` | enable on-incident explanations |
| `--explain-cooldown` | `1h` | per-object minimum gap |
| `--explain-budget` | `20` | calls per hour; also the bucket capacity |
| `--model` | `$KUBEAGENT_MODEL` or `claude-opus-4-8` | same resolution as `scan --explain` |

Environment reused unchanged: `ANTHROPIC_API_KEY`,
`KUBEAGENT_EXPLAIN_ENDPOINT`, `KUBEAGENT_EXPLAIN_API_KEY`, `KUBEAGENT_MODEL`.

Validation, with the other fail-fast checks: budget ≥ 1, cooldown ≥ 0, and the
key/endpoint presence check. A cooldown of exactly `0` is legal and disables the
per-object gap, leaving the budget as the only limit — useful for the chaos
scenario and for operators who want every transition explained until the budget
runs out.

`main.go`'s `watch` usage string gains the new flags. A usage string that omits
half a subcommand's flags is a defect, not a cosmetic gap.

**Helm.** New values for the flags, plus the API key wired as a `secretKeyRef` to
a Secret the operator creates. The chart must never accept a plaintext key in
`values.yaml` — a values file lands in Git, in `helm get values`, and in CI logs.

Chart templates change, so: **minor chart bump, and the release gate is the full
chaos run.**

## Failure isolation and lifecycle

**Cold-start suppression.** The daemon's first reconcile sees every pre-existing
issue as `New`. Without a guard, a restart — or a CrashLoop — spends the whole
budget re-explaining problems nobody just caused. The initial snapshot seeds the
tracker and produces **no** explanations; only transitions observed after it
trigger. Same discipline as the SLO slice, where chaos scenario 13 exists
precisely to prove a cold daemon must not page.

**Shutdown ordering is load-bearing.** The explainer is a *producer* for the
alert sink, so it must be fully stopped before the sink closes; enqueueing onto a
closed sink is a panic, not a dropped message. `Run` already encodes one such
ordering. The explainer slots inside it:

```text
defer al.sink.Close()      // deferred 1st → runs 4th  (sink outlives everything)
defer stopAlerts()         // deferred 2nd → runs 3rd
defer ex.Close()           // deferred 3rd → runs 2nd  (drains, bounded wait)
defer stopExplain()        // deferred 4th → runs 1st  (cancels in-flight HTTP)
```

`Close()` waits for the worker for at most **5s** — matching the existing
`srv.Shutdown` grace in `Run` — so a hung endpoint delays shutdown by that,
not forever.

**Per-call timeout: 60s**, derived from the worker context, not configurable in
this slice. A slow endpoint costs one worker slot for that long; the bounded
queue absorbs the backlog into `dropped_total`.

**Failure paths, all non-fatal:**

| Failure | Behavior |
| --- | --- |
| model returns error | log (never the key, never a full URL), `failed_total++`, no notification |
| model returns empty text | same as error — an incident explanation is never legitimately empty |
| endpoint permanently broken | bounded to 20 wasted calls/hour by the budget; visible as a climbing `failed_total` |
| queue full | `dropped_total++`, reconcile unaffected |
| API key revoked mid-run | the error path; the daemon keeps diagnosing and alerting |

The through-line: nothing here can stop the daemon diagnosing or alerting. The
deterministic core stays usable with the model endpoint dead — the same promise
`scan` makes offline.

## Notification and endpoint shapes

**One new `Reason`.** `alertstate` gains
`ReasonExplanation Reason = "explanation"`. Receivers can filter it; existing
consumers that switch on `Reason` see a value they can ignore rather than a state
transition that never happened.

**One new `Notification` field:**

```go
Text string `json:"text,omitempty"` // explanation prose; set only by ReasonExplanation
```

The explanation is prose and does not belong in `Issues`, which is documented as
a sorted unique set of issue names; putting paragraphs there would make the field
lie to every existing formatter.

The follow-up notification is `StatusFiring` + `ReasonExplanation`, the same
`Object`, `Issues` copied from what triggered it, and `Text` set. It goes through
`al.enqueue`.

**`/explanations`,** mirroring `/issues`:

```json
{
  "explanations": [
    { "kind": "Deployment", "namespace": "shop", "name": "web",
      "issues": ["ImagePullBackOff"],
      "explainedAt": "2026-07-26T10:04:12Z",
      "model": "claude-opus-4-8",
      "text": "..." }
  ],
  "stats": { "allowedTotal": 3, "throttledTotal": 30,
             "failedTotal": 0, "droppedTotal": 0 }
}
```

**Bounded, explicitly.** Latest-per-object, capped at 100 entries with
oldest-eviction plus a 24h maximum age. The cooldown map's pruning rule does not
apply — an explanation stays useful after its cooldown expires, so it needs its
own bound. An unbounded map in a process designed to run for months is precisely
the bug class this codebase keeps catching.

**Metrics** (prefix follows the existing `kubeagent_<subsystem>_…` convention;
there is no `watch_` segment in any current metric name):

```text
kubeagent_explain_allowed_total       counter
kubeagent_explain_throttled_total     counter   # cooldown or budget
kubeagent_explain_failed_total        counter   # model error or empty
kubeagent_explain_dropped_total       counter   # queue full
kubeagent_explain_budget_remaining    gauge     # tokens left in the bucket
```

`budget_remaining` as a gauge is what makes "why did my incident go unexplained?"
answerable from a dashboard rather than from logs.

## Testing

**`oncall.Throttle` — pure, fake clock.** Cooldown holds a re-transition; the
bucket allows a burst of `capacity` then drips; a cooldown-blocked object does
**not** consume a token (the check-order property, invisible unless asserted
directly); a budget-denied object stays eligible rather than being stamped;
pruning keeps the map at its stated bound.

**`oncall.Explainer` — fake summarizer, fake clock.** Reuses the existing
unexported-interface pattern from `internal/explain`, so no network in unit
tests. Cases: a cold-start snapshot produces zero calls; two new issues on one
object produce one call; a full queue increments `dropped` and not `throttled`; a
summarizer error produces `failed++` and **no** notification; success enqueues
exactly one notification with `ReasonExplanation` and non-empty `Text`; the
latest-map evicts at its cap.

**Shutdown ordering.** A real-goroutine test asserting the explainer stops before
the sink closes, and that a call in flight at cancel cannot enqueue onto a closed
sink. This is the failure the defer chain exists to prevent, and a comment does
not survive a refactor.

**`explain.BuildIncidentPrompt` — the egress guard.** Positive assertions that
the object, its findings, its correlation hints and the cluster context are
present. And the one that matters: plant a secret-shaped value in a field the
builder is not supposed to send, then assert the prompt does **not** contain it.
A positive-only test would pass just as happily if the builder started
serializing whole pod specs.

**`internal/watch` wiring — fake clientset.** `--explain` off by default produces
zero calls; validation errors fail fast before the listener binds.

**Chaos scenario 14 — non-vacuous and credential-free.** Scenario 12 already runs
the watch daemon from the host and curls `/issues`, so: run `watch --explain`
against the Kind cluster with `KUBEAGENT_EXPLAIN_ENDPOINT` pointed at a local
stub OpenAI-compatible server, break two workloads with `--explain-budget 1`, and
assert the end-to-end shape — one explanation delivered, one throttled,
`/explanations` carrying the text, `kubeagent_explain_throttled_total` at 1.

This exercises the full path with **no API key in the shell**, honoring the
standing rule while still being a real test rather than a startup-error check.
The Anthropic backend itself stays covered by unit tests.

**Expectation strings must assert what actually happens.** `record()` in
`chaos/run.sh` asserts nothing, so a scenario's expectation string *is* the test.
Scenario 14's string states the invariant it genuinely proves and names anything
it does not.

## Documentation

- `internal/watch` package comment (above).
- `website/docs/` — the watch daemon page gains an on-incident explain section
  covering opt-in, cost, the two rate limits, `/explanations` exposure on the
  unauthenticated metrics port, and the Secret-based key wiring.
- `website/docs/roadmap.md` — move slice 4 to Shipped when it lands.
- `CHANGELOG.md` under `[Unreleased]`.
- `docs/go-concepts.md` — token-bucket rate limiting and the
  producer-before-consumer shutdown ordering are both new concepts for this
  codebase; each gets an entry in the established style (an everyday example
  first, then the kubeagent example).

## Out of scope

- Explaining *resolutions* ("why did this recover?").
- Any agentic or tool-using loop in the daemon; `--investigate` stays
  `scan`-only.
- Persisting explanations across restarts.
- Cluster-level storm coalescing.
- Making the model's output actionable beyond prose (no suggested `--fix`).
