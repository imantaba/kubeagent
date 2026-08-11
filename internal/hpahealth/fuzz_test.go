package hpahealth

import (
	"reflect"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// The HPA objects are built here rather than in internal/fuzzgen because this is
// their only caller. fuzzgen owns the shapes several targets share and the
// cursor primitives every target draws from.

// fuzzHPAs draws the autoscalers Assess takes. The HPA's own namespace and name
// are DNS-1123 labels, so they come from that alphabet; its scale-target kind and
// name are checked only with IsPathSegmentName and its condition messages are not
// checked at all, so all three come from hostile bytes.
func fuzzHPAs(c *fuzzgen.Cursor) []autoscalingv2.HorizontalPodAutoscaler {
	var out []autoscalingv2.HorizontalPodAutoscaler
	for i := 0; i < c.IntN(4); i++ {
		h := autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Namespace: c.Name(20), Name: c.Name(20)},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: c.Hostile(48),
					Name: c.Hostile(48),
				},
				MaxReplicas: c.Int32(),
			},
		}
		for k := 0; k < c.IntN(4); k++ {
			h.Status.Conditions = append(h.Status.Conditions, autoscalingv2.HorizontalPodAutoscalerCondition{
				Type:    autoscalingv2.HorizontalPodAutoscalerConditionType(c.Pick([]string{"AbleToScale", "ScalingActive", "ScalingLimited"})),
				Status:  corev1.ConditionStatus(c.Pick([]string{"True", "False", "Unknown"})),
				Reason:  c.Pick([]string{"TooManyReplicas", "FailedGetScale", "FailedGetResourceMetric", "DesiredWithinRange"}),
				Message: c.Hostile(96),
			})
		}
		out = append(out, h)
	}
	return out
}

// FuzzHPAAssess feeds hostile text to both fields an HPA issue can carry from the
// cluster: the reason, built from a condition message, and the target, built from
// a scale-target reference the API server barely validates.
func FuzzHPAAssess(f *testing.F) {
	f.Add([]byte("seed"))
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte("\x1b[2J\x1b[H"))
	f.Add([]byte("\xff\xfe\xfd"))
	f.Add([]byte("‮​"))

	categories := map[string]bool{"unable": true, "metrics": true, "capped": true}

	f.Fuzz(func(t *testing.T, params []byte) {
		hpas := fuzzHPAs(fuzzgen.New(params))
		got := Assess(hpas)

		for _, iss := range got {
			fuzzgen.AssertSafe(t, "issue.reason", iss.Reason)
			fuzzgen.AssertSafe(t, "issue.target", iss.Target)
			if !categories[iss.Category] {
				t.Errorf("Category = %q, want one of this package's three literals", iss.Category)
			}
			// The target's two halves are sanitized separately, so its budget is
			// two lines and a separator — bounded, just not by one line.
			fuzzgen.AssertBounded(t, "issue.target", iss.Target, 2*safetext.MaxLine+1)
		}

		again := Assess(hpas)
		if !reflect.DeepEqual(got, again) {
			t.Errorf("Assess is not deterministic")
		}
	})
}
