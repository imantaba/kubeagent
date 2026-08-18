# CI/CD gate

A pipeline needs a yes/no answer it can branch on, not a wall of text to grep.
`kubeagent scan` exits `0` whether the cluster is healthy or on fire — it is a
report, not a check — so a CI step could previously only gate a deploy by
parsing the text output for magic strings. `kubeagent gate` is a separate
subcommand built for that job: it runs the same read-only diagnosis and turns
it into a small, stable exit-code contract a build step can branch on
directly.

## Two modes

- **Pre-deploy sanity** — `kubeagent gate` with no `--wait-for` judges the
  whole cluster (or `-n namespace`) as it stands right now. Run it before a
  deploy to refuse to ship onto an already-broken cluster.
- **Post-deploy verify** — `kubeagent gate --wait-for deployment/api -n prod`
  waits for that one workload's rollout to settle, then judges only the
  findings attributable to it. Run it after a deploy to confirm the thing you
  just shipped actually came up healthy, without an unrelated failure
  elsewhere in the namespace failing your build.

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `--kubeconfig` | `$KUBECONFIG` or `~/.kube/config` | path to kubeconfig |
| `--context` | current-context | kubeconfig context to use |
| `--output` | `text` | output format: `text` \| `json` \| `sarif` |
| `--fail-on` | `critical` | fail the gate at this severity or above: `critical` \| `warning` \| `info` |
| `--wait-for` | (empty — pre-deploy mode) | post-deploy verify: wait for this workload's rollout to settle, then judge only it (`kind/name`, e.g. `deployment/api`) |
| `--timeout` | `5m0s` | with `--wait-for`: give up waiting after this long (exit `3`) |
| `--poll-interval` | `2s` | with `--wait-for`: how often to re-read the workload |
| `--allow-partial-read` | (none) | accept that this resource cannot be read, instead of exiting `2` (repeatable, e.g. `leases`) |
| `--namespace` / `-n` | all namespaces | namespace to judge |

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | Pass — nothing at or above `--fail-on` |
| `1` | Fail — findings at or above `--fail-on` |
| `2` | Inconclusive — kubeagent could not see enough to judge |
| `3` | Timeout — `--wait-for` did not settle within `--timeout` |
| `4` | Usage — bad flags or arguments |

The boundary between `4` and `2` is deliberate, not incidental:

- **`4`** covers input kubeagent never got to use against the cluster: a bad
  flag, an unparsable `--wait-for`, or a kubeconfig/context that fails to even
  build a client. `cluster.NewClient` builds a `rest.Config` and a clientset
  without touching the network, so its failure is an unusable-input problem,
  the same class as a typo in a flag name — not a claim about the cluster's
  health.
- **`2`** covers everything that got as far as talking to the cluster but
  could not finish: an unreachable API server, an RBAC denial, a failed
  rollout poll, a failed scan, or a failed render (including a closed stdout
  pipe). An unreachable cluster or an RBAC-denied `list` surfaces here — as a
  read failure during the scan — not as a startup failure, so it is never a
  `4`.

## Why exit 2 exists, and is not opt-in

A gate that goes green because kubeagent could not see the cluster is worse
than one that fails: it tells a pipeline "safe to ship" on the strength of no
evidence at all. So a partial read is never silently downgraded to a pass —
it costs a distinct exit code, and a pipeline that wants to soldier on
through it has to say so explicitly, either broadly:

```bash
kubeagent gate || [ $? -eq 2 ]
```

or narrowly, per resource, when the operator has already decided one
specific read failure is acceptable to ignore:

```bash
kubeagent gate --allow-partial-read leases
```

A waived resource still appears in the output (`RenderText` prints it,
`--output json` lists it under `inconclusive`, marked waived) — an operator
should still see what they chose not to be told about, even though it no
longer forces exit `2`.

One carve-out: a `--policy` rule kubeagent could not evaluate is exit `1`, not
`2`, even when the read failure behind it also shows up as a blind spot (see
[Policy as code](policy.md#a-rule-that-could-not-be-evaluated-is-not-a-pass)).
The `kubeagent gate || [ $? -eq 2 ]` pattern above is for soldiering on
through a partial read — it is not meant to also soldier on through a rule
that never ran, so an unevaluated rule at or above `--fail-on` keeps the exit
code at `1` regardless of `--allow-partial-read`.

## A preflight check for missing grants

`kubeagent gate` fails closed on a resource it cannot read — that is exit
`2`, deliberately, per the section above — but a red exit `2` on its own
doesn't say *which* grant is missing. Running `kubeagent rbac check
--profile scan` as an earlier step in the same pipeline turns "the gate is
red because a grant is missing" into a message that names the grant before
`gate` ever runs. Like `gate`, `rbac check` exits `1` when anything it
checked is blocked, so it composes into the same "fail the step and stop the
pipeline" pattern — see [Least-privilege RBAC](rbac.md#kubeagent-rbac-check).

## `--wait-for` scope

With `--wait-for`, only findings attributable to the named workload decide
the exit code: the workload itself, and anything owned by it (its pods).
Everything else the scan turned up is still printed, under a
`not counted (below --fail-on)` line, so an unrelated problem elsewhere in the
namespace is visible without failing the build for it.

Accepted kinds, case-insensitive, each also taking its `kubectl` short form
and plural:

| Kind | Aliases |
|------|---------|
| Deployment | `deployment`, `deployments`, `deploy` |
| StatefulSet | `statefulset`, `statefulsets`, `sts` |
| DaemonSet | `daemonset`, `daemonsets`, `ds` |

**Limitation — staged StatefulSet rollouts.** kubeagent treats a StatefulSet
as settled only once its rollout has fully converged
(`currentRevision == updateRevision`). `kubectl rollout status`, by contrast,
calls a `partition`-based staged rollout complete as soon as the updated
count clears the partition boundary — it does not wait for every replica to
move onto the new revision. Gating a deliberately staged canary on
`--wait-for statefulset/…` will therefore never see kubeagent's stricter
condition, run out `--timeout`, and exit `3`. This is a documented
limitation of this slice, not a bug: gate the final, un-partitioned step of
the rollout instead of the intermediate staged one.

## Output formats

### `--output text` — a failing gate, no `--wait-for`

```bash
kubeagent gate --context kind-kubeagent-gate -n demo
```

```text
GATE: fail — 1 finding at or above critical (scope: cluster)

  critical  Pod demo/broken-68bbbbcf5b-vn8vr  ImagePullBackOff
            Bad image reference or registry authentication (container "broken": Back-off pulling image "registry.k8s.io/does-not-exist:v1": ErrImagePull: rpc error: code = NotFound desc = failed to pull and unpack image "registry.k8s.io/does-not-exist:v1": failed to resolve reference "registry.k8s.io/does-not-exist:v1": registry.k8s.io/does-not-exist:v1: not found)
```

Exit code: `1`.

### `--output text` — a `--wait-for`-scoped pass, same cluster

The same cluster also has a healthy Deployment, `api`. Scoping the gate to
just that workload passes, even though the cluster as a whole has the
`broken` Deployment failing above:

```bash
kubeagent gate --context kind-kubeagent-gate --wait-for deployment/api -n demo
```

```text
Deployment/api in demo: 2/2 updated, 2 available

GATE: pass — nothing at or above critical (scope: Deployment/api in demo)

not counted (below --fail-on): 1 finding
```

Exit code: `0`. The `broken` Deployment's finding is still real, still
running, and still reported — it is just outside `demo/api`'s scope, so it
did not decide this exit code.

### `--output json`

The same failing, unscoped run as above, as JSON:

```bash
kubeagent gate --context kind-kubeagent-gate -n demo --output json
```

```json
{
  "schemaVersion": "1.0",
  "verdict": "fail",
  "exitCode": 1,
  "failOn": "critical",
  "scope": "cluster",
  "failing": [
    {
      "level": "critical",
      "kind": "Pod",
      "namespace": "demo",
      "name": "broken-68bbbbcf5b-vn8vr",
      "issue": "ImagePullBackOff",
      "reason": "Bad image reference or registry authentication (container \"broken\": Back-off pulling image \"registry.k8s.io/does-not-exist:v1\": ErrImagePull: rpc error: code = NotFound desc = failed to pull and unpack image \"registry.k8s.io/does-not-exist:v1\": failed to resolve reference \"registry.k8s.io/does-not-exist:v1\": registry.k8s.io/does-not-exist:v1: not found)",
      "owner": "Deployment/broken"
    }
  ],
  "reported": [],
  "inconclusive": []
}
```

`verdict` and `exitCode` are derived together and never disagree: a shell
script reads `exitCode` (or the process exit status directly), a `jq` filter
reads `verdict`, and neither has to derive the other. `failing` only ever
holds findings that decided this exit code; `reported` holds everything else
kubeagent saw (out of scope, or below `--fail-on`); `inconclusive` lists any
blind spot, waived or not.

The shape of this document is versioned; see
[JSON schema contract](json-schema.md).

## SARIF

`kubeagent gate --output sarif` renders the same verdict as a
[SARIF 2.1.0](https://json.schemastore.org/sarif-2.1.0.json) document, so a
CI pipeline can upload kubeagent's findings straight to GitHub code
scanning.

SARIF results are keyed to a `physicalLocation` — normally a file and a line.
kubeagent findings are live cluster objects, not source files, so there is
nothing to point a line number at. Rather than invent one, the renderer
emits a synthetic `k8s://<namespace>/<Kind>/<name>` URI (or `k8s://<Kind>/<name>`
for a cluster-scoped object) and no region. Mapping a finding back to the
repo YAML that produced it — Helm values, kustomize overlays,
operator-created objects — is a separate, much larger problem and is
deliberately out of scope for this slice.

Severity maps onto SARIF's three levels:

| kubeagent | SARIF |
|-----------|-------|
| `critical` | `error` |
| `warning` | `warning` |
| `info` | `note` |

An unwaived partial read — a blind spot the operator has not explicitly
accepted with `--allow-partial-read` — sets the run's `executionSuccessful`
to `false`, and so does a `--wait-for` that timed out. A waived partial read
does not: the operator already said that gap was acceptable. This is on
purpose: a code-scanning upload must not look clean when the gate was blind
or the rollout never settled, even though the exit code (not the SARIF
document) is what actually fails the build.

The same failing run rendered as SARIF:

```bash
kubeagent gate --context kind-kubeagent-gate -n demo --output sarif
```

```json
{
  "$schema": "https://json.schemastore.org/sarif-2.1.0.json",
  "version": "2.1.0",
  "runs": [
    {
      "tool": {
        "driver": {
          "name": "kubeagent",
          "version": "dev",
          "informationUri": "https://github.com/imantaba/kubeagent",
          "rules": [
            {
              "id": "ImagePullBackOff",
              "name": "ImagePullBackOff",
              "shortDescription": {
                "text": "ImagePullBackOff"
              },
              "defaultConfiguration": {
                "level": "error"
              }
            }
          ]
        }
      },
      "results": [
        {
          "ruleId": "ImagePullBackOff",
          "level": "error",
          "message": {
            "text": "Bad image reference or registry authentication (container \"broken\": Back-off pulling image \"registry.k8s.io/does-not-exist:v1\": ErrImagePull: rpc error: code = NotFound desc = failed to pull and unpack image \"registry.k8s.io/does-not-exist:v1\": failed to resolve reference \"registry.k8s.io/does-not-exist:v1\": registry.k8s.io/does-not-exist:v1: not found)"
          },
          "locations": [
            {
              "physicalLocation": {
                "artifactLocation": {
                  "uri": "k8s://demo/Pod/broken-68bbbbcf5b-vn8vr"
                }
              }
            }
          ]
        }
      ],
      "invocations": [
        {
          "executionSuccessful": true,
          "toolConfigurationNotifications": []
        }
      ]
    }
  ]
}
```

`"version": "dev"` here is this build's own version string — a released
binary reports its tag instead. `executionSuccessful` is `true` in this
example because the run saw the cluster cleanly; it went blind on nothing,
so the failing finding above is a confident `error`, not a guess.

## GitHub Actions example

```yaml
- name: kubeagent gate
  run: kubeagent gate --wait-for deployment/api -n prod --output sarif > kubeagent.sarif
  continue-on-error: true
- uses: github/codeql-action/upload-sarif@v3
  with:
    sarif_file: kubeagent.sarif
- name: fail the build on a gate failure
  run: kubeagent gate --wait-for deployment/api -n prod
```

The first step's `continue-on-error: true` matters: without it, a non-zero
exit from the SARIF run would stop the job before the upload step runs, and
the whole point of uploading is to see the findings even on a failing gate.
The second `kubeagent gate` invocation (plain text output, no
`continue-on-error`) is what actually fails the job — running the gate twice
is deliberate, not wasteful: one run's job is to produce the artifact, the
other's is to set the exit code.

## Not in this slice

Deliberately absent, and not planned for this slice:

- The opt-in advisory sections — `--logs`, `--security`, `--certs`,
  `--operators`, `--drift`, `--capacity`, and the three health probes
  (`--kubelet-health`, `--control-plane-health`, `--dns-health`) — are not
  exposed on `gate`. It runs the same bare scan `scan` runs with none of
  those flags set.
- No mapping of findings back to repository YAML (Helm values, kustomize
  overlays, the manifest that actually produced the object).
- Diff mode covers restart rates only: `--baseline` compares this run's restart
  rates against a [captured baseline](baseline.md). Nothing else is compared
  against a previous run — findings, inventory and resource usage are judged
  fresh each time.
- No JUnit XML output.
- No packaged GitHub Action — the example above is a plain shell invocation
  a workflow can call directly.
