# `kubeagent_inspect` object resolution — design

**Date:** 2026-08-08
**Status:** approved, not yet implemented
**Surface:** `internal/mcp` (the `kubeagent mcp` server), plus the shipped
Claude Code plugin's skills and commands.

## The defect

`kubeagent_inspect` cannot resolve the object identity `kubeagent_triage`
hands it for a critical finding.

Confirmed against a real multi-namespace cluster, not inferred. Triage
returned nine findings: six `critical`, every one shaped
`Pod/<pod-name>` for a controller-owned pod, and three `warning`, all
webhook configurations. Inspecting the first critical finding with the
namespace and name the finding itself supplied:

```
kubeagent_inspect {kind: "pod", namespace: "<ns>", name: "<pod>"}
  → found: false, pods: [], findings: []      (one event still returned)
```

`kubectl` confirmed that pod exists and is `Running` (one restart 36 days
earlier, age 82 days), so `found: false` is not a race against churn. The
owning Deployment resolved correctly and carried the pod's finding:

```
kubeagent_inspect {kind: "deployment", namespace: "<ns>", name: "<deploy>"}
  → found: true, Deployment, 1/1 ready
    pods:     ["<pod>"]
    findings: [("critical", "Pod/<pod>", "OOMKilled")]
```

So the shipped instruction in `commands/triage.md` step 4 and
`skills/triaging-a-cluster/SKILL.md` step 3 — "call `kubeagent_inspect` on
every `critical` finding, using the `namespace` and `name` the finding
already gave you" — fails for every controller-owned pod. On the tested
cluster that was six of six.

### Mechanism

Three facts that do not compose:

1. `internal/mcp/view.go`'s `fromDiagnose` emits `Kind: "Pod"` with the
   pod's own name for **every** critical finding.
2. `internal/inventory`'s `PodOwners` assigns a pod `kind = "Pod"` **only**
   when `controllerOwner` returns nil. A ReplicaSet-owned pod becomes
   `Deployment/<name>`, so it is never a workload in its own right.
3. `internal/mcp/inspect.go` matches the requested object against
   `res.Inventory.Workloads` and nothing else.

`fromDiagnose` is not wrong: the finding *is* about a pod. The defect is
that `inspect` has no pod lookup, even though `pod` is the first value in
its published `inspectKinds` enum.

### The same defect, wider than pods

`res.Inventory.Workloads` is `inventory.Prioritize`'s output, and
`Prioritize` drops healthy-quiet workloads outright:

```go
default:
    // healthy-quiet — always hidden
```

`inspect` passes `IncludeCron: true, IncludeRestarts: true`, so flagged,
restart-carrying and cron workloads survive — but a Deployment that is
`Ready == Desired` with zero restarts does not. Inspecting a healthy
Deployment therefore also answers `found: false` today. That is the same
lie in the same shape: a lookup answered against a list built for
*display*.

`Assemble` additionally truncates a Job or CronJob's pod rows at
`jobPodCap = 3`, so a CronJob's fourth pod is absent from
`Workloads[].Pods` as well.

### Why the existing test did not catch it

`TestInspect_PodReturnsItsFindingsAndEvents` uses `crashingPod()` — a
**bare** pod with no owner references. `PodOwners` gives it
`kind: "Pod"`, `Assemble` seeds it as its own workload, and it is
`Flagged()`, so it survives `Prioritize`. The test passes for the one pod
shape that essentially never occurs in a real cluster.

## Decisions

Four forks were resolved before this spec was written.

### 1. Resolve all seven kinds, not just pods

Existence is answered against `scan.Result.Inputs` — the raw, unfiltered
snapshot the scan already collected. Its seven collections (`Pods`,
`Deployments`, `ReplicaSets`, `StatefulSets`, `DaemonSets`, `Jobs`,
`CronJobs`) are **exactly** the seven values in `inspectKinds`.

Fixing pods alone would leave "inspect a healthy Deployment" answering
`found: false`. One mechanism fixes both, and after it `found: false`
means one thing: *no object of that kind with that name exists in that
namespace in this snapshot*. Today it means "absent, or healthy, or
truncated".

### 2. A pod answer describes the pod, and names its owner

`Kind` stays `"Pod"`. `Status` is the pod's phase. `Pods` carries that one
row. `Findings` are the pod's own `diagnose.Finding`s.

A new field carries the escalation pointer:

```go
Owner string `json:"owner,omitempty"` // "Deployment/web" — the controller that owns this pod
```

Rejected alternative: report the owning workload's status under
`Kind: "Deployment"`. A caller asked about a pod; an answer describing its
Deployment silently answers a different question. Rejected alternative:
omit `owner`. The model would then have to guess the owning workload's
name to escalate, which is the failure this spec exists to remove.

`Desired` and `Ready` stay unset. Both are `omitempty`, so they disappear
from the JSON rather than rendering as `0/0`. Absence must never read as
zero.

### 3. Fix `inspect`, not `fromDiagnose`

The alternative remedy was to make triage emit the owning workload's
identity so that everything it hands out is inspectable. Rejected on three
counts: it collapses six distinct OOMKilled pods into six identical
`Deployment/<name>` rows; it discards the pod name a human needs to run
`kubectl logs`; and it is a breaking change to a shipped tool payload,
where the fix here is an added lookup. `view.go` is untouched.

### 4. The un-inspectable warning kinds stay un-inspectable

Triage's warning findings name Service, Ingress, PersistentVolumeClaim,
PodDisruptionBudget, HorizontalPodAutoscaler, ResourceQuota and webhook
configuration kinds. None is in `inspectKinds`, and none is added here:
each would need its own resolver and several would need reads the scan
does not make. Widening the enum is its own slice.

The honest fix is that the model is told, before it calls. The tool's own
description names the seven kinds, and the two shipped documents that list
what cannot be inspected gain webhook configurations — which they omit
today, and which were three of three warnings on the tested cluster.

## Implementation

### The resolver

One pure function in `internal/mcp/inspect.go`, replacing the
`res.Inventory.Workloads` loop. Pure so it is unit-testable without
standing up a server:

- **Controller kinds** (`deployment`, `statefulset`, `daemonset`,
  `replicaset`, `job`, `cronjob`) match against
  `inventory.Assemble(res.Inputs, findings)`, where `findings` are gathered
  from `res.Inventory.Workloads[].Findings`. That is the **unfiltered**
  workload set, recomputed in memory from objects already collected — no
  new cluster read, and no duplicate of the unexported `workloadStatus`
  and `terminalPodStatus` logic that produces a correct `Status`,
  `Desired` and `Ready`.

  Re-attaching findings is lossless: `Workload.Flagged()` returns true when
  `len(Findings) > 0`, so `Prioritize` can never have dropped a workload
  that had one.

  Recomputing loses no field `InspectOutput` carries. The annotations the
  scan applies to the prioritized set — `NetworkPolicies`, `Rollout`,
  `RootCause` — are not part of `InspectOutput`, so their absence from the
  recomputed set changes no output byte.

- **`kind: pod`** looks the pod up in `res.Inputs.Pods` directly. Going to
  the raw list rather than through the owner's `Pods` slice means
  `jobPodCap` truncation cannot hide a pod that exists.

  The canonical `Kind` spelling (`"Pod"`, `"Deployment"`, …) comes from the
  resolver's own switch, never from the object: typed client-go objects do
  not populate `TypeMeta`.

  The requested kind is matched case-insensitively, as it is today. The
  published enum only admits the seven lowercase spellings, so this is
  belt-and-braces rather than load-bearing — but the resolver is a pure
  function that a test can call with any string, and it must not answer
  `found: false` for `"Pod"` when the caller meant `"pod"`.

### One new export in `internal/inventory`

Building a pod's row needs `podReady`, `podRestarts`, `podImage` and
`HumanAge`, three of which are unexported. Rather than duplicate them:

```go
// PodRowFor projects one pod into the row shape a report renders.
func PodRowFor(p corev1.Pod, now time.Time) PodRow
```

`Assemble` is refactored to call it, so there is one implementation. The
row is field-for-field what `Assemble` builds today, which two things
pin: a direct test, and `internal/report/testdata/golden-scan.txt`
staying byte-identical.

`Assemble` passes `time.Now()`, exactly as it does inline today. The MCP
handler passes its own injected `now func() time.Time`, which makes the
pod-path age deterministic under test.

### Findings on the pod path

The pod's findings are its owning workload's `Findings` filtered to
`f.Pod == namespace + "/" + name` — the same key `Assemble` uses to attach
them.

No `fromWorkload` fallback on the pod path. `fromWorkload` describes a
workload's ready-versus-desired; synthesising it for a pod would invent a
finding no detector emitted. An unready pod is already visible in its
row's `ready` field.

`Owner` is set from the exported `inventory.PodOwners`, and omitted when
the resolved owner is the pod itself — a bare pod has no controller and
must not claim one.

## What does not change

- The `inspectKinds` enum, `internal/mcp/view.go`, and the
  `kubeagent_triage` payload.
- The number of cluster reads. The resolver runs over the snapshot
  `scan.Evaluate` already returned.
- `found: false` still returns events. The unconditional event read is
  deliberate — "the object is gone but its events explain why" is exactly
  what a drill-down must answer — and it stays.
- Sanitization. No new field reads a value the API server does not
  validate: a pod phase is a server-set enum, and no message, reason or
  condition text is newly surfaced. No new `internal/safetext.Line`
  ingress point is required.
- Schema versions. `InspectOutput` is not one of the eight versioned JSON
  documents (`report.ScanReport`, `gate.Verdict`,
  `rbacprofile.RulesDocument`, `rbacprofile.CheckDocument`,
  `watch.IssuesReport`, `watch.ExplanationsReport`, `baseline.Document`,
  `fleet.Report`), so nothing moves and no schema is regenerated.
- `internal/mcp` remains **read-only toward the cluster**: `get`/`list`
  only, no writes, and it must never import `internal/remediate` or
  `internal/explain`. Separately and additionally: it makes **no LLM
  call**. Neither promise implies the other, and nothing in this slice may
  be phrased as though a tool result were related to `--explain`.
- `internal/mcp`'s single carve-out is unchanged: the eager startup
  connection check names the kubeconfig path and context on stderr. The
  protocol stream and every tool result stay path-free, and the new code
  path introduces no kubeconfig path, no full server URL and no node name
  that was not already there.

## Documentation

| File | Change |
|---|---|
| `internal/mcp/inspect.go` | The tool description names the seven inspectable kinds, so the model learns the limit before a schema rejection |
| `skills/triaging-a-cluster/SKILL.md` | Add webhook configurations to the non-inspectable list (six kinds becomes seven); state that a `critical` pod finding is directly inspectable and that the answer names its owner |
| `commands/triage.md` | The same webhook addition to step 4's list |
| `skills/reading-kubeagent-findings/SKILL.md` | "Seven checks are skipped on every call" becomes seven, or eight when the server runs without `--logs` — the shipped manifest passes `--logs`, so seven is right for a plugin install and wrong for a hand-configured server |
| `website/docs/features/mcp.md` | Document the `owner` field and what `found: false` now means |

`plugin_manifest_test.go`'s `TestShippedDocsNameOnlyRegisteredTools` stays
green: no document names a tool that is not registered.

## Testing

TDD throughout — failing test first. `internal/mcp` uses client-go's fake
clientset; the resolver is pure and tested directly. Every fixture uses
synthetic names.

1. `PodRowFor` produces the row `Assemble` produced before the refactor.
2. The resolver, as a table over all seven kinds: each found by its own
   kind, and not found under a different one.
3. A controller-owned pod (Deployment → ReplicaSet → Pod, pod OOMKilled)
   is `found: true` with `Kind: "Pod"`, one pod row, `owner:
   "Deployment/<name>"`, and the critical finding attached. This is the
   reported defect; it must be seen to fail before the fix.
4. A healthy Deployment (`1/1`, zero restarts) is `found: true` with a
   correct `status`, `desired` and `ready` — the second half of the same
   defect.
5. A genuinely absent object is still `found: false`, still carries empty
   lists rather than nulls, and still returns its events.
6. A pod answer's JSON carries no `desired` and no `ready` key, so absence
   never renders as zero.
7. A bare pod's JSON carries no `owner` key.
8. A Job's fourth pod, past `jobPodCap`, is inspectable.
9. `internal/report/testdata/golden-scan.txt` is byte-identical. Never
   regenerated with `-update`.

## Constraints

Inherited from `CLAUDE.md`; all binding.

- `internal/mcp` is read-only toward the cluster and must never import
  `internal/remediate` or `internal/explain`. Separately, it makes no LLM
  call. The two are never blurred, and no comment, doc line, help string
  or commit message may suggest an MCP tool is related to `--explain`.
- Untrusted API text is sanitized at ingress via `internal/safetext.Line`,
  never at the renderer. Matching decisions run on the raw value.
- No new dependency: `go.mod` and `go.sum` must not change.
- No schema moves, and no test is run with `-update`, for any reason.
- `internal/report/testdata/golden-scan.txt` stays byte-identical. The
  README demo GIF and `website/docs/quickstart.md` are not regenerated.
- `plugin_manifest_test.go` and `internal/cli/plugin_flags_test.go` must
  stay green.
- No secrets, credentials, private IPs or internal hostnames anywhere —
  code, tests, fixtures, docs or help text. Every example and fixture uses
  a synthetic name. Documentation IPs are RFC 5737; example domains are
  RFC 2606. Nothing beyond `scheme://host`.
- `go test` runs with `-p 2`, never `-short`. Every commit is
  `git commit -s`, authored solely by the repository owner, with no
  `Co-Authored-By` trailer and no AI attribution of any kind.
- No cluster is needed to implement this slice, and `./chaos/run.sh` is
  never run.

## Recorded, out of scope

`inventory.PodRow` carries `Node` and `IP`, and every `kubeagent_inspect`
result already emits both for every pod row it returns — this predates the
slice and is not introduced by it. `CLAUDE.md` lists Kubernetes node names
among the values treated as credentials, while the scan's own text report
shows a `NODE` column by design, so whether an MCP tool result should
carry one is a real question with a real trade-off: a node name is how an
operator correlates failures across workloads, and it is also an
infrastructure identifier being forwarded off the operator's machine.

That decision is not made here. Fixing object resolution and changing what
a pod row discloses are two different changes, and bundling them would
make one reviewable diff into two arguments.
