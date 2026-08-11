package diagnose

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// VolumeMountDetector flags a pod stuck at container creation because a volume
// cannot be mounted (a FailedMount event).
//
// Separate from VolumeAttachDetector, which matches FailedAttachVolume only.
// The two look alike and are not: an attach failure is a storage problem, while
// the most common mount failure has no storage in it at all — a ConfigMap or
// Secret named in the pod spec that does not exist. The kubelet reports no
// CreateContainerConfigError for a volume source, so without this detector that
// pod carries no diagnosis anywhere.
type VolumeMountDetector struct{}

func (d VolumeMountDetector) Detect(facts PodFacts) *Finding {
	if podReady(facts.Pod) || !stuckCreating(facts.Pod) {
		return nil
	}
	ev := newestMountEvent(facts.Events)
	if ev == nil {
		return nil
	}
	// Matching runs on the raw message — a control character spliced mid-word
	// must not be able to route a finding into the wrong arm. safetext.Line is
	// applied where the message becomes evidence, below.
	reason := "a volume the pod needs could not be mounted — the pod cannot start"
	if absentVolumeSource(ev.Message) {
		reason = "a ConfigMap or Secret the pod mounts as a volume does not exist — the pod cannot start"
	}
	return &Finding{
		Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
		Issue:    "VolumeMountError",
		Reason:   reason,
		Evidence: safetext.Line(ev.Message),
	}
}

// absentVolumeSource reports whether the kubelet's message names a volume
// source object that does not exist. The default arm deliberately claims less
// than this one and less than VolumeAttachDetector's: kubeagent knows the mount
// did not complete, not that a node could not attach anything.
func absentVolumeSource(message string) bool {
	if !strings.Contains(message, "not found") {
		return false
	}
	return strings.Contains(message, "configmap ") || strings.Contains(message, "secret ")
}

// newestMountEvent returns the most recent FailedMount event (by LastTimestamp),
// or nil.
func newestMountEvent(events []corev1.Event) *corev1.Event {
	var matches []corev1.Event
	for _, e := range events {
		if e.Reason == "FailedMount" {
			matches = append(matches, e)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].LastTimestamp.After(matches[j].LastTimestamp.Time)
	})
	return &matches[0]
}
