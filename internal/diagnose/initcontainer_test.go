package diagnose

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podWithInit builds a pod with the given init-container statuses.
func podWithInit(ns, name string, initStatuses ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     corev1.PodStatus{InitContainerStatuses: initStatuses},
	}
}

func TestInitContainerDetector_CrashLoop(t *testing.T) {
	pod := podWithInit("shop", "orders",
		corev1.ContainerStatus{Name: "wait-for-db", RestartCount: 6,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
		corev1.ContainerStatus{Name: "migrate",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
	)
	f := InitContainerDetector{}.Detect(PodFacts{Pod: pod})
	if f == nil {
		t.Fatal("expected an Init:CrashLoopBackOff finding, got nil")
	}
	if f.Issue != "Init:CrashLoopBackOff" || f.Container != "wait-for-db" {
		t.Errorf("Issue/Container = %q/%q, want Init:CrashLoopBackOff/wait-for-db", f.Issue, f.Container)
	}
	if want := `init container "wait-for-db" (1/2), restartCount=6`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

func TestInitContainerDetector_ImagePull(t *testing.T) {
	pod := podWithInit("shop", "orders",
		corev1.ContainerStatus{Name: "ok-step", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		corev1.ContainerStatus{Name: "fetch-config",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: `Back-off pulling image "reg/config:bad": not found`}}},
		corev1.ContainerStatus{Name: "third",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
	)
	f := InitContainerDetector{}.Detect(PodFacts{Pod: pod})
	if f == nil {
		t.Fatal("expected an Init:ImagePullBackOff finding")
	}
	if f.Issue != "Init:ImagePullBackOff" {
		t.Errorf("Issue = %q, want Init:ImagePullBackOff", f.Issue)
	}
	if want := `init container "fetch-config" (2/3): Back-off pulling image "reg/config:bad": not found`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

func TestInitContainerDetector_OOMKilledPrecedenceAndResources(t *testing.T) {
	pod := podWithInit("shop", "orders",
		corev1.ContainerStatus{Name: "loader", RestartCount: 3,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}},
	)
	pod.Spec.InitContainers = []corev1.Container{{Name: "loader", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
	}}}
	f := InitContainerDetector{}.Detect(PodFacts{Pod: pod})
	if f == nil {
		t.Fatal("expected an Init:OOMKilled finding")
	}
	if f.Issue != "Init:OOMKilled" {
		t.Errorf("Issue = %q, want Init:OOMKilled (OOM takes precedence over CrashLoopBackOff)", f.Issue)
	}
	if want := `init container "loader" (1/1), exitCode=137`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
	if f.Resources == nil || f.Resources.MemLimit != "32Mi" {
		t.Errorf("Resources must carry the init container's limits (32Mi), got %+v", f.Resources)
	}
}

func TestInitContainerDetector_SkipsSucceededInits(t *testing.T) {
	// All inits succeeded; the MAIN container is crash-looping. The init detector
	// must stay silent (CrashLoopDetector owns the main container).
	pod := podWithInit("shop", "orders",
		corev1.ContainerStatus{Name: "setup", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
	)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	if f := (InitContainerDetector{}).Detect(PodFacts{Pod: pod}); f != nil {
		t.Errorf("succeeded inits + crashing main must not yield an init finding, got %+v", f)
	}
}

func TestInitContainerDetector_HealthyRunningSidecarNotFlagged(t *testing.T) {
	pod := podWithInit("shop", "orders",
		corev1.ContainerStatus{Name: "logging-sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	)
	if f := (InitContainerDetector{}).Detect(PodFacts{Pod: pod}); f != nil {
		t.Errorf("a healthy Running (native sidecar) init container must not be flagged, got %+v", f)
	}
}

func TestInitContainerDetector_NoInitContainers(t *testing.T) {
	if f := (InitContainerDetector{}).Detect(PodFacts{Pod: podWithInit("shop", "orders")}); f != nil {
		t.Errorf("a pod with no init containers must not be flagged, got %+v", f)
	}
}
