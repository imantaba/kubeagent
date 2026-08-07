# Fleet selection from a file — design

**Status:** approved, ready for a plan
**Date:** 2026-08-07
**Scope:** post-1.0 roadmap, fleet-scale item, slice 3 — the last one in the item

Slice 1 (v1.7.0) made `kubeagent fleet` sweep every selected kubeconfig context
in bounded parallel and print one verdict row per cluster, worst first. Slice 2
(v1.10.0) added cross-cluster correlation. Both selected clusters from exactly
one kubeconfig. This slice adds a second selection source: a file that names the
clusters, so a sweep can span more than one kubeconfig.

---

## 1. The problem

`kubeagent fleet` today resolves its clusters through
`cluster.Contexts(o.kubeconfig)` → `selectContexts` → `buildFleetTargets` →
`cluster.NewClient(kubeconfig, name)`. Every one of those steps takes a single
kubeconfig path. `--all-contexts` means *every context in that one file*.

An operator whose clusters are not all in one kubeconfig cannot sweep them
together. That is the common case for per-cluster credential files: a k3s or
kind cluster ships its own kubeconfig, and every one of them names its context
`default`.

There is a second, smaller problem underneath. The fleet a team cares about is
usually a stable set that belongs in version control — but today it can only be
expressed as a shell invocation (`--context a --context b --context c`) or as a
glob that silently changes meaning when someone adds a context to their
kubeconfig.

## 2. What ships

One new flag on `kubeagent fleet`:

```text
--fleet-file <path>
```

It is not repeatable: one file names the fleet. The file lists the clusters to
sweep. Selection comes from the file;
**credentials still come from kubeconfigs the file points at**. No server URL,
no bearer token and no CA data ever enters a kubeagent value — exactly as
today.

```yaml
# clusters.yaml
- context: prod-eu
- context: prod-us
- name: edge-a
  kubeconfig: /path/to/edge-a.kubeconfig
  context: default
- name: edge-b
  kubeconfig: /path/to/edge-b.kubeconfig
  context: default
```

```text
$ kubeagent fleet --fleet-file clusters.yaml

FLEET  4 clusters, 2 failing, 0 unreachable

CLUSTER  VERDICT       CRIT  WARN  INFO  TOP ISSUES
edge-a   fail             2     0     0  ImagePullBackOff, OOMKilled
edge-b   fail             1     0     0  OOMKilled
prod-eu  pass             0     0     0
prod-us  pass             0     0     0

SHARED ISSUES  in 2 or more of 4 judged clusters

  2/4  OOMKilled  edge-a, edge-b

verdict: fail (exit 1)
```

## 3. Alternatives considered and rejected

**EKS / GKE / AKS cluster-list APIs.** Ruled out by the standing NO NEW
DEPENDENCY constraint, before any other consideration: each needs its cloud
provider's SDK in `go.mod`.

**A Cluster API or Open Cluster Management registry CRD.** Not ruled out by
dependency — kubeagent already builds `dynamic` and `discovery` clients
(`cluster.NewDynamicClients`, added for `scan --operators`), so listing
`Cluster` objects on a hub is cheap. Ruled out on credentials. A workload
cluster's credentials live in a `Secret` (CAPI's `<cluster>-kubeconfig`), so
discovery that actually reaches those clusters means kubeagent reading
cluster-admin credentials out of Secrets. `rbacprofile.coreRules` grants no
Secret read, and `Secret` is deliberately not a `--policy`-selectable kind. A
registry source that read only names and still required a matching kubeconfig
would be fragile and would buy nothing this file does not.

**A directory of kubeconfigs (`--kubeconfig-dir`).** Deferred, not dropped. It
is a thin adapter that would produce the same entries this file produces, so
it can be added later without moving anything designed here. It cannot express
a per-cluster display name, which is exactly what the per-cluster-kubeconfig
case needs, so it is strictly weaker than the file on its own motivating
scenario.

**Context names on stdin (`--contexts-from -`).** Rejected: contexts would
still come from one kubeconfig, so it does not escape the kubeconfig at all.
It is a different spelling of `--context`.

## 4. Where the code goes

### 4.1 `internal/fleetfile` — new package

`internal/fleet`'s own package comment states that a kubeconfig path is "a
credential this package must never hold", which is why `Target` carries a
built client rather than a path. The fleet file carries paths. Parsing it
inside `internal/fleet` would break that promise, so it gets its own package.

`internal/fleetfile` is **pure**: no client, no context, no I/O beyond the
bytes it is handed — the same shape as `internal/policy`. It imports exactly
three things: `fmt`, `sigs.k8s.io/yaml`, and
`github.com/imantaba/kubeagent/internal/safetext`.

**The wall it inherits, stated explicitly:** it inherits `internal/fleet`'s —
it must never import `internal/remediate` or `internal/explain`.
`internal/fleetfile/imports_test.go` enforces that half on
`internal/fleet/imports_test.go`'s pattern (a `go/parser` walk over every
`*.go` in the directory, test files included, fataling on an empty file list
so the guard cannot pass vacuously). It enforces a second half that
`internal/fleet` cannot: no `k8s.io/client-go` and no `internal/cluster`
import, which makes "holds no client" a structural fact rather than a stated
one.

It is **not** in the stdlib-only class of `internal/glob`, `internal/baseline`,
`internal/knownissues` and `internal/policypack` — it needs a YAML decoder, and
`sigs.k8s.io/yaml` is a direct dependency already.

```go
// Entry is one cluster in a fleet file.
type Entry struct {
	Name       string `json:"name,omitempty"`
	Kubeconfig string `json:"kubeconfig,omitempty"`
	Context    string `json:"context"`
}

// Load decodes and validates a fleet file's bytes.
func Load(data []byte) ([]Entry, error)
```

`sigs.k8s.io/yaml` converts YAML to JSON and then uses `encoding/json`, so the
struct tags are `json:` tags — the same as `policy.Rule`.

`Load` returns entries in file order, with `Name` already resolved (defaulted
to `Context` when the file set none) and already sanitized. The caller never
has to re-derive either.

### 4.2 `internal/cli` — the flag, the file read, and selection

`internal/cli` owns `os.ReadFile` and owns naming the path in an error, on the
precedent `readPolicyFile` and `namePath` already set for `--policy`. Two new
pure helpers alongside the existing `selectContexts`:

```go
// selectEntries filters a fleet file's entries by --match and refuses an
// empty result.
func selectEntries(entries []fleetfile.Entry, match string) ([]fleetfile.Entry, error)

// buildFleetFileTargets connects to each entry's cluster.
func buildFleetFileTargets(fallbackKubeconfig string, entries []fleetfile.Entry) ([]fleet.Target, error)
```

`buildFleetFileTargets` calls `cluster.NewClient(kubeconfig, entry.Context)`
where `kubeconfig` is `entry.Kubeconfig` when set and `fallbackKubeconfig`
otherwise, and builds `fleet.Target{Name: entry.Name, Context: entry.Context,
Client: client}`.

It makes the same ruling `buildFleetTargets` makes and for the same reason: a
client that cannot be built is fatal at exit 4, because `cluster.NewClient`
does no network I/O, so a failure there is a configuration defect and never a
reachability event. This is also the one place a kubeconfig path may be named —
on stderr, the operator's own channel.

### 4.3 `internal/fleet`

`internal/fleet` learns nothing about files. It still takes `[]Target` and
returns a `Report`. Its changes are all about row identity and are set out in
§5.

## 5. Row identity

A fleet file can give a cluster a display name distinct from its kubeconfig
context, because four per-cluster k3s kubeconfigs are four clusters whose
context is `default`.

### 5.1 `Target` gains `Context`

```go
type Target struct {
	// Name is the row identity: what the report calls this cluster. For a
	// kubeconfig sweep it is the context name.
	Name string

	// Context is the kubeconfig context this cluster was reached through.
	// Empty means it is the same as Name — a caller that did not distinguish
	// the two has only one identity to report.
	Context string

	Client kubernetes.Interface
}
```

`Name` keeps its current meaning exactly: the identity that reaches the
report. `Context` defaults to `Name` inside `Sweep` when unset, so
`buildFleetTargets` and every existing test that writes `Target{Name: …,
Client: …}` need no change at all.

**`Target` must never gain a kubeconfig path field.** Pinned by a test that
reflects over `Target`'s fields and asserts the set is exactly `{Name,
Context, Client}`, so a future `Kubeconfig string` fails the suite rather than
quietly placing a credential inside `internal/fleet`.

### 5.2 `ClusterSummary` and `Unreachable` each gain `Name`

```go
Name string `json:"name,omitempty"`
```

`summarize` sets `ClusterSummary.Name`, and `Sweep` sets `Unreachable.Name`,
each **only when it differs from `Context`**. `Context` therefore always holds a
real kubeconfig context name, as it has since v1.7.0 — a consumer piping it
into `kubectl --context` keeps working. A sweep with no fleet file encodes no
`name` key at all, so its JSON is byte-identical to v1.10.0's. That is the same
discipline `shared` uses.

`Unreachable` gains it too, and not as an afterthought: an unreachable
per-cluster-kubeconfig entry would otherwise render as `default`, which names
nothing.

### 5.3 A determinism defect the identity change exposes

`sortSummaries`'s last tiebreak is `a.Context < b.Context`, and its comment
justifies it: the context name "is unique within a kubeconfig". That premise
dies the moment a sweep spans several kubeconfigs. Four clusters whose context
is `default` make the comparator non-total, and `sort.Slice` is not stable, so
two runs over the same fleet could render different bytes. The `Unreachable`
sort in `Sweep` has the same defect.

Both must sort on the **row identity**, and the comment must be corrected to
say the identity is unique because `fleetfile.Load` refuses a duplicate — which
is what makes the order total.

A small unexported helper is the single definition of the identity, used by
the sorts, the renderer and the evidence:

```go
// identity is what the report calls a cluster: the operator's name when the
// selection source gave one, the kubeconfig context otherwise.
func identity(name, context string) string
```

### 5.4 Renaming what is no longer a context

Three unexported names currently say "context" and now carry an identity that
may not be one. Each is renamed so the code does not claim something untrue:

- `row.context` → `row.id`
- `clusterEvidence.context` → `clusterEvidence.id`
- `namedClusters(contexts []string)` → `namedClusters(ids []string)`

The shared-signal section must name what the table names, or an operator
cannot cross-reference the two. `correlate` itself needs no change: it already
folds over whatever string the evidence carries.

## 6. The file format

A bare YAML list, like a policy file — no `apiVersion`, no `kind`, no wrapper.

| Field | Required | Meaning |
|---|---|---|
| `context` | yes | the kubeconfig context to reach this cluster through |
| `kubeconfig` | no | path to the kubeconfig; falls back to `--kubeconfig`, then `$KUBECONFIG`, then `~/.kube/config` |
| `name` | no | the row identity; defaults to `context` |

**`context` is required deliberately.** An entry naming no context would take
its kubeconfig's current-context, which can change under the operator between
runs — a checked-in fleet file has to be reproducible, and its identity has to
be knowable without loading a kubeconfig.

### 6.1 The format cannot express a credential

`Entry` has three string fields and `Load` decodes with
`yaml.UnmarshalStrict`, so `server:`, `token:`, `certificate-authority-data:`
and every other kubeconfig field are **load errors**, not silently ignored
keys. This is structural rather than a rule anyone has to follow, and it gets
its own test.

The strictness pays a second way: a typo'd `kubconfig:` fails loudly instead
of silently falling back to the default kubeconfig and sweeping the wrong
cluster.

### 6.2 Load-time refusals

Every one is exit 4 — bad input, discovered before any cluster was touched.

| Condition | Why |
|---|---|
| the list is empty | An empty sweep reporting `pass` is the worst possible answer, because it looks like good news. `selectContexts` already rules exactly this way. |
| `context` is empty after trimming | See 6. |
| two entries resolve to the same `name` | Two rows with one identity make the report ambiguous, `--match` unpredictable, and the sort order non-total. |
| an unknown field | See 6.1. |
| the YAML does not parse | It is not a fleet file. |

Errors name the entry by its 1-based position and by its resolved name. They
never name an entry's `kubeconfig` — no validation failure is about that field,
so the question never arises, and `internal/fleetfile` holds no path in any
error it produces.

### 6.3 Sanitizing at ingress

`name` passes through `safetext.Line` inside `Load`, on the same reasoning
`policy.Load` sanitizes `r.Message`: it reaches a terminal and a document
written to be forwarded. A `name` that is empty after sanitizing is a load
error.

`context` and `kubeconfig` are not sanitized. `context` is handed to
`clientcmd` as a lookup key, where a mangled value would silently select
nothing; `kubeconfig` is a filesystem path handed to `os.Open` via client-go,
where the same applies. Both follow the standing rule that matching and lookup
run on the raw value.

## 7. CLI surface

```
--fleet-file <path>   read the clusters to sweep from a file
```

`fleet` only. No other command gains it.

| Combination | Outcome |
|---|---|
| `--fleet-file` + `--context` | refused, exit 4 — the file names the clusters |
| `--fleet-file` + `--all-contexts` | refused, exit 4 — same |
| `--fleet-file` + `--kubeconfig` | allowed; `--kubeconfig` becomes the fallback for entries that set none |
| `--fleet-file` + `--match` | allowed; matches the **row identity** |
| `--match` without `--fleet-file` or `--all-contexts` | refused, exit 4 — unchanged wording widened to name both |

`--match` today refuses unless `--all-contexts` is set. That check widens to
"`--all-contexts` or `--fleet-file`". It keeps using `internal/glob`, whose
two-metacharacter matcher already handles the slashes an OpenShift context
name carries. A `--match` that selects nothing is exit 4, as it is today.

The `--fleet-file` path reaches stderr only, through `namePath`. It never
reaches the report, which `internal/fleet` cannot do anyway — the path never
crosses into that package.

## 8. What does not change

- **The per-cluster pipeline.** `scan.Evaluate` then the pure `gate.Decide`,
  untouched. A fleet-file sweep and `kubeagent gate --context X` still cannot
  disagree about the same cluster.
- **`decide()`.** A selection source changes no verdict. A test pins the fleet
  verdict and exit code identical whether the same clusters were selected from
  a kubeconfig or from a file.
- **`correlate()`.** Untouched.
- **Read-only toward every cluster swept:** `get`/`list` only, no write of any
  kind, no `--fix` path. **Separately and additionally: no LLM call.** Two
  promises, not one restatement.
- **`internal/report/testdata/golden-scan.txt`** stays byte-identical — fleet
  has no scan render path, so it cannot move. No demo GIF and no
  `website/docs/quickstart.md` regeneration.
- **`go.mod` and `go.sum`.** `sigs.k8s.io/yaml v1.6.0` is already a direct
  dependency, used by `internal/policy`.
- **The `Unreachable.Reason` vocabulary** stays at two entries. The fleet file
  adds a third class of exit-4 failure (a malformed file) and nothing to
  `unreachable`. `cluster.NewClient` does no network I/O, so a client that
  cannot be built stays fatal rather than becoming a reachability event.

## 9. Schema

`fleet` moves **1.1 → 1.2**: two added optional properties, `clusters[].name`
and `unreachable[].name`, both `omitempty` and both absent from `required`.
Additive, so MINOR. `scan` stays 1.2, `gate` stays 1.1, and the other five do
not move.

Regenerated exactly once, with
`go test ./internal/schemadoc -run TestSchemaDrift -update`. `TestSchemaDrift`
must report the change as additive.

A sweep selected from a kubeconfig encodes no `name` key anywhere, so a
consumer written against fleet 1.0 or 1.1 is unaffected.

## 10. Credentials

| Value | Where it may appear |
|---|---|
| the `--fleet-file` path | stderr only, via `namePath` — never a report, and it never crosses into `internal/fleet` |
| an entry's `kubeconfig` path | handed to `cluster.NewClient` and nowhere else; never a report, never a log, never an error |
| an entry's `context` | the report, by design — fleet has served context names since v1.7.0 |
| an entry's `name` | the report, by design; sanitized at ingress |
| a server URL, token or CA data | **cannot exist** — the format has no field for one, and `UnmarshalStrict` refuses one |

Test fixtures use invented, generic names (`prod-eu`, `prod-us`, `staging`,
`edge-a`, `edge-b`) and temp-directory paths from `t.TempDir()`. Documentation
IPs are RFC 5737 and example domains RFC 2606, though this feature's examples
need neither.

## 11. Testing

Everything except `buildFleetFileTargets` is a pure function over values built
in the test. No cluster, no network, and no fake clientset is needed for the
parse, the selection, the identity or the render.

**`internal/fleetfile`** — a table over bytes: a valid multi-entry file; `name`
defaulting to `context`; a missing `context`; an empty list; a duplicate
resolved `name`; an unknown field; a `server:`/`token:` key refused; a `name`
carrying a control character, sanitized; a `name` that sanitizes to empty,
refused; malformed YAML. Plus `imports_test.go`, both halves.

**`internal/cli`** — `selectEntries` over a table (no match, a match selecting
a subset, a match selecting nothing); the flag-conflict matrix from §7;
`buildFleetFileTargets` against kubeconfigs written into `t.TempDir()`, both
the per-entry path and the fallback.

**`internal/fleet`** — `summarize` with `name != context` and with `name ==
context` (the second must leave `ClusterSummary.Name` empty); the reflect test
pinning `Target`'s field set; `sortSummaries` over four summaries sharing the
context `default`, asserting a total order; the `Unreachable` sort likewise;
`RenderText` with names, byte-compared against the rendered table in §2 (the
report's own bytes, without the shell prompt line); `RenderJSON` asserting no
`name` key appears anywhere when no name differs.

One more, which is what actually pins §8's claim that a selection source
changes no verdict: build one set of fake clients, wrap it twice — once the way
a kubeconfig sweep does (`Target{Name: ctx, Client: c}`, `Context` left unset)
and once the way a fleet file does (`Target{Name: "edge-a", Context: "default",
Client: c}`) — and assert both sweeps return the same `Verdict` and the same
`Code`. The rows differ only in what they are called.

**`internal/schemadoc`** — `TestSchemaDrift` green after the one regeneration.

## 12. Out of scope

- `--kubeconfig-dir`. Deferred; it can be added later as an adapter producing
  the same entries.
- Registry discovery (Cluster API, Open Cluster Management). Dropped on
  credentials — see §3.
- Labels on an entry and a `--select` flag. `--match` over the row identity
  covers the common subset case; labels can follow if it proves insufficient.
- Reading the fleet file from stdin.
- Any change to what a sweep *does* with a cluster once it has a client.

## 13. Docs to update

- `website/docs/features/fleet.md` — the new source, the format table, the
  flag matrix, and the credential statement from §10.
- `website/docs/features/json-schema.md` — fleet 1.1 → 1.2.
- `website/docs/schemas/fleet-v1.json` — regenerated once.
- `CHANGELOG.md` — under `[Unreleased]`.
- `CLAUDE.md` — the `internal/fleet` invariant paragraph gains
  `internal/fleetfile` and its wall; the post-1.0 fleet bullet records the
  slice.
- `website/docs/roadmap.md` — fleet-scale complete.
- `internal/fleet/fleet.go`'s package comment — the report now names an
  operator-chosen name as well as a context name.
