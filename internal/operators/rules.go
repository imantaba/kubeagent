package operators

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// maxReasonRunes caps a reported reason. An operator can put an arbitrarily
// long string in a condition's reason field; one CR must not flood the report.
const maxReasonRunes = 120

// ConditionRule reads .status.conditions[type=Type].status, the Kubernetes API
// convention: "True" ⇒ healthy, "False" ⇒ unhealthy, "Unknown" ⇒ progressing,
// the condition absent ⇒ unknown.
//
// Only the condition's reason is reported, never its message. A reason is a
// CamelCase token by API convention; a message is arbitrary operator prose that
// routinely embeds URLs — cert-manager puts ACME order URLs in it — and a URL
// can carry a token.
type ConditionRule struct{ Type string }

// Evaluate implements Rule.
func (r ConditionRule) Evaluate(obj map[string]any) (State, string) {
	conds, found, err := unstructured.NestedSlice(obj, "status", "conditions")
	if err != nil || !found {
		return StateUnknown, ""
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t != r.Type {
			continue
		}
		status, _, _ := unstructured.NestedString(m, "status")
		reason, _, _ := unstructured.NestedString(m, "reason")
		switch status {
		case "True":
			return StateHealthy, ""
		case "False":
			return StateUnhealthy, conditionReason(r.Type, status, reason)
		case "Unknown":
			return StateProgressing, conditionReason(r.Type, status, reason)
		}
		return StateUnknown, ""
	}
	return StateUnknown, ""
}

// conditionReason renders "Type=Status", plus the operator's reason when set.
func conditionReason(condType, status, reason string) string {
	out := condType + "=" + status
	if reason != "" {
		out += ": " + reason
	}
	return shortReason(out)
}

// FieldRule reads a string at Path and maps it through explicit value sets.
// Matching is exact and case-sensitive: Longhorn writes "healthy", Argo CD
// writes "Healthy". A value in no set ⇒ unknown — an unrecognized value from a
// CRD version kubeagent has not seen must never be reported as an outage.
type FieldRule struct {
	Path        []string
	Healthy     []string
	Progressing []string
	Unhealthy   []string
	Suspended   []string
}

// Evaluate implements Rule.
func (r FieldRule) Evaluate(obj map[string]any) (State, string) {
	v, found, err := unstructured.NestedString(obj, r.Path...)
	if err != nil || !found || v == "" {
		return StateUnknown, ""
	}
	label := shortReason(fieldLabel(r.Path) + "=" + v)
	switch {
	case contains(r.Healthy, v):
		return StateHealthy, ""
	case contains(r.Progressing, v):
		return StateProgressing, label
	case contains(r.Suspended, v):
		return StateSuspended, label
	case contains(r.Unhealthy, v):
		return StateUnhealthy, label
	}
	return StateUnknown, ""
}

// fieldLabel names the field for a report line: the path with a leading
// "status" element dropped, joined with dots — "robustness", "health.status".
func fieldLabel(path []string) string {
	if len(path) > 1 && path[0] == "status" {
		path = path[1:]
	}
	return strings.Join(path, ".")
}

// shortReason reduces a reason to one line of at most maxReasonRunes.
func shortReason(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return truncateRunes(strings.TrimSpace(s), maxReasonRunes)
}

// truncateRunes shortens s to max runes, marking that it cut. Local to this
// package on purpose: collect has an unexported twin, and collect imports
// operators — sharing it would invert the dependency.
func truncateRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
