# Watch mode (daemon)

`kubeagent watch` runs kubeagent **inside your cluster** as a long-lived,
strictly read-only daemon: it watches the cluster and continuously exposes the
same deterministic diagnosis `scan` produces — as Prometheus metrics and
structured logs.

!!! note
    Watch mode is **strictly read-only**: its RBAC grants only `get`, `list`,
    and `watch` — it can never create, update, patch, or delete anything. It
    makes **no LLM calls** and works fully offline.

## How it evaluates

Watch mode is **event-driven, not polling**. Kubernetes informers stream
changes (pods, deployments, replicasets, nodes, services, endpointslices) to
the daemon; a change triggers a re-evaluation, debounced so a burst of events
becomes one pass. A configurable heartbeat (default 60s) re-evaluates as a
safety net. Detection latency is typically seconds — without hammering the API
server the way a tight poll would.

Every evaluation reuses the same pipeline as `kubeagent scan`: the failure
detectors, cluster/service health, NetworkPolicy hints, and
[what-changed rollout awareness](diagnostics.md#what-changed).

## Metrics

The daemon serves Prometheus text on `--metrics-addr` (default `:8080`):

| Metric | Meaning |
|--------|---------|
| `kubeagent_cluster_healthy` | 1 if the cluster verdict is Healthy, else 0 |
| `kubeagent_nodes_ready` / `kubeagent_nodes_total` | node readiness |
| `kubeagent_workloads_flagged` | workloads currently needing attention |
| `kubeagent_findings{issue="..."}` | current findings by type (e.g. `CrashLoopBackOff`, `ImagePullBackOff`, `OOMKilled`, `VolumeAttachError`, `RestartLoop`) |
| `kubeagent_service_issues` | Service issues (no ready endpoints, LB pending); excludes intentionally-empty (parked) Services |
| `kubeagent_nodes_without_reservations` | Number of nodes whose kubelet reserves no memory (allocatable == capacity) |
| `kubeagent_pvcs_reclaim_delete` | Number of PVCs whose bound PV has reclaimPolicy Delete |
| `kubeagent_node_fs_usage_ratio{node}` | Node root-filesystem usage ratio (opt-in; requires `--disk-usage` / `KUBEAGENT_DISK_USAGE=true`) |
| `kubeagent_volumes_over_disk_threshold` | Number of node filesystems and PVCs at or over `--disk-threshold` (opt-in) |
| `kubeagent_ingress_route_issues` | Number of Ingress routes whose backend Service is missing, has no ready endpoints, or does not expose the referenced port; excludes intentionally-empty (parked) routes |
| `kubeagent_pvc_pending_issues` | Number of PersistentVolumeClaims stuck Pending because provisioning or binding failed |
| `kubeagent_nodes_stale_heartbeat` | Number of Ready nodes whose kubelet lease is stale (kubelet not heartbeating) |
| `kubeagent_nodes_expected_absent` | Number of declared expected nodes that are absent from the cluster (opt-in; requires `--expected-nodes` / `KUBEAGENT_EXPECTED_NODES`) |
| `kubeagent_kubelet_unhealthy` | Number of nodes whose kubelet /healthz reported unhealthy (opt-in; requires `--kubelet-health` / `KUBEAGENT_KUBELET_HEALTH` and the `nodes/proxy` add-on) |
| `kubeagent_certificates_expired` | Number of expired TLS certificates (opt-in; requires `--certs` / `KUBEAGENT_CERTS` and the secrets add-on) |
| `kubeagent_certificates_expiring` | Number of TLS certificates expiring within the warn window (opt-in; requires `--certs` / `KUBEAGENT_CERTS` and the secrets add-on) |
| `kubeagent_resources_stuck_terminating` | Number of Namespaces, Pods, and PVCs wedged in Terminating past two minutes |
| `kubeagent_pdb_blocking_issues` | Number of PodDisruptionBudgets that will block a node drain (unsatisfiable, stale, or blocking) |
| `kubeagent_hpa_scaling_issues` | Number of HorizontalPodAutoscalers that cannot scale as intended (unable, metrics, or capped) |
| `kubeagent_admission_webhooks_failing` | Number of Validating/Mutating webhooks with `failurePolicy: Fail` whose backing Service is missing or has no ready endpoints |
| `kubeagent_last_scan_timestamp_seconds` / `kubeagent_scan_duration_seconds` | evaluation freshness and cost |
| `kubeagent_scans_total` / `kubeagent_scan_errors_total` | evaluation counters |

`/healthz` and `/readyz` back the liveness/readiness probes; `/readyz` turns
200 only after the informer caches sync and the first evaluation completes.

Logs are **change-gated**: the daemon writes a line only when tracked issue
state changes — an issue going new, resolving, or starting to flap — not on
every reconcile. See "Issue tracking" below for the exact line shapes.

## Issue tracking (state across reconciles)

Watch mode remembers issue state across reconciles, so each cycle reports
*what changed* instead of re-announcing the whole picture every time.

### What the daemon remembers

Each tracked issue is identified by `(kind, namespace, name, issue)` — for
example `Deployment/shop/web:CrashLoopBackOff`, or the cluster-scoped
`Node/worker-2:KubeletUnhealthy` (no namespace). Every reconcile projects the
evaluation into that set of keys and folds it into an in-memory tracker, which
reports the transitions: which issues are newly firing, which resolved since
the last cycle, and which are flapping (firing repeatedly within a short
window).

A failed evaluation never touches this state: an API error is not "all
clear," so it neither resolves the issues that were firing nor re-fires them
once the API recovers — only a successful evaluation updates the tracker.
(The evaluation error is still logged and still counted in
`kubeagent_scan_errors_total`.)

### Log output

The daemon logs one line per transition, in this order, followed by a summary
line — and only when something changed:

```
kubeagent: NEW Deployment/shop/web:CrashLoopBackOff
kubeagent: RESOLVED Deployment/shop/web:CrashLoopBackOff (fired for 4m12s)
kubeagent: FLAPPING Deployment/shop/web:CrashLoopBackOff (3 firings in 30m0s)
kubeagent: cluster Degraded (2/3 nodes ready) — 4 issue(s) active, 1 new, 1 resolved
```

A reconcile where nothing changed — the same issues still firing, nothing new,
nothing resolved, nothing newly flapping — logs **nothing at all**. Steady
state stays quiet.

### Metrics

Alongside the point-in-time gauges above, the daemon exposes ten series that
track issue lifecycle:

| Metric | Type | Meaning |
|--------|------|---------|
| `kubeagent_issues_active` | gauge | Issues currently firing, tracked across reconciles |
| `kubeagent_issues_flapping` | gauge | Active issues that have crossed the flap threshold |
| `kubeagent_issues_new_total` | counter | Issue firings observed since start |
| `kubeagent_issues_resolved_total` | counter | Issue firings that resolved since start |
| `kubeagent_issues_flapping_total` | counter | Times an issue crossed the flap threshold since start |
| `kubeagent_issues_dropped_total` | counter | New issues left untracked because the tracker is at capacity |
| `kubeagent_issue_resolution_seconds_sum` | counter | Seconds issues spent firing before resolving (MTTR numerator) |
| `kubeagent_issue_resolution_seconds_count` | counter | Issue firings that resolved (MTTR denominator) |
| `kubeagent_issue_active{kind,namespace,name,issue}` | gauge | 1 while this issue instance is firing |
| `kubeagent_issue_age_seconds{kind,namespace,name,issue}` | gauge | Seconds since this issue instance started firing |

There is no dedicated MTTR series — compute mean time to resolution as
`kubeagent_issue_resolution_seconds_sum / kubeagent_issue_resolution_seconds_count`.

### `/issues`

`GET /issues` on `--metrics-addr` returns the same tracked state as JSON:

```bash
curl -s localhost:8080/issues | jq .
```

```json
{
  "active": [
    {
      "kind": "Deployment",
      "namespace": "shop",
      "name": "web",
      "issue": "CrashLoopBackOff",
      "firstSeen": "2026-07-25T10:00:00Z",
      "firingSince": "2026-07-25T10:12:00Z",
      "lastSeen": "2026-07-25T10:16:12Z",
      "firings": 2,
      "flapping": false,
      "ageSeconds": 252
    }
  ],
  "resolved": [
    {
      "kind": "Service",
      "namespace": "shop",
      "name": "api-svc",
      "issue": "NoEndpoints",
      "firstSeen": "2026-07-25T10:00:00Z",
      "firingSince": "2026-07-25T10:00:00Z",
      "lastSeen": "2026-07-25T10:04:00Z",
      "firings": 1,
      "flapping": false,
      "resolvedAt": "2026-07-25T10:04:12Z",
      "resolutionSeconds": 252
    }
  ],
  "stats": {
    "newTotal": 7,
    "resolvedTotal": 3,
    "flapTotal": 1,
    "droppedTotal": 0,
    "resolutionSecondsSum": 812,
    "resolutionSecondsCount": 3
  }
}
```

Fields: `kind` / `namespace` / `name` / `issue` identify the tracked instance
(`namespace` is omitted entirely for a cluster-scoped issue, e.g. `Node`);
`firstSeen` is the first time this key was ever observed; `firingSince` is
when the *current* firing started (a re-fire after resolving moves this
forward, `firstSeen` does not); `lastSeen` is the last reconcile that observed
it; `firings` counts inactive→active transitions; `flapping` is whether it has
crossed the flap threshold. `ageSeconds` (seconds since `firingSince`) appears
only on records in `active`; `resolvedAt` and `resolutionSeconds` appear only
on records in `resolved`. `stats` mirrors the six counter metrics above
(`newTotal`, `resolvedTotal`, `flapTotal`, `droppedTotal`,
`resolutionSecondsSum`, `resolutionSecondsCount`). Both `active` and
`resolved` are `[]`, never `null`, when there is nothing to report.

### Limits and restart semantics

Retention and flap detection use fixed defaults; there are no flags to tune
them:

- up to **500** issues tracked at once — a new issue beyond that cap is left
  untracked, not reported, and counted in `kubeagent_issues_dropped_total`;
- a resolved issue stays visible in `/issues` and countable in `/metrics` for
  **1 hour** after it resolves, then is purged;
- flapping is judged over a **30-minute** window, at **3** firings inside that
  window to cross the threshold.

State is **in-memory only** — nothing is written to disk, and there is no
persistence across restarts. After the daemon restarts, the first reconcile
reports every issue that is currently firing as `NEW` (even if it had already
been firing for hours before the restart), every age starts counting from
zero, and every counter (`kubeagent_issues_new_total`,
`kubeagent_issues_resolved_total`, and the rest) resets to zero. Don't read a
burst of `NEW` lines right after a restart as a fresh incident storm.

**A failure mode that changes is a different issue.** The issue name is part of
the key, so a workload whose failure evolves reports the old mode resolved and
the new one new — a bad image walks through `Degraded`, `ErrImagePull`, and
`ImagePullBackOff`, and each step logs a `RESOLVED` for the previous mode
alongside the `NEW`:

```text
kubeagent: NEW Deployment/shop/web:ErrImagePull
kubeagent: RESOLVED Deployment/shop/web:Degraded (fired for 2s)
kubeagent: NEW Deployment/shop/web:ImagePullBackOff
kubeagent: RESOLVED Deployment/shop/web:ErrImagePull (fired for 15s)
```

A `RESOLVED` line therefore means *that issue* stopped firing, not that the
workload recovered — check whether a `NEW` line for the same object arrived in
the same reconcile. Mean time to resolution follows the same rule: it measures
how long each distinct failure mode fired, not how long the object was broken.

**Known limitation:** a node named via `--expected-nodes` that is absent from
the cluster is **not** tracked as an issue, and never appears in `/issues` or
the per-issue metrics. The cluster-health detector reports it only as the
`kubeagent_nodes_expected_absent` counter plus free-text prose in the cluster
summary, with no stable per-node identity to key a tracked record on. It still
shows up in `scan` output and in that counter — it just doesn't participate in
NEW/RESOLVED/FLAPPING tracking. Giving the detector a structured field to fix
this is a separate, future change.

### Still read-only

Issue tracking adds no new API calls and no writes: the tracker only consumes
the same evaluation `scan` already performs, entirely in memory, and
`/issues` is a read-only view over it. Watch mode's RBAC (`get`/`list`/`watch`
only) is unchanged.

## Alerting

The daemon can push transitions to a webhook instead of waiting to be scraped. It
stays read-only toward the cluster: alerting adds exactly one egress destination,
the URL you configure.

**One alert per object, not per issue.** An alert opens when an object acquires
its first issue, updates when its issue set changes, and clears only when the
object has no active issues at all. This is deliberate: because tracking is keyed
on the issue, a workload whose failure mode evolves —
`Degraded` → `ErrImagePull` → `ImagePullBackOff` — logs a `RESOLVED` for each
superseded mode while it is still broken. Rolled up per object, that is one alert
that stays firing until the Deployment actually recovers.

Enable it by setting the URL in the environment:

```bash
export KUBEAGENT_ALERT_WEBHOOK=<WEBHOOK_URL>
kubeagent watch --alert-format slack
```

There is no `--alert-webhook` flag on purpose: a Slack incoming-webhook URL is a
bearer token in URL form, and a flag would put it in the pod spec's args and in
`ps` output. Only `scheme://host` is ever logged.

| Flag | Default | Meaning |
|------|---------|---------|
| `--alert-format` | `json` | `json`, `slack`, or `alertmanager` |
| `--alert-repeat` | `4h`, or `60s` for `alertmanager` | Re-send interval for still-firing alerts |

The re-send is what makes a dropped notification self-healing: a receiver that was
down when the alert opened learns about it on the next repeat.

### Payloads

`json` — kubeagent's native body:

```json
{
  "status": "firing",
  "reason": "changed",
  "kind": "Deployment",
  "namespace": "shop",
  "name": "web",
  "issues": ["ImagePullBackOff"],
  "firingSince": "2026-07-25T10:04:11Z",
  "flapping": false
}
```

`reason` is `new`, `changed`, `repeat`, or `resolved`. A resolved body carries an
empty `issues` array and a `resolvedAt`.

`firingSince` is when the **object** broke, not when the issues currently listed
appeared. It only ever moves earlier: the issue that opened the alert can resolve
while the object stays broken, so the alert keeps the original break time rather
than restarting the clock on each new failure mode.

`slack` — an incoming-webhook body: `*FIRING* Deployment/shop/web` with the issue
list and firing time, or `*RESOLVED* Deployment/shop/web (fired for 4m12s)`.

`alertmanager` — a `POST /api/v2/alerts` array. Labels are `alertname`
(`KubeagentIssue`), `kind`, `name`, and — only for a namespaced object —
`namespace`; a cluster-scoped alert such as a `Node` carries no `namespace`
label at all. The issue list is an **annotation**, because a label that changes
as the failure evolves would create a second alert instead of updating the open
one. A bare host URL gets `/api/v2/alerts` appended.

Alertmanager expires an alert `resolve_timeout` (5m by default) after the last
POST, so the re-send interval must stay under it — `--alert-repeat` above `4m`
with this format is a startup error.

### No severity

kubeagent has no severity model, so no payload claims one. Route on what is
actually known, and derive severity in Alertmanager if you want it:

```yaml
route:
  routes:
    - matchers: [alertname="KubeagentIssue", namespace="payments"]
      receiver: pager
```

Route on **labels only**. Alertmanager's routing tree cannot match annotations,
so a matcher on `issues` never fires — that is the cost of keeping the issue
list out of the label set. The labels available to route on are `alertname`,
`kind`, `name`, and `namespace`. Reach for `issues` in the notification
template, where annotations are in scope:

```text
{{ range .Alerts }}{{ .Labels.kind }}/{{ .Labels.name }}: {{ .Annotations.issues }}
{{ end }}
```

### Delivery

Sends run on their own goroutine behind a 64-slot queue, so a hung receiver never
stalls the reconcile loop. Each POST gets three attempts (1s then 2s backoff);
a 4xx is not retried. Drops are counted, never silent:

| Series | Meaning |
|--------|---------|
| `kubeagent_alerts_sent_total{status,outcome}` | Deliveries by `firing`/`resolved` and `ok`/`failed` |
| `kubeagent_alerts_dropped_total{reason}` | `queue_full` or `retries_exhausted` |
| `kubeagent_alert_last_success_timestamp_seconds` | Last successful delivery (0 if none) |

## SLO burn rate

Opt-in: set `--slo-target` (a percentage, e.g. `99.9`) and the daemon starts
tracking a time-weighted availability SLI and reporting a multi-window
error-budget burn rate, alongside everything above. Unset — or `0`, the
default — means no SLO tracking at all: no series render, no verdict is
computed, no alert can fire. It is independent of the webhook alerting above:
setting a target with no `KUBEAGENT_ALERT_WEBHOOK` configured still renders the
series, it just never pages. The two are separate switches.

`--slo-target` must be greater than 0 and less than 100 (`NaN`/`Inf` included in
the rejection); a value outside that range is a startup error, checked at the
same point as alert-config validation, before anything that could hide the
failure behind a slow informer cache sync.

### The SLI and the arithmetic

The signal is **time-weighted workload availability**, not request-based:
every reconcile contributes `good`/`total` workload counts weighted by the
seconds since the previous reconcile, so an outage costs budget in proportion
to how long it lasted and how much of the estate it took down. A workload
counts as "good" when it has no findings — the same predicate the issue
tracker uses to decide whether to track it, so the SLI and `/issues` can never
disagree about what "broken" means.

```
good  += dt * count(workloads with no findings)
total += dt * count(workloads)

SLI       = good / total
burn rate = (1 - SLI) / (1 - target)
```

Worked example: three workloads broken out of two hundred, for an hour,
against a 99.9% target — SLI 98.5%, burn `(1 - 0.985) / (1 - 0.999) = 15×`, a
fast-burn page. The same three broken workloads on a two-thousand-workload
cluster burn only 1.5× — visible, not paged. The signal scales with the size
of the estate, which a binary cluster-healthy gauge does not: a single stuck
PVC would pin that at maximum forever.

### The five series

Rendered only while SLO tracking is on:

| Metric | Labels | Meaning |
|--------|--------|---------|
| `kubeagent_slo_target_ratio` | none | Configured availability SLO as a ratio |
| `kubeagent_slo_availability_ratio` | `window` (`fast`/`slow`) | Time-weighted fraction of workload-seconds with no findings, over the window |
| `kubeagent_slo_burn_rate` | `window` (`fast`/`slow`) | Error-budget consumption multiple (1 = spending exactly at budget) |
| `kubeagent_slo_window_coverage_ratio` | `window` (`fast`/`slow`) | Fraction of the window carrying samples |
| `kubeagent_slo_error_budget_remaining_ratio` | none | Budget left over the **slow window only**, clamped to `[0,1]` |

Five metric names, eight samples (three carry both a `fast` and a `slow`
series). `kubeagent_slo_error_budget_remaining_ratio` is `1 - slowBurnRate`
clamped at zero — it does not blend both windows — since a burn above 1×
already means that window's budget is spent, and a negative "remaining" would
be nonsense on a dashboard.

### Fixed windows, thresholds, and the coverage gate

The windows, thresholds, bucket width, and coverage gate are fixed constants,
not flags: letting operators mix their own pair produces alerting that is
either deafening or silent with no way to tell which. kubeagent uses the fixed
pair from the Google SRE workbook's multi-window recipe:

| Constant | Value |
|----------|-------|
| Bucket width | 1 minute (360 buckets cover the 6-hour window in a small fixed ring) |
| Fast window | 1 hour |
| Slow window | 6 hours |
| Fast-window burn threshold | 14.4× |
| Slow-window burn threshold | 6× |
| Coverage gate | 60% of **both** windows |

The alert fires only when **all four** conditions hold at once: fast burn ≥
14.4×, slow burn ≥ 6×, fast coverage ≥ 60%, slow coverage ≥ 60%. Requiring
both windows is what makes this multi-window: the fast window alone would page
for a blip that already passed, and the slow window alone would take hours to
notice a total outage.

A sample's time weight is also capped, so a daemon that stalls (an
unresponsive API server, for example) cannot resume and assert that its last
known state held for the whole stall — the clamped-away time is simply never
counted, which lowers coverage rather than inventing history for it. That cap
is **2× the configured `--heartbeat`**, not a fixed duration — at the default
60s heartbeat it works out to 2 minutes, and it scales with `--heartbeat`, so
raising the heartbeat interval does not by itself starve the windows of
coverage: reconciles ticking at a steady heartbeat always land well inside the
cap. What actually erodes coverage is a genuine stall longer than that
2×-heartbeat cap — a slow or unresponsive API server, or the daemon blocked for
some other reason — regardless of which `--heartbeat` value is configured.

### The restart caveat

State is **in-memory only**, exactly as issue tracking is: nothing persists
across restarts. After the daemon restarts, both windows start empty, and the
coverage gate means the burn alert cannot fire until the **slow** window is
roughly **3.6 hours** warm (60% of 6 hours). A daemon that just started would
otherwise compute a burn rate from a few minutes of data and page on every
rollout of the daemon itself.

`kubeagent_slo_window_coverage_ratio` is how you see this happening — watch it
climb toward 1.0 after a restart. A `kubeagent_slo_burn_rate` above threshold
alongside low coverage is the daemon still warming up, not a real breach.
Don't read the quiet in that window as "nothing is wrong."

### Alerting on burn rate

When the firing condition holds, the daemon raises exactly one alert,
separate from the per-object alerts above: kind `SLO`, name `error-budget`,
issue `ErrorBudgetBurn` — not `FastBurn`, because firing needs *both* windows,
and naming it after the fast one alone would misdescribe what fired. It goes
through the same sink as object alerts (the same bounded queue, retries,
backoff, and URL redaction), reusing `--alert-repeat` for its re-send cadence,
and clears with a single `resolved` notification on the clearing edge:

```text
kubeagent: NEW SLO/error-budget:ErrorBudgetBurn (fast=18.2x slow=7.1x, coverage fast=100% slow=95%)
kubeagent: RESOLVED SLO/error-budget (burn back under threshold; fast=2.1x slow=1.4x)
```

This alert is **not** an object: it never enters `watchstate` or `alertstate`,
so it does not appear in `/issues` or in any `kubeagent_issues_*` series. An
operator reading those as object counts would otherwise be misled by a signal
that isn't about any one object.

The series render even with no webhook configured, so an operator who would
rather alert on them directly than rely on the webhook can translate the
firing condition straight into a Prometheus rule:

```yaml
- alert: KubeagentErrorBudgetBurn
  expr: |
    kubeagent_slo_burn_rate{window="fast"} >= 14.4
    and kubeagent_slo_burn_rate{window="slow"} >= 6
    and kubeagent_slo_window_coverage_ratio{window="fast"} >= 0.6
    and kubeagent_slo_window_coverage_ratio{window="slow"} >= 0.6
  for: 2m
  labels:
    severity: page
  annotations:
    summary: "Error budget burning fast (fast burn {{ $value }}x)"
```

## Run it

```bash
# in-cluster (read-only RBAC + Deployment + metrics Service)
kubectl apply -f deploy/

# or locally against a kubeconfig, for a quick look
./kubeagent watch --kubeconfig ~/.kube/config --metrics-addr :8080
curl localhost:8080/metrics
```

Flags (each with a `KUBEAGENT_*` env fallback): `--metrics-addr` (`:8080`),
`--heartbeat` (`60s`), `--debounce` (`2s`), `--namespace`/`-n` (default all
namespaces), `--node-heartbeat-threshold` / `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD`
(`40s`; `0` disables the kubelet-lease staleness check),
`--expected-nodes` / `KUBEAGENT_EXPECTED_NODES` (comma-separated node names;
unset by default — declares which nodes must be present),
`--kubelet-health` / `KUBEAGENT_KUBELET_HEALTH` (off by default — probes each
node's kubelet `/healthz` via the `nodes/proxy` add-on, the same grant
`--disk-usage` uses; see [Disk-usage check](#disk-usage-check-opt-in)),
`--certs` / `KUBEAGENT_CERTS` (off by default — enables the certificate-expiry
check; requires the secrets add-on `deploy/rbac-certs.yaml` or Helm
`certs.enabled=true`), `--cert-warn-days` / `KUBEAGENT_CERT_WARN_DAYS` (default
`30` — warn window in days).

### Disk-usage check (opt-in)

Set `KUBEAGENT_DISK_USAGE=true` (and optionally `KUBEAGENT_DISK_THRESHOLD`,
default `0.80`) to enable the daemon disk-usage check. This requires the
`nodes/proxy` RBAC add-on — apply `deploy/rbac-diskusage.yaml` or set Helm
`diskUsage.enabled=true`. Without the add-on the daemon stays strictly
`get`/`list`/`watch`. When enabled, the daemon exposes
`kubeagent_node_fs_usage_ratio{node}` and
`kubeagent_volumes_over_disk_threshold` in addition to the standard metrics.

## Prometheus alerting rules

Independent of the webhook [Alerting](#alerting) above, you can also point
Prometheus at the metrics Service (it carries the `prometheus.io/scrape`
annotations) and alert on the gauges directly, e.g.:

```yaml
- alert: KubeagentClusterDegraded
  expr: kubeagent_cluster_healthy == 0
  for: 5m
- alert: KubeagentWorkloadsFlagged
  expr: kubeagent_workloads_flagged > 0
  for: 10m
```

## Roadmap

Watch mode is the first phase of the daemon roadmap. Planned next, each as its
own guarded step: multi-cluster (an agent per cluster reporting to a hub),
on-incident `--explain` (rate-limited, key via a Secret — never in the hot
loop), and opt-in autonomous remediation with stricter rails than the
interactive `--fix`.
