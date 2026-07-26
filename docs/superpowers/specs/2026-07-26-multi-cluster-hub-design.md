# Multi-cluster hub — design

**Status:** approved, ready for planning
**Date:** 2026-07-26
**Theme:** E (continuous operation), slice 5 — the last Theme E slice

## Problem

`kubeagent watch` watches exactly one cluster. An operator with three clusters
runs three daemons, scrapes three endpoints, and correlates by hand. The
roadmap promises "a multi-cluster hub" and commits to nothing further.

Three readings of that phrase produce three different products: one daemon
watching N clusters; a `kubeagent hub` process aggregating existing per-cluster
daemons over HTTP; or multi-cluster `scan` only. This design builds the first.

## Shape

`kubeagent watch --context prod-eu --context prod-us --context staging` runs one
informer set per cluster inside a single process, behind one HTTP endpoint.
Every metric series carries a `cluster` label, `/issues` and `/explanations`
carry a cluster field, and every alert names its cluster.

Chosen because it reuses the entire existing per-cluster pipeline unchanged and
invents no new network protocol — the API server's own authn/authz stays the
only trust boundary. The accepted cost is stated plainly: one process holds
read-only credentials for every cluster it watches, so that process and its
kubeconfig Secret are as sensitive as the union of those clusters.

The rejected alternatives, for the record. A `hub` aggregating per-cluster
daemons would need a new protocol, a new listener on every daemon, and a new
authentication story for hub-to-daemon traffic — a second trust boundary
kubeagent would then own. Multi-cluster `scan` alone would not deliver
continuous operation, which is the whole point of Theme E.

## Targets and naming

A **target** is one cluster the daemon watches: a name plus a client.

- `--context` becomes repeatable on `watch`. `scan` keeps its single `--context`.
- `--cluster-name` names the **default target** — the one built with no
  `--context`, from in-cluster credentials or the kubeconfig's current-context.
  Defaults to `local`.
- `--include-local` adds that default target alongside the listed contexts. Off
  by default; the chart turns it on. With no `--context` given it is a no-op,
  because the default target is then already the only target. It is not tied to
  in-cluster credentials: run outside a cluster, the default target is the
  kubeconfig's current-context, which is what makes it testable locally.
- Each `--context X` target is named `X`.

Startup fails, before anything listens, on: an empty target list, a duplicate
target name, or a client that cannot be constructed. Client construction
contacts no API server, so a failure there is a configuration error — a
misspelled context name — and must be fatal rather than degrade into silently
watching two of the three clusters an operator asked for. An API server that is
merely unreachable is not detectable here and is handled at runtime instead.

## Process shape

```go
// Target is one cluster to watch.
type Target struct {
	Name   string
	Client kubernetes.Interface
}

func Run(ctx context.Context, targets []Target, cfg Config) error
```

Single-cluster operation is a one-element slice. `Config` is unchanged except
for the fields the new flags feed.

**Per target, in its own goroutine:** the informer factory, the trigger channel,
the debounce timer, the heartbeat ticker, the `watchstate.Tracker`, the
`alertstate.Roller`, and the SLO tracker with its notifier. Each worker runs the
loop `watch.Run` runs today, against its own client, and publishes into the
shared snapshot under its own name.

**Shared, process-wide:** the HTTP server, the `metrics` snapshot, the alert
sink, and the `oncall.Explainer`.

The alert sink is shared because there is one webhook: one bounded queue, one
retry policy, one set of delivery counters. The explainer is shared because the
hourly budget is a **cost** control, and cost is a property of the process, not
of a cluster. A noisy cluster consuming the whole budget is therefore the
correct behaviour, not a bug — the budget exists precisely to cap spend
regardless of where the noise comes from.

Every log line the daemon writes gains a `[<cluster>]` prefix. Without it, three
interleaved reconcile loops produce an unreadable log.

## Cluster identity lives at the boundary

`alertstate.Object` gains a `Cluster` field. Each target owns its own
`watchstate.Tracker` and `alertstate.Roller`, so `watchstate.Key` values cannot
collide across clusters and `watchstate` needs no change at all; the Roller
stamps its own cluster name onto every Object it builds.

`oncall.Explainer` is the one shared component that keys by object, so it does
need the name: `Consider` gains a cluster parameter, and `oncall.Explanation`
gains a `Cluster` field. Without this, `shop/web` in two clusters would share
one cooldown slot and overwrite each other in the served store. `Consider`'s
existing `cluster clusterhealth.ClusterHealth` parameter is renamed `health` to
free the name.

Two alternatives were rejected. Putting `Cluster` in `watchstate.Key` touches
the tracker, key parsing, `issueLabels`, and every watchstate and alertstate
test, and buys nothing that per-cluster trackers do not already give. Prefixing
the object name (`prod-eu/shop/web`) is cheapest but produces unparseable metric
labels and a JSON `name` field that lies about what it holds.

`Object.String()` renders `prod-eu/Deployment/shop/web`, and
`prod-eu/Node/worker-2` for cluster-scoped objects.

## Metrics

Every series gains `cluster="<name>"` — **always**, including single-cluster
operation, where the value defaults to `local`.

A label that appears only once a second cluster is added would break every
dashboard and recording rule on the day an operator adds their second cluster,
which is the worst possible moment. PromQL selectors match regardless of extra
labels, so queries written against today's output keep working; only recording
rules that group `by (...)` need review, and they need it once, now, rather
than later by surprise.

`render()` iterates target names in sorted order so the output is deterministic.

New series:

- `kubeagent_cluster_up{cluster}` — 1 when the last evaluation for that cluster
  succeeded, 0 otherwise.
- `kubeagent_clusters_total` — how many targets the daemon was configured with.
  Unlabelled: it is a property of the process.

The alert and explain series stay **unlabelled**:
`kubeagent_alerts_sent_total`, `kubeagent_alerts_dropped_total`,
`kubeagent_alert_last_success_timestamp_seconds`, and every
`kubeagent_explain_*`. There is one sink and one budget; labelling them per
cluster would attribute process-wide counters to individual clusters, which is
false.

Everything else — the cluster-health gauges, `kubeagent_findings`, the issue
series, the node filesystem series, the certificate series, the SLO series, and
the scan counters — is per cluster and gains the label. The SLO series keep
their existing `window="fast"|"slow"` label alongside it; an availability SLO
computed across clusters would be meaningless, so each cluster burns its own
error budget.

The `internal/report` golden test is unaffected: it snapshots `scan` output, not
`watch` metrics. The metrics tests in `internal/watch/metrics_test.go` are
rewritten to expect the label.

## Failure isolation

The rule: **a configuration error is fatal at startup; a cluster failure at
runtime degrades that cluster only.**

At runtime, a target whose informer caches never sync, or whose evaluation
returns an error, publishes `kubeagent_cluster_up 0` and an error string on
`/issues`. Its tracked issues stay firing — the existing rule in `applyResult`,
that an evaluation error is not "all clear", already guarantees this and must
survive unchanged. Its SLO tracker records no sample, so the gap shows up
honestly as reduced window coverage. Every other target keeps reconciling on its
own schedule, unaffected.

`/readyz` reports ready once **every** target has completed its first reconcile
*attempt* — success or failure — and never flips afterward on cluster health.
Readiness answers "can this process serve?", not "is everything fine". Tying it
to cluster health would let one unreachable remote cluster pull the pod out of
its Service endpoints, stopping Prometheus from scraping it, and so blind the
operator to the clusters that are working — the exact opposite of what a
multi-cluster daemon is for.

`/healthz` is unchanged.

## HTTP surface

`/issues` gains a `cluster` field on every record. The `active` and `resolved`
arrays merge across clusters; `stats` sums across clusters. A new top-level
`clusters` array reports per-target status:

```json
{
  "clusters": [
    {"name": "prod-eu", "up": true,  "lastScan": "2026-07-26T10:04:11Z"},
    {"name": "prod-us", "up": false, "lastScan": "2026-07-26T09:58:02Z",
     "error": "Get \"https://…\": context deadline exceeded"}
  ],
  "active": [
    {"cluster": "prod-eu", "kind": "Deployment", "namespace": "shop",
     "name": "web", "issue": "CrashLoopBackOff", "firstSeen": "…",
     "firingSince": "…", "lastSeen": "…", "firings": 1, "flapping": false,
     "ageSeconds": 240}
  ],
  "resolved": [],
  "stats": {"newTotal": 4, "resolvedTotal": 3, "flapTotal": 0,
            "droppedTotal": 0, "resolutionSecondsSum": 812,
            "resolutionSecondsCount": 3}
}
```

Every field except `cluster` is what `/issues` serves today; the timestamps are
elided above only for width.

`/explanations` gains a `cluster` field on every explanation view. Its `stats`
stay process-wide, matching the unlabelled explain metrics.

The `error` field carries whatever the client returned. Client errors can embed
the API server URL, which is a host address rather than a credential, so it may
appear — but it must go through the same redaction the alert webhook uses if it
ever carries userinfo or a query string.

## Alerts

`alertstate.Notification.Object` carries the cluster, so every format gets it:

- **json** — a new `"cluster"` field alongside `kind`/`namespace`/`name`.
- **alertmanager** — a new `cluster` label alongside `kind`/`namespace`/`name`.
- **slack** — inherited through `Object.String()`, so the text reads
  `*FIRING* prod-eu/Deployment/shop/web`.

One webhook receives every cluster's alerts. Per-cluster webhooks are not in
scope: an operator who wants them runs one daemon per cluster, which still
works.

## Helm

New `multicluster` values:

```yaml
multicluster:
  enabled: false
  # A kubeconfig holding one entry per remote cluster. It is a credential, so it
  # comes from a Secret and never from values.yaml.
  existingSecret: ""
  secretKey: kubeconfig
  contexts: []
  # Also watch the cluster the daemon runs in, via its ServiceAccount.
  includeLocal: true
  localName: local
```

The Secret mounts read-only at `/etc/kubeagent/kubeconfig`. The container gets
`--kubeconfig=/etc/kubeagent/kubeconfig/<secretKey>`, one `--context` per entry,
`--cluster-name=<localName>`, and `--include-local` when `includeLocal` is true.

Template guard-rails, mirroring the `explain.*` pattern already in the chart:

- `multicluster.enabled` without `existingSecret` → `fail`, with the reason
  spelled out: the kubeconfig is a credential and must come from a Secret.
- `multicluster.enabled` with an empty `contexts` list → `fail`. Enabling
  multi-cluster and naming no remote cluster is always a mistake, whatever
  `includeLocal` is set to.

**RBAC is unchanged.** The chart's ClusterRole still grants get/list/watch in
the local cluster only. Permissions in remote clusters ride entirely on the
credentials inside the mounted kubeconfig, which kubeagent neither creates nor
validates. The documentation must say this outright: **each remote credential
must be read-only, and kubeagent cannot enforce that from inside.** A kubeconfig
with a cluster-admin token would give a read-only daemon write-capable
credentials it will never use but would nonetheless hold.

## Invariants preserved

- Every cluster is touched with get/list/watch only. No new verbs, no new local
  RBAC, no writes anywhere.
- The daemon still makes no LLM call unless `--explain` is set, and the
  explainer still receives no Kubernetes client.
- The webhook URL, the model endpoint, and now the kubeconfig are credentials:
  never an argument, never a `values.yaml` literal, never logged beyond
  `scheme://host`.

## Testing

**Unit.** N fake clientsets in one `Run`; per-cluster label rendering across
every series; a clientset that always errors, proving the other targets keep
reconciling and that the failing one reports `cluster_up 0` with its issues
still firing; `/readyz` reporting ready with one target permanently broken;
startup rejection of duplicate names, an empty target list, and a bad context;
`Object.String()` and all three alert encoders carrying the cluster.

**Chaos scenario 15**, without paying for a second Kind cluster. The harness
adds a second context to the same Kind cluster under a different name, plus a
third context pointing at a dead endpoint:

- two names for the same cluster prove labelling and cross-cluster merge — the
  same broken workload must appear twice, once per cluster label;
- the dead third target proves degradation: `kubeagent_cluster_up{cluster="…"} 0`
  for it, `1` for the others, `/readyz` still 200, and the working clusters'
  issues still tracked.

This is a real test of the isolation logic, which is the part most likely to
regress. It does not test genuinely divergent cluster state, and the scenario
text must say so rather than overclaim.

## Documentation

- `website/docs/` — the watch reference gains the multi-cluster flags, the
  `cluster` label, and the read-only-credential warning.
- `deploy/README.md` and the chart README — the `multicluster` values and the
  Secret requirement.
- `docs/go-concepts.md` — running N informer sets in one process: one goroutine
  per target sharing a mutex-guarded snapshot. New concept for this project;
  everyday example first, then the kubeagent example, no Python comparisons.
- `CHANGELOG.md` — under Added, including the metrics-shape note: every series
  now carries `cluster`, defaulting to `local`.
- `website/docs/roadmap.md` — Theme E complete.

## Out of scope

- Per-cluster webhooks, per-cluster budgets, per-cluster SLO targets. The
  cross-cutting settings stay global; an operator needing them split runs one
  daemon per cluster.
- Cross-cluster correlation ("the same image is failing in all three"). A
  separate feature that this slice makes possible but does not attempt.
- Multi-cluster `scan`. The one-shot CLI stays single-cluster.
- Any hub-to-daemon protocol.
