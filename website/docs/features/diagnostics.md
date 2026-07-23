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

### ProbeFailure

A pod that is **Running but not Ready** because a container's **readiness**,
**liveness**, or **startup** probe keeps failing. `kubeagent` reads the kubelet's
`Unhealthy` events and names the probe, the container, and a plain-language reason
(`HTTP 503`, `connection refused`, `timed out`, `DNS lookup failed`,
`gRPC NOT_SERVING`, …) — for example `container "web": readiness probe failed —
HTTP 503`. It is complementary to `RestartLoop`/`CrashLoopBackOff`: a liveness probe
that restarts a container shows both the pattern and the probe as the cause. To keep
the failure reason safe for `--explain`, the raw probe message is never surfaced — no
pod IP and no `exec`-probe command output ever leaves the local report. Read-only: it
lists `Unhealthy` events (no extra permission beyond the scan's existing event list).

### Init container failures

A pod stuck in its **init phase** because an init container is failing —
`Init:CrashLoopBackOff` (crash-looping), `Init:ImagePullBackOff` /
`Init:ErrImagePull` (its image can't be pulled), or `Init:OOMKilled` (killed for
exceeding its memory limit). `kubeagent` reads `Status.InitContainerStatuses` — the
slice the main-container crash detectors don't look at — and names which init
container is failing, its position, and the reason — for a crash loop, `init container
"wait-for-db" (1/2), restartCount=6` (an image-pull or OOM failure shows the pull
message or `exitCode` instead). Init containers run sequentially and block the
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
`LimitRange` rejects the pod's resources, or an admission webhook blocks it. The
pod-level detectors see nothing (there is no pod), so the workload would
otherwise show only `0/N Degraded` with no cause. kubeagent reads the
controller's `FailedCreate` events and names the cause on the workload — e.g.
`⚠ FailedCreate: the controller cannot create pods — blocked by a ResourceQuota`,
with the raw admission message as evidence. A Deployment's event lands on its
ReplicaSet and is resolved back to the Deployment; StatefulSets and DaemonSets
are matched directly. Read-only, always-on, no new RBAC.

### CreateContainerConfigError

A container (main or init) that cannot start because a referenced ConfigMap or
Secret is **missing from the cluster**, or a **required key is absent** from an
existing object. Kubernetes surfaces this as a `CreateContainerConfigError`
**waiting state** on the container — the container never reaches `Running`.
`kubeagent` reads the kubelet's message directly from the pod's container status
(`containerStatuses[*].state.waiting.message`) and names the missing object, for
example: `container "worker": configmap "worker-config" not found`. It covers
main and init containers. Unlike pod events (which expire after ~1 h), the
waiting state persists as long as the container is stuck — read-only, no new
RBAC.

### RolloutStuck (Deployment rollout wedged)

A **Deployment** whose rollout has stalled and the new pods are not becoming
available. `kubeagent` checks two signals on the Deployment's conditions:

- **`ProgressDeadlineExceeded`** — the `Progressing` condition has flipped to
  `status: False` with reason `ProgressDeadlineExceeded`, meaning the rollout did
  not finish within `spec.progressDeadlineSeconds`.
- **`ReplicaFailure`** — the ReplicaSet controller reports it cannot create the
  new pods (e.g. a quota or admission block), so the Deployment is wedged at the
  controller level.

The finding is surfaced **only when no pod-level detector already explains the
failure** — zero redundancy. If `ImagePullBackOff` or `CrashLoopBackOff` already
names the cause on the pods, `RolloutStuck` stays silent.

Read-only, always-on, no new flag, metric, or RBAC. Example output:

```text
✗ shop/api  Deployment  2/3 Degraded
    ⚠ RolloutStuck: the Deployment's rollout cannot complete — the new pods are not becoming available
      ↳ Progressing (ProgressDeadlineExceeded): ReplicaSet "api-7f9c" has timed out progressing.
```

### ResourceQuota near-exhaustion

`scan` flags a namespace's ResourceQuota entry whose `used/hard` ratio is at or
over **90%** — catching a quota that is about to block new objects before the
controller starts emitting `FailedCreate` events. Every resource in every
ResourceQuota is evaluated generically (CPU, memory, pods, storage, …). Two
severity levels are distinguished:

- **exhausted** (`used == hard`, 100%) — the quota is fully consumed; new
  objects are being **blocked right now**.
- **near limit** (90–99%) — the quota is nearly full; a burst of new pods,
  requests, or storage claims will hit the wall.

This is the **proactive** complement to the reactive `FailedCreate` detector:
`FailedCreate` fires after the controller is already being denied; the quota
check fires while there is still headroom to act.

The threshold defaults to `0.90` and is tunable via the environment variable
`KUBEAGENT_QUOTA_THRESHOLD` (e.g. `KUBEAGENT_QUOTA_THRESHOLD=0.80` to warn
earlier). Read-only, always-on, no CLI flag required. The daemon exposes the
gauge `kubeagent_resourcequota_issues`. Adds a `resourcequotas` read grant.

Example output:

```text
✗ shop/compute  ResourceQuota  requests.cpu
    ⚠ QuotaExhausted: used 4 / hard 4 (100%)
✗ web/compute  ResourceQuota  pods
    ⚠ QuotaNearLimit: used 47 / hard 50 (94%)
```

### Root-cause attribution

When a node is **hard-down** — `NotReady`, or Ready but its kubelet has stopped
heartbeating (a stale `Lease`) — every workload with a pod on it fails at once.
Instead of leaving those as disconnected findings, `scan` attributes each affected
workload to the node with a hedged `↳ likely caused by node <name> (<reason>)`
line, and rolls the count up on the attention line (`3 workloads failing (3 ⇐ node
worker-2)`). The workload's own findings still show — attribution is additive, and
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
`Pending`/`ContainerCreating` (which has no pod-level finding of its own) to the
storage cause kubeagent already reports. Because the PVC is independently
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

### Control-plane health (opt-in)

`scan --control-plane-health` probes the apiserver `/readyz?verbose` endpoint
and flags an unhealthy control plane — naming the individual checks that are
failing (etcd, admission/controller poststarthooks, informer-sync). It covers
**apiserver and etcd**; scheduler/controller-manager health is a documented
follow-on.

It is **opt-in**: off by default because it requires a `/readyz` grant not
included in the base RBAC profile. Enable the add-on grant with the Helm value
`controlPlaneHealth.enabled=true` or by applying
`deploy/rbac-controlplane.yaml`. In the daemon, set
`KUBEAGENT_CONTROL_PLANE_HEALTH=true`; the gauge
`kubeagent_control_plane_unhealthy` is `1` when the check fires, `0`
otherwise.

The check is **advisory** — it appears in a `CONTROL PLANE` section and JSON
`controlPlane` but does not change the cluster verdict. Example output:

```text
CONTROL PLANE  (opt-in)
  ✗ control plane not ready
      ⚠ 2 checks failing: etcd, poststarthook/start-kube-apiserver-admission-initializer
```

### DNS / CoreDNS resolution health (opt-in)

`scan --dns-health` probes each CoreDNS pod's `:9153/metrics` endpoint (via the
`pods/proxy` subresource) and flags an elevated **SERVFAIL+REFUSED response
ratio** — the sign that DNS is up but failing to resolve, a failure mode the
CoreDNS-pod health check misses entirely.

The check fires when the ratio of SERVFAIL + REFUSED responses to total responses
is at or above **5%** (the default; set `KUBEAGENT_DNS_SERVFAIL_RATIO` to tune)
over a minimum floor of **100 responses**. Below the floor the ratio is too
noisy to be actionable and is skipped. Findings are aggregated across all CoreDNS
pods so a single ratio and count appear in the output.

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
Ingress routes each certificate fronts. Privacy by construction: only the
**public** certificate (`tls.crt`) is parsed — the private key is never read —
and only metadata (names and dates) is reported. Off by default: without the
flag kubeagent makes no Secrets API calls at all. The in-cluster daemon needs
the secrets add-on grant (`deploy/rbac-certs.yaml` or Helm
`certs.enabled=true`) and enables the check with `KUBEAGENT_CERTS=true`.

### Next-step suggestions (opt-in)

`scan --suggest` prints a deterministic, reviewed next-step suggestion and a
read-only `kubectl` investigation command directly under each pod finding.
It works **offline** (no API key required) and is **read-only** — kubeagent
prints the command, it never runs it.

```text
✗ shop/web  Deployment  0/2 Degraded
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting
      ↳ container "web": restartCount=8
      ↳ next step: starts then crashes — inspect the crash output
      ↳ try: kubectl -n shop logs web-abc -c web --previous
```

Each finding maps to a single focused next step — for example,
`CrashLoopBackOff` → check the previous logs; `ImagePullBackOff` → verify the
tag and credentials; `OOMKilled` → inspect the memory limits. The suggestions
are deterministic and never model-decided: no finding is paraphrased or
reordered by an LLM.

This is the first **Theme C** (principled intelligence) slice — the
deterministic remediation core that a later slice will hand to `--explain` for
LLM ranking and phrasing (the LLM ranks; it never invents the remediation).

### Stuck-terminating resources

`scan` flags a resource wedged in `Terminating` — deletion pending longer than
two minutes — and names the blocker: a **Namespace** stuck on a finalizer or a
downstream condition (`NamespaceFinalizersRemaining` / `NamespaceContentRemaining`
/ `NamespaceDeletionContentFailure`, message shown as-is), a **Pod** stuck past
its grace period (its finalizers, or "deletion not confirmed" when the node is
gone), or a **PVC** held by `pvc-protection` (cross-referenced to a pod still
mounting it). Read-only and advisory — it never removes a finalizer (that is a
`--fix` concern) and never changes the cluster verdict. The daemon exposes
`kubeagent_resources_stuck_terminating`.

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
and whose backing Service is **missing** (`missing-service`) or **has no ready
endpoints** (`no-endpoints`). Either condition means the webhook will reject
every `create`/`update` it intercepts — making the cluster effectively read-only
for the affected resource kinds without any obvious error at the workload level.

Two problems are detected:

- **missing-service** — the Service referenced in the webhook's `clientConfig`
  does not exist in the cluster.
- **no-endpoints** — the Service exists but has no ready Pod endpoints behind it.

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
    ⚠ WebhookDown: backend Service kube-system/policy-svc has no ready
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

The check is **always-on**, **cluster-wide only** (skipped under
`--namespace`), and **advisory** — it does not change the cluster verdict. The
daemon exposes `kubeagent_admission_webhook_latency_risks`. No new RBAC is
required (reuses the `admissionregistration.k8s.io` grant above). Example
output:

```text
WEBHOOK
  ✗ slow-validator  ValidatingWebhookConfiguration  webhook policy.example.com
      ⚠ WebhookSlow: timeoutSeconds 30 ≥ 15s under failurePolicy Fail — a slow webhook blocks every intercepted create/update for up to 30s, then rejects it
```

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

### Crash log root-cause (opt-in)

`scan --logs` fetches each crashing container's `--previous` logs (the instance
that just exited) and classifies the failure line into a plain-language cause shown
directly under the finding:

```text
      logs (previous container):
        panic: runtime error: index out of range
        → application panic (code bug)
```

Recognised signatures include:

- `application panic (code bug)` — a Go/Python/JVM panic or unhandled exception
- `cannot reach a dependency (…) — connection refused` — a dependency is not up yet, or the address is wrong
- `bad command or entrypoint` — the container command / entrypoint does not exist in the image
- `ran out of memory in-process` — the process hit an allocation failure (distinct from a kernel OOM-kill, which the `OOMKilled` detector reports)
- `configuration parse/validation error` — malformed YAML/JSON, a failed unmarshal, or an invalid config on startup

Only the crash findings (**CrashLoopBackOff**, **RestartLoop**, **OOMKilled**) are
probed — `--logs` is a no-op for ImagePullBackOff, Pending, and other non-crash
detectors.

It is **read-only**, **opt-in**, and **scan-only** (not available in the `watch`
daemon). Running it in-cluster requires the `pods/log` RBAC add-on
(`deploy/rbac-logs.yaml`); most human kubeconfigs already allow `pods/log`. Without
the grant, `--logs` reports no log cause and continues non-fatally.

`--explain` receives **only** the derived cause (`logCause`) — never the raw log text
(`logExcerpt`) — so no container output is sent to the Claude API.

### Finding confidence

Every finding carries a **confidence** level reflecting how directly the observed
signal implies the diagnosis: **high** when Kubernetes itself asserts the state
(CrashLoopBackOff, OOMKilled, Unschedulable, a controller event, …) and
**medium** for a kubeagent heuristic (`RestartLoop`, `ProbeFailure`) or a
statistical correlation (a shared-registry attribution). High is the unmarked
default; the text report tags only the less-certain findings and hints
(`⚠ RestartLoop [medium]: …`, `↳ likely caused by registry … [medium]`) so the
tag draws the eye to exactly what to second-guess. JSON always carries
`"confidence"` on every finding. Confidence is informational — it never changes a
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

## Status

`kubeagent scan` performs a read-only, whole-cluster scan and reports
CrashLoopBackOff, ImagePullBackOff/ErrImagePull, OOMKilled,
Pending/Unschedulable, VolumeAttachError (Multi-Attach), RestartLoop, ProbeFailure, init-container failures, failed Jobs/CronJobs, controllers that cannot create pods (FailedCreate), containers blocked by a missing ConfigMap or Secret (CreateContainerConfigError), and Deployments whose rollout has wedged (RolloutStuck), in text or JSON.

The optional `--suggest` flag prints a deterministic next-step suggestion and
a read-only `kubectl` investigation command under each finding — offline, no
API key required.

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
