package diagnose

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pinned is the instant every test in this file measures against. A detector
// that reads a clock is only deterministic if the clock is an input.
var pinned = time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

// podStartFailure returns a pod whose single main container is Waiting with
// reason+message, created age before the pinned instant, with restartCount
// restarts behind it.
func podStartFailure(name, reason, message string, restarts int32, age time.Duration) *corev1.Pod {
	p := podWaiting("shop", name, "api", reason, message)
	p.CreationTimestamp = metav1.NewTime(pinned.Add(-age))
	p.Status.ContainerStatuses[0].RestartCount = restarts
	return p
}

// The five reasons in the closed set are the whole vocabulary this detector
// answers for. One positive per reason, so widening or narrowing the set
// cannot pass silently.
func TestContainerStartError_FiresOnEveryReasonInTheSet(t *testing.T) {
	for _, reason := range []string{
		"RunContainerError",
		"CreateContainerError",
		"PreStartHookError",
		"PostStartHookError",
		"StartError",
	} {
		t.Run(reason, func(t *testing.T) {
			pod := podStartFailure("api-1", reason, "failed to start container", 2, time.Minute)
			got := ContainerStartErrorDetector{Now: pinned}.Detect(PodFacts{Pod: pod})
			if got == nil {
				t.Fatalf("no finding for %s", reason)
			}
			if got.Issue != "ContainerStartError" {
				t.Errorf("Issue = %q, want ContainerStartError", got.Issue)
			}
			if !strings.Contains(got.Evidence, reason) {
				t.Errorf("Evidence %q does not name the reason %s", got.Evidence, reason)
			}
			if !strings.Contains(got.Evidence, "failed to start container") {
				t.Errorf("Evidence %q does not carry the kubelet message", got.Evidence)
			}
			if got.Container != "api" {
				t.Errorf("Container = %q, want api", got.Container)
			}
			if got.Pod != "shop/api-1" {
				t.Errorf("Pod = %q, want shop/api-1", got.Pod)
			}
		})
	}
}

// The dwell: a container that has already restarted is reported at once, a
// brand-new one gets a minute of grace, and the grace expires.
func TestContainerStartError_Dwell(t *testing.T) {
	cases := []struct {
		name     string
		restarts int32
		age      time.Duration
		want     bool
	}{
		{name: "restarted, still young", restarts: 2, age: 5 * time.Second, want: true},
		{name: "never restarted, old enough", restarts: 0, age: 90 * time.Second, want: true},
		{name: "never restarted, still young", restarts: 0, age: 5 * time.Second, want: false},
		{name: "never restarted, exactly at the dwell", restarts: 0, age: 60 * time.Second, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod := podStartFailure("api-1", "RunContainerError", "failed to start container", c.restarts, c.age)
			got := ContainerStartErrorDetector{Now: pinned}.Detect(PodFacts{Pod: pod})
			if (got != nil) != c.want {
				t.Errorf("finding = %v, want %v", got, c.want)
			}
		})
	}
}

// Reasons outside the set stay outside it — including the four the image
// family owns and the two ordinary transient states every pod passes through.
func TestContainerStartError_IgnoresEveryOtherReason(t *testing.T) {
	for _, reason := range []string{
		"ContainerCreating",
		"PodInitializing",
		"CrashLoopBackOff",
		"ImagePullBackOff",
		"ErrImagePull",
		"CreateContainerConfigError",
		"InvalidImageName",
		"ErrImageNeverPull",
		"RegistryUnavailable",
		"SignatureValidationFailed",
	} {
		t.Run(reason, func(t *testing.T) {
			pod := podStartFailure("api-1", reason, "some message", 2, 10*time.Minute)
			if got := (ContainerStartErrorDetector{Now: pinned}).Detect(PodFacts{Pod: pod}); got != nil {
				t.Errorf("fired on %s: %+v", reason, got)
			}
		})
	}
}

func TestContainerStartError_IgnoresAHealthyPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-1",
			CreationTimestamp: metav1.NewTime(pinned.Add(-time.Hour))},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "api",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(pinned.Add(-time.Hour))}},
			}},
		},
	}
	if got := (ContainerStartErrorDetector{Now: pinned}).Detect(PodFacts{Pod: pod}); got != nil {
		t.Errorf("fired on a healthy Running pod: %+v", got)
	}
}

// Main containers only. An init container failing to start is a different
// diagnosis — nothing in the pod has run yet — and InitContainerDetector
// already reports it as Init:<reason>.
func TestContainerStartError_IgnoresInitContainers(t *testing.T) {
	pod := podStartFailure("api-1", "RunContainerError", "failed to start container", 2, time.Minute)
	pod.Status.InitContainerStatuses = pod.Status.ContainerStatuses
	pod.Status.ContainerStatuses = nil
	if got := (ContainerStartErrorDetector{Now: pinned}).Detect(PodFacts{Pod: pod}); got != nil {
		t.Errorf("fired on an init container: %+v", got)
	}
}

// The pod R6 was found on: OOM-killed while starting, so the kubelet reports
// the start failure in Waiting and the OOM in LastTerminationState. Before
// this detector existed the workload rendered as failing with nothing under
// it — OOMKilledDetector reads State/LastTerminationState.Reason == OOMKilled
// and this pod's says StartError.
func TestContainerStartError_FiresOnTheOOMAtStartPod(t *testing.T) {
	pod := podStartFailure("api-1", "RunContainerError",
		"failed to start container: OOM-killed while starting", 2, 2*time.Minute)
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "StartError", ExitCode: 128},
	}
	got := ContainerStartErrorDetector{Now: pinned}.Detect(PodFacts{Pod: pod})
	if got == nil {
		t.Fatal("no finding on the OOM-at-start pod")
	}
	if got.Issue != "ContainerStartError" {
		t.Errorf("Issue = %q, want ContainerStartError", got.Issue)
	}
}

// Evidence is an ingress point for two API values the API server does not
// validate. Both go through safetext.Line here; the matching above runs on
// the raw reason, which is why a mangled reason simply does not match rather
// than matching a sanitized lookalike.
func TestContainerStartError_SanitizesTheKubeletMessage(t *testing.T) {
	pod := podStartFailure("api-1", "RunContainerError", "failed\x1b[2Jto start", 2, time.Minute)
	got := ContainerStartErrorDetector{Now: pinned}.Detect(PodFacts{Pod: pod})
	if got == nil {
		t.Fatal("no finding")
	}
	if strings.ContainsRune(got.Evidence, 0x1b) {
		t.Errorf("Evidence carries a raw escape byte: %q", got.Evidence)
	}
}
