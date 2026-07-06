package diagnose

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// VolumeAttachDetector flags a pod stuck at container creation because a volume
// cannot be attached (a FailedAttachVolume event) — most often a Multi-Attach
// error (a ReadWriteOnce volume still attached to another node).
type VolumeAttachDetector struct{}

func (d VolumeAttachDetector) Detect(facts PodFacts) *Finding {
	if podReady(facts.Pod) || !stuckCreating(facts.Pod) {
		return nil
	}
	ev := newestAttachEvent(facts.Events)
	if ev == nil {
		return nil
	}
	reason := "a volume cannot be attached to the pod's node"
	if strings.Contains(ev.Message, "Multi-Attach") {
		reason = "the volume is attached to another node (Multi-Attach) — the pod cannot mount it"
	}
	return &Finding{
		Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
		Issue:    "VolumeAttachError",
		Reason:   reason,
		Evidence: ev.Message,
	}
}

// podReady reports whether the pod has a Ready condition with status True.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// stuckCreating reports whether the pod is still at container creation: a
// container Waiting with reason ContainerCreating, or no container statuses yet.
// This excludes pods that progressed past volume setup (Running, CrashLoopBackOff,
// ImagePullBackOff), so a stale attach event cannot cause a false positive.
func stuckCreating(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason == "ContainerCreating" {
			return true
		}
	}
	return false
}

// newestAttachEvent returns the most recent FailedAttachVolume event (by
// LastTimestamp), or nil.
func newestAttachEvent(events []corev1.Event) *corev1.Event {
	var matches []corev1.Event
	for _, e := range events {
		if e.Reason == "FailedAttachVolume" {
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
