# Roadmap

## Shipped

- **v1** — `kubeagent scan`: deterministic whole-cluster scan and diagnosis of
  [CrashLoopBackOff, ImagePullBackOff/ErrImagePull, OOMKilled, and
  Pending/Unschedulable pods](features/diagnostics.md)
- **v2** — optional `--explain` flag: one Claude API call summarizes findings in
  plain English; the deterministic core still works offline with no API key
- **Resource context** — compact CPU/memory summary (allocatable, reserved,
  limits, live usage); OOMKilled findings annotated with the container's
  requests/limits; sent to `--explain` — see [Resource context](features/resource-context.md)
- **Platform facts** — CNI, ingress, storage provisioner, Kubernetes version +
  distribution, container runtime, and cloud detected read-only and shown under
  the cluster verdict; sent to `--explain` — see [Platform facts](features/platform-facts.md)
- **Service health** — flags selector-based Services with zero ready endpoints
  and LoadBalancer Services with no external address; backing-workload
  annotations distinguish expected-empty from broken — see [Service health](features/service-health.md)
- **NetworkPolicy hints** — when a workload is degraded with no detector finding,
  names the NetworkPolicies whose podSelector matches its pods — see
  [NetworkPolicy hints](features/networkpolicy.md)
- **Connectivity diagnostics** — when the API server is unreachable, prints an
  actionable diagnosis (down control plane, timeout, TLS/cert error, 401/403,
  DNS) instead of a raw transport error — see [Connectivity diagnostics](features/connectivity.md)
- **Credential lint** — `scan --lint-secrets` flags credentials stored in the
  clear in ConfigMaps and pod env literals; reports location and pattern only,
  never the value, and never sends findings to `--explain` — see
  [Credential lint](features/credential-lint.md)
- **Remediation (`--fix`)** — opt-in, guard-railed writes that apply safe,
  reversible fixes (`RolloutUndo`, `Uncordon`) after a per-action confirmation;
  deterministic and never model-decided, with a fixed allowlist and protected
  namespaces — see [Remediation](features/remediation.md)
- **Daemon watch mode** — `kubeagent watch` runs in-cluster (read-only) and
  exposes continuous cluster-health diagnosis as Prometheus metrics + structured
  logs; see `deploy/`. First phase of a daemon roadmap (multi-cluster, on-incident
  `--explain`, and guarded autonomous remediation to follow).
- **Volume-attach & restart-loop detection** — `VolumeAttachError` flags a pod
  stuck because a volume can't attach (Multi-Attach); `RestartLoop` flags a
  currently-Running container that keeps erroring and restarting — the flapping
  case `CrashLoopBackOff` misses. See [Failure diagnostics](features/diagnostics.md).
- **Node & storage safety checks** — a node reservation check warns when a
  node's kubelet reserves no memory (`allocatable == capacity`), and a PVC
  reclaim-policy check lists Bound PersistentVolumeClaims whose bound PV reclaims
  with `Delete` (data-loss-prone). Both are read-only and advisory, and appear in
  the daemon as `kubeagent_nodes_without_reservations` and
  `kubeagent_pvcs_reclaim_delete`. See [Failure diagnostics](features/diagnostics.md).
- **Helm chart** — the read-only watch daemon is packaged as a Helm chart under
  [`deploy/helm/kubeagent/`](https://github.com/imantaba/kubeagent/tree/main/deploy/helm/kubeagent),
  alongside the raw manifests — see [Install](install.md#with-helm).
- **Disk-usage check (opt-in)** — `scan --disk-usage` reads each node's kubelet
  `/stats/summary` (via `nodes/proxy`) and flags node filesystems and PVCs at or
  over `--disk-threshold` (default `0.80`) — an early warning before the
  kubelet's `DiskPressure` eviction signal. Off by default (needs a `nodes/proxy`
  add-on); the daemon exposes `kubeagent_node_fs_usage_ratio` and
  `kubeagent_volumes_over_disk_threshold`. See
  [Failure diagnostics](features/diagnostics.md).
- **Ingress route health** — `scan` follows each Ingress rule to its backend
  Service and flags routes whose Service is missing, has no ready endpoints, or
  does not expose the referenced port — the usual causes of a 502/503 — in
  NEEDS ATTENTION, JSON `ingressIssues`, and the daemon gauge
  `kubeagent_ingress_route_issues`. See [Failure diagnostics](features/diagnostics.md).

- **Workload security posture** — opt-in `scan --security` flags PSS-aligned
  hardening problems (privileged/insecure containers, exposed Services) in a
  `SECURITY` section and JSON `securityIssues`, labelled baseline/restricted/
  kubeagent. Read-only, advisory, no new RBAC. See
  [Failure diagnostics](features/diagnostics.md).

- **Node heartbeat freshness** — `scan` flags a Ready node whose kubelet `Lease`
  has gone stale (kubelet not heartbeating) before it flips to `NotReady`, and
  the daemon exposes `kubeagent_nodes_stale_heartbeat`. See
  [Failure diagnostics](features/diagnostics.md).

- **Expected-node baseline** — opt-in `scan --expected-nodes` flags a declared
  node that is absent from the cluster (never registered or dropped out), and
  the daemon exposes `kubeagent_nodes_expected_absent`. See
  [Failure diagnostics](features/diagnostics.md).

- **Kubelet health probe** — opt-in `scan --kubelet-health` probes each kubelet's
  `/healthz` via `nodes/proxy` and flags an alive-but-unhealthy kubelet in a
  `KUBELET HEALTH` section, with the daemon gauge `kubeagent_kubelet_unhealthy`.
  See [Failure diagnostics](features/diagnostics.md).

- **Probe, init-container & batch failures** — `ProbeFailure` flags a
  Running-but-not-Ready pod whose readiness/liveness/startup probe is failing;
  `Init:*` failures flag a pod stuck in its init phase (crash loop, image pull, or
  OOM in an init container); `JobFailed` flags a failed Job (`BackoffLimitExceeded`
  / `DeadlineExceeded`) and a CronJob whose most-recent run failed (shown by
  default). See [Failure diagnostics](features/diagnostics.md).

- **Pending-PVC provisioning** — `scan` flags a PersistentVolumeClaim stuck
  `Pending` because provisioning or binding failed (a missing StorageClass, a
  broken provisioner), while never flagging a `WaitForFirstConsumer` PVC that is
  simply waiting for its pod — with the daemon gauge
  `kubeagent_pvc_pending_issues`. See [Failure diagnostics](features/diagnostics.md).

- **Can't-create-pods (`FailedCreate`)** — `scan` names the cause when a workload
  sits below its desired replicas because its controller cannot *create* pods — a
  `ResourceQuota`, `LimitRange`, or admission webhook is rejecting them (the
  pod-level detectors see nothing because there are no pods). Covers Deployments
  (via their ReplicaSet), StatefulSets, and DaemonSets. See
  [Failure diagnostics](features/diagnostics.md).

- **Quiet intentionally-empty endpoints** — a Service/Ingress route that is empty
  *on purpose* (backend scaled to zero, a Job/CronJob between runs, or a Service
  annotated `kubeagent.io/expected-empty: "true"`) is shown as a parked note
  instead of a false 502/503 alarm, and is excluded from the
  `kubeagent_service_issues` / `kubeagent_ingress_route_issues` gauges — so alerts
  fire on real outages only. See [Failure diagnostics](features/diagnostics.md).

- **Crash log root-cause** — opt-in `scan --logs` reads the last log lines of a
  crashing container and labels the likely cause (application panic, OOM, config
  error) as a one-line `LogCause` on the finding; never sent verbatim to a shared
  service. See [Failure diagnostics](features/diagnostics.md).

- **Root-cause attribution (nodes, registries & PVCs)** — a hard-down node
  (NotReady or kubelet-not-heartbeating) becomes the named root cause of the
  workloads with pods on it; a registry shared by two-plus failing image pulls
  becomes the named root cause of those workloads; and a PVC that cannot
  provision becomes the named root cause of the workloads mounting it; the first
  slices of the root-cause correlation theme. See
  [Failure diagnostics](features/diagnostics.md).

- **Certificate expiry (opt-in)** — `scan --certs` flags expired and soon-expiring TLS certificates (public cert metadata only) with the Ingress routes they front; daemon gauges + a separate secrets RBAC add-on. See [Failure diagnostics](features/diagnostics.md).

- **Finding confidence** — every finding and correlation hint is labelled high
  (direct Kubernetes state) or medium (kubeagent heuristic / statistical
  correlation); tagged in the report only when not high, always in JSON. See
  [Failure diagnostics](features/diagnostics.md).

- **Stuck-terminating detection** — flags namespaces/pods/PVCs wedged in
  Terminating past two minutes and names the blocking finalizer or condition. See
  [Failure diagnostics](features/diagnostics.md).

- **PDB-blocked drains** — flags a PodDisruptionBudget that will block a node
  drain: unsatisfiable (requires more healthy pods than exist), stale (selector
  matches no pods), or blocking (workload already degraded so
  `DisruptionsAllowed == 0`). Advisory and read-only; the daemon exposes
  `kubeagent_pdb_blocking_issues`. See [Failure diagnostics](features/diagnostics.md).

- **HPA-can't-scale** — flags a HorizontalPodAutoscaler that is stuck: can't
  fetch metrics (`metrics` category), can't act on its scale target at all
  (`unable` category), or is pinned at `maxReplicas` while demand exceeds the
  cap (`capped` category). Advisory and read-only; the daemon exposes
  `kubeagent_hpa_scaling_issues`. See [Failure diagnostics](features/diagnostics.md).

- **Admission-webhook failure** — `scan` flags a Validating/Mutating webhook
  whose `failurePolicy` is `Fail` and whose backing Service is missing or has no
  ready endpoints — it would silently reject every intercepted create/update.
  Cluster-wide only (skipped under `--namespace`); advisory and read-only; the
  daemon exposes `kubeagent_admission_webhooks_failing`. See
  [Failure diagnostics](features/diagnostics.md).

- **Service-no-endpoints root cause** (first Theme-A / root-cause step for the
  Service → Pod → Node graph) — for a broken Service with no ready endpoints,
  `scan` names *why*: the selector matches no pods, the matching pods are on a
  down node, or they exist but none are Ready. Read-only correlation over
  collected pods and node health; enriches the existing service finding with no
  new flag, metric, or RBAC. See [Service health](features/service-health.md).

- **Ingress-route root cause** (extends the Theme-A chain to Ingress → Service →
  Pod → Node) — a broken ingress route now names *why* its backend Service is
  empty using the same endpoint-cause logic, one hop up the graph — so the 502 is
  explained on the route itself without cross-referencing the Service finding.
  Read-only; no new flag, metric, or RBAC. See
  [Failure diagnostics](features/diagnostics.md).

- **PVC provisioning root cause** (completes the Theme-A root-cause chain with
  PVC → StorageClass → PV) — a Pending PVC now names the structural cause: it
  references a StorageClass that does not exist, or (for a static claim) no
  available PersistentVolume matches its size and access modes. Fires even when no
  `ProvisioningFailed` event is present (long-stuck PVC with expired events).
  Read-only; correlates against collected StorageClasses and PVs; no new flag,
  metric, or RBAC. See [Failure diagnostics](features/diagnostics.md).

- **Missing-config detection (`CreateContainerConfigError`)** (Theme-B deeper
  diagnosis) — `scan` flags a container (main or init) that cannot start because
  a referenced ConfigMap or Secret is missing from the cluster, or a required key
  is absent — naming the object directly from the kubelet event message. Read-only;
  no new flag, metric, or RBAC. See [Failure diagnostics](features/diagnostics.md).

- **Stuck-rollout detection (`RolloutStuck`)** (Theme-B deeper diagnosis) —
  `scan` flags a Deployment whose rollout has wedged, naming it distinctly from
  any underlying pod crash: its `Progressing` condition is
  `ProgressDeadlineExceeded`, or it carries a `ReplicaFailure` condition, and the
  new pods are not becoming available. Surfaced only when no pod-level finding
  already explains the failure (zero redundancy). Read-only, always-on; no new
  flag, metric, or RBAC. See [Failure diagnostics](features/diagnostics.md).

- **ResourceQuota near-exhaustion** (Theme-B deeper diagnosis) — `scan` flags a
  namespace's ResourceQuota entry whose `used/hard` ratio is at or over 90%,
  labelled `exhausted` (100%, blocking new objects now) or `near limit` — the
  proactive early-warning half of quota diagnosis, complementing the reactive
  `FailedCreate` detector that fires only after creation is already being denied.
  Threshold tunable via `KUBEAGENT_QUOTA_THRESHOLD`; the daemon exposes
  `kubeagent_resourcequota_issues`; adds a `resourcequotas` read grant. See
  [Failure diagnostics](features/diagnostics.md).

- **Control-plane / etcd health (`--control-plane-health`)** (Theme-B control-plane closer) —
  opt-in `scan --control-plane-health` probes the apiserver `/readyz?verbose`
  endpoint and flags an unhealthy control plane, naming the failing checks (etcd,
  admission/controller poststarthooks, informer-sync). Covers apiserver + etcd;
  scheduler/controller-manager health is a documented follow-on. Read-only; needs
  the `/readyz` add-on grant (`deploy/rbac-controlplane.yaml` or Helm
  `controlPlaneHealth.enabled=true`); the daemon exposes
  `kubeagent_control_plane_unhealthy`. See
  [Failure diagnostics](features/diagnostics.md).

- **DNS / CoreDNS resolution health (`--dns-health`)** (Theme-B control-plane closer) —
  opt-in `scan --dns-health` probes each CoreDNS pod's `:9153/metrics` and flags
  an elevated SERVFAIL+REFUSED response ratio (default ≥ 5% over a 100-response
  floor; env `KUBEAGENT_DNS_SERVFAIL_RATIO`) — catching DNS that is up but failing
  to resolve, which the CoreDNS-pod health check misses. Read-only; needs the
  `pods/proxy` add-on grant (`deploy/rbac-dnshealth.yaml` or Helm
  `dnsHealth.enabled=true`); the daemon exposes `kubeagent_dns_servfail_ratio`.
  See [Failure diagnostics](features/diagnostics.md).

- **Admission-webhook latency risk** (Theme-B closer — closes the admission-webhook line) —
  always-on `scan` check that flags a Fail-policy webhook whose `timeoutSeconds`
  is at or above 15 (env `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS`, Helm
  `webhookLatency.timeoutThreshold`) — a latency landmine that blocks every
  intercepted create/update for up to that long, then rejects it. Rendered
  `WebhookSlow`; complements the existing webhook-failure check (missing/no-endpoints
  backend). Read-only, always-on, advisory; the daemon exposes
  `kubeagent_admission_webhook_latency_risks`; no new RBAC.
  See [Failure diagnostics](features/diagnostics.md).

- **`--explain` ranked and grounded remediation** (Theme-C — the LLM-ranking layer over
  the deterministic `--suggest` core) — `--explain` now opens with a `Fix first:` ordered
  remediation list (cluster P1 before workload P2, most-blocking first), and each per-issue
  Fix is anchored to kubeagent's deterministic, pre-reviewed `--suggest` command — the model
  ranks, sequences, and phrases, but never invents or substitutes a command. The deterministic
  offline core is unchanged; `--explain` remains opt-in and requires an API key. See
  [Failure diagnostics](features/diagnostics.md#status).

- **Local-model (offline) `--explain`** (Theme-C — offline/local explain) — set
  `KUBEAGENT_EXPLAIN_ENDPOINT` to any OpenAI-compatible `/chat/completions` base URL
  (Ollama, vLLM, llama.cpp, LM Studio) and `--explain` runs against that local model: no
  `ANTHROPIC_API_KEY`, and nothing leaves the network. The prompt, ranked `Fix first:`
  output, and offline scan core are unchanged. See
  [Failure diagnostics](features/diagnostics.md#status).

- **`--suggest` next steps** (first Theme-C / principled intelligence slice) —
  opt-in `scan --suggest` prints a deterministic, reviewed next-step suggestion
  and a read-only `kubectl` investigation command under each pod finding. Offline
  (no API key), never LLM-decided, and read-only — it prints the command, it
  never runs it. This is the deterministic remediation core that a later
  Theme-C slice will hand to `--explain` for LLM ranking and phrasing (the LLM
  ranks; it never invents the remediation). See
  [Failure diagnostics](features/diagnostics.md).

- **`--investigate`** — agentic read-only follow-up reads (bounded tool-use loop
  over findings: describe objects, list events, hop to related owner/node/PVC) to
  chase a root cause and emit a grounded Investigation section; closing Theme C's
  principled-intelligence slices. See
  [Failure diagnostics](features/diagnostics.md).

- **`--fix` diff preview + preview→apply contract** (Theme D — slice 1, remediation
  that earns trust) — every proposed fix now shows a plan-time `will change:` diff
  (revision, per-container images, a safe count of other template changes — never env
  values or template contents), and `Apply` is bound to that preview: if the cluster
  drifted since (a new rollout, the target revision gone), it refuses with
  `state changed since preview` and makes no write. With `--output json`, the plan
  appears as `remediationPlan` (status `proposed`) — the foundation for the coming
  audit-log and RBAC-preflight slices. See [Remediation](features/remediation.md).

- **`--fix` audit log** (`--audit-log`, append-only JSON-Lines record of every
  remediation disposition) — the accountability half of the remediation contract.

- **`--fix` RBAC preflight** (`SelfSubjectAccessReview` before each write; clean
  up-front refusal with `skipped:` message, new `preflight` audit disposition,
  dry-run permission report for each proposed fix) — the third write-path
  hardening slice of Theme D.

- **`--fix` rollback** (`--rollback`, undo the last applied fix from the audit
  log through every guard rail: curated preview diff, `[y/N]`, drift bond, RBAC
  preflight, `rollback` audit disposition; inverse derived from structured
  `fromRevision`/`toRevision` fields; pre-v0.54 records refused cleanly) — the
  fourth and final write-path hardening slice, **completing Theme D**. See
  [Remediation](features/remediation.md#rollback-rollback).

- **Stateful `watch`** (Theme E — slice 1, the stateful core) — the daemon now
  tracks issue state across reconciles instead of re-deriving the whole
  picture every cycle, logging only the transitions (`NEW` / `RESOLVED` /
  `FLAPPING`, steady state silent), exposing ten new Prometheus series
  including mean-time-to-resolution, and serving a read-only `/issues` JSON
  endpoint. In-memory only (state resets on restart); fixed, unconfigurable
  defaults; no new flags or RBAC. See
  [Watch mode](features/watch-mode.md#issue-tracking-state-across-reconciles).

- **`watch` alerting** (Theme E — slice 2) — the daemon can now push transitions
  outbound: one webhook alert per broken object in `json`, `slack`, or
  `alertmanager` form. Alerts roll up on the object, not the issue, so an
  evolving failure (`Degraded` → `ErrImagePull` → `ImagePullBackOff`) opens a
  single alert that clears only once the object has no active issues at all —
  a still-broken workload never reports a recovery. Off unless
  `KUBEAGENT_ALERT_WEBHOOK` is set; the URL is env-only, Secret-only in the
  chart, and never logged beyond `scheme://host`. Delivery is a bounded queue
  with three attempts and counted drops, on its own goroutine, so a hung
  receiver cannot stall the reconcile loop. The daemon stays strictly
  read-only toward the cluster and calls no LLM. See
  [Watch mode](features/watch-mode.md#alerting).

- **SLO burn-rate signals** (Theme E — slice 3) — the daemon can now track a
  time-weighted availability SLI (`good`/`total` workload-seconds, good
  meaning not flagged — the same predicate the issue tracker uses, over the
  unfiltered census rather than the display list) and report a multi-window
  error-budget burn rate over it, following the Google SRE
  workbook's fixed fast (1h, 14.4×) / slow (6h, 6×) pair. An alert fires only
  when both windows breach at once and both carry at least 60% coverage, so a
  daemon that just restarted — state is in-memory only — cannot page on its
  own warm-up; `kubeagent_slo_window_coverage_ratio` shows that happening.
  Five new Prometheus series render only once `--slo-target` is set, and the
  burn alert reuses the existing sink rather than the per-object tracker, so
  it never appears in `/issues` or `kubeagent_issues_*`. Off by default; no
  new RBAC. See
  [Watch mode](features/watch-mode.md#slo-burn-rate).

- **On-incident `--explain`** (Theme E — slice 4) — opt-in, rate-limited
  explanations for `watch`: when an object breaks, the daemon sends a second,
  model-written message a few seconds after the object's alert — likely
  cause, how to confirm, and the deterministic fix kubeagent already computed
  — through the same webhook sink as a follow-up notification, so retry,
  backoff, and URL redaction all apply unchanged and the page itself never
  waits on the model. A per-object cooldown (default `1h`) and an hourly
  token bucket (default `20`, capacity equal to the rate) bound the spend,
  and a restart explains nothing from its first snapshot so a crash-looping
  daemon can't spend its budget re-explaining pre-existing problems. Five
  `kubeagent_explain_*` series and a read-only `/explanations` endpoint make
  the throttling visible; works against a local OpenAI-compatible model via
  `KUBEAGENT_EXPLAIN_ENDPOINT`, with the API key wired from a Secret in the
  chart and no flag ever accepting it. The read-only invariant is enforced by
  the explainer's type signature, which takes no Kubernetes client — the
  daemon stays strictly read-only toward the cluster in every configuration;
  the model call itself is outbound HTTP once enabled. See
  [Watch mode](features/watch-mode.md#on-incident-explanations-explain).

- **Multi-cluster hub** (Theme E — slice 5) — `kubeagent watch --context
  prod-eu --context prod-us` runs one informer set per cluster inside a single
  process, behind one HTTP endpoint. `--context` is repeatable, `--cluster-name`
  names the default cluster (the one watched with no `--context`), and
  `--include-local` adds it alongside the listed contexts. Every metric series
  carries a `cluster` label (defaulting to `local` so single-cluster queries
  keep working), `/issues` and `/explanations` carry a `cluster` field,
  `/issues` gains a `clusters` roster with each target's up/down state, and
  every alert names its cluster — the alert and explanation series themselves
  stay unlabelled, since there is one sink and one budget per process, not per
  cluster. A context missing from the kubeconfig is fatal at startup; a
  cluster that fails at runtime reports `kubeagent_cluster_up 0` and degrades
  on its own while the others keep reconciling. `/readyz` reports ready once
  every cluster has finished a first reconcile attempt and never flips on
  cluster health after that — readiness answers "can this process serve?",
  not "is everything fine." The Helm chart gained `multicluster.*`: a
  kubeconfig mounted read-only from a Secret, never a `values.yaml` value.
  The daemon remains strictly read-only toward every cluster it watches, and
  this slice adds no new RBAC — remote access rides entirely on the
  credentials inside the mounted kubeconfig. **Completing Theme E.** See
  [Watch mode](features/watch-mode.md#watching-several-clusters).
- **MCP server (`kubeagent mcp`)** — serves kubeagent's deterministic, read-only
  diagnosis to other AI agents over MCP on stdio: `kubeagent_triage`,
  `kubeagent_inspect`, `kubeagent_advisory`, and (only with
  `--allow-context-switch`) `list_contexts`. There is no write path and no model
  call anywhere in the server, and kubeconfig paths never reach a caller.
  **Theme G — slice 1.** See [MCP server](features/mcp.md).
- **`kubectl` plugin (krew)** — kubeagent installs as a `kubectl` plugin through
  [krew](https://krew.sigs.k8s.io), so `kubectl kubeagent scan` works anywhere
  `kubectl` does. Releases now carry four platform archives (linux and macOS ×
  amd64 and arm64) and a krew manifest rendered from those archives' checksums;
  the binary is unchanged apart from usage text that names whichever command you
  typed. Not in the upstream krew-index yet, so install is by `--manifest-url`.
  **Theme G — slice 2.** See [Install](install.md).
- **CI/CD gate mode (`kubeagent gate`)** — a pipeline-friendly subcommand with a
  stable five-code exit contract (`0` pass, `1` fail, `2` inconclusive, `3`
  timeout, `4` usage) and a SARIF 2.1.0 renderer for GitHub code scanning.
  `gate` with no `--wait-for` is a pre-deploy sanity check; `gate --wait-for
  deployment/api -n prod` waits for that rollout to settle and judges only the
  findings attributable to it. "kubeagent could not see the cluster" is its own
  exit code, never a silent pass — the escape hatch is explicit
  (`--allow-partial-read <resource>`, or `kubeagent gate || [ $? -eq 2 ]`).
  Read-only, and no LLM call on any gate path. **Theme G — slice 3.** See
  [CI/CD gate](features/ci-gate.md).
- **Verifiable releases** — keyless cosign signatures over `SHA256SUMS` and the
  container image, an SPDX SBOM, SLSA build provenance, and byte-reproducible
  archives, all checkable without a key — see [Verifying a release](verify.md)

!!! info "Version history"
    [GitHub Releases](https://github.com/imantaba/kubeagent/releases) and the
    [CHANGELOG](https://github.com/imantaba/kubeagent/blob/main/CHANGELOG.md)
    are the source of truth for what shipped in each version.

---

## Where kubeagent is headed

The goal is the **most trustworthy Kubernetes troubleshooting agent that exists**:
it tells you what is actually broken, why, and (when you ask) how to fix it —
deterministically, with evidence for every claim, and without ever surprising the
cluster.

### Principles that don't change

These are the north star; every item below is measured against them.

1. **Evidence-first & deterministic.** Every finding cites the exact signal it saw
   and is reproducible. The core works fully offline, with no LLM and no API key.
2. **Zero false positives is a feature.** Alert fatigue is the enemy. Findings are
   confidence-ranked, "expected/parked" states are understood, and the golden
   snapshot + chaos gate defend the signal on every release.
3. **Read-only by default.** Writes exist only behind `--fix`: a fixed allowlist,
   protected namespaces, per-action confirmation, re-verify — and **never**
   model-decided.
4. **Privacy by construction.** No secrets, pod IPs, or env values ever leave the
   process; the LLM path is opt-in and redaction-checked, with a local-model
   option on the way.
5. **One fast binary, minimal dependencies.** No agent sprawl, no control plane to
   babysit.

### Themes (each spans several releases)

- **A · Root-cause, not symptoms** ✅ — correlate findings across the resource graph
  (Deployment → ReplicaSet → Pod → Node; Service → EndpointSlice → Pod; Ingress →
  Service → backend; PVC → PV → StorageClass) so a wall of red collapses to the one
  thing that's actually wrong, with a confidence score per finding. Theme A is
  complete: the chain closed with the PVC-provisioning root cause.
- **B · Deeper & broader diagnosis** ✅ — more failure modes: admission-webhook
  latency, CoreDNS/DNS health, control-plane & etcd health. Theme B is complete;
  new detectors still land continuously, they just no longer belong to a theme.
- **C · Principled intelligence** ✅ — `--explain` grows from a summary into ranked,
  deterministic *remediation suggestions* and on-call runbooks; an opt-in read-only
  investigation mode lets the model request bounded, allow-listed follow-up reads
  (logs, describe, events) to deepen a finding — the deterministic core never
  changes, and every query is logged. Local-model (offline) explain. Theme C is
  complete, closed by `--investigate`.
- **D · Remediation that earns trust** ✅ — `--fix` gains plan/dry-run with a
  diff, an audit log, RBAC preflight (only offer what the caller can actually
  do), and rollback (`--rollback`). Theme D is complete; guarded, policy-gated
  autonomous remediation inside `watch` moves to Theme E.
- **E · Continuous operations** ✅ — `watch` gains state (regressions, flapping,
  MTTR, "new since last"), webhook alerting (JSON / Slack / Alertmanager
  shipped; PagerDuty remains an open receiver), SLO burn-rate signals
  (shipped), rate-limited on-incident `--explain` (shipped), and a
  multi-cluster hub (shipped). Theme E is complete; guarded, policy-gated
  autonomous remediation inside `watch` is a separate, future track.
- **F · Ecosystem & operators** ✅ — first-class awareness of the operators
  people actually run, in three slices: operator/CRD adapters for
  cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, and the Prometheus
  operator; reconciler-reported GitOps drift (`--drift`); and advisory
  scheduling-headroom + structural right-sizing hints (`--capacity`). Theme F
  is complete.
- **G · Meet people where they work** — an **MCP server** so other AI agents can
  call kubeagent's read-only diagnosis as a trusted tool (shipped, `kubeagent
  mcp`); a **`kubectl` krew plugin** (shipped, `kubectl kubeagent`); a **CI/CD
  gate mode** (shipped, `kubeagent gate` — pre-deploy sanity, post-deploy
  verify, SARIF, exit codes); a **shareable HTML report** (shipped, `scan
  --output html`); and an **interactive TUI** (shipped, `kubeagent tui`). An
  optional in-cluster dashboard remains ahead.
- **H · Supply-chain & trust** — signed releases, SBOM and build provenance
  (shipped: keyless cosign signatures, an SPDX SBOM, SLSA build provenance and
  byte-reproducible archives — see [Verifying a release](verify.md));
  **per-feature least-privilege RBAC** (shipped: `kubeagent rbac print` and
  `kubeagent rbac check`, and every RBAC manifest generated from one feature
  table — see [Least-privilege RBAC](features/rbac.md)). Fuzzed detectors
  remain ahead.

### Milestones

Versions are **theme markers, not date commitments** — each release ships when its
work clears the spec → plan → subagent-driven build → chaos/smoke gate pipeline,
one guarded step at a time. Roughly:

| Milestone | Theme | Highlights |
|-----------|-------|------------|
| **v0.29–v0.31** | Root-cause correlation (A, B) | Resource-graph causality chaining; per-finding **confidence score** (text + JSON); new detectors — certificate expiry, stuck-terminating resources, PDB-blocked drains, HPA-can't-scale, admission-webhook & DNS/CoreDNS health |
| **v0.32–v0.35** | Principled intelligence & safer fixes (C, D) | `--explain` → ranked remediation suggestions + runbooks; opt-in read-only `--investigate`; local-model explain; `--fix` plan/dry-run + diff + audit log + RBAC preflight + rollback; larger reversible allowlist |
| **v0.36–v0.40** | Continuous operations (E, D) | Stateful `watch` (trends, flapping, MTTR, new-since-last); Slack/PagerDuty/webhook alerts; SLO burn-rate; on-incident `--explain`; multi-cluster hub; guarded autonomous remediation |
| **v0.41–v0.45** | Ecosystem & operators (F) | Operator/CRD adapters (CNPG, cert-manager, Longhorn, Argo/Flux, mesh); GitOps drift; cost/right-sizing; deep networking & storage checks |
| **v0.5x** | Interfaces & adoption (G) | **MCP server** (shipped, `kubeagent mcp`); **`kubectl` krew plugin** (shipped); **CI/CD gate mode + SARIF** (shipped, `kubeagent gate`); **shareable HTML report** (shipped, `scan --output html`); **interactive TUI** (shipped, `kubeagent tui`); optional in-cluster dashboard |
| **v1.0** | Production-grade contract (H) | Stable versioned JSON schema; cosign-signed releases + SBOM + provenance; per-feature least-privilege RBAC; cross-version/distro chaos matrix; a **detector/plugin SDK** and policy-as-code custom checks; the two v1 simplifications (stdlib-`flag` CLI, sequential scan) retired deliberately — Cobra + bounded scan concurrency — behind the same test bar |
| **post-1.0** | The best, sustained | Anomaly/baseline learning ("what's normal for *this* cluster"); fleet-scale (hundreds of clusters); a curated community detector library and known-issues knowledge base |

### How we keep it the best

The features are only half of it. The moat is the discipline behind them: **every
change is TDD'd, reviewed by an independent pass, and gated on a golden-output
snapshot plus a chaos suite that injects real outages** before it can ship. New
detectors are pure functions with fake-object tests; anything that touches the
cluster gets the full chaos gate. That is what lets kubeagent add breadth without
ever trading away the signal-to-noise that makes it worth running.

> Have a failure mode kubeagent should catch, or an integration you'd reach for
> first? [Open an issue](https://github.com/imantaba/kubeagent/issues) — real
> incidents are the best roadmap input there is.
