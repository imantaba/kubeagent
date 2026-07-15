# Failure diagnostics

`kubeagent` scans your cluster, finds unhealthy pods, and explains *why* they
are failing — covering the most common pod failure modes.

## Read-only operation

`kubeagent` talks to the cluster directly via the official Kubernetes Go client
(`client-go`) — the same library `kubectl` and operators use — and operates
**read-only**. It never creates, updates, patches, or deletes cluster resources.

## Failure modes detected

### CrashLoopBackOff

The container keeps restarting. Kubernetes backs off exponentially between
attempts. `kubeagent` surfaces the exit code and last termination reason so you
can spot crash loops without tailing logs manually.

### ImagePullBackOff / ErrImagePull

The image cannot be pulled — either the image tag does not exist or the node
lacks credentials for the registry. `kubeagent` reports the image reference and
the pull error from the pod's conditions.

### OOMKilled

The container exceeded its memory limit and was killed by the kernel OOM killer.
`kubeagent` annotates the finding with the container's configured requests and
limits (see [Resource context](resource-context.md)) so you can judge whether to
raise the limit or reduce memory pressure.

### Pending / Unschedulable

No node can place the pod. This covers insufficient CPU or memory, a missing
taint toleration, an unsatisfied node affinity, or no nodes at all.
`kubeagent` reports the scheduler message from the pod's events.

### VolumeAttachError

A pod stuck at container creation because a volume cannot be attached.
`kubeagent` reads the pod's `FailedAttachVolume` Warning events and names the
**Multi-Attach** case specifically (a ReadWriteOnce volume still attached to
another node). Read-only: events are fetched with a single field-selected List.

### RestartLoop

A container that keeps exiting with a non-OOM error and restarting (≥ 3
restarts, current run younger than 10 min) even though it is currently
`Running`. This is the flapping case that `CrashLoopBackOff` misses — that
condition only fires while the container is in a `Waiting` / back-off state.
`kubeagent` reads `RestartCount` and `lastState.Terminated` from the pod
status. Read-only.

### Node reservations

`scan` reports each node's aggregate kubelet resource reservation for **memory,
CPU, and ephemeral-storage**, computed as `Capacity − Allocatable` (the combined
effect of `system-reserved`, `kube-reserved`, and `eviction-hard` — the Node API
cannot split kube- from system-reserved). A per-resource summary appears under
`CONTEXT` — one line each for memory, CPU, and ephemeral-storage, reading `N of M nodes
reserve none` or `all M nodes reserve some` (with `⚠`/`✓` on the memory and
ephemeral-storage lines). A node that reserves no
**memory** or no **ephemeral-storage** is flagged with a **WARNING** in `NOTES` —
both let OS/kubelet memory or disk pressure destabilise the node. CPU reservation
is shown but not warned, since it is compressible and many clusters intentionally
leave it unset; a resource a node does not report is shown as `not reported`. The
check reads only the Node objects already listed during a scan, so it needs no
extra permissions, and it is advisory: it never changes the cluster verdict.

### PVC reclaim policy

`scan` lists Bound PersistentVolumeClaims whose bound PersistentVolume has
`reclaimPolicy: Delete`. For those volumes, deleting the PVC (or the PV) tells
the provisioner to destroy the underlying storage — so the section is a
data-loss audit: which claims are *not* protected by `Retain`. The reclaim
policy is read from the bound PV (the authoritative value), so only Bound PVCs
appear. `Delete` is the common default for dynamic provisioners, so the list can
be long; it is informational and never changes the cluster verdict. Reading PVCs
and PVs needs only `get`/`list`/`watch`.

### Disk usage (opt-in)

`scan --disk-usage` reads each node's kubelet `/stats/summary` and warns when a
node's root filesystem or a PersistentVolumeClaim is at or over
`--disk-threshold` (default `0.80`) — an early warning that fires before the
kubelet's `DiskPressure` eviction signal. Over-threshold volumes appear in
**NEEDS ATTENTION**; the full detail is in JSON `diskUsage`.

It is **off by default**: it needs the `nodes/proxy` subresource (a broader grant
than kubeagent's usual `get`/`list`/`watch`), so you opt in explicitly with the
flag and, in-cluster, with the `nodes/proxy` RBAC add-on. It never changes the
cluster verdict.

### Ingress route health

`scan` walks every Ingress rule (and default backend) and follows the route to
its backend Service. It flags a route when the Service is missing, has no ready
endpoints (the usual cause of a 502/503), or does not expose the referenced
port — so a broken public route reads as, e.g., `ingress shop/web
example.com/api backend Service api-svc:8080 has no ready endpoints (likely
502/503)`. Only Service backends are checked (Resource backends are skipped), and
routes resolve within the Ingress's own namespace. It is read-only and advisory:
it appears in **NEEDS ATTENTION** and JSON `ingressIssues` but does not change
the cluster verdict.

### Node heartbeat freshness

Each node renews a `Lease` in `kube-node-lease` about every 10 seconds; the
control plane only marks a node `NotReady` after ~40 seconds of missed renewals.
`scan` reads those Leases and flags a node that still reads **Ready** but whose
lease has gone stale — `✗ node worker-2 kubelet not heartbeating (lease 48s
stale)` — so a crashed, hung, or partitioned kubelet shows up *before* the node
flips to `NotReady`. It degrades the cluster verdict, and the threshold is
tunable with `--node-heartbeat-threshold` (default `40s`; `0` disables it).
Compares against the scanner's clock, so run it in-cluster (the watch daemon) or
on a clock-synced host. The count of flagged nodes is also exposed in JSON as `nodesStaleHeartbeat`.

### Expected-node baseline

`scan --expected-nodes nova-worker-1,nova-worker-2,…` declares the node names you
expect. kubeagent flags each declared node that has **no `Node` object** in the
cluster — `✗ node nova-worker-2 expected but absent from the cluster` — which
catches a kubelet that never registered its node, or a node that dropped out of
the cluster entirely. It degrades the cluster verdict. A node that exists but is
`NotReady` counts as **present** (its health is flagged by the NotReady /
heartbeat checks); unexpected/extra nodes are never flagged. It is opt-in (off
until you declare a list) and best on clusters with **stable** node names —
autoscaled clusters whose node names churn would false-positive. The count is
also exposed in JSON as `nodesExpectedAbsent`.

### Kubelet health probe (opt-in)

`scan --kubelet-health` actively probes each node's kubelet `/healthz` through
the `nodes/proxy` subresource (the same add-on `--disk-usage` uses) and flags a
kubelet that is **reachable but reporting unhealthy** — `✗ node worker-2 kubelet
/healthz unhealthy: [-]pleg failed`. This is the "alive but sick" failure mode
(a failing PLEG/runtime/syncloop subcheck) that the passive lease-heartbeat and
`NotReady` checks miss, and it often shows *before* the node flips to `NotReady`.
A dead/unreachable kubelet is skipped (already flagged by the node checks), and a
missing `nodes/proxy` grant prints a one-line hint. It is read-only (a `GET`),
opt-in, and **advisory** — it appears in the `KUBELET HEALTH` section and JSON
`kubeletHealth` but does not change the cluster verdict. Enable it in the daemon
with `KUBEAGENT_KUBELET_HEALTH=true` and the `nodes/proxy` add-on
(`deploy/rbac-diskusage.yaml` or Helm `kubeletHealth.enabled=true`).

### Security posture (opt-in)

`scan --security` walks every workload's pod template and each Service and flags
high-signal, Pod Security Standards-aligned problems: privileged or
over-privileged containers (privileged, host namespaces, `hostPath`, `hostPort`,
dangerous added capabilities), insecure container defaults (runs as root,
`allowPrivilegeEscalation` not disabled, capabilities not dropped), and Services
exposed outside the cluster (`NodePort` / `LoadBalancer` / `externalIPs`). Each
finding is labelled `baseline`, `restricted`, or `kubeagent` and printed in a
dedicated **SECURITY** section (also JSON `securityIssues`). The **SECURITY**
section is signal-first: it opens with a one-line tier summary, shows the
dangerous `baseline` and exposed-service findings in full per workload, and
folds the near-universal `restricted` hardening gaps into a per-check aggregate;
pass `--security-verbose` to list every finding per workload instead. JSON
`securityIssues` always contains all findings regardless of the flag. It is a
curated subset aligned with the Pod Security Standards, not a conformance
scanner. It is read-only and **advisory** — it does not change the cluster
verdict — needs no extra RBAC, and skips
`kube-system`/`kube-node-lease`/`kube-public` unless you target one with `-n`.

### Output layout

`scan --output text` groups findings by how urgently they need action:

- **NEEDS ATTENTION** — failing workloads, Services with no ready endpoints,
  credential warnings, volumes over the disk-usage threshold, and broken ingress
  routes.
- **NOTES** — advisories that rarely need immediate action: PersistentVolumeClaims
  on a `Delete` reclaim policy (a grouped summary; pass `--pvc-reclaim` for the
  full list), Services that are intentionally empty (scaled to zero or a CronJob
  between runs), and counts of workloads hidden behind `--include-restarts` /
  `--include-cron`.
- **CONTEXT** — reference data: node readiness and kubelet reservations (collapsed
  to one line when all nodes are fine), the cluster resource summary, and platform
  facts.

A "Needs attention" line under the cluster verdict summarizes how many workloads
are failing and how many Services have no endpoints. `--output json` is
unaffected and always contains the full detail.

Each finding in **NEEDS ATTENTION** now shows its underlying signal on an
indented `↳` line — for example, an unschedulable pod prints the scheduler's
verbatim message (`0/5 nodes are available: 3 Insufficient memory, …`) directly
in the text output, without needing `--output json` or `--explain`. Similarly,
a `NotReady` node names its kubelet-reported cause (the `NodeReady` condition's
reason and message) instead of a bare `NotReady`. The cluster verdict and JSON
schema are unchanged.

## Status

`kubeagent scan` performs a read-only, whole-cluster scan and reports
CrashLoopBackOff, ImagePullBackOff/ErrImagePull, OOMKilled,
Pending/Unschedulable, VolumeAttachError (Multi-Attach), and RestartLoop pods, in text or JSON.

The optional `--explain` flag makes a single Claude API call to summarize
findings in plain English. The deterministic core still works offline with no
API key.

## Example output

```text
P2 — Workload issues

  NAMESPACE   NAME               KIND        READY   STATUS              RESTARTS
  staging     api-server         Deployment  0/2     CrashLoopBackOff    47
  staging     image-builder      Deployment  0/1     ImagePullBackOff    0
  production  worker             Deployment  0/3     OOMKilled           12
  production  batch-processor    Job         0/1     Pending             0
```

## What changed

When a Deployment is flagged and its most recent rollout is recent (within 7
days), kubeagent adds a `changed:` line with the revision, its age, and the
first-container image delta:

```text
⚠ shop/web  Deployment  0/1 Degraded
    ⚠ ImagePullBackOff: Bad image reference or registry authentication
    ↳ changed: rollout to revision 6, 4d ago · image nginx:1.27 → nginx:bad
```

It reuses the ReplicaSet history already collected (read-only), states only what
changed and when, and never claims the rollout caused the problem — that
connection is left to you (or `--explain`).
