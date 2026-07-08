# Changelog

All notable changes to kubeagent are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **Redesigned `scan` text output.** The human-readable output is now organized
  by severity into **NEEDS ATTENTION** (failing workloads, dead Services,
  credential warnings), **NOTES** (advisories — Delete-policy PVCs, expected-empty
  Services, hidden-workload counts), and **CONTEXT** (nodes/reservations,
  resources, platform), with a workload-scoped "Needs attention" line under the
  cluster verdict. All-OK node reservations collapse to one line, and
  Delete-policy PVCs show as a grouped summary — pass `--pvc-reclaim` for the full
  per-PVC list. `--output json` is unchanged, and `--fix` behavior is unchanged.

## [0.13.0] - 2026-07-08

### Added

- **PVC reclaim-policy check.** `scan` now lists Bound PersistentVolumeClaims
  whose bound PersistentVolume has `reclaimPolicy: Delete` — the data-loss-prone
  case where deleting the PVC or PV destroys the underlying storage. Shown as a
  "PVCs with reclaim policy Delete" section (text + JSON `pvcReclaim`) and, in
  the watch daemon, as the gauge `kubeagent_pvcs_reclaim_delete`. Reads PVCs and
  their bound PVs (two new read-only RBAC grants); advisory only (does not change
  the cluster verdict).

## [0.12.0] - 2026-07-08

### Added

- **Node reservation check.** `scan` now reports each node's aggregate kubelet
  reservation (`Capacity − Allocatable`, i.e. kube-reserved + system-reserved +
  eviction-hard combined) and warns when a node reserves **no memory** —
  a kubelet that can be OOM'd under pressure. Shown as a "Node reservations"
  section (text + JSON `nodeReserve`) and, in the watch daemon, as the gauge
  `kubeagent_nodes_without_reservations`. Read-only; no new RBAC. Advisory only
  (does not change the cluster verdict).

- **Helm chart.** The in-cluster watch daemon is now packaged as a Helm chart
  under `deploy/helm/kubeagent/`, alongside the raw manifests. It renders the
  identical read-only RBAC (`get`/`list`/`watch` only), deployment, and metrics
  Service, with image, replicas, watch cadence, metrics port, RBAC/ServiceAccount
  creation, resources, security context, and scheduling exposed as values.

## [0.11.0] - 2026-07-07

### Added

- **Restart-loop detection.** A new `RestartLoop` finding flags a container that
  keeps exiting with a non-OOM error and restarting (`RestartCount ≥ 3`, current
  run younger than 10 min) even though it is currently `Running` — a flapping pod
  the point-in-time detectors (`CrashLoopBackOff`/`OOMKilled`) miss. Durable
  (reads `RestartCount` + `lastState.Terminated`), so it appears in the scan,
  `--explain`, and `kubeagent_findings{issue="RestartLoop"}`. Read-only.

## [0.10.0] - 2026-07-06

### Added

- **Volume-attach detection.** A new `VolumeAttachError` finding flags a pod stuck
  at container creation because a volume cannot be attached (`FailedAttachVolume`
  Warning event) — most often a **Multi-Attach** error (a ReadWriteOnce volume
  still attached to another node). Detected by reading the pod's events (one cheap
  field-selected List; the watch daemon needs no events informer). Read-only; the
  daemon's RBAC gains `events` read.

## [0.9.0] - 2026-07-05

### Added

- **Daemon watch mode (`kubeagent watch`).** Run kubeagent in-cluster as a
  strictly read-only daemon: it watches the cluster via informers, re-runs the
  deterministic diagnosis on change (debounced) plus a heartbeat, and exposes the
  result as structured logs and hand-rolled Prometheus `/metrics` (with `/healthz`
  and `/readyz`). No cluster writes, no LLM calls, no new dependency. Read-only
  RBAC and Deployment manifests are in `deploy/`. (Multi-cluster, Kubernetes
  Events, `--explain`, and autonomous remediation are planned for later phases.)
- **Dockerfile.** A multi-stage build producing a small distroless, non-root
  image for running the daemon in-cluster (used by `deploy/deployment.yaml`).

## [0.8.0] - 2026-07-04

### Added

- **"What changed" rollout awareness.** A flagged Deployment now shows its most
  recent rollout when it is recent (within 7 days) — the revision, its age, and
  the image delta (`↳ changed: rollout to revision 6, 4d ago · image A → B`) — in
  text, JSON (`rollout`), and `--explain`. Deterministic and read-only (reuses
  the ReplicaSets already collected); factual, with no causal claim.

### Changed

- **`--fix` `RolloutUndo` is more conservative.** A Deployment rollback is now
  proposed only when the Deployment is **degraded** (fewer ready replicas than
  desired). A rollout stuck on `ImagePullBackOff` while its previous revision is
  still fully serving is left alone (the failure still shows in the scan and
  `--explain`; only the automatic rollback proposal is withheld).

## [0.7.0] - 2026-07-01

### Added

- **`--fix` remediation: `Uncordon`.** A second guard-railed action — an
  accidentally-cordoned node (`SchedulingDisabled`, no `NoExecute` taint) is made
  schedulable again after a per-action confirmation. Same rails as `RolloutUndo`
  (allowlist, apply-time precondition re-check, single write, never LLM-decided).

### Changed

- **Sharper `--explain`.** The `--explain` prompt now instructs a consistent,
  scannable structure (per issue: root cause → checks → fix; cluster/kube-system
  problems before workloads) and is grounded strictly in the scan's facts (told
  not to invent causes), reducing misattributed root causes. Still opt-in,
  read-only, structured-facts-only, and independent of `--fix`.

## [0.6.0] - 2026-07-01

### Added

- **`--fix` remediation (opt-in).** `scan --fix` proposes and, after a per-action
  `[y/N]` confirmation, applies safe reversible remediations (`--dry-run` to
  preview, `--yes` for non-interactive). v1 ships `RolloutUndo` (roll a Deployment
  with a failed image rollout back to its previous revision). Guard-railed:
  allowlist, protected namespaces, apply-time precondition re-check, re-verify;
  never LLM-decided. This is the first feature that can write to the cluster;
  default behavior remains read-only.

## [0.5.0] - 2026-06-30

No changes to the `kubeagent` binary since 0.4.0 — this release adds project
infrastructure (a documentation site and a pre-release chaos-test harness).

### Added

- **Documentation website.** A MkDocs + Material site (landing page, quickstart,
  per-feature docs, install, roadmap), published to GitHub Pages at
  [k8sproject.top](https://k8sproject.top) via a `pages.yml` workflow.
- **Pre-release chaos-test harness.** `chaos/run.sh` spins up a disposable Kind
  cluster (Calico CNI), injects the 10 most common production outages, runs
  `kubeagent scan` against each (adding `--explain` when `ANTHROPIC_API_KEY` is
  set), and writes a results report — a manual gate before each release, wired
  into the release checklist. See `chaos/README.md`.

## [0.4.0] - 2026-06-30

### Added

- **Service backing awareness.** A "no ready endpoints" Service issue is now
  annotated with its backing workload when that workload expects no pods — a
  CronJob/Job, or a DaemonSet/Deployment/StatefulSet scaled to 0 — so these stop
  reading as primary problems (text + JSON `expected`/`backing`). A
  Deployment/StatefulSet with replicas and no endpoints stays a primary issue.

### Fixed

- **Credential lint precision.** `--lint-secrets` no longer flags `*_FILE` env
  vars (which hold a path to a secret file, not the secret itself) or values that
  are dotted version numbers — removing two false-positive classes found in live
  use. Real secret values in `*_FILE`-named vars are still flagged.

## [0.3.0] - 2026-06-29

### Added

- **Connectivity diagnostics.** An unreachable or broken API server now yields an
  actionable diagnosis (down / timeout / TLS-cert / auth / DNS) with a `details:`
  line, instead of a raw transport error.
- **NetworkPolicy awareness.** A degraded workload with no detector finding is
  annotated with the NetworkPolicies selecting its pods (a root-cause hint), in
  text, JSON, and `--explain`.
- **Service / LoadBalancer health.** `scan` flags selector-based Services with no
  ready endpoints and LoadBalancer Services with no external address, in a new
  "Service issues" section (text + JSON) and in `--explain`.
- **Credential lint (opt-in).** `scan --lint-secrets` flags credentials stored in
  the clear (ConfigMap values, pod env literals) by location and pattern — never
  the value, and never sent to `--explain`.

## [0.2.0] - 2026-06-29

### Added

- **Resource context.** OOMKilled findings now show the killed container's CPU
  and memory requests + limits. `scan` also prints a cluster resource summary
  (CPU/memory: allocatable, reserved/requests, limits, and — when metrics-server
  is present — live usage) in text and JSON, and feeds it to `--explain` so the
  model can judge whether to raise a limit or scale out. Live usage is
  best-effort and degrades gracefully when metrics-server is absent.
- **Platform facts.** A second line under the cluster verdict naming the detected
  stack — CNI, ingress, storage provisioner(s), Kubernetes version + distribution,
  container runtime, and cloud — also in JSON (`platform`) and in `--explain`. No
  instance identifiers (e.g. raw `providerID`) are emitted.

### Fixed

- Completed/failed **bare pods** (e.g. a one-shot `kubectl run` pod in
  `Succeeded`/`Completed`) are no longer reported as `Degraded`. A pod-derived
  workload in a terminal phase is now treated like a finished Job (`Complete` /
  `Failed`) instead of being run through the ready/desired health model.
- The release `scan` now emits a warning when a metrics-server response is
  present but malformed (previously silently discarded).

## [0.1.0] - 2026-06-27

### Added

- Initial release. `kubeagent scan`: a read-only, prioritized cluster problem
  report — a P1 cluster-health verdict (nodes + `kube-system`) followed by P2
  workload/pod failures, in text or JSON.
- Deterministic detectors: CrashLoopBackOff, ImagePullBackOff/ErrImagePull,
  OOMKilled, Pending/Unschedulable.
- Workload inventory grouped by controller (Deployment / StatefulSet / DaemonSet /
  Job / CronJob / bare pod) with restart history; `--include-restarts` and
  `--include-cron` opt-ins.
- Optional `--explain` flag: a single Claude API call (official Go SDK)
  summarizing findings in plain English; the deterministic core stays usable
  offline. Model selectable via `--model` / `KUBEAGENT_MODEL`.
- `kubeagent version` subcommand (stamped at release time).
- CI (vet/test/build on push & PR) and a release workflow publishing a
  linux/amd64 tarball + `SHA256SUMS` to a GitHub Release.

[0.13.0]: https://github.com/imantaba/kubeagent/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/imantaba/kubeagent/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/imantaba/kubeagent/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/imantaba/kubeagent/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/imantaba/kubeagent/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/imantaba/kubeagent/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/imantaba/kubeagent/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/imantaba/kubeagent/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/imantaba/kubeagent/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/imantaba/kubeagent/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/imantaba/kubeagent/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/imantaba/kubeagent/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/imantaba/kubeagent/releases/tag/v0.1.0
