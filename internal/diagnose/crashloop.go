package diagnose

import "fmt"

// CrashLoopDetector flags containers stuck in CrashLoopBackOff.
type CrashLoopDetector struct{}

func (d CrashLoopDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
			return &Finding{
				Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:     "CrashLoopBackOff",
				Reason:    "Container repeatedly crashes after starting",
				Evidence:  fmt.Sprintf("container %q, restartCount=%d", cs.Name, cs.RestartCount),
				Container: cs.Name,
			}
		}
	}
	return nil
}
