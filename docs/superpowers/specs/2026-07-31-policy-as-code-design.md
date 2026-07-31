# Policy-as-code custom checks — design

**Status:** approved, ready for planning
**Theme:** H (v1.0 production contract), sub-project 6 of 7
**Date:** 2026-07-31

## Problem

Every check kubeagent runs is compiled in. The eleven pod detectors implement
`diagnose.Detector`; the twenty-odd non-pod checks are each their own
`internal/<x>health` package with its own `Issue` type. Both are Go code in this
repository.

An operator with an organization-specific rule — "no image may come from a
registry outside our allowlist", "every production Deployment must be covered by
a PodDisruptionBudget", "every workload must carry an owner label" — has no way
to express it. The only options are forking kubeagent or running a second tool.

This sub-project closes that gap. It is the last feature before v1.0;
sub-project 7 (the cross-version/distro chaos matrix) is a validation gate, not
a feature.

## What this is not

The roadmap line reads "a detector/plugin SDK and policy-as-code custom checks".
Those are two different products, and only one of them is built here.

A runtime plugin mechanism is deliberately rejected. Go's `plugin.Open` requires
the host and the plugin to be built with an identical Go toolchain, identical
dependency versions, and identical build flags; it would be the least stable
surface in the release whose entire theme is a stable production contract. A
subprocess protocol avoids the ABI problem but replaces it with a worse one: a
custom check would be an arbitrary executable holding the operator's
credentials, able to write to the cluster, which contradicts the read-only
contract directly.

So the "SDK" here is the documented policy contract and the Go types behind it —
`internal/policy`'s `Rule`, `Assert`, and `Violation`, the published JSON schema,
and `website/docs/features/policy.md`. Nothing is loaded at runtime except data.

## Decisions

| Decision | Choice |
|---|---|
| Mechanism | Declarative policy file. No plugin ABI, no subprocess. |
| Rule language | Fixed operator set. No expression language, no CEL, no Rego. |
| Selectable kinds | Only kinds kubeagent already reads. RBAC does not change. |
| Surfaces | `scan`, `gate`, and a new `kubeagent policy validate`. |
| Severity | Advisory in `scan` (never moves the cluster verdict); enforced in `gate` through the existing `--fail-on`. |
| Rule shape | One `match`, one `assert`. No `allOf`/`anyOf` in v1. |

## Architecture

```
policy file(s) ──load──▶ []policy.Rule ──┐
                                          ├─▶ policy.Evaluate ──▶ []policy.Violation
cluster objects ──collect (demand-driven)─┘                            │
                                                     ┌─────────────────┴──────────────┐
                                                     ▼                                ▼
                                          report.Input.Policy               findings.From (gate)
                                          (text / JSON / HTML)              (--fail-on, SARIF)
```

`internal/policy` is pure: no client, no context, no I/O beyond reading the
policy bytes it is handed. It imports nothing from kubeagent except
`internal/safetext`, so `gate`, `mcp`, `tui` and `htmlreport` may all import it
without reaching `remediate` or `explain`.

### Files

```
internal/policy/policy.go     types: Level, Rule, Match, Assert, Op, Relation, Violation
internal/policy/load.go       strict YAML decode + validation; Load, LoadPaths, Kinds
internal/policy/path.go       dotted-path resolution over unstructured objects
internal/policy/glob.go       the one wildcard matcher
internal/policy/op.go         the closed operator set
internal/policy/relation.go   the two cross-resource relations
internal/policy/eval.go       Evaluate: match, assert, sort, sanitize
internal/cli/policy.go        the `policy validate` command
website/docs/features/policy.md
```

## The rule model

```yaml
# policies/registry.yaml
- id: registry-allowlist
  match:
    kind: Pod
    namespaceLabels: {tier: prod}
  assert:
    path: spec.containers[*].image
    op: matches
    values: ["registry.example.com/*"]
  level: critical
  message: image comes from a registry outside the allowlist

- id: prod-deployments-need-a-pdb
  match:
    kind: Deployment
    namespaceLabels: {tier: prod}
  assert:
    relation: hasPodDisruptionBudget
  level: warning
  message: no PodDisruptionBudget covers this Deployment
```

### Go types

```go
// Level is a rule's declared severity. It is policy's own type, not
// findings.Level: internal/findings imports internal/scan, and internal/scan
// imports internal/policy, so depending on findings here would be a cycle.
type Level string

const (
    LevelCritical Level = "critical"
    LevelWarning  Level = "warning"
    LevelInfo     Level = "info"
)

type Rule struct {
    ID      string `json:"id"`
    Match   Match  `json:"match"`
    Assert  Assert `json:"assert"`
    Level   Level  `json:"level"`
    Message string `json:"message"`
}

type Match struct {
    Kind            string            `json:"kind"`
    Namespaces      []string          `json:"namespaces,omitempty"`
    Labels          map[string]string `json:"labels,omitempty"`
    NamespaceLabels map[string]string `json:"namespaceLabels,omitempty"`
}

// Assert is one claim. Exactly one of (Path+Op) and Relation is set.
type Assert struct {
    Path     string   `json:"path,omitempty"`
    Op       Op       `json:"op,omitempty"`
    Values   []string `json:"values,omitempty"`
    Relation Relation `json:"relation,omitempty"`
}

type Violation struct {
    RuleID    string `json:"ruleId"`
    Level     Level  `json:"level"`
    Kind      string `json:"kind"`
    Namespace string `json:"namespace,omitempty"`
    Name      string `json:"name"`
    Message   string `json:"message"`
    Evidence  string `json:"evidence,omitempty"`
}
```

`Match.Namespaces` is a list of exact names, `Labels` matches the object's own
labels (all pairs must match), and `NamespaceLabels` matches the labels on the
object's namespace — the maintainable way to write "production".

### The evaluation entry point

```go
// Inputs is everything Evaluate is allowed to see. The caller assembles it;
// Evaluate makes no call of its own, which is what keeps this package pure and
// unit-testable with fake objects.
type Inputs struct {
    // Objects holds the selectable resources, keyed by kind.
    Objects map[string][]*unstructured.Unstructured
    // Namespaces backs Match.NamespaceLabels.
    Namespaces []*unstructured.Unstructured
    // PDBs and HPAs back the two relations.
    PDBs []*unstructured.Unstructured
    HPAs []*unstructured.Unstructured
    // Unreadable names the kinds whose read failed, so rules against them are
    // reported as not evaluated rather than as passed.
    Unreadable map[string]bool
}

// Evaluate applies every rule to every matching object. Violations are sorted
// by (RuleID, Kind, Namespace, Name). NotEvaluated lists the rule ids whose
// kind could not be read.
func Evaluate(rules []Rule, in Inputs) (violations []Violation, notEvaluated []string)
```

## Evaluation over unstructured objects

Paths resolve against the object converted with
`runtime.DefaultUnstructuredConverter.ToUnstructured`, so a path is written
exactly as the field appears in `kubectl get -o yaml`. Authoring against what
the operator can already see beats authoring against Go field names, and it
keeps `internal/policy` free of any dependency on the typed API packages.

A path is dot-separated. The single wildcard form is `[*]`, which iterates a
list. `spec.containers[*].image` resolves to one entry per container, in order.
Map keys containing dots are not addressable; that is an accepted limitation.

Kind names are written bare (`Deployment`, not `apps/v1 Deployment`). No two
kinds in the selectable set share a name, so a bare name is unambiguous.

### Resolution yields slots, not values

This is the subtlest part of the design and getting it wrong produces silently
wrong results, so it is pinned precisely.

A path resolves to an **ordered list of slots**. Each slot is either a value or
**absent**. A wildcard produces one slot per list element *even when that
element does not have the rest of the path*: on a Pod with three containers
where only one sets a CPU limit, `spec.containers[*].resources.limits.cpu`
resolves to three slots — one value and two absent — never to a single value.

The naive alternative, where a missing field simply contributes nothing, makes
`exists` pass on that Pod because "at least one value was found". A rule reading
"every container must set a CPU limit" would then be satisfied by one container
out of three. That is the failure this design exists to avoid.

**Every slot must satisfy the assertion. The first slot that fails becomes the
violation**, and one resource produces at most one violation per rule.

| operator | absent slot | zero slots |
|---|---|---|
| `exists` | violation | violation |
| `notExists` | satisfied | satisfied |
| every other operator | **skipped** | no violation |

Skipping absent slots for the comparison and membership operators is deliberate:
a policy must not turn a field nobody set into a false accusation. The
consequence is that "the memory limit must be under 4Gi" is genuinely two rules
— an `exists` rule and an `lte` rule — because a container that sets no limit
resolves to an absent slot and `lte` has nothing to judge. That falls out of the
one-match-one-assertion model, so it is at least consistent, but it is the
single thing most likely to trip an author and it must be the first warning in
the feature documentation.

`Violation.Evidence` is the failing slot's value. For a slot that fails because
it is absent, `Evidence` is empty and the rule's `message` carries the meaning.

### The operator set

Ten operators, closed. Anything else is a load error.

| operator | meaning |
|---|---|
| `exists` | at least one resolved value, and it is not the empty string |
| `notExists` | no resolved value |
| `in` | value is exactly one of `values` |
| `notIn` | value is none of `values` |
| `matches` | value matches at least one glob in `values` |
| `notMatches` | value matches no glob in `values` |
| `gt` `gte` `lt` `lte` | numeric or quantity comparison against `values[0]` |

`in`, `notIn`, `matches`, `notMatches` require a non-empty `values`. The four
comparison operators require exactly one value. `exists` and `notExists` reject
`values`. All of this is checked at load, not at evaluation.

**Glob, not regex.** This is not a ReDoS concern — Go's `regexp` is RE2 and runs
in linear time. It is that `path.Match` refuses to let `*` cross a `/`, which
breaks `registry.example.com/*` against `registry.example.com/team/app:1.0`, and
that a real regex is a worse authoring surface for image references than a glob.
`internal/policy/glob.go` implements one matcher where `*` matches any run of
characters including `/`, and `?` matches exactly one. No other metacharacter is
special. It is iterative, allocation-free, and has its own fuzz target.

**Quantity comparison.** `gt`/`gte`/`lt`/`lte` first try to parse both sides as
plain numbers. If either side is not a plain number, both are parsed with
`k8s.io/apimachinery/pkg/api/resource.ParseQuantity`, so
`spec.containers[*].resources.limits.memory` is comparable against `4Gi` and
`...limits.cpu` against `500m`. If either side still fails to parse, the
assertion produces no violation and the resource is skipped — a policy must not
turn an unparseable field into a false accusation.

### Cross-resource relations

Exactly two, both over kinds already collected:

- **`hasPodDisruptionBudget`** — some PDB in the same namespace whose selector
  matches the workload's pod-template labels. Valid on `Deployment`,
  `StatefulSet`, `ReplicaSet`, `DaemonSet`.
- **`hasHorizontalPodAutoscaler`** — some HPA in the same namespace whose
  `scaleTargetRef` names this workload by kind and name. Valid on `Deployment`,
  `StatefulSet`, `ReplicaSet`.

`assert: {relation: hasPodDisruptionBudget}` produces a violation when the
relation does **not** hold. There is no inverse form in v1, and a relation used
on a kind it is not valid for is a load error, not a silent pass.

Relations receive their side inputs (the namespace's PDBs, the namespace's HPAs)
through the `Inputs` value the caller assembles, so `Evaluate` stays pure.

## Selectable kinds

A rule may select any of these 23 kinds, and only these:

| API group | kinds |
|---|---|
| (core) | Pod, Node, Namespace, Service, ConfigMap, PersistentVolumeClaim, PersistentVolume, ResourceQuota |
| apps | Deployment, ReplicaSet, StatefulSet, DaemonSet |
| batch | Job, CronJob |
| discovery.k8s.io | EndpointSlice |
| networking.k8s.io | Ingress, IngressClass, NetworkPolicy |
| storage.k8s.io | StorageClass |
| policy | PodDisruptionBudget |
| autoscaling | HorizontalPodAutoscaler |
| admissionregistration.k8s.io | ValidatingWebhookConfiguration, MutatingWebhookConfiguration |

Every one is already in `rbacprofile`'s `coreRules`, so **the shipped ClusterRole
does not change and `rbac print` keeps telling the truth**. A policy can never
require a grant kubeagent does not already hold. That property is what makes
policy compatible with the least-privilege work shipped in v0.69.0, and it is
worth more than the CRD support it costs.

### Kinds deliberately excluded, and why

- **Secret.** `collect.TLSSecrets` reads Secrets for certificate expiry, so a
  Secret is technically within reach. It is excluded anyway, at the kind level,
  because a violation carries evidence and evidence would be secret material.
- **ConfigMap `data` and `binaryData`.** ConfigMap is selectable, but these two
  fields are **not resolvable paths** — a path beginning `data.` or
  `binaryData.` on a ConfigMap is a load error. Without this, a policy file
  committed to a repository could be crafted to lift configuration values into a
  SARIF report uploaded to a code-scanning dashboard. kubeagent must not become
  an exfiltration channel for whoever can land a file in `policies/`. Policy on
  ConfigMap metadata (labels, annotations, ownership) remains available, which
  is what the legitimate use cases actually want.
- **Event and Lease.** Collected, but carrying no policy value: an Event is a
  transient record and a node heartbeat Lease is machine state.

## Loading

`--policy PATH` is registered on **`scan` and `gate` only**, repeatable, and
accepts a file or a directory. A directory contributes its `*.yaml` and `*.yml`
entries in sorted order, non-recursively. Per the CLI invariant the flag is
declared per command and is never a persistent flag.

**There is no implicit discovery.** No `./kubeagent-policy.yaml` pickup, no
`~/.config/kubeagent`, no environment variable naming a file. A scan's output
must not depend on the working directory, and for a tool that gates deploys,
configuration found by accident is worse than configuration not found.

Loading is **fail-fast and happens before any cluster call**. A load error is
fatal and the command exits without connecting. The errors:

- unreadable path, or a directory containing no policy file
- YAML that does not parse
- an unknown field (strict decoding via `yaml.UnmarshalStrict`)
- unknown `kind`, `op`, `relation`, or `level`
- empty `id`, or a duplicate `id` across all loaded files
- both `path` and `relation` set, or neither
- an operator whose `values` arity is wrong
- a `path` beginning `data.` or `binaryData.` on a ConfigMap
- a relation used on a kind it is not valid for
- an empty `message`

A load error names the offending file and rule id **on stderr only**. It never
appears in JSON, SARIF, or the HTML report. This is the same carve-out shape
already accepted for `kubeagent mcp`'s startup connection error: the operator's
own channel may carry a path, forwarded artifacts never may.

## Demand-driven collection

`policy.Kinds(rules)` returns the distinct kinds the loaded rules reference.
`internal/scan` maps each to the `collect.X` call that already exists for it and
fetches only what the run does not already have, through the existing
`internal/parallel` worker pool. No new collector function, no new API surface,
and determinism is preserved the same way every other parallel read is: each
closure writes only its own destination.

## A refused read is never a pass

If a policy references a kind whose read fails — RBAC refusal, timeout, anything
— the rules for that kind are reported as **not evaluated**. They are never
reported as passed.

This is the principle from v0.69.0 ("a refused read is named as a blind spot
instead of rendering an empty section") applied where it matters most: a
silently skipped registry-allowlist rule in CI is a security failure wearing a
green build. Concretely:

- the failed read enters `scan.Result.PartialReads` as it does today, and
  renders in the existing blind-spots section
- `scan` renders the affected rule ids as not evaluated
- **`gate` treats an unevaluated rule at or above `--fail-on` as a gate
  failure**, with a verdict reason naming the rule and the kind

## Severity and the cluster verdict

A violation never moves the cluster verdict. `scan`'s verdict is kubeagent's own
judgement about cluster health, computed from cluster state; a rule about
required labels is not cluster health, and a verdict that depends on a file
stops being reproducible from the cluster alone. This is the contract `--drift`
and `--capacity` already carry.

In `gate`, violations are ordinary findings at their declared level and cross
the existing `--fail-on` threshold, so CI enforcement needs no new flag.

## Surfaces

| surface | change |
|---|---|
| `scan` text | a `POLICY` section, rendered only when `--policy` was given |
| `scan --output json` | `policyViolations`, `omitempty` |
| `scan --output html` | the same section; `htmlreport` imports `policy`, which is pure |
| `gate` | violations map into `findings.Finding`; `Issue` is `policy/<ruleID>` |
| `gate --output sarif` | the rule id becomes the SARIF rule id |
| `kubeagent policy validate PATH…` | validates without a cluster or a kubeconfig |

`policy validate` follows the shape of `kubeagent schema`: it contacts nothing,
reads no kubeconfig, prints a one-line summary (`N rules across M kinds`) on
success, and exits non-zero with the load error on failure. It exists so a
policy file can be checked in CI before the cluster is involved.

### Schema versions

`scan` and `gate` both gain a field, so both bump — additively, MINOR:

- `jsonschema.ScanVersion` `1.0` → `1.1`
- `jsonschema.GateVersion` `1.0` → `1.1`

Regenerated with `go test ./internal/schemadoc -run TestSchemaDrift -update`,
which must classify both as additive.

### Golden output

`internal/report/testdata/golden-scan.txt` **stays byte-identical**: the golden
run passes no `--policy`, so no section is rendered and the JSON field is
omitted. A second golden fixture covers a scan with a policy loaded. The demo
GIF and `website/docs/quickstart.md` therefore need no refresh.

## RBAC

`internal/rbacprofile` gains a `policy` feature entry:

```go
{
    Name:      "policy",
    Flag:      "--policy",
    Summary:   "organization-specific checks from a policy file; reads only kinds core already grants",
    CoveredBy: "core",
    ScanOnly:  true,
}
```

`ScanOnly` is true because the watch daemon does not accept `--policy` — it is
out of scope here — so the Helm chart must gate no grant for it. `HelmCondition`
is therefore empty, which the `Feature` doc comment already requires of a
`ScanOnly` feature.

`Rules` is nil. The entry exists so `rbac print --features` lists the feature and
`rbac check` confirms an identity can run it, and it encodes the invariant in
the one table that generates every manifest: **a policy can never require a
grant beyond core.** A future change that let a policy read a new kind would have
to change this entry, which is exactly where such a change should be visible.

## Sanitization

- `Rule.Message` is operator-authored but may arrive from a repository. It is
  passed through `safetext.Line` at **load** time.
- `Violation.Evidence` is a value read from the cluster and is untrusted. It is
  passed through `safetext.Line` at **evaluation** time and truncated to 120
  runes.
- **Matching runs on the raw value.** Sanitizing before matching would let a
  control character spliced mid-word evade a glob — the existing invariant,
  restated because this is a new place it applies.

## Determinism

`Evaluate` sorts violations by `(RuleID, Kind, Namespace, Name)`. Rule order in
the file does not affect output; file order in a directory does not affect
output. Evaluating the same inputs twice produces byte-identical results, and a
fuzz target asserts it.

## Testing

- **Unit** — table-driven tests on the pure evaluator with fake objects, in the
  established style. `path.go`, `glob.go`, `op.go` and `relation.go` each get
  their own table.
- **Load** — one table of malformed policy files, one error each, asserting the
  message names the file and the rule id.
- **Fuzz** — four new targets, all under the rules `internal/fuzzgen` already
  established:
  - `FuzzLoadPolicy` — arbitrary bytes never panic; the result is an error or a
    valid policy
  - `FuzzEvaluatePolicy` — `fuzzgen` objects against a seed rule set never
    panic, always produce identical bytes on a second run, and every emitted
    string passes `AssertSafe`
  - `FuzzResolvePath` — arbitrary path strings against arbitrary objects
  - `FuzzGlob` — arbitrary pattern and subject
- **Golden** — `golden-scan.txt` unchanged; a second fixture for a policy run.
- **Schema drift** — `TestSchemaDrift` must report both bumps as additive.
- **Layering** — the existing import-invariant test gains `internal/policy` to
  the set that must not reach `remediate` or `explain`.

## Gate

Full chaos gate, not the lightweight smoke: this touches `internal/collect` and
evaluates against real cluster state. The suite runs with `ANTHROPIC_API_KEY`
unset, as always.

Additional gate evidence specific to this sub-project:

1. A scan with **no** `--policy` is byte-identical to a `main` binary across
   text, JSON, and HTML.
2. A policy whose kind is refused by a deliberately narrowed role reports the
   rules as not evaluated, and `gate` fails rather than passes.
3. `policy validate` runs with no kubeconfig present at all.

## Documentation

- `website/docs/features/policy.md`, with the empty-list rule as its first
  warning and every example using RFC 2606 domains and RFC 5737 addresses
- `website/mkdocs.yml` nav entry after "Shell completion"
- `website/docs/roadmap.md` — Theme H slice 7 stamped when it ships
- `CHANGELOG.md` under `[Unreleased]`
- `CLAUDE.md` — the policy invariants: `internal/policy` is pure and must never
  import `remediate` or `explain`; a policy can never require a grant beyond
  core; Secret is not selectable and ConfigMap `data` is not resolvable
- `docs/go-concepts.md` — a new entry on type switches over `any`, which is what
  path resolution over unstructured data requires and which no existing entry
  covers

## Global constraints

Carried verbatim into the implementation plan:

- Every commit needs a `Signed-off-by` trailer matching its author
  (`git commit -s`); `main` enforces DCO. No `Co-Authored-By` and no AI
  attribution anywhere — commits, PR bodies, code, docs, changelog.
- Read-only toward the cluster: `get`/`list` only. A policy can never write, and
  policy has no `--fix` integration. This is the contract, not a v1 limitation.
- Detectors stay pure functions; `internal/policy` is pure — no client, no
  context.
- `internal/report/testdata/golden-scan.txt` stays byte-identical.
- `go test` runs with `-p 2` and never `-short`; CI's `go test -race ./...` must
  stay green.
- No secrets, private IPs, or internal hostnames anywhere, including policy
  examples, seed corpora, and test tables. Documentation IPs are RFC 5737
  (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains are
  RFC 2606 (`example.com`, `example.org`).
- URLs are credentials: no artifact carries more than `scheme://host`.
- Filesystem paths are credentials: a policy path may appear on stderr and
  nowhere else.
- Blocked and refusal reasons are kubeagent's own words, never the API server's.
- **No new dependency.** `sigs.k8s.io/yaml` is already a direct require and
  `k8s.io/apimachinery` is already in use; `go.mod` and `go.sum` do not change.

## Out of scope

Each is additive later without breaking a file written against v1:

- `allOf` / `anyOf` / `not` composition
- regular expressions
- CRDs and arbitrary kinds via the dynamic client
- offline manifest linting (`policy eval -f manifests/`)
- the watch daemon, MCP, and TUI surfaces
- shipped policy bundles or presets
- inverse relations, and relations beyond the two above
