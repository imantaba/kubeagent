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
	}
	return Unverified, evidenceNoRule
}

// checkNode re-checks a node candidate against the fresh node read. The
// reason inside the candidate's last parentheses picks the rule: the two
// heartbeat reasons cannot be refuted by a Ready=True condition alone,
// because the lease was not re-read; every other reason reads like NotReady.
func checkNode(h inventory.Hypothesis, reads Reads) (Outcome, string) {
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
