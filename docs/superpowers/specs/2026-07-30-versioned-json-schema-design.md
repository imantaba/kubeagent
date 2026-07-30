# Stable versioned JSON schema (Theme H sub-project 3)

**Status:** approved, ready for a plan
**Branch:** `json-schema`, cut off `main` at `2c50f2a`
**Gate:** full chaos (see "Gate" below — this deviates from the decomposition's
"smoke" on purpose)

## The problem

kubeagent emits JSON that other programs parse: a platform team reads
`scan --output json`, a CI pipeline reads `gate --output json`, a dashboard polls
the watch daemon's `/issues`, an operator scripts `rbac check --output json`.
None of it declares a version. A consumer cannot tell whether the document it
holds is the shape it was written against, and kubeagent has no mechanical way
to notice that a rename broke somebody's parser. The v1.0 production contract
needs both halves: a version in the document, and a test that fails when the
shape moves without one.

There is also no published schema. A consumer that wants to generate types, or
validate a captured document in CI, has to read Go source.

## What ships

1. A `schemaVersion` string in every one of the six machine-readable JSON
   documents kubeagent produces.
2. A JSON Schema document per output, generated from the Go types by
   reflection, committed, and published at a URL that resolves.
3. `kubeagent schema [name]` — the running binary prints its own schema.
4. A drift test that fails when generated output no longer matches the committed
   document, and that says whether the change was additive or breaking.
5. The compatibility contract, written down: what is promised, what is not.

## The surfaces

Four **surfaces**, each with its own version, spanning six **documents**:

| Document | Surface | Root Go type | Emitted by |
|---|---|---|---|
| `scan` | scan | `report.ScanReport` | `scan --output json` |
| `gate` | gate | `gate.Verdict` | `gate --output json` |
| `rbac-print` | rbac | `rbacprofile.RulesDocument` | `rbac print --output json` |
| `rbac-check` | rbac | `rbacprofile.CheckDocument` | `rbac check --output json` |
| `watch-issues` | watch | `watch.IssuesReport` | watch daemon `GET /issues` |
| `watch-explanations` | watch | `watch.ExplanationsReport` | watch daemon `GET /explanations` |

Deliberately **out of scope**:

- `gate --output sarif` — SARIF 2.1.0 is versioned by OASIS. kubeagent does not
  get to promise anything about someone else's schema.
- `--alert-format slack` and `--alert-format alertmanager` — those shapes belong
  to the receiver. `--alert-format json` is ours, but its consumer is a webhook
  we do not define; it stays out until someone asks.
- The `--fix` audit journal (JSONL) — a write-side record, not a read contract.
- `/metrics` — Prometheus text, already versioned (`version=0.0.4`).
- `/healthz`, `/readyz` — plain text, not JSON.

Per-surface versions mean a new scan detector field does not bump the version a
CI pipeline sees on `gate`. A surface's version covers all of its documents:
`rbac-print` and `rbac-check` move together, because a consumer that scripts one
usually scripts both.

## The version field

Every document carries, as its first property:

```json
{
  "schemaVersion": "1.0",
  "…": "…"
}
```

A string, `MAJOR.MINOR`:

- **MINOR bump** — fields added, an optional field became optional in a new
  place, an enum gained a value. A parser written against `1.0` still works
  against `1.3`.
- **MAJOR bump** — a field was removed or renamed, a type changed, a field that
  was always present became optional, an enum lost a value. A parser written
  against `1.x` may break.

All four surfaces start at `1.0`. The version is **not** the kubeagent release
version and does not move with it.

### Two outputs change shape, on purpose

`rbac print --output json` and `rbac check --output json` currently emit bare
JSON arrays. An array root cannot carry a `schemaVersion` field, so both get
wrapped in an object:

```text
# before                                  # after
[                                         {
  { "apiGroup": "", "resources": […],       "schemaVersion": "1.0",
    "verbs": ["get","list"] },              "roleName": "kubeagent",
  …                                         "rules": [ { "apiGroup": "", … }, … ]
]                                         }
```

```text
# before                          # after
[                                 {
  { "feature": "scan", … },         "schemaVersion": "1.0",
  …                                 "features": [ { "feature": "scan", … }, … ]
]                                 }
```

This is a **breaking change** to two outputs, taken deliberately while the
project is pre-1.0. An unversioned array root can never gain a version later
without exactly this break; doing it at 0.71 costs less than doing it after 1.0
promised not to. It goes in the CHANGELOG under `### Changed` with the old and
new shapes side by side, and in the docs page, and the `rbac` docs get a `jq`
one-liner for anyone who needs the old shape (`jq '.rules'`).

`scan`, `gate`, and the two watch documents are already object-rooted, so their
change is purely additive: one new property.

## The generator

A new `internal/jsonschema` package. **Standard library only** — no JSON Schema
dependency exists in the module and none is being added. It walks a
`reflect.Type` and emits JSON Schema draft 2020-12.

```go
// Schema is one JSON Schema object. A map, so encoding/json sorts its keys and
// output is byte-deterministic without an ordered container.
type Schema = map[string]any

// Meta is what the caller knows and reflection cannot: the document's identity,
// and the two things reflection gets wrong (see below).
type Meta struct {
    Name        string // "scan"
    Version     string // "1.0"
    Title       string
    Description string
    Enums       map[string][]string // "gitops.State" → its constants
    Overrides   map[string]Schema   // "findings.Level" → the schema its MarshalJSON implies
}

// Generate renders root as a JSON Schema document. Output is deterministic:
// the same type and Meta always produce identical bytes.
func Generate(root reflect.Type, m Meta) ([]byte, error)

// TypeKey is the "<pkgbase>.<TypeName>" key used by $defs, Enums and Overrides,
// exported so a caller's guard tests key the same way the generator does.
func TypeKey(t reflect.Type) string
```

The tables arrive through `Meta` rather than living in the generator: which
kubeagent type is an enum is not something a generic reflection-to-schema
package should know, and passing them in lets the generator's own unit tests use
hand-built types instead of real report structs.

`internal/jsonschema` imports nothing from kubeagent. That keeps it importable
by every surface package — including `internal/gate` and
`internal/rbacprofile`, which may not import `internal/remediate` or
`internal/explain` — and makes the "never imports remediate or explain"
invariant trivially true, the same way it is for `internal/safetext`.

The version constants live here too, so `internal/report`, `internal/gate`,
`internal/rbacprofile` and `internal/watch` can each stamp their own output
without importing one another:

```go
const (
    ScanVersion = "1.0"
    GateVersion = "1.0"
    RBACVersion = "1.0"
    WatchVersion = "1.0"
)
```

### Mapping rules

| Go | JSON Schema |
|---|---|
| `string` | `{"type": "string"}` |
| `int`, `int64`, … | `{"type": "integer"}` |
| `float64`, `float32` | `{"type": "number"}` |
| `bool` | `{"type": "boolean"}` |
| `struct` | `{"$ref": "#/$defs/<pkg>.<Type>"}`, defined once in `$defs` |
| `[]T` with `omitempty` | `{"type": "array", "items": …}` |
| `[]T` without `omitempty` | `{"type": ["array", "null"], "items": …}` |
| `*T` with `omitempty` | the schema for `T` |
| `*T` without `omitempty` | `T`'s schema with `"null"` added to its type |
| `map[K]T`, `K`'s kind is string | `{"type": "object", "additionalProperties": …}` |
| `[]byte` | `{"type": "string", "contentEncoding": "base64"}` |
| field tagged `json:"-"` | omitted |
| unexported field | omitted |

Two map keys in the scan graph are **named** string types, not `string`:
`map[gitops.State]int` and `map[operators.State]int`. So the rule keys off the
key type's *kind*, and where the key type is in the enum table the map also gets
`"propertyNames": {"enum": […]}` — a free promise that the counts object holds no
key outside the enum.

Anything the six graphs do not contain is a **generation error**, not a guess: an
interface, an embedded struct field promoted with no JSON name, a map keyed by
something that is not a string. None appears today (see below), and a schema that
silently invented a shape for one would be worse than a test that fails.

The `["array", "null"]` rule is not pedantry: a nil Go slice marshals to `null`,
not `[]`. Where the code happens to initialize the slice, the schema is merely
permissive; where it does not, a stricter schema would be a lie. Which fields
those are is discoverable, but the promise a consumer needs is "this may be
null", and that is what gets written.

Named struct types become `$defs` entries keyed `<pkgbase>.<TypeName>` — e.g.
`findings.Finding`, `capacity.Rule`. Every schema object is a Go map, so
`encoding/json` sorts its keys: `$defs`, `properties` and every nested object
come out alphabetically, and generation is byte-deterministic without an ordered
container. `required` is sorted explicitly, being a slice. Property order is
alphabetical rather than document order — JSON object order carries no meaning,
and the alternative is a custom marshaler for no gain.

`required` lists exactly the fields **without** `omitempty`.
`additionalProperties` is left unset (JSON Schema's default: permitted), with a
`$comment` saying so — a minor bump adds properties, and a consumer validating
today's document against yesterday's schema must not fail for that reason.

### The root's own version property

`schemaVersion` is the one property the generator special-cases. Reflection sees
a `string` and would emit `{"type": "string"}`, which promises nothing; a `const`
of `Meta.Version` would be worse, because a `1.1` document would then fail
validation against the published `1.0` schema and the whole minor-bump promise
would be void. So the root's `schemaVersion` becomes a pattern over the major:

```json
"schemaVersion": { "type": "string", "pattern": "^1\\.[0-9]+$" }
```

derived from `Meta.Version`'s major component. Every future `1.x` document
validates; a `2.0` document does not, which is exactly what a major bump means.

### The two things reflection gets wrong

**A custom marshaler.** `findings.Level` is an `int` whose `MarshalJSON` emits
`"critical"`. Reflection sees `reflect.Int` and would document an integer — the
schema would be wrong about a field a CI pipeline reads. So the generator keeps
an override table keyed by `<pkgbase>.<TypeName>`, and a **guard test** walks
every document's type graph and fails if any type implements `json.Marshaler`
or `encoding.TextMarshaler` without an override. Today `findings.Level` is the
only one; the guard is what keeps the next one from shipping wrong silently.

**Enums.** Reflection cannot see a `const` block. Named string types get an
enum table:

| Type | Values |
|---|---|
| `findings.Level` | `info`, `warning`, `critical` (string, via the override) |
| `gitops.State` | `synced`, `pending`, `stale`, `blocked`, `unknown` |
| `operators.State` | `healthy`, `progressing`, `unhealthy`, `suspended`, `unknown` |
| `capacity.RuleName` | `noRequests`, `limitNoRequest`, `neverSchedulable` |

Two tests hold this honest: one asserts each table entry equals the package's
own constants, so renaming a constant fails the test rather than the schema
quietly drifting; another walks the document graphs and fails when a **named**
string type appears that is neither in the enum table nor in an explicit
free-form list. An un-enumerated enum is the failure mode that matters — a
consumer switching on a value it was never told about.

Walking the graphs says the table is complete: the only named string types
anywhere in the six documents are `capacity.RuleName`, `gitops.State` and
`operators.State`, and the only custom marshaler is `findings.Level`, which
reaches JSON through `gate` alone and appears nowhere in the scan graph. So the
free-form list starts **empty**, and the override table has exactly one entry.

### Every type in every document is kubeagent's own

Walking the six roots settles a question worth settling before writing a
generator: **no Kubernetes API type, no interface, and no `time.Time` appears in
any document.** The `scan` graph is 59 named types across 28 kubeagent packages
and nothing else; `gate` is 4 types across two; each watch document is 3-4 types
in one. No embedded struct field, no `[]byte`, and no pointer field without
`omitempty` appears anywhere. Timestamps are already
RFC 3339 strings. `rbac print --output json` emits `[]rbacprofile.Rule`
(`apiGroup`, `resources`, `nonResourceURLs`, `verbs`) — kubeagent's own
four-field type, not `rbacv1.PolicyRule`, which never reaches JSON at all: the
RBAC manifest renderers write YAML text.

So the generator needs no opaque-type escape hatch, and there is no upstream
shape to describe. What the project still needs is the *policy*, enforced rather
than written down and forgotten: kubeagent must not freeze a type it does not
own. A **foreign-type guard test** walks all six graphs and fails on any type
whose package is neither `github.com/imantaba/kubeagent/...` nor the standard
library. It passes today. The day someone puts a `corev1.PodStatus` in a
report, the test stops them and the decision — reference it by description, or
own a projection of it — gets made deliberately, with a version bump attached.

The reason to care: a field added to an upstream type in a future Kubernetes
release would trip the drift test as a breaking change nobody could fix, and
would silently expand what kubeagent has promised.

## The document registry

The generator takes a `reflect.Type`; something has to name the six roots. That
cannot be `internal/jsonschema`, which the surface packages import — it would be
a cycle. So a second package, `internal/schemadoc`, holds the table:

```go
type Document struct {
    Name    string       // "scan" — the file stem and the schema command's argument
    Surface string       // "scan" — which version constant applies
    Version string
    Root    reflect.Type
    Title       string
    Description string
}

var Documents = []Document{ … }   // six entries, the single source of truth

func Generate(name string) ([]byte, error)   // one document
func Names() []string
```

One table drives the generated files, the `schema` command, and the drift test —
the same shape as `rbacprofile.Feature`, which already generates every RBAC
manifest and the chart ClusterRole from one list.

`internal/schemadoc` imports `internal/report`, `internal/gate`,
`internal/rbacprofile` and `internal/watch`, so it transitively reaches
`internal/remediate` and `internal/explain`. That is fine and worth stating
plainly: only `main.go` and its own test import `schemadoc`. The invariants
constrain what `gate`, `mcp`, `tui`, `rbacprofile`, `safetext` and `fuzzgen`
**import**, not who imports them. `schemadoc` holds no client and no context and
makes no call — it reads types.

### Root types get exported

Reflection reaches an unexported type through an exported field without trouble,
but `schemadoc` has to *name* each root in Go source. Four renames, no behavior
change:

- `report.inventoryReport` → `report.ScanReport`
- `watch.issuesView` → `watch.IssuesReport`
- `watch.explanationsView` → `watch.ExplanationsReport`
- plus the nested view types they reach (`issueView`, `clusterView`,
  `statsView`, `explanationView`, `explainStatsView`, `investigationView`,
  `remediationActionView`) exported too — not because reflection needs it, but
  because their names become `$defs` keys in a published document, and
  `watch.issueView` reads like a leaked internal where `watch.IssueView` reads
  like a contract.

`rbacprofile` gains the two new document types.

## Publishing

Generated files are committed at `website/docs/schemas/<name>-v<MAJOR>.json`,
published by the existing Pages workflow, and declare themselves:

```json
"$schema": "https://json-schema.org/draft/2020-12/schema",
"$id": "https://k8sproject.top/schemas/scan-v1.json"
```

The `$id` carries the **major** only, so a `1.0 → 1.1` minor bump does not move
a URL a consumer pinned. A major bump publishes a new file beside the old one;
the old file stays, because a document already in someone's CI does not stop
existing when kubeagent moves on.

## The drift test

`internal/schemadoc`'s test regenerates all six documents and compares each to
its committed file. On a mismatch it flattens both documents to a set of
`path → type signature` lines, diffs the sets, and reports which kind of change
happened:

- a path removed, a type changed, a `required` entry added, a `required` entry
  removed, an enum value removed → **breaking**: the failure says a MAJOR bump
  is needed.
- a path added, an enum value added → **additive**: the failure says
  regenerate and bump MINOR.

Both fail. The point is not to permit one silently; it is that the person who
made the change is told which kind it was, in the terms the contract uses.
Regeneration follows the golden-output house style:

```text
go test ./internal/schemadoc -run TestSchemaDrift -update
```

## The `schema` command

```text
kubeagent schema             # list the documents, their surfaces and versions
kubeagent schema scan        # print the scan schema to stdout
```

Generated at runtime by the same code path, so what the binary prints is what
the binary's types are. There is no embedded copy that could drift: the
committed files are exactly this output, and the drift test is what proves it.
An unknown name is an error naming the valid ones. Usage text uses `invokedAs`,
never a hardcoded `kubeagent`, so the `kubectl kubeagent schema` spelling reads
correctly.

Read-only, no cluster connection, no LLM call — it does not even need a
kubeconfig.

## The contract, written down

A new `website/docs/features/json-schema.md`:

- the four surfaces, six documents, and where each is emitted
- what MINOR and MAJOR mean, with the field-level rules above
- **what is not promised:** the order of object properties; the order of array
  elements unless the docs say a list is sorted; the exact wording of any
  human-readable string (`reason`, `summary`, `evidence`, `explanation`) — those
  are prose for an operator, not data to match on; anything under `explanation`
  or `investigation`, which is model output; and the shape of upstream
  Kubernetes types carried through verbatim.
- how to pin: compare `schemaVersion`'s major, and treat an unknown minor as
  compatible
- how to validate a captured document offline
- the `rbac` shape change, with the `jq` one-liner
- the `kubeagent schema` command

`website/docs/features/ci-gate.md`, `rbac.md` and the watch docs each gain a
line pointing here, and the `--output json` mentions in the quickstart get one
too.

## Tests

- `internal/jsonschema` — unit tests over hand-built types: a flat struct,
  nested structs, a slice with and without `omitempty`, a pointer with and
  without, a map, `json:"-"`, an unexported field, an enum, the marshaler
  override, and determinism (generate twice, compare
  bytes). Pure functions, no fixtures.
- `internal/schemadoc` — the drift test with `-update`; the marshaler guard; the
  foreign-type guard; the enum-table-matches-constants test; the
  named-string-type coverage test; a test
  that every `Documents` entry has a non-empty name, version, title and root,
  and that names are unique.
- Per-surface stamping tests: each of the six outputs actually contains
  `"schemaVersion"` with the right value. For `gate`, also that SARIF output is
  unchanged by the new field.
- `internal/report/testdata/golden-scan.txt` must stay **byte-identical** — the
  new field is JSON-only and the type rename touches no text rendering. Proven,
  not assumed.
- A `main` test for `schema` (list, one document, unknown name) in whatever
  style the existing subcommand tests use.

## Gate

The approved decomposition marked this sub-project **smoke**. It touches
`internal/watch` (the daemon's HTTP documents) and `internal/rbacprofile` (both
output shapes), and the standing rule is that anything touching the watch daemon
or RBAC gets the **full chaos gate**. The rule wins: full chaos, plus a manual
check of the two rbac verbs and the daemon's `/issues` and `/explanations`
against the chaos cluster while it is up — the two watch documents are otherwise
only covered by unit tests.

## Global constraints

Unchanged, and every task inherits them:

- Every commit carries a `Signed-off-by` trailer matching its author
  (`git commit -s`) — `main` enforces DCO.
- No `Co-Authored-By` and no AI attribution anywhere: commits, PRs, code, docs,
  changelog.
- No new third-party dependency. The generator is `reflect` + `encoding/json`.
- Standard-library `flag` only — Cobra is sub-project 5.
- Detectors stay pure functions; the scan stays sequential.
- `internal/report/testdata/golden-scan.txt` stays byte-identical.
- `go test` runs with `-p 2`, never `-short`.
- No secrets, credentials, private IPs or internal hostnames anywhere, including
  schema descriptions and examples — RFC 5737 IPs, RFC 2606 domains.
- Untrusted API text is sanitized at ingress. A schema describes shape and adds
  no new ingress, so this work introduces no new `safetext` call site.
- `internal/jsonschema` must never import `internal/remediate` or
  `internal/explain` (it imports nothing from kubeagent at all).
- Usage and error text use `invokedAs`, never a hardcoded `kubeagent`.
- TDD: the failing test first, watched failing, then the implementation.

## Task shape for the plan

Roughly six tasks, each independently testable:

1. `internal/jsonschema`: the generator, the mapping rules, `TypeKey`, the
   version constants, unit tests over hand-built types.
2. Export the root and view types in `report` and `watch`; add
   `rbacprofile.RulesDocument` and `CheckDocument`. No behavior change yet;
   golden text proven unchanged.
3. Stamp `schemaVersion` into all six outputs, including the two rbac wrappers
   that change shape; per-surface tests; SARIF unchanged; golden text unchanged.
4. `internal/schemadoc`: the `Documents` table, the kubeagent enum and override
   tables, `Generate`, the drift test with `-update`, the marshaler, foreign-type
   and named-string guards, and the six committed schema files. Stamping comes
   first so the committed files are generated from types that already carry the
   field, and no file is written twice.
5. `kubeagent schema [name]` plus the usage string.
6. Docs: `website/docs/features/json-schema.md`, mkdocs nav, cross-links,
   CHANGELOG (`### Added` and the breaking `### Changed`), roadmap Theme H
   bullet, CLAUDE.md invariant and Theme H paragraph, and a
   `docs/go-concepts.md` entry on reflection — a new concept here, since
   `reflect` appears in no production file today.
