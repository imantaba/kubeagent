package diagnose

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mountEvent(ns, podName, msg string) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: podName + ".ev"},
		Reason:         "FailedMount",
		Type:           "Warning",
		Message:        msg,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: podName},
		LastTimestamp:  metav1.Now(),
	}
}

// R17, the case this detector exists for: a ConfigMap that does not exist,
// mounted as a volume rather than read through envFrom. The kubelet reports no
// CreateContainerConfigError for a volume source — the container sits in
// ContainerCreating and the failure is only ever an event — so before this
// detector the workload rendered Degraded with no cause at all.
func TestVolumeMount_MissingConfigMapSource(t *testing.T) {
	ev := mountEvent("shop", "web-0",
		`MountVolume.SetUp failed for volume "config" : configmap "app-config" not found`)
	f := VolumeMountDetector{}.Detect(PodFacts{Pod: podCreating("shop", "web-0"), Events: []corev1.Event{ev}})
	if f == nil {
		t.Fatal("expected a VolumeMountError finding, got nil")
	}
	if f.Issue != "VolumeMountError" {
		t.Errorf("Issue = %q, want VolumeMountError", f.Issue)
	}
	if f.Reason != "a ConfigMap or Secret the pod mounts as a volume does not exist — the pod cannot start" {
		t.Errorf("Reason = %q", f.Reason)
	}
	if !strings.Contains(f.Evidence, "app-config") {
		t.Errorf("Evidence = %q, want it to carry the kubelet's message", f.Evidence)
	}
}

func TestVolumeMount_MissingSecretSource(t *testing.T) {
	ev := mountEvent("shop", "web-0",
		`MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`)
	f := VolumeMountDetector{}.Detect(PodFacts{Pod: podCreating("shop", "web-0"), Events: []corev1.Event{ev}})
	if f == nil {
		t.Fatal("expected a VolumeMountError finding, got nil")
	}
	if !strings.Contains(f.Reason, "does not exist") {
		t.Errorf("Reason = %q, want the absent-source arm", f.Reason)
	}
}

// The default arm must claim LESS than VolumeAttachDetector's wording:
// kubeagent knows the mount did not complete, not that a node could not attach
// the volume. A timeout is the common shape and has no named cause at all.
func TestVolumeMount_UnknownCauseClaimsOnlyTheMountFailed(t *testing.T) {
	ev := mountEvent("shop", "web-0",
		`Unable to attach or mount volumes: timed out waiting for the condition`)
	f := VolumeMountDetector{}.Detect(PodFacts{Pod: podCreating("shop", "web-0"), Events: []corev1.Event{ev}})
	if f == nil {
		t.Fatal("expected a VolumeMountError finding, got nil")
	}
	if f.Reason != "a volume the pod needs could not be mounted — the pod cannot start" {
		t.Errorf("Reason = %q, want the default arm", f.Reason)
	}
	if strings.Contains(f.Reason, "node") {
		t.Errorf("Reason = %q must not claim a node problem it cannot know about", f.Reason)
	}
}

// FailedAttachVolume stays VolumeAttachDetector's. Widening one detector to
// cover both was the rejected option: issue is what a consumer filters on, and
// a mistyped ConfigMap name is not an attach error.
func TestVolumeMount_IgnoresAttachEvents(t *testing.T) {
	ev := attachEvent("shop", "db-0", `Multi-Attach error for volume "pvc-1" Volume is already exclusively attached to one node`)
	facts := PodFacts{Pod: podCreating("shop", "db-0"), Events: []corev1.Event{ev}}
	if f := (VolumeMountDetector{}).Detect(facts); f != nil {
		t.Fatalf("an attach event is not this detector's to report, got %+v", f)
	}
	if f := (VolumeAttachDetector{}).Detect(facts); f == nil || f.Issue != "VolumeAttachError" {
		t.Fatalf("VolumeAttachDetector must be unchanged, got %+v", f)
	}
}

// The same gates VolumeAttachDetector applies: a pod that got past volume setup
// must not be flagged by a stale event.
func TestVolumeMount_ReadyPodNotFlagged(t *testing.T) {
	pod := podCreating("shop", "web-0")
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	ev := mountEvent("shop", "web-0", `MountVolume.SetUp failed for volume "config" : configmap "app-config" not found`)
	if f := (VolumeMountDetector{}).Detect(PodFacts{Pod: pod, Events: []corev1.Event{ev}}); f != nil {
		t.Fatalf("a Ready pod must not be flagged, got %+v", f)
	}
}

func TestVolumeMount_RunningContainerNotFlagged(t *testing.T) {
	pod := podCreating("shop", "web-0")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "c",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	ev := mountEvent("shop", "web-0", `MountVolume.SetUp failed for volume "config" : configmap "app-config" not found`)
	if f := (VolumeMountDetector{}).Detect(PodFacts{Pod: pod, Events: []corev1.Event{ev}}); f != nil {
		t.Fatalf("a pod past volume setup must not be flagged, got %+v", f)
	}
}

func TestVolumeMount_NoEvent(t *testing.T) {
	if f := (VolumeMountDetector{}).Detect(PodFacts{Pod: podCreating("shop", "web-0")}); f != nil {
		t.Fatalf("no FailedMount event means no finding, got %+v", f)
	}
}

// The arm is chosen from the RAW message, so a control character spliced
// mid-word cannot route a finding into the wrong arm; sanitizing happens where
// the message enters Evidence.
func TestVolumeMount_MatchesRawSanitizesEvidence(t *testing.T) {
	ev := mountEvent("shop", "web-0",
		"MountVolume.SetUp failed for volume \"config\" : configmap \"app\x1b[2Jconfig\" not found")
	f := VolumeMountDetector{}.Detect(PodFacts{Pod: podCreating("shop", "web-0"), Events: []corev1.Event{ev}})
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if !strings.Contains(f.Reason, "does not exist") {
		t.Errorf("Reason = %q — the arm must be chosen from the raw message", f.Reason)
	}
	if strings.Contains(f.Evidence, "\x1b") {
		t.Errorf("Evidence = %q still carries an escape sequence", f.Evidence)
	}
}
