# Curated policy packs, slice B: the cost pack — design

**Date:** 2026-08-08
**Status:** approved, ready for a plan
**Roadmap item:** post-1.0, curated policy packs, second half. Slice A (the
`security` pack) shipped as v1.12.0. This is slice B. Slice C — a pack
contributed by someone other than kubeagent itself — is out of scope here.

## Goal

Ship a third kubeagent-curated policy pack, `cost`, evaluated by the existing
`--policy` engine via `scan --policy-pack cost` and `gate --policy-pack cost`.
Sixteen rules, every one at `info`, over seven kinds. No engine change, no new
dependency, no schema move.

## What was verified before designing

Four facts were read out of the tree rather than assumed. Two of them
contradicted the framing the brainstorm started from.

### 1. The engine compares Kubernetes quantities correctly

This was the brainstorm's central worry and it is unfounded.
`internal/policy/op.go` routes `gt`/`gte`/`lt`/`lte` through `compareNumeric`,
which tries three parses in order: `strconv.ParseInt` (exact whole integers,
deliberately first so integers past 2^53 do not round together),
`strconv.ParseFloat`, and finally `resource.ParseQuantity` with `Cmp`.

So `500m` compares correctly against `8`, and `32Gi` against `64Gi`. A cost
threshold is not lexical string comparison and is not theatre. If neither side
parses, `checkOp` returns `skip` — the slot says nothing rather than becoming a
false accusation.

`internal/policy` already imports `k8s.io/apimachinery/pkg/api/resource` for
this, so nothing is added to `go.mod`.

### 2. `lte` skips an absent slot, which removes the need for pairing

`checkOp` gives only `exists` and `notExists` an opinion about absence; every
other operator returns `(ok=false, skip=true)` when the slot is not present.
The security pack had to pair rules because of this. The cost pack does not,
and that is a design property rather than an omission:

- A threshold rule fires only on a value someone actually wrote. A workload
  that sets no CPU request is never accused of setting a large one.
- Where the API defines a default — `successfulJobsHistoryLimit` is 3,
  `failedJobsHistoryLimit` is 1, `backoffLimit` is 6 — absence *is* the safe
  value, so a rule that fired on absence would be wrong. `lte` gets this right
  for free.

**The cost pack ships zero paired rules, and that is correct rather than
incomplete.**

### 3. The kinds the pack wants are already selectable

`internal/policy/policy.go`'s `selectableKinds` already contains
`HorizontalPodAutoscaler`, `Job`, `PersistentVolumeClaim` and `ResourceQuota`
alongside the four workload kinds. So the pack needs no widening,
`TestSelectableKindsMatchesRBACProfileCore` still pins the selectable set to
what `rbacprofile.coreRules` grants, and **no RBAC manifest moves.** A plain
`kubeagent scan` already reads every kind this pack selects.

`LimitRange` is *not* selectable. Nothing in this design wants it.

### 4. reliability already owns six rules the cost pack must not duplicate

Read from `internal/policypack/packs/reliability.yaml`. Four are about
`resources`; the other two share a kind with something the cost pack wants:

| reliability rule | kind | assertion | level |
| --- | --- | --- | --- |
| `reliability.deploy-memory-limit` | Deployment | `T.containers[*].resources.limits.memory` exists | warning |
| `reliability.statefulset-memory-limit` | StatefulSet | `T.containers[*].resources.limits.memory` exists | warning |
| `reliability.deploy-cpu-request` | Deployment | `T.containers[*].resources.requests.cpu` exists | info |
| `reliability.deploy-memory-request` | Deployment | `T.containers[*].resources.requests.memory` exists | info |
| `reliability.deploy-replicas-min-two` | Deployment | `spec.replicas` gte `2` | warning |
| `reliability.pvc-storage-class` | PersistentVolumeClaim | `spec.storageClassName` exists | info |

`reliability.cronjob-concurrency-policy` also selects CronJob, on
`spec.concurrencyPolicy` — a path no cost rule touches.

`T` is `spec.template.spec`. This constrains the cost pack more than anything
else in the design: the cost pack must say what reliability does not.

## Decisions

Four forks, all settled.

### Fork 1 — outlier thresholds, framed as outliers

The pack ships threshold rules, because the engine supports them correctly.
But a threshold is set generously enough that an ordinary workload never trips
one, and every threshold message says **confirm the size is deliberate**, never
*this is waste*. The pack cannot know your budget and must not pretend to.

An operator who disagrees with a threshold runs `kubeagent policy packs --print
cost > cost.yaml`, edits the number, and runs `--policy cost.yaml`. That is the
existing override path and this slice adds no new one.

### Fork 2 — every rule is `info`

reliability and security both carry `warning` rules and can therefore fail a
gate at `--fail-on warning`. The cost pack cannot fail a gate at any `--fail-on`
above `info`. This is a stated property, not an accident:

```
cost: 0 critical, 0 warning, 16 info

--fail-on critical  -> never fails
--fail-on warning   -> never fails
--fail-on info      -> fails
```

The justification: a cost finding is budget-dependent in a way a security
finding is not. "Privileged is bad" holds in every cluster. "Sixteen CPUs is
too many" does not. A pack that cannot know cannot accuse, so it advises.

`TestNoPackRuleIsCritical` is inherited unchanged and stays generic over
`policypack.All()`.

### Fork 3 — say only what reliability does not

No duplicate rule anywhere. The division:

| property | reliability covers | cost adds |
| --- | --- | --- |
| `requests.cpu` exists | Deployment | StatefulSet, DaemonSet, CronJob |
| `requests.memory` exists | Deployment | StatefulSet, DaemonSet, CronJob |
| `limits.memory` exists | Deployment, StatefulSet | nothing |
| `limits.ephemeral-storage` exists | nothing | Deployment, DaemonSet |
| request size ceiling | nothing | Deployment |
| replica count | floor of 2, Deployment | nothing |
| Job and CronJob retention | nothing | CronJob, Job |
| run duration bound | nothing | CronJob |
| autoscaler ceiling | nothing | HorizontalPodAutoscaler |
| claim size | `reliability.pvc-storage-class`, a different path | PersistentVolumeClaim |

Those ten rows account for all sixteen rules: 3 + 3 + 0 + 2 + 2 + 0 + 3 + 1 +
1 + 1.

The DaemonSet request messages name the multiplier — *scheduled once per node* —
which is a cost claim reliability's scheduler-framed message does not make.
Two rules can share a path and a kind-family without saying the same thing;
here they do not even share a kind.

Deliberately **not** added, on YAGNI:

- `limits.memory` spillover to DaemonSet and CronJob. That is reliability's
  rule aimed at two more kinds, and it belongs in reliability if anywhere.
- `imagePullPolicy: Always`. The bandwidth argument is real but marginal, and
  `reliability.deploy-image-not-latest` already catches the driver.

### Fork 4 — seven kinds

Beyond the four workload kinds, the pack selects `HorizontalPodAutoscaler`
(the one object whose entire purpose is authorizing more spend), `Job` (a high
`backoffLimit` burns a request on every failed attempt) and
`PersistentVolumeClaim` (storage is billed directly). All three are already
selectable, so nothing moves.

## The sixteen rules

`T` is `spec.template.spec`. The CronJob container rules spell their paths in
full, because a CronJob's pod template lives one level deeper at
`spec.jobTemplate.spec.template.spec` — the same convention slice A settled on.

Every rule is `level: info`.

| id | kind | path | op | values |
| --- | --- | --- | --- | --- |
| `cost.deploy-ephemeral-storage-limit` | Deployment | `T.containers[*].resources.limits.ephemeral-storage` | exists | — |
| `cost.deploy-large-cpu-request` | Deployment | `T.containers[*].resources.requests.cpu` | lte | `"8"` |
| `cost.deploy-large-memory-request` | Deployment | `T.containers[*].resources.requests.memory` | lte | `"32Gi"` |
| `cost.statefulset-cpu-request` | StatefulSet | `T.containers[*].resources.requests.cpu` | exists | — |
| `cost.statefulset-memory-request` | StatefulSet | `T.containers[*].resources.requests.memory` | exists | — |
| `cost.daemonset-cpu-request` | DaemonSet | `T.containers[*].resources.requests.cpu` | exists | — |
| `cost.daemonset-memory-request` | DaemonSet | `T.containers[*].resources.requests.memory` | exists | — |
| `cost.daemonset-ephemeral-storage-limit` | DaemonSet | `T.containers[*].resources.limits.ephemeral-storage` | exists | — |
| `cost.cronjob-cpu-request` | CronJob | `spec.jobTemplate.spec.template.spec.containers[*].resources.requests.cpu` | exists | — |
| `cost.cronjob-memory-request` | CronJob | `spec.jobTemplate.spec.template.spec.containers[*].resources.requests.memory` | exists | — |
| `cost.cronjob-successful-history` | CronJob | `spec.successfulJobsHistoryLimit` | lte | `"10"` |
| `cost.cronjob-failed-history` | CronJob | `spec.failedJobsHistoryLimit` | lte | `"10"` |
| `cost.cronjob-active-deadline` | CronJob | `spec.jobTemplate.spec.activeDeadlineSeconds` | exists | — |
| `cost.job-backoff-limit` | Job | `spec.backoffLimit` | lte | `"10"` |
| `cost.hpa-max-replicas` | HorizontalPodAutoscaler | `spec.maxReplicas` | lte | `"50"` |
| `cost.pvc-large-storage` | PersistentVolumeClaim | `spec.resources.requests.storage` | lte | `"1Ti"` |

Kind distribution: Deployment 3, StatefulSet 2, DaemonSet 3, CronJob 5, Job 1,
HorizontalPodAutoscaler 1, PersistentVolumeClaim 1. Total 16.
Level distribution: 16 `info`, 0 `warning`, 0 `critical`.

### The sixteen messages, verbatim

Every message below is **dot-free**, which `TestPackCarriesNoHostOrAddress`
requires. Thresholds are spelled as words (*eight*, *thirty-two gibibytes*) so
no decimal point can creep in. Use these verbatim; do not reword.

```
cost.deploy-ephemeral-storage-limit
  a container has no ephemeral-storage limit, so local disk use is bounded only by the node

cost.deploy-large-cpu-request
  a container reserves more than eight CPUs, so confirm the size is deliberate

cost.deploy-large-memory-request
  a container reserves more than thirty-two gibibytes of memory, so confirm the size is deliberate

cost.statefulset-cpu-request
  a container has no CPU request, so the scheduler cannot size it and the cluster is provisioned for a guess

cost.statefulset-memory-request
  a container has no memory request, so the scheduler cannot size it and the cluster is provisioned for a guess

cost.daemonset-cpu-request
  a container has no CPU request, and this workload is scheduled once per node

cost.daemonset-memory-request
  a container has no memory request, and this workload is scheduled once per node

cost.daemonset-ephemeral-storage-limit
  a container has no ephemeral-storage limit, and this workload writes on every node

cost.cronjob-cpu-request
  a container has no CPU request, so every run is placed as if it were free

cost.cronjob-memory-request
  a container has no memory request, so every run is placed as if it were free

cost.cronjob-successful-history
  the CronJob keeps more than ten successful runs, so completed Jobs and their pods accumulate

cost.cronjob-failed-history
  the CronJob keeps more than ten failed runs, so completed Jobs and their pods accumulate

cost.cronjob-active-deadline
  the job template sets no activeDeadlineSeconds, so a run that hangs occupies its request until something else stops it

cost.job-backoff-limit
  the Job retries more than ten times, so a run that cannot succeed still consumes its request on every attempt

cost.hpa-max-replicas
  the autoscaler may reach more than fifty replicas, so confirm the ceiling is deliberate

cost.pvc-large-storage
  the claim reserves more than one tebibyte, so confirm the size is deliberate
```

## What the cost pack cannot say

Three gaps, written down rather than worked around, following the precedent
slice A set. Each comes from a property the rule grammar does not have; adding
one would be an engine change, not a pack change.

1. **"limits set without requests" is not expressible.** The grammar has no
   cross-field relation — a rule asserts about one path, not about two at once.
   When a container sets a limit and no request, Kubernetes defaults the request
   to the limit, reserving the ceiling rather than the expected use. That is
   probably the single largest cost defect in a typical cluster, and this pack
   cannot name it.
2. **A Deployment with no CPU or memory request is unsaid if you run `cost`
   alone.** The `exists` half of that question lives in `reliability` and the
   size ceiling lives here, deliberately, so the two packs do not duplicate.
   Run both and the question is fully covered; run only `cost` and the unset
   case on a Deployment goes unmentioned.
3. **Absence across a namespace is not expressible.** A rule asserts over
   objects that exist. "This namespace has no `ResourceQuota`" is a claim about
   the absence of an object, which no rule can make — and the same shape rules
   out "this HorizontalPodAutoscaler has `minReplicas` equal to `maxReplicas`",
   which needs two fields compared to each other rather than to a constant.

## What it does not do

Three separate statements. None implies another and none may be blurred into
another.

- **A rule can never write to the cluster.** There is no `--fix` path from a
  policy rule. The pack is data handed to `internal/policy`, which is pure.
- **Separately: a pack makes no LLM call.** A pack has nothing to do with
  `--explain`, which is the model path.
- **Separately again, and new to this pack: kubeagent has no prices.** No
  billing data, no instance types, no node cost, no cloud API. The pack names
  shapes that usually cost money; it cannot tell you what anything costs, and
  no rule, message or doc line may imply otherwise.

## Architecture

Nothing new. The pack is a YAML file compiled into the existing stdlib-only
`internal/policypack` and evaluated by the existing `internal/policy`.

```
internal/policypack/packs/cost.yaml      (new)  16 rules
internal/policypack/policypack.go        (edit) one registry entry
internal/policypack/cost_rules_test.go   (new)  per-rule coverage
internal/cli/policy_test.go              (edit) widen listing + refusal tests
website/docs/features/policy-packs.md    (edit) restructure for three packs
CHANGELOG.md                             (edit) [Unreleased] ### Added
CLAUDE.md                                (edit) curated-packs bullet
website/docs/roadmap.md                  (edit) curated-packs item
```

No CLI help string enumerates the pack names — `--policy-pack name
(repeatable)` in `internal/cli/root.go` is generic — so no usage text moves.

Two lines in `website/docs/features/policy-packs.md` go stale the moment a
third pack ships and must be found rather than left: one reading *"of the two
packs that happen to ship"* and one reading *"every rule in both shipped
packs"*. The page is restructured for three packs, not appended to, and the
existing *"Not in this slice"* bullet narrows to slice C alone rather than
being deleted — retraction is not deletion.

`internal/policypack` stays stdlib-only (`embed` + `sort`) and imports nothing
from kubeagent; `internal/policypack/imports_test.go` enforces both halves and
is not edited. `internal/policy` is not touched by any task — no new operator,
no new relation, no new selectable kind. If a rule seems to need one, the rule
is wrong.

The registry slice stays sorted by name: `cost` sorts before `reliability`
before `security`, so the new entry goes **first**, not appended.
`TestAllIsSortedByName` checks this.

## Testing

Four generic pack tests are inherited the moment the registry entry lands, with
no edit to `packs_test.go`: `TestEveryPackLoads`,
`TestRuleIDsCarryTheirPackPrefix`, `TestNoPackRuleIsCritical` and
`TestPackCarriesNoHostOrAddress`.

On top of them, following slice A's shape:

- **Kind distribution** — one test pinning 3/2/3/5/1/1/1 across the seven kinds
  and 16/0/0 across the three levels.
- **Fires and passes** — one subtest per rule, sixteen in all, each built from
  a good fixture with exactly one thing broken. Every threshold case must set
  the field **explicitly** to a value past the threshold: a case whose field is
  merely absent makes `lte` skip, which is not a violation and would be a test
  that proves nothing.
- **No pairing** — a test asserting the pack contains no `exists`/value pair on
  the same path and kind, which is the inverse of slice A's
  `TestPairedRulesDivideTheWork`. It pins verified fact 2, so a later edit
  cannot quietly reintroduce a pair without a decision.
- **Quantity comparison is real** — at least one subtest proving a `500m` CPU
  request passes `lte "8"` while `16` fails it, so the design's central claim
  about `compareNumeric` is machine-checked rather than asserted in prose.

TDD throughout: failing test first, watch it fail, then implement. `go test`
runs with `-p 2` locally, never `-short`.

## Constraints inherited

- READ-ONLY toward the cluster: `get`/`list` only, no `--fix` path from a rule.
  Separately and additionally: no LLM call.
- NO ENGINE CHANGE. `internal/policy` is untouched.
- `internal/policypack` stays stdlib-only and imports nothing from kubeagent.
- NO NEW DEPENDENCY: `go.mod` and `go.sum` must not change.
- NO SCHEMA MOVES: `scan` stays 1.2, `gate` stays 1.1, the other six do not
  move. Never run any test with `-update`, in any task, for any reason.
- `internal/report/testdata/golden-scan.txt` stays byte-identical; do not
  regenerate the demo GIF or `website/docs/quickstart.md`.
- NO `critical` RULE.
- No dot in any rule message; no `://` and no dotted quad anywhere in the YAML.
- No secrets, credentials, private IPs or internal hostnames anywhere.
  Documentation IPs are RFC 5737; example domains RFC 2606. A curated rule must
  never name a registry hostname.
- Every commit needs `git commit -s`, authored solely by the repository owner —
  no `Co-Authored-By` and no AI attribution of any kind.

## Out of scope

- Slice C, the contribution path for a pack written by someone other than
  kubeagent.
- A quantity-aware operator, a cross-field relation, or any other engine
  widening. If one is wanted, it is its own slice with its own spec.
- Any claim about actual money.
