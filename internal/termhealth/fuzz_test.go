package termhealth

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
)

// The namespaces, pods and claims are built here rather than in internal/fuzzgen
// because this is their only caller. fuzzgen owns the shapes several targets
// share and the cursor primitives every target draws from.

// fuzzBase is the clock Assess is handed, and the instant deletion timestamps are
// drawn around. Fixed, because a fuzz target that reads the wall clock is not
// reproducible.
var fuzzBase = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// finalizerNames are drawn rather than made hostile: metadata.finalizers is
// validated as a qualified name, so a control character cannot reach one.
var finalizerNames = []string{
	"kubernetes.io/pvc-protection",
	"foregroundDeletion",
	"example.com/cleanup",
}

// fuzzTerm draws the three resource lists Assess takes. A namespace condition
// message is the one value here the API server does not validate, so it is the
// one drawn from hostile bytes.
func fuzzTerm(c *fuzzgen.Cursor) ([]corev1.Namespace, []corev1.Pod, []corev1.PersistentVolumeClaim) {
	del := func() *metav1.Time {
		if c.Bool() {
			return nil
		}
		t := c.Time(fuzzBase.Add(-30 * 24 * time.Hour))
		return &t
	}
	finalizers := func() []string {
		var out []string
		for i := 0; i < c.IntN(3); i++ {
			out = append(out, c.Pick(finalizerNames))
		}
		return out
	}

	var nss []corev1.Namespace
	for i := 0; i < c.IntN(3); i++ {
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: c.Name(20), DeletionTimestamp: del()}}
		for k := 0; k < c.IntN(4); k++ {
			ns.Status.Conditions = append(ns.Status.Conditions, corev1.NamespaceCondition{
				Type: corev1.NamespaceConditionType(c.Pick([]string{
					"NamespaceDeletionContentFailure", "NamespaceContentRemaining",
					"NamespaceFinalizersRemaining", "NamespaceDeletionDiscoveryFailure",
				})),
				Status:  corev1.ConditionStatus(c.Pick([]string{"True", "False", "Unknown"})),
				Message: c.Hostile(96),
			})
		}
		for _, f := range finalizers() {
			ns.Spec.Finalizers = append(ns.Spec.Finalizers, corev1.FinalizerName(f))
		}
		nss = append(nss, ns)
	}

	claimName := c.Name(20)
	var pods []corev1.Pod
	for i := 0; i < c.IntN(3); i++ {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: c.Name(20), Name: c.Name(20),
			DeletionTimestamp: del(), Finalizers: finalizers(),
		}}
		if c.Bool() {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
				Name: c.Name(12),
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claimName,
				}},
			})
		}
		pods = append(pods, p)
	}

	var pvcs []corev1.PersistentVolumeClaim
	for i := 0; i < c.IntN(3); i++ {
		pvcs = append(pvcs, corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Namespace: c.Name(20), Name: c.Pick([]string{claimName, c.Name(20)}),
			DeletionTimestamp: del(), Finalizers: finalizers(),
		}})
	}
	return nss, pods, pvcs
}

// FuzzTermAssess feeds hostile namespace-condition text to a stuck-Terminating
// issue's reason, and checks the two bounded fields beside it: the kind, which is
// one of three kubeagent literals, and the age, which is a rendered duration.
func FuzzTermAssess(f *testing.F) {
	f.Add([]byte("seed"))
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte("\x1b[2J\x1b[H"))
	f.Add([]byte("\xff\xfe\xfd"))
	f.Add([]byte("‮​"))

	kinds := map[string]bool{"Namespace": true, "Pod": true, "PersistentVolumeClaim": true}

	f.Fuzz(func(t *testing.T, params []byte) {
		nss, pods, pvcs := fuzzTerm(fuzzgen.New(params))
		got := Assess(nss, pods, pvcs, nil, time.Minute, fuzzBase)

		for _, iss := range got {
			fuzzgen.AssertSafe(t, "issue.reason", iss.Reason)
			fuzzgen.AssertSafe(t, "issue.age", iss.Age)
			if !kinds[iss.Kind] {
				t.Errorf("Kind = %q, want one of this package's three literals", iss.Kind)
			}
			fuzzgen.AssertBounded(t, "issue.age", iss.Age, 24)
		}

		again := Assess(nss, pods, pvcs, nil, time.Minute, fuzzBase)
		if !reflect.DeepEqual(got, again) {
			t.Errorf("Assess is not deterministic")
		}
	})
}
