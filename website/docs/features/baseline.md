# Restart-rate baseline

`kubeagent baseline capture` records what each workload's restart rate
normally is on *this* cluster. `kubeagent scan --baseline` and `kubeagent gate
--baseline` compare a later run against that captured file and report
workloads that are restarting far more than their own normal — not more than
some fixed number picked in advance, but more than what this specific
workload, on this specific cluster, has actually been doing.

```bash
kubeagent baseline capture > cluster-baseline.json
```

## What it honestly measures

**This measures restarts over the lifetimes of the pods present when the
sample was taken. It is not long-term history. A workload whose pods were all
recreated an hour before capture shows only what those pods have done
since.**

There is no database behind it, no ring buffer of yesterday's numbers — a
capture reads `Pod` objects that exist right now and reduces them to one rate
per workload. If every pod under a Deployment restarted five minutes before
you ran `baseline capture`, the baseline records exactly that: a few minutes
of near-zero pod-seconds and whatever restarts happened in them, not
"normal" in any deeper sense.

[Capacity hints](capacity.md) draws the same line for metrics-server: a `GET`
on `/apis/metrics.k8s.io/v1beta1/pods` returns one sample and keeps nothing
before or after it, so kubeagent never calls a request "over-requested" from
a single reading. The restart-rate baseline has the same shape of limit for
the same reason — a snapshot is a snapshot — so recapture periodically if you
want the learned rate to track how the cluster actually behaves over time.
Nothing does that for you automatically; see [What it is not](#what-it-is-not).

## The workflow

```bash
# 1. Capture. Read-only — see Guarantees below.
kubeagent baseline capture > cluster-baseline.json

# 2. Review it. It names your namespaces and workloads in the clear.
cat cluster-baseline.json

# 3. Commit it, like any other file whose diffs you want to see.
git add cluster-baseline.json && git commit -m "capture restart-rate baseline"

# 4. Compare later runs against it.
kubeagent scan --baseline cluster-baseline.json
kubeagent gate --baseline cluster-baseline.json
```

Nothing captures automatically and nothing merges captures together — step 1
is something an operator runs and reviews, not a background job. See
[What it is not](#what-it-is-not).

A capture looks like this:

```json
{
  "schemaVersion": "1.0",
  "capturedAt": "2026-01-15T08:00:00Z",
  "minPodAgeSeconds": 3600,
  "workloads": [
    {
      "kind": "Deployment",
      "namespace": "prod",
      "name": "api",
      "restartsPerHour": 0.4,
      "pods": 3,
      "observedSeconds": 45000
    },
    {
      "kind": "StatefulSet",
      "namespace": "prod",
      "name": "cache",
      "restartsPerHour": 0.1,
      "pods": 2,
      "observedSeconds": 72000
    }
  ]
}
```

## The maths

One rate per workload:

```text
RestartsPerHour = sum(restarts of counted pods) / (sum(age of counted pods) / 3600)
```

A pod counts only when it is at least `--min-pod-age` old (default `1h`).
An excluded pod leaves **both** sides of that fraction — its restarts are not
added to the numerator, and its age is not added to the denominator — because
a pod that has existed for thirty seconds with two restarts implies 240
restarts/hour, and averaging that into an otherwise-quiet workload would
swamp every older pod's contribution with noise from one still starting up.

A workload with no counted pods gets **no entry** in the document. Zero
restarts and "not measured" are different facts, and collapsing them into a
rate of `0.0` would let a workload that was simply too young to sample at
capture time look, later, like it deviated from a normal that was never
actually observed.

## Two thresholds, not one

A workload deviates only when **both** of these hold:

```text
current >= baseline × factor          (default factor: 3.0)
current − baseline >= floor           (default floor: 0.5 restarts/hour)
```

Both conditions exist because each one alone breaks in a different direction:

- The **factor** test alone would flag `cache` the moment it had a single
  restart, because any positive rate clears "3× a baseline of zero" — the
  multiplicative test is trivially satisfied when the baseline is zero. The
  floor is what actually decides whether a previously silent workload's
  first restarts count as a deviation.
- The **floor** test alone would flag a workload that already restarts
  often: one going from 20.0 to 20.5 restarts/hour clears a 0.5/hour floor
  while barely moving relative to its own history.

Requiring both means a deviation has to be large **relative to the
workload's own normal** and large enough **in absolute restarts/hour** to be
worth paging someone about.

Only increases deviate. A workload restarting less than its baseline is
never reported — nobody is paged for a thing improving.

A workload the baseline has never seen, and a workload the baseline has seen
but the cluster no longer has, are both counted and neither is ever flagged:
there is no learned rate to compare a new workload against, and a gone
workload has no current rate to compare. `scan`'s text output reports both
counts in its footer line, alongside how many workloads were actually
compared.

## The flags and environment variables

| Flag | Env var | Default | Command | Meaning |
|---|---|---|---|---|
| `--baseline <file>` | — | (unset — comparison off) | `scan`, `gate` | path to a captured baseline document |
| `--baseline-factor` | `KUBEAGENT_BASELINE_FACTOR` | `3.0` | `scan`, `gate` | the multiplicative threshold |
| `--baseline-floor` | `KUBEAGENT_BASELINE_FLOOR` | `0.5` | `scan`, `gate` | the absolute threshold, in restarts/hour |
| `--min-pod-age` | `KUBEAGENT_BASELINE_MIN_POD_AGE` | `1h` | `baseline capture` | how old a pod must be to count toward the rate |

`--baseline-factor` and `--baseline-floor` only do anything when `--baseline`
is also set; without it, `scan` and `gate` run exactly as they did before
this feature existed.

## In `gate`

A deviation becomes a `Finding` at `Info` — the level reserved for a signal
that is not a match on a concrete, named failure mode, only an inference from
a learned rate. It is reported at every `--fail-on` setting, but it fails the
gate only at `--fail-on info`, which is the operator opting in explicitly.
Because `--fail-on` defaults to `critical`, adding `--baseline` to an
existing `gate` invocation changes no pipeline's pass/fail behavior until
that operator asks for it.

## Guarantees

`kubeagent baseline capture` is **read-only toward the cluster** — it issues
`List` calls only, the same calls `scan` already makes, nothing else — and it
**makes no model call**. Those are two separate promises: read-only describes
what it does to the cluster, no-model-call describes what it does with the
result, and neither implies the other.

It needs **no RBAC grant beyond what `scan` already has** — there is no
`deploy/rbac-baseline.yaml`, because there is nothing for one to grant.

It **writes no file**. The document goes to stdout, so the operator sees it,
and decides where it goes, before it exists anywhere on disk.

## What it is not

- **It has no HTML, TUI, MCP, or dashboard surface.** The document is JSON on
  stdout; there is no rendering of it anywhere else.
- **It is not wired into the `watch` daemon.** `watch` does not capture, does
  not compare, and carries no `--baseline` flag.
- **It tracks no inventory drift.** A workload appearing or disappearing
  between a capture and a later run is counted (see [Two
  thresholds](#two-thresholds-not-one)), never reported as drift in its own
  right — that is a different question this feature does not answer.
- **It has one dimension.** Restart rate, and nothing else. No CPU, no
  memory, no latency, no error rate — those would each be their own learned
  normal, and none of them exist here.
- **Nothing captures automatically.** There is no scheduled job, no
  in-cluster capture, no flag that captures on a timer. An operator runs
  `baseline capture` and decides when.
- **There is no multi-baseline merge.** `--baseline` reads exactly one file.
  Combining multiple captures, or captures from multiple clusters, is a step
  an operator would take outside kubeagent, if at all.

## The schema

The document is the seventh of kubeagent's [versioned JSON
documents](json-schema.md), published at
[`../schemas/baseline-v1.json`](../schemas/baseline-v1.json).

```bash
kubeagent schema baseline
```

prints the same schema straight from the running binary — no cluster, no
kubeconfig, nothing else needed.
