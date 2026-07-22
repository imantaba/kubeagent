<p align="center">
  <img src="docs/kubeagent-logo.svg" alt="kubeagent logo" width="150">
</p>

# kubeagent

> A read-only Kubernetes troubleshooting CLI that tells you **why** your pods are broken — not just that they are.

![kubeagent scan demo](docs/kubeagent-demo.gif)

[![CI](https://github.com/imantaba/kubeagent/actions/workflows/ci.yml/badge.svg)](https://github.com/imantaba/kubeagent/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/imantaba/kubeagent)](https://goreportcard.com/report/github.com/imantaba/kubeagent)
[![Release](https://img.shields.io/github/v/release/imantaba/kubeagent)](https://github.com/imantaba/kubeagent/releases)
[![License](https://img.shields.io/github/license/imantaba/kubeagent)](LICENSE)

**Highlights:**

- 🔒 **Read-only by default** — only `get`/`list`/`watch`, safe against prod. (Opt-in `--fix` applies a fixed allowlist of reversible remediations, each behind a `[y/N]` confirm, never in `kube-system`.)
- 📴 **Deterministic & offline** — the whole diagnostic core needs no API key. AI is strictly opt-in.
- 🤖 **Optional `--explain`** — one Claude API call summarizes findings in plain English (never sends pod specs, env, or secrets).
- 📦 **Single Go binary** — built on `client-go`, the same library `kubectl` uses. No CRDs, no in-cluster agent required.
- 📊 **`watch` daemon** — run it in-cluster for continuous read-only diagnosis with Prometheus metrics.

```bash
go install github.com/imantaba/kubeagent@latest
kubeagent scan
```

📖 **Docs & site:** [k8sproject.top](https://k8sproject.top)

`kubeagent` scans a Kubernetes cluster, finds unhealthy pods, and explains
*why* they're failing — covering the most common pod failure modes:

- **CrashLoopBackOff** — container keeps restarting
- **ImagePullBackOff / ErrImagePull** — bad image or registry auth
- **OOMKilled** — container hit its memory limit
- **Pending / Unschedulable** — no node can place the pod
- **VolumeAttachError** — a pod stuck at container creation because a volume
  cannot be attached (`FailedAttachVolume`), most often a **Multi-Attach** error
  (a ReadWriteOnce volume still attached to another node).
- **RestartLoop** — a container that keeps exiting with a non-OOM error and
  restarting (≥ 3 restarts, still flapping) even though it is currently Running —
  the case `CrashLoopBackOff` misses.
- **ProbeFailure** — a Running-but-not-Ready pod whose readiness, liveness, or
  startup probe keeps failing; names the probe, container, and reason.
- **InitContainer failures** — a pod stuck in its init phase because an init
  container is crash-looping, can't pull its image, or was OOM-killed; names which
  init container and why.
- **JobFailed** — a Job or CronJob whose run failed (exhausted retries or hit its
  deadline); a failing CronJob is shown even without `--include-cron`.
- **FailedCreate** — a workload whose controller cannot create pods because a
  ResourceQuota, LimitRange, or admission webhook is rejecting them.
- **Node reservation check** — warns when a node's kubelet reserves no memory
  (allocatable == capacity), meaning OS or kubelet memory pressure can destabilise
  the node. Advisory and read-only; no new RBAC.
- **PVC reclaim-policy check** — lists Bound PVCs whose PV reclaims with Delete
  (the data-loss-prone default for dynamic provisioners). Advisory and read-only;
  adds PVC + PV read RBAC.
- **Disk-usage check (opt-in)** — `scan --disk-usage` flags node filesystems and
  PVCs at or over `--disk-threshold` (default `0.80`) before the kubelet's
  `DiskPressure` eviction fires. Needs the `nodes/proxy` add-on; off by default.
- **Ingress route health** — `scan` follows each Ingress rule to its backend
  Service and flags routes whose Service is missing, has no ready endpoints
  (`NoEndpoints` — the classic 502/503), or does not expose the referenced port
  (`PortNotExposed`). For a `NoEndpoints` route the Detail also names the backend
  root cause — the selector matches no pods, the matching pods are on a down node,
  or they exist but none are Ready — the same diagnosis as the Service check, one
  hop up the graph. Advisory and read-only; adds Ingress read RBAC.
- **Service root-cause** — For a broken Service (not expected-empty), `scan` names the root cause in the
  Detail line: the selector matches no pods, the matching pods are on a down node,
  or they exist but none are Ready. Read-only correlation over collected pods and
  node health — no new RBAC.
- A Service (or Ingress route) that is empty on purpose — its backend is scaled to zero, or
  it carries `kubeagent.io/expected-empty: "true"` — is shown as a quiet note, not an alert.
- **Pending-PVC provisioning check** — `scan` flags a PersistentVolumeClaim stuck
  `Pending` because provisioning/binding failed (`ProvisioningFailed` /
  `FailedBinding` events), naming the cause. Event-based (like `VolumeAttachError`),
  so the normal `WaitForFirstConsumer` state is never flagged. Advisory and
  read-only; no new RBAC.
- **Stuck-terminating** — a Namespace, Pod, or PVC wedged in Terminating past two minutes, with the blocking finalizer/condition named.
- **PodDisruptionBudget-blocked drains** — flags a PDB that will block a node
  drain: one that can never allow a voluntary eviction (unsatisfiable), whose
  selector matches no pods (stale), or that is blocking evictions on an
  already-degraded workload. Advisory and read-only; the daemon exposes
  `kubeagent_pdb_blocking_issues`. Adds a base `policy/poddisruptionbudgets`
  read grant.
- **HPA-can't-scale detection** — `scan` flags a HorizontalPodAutoscaler that
  is stuck: can't fetch metrics (broken autoscaling), can't scale because its
  target is missing or the scale subresource errors, or is pinned at
  `maxReplicas` while demand exceeds the cap. Advisory and read-only; the daemon
  exposes `kubeagent_hpa_scaling_issues`. Adds a base
  `autoscaling/horizontalpodautoscalers` read grant.
- **Admission-webhook-failure detection** — `scan` flags a Validating/Mutating
  webhook whose `failurePolicy` is `Fail` and whose backing Service is missing
  or has no ready endpoints — it would reject every create/update it intercepts.
  Read-only, advisory, and cluster-wide only (skipped under `--namespace`); the
  daemon exposes `kubeagent_admission_webhooks_failing`. Adds a base
  `admissionregistration.k8s.io` read grant.
- **Root-cause attribution** — when a node is NotReady or its kubelet stops
  heartbeating, workloads with pods on it are attributed to that node ("↳ likely
  caused by node X"); when several workloads fail image pulls from the same
  registry, they are attributed to that registry; when a workload's pod mounts a
  PVC that cannot provision, it is attributed to that PVC — one shared cause
  instead of N disconnected findings.
- **Workload security posture (opt-in)** — `scan --security` flags PSS-aligned
  hardening problems (privileged/insecure containers, exposed Services) in a
  dedicated `SECURITY` section and JSON `securityIssues`. Advisory and
  read-only; needs no new RBAC.
- **Node heartbeat freshness** — `scan` flags a Ready node whose kubelet `Lease`
  has gone stale (kubelet not heartbeating), catching a dark kubelet before the
  node flips to `NotReady`. Tunable via `--node-heartbeat-threshold` (default
  `40s`); adds `leases` read RBAC; on by default.
- **Expected-node baseline (opt-in)** — `scan --expected-nodes name1,name2,…`
  declares the node names you expect; kubeagent flags each declared node absent
  from the cluster (`node name expected but absent from the cluster`), catching
  a node that never registered or dropped out. Read-only; no new RBAC; best on
  clusters with stable node names.
- **Kubelet health probe (opt-in)** — `scan --kubelet-health` probes each node's
  kubelet `/healthz` via `nodes/proxy` and flags a kubelet that is reachable but
  unhealthy (PLEG/runtime failures), the "alive but sick" case heartbeat and
  NotReady checks miss. Reuses the `nodes/proxy` add-on from `--disk-usage`.
- **Certificate expiry (--certs)** — flags expired and soon-expiring TLS certificates (public cert metadata only; never reads keys), with the Ingress routes they front.
- **Crash log root-cause (opt-in)** — `scan --logs` reads a crashing container's
  previous logs and names the failure (panic, connection refused, bad entrypoint, …).
  Needs the `pods/log` grant (`deploy/rbac-logs.yaml`).
- **Finding confidence** — every finding is labelled high (a direct Kubernetes
  state) or medium (a kubeagent heuristic or correlation); the report tags only
  the less-certain ones, and JSON always carries it.

It talks to the cluster directly via the official Kubernetes Go client
(`client-go`) — the same library `kubectl` and operators are built on — and
operates **read-only by default** (an opt-in `--fix` flag can apply safe, reversible remediations — see below).

## Status

✅ **v1 shipped** — `kubeagent scan` performs a read-only, whole-cluster scan and
reports CrashLoopBackOff, ImagePullBackOff/ErrImagePull, OOMKilled,
Pending/Unschedulable, VolumeAttachError (Multi-Attach), RestartLoop, ProbeFailure, init-container failures, failed Jobs/CronJobs, and pod-creation denials (FailedCreate), in text or JSON.

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
> The explanation is structured — per issue, a root cause, read-only checks, and an
> exact fix, with cluster/kube-system problems (P1) before workloads (P2) — and is
> grounded strictly in the scan's facts (the model is told not to invent causes).
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

### What changed

For a flagged Deployment, kubeagent also correlates the problem with its most
recent rollout when that rollout is recent — showing the revision, its age, and
the image change (`↳ changed: rollout to revision 6, 4d ago · image A → B`) so
you can see *what changed* at a glance. It is deterministic and never claims the
rollout caused the failure.

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

- **`RolloutUndo`** — when a Deployment is **degraded** (fewer ready replicas than
  desired) because its newest rollout can't pull its image
  (`ImagePullBackOff`/`ErrImagePull`) and a prior revision exists, roll it back to
  that revision (a single, reversible `Deployment` update via client-go). A rollout
  that is stuck but still serving on its previous revision is left alone.
- **`Uncordon`** — when a node is cordoned (`SchedulingDisabled`) and has no
  `NoExecute` taint (i.e. it's accidentally cordoned, not being drained), make it
  schedulable again (a single `Node` update; reversible with `kubectl cordon`).

### Watch mode (daemon)

`kubeagent watch` runs kubeagent in-cluster as a continuously running,
**strictly read-only** daemon. It opens an informer against the cluster (using
the in-cluster service account, or `--kubeconfig` as fallback), re-runs the
deterministic diagnosis whenever the watch stream signals a change (debounced),
and also re-runs on a periodic heartbeat — emitting findings as structured log
lines and exposing them as Prometheus metrics.

```bash
./kubeagent watch                        # in-cluster defaults
./kubeagent watch --metrics-addr :9090   # metrics/health port (default :8080)
./kubeagent watch --heartbeat 60s        # re-scan interval (default 60s)
./kubeagent watch --debounce 5s          # change-flood cooldown (default 2s)
./kubeagent watch -n my-namespace        # scope to one namespace
```

Endpoints exposed on `--metrics-addr`:

| Path | Description |
| ---- | ----------- |
| `/metrics` | Prometheus metrics (unhealthy pod/node counts, scan duration, etc.) |
| `/healthz` | Liveness probe — returns 200 when the daemon is running |
| `/readyz` | Readiness probe — returns 200 after the first scan completes |

The daemon makes **no cluster writes**, makes **no LLM/Anthropic calls**, and
adds **no new dependencies** (informers are part of `client-go`; metrics are
hand-rolled, no Prometheus library needed). RBAC (`get`/`list`/`watch` only) and
a Deployment manifest are in [`deploy/`](deploy/).

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
