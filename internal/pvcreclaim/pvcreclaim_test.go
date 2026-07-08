package pvcreclaim

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pv(name string, policy corev1.PersistentVolumeReclaimPolicy) corev1.PersistentVolume {
	return corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: policy},
	}
}

// pvc builds a PVC. class "" means no storageClassName. cap "" means no status capacity.
func pvc(ns, name, volumeName, class, capacity string, phase corev1.PersistentVolumeClaimPhase) corev1.PersistentVolumeClaim {
	spec := corev1.PersistentVolumeClaimSpec{VolumeName: volumeName}
	if class != "" {
		c := class
		spec.StorageClassName = &c
	}
	st := corev1.PersistentVolumeClaimStatus{Phase: phase}
	if capacity != "" {
		st.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)}
	}
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spec,
		Status:     st,
	}
}

func find(r Report, ns, name string) (PVCReclaim, bool) {
	for _, p := range r.PVCs {
		if p.Namespace == ns && p.Name == name {
			return p, true
		}
	}
	return PVCReclaim{}, false
}

func TestAssess_ListsBoundDeletePVC(t *testing.T) {
	pvs := []corev1.PersistentVolume{pv("pv-a", corev1.PersistentVolumeReclaimDelete)}
	pvcs := []corev1.PersistentVolumeClaim{pvc("shop", "data-0", "pv-a", "standard", "8Gi", corev1.ClaimBound)}
	r := Assess(pvcs, pvs)
	p, ok := find(r, "shop", "data-0")
	if !ok {
		t.Fatalf("expected shop/data-0 listed, got %+v", r)
	}
	if p.PV != "pv-a" || p.StorageClass != "standard" || p.Capacity != "8Gi" {
		t.Errorf("wrong row: %+v", p)
	}
	if r.Count != 1 {
		t.Errorf("want Count 1, got %d", r.Count)
	}
}

func TestAssess_SkipsRetainPVC(t *testing.T) {
	pvs := []corev1.PersistentVolume{pv("pv-r", corev1.PersistentVolumeReclaimRetain)}
	pvcs := []corev1.PersistentVolumeClaim{pvc("shop", "keep", "pv-r", "standard", "8Gi", corev1.ClaimBound)}
	r := Assess(pvcs, pvs)
	if _, ok := find(r, "shop", "keep"); ok {
		t.Errorf("Retain PVC must not be listed")
	}
	if r.Count != 0 {
		t.Errorf("want Count 0, got %d", r.Count)
	}
}

func TestAssess_SkipsUnboundPVC(t *testing.T) {
	pvs := []corev1.PersistentVolume{pv("pv-a", corev1.PersistentVolumeReclaimDelete)}
	// Pending PVC with no volumeName.
	pvcs := []corev1.PersistentVolumeClaim{pvc("shop", "pending", "", "standard", "", corev1.ClaimPending)}
	r := Assess(pvcs, pvs)
	if r.Count != 0 {
		t.Errorf("unbound PVC must not be listed, got %+v", r)
	}
}

func TestAssess_SkipsWhenBoundPVMissing(t *testing.T) {
	// Bound to a PV name that has no matching PV object — defensive.
	pvcs := []corev1.PersistentVolumeClaim{pvc("shop", "orphan", "pv-gone", "standard", "8Gi", corev1.ClaimBound)}
	r := Assess(pvcs, nil)
	if r.Count != 0 {
		t.Errorf("PVC with no resolvable PV must not be listed, got %+v", r)
	}
}

func TestAssess_NoStorageClassStillListed(t *testing.T) {
	pvs := []corev1.PersistentVolume{pv("pv-s", corev1.PersistentVolumeReclaimDelete)}
	pvcs := []corev1.PersistentVolumeClaim{pvc("shop", "static", "pv-s", "", "", corev1.ClaimBound)}
	r := Assess(pvcs, pvs)
	p, ok := find(r, "shop", "static")
	if !ok {
		t.Fatalf("static Delete PVC should be listed")
	}
	if p.StorageClass != "" || p.Capacity != "" {
		t.Errorf("want empty class/capacity, got %+v", p)
	}
}
