# Changelog

All notable changes to kubeagent are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Planned

Driven by the 2026-06-29 chaos / failure-injection tests
(`docs/testing/2026-06-29-chaos-failure-tests.md`), which confirmed kubeagent's
workload/node/kube-system coverage and surfaced four blind spots. Each will ship
as its own feature:

- **Service / LoadBalancer health** — flag Services with no Endpoints and
  `LoadBalancer` Services stuck without an external address.
- **NetworkPolicy awareness** — when a workload is degraded/not-Ready, note a
  restrictive NetworkPolicy selecting its pods (root-cause hint, not just symptom).
- **Connectivity / control-plane diagnostics** — turn an API-server connection
  failure into a clear, actionable message instead of a raw transport error.
- **Secret / credential lint** — opt-in scan for obviously leaked or risky
  credentials in Secrets / environment variables.

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
