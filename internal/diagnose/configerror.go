package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ConfigErrorDetector flags a container stuck in CreateContainerConfigError — a
// referenced ConfigMap/Secret is missing, or a required key is absent, so the
// container cannot start. Scans main and init containers.
type ConfigErrorDetector struct{}

func (d ConfigErrorDetector) Detect(facts PodFacts) *Finding {
	if f := configErrorIn(facts, facts.Pod.Status.ContainerStatuses, "container"); f != nil {
		return f
	}
	return configErrorIn(facts, facts.Pod.Status.InitContainerStatuses, "init container")
}

func configErrorIn(facts PodFacts, statuses []corev1.ContainerStatus, kind string) *Finding {
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && w.Reason == "CreateContainerConfigError" {
			return &Finding{
				Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:     "CreateContainerConfigError",
				Reason:    "a referenced ConfigMap or Secret is missing, or a required key is absent — the container cannot start",
				Evidence:  fmt.Sprintf("%s %q: %s", kind, cs.Name, w.Message),
				Container: cs.Name,
			}
		}
	}
	return nil
}
