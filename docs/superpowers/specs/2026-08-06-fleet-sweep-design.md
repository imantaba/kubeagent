# `kubeagent fleet` — a one-shot sweep across many clusters

**Status:** design approved, not yet implemented
**Roadmap:** post-1.0 item 2, "fleet-scale (hundreds of clusters)"
([website/docs/roadmap.md:559](../../../website/docs/roadmap.md))
**Slice:** 1 of N. Ships useful on its own.

## The problem

An operator with three clusters runs `kubeagent scan` three times. An operator
with three hundred cannot. The one question that scales — *which of my clusters
are broken right now?* — has no answer today, because every one-shot surface is
single-cluster by construction: `scan` and `gate` each take exactly one
`--context` ([internal/cli/scan.go:88](../../../internal/cli/scan.go),
[internal/cli/gate.go:78](../../../internal/cli/gate.go)).

Multi-cluster already shipped, but only for the daemon. Theme E's multi-cluster
hub gave `kubeagent watch` a `watch.Target` per cluster, one informer set each,
and a `cluster` label on every metric series. Its spec closes with an explicit
Out of scope
([2026-07-26-multi-cluster-hub-design.md:310-318](2026-07-26-multi-cluster-hub-design.md))
naming three things it did not attempt:

1. cross-cluster correlation — "a separate feature that this slice makes
   possible but does not attempt";
2. multi-cluster `scan` — "The one-shot CLI stays single-cluster";
3. any hub-to-daemon protocol.

This slice closes (2). It does not attempt (1) or (3).

Scaling the *daemon* to hundreds of clusters is a different problem and is not
this slice: each target costs a full informer set, so the ceiling is memory, and
the honest answer today remains "run more than one daemon". A one-shot sweep has
no such ceiling — it holds one cluster's read at a time per worker and drops it.

## What ships

One new command:

```bash
kubeagent fleet --all-contexts --match 'prod-*'
```

It sweeps every selected cluster in bounded parallel, runs the **same evaluation
`kubeagent gate` already runs** against each, and prints one row per cluster,
worst first, with an exit code a CI job can read.

```text
FLEET  17 clusters, 2 failing, 1 unreachable

CLUSTER                 VERDICT        CRIT  WARN  INFO  TOP ISSUES
prod-ap-1               unreachable                      connecting to the cluster
staging-2               inconclusive      0     1     0  (2 blind spots)
prod-eu-1               fail              4     2     0  CrashLoopBackOff, ImagePullBackOff
prod-us-3               fail              1     5     1  Unschedulable
prod-eu-2               pass              0     0     0
[…12 more passing]

verdict: inconclusive (exit 2)
```

## Architecture

Three calls per cluster, all of which already exist and two of which are pure:

```text
internal/cli/fleet.go                   flags, kubeconfig, client construction
        │  []fleet.Target{Name, Client}
        ▼
internal/fleet                          bounded sweep, per-cluster verdict, render
        │  per cluster:
        │      scan.Evaluate(ctx, client, opts)   →  scan.Result      (I/O)
        │      gate.Decide(res, opts)             →  gate.Verdict     (pure)
        │      summarize(verdict)                 →  ClusterSummary   (pure)
        ▼
fleet.Report  →  JSON (schemaVersion 1.0)  |  text table
```

`gate.Decide` is already a pure function of a `scan.Result`
([internal/gate/gate.go:172](../../../internal/gate/gate.go)), and
`scan.Evaluate` already takes a `kubernetes.Interface`
([internal/scan/scan.go:166](../../../internal/scan/scan.go)). The sweep adds no
diagnosis logic whatsoever — that is the point of choosing the full gate
evaluation over a second, shallower path that would drift from it.

### Why the client construction lives in `internal/cli`

`internal/fleet` receives `[]Target{Name string, Client kubernetes.Interface}`
and never reads a kubeconfig. This mirrors `watch.Target` and
`buildTargets` ([internal/cli/watch.go:47-64](../../../internal/cli/watch.go)),
and it buys two things: the package is unit-testable end to end with client-go's
fake clientset and no cluster, and the one place allowed to name a kubeconfig
path — a connection error on **stderr** — stays in the CLI layer, structurally
unable to reach the report.

### Package layering

Two packages are added, and they sit in different classes.

`internal/fleet` imports `internal/scan`, `internal/gate`, `internal/findings`,
`internal/parallel`, `internal/glob`, `internal/jsonschema`, and
`k8s.io/client-go/kubernetes`.

**It inherits gate's wall: `internal/fleet` must never import
`internal/remediate` or `internal/explain`.** A test in the package enforces it,
in the style of `internal/dashboard/imports_test.go`.

Two separate promises, neither implying the other:

- **Read-only toward every cluster.** `get`/`list` only, exactly what
  `scan.Evaluate` already issues. There is no `--fix` path from a fleet sweep,
  and no code path from `internal/fleet` into a write.
- **No LLM calls.** `fleet` has no `--explain` and no `--investigate`, and
  cannot acquire one without crossing the import wall above.

`internal/glob` joins the strictest class instead: like `internal/jsonschema`,
`internal/dashboard` and `internal/baseline`, it **imports nothing from kubeagent
at all** and nothing outside the standard library, which makes reaching
`internal/remediate` or `internal/explain` impossible by construction rather than
by rule. `internal/glob/imports_test.go` enforces both halves. It holds no client
and no context, issues no cluster call and makes no model call. This matters
beyond tidiness: `internal/policy` is the most constrained package in the repo
and may not import `scan`, `findings`, `report`, `investigate`, `remediate` or
`explain`, so whatever `policy` depends on has to be at least as clean as
`policy` is.

No cycle is created: `findings` imports `scan`, `gate` imports `findings`,
`policy` and `fleet` both import the leaf `glob`, and nothing imports `fleet`
except `internal/cli`.

### Concurrency and determinism

`internal/parallel.Do` runs the sweep, capped at `--workers`
(`KUBEAGENT_FLEET_WORKERS`, 8 by default — the same default and the same shape
as `KUBEAGENT_SCAN_WORKERS`). `Do` returns results in **index** order, never
completion order, which is exactly why it exists
([internal/parallel/parallel.go:10-18](../../../internal/parallel/parallel.go)).

Determinism is preserved by construction, not by discipline, in the same way
`scan`'s worker pool preserves it: each cluster's closure writes only its own
result slot and touches no shared state, and a single sequential pass afterwards
sorts on a **total** order. That order, in full, printed first to last:

1. **verdict rank** — `unreachable` (0), `inconclusive` (1), `fail` (2),
   `pass` (3);
2. **critical** count, descending; then **warning**, then **info**;
3. **context name**, ascending.

Context names are unique within a kubeconfig, so the final tiebreak is total: no
two rows can compare equal, and the rendered bytes cannot depend on which cluster
answered first.

There is exactly **one** rank in this design and this is it — the same one the
verdict uses, so `unreachable` and `inconclusive` sort above `fail` for the same
reason they outrank it in the verdict. A cluster kubeagent could not judge might
be worse than one it judged as failing, and an operator scanning 300 rows
top-down should meet that fact before anything else. Ranking rows and ranking
verdicts by two different orders would mean explaining which applies where; one
order needs no explaining.

`--cluster-timeout` (default 60s) bounds each cluster's read with its own derived
context. One unreachable-but-not-refusing API server must not hang a 300-cluster
sweep; a cluster that exceeds it becomes `unreachable`, not a silent omission.

## Cluster selection

Exactly one of two forms, and the command refuses anything ambiguous rather than
guessing:

| Form | Meaning |
|------|---------|
| `--context NAME` (repeatable) | Sweep exactly these. |
| `--all-contexts` | Sweep every context `cluster.Contexts()` finds. |
| `--all-contexts --match 'prod-*'` | …narrowed by a glob over context **names**. |

- `--match` without `--all-contexts` is a **usage error** naming both flags. It
  has nothing to filter, and silently implying `--all-contexts` would turn a typo
  into a fleet-wide read.
- `--context` together with `--all-contexts` is a **usage error**. One says
  "these", the other says "all"; picking one for the operator is guessing.
- A selection that resolves to **zero** clusters is a **usage error**, not a
  pass. A sweep that found nothing to sweep must never exit 0 — that is the
  failure mode `buildTargets` already calls out for the daemon: "an operator who
  asked for three clusters and silently got two is worse off than one whose
  daemon refused to start".

Enumeration uses `cluster.Contexts()`
([internal/cluster/client.go:142](../../../internal/cluster/client.go)), which
already exists, already sorts by name, and is already contractually path-free —
it discards the underlying error and returns a fixed message precisely so a
kubeconfig path cannot ride out on it. `internal/mcp` depends on that same
guarantee.

### `--match` needs the glob `internal/policy` already has

`path.Match` is the wrong tool: its `*` will not cross a `/`, and kubeconfig
context names routinely contain one — OpenShift generates them in the shape
`default/api-example-com:6443/kube:admin`. That is the same limitation that made
`internal/policy` write its own matcher
([internal/policy/glob.go:8-13](../../../internal/policy/glob.go)), and its
two-metacharacter syntax (`*` any run including `/`, `?` one byte, everything
else literal) is exactly what `--match` wants.

`globMatch` is unexported, so this slice **promotes it to `internal/glob`** — a
stdlib-only leaf package in the same class as `internal/redact`, imported by
both `internal/policy` and `internal/fleet`. Copying it instead would duplicate a
function whose doc comment carries a load-bearing safety caveat: it is
O(len(pattern) × len(s)) in the worst case, and `internal/policy` caps the
compared value at `maxMatchLen` before calling it. A copy is how that caveat gets
lost. Fleet's inputs are context names from the operator's own kubeconfig, not
values from the network, so the cap is not needed there — but the reasoning has
to survive next to the code, in one place.

Cost, stated so it is not a surprise during implementation: the `FuzzGlob` target
moves with the function, so `.github/workflows/fuzz.yml`'s matrix entry changes
from `./internal/policy` to `./internal/glob`, and the seed corpus moves with it.
Nothing else in the fuzz matrix is touched.

### Failing to build a client is fatal; failing to reach a cluster is not

Building a client contacts no API server, so a failure there is a configuration
error — a misspelled context — and it is **fatal before the sweep starts**, the
same ruling `buildTargets` makes for the daemon. A failure *during* the sweep is
a live-cluster condition and becomes an `unreachable` entry with the run
continuing.

## The report

A new versioned JSON document, `fleet.Report`, entering at **schemaVersion 1.0**
— the **eighth** document in the contract. Nothing existing moves: `scan` stays
at 1.2, `gate` stays at 1.1. `internal/jsonschema` gains `FleetVersion`,
`internal/schemadoc` gains a `fleet` entry publishing
`website/docs/schemas/fleet-v1.json`, and `kubeagent schema fleet` prints it from
the running binary with no cluster and no kubeconfig.

```go
// Report is `kubeagent fleet --output json` verbatim.
type Report struct {
    SchemaVersion string           `json:"schemaVersion"`
    Verdict       string           `json:"verdict"`  // pass | fail | inconclusive
    Code          int              `json:"exitCode"`
    FailOn        findings.Level   `json:"failOn"`
    Clusters      []ClusterSummary `json:"clusters"`
    Unreachable   []Unreachable    `json:"unreachable"`
}

// ClusterSummary is one cluster's outcome. It carries counts and issue KINDS,
// never object names — see "What the report may name" below.
type ClusterSummary struct {
    Context    string   `json:"context"`
    Verdict    string   `json:"verdict"`  // pass | fail | inconclusive
    Critical   int      `json:"critical"`
    Warning    int      `json:"warning"`
    Info       int      `json:"info"`
    Blindspots int      `json:"blindspots"`

    // TopIssues is at most THREE issue kinds, most frequent first, ties broken
    // by kind name ascending so the slice is deterministic. Three because the
    // column has to stay readable at 300 rows and because the fourth-most-common
    // kind has never been what makes an operator open a cluster. It is a
    // signpost, not an inventory: the operator runs `scan` against that one
    // context for the full list.
    TopIssues  []string `json:"topIssues,omitempty"`
}

// Unreachable is a cluster that was selected and could not be judged. Reason is
// drawn from a fixed vocabulary and is never an err.Error().
type Unreachable struct {
    Context string `json:"context"`
    Reason  string `json:"reason"`
}
```

`Unreachable` is the fleet-scale form of the rule the least-privilege RBAC slice
established and `gate.Blindspot` encodes: a read kubeagent could not perform is
**named as a blind spot**, never silently dropped. At fleet size the temptation
is worse, because 1 missing row out of 300 is invisible — so the count appears in
the header line and the verdict, not only in the list.

The two renderers present that list differently, deliberately. **JSON** keeps
`Unreachable` as its own array, because a consumer filtering `clusters[]` for
failures must not have to know that some entries have no counts. **Text**
interleaves those clusters into the one table at rank 0, showing the reason in
the `TOP ISSUES` column and leaving the count columns blank — a reader scanning
rows top-down should not have to find a second table below the fold to learn a
cluster went unjudged. Same data, and the shape each consumer needs.

### What the report may name

This is the design's sharpest constraint, and it is settled here rather than
deferred, because a fleet report is by construction a list of cluster identities.

**It may name:** kubeconfig **context names**, and issue **kinds**
(`CrashLoopBackOff`, `ImagePullBackOff`, `Unschedulable`).

A context name is the operator's own label for their own cluster, and it is the
only thing that can answer "which one". This is not a new exposure: `internal/mcp`'s
`list_contexts` tool already serves context names to a *remote* caller by design
([internal/mcp/contexts.go:31-58](../../../internal/mcp/contexts.go)), and the
watch daemon has carried one as a `cluster` metric label since Theme E.

**It may never name:** a kubeconfig path or any filesystem path; a full API
server URL (nothing beyond `scheme://host`, and slice 1 carries no server URL at
all); a Kubernetes **node** name; a namespace, pod, or workload name.

That last exclusion is not a restriction bolted onto the summary — it is *why*
the report is a summary. Counts plus issue kinds cannot carry an object name,
so the rule is structural rather than a filter someone must remember to apply.
A test asserts that a `Report` rendered from a fixture whose objects are named
with distinctive markers contains none of those markers.

`Unreachable.Reason` comes from a fixed vocabulary — `"connecting to the
cluster"`, `"timed out"` — and never from `err.Error()`, which can carry a server
URL or a path. The underlying error is dropped rather than routed somewhere
safer: a fleet report is written to be forwarded, and there is no stream on
which `fleet` could publish a per-cluster error without also publishing it to
whoever receives the report. An operator who needs the detail runs `kubeagent
gate --context <name>` against the one cluster. (This is a different mechanism
from `buildFleetTargets`'s kubeconfig-path carve-out, which does write to
stderr, from `internal/cli`, and aborts before a sweep ever starts — the two are
mutually exclusive.)

**Unreachable is not the same as refused, and the two must not be conflated.** A
cluster kubeagent reached but was not allowed to read fully is *not* unreachable:
`scan.Evaluate` returns normally, `gate.Decide` records the refusal as a
`Blindspot`, and the cluster gets an ordinary `ClusterSummary` with a non-zero
`Blindspots` count and an `inconclusive` verdict. `Unreachable` is only for a
cluster that produced no `scan.Result` at all. Both roads lead to the same
fleet-level verdict — `inconclusive`, exit 2 — but they are different facts and
the report says which.

## Verdict and exit codes

`fleet` reuses `gate`'s exit-code constants unchanged
([internal/gate/gate.go:23-27](../../../internal/gate/gate.go)) so one mental
model covers both:

| Verdict | Code | When |
|---------|------|------|
| `pass` | 0 | Every selected cluster was reached and passed. |
| `fail` | 1 | At least one cluster has findings at or above `--fail-on`. |
| `inconclusive` | 2 | At least one selected cluster was unreachable, or a cluster's own verdict was inconclusive. |
| usage | 4 | Bad selection (see above). |

**`inconclusive` outranks `fail`.** This mirrors `gate.Decide`'s own switch,
where `case blind` is evaluated *before* `case len(v.Failing) > 0`
([internal/gate/gate.go:229-240](../../../internal/gate/gate.go)). The reasoning
carries over exactly: when kubeagent could not see enough, a "fail" verdict may
understate what is actually wrong, so the honest answer is that the run could not
judge. Inverting this at fleet scope would let one unreachable cluster hide
behind another cluster's failure.

`--fail-on` defaults to `critical`, matching `gate`.

## Flags

```text
--kubeconfig PATH        path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)
--context NAME           kubeconfig context to sweep (repeatable)
--all-contexts           sweep every context the kubeconfig defines
--match GLOB             with --all-contexts: only contexts whose name matches
--fail-on LEVEL          critical | warning | info   (default critical)
--workers N              clusters read concurrently  (default 8, KUBEAGENT_FLEET_WORKERS)
--cluster-timeout DUR    per-cluster budget          (default 60s)
--output FORMAT          text | json                 (default text)
-n, --namespace NS       namespace to judge          (default: all namespaces)
```

Deliberately **absent**: `--logs`, `--disk-usage`, `--certs`, `--capacity`,
`--explain`, `--investigate`, `--fix`, `--rollback`. This needs no special
refusal logic — `gate` already exposes none of them, building a fixed
`scan.Options` instead, so a fleet sweep inherits a bounded per-cluster read for
free. Multiplying a proxied per-node read by three hundred clusters is a shape
this command will not offer.

Per project convention, flags are declared on the command and never as
persistent flags, and `internal/cli.Normalize` gives every long flag its
single-dash spelling.

## Testing

- `internal/fleet` is unit-tested end to end with client-go's **fake clientset**:
  N fake targets, no cluster, asserting the summary, the sort order, the verdict
  and the exit code. Determinism gets an explicit test that runs the same fake
  fleet repeatedly and asserts byte-identical output.
- Selection parsing (`--match` without `--all-contexts`, `--context` with
  `--all-contexts`, zero matches) is tested through the pure flag-parsing path
  `internal/cli` already uses for `parseWatchFlags`.
- The credential rule gets its own test, described above: distinctive markers in
  node, namespace and pod names must not appear in a rendered `Report`.
- An `imports_test.go` in `internal/fleet` enforces the layering wall, and one in
  `internal/glob` enforces the stricter stdlib-only rule.
- The glob extraction is behaviour-preserving and must be provable as such:
  `internal/policy/glob_test.go` moves to `internal/glob` **unchanged**, so a
  green run is evidence the semantics did not shift. `FuzzGlob` moves out of the
  shared `internal/policy/fuzz_test.go` into `internal/glob/fuzz_test.go`, and
  its one seed (`testdata/fuzz/FuzzGlob/7a18e8a7330619a0`) moves with it — a seed
  left behind is a past finding silently stopped being replayed.
- `TestSchemaDrift` covers the new document; `fleet-v1.json` is generated, never
  hand-written.
- `internal/report/testdata/golden-scan.txt` is untouched — `fleet` adds no
  output to `scan`. The demo GIF and `website/docs/quickstart.md` are not
  regenerated.
- `go.mod` and `go.sum` do not change.

## Documentation

- `website/docs/features/fleet.md` — the feature page.
- `website/mkdocs.yml` — nav entry.
- `website/docs/features/json-schema.md` — eight documents, not seven.
- `CLAUDE.md` — the eighth document; `internal/fleet`'s wall; `internal/glob`
  joining the imports-nothing class; `--kubeconfig`'s command count.
- `.github/workflows/fuzz.yml` — `FuzzGlob`'s matrix entry moves from
  `./internal/policy` to `./internal/glob`.
- `CHANGELOG.md` — under `[Unreleased]`.
- `website/docs/roadmap.md` — fleet-scale slice 1 shipped.
- `docs/go-concepts.md` — reusing one generic bounded pool across a second
  call site, and why index-ordered results make the output deterministic. New
  angle for this project; everyday example first, then the kubeagent example.

## Out of scope

- **Cross-cluster correlation** ("the same image is failing in all three"). This
  slice makes it possible — it is the first thing in the repo that holds many
  clusters' findings at once — and deliberately does not attempt it.
- **Scaling the watch daemon** to hundreds of informer sets. Different problem,
  memory-bound, and unchanged by this slice.
- **A fleet file** listing clusters with labels, outside a kubeconfig. Selection
  in slice 1 is kubeconfig-derived only.
- **Per-cluster detail.** `fleet` says which cluster; the operator then runs the
  `scan` or `gate` they already have against that one context.
- **A hub-to-daemon protocol.** Still out of scope, as in Theme E.
- **`--output sarif`** and `--policy` / `--baseline` at fleet scope. Both are
  plausible slice 2 material; neither is needed to answer "which of my clusters
  are broken".
