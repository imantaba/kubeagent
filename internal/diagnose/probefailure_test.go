package diagnose

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pfPod builds a Running-but-not-Ready pod with one Running container.
func pfPod(ns, name, container string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  container,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

// pfEvent builds an Unhealthy probe event targeting a pod's container.
func pfEvent(ns, pod, container, message string) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: pod + ".ev"},
		Reason:         "Unhealthy",
		Type:           "Warning",
		Message:        message,
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: pod, FieldPath: "spec.containers{" + container + "}"},
	}
}

func TestProbeFailureDetector_ReadinessHTTP(t *testing.T) {
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{
		pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
	}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a ProbeFailure finding, got nil")
	}
	if f.Issue != "ProbeFailure" || f.Container != "web" {
		t.Errorf("Issue/Container = %q/%q, want ProbeFailure/web", f.Issue, f.Container)
	}
	if want := `container "web": readiness probe failed — HTTP 503`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
	if !strings.Contains(f.Reason, "readiness probe keeps failing") {
		t.Errorf("Reason = %q, want it to name the readiness probe", f.Reason)
	}
}

func TestProbeFailureDetector_NoPodIPLeak(t *testing.T) {
	msg := `Liveness probe failed: Get "http://10.244.1.5:8080/healthz": dial tcp 10.244.1.5:8080: connect: connection refused`
	facts := PodFacts{Pod: pfPod("shop", "api-1", "api"), Events: []corev1.Event{pfEvent("shop", "api-1", "api", msg)}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding")
	}
	if strings.Contains(f.Evidence, "10.244.1.5") || strings.Contains(f.Reason, "10.244.1.5") {
		t.Errorf("pod IP leaked: Evidence=%q Reason=%q", f.Evidence, f.Reason)
	}
	if want := `container "api": liveness probe failed — connection refused`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

func TestProbeFailureDetector_SkipsWaitingContainer(t *testing.T) {
	pod := pfPod("shop", "web-1", "web")
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	facts := PodFacts{Pod: pod, Events: []corev1.Event{
		pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
	}}
	if f := (ProbeFailureDetector{}).Detect(facts); f != nil {
		t.Errorf("a Waiting (CrashLoopBackOff) container must not be flagged, got %+v", f)
	}
}

func TestProbeFailureDetector_SkipsReadyPod(t *testing.T) {
	pod := pfPod("shop", "web-1", "web")
	pod.Status.Conditions[0].Status = corev1.ConditionTrue
	facts := PodFacts{Pod: pod, Events: []corev1.Event{
		pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
	}}
	if f := (ProbeFailureDetector{}).Detect(facts); f != nil {
		t.Errorf("a Ready pod must not be flagged, got %+v", f)
	}
}

func TestProbeFailureDetector_FallbackNoFieldPath(t *testing.T) {
	ev := pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503")
	ev.InvolvedObject.FieldPath = ""
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{ev}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("with empty FieldPath but pod Running+notReady, expected a finding")
	}
	if f.Container != "" {
		t.Errorf("Container = %q, want empty", f.Container)
	}
	if want := "readiness probe failed — HTTP 503"; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q (no container prefix)", f.Evidence, want)
	}
}

func TestContainerFromFieldPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"spec.containers{web}", "web"},
		{"spec.initContainers{init}", "init"},
		{"spec.containers{}", ""},
		{"", ""},
		{"spec.containers", ""},
	}
	for _, c := range cases {
		if got := containerFromFieldPath(c.in); got != c.want {
			t.Errorf("containerFromFieldPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClassifyProbe(t *testing.T) {
	cases := []struct{ msg, wantType, wantReason string }{
		{"Readiness probe failed: HTTP probe failed with statuscode: 503", "readiness", "HTTP 503"},
		{"Liveness probe failed: dial tcp 10.0.0.1:8080: connect: connection refused", "liveness", "connection refused"},
		{`Startup probe failed: Get "http://10.0.0.1/": context deadline exceeded`, "startup", "timed out"},
		{"Readiness probe failed: dial tcp: lookup db on 10.96.0.10:53: no such host", "readiness", "DNS lookup failed"},
		{`Liveness probe failed: service unhealthy (responded with "NOT_SERVING")`, "liveness", "gRPC NOT_SERVING"},
		{"Liveness probe failed: cat: /tmp/healthy: No such file or directory", "liveness", ""},
		{"BackOff restarting failed container", "", ""},
	}
	for _, c := range cases {
		gotType, gotReason := classifyProbe(c.msg)
		if gotType != c.wantType || gotReason != c.wantReason {
			t.Errorf("classifyProbe(%q) = (%q,%q), want (%q,%q)", c.msg, gotType, gotReason, c.wantType, c.wantReason)
		}
	}
}
