package diagnose

import "fmt"

// ImagePullDetector flags containers that cannot pull their image.
type ImagePullDetector struct{}

func (d ImagePullDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull") {
			return &Finding{
				Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:    w.Reason,
				Reason:   "Bad image reference or registry authentication",
				Evidence: fmt.Sprintf("container %q: %s", cs.Name, w.Message),
			}
		}
	}
	return nil
}
