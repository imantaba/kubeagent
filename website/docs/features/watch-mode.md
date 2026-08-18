# Watch mode (daemon)

`kubeagent watch` runs kubeagent **inside your cluster** as a long-lived,
strictly read-only daemon: it watches the cluster and continuously exposes the
same deterministic diagnosis `scan` produces — as Prometheus metrics and
structured logs.

!!! note
    Watch mode is **strictly read-only toward the cluster**: its RBAC grants
    only `get`, `list`, and `watch` — it can never create, update, patch, or
    delete anything. That holds in every configuration. It makes **no LLM
    calls and works fully offline** unless you opt into
    [`--explain`](#on-incident-explanations-explain), which sends findings the
    daemon already collected — never pod specs, secrets, or logs — to a model
    over outbound HTTP.

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

The daemon serves Prometheus text on `--metrics-addr` (default `:8080`). Every series
in this table also carries a `cluster` label (default `local`); see
[Watching several clusters](#watching-several-clusters):

| Metric | Meaning |
|--------|---------|
| `kubeagent_cluster_healthy` | 1 if the cluster verdict is Healthy, else 0 |
| `kubeagent_nodes_ready` / `kubeagent_nodes_total` | node readiness |
| `kubeagent_workloads_flagged` | workloads currently needing attention |
| `kubeagent_findings{cluster="...",issue="..."}` | current findings by type (e.g. `CrashLoopBackOff`, `ImagePullBackOff`, `OOMKilled`, `VolumeAttachError`, `RestartLoop`) |
| `kubeagent_service_issues` | Service issues (no ready endpoints, LB pending); excludes intentionally-empty (parked) Services |
| `kubeagent_nodes_without_reservations` | Number of nodes whose kubelet reserves no memory (allocatable == capacity) |
| `kubeagent_pvcs_reclaim_delete` | Number of PVCs whose bound PV has reclaimPolicy Delete |
| `kubeagent_node_fs_usage_ratio{cluster,node}` | Node root-filesystem usage ratio (opt-in; requires `--disk-usage` / `KUBEAGENT_DISK_USAGE=true`) |
| `kubeagent_volumes_over_disk_threshold` | Number of node filesystems and PVCs at or over `--disk-threshold` (opt-in) |
| `kubeagent_ingress_route_issues` | Number of Ingress routes whose backend Service is missing, has no ready endpoints, or does not expose the referenced port; excludes intentionally-empty (parked) routes |
| `kubeagent_pvc_pending_issues` | Number of PersistentVolumeClaims stuck Pending because provisioning or binding failed |
| `kubeagent_nodes_stale_heartbeat` | Number of Ready nodes whose kubelet lease is stale (kubelet not heartbeating) |
| `kubeagent_nodes_expected_absent` | Number of declared expected nodes that are absent from the cluster (opt-in; requires `--expected-nodes` / `KUBEAGENT_EXPECTED_NODES`) |
| `kubeagent_kubelet_unhealthy` | Number of nodes whose kubelet /healthz reported unhealthy (opt-in; requires `--kubelet-health` / `KUBEAGENT_KUBELET_HEALTH` and the `nodes/proxy` add-on) |
| `kubeagent_control_plane_checked` | 1 if apiserver `/readyz` returned a health verdict this cycle, else 0 (opt-in; requires `KUBEAGENT_CONTROL_PLANE_HEALTH`) |
| `kubeagent_control_plane_unhealthy` | 1 if apiserver `/readyz` reported the control plane not ready, else 0 (opt-in; requires `KUBEAGENT_CONTROL_PLANE_HEALTH`) |
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
kubeagent: [local] NEW Deployment/shop/web:CrashLoopBackOff
kubeagent: [local] RESOLVED Deployment/shop/web:CrashLoopBackOff (fired for 4m12s)
kubeagent: [local] FLAPPING Deployment/shop/web:CrashLoopBackOff (3 firings in 30m0s)
kubeagent: [local] cluster Degraded (2/3 nodes ready) — 4 issue(s) active, 1 new, 1 resolved
```

A reconcile where nothing changed — the same issues still firing, nothing new,
nothing resolved, nothing newly flapping — logs **nothing at all**. Steady
state stays quiet.

### Metrics

Alongside the point-in-time gauges above, the daemon exposes ten series that
track issue lifecycle. Every one of them also carries a `cluster` label (default
`local`):

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
| `kubeagent_issue_active{cluster,kind,namespace,name,issue}` | gauge | 1 while this issue instance is firing |
| `kubeagent_issue_age_seconds{cluster,kind,namespace,name,issue}` | gauge | Seconds since this issue instance started firing |

There is no dedicated MTTR series — compute mean time to resolution as
`kubeagent_issue_resolution_seconds_sum / kubeagent_issue_resolution_seconds_count`.

### `/issues`

`GET /issues` on `--metrics-addr` returns the same tracked state as JSON:

```bash
curl -s localhost:8080/issues | jq .
```

```json
{
  "schemaVersion": "1.0",
  "clusters": [
    {
      "name": "local",
      "up": true,
      "lastScan": "2026-07-25T10:16:12Z"
    }
  ],
  "active": [
    {
      "cluster": "local",
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
      "cluster": "local",
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

Fields: `clusters` is the roster of every watched cluster (`name`, `up`,
`lastScan`, and `error` when unreachable — see
[Watching several clusters](#watching-several-clusters)). Each record's own
`cluster` field ties it back to one of those roster entries. `kind` /
`namespace` / `name` / `issue` identify the tracked instance (`namespace` is
omitted entirely for a cluster-scoped issue, e.g. `Node`); `firstSeen` is the
first time this key was ever observed; `firingSince` is when the *current*
firing started (a re-fire after resolving moves this forward, `firstSeen`
does not); `lastSeen` is the last reconcile that observed it; `firings`
counts inactive→active transitions; `flapping` is whether it has crossed the
flap threshold. `ageSeconds` (seconds since `firingSince`) appears only on
records in `active`; `resolvedAt` and `resolutionSeconds` appear only on
records in `resolved`. `stats` mirrors the six counter metrics above
(`newTotal`, `resolvedTotal`, `flapTotal`, `droppedTotal`,
`resolutionSecondsSum`, `resolutionSecondsCount`). Both `active` and
`resolved` are `[]`, never `null`, when there is nothing to report.

The shape of this document, and of `/explanations` below, is versioned; see
[JSON schema contract](json-schema.md).

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
kubeagent: [local] NEW Deployment/shop/web:ErrImagePull
kubeagent: [local] RESOLVED Deployment/shop/web:Degraded (fired for 2s)
kubeagent: [local] NEW Deployment/shop/web:ImagePullBackOff
kubeagent: [local] RESOLVED Deployment/shop/web:ErrImagePull (fired for 15s)
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

Enable it by setting the credential in the environment:

```bash
export KUBEAGENT_ALERT_WEBHOOK=<WEBHOOK_URL>
kubeagent watch --alert-format slack
```

There is no `--alert-webhook` flag on purpose: a Slack incoming-webhook URL is a
bearer token in URL form, and a flag would put it in the pod spec's args and in
`ps` output. Only `scheme://host` is ever logged.

`pagerduty` authenticates differently — with an Events API v2 integration key in
the request **body**, not with the URL — so it reads
`KUBEAGENT_ALERT_ROUTING_KEY`, which has no flag for the same reason:

```bash
export KUBEAGENT_ALERT_ROUTING_KEY=<ROUTING_KEY>
kubeagent watch --alert-format pagerduty
```

The routing key never reaches a log line, a metric label, an error message or a
rendered manifest. `KUBEAGENT_ALERT_WEBHOOK` is optional for this format and
defaults to PagerDuty's published endpoint; set it to point at a non-default
service region or an egress proxy, and a URL with no path gains `/v2/enqueue`.

| Flag | Default | Meaning |
|------|---------|---------|
| `--alert-format` | `json` | `json`, `slack`, `alertmanager`, or `pagerduty` |
| `--alert-repeat` | `4h`, or `60s` for `alertmanager` | Re-send interval for still-firing alerts |

The re-send is what makes a dropped notification self-healing: a receiver that was
down when the alert opened learns about it on the next repeat.

### Payloads

`json` — kubeagent's native body:

```json
{
  "status": "firing",
  "reason": "changed",
  "cluster": "local",
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

`slack` — an incoming-webhook body: `*FIRING* local/Deployment/shop/web` with the issue
list and firing time, or `*RESOLVED* local/Deployment/shop/web (fired for 4m12s)`.

`alertmanager` — a `POST /api/v2/alerts` array. Labels are `alertname`
(`KubeagentIssue`), `cluster`, `kind`, `name`, and — only for a namespaced object —
`namespace`; a cluster-scoped alert such as a `Node` carries no `namespace`
label at all. The issue list is an **annotation**, because a label that changes
as the failure evolves would create a second alert instead of updating the open
one. A bare host URL gets `/api/v2/alerts` appended.

Alertmanager expires an alert `resolve_timeout` (5m by default) after the last
POST, so the re-send interval must stay under it — `--alert-repeat` above `4m`
with this format is a startup error.

`pagerduty` — a [PagerDuty Events API v2](https://developer.pagerduty.com/docs/events-api-v2-overview)
event:

```json
{
  "routing_key": "<ROUTING_KEY>",
  "event_action": "trigger",
  "dedup_key": "local/Deployment/shop/web",
  "payload": {
    "summary": "local/Deployment/shop/web: ImagePullBackOff",
    "source": "local",
    "severity": "error",
    "timestamp": "2026-08-05T10:04:11Z",
    "custom_details": {
      "cluster": "local",
      "kind": "Deployment",
      "namespace": "shop",
      "name": "web",
      "issues": ["ImagePullBackOff"],
      "reason": "new",
      "flapping": false
    }
  }
}
```

`dedup_key` is the object's identity — the same string the `slack` format
already sends. Deriving it from identity rather than state is what makes it
survive a daemon restart: the restart re-triggers whatever is still broken, and
PagerDuty folds that onto the open incident instead of opening a second one. A
key past PagerDuty's 255-character cap keeps a readable prefix and gains a short
digest, so two objects that share a long prefix still get two incidents.

`timestamp` is when the **object** broke, not when the daemon noticed — the same
`firingSince` the `json` format carries.

An **explanation** (`--explain`) is a `trigger` on the same `dedup_key` with the
prose in `custom_details.explanation`, not a new event kind: the object is still
firing, and an explanation is more detail about one state rather than a
transition to another. The daemon's `/explanations` endpoint and the dashboard
remain the authoritative place to read that prose.

A **resolve** carries only `routing_key`, `event_action` and `dedup_key`.
PagerDuty computes the incident duration itself, so anything kubeagent added
would be a second copy free to disagree with the first.

`--alert-repeat` defaults to `4h` here, as it does for `json` and `slack`, and
takes no ceiling: PagerDuty does not expire alerts, so a slow cadence produces no
false recovery to guard against.

### Severity

kubeagent has no severity model: `diagnose.Finding` carries no rank, and no
payload derives one. The `pagerduty` format sends the constant `error` because
the Events API requires the field — it is the same default Alertmanager's own
PagerDuty notifier picks, and it is the honest answer: kubeagent knows something
is broken, not how badly. Every other format sends no severity at all.

Route on what is actually known, and derive severity in Alertmanager if you want
it:

```yaml
route:
  routes:
    - matchers: [alertname="KubeagentIssue", namespace="payments"]
      receiver: pager
```

Route on **labels only**. Alertmanager's routing tree cannot match annotations,
so a matcher on `issues` never fires — that is the cost of keeping the issue
list out of the label set. The labels available to route on are `alertname`,
`cluster`, `kind`, `name`, and `namespace`. Reach for `issues` in the notification
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
the rejection); a value outside that range is a startup error, checked first of
all — ahead of the alert config, the metrics server, and the informers — so the
failure cannot hide behind a slow cache sync.

### The SLI and the arithmetic

The signal is **time-weighted workload availability**, not request-based:
every reconcile contributes `good`/`total` workload counts weighted by the
seconds since the previous reconcile, so an outage costs budget in proportion
to how long it lasted and how much of the estate it took down. The
denominator is every long-running workload in scope — Job and CronJob are
excluded, since neither is expected to be continuously up — and a workload
counts as "good" when it is **not flagged**: no findings, not
under-replicated (`Ready < Desired`), and not `Failed`. That is the same
predicate the issue tracker uses to decide whether a workload is broken, so
the SLI and `/issues` can never disagree about what "broken" means.

```
good  += dt * count(non-Job/CronJob workloads that are not flagged)
total += dt * count(non-Job/CronJob workloads)

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
| `kubeagent_slo_target_ratio` | `cluster` | Configured availability SLO as a ratio |
| `kubeagent_slo_availability_ratio` | `cluster`, `window` (`fast`/`slow`) | Time-weighted fraction of workload-seconds that are not flagged, over the window |
| `kubeagent_slo_burn_rate` | `cluster`, `window` (`fast`/`slow`) | Error-budget consumption multiple (1 = spending exactly at budget) |
| `kubeagent_slo_window_coverage_ratio` | `cluster`, `window` (`fast`/`slow`) | Fraction of the window carrying samples |
| `kubeagent_slo_error_budget_remaining_ratio` | `cluster` | Budget left over the **slow window only**, clamped to `[0,1]` |

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
kubeagent: [local] NEW SLO/error-budget:ErrorBudgetBurn (fast=18.2x slow=7.1x, coverage fast=100% slow=95%)
kubeagent: [local] RESOLVED SLO/error-budget (burn back under threshold; fast=2.1x slow=1.4x)
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

## On-incident explanations (`--explain`)

Off by default. When enabled, an object that breaks gets a second, model-written
message a few seconds after its page: what likely caused it, how to confirm, and
the deterministic fix kubeagent already computed.

```bash
export ANTHROPIC_API_KEY=<PLACEHOLDER>
kubeagent watch --explain --explain-budget 20 --explain-cooldown 1h
```

There is no `--explain-api-key` flag, on purpose — a process argument is
world-readable via `/proc`. The key reaches the daemon only through an
environment variable: `ANTHROPIC_API_KEY` (read directly by the Anthropic SDK),
or `KUBEAGENT_EXPLAIN_API_KEY` as the bearer token when
`KUBEAGENT_EXPLAIN_ENDPOINT` points at a local model instead.

The alert itself never waits on the model. It fires immediately and LLM-free,
exactly as it does without this flag; the explanation is enqueued separately
through the same webhook sink, referencing the same object, so it lands under
the original page.

### What the model sees

The object that broke, the other workloads currently flagged, the cluster
verdict, and the correlation hints kubeagent already computed — so it can say
"one of twelve workloads failing to pull from the same registry" rather than
guessing from one object in isolation.

It does **not** see pod specs, environment variables, ConfigMap or Secret
contents, pod names, pod IPs, node names, or logs. Enabling `--explain` adds no
cluster read and no RBAC verb: the daemon sends only findings it had already
collected.

Each finding travels with the deterministic kubectl command kubeagent would
print for it, so the model can hand back a `Fix:` line rather than inventing
one — with the pod's generated name replaced by `<pod>`. The namespace, the
verb and the container are what make the command useful; the replica's name is
what would identify your cluster. The report you read locally keeps the real
name.

### Cost control

Two limits, for two different ways spend runs away:

| Limit | Default | Guards against |
|-------|---------|-----------------|
| `--explain-cooldown` | `1h` | one flapping object being re-explained every reconcile |
| `--explain-budget` | `20`/hour | a mass outage where many distinct objects break at once |

The budget is a token bucket whose capacity equals its hourly rate, so a real
mass outage gets its whole allowance at once and then drips. The cooldown is
checked first, since it costs nothing to check; only a call the throttle
actually allows gets stamped. Over budget, the call is skipped rather than
queued — a stale explanation is worse than none — and the skip is counted. A
restart explains nothing from its first snapshot, so a crash-looping daemon
cannot spend its budget re-explaining pre-existing problems.

Five series render only while `--explain` is on:

| Metric | Meaning |
|--------|---------|
| `kubeagent_explain_allowed_total` | Incident explanations the throttle admitted since start |
| `kubeagent_explain_throttled_total` | Incident explanations refused by the cooldown or the hourly budget |
| `kubeagent_explain_failed_total` | Incident explanations whose model call errored or returned no text |
| `kubeagent_explain_dropped_total` | Incident explanations admitted but dropped because the worker queue was full |
| `kubeagent_explain_budget_remaining` | Model calls left in the hourly budget |

Watch `kubeagent_explain_budget_remaining` to see why an incident went
unexplained.

### `/explanations`

The latest explanation per object, alongside the counters:

```bash
curl -s localhost:8080/explanations | jq .
```

With `--explain` off, this still returns the same shape — an empty explanation
list and zeroed counters — rather than `404`, the same contract `/issues`
follows.

This is model-written prose about your failures, served on the same
unauthenticated metrics port as `/issues`. Same sensitivity class as `/issues`,
but worth knowing before you enable it.

### With a local model

No data leaves your network:

```bash
export KUBEAGENT_EXPLAIN_ENDPOINT=http://ollama.llm.svc.cluster.local:11434/v1
kubeagent watch --explain --model llama3.1
```

`--model` (or `KUBEAGENT_MODEL`) is required once `KUBEAGENT_EXPLAIN_ENDPOINT`
is set — a local endpoint has no default model name to fall back to.

### In the chart

Neither the API key nor the endpoint URL is ever a chart value — a values
file lands in Git, in `helm get values`, and in CI logs. Both live in a
Secret:

```bash
kubectl -n kubeagent create secret generic kubeagent-llm --from-literal=apiKey=<PLACEHOLDER>
helm upgrade --install kubeagent deploy/helm/kubeagent \
  --set explain.enabled=true --set explain.existingSecret=kubeagent-llm
```

For a local model, add the endpoint to the same Secret under its own key and
point `explain.endpointSecretKey` at it:

```bash
kubectl -n kubeagent create secret generic kubeagent-llm \
  --from-literal=apiKey=<PLACEHOLDER> \
  --from-literal=endpoint=http://ollama.llm.svc.cluster.local:11434/v1
helm upgrade --install kubeagent deploy/helm/kubeagent \
  --set explain.enabled=true --set explain.existingSecret=kubeagent-llm \
  --set explain.endpointSecretKey=endpoint --set explain.model=llama3.1
```

`explain.existingSecret` is required whenever `explain.enabled` is true — the
API key and any endpoint URL are both credentials and must come from a
Secret, never from `values.yaml`.

## Dashboard (`--dashboard`)

`--dashboard` serves the same tracked state as an HTML page at `/dashboard` on
the metrics port — the URL you hand someone instead of a `curl | jq`. It
performs no extra cluster read and needs no extra RBAC. Separately: a dashboard
request makes no model call. It is unauthenticated, exactly like `/metrics` and
`/issues` on the same port.

See [In-cluster dashboard](dashboard.md).

## Watching several clusters

```bash
kubeagent watch --context prod-eu --context prod-us --context staging
```

One informer set per cluster runs inside a single process behind one HTTP
endpoint. Every metric series carries a `cluster` label, `/issues` and
`/explanations` carry a `cluster` field, and every alert names its cluster.

| Flag | Default | Meaning |
|---|---|---|
| `--context <name>` | current-context | Cluster to watch. **Repeat the flag** to watch several clusters from one daemon. |
| `--cluster-name <name>` | `local` | Name for the default cluster — the one watched when no `--context` is given. Becomes its `cluster` metric label. |
| `--include-local` | off | Also watch the default cluster alongside every `--context`. A no-op when no `--context` is given. |

The `cluster` label is present even with one cluster, where it defaults to
`local`. PromQL selectors match regardless of extra labels, so a query written
against a single-cluster daemon keeps working; a recording rule that groups
`by (...)` should add `cluster` to the grouping.

**A configuration error is fatal; a cluster failure is not.** A context that is
not in the kubeconfig stops the daemon at startup — building a client contacts no
API server, so a failure there is a typo, and silently watching two of the three
clusters you asked for is worse than not starting. A cluster that becomes
unreachable at runtime reports `kubeagent_cluster_up 0` and an error on the
`/issues` roster; its tracked issues stay firing, and every other cluster keeps
reconciling.

`/readyz` reports ready once every cluster has finished its **first reconcile
attempt** — success or failure — and never flips afterward on cluster health.
Readiness answers "can this process serve?", not "is everything fine": tying it
to cluster health would let one unreachable remote cluster pull the pod out of
its Service endpoints, stopping Prometheus from scraping it, and so blind you to
the clusters that are working.

**Credentials.** One process holds read-only credentials for every cluster it
watches, so the daemon and its kubeconfig Secret are as sensitive as the union of
those clusters. **Each credential in that kubeconfig must be read-only
(get/list/watch).** kubeagent issues no writes, but it cannot enforce that from
inside the pod: a kubeconfig holding a cluster-admin token would give this daemon
write-capable credentials it never uses but nonetheless holds.

Cross-cutting settings stay global: one webhook, one explanation budget, one
`--slo-target`. If you need them split per cluster, run one daemon per cluster —
that still works, and each one labels its series with its own `--cluster-name`.
On the chart that is `watch.clusterName`, and it applies to a single-cluster
install too: without it every series reads `cluster="local"`, which stops meaning
anything the moment a second daemon's metrics reach the same Prometheus.

## Run it

```bash
# in-cluster (read-only RBAC + Deployment + metrics Service)
kubectl apply -f deploy/

# or locally against a kubeconfig, for a quick look
./kubeagent watch --kubeconfig ~/.kube/config --metrics-addr :8080
curl localhost:8080/metrics
```

Flags (each with a `KUBEAGENT_*` env fallback, except `--context`, which is
repeatable and has none): `--context` (repeatable; default: current-context),
`--cluster-name` / `KUBEAGENT_CLUSTER_NAME` (`local`; see
[Watching several clusters](#watching-several-clusters)), `--include-local` /
`KUBEAGENT_INCLUDE_LOCAL` (off by default), `--metrics-addr` /
`KUBEAGENT_METRICS_ADDR` (`:8080`), `--heartbeat` / `KUBEAGENT_HEARTBEAT`
(`60s`), `--debounce` / `KUBEAGENT_DEBOUNCE` (`2s`), `--namespace`/`-n` /
`KUBEAGENT_NAMESPACE` (default all namespaces).

Settings with **no flag** — the daemon reads these from the environment only,
so set them in the container's `env:`: `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD`
(`40s`; `0` disables the kubelet-lease staleness check),
`KUBEAGENT_EXPECTED_NODES` (comma-separated node names; unset by default —
declares which nodes must be present), `KUBEAGENT_KUBELET_HEALTH` (off by
default — probes each node's kubelet `/healthz` via the `nodes/proxy` add-on,
the same grant the disk-usage check uses; see
[Disk-usage check](#disk-usage-check-opt-in)), `KUBEAGENT_CERTS` (off by
default — enables the certificate-expiry check; requires the secrets add-on
`deploy/rbac-certs.yaml` or Helm `certs.enabled=true`),
`KUBEAGENT_CERT_WARN_DAYS` (default `30` — warn window in days),
`KUBEAGENT_CONTROL_PLANE_HEALTH`, `KUBEAGENT_DNS_HEALTH`,
`KUBEAGENT_DISK_USAGE` and `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS` (default `15`).

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

On-incident `--explain` and the multi-cluster hub have both shipped — see
[above](#on-incident-explanations-explain) and
[Watching several clusters](#watching-several-clusters). Watch mode's
remaining roadmap: opt-in autonomous remediation with stricter rails than the
interactive `--fix`.
