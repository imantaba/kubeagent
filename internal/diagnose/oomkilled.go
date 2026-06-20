package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// OOMKilledDetector flags containers killed for exceeding their memory limit.
type OOMKilledDetector struct{}

func (d OOMKilledDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		// Check both the current and the previous termination state:
		// a still-dead container reports in State, a restarted one in LastTerminationState.
		for _, term := range []*corev1.ContainerStateTerminated{
			cs.State.Terminated, cs.LastTerminationState.Terminated,
		} {
			if term != nil && term.Reason == "OOMKilled" {
				return &Finding{
					Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
					Issue:    "OOMKilled",
					Reason:   "Container exceeded its memory limit and was killed",
					Evidence: fmt.Sprintf("container %q, exitCode=%d", cs.Name, term.ExitCode),
				}
			}
		}
	}
	return nil
}
