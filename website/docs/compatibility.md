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

### Environment variables

Every `KUBEAGENT_*` variable this site documents is stable in the same sense a
flag is: within 1.x it is not removed, not renamed, and not given a different
meaning. New ones may arrive in any MINOR.

Two things about them are deliberately **not** promised:

- **The tuning defaults.** `KUBEAGENT_SCAN_WORKERS` defaults to `8` and
  `KUBEAGENT_QPS`/`KUBEAGENT_BURST` default to no client-side limit at all.
  Those numbers are performance behaviour, not contract — they may be retuned in
  a MINOR. What is stable is that setting them works, that they only affect how
  fast a scan reads, and that they never change what it reports. See
  [Performance tuning](features/tuning.md).
- **What a bad value does beyond staying non-fatal.** The tuning variables
  ignore a value they cannot use and run with the default rather than failing
  the scan; that tolerance is the contract, the exact clamping is not.

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

### The watch daemon's `/dashboard` endpoint

`watch --dashboard` serves an HTML page at `/dashboard` on the metrics port.
Within 1.x that endpoint keeps its path, keeps returning HTML when the flag is
set, and keeps returning `404` when it is not. The flag, its
`KUBEAGENT_DASHBOARD` spelling and the `dashboard.enabled` chart value are
stable under the three rules above, like every other flag, variable and chart
key. **The page's markup is not** — it is listed under unstable surfaces below.

## Unstable surfaces — do not build on these

- **The text report's wording and layout.** `internal/report/testdata/golden-scan.txt`
  is a regression guard so a change to the report is always deliberate — it is
  not a promise to consumers. Parse `--output json`, never the text.
- **The HTML report's and the dashboard's markup.** Both are artifacts for a
  human to look at — a shareable file in one case, a page in a browser in the
  other — and their structure will change. Parse `--output json` or `/issues`.
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
Actions matrix runs the full 23-scenario chaos suite — real injected outages, on
a real cluster — once per supported minor, each on its own disposable kind
cluster, with 134 machine-checked assertions per cell. A minor is listed here
because that suite passes on it, and it stops being listed when the suite stops
being run against it.

The window moves forward as Kubernetes releases: a new minor is added after the
matrix passes against it, and the oldest is dropped once it leaves upstream
support. Adding or dropping a minor is a MINOR release, not a MAJOR one — the
support window is a statement about what is tested, not a stable API.

**What the matrix does not cover:** one distribution (kind), one architecture
(amd64), one CNI (Calico), on `ubuntu-latest`. kubeagent uses only stable
`client-go` APIs and should work on any conformant cluster in the window, but
EKS, GKE, AKS, OpenShift, k3s, and RKE2 are **not gated in CI**, and nothing on
this page claims they are.

The harness itself can now be pointed at a cluster it did not create:
`./chaos/run.sh --context <ctx>` runs the subset of scenarios whose blast radius
is a namespace it creates and deletes — all but one, which only reads —
refuses every scenario that would write a cluster-scoped object or touch a
node, and names each skipped scenario and its reason in the assertion summary
— so a partial run can never be mistaken for a full one. That makes a
cross-distribution answer **obtainable by hand**. It does not make one
**gated**, which is still ahead.

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

- **Remediation** — `scan --fix`, and `scan --rollback` which undoes what a
  previous `--fix` recorded in its audit log. These are the only two flags in
  kubeagent that write, they are mutually exclusive, and both are opt-in and
  carry the same guard rails: a fixed allowlist, protected namespaces refused, a
  permission preflight, a per-action confirmation, an audit-log entry, and a
  re-verify afterwards. Neither is **ever LLM-decided**. See
  [Remediation (--fix)](features/remediation.md).
- **`kubeagent rbac check`** creates `SelfSubjectAccessReview` objects — the same
  virtual resource `kubectl auth can-i` uses. The API server evaluates them and
  never persists them. It is the only path outside remediation that issues a
  POST, and it changes no cluster state.

`kubeagent mcp`, `kubeagent gate`, `kubeagent tui`, and `kubeagent rbac` make no
model API call at all, whatever flags you pass. Read-only and makes-no-external-call
are separate promises, and both hold for those four.
