# CI/CD gate mode — design

**Theme G slice 3.** Roadmap wording: *"a CI/CD gate mode (pre-deploy sanity,
post-deploy verify, SARIF, exit codes)"*. Ships in v0.65.0.

## Goal

Give a pipeline a way to ask kubeagent a yes/no question and act on the answer.
Today `kubeagent scan` exits 0 whether the cluster is healthy or on fire, so a
CI step can only gate by grepping human-readable text. This slice adds a
`gate` subcommand with a stable exit-code contract and a SARIF renderer, so the
same deterministic diagnosis that `scan` prints can fail a build and populate
GitHub code scanning.

The gate reuses kubeagent's own detectors. There is no second, weaker rule set
to keep in sync: it judges the same `scan.Result` a bare `kubeagent scan`
produces. The opt-in sections (`--logs`, `--security`, `--certs`, `--operators`,
`--drift`, `--capacity`, and the three health probes) are **not** exposed on
`gate` in this slice — each one is extra API reads and its own gate tests, and
the default check set is what a pipeline needs first. Adding them later is
additive and breaks no contract.

## Command surface

A fourth subcommand alongside `scan`, `watch`, and `mcp`:

```
kubeagent gate [--kubeconfig path] [--context name] [-n namespace]
               [--wait-for kind/name] [--timeout dur]
               [--fail-on critical|warning|info]
               [--output text|json|sarif]
```

- **Pre-deploy sanity** is `gate` with no `--wait-for`: scan now, judge now.
- **Post-deploy verify** is `gate --wait-for deployment/api -n prod`: poll that
  workload until its rollout settles, then judge it.

`--fail-on` defaults to `critical`. `--timeout` defaults to `5m` and is only
meaningful with `--wait-for`. `--output` defaults to `text`.

`scan`'s flags and exit behavior are untouched. `scan` still exits 0 on an
unhealthy cluster, because scripts already depend on that; the gate is where the
new contract lives.

### Why a subcommand and not flags on `scan`

`scan`'s flag list is already ~30 entries. Adding an exit contract and a time
dimension to it would change what `scan` means for every existing caller. A
separate verb keeps each command's help text honest about what it does.

## Verify scope

`--wait-for kind/name` polls the named workload until its rollout completes,
fails, or `--timeout` elapses. Once it settles, **only findings whose object is
that workload or a pod it owns decide the exit code.** A pre-existing failure in
an unrelated namespace does not fail your deploy — that is the point of
post-deploy verify.

Findings outside the scope are still printed, explicitly flagged as not counted,
so the operator sees them without the pipeline acting on them.

Accepted `--wait-for` kinds: `deployment`, `statefulset`, `daemonset`. These are
the three workload kinds with a rollout that can be observed to completion.

## Severity

`--fail-on` and SARIF levels both need a severity per finding, and today
severity is assigned ad hoc in `internal/mcp/view.go`, `internal/watch/issues.go`,
`internal/report/report.go`, `internal/gitops`, and `internal/quotahealth` — with
only `critical` and `warning`, and a `severityRank` helper that notes the
alphabetical ordering is a coincidence.

This slice introduces **one** package that owns severity, and gives it exactly
one consumer.

**`internal/findings`** owns:

- `Level` — an ordered type, `Info < Warning < Critical`.
- The finding-kind → level table, in one reviewable place.
- `Parse(string) (Level, error)` for `--fail-on`.
- `Flatten(scan.Result) []Finding`, where
  `Finding{Kind, Namespace, Name, Issue, Reason, Level}`.

It is named for what it owns. A bare severity table with no flattener would push
the ad-hoc mapping into `gate` anyway, which is the duplication this is meant to
end.

**`internal/gate` is its only consumer in this slice.** `mcp/view.go`,
`watch/issues.go`, and `report.go` keep their current behavior and are not
touched. Migrating them changes the MCP tool payloads — a contract shipped in
v0.63.0 — and would regenerate the golden report fixture, so it is a separate,
deliberate slice, not a passenger on a gate change.

This means `internal/mcp/view.go` and `internal/findings` both flatten a
`scan.Result` for the duration of this slice. That duplication is deliberate and
recorded here so a reviewer reads it as a decision, not an oversight.

## Exit codes

The dangerous failure mode is a gate that goes green because kubeagent could not
see the cluster, rather than because the cluster is healthy. "Could not tell"
therefore gets its own code, always — not behind a `--strict` flag, because the
safe behavior must not be opt-in.

| Code | Meaning |
|------|---------|
| `0` | Pass — nothing at or above `--fail-on` |
| `1` | Fail — findings at or above `--fail-on` |
| `2` | Inconclusive — kubeagent could not see enough to judge (partial read in scope, RBAC denial, API unreachable) |
| `3` | Timeout — `--wait-for` did not settle within `--timeout` |
| `4` | Usage — bad flags or arguments |

A pipeline that genuinely wants to tolerate inconclusive writes
`kubeagent gate || [ $? -eq 2 ]`, which is explicit at the call site.

Inconclusive is evaluated **within the gate's scope**. `scan.Result.PartialReads`
already carries what is needed. Concretely, a `ReadFailure` is in scope when
either of these holds:

- it is **cluster-scoped or names no namespace** — it could hide anything, so it
  always counts; or
- it names the namespace the gate is judging (the `-n` namespace, or the
  `--wait-for` workload's namespace).

Everything else is reported but does not change the exit code. When the gate is
judging all namespaces, every partial read is in scope by definition.

## Text and JSON output

`--output text` is the default and is what a human reads in a CI log:

```text
waiting for deployment/api in prod … 3/3 updated, 3 available

GATE: fail — 1 finding at or above critical (scope: deployment/api in prod)

  critical  Pod prod/api-5f9c7d8b4-nk2wv  CrashLoopBackOff
            Container repeatedly crashes after starting (container "api", restartCount=4)

not counted (outside scope): 2 findings elsewhere in the cluster
```

`--output json` emits the `Verdict` directly — the gate's own decision, not the
full scan report:

```json
{
  "verdict": "fail",
  "exitCode": 1,
  "failOn": "critical",
  "scope": "deployment/api in prod",
  "failing": [
    {
      "level": "critical",
      "kind": "Pod",
      "namespace": "prod",
      "name": "api-5f9c7d8b4-nk2wv",
      "issue": "CrashLoopBackOff",
      "reason": "Container repeatedly crashes after starting (container \"api\", restartCount=4)"
    }
  ],
  "reported": [],
  "inconclusive": []
}
```

`verdict` is one of `pass`, `fail`, `inconclusive`, `timeout`, and always agrees
with `exitCode`. Both fields are present because a shell reads the exit code and
a `jq` filter reads the string; neither should have to derive the other.

## SARIF

SARIF results carry a `physicalLocation` keyed to an artifact URI and line.
kubeagent findings are cluster objects, not source lines, and there is no file to
point at. Rather than invent one, the renderer emits a synthetic cluster URI and
no region:

```json
{
  "$schema": "https://json.schemastore.org/sarif-2.1.0.json",
  "version": "2.1.0",
  "runs": [{
    "tool": {"driver": {
      "name": "kubeagent",
      "version": "v0.65.0",
      "informationUri": "https://github.com/imantaba/kubeagent",
      "rules": [{
        "id": "CrashLoopBackOff",
        "name": "CrashLoopBackOff",
        "shortDescription": {"text": "Container repeatedly crashes after starting"},
        "defaultConfiguration": {"level": "error"}
      }]
    }},
    "results": [{
      "ruleId": "CrashLoopBackOff",
      "level": "error",
      "message": {"text": "Container repeatedly crashes after starting (container \"worker\", restartCount=5)"},
      "locations": [{
        "physicalLocation": {
          "artifactLocation": {"uri": "k8s://payments/Pod/worker-7d9c6f6b8-x2z4q"}
        }
      }]
    }],
    "invocations": [{
      "executionSuccessful": false,
      "toolConfigurationNotifications": [{
        "descriptor": {"id": "partial-read"},
        "level": "error",
        "message": {"text": "could not list pods in namespace payments"}
      }]
    }]
  }]
}
```

Decisions worth stating:

- **Level mapping:** `Critical → error`, `Warning → warning`, `Info → note`.
- **Only rules that actually fired** appear in `driver.rules`. A static catalogue
  of every detector would describe checks this run may never have executed.
- **Partial reads become `toolConfigurationNotifications` with
  `executionSuccessful: false`.** This is the honest SARIF encoding of "could not
  tell": an upload cannot look clean when the gate never saw the cluster.
- **Deterministic ordering.** Results sort by (level, namespace, kind, name,
  issue), so an unchanged cluster renders byte-identical SARIF and two runs diff
  cleanly. Same discipline as `internal/mcp`'s `sortFindings`.
- **Redaction holds.** No kubeconfig paths anywhere in the document; any API
  server URL in a message is reduced to `scheme://host`. `informationUri` is the
  public project URL, which is not a credential.

## Architecture

Four units, each independently testable:

| Package | Responsibility | I/O |
|---------|----------------|-----|
| `internal/findings` | `Level`, the severity table, `Parse`, `Flatten(scan.Result)` | none — pure |
| `internal/gate` | `Decide(res scan.Result, opts Options) Verdict` | none — pure |
| `internal/sarif` | render a `Verdict` to SARIF 2.1.0 | none — pure |
| `internal/rolloutwait` | poll one workload until its rollout settles or times out | read-only `get` |

```
cluster (connect) → collect → scan.Result → findings.Flatten → gate.Decide → text | json | sarif
                                   ↑
                        rolloutwait (only with --wait-for)
```

```go
type Verdict struct {
    Verdict      string              `json:"verdict"`      // pass|fail|inconclusive|timeout
    Code         int                 `json:"exitCode"`
    FailOn       findings.Level      `json:"failOn"`
    Scope        string              `json:"scope"`
    Failing      []findings.Finding  `json:"failing"`
    Reported     []findings.Finding  `json:"reported"`
    Inconclusive []scan.ReadFailure  `json:"inconclusive"`
}
```

`Failing` decides the exit code; `Reported` is everything else, printed but not
counted. `Verdict` and `Code` are derived together in `Decide` and can never
disagree. This struct is the `--output json` document verbatim — the JSON shape
above is its serialization, not a separate view type.

`internal/sarif` is separate from `internal/gate` because SARIF is a
serialization concern with its own schema and its own golden fixture. Keeping it
out means `gate` can be reviewed as pure decision logic.

`internal/rolloutwait` is the only new package that touches the cluster. It is
**read-only** — `get` on one workload — and **sequential**: a `time.Ticker` in a
plain loop, no goroutines, honoring the v1 constraint that the scan-side CLI
stays single-threaded. It is the first scan-side operation with a time
dimension, so it takes an injected clock to keep its timeout path testable.

`main.go` gains the `gate` subcommand and one invasive change: `run()` returns an
exit code instead of `main()` hardcoding `os.Exit(1)`. `scan`, `watch`, `mcp`,
and `version` keep exiting 0/1 exactly as they do today.

## Error handling

| Situation | Exit | Output |
|-----------|------|--------|
| Nothing at or above `--fail-on` | 0 | verdict; SARIF if requested |
| Findings at or above `--fail-on` | 1 | verdict; SARIF still written, so CI can upload it and then fail |
| Partial read in scope, unreachable API, RBAC denial | 2 | verdict; SARIF with `executionSuccessful: false` |
| `--wait-for` did not settle | 3 | verdict; SARIF with `executionSuccessful: false` |
| Bad flags or arguments | 4 | usage on stderr; **no SARIF** |

The last row matters: writing a valid, empty SARIF document on a usage error
would upload as a clean scan. A typo in a flag name must not read as "no
problems found".

Writing SARIF on exits 1, 2, and 3 is what makes the documented pipeline work:

```yaml
- run: kubeagent gate --output sarif > kubeagent.sarif
  continue-on-error: true
- uses: github/codeql-action/upload-sarif@v3
  with: {sarif_file: kubeagent.sarif}
```

## Testing

- **`internal/findings`** — table tests over the kind → level mapping, and
  `Flatten` against fabricated `scan.Result` values. Pure, no cluster.
- **`internal/gate`** — `Decide` tests covering each exit code, the `--fail-on`
  thresholds, scoped vs unscoped judgement, and the distinction between a partial
  read inside the scope (exit 2) and outside it (not exit 2). Pure, no cluster.
- **`internal/sarif`** — a golden fixture under `testdata/`, plus a determinism
  test that renders the same verdict twice and compares bytes, plus assertions
  that the document validates against the SARIF 2.1.0 required fields.
- **`internal/rolloutwait`** — client-go's fake clientset with a reactor that
  advances the workload's status across successive polls, an injected clock for
  the timeout path, and a test that it issues only `get`.
- **`main_test.go`** — flag parsing, `--fail-on` rejection of an unknown level
  (exit 4), and the exit-code table.

`internal/report/testdata/golden-scan.txt` must stay byte-identical: this slice
adds a command, it does not change the scan report.

## Release gate

This slice touches no `internal/collect`, `internal/cluster`, RBAC manifest,
`--fix`, watch-daemon, or Helm-template code, so the full chaos suite is not the
right gate. `rolloutwait` does add a new cluster read, but `get` on
deployments/statefulsets/daemonsets is already covered by the existing read-only
ClusterRole, so no RBAC change ships here.

The gate is a real Kind cluster carrying one healthy workload and one
deliberately broken one, asserting:

1. `gate --wait-for deployment/<healthy>` exits **0**.
2. `gate --wait-for deployment/<broken>` exits **1**.
3. `gate` under a stripped-down role that cannot list pods exits **2** — the
   green-when-blind case this design exists to prevent.
4. `gate --output sarif` emits a document that parses and carries the expected
   rule and level.

## Out of scope

Recorded deliberately, not built:

- `--sarif-anchor PATH` and any mapping of findings back to repo YAML. Getting a
  true `file:line` means parsing the working tree and handling Helm, kustomize,
  and operator-created objects — a large scope that belongs in its own slice, if
  ever.
- Baseline/diff against a previous run ("fail only on new findings"). Useful, and
  a natural follow-up, but it needs a stored baseline artifact and a change
  contract of its own.
- JUnit XML output.
- A packaged GitHub Action wrapper. A `uses:` one-liner is a second repo surface
  with its own release cadence and versioning.
- Migrating `mcp`, `watch`, and `report` onto `internal/findings`.
