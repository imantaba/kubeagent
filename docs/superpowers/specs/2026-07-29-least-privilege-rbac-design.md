# Least-privilege RBAC profiles — design

Theme H, sub-project 1 of the drive to the v1.0 production contract.

## The problem

kubeagent ships eight RBAC manifests: one broad `kubeagent-readonly` ClusterRole
granting `get, list, watch` across ten API groups, and seven opt-in add-ons
(`rbac-certs.yaml`, `rbac-controlplane.yaml`, `rbac-diskusage.yaml`,
`rbac-dnshealth.yaml`, `rbac-gitops.yaml`, `rbac-logs.yaml`,
`rbac-operators.yaml`). The Helm chart carries its own copy of the same rules in
`templates/clusterrole.yaml`.

Nothing connects those manifests to the code that needs them. The mapping from a
feature to the permissions it requires lives in prose header comments and in
whatever API calls a collector happens to make. Three consequences follow, and
all three are visible in the tree today:

**The copies have already drifted.** `deploy/` ships add-on manifests for
`--logs`, `--operators` and `--drift`; the Helm chart has no toggle for any of
them. A chart user who enables those features gets no grant and no warning.

**An operator cannot find out what a feature costs without reading Go.** The
question "if I turn on `--certs`, what am I granting?" is answerable only by
grepping `internal/collect`. There is no command that answers it, and no
machine-readable form of the answer.

**A missing grant is silent.** `scan.Result.PartialReads` records read failures
and every renderer displays them, but its eighteen `note()` call sites cover only
the core resource lists. The feature-flagged collectors do not participate. Two
of them cannot participate as written: `collect.NodeStats` returns
`(zero, false, nil)` and `collect.PreviousLogs` returns `("", false)` on *any*
failure, so a missing `nodes/proxy` or `pods/log` grant is not merely unreported
— it is unrepresentable. The result is an empty section that looks like a clean
bill of health.

That last one is the same class of defect `kubeagent gate` exists to prevent:
green when blind.

## Goals

1. One machine-checkable source of truth for feature → permission.
2. The shipped manifests and the Helm chart are generated from it, so drift
   fails a test instead of reaching a user.
3. An operator can ask, before running anything, which features their current
   identity can actually use.
4. A feature blocked by a missing grant says so, in every output format.

## Non-goals

- Narrowing `kubeagent-readonly` itself. The in-cluster daemon binds it by name;
  changing its rules would break existing installs. The narrower profile arrives
  additively (see "Profiles" below).
- Namespace-scoped Roles. Every current consumer is cluster-scoped.
- Any change to `--fix`, `internal/remediate`, or `internal/explain`.

## Architecture

### 1. The table — `internal/rbacprofile`

A new package holds the table and the rendering. It has no Kubernetes client and
no context in the table half; the `check` half takes a client.

```go
// Rule is one RBAC rule: either resources in an API group, or non-resource
// URLs, plus the verbs needed. Exactly one of Resources and NonResourceURLs
// is set.
type Rule struct {
	APIGroup        string
	Resources       []string
	NonResourceURLs []string
	Verbs           []string
}

// Feature is one thing an operator can turn on, and the permissions it costs.
type Feature struct {
	Name          string // "core", "certs", "logs", "watch"
	Flag          string // "--certs"; "" for core and for subcommands
	Summary       string // one line, shown by `kubeagent rbac`
	Doc           string // the header comment its generated manifest carries
	Manifest      string // "rbac-certs.yaml"; "" = folded into the base manifest
	RoleName      string // "kubeagent-certs"
	HelmCondition string // raw template condition; "" = not chart-gated
	Rules         []Rule
}
```

The entries, exhaustively:

| Name | Flag | Rules |
|------|------|-------|
| `core` | — | the ten API groups of today's base role, at `get, list` |
| `certs` | `--certs` | `secrets: list` |
| `logs` | `--logs` | `pods/log: get` |
| `diskusage` | `--disk-usage` | `nodes/proxy: get` |
| `kubelethealth` | `--kubelet-health` | `nodes/proxy: get` |
| `dnshealth` | `--dns-health` | `pods/proxy: get` |
| `controlplane` | `--control-plane-health` | `/readyz: get` (nonResourceURL) |
| `operators` | `--operators` | seven CRD groups at `list` |
| `gitops` | `--drift` | three of those seven groups at `list` |
| `capacity` | `--capacity` | none |
| `security` | `--security` | none |
| `pvcreclaim` | `--pvc-reclaim` | none |
| `credlint` | `--lint-secrets` | none |
| `cronjobs` | `--include-cron` | none |
| `restarts` | `--include-restarts` | none |

Subcommands are **not** table entries; they are profiles over these features.
`kubeagent watch` is the `watch` profile, and `gate`, `tui` and `mcp` are the
`scan` profile — none of the three opens a watch. Modeling them as features
would duplicate core's rules four times and give the renderer no way to tell a
subcommand's grant from an add-on's.

The `watch` verb is likewise not a feature. It is a verb elevation applied to
core's rules by the `watch` profile, because a separate `watch` feature would
emit a second copy of all ten rules differing only in verbs, and the renderer
would have no basis for collapsing them.

A feature whose `Rules` is empty is meaningful, not a stub: it records that the
feature needs nothing beyond core. `internal/capacity`, `internal/credlint`,
`internal/secscan` and `internal/pvcreclaim` hold no Kubernetes client at all —
they compute over inputs the core collection already gathered. Stating that in
the table is better than leaving an operator to guess, and `rbac check` reports
those features `ready` whenever core is ready.

`gitops`' three rules are a strict subset of `operators`' seven. The table
expresses that by sharing the rule values; the renderer deduplicates when a
profile pulls in both.

### 2. Profiles

A profile is a named set of features, so an operator does not have to enumerate.

| Profile | Features | Verbs |
|---------|----------|-------|
| `scan` | core | `get, list` |
| `watch` | core, verb-elevated | `get, list, watch` |
| `full` | core + every add-on | `get, list`, plus what each add-on requires |

`scan` is the least-privilege one and is new: a one-shot `kubeagent scan` never
opens a watch, so `watch` on every resource is privilege it does not use.
`deploy/rbac.yaml` continues to render the `watch` profile under the name
`kubeagent-readonly`, unchanged, because the daemon binds it.

### 3. Generation — a golden test, not a separate binary

`internal/rbacprofile/golden_test.go` renders every shipped manifest from the
table and diffs against the committed file. Drift fails the test; `-update`
rewrites the files. This is the pattern `internal/report/golden_test.go` already
establishes, and it has the property that matters here: generation and assertion
are the same code path, so they cannot disagree.

Generated:

- `deploy/rbac.yaml` — ServiceAccount, `kubeagent-readonly` ClusterRole
  (`watch` profile), ClusterRoleBinding.
- `deploy/rbac-<name>.yaml` — one per add-on feature, each keeping its current
  filename and role name so existing `kubectl apply -f` invocations and every
  doc link keep working.
- `deploy/helm/kubeagent/templates/clusterrole.yaml` — the base rules plus one
  `{{- if <HelmCondition> }}` block per chart-gated feature.

Each generated file carries a header line naming the table as its source and the
`-update` command that regenerates it, so a reader who edits the YAML by hand
learns immediately that the edit will be reverted.

Resolving the Helm drift is part of this: `logs`, `operators` and `gitops` gain
`.Values.logs.enabled`, `.Values.operators.enabled` and `.Values.gitops.enabled`
in `values.yaml`, defaulting to `false` — the same default the standalone
add-ons imply, so no existing chart install changes what it grants.

### 4. `kubeagent rbac` — two verbs

```text
kubeagent rbac print [--profile scan|watch|full] [--features a,b,…] [--output yaml|json]
kubeagent rbac check [--kubeconfig path] [--context name] [--output text|json]
```

`print` needs no cluster. It writes the exact minimal ClusterRole for the named
profile or feature list — the same rendering the generator uses, so what an
operator prints is what the project ships.

`check` runs one `SelfSubjectAccessReview` per rule and reports per feature:

```text
core          ready
--logs        ready
--certs       blocked   list secrets not permitted
--disk-usage  blocked   get nodes/proxy not permitted
watch         ready
```

Exit status: 0 when every feature checked is ready, 1 when any is blocked, so a
CI job can assert its identity is sufficient before it depends on a scan.

Argument parsing follows the existing top-level dispatch in `main.go`: a switch
on the verb, then a `flag.FlagSet` per verb. No new dependency; v1's
standard-library-`flag`-only rule holds.

### 5. Reasons are kubeagent's words, never the cluster's

`apierrors.NewForbidden` interpolates the authorizer's own message, so a
Forbidden response embeds the requesting identity — an IAM ARN, a node's internal
DNS name, an OIDC email — and under webhook authorization carries a third-party
backend's free text as well. `SelfSubjectAccessReview.Status.Reason` has exactly
the same provenance.

`internal/htmlreport` already settled this question at
`internal/htmlreport/htmlreport.go:106-124`: classify, never quote. `rbac check`
follows it. The blocked reason is composed from the table — verb, resource,
`"not permitted"` — and `Status.Reason` is never printed, in either output
format. The same holds for blind-spot reasons added below: the text and JSON
scan reports keep quoting the raw error as they do today, because those are not
written to be forwarded, but nothing new starts quoting one.

### 6. Uniform blind spots

Every feature-flagged collector routes a permission failure into
`scan.Result.PartialReads`, so all five renderers — text, JSON, HTML, gate,
TUI — report it without further change.

Two collectors need their signatures widened before they can report anything,
because today they discard the error:

- `collect.NodeStats` — currently `(diskusage.NodeSummary, bool, error)`
  returning `(zero, false, nil)` on every failure. It must distinguish
  forbidden from unreachable.
- `collect.PreviousLogs` — currently `(string, bool)`, where `false` means any
  of: no previous instance, forbidden, transport error, empty log. Only the
  first is normal.

`collect.KubeletHealthz`, `collect.CoreDNSMetrics` and
`collect.ControlPlaneReadyz` already carry an HTTP status code and already
classify 401/403, so they need only to be wired to `note()`.

`collect.TLSSecrets` returns its error and `internal/scan` already sets
`rep.Forbidden` from it; it gains a `note()` call so the blind spot appears
alongside the others rather than only inside the certificate report.

## The read-only invariant

`SelfSubjectAccessReview` is created with a POST. It persists nothing: the API
server evaluates the request against its authorizers and returns the answer in
the response body. No object is stored, no cluster state changes, and it is the
API behind `kubectl auth can-i`. `internal/remediate` already uses it.

Shipping `rbac check` nonetheless puts a POST on a path that is not `--fix`, and
CLAUDE.md currently reads "Only `List`/`Get`-style calls, EXCEPT the opt-in
`--fix`". That invariant gains a second named carve-out, worded like the
existing `kubeagent mcp` one:

> `kubeagent rbac` (`internal/rbacprofile`) is a fifth case: a one-shot,
> read-only command that makes **no LLM calls** and must never import
> `internal/remediate` or `internal/explain`. Its `check` verb creates
> `SelfSubjectAccessReview` objects — a virtual resource the API server
> evaluates and never persists, the same API `kubectl auth can-i` uses. It is
> the sole non-`--fix` path in kubeagent that issues a POST, and it changes no
> cluster state.

`rbac print` touches no cluster at all.

## Testing

- **Table and rendering** — pure functions, unit-tested directly. Profile
  composition, rule deduplication when `gitops` and `operators` are both
  selected, and YAML/JSON rendering.
- **Golden manifests** — the drift test above, covering all nine generated
  files.
- **`rbac check`** — client-go's fake clientset with a reactor returning
  `Allowed: true/false` per `SelfSubjectAccessReview`, asserting the per-feature
  verdicts, the exit status, and that a `Status.Reason` supplied by the fake
  never reaches the output.
- **Blind spots** — fake clientset reactors returning `Forbidden` for
  `secrets`, `pods/log` and the proxy subresources, asserting a named
  `PartialReads` entry for each.
- **Helm** — `helm template` with each new toggle on and off, asserting the
  rendered rules match what the table says that feature needs.
- **`internal/report/testdata/golden-scan.txt`** stays byte-identical: no
  scenario in the golden fixture is forbidden, so no blind spot appears.

## Gate

Full chaos gate. This touches `internal/collect`, `internal/scan`, RBAC
manifests, `nodes/proxy`, and Helm templates — every trigger in the project's
gate rule at once.

The chaos harness gains one scenario: a ServiceAccount bound to the `scan`
profile only, running `kubeagent scan --certs --logs --disk-usage`, asserting
the run succeeds, exits as it does today, and names three blind spots rather
than printing three empty sections.

## Documentation

- `website/docs/features/rbac.md` — new page: the permission table rendered from
  the source of truth, the profiles, both `rbac` verbs, and how a blocked
  feature presents.
- `deploy/README.md` — point the RBAC section at the new page and at
  `kubeagent rbac print`.
- `website/docs/features/ci-gate.md` — note `rbac check` as the preflight for a
  gate job.
- `mkdocs.yml` nav, `CHANGELOG.md` `[Unreleased]`, and the Theme H bullet in
  `website/docs/roadmap.md`.
- `CLAUDE.md` — the carve-out above.
- `docs/go-concepts.md` — no new Go concept is introduced; struct tables,
  `flag.FlagSet` and the fake clientset are all already covered. No entry.
