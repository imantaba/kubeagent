package fuzzgen

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The reasons real clusters emit, plus "" — a kubelet may leave a reason empty,
// and a detector keyed on a string comparison must survive that.
var (
	waitingReasons = []string{
		"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull",
		"CreateContainerConfigError", "ContainerCreating", "PodInitializing", "",
	}
	terminatedReasons = []string{"OOMKilled", "Error", "Completed", "ContainerStatusUnknown", ""}
	conditionReasons  = []string{"Unschedulable", "ContainersNotReady", "PodCompleted", ""}
	eventReasons      = []string{"Unhealthy", "FailedAttachVolume", "FailedMount", "BackOff", "FailedScheduling", "Killing", ""}
	podPhases         = []corev1.PodPhase{
		corev1.PodPending, corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown, "",
	}
	probeMessagePrefixes = []string{
		"Readiness probe failed: ",
		"Liveness probe failed: ",
		"Startup probe failed: ",
		"Readiness probe failed: HTTP probe failed with statuscode: 503 ",
		"Liveness probe failed: dial tcp 192.0.2.10:8080: connect: connection refused ",
	}
)

// Pod builds a pod whose identity is DNS-1123-valid and whose unvalidated fields
// are hostile: 1-3 container statuses with matching spec containers, 0-2 init
// container statuses, 0-3 conditions, and a phase.
func (c *Cursor) Pod() *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: c.Name(20),
			Name:      c.Name(30),
		},
		Status: corev1.PodStatus{Phase: podPhases[c.IntN(len(podPhases))]},
	}
	pod.Status.ContainerStatuses = c.containerStatuses(1 + c.IntN(3))
	pod.Status.InitContainerStatuses = c.containerStatuses(c.IntN(3))
	for _, cs := range pod.Status.ContainerStatuses {
		pod.Spec.Containers = append(pod.Spec.Containers, c.container(cs.Name))
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, c.container(cs.Name))
	}
	for n := c.IntN(4); n > 0; n-- {
		pod.Status.Conditions = append(pod.Status.Conditions, c.condition())
	}
	if c.Bool() {
		pod.Spec.NodeName = c.Name(20)
	}
	if c.Bool() {
		t := c.Time(Base)
		pod.DeletionTimestamp = &t
	}
	return pod
}

// containerStatuses builds n statuses. Reason strings are drawn from the real
// vocabulary so the detectors' string comparisons match; Message is hostile,
// because the API server does not validate it.
func (c *Cursor) containerStatuses(n int) []corev1.ContainerStatus {
	var out []corev1.ContainerStatus
	for i := 0; i < n; i++ {
		cs := corev1.ContainerStatus{
			Name:         c.Name(20),
			RestartCount: c.Int32(),
		}
		switch c.IntN(3) {
		case 0:
			cs.State.Waiting = &corev1.ContainerStateWaiting{
				Reason:  c.Pick(waitingReasons),
				Message: c.Hostile(96),
			}
		case 1:
			cs.State.Running = &corev1.ContainerStateRunning{StartedAt: c.Time(Base)}
		case 2:
			cs.State.Terminated = c.terminated()
		}
		if c.Bool() {
			cs.LastTerminationState.Terminated = c.terminated()
		}
		out = append(out, cs)
	}
	return out
}

func (c *Cursor) terminated() *corev1.ContainerStateTerminated {
	// Reason is usually drawn from the real vocabulary so the detectors' string
	// comparisons (OOMKilledDetector matches "OOMKilled" exactly) stay reachable,
	// but 1 in 8 draws goes hostile instead. Unlike a container, namespace, or
	// pod name — which the API server validates as DNS-1123 — the kubelet/CRI
	// sets Reason as a free-form string the API server never validates, so a
	// generator that only ever drew clean Reasons would leave that ingress point
	// permanently untested.
	var reason string
	if c.IntN(8) == 0 {
		reason = c.Hostile(64)
	} else {
		reason = c.Pick(terminatedReasons)
	}
	return &corev1.ContainerStateTerminated{
		Reason:     reason,
		Message:    c.Hostile(96),
		ExitCode:   c.Int32(),
		StartedAt:  c.Time(Base),
		FinishedAt: c.Time(Base),
	}
}

func (c *Cursor) condition() corev1.PodCondition {
	types := []corev1.PodConditionType{
		corev1.PodScheduled, corev1.PodReady, corev1.PodInitialized, corev1.ContainersReady,
	}
	statuses := []corev1.ConditionStatus{corev1.ConditionTrue, corev1.ConditionFalse, corev1.ConditionUnknown, ""}
	return corev1.PodCondition{
		Type:    types[c.IntN(len(types))],
		Status:  statuses[c.IntN(len(statuses))],
		Reason:  c.Pick(conditionReasons),
		Message: c.Hostile(96),
	}
}

// container builds the spec entry for a status container, sometimes with a
// memory limit so the OOMKilled finding's Resources path is reachable.
func (c *Cursor) container(name string) corev1.Container {
	ctr := corev1.Container{Name: name, Image: c.Name(20) + ":" + c.Name(8)}
	if c.Bool() {
		ctr.LivenessProbe = &corev1.Probe{}
	}
	if c.Bool() {
		ctr.ReadinessProbe = &corev1.Probe{}
	}
	if c.Bool() {
		ctr.Resources.Limits = corev1.ResourceList{
			corev1.ResourceMemory: *resource.NewQuantity(int64(c.IntN(4096)+1)*1024*1024, resource.BinarySI),
		}
	}
	return ctr
}

// Events builds 0..max events for pod. Reason is drawn from the real vocabulary
// so the detectors' filters match; Message and FieldPath are hostile, because
// the API server validates neither and ProbeFailureDetector parses both.
func (c *Cursor) Events(pod *corev1.Pod, max int) []corev1.Event {
	var out []corev1.Event
	for n := c.IntN(max + 1); n > 0; n-- {
		e := corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: pod.Namespace, Name: c.Name(20)},
			Reason:        c.Pick(eventReasons),
			LastTimestamp: c.Time(Base),
			InvolvedObject: corev1.ObjectReference{
				Kind:      "Pod",
				Namespace: pod.Namespace,
				Name:      pod.Name,
			},
		}
		switch c.IntN(3) {
		case 0:
			e.Message = c.Hostile(96)
		case 1:
			e.Message = c.Pick(probeMessagePrefixes) + c.Hostile(48)
		case 2:
			e.Message = "Multi-Attach error for volume " + c.Name(20) + " " + c.Hostile(48)
		}
		switch {
		case c.Bool() && len(pod.Spec.Containers) > 0:
			e.InvolvedObject.FieldPath = "spec.containers{" + pod.Spec.Containers[c.IntN(len(pod.Spec.Containers))].Name + "}"
		case c.Bool():
			e.InvolvedObject.FieldPath = "spec.containers{" + c.Hostile(24) + "}"
		default:
			e.InvolvedObject.FieldPath = c.Hostile(24)
		}
		out = append(out, e)
	}
	return out
}

// TLSSecret wraps crt in a kubernetes.io/tls Secret. Deliberately NO tls.key
// entry: certhealth.Assess parses only the public certificate and must never
// depend on the private key. A nil or empty crt leaves tls.crt absent, which is
// the shape that reaches the "missing tls.crt" branch.
func (c *Cursor) TLSSecret(crt []byte) corev1.Secret {
	s := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: c.Name(20), Name: c.Name(30)},
		Type:       corev1.SecretTypeTLS,
	}
	if len(crt) > 0 {
		s.Data = map[string][]byte{"tls.crt": crt}
	}
	return s
}
