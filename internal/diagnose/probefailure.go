package diagnose

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// ProbeFailureDetector flags a pod that is not Ready because a container's
// readiness, liveness, or startup probe keeps failing (an "Unhealthy" event).
// It is complementary to the restart detectors: a liveness/startup probe that
// restarts a container also trips RestartLoop/CrashLoop; ProbeFailure names the
// probe as the cause. The "container currently Running" guard keeps a
// CrashLoopBackOff/ImagePullBackOff container (which is Waiting) from being
// double-flagged here. To preserve the --explain privacy guarantee, the raw
// probe message (which may carry a pod IP or arbitrary exec-probe output) is
// never stored; Reason and Evidence are built only from fixed strings.
type ProbeFailureDetector struct{}

func (d ProbeFailureDetector) Detect(facts PodFacts) *Finding {
	if podReady(facts.Pod) {
		return nil
	}
	ev := newestUnhealthyEvent(facts.Events)
	if ev == nil {
		return nil
	}
	container := containerFromFieldPath(ev.InvolvedObject.FieldPath)
	if container != "" {
		if !containerRunning(facts.Pod, container) {
			return nil
		}
	} else if facts.Pod.Status.Phase != corev1.PodRunning {
		return nil
	}
	probeType, reason := classifyProbe(ev.Message)
	if probeType == "" {
		return nil
	}
	return &Finding{
		Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
		Issue:     "ProbeFailure",
		Reason:    probeReason(probeType),
		Evidence:  probeEvidence(container, probeType, reason),
		Container: container,
	}
}

// newestUnhealthyEvent returns the most recent Reason=="Unhealthy" event (by
// LastTimestamp), or nil.
func newestUnhealthyEvent(events []corev1.Event) *corev1.Event {
	var matches []corev1.Event
	for _, e := range events {
		if e.Reason == "Unhealthy" {
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

// containerFromFieldPath extracts the container name from an event involvedObject
// FieldPath, e.g. `spec.containers{web}` -> "web"; "" when there are no braces.
func containerFromFieldPath(fp string) string {
	openIdx := strings.IndexByte(fp, '{')
	closeIdx := strings.IndexByte(fp, '}')
	if openIdx < 0 || closeIdx < 0 || closeIdx < openIdx {
		return ""
	}
	return fp[openIdx+1 : closeIdx]
}

// containerRunning reports whether the named container is currently Running.
func containerRunning(pod *corev1.Pod, name string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name {
			return cs.State.Running != nil
		}
	}
	return false
}

// classifyProbe reads the probe type from the message prefix and derives a
// coarse, IP-free failure reason from the tail. The raw message is never
// returned. reason is "" when the tail is unrecognized (e.g. exec output);
// probeType is "" when the message is not a recognized probe failure.
func classifyProbe(message string) (probeType, reason string) {
	switch {
	case strings.HasPrefix(message, "Readiness probe failed"):
		probeType = "readiness"
	case strings.HasPrefix(message, "Liveness probe failed"):
		probeType = "liveness"
	case strings.HasPrefix(message, "Startup probe failed"):
		probeType = "startup"
	default:
		return "", ""
	}
	return probeType, probeReasonTail(message)
}

// probeReasonTail maps a probe message to a coarse, IP-free reason; "" if none match.
func probeReasonTail(message string) string {
	m := strings.ToLower(message)
	switch {
	case strings.Contains(m, "connection refused"):
		return "connection refused"
	case strings.Contains(m, "connection reset"):
		return "connection reset"
	case strings.Contains(m, "no route to host"), strings.Contains(m, "network is unreachable"):
		return "unreachable"
	case strings.Contains(m, "no such host"), strings.Contains(m, "server misbehaving"):
		return "DNS lookup failed"
	case strings.Contains(m, "context deadline exceeded"), strings.Contains(m, "timeout"):
		return "timed out"
	case strings.Contains(m, "statuscode:"):
		if code := httpStatusCode(message); code != "" {
			return "HTTP " + code
		}
		return ""
	case strings.Contains(m, "not_serving"):
		return "gRPC NOT_SERVING"
	default:
		return ""
	}
}

// httpStatusCode extracts the integer following "statuscode: " in an HTTP probe message.
func httpStatusCode(message string) string {
	const marker = "statuscode: "
	// Search case-insensitively so this stays aligned with probeReasonTail's
	// lowercased gate; ASCII ToLower preserves byte offsets, so i indexes the original.
	i := strings.Index(strings.ToLower(message), marker)
	if i < 0 {
		return ""
	}
	rest := message[i+len(marker):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	return rest[:j]
}

// probeReason is the static, clean, per-probe-type root cause sentence.
func probeReason(probeType string) string {
	switch probeType {
	case "readiness":
		return "the readiness probe keeps failing — the pod is kept out of Service endpoints"
	case "liveness":
		return "the liveness probe keeps failing — the kubelet restarts the container"
	case "startup":
		return "the startup probe keeps failing — the container never finishes starting"
	default:
		return "a probe keeps failing"
	}
}

// probeEvidence builds the clean, IP-free evidence line; the reason suffix and the
// container prefix are each omitted when empty.
func probeEvidence(container, probeType, reason string) string {
	var e string
	if container != "" {
		e = fmt.Sprintf("container %q: %s probe failed", container, probeType)
	} else {
		e = fmt.Sprintf("%s probe failed", probeType)
	}
	if reason != "" {
		e += " — " + reason
	}
	return e
}
