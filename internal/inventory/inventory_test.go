package inventory

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

func TestTermTime(t *testing.T) {
	if got := termTime(metav1.Time{}); got != "" {
		t.Errorf("zero time: got %q, want empty", got)
	}
	ts := metav1.Date(2026, 6, 22, 8, 14, 3, 0, time.UTC)
	if got := termTime(ts); got != "2026-06-22T08:14:03Z" {
		t.Errorf("got %q, want RFC3339 UTC", got)
	}
}

func TestHumanAge(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"days", now.Add(-36 * 24 * time.Hour), "36d"},
		{"hours", now.Add(-5 * time.Hour), "5h"},
		{"minutes", now.Add(-3 * time.Minute), "3m"},
		{"seconds", now.Add(-10 * time.Second), "10s"},
		{"future clamps to 0s", now.Add(time.Hour), "0s"},
	}
	for _, c := range cases {
		if got := humanAge(c.t, now); got != c.want {
			t.Errorf("%s: humanAge = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestControllerOwner(t *testing.T) {
	yes := true
	no := false
	refs := []metav1.OwnerReference{
		{Kind: "Node", Name: "n1", Controller: &no},
		{Kind: "ReplicaSet", Name: "rs1", Controller: &yes},
	}
	if o := controllerOwner(refs); o == nil || o.Kind != "ReplicaSet" {
		t.Errorf("expected the controller ref (ReplicaSet), got %+v", o)
	}
	if o := controllerOwner(nil); o != nil {
		t.Errorf("expected nil for no refs, got %+v", o)
	}
	noController := []metav1.OwnerReference{
		{Kind: "Node", Name: "n1", Controller: &no},
	}
	if o := controllerOwner(noController); o == nil || o.Kind != "Node" {
		t.Errorf("expected first ref when no controller is set, got %+v", o)
	}
}

func TestPodRestarts(t *testing.T) {
	t1 := metav1.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t2 := metav1.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC) // later
	p := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{RestartCount: 31, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: t1}}},
		{RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: t2}}},
	}}}
	n, last := podRestarts(p)
	if n != 32 {
		t.Errorf("total restarts = %d, want 32", n)
	}
	if termTime(last) != "2026-06-10T00:00:00Z" {
		t.Errorf("last restart = %q, want the later time", termTime(last))
	}
}

func TestPodReadyAndIsReady(t *testing.T) {
	p := corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Ready: true}, {Ready: false},
		}},
	}
	if got := podReady(p); got != "1/2" {
		t.Errorf("podReady = %q, want 1/2", got)
	}
	if podIsReady(p) {
		t.Error("podIsReady should be false when a container is not ready")
	}
	p.Status.ContainerStatuses[1].Ready = true
	if !podIsReady(p) {
		t.Error("podIsReady should be true when all containers are ready")
	}
}

func TestWorkloadStatusAndFlagged(t *testing.T) {
	if workloadStatus(3, 3) != "Running" {
		t.Error("3/3 should be Running")
	}
	if workloadStatus(1, 2) != "Degraded" {
		t.Error("1/2 should be Degraded")
	}
	healthy := Workload{Ready: 3, Desired: 3}
	if healthy.Flagged() {
		t.Error("healthy workload should not be flagged")
	}
	degraded := Workload{Ready: 1, Desired: 2}
	if !degraded.Flagged() {
		t.Error("degraded workload should be flagged")
	}
	withFinding := Workload{Ready: 1, Desired: 1, Findings: []diagnose.Finding{{Pod: "ns/p", Issue: "X"}}}
	if !withFinding.Flagged() {
		t.Error("a workload with a finding should be flagged even when ready==desired")
	}
}
