package diagnose

import corev1 "k8s.io/api/core/v1"

// PendingDetector flags pods the scheduler cannot place on any node.
type PendingDetector struct{}

func (d PendingDetector) Detect(facts PodFacts) *Finding {
	if facts.Pod.Status.Phase != corev1.PodPending {
		return nil
	}
	for _, c := range facts.Pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == "Unschedulable" {
			return &Finding{
				Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:    "Unschedulable",
				Reason:   "No node can schedule this pod (resources, taints, or affinity)",
				Evidence: c.Message,
			}
		}
	}
	return nil
}
