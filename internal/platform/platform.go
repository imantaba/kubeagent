// Package platform detects a cluster's platform stack — CNI, ingress, storage,
// Kubernetes version/distro, container runtime, and cloud — from cluster-wide
// resources. Detection is best-effort: an unrecognized fact is left empty.
package platform

import (
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
)

// Storage is one detected storage provisioner (friendly name) and whether it is
// the cluster default StorageClass.
type Storage struct {
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
}

// Facts is the detected platform stack. Every field is best-effort; an
// undetected fact is the zero value.
type Facts struct {
	CNI         string    `json:"cni,omitempty"`
	Ingress     string    `json:"ingress,omitempty"`
	Storage     []Storage `json:"storage,omitempty"`
	KubeVersion string    `json:"kubeVersion,omitempty"`
	Distro      string    `json:"distro,omitempty"`
	Runtime     string    `json:"runtime,omitempty"`
	Cloud       string    `json:"cloud,omitempty"`
}

// cniByDaemonSet maps a known CNI DaemonSet name fragment to its product name,
// in priority order (first match wins).
// Matching is best-effort substring (e.g. "kube-flannel" matches "kube-flannel-ds"); an unrelated kube-system DaemonSet whose name contains a fragment (e.g. "canal") could false-positive.
var cniByDaemonSet = []struct{ fragment, product string }{
	{"cilium", "Cilium"},
	{"calico-node", "Calico"},
	{"canal", "Canal"},
	{"kube-flannel", "Flannel"},
	{"weave-net", "Weave Net"},
	{"antrea-agent", "Antrea"},
	{"kube-ovn", "Kube-OVN"},
	{"aws-node", "AWS VPC CNI"},
}

var ingressByController = map[string]string{
	"traefik.io/ingress-controller":  "Traefik",
	"k8s.io/ingress-nginx":           "ingress-nginx",
	"haproxy.org/ingress-controller": "HAProxy",
	"projectcontour.io/contour":      "Contour",
	"ingress.k8s.aws/alb":            "AWS ALB",
}

var storageByProvisioner = map[string]string{
	"csi.hetzner.cloud":            "Hetzner CSI",
	"nfs.csi.k8s.io":               "NFS CSI",
	"ebs.csi.aws.com":              "AWS EBS",
	"pd.csi.storage.gke.io":        "GCE PD",
	"disk.csi.azure.com":           "Azure Disk",
	"driver.longhorn.io":           "Longhorn",
	"rancher.io/local-path":        "local-path",
	"kubernetes.io/no-provisioner": "static",
}

var cloudByScheme = map[string]string{
	"hcloud":       "Hetzner Cloud",
	"aws":          "AWS",
	"gce":          "GCP",
	"azure":        "Azure",
	"digitalocean": "DigitalOcean",
	"vsphere":      "vSphere",
}

// Detect derives platform Facts from cluster-wide inputs.
func Detect(nodes []corev1.Node, systemDaemonSets []appsv1.DaemonSet, scs []storagev1.StorageClass, ics []networkingv1.IngressClass) Facts {
	var f Facts
	f.CNI = detectCNI(systemDaemonSets)
	f.Ingress = detectIngress(ics)
	f.Storage = detectStorage(scs)
	if len(nodes) > 0 {
		n := nodes[0]
		f.KubeVersion, f.Distro = parseKubeVersion(n.Status.NodeInfo.KubeletVersion)
		f.Runtime = before(n.Status.NodeInfo.ContainerRuntimeVersion, "://")
		f.Cloud = cloudByScheme[before(n.Spec.ProviderID, "://")]
	}
	return f
}

func detectCNI(dss []appsv1.DaemonSet) string {
	for _, want := range cniByDaemonSet {
		for _, d := range dss {
			if strings.Contains(d.Name, want.fragment) {
				return want.product
			}
		}
	}
	return ""
}

// detectIngress returns the first recognized controller (mapped to a friendly name); if none is recognized it falls back to the first non-empty raw controller string. Order follows the IngressClass list, which the API does not guarantee — best-effort for the normal single-controller case.
func detectIngress(ics []networkingv1.IngressClass) string {
	var firstRaw string
	for _, c := range ics {
		ctrl := c.Spec.Controller
		if name, ok := ingressByController[ctrl]; ok {
			return name
		}
		if firstRaw == "" && ctrl != "" {
			firstRaw = ctrl
		}
	}
	return firstRaw
}

func detectStorage(scs []storagev1.StorageClass) []Storage {
	seen := map[string]int{} // name -> index in out
	var out []Storage
	for _, s := range scs {
		name := storageByProvisioner[s.Provisioner]
		if name == "" {
			name = s.Provisioner
		}
		if name == "" {
			continue
		}
		isDefault := s.Annotations["storageclass.kubernetes.io/is-default-class"] == "true"
		if i, ok := seen[name]; ok {
			if isDefault {
				out[i].Default = true
			}
			continue
		}
		seen[name] = len(out)
		out = append(out, Storage{Name: name, Default: isDefault})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// parseKubeVersion returns "vMAJOR.MINOR" and the distro inferred from the build
// metadata suffix (rke2/k3s/eks/gke), each empty when undetected.
func parseKubeVersion(v string) (version, distro string) {
	if v == "" {
		return "", ""
	}
	low := strings.ToLower(v)
	switch {
	case strings.Contains(low, "rke2"):
		distro = "RKE2"
	case strings.Contains(low, "k3s"):
		distro = "k3s"
	case strings.Contains(low, "eks"):
		distro = "EKS"
	case strings.Contains(low, "gke"):
		distro = "GKE"
	}
	base := v
	// RKE2/k3s use a '+' build suffix (v1.35.4+rke2r1); EKS uses a '-' suffix (v1.29.4-eks-036c24b) with no '+', so Split(".")[1] (the minor) is still clean either way.
	if i := strings.IndexByte(base, '+'); i >= 0 {
		base = base[:i]
	}
	parts := strings.Split(strings.TrimPrefix(base, "v"), ".")
	if len(parts) >= 2 {
		version = "v" + parts[0] + "." + parts[1]
	} else {
		version = base
	}
	return version, distro
}

// before returns s up to the first occurrence of sep, or "" when sep is absent.
func before(s, sep string) string {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i]
	}
	return ""
}

// Line renders the facts as a single human-readable summary, e.g.
// "Cilium CNI · Traefik ingress · Hetzner CSI (+NFS CSI) storage · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud".
// It returns "" when no fact is set.
func (f Facts) Line() string {
	var parts []string
	if f.CNI != "" {
		parts = append(parts, f.CNI+" CNI")
	}
	if f.Ingress != "" {
		parts = append(parts, f.Ingress+" ingress")
	}
	if s := f.storageSummary(); s != "" {
		parts = append(parts, s+" storage")
	}
	if f.KubeVersion != "" {
		v := "Kubernetes " + f.KubeVersion
		if f.Distro != "" {
			v += " (" + f.Distro + ")"
		}
		parts = append(parts, v)
	}
	if f.Runtime != "" {
		parts = append(parts, f.Runtime)
	}
	if f.Cloud != "" {
		parts = append(parts, f.Cloud)
	}
	return strings.Join(parts, " · ")
}

func (f Facts) storageSummary() string {
	if len(f.Storage) == 0 {
		return ""
	}
	primary := f.Storage[0].Name
	var others []string
	for _, s := range f.Storage[1:] {
		others = append(others, s.Name)
	}
	if len(others) > 0 {
		return primary + " (+" + strings.Join(others, ", ") + ")"
	}
	return primary
}
