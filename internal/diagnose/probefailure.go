package diagnose

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/safetext"
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
//
// When more than one probe is failing at once the finding names the heaviest —
// liveness, then startup, then readiness — rather than the most recent, and its
// evidence lists every probe type failing on that container. See
// probeCandidates for the window that bounds the comparison.
type ProbeFailureDetector struct{}

func (d ProbeFailureDetector) Detect(facts PodFacts) *Finding {
	if podReady(facts.Pod) {
		return nil
	}
	candidates := probeCandidates(facts)
	if len(candidates) == 0 {
		return nil
	}
	sel := candidates[0]
	reason := sel.reason
	if reason == "" {
		// The reason vocabulary is HTTP- and TCP-shaped, so an exec probe
		// almost always lands here. Its output is exactly the text that may not
		// be forwarded, so name the handler and say the output was withheld
		// rather than end the line with no explanation of the silence.
		if h := probeHandler(facts.Pod, sel.container, sel.probeType); h != "" {
			reason = h + " probe, output withheld"
		}
	}
	return &Finding{
		Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
		Issue:    "ProbeFailure",
		Reason:   probeReason(sel.probeType),
		Evidence: probeEvidence(sel.container, probeTypesFor(candidates, sel.container), reason),
		// containerFromFieldPath returns a substring of an unvalidated field
		// path. probeEvidence escapes it with %q, but this field is raw, and it
		// reaches JSON, SARIF and the TUI. A real cluster's field path already
		// carries a DNS-1123 name, so this is a no-op on every real input.
		Container: safetext.Line(sel.container),
	}
}

// probeRankWindow is how far back from the newest Unhealthy event a competing
// probe failure may sit and still be ranked against it.
//
// The window is what keeps the ranking honest. A container whose liveness probe
// failed once an hour ago and whose readiness probe is failing now has a
// readiness problem; without a bound, the heavier type would win forever — or
// until the event expired, which is not a diagnosis.
const probeRankWindow = 2 * time.Minute

// probeCandidate is one Unhealthy event that named a probe type, already
// classified.
type probeCandidate struct {
	container string
	probeType string
	reason    string
	event     corev1.Event
}

// probeWeight ranks a probe type by consequence: a failing liveness probe
// restarts the container, a failing startup probe stops it ever starting, and a
// failing readiness probe only takes it out of Service endpoints. Ranking by
// timestamp alone made a readiness failure one second newer than a liveness
// failure decide the whole finding.
func probeWeight(probeType string) int {
	switch probeType {
	case "liveness":
		return 3
	case "startup":
		return 2
	case "readiness":
		return 1
	}
	return 0
}

// probeCandidates returns the classifiable Unhealthy events inside the ranking
// window whose container is eligible for a finding, worst first.
//
// The window is anchored on the newest Unhealthy event rather than on now,
// which is why this detector still reads no clock: a whole pod's events shifted
// an hour into the past rank exactly as they do fresh. Ineligible containers are
// dropped before ranking, not after, so a liveness failure on a container that
// is no longer Running cannot win the ranking and then suppress a live readiness
// failure on a container that is.
func probeCandidates(facts PodFacts) []probeCandidate {
	newest := newestUnhealthyEvent(facts.Events)
	if newest == nil {
		return nil
	}
	cutoff := newest.LastTimestamp.Time.Add(-probeRankWindow)
	var out []probeCandidate
	for _, e := range facts.Events {
		if e.Reason != "Unhealthy" || e.LastTimestamp.Time.Before(cutoff) {
			continue
		}
		probeType, reason := classifyProbe(e.Message)
		if probeType == "" {
			continue
		}
		container := containerFromFieldPath(e.InvolvedObject.FieldPath)
		if !probeContainerEligible(facts.Pod, container) {
			continue
		}
		out = append(out, probeCandidate{container: container, probeType: probeType, reason: reason, event: e})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if wa, wb := probeWeight(a.probeType), probeWeight(b.probeType); wa != wb {
			return wa > wb
		}
		if !a.event.LastTimestamp.Equal(&b.event.LastTimestamp) {
			return a.event.LastTimestamp.After(b.event.LastTimestamp.Time)
		}
		// Two events of the same type at the same instant still have to order
		// the same way on every run, whatever the API server listed them in.
		if a.event.Name != b.event.Name {
			return a.event.Name < b.event.Name
		}
		if a.event.InvolvedObject.FieldPath != b.event.InvolvedObject.FieldPath {
			return a.event.InvolvedObject.FieldPath < b.event.InvolvedObject.FieldPath
		}
		return a.event.Message < b.event.Message
	})
	return out
}

// probeContainerEligible reports whether a finding may name this event's
// container: a named container must currently be Running, and an event with no
// container in its field path falls back to the pod's own phase.
func probeContainerEligible(pod *corev1.Pod, container string) bool {
	if container != "" {
		return containerRunning(pod, container)
	}
	return pod.Status.Phase == corev1.PodRunning
}

// probeTypesFor lists the distinct probe types failing on one container inside
// the window, heaviest first. candidates is already sorted worst first, so one
// pass preserves that order.
//
// Only the selected container's types are listed: a liveness failure on a
// sidecar may take the finding, but the app container's readiness failure is a
// different container's problem and must not be collected into the sidecar's
// evidence line.
func probeTypesFor(candidates []probeCandidate, container string) []string {
	var types []string
	for _, c := range candidates {
		if c.container != container {
			continue
		}
		seen := false
		for _, t := range types {
			if t == c.probeType {
				seen = true
				break
			}
		}
		if !seen {
			types = append(types, c.probeType)
		}
	}
	return types
}

// probeHandler names the handler kind of one container's probe — "exec",
// "httpGet", "tcpSocket" or "gRPC" — or "" when the container or that probe is
// absent from the spec.
//
// These are typed fields on an object the API server validates, not free text,
// so naming one adds no sanitization surface and no new leak surface: the exec
// command itself, the HTTP path and the port are never read.
func probeHandler(pod *corev1.Pod, container, probeType string) string {
	spec := containerSpec(pod, container)
	if spec == nil {
		return ""
	}
	var p *corev1.Probe
	switch probeType {
	case "readiness":
		p = spec.ReadinessProbe
	case "liveness":
		p = spec.LivenessProbe
	case "startup":
		p = spec.StartupProbe
	}
	if p == nil {
		return ""
	}
	switch {
	case p.Exec != nil:
		return "exec"
	case p.HTTPGet != nil:
		return "httpGet"
	case p.TCPSocket != nil:
		return "tcpSocket"
	case p.GRPC != nil:
		return "gRPC"
	}
	return ""
}

// containerSpec returns the named container's spec, main containers first, or
// nil.
func containerSpec(pod *corev1.Pod, name string) *corev1.Container {
	if name == "" {
		return nil
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
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
//
// probeTypes names every probe type failing on this container inside the
// ranking window, not only the one that decided the finding — an operator who
// is told the liveness probe is failing still wants to know the readiness probe
// is failing too. The reason suffix comes from the selected event alone.
func probeEvidence(container string, probeTypes []string, reason string) string {
	e := joinProbeTypes(probeTypes)
	if container != "" {
		e = fmt.Sprintf("container %q: %s", container, e)
	}
	if reason != "" {
		e += " — " + reason
	}
	return e
}

// joinProbeTypes renders the probe-type list as a sentence subject: "liveness
// probe failed", "liveness and readiness probes failed", "liveness, startup and
// readiness probes failed".
func joinProbeTypes(types []string) string {
	switch len(types) {
	case 0:
		return "probe failed"
	case 1:
		return types[0] + " probe failed"
	}
	return strings.Join(types[:len(types)-1], ", ") + " and " + types[len(types)-1] + " probes failed"
}
