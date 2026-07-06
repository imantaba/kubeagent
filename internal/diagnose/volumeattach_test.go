package diagnose

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podCreating returns a not-Ready pod stuck at container creation.
func podCreating(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}},
		},
	}
}

func attachEvent(ns, podName, msg string) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: podName + ".ev"},
		Reason:         "FailedAttachVolume",
		Type:           "Warning",
		Message:        msg,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: podName},
		LastTimestamp:  metav1.Now(),
	}
}

func TestVolumeAttach_MultiAttachOnStuckPod(t *testing.T) {
	ev := attachEvent("shop", "db-0", `Multi-Attach error for volume "pvc-1" Volume is already exclusively attached to one node`)
	f := VolumeAttachDetector{}.Detect(PodFacts{Pod: podCreating("shop", "db-0"), Events: []corev1.Event{ev}})
	if f == nil || f.Issue != "VolumeAttachError" {
		t.Fatalf("want VolumeAttachError, got %+v", f)
	}
	if !strings.Contains(f.Reason, "Multi-Attach") {
		t.Errorf("reason should name Multi-Attach: %q", f.Reason)
	}
	if !strings.Contains(f.Evidence, "pvc-1") {
		t.Errorf("evidence should carry the event message: %q", f.Evidence)
	}
}

func TestVolumeAttach_GenericAttachFailure(t *testing.T) {
	ev := attachEvent("shop", "db-0", `AttachVolume.Attach failed for volume "pvc-2": timed out waiting for external-attacher`)
	f := VolumeAttachDetector{}.Detect(PodFacts{Pod: podCreating("shop", "db-0"), Events: []corev1.Event{ev}})
	if f == nil {
		t.Fatal("want a finding for a generic attach failure")
	}
	if strings.Contains(f.Reason, "Multi-Attach") {
		t.Errorf("non-Multi-Attach message should use the generic reason: %q", f.Reason)
	}
}

func TestVolumeAttach_ReadyPodIgnored(t *testing.T) {
	pod := podCreating("shop", "db-0")
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	ev := attachEvent("shop", "db-0", "Multi-Attach error ...")
	if f := (VolumeAttachDetector{}).Detect(PodFacts{Pod: pod, Events: []corev1.Event{ev}}); f != nil {
		t.Errorf("a Ready/Running pod must not be flagged, got %+v", f)
	}
}

func TestVolumeAttach_CrashLoopingPodWithStaleEventIgnored(t *testing.T) {
	// Volume attached; the pod is now CrashLoopBackOff (past creation) but a stale
	// FailedAttachVolume event is still within its TTL — must NOT flag VolumeAttachError.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-0"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}},
		},
	}
	ev := attachEvent("shop", "db-0", "Multi-Attach error ...")
	if f := (VolumeAttachDetector{}).Detect(PodFacts{Pod: pod, Events: []corev1.Event{ev}}); f != nil {
		t.Errorf("crashlooping pod (past volume setup) must not be flagged, got %+v", f)
	}
}

func TestVolumeAttach_NoEvent(t *testing.T) {
	if f := (VolumeAttachDetector{}).Detect(PodFacts{Pod: podCreating("shop", "db-0")}); f != nil {
		t.Errorf("no event -> no finding, got %+v", f)
	}
}
