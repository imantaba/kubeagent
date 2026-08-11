package diagnose

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var clNow = time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)

// backingOffPod: a container the kubelet has put in CrashLoopBackOff, with a
// restart count and a recorded prior termination. The two fields the enrichment
// reads are durable — they are readable in the back-off window and in the run
// window alike, which is the whole point of R9.
func backingOffPod(restarts int32, exit int32, reason string, finishedAgo time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				RestartCount: restarts,
				State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: exit, Reason: reason, FinishedAt: metav1.NewTime(clNow.Add(-finishedAgo)),
				}},
			}},
		},
	}
}

func TestCrashLoopDetector_FiresOnCrashLoopBackOff(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "CrashLoopBackOff", "")}

	f := CrashLoopDetector{}.Detect(facts)

	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Issue != "CrashLoopBackOff" {
		t.Errorf("Issue = %q, want CrashLoopBackOff", f.Issue)
	}
	if f.Pod != "default/web" {
		t.Errorf("Pod = %q, want default/web", f.Pod)
	}
}

func TestCrashLoopDetector_IgnoresOtherWaitingReasons(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := (CrashLoopDetector{}).Detect(facts); f != nil {
		t.Errorf("expected nil for non-crashloop pod, got %+v", f)
	}
}

func TestCrashLoopDetector_SetsContainer(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "CrashLoopBackOff", "")}
	f := CrashLoopDetector{}.Detect(facts)
	if f == nil || f.Container != "app" {
		t.Fatalf("expected Container=\"app\", got %+v", f)
	}
}

// R9: the same pod reads CrashLoopBackOff or RestartLoop depending on which
// second the scan lands, and only the RestartLoop wording carried the exit code
// and the last-exit age. Both are readable in the back-off window too, so the
// CrashLoopBackOff finding now carries them itself.
func TestCrashLoopDetector_EnrichesWithTheLastExit(t *testing.T) {
	f := CrashLoopDetector{Now: clNow}.Detect(PodFacts{Pod: backingOffPod(5, 1, "Error", 27*time.Second)})
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	want := `container "app", restartCount=5, last exit 1 (Error), 27s ago`
	if f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

// The enrichment is evidence, not a downgrade of certainty: waiting.Reason is
// still the kubelet's own verdict, so the issue kind, the reason sentence and
// the absent confidence tag all stay exactly as they were.
func TestCrashLoopDetector_EnrichmentChangesNothingElse(t *testing.T) {
	f := CrashLoopDetector{Now: clNow}.Detect(PodFacts{Pod: backingOffPod(5, 1, "Error", 27*time.Second)})
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Issue != "CrashLoopBackOff" {
		t.Errorf("Issue = %q, want CrashLoopBackOff", f.Issue)
	}
	if f.Reason != "Container repeatedly crashes after starting" {
		t.Errorf("Reason = %q, want the unchanged sentence", f.Reason)
	}
	if f.Confidence != "" {
		t.Errorf("Confidence = %q, want it left unset — setting it would start printing a tag", f.Confidence)
	}
	if f.Container != "app" {
		t.Errorf("Container = %q, want app", f.Container)
	}
}

// The enrichment fires only when the container also satisfies RestartLoop's
// durable conditions. Below the threshold there is no loop to describe yet.
func TestCrashLoopDetector_NoEnrichmentBelowTheRestartThreshold(t *testing.T) {
	f := CrashLoopDetector{Now: clNow}.Detect(PodFacts{Pod: backingOffPod(RestartThreshold-1, 1, "Error", 27*time.Second)})
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	want := `container "app", restartCount=2, last exit 1 (Error), 27s ago`
	if f.Evidence == want {
		t.Fatalf("Evidence enriched below the threshold: %q", f.Evidence)
	}
	if got := `container "app", restartCount=2`; f.Evidence != got {
		t.Errorf("Evidence = %q, want %q", f.Evidence, got)
	}
}

// An OOM kill is OOMKilledDetector's finding, and a graceful exit is not a
// loop. Neither may be described here as a crash the pod keeps repeating.
func TestCrashLoopDetector_NoEnrichmentForOOMOrGracefulExit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exit   int32
		reason string
	}{
		{"OOMKilled", 137, "OOMKilled"},
		{"graceful exit", 0, "Completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := CrashLoopDetector{Now: clNow}.Detect(PodFacts{Pod: backingOffPod(5, tc.exit, tc.reason, 27*time.Second)})
			if f == nil {
				t.Fatal("expected a finding, got nil")
			}
			if f.Evidence != `container "app", restartCount=5` {
				t.Errorf("Evidence = %q, want the plain form", f.Evidence)
			}
		})
	}
}

// No prior termination recorded at all — the kubelet has declared the back-off
// but kubeagent has no exit to name.
func TestCrashLoopDetector_NoEnrichmentWithoutAPriorTermination(t *testing.T) {
	pod := backingOffPod(5, 1, "Error", 27*time.Second)
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	f := CrashLoopDetector{Now: clNow}.Detect(PodFacts{Pod: pod})
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Evidence != `container "app", restartCount=5` {
		t.Errorf("Evidence = %q, want the plain form", f.Evidence)
	}
}

// terminated.Reason is a field the API server does not validate, so it is
// sanitized where it enters the finding — the same rule RestartLoopDetector
// follows for the same field.
func TestCrashLoopDetector_SanitizesTheTerminationReason(t *testing.T) {
	f := CrashLoopDetector{Now: clNow}.Detect(PodFacts{Pod: backingOffPod(5, 1, "Err\x1b[2Jor", 27*time.Second)})
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if strings.ContainsRune(f.Evidence, '\x1b') {
		t.Errorf("Evidence carries an escape byte: %q", f.Evidence)
	}
}
