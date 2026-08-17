package hpahealth

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

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

func TestAssess_ScalingDisabled(t *testing.T) {
	h := hpa("shop", "batch-hpa", "Deployment", "batch", 6,
		cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "ScalingDisabled", "scaling is disabled since the replica count of the target is zero"))
	is, ok := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "batch-hpa")
	if !ok || is.Category != "disabled" {
		t.Fatalf("want disabled, got %+v", is)
	}
	if is.Reason != "scaling is disabled — scaling is disabled since the replica count of the target is zero" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_AmbiguousSelector(t *testing.T) {
	h := hpa("shop", "dup-hpa", "Deployment", "dup", 6,
		cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "AmbiguousSelector", "selector overlaps with hpa other-hpa"))
	is, ok := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "dup-hpa")
	if !ok || is.Category != "ambiguous" {
		t.Fatalf("want ambiguous, got %+v", is)
	}
	if is.Reason != "two HPAs target the same pods — selector overlaps with hpa other-hpa" {
		t.Errorf("reason = %q", is.Reason)
	}
}

// TestAssess_UnknownScalingActiveReasonStaysMetrics pins the default arm: a
// reason the table does not name must keep the pre-existing "metrics"
// behaviour exactly, so a future edit that widens the table by accident is
// caught here.
func TestAssess_UnknownScalingActiveReasonStaysMetrics(t *testing.T) {
	h := hpa("shop", "weird-hpa", "Deployment", "weird", 6,
		cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "SomeFutureControllerReason", "a reason this table does not name"))
	is, ok := find(Assess([]autoscalingv2.HorizontalPodAutoscaler{h}), "weird-hpa")
	if !ok || is.Category != "metrics" {
		t.Fatalf("want metrics (the default), got %+v", is)
	}
	if is.Reason != "can't fetch metrics — a reason this table does not name" {
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

// hostileMsg carries what a condition message may contain but a terminal must
// never receive: an ANSI escape that clears the screen, a right-to-left override
// that reorders everything after it, a NUL, and an invalid UTF-8 byte.
const hostileMsg = "no metrics\x1b[2J‮gnp\x00 for \xff target"

// A condition message is free text the API server does not validate, and this
// one lands in the Issue's Reason — which travels into findings.Finding.Reason,
// gate's verdict JSON and the SARIF a pipeline uploads.
func TestAssess_SanitizesAConditionMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		cond autoscalingv2.HorizontalPodAutoscalerCondition
	}{
		{"AbleToScale", cond(autoscalingv2.AbleToScale, corev1.ConditionFalse, "FailedGetScale", hostileMsg)},
		{"ScalingActive", cond(autoscalingv2.ScalingActive, corev1.ConditionFalse, "FailedGetResourceMetric", hostileMsg)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Assess([]autoscalingv2.HorizontalPodAutoscaler{hpa("shop", "web", "Deployment", "web", 10, tc.cond)})
			if len(got) != 1 {
				t.Fatalf("want one issue, got %d", len(got))
			}
			assertSanitized(t, "reason", got[0].Reason)
		})
	}
}

// An HPA's scale-target reference is not a DNS name. The API server validates
// spec.scaleTargetRef.kind and .name with IsPathSegmentName, which refuses only
// ".", "..", and a value containing "/" or "%" — a control character, invalid
// UTF-8 and an unbounded length all pass it. The two are composed into the
// Issue's Target, which the text renderer prints and scan's JSON document
// carries, so this is the point at which they become kubeagent values.
func TestAssess_SanitizesTheScaleTargetReference(t *testing.T) {
	h := hpa("shop", "web", "Deploy\x1b[2Jment", "we\x00b\xff‮gnp", 10,
		cond(autoscalingv2.AbleToScale, corev1.ConditionFalse, "FailedGetScale", "the scale target was not found"))
	got := Assess([]autoscalingv2.HorizontalPodAutoscaler{h})
	if len(got) != 1 {
		t.Fatalf("want one issue, got %d", len(got))
	}
	assertSanitized(t, "target", got[0].Target)
	// The two halves are sanitized separately, so the separator survives and a
	// long kind cannot push the name out of one line's budget.
	if !strings.Contains(got[0].Target, "/") {
		t.Errorf("target = %q, want the kind and name still separated", got[0].Target)
	}
}

// assertSanitized fails when s carries anything safetext.Line removes. It is
// deliberately not "s == safetext.Line(raw)": the point is what reaches the
// terminal, not which helper produced it.
func assertSanitized(t *testing.T, where, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("invalid UTF-8 reached the %s: %q", where, s)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Errorf("control or formatting character %U reached the %s: %q", r, where, s)
		}
	}
}
