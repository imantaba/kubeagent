package hypothesis

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// Evidence sentences shared by every rule.
const (
	evidenceNeverRead    = "not re-read: the read budget was spent first"
	evidenceFailedPrefix = "fresh read failed: "
	evidenceNoRule       = "no rule re-checks this candidate kind"
)

// check dispatches one candidate to the rule for its kind.
func check(w inventory.Workload, h inventory.Hypothesis, reads Reads) (Outcome, string) {
	switch h.Kind {
	case "node":
		return checkNode(h, reads)
	case "pvc":
		return checkPVC(w, h, reads)
	case "registry":
		return checkRegistry(w, h, reads)
	}
	return Unverified, evidenceNoRule
}

// checkNode re-checks a node candidate against the fresh node read. The
// reason inside the candidate's last parentheses picks the rule: the two
// heartbeat reasons cannot be refuted by a Ready=True condition alone,
// because the lease was not re-read; every other reason reads like NotReady.
func checkNode(h inventory.Hypothesis, reads Reads) (Outcome, string) {
	// A candidate with no object does not occur: rootcause always names the node or claim.
	if h.Object == "" {
		return Unverified, evidenceNeverRead
	}
	if msg, ok := reads.Failed["node/"+h.Object]; ok {
		return Unverified, evidenceFailedPrefix + msg
	}
	n, ok := reads.Nodes[h.Object]
	if !ok || n == nil {
		return Unverified, evidenceNeverRead
	}
	heartbeat := false
	switch parenthesized(h.Cause) {
	case "kubelet not heartbeating", "no kubelet lease":
		heartbeat = true
	}
	status, found := readyCondition(n)
	switch {
	case !found:
		return Confirmed, "the node has no Ready condition"
	case status == corev1.ConditionFalse:
		return Confirmed, "Ready condition is False now"
	case status == corev1.ConditionUnknown:
		return Confirmed, "Ready condition is Unknown now"
	case status == corev1.ConditionTrue && heartbeat:
		return Unverified, "Ready condition is True, but the kubelet lease was not re-read"
	case status == corev1.ConditionTrue:
		return Refuted, "Ready condition is True now"
	}
	return Unverified, "Ready condition is not one kubeagent expects"
}

// checkPVC re-checks a PVC candidate against the fresh claim read. The
// claim is keyed by the workload's namespace, because a PVC candidate
// names a claim the workload's pods mount.
func checkPVC(w inventory.Workload, h inventory.Hypothesis, reads Reads) (Outcome, string) {
	// A candidate with no object does not occur: rootcause always names the node or claim.
	if h.Object == "" {
		return Unverified, evidenceNeverRead
	}
	key := w.Namespace + "/" + h.Object
	if msg, ok := reads.Failed["pvc/"+key]; ok {
		return Unverified, evidenceFailedPrefix + msg
	}
	pvc, ok := reads.PVCs[key]
	if !ok || pvc == nil {
		return Unverified, evidenceNeverRead
	}
	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		return Refuted, "phase is Bound now"
	case corev1.ClaimPending:
		return Confirmed, "phase is still Pending"
	case corev1.ClaimLost:
		return Confirmed, "phase is Lost"
	}
	return Unverified, "phase is not one kubeagent expects"
}

// readyCondition returns the node's Ready condition status and whether the
// condition exists at all.
func readyCondition(n *corev1.Node) (corev1.ConditionStatus, bool) {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status, true
		}
	}
	return "", false
}

// parenthesized returns the text inside the last "(…)" pair of s, or ""
// when s has no complete pair. "node worker-1 (NotReady)" → "NotReady".
func parenthesized(s string) string {
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return ""
	}
	start := strings.LastIndexByte(s[:end], '(')
	if start < 0 {
		return ""
	}
	return s[start+1 : end]
}

// The three closed literal lists. Matching runs on the lowercased raw
// message; only a literal from these lists ever enters a sentence.
// authLiterals is ordered most-specific-first so that Docker Hub's
// "pull access denied … repository does not exist" reads as auth.
var (
	connectionLiterals = []string{"dial tcp", "i/o timeout", "connection refused", "connection reset",
		"no such host", "network is unreachable", "tls handshake", "x509:", "502 bad gateway",
		"503 service unavailable", "504 gateway timeout", "toomanyrequests", "429 too many requests"}
	authLiterals  = []string{"pull access denied", "no basic auth credentials", "unauthorized", "denied"}
	imageLiterals = []string{"manifest unknown", "not found", "name unknown", "repository does not exist",
		"invalid reference format"}
)

const (
	evidenceNoPullEvent   = "no pull event names the failure; events may have aged out"
	evidencePullPodUnread = "events of the pulling pod were not read"
)

// pullPod is the pod name of the first pull finding, or "" when there is none.
func pullPod(w inventory.Workload) string {
	for _, f := range w.Findings {
		if f.Issue == "ImagePullBackOff" || f.Issue == "ErrImagePull" {
			return podPart(f.Pod)
		}
	}
	return ""
}

// eventsPod is the pod whose events the gather reads for this workload:
// the first finding's pod, or the workload name when there is no finding.
// It mirrors the gather's choice exactly.
func eventsPod(w inventory.Workload) string {
	if len(w.Findings) > 0 {
		if p := podPart(w.Findings[0].Pod); p != "" {
			return p
		}
	}
	return w.Name
}

// podPart returns the name half of a "namespace/name" pod reference, or
// "" when the reference has no slash. It matches the gather's copy.
func podPart(pod string) string {
	if _, name, ok := strings.Cut(pod, "/"); ok {
		return name
	}
	return ""
}

// checkRegistry re-checks a registry candidate against the pull pod's
// fresh events. The gather reads one pod's events per workload; when that
// is not the pulling pod, the rule says so instead of guessing.
func checkRegistry(w inventory.Workload, h inventory.Hypothesis, reads Reads) (Outcome, string) {
	pod := pullPod(w)
	key := w.Namespace + "/" + pod
	if pod != "" {
		if evs, ok := reads.Events[key]; ok {
			return classifyPullEvents(evs)
		}
		if msg, ok := reads.Failed["events/"+key]; ok {
			return Unverified, evidenceFailedPrefix + msg
		}
	}
	if pod == "" || pod != eventsPod(w) {
		return Unverified, evidencePullPodUnread
	}
	return Unverified, evidenceNeverRead
}

// isPullEvent reports whether e is a kubelet pull failure: Reason Failed
// or BackOff, and a message that mentions a pull.
func isPullEvent(e corev1.Event) bool {
	if e.Reason != "Failed" && e.Reason != "BackOff" {
		return false
	}
	return strings.Contains(strings.ToLower(e.Message), "pull")
}

// classifyPullEvents picks the strongest class any pull event shows:
// connection beats auth beats image.
func classifyPullEvents(evs []corev1.Event) (Outcome, string) {
	var msgs []string
	for _, e := range evs {
		if isPullEvent(e) {
			msgs = append(msgs, strings.ToLower(e.Message))
		}
	}
	if lit := firstMatch(msgs, connectionLiterals); lit != "" {
		return Confirmed, "a pull event shows a connection error: " + lit
	}
	if lit := firstMatch(msgs, authLiterals); lit != "" {
		return Unverified, "a pull event shows an auth error: " + lit + "; that can be one image or the whole host"
	}
	if lit := firstMatch(msgs, imageLiterals); lit != "" {
		return Refuted, "a pull event shows an image error: " + lit + "; this pull fails for this image, not the host"
	}
	return Unverified, evidenceNoPullEvent
}

// firstMatch returns the first literal, in list order, that any message
// contains, or "".
func firstMatch(msgs, literals []string) string {
	for _, lit := range literals {
		for _, m := range msgs {
			if strings.Contains(m, lit) {
				return lit
			}
		}
	}
	return ""
}
