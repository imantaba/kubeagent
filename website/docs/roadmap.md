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

- **`--suggest` next steps** (first Theme-C / principled intelligence slice) —
  opt-in `scan --suggest` prints a deterministic, reviewed next-step suggestion
  and a read-only `kubectl` investigation command under each pod finding. Offline
  (no API key), never LLM-decided, and read-only — it prints the command, it
  never runs it. This is the deterministic remediation core that a later
  Theme-C slice will hand to `--explain` for LLM ranking and phrasing (the LLM
  ranks; it never invents the remediation). See
  [Failure diagnostics](features/diagnostics.md).

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

- **A · Root-cause, not symptoms** — correlate findings across the resource graph
  (Deployment → ReplicaSet → Pod → Node; Service → EndpointSlice → Pod; Ingress →
  Service → backend; PVC → PV → StorageClass) so a wall of red collapses to the one
  thing that's actually wrong, with a confidence score per finding.
- **B · Deeper & broader diagnosis** — more failure modes: admission-webhook
  latency, CoreDNS/DNS health, control-plane & etcd health.
- **C · Principled intelligence** — `--explain` grows from a summary into ranked,
  deterministic *remediation suggestions* and on-call runbooks; an opt-in read-only
  investigation mode lets the model request bounded, allow-listed follow-up reads
  (logs, describe, events) to deepen a finding — the deterministic core never
  changes, and every query is logged. Local-model (offline) explain.
- **D · Remediation that earns trust** — `--fix` gains plan/dry-run with a diff,
  an audit log, RBAC preflight (only offer what the caller can actually do), a
  larger *reversible* allowlist, and rollback; then guarded, policy-gated
  autonomous remediation inside `watch`.
- **E · Continuous operations** — `watch` gains state (regressions, flapping, MTTR,
  "new since last"), alerting integrations (Slack / PagerDuty / webhook), SLO
  burn-rate signals, rate-limited on-incident `--explain`, and a multi-cluster hub.
- **F · Ecosystem & operators** — first-class awareness of the operators people
  actually run (CloudNativePG, cert-manager, Longhorn/Ceph, Argo CD / Flux GitOps
  drift, Prometheus operator, service meshes), plus cost/right-sizing and
  scheduling-headroom hints.
- **G · Meet people where they work** — an **MCP server** so other AI agents can
  call kubeagent's read-only diagnosis as a trusted tool; a `kubectl` krew plugin;
  a CI/CD gate mode (pre-deploy sanity, post-deploy verify, SARIF, exit codes); an
  interactive TUI and a shareable HTML report.
- **H · Supply-chain & trust** — signed releases, SBOM and build provenance,
  least-privilege RBAC profiles per feature, and fuzzed detectors.

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
| **v0.5x** | Interfaces & adoption (G) | **MCP server**; `kubectl` krew plugin; CI/CD gate mode + SARIF; interactive TUI + HTML report; optional in-cluster dashboard |
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
