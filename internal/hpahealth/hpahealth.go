// Package hpahealth flags HorizontalPodAutoscalers that cannot scale as intended —
// one that can't fetch metrics, can't act on its scale target, or is pinned at
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
)

// Issue is one HorizontalPodAutoscaler that cannot scale as intended.
type Issue struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Target    string `json:"target"`   // "Deployment/api" from spec.scaleTargetRef
	Category  string `json:"category"` // "unable" | "metrics" | "capped"
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
				Target:    h.Spec.ScaleTargetRef.Kind + "/" + h.Spec.ScaleTargetRef.Name,
				Category:  cat,
				Reason:    reason,
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

// classify returns the first matching category (unable → metrics → capped) for an
// HPA, or ok=false when it is healthy/benign.
func classify(h autoscalingv2.HorizontalPodAutoscaler) (category, reason string, ok bool) {
	if c := condition(h, autoscalingv2.AbleToScale); c != nil && c.Status == corev1.ConditionFalse {
		return "unable", "can't scale — " + trimMsg(c.Message), true
	}
	if c := condition(h, autoscalingv2.ScalingActive); c != nil && c.Status == corev1.ConditionFalse {
		return "metrics", "can't fetch metrics — " + trimMsg(c.Message), true
	}
	// "TooManyReplicas" is the literal reason the upstream HPA controller sets on
	// ScalingLimited when it clamps the desired count down to maxReplicas.
	if c := condition(h, autoscalingv2.ScalingLimited); c != nil && c.Status == corev1.ConditionTrue && c.Reason == "TooManyReplicas" {
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
