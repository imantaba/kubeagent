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

`scan` shows each node's aggregate kubelet resource reservation, computed as
`Capacity − Allocatable` (the combined effect of `system-reserved`,
`kube-reserved`, and `eviction-hard`). A node is flagged with a **WARNING** when
it reserves no memory (allocatable memory equals capacity) — a kubelet
configuration that lets OS or kubelet memory pressure destabilise the node. CPU
reservation is shown but not warned, since many clusters intentionally leave it
unset. The check reads only the Node objects already listed during a scan, so it
needs no extra permissions, and it is advisory: it never changes the cluster
verdict.

### PVC reclaim policy

`scan` lists Bound PersistentVolumeClaims whose bound PersistentVolume has
`reclaimPolicy: Delete`. For those volumes, deleting the PVC (or the PV) tells
the provisioner to destroy the underlying storage — so the section is a
data-loss audit: which claims are *not* protected by `Retain`. The reclaim
policy is read from the bound PV (the authoritative value), so only Bound PVCs
appear. `Delete` is the common default for dynamic provisioners, so the list can
be long; it is informational and never changes the cluster verdict. Reading PVCs
and PVs needs only `get`/`list`/`watch`.

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
