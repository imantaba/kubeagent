# Capacity hints (`scan --capacity`) — design

**Theme F slice 3 (final F slice): cost/right-sizing + scheduling-headroom hints.**

Slice 1 (v0.60.0) shipped operator/CRD adapters (`--operators`); slice 2 (v0.61.0)
shipped reconciler-reported GitOps drift (`--drift`). This slice closes Theme F with
one opt-in, advisory `CAPACITY` section carrying two sub-blocks: **Headroom** (how much
room is left, and whether the cluster survives losing a node) and **Right-sizing**
(workload shapes that are structurally wrong).

## The honesty premise

Two numbers this feature will never produce:

1. **Money.** kubeagent has no price table, and no cluster publishes one. Every figure
   here is cores and GiB. The words *cost*, *spend*, and any currency symbol do not
   appear in the output.
2. **A peak.** metrics-server keeps no history. `/apis/metrics.k8s.io/v1beta1/pods`
   returns a single sample — roughly a 30-second average — and retains nothing. It is
   unknowable whether that sample landed in a quiet minute or a traffic spike.

Consequently no workload is ever labelled over-requested, oversized, or wasteful. The
words **peak**, **over-requested**, **oversized**, and **waste** are structurally absent
from `internal/capacity`, from the report, and from the docs — the same discipline that
keeps the string `drifted for` out of `internal/gitops`.

## 1. Architecture and surface

New pure package `internal/capacity`:

```go
func Assess(nodes []corev1.Node, pods []corev1.Pod,
    usage map[string]corev1.ResourceList, namespace string) Report
```

No client, no `context.Context` — nodes and pods are already collected on every scan, so
the headroom half costs **zero** extra API calls. The one new fetch is per-pod usage:

```go
func PodMetrics(ctx context.Context, client kubernetes.Interface) (map[string]corev1.ResourceList, bool, error)
```

in `internal/collect`, a raw GET on `/apis/metrics.k8s.io/v1beta1/pods` mirroring the
existing `NodeMetrics` (`internal/collect/collect.go:176`) exactly: same read-only idiom,
same "unavailable is not an error" handling, keyed `namespace/name`.

Wiring follows the CLI-composed-view pattern already used by `--operators` and
`--drift`: `main.go` calls the collector and `capacity.Assess`, sets `in.Capacity`, and
the report renders it. `internal/scan` and `internal/watch` are untouched and the daemon
gains no new RBAC. Reading `metrics.k8s.io` needs no grant beyond what the existing
node-metrics path already uses.

The section is **advisory**: it never produces a `Finding`, never appears in
`hasAttention`, never changes the Healthy/Degraded verdict, and never changes the exit
code.

**Flag:** `--capacity` (off by default), env `KUBEAGENT_CAPACITY`. No second flag and no
threshold flag — nothing in this feature has a tunable boundary.

## 2. Headroom — per-node arithmetic with stated exclusions

### Node inclusion

A node is **included** only when all three hold:

- `status.conditions[type=Ready].status == "True"`
- `spec.unschedulable` is false (not cordoned)
- it carries no taint with effect `NoSchedule` or `NoExecute`

Excluded nodes are enumerated with their reason. Counting a tainted control plane's free
cores as available headroom is the classic headroom lie, so the exclusion list is part of
the output, not a hidden implementation detail. When a node is excluded for more than one
reason, the first reason in the order above is reported.

### Rows

| Row | Computation |
| --- | --- |
| `schedulable` | Σ over included nodes of `max(0, allocatable − requests)`, per resource, plus `across N of M nodes`. |
| `largest pod fit` | The included node maximising free CPU, named, with **its own** free memory beside it. If a *different* node maximises free memory, it gets a second line. |
| `tightest node` | The included node with the highest `requests / allocatable` ratio, taking the larger of its CPU and memory ratios. |
| `lose <node>` | Node-loss arithmetic — see below. |

`largest pod fit` never mixes nodes. A pod lands on exactly one node, so reporting
`2.4 cores` from worker1 beside `42Gi` from worker3 would describe a shape nothing can
schedule.

Per-node requests count non-terminal pods only (`Succeeded`/`Failed` reserve nothing) —
the same rule `internal/resources` already applies.

### Node-loss (`lose <node>`)

Take the largest included node by allocatable CPU — ties broken by node name ascending,
so the row is deterministic — remove it, and attempt to place its
**non-DaemonSet** pods onto the remaining included nodes by first-fit-decreasing (FFD),
sorted by CPU request descending. DaemonSet-owned pods are excluded because they do not
rehome — a DaemonSet pod on a deleted node is simply gone.

FFD is *one-sided sound*: when it places everything, that is a constructive proof the
requests fit. When it fails, a different packing might still succeed. The wording
respects that asymmetry:

- success → `fits — first-fit placed all N pods`
- failure → `may not fit — first-fit could not place <owner> (2.1 cores)`

Never a flat "does not fit".

### Boundary statement and edge cases

The section header states the boundary once: this is resource arithmetic on requests, and
it ignores affinity/anti-affinity, topology spread constraints, PVC zoning, and
PodDisruptionBudgets.

- Zero included nodes → print the exclusion list and no rows.
- Exactly one included node → the node-loss row reads
  `single node — no node-loss arithmetic possible`.
- A node reporting zero allocatable for a resource → its ratio is treated as 0, matching
  `resources.pct`.

## 3. Right-sizing — three structural rules

Each rule is provable from the pod spec alone, with no usage data.

| Rule | Trigger | Why it is real |
| --- | --- | --- |
| `no requests set` | A container declares neither a CPU nor a memory request. | The container reserves nothing. When *every* container in the pod does this the pod is BestEffort — first evicted under node pressure, and the row says so; when only some do, the row omits the BestEffort note. |
| `limit, no request` | A container sets a limit for a resource but no request for it. | Kubernetes defaults the request to the limit. The workload reserves the full limit cluster-wide, which its author usually did not intend. |
| `never schedulable` | A container's CPU or memory request exceeds the largest **included** node's allocatable for that resource. | The pod can never be placed. Provable now — including for a workload scaled to zero, before any Pending pod exists. |

Deliberately **not** rules, on YAGNI and opinion-neutrality grounds:

- `request == limit` — usually a deliberate Guaranteed-QoS choice.
- `no memory limit` — a defensible cluster-wide policy, not a defect.

`never schedulable` overlaps the existing Pending/Unschedulable detector whenever
replicas exist. That is intentional and non-contradictory: the detector reports the
*symptom* as a Finding; this reports the *shape* as advice. The capacity section produces
no Finding, so nothing is double-counted in the verdict.

### Roll-up, ordering, and cap

Rows roll up by **owner** (`Deployment/prod/payments`), matching how NEEDS ATTENTION
already reports workloads, resolved from the pod's controller `ownerReferences` — for a
ReplicaSet-owned pod, the ReplicaSet's own owner (the Deployment) when present, else the
ReplicaSet. A pod with no controller owner is reported as `Pod/ns/name`.

Ordering within a rule is deterministic: by namespace, then owner name. At most **20**
owners are enumerated per rule, with the remainder as `… +N more` — never silently
dropped. This matches the `--drift` cap.

`-n <namespace>` scopes the right-sizing enumeration — hence the `namespace` parameter on
`Assess`, which filters the enumeration only. Headroom stays cluster-wide regardless,
because nodes are cluster-scoped and requests accounting must be too. `Assess` is
therefore handed the **cluster-wide** pod list in every case: `main.go` already refetches
all pods for the resources summary when a namespace is set (`resourcePods`), and
`--capacity` reuses that exact slice rather than issuing its own list call.

## 4. The observed sample — attached, never selecting

The usage sample **never puts a workload on the list**. A workload appears because a
structural rule flagged it; the sample is then attached to that row as context. The
direct consequence, stated plainly in the docs: a healthy workload with a tiny sample
never appears, because a single reading cannot justify changing its requests.

- Per-owner samples are summed across that owner's reporting pods.
- Coverage is always reported: `metrics-server: 14 of 16 pods reporting`.
- Fixed footer wording: `one sample per pod, ~30s average — not a peak, not a history`.
- When metrics-server is absent, the sub-block still renders every structural row and
  states `metrics-server unavailable — structural rules only`. This is the path a bare
  Kind cluster actually takes, so it is the path the chaos gate exercises.

Nothing from a pod `spec` other than resource quantities, container names, and owner
metadata reaches the output. No image reference, no command, no env var, no annotation.

## 5. Report surface

Text section `CAPACITY`, rendered after `GITOPS DRIFT`:

```text
CAPACITY  (advisory — resource arithmetic on requests; ignores affinity,
           topology spread, PVC zoning, and PodDisruptionBudgets)
  Headroom
    schedulable       5.9 cores, 108Gi free across 3 of 5 nodes
    largest pod fit   worker1  2.4 cores, 42Gi
    tightest node     worker2  92% of CPU requested
    lose worker1      may not fit — first-fit could not place
                      StatefulSet/prod/db (2.1 cores)
    excluded          control-plane-1, control-plane-2  (NoSchedule taint)
                      worker3  (cordoned)
  Right-sizing        (metrics-server: 14 of 16 pods reporting)
    no requests set   Deployment/staging/web  · 0.03 cores observed
                      Deployment/staging/api  · 0.11 cores observed
                        — BestEffort: first evicted under pressure
    limit, no request Deployment/prod/cache  lim 256Mi · 240Mi observed
    never schedulable Job/batch/trainer  req 40 cores > largest node (16)

    one sample per pod, ~30s average — not a peak, not a history
```

An empty sub-block renders nothing; a `CAPACITY` section with neither sub-block
populated renders nothing at all.

**JSON.** The key goes on `inventoryReport` — the struct `--output json` actually
marshals (`internal/report/report.go`) — as `capacity,omitempty`, populated in the
`enc.Encode(inventoryReport{...})` literal. It does **not** go on `report.Input`, which
is a parameter bag and is never marshalled. A default scan's JSON is unchanged.

## 6. Testing

- **Unit (`internal/capacity`), pure, fake nodes and pods:** every inclusion/exclusion
  reason including multi-reason precedence; each of the three structural rules including
  its near-miss; owner roll-up through ReplicaSet to Deployment and the ownerless-pod
  case; the 20-cap and `… +N more`; deterministic ordering; namespace scoping; zero
  included nodes; single included node; zero allocatable.
- **FFD table:** placement success as constructive proof, placement failure wording, and
  the DaemonSet-exclusion case.
- **Sample discipline:** a test asserting a workload with a small sample and sane
  requests produces no row; a test asserting the absent-metrics path renders every
  structural row plus the unavailable line; a test asserting the forbidden words
  (`peak`, `over-requested`, `oversized`, `waste`) appear nowhere in rendered output.
- **Report:** section rendering, empty-block suppression, JSON key present when set and
  absent when nil, golden fixture regenerated.
- **Chaos scenario 18:** apply a BestEffort Deployment, a limit-without-request
  Deployment, and a 40-core Job to the Kind cluster — which runs **no metrics-server** —
  and assert all three structural rows appear, the unavailable line appears, the
  headroom exclusion list names the control-plane node, and the cluster verdict is
  unchanged.

## 7. Documentation

- `website/docs/features/capacity.md` — the feature page, in the style of
  `gitops-drift.md`: what it answers, what it refuses to claim and why, the two
  sub-blocks, the three rules with their rationale, the FFD asymmetry, and the
  no-money/no-peak premise stated up front.
- `website/mkdocs.yml` nav entry.
- `README.md` flag row, `CHANGELOG.md` under `[Unreleased]`.
- No new RBAC manifest: nodes and pods are already granted, and `metrics.k8s.io` reads
  need nothing beyond the existing node-metrics path.
- `website/docs/roadmap.md` — mark Theme F complete.
