# kubeagent — Deploy Manifests

This directory contains the Kubernetes manifests to run the `kubeagent watch` daemon
in-cluster. The daemon is **strictly read-only** (RBAC grants only `get`/`list`/`watch`
— no write verbs anywhere) and makes **no LLM calls**.

## Quick start

### 1. Create the namespace

```bash
kubectl create namespace kubeagent
```

### 2. Build and push your image

The manifests reference `ghcr.io/imantaba/kubeagent:latest` as a placeholder.
Replace it with your own published image before applying:

```bash
# build your image (the release tarball is a binary; wrapping it in a container
# image is the operator's step)
docker build -t ghcr.io/<your-org>/kubeagent:latest .
docker push ghcr.io/<your-org>/kubeagent:latest

# update the image reference in deploy/deployment.yaml, then apply:
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

## Security notes

- The daemon runs as UID 65532 (non-root) with a read-only root filesystem and
  all Linux capabilities dropped.
- The `ClusterRole` grants **only** `get`, `list`, and `watch` — no `create`,
  `update`, `patch`, `delete`, or `deletecollection` anywhere.
- No LLM or external API calls are made; `kubeagent watch` is a purely
  deterministic, offline daemon.
