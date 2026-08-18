package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// InitContainerDetector flags a pod blocked in its init phase because an init
// container is failing. Init containers run sequentially and block the pod, so at
// most one is actively failing; the detector reports the first failing one. It
// reads pod.Status.InitContainerStatuses — the slice no other detector looks at —
// so there is no overlap: while an init container fails, the main containers sit
// in Waiting/PodInitializing, which no detector matches.
//
// That claim is load-bearing and was once false: ConfigErrorDetector read the
// init slice as a fallback, so a missing ConfigMap on an init container was
// reported as the main-container kind. It reads main containers only now, and
// the init case is the CreateContainerConfigError arm below.
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
// CreateContainerConfigError, then CrashLoopBackOff. The config-error arm sits
// above the crash-loop one because it names the cause: a container that cannot
// be created for a missing ConfigMap still accumulates restarts.
func initFinding(pod *corev1.Pod, cs corev1.ContainerStatus, idx, total int) *Finding {
	pos := fmt.Sprintf("(%d/%d)", idx+1, total)
	podName := pod.Namespace + "/" + pod.Name

	if w := cs.State.Waiting; w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull") {
		return &Finding{
			Pod:       podName,
			Issue:     "Init:" + w.Reason,
			Reason:    "an init container's image cannot be pulled — the pod cannot start",
			Evidence:  fmt.Sprintf("init container %q %s: %s", cs.Name, pos, safetext.Line(w.Message)),
			Container: cs.Name,
			Image:     cs.Image,
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
	if w := cs.State.Waiting; w != nil && w.Reason == "CreateContainerConfigError" {
		return &Finding{
			Pod:       podName,
			Issue:     "Init:" + w.Reason,
			Reason:    "an init container's ConfigMap or Secret is missing, or a required key is absent — the pod cannot start",
			Evidence:  fmt.Sprintf("init container %q %s: %s", cs.Name, pos, safetext.Line(w.Message)),
			Container: cs.Name,
		}
	}
	// The crash-loop arm matches two windows of the same fault. Waiting +
	// CrashLoopBackOff is the kubelet's own verdict that the container is
	// looping. Terminated with a non-zero exit is kubeagent inferring the loop
	// from one sample, so it carries a threshold the Waiting arm does not: one
	// prior restart is the price of inferring. The asymmetry is deliberate.
	//
	// State.Terminated only, never LastTerminationState — an init container that
	// failed once and then succeeded is healthy, and its last termination still
	// carries the error.
	crashing := false
	if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
		crashing = true
	}
	if t := cs.State.Terminated; t != nil && t.ExitCode != 0 && t.Reason != "OOMKilled" && cs.RestartCount >= 1 {
		crashing = true
	}
	if crashing {
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
