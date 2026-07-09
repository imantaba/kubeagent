# kubeagent — Deploy Manifests

This directory contains the Kubernetes manifests to run the `kubeagent watch` daemon
in-cluster. The daemon is **strictly read-only** (RBAC grants only `get`/`list`/`watch`
— no write verbs anywhere) and makes **no LLM calls**.

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
  --set image.tag=v0.15.0

# scope the daemon to a single namespace, tune scan cadence
helm install kubeagent deploy/helm/kubeagent -n kubeagent --create-namespace \
  --set watch.namespace=payments \
  --set watch.heartbeat=30s
```

See [`helm/kubeagent/values.yaml`](helm/kubeagent/values.yaml) for the full list
of values (image, replicas, watch cadence, metrics port, RBAC/ServiceAccount
creation, resources, security context, scheduling).

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

## Security notes

- The daemon runs as UID 65532 (non-root) with a read-only root filesystem and
  all Linux capabilities dropped.
- The `ClusterRole` grants **only** `get`, `list`, and `watch` — no `create`,
  `update`, `patch`, `delete`, or `deletecollection` anywhere.
- No LLM or external API calls are made; `kubeagent watch` is a purely
  deterministic, offline daemon.
