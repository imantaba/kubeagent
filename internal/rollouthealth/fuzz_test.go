package rollouthealth

import (
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// The controller objects are built here rather than in internal/fuzzgen because
// this is their only caller. fuzzgen owns the shapes several targets share and
// the cursor primitives every target draws from; a builder with one consumer
// belongs beside that consumer.

// fuzzBase is the instant timestamps are drawn around. Fixed, because a fuzz
// target that reads the wall clock is not reproducible.
var fuzzBase = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// fuzzRollout draws a flagged workload of one of the three kinds and the
// controller object behind it. A Deployment's condition type, status and reason
// are drawn from the vocabularies the API server writes; its message is drawn
// from hostile bytes, because that field is not validated.
func fuzzRollout(c *fuzzgen.Cursor) ([]inventory.Workload, []appsv1.Deployment, []appsv1.StatefulSet, []appsv1.DaemonSet, []corev1.Pod) {
	ns, name := c.Name(20), c.Name(20)
	kind := c.Pick([]string{"Deployment", "StatefulSet", "DaemonSet"})
	desired := int32(c.IntN(6))
	ready := int32(c.IntN(6))
	ws := []inventory.Workload{{
		Namespace: ns, Name: name, Kind: kind,
		Desired: int(desired), Ready: int(ready), Status: c.Pick([]string{"Degraded", "Progressing", "Healthy"}),
	}}

	meta := metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: c.Time(fuzzBase)}
	var deps []appsv1.Deployment
	var stss []appsv1.StatefulSet
	var dss []appsv1.DaemonSet
	switch kind {
	case "Deployment":
		d := appsv1.Deployment{ObjectMeta: meta}
		for i := 0; i < c.IntN(4); i++ {
			d.Status.Conditions = append(d.Status.Conditions, appsv1.DeploymentCondition{
				Type:    appsv1.DeploymentConditionType(c.Pick([]string{"Progressing", "Available", "ReplicaFailure"})),
				Status:  corev1.ConditionStatus(c.Pick([]string{"True", "False", "Unknown"})),
				Reason:  c.Pick([]string{"ProgressDeadlineExceeded", "NewReplicaSetAvailable", "FailedCreate", ""}),
				Message: c.Hostile(96),
			})
		}
		d.Status.Replicas = desired
		d.Status.ReadyReplicas = ready
		d.Status.UpdatedReplicas = int32(c.IntN(6))
		deps = append(deps, d)
	case "StatefulSet":
		s := appsv1.StatefulSet{ObjectMeta: meta}
		s.Status.Replicas = desired
		s.Status.ReadyReplicas = ready
		s.Status.UpdatedReplicas = int32(c.IntN(6))
		s.Status.CurrentRevision = c.Name(12)
		s.Status.UpdateRevision = c.Name(12)
		stss = append(stss, s)
	default:
		d := appsv1.DaemonSet{ObjectMeta: meta}
		d.Status.DesiredNumberScheduled = desired
		d.Status.NumberReady = ready
		d.Status.UpdatedNumberScheduled = int32(c.IntN(6))
		dss = append(dss, d)
	}

	var pods []corev1.Pod
	for i := 0; i < c.IntN(3); i++ {
		pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: c.Name(20), CreationTimestamp: c.Time(fuzzBase),
		}})
	}
	return ws, deps, stss, dss, pods
}

// FuzzRolloutAnnotate feeds hostile Deployment condition text to the
// RolloutStuck finding, whose evidence is built from that message.
func FuzzRolloutAnnotate(f *testing.F) {
	f.Add([]byte("seed"))
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte("\x1b[2J\x1b[H"))
	f.Add([]byte("\xff\xfe\xfd"))
	f.Add([]byte("‮​"))

	f.Fuzz(func(t *testing.T, params []byte) {
		ws, deps, stss, dss, pods := fuzzRollout(fuzzgen.New(params))
		got := append([]inventory.Workload{}, ws...)
		Annotate(got, deps, stss, dss, pods, fuzzBase)

		for _, w := range got {
			for _, fnd := range w.Findings {
				fuzzgen.AssertSafe(t, "finding.reason", fnd.Reason)
				fuzzgen.AssertSafe(t, "finding.evidence", fnd.Evidence)
				if fnd.Issue != "RolloutStuck" {
					t.Errorf("Issue = %q, want RolloutStuck — this package emits one kind", fnd.Issue)
				}
			}
		}

		again := append([]inventory.Workload{}, ws...)
		Annotate(again, deps, stss, dss, pods, fuzzBase)
		if !reflect.DeepEqual(got, again) {
			t.Errorf("Annotate is not deterministic")
		}
	})
}
