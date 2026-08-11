package batchhealth

import (
	"reflect"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// The Job objects are built here rather than in internal/fuzzgen because this
// is their only caller. fuzzgen owns the shapes several targets share — a pod,
// its events, a TLS secret — and the cursor primitives every target draws from;
// a builder with one consumer belongs beside that consumer.

// fuzzBase is the instant creation timestamps are drawn around. Fixed, because
// a fuzz target that reads the wall clock is not reproducible.
var fuzzBase = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// fuzzJobs draws a workload and the Jobs that back it. A Job's condition type,
// status, reason and message are all drawn: the reason and message from hostile
// bytes, because the API server validates neither.
func fuzzJobs(c *fuzzgen.Cursor) ([]inventory.Workload, []batchv1.Job) {
	ns, name := c.Name(20), c.Name(20)
	kind := c.Pick([]string{"Job", "CronJob"})
	ws := []inventory.Workload{{Namespace: ns, Name: name, Kind: kind}}

	jobs := make([]batchv1.Job, 0, 3)
	for i := 0; i < c.IntN(4); i++ {
		j := batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              c.Name(20),
			CreationTimestamp: c.Time(fuzzBase),
		}}
		if kind == "CronJob" && c.Bool() {
			ctrl := true
			j.OwnerReferences = []metav1.OwnerReference{{Kind: "CronJob", Name: name, Controller: &ctrl}}
		}
		for k := 0; k < c.IntN(3); k++ {
			j.Status.Conditions = append(j.Status.Conditions, batchv1.JobCondition{
				Type:    batchv1.JobConditionType(c.Pick([]string{"Failed", "Complete", "Suspended"})),
				Status:  corev1.ConditionStatus(c.Pick([]string{"True", "False", "Unknown"})),
				Reason:  c.Hostile(48),
				Message: c.Hostile(96),
			})
		}
		jobs = append(jobs, j)
	}
	return ws, jobs
}

// FuzzBatchAnnotate feeds hostile Job condition text to the JobFailed finding.
// Both fields it can reach are asserted: the evidence, which is the message,
// and the reason, which carries an unrecognised failure reason verbatim.
func FuzzBatchAnnotate(f *testing.F) {
	f.Add([]byte("seed"))
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte("\x1b[2J\x1b[H"))
	f.Add([]byte("\xff\xfe\xfd"))
	f.Add([]byte("‮​"))

	f.Fuzz(func(t *testing.T, params []byte) {
		ws, jobs := fuzzJobs(fuzzgen.New(params))
		got := append([]inventory.Workload{}, ws...)
		Annotate(got, jobs)

		for _, w := range got {
			for _, fnd := range w.Findings {
				fuzzgen.AssertSafe(t, "finding.reason", fnd.Reason)
				fuzzgen.AssertSafe(t, "finding.evidence", fnd.Evidence)
				if fnd.Issue != "JobFailed" {
					t.Errorf("Issue = %q, want JobFailed — this package emits one kind", fnd.Issue)
				}
			}
		}

		again := append([]inventory.Workload{}, ws...)
		Annotate(again, jobs)
		if !reflect.DeepEqual(got, again) {
			t.Errorf("Annotate is not deterministic")
		}
	})
}
