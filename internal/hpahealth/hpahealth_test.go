package hpahealth

import (
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cond(t autoscalingv2.HorizontalPodAutoscalerConditionType, s corev1.ConditionStatus, reason, msg string) autoscalingv2.HorizontalPodAutoscalerCondition {
	return autoscalingv2.HorizontalPodAutoscalerCondition{Type: t, Status: s, Reason: reason, Message: msg}
}

func hpa(ns, name, kind, target string, maxReplicas int32, conds ...autoscalingv2.HorizontalPodAutoscalerCondition) autoscalingv2.HorizontalPodAutoscaler {
	return autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: kind, Name: target},
			MaxReplicas:    maxReplicas,
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{Conditions: conds},
	}
}

func find(issues []Issue, name string) (Issue, bool) {
	for _, i := range issues {
		if i.Name == name {
			return i, true
		}
	}
	return Issue{}, false
}

func TestAssess_Unable(t *testing.T) {
	h := hpa("shop", "worker-hpa", "Deployment", "worker", 5,
		cond(autoscalingv2.AbleToScale, corev1.ConditionFalse, "FailedGetScale", "the scale target Deployment/worker was not found"))
	is, ok := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "worker-hpa")
	if !ok || is.Category != "unable" {
		t.Fatalf("want unable, got %+v", is)
	}
	if is.Target != "Deployment/worker" {
		t.Errorf("target = %q, want Deployment/worker", is.Target)
	}
	if is.Reason != "can't scale — the scale target Deployment/worker was not found" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_Metrics(t *testing.T) {
	// trailing period must be trimmed.
	h := hpa("shop", "api-hpa", "Deployment", "api", 8,
		cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "FailedGetResourceMetric", "unable to get resource metric cpu: no metrics returned."))
	is, ok := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "api-hpa")
	if !ok || is.Category != "metrics" {
		t.Fatalf("want metrics, got %+v", is)
	}
	if is.Reason != "can't fetch metrics — unable to get resource metric cpu: no metrics returned" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_Capped(t *testing.T) {
	h := hpa("ops", "ingest-hpa", "Deployment", "ingest", 10,
		cond(autoscalingv2.ScalingLimited, corev1.ConditionTrue, "TooManyReplicas", "the desired replica count is more than the maximum replica count"))
	is, ok := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "ingest-hpa")
	if !ok || is.Category != "capped" {
		t.Fatalf("want capped, got %+v", is)
	}
	if is.Reason != "pinned at maxReplicas 10 — desired exceeds the cap" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_UnableBeatsMetrics(t *testing.T) {
	h := hpa("a", "both", "Deployment", "x", 3,
		cond(autoscalingv2.AbleToScale, corev1.ConditionFalse, "FailedGetScale", "no scale"),
		cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "FailedGetResourceMetric", "no metric"))
	if is, _ := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "both"); is.Category != "unable" {
		t.Fatalf("unable must win precedence, got %+v", is)
	}
}

func TestAssess_NotFlagged(t *testing.T) {
	cases := []autoscalingv2.HorizontalPodAutoscaler{
		hpa("a", "healthy", "Deployment", "h", 5,
			cond(autoscalingv2.AbleToScale, corev1.ConditionTrue, "ReadyForNewScale", ""),
			cond(autoscalingv2.ScalingActive, corev1.ConditionTrue, "ValidMetricFound", ""),
			cond(autoscalingv2.ScalingLimited, corev1.ConditionFalse, "DesiredWithinRange", "")),
		hpa("a", "atfloor", "Deployment", "f", 5,
			cond(autoscalingv2.ScalingLimited, corev1.ConditionTrue, "TooFewReplicas", "")), // idle at min → benign
		hpa("a", "fresh", "Deployment", "n", 5), // no conditions yet
	}
	if got := Assess(cases); len(got) != 0 {
		t.Fatalf("expected nothing flagged, got %+v", got)
	}
}

func TestAssess_SortedByNamespaceName(t *testing.T) {
	mk := func(ns, name string) autoscalingv2.HorizontalPodAutoscaler {
		return hpa(ns, name, "Deployment", "d", 3,
			cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "FailedGetResourceMetric", "no metric"))
	}
	got := Assess([]autoscalingv2.HorizontalPodAutoscaler{mk("b", "z"), mk("a", "y"), mk("a", "x")})
	if len(got) != 3 || got[0].Name != "x" || got[1].Name != "y" || got[2].Name != "z" {
		t.Fatalf("not sorted by (ns,name): %+v", got)
	}
}

func TestAssess_EmptyMessageNoDanglingDash(t *testing.T) {
	h := hpa("a", "nomsg", "Deployment", "x", 3,
		cond(autoscalingv2.AbleToScale, corev1.ConditionFalse, "FailedGetScale", ""))
	is, ok := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "nomsg")
	if !ok || is.Reason != "can't scale" {
		t.Fatalf(`empty message must yield "can't scale" with no trailing dash, got %q`, is.Reason)
	}
}

func TestAssess_MetricsBeatsCapped(t *testing.T) {
	h := hpa("a", "mc", "Deployment", "x", 4,
		cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "FailedGetResourceMetric", "no metric"),
		cond(autoscalingv2.ScalingLimited, corev1.ConditionTrue, "TooManyReplicas", "clamped"))
	if is, _ := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "mc"); is.Category != "metrics" {
		t.Fatalf("metrics must win over capped, got %+v", is)
	}
}
