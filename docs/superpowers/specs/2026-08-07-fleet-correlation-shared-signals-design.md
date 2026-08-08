# Fleet-scale slice 2 — cross-cluster correlation ("shared signals")

**Status:** design approved, ready to plan
**Date:** 2026-08-07
**Roadmap item:** post-1.0, fleet-scale — the follow-up named at
[website/docs/roadmap.md](../../../website/docs/roadmap.md) as "cross-cluster
correlation (*the same image is failing in all three*), which this slice makes
possible and deliberately does not attempt".

## What shipped in slice 1

`kubeagent fleet` (v1.7.0) sweeps every selected kubeconfig context in bounded
parallel and prints one row per cluster, worst first. The per-cluster pipeline
is exactly `gate`'s — `scan.Evaluate` then the pure `gate.Decide` — so a sweep
and a single-cluster `gate` can never disagree about the same cluster, and fleet
adds no diagnosis of its own.

Slice 1 reduces each cluster to counts plus at most three issue kinds. It
answers "which cluster do I open first". It cannot answer "is this one problem
or five".

## Goal

Answer the second question, from data the sweep already reads. `kubeagent fleet`
gains two sections: which **issue kinds** and which **refused reads** appear in
two or more of the judged clusters, most widespread first.

No new cluster read. No new API call. No new package. The sweep costs exactly
what it cost yesterday.

## Scope

**In:** the shared-signals correlation, its text sections, one optional JSON
property, the `fleet` schema bump, tests, docs.

**Out, and deliberately a separate slice:** selection from something other than
a kubeconfig. The two are independent and merging them would produce one
untestable change.

**Out, and deliberately not built:** correlation on an image reference, a
workload identity, a namespace, or a node. See "The axis the roadmap named"
below.

## The axis the roadmap named, and why it is not the one we build

The roadmap's own example is an image: *the same image is failing in all three*.
That axis is not available, and the reason is structural rather than a matter of
effort.

`findings.Finding` is `{Level, Kind, Namespace, Name, Issue, Reason, Owner}`.
There is no image field. Nothing in `scan` → `findings` → `gate` carries an
image reference at any point, so "correlate on image" is not a fleet change at
all — it is an edit to three packages outside fleet, it moves `gate`'s
`schemaVersion`, and it lands a registry host in a document written to be
forwarded. A private registry host in an image reference is an internal
hostname, which this repository bans outright. Making that axis safe would mean
stripping the host and correlating on the remainder, at which point the axis is
"a repository path and tag" and no longer the thing the roadmap sentence
promised.

Meanwhile two axes are already computed inside fleet and thrown away:

1. [`summarize`](../../../internal/fleet/summarize.go) builds
   `counts map[string]int` over every finding's `Issue`, takes the top three,
   and discards the rest. Issue kinds are already the declared-admissible fleet
   vocabulary, closed at thirteen by `internal/knownissues` and kept closed by
   four tests in `internal/diagnose`.
2. `gate.Verdict.Inconclusive` is `[]gate.Blindspot`, and `Blindspot.Resource`
   is a closed set of API resource *type* names — every one a string literal in
   [internal/scan/scan.go](../../../internal/scan/scan.go): `nodes/proxy`,
   `pods/log`, `pods/proxy`, `/readyz`, `secrets`, `events`, `endpointslices`,
   `horizontalpodautoscalers`, `ingresses`, `leases`,
   `mutatingwebhookconfigurations`, `namespaces`, `networkpolicies`,
   `persistentvolumeclaims`, `persistentvolumes`, `poddisruptionbudgets`,
   `resourcequotas`, `services`, `storageclasses`,
   `validatingwebhookconfigurations`. `ClusterSummary` reduces all of that to
   `Blindspots int` — the count, never the names.

A refused read repeated across a fleet is the more actionable of the two: it
means one RBAC binding is missing everywhere, and it is the class of problem a
per-cluster view is worst at surfacing, because each cluster reports it as a
single quiet line.

## Architecture

**No new package.** `internal/fleet` already carries the wall this needs —
never `internal/remediate`, never `internal/explain`, machine-enforced by
[internal/fleet/imports_test.go](../../../internal/fleet/imports_test.go) — and
a correlation is a property of one sweep, not a reusable primitive. New file
`internal/fleet/correlate.go` with its own `correlate_test.go`.

### Data flow

```
Sweep
 ├─ per cluster (unchanged): scan.Evaluate → gate.Decide → gate.Verdict
 ├─ summarize(name, verdict) → (ClusterSummary, clusterEvidence)   ← changed
 ├─ sortSummaries / decide (unchanged)
 └─ correlate(evidence) → []Shared                                 ← new, pure
```

`summarize` starts returning a second value alongside the row. `Sweep` collects
the evidence in the same loop that appends the summaries, then calls `correlate`
once, sequentially, after the parallel phase — so determinism is preserved the
way the rest of the package preserves it: the parallel closures write only their
own destinations, and a sequential pass afterwards walks a fixed order.

### Types

```go
// clusterEvidence is what one judged cluster contributes to a correlation:
// which signals it showed, not how many times it showed them.
type clusterEvidence struct {
	context    string
	issues     map[string]bool
	blindspots map[string]bool
}
```

Sets, not counts, and that is the load-bearing choice. A kind hitting four
hundred pods in one cluster is still **one** cluster. Correlation asks "in how
many clusters", and a count-weighted fold would let a single noisy cluster
manufacture a fleet-wide signal that does not exist.

```go
// Shared is one signal that appeared in two or more judged clusters.
type Shared struct {
	Signal   string   `json:"signal"`   // "ImagePullBackOff" | "nodes/proxy"
	Source   string   `json:"source"`   // SourceIssue | SourceBlindspot
	Clusters []string `json:"clusters"` // context names, ascending
}

const (
	SourceIssue     = "issue"
	SourceBlindspot = "blindspot"
)
```

No count field. `len(Clusters)` **is** the count, and a stored duplicate is a
defect waiting to disagree with the array beside it.

`Source` is required because the two vocabularies are different things that must
not be confused: `ImagePullBackOff` is something wrong in the cluster,
`nodes/proxy` is something kubeagent was not allowed to look at. In text the
distinction is carried by two labelled sections; in JSON a consumer needs it on
the record.

### Memory cost at fleet size

Bounded and small. Evidence is at most 13 issue-kind strings plus at most 20
blind-spot strings per cluster — the two vocabularies are closed — so a
300-cluster sweep holds at most ~10 000 map entries of short interned-ish
strings. It is retained only between the parallel phase and the `correlate`
call; the verdicts themselves were already held for that long.

### Threshold and ordering

`minShared = 2`, not configurable. A signal in one cluster is not a
correlation, and every number above two is an arbitrary line the operator would
have to learn.

`correlate` returns `nil` when fewer than two clusters were judged — structurally
implied by the threshold, but written as an explicit early return so the intent
is readable.

Total order, so two runs over the same fleet render identical bytes:

1. `Source` — every `issue` before every `blindspot`
2. `len(Clusters)` descending
3. `Signal` ascending

`Clusters` within an entry sorts ascending by context name. Go randomizes map
iteration, so every one of these tiebreaks is required for determinism, not a
nicety.

## What "same" means

Exact string equality on `Issue`, and exact string equality on
`Blindspot.Resource`. No normalization, no fuzzy matching, no prefix grouping.

Both are closed, kubeagent-authored vocabularies — not free text from the API
server — so exact match is the strongest available comparison and there is
nothing to normalize. `Init:CrashLoopBackOff` and `CrashLoopBackOff` are
different kinds and stay different: they have different causes and different
fixes, and folding them together would be exactly the coincidence-firing this
section exists to prevent.

## The credential wall

This is the part that had to be resolved head-on, because a correlation exists
to name something.

**Admissible, and each for a stated reason:**

| Carried | Why it is safe |
|---|---|
| Issue kinds | Closed at thirteen, kubeagent's own vocabulary, kept closed by four tests in `internal/diagnose`. Already in the report as `TopIssues`. |
| Blind-spot **resources** | A closed set of API resource *type* names, every one a string literal in `internal/scan/scan.go`. Names a kind of read, never an object. |
| Context names | Already the first column of the existing table, and already served to a remote caller by `internal/mcp`'s `list_contexts` by design. |

**Not carried, and this is the one sharp edge:** `gate.Blindspot.Reason`. It is
`redact.Error(err)` on the non-forbidden path — redacted, but redacted is not
the same as vocabulary-bounded, and a correlation is a fleet-wide document. The
correlation reads `Resource` and nothing else off a `Blindspot`.

**Still not carried, unchanged from slice 1:** node names, namespaces, pod
names, workload names, kubeconfig paths, server URLs, images.

A test pins the sharp edge directly: build a verdict whose `Blindspot.Reason` is
a sentinel string, sweep, render both text and JSON, assert the sentinel appears
in neither.

### The documented promise widens, deliberately

Two places state fleet's credential promise in prose and both must be edited in
the same commit as the code, not after:

- [internal/fleet/fleet.go](../../../internal/fleet/fleet.go) package comment:
  "The report names kubeconfig context names and issue kinds."
- `CLAUDE.md`, fleet bullet: "Its report names kubeconfig context names and
  issue kinds — never a node, namespace, pod or workload name."

Both become "…context names, issue kinds, and the API resource names of refused
reads — never a node, namespace, pod or workload name." A promise that no
longer describes the code is a defect, and the remedy here is to widen the
claim honestly rather than to narrow the feature.

## Output

### Text — two sections between the table and the verdict line

```
FLEET  5 clusters, 1 failing, 0 unreachable

CLUSTER    VERDICT       CRIT  WARN  INFO  TOP ISSUES
prod-us    inconclusive     2     0     1  ImagePullBackOff, OOMKilled (1 blind spot)
dev        inconclusive     0     0     0  (1 blind spot)
prod-eu    fail             3     1     0  ImagePullBackOff, OOMKilled
staging    pass             0     2     0  ImagePullBackOff
sandbox    pass             0     0     0

SHARED ISSUES  in 2 or more of 5 judged clusters

  3/5  ImagePullBackOff   prod-eu, prod-us, staging
  2/5  OOMKilled          prod-eu, prod-us

SHARED BLIND SPOTS  in 2 or more of 5 judged clusters

  2/5  nodes/proxy        dev, prod-us

verdict: inconclusive (exit 2)
```

The example is worth reading twice, because it is the case the feature exists
for. Every row is individually unremarkable — one cluster with a blind spot,
another with a blind spot, three clusters each reporting an image pull problem.
The two sections below say what no row can: it is *one* missing RBAC binding and
*one* bad image, not five unrelated incidents. And note that the two blind spots
force the fleet verdict to `inconclusive` even though only one cluster failed —
that is slice 1's rule unchanged, not something the correlation did.

- A section with no entries is **omitted entirely**, header included. A heading
  over nothing reads as a failed render.
- The denominator is the count of **judged** clusters, not selected ones. An
  unreachable cluster produced no verdict and could not have contributed a
  signal; counting it would make a 2-of-2 correlation read as 2-of-5 and
  understate. The header word "judged" is constant, not conditional on whether
  any cluster was unreachable.
- `maxNamedClusters = 3` context names, then `+N more` — following
  `maxTopIssues`'s established "signpost, not an inventory" reasoning. JSON
  carries every name.
- The signal column is padded to one width computed across **both** sections, so
  they line up. Two sections that nearly align read as a bug.
- The `N/M` cell is left-padded to the widest cell present, so `12/300` and
  `2/300` stack.

### JSON — one optional property

```json
{
  "schemaVersion": "1.1",
  "verdict": "fail",
  "exitCode": 1,
  "failOn": "critical",
  "clusters": [ … ],
  "unreachable": [],
  "shared": [
    {"signal": "ImagePullBackOff", "source": "issue",     "clusters": ["prod-eu","prod-us","staging"]},
    {"signal": "OOMKilled",        "source": "issue",     "clusters": ["prod-eu","prod-us"]},
    {"signal": "nodes/proxy",      "source": "blindspot", "clusters": ["dev","prod-us"]}
  ]
}
```

`Shared []Shared \`json:"shared,omitempty"\`` — a sweep with no correlation
encodes no key, and every v1.7.0 consumer is unaffected.

## Schema version

`fleet` moves **1.0 → 1.1**. One optional property added, nothing removed,
nothing changed type, nothing newly required — additive, therefore MINOR, which
is what `TestSchemaDrift` will say.

`scan` stays 1.2. `gate` stays 1.1. The other six documents do not move.
Regeneration is `go test ./internal/schemadoc -run TestSchemaDrift -update`,
run once at implementation time and never during a review.

## Gating

**Purely reportorial.** `decide()` is untouched.

The reasoning is not squeamishness: every finding a correlation counts was
already counted in the cluster that produced it, and every one of those clusters
already got its verdict from `gate.Decide`. Letting a correlation add severity
would double-count the same evidence, and it would let a sweep disagree with a
single-cluster `gate` about the same cluster — which
[internal/fleet/fleet.go](../../../internal/fleet/fleet.go)'s package comment
declares can never happen. The precedent is settled elsewhere too: the baseline
slice ruled a heuristic is `findings.Info` and never fails a default gate.

`inconclusive` still outranks `fail`, and unreachable clusters still force
`inconclusive`. A test pins verdict and exit code identical to slice 1's for the
same set of clusters, with a correlation present.

## Testing

Everything here is a pure function over values built in the test. No cluster, no
fake clientset, no network.

**`correlate_test.go`**
- no clusters → nil; one cluster with signals → nil (threshold)
- a signal in exactly two clusters → one entry, `Clusters` ascending
- a signal in exactly one cluster → absent
- ordering: count descending, then signal ascending on a tie
- every `issue` entry precedes every `blindspot` entry
- an issue kind and a blind-spot resource that happen to share a string are two
  distinct entries, not one

**`summarize_test.go`**
- evidence is a **set**: a verdict with three findings of the same `Issue`
  yields one evidence entry, and the existing `TopIssues` behaviour is unchanged
- evidence carries `Blindspot.Resource` for every entry in `Inconclusive`,
  waived or not — an operator who waived a read still has that blind spot
- the existing `ClusterSummary` assertions still hold byte-for-byte

**`render_test.go`**
- both sections render, in order, with the blank-line spacing above
- an empty section is omitted, header included
- `+N more` fires at four clusters and not at three
- signal-column width is shared across both sections

**`fleet_test.go`**
- the gating test: same clusters, correlation present, `Verdict` and `Code`
  identical to slice 1
- the credential test: a sentinel `Blindspot.Reason` appears in neither the text
  nor the JSON render
- `shared` is absent from the JSON when there is no correlation

TDD throughout: write the failing test, watch it fail, then implement.

## Files

**Create**
- `internal/fleet/correlate.go`
- `internal/fleet/correlate_test.go`

**Modify**
- `internal/fleet/fleet.go` — `Shared` type, `Source*` constants,
  `Report.Shared`, `Sweep` collects evidence, package comment widened
- `internal/fleet/summarize.go` — `summarize` returns evidence
- `internal/fleet/summarize_test.go`, `internal/fleet/fleet_test.go`,
  `internal/fleet/render_test.go`
- `internal/fleet/render.go` — the two sections
- `internal/jsonschema/jsonschema.go` — `FleetVersion` 1.0 → 1.1
- `website/docs/schemas/fleet.json` — regenerated, not hand-edited
- `website/docs/features/fleet.md` — a section on shared signals
- `CHANGELOG.md`, `CLAUDE.md`, `website/docs/roadmap.md`

**Untouched, and named here because a reviewer should check it:**
`internal/report/testdata/golden-scan.txt` stays byte-identical — fleet has no
`scan` render path, so the golden file cannot move. Do not regenerate the demo
GIF or `website/docs/quickstart.md`.

## Global constraints

Inherited, non-negotiable, and every one of them binds every task:

- **READ-ONLY toward every cluster swept** — get/list only, no write of any
  kind, no `--fix` path. **Separately and additionally: no LLM call.** These are
  two promises, not one; never blur them, and never let a comment, doc line,
  help string or commit message suggest a correlation is related to `--explain`,
  which is the model path.
- `internal/fleet` must never import `internal/remediate` or `internal/explain`.
  No new package, so no new wall to declare — `correlate.go` inherits fleet's,
  and `imports_test.go` already covers every `*.go` in the directory including
  test files.
- **NO NEW DEPENDENCY:** `go.mod` and `go.sum` must not change. `sort`,
  `strings`, `fmt` are standard library and already imported.
- **CREDENTIALS:** no secrets, credentials, private IPs or internal hostnames
  anywhere — code, tests, fixtures, docs, help text, schema descriptions.
  Documentation IPs are RFC 5737; example domains RFC 2606. URLs are
  credentials: nothing beyond `scheme://host`, and the project's own
  `https://k8sproject.top` links are the only permitted host. Kubeconfig paths
  and node names are credentials. Context names in fixtures are invented and
  generic (`prod-eu`, `staging`, `dev`).
- **Only `fleet`'s `schemaVersion` moves**, 1.0 → 1.1. `scan` stays 1.2, `gate`
  stays 1.1. Regenerate exactly once with
  `go test ./internal/schemadoc -run TestSchemaDrift -update`; never run any
  other test with `-update`.
- `internal/report/testdata/golden-scan.txt` must stay byte-identical.
- **TDD.** Pure functions, values built in the test.
- Go lives at `/usr/local/go/bin`. `go test` runs with `-p 2` locally, never
  `-short`.
- Every commit needs `git commit -s` (DCO enforced on `main`), authored solely
  by the human — no `Co-Authored-By` trailer and no AI attribution of any kind,
  anywhere.

**DANGER:** never run `./chaos/run.sh` in any form — a run takes ~40 minutes and
injects real outages. Nothing in this design needs a cluster; no task creates,
deletes or touches one.

## What this slice does not attempt

- Correlation on an image, a workload, a namespace or a node — see above.
- A configurable threshold. Two is the threshold.
- Any change to how a cluster is selected. That is the next slice.
- Any change to a verdict, an exit code, or `gate`'s document.
