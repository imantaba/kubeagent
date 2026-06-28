package diagnose

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podWaiting returns a pod whose single container is Waiting with reason+message.
func podWaiting(namespace, name, container, reason, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: container,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message},
				},
			}},
		},
	}
}

// podOOMKilled returns a pod with a container terminated by OOMKilled.
// If viaLastTermination is true, the OOM is recorded in LastTerminationState
// (the pod has since restarted); otherwise in the current State.
func podOOMKilled(namespace, name, container string, exitCode int32, viaLastTermination bool) *corev1.Pod {
	term := &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: exitCode}
	cs := corev1.ContainerStatus{Name: container}
	if viaLastTermination {
		cs.LastTerminationState = corev1.ContainerState{Terminated: term}
	} else {
		cs.State = corev1.ContainerState{Terminated: term}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

// podOOMKilledWithResources is an OOMKilled pod whose spec declares the killed
// container with the given requests/limits.
func podOOMKilledWithResources(ns, name, container string, res corev1.ResourceRequirements) *corev1.Pod {
	p := podOOMKilled(ns, name, container, 137, false)
	p.Spec.Containers = []corev1.Container{{Name: container, Resources: res}}
	return p
}

// podUnschedulable returns a Pending pod with a PodScheduled=False/Unschedulable condition.
func podUnschedulable(namespace, name, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  "Unschedulable",
				Message: message,
			}},
		},
	}
}
