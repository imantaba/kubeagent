# kubeagent — Design: platform facts line

**Status:** approved design (pre-implementation)
**Date:** 2026-06-28

## Goal

Add a second line under the cluster verdict that reports the detected platform
stack, so an operator (and `--explain`) instantly knows what they're working with:

`Platform: Cilium CNI · Traefik ingress · Hetzner CSI storage (+NFS CSI) · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud`

Six facts: **CNI**, **ingress**, **storage**, **Kubernetes version+distro**,
**container runtime**, **cloud/provider**.

## Decisions (from brainstorming)

- **Facts included:** CNI, ingress, storage (requested) + Kubernetes
  version&distro, container runtime, cloud/provider. **No** add-on detection
  (cert-manager / service mesh / metrics-server in the line).
- **Surfacing:** always on — line 2 in text (under the verdict, above the
  Resources block), a `platform` object in JSON, and a `Platform:` line in the
  `--explain` prompt.
- **Detection:** authoritative API fields where they exist (StorageClass
  `.provisioner`, IngressClass `.spec.controller`, node
  `kubeletVersion`/`containerRuntimeVersion`/`providerID`); name-heuristics for
  CNI (kube-system DaemonSet names). Every fact degrades to omitted when
  undetected.

## Invariants preserved

- **READ-ONLY:** only new List calls (StorageClasses, IngressClasses, kube-system
  DaemonSets). Never mutate.
- **No new Go module dependency.** `k8s.io/api/storage/v1` and
  `k8s.io/api/networking/v1` are subpackages of `k8s.io/api`, already required
  (corev1/appsv1 come from it). No go.mod change.
- **Sequential**, stdlib `flag`, exit codes unchanged.
- **Cluster-wide regardless of `-n`:** facts come from cluster-scoped resources
  (StorageClasses/IngressClasses/nodes) and an explicit kube-system DaemonSet
  list, so a namespace-scoped scan still reports the true platform.
- **`--explain` egress:** only infrastructure **type names** are sent — never pod
  IPs, per-node names, raw specs, secrets, or the raw `providerID` (which embeds
  an instance ID). Only the derived cloud name (e.g. "Hetzner Cloud") is emitted.

## Architecture

A new pure detection step sits beside the existing cluster steps:

```text
collect (nodes, +kube-system DaemonSets, +StorageClasses, +IngressClasses)
      → platform.Detect(nodes, systemDaemonSets, scs, ics)  ← new pure step
      → report / explain  (alongside the existing resources.Summary)
```

## Component 1 — `internal/platform` (pure detection)

```go
// Storage is one detected storage provisioner (friendly name) and whether it is
// the cluster default StorageClass.
type Storage struct {
    Name    string `json:"name"`
    Default bool   `json:"default,omitempty"`
}

// Facts is the detected platform stack. Every field is best-effort; an
// undetected fact is the zero value (omitted from output).
type Facts struct {
    CNI         string    `json:"cni,omitempty"`         // "Cilium"
    Ingress     string    `json:"ingress,omitempty"`     // "Traefik"
    Storage     []Storage `json:"storage,omitempty"`     // default first, then by name
    KubeVersion string    `json:"kubeVersion,omitempty"` // "v1.35"
    Distro      string    `json:"distro,omitempty"`      // "RKE2" | "k3s" | "EKS" | "GKE" | ""
    Runtime     string    `json:"runtime,omitempty"`     // "containerd"
    Cloud       string    `json:"cloud,omitempty"`       // "Hetzner Cloud"
}

// Detect derives platform Facts from cluster-wide inputs. It reads only the
// fields named below; anything it cannot classify is left empty.
func Detect(nodes []corev1.Node, systemDaemonSets []appsv1.DaemonSet,
            scs []storagev1.StorageClass, ics []networkingv1.IngressClass) Facts
```

Detection rules (each a small lookup with a documented fallback):

- **CNI** — first kube-system DaemonSet whose name matches, in priority order:
  `cilium`→Cilium, `calico-node`→Calico, `canal`→Canal, `kube-flannel`→Flannel,
  `weave-net`→Weave Net, `antrea-agent`→Antrea, `kube-ovn`→Kube-OVN,
  `aws-node`→AWS VPC CNI. No match → "".
- **Ingress** — first IngressClass `.spec.controller`:
  `traefik.io/ingress-controller`→Traefik, `k8s.io/ingress-nginx`→ingress-nginx,
  `haproxy.org/ingress-controller`→HAProxy, `projectcontour.io/contour`→Contour,
  `ingress.k8s.aws/alb`→AWS ALB; otherwise the raw controller string. No
  IngressClass → "".
- **Storage** — one `Storage` per StorageClass, `Name` from a friendly map on
  `.provisioner` (`csi.hetzner.cloud`→Hetzner CSI, `nfs.csi.k8s.io`→NFS CSI,
  `ebs.csi.aws.com`→AWS EBS, `pd.csi.storage.gke.io`→GCE PD,
  `disk.csi.azure.com`→Azure Disk, `driver.longhorn.io`→Longhorn,
  `rancher.io/local-path`→local-path, `kubernetes.io/no-provisioner`→static)
  else the raw provisioner. `Default` from the
  `storageclass.kubernetes.io/is-default-class: "true"` annotation. Deduplicate
  by `Name`; sort default-first then alphabetically.
- **KubeVersion / Distro** — from the first node's `status.nodeInfo.kubeletVersion`
  (e.g. `v1.35.4+rke2r1`): `KubeVersion` = `v<major>.<minor>` ("v1.35");
  `Distro` from the build suffix — contains `rke2`→RKE2, `k3s`→k3s, `eks`→EKS,
  `gke`→GKE; else "".
- **Runtime** — first node `status.nodeInfo.containerRuntimeVersion` before `://`
  ("containerd").
- **Cloud** — first node `spec.providerID` **scheme** (text before `://`):
  `hcloud`→Hetzner Cloud, `aws`→AWS, `gce`→GCP, `azure`→Azure,
  `digitalocean`→DigitalOcean, `vsphere`→vSphere; else "". The raw providerID is
  never stored.

## Component 2 — collection (`internal/collect`)

Three read-only Lists (each wraps its error; the caller treats failure as
best-effort and proceeds with empty input):

```go
func StorageClasses(ctx context.Context, client kubernetes.Interface) ([]storagev1.StorageClass, error)
func IngressClasses(ctx context.Context, client kubernetes.Interface) ([]networkingv1.IngressClass, error)
func SystemDaemonSets(ctx context.Context, client kubernetes.Interface) ([]appsv1.DaemonSet, error) // kube-system
```

- `StorageClasses` → `client.StorageV1().StorageClasses().List(...)`.
- `IngressClasses` → `client.NetworkingV1().IngressClasses().List(...)`.
- `SystemDaemonSets` → `client.AppsV1().DaemonSets("kube-system").List(...)`.

## Component 3 — surfacing

### text (`internal/report`)

A `Platform:` line prints directly after the cluster-verdict block and before the
Resources block:

```text
Platform: Cilium CNI · Traefik ingress · Hetzner CSI storage (+NFS CSI) · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud
```

Built from only the non-empty facts, joined by ` · `:
- CNI → `<CNI> CNI`; Ingress → `<Ingress> ingress`.
- Storage → `<default> storage` plus `(+<other>, <other>)` when there is more
  than one; if no default flag, just lists them as `<a>, <b> storage`.
- KubeVersion → `Kubernetes <ver>` with ` (<Distro>)` appended when Distro set.
- Runtime → as-is; Cloud → as-is.

If `facts` is nil or all fields are empty, the line is omitted entirely.

### json (`internal/report`)

`inventoryReport` gains `Platform *platform.Facts` (`json:"platform,omitempty"`).

### explain (`internal/explain`)

`buildInventoryPrompt` emits a single line when facts are present, e.g.:
`Platform: CNI Cilium, ingress Traefik, storage Hetzner CSI (default)/NFS CSI, Kubernetes v1.35 (RKE2), runtime containerd, cloud Hetzner Cloud.`
`ExplainInventory` takes the `*platform.Facts` as a new parameter; the
healthy-and-empty skip is unchanged.

## Component 4 — wiring (`main.go`)

After `collect.Nodes(...)` and the existing metrics/summary block:

```go
scs, _ := collect.StorageClasses(ctx, client)
ics, _ := collect.IngressClasses(ctx, client)
sysDS, _ := collect.SystemDaemonSets(ctx, client)
facts := platform.Detect(nodes, sysDS, scs, ics)
```

`&facts` is threaded into `report.PrintInventory` and
`explain.ExplainInventory` as a new parameter alongside the existing
`*resources.Summary`. Everything else in `run` is unchanged.

## Testing (TDD)

- `platform.Detect` — table tests: each CNI / ingress / storage / cloud / distro
  mapping; the raw-string fallbacks; the "unknown"/empty cases; default-first
  storage ordering and dedupe; the full hetzner-nova combination
  (Cilium/Traefik/Hetzner CSI+NFS/v1.35 RKE2/containerd/Hetzner Cloud).
- `collect` — `StorageClasses`, `IngressClasses`, `SystemDaemonSets` via the fake
  clientset (objects across namespaces; kube-system filter for DaemonSets).
- `report` — the Platform line renders with all facts, omits empty segments, and
  the whole line is omitted when facts nil/empty; JSON carries `platform`.
- `explain` — the prompt includes the Platform line when facts present and omits
  it when nil; egress guard (no providerID / IP / node-name leak).
- `main` — wiring stays green; List failures are non-fatal.

## Out of scope (explicit non-goals)

- Add-on detection (cert-manager, service mesh, metrics-server) in the line.
- Per-node platform differences (facts are derived from the first node + cluster
  resources; a heterogeneous cluster reports the first node's runtime/version).
- Deep CNI/ingress version reporting (type only, not version).
- A flag to toggle the line (always on per the decision above).
- Grouping the growing cluster-context parameters into one struct (possible
  later refactor; this change keeps the established explicit-param style).
