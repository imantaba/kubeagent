# Capacity hints (`--capacity`)

`kubeagent scan --capacity` answers two questions cluster operators keep
asking by hand: **how much scheduling room is actually left, and would the
cluster survive losing its biggest node?** — plus a second, quieter question:
**which workloads are shaped wrong on paper, before anything has gone
wrong?**

```bash
kubeagent scan --capacity
```

```text
CAPACITY  (advisory — resource arithmetic on requests; ignores affinity,
           topology spread, PVC zoning, and PodDisruptionBudgets)
  Headroom
    schedulable        5.9 cores, 8Gi free across 2 of 3 nodes
    largest pod fit    worker1  3.5 cores, 6Gi
    tightest node      worker2  78% of memory requested
    lose worker1       may not fit — first-fit could not place StatefulSet/prod/db (2.1 cores)
    excluded           control-plane-1  (NoSchedule taint)
  Right-sizing  (metrics-server: 11 of 12 pods reporting)
    no requests set    Deployment/staging/web  · 0.0 cores observed
                         — BestEffort: first evicted under pressure
    limit, no request  Deployment/prod/cache  lim 256Mi  · 240Mi observed

    one sample per pod, ~30s average — not a peak, not a history
```

!!! note
    The example above is synthetic. Your cluster's output will reflect its
    own nodes, workloads, and whether metrics-server is installed.

The flag is off by default; set `KUBEAGENT_CAPACITY=true` to enable it
without passing `--capacity` on every invocation.

## Two numbers this feature will never produce

kubeagent has no price table, and no cluster publishes one. Every figure in
this section is cores and GiB — there is no currency symbol, no `cost`, no
`spend`, anywhere in the output.

And metrics-server keeps no history. A `GET` on
`/apis/metrics.k8s.io/v1beta1/pods` returns one sample — roughly a 30-second
average — and retains nothing before or after it. Whether that sample landed
in a quiet minute or a traffic spike is unknowable, so kubeagent never calls
a workload's request **over-requested**, **oversized**, or **wasteful**.
Those words do not appear here, for any workload, under any reading.

Everything this section says instead is one of two things: arithmetic on
what a pod *asked for* (its requests and limits — provable from the spec
alone), or a single, honestly-labelled usage sample attached as context to a
row a structural rule already produced.

## Headroom

Headroom is resource arithmetic on requests, computed over the nodes that
can actually take a new pod right now — not every node in the cluster. A
node is **excluded** when any of these hold, checked in this order (a node
failing more than one is reported for the first that matches):

| Excluded when | Reported as |
| --- | --- |
| `Ready` condition is not `True` | `NotReady` |
| `spec.unschedulable` is true | `cordoned` |
| carries a `NoSchedule` taint | `NoSchedule taint` |
| carries a `NoExecute` taint | `NoExecute taint` |

Counting a tainted control-plane node's free cores as available headroom is
the classic headroom lie, so the exclusion list is printed, not buried. In
the example above, `control-plane-1` is excluded and named, not silently
dropped from the denominator.

| Row | What it means |
| --- | --- |
| `schedulable` | Free CPU and memory summed across every **included** node, and how many of the cluster's nodes that is. |
| `largest pod fit` | The single included node with the most free CPU, named — with **its own** free memory printed beside it, never another node's. A pod lands on one node, so mixing the CPU high-water mark from one node with the memory high-water mark from another would describe a shape nothing can schedule. When a different node has more free memory, that node gets a second, unlabeled line right below — its own CPU and memory, for the same reason. |
| `tightest node` | The included node closest to full, by whichever of its CPU-requested or memory-requested ratio is higher. |
| `lose <node>` | What happens if the largest included node disappeared — see below. |

Non-terminal pods only count toward a node's requests — a `Succeeded` or
`Failed` pod reserves nothing, the same rule the Resources summary already
applies.

### `lose <node>` and the first-fit asymmetry

kubeagent takes the largest included node by allocatable CPU, removes it,
and tries to place its pods — DaemonSet pods excluded, since they do not
rehome onto another node — onto the remaining included nodes by
first-fit-decreasing, largest CPU request first.

This is **one-sided sound**: a successful placement is a constructive proof
that the requests fit somewhere. A failed placement proves nothing — a
different bin-packing might still succeed where first-fit didn't. So the
wording respects that asymmetry deliberately:

- success reads `fits — first-fit placed all N pods`
- failure reads `may not fit — first-fit could not place <owner> (N cores)`

It never reads "does not fit". A single included node has no node to lose
against, so that row reads `single node — no node-loss arithmetic possible`
instead of running the algorithm against nothing.

## Right-sizing

Three rules, each provable from a pod's spec alone — no usage sample is
needed to raise any of them:

| Rule | Fires when | Why it's real |
| --- | --- | --- |
| `no requests set` | A container declares neither a CPU nor a memory request. | The container reserves nothing. When *every* container in the pod is like this, the pod is BestEffort — first evicted under node pressure, and the row says so; when only some are, the note is omitted. |
| `limit, no request` | A container sets a limit for a resource but no request for it. | Kubernetes defaults an unset request to the limit, so the workload silently reserves the full limit cluster-wide — usually not what its author intended. |
| `never schedulable` | No single **included** node's allocatable CPU and memory can both satisfy a container's request — whether the request outright exceeds every node's CPU or memory, or (on a heterogeneous pool) its CPU fits one node's maximum and its memory fits a different node's, but no one node has both. | The pod can never be placed, anywhere — a pod lands on exactly one node, so it needs one node with both. This is provable now, even for a workload scaled to zero, before a single Pending pod exists. |

Two rules were deliberately left out, on the same YAGNI-and-opinion-neutrality
grounds:

- **`request == limit`** — usually a deliberate Guaranteed-QoS choice, not a
  defect.
- **`no memory limit`** — a defensible cluster-wide policy on plenty of
  clusters, not something kubeagent should second-guess.

`never schedulable` can overlap the existing Pending/Unschedulable detector
once replicas actually exist and fail to place. That's intentional, not
double-counting: the detector reports the *symptom* as a `Finding`; this
reports the *shape* as advice. This section produces no `Finding` of its
own, so nothing here changes what the verdict counts.

Rows roll up by owner (`Deployment/prod/payments`) the same way NEEDS
ATTENTION does — through a pod's ReplicaSet to its Deployment when one
exists — ordered by namespace then name, with at most 20 owners listed per
rule and the remainder shown as `… +N more`, matching the `--drift` cap.
`-n <namespace>` scopes this enumeration; headroom stays cluster-wide
regardless, since nodes are cluster-scoped and the requests arithmetic has
to be too. To do that, kubeagent refetches the full pod list when `-n` is
set; if that refetch fails — typically a service account whose list-pods
grant is namespace-scoped — the scan still completes, degraded rather than
failed, and prints `kubeagent: warning: cluster-wide pod list unavailable`
on stderr naming the namespace the numbers fell back to.

## The usage sample never selects a workload

A workload appears in Right-sizing because a structural rule flagged it —
never because of what metrics-server reported. Once a row exists, the usage
sample is attached to it as extra context, nothing more.

The direct consequence: **a healthy workload with a tiny usage sample never
appears.** One 30-second reading can annotate a row a structural rule
already raised; it can never, by itself, justify a claim that a workload's
requests are wrong. Coverage is always stated up front —
`metrics-server: 11 of 12 pods reporting` — and when metrics-server is
absent entirely, every structural row still renders, with
`metrics-server unavailable — structural rules only` in place of the
coverage count.

## What it deliberately does not do

- **It never affects the cluster verdict.** Like `--operators` and
  `--drift`, this section is advisory: it never produces a `Finding`, never
  appears in NEEDS ATTENTION, and never changes Healthy/Degraded or the exit
  code.
- **It never remediates.** No resize, no eviction, no scale. `--fix` is not
  extended by this flag.
- **It is not wired into the `watch` daemon.** Like `--operators` and
  `--drift`, this is a `scan`-only composed view.
- **It needs no new RBAC.** Nodes and pods are already read on every scan,
  and the one new read — a `GET` on `/apis/metrics.k8s.io/v1beta1/pods` —
  needs nothing beyond the grant the existing node-metrics path already
  uses. There is no `deploy/rbac-capacity.yaml`, because there is nothing
  for it to grant.
