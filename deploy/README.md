# kubeagent — Deploy Manifests

This directory contains the Kubernetes manifests to run the `kubeagent watch` daemon
in-cluster. The daemon is **strictly read-only toward the cluster** (RBAC grants only
`get`/`list`/`watch` — no write verbs anywhere); its deterministic core makes no
outbound calls, and only `--explain` adds an outbound HTTPS call to the model
provider (see the Security notes below).

## Quick start

### 1. Create the namespace

```bash
kubectl create namespace kubeagent
```

### 2. The image

The manifests use the official image
[`imantaba/kubeagent`](https://hub.docker.com/r/imantaba/kubeagent) on Docker
Hub (distroless, non-root), pinned to a release version — no build step needed.

To build and use your own instead:

```bash
docker build -t <your-registry>/kubeagent:v0.9.0 --build-arg VERSION=v0.9.0 .
docker push <your-registry>/kubeagent:v0.9.0
# then update the image reference in deploy/deployment.yaml
```

### 3. Apply all manifests

```bash
kubectl apply -f deploy/
```

This creates:
- `ServiceAccount`, `ClusterRole` (read-only), and `ClusterRoleBinding` from `rbac.yaml`
- A single-replica `Deployment` running `kubeagent watch` from `deployment.yaml`
- A `ClusterIP` Service exposing the metrics endpoint from `service.yaml`

**Every `rbac*.yaml` in this directory, and the Helm chart's `ClusterRole`, are
generated** from a single feature table in `internal/rbacprofile` — do not
hand-edit them. To change a grant, edit the table and regenerate:

```bash
go test ./internal/rbacprofile -run TestGeneratedManifests -update
```

A golden test fails the build if a manifest ever drifts from the table. If
none of the shipped manifests are narrow enough — say, an identity that only
ever runs `scan --certs` and nothing else — `kubeagent rbac print --features
core,certs` prints exactly that role instead of the full `full` profile. See
[Least-privilege RBAC](https://k8sproject.top/features/rbac/) for `rbac print`
and `rbac check` in full.

### 4. Verify the daemon is running

```bash
kubectl -n kubeagent get pods
kubectl -n kubeagent logs -l app=kubeagent
```

### 5. Scrape metrics locally

```bash
kubectl -n kubeagent port-forward svc/kubeagent-metrics 8080:8080
curl localhost:8080/metrics
```

Prometheus will auto-discover the metrics endpoint via the
`prometheus.io/scrape: "true"` annotation on the Service (if your cluster runs
a standard Prometheus stack).

## Helm chart

The same daemon is packaged as a Helm chart under [`helm/kubeagent/`](helm/kubeagent/).
It renders the identical read-only RBAC, deployment, and metrics Service, with the
common knobs exposed as values.

```bash
helm install kubeagent deploy/helm/kubeagent \
  --namespace kubeagent --create-namespace
```

Useful overrides:

```bash
# pin a different image tag (defaults to the chart's appVersion)
helm install kubeagent deploy/helm/kubeagent -n kubeagent --create-namespace \
  --set image.tag=v1.12.0

# scope the daemon to a single namespace, tune scan cadence
helm install kubeagent deploy/helm/kubeagent -n kubeagent --create-namespace \
  --set watch.namespace=payments \
  --set watch.heartbeat=30s

# name this cluster, so its metrics do not all read cluster="local"
helm install kubeagent deploy/helm/kubeagent -n kubeagent --create-namespace \
  --set watch.clusterName=prod-eu
```

`watch.clusterName` sets the `cluster` label on every metric series and the
`cluster` field in `/issues` and `/explanations`. Left empty the daemon uses its
own default, `local` — fine for one cluster, useless the moment a second
cluster's metrics land in the same Prometheus. It names the same thing as
`multicluster.localName` below and takes precedence over it.

See [`helm/kubeagent/values.yaml`](helm/kubeagent/values.yaml) for the full list
of values (image, replicas, watch cadence, cluster name, metrics port,
RBAC/ServiceAccount creation, resources, security context, scheduling).

Uninstall:

```bash
helm uninstall kubeagent -n kubeagent
```

## Disk usage (opt-in)

Applying `deploy/rbac-diskusage.yaml` (or setting Helm `diskUsage.enabled=true`)
grants the `nodes/proxy` `get` subresource and sets `KUBEAGENT_DISK_USAGE=true`
in the daemon environment. Without this add-on, kubeagent stays strictly
`get`/`list`/`watch` and makes no kubelet proxy calls. When enabled, the daemon
also exposes `kubeagent_node_fs_usage_ratio{node}` and
`kubeagent_volumes_over_disk_threshold` as Prometheus gauges.

## Certificate expiry (opt-in)

Applying `deploy/rbac-certs.yaml` (or setting Helm `certs.enabled=true`) grants
the kubeagent ServiceAccount `list` on Secrets and sets `KUBEAGENT_CERTS=true`
and `KUBEAGENT_CERT_WARN_DAYS=30` (override via `--set certs.warnDays=<days>`)
in the daemon environment. Without this add-on, kubeagent makes **no** Secrets
API calls at all. Only the public certificate (`tls.crt`) of
`kubernetes.io/tls` Secrets is inspected — `tls.key` is never read and no
Secret values are ever printed.

## Crash log root-cause (opt-in)

Applying `deploy/rbac-logs.yaml` grants the `pods/log` `get` subresource needed
by `scan --logs`. This is a scan-only add-on (not used by the watch daemon);
most human kubeconfigs already allow `pods/log`. Without it, `--logs` reports no
log cause and continues non-fatally.

## Operator health (opt-in)

Applying `deploy/rbac-operators.yaml` grants `list` on the custom resources
`scan --operators` reads: cert-manager, CloudNativePG, Longhorn, Argo CD, Flux,
and the Prometheus operator. This is a scan-only add-on (not used by the watch
daemon, and not wired into the Helm chart); most human kubeconfigs already allow
these. Without it, `--operators` still names which operators are installed — API
discovery is open to every authenticated user — and marks each kind forbidden.

kubeagent only ever `list`s these resources, and the report carries metadata and
state alone: namespace, name, kind, state, and the operator's own condition
reason. No CR `spec` content is read into the report.

## GitOps drift (opt-in)

Applying `deploy/rbac-gitops.yaml` grants `list` on the three custom resources
`scan --drift` reads: Argo CD `Application`s and Flux `Kustomization`s and
`HelmRelease`s. This is a scan-only add-on (not used by the watch daemon, and
not wired into the Helm chart); most human kubeconfigs already allow these.
Without it, `--drift` still names which reconciler is installed — API discovery
is open to every authenticated user — and marks each kind forbidden.

Its three rules are a subset of `deploy/rbac-operators.yaml`, so applying that
file alone covers both flags; this one exists so a drift-only user needs no
grant on Longhorn volumes or CNPG clusters. Nothing is compared against a Git
host: every signal comes from the reconciler's own status, and revisions are
reduced to a 7-character SHA or withheld, so no repository URL, `spec` content,
or condition message reaches the report.

## Alerting (opt-in)

The daemon can POST one alert per broken object to a receiver — generic JSON, a
Slack incoming webhook, Alertmanager's `/api/v2/alerts`, or PagerDuty's Events
API v2. It stays read-only toward the cluster; the receiver is its only other
egress.

The credential is read from the environment and never passed as a flag. For
JSON, Slack and Alertmanager that is the webhook URL:

```bash
kubectl -n kubeagent create secret generic kubeagent-alerts \
  --from-literal=webhook-url=<WEBHOOK_URL>

helm upgrade --install kubeagent deploy/helm/kubeagent \
  --set alerts.enabled=true \
  --set alerts.format=slack \
  --set alerts.existingSecret=kubeagent-alerts
```

For PagerDuty it is the integration key, in the same Secret shape — the chart
grows no new values, and the endpoint defaults to PagerDuty's published one:

```bash
kubectl -n kubeagent create secret generic kubeagent-pagerduty \
  --from-literal=routing-key=<ROUTING_KEY>

helm upgrade --install kubeagent deploy/helm/kubeagent \
  --set alerts.enabled=true \
  --set alerts.format=pagerduty \
  --set alerts.existingSecret=kubeagent-pagerduty \
  --set alerts.secretKey=routing-key
```

Only `scheme://host` is ever logged, and the routing key is never logged at all.
See the [watch mode docs](https://k8sproject.top/features/watch-mode/) for the
payload shapes and the Alertmanager cadence rule.

## SLO burn rate (opt-in)

The daemon can also track an availability SLO and expose multi-window
error-budget burn rate as Prometheus gauges. It needs no extra RBAC — it reads
the evaluation the daemon already performs — and stays off unless a target is
set:

```bash
helm upgrade --install kubeagent deploy/helm/kubeagent \
  --set slo.enabled=true \
  --set slo.target=99.9
```

`slo.target` is a percentage and must be greater than 0 and less than 100;
enabling with a target outside that range fails the chart render rather than
producing a daemon that starts and immediately errors. See the
[watch mode docs](https://k8sproject.top/features/watch-mode/#slo-burn-rate)
for the SLI definition, the fixed windows/thresholds, and the restart caveat.

## In-cluster dashboard (opt-in)

Serve the read-only dashboard on the metrics port:

```bash
helm upgrade --install kubeagent deploy/helm/kubeagent \
  --namespace kubeagent --create-namespace \
  --set dashboard.enabled=true
```

It is unauthenticated, exactly like `/metrics` and `/issues` on the same port,
and kubeagent terminates no TLS. Keep the Service `ClusterIP`, or put an
authenticating proxy in front. It adds no Service port and no RBAC rule.

## Multi-cluster hub (opt-in)

The daemon can watch several clusters from one process: one informer set per
context, one process, one `/metrics` endpoint. This needs no extra RBAC —
remote access rides entirely on the credentials inside the mounted
kubeconfig, not this cluster's ServiceAccount — and the chart's `ClusterRole`
still covers the **local** cluster only.

The kubeconfig is a credential and is NEVER a chart value: a `values.yaml`
file lands in Git, in `helm get values`, and in CI logs, and a kubeconfig
carries the credentials for every cluster it names. Create a Secret and name
it in `multicluster.existingSecret`. **Each credential inside that kubeconfig
must itself be read-only (get/list/watch)** — kubeagent issues no writes, but
it cannot enforce that from inside the pod: a kubeconfig holding a
cluster-admin token would give this daemon write-capable credentials it
never uses but nonetheless holds.

```bash
kubectl -n kubeagent create secret generic kubeagent-clusters \
  --from-file=kubeconfig=<PLACEHOLDER>

helm upgrade --install kubeagent deploy/helm/kubeagent -n kubeagent \
  --set multicluster.enabled=true \
  --set multicluster.existingSecret=kubeagent-clusters \
  --set 'multicluster.contexts={prod-eu,prod-us}'
```

By default the chart also watches the cluster it runs in, alongside the
listed contexts (`multicluster.includeLocal=true`), labelled by
`watch.clusterName` — or by `multicluster.localName` (default `local`) when
that is empty; set `includeLocal=false` to watch only the remote contexts. See the
[watch mode docs](https://k8sproject.top/features/watch-mode/#watching-several-clusters)
for the per-cluster metric label, the `/issues` and `/explanations` cluster
fields, and the config-error-vs-runtime-failure isolation model.

## Security notes

- The daemon runs as UID 65532 (non-root) with a read-only root filesystem and
  all Linux capabilities dropped.
- The `ClusterRole` grants **only** `get`, `list`, and `watch` — no `create`,
  `update`, `patch`, `delete`, or `deletecollection` anywhere.
- Read-only means read-only *toward the cluster*. The deterministic core makes
  no outbound calls at all, and without `--explain` the daemon runs entirely
  offline. With `--explain` it makes an outbound HTTPS call to the model
  provider — an egress decision to make deliberately, not a cluster operation.
- Multi-cluster mode adds no new RBAC: remote access rides entirely on the
  credentials inside the mounted kubeconfig Secret, not this cluster's
  ServiceAccount. Each of those credentials must itself be read-only.
