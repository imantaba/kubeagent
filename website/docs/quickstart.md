# Quickstart

`kubeagent scan` produces a prioritized problem report: cluster health (P1 —
nodes and kube-system) first, then workload/pod failures (P2). Healthy
workloads, restart-only workloads, and CronJobs are hidden by default.

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

```text
Cluster: Healthy (3/3 nodes ready)
Platform: Cilium CNI · Traefik ingress · Kubernetes v1.29 (RKE2) · containerd · Hetzner Cloud

Resources: CPU allocatable 12000m  reserved 2400m  limits 9600m
           Memory allocatable 24Gi  reserved 3Gi  limits 18Gi

P2 — Workload failures

NAMESPACE   NAME              KIND        READY   STATUS             RESTARTS   ISSUE
production  api-server        Deployment  0/3     CrashLoopBackOff   42         exit code 1
staging     worker            Deployment  1/2     ImagePullBackOff   0          image not found
```
