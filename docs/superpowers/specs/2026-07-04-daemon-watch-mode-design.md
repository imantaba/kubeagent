# kubeagent — Design: daemon watch mode (Phase 1)

**Status:** approved design (pre-implementation)
**Date:** 2026-07-04

## Goal

Run kubeagent as an in-cluster, **read-only** workload that continuously watches
a single cluster and surfaces its deterministic health diagnosis as logs and
Prometheus metrics. This is the foundation ("Phase 1") of a longer daemon
roadmap; it deliberately ships the smallest thing that is genuinely useful and
strictly safe.

## Phase decomposition (this spec = Phase 1 only)

1. **Phase 1 (this spec):** single-cluster, read-only daemon — `kubeagent watch`,
   informer-driven evaluation, logs + `/metrics`, no cluster writes.
2. Phase 2: multi-cluster (agent-per-cluster + optional hub).
3. Phase 3: on-incident `--explain` (Anthropic key via Secret, rate-limited,
   de-duplicated; never in the hot loop).
4. Phase 4: optional guarded autonomous remediation (separate, even more
   conservative opt-in).

Phases 2–4 are explicit non-goals here.

## Decisions (from brainstorming)

- **Output = structured logs + Prometheus `/metrics` only.** Zero cluster writes;
  the read-only invariant stays literally true. Kubernetes Events are deferred.
- **Metrics are hand-rolled** (Prometheus text exposition from a stdlib
  `net/http` handler) — **no new module dependency**, consistent with kubeagent's
  minimalism. `prometheus/client_golang` is not taken on.
- **Trigger model = informer-driven + heartbeat, List-based evaluation
  (Approach A).** Informers provide sub-second change detection; a debounce
  coalesces bursts; a long heartbeat (default 60s) is a safety-net re-evaluation.
  Each reconcile reuses the existing List-based pipeline. Reading evaluation
  from informer *listers* (zero re-List) is a deferred Phase-1b optimization.

## Invariants

- **READ-ONLY, absolute.** The daemon makes only `get`/`list`/`watch` calls. No
  writes of any kind in Phase 1 (no Events, no `--fix`). Its RBAC ClusterRole
  grants only read verbs.
- **Deterministic core, no LLM.** No Anthropic/LLM calls anywhere in the daemon
  in Phase 1. Fully offline.
- **No new Go module dependency.** Informers/listers come from the existing
  `k8s.io/client-go`; metrics are hand-rolled; HTTP is stdlib.
- **Concurrency:** this retires the v1 "sequential, no goroutines" rule (informers,
  a heartbeat ticker, and the HTTP server run concurrently). Documented in
  CLAUDE.md and the design notes.

## Component 1 — `kubeagent watch` subcommand + in-cluster client

- New subcommand in `main.go run()`: `kubeagent watch [flags]`.
- Flags (each with an optional `KUBEAGENT_*` env fallback):
  - `--metrics-addr` (default `:8080`) — address for `/metrics`, `/healthz`, `/readyz`
    (`:8080` follows the controller-runtime metrics convention and avoids colliding
    with Prometheus's own `:9090`).
  - `--heartbeat` (default `60s`) — safety-net full re-evaluation interval.
  - `--debounce` (default `2s`) — coalescing window for informer events.
  - `--namespace` / `-n` (default all) — scope, mirroring `scan`.
  - `--kubeconfig` / `--context` — local-dev fallback.
- Client construction (`internal/cluster`): add `NewInClusterOrKubeconfigClient`:
  use `rest.InClusterConfig()` when running in a pod (service-account token
  present); otherwise fall back to the existing kubeconfig path. The current
  `NewClient` is unchanged; the CLI `scan` keeps using it.

## Component 2 — shared evaluation (`internal/scan`)

Extract the orchestration currently inline in `main.go run()` (collect →
diagnose → inventory assemble/prioritize → `netpolicy`/`rollout` annotate →
clusterhealth → svchealth) into a reusable, side-effect-free function:

```go
package scan

type Options struct {
    Namespace       string
    IncludeCron     bool
    IncludeRestarts bool
}

type Result struct {
    Health        clusterhealth.ClusterHealth
    Inventory     inventory.Result
    ServiceIssues []svchealth.Issue
}

func Evaluate(ctx context.Context, client kubernetes.Interface, opts Options) (Result, error)
```

Both the CLI `scan` path and the daemon call `Evaluate`. The CLI keeps its own
printing (`report`), resource-summary, platform-facts, credential-lint, and
`--explain`/`--fix` handling — those stay in `main.go` and are **not** part of the
daemon. Phase 1 keeps `Evaluate` focused on the health picture the daemon needs;
the CLI composes the extras around it. The refactor must be behavior-preserving
for `scan` (verified by the existing `main`/report tests plus a new `Evaluate`
unit test).

## Component 3 — the daemon (`internal/watch`)

`Run(ctx context.Context, client kubernetes.Interface, cfg Config) error`:

- Build a `SharedInformerFactory` (namespace-scoped when `--namespace` is set)
  with informers for pods, deployments, replicasets, nodes, services,
  endpointslices. Add event handlers (add/update/delete) that all call
  `trigger()`.
- **Debounced reconcile:** `trigger()` marks work pending; a single reconcile
  goroutine runs at most once per `--debounce` window, coalescing bursts. The
  heartbeat ticker also calls `trigger()`.
- **Reconcile:** call `scan.Evaluate`, then (a) update the metrics registry
  (Component 4), and (b) emit a structured log line **when the health picture
  changes** vs the previous reconcile (verdict flip, a workload newly flagged or
  cleared, a new finding) — steady-state reconciles are silent to avoid log spam.
- Wait for informer cache sync before serving `/readyz` as ready.
- Graceful shutdown: on `ctx` cancel (SIGTERM/SIGINT), stop informers, drain the
  reconcile, shut down the HTTP server.

## Component 4 — metrics + health (`internal/watch/metrics.go`)

A tiny hand-rolled registry, updated each reconcile under a mutex, rendered as
Prometheus text by a stdlib handler. Metrics:

- `kubeagent_cluster_healthy` (gauge, 1 Healthy / 0 Degraded)
- `kubeagent_nodes_ready`, `kubeagent_nodes_total` (gauges)
- `kubeagent_workloads_flagged` (gauge)
- `kubeagent_findings{issue="<Issue>"}` (gauge per issue type present)
- `kubeagent_service_issues` (gauge)
- `kubeagent_last_scan_timestamp_seconds` (gauge)
- `kubeagent_scan_duration_seconds` (gauge)
- `kubeagent_scans_total` (counter)
- `kubeagent_scan_errors_total` (counter)

HTTP endpoints (stdlib `net/http`): `/metrics` (text exposition), `/healthz`
(always 200 while the process lives), `/readyz` (200 once informers have synced
and at least one reconcile has completed).

## Component 5 — deploy manifests (`deploy/`)

Minimal, kustomize-friendly raw YAML:

- `rbac.yaml`: a **read-only** ClusterRole (`get`/`list`/`watch` on the read
  resources), a ServiceAccount, and a ClusterRoleBinding.
- `deployment.yaml`: a single-replica Deployment running `kubeagent watch`, with
  `readinessProbe`/`livenessProbe` on `/readyz`/`/healthz`, resource
  requests/limits, and `securityContext` (non-root, read-only root FS).
- `service.yaml`: a ClusterIP Service exposing the metrics port (with the
  `prometheus.io/scrape` annotation).
- `deploy/README.md`: how to apply and scrape.

## Testing (TDD)

- **`internal/scan.Evaluate`** (fake clientset): a cluster with a crash-looping
  pod → `Result` with the expected verdict and one flagged workload; a healthy
  cluster → Healthy verdict, no flagged workloads. Confirms the extraction is
  faithful.
- **Metrics rendering** (`internal/watch`): a known `scan.Result` → the expected
  Prometheus text lines (including a `kubeagent_findings{issue="CrashLoopBackOff"}`
  sample and the health gauge). Table-driven.
- **Debounce/coalescing:** a burst of `trigger()` calls within one window →
  exactly one reconcile; a later trigger → a second reconcile. Uses an injected
  reconcile func and a controllable clock/channel (no real sleeps).
- **HTTP handlers** (`httptest`): `/metrics` returns the text and `text/plain`
  content type; `/readyz` is 503 before first sync, 200 after; `/healthz` 200.
- **Change-detection log gate:** two reconciles with the same result → one log
  line (or zero after the first); a changed result → a new line.
- **Graceful shutdown:** cancelling `ctx` returns from `Run` promptly with the
  HTTP server closed.

Real-world validation (outside unit tests): build the image, deploy into a Kind
cluster via `deploy/`, `curl` `/metrics` (expect Healthy), inject a bad-image
rollout, and observe `kubeagent_workloads_flagged` rise and a structured log
line appear; delete the injection and observe it clear.

## Out of scope (explicit non-goals)

- Multi-cluster, Kubernetes Events, `--explain`/any LLM call, autonomous
  remediation (later phases).
- Reading evaluation from informer listers / zero-List reconcile (Phase-1b
  optimization).
- Helm chart, leader election / HA (single replica is fine; the daemon is
  side-effect-free so duplicate replicas would be harmless but are unnecessary).
- Alerting rules / dashboards (the operator wires Prometheus/Alertmanager).
