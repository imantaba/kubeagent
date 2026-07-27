# GitOps drift — design (Theme F slice 2)

**Status:** approved, ready for planning
**Date:** 2026-07-27
**Predecessor:** [operator/CRD adapters](2026-07-26-operator-crd-adapters-design.md) (shipped as v0.60.0)

## Goal

Answer one question for a cluster reconciled by Argo CD or Flux: **is this cluster
still converging on Git, and if not, for how long?**

Report it as an advisory `GITOPS DRIFT` section behind an opt-in `--drift` flag.
Strictly read-only, never a Finding, never affects the cluster verdict or the exit
code.

## Background: why slice 1 deferred this

`internal/operators/adapters.go` carries the deferral verbatim:

> `status.sync.status` is deliberately not read: OutOfSync is drift, the next Theme F
> slice, and flagging it here would make every pending deploy look like a failure.

Slice 1 judges *operator-reported health* — is the Application's workload healthy.
This slice judges *convergence* — has the reconciler landed what Git says. They are
different questions with different failure modes, so they get different flags and
different sections.

## Non-goals

- **No comparison against Git.** kubeagent never clones a repo, never talks to a Git
  host, never renders a manifest. Every signal is read from the reconciler's own
  `status`. "Drift" here means *the reconciler says it has not converged*, not *we
  diffed the cluster against HEAD*.
- **No remediation.** No sync trigger, no reconcile annotation, no unsuspend. `--fix`
  is not extended.
- **No watch-daemon integration.** Like `--operators`, this is a `scan`-only composed
  view. `internal/scan` and `internal/watch` are untouched and the daemon gains no
  RBAC.
- **No new detectors.** Drift produces no `Finding`.

## Architecture

A new pure package, `internal/gitops`, with no Kubernetes client — the same shape as
`internal/operators`. It reuses the shipped collection path unchanged.

```text
main.go  --drift
   │
   ├─ cluster.NewDynamicClients(kubeconfig, context)          (already exists)
   ├─ collect.OperatorResources(ctx, disco, dyn, adapters, ns) (already exists,
   │       generic over []operators.Adapter → []operators.Fetched)
   ├─ gitops.Assess(fetched, now, threshold) → gitops.Report   (new, pure)
   └─ in.GitOps = &rep                                         (CLI-composed view)
```

`internal/gitops` imports `internal/operators` for the `Adapter` and `Fetched` types
only. Both are plain data; the dependency is one-directional and `operators` gains no
import of `gitops`.

### Shared fetch when both flags are set

`gitops.Assess` selects the objects it cares about by `Group` + `Resource`, so it does
not care whether it was handed three kinds or ten. `main.go` therefore lists once:

| flags | adapters passed to `collect.OperatorResources` | assessors run |
|---|---|---|
| `--operators` only | `operators.Adapters()` (10 rows) | `operators.Assess` |
| `--drift` only | `gitops.Adapters()` (3 rows) | `gitops.Assess` |
| both | `operators.Adapters()` (superset — it already contains all three GitOps rows) | both, over the same `[]Fetched` |

One dynamic client, one discovery pass, no kind listed twice.

`gitops.Adapters()` returns the three rows carrying `Operator`/`Group`/`Version`/
`Resource`/`Kind` only — no `Rule`, no `SuspendPath`. Health rules belong to slice 1;
this package reads its own field paths.

| Reconciler | Group | Version | Resource | Kind |
|---|---|---|---|---|
| Argo CD | `argoproj.io` | `v1alpha1` | `applications` | `Application` |
| Flux | `kustomize.toolkit.fluxcd.io` | `v1` | `kustomizations` | `Kustomization` |
| Flux | `helm.toolkit.fluxcd.io` | `v2` | `helmreleases` | `HelmRelease` |

Discovery remains the installation signal: a cluster with neither reconciler installed
costs one discovery round trip, produces no error, and renders no section.

## The two reconcilers publish different signals

This is the crux of the design. Argo CD publishes drift directly; Flux never says
"OutOfSync" because it reapplies continuously. Forcing one vocabulary onto both would
mean inventing a field, which is what slice 1 refused to do for `ServiceMonitor`.

| | Argo CD `Application` | Flux `Kustomization` | Flux `HelmRelease` |
|---|---|---|---|
| differs? | `status.sync.status == "OutOfSync"` | `status.lastAttemptedRevision != status.lastAppliedRevision` | *no equivalent field in v2* |
| age anchor | `status.operationState.finishedAt` | `Ready` condition `lastTransitionTime` | `Ready` condition `lastTransitionTime` |
| will it self-heal? | `spec.syncPolicy.automated` present | not `spec.suspend`, not `Stalled=True` | not `spec.suspend`, not `Stalled=True` |
| failing? | `status.operationState.phase` in {`Failed`, `Error`} | `Ready=False` (+ `reason`) | `Ready=False` (+ `reason`) |

Two honesty constraints fall out of that table:

- **Argo does not publish how long an Application has been OutOfSync.** No such
  timestamp exists. `status.operationState.finishedAt` is when the last sync operation
  finished, so the line reads *"last synced 6d ago"* — true — and never *"drifted for
  6d"* — invented.
- **HelmRelease v2 has no `lastAppliedRevision`** (it existed in v2beta1 and was
  removed). HelmRelease therefore gets condition-based assessment only. The asymmetry
  is documented in the package doc rather than papered over.

## Drift states

```go
type State string

const (
    StateSynced  State = "synced"  // reconciler reports converged
    StatePending State = "pending" // differs, younger than the threshold, can self-heal
    StateStale   State = "stale"   // differs for longer than the threshold
    StateBlocked State = "blocked" // cannot self-heal at any age
    StateUnknown State = "unknown" // no usable signal
)
```

`pending` is the state that makes the feature safe to turn on: a deploy that landed
four minutes ago is *converging*, not a problem, and must not read like one.

**A missing timestamp is never "older than the threshold."** An object that differs
but carries no usable age renders `age unknown` and is classified `pending` (if it can
self-heal) or `blocked` (if it cannot) — never `stale`. This is the same discipline as
`operators.StateUnknown`: a heuristic that misses degrades to "I cannot tell", never
to "your deployment is broken".

### Argo CD `Application`

Evaluated in order:

1. `status.sync.status == "Synced"` → **synced**.
2. `status.sync.status == "OutOfSync"`:
   1. `status.operationState.phase` in {`Failed`, `Error`} → **blocked**, detail
      `last sync failed`.
   2. `spec.syncPolicy.automated` absent → **blocked**, detail `auto-sync off`.
   3. age unknown → **pending**, detail `age unknown`.
   4. age > threshold → **stale**. Otherwise → **pending**.
3. Anything else (`Unknown`, empty, field absent) → **unknown**.

`phase` is checked only under `OutOfSync`, so an Application that recovered from a
failed operation and is now Synced is not flagged for its history.

`spec.syncPolicy.automated` is read as a **presence bool**. Reading a bool out of
`spec` is established precedent — slice 1's `SuspendPath: {"spec","suspend"}`. Reading
arbitrary `spec` **strings** is not, and this design reads none.

### Flux `Kustomization`

Evaluated in order:

1. `spec.suspend == true` → **blocked**, detail `suspended`.
2. `Stalled` condition `True` → **blocked**, detail `stalled: <reason>`.
3. `Ready` condition `False` → age from that condition's `lastTransitionTime`;
   > threshold → **stale**, else **pending**; detail `not ready: <reason>`.
4. `status.lastAttemptedRevision` differs from `status.lastAppliedRevision` — either
   both are present and unequal, or attempted is present while applied is absent
   (nothing has ever landed) → age from the `Ready` condition's `lastTransitionTime`;
   > threshold → **stale**, else **pending**; detail `attempted <sha>, applied <sha>`
   (`applied none` when absent).
5. `Ready` condition `True` → **synced**.
6. No `Ready` condition → **unknown**.

### Flux `HelmRelease`

Steps 1, 2, 3, 5, 6 above. Step 4 is skipped — the field does not exist in v2.

### What is never read

- Any condition `message`. A `reason` is a CamelCase token by API convention; a
  `message` is arbitrary operator prose that routinely embeds URLs, and a URL can
  carry a token.
- `spec.source.repoURL`, `spec.sourceRef`, `spec.path`, `spec.chart`,
  `spec.destination`, or any other `spec` string.
- `status.operationState.message`, `status.operationState.syncResult`, or Argo's
  `status.resources[]` (per-resource diffs are a repo-shaped payload, not metadata).

## Revision strings are the leak surface

An Argo `Application` can be pointed at `https://<token>@github.example/org/repo`, and
Flux publishes revisions as `<ref>@sha1:<hash>`, where the `<ref>` half is arbitrary
user text (a branch name, a tag). Both would reach the report through a naive
revision render.

**Rule.** Given a raw revision string:

1. If it contains `@`, keep only the substring after the last `@`.
2. If it contains `:`, keep only the substring after the last `:`.
3. Accept the result only if it matches `^[0-9a-f]{7,40}$`.
4. Render the first 7 characters. Anything else renders `(revision withheld)`.

So `main@sha1:a1b2c3d4e5f6…` → `a1b2c3d`; `v1.2.3` → `(revision withheld)`; a chart
version, a branch name, a URL, or an empty string → `(revision withheld)`. The rule is
one exported helper with its own table test, and it is the only path by which any
revision-derived text reaches output.

## Report surface

Rendered after `OPERATORS`, before `NOTES`. Advisory, so — exactly like `OPERATORS` —
it does **not** participate in the all-clear suppression condition and does not set
`hasAttention`.

```
GITOPS DRIFT  (advisory — reconciler-reported; threshold 1h; no repo URLs)
  Argo CD (argoproj.io/v1alpha1)
    Application     14 synced, 1 pending, 1 blocked
      ✗ prod/payments      OutOfSync a1b2c3d, last synced 6d ago (auto-sync off)
      · staging/web        OutOfSync 9f8e7d6, last synced 4m ago
  Flux (kustomize.toolkit.fluxcd.io/v1, helm.toolkit.fluxcd.io/v2)
    Kustomization   9 synced, 1 stale, 1 blocked
      ✗ apps/web           suspended
      ✗ flux-system/infra  attempted a1b2c3d, applied 9f8e7d6, not ready 3d: BuildFailed
    HelmRelease     4 synced
```

- **Markers:** `✗` for `stale` and `blocked` — the cluster will not converge on Git
  without a human. `·` for `pending` and `unknown` — converging, or not determinable.
  `synced` objects are counted, never enumerated. A suspended object is `blocked` and
  therefore carries `✗`: the suspension may well be deliberate, but the section's job
  is to say that reconciliation has stopped, not to guess why.
- **Counts** are per kind, in a fixed state order (synced, pending, stale, blocked,
  unknown), skipping zeros. The renderer walks a fixed `[]gitops.State` slice — it
  never ranges the counts map, which would be nondeterministic.
- **Enumeration** covers every non-synced object, ordered **blocked, stale, pending,
  unknown**, then namespace, then name — worst first, so truncation drops the least
  interesting rows. Capped at `MaxPerKind = 20`, with the remainder rendered as
  `… +N more`.
- **Forbidden** kinds render `(forbidden — see deploy/rbac-gitops.yaml)`, matching the
  slice-1 hint.
- **Errors** from the collector are rendered through `alert.RedactError`, the same
  path `operators.kindReport` uses.
- The threshold is printed in the header so the numbers are self-describing.

### JSON

`report.Input` gains `GitOps *gitops.Report` with json tag `gitops,omitempty`, so a
scan without `--drift` produces byte-identical JSON to today.

## CLI

Two new `scan` flags — the 16th and 17th — using the standard-library `flag` package
only, matching the existing style.

| flag | type | default | env |
|---|---|---|---|
| `--drift` | bool | `false` | `KUBEAGENT_DRIFT` |
| `--drift-age` | duration | `1h` | `KUBEAGENT_DRIFT_AGE` |

`--drift-age` uses `fs.Duration`, so `30m`, `2h`, `168h` all parse. A new `envDuration`
helper sits beside `envBool`/`envInt` in `main.go` and falls back to the default on an
unparseable value, exactly as `envInt` does. A negative threshold is clamped to zero
(everything that differs is immediately `stale`) — a legitimate "show me everything"
setting, not an error.

Client construction stays lazy: a scan with neither `--operators` nor `--drift`
constructs no dynamic client and issues no discovery call. When
`cluster.NewDynamicClients` fails, the existing behaviour holds — a warning to stderr,
the scan continues, the section is absent.

## RBAC

New `deploy/rbac-gitops.yaml`: a scan-only ClusterRole + binding covering the three
GitOps apiGroups with `verbs: [list]` only, so a drift-only operator does not have to
grant `list` on Longhorn volumes and CNPG clusters. Structured identically to
`deploy/rbac-operators.yaml`. Discovery itself needs no grant (`system:discovery` is
bound to `system:authenticated` on every conformant cluster).

## Testing

**Unit — `internal/gitops` (pure, no cluster).** Table tests over hand-built
`map[string]any` objects with a fixed `now`, covering every state of every kind:
Argo synced / OutOfSync+auto / OutOfSync+manual / OutOfSync+failed-phase / missing
`operationState` / garbage `sync.status`; Kustomization suspended / stalled /
not-ready-young / not-ready-old / revision-mismatch / synced / no-conditions;
HelmRelease suspended / stalled / not-ready / synced. Plus:

- a **threshold boundary** test (exactly at the threshold is `pending`, one nanosecond
  past is `stale`);
- a **completeness guard** asserting every row of `gitops.Adapters()` has a fixture,
  the slice-1 precedent that stops a new row shipping untested;
- a **redaction table test** for the revision helper;
- an **ordering + truncation** test (blocked-first ordering survives the 20 cap).

**Leak test.** One test plants a token-bearing repo URL, a branch-qualified revision,
and a prose condition `message` in every kind, renders both the text report and the
JSON, and asserts none of those strings appear in either.

**Golden.** `internal/report/testdata/golden-scan.txt` gains the `GITOPS DRIFT` block;
regenerate with `go test ./internal/report -run TestGoldenScanOutput -update`, then
refresh the demo GIF and `website/docs/quickstart.md` per CLAUDE.md.

**Chaos scenario 17** (`chaos/run.sh`, `--context kind-kubeagent-chaos` pinned on every
call, like every other scenario): install real Flux, then

- a `Kustomization` whose `GitRepository` points at a `.invalid` host with
  `chaosonlytoken` embedded in the URL — DNS fails fast and deterministically with no
  network dependency on a real repo, driving `Ready=False`;
- a second `Kustomization` with `spec.suspend: true`;
- assert the `GITOPS DRIFT` section renders both, and assert `chaosonlytoken` and the
  repo host appear **nowhere** in the scan's text or JSON output.

Real Flux rather than bare CRDs, following scenario 16's precedent of installing real
cert-manager: hand-written CRs prove the parser reads fields, not that the fields are
the ones a live controller writes.

## Docs

- `website/docs/features/gitops-drift.md` + `mkdocs.yml` nav.
- README flag table row.
- `CHANGELOG.md` under `[Unreleased]`.
- `docs/go-concepts.md` only if the implementation introduces a Go concept not already
  covered (`time.Duration` flags are the likely candidate).

## Release gate

Touching `internal/collect` triggers the **full chaos gate** per the release skill.
`deploy/helm/` templates are untouched, so the chart takes a patch bump.

## Invariants this slice must not break

1. Read-only: `list` only, no writes, no new verbs.
2. No CR `spec` content and no condition `message` reaches a report field, log line, or
   error string. Booleans read out of `spec` decide; they are never rendered.
3. No revision-derived text escapes the redaction helper.
4. `pending` never reads as a failure; `unknown` never reads as a problem.
5. `internal/scan` and `internal/watch` are unchanged; the daemon gains no RBAC.
6. A scan without `--drift` is byte-identical to v0.60.0 in both text and JSON.
