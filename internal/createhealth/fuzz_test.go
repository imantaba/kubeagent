package createhealth

import (
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// The controller objects and events are built here rather than in
// internal/fuzzgen because this is their only caller. fuzzgen owns the shapes
// several targets share and the cursor primitives every target draws from.

// fuzzBase is the instant timestamps are drawn around. Fixed, because a fuzz
// target that reads the wall clock is not reproducible.
var fuzzBase = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// fuzzCreate draws a workload short of its pods, the ReplicaSet that may back
// it, and the events that may name either. An event's message is drawn from
// hostile bytes; its reason is drawn from the vocabulary the API server writes,
// because "FailedCreate" is the literal this package matches on.
func fuzzCreate(c *fuzzgen.Cursor) ([]inventory.Workload, []appsv1.ReplicaSet, []corev1.Event) {
	ns, name := c.Name(20), c.Name(20)
	kind := c.Pick([]string{"Deployment", "StatefulSet", "DaemonSet"})
	ws := []inventory.Workload{{
		Namespace: ns, Name: name, Kind: kind,
		Desired: c.IntN(6), Ready: c.IntN(6),
		Status: c.Pick([]string{"Degraded", "Progressing", "Healthy"}),
	}}

	rsName := c.Name(20)
	var rss []appsv1.ReplicaSet
	if kind == "Deployment" {
		ctrl := true
		rss = append(rss, appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: rsName,
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: name, Controller: &ctrl}},
		}})
	}

	var evs []corev1.Event
	for i := 0; i < c.IntN(4); i++ {
		evs = append(evs, corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: ns, Name: c.Name(20)},
			Reason:        c.Pick([]string{"FailedCreate", "SuccessfulCreate", "Scheduled"}),
			Type:          "Warning",
			Message:       c.Hostile(128),
			LastTimestamp: c.Time(fuzzBase),
			InvolvedObject: corev1.ObjectReference{
				Kind:      c.Pick([]string{"ReplicaSet", "StatefulSet", "DaemonSet"}),
				Namespace: ns,
				Name:      c.Pick([]string{rsName, name}),
			},
		})
	}
	return ws, rss, evs
}

// FuzzCreateAnnotate feeds hostile FailedCreate event text to the finding. The
// reason is asserted too: it is chosen by a classifier that reads the raw
// message, so a bug there would put the message into a field that travels into
// gate's verdict JSON.
func FuzzCreateAnnotate(f *testing.F) {
	f.Add([]byte("seed"))
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte("\x1b[2J\x1b[H"))
	f.Add([]byte("\xff\xfe\xfd"))
	f.Add([]byte("‮​"))

	f.Fuzz(func(t *testing.T, params []byte) {
		ws, rss, evs := fuzzCreate(fuzzgen.New(params))
		got := append([]inventory.Workload{}, ws...)
		Annotate(got, rss, evs)

		for _, w := range got {
			for _, fnd := range w.Findings {
				fuzzgen.AssertSafe(t, "finding.reason", fnd.Reason)
				fuzzgen.AssertSafe(t, "finding.evidence", fnd.Evidence)
				// The evidence is exactly one sanitized value, so unlike a
				// composed field it must fit inside one line's budget.
				fuzzgen.AssertBounded(t, "finding.evidence", fnd.Evidence, safetext.MaxLine)
				if fnd.Issue != "FailedCreate" {
					t.Errorf("Issue = %q, want FailedCreate — this package emits one kind", fnd.Issue)
				}
			}
		}

		again := append([]inventory.Workload{}, ws...)
		Annotate(again, rss, evs)
		if !reflect.DeepEqual(got, again) {
			t.Errorf("Annotate is not deterministic")
		}
	})
}
