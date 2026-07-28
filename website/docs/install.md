# Install

## As a `kubectl` plugin (krew)

With [krew](https://krew.sigs.k8s.io) installed, install kubeagent from the
manifest attached to the latest release:

```bash
kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml
kubectl kubeagent scan
```

kubeagent is not in the upstream krew-index yet, so `--manifest-url` is
required — plain `kubectl krew install kubeagent` will not find it.

!!! warning "Flags go after the plugin name"
    `kubectl` does not forward its own global flags to plugins:

    ```bash
    kubectl kubeagent scan --context prod-eu     # works
    kubectl --context prod-eu kubeagent scan     # does not
    ```

    kubeagent's `--context` and `--kubeconfig` are spelled exactly like
    kubectl's, and `KUBECONFIG` is read from the environment the same way, so
    the habit transfers intact.

## Prebuilt binary

Binaries are attached to each GitHub Release for:

| OS | Arch | Archive |
|----|------|---------|
| linux | amd64 | `kubeagent_<version>_linux_amd64.tar.gz` |
| linux | arm64 | `kubeagent_<version>_linux_arm64.tar.gz` |
| macOS | amd64 | `kubeagent_<version>_darwin_amd64.tar.gz` |
| macOS | arm64 | `kubeagent_<version>_darwin_arm64.tar.gz` |

Windows is not published: nothing in this project's test or chaos suite has
ever run on it.

Download, verify the checksum, and run:

```bash
VERSION=v1.2.3   # the release you want
OS=linux; ARCH=amd64
base="https://github.com/imantaba/kubeagent/releases/download/${VERSION}"
curl -sSLO "${base}/kubeagent_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -sSLO "${base}/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
tar xzf "kubeagent_${VERSION}_${OS}_${ARCH}.tar.gz"
./kubeagent version   # prints the build's version
./kubeagent scan
```

!!! tip "Latest release"
    Find all releases — including the latest version number to substitute for
    `VERSION` above — on the
    [Releases page](https://github.com/imantaba/kubeagent/releases).

## Run on Kubernetes (daemon)

To run kubeagent **in-cluster** as the read-only [watch daemon](features/watch-mode.md)
— continuously diagnosing the cluster and exposing Prometheus metrics — apply the
manifests in [`deploy/`](https://github.com/imantaba/kubeagent/tree/main/deploy).
They use the official image
[`imantaba/kubeagent`](https://hub.docker.com/r/imantaba/kubeagent) on Docker Hub.

```bash
# clone the repo (or download the deploy/ manifests) and apply them
git clone https://github.com/imantaba/kubeagent
kubectl create namespace kubeagent
kubectl apply -f kubeagent/deploy/
kubectl -n kubeagent rollout status deploy/kubeagent
```

This creates, in the `kubeagent` namespace:

- a **read-only** `ClusterRole` (only `get`/`list`/`watch`), `ServiceAccount`, and binding,
- a single-replica `Deployment` running `kubeagent watch` (distroless, non-root, read-only root FS), and
- a `ClusterIP` `Service` exposing `/metrics` (annotated `prometheus.io/scrape: "true"`).

Scrape it, or take a quick look:

```bash
kubectl -n kubeagent port-forward svc/kubeagent-metrics 8080:8080
curl localhost:8080/metrics
```

The daemon is **strictly read-only toward the cluster**. Its deterministic core makes
no outbound calls, and it runs fully offline unless you opt into
[`--explain`](features/watch-mode.md#on-incident-explanations-explain), which makes an
outbound HTTPS call to the model provider — an egress decision, not a cluster
operation. To pin a specific version, set the image tag in `deploy/deployment.yaml` (e.g.
`imantaba/kubeagent:v0.63.0`); to build your own image, see
[`deploy/README.md`](https://github.com/imantaba/kubeagent/blob/main/deploy/README.md).

### With Helm

The same daemon is packaged as a Helm chart under
[`deploy/helm/kubeagent/`](https://github.com/imantaba/kubeagent/tree/main/deploy/helm/kubeagent).
It renders the identical read-only RBAC, deployment, and metrics Service:

```bash
git clone https://github.com/imantaba/kubeagent
helm install kubeagent kubeagent/deploy/helm/kubeagent \
  --namespace kubeagent --create-namespace
```

Common overrides via `--set` (see the chart's `values.yaml` for the full list):

```bash
# pin an image tag (defaults to the chart appVersion)
--set image.tag=v0.63.0
# scope the daemon to one namespace, tune scan cadence
--set watch.namespace=payments --set watch.heartbeat=30s
```

Uninstall with `helm uninstall kubeagent -n kubeagent`.

## Build from source

If you have Go installed, you can build directly from the repository:

```bash
go build -o kubeagent .
```

Requires Go 1.26 or later. The resulting binary has no external runtime
dependencies.
