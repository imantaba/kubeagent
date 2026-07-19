package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// InitContainerDetector flags a pod blocked in its init phase because an init
// container is failing. Init containers run sequentially and block the pod, so at
// most one is actively failing; the detector reports the first failing one. It
// reads pod.Status.InitContainerStatuses — the slice the main-container crash
// detectors do not look at — so there is no overlap: while an init container
// fails, the main containers sit in Waiting/PodInitializing, which no detector
// matches.
type InitContainerDetector struct{}

func (d InitContainerDetector) Detect(facts PodFacts) *Finding {
	statuses := facts.Pod.Status.InitContainerStatuses
	for i, cs := range statuses {
		if f := initFinding(facts.Pod, cs, i, len(statuses)); f != nil {
			return f
		}
	}
	return nil
}

// initFinding classifies one init container's failure, or returns nil if it is not
// in a failing state (succeeded, not yet started, or a healthy running sidecar).
// Precedence: image-pull, then OOMKilled (current or last termination), then
// CrashLoopBackOff.
func initFinding(pod *corev1.Pod, cs corev1.ContainerStatus, idx, total int) *Finding {
	pos := fmt.Sprintf("(%d/%d)", idx+1, total)
	podName := pod.Namespace + "/" + pod.Name

	if w := cs.State.Waiting; w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull") {
		return &Finding{
			Pod:       podName,
			Issue:     "Init:" + w.Reason,
			Reason:    "an init container's image cannot be pulled — the pod cannot start",
			Evidence:  fmt.Sprintf("init container %q %s: %s", cs.Name, pos, w.Message),
			Container: cs.Name,
		}
	}
	for _, term := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
		if term != nil && term.Reason == "OOMKilled" {
			return &Finding{
				Pod:       podName,
				Issue:     "Init:OOMKilled",
				Reason:    "an init container was killed for exceeding its memory limit — the pod cannot start",
				Evidence:  fmt.Sprintf("init container %q %s, exitCode=%d", cs.Name, pos, term.ExitCode),
				Resources: containerResources(pod, cs.Name),
				Container: cs.Name,
			}
		}
	}
	if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
		return &Finding{
			Pod:       podName,
			Issue:     "Init:CrashLoopBackOff",
			Reason:    "an init container is crash-looping — the pod cannot start its main containers",
			Evidence:  fmt.Sprintf("init container %q %s, restartCount=%d", cs.Name, pos, cs.RestartCount),
			Container: cs.Name,
		}
	}
	return nil
}
