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

!!! info "Version history"
    [GitHub Releases](https://github.com/imantaba/kubeagent/releases) and the
    [CHANGELOG](https://github.com/imantaba/kubeagent/blob/main/CHANGELOG.md)
    are the source of truth for what shipped in each version.
