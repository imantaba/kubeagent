package platform

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ds(name string) appsv1.DaemonSet {
	return appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: name}}
}

func sc(name, provisioner string, isDefault bool) storagev1.StorageClass {
	s := storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: provisioner}
	if isDefault {
		s.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	}
	return s
}

func ic(name, controller string) networkingv1.IngressClass {
	return networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: networkingv1.IngressClassSpec{Controller: controller}}
}

func node(kubelet, runtime, providerID string) corev1.Node {
	return corev1.Node{
		Spec: corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			KubeletVersion:          kubelet,
			ContainerRuntimeVersion: runtime,
		}},
	}
}

func TestDetect_HetznerNovaCombo(t *testing.T) {
	f := Detect(
		[]corev1.Node{node("v1.35.4+rke2r1", "containerd://2.2.3-k3s1", "hcloud://131304002")},
		[]appsv1.DaemonSet{ds("cilium"), ds("hcloud-csi-node"), ds("rke2-traefik")},
		[]storagev1.StorageClass{sc("hcloud-volumes", "csi.hetzner.cloud", true), sc("hcloud-volumes-retain", "csi.hetzner.cloud", false), sc("nfs", "nfs.csi.k8s.io", false)},
		[]networkingv1.IngressClass{ic("traefik", "traefik.io/ingress-controller")},
	)
	if f.CNI != "Cilium" {
		t.Errorf("CNI = %q, want Cilium", f.CNI)
	}
	if f.Ingress != "Traefik" {
		t.Errorf("Ingress = %q, want Traefik", f.Ingress)
	}
	if len(f.Storage) != 2 || f.Storage[0].Name != "Hetzner CSI" || !f.Storage[0].Default || f.Storage[1].Name != "NFS CSI" {
		t.Errorf("Storage = %+v, want [Hetzner CSI(default), NFS CSI]", f.Storage)
	}
	if f.KubeVersion != "v1.35" || f.Distro != "RKE2" {
		t.Errorf("version/distro = %q/%q, want v1.35/RKE2", f.KubeVersion, f.Distro)
	}
	if f.Runtime != "containerd" || f.Cloud != "Hetzner Cloud" {
		t.Errorf("runtime/cloud = %q/%q, want containerd/Hetzner Cloud", f.Runtime, f.Cloud)
	}
	want := "Cilium CNI · Traefik ingress · Hetzner CSI (+NFS CSI) storage · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud"
	if got := f.Line(); got != want {
		t.Errorf("Line()\n got %q\nwant %q", got, want)
	}
}

func TestDetect_FallbacksAndUnknowns(t *testing.T) {
	// Unknown CNI daemonset, raw ingress controller, raw provisioner, EKS distro, no providerID.
	f := Detect(
		[]corev1.Node{node("v1.29.4-eks-036c24b", "containerd://1.7.0", "")},
		[]appsv1.DaemonSet{ds("some-random-agent")},
		[]storagev1.StorageClass{sc("custom", "example.com/custom", false)},
		[]networkingv1.IngressClass{ic("x", "example.com/my-ingress")},
	)
	if f.CNI != "" {
		t.Errorf("CNI = %q, want empty (unknown)", f.CNI)
	}
	if f.Ingress != "example.com/my-ingress" {
		t.Errorf("Ingress = %q, want raw controller fallback", f.Ingress)
	}
	if len(f.Storage) != 1 || f.Storage[0].Name != "example.com/custom" {
		t.Errorf("Storage = %+v, want raw provisioner fallback", f.Storage)
	}
	if f.KubeVersion != "v1.29" || f.Distro != "EKS" {
		t.Errorf("version/distro = %q/%q, want v1.29/EKS", f.KubeVersion, f.Distro)
	}
	if f.Cloud != "" {
		t.Errorf("Cloud = %q, want empty (no providerID)", f.Cloud)
	}
}

func TestDetect_CNIVariants(t *testing.T) {
	cases := []struct{ dsName, want string }{
		{"calico-node", "Calico"},
		{"canal", "Canal"},
		{"kube-flannel-ds", "Flannel"}, // substring match
		{"weave-net", "Weave Net"},
		{"aws-node", "AWS VPC CNI"},
	}
	for _, c := range cases {
		f := Detect([]corev1.Node{}, []appsv1.DaemonSet{ds(c.dsName)}, nil, nil)
		if f.CNI != c.want {
			t.Errorf("ds %q: CNI = %q, want %q", c.dsName, f.CNI, c.want)
		}
	}
}

func TestLine_OmitsEmptyAndIsEmptyForZero(t *testing.T) {
	if got := (Facts{}).Line(); got != "" {
		t.Errorf("empty Facts Line() = %q, want empty", got)
	}
	f := Facts{CNI: "Cilium", Cloud: "AWS"} // only two facts
	if got := f.Line(); got != "Cilium CNI · AWS" {
		t.Errorf("Line() = %q, want \"Cilium CNI · AWS\"", got)
	}
}
