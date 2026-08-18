# Failure diagnostics

`kubeagent` scans your cluster, finds unhealthy pods, and explains *why* they
are failing — covering the most common pod failure modes.

## Read-only operation

`kubeagent` talks to the cluster directly via the official Kubernetes Go client
(`client-go`) — the same library `kubectl` and operators use — and operates
**read-only**. It never creates, updates, patches, or deletes cluster resources.

## Failure modes detected

Sixteen of the failure modes below — the pod-level ones a detector reports as
an issue kind — are also in the binary's own offline reference:
`kubeagent known-issues <kind>` prints what one means, what usually causes it,
and what to check, with no cluster and no network. Run it with no argument to
see exactly which sixteen; the rest of this page keeps its prose only here.
See [Known issues reference](known-issues.md).

### CrashLoopBackOff

The container keeps restarting. Kubernetes backs off exponentially between
attempts. `kubeagent` always names the container and its restart count, and
adds the last exit code, its reason and how long ago it happened once the
container has restarted at least three times with a non-OOM error termination
— so you can spot crash loops without tailing logs manually.

Those are the same durable fields [RestartLoop](#restartloop) reads. A flapping
pod alternates between the two kinds depending on which instant the scan
samples — one is `Waiting`, the other `Running` — and reading the exit code
from both means an operator does not have to run the scan twice to see it.

### ImagePullBackOff / ErrImagePull

The image cannot be pulled — either the image tag does not exist or the node
lacks credentials for the registry. `kubeagent` reports the image reference and
the pull error from the pod's container status.

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

The evidence is the **newest** `FailedAttachVolume` event still inside the API
server's event TTL (~1 h), which is not necessarily the attempt happening now.
What keeps a *resolved* attach failure from being reported is the pod's own
state, not the event's age: the finding is raised only while the pod is
currently not Ready and still at container creation, so a pod that progressed
past volume setup is never described by an old attach event. The residual case
is a pod stuck at `ContainerCreating` today for some other reason while an
attach event from an earlier incident is still in the cluster — there, the
newest event is quoted and the attribution is the older failure's.

### VolumeMountError

A pod stuck at container creation because a volume cannot be **mounted**.
`kubeagent` reads the kubelet's `FailedMount` Warning events. This is a
different failure from `VolumeAttachError`, which matches `FailedAttachVolume`
only: an attach failure is a storage problem, while the most common mount
failure has no storage in it at all.

That common case is a **ConfigMap or Secret named in the pod spec as a volume
source that does not exist**. It lands here rather than in
[CreateContainerConfigError](#createcontainerconfigerror), because the kubelet
reports a container config error only for a ConfigMap or Secret consumed
through `env`/`envFrom` — a *volume* source that cannot be resolved never
reaches container creation at all, so no container ever enters a waiting state
naming it. Without this detector such a pod carries no diagnosis anywhere.
`kubeagent` names that case specifically; for every other mount failure it says
only that the mount did not complete, which is all it knows.

Read-only: events are fetched with a single field-selected List. Note the
consequence of being event-based — Kubernetes expires events after ~1 h, so a
pod left stuck overnight loses this finding while staying just as stuck.

As with `VolumeAttachError`, the evidence is the newest matching `FailedMount`
event still inside that TTL rather than the mount attempt happening now, and the
guard against reporting a mount failure that has since been fixed is the pod's
own state — not Ready and still at container creation — rather than any age
check on the event.

### RestartLoop

A container that keeps exiting with a non-OOM error and restarting (≥ 3
restarts, current run younger than 10 min) even though it is currently
`Running`. This is the flapping case that `CrashLoopBackOff` misses — that
condition only fires while the container is in a `Waiting` / back-off state.
`kubeagent` reads `RestartCount` and `lastState.Terminated` from the pod
status. Read-only.

### ProbeFailure

A pod that is **Running but not Ready** because a container's **readiness**,
**liveness**, or **startup** probe keeps failing. `kubeagent` reads the kubelet's
`Unhealthy` events and names the probe, the container, and a plain-language reason
(`HTTP 503`, `connection refused`, `timed out`, `DNS lookup failed`,
`gRPC NOT_SERVING`, …) — for example `container "web": readiness probe failed —
HTTP 503`. It is complementary to `RestartLoop`/`CrashLoopBackOff`: a liveness probe
that restarts a container shows both the pattern and the probe as the cause.

When more than one probe on a container is failing at once, the finding names the
**heaviest** one rather than the most recent: liveness (the kubelet restarts the
container), then startup (it never finishes starting), then readiness (it is only
kept out of Service endpoints). The evidence line still lists every probe type
failing on that container — `container "web": liveness and readiness probes failed
— connection refused`. The comparison is bounded to a two-minute window ending at
the newest `Unhealthy` event, so a liveness probe that failed once an hour ago
cannot outrank a readiness probe that is failing now. The window is anchored on
that newest event rather than on the clock, which keeps the detector a pure
function of the objects it is handed.

To keep the failure reason safe for `--explain`, the raw probe message is never
surfaced — no pod IP and no `exec`-probe command output ever leaves the local
report. That is why an `exec` probe usually has no reason to show: its failure text
is the command's own output. `kubeagent` names the **handler kind** instead, read
from the pod spec's typed `exec`/`httpGet`/`tcpSocket`/`grpc` field rather than from
any message — `container "web": readiness probe failed — exec probe, output
withheld`. The command, the HTTP path and the port are not read.

Read-only: it lists `Unhealthy` events and reads the pod spec already collected (no
extra permission beyond the scan's existing event list).

### Init container failures

A pod stuck in its **init phase** because an init container is failing —
`Init:CrashLoopBackOff` (crash-looping), `Init:ImagePullBackOff` /
`Init:ErrImagePull` (its image can't be pulled), `Init:OOMKilled` (killed for
exceeding its memory limit), or `Init:CreateContainerConfigError` (a ConfigMap or
Secret it references is missing, or a required key is absent).
`kubeagent` reads `Status.InitContainerStatuses` — the
slice no other detector looks at — and names which init
container is failing, its position, and the reason — for a crash loop, `init container
"wait-for-db" (1/2), restartCount=6` (an image-pull, OOM or config failure shows the
kubelet's message or `exitCode` instead). Init containers run sequentially and block the
pod, so at most one is failing; a pod whose inits all succeeded is left to the
main-container detectors (no overlap). Read-only; reads pod status already collected
(no new RBAC).

### Job / CronJob failures

`scan` flags a batch workload whose run failed: a standalone **Job** with a `Failed`
condition (`BackoffLimitExceeded` — exhausted its retries; `DeadlineExceeded` — hit its
`activeDeadlineSeconds`), and a **CronJob** whose most-recent scheduled run failed. It
names the cause on the workload — e.g. `⚠ JobFailed: the Job failed — exhausted its
retries (BackoffLimitExceeded)`. A **failing CronJob is shown by default** (previously all
CronJobs were hidden without `--include-cron`; healthy ones still are). Only the *latest*
scheduled run's outcome is considered, so an older, already-superseded failure is not
re-flagged. Read-only; Jobs/CronJobs are already listed, so it needs no extra permission.

### FailedCreate (controller can't create pods)

A workload can sit below its desired replicas with **no pods at all** when its
controller is being denied pod *creation* — a `ResourceQuota` is exhausted, a
`LimitRange` rejects the pod's resources, or an admission webhook blocks it —
either by denying the pod (`rejected by an admission webhook`) or by not
answering at all, its backend down, mis-served or cert-expired
(`an admission webhook could not be reached`, which claims only that the API
server's call did not complete, not why). The
pod-level detectors see nothing (there is no pod), so the workload would
otherwise show only `0/N Degraded` with no cause. kubeagent reads the
controller's `FailedCreate` events and names the cause on the workload — e.g.
`⚠ FailedCreate: the controller cannot create pods — blocked by a ResourceQuota`,
with the admission message quoted beneath it as evidence. A Deployment's event
lands on its ReplicaSet and is resolved back to the Deployment; StatefulSets and
DaemonSets are matched directly.

**The cause and the quote are read from two different forms of the same
message, and they can disagree.** The cause is decided on the message exactly as
the API server stored it; the evidence line prints that message's *printable*
form, with control characters removed. The split is deliberate: matching on the
printable form would let a control character spliced mid-word — `exceeded
qu<NUL>ota` — slip past the signature it should have matched, so the match must
see the bytes. It follows that a message carrying such a character can be named
`forbidden by admission` while the line quoted below it reads `exceeded quota`,
because the byte that made the match fail is not one a terminal may be shown.
The named cause is the correct one. No controller writes messages like this;
reaching it takes a hand-written event.

"No pods at all" is the motivating case, not the rule: the check fires whenever a
workload has **fewer pods than it wants**, so a quota that allows one of two
replicas is named too — and there the `FailedCreate` finding appears **beside** the
pod-level finding for the one pod that did get created, because both causes are
real and fixing either alone leaves the workload broken. A workload that has all
its pods is never told its controller cannot create pods, even while a
`FailedCreate` event from before the fix is still in the cluster (Kubernetes keeps
events for about an hour).

Read-only, always-on, no new RBAC.

### CreateContainerConfigError

A **main container** that cannot start because a referenced ConfigMap or
Secret is **missing from the cluster**, or a **required key is absent** from an
existing object. Kubernetes surfaces this as a `CreateContainerConfigError`
**waiting state** on the container — the container never reaches `Running`.
`kubeagent` reads the kubelet's message directly from the pod's container status
(`containerStatuses[*].state.waiting.message`) and names the missing object, for
example: `container "worker": configmap "worker-config" not found`. The same
failure on an **init container** is reported as its own kind,
`Init:CreateContainerConfigError`, which additionally names that container's
position in the init sequence — see
[Init container failures](#init-container-failures). A ConfigMap or Secret the
pod mounts as a **volume** is a third case again and is reported as
[VolumeMountError](#volumemounterror) — the kubelet raises this waiting state
only for a reference consumed through `env`/`envFrom`. Unlike pod events (which
expire after ~1 h), the waiting state persists as long as the container is
stuck — read-only, no new RBAC.

### ContainerStartError

A **main container** whose image was resolved but which the kubelet **created
and could not start**. This is the catch-all of the waiting-state family, and it
exists because the alternative was silence: the kubelet emits many more waiting
reasons than kubeagent names individually, and a container stuck on one nobody
owned used to render as a failing workload with no `⚠` line under it at all.

It fires on a closed set of five reasons — `RunContainerError`,
`CreateContainerError`, `PreStartHookError`, `PostStartHookError` and
`StartError` — and quotes the kubelet verbatim as its evidence: `container
"web": RunContainerError: container init was OOM-killed (memory limit too
low?)`. That example is the motivating case. A container killed for exceeding
its memory limit *during startup* reports `RunContainerError` on the waiting
state and records the OOM only in `lastState.terminated.Reason=StartError`, so
[OOMKilled](#oomkilled) — which matches the reason `OOMKilled` — never sees it.

The finding says the container did not start and does not claim to know why,
which is why it is [medium confidence](#finding-confidence).

**In practice this catches a narrower set of failures than the five reasons
suggest**, and the difference is worth knowing before you go looking for it.
A container that is created and *then* fails — a lifecycle hook exiting
non-zero, an entrypoint that runs and dies — is restarted by the kubelet, and
within a second or two its waiting reason is `CrashLoopBackOff`, not one of the
five. Those land in [CrashLoopBackOff](#crashloopbackoff) instead, which is the
right answer for a container that keeps crashing and which carries the start
reason in its evidence — `last exit 128 (StartError)`. What reaches
`ContainerStartError` is a container the kubelet could not *create*, on input it
cannot satisfy however many times it retries: a `Localhost` seccomp profile
naming a file that is not on the node, a mount point that cannot be made, an
unhealthy container runtime. Those hold one of the five reasons indefinitely, at
`restartCount=0`, which is why a scan sees them.

Two deliberate limits. **Image-family reasons are not in the set** —
`InvalidImageName`, `ErrImageNeverPull`, `RegistryUnavailable` and
`SignatureValidationFailed` are pull failures and belong beside
[ImagePullBackOff](#imagepullbackoff-errimagepull); a container stuck on one of
those is not reported here. And a brand-new pod gets a **minute of grace**:
container creation is not instant, so the finding is raised only once the
container has already restarted at least once, or the pod is at least 60 s old.
The same failure on an **init container** is reported by
[Init container failures](#init-container-failures) instead, which names the
failing container's position in the sequence.

Read-only; reads pod status the scan already collects, so no new RBAC and no
extra cluster call.

### RolloutStuck (rollout wedged)

A **Deployment**, **StatefulSet** or **DaemonSet** whose rollout has stalled and
whose new pods are not becoming available.

For a **Deployment**, `kubeagent` checks two signals on the Deployment's
conditions:

- **`ProgressDeadlineExceeded`** — the `Progressing` condition has flipped to
  `status: False` with reason `ProgressDeadlineExceeded`, meaning the rollout did
  not finish within `spec.progressDeadlineSeconds`.
- **`ReplicaFailure`** — the ReplicaSet controller reports it cannot create the
  new pods (e.g. a quota or admission block), so the Deployment is wedged at the
  controller level.

A creation block produces both signals from one cause, and which one you see
depends on how long it has been going on: while the ReplicaSet's events are
still alive, [FailedCreate](#failedcreate-controller-cant-create-pods) names
the cause specifically and `RolloutStuck` stays silent; a Kubernetes event
expires after about an hour, and once they have aged out the `ReplicaFailure`
condition — which does not expire — is the evidence that remains. The same
cause, reported more specifically early and more generally later.

A **StatefulSet** and a **DaemonSet** publish no conditions at all, so their
counters are read instead:

- **StatefulSet, stuck update** — `updateRevision` differs from `currentRevision`
  and `updatedReplicas` is short of `spec.replicas`: the new revision is not
  reaching the replicas.
- **StatefulSet, stuck without an update** — the revisions match and
  `readyReplicas` is short of `spec.replicas`.
- **DaemonSet** — `numberReady` is short of `desiredNumberScheduled`. Whether
  `updatedNumberScheduled` has also fallen behind only decides which counters the
  evidence names.

Because a counter cannot say whether a rollout is wedged or merely young, those
two arms wait **600 seconds** before claiming anything — the controller itself
and every not-ready pod it owns must be older than that. The number is not
kubeagent's: it is Kubernetes' own default `spec.progressDeadlineSeconds` on a
Deployment, so all three kinds are exactly as patient as the Deployment arm
already was. It is not configurable.

The finding is surfaced **only when no pod-level detector already explains the
failure** — zero redundancy. It stays silent when a pod-level detector has fired
in that scan, **or** when a pod in the workload has restarted repeatedly (three
or more times, the same threshold `RestartLoop` uses).

The second half of that test is what makes the answer stable. A crash-looping
container is only in `Waiting` between restart attempts, so `CrashLoopBackOff`
matches on some scans and not others; a gate reading only the first half
reported `RolloutStuck` on one sample and `CrashLoopBackOff` on the next for the
same unchanged Deployment. The restart count is durable across the whole cycle.
`ImagePullBackOff` never had this problem — an unpullable image parks the
container in `Waiting` and keeps it there.

Read-only, always-on, no new flag, metric, or RBAC. Example output:

```text
✗ shop/api  Deployment  2/3 Degraded
    ⚠ RolloutStuck: the Deployment's rollout cannot complete — the new pods are not becoming available
      ↳ Progressing (ProgressDeadlineExceeded): ReplicaSet "api-7f9c" has timed out progressing.
✗ shop/db  StatefulSet  0/3 Degraded
    ⚠ RolloutStuck: the StatefulSet's rollout cannot complete — the new pods are not becoming available
      ↳ updatedReplicas 0/3, update revision pending
```

The evidence for a StatefulSet or a DaemonSet is only ever those counters. The
revision names are compared and never printed, and no pod, node or image is
named.

### ResourceQuota near-exhaustion

`scan` flags a namespace's ResourceQuota entry whose `used/hard` ratio is at or
over **90%** — catching a quota that is about to block new objects before the
controller starts emitting `FailedCreate` events. Every resource in every
ResourceQuota is evaluated generically (CPU, memory, pods, storage, …), except
an entry whose `hard` is **zero**, which is skipped: a zero quota is a
deliberate prohibition (`services.nodeports: "0"`, `count/secrets: "0"`) rather
than a capacity about to run out, and `0/0` has no ratio to compare against a
threshold. Two severity levels are distinguished:

- **exhausted** (`used >= hard`) — the quota is fully consumed; new objects are
  being **blocked right now**. A quota narrowed below its namespace's current
  usage reports over 100%.
- **near limit** (at or over the threshold, below 100%) — the quota is nearly
  full; a burst of new pods, requests, or storage claims will hit the wall. The
  floor of this band is the threshold below, not a fixed 90.

This is the **proactive** complement to the reactive `FailedCreate` detector:
`FailedCreate` fires after the controller is already being denied; the quota
check fires while there is still headroom to act.

The threshold defaults to `0.90` and is tunable via the environment variable
`KUBEAGENT_QUOTA_THRESHOLD` (e.g. `KUBEAGENT_QUOTA_THRESHOLD=0.80` to warn
earlier). It must be a fraction in `(0, 1]`: a value that is unparseable or out
of range — `80` for "80%" is the common slip — is refused **out loud**, with one
line on stderr naming the variable, the value received and the threshold
actually used, and the scan continues at the default. Read-only, always-on, no
CLI flag required. The daemon exposes the gauge
`kubeagent_resourcequota_issues`. Adds a `resourcequotas` read grant.

Example output:

```text
✗ shop/compute  ResourceQuota  requests.cpu
    ⚠ QuotaExhausted: used 4 / hard 4 (100%)
✗ shop/compute  ResourceQuota  pods
    ⚠ QuotaExhausted: used 8 / hard 4 (200%)
✗ web/compute  ResourceQuota  pods
    ⚠ QuotaNearLimit: used 47 / hard 50 (94%)
```

The middle line is a quota that was narrowed below what its namespace was
already using: it is being enforced now, and nothing new can be created until
usage drops under the new limit. The percentage is **floored**, never rounded
up, so `100%` on a `QuotaExhausted` line always means at or over the limit and a
quota at 99.9% reads `99%` beside its `QuotaNearLimit` label.

### Root-cause attribution

When a node is **hard-down** — `NotReady`, or Ready but its kubelet has stopped
heartbeating (a stale `Lease`) — every workload with a pod on it fails at once.
The stale-`Lease` half of that is the [node heartbeat
check](#node-heartbeat-freshness) and follows its `--node-heartbeat-threshold`:
setting the threshold to `0` turns the check off and, with it, every attribution
that depended on it. The `NotReady` half is unaffected and keeps attributing.

Instead of leaving those as disconnected findings, `scan` attributes each affected
workload to the node with a hedged `↳ likely caused by node <name> (<reason>)`
line, and rolls the count up on the attention line (`3 workloads failing (3 ⇐ node
worker-2)`). That naming form is used when every attribution in the report points
at the **same** cause; when two or more distinct causes are attributed, the rollup
counts them instead — `29 workloads failing (29 ⇐ 4 root causes)`, where the left
number is how many failing workloads were attributed and the right is how many
distinct causes they were attributed to. The workload's own findings still show —
attribution is additive, and
the wording is deliberately "likely" (correlation, not a hard causation claim).
Read-only, always-on, no new RBAC. Cordoned and node-pressure causes are not yet
attributed.

The same mechanism names a **shared registry** as the root cause: when two or
more workloads are failing image pulls (`ImagePullBackOff` / `ErrImagePull`)
whose images resolve to the same registry host, each is attributed
`↳ likely caused by registry <host> (<N> workloads failing to pull)` — the
signature of a registry outage, expired pull credentials, or rate limiting. A
single workload failing a pull is never blamed on the registry (that is usually a
typo'd image), and a workload already attributed to a hard-down node keeps the
node attribution. Docker Hub images (`nginx:...`) group under `docker.io`.

A **broken PersistentVolumeClaim** is joined the same way: when a workload's pod
mounts a PVC that the [Pending-PVC check](#pending-pvc-storage-provisioning) has
diagnosed as failing to provision or bind, the workload is attributed
`↳ likely caused by PVC <name> (MissingStorageClass)` — the parenthetical is the PVC's failure reason and can be `MissingStorageClass`, `NoMatchingPV`, `ProvisioningFailed`, or `FailedBinding` depending on what the cluster reports — connecting a pod stuck in
`Pending`/`ContainerCreating` to the storage cause kubeagent already reports. The
pod normally carries a finding of its own as well: one that cannot be scheduled
because its claim is unbound is reported `Unschedulable` at **high** confidence,
with the scheduler's `pod has unbound immediate PersistentVolumeClaims` as its
evidence. What the attribution adds is the *cause* — which claim, and why that
claim failed — not the first sign of trouble. Because the PVC is independently
diagnosed, a single affected workload is enough — unlike the registry case, this
is a join against evidence, not an inference. Node attribution still takes
precedence.

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
leave it unset. `not reported` is a whole-row state: it is shown only when **no**
node in the cluster reports `ephemeral-storage`. On a mixed cluster, where some
nodes report it and some do not, the line instead prints the ratio over the
reporting nodes, with the excluded count named beside it — `N of M nodes reserve
none  (K nodes do not report it)`. The
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
its backend Service, in one of three forms:

- **missing Service** — `backend Service api-svc not found`
- **no ready endpoints** (the usual cause of a 502/503) — `backend Service
  api-svc:8080 has no ready endpoints (likely 502/503)`. The `:8080` appears
  only when the Ingress's requested port actually resolves on the Service;
  when it does not, the Detail names the Service alone rather than
  misattributing a port the Service never exposed.
- **port not exposed** — `backend Service api-svc does not expose port 8080`

Only Service backends are checked (Resource backends are skipped), and routes
resolve within the Ingress's own namespace. It is read-only and advisory: it
appears in **NEEDS ATTENTION** and JSON `ingressIssues` but does not change
the cluster verdict.

When a broken route's backend Service has no ready endpoints, the Detail also
names *why* — the same root cause the [Service check](service-health.md) reports,
one hop up the graph:

- **the selector matches no pods** — `backend Service payments:80 has no ready
  endpoints (likely 502/503) — the selector matches no pods`
- **matching pods are on a down node** — `backend Service payments:80 has no
  ready endpoints (likely 502/503) — matching pods on down node worker-2
  (NotReady)`
- **matching pods exist but none are Ready** — `backend Service payments:80 has
  no ready endpoints (likely 502/503) — 3 matching pods, 0 ready`

This means the 502 is explained on the route itself — you do not have to cross-
reference the Service finding to understand why. The enrichment is read-only and
reuses the endpoint-cause logic from the Service check (no new flag, metric, or
RBAC).

A route whose backend Service is **intentionally empty** — the backing workload is scaled to zero (or a Job/CronJob between runs), or the Service is explicitly annotated `kubeagent.io/expected-empty: "true"` — is treated as **parked**: it moves to the quiet NOTES section instead of NEEDS ATTENTION, so a deliberately-idle app or an operator-managed role-split Service (e.g. a CloudNativePG `-ro` service on a single-instance cluster) does not read as a 502/503 outage. Set the annotation on the **Service** to silence a route (or the bare Service finding) kubeagent cannot infer is empty by design:

```yaml
metadata:
  annotations:
    kubeagent.io/expected-empty: "true"
```

### Pending PVC (storage provisioning)

`scan` flags a PersistentVolumeClaim stuck **Pending** and names a structural root cause
by correlating the PVC against the cluster's StorageClasses and PVs:

- **Missing StorageClass** — the PVC references a StorageClass that does not exist:
  `✗ shop/reports-data  PersistentVolumeClaim  Pending — references StorageClass "fast-ssd" which does not exist`
- **No matching PV** — for a static (non-dynamic) claim, no available PersistentVolume
  matches its request:
  `✗ shop/data-pvc  PersistentVolumeClaim  Pending — no available PersistentVolume matches its request (10Gi, ReadWriteOnce)`

These structural checks fire **even when no `ProvisioningFailed` event is present** — catching
a PVC that has been stuck long enough for its events to expire. When no structural cause is
found, kubeagent falls back to reading the PVC's `ProvisioningFailed` / `FailedBinding`
events for an event-based reason.

`WaitForFirstConsumer` PVCs that are simply waiting for a pod to consume them — which emit
no failure event and have a schedulable StorageClass — are never flagged. It is the
provision-time complement to `VolumeAttachError` (attach-time). It appears in **NEEDS
ATTENTION** and JSON `pvcIssues` but is advisory (it does not change the cluster verdict).
Read-only; correlates against collected StorageClasses and PVs (no new flag or metric).

### Node heartbeat freshness

Each node renews a `Lease` in `kube-node-lease` about every 10 seconds; the
control plane only marks a node `NotReady` after ~40 seconds of missed renewals.
`scan` reads those Leases and flags a node that still reads **Ready** but whose
lease has gone stale — `✗ node worker-2 kubelet not heartbeating (lease 48s
stale)` — so a crashed, hung, or partitioned kubelet shows up *before* the node
flips to `NotReady`. A node with no Lease at all (or one whose lease has never
been renewed) is flagged the same way, rendered `✗ node worker-2 no kubelet
lease` — that rendering does not consult the threshold, since there is no
renewal timestamp to measure staleness against. It degrades the cluster verdict, and
the threshold is tunable with `--node-heartbeat-threshold` (default `40s`;
`0` disables it). Compares against the scanner's clock, so run it in-cluster
(the watch daemon) or on a clock-synced host. The count of flagged nodes is
also exposed in JSON as `nodesStaleHeartbeat`.

### Expected-node list

`scan --expected-nodes node-a,node-b,…` declares the node names you
expect. kubeagent flags each declared node that has **no `Node` object** in the
cluster — `✗ node node-b expected but absent from the cluster` — which
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

### Control-plane health (opt-in)

`scan --control-plane-health` probes the apiserver `/readyz?verbose` endpoint
and flags a control plane that reports itself **not ready**. It covers
**apiserver and etcd** — those are the checks `/readyz` runs; scheduler and
controller-manager health is not covered.

It is **opt-in**: off by default because it costs an extra request per scan and
is advisory. Most clusters already allow it — Kubernetes grants `get /readyz`
to `system:authenticated` through the default `system:public-info-viewer` and
`system:discovery` roles — but kubeagent still ships the grant explicitly, for
clusters that have narrowed those defaults and so that `kubeagent rbac print`
names every path the feature reads. Apply it with the Helm value
`controlPlaneHealth.enabled=true` or `deploy/rbac-controlplane.yaml`. In the daemon, set
`KUBEAGENT_CONTROL_PLANE_HEALTH=true`; the gauge
`kubeagent_control_plane_unhealthy` is `1` when the check fires, `0`
otherwise.

The check is **advisory** — it appears in a `CONTROL PLANE` section and JSON
`controlPlane` but does not change the cluster verdict. Example output:

```text
CONTROL PLANE  (opt-in)
  ✗ control plane not ready
      ⚠ apiserver /readyz reported not ready
```

kubeagent reports *that* the control plane is not ready, not *which* readyz
check failed: the apiserver returns the per-check detail as `text/plain`, and
the Kubernetes client discards the body of a non-2xx response it cannot
decode. Run `kubectl get --raw '/readyz?verbose'` for the per-check list.

### DNS / CoreDNS resolution health (opt-in)

`scan --dns-health` probes each CoreDNS pod's `:9153/metrics` endpoint (via the
`pods/proxy` subresource) and flags an elevated **SERVFAIL+REFUSED response
ratio** — the sign that DNS is up but failing to resolve, a failure mode the
CoreDNS-pod health check misses entirely.

The check fires when the ratio of SERVFAIL + REFUSED responses to total responses
is at or above **5%** (the default; set `KUBEAGENT_DNS_SERVFAIL_RATIO` to tune)
over a minimum floor of **100 responses**. `KUBEAGENT_DNS_SERVFAIL_RATIO` takes
a fraction in `(0, 1]`, so the default `0.05` is 5%. Below the floor the ratio is
too noisy to be actionable and is skipped. Findings are aggregated across all
CoreDNS pods so a single ratio and count appear in the output.

It is **opt-in**: off by default because it requires the `pods/proxy` subresource
— a broader grant than kubeagent's usual `get`/`list`/`watch`. Enable the add-on
grant with the Helm value `dnsHealth.enabled=true` or by applying
`deploy/rbac-dnshealth.yaml`. In the daemon, set
`KUBEAGENT_DNS_HEALTH=true`; the gauge `kubeagent_dns_servfail_ratio` reports the
current ratio as a float.

The check is **advisory** — it appears in a `DNS` section and JSON `dns` but does
not change the cluster verdict. Example output:

```text
DNS  (opt-in)
  ✗ cluster DNS is failing to resolve
      ⚠ CoreDNS SERVFAIL+REFUSED ratio 12.3% (1234/10000 responses across 2 pods)
```

### Certificate expiry (opt-in)

`scan --certs` reads the cluster's `kubernetes.io/tls` Secrets and flags
certificates that are **expired** or expiring within the warn window
(`--cert-warn-days`, default 30) in an advisory `CERTIFICATES` section, with the
Ingress routes each certificate fronts. kubeagent's own code never reads,
prints, or stores the private key: only the **public** certificate
(`tls.crt`) is parsed, and only metadata (names and dates) is reported. That
is a property of kubeagent's code, not of what crosses the network —
`list secrets` is a whole-object read, so the API server returns `tls.key` in
the response body alongside `tls.crt`, and the grant this feature needs is
therefore the ability to receive private keys, not merely to receive
certificates (see [least-privilege RBAC](rbac.md#the-feature-table) for what
that costs). Off by default: without the flag kubeagent makes no Secrets API
calls at all. The in-cluster daemon needs the secrets add-on grant
(`deploy/rbac-certs.yaml` or Helm `certs.enabled=true`) and enables the check
with `KUBEAGENT_CERTS=true`.

The `Invalid` category names a `kubernetes.io/tls` Secret whose certificate
could not be parsed: `detail` is `empty tls.crt` when `tls.crt` is missing or
empty, `invalid certificate data` when every PEM block in it fails to parse —
so a malformed secret is reported rather than silently skipped, and a JSON
consumer can match on either `detail` value under the `invalid` key.

A refused `secrets` read is visibly not a clean result: the section prints
`certificates: secrets access denied — apply deploy/rbac-certs.yaml (or Helm
certs.enabled=true)` in place of any findings, and the same refusal also
names `secrets` in the [`BLIND SPOTS`](rbac.md#blind-spots) section — the
read is refused once and reported in both places.

Example output:

```text
CERTIFICATES  (advisory — public certificate metadata only)
  ✗ web/tls-example  EXPIRED 4d ago  (CN example.com)
      — fronts ingress web/example-ingress (example.com)
  ⚠ api/tls-internal  expires in 12d  (CN api.example.org)
  ⚠ payments/tls-broken  invalid certificate data
  · 3 certificates checked (warn window 30d)
```

### Next-step suggestions (opt-in)

`scan --suggest` prints a deterministic, reviewed next-step suggestion and a
read-only `kubectl` investigation command directly under each finding —
including workload-level findings such as `RolloutStuck`, `JobFailed` and
`FailedCreate`, which may have no pod row beneath them. It works **offline**
(no API key required) and is **read-only** — kubeagent prints the command, it
never runs it.

```text
✗ shop/web  Deployment  0/2 Degraded  · 8 restarts, last 1m ago
    image shop/web:1.4.2
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting
      ↳ container "web", restartCount=8, last exit 1 (Error), 1m7s ago
      ↳ next step: starts then crashes — inspect the crash output
      ↳ try: kubectl -n shop logs web-abc -c web --previous
```

Because a suggestion names the pod, `--suggest` also splits findings that would
otherwise be grouped as `×N` — see [Grouping identical
findings](#grouping-identical-findings) for why.

Each finding maps to a single focused next step — for example,
`CrashLoopBackOff` → check the previous logs; `ImagePullBackOff` → verify the
tag and credentials; `OOMKilled` → inspect the memory limits. The suggestions
are deterministic and never model-decided: no finding is paraphrased or
reordered by an LLM.

A kind without its own next step — `ContainerStartError` is the current
example — gets the generic `describe` command instead, as a property of the
design rather than an omission: its cause is not knowable from the issue name
alone, so the command that shows everything is the right one.

A pod can carry more than one finding, and each gets its own next step, printed
in finding order rather than in priority order — a container that both crash-loops
and is OOM-killed shows the crash step first.

This was the first **Theme C** (principled intelligence) slice — the
deterministic remediation core that `--explain` now ranks and phrases: the LLM
ranks and sequences these commands, and never invents or substitutes one. See
[Explaining findings](#explaining-findings-opt-in) for how that grounding works.

### Stuck-terminating resources

`scan` flags a resource wedged in `Terminating` — deletion pending longer than
two minutes by default (see `KUBEAGENT_TERMINATING_THRESHOLD`) — and names the
blocker: a **Namespace** stuck on a finalizer or a downstream condition
(`NamespaceDeletionContentFailure` / `NamespaceFinalizersRemaining` /
`NamespaceContentRemaining`, message sanitized and trimmed), a **Pod** stuck
past its grace period (its own finalizers, or the node named as NotReady or no
longer existing when node data resolves `spec.nodeName` — an unset name, no
node data, or a matched Ready node keeps the generic "deletion not confirmed"
fallback), or a **PVC** held by `pvc-protection` (cross-referenced to the pod
or pods still mounting it) or by any other finalizer. Read-only and advisory —
it never removes a finalizer (removing one is a deliberate manual act, outside
kubeagent's remediation allowlist) and never changes the cluster verdict. The
daemon exposes `kubeagent_resources_stuck_terminating`.

### PodDisruptionBudget-blocked drains

`scan` flags a PodDisruptionBudget that will block a node drain, covering three
categories:

- **unsatisfiable** — the budget requires more healthy pods than the workload has
  (e.g. `minAvailable: 3` covering only 3 replicas), so no voluntary eviction can
  ever be permitted; every `kubectl drain` will hang indefinitely.
- **stale** — the PDB's selector matches no pods (the workload was renamed,
  deleted, or the selector drifted), so the budget protects nothing but would
  still block drain attempts.
- **blocking** — the workload is already degraded (fewer healthy pods than the PDB
  demands), so `DisruptionsAllowed == 0` and the node cannot be drained until the
  workload heals.

Findings appear in **NEEDS ATTENTION** with the rule and the reason, e.g.:
`✗ shop/api-pdb  PodDisruptionBudget  minAvailable: 3` / `⚠ PDBBlocked: covers
all 3 pods — no voluntary eviction can ever proceed; every node drain will hang`.
Read-only and advisory — it does not change the cluster verdict. The daemon
exposes `kubeagent_pdb_blocking_issues`. Adds a base
`policy/poddisruptionbudgets` read grant.

### HPA-can't-scale

`scan` flags a HorizontalPodAutoscaler that is stuck and cannot scale as
intended, covering three categories:

- **unable** — the HPA's `AbleToScale` condition is `False`, meaning the
  controller can't act on the scale target at all (the target Deployment or
  StatefulSet is missing, or the `scale` subresource returned an error).
- **metrics** — the HPA's `ScalingActive` condition is `False`, meaning metric
  collection has failed (a custom or external metrics adapter is down, or the
  metrics server cannot return the resource metric), so the HPA's replica
  calculation is stuck.
- **capped** — the workload is pinned at `maxReplicas` while demand exceeds the
  cap (`TooManyReplicas` reason on `ScalingLimited`), so the autoscaler has run
  out of headroom and the workload is under-replicated.

Each finding names the HPA, its scale target, and the reason — for example:
`✗ shop/api-hpa  HorizontalPodAutoscaler  targets Deployment/api` /
`⚠ HPAStuck: can't fetch metrics — unable to get resource metric cpu: no
metrics returned`.

Read-only and advisory — it does not change the cluster verdict. The daemon
exposes `kubeagent_hpa_scaling_issues`. Adds a base
`autoscaling/horizontalpodautoscalers` read grant.

### Admission-webhook failure

`scan` flags a Validating or Mutating webhook whose `failurePolicy` is `Fail`
and whose backing Service is **missing** (`MissingService`) or **has no ready
endpoints** (`NoEndpoints`). Either condition means the webhook will reject
every `create`/`update` it intercepts — making the cluster effectively read-only
for the affected resource kinds without any obvious error at the workload level.

Two problems are detected:

- **MissingService** — the Service referenced in the webhook's `clientConfig`
  does not exist in the cluster.
- **NoEndpoints** — the Service exists but has no ready Pod endpoints behind it.

The check only flags webhooks under `failurePolicy: Fail` (the default in
`admissionregistration.k8s.io/v1` when the field is omitted). Webhooks with
`failurePolicy: Ignore` are skipped — if their backend is down the API server
falls through silently, which is by design.

The check is **cluster-wide only**: it is skipped when `--namespace`/`-n` is
set, because the check cross-references each webhook's backend Service against
the collected Services — a namespace-scoped scan only collects that namespace's
Services, so a backend in any other namespace would appear missing and produce
false positives.

Findings appear in **NEEDS ATTENTION** with the configuration name, kind, and
the webhook name, followed by the reason — for example:

```text
✗ policy-webhook  ValidatingWebhookConfiguration  webhook validate.policy.io
    ⚠ NoEndpoints: backend Service kube-system/policy-svc has no ready
      endpoints — failurePolicy Fail rejects every intercepted create/update
```

Read-only and advisory — it never changes the cluster verdict. The daemon
exposes `kubeagent_admission_webhooks_failing` (backend failures only). Adds a
base `admissionregistration.k8s.io` read grant.

### Admission-webhook latency risk

`scan` also flags a Validating or Mutating webhook whose `failurePolicy` is
`Fail` and whose `timeoutSeconds` is **≥ 15** — a latency landmine. Under
`failurePolicy: Fail`, a slow webhook blocks every `create`/`update` it
intercepts for up to that many seconds, then rejects it; the result is a
cluster that appears to accept traffic but silently stalls and then errors on
every affected operation.

The threshold defaults to 15 and is tunable via the environment variable
`KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS` (e.g.
`KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS=10` to warn earlier) or the Helm value
`webhookLatency.timeoutThreshold`. Webhooks with `failurePolicy: Ignore` and
those with a `nil` (unset) `timeoutSeconds` are never flagged.

Valid values are 1–30. The API server refuses a webhook `timeoutSeconds` above
30 ("the timeout value must be between 1 and 30 seconds"), so a threshold above
30 could only ever match nothing; kubeagent refuses it rather than reporting a
clean posture.

The check is **always-on**, **cluster-wide only** (skipped under
`--namespace`), and **advisory** — it does not change the cluster verdict. The
daemon exposes `kubeagent_admission_webhook_latency_risks`. No new RBAC is
required (reuses the `admissionregistration.k8s.io` grant above). Example
output:

```text
WEBHOOK
  ✗ slow-validator  ValidatingWebhookConfiguration  webhook policy.example.com
      ⚠ HighTimeout: timeoutSeconds 30 ≥ 15s under failurePolicy Fail — a slow webhook blocks every intercepted create/update for up to 30s, then rejects it
```

### Security posture (opt-in)

`scan --security` walks every running pod and each Service and flags
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
`kube-system`/`kube-node-lease`/`kube-public` unless you target one with `-n`;
those three are the only namespaces skipped, and an addon namespace is
scanned like any other. A workload with no running pods — a Deployment
scaled to zero, a CronJob that has not yet fired — is not examined even when
its pod template is unsafe.

### Crash log root-cause (opt-in)

`scan --logs` fetches each crashing container's `--previous` logs (the instance
that just exited) and classifies the failure line into a plain-language cause shown
directly under the finding:

```text
      logs (previous container):
        panic: runtime error: index out of range
        → application panic (code bug)
```

kubeagent reads the **last 25 lines** of the previous instance and shows one line,
truncated at 200 characters. A signature older than those 25 lines is not seen, and
the finding falls back to the last line instead.

Recognised signatures are:

- `application panic (code bug)` — a Go/Python/JVM panic or unhandled exception
- `bad command or entrypoint` — the container's own output contains an `exec:` line, which is what a shell entrypoint prints when the command it wraps is missing or not executable. A container whose entrypoint is missing outright never starts and writes no log; that case is reported from the kubelet's message by the container-start detector, with no log block.
- `cannot reach a dependency — connection refused` — a dependency is not up yet, or the address is wrong
- `DNS resolution failed (name lookup)` — a name did not resolve
- `ran out of memory in-process` — the process hit an allocation failure (distinct from a kernel OOM-kill, which the `OOMKilled` detector reports)
- `configuration parse/validation error` — malformed YAML/JSON, a failed unmarshal, or an invalid config on startup
- `port already in use` — the port the process binds is taken
- `authentication/authorization failure to a dependency` — credentials rejected
- `permission denied — check securityContext / file permissions` — a file or device the process needs is not readable/writable as the container's user

When no signature matches, the last non-empty line is shown with the cause
`last output before exit (no signature in the last 25 lines)` — kubeagent found
the log but recognised nothing in it.

Every finding that names a container **and** whose container has a previous
instance is probed — this includes **CrashLoopBackOff**, **RestartLoop**,
**OOMKilled** and **Init:CrashLoopBackOff**. Findings that name no container
(ImagePullBackOff, Pending, the cluster-level checks) are never probed, and
neither is a container that has not yet restarted.

It is **read-only** and **opt-in**. `--logs` is available on `scan` and on
`kubeagent mcp`, and is not available in the `watch` daemon or in `gate`.
Running it in-cluster requires the `pods/log` RBAC add-on
(`deploy/rbac-logs.yaml`); most human kubeconfigs already allow `pods/log`. Without
the grant, `--logs` reports no log cause and continues non-fatally.

`--explain` receives **only** the derived cause (`logCause`) — never the raw log text
(`logExcerpt`) — so no container output is sent to the Claude API.

### Finding confidence

Every finding carries a **confidence** level reflecting how directly the observed
signal implies the diagnosis: **high** when Kubernetes itself asserts the state
(CrashLoopBackOff, OOMKilled, Unschedulable, a controller event, …) and
**medium** for a kubeagent heuristic (`RestartLoop`, `ProbeFailure`), a finding
that reports a failure without claiming to know its cause
(`ContainerStartError`), or an inference from counters rather than a
controller-set condition (a StatefulSet or DaemonSet's `RolloutStuck` finding —
the same issue string is high on a Deployment, where the controller sets the
condition itself). A correlation hint's own medium level is a different case,
explained in the next paragraph rather than here. High is the unmarked
default; the text report tags only the less-certain findings and hints
(`⚠ RestartLoop [medium]: …`, `↳ likely caused by registry … [medium]`) so the
tag draws the eye to exactly what to second-guess. `scan --output json`
carries `"confidence"` on every finding; the gate's JSON and the watch
daemon's `/issues` publish a narrower finding record that omits it.

The tag on a **hint** works differently from the one on a finding, and only in the
text report. A root-cause attribution is not a finding and has no `confidence`
field: JSON carries it as the plain string `"rootCause"`. The `[medium]` the text
report prints beside one is derived from the *kind* of cause — node and PVC
attributions are high (and so print unmarked), a shared-registry attribution is
medium — so a JSON consumer reads the level from the cause type rather than from a
field. The HTML report shows neither tag, on a finding or a hint.

A `--baseline` deviation is a third case: its confidence is stated once, in
the `BASELINE DEVIATIONS` heading, rather than per row, and it carries no
`confidence` field in any JSON document — neither `baseline.Deviation`, which
has none, nor the `findings.Finding`s `FromBaseline` produces. See
[Baseline](baseline.md).

Confidence is informational — it never changes a
finding's priority or the cluster verdict.

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

### Grouping identical findings

Findings on the same workload that would print the same lines are collapsed into
one block with a count, so a twenty-replica Deployment whose replicas all crash
the same way prints one `⚠` line reading `×20` rather than twenty identical
ones:

```text
✗ shop/web  Deployment  0/20 Degraded
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting ×20
      ↳ container "web", restartCount=4
```

A collapse drops no distinct `↳` signal: each one is printed on its own line,
and the `×N` on the head is the number of findings the block stands for. What
it does not render is how many findings sit behind each individual `↳` line —
three pods reporting one signal and one pod reporting another print as `×4`
above two lines. It does print fewer lines than it would uncollapsed — that is
the point — so read the count, not the line count, when you want to know how
many pods are affected. Anything that differs between pods keeps them apart — a
`--suggest` command names the pod, a resources block names that container's
limits — and the one part allowed to vary inside a block is the `↳` signal,
where every distinct value is printed on its own line. That is what shows the
restart counts when they differ:

```text
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting ×2
      ↳ container "web", restartCount=1
      ↳ container "web", restartCount=7
```

This is a text-rendering decision only. `--output json` still carries one
finding per pod, each naming its own pod, and no count appears in it.

A `↳` signal is capped at 500 characters in the text output — the line itself
measures 508, the extra eight being the indent and the `↳` marker — ending in
`… (truncated)` when the cap bites. A container runtime repeats every layer of a
failure — the back-off preamble, the rpc error, the unpack failure, the resolve
failure, the bare image reference — and on a long registry path that line runs
past the screen. Real ones measure a few hundred characters and arrive whole;
the cap is set where it bites only on the pathological. The cut is on
characters, not bytes, so a multi-byte character is never split, and it is
marked because a silently shortened error reads as the whole error.
`--output json` is not subject to the 500-character text cap: it carries the
evidence as kubeagent stored it. That is still not the runtime's whole
message: the untrusted text inside it passed through a 512-character limit on
the way in, ending in `…` when it bites, and kubeagent's own framing — the
container name, for instance — sits around that. So an unusually long
container-runtime message is shortened in every kubeagent surface. When the
JSON evidence ends in `…` and you need the rest, `kubectl -n <ns> describe pod
<pod>` has the untruncated original.

### Agentic investigation (`--investigate`)

`kubeagent scan --investigate` runs the full scan, then — for each finding —
launches a bounded, read-only, model-driven tool-use loop. The model can
describe a flagged object, list its events, and hop to related resources
(owner Deployment, node, PVC) to chase the root cause across the finding's
resource graph. When the loop concludes it emits an **Investigation** section:
an evidence trail (`consulted: ...` line) followed by a grounded Fix-first
narrative. The **commands** are kubeagent's deterministic, pre-reviewed ones,
and the narrative around them — the ranking, the sequencing, and any remedial
step described in prose — is the model's and is **not** pre-reviewed.

```bash
export ANTHROPIC_API_KEY=sk-ant-...
./kubeagent scan --investigate
```

Sample output for a CrashLoopBackOff pod:

```text
── Investigation ──
consulted: describe pod shop/api-7f9c-xr2kp · events shop/api-7f9c-xr2kp · related shop/api-7f9c-xr2kp→node
(model-generated; verify commands before running)
Fix first: the pod's readiness probe fails every attempt on the same GET
/healthz timeout, and the node it is scheduled on shows no other pod
failing the same way, which rules out a node-level cause. Raise the
container's readinessProbe.timeoutSeconds or find why /healthz is slow to
answer.
```

**Reachable scope:**

- The set of objects the loop may read is seeded from the flagged workloads
  **alone** — each workload named in a finding, its pods, and each pod's
  node. A run triggered only by a Service issue, or only by a Degraded
  cluster verdict, begins with nothing reachable: the workload seed is
  empty, so every tool call the model attempts on such a run is refused by
  the closure guard.
- Reachability follows the object's **kind**, and the three tools gate
  differently. `describe` reads structured status only for `pod`,
  `deployment`, `replicaset`, `statefulset`, `daemonset`, `job`, `node` and
  `pvc`, and only when the scope already holds that object; any other kind is
  refused. `get_related` sources only from an in-scope pod and grows the set
  by `owner`, `node` or `pvc`. `get_events` is the exception: it gates on
  namespace and name **alone**, ignoring kind, and queries the API by
  `involvedObject.name` — so events for an object of any kind come back once
  something of that namespace and name is in scope.
- Applied to the object-level finding families a scan can flag: a
  PersistentVolumeClaim finding is describable, one hop from an in-scope pod
  that mounts the claim. Stuck-terminating is three kinds rather than one — a
  Namespace, a Pod or a PersistentVolumeClaim — and its Pod and
  PersistentVolumeClaim arms use those same paths. PodDisruptionBudget,
  HorizontalPodAutoscaler, admission webhook, ResourceQuota, ingress route
  and stuck-terminating's Namespace arm name kinds `describe` does not
  accept, so their structured status is out of reach; their events are not,
  whenever the object shares a namespace and name with something already in
  scope — which is ordinary for an HPA, a PDB or an Ingress named after the
  workload or Service it targets.
- A scan with no workload findings, no Service findings, and a cluster
  verdict that is not Degraded runs no investigation at all — the report
  says so, printing `Investigation skipped — no workload findings, no
  service findings, and the cluster verdict is not Degraded.` under the
  `── Investigation ──` heading instead of staying silent.

**Constraints and requirements:**

- **Anthropic-only** — requires `ANTHROPIC_API_KEY`. Tool-use is not available
  through the local-model path (`KUBEAGENT_EXPLAIN_ENDPOINT`); if only that
  endpoint is set, `--investigate` errors clearly.
- **Supersedes `--explain`** — `--investigate` is the agentic superset.
  Running both flags is unnecessary; `--investigate` includes the grounded
  narrative that `--explain` provides, plus the follow-up reads. When both
  flags are passed, `--investigate` runs and `--explain` is silently ignored.
- **Capped** — the loop is bounded per finding: at most **8 reads** and **6
  turns**, so the total API cost is predictable and the scan remains fast.
- **No logs** — `--investigate` does not fetch container logs. It uses
  structured Kubernetes object reads only (describe / events / get), so no
  raw container output leaves the process.
- **Structured-only egress** — only object metadata, conditions, and events
  are sent to the model. No pod specs, env values, or secrets.
- **Never writes** — all tool calls are `get`/`list` only. The read-only
  invariant is not relaxed.
- **Model selection** — reuses `--model` / `KUBEAGENT_MODEL` (default
  `claude-opus-4-8`).

## Status

`kubeagent scan` performs a read-only, whole-cluster scan, in text or JSON.
The `###` sections on this page are the inventory of what it checks;
`kubeagent known-issues` is the same inventory as a command, machine-checked
against the detector set.

The optional `--suggest` flag prints a deterministic next-step suggestion and
a read-only `kubectl` investigation command under each finding — offline, no
API key required.

### Explaining findings (opt-in)

The optional `--explain` flag makes a single API call to summarize findings
in plain English. The explanation now **opens with a `Fix first:` ranked
remediation list** — cluster/kube-system problems (P1) before workload issues
(P2), most-blocking first — and each per-issue Fix is **grounded on
kubeagent's deterministic, pre-reviewed `--suggest` command**: the
**commands** are kubeagent's deterministic, pre-reviewed ones, and the
narrative around them — the ranking, the sequencing, and any remedial step
described in prose — is the model's and is **not** pre-reviewed. The
deterministic offline core (`scan`, `--suggest`) is unchanged; `--explain`
remains opt-in. The Fix command sent to the model has its pod name replaced
with `<pod>` — a generated per-replica name identifies one instance rather
than explaining the workload, the same reason kubeagent does not render pod
rows — so a rendered `--explain` Fix line may read `kubectl -n shop describe
pod <pod>` rather than the real name; run `--suggest` alongside `--explain`
for the real command.

By default `--explain` calls the Claude API and requires `ANTHROPIC_API_KEY`.
To run **fully offline / on-network** against a local model instead, set
`KUBEAGENT_EXPLAIN_ENDPOINT` to any OpenAI-compatible `/chat/completions` base
URL — for example:

```bash
# Ollama (no key needed)
export KUBEAGENT_EXPLAIN_ENDPOINT=http://localhost:11434/v1
./kubeagent scan --explain --model llama3.1

# vLLM / llama.cpp / LM Studio
export KUBEAGENT_EXPLAIN_ENDPOINT=http://localhost:8000/v1
./kubeagent scan --explain --model mistral-7b
```

When `KUBEAGENT_EXPLAIN_ENDPOINT` is set, `ANTHROPIC_API_KEY` is not required
and nothing leaves the network. `--model` / `KUBEAGENT_MODEL` names the local
model (required for the local path). `KUBEAGENT_EXPLAIN_API_KEY` is an optional
bearer token for endpoints that require authentication (local Ollama needs none).
The prompt, the ranked `Fix first:` output, and the offline scan core are
unchanged.

## Example output

```text
NEEDS ATTENTION
✗ shop/web  Deployment  0/1 Degraded
    ⚠ ImagePullBackOff: Bad image reference or registry authentication
    ↳ changed: rollout to revision 6, 4d ago · image nginx:1.27 → nginx:bad
✗ shop/cart  Deployment  1/2 Degraded
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting
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

Revision 1 gets no line. That revision is the Deployment's creation, not a
change from anything, so reporting it would be the one case where `changed:`
named no change — and its absence is what lets the line's presence mean
something. The gate is the revision number, not the survival of an older
ReplicaSet: a Deployment at revision 6 whose earlier ReplicaSets have been
garbage-collected still gets its line, without the image delta. The rule holds
for `--output json` too, where the `rollout` key is simply absent — it is an
optional key, already absent for workloads that are not flagged Deployments and
for rollouts older than the window, and that is a different absence from a
healthy-quiet workload's: `inventory.Prioritize` drops those from the
`workloads` array entirely, so a workload can be in the array without a
`rollout` key, or missing from the array altogether, and the two mean
different things.
