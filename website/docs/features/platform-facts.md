# Platform facts

`scan` prints a second line under the cluster verdict naming the detected
stack — CNI, ingress, storage provisioner(s), Kubernetes version and
distribution, container runtime, and cloud provider.

## Example

```text
Platform: Cilium CNI · Traefik ingress · Kubernetes v1.30 · containerd
```

A fuller example with storage and cloud:

```text
Platform: Cilium CNI · Traefik ingress · Longhorn storage · Kubernetes v1.30 (RKE2) · containerd · Hetzner Cloud
```

!!! note
    The examples above are synthetic. Your cluster's output will reflect
    its own stack.

## How detection works

Detection is **best-effort and read-only**:

- Lists `StorageClasses` and `IngressClasses`.
- Inspects `kube-system` DaemonSets to identify the CNI.
- Reads node info for the Kubernetes version, container runtime, and cloud
  provider hint.

An unrecognized fact is silently omitted — no placeholder is printed for
components `kubeagent` cannot identify.

No instance identifiers (such as the raw `providerID`) are emitted. Only the
derived cloud name (e.g. `Hetzner Cloud`, `AWS`) is shown.

## Where the data appears

| Output mode | Location |
|-------------|----------|
| Text (`--output text`) | Second line of the cluster verdict block |
| JSON (`--output json`) | `platform` field at the top level |
| `--explain` | Included in the prompt so the model can give stack-aware advice |
