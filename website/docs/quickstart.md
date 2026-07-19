# Quickstart

`kubeagent` is a read-only Kubernetes troubleshooting CLI that tells you **why**
your workloads are broken, not just that they are. A single `scan` diagnoses the
common pod failure modes — CrashLoopBackOff, ImagePullBackOff / ErrImagePull,
OOMKilled, Pending / Unschedulable, VolumeAttachError, silent restart loops, failing readiness/liveness/startup probes, and failing init containers —
and names the underlying cause for each (the container exit reason, the scheduler's
message, the failed image pull). It also checks cluster and node health (NotReady
nodes *with* their root cause, stale kubelet heartbeats, a declared expected-node
baseline, and kubelet resource reservations) and runs a set of advisory checks:
broken Ingress routes, Services with no endpoints, credentials stored in the clear,
PVCs on a `Delete` reclaim policy, PVCs stuck provisioning, workload security posture, node disk usage, and a
kubelet `/healthz` probe.

The report is **prioritized**: cluster health (P1 — nodes and kube-system) first,
then workload/pod failures (P2). Healthy workloads, restart-only workloads, and
CronJobs are hidden by default. Everything is read-only and works offline; opt-in
extras add a plain-English `--explain` summary (one Claude API call), guard-railed
`--fix` remediation, `--output json`, and an in-cluster [`watch`](features/watch-mode.md)
daemon that exposes Prometheus metrics.

## Build and run

```bash
go build -o kubeagent .

# scan: prioritized problem report — cluster health (P1) then workload failures (P2)
./kubeagent scan
```

## Flags

```bash
# also show workloads that are healthy now but have restarted
./kubeagent scan --include-restarts

# also show CronJobs
./kubeagent scan --include-cron

# pick a context and scope to one namespace, emit JSON
./kubeagent scan --context my-cluster -n my-namespace --output json

# point at a specific kubeconfig file
./kubeagent scan --kubeconfig /path/to/config

# summarize the findings in plain English (needs ANTHROPIC_API_KEY)
export ANTHROPIC_API_KEY=sk-ant-...
./kubeagent scan --explain

# choose the model (default: claude-opus-4-8; or set KUBEAGENT_MODEL)
./kubeagent scan --explain --model claude-sonnet-4-6

# flag credentials stored in the clear (ConfigMaps and pod env literals)
./kubeagent scan --lint-secrets

# read a crashing container's previous logs and classify the failure
./kubeagent scan --logs
```

See [Credential lint](features/credential-lint.md) for details on what
`--lint-secrets` checks and how findings are reported.

!!! note "`--explain` privacy"
    `--explain` sends **only** a structured summary to the Claude API: the
    cluster-health verdict (node counts, and the names of unhealthy nodes when
    degraded) and, for the notable workloads, their namespace, name, kind,
    ready/desired counts, status, restart count, and any detector issue. It
    never sends raw pod specs, pod IPs, environment variables, or secrets.
    Without `--explain`, kubeagent makes no external calls.

    Model precedence for `--explain`: the `--model` flag, then the
    `KUBEAGENT_MODEL` environment variable, then the default `claude-opus-4-8`.

## Example output

A scan of a cluster with several problems, run with the opt-in advisory checks
(`--security`, `--lint-secrets`, `--expected-nodes`) so every section is shown. The
verdict and node health come first (P1), then the failing workloads (P2), then the
advisory `SECURITY`, `NOTES`, and `CONTEXT` blocks:

```text
$ kubeagent scan --security --lint-secrets --expected-nodes cp-0,worker-1,worker-2,payments-db-01
Cluster: Degraded — 3/3 nodes Ready
  ✗ node worker-2 SchedulingDisabled
  ✗ node payments-db-01 expected but absent from the cluster
  Needs attention: 3 workloads failing · 1 service without endpoints · 1 ingress route broken

NEEDS ATTENTION
✗ shop/api  Deployment  0/1 Degraded
    image nginx:9.9.9-does-not-exist
    ⚠ ImagePullBackOff: Bad image reference or registry authentication
      ↳ container "api": Back-off pulling image "nginx:9.9.9-does-not-exist": not found
    ↳ changed: rollout to revision 2, 1m ago · image nginx:1.27-alpine → nginx:9.9.9-does-not-exist
    api-7cdbc7fdf7-htfzc  0/1  Pending  restarts=0  worker-1  10.244.2.4  1m
✗ shop/billing-worker  Deployment  0/1 Degraded  · 4 restarts, last 27s ago
    image polinux/stress
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting
      ↳ container "worker", restartCount=4
    ⚠ OOMKilled: Container exceeded its memory limit and was killed
      ↳ container "worker", exitCode=137
      resources: memory req=32Mi limit=64Mi · cpu req=unset limit=unset
    ↳ changed: rollout to revision 1, 2m ago
    billing-worker-7c7df46f98-vbgd7  0/1  Running  restarts=4 (27s ago)  worker-2  10.244.1.2  2m
✗ shop/web  Deployment  0/1 Degraded  · 4 restarts, last 38s ago
    image busybox:1.36
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting
      ↳ container "web", restartCount=4
    ↳ changed: rollout to revision 1, 2m ago
    web-5b85758b4c-8fskg  0/1  Running  restarts=4 (38s ago)  worker-1  10.244.2.2  2m
  ✗ shop/payments  NodePort  no ready endpoints
  ✗ shop/app-config  ConfigMap[AWS_SECRET_ACCESS_KEY]  AWS access key
  ✗ ingress shop/storefront  shop.example.com/  backend Service payments:80 has no ready endpoints (likely 502/503)

SECURITY  (advisory — does not affect the cluster verdict)
  2 baseline · 1 exposed service · 18 restricted hardening gaps · 7 workloads

  ✗ shop/legacy-agent  Deployment
      [baseline] HostPath — mounts hostPath /var/run (writable host filesystem)
      [baseline] Privileged — container "agent" runs privileged (full host access)
  ✗ shop/payments  Service
      [kubeagent] ExposedService — type NodePort exposes port(s) 80 externally

  restricted (hardening gaps, near-universal): 18 across 6 workloads
    RunAsRoot ×6 · AllowPrivilegeEscalation ×6 · CapabilitiesNotDropped ×6
    → run with --security-verbose to list every finding per workload

NOTES
  • 3 nodes reserve no memory: cp-0, worker-1, worker-2
      — OS/kubelet memory pressure can destabilize the node
  • 3 nodes reserve no ephemeral-storage: cp-0, worker-1, worker-2
      — disk pressure can destabilize the node
  • 1 PVC on Delete reclaim policy — standard ×1   [--pvc-reclaim]

CONTEXT
Kubelet reservations (combined kube+system)
  memory            3 of 3 nodes reserve none  ⚠
  cpu               3 of 3 nodes reserve none
  ephemeral-storage 3 of 3 nodes reserve none  ⚠
Resources (cluster):
  CPU     36.0 cores · req 1.1 (3%) · lim 0.3 (0%)
  Memory  117Gi · req 422Mi (0%) · lim 554Mi (0%)
  (usage: metrics-server unavailable)

Platform: local-path storage · Kubernetes v1.34 · containerd
```
