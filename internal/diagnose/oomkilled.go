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
					Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
					Issue:     "OOMKilled",
					Reason:    "Container exceeded its memory limit and was killed",
					Evidence:  fmt.Sprintf("container %q, exitCode=%d", cs.Name, term.ExitCode),
					Resources: containerResources(facts.Pod, cs.Name),
					Container: cs.Name,
				}
			}
		}
	}
	return nil
}

// containerResources finds the named container in the pod spec (main OR init) and
// returns its cpu/memory requests and limits; nil if the container is not in the spec.
func containerResources(pod *corev1.Pod, name string) *ContainerResources {
	for _, list := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, c := range list {
			if c.Name == name {
				return &ContainerResources{
					Container:  name,
					CPURequest: quantityOrUnset(c.Resources.Requests, corev1.ResourceCPU),
					CPULimit:   quantityOrUnset(c.Resources.Limits, corev1.ResourceCPU),
					MemRequest: quantityOrUnset(c.Resources.Requests, corev1.ResourceMemory),
					MemLimit:   quantityOrUnset(c.Resources.Limits, corev1.ResourceMemory),
				}
			}
		}
	}
	return nil
}

// quantityOrUnset returns the String() of the named resource in rl, or "unset" when absent.
func quantityOrUnset(rl corev1.ResourceList, n corev1.ResourceName) string {
	if q, ok := rl[n]; ok {
		return q.String()
	}
	return "unset"
}
