package diagnose

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// CrashLoopDetector flags containers stuck in CrashLoopBackOff.
//
// Now is read only to age the container's last termination, which is why this
// detector takes a clock at all. A zero Now still produces a finding — the
// enrichment below is skipped when there is no prior termination to age, and a
// caller that has no clock to inject has nothing else to lose.
type CrashLoopDetector struct{ Now time.Time }

func (d CrashLoopDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
			return &Finding{
				Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:     "CrashLoopBackOff",
				Reason:    "Container repeatedly crashes after starting",
				Evidence:  d.evidence(cs),
				Container: cs.Name,
			}
		}
	}
	return nil
}

// evidence names the container and its restart count, and adds the last exit
// when the container also satisfies RestartLoopDetector's durable conditions.
//
// Those two fields — RestartCount and LastTerminationState — are readable in
// the back-off window and in the run window alike. Only RestartLoopDetector
// read them, and it fires only while the container is Running, so an operator
// scanning a flapping pod got the exit code roughly half the time depending on
// which instant the scan sampled. The kind does not change: waiting.Reason is
// the kubelet's own verdict, and this is evidence for it, not a second opinion.
func (d CrashLoopDetector) evidence(cs corev1.ContainerStatus) string {
	plain := fmt.Sprintf("container %q, restartCount=%d", cs.Name, cs.RestartCount)
	if int(cs.RestartCount) < RestartThreshold {
		return plain
	}
	term := cs.LastTerminationState.Terminated
	if term == nil || term.ExitCode == 0 || term.Reason == "OOMKilled" {
		// No prior error termination, a graceful exit, or an OOM kill —
		// OOMKilledDetector names that one, and it is not this loop.
		return plain
	}
	age := d.Now.Sub(term.FinishedAt.Time).Truncate(time.Second)
	return fmt.Sprintf("%s, last exit %d (%s), %s ago", plain, term.ExitCode, safetext.Line(term.Reason), age)
}
