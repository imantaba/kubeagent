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
| `kubeagent_service_issues` | Service issues (no ready endpoints, LB pending) |
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
namespaces).

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
