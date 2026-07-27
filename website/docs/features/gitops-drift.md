# GitOps drift (`--drift`)

`kubeagent scan --drift` answers one question for a cluster reconciled by Argo
CD or Flux: **is this cluster still converging on Git, and if not, for how
long?**

```bash
kubeagent scan --drift
```

```text
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

!!! note
    The example above is synthetic. Your cluster's output will reflect only
    the reconciler(s) it actually runs.

The flag is off by default; set `KUBEAGENT_DRIFT=true` to enable it without
passing `--drift` on every invocation.

## What it answers, and what it deliberately does not

kubeagent never clones a repository, never talks to a Git host, and never
renders a manifest. Every signal in this section is read from the
reconciler's own `status`: "drift" here means *the reconciler itself reports
it has not converged*, not *kubeagent diffed the cluster against HEAD*. There
is no comparison against Git anywhere in this feature — if Argo CD or Flux
says `Synced`/`Ready`, kubeagent takes that at face value.

This is also why `--drift` is a separate flag and section from
[`--operators`](operators.md), not an extra column on it. `--operators`
judges whether the *workload* is healthy; `--drift` judges whether the
*reconciler has landed what Git says*. An Application can be perfectly
`OutOfSync` and perfectly healthy (a deploy that landed four minutes ago), so
conflating the two would make every pending rollout look like a failure.

## The five states

| State | Meaning |
| --- | --- |
| `synced` | The reconciler reports it has converged. |
| `pending` | It differs, but is younger than `--drift-age` and can still self-heal. **This is not a problem** — it is what a deploy in flight looks like. |
| `stale` | It has differed for longer than `--drift-age`. |
| `blocked` | It cannot self-heal at any age — suspended, stalled, auto-sync off, or the last sync failed. |
| `unknown` | No usable signal (a missing field, an unrecognized value). |

`pending` is the state that makes this feature safe to leave on. A missing
age is never treated as "older than the threshold": an object that differs
but carries no usable timestamp renders `age unknown` and is classified
`pending` (if it can still self-heal) or `blocked` (if it cannot) — never
`stale`. The same discipline applies to `unknown`: a heuristic that misses a
field degrades to "I cannot tell", never to "your deployment is broken".

Only `stale` and `blocked` carry the `✗` marker — the cluster will not
converge on Git without a human. `pending` and `unknown` carry `·` —
converging, or not determinable. `synced` objects are counted, never
enumerated.

## Argo CD does not publish how long an Application has been out of sync

There is no such field. The only honest anchor is
`status.operationState.finishedAt` — when the *last sync operation* finished
— so the report says **"last synced 6d ago"**, which is true, and never
**"drifted for 6d"**, which would be invented. The same Application's history
also decides whether it is `blocked`: if the last sync operation failed, or
`spec.syncPolicy.automated` is absent (auto-sync is off), it cannot self-heal
and is `blocked` regardless of age.

## Flux never reports "OutOfSync"

Flux reapplies continuously, so there is no equivalent boolean. Its drift
signal is indirect: suspended, stalled, `Ready=False`, or an
attempted-vs-applied revision mismatch. `HelmRelease` under
`helm.toolkit.fluxcd.io/v2` has no `status.lastAppliedRevision` at all (the
field existed in `v2beta1` and was removed), so a `HelmRelease` gets no
revision comparison — only condition-based assessment.

## Per-reconciler signals

| | Argo CD `Application` | Flux `Kustomization` | Flux `HelmRelease` |
| --- | --- | --- | --- |
| differs? | `status.sync.status == "OutOfSync"` | `status.lastAttemptedRevision != status.lastAppliedRevision` | *no equivalent field in v2* |
| age anchor | `status.operationState.finishedAt` | `Ready` condition `lastTransitionTime` | `Ready` condition `lastTransitionTime` |
| will it self-heal? | `spec.syncPolicy.automated` present | not `spec.suspend`, not `Stalled=True` | not `spec.suspend`, not `Stalled=True` |
| failing? | `status.operationState.phase` in `Failed`/`Error` | `Ready=False` (+ `reason`) | `Ready=False` (+ `reason`) |

Discovery is the installation signal, exactly like `--operators`: a cluster
running neither reconciler costs one discovery round trip, produces no error,
and renders no section. Counts are per resource **kind**: Flux's
`Kustomization` and `HelmRelease` roll up separately.

## `--drift-age`

```bash
kubeagent scan --drift --drift-age 30m
```

`--drift-age` (default `1h`, env `KUBEAGENT_DRIFT_AGE`) is the boundary
between `pending` and `stale`: an object that differs for longer than this is
`stale`; younger, it is `pending`. It accepts any Go duration (`30m`, `2h`,
`168h`). A negative value is clamped to zero — "show me everything that
differs as stale" — which is a legitimate setting, not an error.

## What is never read, and what is never printed

No CR `spec` content and no condition `message` ever reaches the report, the
JSON output, or a log line — only metadata and state:

- Never read: `spec.source.repoURL`, `spec.sourceRef`, `spec.path`,
  `spec.chart`, `spec.destination`, any other `spec` string,
  `status.operationState.message`, `status.operationState.syncResult`, or
  Argo's per-resource `status.resources[]` diff list. A condition's `reason`
  — a CamelCase token by API convention — is read; its free-text `message`
  is not, because operator messages routinely embed URLs.
- Never printed: a repository URL of any kind. An Argo CD `Application` can
  point at `https://<token>@git.example/org/repo`, and a URL like that can
  carry a credential.
- Revisions are reduced before they can reach output: Flux publishes
  revisions as `<ref>@sha1:<hash>`, where `<ref>` is arbitrary user text (a
  branch name, a tag). A raw revision is accepted only if, after stripping
  everything before the last `@` and the last `:`, what remains matches a
  bare lowercase hex commit SHA (7-40 characters) — and even then only its
  first **7 characters** are shown. Anything else — a tag, a chart version, a
  branch name, a URL, an empty string — renders as `(revision withheld)`.

Booleans read out of `spec` (`spec.suspend`, whether
`spec.syncPolicy.automated` is present) decide the state; they are never
rendered themselves.

Large estates are bounded too: counts are always exact, but at most 20
non-synced objects are listed per kind, with the remainder reported as
`… +N more` rather than silently dropped.

## What it deliberately does not do

- **It never affects the cluster verdict.** Like `--operators`, this section
  is advisory. It never produces a `Finding`, never changes Healthy/Degraded,
  and never changes the exit code.
- **It never remediates.** No sync trigger, no reconcile annotation, no
  unsuspend. `--fix` is not extended by this flag.
- **It is not wired into the `watch` daemon.** Like `--operators`, this is a
  `scan`-only composed view.

## RBAC

Most human kubeconfigs already allow these `list` calls. On a restricted
context, or for the in-cluster ServiceAccount, apply the scan-only add-on:

```bash
kubectl apply -f deploy/rbac-gitops.yaml
```

It grants `list` — and nothing else — on the three GitOps custom resources:
Argo CD's `applications` (`argoproj.io`) and Flux's `kustomizations`
(`kustomize.toolkit.fluxcd.io`) and `helmreleases`
(`helm.toolkit.fluxcd.io`). Its rules are a subset of
[`deploy/rbac-operators.yaml`](operators.md#rbac), so applying that file
alone is enough to run both `--operators` and `--drift`; `rbac-gitops.yaml`
exists so a drift-only user needs no grant on Longhorn volumes or CNPG
clusters. Without it, `--drift` still names which reconciler is installed —
API discovery is open to every authenticated user — and marks each kind as
`forbidden` rather than erroring.

`--operators` and `--drift` share one dynamic-client fetch when both are set,
so turning on both costs no extra API discovery or listing round trip beyond
what `--operators` alone already makes.
