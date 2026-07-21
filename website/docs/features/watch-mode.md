# Watch mode (daemon)

`kubeagent watch` runs kubeagent **inside your cluster** as a long-lived,
strictly read-only daemon: it watches the cluster and continuously exposes the
same deterministic diagnosis `scan` produces — as Prometheus metrics and
structured logs.

!!! note
    Watch mode is **strictly read-only**: its RBAC grants only `get`, `list`,
    and `watch` — it can never create, update, patch, or delete anything. It
    makes **no LLM calls** and works fully offline.

## How it evaluates

Watch mode is **event-driven, not polling**. Kubernetes informers stream
changes (pods, deployments, replicasets, nodes, services, endpointslices) to
the daemon; a change triggers a re-evaluation, debounced so a burst of events
becomes one pass. A configurable heartbeat (default 60s) re-evaluates as a
safety net. Detection latency is typically seconds — without hammering the API
server the way a tight poll would.

Every evaluation reuses the same pipeline as `kubeagent scan`: the failure
detectors, cluster/service health, NetworkPolicy hints, and
[what-changed rollout awareness](diagnostics.md#what-changed).

## Metrics

The daemon serves Prometheus text on `--metrics-addr` (default `:8080`):

| Metric | Meaning |
|--------|---------|
| `kubeagent_cluster_healthy` | 1 if the cluster verdict is Healthy, else 0 |
| `kubeagent_nodes_ready` / `kubeagent_nodes_total` | node readiness |
| `kubeagent_workloads_flagged` | workloads currently needing attention |
| `kubeagent_findings{issue="..."}` | current findings by type (e.g. `CrashLoopBackOff`, `ImagePullBackOff`, `OOMKilled`, `VolumeAttachError`, `RestartLoop`) |
| `kubeagent_service_issues` | Service issues (no ready endpoints, LB pending); excludes intentionally-empty (parked) Services |
| `kubeagent_nodes_without_reservations` | Number of nodes whose kubelet reserves no memory (allocatable == capacity) |
| `kubeagent_pvcs_reclaim_delete` | Number of PVCs whose bound PV has reclaimPolicy Delete |
| `kubeagent_node_fs_usage_ratio{node}` | Node root-filesystem usage ratio (opt-in; requires `--disk-usage` / `KUBEAGENT_DISK_USAGE=true`) |
| `kubeagent_volumes_over_disk_threshold` | Number of node filesystems and PVCs at or over `--disk-threshold` (opt-in) |
| `kubeagent_ingress_route_issues` | Number of Ingress routes whose backend Service is missing, has no ready endpoints, or does not expose the referenced port; excludes intentionally-empty (parked) routes |
| `kubeagent_pvc_pending_issues` | Number of PersistentVolumeClaims stuck Pending because provisioning or binding failed |
| `kubeagent_nodes_stale_heartbeat` | Number of Ready nodes whose kubelet lease is stale (kubelet not heartbeating) |
| `kubeagent_nodes_expected_absent` | Number of declared expected nodes that are absent from the cluster (opt-in; requires `--expected-nodes` / `KUBEAGENT_EXPECTED_NODES`) |
| `kubeagent_kubelet_unhealthy` | Number of nodes whose kubelet /healthz reported unhealthy (opt-in; requires `--kubelet-health` / `KUBEAGENT_KUBELET_HEALTH` and the `nodes/proxy` add-on) |
| `kubeagent_certificates_expired` | Number of expired TLS certificates (opt-in; requires `--certs` / `KUBEAGENT_CERTS` and the secrets add-on) |
| `kubeagent_certificates_expiring` | Number of TLS certificates expiring within the warn window (opt-in; requires `--certs` / `KUBEAGENT_CERTS` and the secrets add-on) |
| `kubeagent_resources_stuck_terminating` | Number of Namespaces, Pods, and PVCs wedged in Terminating past two minutes |
| `kubeagent_pdb_blocking_issues` | Number of PodDisruptionBudgets that will block a node drain (unsatisfiable, stale, or blocking) |
| `kubeagent_hpa_scaling_issues` | Number of HorizontalPodAutoscalers that cannot scale as intended (unable, metrics, or capped) |
| `kubeagent_last_scan_timestamp_seconds` / `kubeagent_scan_duration_seconds` | evaluation freshness and cost |
| `kubeagent_scans_total` / `kubeagent_scan_errors_total` | evaluation counters |

`/healthz` and `/readyz` back the liveness/readiness probes; `/readyz` turns
200 only after the informer caches sync and the first evaluation completes.

Logs are **change-gated**: the daemon writes a line only when the health
picture changes (a verdict flip, a workload newly flagged or cleared) — steady
state is silent.

## Run it

```bash
# in-cluster (read-only RBAC + Deployment + metrics Service)
kubectl apply -f deploy/

# or locally against a kubeconfig, for a quick look
./kubeagent watch --kubeconfig ~/.kube/config --metrics-addr :8080
curl localhost:8080/metrics
```

Flags (each with a `KUBEAGENT_*` env fallback): `--metrics-addr` (`:8080`),
`--heartbeat` (`60s`), `--debounce` (`2s`), `--namespace`/`-n` (default all
namespaces), `--node-heartbeat-threshold` / `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD`
(`40s`; `0` disables the kubelet-lease staleness check),
`--expected-nodes` / `KUBEAGENT_EXPECTED_NODES` (comma-separated node names;
unset by default — declares which nodes must be present),
`--kubelet-health` / `KUBEAGENT_KUBELET_HEALTH` (off by default — probes each
node's kubelet `/healthz` via the `nodes/proxy` add-on, the same grant
`--disk-usage` uses; see [Disk-usage check](#disk-usage-check-opt-in)),
`--certs` / `KUBEAGENT_CERTS` (off by default — enables the certificate-expiry
check; requires the secrets add-on `deploy/rbac-certs.yaml` or Helm
`certs.enabled=true`), `--cert-warn-days` / `KUBEAGENT_CERT_WARN_DAYS` (default
`30` — warn window in days).

### Disk-usage check (opt-in)

Set `KUBEAGENT_DISK_USAGE=true` (and optionally `KUBEAGENT_DISK_THRESHOLD`,
default `0.80`) to enable the daemon disk-usage check. This requires the
`nodes/proxy` RBAC add-on — apply `deploy/rbac-diskusage.yaml` or set Helm
`diskUsage.enabled=true`. Without the add-on the daemon stays strictly
`get`/`list`/`watch`. When enabled, the daemon exposes
`kubeagent_node_fs_usage_ratio{node}` and
`kubeagent_volumes_over_disk_threshold` in addition to the standard metrics.

## Alerting

Point Prometheus at the metrics Service (it carries the `prometheus.io/scrape`
annotations) and alert on what matters, e.g.:

```yaml
- alert: KubeagentClusterDegraded
  expr: kubeagent_cluster_healthy == 0
  for: 5m
- alert: KubeagentWorkloadsFlagged
  expr: kubeagent_workloads_flagged > 0
  for: 10m
```

## Roadmap

Watch mode is the first phase of the daemon roadmap. Planned next, each as its
own guarded step: multi-cluster (an agent per cluster reporting to a hub),
on-incident `--explain` (rate-limited, key via a Secret — never in the hot
loop), and opt-in autonomous remediation with stricter rails than the
interactive `--fix`.
