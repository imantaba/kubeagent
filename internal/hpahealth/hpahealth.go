// Package hpahealth flags HorizontalPodAutoscalers that cannot scale as intended —
// one that can't fetch metrics, can't act on its scale target, has scaling
// disabled, targets pods another HPA already claims, or is pinned at
// maxReplicas while demand exceeds the cap — and names why. Pure and read-only:
// the caller supplies the HPA objects; every signal comes from the HPA's own spec
// and status conditions. Advisory (never affects the cluster verdict).
package hpahealth

import (
	"fmt"
	"sort"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// Issue is one HorizontalPodAutoscaler that cannot scale as intended.
type Issue struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Target    string `json:"target"`   // "Deployment/api" from spec.scaleTargetRef
	Category  string `json:"category"` // "unable" | "metrics" | "disabled" | "ambiguous" | "capped"
	Reason    string `json:"reason"`
}

// Assess flags HPAs that cannot scale as intended, sorted by (Namespace, Name).
// A healthy HPA, one limited only at its floor, or a freshly-created HPA with no
// conditions yet, is not flagged.
func Assess(hpas []autoscalingv2.HorizontalPodAutoscaler) []Issue {
	var out []Issue
	for _, h := range hpas {
		if cat, reason, ok := classify(h); ok {
			out = append(out, Issue{
				Namespace: h.Namespace,
				Name:      h.Name,
				// The HPA's own namespace and name are DNS-1123 labels the API
				// server validates, but its scale-target reference is not: kind
				// and name are checked with IsPathSegmentName, which refuses
				// only ".", "..", and a value containing "/" or "%". A control
				// character, invalid UTF-8 and an unbounded length all pass, so
				// this is where those two become kubeagent values. They are
				// sanitized separately rather than after joining, so a long kind
				// cannot push the name out of one line's budget.
				Target: safetext.Line(h.Spec.ScaleTargetRef.Kind) + "/" +
					safetext.Line(h.Spec.ScaleTargetRef.Name),
				Category: cat,
				Reason:   reason,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// reason joins a category prefix with a condition message, omitting the "— <msg>"
// suffix when the message is empty so the reason never ends in a dangling em dash.
func reason(prefix, msg string) string {
	if m := trimMsg(msg); m != "" {
		return prefix + " — " + m
	}
	return prefix
}

// classify returns the first matching category (unable → ScalingActive →
// capped) for an HPA, or ok=false when it is healthy/benign. disabled and
// ambiguous are not new precedence steps: they are sub-cases of the
// ScalingActive step, distinguished by the condition's own reason. Every
// ScalingActive reason this file does not name still falls through to
// metrics, exactly as it did before those two sub-cases existed.
func classify(h autoscalingv2.HorizontalPodAutoscaler) (category, msg string, ok bool) {
	if c := condition(h, autoscalingv2.AbleToScale); c != nil && c.Status == corev1.ConditionFalse {
		return "unable", reason("can't scale", safetext.Line(c.Message)), true
	}
	if c := condition(h, autoscalingv2.ScalingActive); c != nil && c.Status == corev1.ConditionFalse {
		// The reason comparison is a matching decision, so — like the
		// TooManyReplicas comparison below — it runs on the raw c.Reason: a
		// control character spliced into the reason must not make it stop
		// matching.
		switch c.Reason {
		case "ScalingDisabled":
			return "disabled", reason("scaling is disabled", safetext.Line(c.Message)), true
		case "AmbiguousSelector":
			return "ambiguous", reason("two HPAs target the same pods", safetext.Line(c.Message)), true
		default:
			return "metrics", reason("can't fetch metrics", safetext.Line(c.Message)), true
		}
	}
	// "TooManyReplicas" is the literal reason the upstream HPA controller sets on
	// ScalingLimited when it clamps the desired count down to maxReplicas. The
	// comparison is a matching decision, so it runs on the raw value — a control
	// character spliced into the reason must not make it stop matching.
	if c := condition(h, autoscalingv2.ScalingLimited); c != nil && c.Status == corev1.ConditionTrue && c.Reason == "TooManyReplicas" {
		if h.Status.CurrentReplicas < h.Spec.MaxReplicas {
			return "capped", fmt.Sprintf("at %d of maxReplicas %d — desired exceeds the cap", h.Status.CurrentReplicas, h.Spec.MaxReplicas), true
		}
		return "capped", fmt.Sprintf("pinned at maxReplicas %d — desired exceeds the cap", h.Spec.MaxReplicas), true
	}
	return "", "", false
}

// condition returns the HPA's condition of the given type, or nil if absent.
func condition(h autoscalingv2.HorizontalPodAutoscaler, t autoscalingv2.HorizontalPodAutoscalerConditionType) *autoscalingv2.HorizontalPodAutoscalerCondition {
	for i := range h.Status.Conditions {
		if h.Status.Conditions[i].Type == t {
			return &h.Status.Conditions[i]
		}
	}
	return nil
}

// trimMsg drops trailing period/whitespace from a condition message.
func trimMsg(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), ". ")
}
