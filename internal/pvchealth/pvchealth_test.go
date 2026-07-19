package pvchealth

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pendingPVC(ns, name, sc string) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
}

func pvcEvent(ns, name, reason, message string) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: name + ".ev"},
		Reason:         reason,
		Type:           "Warning",
		Message:        message,
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: ns, Name: name},
	}
}

func TestAssess_ProvisioningFailed(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{pendingPVC("shop", "data-pvc", "fast")}
	events := []corev1.Event{pvcEvent("shop", "data-pvc", "ProvisioningFailed", `storageclass "fast" not found`)}
	got := Assess(pvcs, events)
	if len(got) != 1 {
		t.Fatalf("want 1 issue, got %d", len(got))
	}
	if got[0].Reason != "ProvisioningFailed" || got[0].Detail != `storageclass "fast" not found` {
		t.Errorf("Reason/Detail = %q/%q", got[0].Reason, got[0].Detail)
	}
	if got[0].Phase != "Pending" || got[0].StorageClass != "fast" {
		t.Errorf("Phase/StorageClass = %q/%q", got[0].Phase, got[0].StorageClass)
	}
}

func TestAssess_FailedBinding(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{pendingPVC("shop", "legacy", "")}
	events := []corev1.Event{pvcEvent("shop", "legacy", "FailedBinding", "no persistent volumes available for this claim and no storage class is set")}
	got := Assess(pvcs, events)
	if len(got) != 1 || got[0].Reason != "FailedBinding" {
		t.Fatalf("want 1 FailedBinding issue, got %+v", got)
	}
}

func TestAssess_BoundPVCSkipped(t *testing.T) {
	pvc := pendingPVC("shop", "data-pvc", "fast")
	pvc.Status.Phase = corev1.ClaimBound
	events := []corev1.Event{pvcEvent("shop", "data-pvc", "ProvisioningFailed", "stale")}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, events); len(got) != 0 {
		t.Errorf("a Bound PVC must not be flagged, got %+v", got)
	}
}

func TestAssess_WaitForFirstConsumerSkipped(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{pendingPVC("shop", "data-pvc", "local-path")}
	ev := pvcEvent("shop", "data-pvc", "WaitForFirstConsumer", "waiting for first consumer to be created before binding")
	ev.Type = "Normal"
	if got := Assess(pvcs, []corev1.Event{ev}); len(got) != 0 {
		t.Errorf("a WaitForFirstConsumer Pending PVC must not be flagged, got %+v", got)
	}
}

func TestAssess_CorrelationAndOrder(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{
		pendingPVC("shop", "b-pvc", "fast"),
		pendingPVC("shop", "a-pvc", "fast"),
	}
	events := []corev1.Event{
		pvcEvent("shop", "a-pvc", "ProvisioningFailed", "a failed"),
		pvcEvent("shop", "b-pvc", "ProvisioningFailed", "b failed"),
		pvcEvent("other", "a-pvc", "ProvisioningFailed", "wrong namespace"),
	}
	got := Assess(pvcs, events)
	if len(got) != 2 {
		t.Fatalf("want 2 issues, got %d: %+v", len(got), got)
	}
	if got[0].Name != "a-pvc" || got[1].Name != "b-pvc" {
		t.Errorf("issues must be sorted by name, got %q then %q", got[0].Name, got[1].Name)
	}
	if got[0].Detail != "a failed" {
		t.Errorf("a-pvc correlated to the wrong event: %q", got[0].Detail)
	}
}
