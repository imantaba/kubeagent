# Compatibility and support

kubeagent 1.0 is a commitment, not just a version number. This page says
exactly what that commitment covers — and, just as importantly, what it does
not, because a promise that quietly includes everything is a promise nobody can
keep.

kubeagent follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
From 1.0 onward, a **MAJOR** bump is the only release that may break a stable
surface listed below. A **MINOR** adds; a **PATCH** fixes.

## Stable surfaces

### The command line

Every command and flag that `kubeagent` prints in its own usage text is stable.
Within 1.x:

- No command is removed or renamed.
- No flag is removed, renamed, or given a different meaning.
- No flag's default changes in a way that changes what a run reports.
- A flag that takes a value keeps taking one, and keeps accepting the values it
  accepted before.

New commands and new flags may arrive in any MINOR. A run that pinned itself to
the 1.0 surface keeps working.

**The single-dash long-flag spelling is part of this.** kubeagent's CLI moved
from the standard library's `flag` package to Cobra in v0.73.0, and pflag
rejects `-kubeconfig` where the standard library accepted it. `internal/cli`
normalizes a leading `-longname` back to `--longname` for any name the target
command registers, so command lines written against v0.72 and earlier still run
unchanged. Removing that shim would be a breaking change.

### Exit codes

`kubeagent gate` exists to be branched on by a pipeline, so its exit codes are
fixed and may never be reassigned:

| Code | Meaning |
|------|---------|
| `0` | Pass — nothing at or above `--fail-on` |
| `1` | Fail — findings at or above `--fail-on` |
| `2` | Inconclusive — kubeagent could not see enough to judge |
| `3` | Timeout — `--wait-for` did not settle within `--timeout` |
| `4` | Usage — bad flags or arguments |

Code `2` is the one worth wiring up deliberately: a gate that could not read
what it needed is not a passing gate, and kubeagent refuses to report it as one.

Every other command exits `0` on success and `1` on failure. `kubeagent rbac
check` exits `1` when the identity is missing a grant the selected features
need — that is a verdict, not an error.

### The JSON documents

Four surfaces — `scan`, `gate`, `rbac`, and the watch daemon's HTTP endpoints —
each carry their own `schemaVersion` and their own published JSON Schema, and
each versions **independently of the kubeagent release**. A new `scan` field does
not disturb a script reading the `gate` document.

That contract has its own page: **[JSON schema contract](features/json-schema.md)**,
which covers what MINOR and MAJOR mean for a document, how to pin to one, and
what a `schemaVersion` deliberately does not promise. It is the authority; this
page does not restate it.

### The Helm chart's values

Documented keys in `deploy/helm/kubeagent/values.yaml` keep their names and
meanings within 1.x. New keys may be added. The chart's own `version` moves
independently of the application's `appVersion`.

## Unstable surfaces — do not build on these

- **The text report's wording and layout.** `internal/report/testdata/golden-scan.txt`
  is a regression guard so a change to the report is always deliberate — it is
  not a promise to consumers. Parse `--output json`, never the text.
- **The HTML report's markup.** It is a shareable artifact for a human, and its
  structure will change.
- **Every `internal/` package.** Go's `internal/` rule already enforces this:
  nothing outside the module can import them, and the layout is free to move.
  kubeagent is a binary, not a library.
- **`--explain` and `--investigate` output.** Model-generated prose. It has a
  shape; its content is not deterministic and never will be.
- **Any human-readable string** — `reason`, `summary`, `evidence`. Match on
  `issue`, which is a stable identifier.

## Kubernetes versions

kubeagent supports **v1.32, v1.33, and v1.34**.

This is an evidenced window rather than an asserted one. A nightly GitHub
Actions matrix runs the full 20-scenario chaos suite — real injected outages, on
a real cluster — once per supported minor, each on its own disposable kind
cluster, with 105 machine-checked assertions per cell. A minor is listed here
because that suite passes on it, and it stops being listed when the suite stops
being run against it.

The window moves forward as Kubernetes releases: a new minor is added after the
matrix passes against it, and the oldest is dropped once it leaves upstream
support. Adding or dropping a minor is a MINOR release, not a MAJOR one — the
support window is a statement about what is tested, not a stable API.

**What the matrix does not cover:** one distribution (kind), one architecture
(amd64), one CNI (Calico), on `ubuntu-latest`. kubeagent uses only stable
`client-go` APIs and should work on any conformant cluster in the window, but
EKS, GKE, AKS, OpenShift, k3s, and RKE2 are not gated in CI. Cross-distribution
coverage is on the roadmap.

## Deprecation policy

A stable surface is never removed without warning:

1. It is announced as deprecated in the CHANGELOG for the release that
   deprecates it.
2. It keeps working, unchanged, for **at least one full MINOR release**.
3. Using it prints a warning to **stderr** — never to stdout, so a pipeline
   parsing JSON is never corrupted by a deprecation notice.
4. It is removed only in the next MAJOR.

Nothing is deprecated as of 1.0.

## What is read-only, and what is not

This is a safety contract rather than a compatibility one, but it is the one
most worth being explicit about, and it does not change in 1.x.

kubeagent is **read-only toward your cluster by default**. `scan`, `watch`,
`gate`, `mcp`, `tui`, `rbac print`, `policy validate`, `schema`, `version`, and
`completion` issue only `get`, `list`, and `watch` calls. There is no code path
from a policy rule, an MCP tool call, or a model response into a write.

Two documented exceptions:

- **`scan --fix`** is the sole write path. It is opt-in, guard-railed by a fixed
  allowlist, refuses protected namespaces, confirms each action, re-verifies
  afterwards, and is **never LLM-decided**. See
  [Remediation (--fix)](features/remediation.md).
- **`kubeagent rbac check`** creates `SelfSubjectAccessReview` objects — the same
  virtual resource `kubectl auth can-i` uses. The API server evaluates them and
  never persists them. It is the only non-`--fix` path that issues a POST, and it
  changes no cluster state.

`kubeagent mcp`, `kubeagent gate`, `kubeagent tui`, and `kubeagent rbac` make no
model API call at all, whatever flags you pass. Read-only and makes-no-external-call
are separate promises, and both hold for those four.
