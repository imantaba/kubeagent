package gitops

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// condition finds one status condition and returns its status, its reason, and
// the time it last changed.
//
// The message is deliberately not returned. A reason is a CamelCase token by API
// convention; a message is arbitrary operator prose that routinely embeds URLs
// (a Flux clone failure quotes the repository URL verbatim), and a URL can carry
// a token.
func condition(obj map[string]any, condType string) (status, reason string, changed time.Time, found bool) {
	conds, ok, err := unstructured.NestedSlice(obj, "status", "conditions")
	if err != nil || !ok {
		return "", "", time.Time{}, false
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t != condType {
			continue
		}
		status, _, _ = unstructured.NestedString(m, "status")
		reason, _, _ = unstructured.NestedString(m, "reason")
		changed = parseTime(nestedString(m, "lastTransitionTime"))
		return status, reason, changed, true
	}
	return "", "", time.Time{}, false
}

// ageOf returns how long ago t was and whether the timestamp was usable at all.
// A stamp in the future is clock skew, not negative staleness.
func ageOf(t, now time.Time) (time.Duration, bool) {
	if t.IsZero() {
		return 0, false
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return d, true
}

// byAge classifies a difference by how long it has persisted. The threshold is
// exclusive — exactly at it is still converging — and an unknown age is never
// stale: a heuristic that cannot measure must not accuse.
func byAge(age time.Duration, known bool, threshold time.Duration) State {
	if !known {
		return StatePending
	}
	if age > threshold {
		return StateStale
	}
	return StatePending
}

// shortAge renders an elapsed duration the way the rest of the report does.
func shortAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// durText renders a configured threshold the way an operator would write it:
// 1h, 90m, 30s. Unlike shortAge it never rounds a value away.
func durText(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", int(d/time.Second))
	default:
		return d.String()
	}
}

// nestedString reads a string field, yielding "" for absent, wrong-typed, or
// malformed paths — all of which mean the same thing here: no signal.
func nestedString(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

// parseTime reads an RFC3339 API timestamp, yielding the zero time on anything
// unparseable so ageOf reports the age as unknown.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
