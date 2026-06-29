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

### Resource context

`scan` prints a compact cluster resource summary (CPU and memory: allocatable,
reserved/requests, limits, and — when metrics-server is installed — live usage),
and annotates each OOMKilled finding with the killed container's requests and
limits. This context is also sent to `--explain` so the model can judge whether
to raise a limit or scale out. Live usage is best-effort: without metrics-server
the summary still shows allocatable/reserved/limits and notes usage as
unavailable. Reading usage is read-only (a single GET on the metrics API).

### Platform facts

`scan` prints a second line under the cluster verdict naming the detected stack —
CNI, ingress, storage provisioner(s), Kubernetes version + distribution, container
runtime, and cloud — for example:

`Platform: Cilium CNI · Traefik ingress · Hetzner CSI (+NFS CSI) storage · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud`

Detection is best-effort and read-only (it lists StorageClasses, IngressClasses,
and kube-system DaemonSets, and reads node info); an unrecognized fact is omitted.
The same summary is included in the JSON output (`platform`) and sent to
`--explain` so the model can give stack-aware advice. No instance identifiers
(e.g. the raw `providerID`) are emitted — only the derived cloud name.

### Service health

`scan` flags Service-level problems a pod scan misses: a selector-based Service
routing to **zero ready endpoints** (selector typo, all backends down) and a
**LoadBalancer Service with no external address** (showing its age so you can
tell provisioning from stuck). These appear in a "Service issues" section (text
and JSON) and are sent to `--explain`. ExternalName and selectorless Services are
skipped. Checks are read-only and honor the scan's `-n` scope.

### NetworkPolicy hints

When a workload is degraded with no detector finding (e.g. pods Running but never
Ready), `scan` names the NetworkPolicies whose podSelector matches its pods —
`⚠ NetworkPolicy: pods selected by deny-all — may be blocking traffic` — and sends
the names to `--explain`. It is a hint, not a verdict: kubeagent does not analyze
the policy rules or know what traffic the pod needs, so it points you at the
policies to check. Read-only, namespace-scoped; only policy names are sent to the
model. (Note: some CNIs, e.g. kindnet, do not enforce NetworkPolicies at all.)

## Install

Prebuilt **linux/amd64** binaries are attached to each
[GitHub Release](https://github.com/imantaba/kubeagent/releases). Download, verify
the checksum, and run:

```bash
VERSION=v1.2.3   # the release you want
base="https://github.com/imantaba/kubeagent/releases/download/${VERSION}"
curl -sSLO "${base}/kubeagent_${VERSION}_linux_amd64.tar.gz"
curl -sSLO "${base}/SHA256SUMS"
sha256sum -c SHA256SUMS
tar xzf "kubeagent_${VERSION}_linux_amd64.tar.gz"
./kubeagent version   # prints the build's version
./kubeagent scan
```

### Cutting a release

Push a version tag — or run the **Release** workflow manually from the Actions
tab with a `version` input:

```bash
git tag v1.2.3
git push origin v1.2.3
```

The release workflow runs the tests, builds
`kubeagent_<version>_linux_amd64.tar.gz` + `SHA256SUMS`, and attaches them to the
GitHub Release. Every push and PR is checked by the CI workflow (vet + test +
build).

> A manual dispatch creates the tag at the current commit of the branch it runs
> on — make sure that branch is at the commit you mean to release before
> dispatching. A pushed tag releases exactly that tagged commit.

## Roadmap

- **v1** — `kubeagent scan`: deterministic whole-cluster scan + diagnosis
- **v2 (shipped)** — optional `--explain` flag: one Claude API call summarizes findings

## Design

See [docs/design.md](docs/design.md).
