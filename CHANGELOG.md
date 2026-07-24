# Changelog

All notable changes to kubeagent are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.52.0] - 2026-07-24

### Added

- **`--fix` audit log.** A new `--audit-log <path>` flag (with `--fix`) appends a
  durable, append-only JSON-Lines record of every remediation outcome — one line per
  action with its timestamp, target, previewed changes, and disposition
  (`dry-run` / `declined` / `applied` / `refused` / `error`). Secret-free by
  construction (only the previewed diff values and result detail are recorded); the
  file is opened `0o600` and append-only, and an unwritable path fails before any
  write. The accountability half of the remediation contract.

## [0.51.0] - 2026-07-24

### Added

- **`--fix` diff preview + preview→apply contract.** Every proposed fix now shows a
  curated `will change:` diff (revision, per-container images, a safe count of other
  template changes — never env values or template contents) computed at plan time,
  and `Apply` is bound to the preview: if the cluster drifted since (a new rollout,
  the target revision gone), it refuses with `state changed since preview` and makes
  no write. With `--output json`, the plan is included as `remediationPlan`
  (status `proposed`) — the foundation for the coming audit log. Plan and apply now
  share one target-selection rule (highest prior revision with a differing template).

## [0.50.0] - 2026-07-23

### Added

- **Agentic `--investigate`.** After a scan, an opt-in bounded tool-use loop lets the
  model make read-only follow-up reads — describe an object, list its events, hop to a
  related owner/node/PVC — to chase a root cause across the finding's resource graph,
  then emits an `Investigation` section (evidence trail + the grounded fix). Findings-
  scoped, capped (8 reads / 6 turns), no logs, structured-only egress, never writes.
  Anthropic-only (`ANTHROPIC_API_KEY`); supersedes `--explain`.

## [0.49.0] - 2026-07-23

### Added

- **Local-model `--explain`.** Set `KUBEAGENT_EXPLAIN_ENDPOINT` (an OpenAI-compatible
  `/chat/completions` URL — Ollama, vLLM, llama.cpp, LM Studio) and `--explain` runs
  against that local model: no `ANTHROPIC_API_KEY`, and nothing leaves the network.
  `--model`/`KUBEAGENT_MODEL` names the local model; `KUBEAGENT_EXPLAIN_API_KEY` is an
  optional bearer token. Theme-C (principled intelligence) — offline/local explain.

## [0.48.0] - 2026-07-23

### Changed

- **`--explain` now ranks and grounds remediation.** The explanation opens with a
  `Fix first:` ordered remediation list, and each per-issue Fix is anchored to
  kubeagent's deterministic, pre-reviewed `--suggest` command — the model ranks,
  sequences, and phrases, but never invents or substitutes a command. The
  Theme-C (principled intelligence) LLM-ranking layer over the deterministic
  `--suggest` core; the deterministic offline core is unchanged.

## [0.47.0] - 2026-07-23

### Added

- **Admission-webhook latency risk.** `scan` flags a Fail-policy admission webhook
  whose `timeoutSeconds` is at or above 15 (env `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS`,
  Helm `webhookLatency.timeoutThreshold`) — a latency landmine that blocks every
  intercepted create/update for up to that long, then rejects it. Rendered
  `WebhookSlow`; the daemon exposes `kubeagent_admission_webhook_latency_risks`.
  Read-only, always-on, advisory. Closes the Theme-B admission-webhook line.

## [0.46.0] - 2026-07-23

### Added

- **DNS / CoreDNS resolution health (`--dns-health`).** An opt-in probe of each
  CoreDNS pod's `:9153/metrics` flags an elevated SERVFAIL+REFUSED response ratio
  (default ≥ 5% over a 100-response floor; env `KUBEAGENT_DNS_SERVFAIL_RATIO`) —
  catching DNS that is up but failing to resolve, which the CoreDNS-pod health
  check misses. Read-only; needs the `pods/proxy` add-on grant; the daemon exposes
  `kubeagent_dns_servfail_ratio`. Second of the Theme-B control-plane closers.

## [0.45.0] - 2026-07-23

### Added

- **Control-plane / etcd health (`--control-plane-health`).** An opt-in probe of
  the apiserver `/readyz?verbose` endpoint flags an unhealthy control plane —
  naming the failing checks (etcd, admission/controller poststarthooks,
  informer-sync). Read-only; needs the `/readyz` add-on grant; the daemon exposes
  `kubeagent_control_plane_unhealthy`. First of the Theme-B control-plane closers.

## [0.44.0] - 2026-07-23

### Added

- **ResourceQuota near-exhaustion.** `scan` flags a namespace's ResourceQuota
  entry whose usage is at or over 90% of its hard limit (env
  `KUBEAGENT_QUOTA_THRESHOLD` to tune), labelled `exhausted` (blocking new
  objects now) or `near limit` — the proactive complement to the reactive
  `FailedCreate` detector. Read-only, always-on; the daemon exposes
  `kubeagent_resourcequota_issues`. Adds a `resourcequotas` read grant.

## [0.43.0] - 2026-07-22

### Added

- **Stuck-rollout detection (`RolloutStuck`).** `scan` flags a Deployment whose
  rollout has wedged — its `Progressing` condition is
  `ProgressDeadlineExceeded`, or it carries a `ReplicaFailure` condition — so
  the new pods are not becoming available. Surfaced only when no pod-level
  finding already explains the failure (zero redundancy). Read-only, always-on;
  no new flag, metric, or RBAC.

## [0.42.0] - 2026-07-22

### Added

- **`--suggest` next steps.** An opt-in flag prints a deterministic, reviewed
  next-step suggestion and a read-only `kubectl` investigation command under each
  pod finding (CrashLoopBackOff → check the previous logs, ImagePullBackOff →
  verify the tag/credentials, …). Offline (no API key), never LLM-decided, and
  read-only — it prints the command, it never runs it.

## [0.41.0] - 2026-07-22

### Added

- **Missing-config detection (`CreateContainerConfigError`).** `scan` now flags a
  container (main or init) that can't start because a referenced ConfigMap or
  Secret is missing, or a required key is absent — naming the object from the
  kubelet message. Previously such a workload showed only as degraded with no
  explaining finding. Read-only (no new flag or metric).

## [0.40.0] - 2026-07-22

### Added

- **PVC provisioning root cause.** The Pending-PVC check now names *why* a claim
  is stuck by correlating it against the cluster's StorageClasses and PVs — it
  references a StorageClass that does not exist, or (for a static claim) no
  available PersistentVolume matches its size and access modes — and flags these
  even when no `ProvisioningFailed` event is present (catching a PVC whose event
  has expired). Read-only; reuses collected objects (no new flag or metric).

## [0.39.0] - 2026-07-22

### Added

- **Ingress-route root cause.** A broken ingress route (`… has no ready
  endpoints (likely 502/503)`) now names *why* its backend Service is empty —
  the selector matches no pods, the matching pods are on a down node, or none
  are Ready — so the 502 is explained on the route itself. Read-only; reuses the
  Service endpoint-cause logic (no new flag or metric).

## [0.38.0] - 2026-07-22

### Added

- **Service-no-endpoints root cause.** For a broken Service with no ready
  endpoints, `scan` now names *why* — the selector matches no pods, the matching
  pods are on a down node, or they exist but none are Ready — by correlating the
  selector against the collected pods and node health. Read-only; enriches the
  existing service finding (no new flag or metric).

## [0.37.0] - 2026-07-22

### Added

- **Admission-webhook-failure detection.** `scan` flags a Validating/Mutating
  webhook whose `failurePolicy` is `Fail` and whose backing Service is missing
  or has no ready endpoints — it would reject every create/update it intercepts.
  Read-only, advisory, and cluster-wide only (skipped under `--namespace`); the
  daemon exposes `kubeagent_admission_webhooks_failing`. Adds a base
  `admissionregistration.k8s.io` read grant.

## [0.36.0] - 2026-07-21

### Added

- **HPA-can't-scale detection.** `scan` flags a HorizontalPodAutoscaler that is
  stuck — can't fetch metrics (broken autoscaling), can't scale because its
  target is missing or the scale subresource errors, or is pinned at
  `maxReplicas` while demand exceeds the cap — naming the target and the reason.
  Read-only and advisory; the daemon exposes `kubeagent_hpa_scaling_issues`.
  Adds a base `autoscaling/horizontalpodautoscalers` read grant.

## [0.35.0] - 2026-07-21

### Added

- **PodDisruptionBudget-blocked drains.** `scan` flags a PDB that will block a
  node drain — one that can never allow a voluntary eviction, a stale zero-pod
  selector, or a PDB blocking evictions on an already-degraded workload —
  naming the rule and the guarded-pod counts. Read-only and advisory; the
  daemon exposes `kubeagent_pdb_blocking_issues`. Adds a base
  `policy/poddisruptionbudgets` read grant.

## [0.34.0] - 2026-07-21

### Added

- **Stuck-terminating / finalizer-deadlock check.** `scan` flags a Namespace, Pod,
  or PVC wedged in `Terminating` past two minutes and names the blocker — a
  namespace finalizer/condition, a pod's finalizers (or "deletion not confirmed"
  when the node is gone), or `pvc-protection` cross-referenced to the pod still
  mounting the PVC. Read-only and advisory (never removes a finalizer, never
  changes the verdict); the daemon exposes `kubeagent_resources_stuck_terminating`.
  Adds a base `namespaces` read grant.

## [0.33.0] - 2026-07-21

### Added

- **Per-finding confidence score.** Every finding now carries a confidence level —
  high for a directly Kubernetes-asserted state, medium for a kubeagent heuristic
  (`RestartLoop`, `ProbeFailure`) or a statistical correlation (a shared-registry
  attribution). The text report tags only the non-high findings and hints
  (`⚠ RestartLoop [medium]`, `↳ likely caused by registry … [medium]`); JSON
  always carries `"confidence"`. Informational — it never changes priority or the
  cluster verdict. Read-only, always-on, no new RBAC.

## [0.32.0] - 2026-07-21

### Added

- **Certificate-expiry check (opt-in `--certs`).** Flags expired and soon-expiring
  TLS certificates from `kubernetes.io/tls` Secrets — parsing only the public
  certificate, never the key — in an advisory CERTIFICATES section with the
  Ingress routes each cert fronts (`--cert-warn-days`, default 30). Daemon parity
  via `KUBEAGENT_CERTS` + `kubeagent_certificates_expired`/`_expiring` gauges and
  a separate secrets RBAC add-on (`deploy/rbac-certs.yaml` / Helm
  `certs.enabled`); without the flag kubeagent makes no Secrets API calls.

## [0.31.0] - 2026-07-21

### Added

- **Failed-PVC root-cause attribution.** A workload whose pod mounts a
  PersistentVolumeClaim that cannot provision or bind (the v0.26.0 Pending-PVC
  check) is now attributed to that PVC — "↳ likely caused by PVC reports-data
  (ProvisioningFailed)" — connecting a pod stuck Pending/ContainerCreating, which
  has no pod-level finding of its own, to the storage cause kubeagent already
  reports. One affected workload is enough (the PVC is independently diagnosed);
  node attribution takes precedence. Read-only, always-on, no new RBAC.

## [0.30.0] - 2026-07-21

### Added

- **Shared-registry root-cause attribution.** When two or more workloads fail
  image pulls from the same registry host, `scan` names that registry as the
  shared root cause on each ("↳ likely caused by registry ghcr.io (2 workloads
  failing to pull)") — the registry-outage / expired-credentials / rate-limit
  incident. A lone pull failure is never blamed on the registry, and node
  attribution takes precedence. The attention-line rollup now reads
  "(M ⇐ K root causes)" when causes mix. Read-only, always-on, no new RBAC.

## [0.29.0] - 2026-07-21

### Added

- **Node-anchored root-cause attribution.** When a node is hard-down (NotReady, or
  its kubelet stops heartbeating), `scan` attributes each workload with a pod on it
  to that node — a hedged "↳ likely caused by node X (reason)" line plus a rollup
  on the attention line — collapsing a wall of disconnected findings toward the one
  real cause. Additive (the workload's own findings still show), read-only,
  always-on, no new RBAC. The first step of the root-cause correlation roadmap.

## [0.28.2] - 2026-07-20

### Changed

- **Watch gauges exclude parked endpoints.** `kubeagent_service_issues` and
  `kubeagent_ingress_route_issues` now count only real problems, excluding
  intentionally-empty (Expected/parked) Services and routes — a backend scaled to zero or
  annotated `kubeagent.io/expected-empty: "true"` no longer inflates the alert gauges,
  matching how the `scan` report already treats them.

## [0.28.1] - 2026-07-20

### Added

- **Quiet intentionally-empty endpoints.** An Ingress route whose backend Service is empty
  on purpose — the backing workload is scaled to zero (or between runs), or the Service is
  annotated `kubeagent.io/expected-empty: "true"` — is now shown as a parked route in NOTES
  instead of a 502/503 in NEEDS ATTENTION. The annotation also quiets the bare Service
  finding, covering operator-managed role-split Services (e.g. CloudNativePG `-ro` on a
  single-instance cluster) that kubeagent can't infer. Read-only, always-on, no new RBAC.

## [0.28.0] - 2026-07-20

### Added

- **"Can't create pods" (FailedCreate) check.** `scan` now flags a workload stuck below its
  desired replicas because its controller cannot create pods — a `ResourceQuota`,
  `LimitRange`, or admission webhook is rejecting them — naming the cause on the workload
  (e.g. "blocked by a ResourceQuota") with the admission message as evidence. Covers
  Deployments (via their ReplicaSet), StatefulSets, and DaemonSets. Read-only, always-on,
  no new RBAC.

- **`kubeagent_pvc_pending_issues` watch metric.** The `watch` daemon now exposes a
  Prometheus gauge for the count of PersistentVolumeClaims stuck Pending because
  provisioning/binding failed (the v0.26.0 Pending-PVC check), so operators can alert on
  it alongside the existing `kubeagent_*_issues` gauges.

## [0.27.0] - 2026-07-20

### Added

- **Job / CronJob failure check.** `scan` flags a failed Job (`BackoffLimitExceeded` /
  `DeadlineExceeded`) and a CronJob whose most-recent run failed, naming the cause on the
  workload. A failing CronJob is now shown by default (healthy ones stay hidden behind
  `--include-cron`). Read-only, always-on, no new RBAC.

## [0.26.0] - 2026-07-19

### Added

- **Pending-PVC provisioning check.** `scan` flags a PersistentVolumeClaim stuck
  `Pending` because provisioning/binding failed (`ProvisioningFailed` / `FailedBinding`
  events), naming the cause and rendering it in NEEDS ATTENTION (and JSON `pvcIssues`).
  Event-based like `VolumeAttachError`, so the normal `WaitForFirstConsumer` state is
  never flagged. Read-only, always-on, no new RBAC; advisory (does not change the verdict).

## [0.25.0] - 2026-07-19

### Added

- **InitContainer failure detector.** `scan` flags a pod blocked in its init phase —
  `Init:CrashLoopBackOff`, `Init:ImagePullBackOff` / `Init:ErrImagePull`, or
  `Init:OOMKilled` — reading `InitContainerStatuses` (which the main-container crash
  detectors don't look at) and naming which init container is failing, its position,
  and why. Read-only, always-on, no new RBAC.

## [0.24.0] - 2026-07-19

### Added

- **ProbeFailure detector.** `scan` flags a Running-but-not-Ready pod whose readiness,
  liveness, or startup probe keeps failing, reading the kubelet's `Unhealthy` events and
  naming the probe, container, and a plain-language reason (`HTTP 503`, `connection
  refused`, `timed out`, …). Complementary to `RestartLoop`/`CrashLoopBackOff`.
  Read-only, always-on, **no new RBAC**. The raw probe message (which may carry a pod IP
  or `exec` output) is never surfaced, so `--explain` stays privacy-preserving.

## [0.23.0] - 2026-07-18

### Added

- **Crash log root-cause (opt-in).** `scan --logs` reads each crashing container's
  previous-instance logs (`pods/log`) and classifies the failure into a plain-language
  cause — `application panic (code bug)`, `cannot reach a dependency (…) — connection
  refused`, `bad command or entrypoint`, etc. — shown under the finding as
  `logs (previous container): … → <cause>` and in JSON as `logCause`/`logExcerpt`. Only
  the crash findings (CrashLoopBackOff / RestartLoop / OOMKilled) are probed. Read-only,
  scan-only; needs the `pods/log` grant (`deploy/rbac-logs.yaml`). `--explain` receives
  only the derived cause, never raw log text.

## [0.22.0] - 2026-07-15

### Changed

- **Node-reservation reporting is clearer and multi-resource.** `scan` now reports
  the combined kube+system reservation for **memory, CPU, and ephemeral-storage**
  in a labeled per-resource `CONTEXT` block (replacing the cryptic
  `Nodes 0/2 reserve memory OK` line). Reserving no **ephemeral-storage** now
  raises a `NOTES` warning alongside the existing no-memory warning (both are
  node-destabilizers); CPU is informational. Still read-only and advisory; the
  `watch` daemon and `kubeagent_nodes_without_reservations` gauge are unchanged.

- **Relicensed to MIT.** Replaced the Apache-2.0 `LICENSE` with the MIT License
  and removed the Apache-specific `NOTICE` file.

## [0.21.1] - 2026-07-14

### Added

- **Apache 2.0 license.** Added a `LICENSE` (Apache-2.0) and `NOTICE` file, making
  the project's open-source terms explicit.

### Changed

- **README.** Added a hero section with badges (CI, Go Report Card, release,
  license), a highlights list, and a `go install` quick-start.
- **Release packaging.** The release workflow now also publishes an unversioned
  `kubeagent_linux_amd64.tar.gz` asset, so
  `releases/latest/download/kubeagent_linux_amd64.tar.gz` always resolves to the
  newest release.

## [0.21.0] - 2026-07-14

### Added

- **Kubelet health probe.** Opt-in `scan --kubelet-health` probes each node's
  kubelet `/healthz` through the `nodes/proxy` subresource and flags a kubelet
  that is reachable but reporting unhealthy — `✗ node worker-2 kubelet /healthz
  unhealthy: [-]pleg failed` — the "alive but sick" case the lease-heartbeat and
  NotReady checks miss. Shown in a `KUBELET HEALTH` section and JSON
  `kubeletHealth`, with the watch gauge `kubeagent_kubelet_unhealthy` (set
  `KUBEAGENT_KUBELET_HEALTH`). Read-only and **advisory** (does not change the
  cluster verdict); reuses the same `nodes/proxy` add-on as `--disk-usage` (no
  new RBAC).

## [0.20.0] - 2026-07-13

### Added

- **Expected-node baseline.** Opt-in `scan --expected-nodes nova-worker-1,…`
  declares the node names you expect; kubeagent flags each declared node that has
  **no `Node` object** in the cluster — `node nova-worker-2 expected but absent
  from the cluster` — catching a node that never registered or dropped out. It
  degrades the cluster verdict, and the watch daemon exposes
  `kubeagent_nodes_expected_absent` (set `KUBEAGENT_EXPECTED_NODES`). A node that
  exists but is `NotReady` counts as present (its health is flagged elsewhere);
  extra/unexpected nodes are not flagged. Read-only; no new RBAC; best on
  clusters with stable node names.

## [0.19.0] - 2026-07-13

### Added

- **Node heartbeat freshness.** `scan` reads each node's `Lease`
  (`kube-node-lease`) and flags a **Ready** node whose kubelet has stopped
  heartbeating — `kubelet not heartbeating (lease Ns stale)` — catching a dark
  kubelet in the window *before* the control plane marks the node `NotReady`.
  It degrades the cluster verdict, is tunable via `--node-heartbeat-threshold`
  (default `40s`), and the watch daemon exposes
  `kubeagent_nodes_stale_heartbeat` (set `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD`) so
  you can alert before a node goes down. Reads `leases` (a new read-only RBAC
  grant); on by default.

## [0.18.0] - 2026-07-12

### Added

- **Workload security posture.** Opt-in `scan --security` flags PSS-aligned
  hardening problems — privileged/over-privileged containers (privileged, host
  namespaces, hostPath, hostPort, dangerous added capabilities), insecure
  defaults (runs as root, privilege escalation allowed, capabilities not
  dropped), and exposed Services (NodePort/LoadBalancer/externalIPs) — in a
  dedicated `SECURITY` section and JSON `securityIssues`, each labelled
  `baseline`/`restricted`/`kubeagent`. The `SECURITY` section is signal-first:
  it opens with a one-line tier summary, shows the dangerous `baseline` and
  exposed-service findings in full per workload, and folds the near-universal
  `restricted` hardening gaps into a per-check aggregate; pass
  `--security-verbose` to list every finding per workload. JSON `securityIssues`
  always contains all findings regardless of the flag. Read-only and advisory
  (does not change the cluster verdict); needs no new RBAC; excludes system
  namespaces by default.

## [0.17.0] - 2026-07-11

### Added

- **Ingress route health.** `scan` now resolves each Ingress rule's backend
  Service and flags broken routes — the backend Service is missing (`NoService`),
  has no ready endpoints (`NoEndpoints`, the classic 502/503), or does not expose
  the referenced port (`PortNotExposed`) — in the NEEDS ATTENTION section and JSON
  `ingressIssues`, with the watch-daemon gauge `kubeagent_ingress_route_issues`.
  This turns "why is my ingress returning 502?" into a concrete cause. Reads
  Ingresses (a new read-only RBAC grant); advisory (does not change the cluster
  verdict).

## [0.16.0] - 2026-07-09

### Changed

- **Root cause for NotReady nodes and findings.** A `NotReady` node now names its
  cause — the `NodeReady` condition's reason and message (e.g.
  `NotReady: KubeletNotReady — container runtime network not ready: cni config
  uninitialized`) — instead of a bare `NotReady`. And the text scan now prints
  each finding's underlying signal (`Finding.Evidence`) beneath it, so a pending
  pod shows the scheduler's message (`0/5 nodes are available: 3 Insufficient
  memory, …`) without needing `--output json` or `--explain`. Read-only; the
  cluster verdict and JSON schema are unchanged.

## [0.15.0] - 2026-07-09

### Added

- **Disk-usage check (opt-in).** `scan --disk-usage` reads each node's kubelet
  `/stats/summary` (via the `nodes/proxy` subresource) and flags node
  filesystems and PVCs at or over a threshold (`--disk-threshold`, default
  `0.80`) in the NEEDS ATTENTION section and JSON `diskUsage` — an early warning
  before the kubelet's `DiskPressure` eviction signal. Off by default (adds no
  RBAC); enable the daemon with `KUBEAGENT_DISK_USAGE=true` and the
  `nodes/proxy` add-on (`deploy/rbac-diskusage.yaml` or Helm
  `diskUsage.enabled=true`), which also exposes `kubeagent_node_fs_usage_ratio`
  and `kubeagent_volumes_over_disk_threshold`. Read-only; advisory (does not
  change the cluster verdict).

## [0.14.0] - 2026-07-08

### Changed

- **Redesigned `scan` text output.** The human-readable output is now organized
  by severity into **NEEDS ATTENTION** (failing workloads, dead Services,
  credential warnings), **NOTES** (advisories — Delete-policy PVCs, expected-empty
  Services, hidden-workload counts), and **CONTEXT** (nodes/reservations,
  resources, platform), with a workload-scoped "Needs attention" line under the
  cluster verdict. All-OK node reservations collapse to one line, and
  Delete-policy PVCs show as a grouped summary — pass `--pvc-reclaim` for the full
  per-PVC list. `--output json` is unchanged, and `--fix` behavior is unchanged.

## [0.13.0] - 2026-07-08

### Added

- **PVC reclaim-policy check.** `scan` now lists Bound PersistentVolumeClaims
  whose bound PersistentVolume has `reclaimPolicy: Delete` — the data-loss-prone
  case where deleting the PVC or PV destroys the underlying storage. Shown as a
  "PVCs with reclaim policy Delete" section (text + JSON `pvcReclaim`) and, in
  the watch daemon, as the gauge `kubeagent_pvcs_reclaim_delete`. Reads PVCs and
  their bound PVs (two new read-only RBAC grants); advisory only (does not change
  the cluster verdict).

## [0.12.0] - 2026-07-08

### Added

- **Node reservation check.** `scan` now reports each node's aggregate kubelet
  reservation (`Capacity − Allocatable`, i.e. kube-reserved + system-reserved +
  eviction-hard combined) and warns when a node reserves **no memory** —
  a kubelet that can be OOM'd under pressure. Shown as a "Node reservations"
  section (text + JSON `nodeReserve`) and, in the watch daemon, as the gauge
  `kubeagent_nodes_without_reservations`. Read-only; no new RBAC. Advisory only
  (does not change the cluster verdict).

- **Helm chart.** The in-cluster watch daemon is now packaged as a Helm chart
  under `deploy/helm/kubeagent/`, alongside the raw manifests. It renders the
  identical read-only RBAC (`get`/`list`/`watch` only), deployment, and metrics
  Service, with image, replicas, watch cadence, metrics port, RBAC/ServiceAccount
  creation, resources, security context, and scheduling exposed as values.

## [0.11.0] - 2026-07-07

### Added

- **Restart-loop detection.** A new `RestartLoop` finding flags a container that
  keeps exiting with a non-OOM error and restarting (`RestartCount ≥ 3`, current
  run younger than 10 min) even though it is currently `Running` — a flapping pod
  the point-in-time detectors (`CrashLoopBackOff`/`OOMKilled`) miss. Durable
  (reads `RestartCount` + `lastState.Terminated`), so it appears in the scan,
  `--explain`, and `kubeagent_findings{issue="RestartLoop"}`. Read-only.

## [0.10.0] - 2026-07-06

### Added

- **Volume-attach detection.** A new `VolumeAttachError` finding flags a pod stuck
  at container creation because a volume cannot be attached (`FailedAttachVolume`
  Warning event) — most often a **Multi-Attach** error (a ReadWriteOnce volume
  still attached to another node). Detected by reading the pod's events (one cheap
  field-selected List; the watch daemon needs no events informer). Read-only; the
  daemon's RBAC gains `events` read.

## [0.9.0] - 2026-07-05

### Added

- **Daemon watch mode (`kubeagent watch`).** Run kubeagent in-cluster as a
  strictly read-only daemon: it watches the cluster via informers, re-runs the
  deterministic diagnosis on change (debounced) plus a heartbeat, and exposes the
  result as structured logs and hand-rolled Prometheus `/metrics` (with `/healthz`
  and `/readyz`). No cluster writes, no LLM calls, no new dependency. Read-only
  RBAC and Deployment manifests are in `deploy/`. (Multi-cluster, Kubernetes
  Events, `--explain`, and autonomous remediation are planned for later phases.)
- **Dockerfile.** A multi-stage build producing a small distroless, non-root
  image for running the daemon in-cluster (used by `deploy/deployment.yaml`).

## [0.8.0] - 2026-07-04

### Added

- **"What changed" rollout awareness.** A flagged Deployment now shows its most
  recent rollout when it is recent (within 7 days) — the revision, its age, and
  the image delta (`↳ changed: rollout to revision 6, 4d ago · image A → B`) — in
  text, JSON (`rollout`), and `--explain`. Deterministic and read-only (reuses
  the ReplicaSets already collected); factual, with no causal claim.

### Changed

- **`--fix` `RolloutUndo` is more conservative.** A Deployment rollback is now
  proposed only when the Deployment is **degraded** (fewer ready replicas than
  desired). A rollout stuck on `ImagePullBackOff` while its previous revision is
  still fully serving is left alone (the failure still shows in the scan and
  `--explain`; only the automatic rollback proposal is withheld).

## [0.7.0] - 2026-07-01

### Added

- **`--fix` remediation: `Uncordon`.** A second guard-railed action — an
  accidentally-cordoned node (`SchedulingDisabled`, no `NoExecute` taint) is made
  schedulable again after a per-action confirmation. Same rails as `RolloutUndo`
  (allowlist, apply-time precondition re-check, single write, never LLM-decided).

### Changed

- **Sharper `--explain`.** The `--explain` prompt now instructs a consistent,
  scannable structure (per issue: root cause → checks → fix; cluster/kube-system
  problems before workloads) and is grounded strictly in the scan's facts (told
  not to invent causes), reducing misattributed root causes. Still opt-in,
  read-only, structured-facts-only, and independent of `--fix`.

## [0.6.0] - 2026-07-01

### Added

- **`--fix` remediation (opt-in).** `scan --fix` proposes and, after a per-action
  `[y/N]` confirmation, applies safe reversible remediations (`--dry-run` to
  preview, `--yes` for non-interactive). v1 ships `RolloutUndo` (roll a Deployment
  with a failed image rollout back to its previous revision). Guard-railed:
  allowlist, protected namespaces, apply-time precondition re-check, re-verify;
  never LLM-decided. This is the first feature that can write to the cluster;
  default behavior remains read-only.

## [0.5.0] - 2026-06-30

No changes to the `kubeagent` binary since 0.4.0 — this release adds project
infrastructure (a documentation site and a pre-release chaos-test harness).

### Added

- **Documentation website.** A MkDocs + Material site (landing page, quickstart,
  per-feature docs, install, roadmap), published to GitHub Pages at
  [k8sproject.top](https://k8sproject.top) via a `pages.yml` workflow.
- **Pre-release chaos-test harness.** `chaos/run.sh` spins up a disposable Kind
  cluster (Calico CNI), injects the 10 most common production outages, runs
  `kubeagent scan` against each (adding `--explain` when `ANTHROPIC_API_KEY` is
  set), and writes a results report — a manual gate before each release, wired
  into the release checklist. See `chaos/README.md`.

## [0.4.0] - 2026-06-30

### Added

- **Service backing awareness.** A "no ready endpoints" Service issue is now
  annotated with its backing workload when that workload expects no pods — a
  CronJob/Job, or a DaemonSet/Deployment/StatefulSet scaled to 0 — so these stop
  reading as primary problems (text + JSON `expected`/`backing`). A
  Deployment/StatefulSet with replicas and no endpoints stays a primary issue.

### Fixed

- **Credential lint precision.** `--lint-secrets` no longer flags `*_FILE` env
  vars (which hold a path to a secret file, not the secret itself) or values that
  are dotted version numbers — removing two false-positive classes found in live
  use. Real secret values in `*_FILE`-named vars are still flagged.

## [0.3.0] - 2026-06-29

### Added

- **Connectivity diagnostics.** An unreachable or broken API server now yields an
  actionable diagnosis (down / timeout / TLS-cert / auth / DNS) with a `details:`
  line, instead of a raw transport error.
- **NetworkPolicy awareness.** A degraded workload with no detector finding is
  annotated with the NetworkPolicies selecting its pods (a root-cause hint), in
  text, JSON, and `--explain`.
- **Service / LoadBalancer health.** `scan` flags selector-based Services with no
  ready endpoints and LoadBalancer Services with no external address, in a new
  "Service issues" section (text + JSON) and in `--explain`.
- **Credential lint (opt-in).** `scan --lint-secrets` flags credentials stored in
  the clear (ConfigMap values, pod env literals) by location and pattern — never
  the value, and never sent to `--explain`.

## [0.2.0] - 2026-06-29

### Added

- **Resource context.** OOMKilled findings now show the killed container's CPU
  and memory requests + limits. `scan` also prints a cluster resource summary
  (CPU/memory: allocatable, reserved/requests, limits, and — when metrics-server
  is present — live usage) in text and JSON, and feeds it to `--explain` so the
  model can judge whether to raise a limit or scale out. Live usage is
  best-effort and degrades gracefully when metrics-server is absent.
- **Platform facts.** A second line under the cluster verdict naming the detected
  stack — CNI, ingress, storage provisioner(s), Kubernetes version + distribution,
  container runtime, and cloud — also in JSON (`platform`) and in `--explain`. No
  instance identifiers (e.g. raw `providerID`) are emitted.

### Fixed

- Completed/failed **bare pods** (e.g. a one-shot `kubectl run` pod in
  `Succeeded`/`Completed`) are no longer reported as `Degraded`. A pod-derived
  workload in a terminal phase is now treated like a finished Job (`Complete` /
  `Failed`) instead of being run through the ready/desired health model.
- The release `scan` now emits a warning when a metrics-server response is
  present but malformed (previously silently discarded).

## [0.1.0] - 2026-06-27

### Added

- Initial release. `kubeagent scan`: a read-only, prioritized cluster problem
  report — a P1 cluster-health verdict (nodes + `kube-system`) followed by P2
  workload/pod failures, in text or JSON.
- Deterministic detectors: CrashLoopBackOff, ImagePullBackOff/ErrImagePull,
  OOMKilled, Pending/Unschedulable.
- Workload inventory grouped by controller (Deployment / StatefulSet / DaemonSet /
  Job / CronJob / bare pod) with restart history; `--include-restarts` and
  `--include-cron` opt-ins.
- Optional `--explain` flag: a single Claude API call (official Go SDK)
  summarizing findings in plain English; the deterministic core stays usable
  offline. Model selectable via `--model` / `KUBEAGENT_MODEL`.
- `kubeagent version` subcommand (stamped at release time).
- CI (vet/test/build on push & PR) and a release workflow publishing a
  linux/amd64 tarball + `SHA256SUMS` to a GitHub Release.

[Unreleased]: https://github.com/imantaba/kubeagent/compare/v0.52.0...HEAD
[0.52.0]: https://github.com/imantaba/kubeagent/compare/v0.51.0...v0.52.0
[0.51.0]: https://github.com/imantaba/kubeagent/compare/v0.50.0...v0.51.0
[0.50.0]: https://github.com/imantaba/kubeagent/compare/v0.49.0...v0.50.0
[0.49.0]: https://github.com/imantaba/kubeagent/compare/v0.48.0...v0.49.0
[0.48.0]: https://github.com/imantaba/kubeagent/compare/v0.47.0...v0.48.0
[0.47.0]: https://github.com/imantaba/kubeagent/compare/v0.46.0...v0.47.0
[0.46.0]: https://github.com/imantaba/kubeagent/compare/v0.45.0...v0.46.0
[0.45.0]: https://github.com/imantaba/kubeagent/compare/v0.44.0...v0.45.0
[0.44.0]: https://github.com/imantaba/kubeagent/compare/v0.43.0...v0.44.0
[0.43.0]: https://github.com/imantaba/kubeagent/compare/v0.42.0...v0.43.0
[0.42.0]: https://github.com/imantaba/kubeagent/compare/v0.41.0...v0.42.0
[0.41.0]: https://github.com/imantaba/kubeagent/compare/v0.40.0...v0.41.0
[0.40.0]: https://github.com/imantaba/kubeagent/compare/v0.39.0...v0.40.0
[0.39.0]: https://github.com/imantaba/kubeagent/compare/v0.38.0...v0.39.0
[0.38.0]: https://github.com/imantaba/kubeagent/compare/v0.37.0...v0.38.0
[0.37.0]: https://github.com/imantaba/kubeagent/compare/v0.36.0...v0.37.0
[0.36.0]: https://github.com/imantaba/kubeagent/compare/v0.35.0...v0.36.0
[0.35.0]: https://github.com/imantaba/kubeagent/compare/v0.34.0...v0.35.0
[0.34.0]: https://github.com/imantaba/kubeagent/compare/v0.33.0...v0.34.0
[0.33.0]: https://github.com/imantaba/kubeagent/compare/v0.32.0...v0.33.0
[0.32.0]: https://github.com/imantaba/kubeagent/compare/v0.31.0...v0.32.0
[0.31.0]: https://github.com/imantaba/kubeagent/compare/v0.30.0...v0.31.0
[0.30.0]: https://github.com/imantaba/kubeagent/compare/v0.29.0...v0.30.0
[0.29.0]: https://github.com/imantaba/kubeagent/compare/v0.28.2...v0.29.0
[0.28.2]: https://github.com/imantaba/kubeagent/compare/v0.28.1...v0.28.2
[0.28.1]: https://github.com/imantaba/kubeagent/compare/v0.28.0...v0.28.1
[0.28.0]: https://github.com/imantaba/kubeagent/compare/v0.27.0...v0.28.0
[0.27.0]: https://github.com/imantaba/kubeagent/compare/v0.26.0...v0.27.0
[0.26.0]: https://github.com/imantaba/kubeagent/compare/v0.25.0...v0.26.0
[0.25.0]: https://github.com/imantaba/kubeagent/compare/v0.24.0...v0.25.0
[0.24.0]: https://github.com/imantaba/kubeagent/compare/v0.23.0...v0.24.0
[0.23.0]: https://github.com/imantaba/kubeagent/compare/v0.22.0...v0.23.0
[0.22.0]: https://github.com/imantaba/kubeagent/compare/v0.21.1...v0.22.0
[0.21.1]: https://github.com/imantaba/kubeagent/compare/v0.21.0...v0.21.1
[0.21.0]: https://github.com/imantaba/kubeagent/compare/v0.20.0...v0.21.0
[0.20.0]: https://github.com/imantaba/kubeagent/compare/v0.19.0...v0.20.0
[0.19.0]: https://github.com/imantaba/kubeagent/compare/v0.18.0...v0.19.0
[0.18.0]: https://github.com/imantaba/kubeagent/compare/v0.17.0...v0.18.0
[0.17.0]: https://github.com/imantaba/kubeagent/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/imantaba/kubeagent/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/imantaba/kubeagent/compare/v0.14.0...v0.15.0
[0.14.0]: https://github.com/imantaba/kubeagent/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/imantaba/kubeagent/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/imantaba/kubeagent/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/imantaba/kubeagent/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/imantaba/kubeagent/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/imantaba/kubeagent/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/imantaba/kubeagent/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/imantaba/kubeagent/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/imantaba/kubeagent/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/imantaba/kubeagent/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/imantaba/kubeagent/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/imantaba/kubeagent/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/imantaba/kubeagent/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/imantaba/kubeagent/releases/tag/v0.1.0
