# kubeagent

A Kubernetes troubleshooting agent, written in Go.

`kubeagent` scans a Kubernetes cluster, finds unhealthy pods, and explains
*why* they're failing — covering the most common pod failure modes:

- **CrashLoopBackOff** — container keeps restarting
- **ImagePullBackOff / ErrImagePull** — bad image or registry auth
- **OOMKilled** — container hit its memory limit
- **Pending / Unschedulable** — no node can place the pod

It talks to the cluster directly via the official Kubernetes Go client
(`client-go`) — the same library `kubectl` and operators are built on — and
operates **read-only**.

## Status

✅ **v1 shipped** — `kubeagent scan` performs a read-only, whole-cluster scan and
reports CrashLoopBackOff, ImagePullBackOff/ErrImagePull, OOMKilled, and
Pending/Unschedulable pods, in text or JSON.

✅ **v2 shipped** — an optional `--explain` flag makes a single Claude API call
(via the official Go SDK) to summarize findings in plain English. The
deterministic core still works offline with no API key.

## Usage

```bash
go build -o kubeagent .

# scan: a prioritized problem report — cluster-health verdict (P1: nodes +
# kube-system) first, then workload/pod failures (P2). Healthy, restart-only,
# and CronJob workloads are hidden by default.
./kubeagent scan

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
```

> `--explain` sends **only** a structured summary to the Claude API: the
> cluster-health verdict (node counts, and the names of unhealthy nodes when
> degraded) and, for the notable workloads, their namespace, name, kind,
> ready/desired counts, status, restart count, and any detector issue. It never
> sends raw pod specs, pod IPs, environment variables, or secrets. Without
> `--explain`, kubeagent makes no external calls.
>
> Model precedence for `--explain`: the `--model` flag, then the
> `KUBEAGENT_MODEL` environment variable, then the default `claude-opus-4-8`.

Run the tests with `go test ./...`.

## Roadmap

- **v1** — `kubeagent scan`: deterministic whole-cluster scan + diagnosis
- **v2 (shipped)** — optional `--explain` flag: one Claude API call summarizes findings

## Design

See [docs/design.md](docs/design.md).
