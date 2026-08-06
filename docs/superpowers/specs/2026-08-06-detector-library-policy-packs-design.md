# Curated policy packs — design

**Status:** approved
**Roadmap item:** post-1.0 item 3, second half — "a curated community detector
library". The first half, the known-issues knowledge base, shipped as v1.8.0.

## What this is

A curated pack of reliability rules, compiled into the binary and run by name:

```bash
kubeagent policy packs                                # what ships
kubeagent scan --policy-pack reliability              # run one
kubeagent policy packs --print reliability > mine.yaml  # fork it
```

The rules catch, before a workload goes live, the same failures kubeagent's
detectors diagnose after it does: a container with no readiness probe, no
memory limit, a floating `:latest` tag, a single replica with no disruption
budget.

## What was already decided

Three decisions are inherited, not reopened.

**A community detector is a policy rule, not compiled Go.** The v1.0 roadmap
records the reason: policy-as-code was chosen over a compiled plugin SDK
specifically so a custom check can never write to the cluster, panic a scan, or
widen RBAC. A pack changes none of that — it is data evaluated by the same pure
evaluator that already ships.

**A pack rule gates by its own level.** `--policy` already works this way;
a pack introduces no second gating rule.

**Distribution is by name, embedded.** The pack ships inside the binary, listed
and printable, so an operator with only a container image or a krew install has
it. This mirrors `internal/knownissues`, which shipped its registry the same way
for the same reason.

## What runs a pack

Nothing new. `internal/policy` already:

- accepts `Document{Source, Data}` — bytes plus a name for errors, not a path;
- loads many documents at once, so a duplicate rule id is caught **across**
  documents rather than only within one;
- matches on kind, namespaces, labels and namespace labels;
- asserts over ten operators or two relations
  (`hasPodDisruptionBudget`, `hasHorizontalPodAutoscaler`);
- restricts selectable kinds to exactly what `rbacprofile.coreRules` grants.

A pack is therefore a `[]byte` of YAML handed to the existing loader. The
slice adds a place to keep those bytes and a way to name them — no evaluator
change, no new operator, no new relation.

## Architecture

### `internal/policypack` — new package, stdlib-only

Holds the embedded YAML and its metadata. It **imports nothing from kubeagent
and nothing outside the standard library** (`embed`, `fmt`, `sort`, `strings`),
which puts it in the same class as `internal/jsonschema`, `internal/dashboard`,
`internal/baseline`, `internal/glob` and `internal/knownissues`. Reaching
`internal/remediate` or `internal/explain` is impossible by construction rather
than by rule. `internal/policypack/imports_test.go` enforces both halves, on
`internal/baseline/imports_test.go`'s pattern.

It holds no client and no context, issues no cluster call and makes no model
call — two separate promises.

```go
// Pack is one curated rule set, compiled into the binary.
type Pack struct {
    Name        string // "reliability"
    Summary     string // one line, for the list
    RuleCount   int
}

func All() []Pack                       // sorted by name
func Lookup(name string) (Pack, bool)
func Bytes(name string) ([]byte, bool)  // the YAML, for the loader or --print
```

`Bytes` returns a copy, so a caller cannot mutate the embedded pack — the same
promise `knownissues.All` makes about its nested slices.

Returning raw bytes rather than `[]policy.Document` is what keeps the package
stdlib-only: the caller in `internal/cli` builds the `Document`, setting
`Source` to `pack:reliability`. That source string reaches an error message,
and it is deliberately **not** a filesystem path — there is no path to leak.

### `internal/policy` — unchanged

No change to the evaluator, the loader, the operators, the relations or the
selectable-kind set. The pack is input.

### `internal/cli` — one flag, one verb

- `--policy-pack <name>` on `scan` and `gate`, repeatable, alongside the
  existing `--policy`. Both may be given; the loader already refuses a
  duplicate rule id across documents, which is why pack rule ids are
  namespaced (`reliability.deploy-readiness-probe`) and an operator's own
  ids will not collide by accident.
- `kubeagent policy packs` lists what ships; `--print <name>` writes the YAML
  to stdout so an operator can fork a rule instead of arguing with it.

An unknown pack name is refused in `RunE` with the same shape as the
known-issues miss: the name quoted with `%q`, and the valid names listed.

## The reliability pack

Fourteen rules. Each maps to a failure the detectors already diagnose at run
time, which is what makes this pack kubeagent's rather than a generic linter's.

| id | kind | assertion | level |
| --- | --- | --- | --- |
| `reliability.deploy-readiness-probe` | Deployment | `readinessProbe` exists | warning |
| `reliability.deploy-liveness-probe` | Deployment | `livenessProbe` exists | info |
| `reliability.statefulset-readiness-probe` | StatefulSet | `readinessProbe` exists | warning |
| `reliability.daemonset-readiness-probe` | DaemonSet | `readinessProbe` exists | info |
| `reliability.deploy-memory-limit` | Deployment | `limits.memory` exists | warning |
| `reliability.statefulset-memory-limit` | StatefulSet | `limits.memory` exists | warning |
| `reliability.deploy-cpu-request` | Deployment | `requests.cpu` exists | info |
| `reliability.deploy-memory-request` | Deployment | `requests.memory` exists | info |
| `reliability.deploy-image-not-latest` | Deployment | `image` notMatches `*:latest` | warning |
| `reliability.deploy-image-tagged` | Deployment | `image` matches `*:*` | info |
| `reliability.deploy-replicas-min-two` | Deployment | `spec.replicas` gte 2 | warning |
| `reliability.deploy-pdb` | Deployment | `hasPodDisruptionBudget` | warning |
| `reliability.cronjob-concurrency-policy` | CronJob | `spec.concurrencyPolicy` in [Forbid, Replace] | info |
| `reliability.pvc-storage-class` | PersistentVolumeClaim | `spec.storageClassName` exists | info |

### Why the levels are what they are

**No rule in this pack is `critical`, deliberately.** `gate`'s default is
`--fail-on critical`, so adding `--policy-pack reliability` to a pipeline
cannot suddenly fail a build that passed yesterday. An operator who wants these
to block raises `--fail-on warning` — an explicit act. This is the same
reasoning the baseline slice used to make a learned deviation `findings.Info`.

### Two semantics a rule author must know

**`[*]` produces one slot per element, and every slot must satisfy.** A
Deployment with three containers where only one sets a memory limit violates
`deploy-memory-limit` — it does not pass because one was found. This is what
makes the probe and limit rules useful rather than decorative, and it is
already documented behaviour, not something this slice introduces.

**`matches`/`in`/`gte` are skipped on an absent field; `exists` violates.**
So `deploy-image-tagged` catches `image: nginx` (no colon, so no match) but a
registry with a port and no tag — `registry.example.com:5000/app` — contains a
colon and passes. That is a known and accepted limitation of a glob, recorded
on the docs page rather than papered over.

## What is deliberately not in this slice

- **Security and cost packs.** Reliability first; the pack machinery is
  built so a sibling is a YAML file and a table row, not a redesign.
- **Operator-contributed packs at run time.** The pack ships with the binary
  and is curated. An operator who wants their own rules already has `--policy`,
  and `--print` gives them this pack as a starting point.
- **A pack on by default.** Opt-in only. A default-on pack would add a policy
  section to every scan, move the scan JSON for every existing consumer, and
  fail gates that passed yesterday — a breaking change under the 1.x contract.
- **Any change to the evaluator.** No new operator, no new relation, no new
  selectable kind.

## Contracts this slice does not move

- **No JSON schema change.** A pack violation is an ordinary policy violation
  through the existing path, so `scan` stays at **1.2** and `gate` at **1.1**.
  No ninth document. Nothing is regenerated with `-update`.
- **`internal/report/testdata/golden-scan.txt` stays byte-identical** — the
  pack is opt-in, so a plain `scan` renders exactly as before. The demo GIF and
  `website/docs/quickstart.md` are not regenerated.
- **No RBAC change.** Every rule selects a kind already in
  `policy.SelectableKinds()`, which is pinned to `rbacprofile.coreRules`. A
  test asserts it, so `rbac print` keeps telling the truth.
- **No new dependency.** `go.mod` and `go.sum` do not change. `embed` is
  standard library.

## Testing

`policy.Load` already refuses a rule whose kind is not selectable, whose level
is unknown, whose message is empty, or whose id collides with one in another
document — and it runs every message through `safetext.Line` on the way in, so
a pack message is sanitized at the same ingress an operator's file is. A pack
test that re-asserted any of that would be testing `Load`. So the pack tests
assert what `Load` does **not**:

- `internal/policypack/imports_test.go` — stdlib-only, no kubeagent import.
- `internal/policypack/packs_test.go` — imports `internal/policy` (which does
  not import `policypack`, so no cycle) and, for every embedded pack: `Load`
  succeeds on it, which is the whole of the above in one assertion; every rule
  id carries its pack's prefix; `RuleCount` matches the rules actually loaded,
  so the listing cannot drift from the YAML; `Bytes` returns a copy, proved by
  mutating the result and reading again; and **no rule is `critical`**, which
  is the promise that adding a pack to a gate cannot fail it at default
  settings.
- Behavioural table per rule: a fixture object that violates and one that
  satisfies, run through `policy.Evaluate` with inputs built by
  `policy.InputsFrom` — the rule table above is proved, not asserted.
- A credential test over the pack YAML and every rule message: no host, no IP,
  no object name that is not a placeholder.
- `internal/cli` tests for `policy packs`, `--print`, the unknown-name error,
  and `--policy-pack` combined with `--policy`.

## Credentials

The pack YAML names no real host, image, namespace or workload. The one
registry example is `registry.example.com` (RFC 2606). Rule messages use
placeholders, never an object name. `Document.Source` is `pack:<name>`, not a
filesystem path.
