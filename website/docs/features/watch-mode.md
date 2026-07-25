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
