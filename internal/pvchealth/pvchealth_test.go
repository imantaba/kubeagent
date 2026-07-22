package pvchealth

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
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
	events := []corev1.Event{pvcEvent("shop", "data-pvc", "ProvisioningFailed", `quota exceeded`)}
	// SC "fast" exists, so structural cause is bypassed — the event reason surfaces instead.
	scs := []storagev1.StorageClass{scClass("fast")}
	got := Assess(pvcs, events, scs, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 issue, got %d", len(got))
	}
	if got[0].Reason != "ProvisioningFailed" || got[0].Detail != `quota exceeded` {
		t.Errorf("Reason/Detail = %q/%q", got[0].Reason, got[0].Detail)
	}
	if got[0].Phase != "Pending" || got[0].StorageClass != "fast" {
		t.Errorf("Phase/StorageClass = %q/%q", got[0].Phase, got[0].StorageClass)
	}
}

func TestAssess_FailedBinding(t *testing.T) {
	// nil StorageClassName falls through to the event path (ambiguous default-SC case).
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "legacy"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: nil},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	events := []corev1.Event{pvcEvent("shop", "legacy", "FailedBinding", "no persistent volumes available for this claim and no storage class is set")}
	got := Assess([]corev1.PersistentVolumeClaim{pvc}, events, nil, nil)
	if len(got) != 1 || got[0].Reason != "FailedBinding" {
		t.Fatalf("want 1 FailedBinding issue, got %+v", got)
	}
}

func TestAssess_BoundPVCSkipped(t *testing.T) {
	pvc := pendingPVC("shop", "data-pvc", "fast")
	pvc.Status.Phase = corev1.ClaimBound
	events := []corev1.Event{pvcEvent("shop", "data-pvc", "ProvisioningFailed", "stale")}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, events, nil, nil); len(got) != 0 {
		t.Errorf("a Bound PVC must not be flagged, got %+v", got)
	}
}

func TestAssess_WaitForFirstConsumerSkipped(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{pendingPVC("shop", "data-pvc", "local-path")}
	// SC "local-path" exists, so structural cause is bypassed. A real WaitForFirstConsumer
	// event is Normal-typed, but Assess filters on Reason (not Type): the Reason is neither
	// ProvisioningFailed nor FailedBinding, so the PVC is skipped. The Type is set here
	// only to mirror a real event.
	ev := pvcEvent("shop", "data-pvc", "WaitForFirstConsumer", "waiting for first consumer to be created before binding")
	ev.Type = "Normal"
	scs := []storagev1.StorageClass{scClass("local-path")}
	if got := Assess(pvcs, []corev1.Event{ev}, scs, nil); len(got) != 0 {
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
	// SC "fast" exists so structural cause is bypassed — events drive the issues.
	scs := []storagev1.StorageClass{scClass("fast")}
	got := Assess(pvcs, events, scs, nil)
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

func TestAssess_NewestEventWins(t *testing.T) {
	// Two ProvisioningFailed events for the same PVC: newestFailureEvent must pick the
	// one with the later LastTimestamp regardless of slice order.
	older := pvcEvent("shop", "data-pvc", "ProvisioningFailed", "older cause")
	older.LastTimestamp = metav1.NewTime(time.Unix(1000, 0))
	newer := pvcEvent("shop", "data-pvc", "ProvisioningFailed", "newer cause")
	newer.LastTimestamp = metav1.NewTime(time.Unix(2000, 0))
	pvcs := []corev1.PersistentVolumeClaim{pendingPVC("shop", "data-pvc", "fast")}
	// SC "fast" exists so structural cause is bypassed — events drive the issue.
	scs := []storagev1.StorageClass{scClass("fast")}
	// older first, so a naive first-match implementation would pick the wrong event.
	got := Assess(pvcs, []corev1.Event{older, newer}, scs, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 issue, got %+v", got)
	}
	if got[0].Detail != "newer cause" {
		t.Errorf("newest event must win, got Detail=%q", got[0].Detail)
	}
}

func scClass(name string) storagev1.StorageClass {
	return storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// staticPVC is a Pending PVC with storageClassName "" (explicit static) + a size + modes.
func staticPVC(ns, name, size string, modes ...corev1.PersistentVolumeAccessMode) corev1.PersistentVolumeClaim {
	empty := ""
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &empty,
			AccessModes:      modes,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
}

// availPV is an Available, unbound PV with a storage class, size, and modes.
func availPV(name, size, sc string, modes ...corev1.PersistentVolumeAccessMode) corev1.PersistentVolume {
	return corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: sc,
			AccessModes:      modes,
			Capacity:         corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
	}
}

func onlyIssue(t *testing.T, issues []Issue) Issue {
	t.Helper()
	if len(issues) != 1 {
		t.Fatalf("want 1 issue, got %d: %+v", len(issues), issues)
	}
	return issues[0]
}

func TestAssess_MissingStorageClass_NoEvent(t *testing.T) {
	pvc := pendingPVC("shop", "data", "fast-ssd")
	got := onlyIssue(t, Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, nil))
	if got.Reason != "MissingStorageClass" {
		t.Fatalf("reason = %q", got.Reason)
	}
	if got.Detail != `references StorageClass "fast-ssd" which does not exist` {
		t.Fatalf("detail = %q", got.Detail)
	}
	if got.StorageClass != "fast-ssd" {
		t.Errorf("storageClass = %q", got.StorageClass)
	}
}

func TestAssess_MissingStorageClass_PresentSCNotFlagged(t *testing.T) {
	pvc := pendingPVC("shop", "data", "fast-ssd")
	scs := []storagev1.StorageClass{scClass("fast-ssd")}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, scs, nil); len(got) != 0 {
		t.Fatalf("a present dynamic SC with no event must not be flagged, got %+v", got)
	}
}

func TestAssess_StructuralBeatsEvent(t *testing.T) {
	pvc := pendingPVC("shop", "data", "fast-ssd")
	events := []corev1.Event{pvcEvent("shop", "data", "ProvisioningFailed", "some raw provisioner error")}
	got := onlyIssue(t, Assess([]corev1.PersistentVolumeClaim{pvc}, events, nil, nil))
	if got.Reason != "MissingStorageClass" || got.Detail != `references StorageClass "fast-ssd" which does not exist` {
		t.Fatalf("structural cause must win over the event, got %+v", got)
	}
}

func TestAssess_EventFallback_ValidDynamicSC(t *testing.T) {
	pvc := pendingPVC("shop", "data", "standard")
	scs := []storagev1.StorageClass{scClass("standard")}
	events := []corev1.Event{pvcEvent("shop", "data", "ProvisioningFailed", "quota exceeded")}
	got := onlyIssue(t, Assess([]corev1.PersistentVolumeClaim{pvc}, events, scs, nil))
	if got.Reason != "ProvisioningFailed" || got.Detail != "quota exceeded" {
		t.Fatalf("valid dynamic SC must fall through to the event, got %+v", got)
	}
}

func TestAssess_ValidDynamicSC_NoEvent_NotFlagged(t *testing.T) {
	pvc := pendingPVC("shop", "data", "standard")
	scs := []storagev1.StorageClass{scClass("standard")}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, scs, nil); len(got) != 0 {
		t.Fatalf("a normally-provisioning PVC must not be flagged, got %+v", got)
	}
}

func TestAssess_NoMatchingPV_Empty(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteOnce)
	got := onlyIssue(t, Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, nil))
	if got.Reason != "NoMatchingPV" {
		t.Fatalf("reason = %q", got.Reason)
	}
	if got.Detail != "no available PersistentVolume matches its request (10Gi, ReadWriteOnce)" {
		t.Fatalf("detail = %q", got.Detail)
	}
}

func TestAssess_NoMatchingPV_MatchingPVNotFlagged(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteOnce)
	pvs := []corev1.PersistentVolume{availPV("pv-1", "20Gi", "", corev1.ReadWriteOnce)}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, pvs); len(got) != 0 {
		t.Fatalf("a matching Available static PV must satisfy the claim, got %+v", got)
	}
}

func TestAssess_NoMatchingPV_TooSmall(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteOnce)
	pvs := []corev1.PersistentVolume{availPV("pv-1", "5Gi", "", corev1.ReadWriteOnce)}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, pvs); len(got) != 1 || got[0].Reason != "NoMatchingPV" {
		t.Fatalf("a too-small PV must not satisfy the claim, got %+v", got)
	}
}

func TestAssess_NoMatchingPV_WrongMode(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteMany)
	pvs := []corev1.PersistentVolume{availPV("pv-1", "20Gi", "", corev1.ReadWriteOnce)}
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, pvs); len(got) != 1 || got[0].Reason != "NoMatchingPV" {
		t.Fatalf("a PV lacking the requested access mode must not satisfy, got %+v", got)
	}
}

func TestAssess_NoMatchingPV_BoundPVNotCandidate(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteOnce)
	pv := availPV("pv-1", "20Gi", "", corev1.ReadWriteOnce)
	pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: "other", Name: "someone-else"}
	pv.Status.Phase = corev1.VolumeBound
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, []corev1.PersistentVolume{pv}); len(got) != 1 || got[0].Reason != "NoMatchingPV" {
		t.Fatalf("a bound PV must not be a candidate, got %+v", got)
	}
}

func TestAssess_NoMatchingPV_DynamicPVNotCandidate(t *testing.T) {
	pvc := staticPVC("shop", "data", "10Gi", corev1.ReadWriteOnce)
	pvs := []corev1.PersistentVolume{availPV("pv-1", "20Gi", "standard", corev1.ReadWriteOnce)} // dynamic class
	if got := Assess([]corev1.PersistentVolumeClaim{pvc}, nil, nil, pvs); len(got) != 1 || got[0].Reason != "NoMatchingPV" {
		t.Fatalf("a dynamic-class PV must not satisfy a static claim, got %+v", got)
	}
}
