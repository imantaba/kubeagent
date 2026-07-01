# Changelog

All notable changes to kubeagent are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`--fix` remediation: `Uncordon`.** A second guard-railed action — an
  accidentally-cordoned node (`SchedulingDisabled`, no `NoExecute` taint) is made
  schedulable again after a per-action confirmation. Same rails as `RolloutUndo`
  (allowlist, apply-time precondition re-check, single write, never LLM-decided).

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

[Unreleased]: https://github.com/imantaba/kubeagent/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/imantaba/kubeagent/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/imantaba/kubeagent/releases/tag/v0.1.0
