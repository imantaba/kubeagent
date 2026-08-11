package diagnose

import (
	"fmt"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// ConfigErrorDetector flags a main container stuck in CreateContainerConfigError
// — a referenced ConfigMap/Secret is missing, or a required key is absent, so the
// container cannot start.
//
// Main containers only. The same failure on an init container is a different
// diagnosis — nothing in the pod has run yet — and is reported by
// InitContainerDetector as Init:CreateContainerConfigError, with the position of
// the failing init container in the sequence.
type ConfigErrorDetector struct{}

func (d ConfigErrorDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason == "CreateContainerConfigError" {
			return &Finding{
				Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:     "CreateContainerConfigError",
				Reason:    "a referenced ConfigMap or Secret is missing, or a required key is absent — the container cannot start",
				Evidence:  fmt.Sprintf("container %q: %s", cs.Name, safetext.Line(w.Message)),
				Container: cs.Name,
			}
		}
	}
	return nil
}
