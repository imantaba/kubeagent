package diagnose

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestConfigErrorDetector_MainContainer(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("shop", "api", "app", "CreateContainerConfigError", `configmap "app-config" not found`)}
	f := ConfigErrorDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Issue != "CreateContainerConfigError" {
		t.Errorf("Issue = %q", f.Issue)
	}
	if f.Reason != "a referenced ConfigMap or Secret is missing, or a required key is absent — the container cannot start" {
		t.Errorf("Reason = %q", f.Reason)
	}
	if !strings.Contains(f.Evidence, `container "app"`) || !strings.Contains(f.Evidence, "app-config") {
		t.Errorf("Evidence = %q", f.Evidence)
	}
	if f.Container != "app" {
		t.Errorf("Container = %q", f.Container)
	}
}

func TestConfigErrorDetector_InitContainer(t *testing.T) {
	init := corev1.ContainerStatus{
		Name:  "wait-db",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: `secret "db-creds" not found`}},
	}
	f := ConfigErrorDetector{}.Detect(PodFacts{Pod: podWithInit("shop", "api", init)})
	if f == nil {
		t.Fatal("expected a finding for an init container, got nil")
	}
	if !strings.HasPrefix(f.Evidence, `init container "wait-db"`) {
		t.Errorf("Evidence = %q, want it to start with init container", f.Evidence)
	}
}

func TestConfigErrorDetector_MainBeatsInit(t *testing.T) {
	pod := podWaiting("shop", "api", "app", "CreateContainerConfigError", `configmap "main" not found`)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  "wait",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: `configmap "init" not found`}},
	}}
	f := ConfigErrorDetector{}.Detect(PodFacts{Pod: pod})
	if f == nil || f.Container != "app" {
		t.Fatalf("main container must take precedence, got %+v", f)
	}
}

func TestConfigErrorDetector_OtherReason(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("shop", "api", "app", "CrashLoopBackOff", "")}
	if f := (ConfigErrorDetector{}).Detect(facts); f != nil {
		t.Fatalf("a different waiting reason must not fire, got %+v", f)
	}
}

func TestConfigErrorDetector_RunningPod(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Namespace, pod.Name = "shop", "api"
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	if f := (ConfigErrorDetector{}).Detect(PodFacts{Pod: pod}); f != nil {
		t.Fatalf("a running container must not fire, got %+v", f)
	}
}
