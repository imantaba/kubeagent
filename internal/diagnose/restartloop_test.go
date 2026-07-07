package diagnose

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var rlNow = time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

// flapPod: currently Running (started ranFor ago), with restartCount and a last
// termination of exit/reason that finished finishedAgo ago.
func flapPod(restarts int32, ranFor time.Duration, exit int32, reason string, finishedAgo time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "flapper"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				RestartCount: restarts,
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(rlNow.Add(-ranFor))}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: exit, Reason: reason, FinishedAt: metav1.NewTime(rlNow.Add(-finishedAgo)),
				}},
			}},
		},
	}
}

func TestRestartLoop_FlappingRunningPod(t *testing.T) {
	f := RestartLoopDetector{Now: rlNow}.Detect(PodFacts{Pod: flapPod(3, 20*time.Second, 1, "Error", 25*time.Second)})
	if f == nil || f.Issue != "RestartLoop" {
		t.Fatalf("want RestartLoop, got %+v", f)
	}
	if !strings.Contains(f.Evidence, "3 restarts") || !strings.Contains(f.Evidence, "exit 1") {
		t.Errorf("evidence missing detail: %q", f.Evidence)
	}
}

func TestRestartLoop_RecoveredOldRunIgnored(t *testing.T) {
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: flapPod(9, 30*time.Minute, 1, "Error", 30*time.Minute)}); f != nil {
		t.Errorf("stable run past the window must not flag, got %+v", f)
	}
}

func TestRestartLoop_BelowThresholdIgnored(t *testing.T) {
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: flapPod(2, 20*time.Second, 1, "Error", 25*time.Second)}); f != nil {
		t.Errorf("restarts<3 must not flag, got %+v", f)
	}
}

func TestRestartLoop_OOMKilledIgnored(t *testing.T) {
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: flapPod(5, 20*time.Second, 137, "OOMKilled", 25*time.Second)}); f != nil {
		t.Errorf("OOMKilled is covered elsewhere, got %+v", f)
	}
}

func TestRestartLoop_GracefulExitIgnored(t *testing.T) {
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: flapPod(5, 20*time.Second, 0, "Completed", 25*time.Second)}); f != nil {
		t.Errorf("exit 0 must not flag, got %+v", f)
	}
}

func TestRestartLoop_NotRunningIgnored(t *testing.T) {
	pod := flapPod(5, 20*time.Second, 1, "Error", 25*time.Second)
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: pod}); f != nil {
		t.Errorf("not-Running must not flag (covered by CrashLoop), got %+v", f)
	}
}

func TestRestartLoop_NeverRestartedIgnored(t *testing.T) {
	pod := flapPod(0, 20*time.Second, 0, "", 0)
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	if f := (RestartLoopDetector{Now: rlNow}).Detect(PodFacts{Pod: pod}); f != nil {
		t.Errorf("never restarted must not flag, got %+v", f)
	}
}
