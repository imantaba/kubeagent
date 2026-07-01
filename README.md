# kubeagent

A Kubernetes troubleshooting agent, written in Go.

📖 **Docs & site:** [k8sproject.top](https://k8sproject.top)

`kubeagent` scans a Kubernetes cluster, finds unhealthy pods, and explains
*why* they're failing — covering the most common pod failure modes:

- **CrashLoopBackOff** — container keeps restarting
- **ImagePullBackOff / ErrImagePull** — bad image or registry auth
- **OOMKilled** — container hit its memory limit
- **Pending / Unschedulable** — no node can place the pod

It talks to the cluster directly via the official Kubernetes Go client
(`client-go`) — the same library `kubectl` and operators are built on — and
operates **read-only by default** (an opt-in `--fix` flag can apply safe, reversible remediations — see below).

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
skipped. A "no ready endpoints" issue whose backing workload expects no pods — a
CronJob/Job, or a DaemonSet/Deployment/StatefulSet scaled to 0 — is annotated
with that backing (e.g. `backs CronJob — expected between runs`) so it does not
read as a primary problem; a Deployment/StatefulSet with replicas and no
endpoints stays primary. Checks are read-only and honor the scan's `-n` scope.

### NetworkPolicy hints

When a workload is degraded with no detector finding (e.g. pods Running but never
Ready), `scan` names the NetworkPolicies whose podSelector matches its pods —
`⚠ NetworkPolicy: pods selected by deny-all — may be blocking traffic` — and sends
the names to `--explain`. It is a hint, not a verdict: kubeagent does not analyze
the policy rules or know what traffic the pod needs, so it points you at the
policies to check. Read-only, namespace-scoped; only policy names are sent to the
model. (Note: some CNIs, e.g. kindnet, do not enforce NetworkPolicies at all.)

### Connectivity diagnostics

When the API server can't be reached, `scan` prints an actionable diagnosis
instead of a raw transport error — distinguishing a down control plane
(connection refused/reset), a timeout, a TLS/expired-certificate problem,
authentication/authorization (401/403), and DNS/wrong-host — followed by a
`details:` line with the underlying error. This is classification only: kubeagent
issues no extra calls and exits non-zero as before.

### Credential lint (opt-in)

`scan --lint-secrets` flags credentials stored in the clear — values in ConfigMaps
and pod env `value:` literals (where a `secretKeyRef` should have been used) that
match a known pattern (AWS key, private key, GitHub token, JWT) or a
credential-like key name with a literal value. It reports only the location and
pattern — **never the value** — and these findings are **never sent to
`--explain`**. Off by default (no ConfigMaps are read without the flag).
Read-only and namespace-scoped.

### Remediation (--fix, opt-in)

By default kubeagent only reads. `scan --fix` additionally proposes safe,
reversible remediations for what it finds and applies each one **only after you
confirm** (`Apply? [y/N]`, default No). Writes are guard-railed: a fixed allowlist
of actions, never in protected namespaces (`kube-system`, `kube-public`,
`kube-node-lease`), preconditions re-checked against live state, and the result
re-verified. Nothing about remediations is sent to `--explain`.

```bash
./kubeagent scan --fix             # propose + confirm each fix
./kubeagent scan --fix --dry-run   # show proposals only; never prompt or write
./kubeagent scan --fix --yes       # apply all proposals without prompting
```

**Remediations:**

- **`RolloutUndo`** — when a Deployment's newest rollout can't pull its image
  (`ImagePullBackOff`/`ErrImagePull`) and a prior revision exists, roll it back to
  that revision (a single, reversible `Deployment` update via client-go).
- **`Uncordon`** — when a node is cordoned (`SchedulingDisabled`) and has no
  `NoExecute` taint (i.e. it's accidentally cordoned, not being drained), make it
  schedulable again (a single `Node` update; reversible with `kubectl cordon`).

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

**Pre-release chaos test.** On a machine with Docker, run the chaos suite on a
disposable Kind cluster and review the report for detection regressions before
tagging (see [chaos/README.md](chaos/README.md)):

```bash
./chaos/run.sh --recreate --teardown          # deterministic
# or, to also exercise --explain:
ANTHROPIC_API_KEY=sk-ant-... ./chaos/run.sh --recreate --teardown
```

Review the generated `docs/testing/*-chaos-results.md` (git-ignored): confirm
each scenario's expected signal still appears, then proceed.

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
