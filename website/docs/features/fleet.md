# Fleet sweep

An operator with three clusters runs `kubeagent scan` three times. An
operator with three hundred cannot. `kubeagent fleet` sweeps every selected
kubeconfig context in bounded parallel, runs the same evaluation `kubeagent
gate` already runs against each one, and prints one row per cluster, worst
first, with an exit code a CI job can read. The per-cluster pipeline is
exactly gate's — `scan.Evaluate` then the pure `gate.Decide` — so a fleet
sweep and a single-cluster `gate` run can never disagree about the same
cluster.

```bash
kubeagent fleet --all-contexts --match 'example-*'
```

```text
FLEET  5 clusters, 2 failing, 1 unreachable

CLUSTER            VERDICT       CRIT  WARN  INFO  TOP ISSUES
example-ap-1       unreachable                     connecting to the cluster
example-staging-2  inconclusive     0     1     0  (2 blind spots)
example-eu-1       fail             4     2     0  CrashLoopBackOff, ImagePullBackOff
example-us-3       fail             1     5     1  Unschedulable
example-eu-2       pass             0     0     0

verdict: inconclusive (exit 2)
```

Every selected cluster gets a row. There is no elision: a three-hundred-cluster
sweep is three hundred rows, because the summary that keeps that readable is
the row itself, not a cut-off.

## Guarantees

`kubeagent fleet` is **read-only toward every cluster it sweeps** — `get` and
`list` only, the exact calls the per-cluster `gate` evaluation it reuses
already makes against that one context. There is no write of any kind and no
`--fix` path. Separately: fleet **makes no LLM call**. Those are two separate
promises — read-only describes what it does to each cluster, no-model-call
describes what it does with the result — and neither implies the other.

## Flags

| Flag | Default | Env var | Meaning |
|------|---------|---------|---------|
| `--kubeconfig` | `$KUBECONFIG` or `~/.kube/config` | — | path to kubeconfig |
| `--context` | (none — repeatable) | — | kubeconfig context to sweep |
| `--all-contexts` | `false` | — | sweep every context the kubeconfig defines |
| `--match` | (empty) | — | with `--all-contexts`: only contexts whose name matches this glob |
| `--fail-on` | `critical` | — | severity that fails the sweep: `critical`, `warning` or `info` |
| `--workers` | `8` | `KUBEAGENT_FLEET_WORKERS` | clusters read concurrently |
| `--cluster-timeout` | `60s` | `KUBEAGENT_FLEET_CLUSTER_TIMEOUT` | per-cluster budget |
| `--output` | `text` | — | output format: `text` or `json` |
| `-n`, `--namespace` | all namespaces | — | namespace to judge |

`--workers` is clamped to 1–64 inside the sweep itself, whatever value is
passed: three hundred concurrent per-cluster reads from one process is a
thundering herd the pool refuses to create even if asked. A cluster whose
read overruns `--cluster-timeout` is reported `unreachable` with reason
`"timed out"`, and every other cluster keeps going.

Deliberately absent: the opt-in advisory flags `gate` itself does not expose
either — `--logs`, `--security`, `--certs`, `--operators`, `--drift`,
`--capacity`, the three health probes, `--explain`, `--investigate`,
`--fix`, `--rollback`. `gate` already builds a fixed `scan.Options` with none
of them set, so a fleet sweep inherits a bounded per-cluster read for free —
multiplying a proxied per-node read by three hundred clusters is a shape this
command will not offer.

## Exit codes

| Verdict | Code | When |
|---------|------|------|
| `pass` | `0` | Every selected cluster was reached and passed. |
| `fail` | `1` | No cluster was unreachable or inconclusive, and at least one cluster's own verdict is `fail`. |
| `inconclusive` | `2` | At least one selected cluster was unreachable, or at least one cluster's own verdict is `inconclusive`. |
| usage | `4` | Bad flags, a bad cluster selection, or a context whose client could not be built — discovered before any cluster was read. |

`fleet` reuses `gate`'s exit-code constants unchanged, so one mental model
covers both — but it never produces `3`: that code is `gate`'s `--wait-for`
timeout, and `fleet` has no `--wait-for` equivalent.

**`inconclusive` outranks `fail`.** A single cluster whose own verdict is
`inconclusive` — or a single cluster that could not be reached at all — makes
the whole sweep `inconclusive`, no matter how many other clusters failed
outright. Only the ordering of those two outcomes carries over from
`gate.Decide` — not its case list, which reaches `fail` by two routes and
evaluates one of them ahead of the blind case. The reasoning is the same
either way: when kubeagent could not see enough, a `fail` verdict might
understate what is actually wrong, so the honest answer is that the run could
not judge. Inverting this at fleet scope would let one unreachable cluster
hide behind another cluster's failure.

## Cluster selection

| You pass | fleet sweeps |
|----------|--------------|
| `--context NAME` (repeatable) | exactly the contexts you named, in the order given |
| `--all-contexts` | every context the kubeconfig defines |
| `--all-contexts --match 'example-*'` | …narrowed to context names matching the glob |
| neither flag | the kubeconfig's current context, if it has one |

`--match`'s glob is not `path.Match`: kubeconfig context names routinely
contain a `/` — OpenShift generates them in the shape
`default/api-example-com:6443/kube:admin` — and `path.Match`'s `*` will not
cross one. `--match` uses the same two-metacharacter matcher `--policy` uses
for image references (`*` matches any run of characters including `/`; `?`
matches exactly one byte; everything else is literal), shared through the
stdlib-only `internal/glob` package.

The command refuses anything ambiguous rather than guessing:

- `--match` without `--all-contexts` is a usage error: there is nothing to
  filter, and silently implying `--all-contexts` would turn a typo into a
  fleet-wide read.
- `--context` together with `--all-contexts` is a usage error: one says
  "these", the other says "all".
- Neither flag, when the kubeconfig names no current context, is a usage
  error too — there is nothing to fall back to.
- A `--match` that matches nothing, or a `--context` name the kubeconfig does
  not define, is a usage error naming what went wrong.
- Any selection that resolves to zero clusters is a usage error, never a
  pass. A sweep that found nothing to sweep must never exit `0` — that would
  look like good news.

Every one of these is exit `4`, discovered before any cluster is touched.
Building a client for a selected context is the same class of failure, and for
a precise reason: `cluster.NewClient` performs no network I/O at all — it reads
the kubeconfig and constructs a clientset. A failure there is therefore always a
configuration defect, never a reachability event, and re-running will not change
it. A cluster that is merely gone builds a client without complaint and lands in
`unreachable` on the graceful path, where it belongs.

## `--output json`

The same sweep as above, as JSON — the eighth of kubeagent's [versioned JSON
documents](json-schema.md):

```json
{
  "schemaVersion": "1.0",
  "verdict": "inconclusive",
  "exitCode": 2,
  "failOn": "critical",
  "clusters": [
    {
      "context": "example-staging-2",
      "verdict": "inconclusive",
      "critical": 0,
      "warning": 1,
      "info": 0,
      "blindspots": 2
    },
    {
      "context": "example-eu-1",
      "verdict": "fail",
      "critical": 4,
      "warning": 2,
      "info": 0,
      "blindspots": 0,
      "topIssues": [
        "CrashLoopBackOff",
        "ImagePullBackOff"
      ]
    },
    {
      "context": "example-us-3",
      "verdict": "fail",
      "critical": 1,
      "warning": 5,
      "info": 1,
      "blindspots": 0,
      "topIssues": [
        "Unschedulable"
      ]
    },
    {
      "context": "example-eu-2",
      "verdict": "pass",
      "critical": 0,
      "warning": 0,
      "info": 0,
      "blindspots": 0
    }
  ],
  "unreachable": [
    {
      "context": "example-ap-1",
      "reason": "connecting to the cluster"
    }
  ]
}
```

`clusters` and `unreachable` are separate arrays, deliberately: a consumer
filtering `clusters[]` for failures must not have to know that some entries
have no counts at all. The text table above interleaves them into one view
for a different reason — a reader scanning rows top-down should not have to
find a second table below the fold to learn that a cluster went unjudged.
Same data, shaped for what each consumer needs. A passing cluster carries no
`topIssues` key at all: `omitempty` drops it rather than writing an empty
array.

## What the report may name

**It may name:** kubeconfig context names, and issue kinds
(`CrashLoopBackOff`, `ImagePullBackOff`, `Unschedulable`, and so on). A
context name is the operator's own label for their own cluster — it is the
only thing that can answer "which one". This is not a new exposure:
`internal/mcp`'s `list_contexts` tool already serves context names to a
remote caller by design, and the watch daemon has carried one as a `cluster`
metric label since its multi-cluster hub shipped.

**It may never name:** a kubeconfig path or any other filesystem path; a
full API server URL (nothing beyond `scheme://host`, and this slice carries
no server URL at all); a Kubernetes node name; a namespace, pod, or workload
name.

That last exclusion is not a filter applied to a report that could otherwise
carry more — it is why the report is a summary at all. A `ClusterSummary`
carries counts and issue kinds, and neither of those can hold an object
name, so the exclusion is structural: there is no field to accidentally
populate with one, and no filter for a future change to accidentally bypass.

`Unreachable.Reason` comes from a fixed, two-entry vocabulary —
`"connecting to the cluster"`, `"timed out"` — never from `err.Error()`,
which can carry a server URL or a filesystem path. The underlying error is
dropped rather than routed somewhere safer: a fleet report is written to be
forwarded, and there is no stream on which `fleet` could publish a per-cluster
error without also publishing it to whoever receives the report. That is a
deliberate trade of detail for safety, and it leaves a gap the fixed vocabulary
cannot fill. When you need the detail, run the single-cluster command against
that one context:

```bash
kubeagent gate --context example-eu-1
```

`gate` reports the failure in full, to the operator running it.

## Unreachable is not the same as refused

A cluster kubeagent reached but was not allowed to read fully is *not*
unreachable. The per-cluster evaluation still runs to completion, the
refusal is recorded as a blind spot, and that cluster gets an ordinary row
with a non-zero blind-spot count and an `inconclusive` verdict. `Unreachable`
is reserved for a cluster that produced no result at all — a client that
could not connect, or a read that did not finish inside
`--cluster-timeout`. Both roads lead to the same fleet-level verdict,
`inconclusive` at exit `2`, but they are different facts, and the report
says which: a blind-spot count in a `ClusterSummary` row, or a named reason
in `unreachable`.

Every per-cluster evaluation runs with no waived reads: `fleet` never passes
an `--allow-partial-read` equivalent through to the `gate` evaluation it
reuses, so a blind spot always costs that cluster an `inconclusive` verdict.
An operator who has already decided one specific missing grant is
acceptable runs `kubeagent gate --allow-partial-read` directly against that
one context instead.

## The schema

The document is the eighth of kubeagent's [versioned JSON
documents](json-schema.md), published at
[`../schemas/fleet-v1.json`](../schemas/fleet-v1.json).

```bash
kubeagent schema fleet
```

prints the same schema straight from the running binary — no cluster, no
kubeconfig, nothing else needed.

## Not in this slice

Deliberately absent, and not planned for this slice:

- **Cross-cluster correlation** — "the same image is failing in all three".
  This slice is the first thing in the repo that holds many clusters'
  findings at once, which makes that question possible to ask, but it does
  not attempt to answer it: each row is one cluster's own verdict, computed
  independently of every other row.
- `--output sarif`, `--policy` and `--baseline` at fleet scope. Each is
  plausible for a later slice; none is needed to answer "which of my
  clusters are broken".
- **A fleet file** listing clusters with labels, outside a kubeconfig.
  Selection stays kubeconfig-derived only.
- **Per-cluster detail.** `fleet` says which cluster; the operator then runs
  the `scan` or `gate` they already have against that one context.
