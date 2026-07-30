package diagnose

import (
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// evidenceBudget is the ceiling a composed Evidence field must stay under. The
// untrusted part is bounded by safetext.MaxLine; the extra 256 runes are the
// budget for kubeagent's own fixed prefix and a DNS-1123 container name (63
// characters, plus the quotes fmt %q adds). This catches unbounded growth — a
// field that carried a whole log or a whole event message — not an off-by-a-few.
const evidenceBudget = safetext.MaxLine + 256

// FuzzDetectors asserts four properties of the production detector set over
// arbitrary pods and events:
//
//	no panic     — Run returns for every input
//	purity       — the facts handed in are not mutated
//	determinism  — the same input yields the same findings
//	output safe  — every string field is printable and bounded
//
// The pod's identity is DNS-1123-valid because the API server validates it; the
// fields it does not validate (messages, reasons, field paths) are hostile. See
// internal/fuzzgen.
func FuzzDetectors(f *testing.F) {
	f.Add([]byte("crashloop"))
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte("\x1b[2J\x1b[H"))
	f.Add([]byte("\xff\xfe\xfd"))
	f.Add([]byte("Readiness probe failed: HTTP probe failed with statuscode: 503"))
	f.Add([]byte("Multi-Attach error for volume pvc-0"))
	f.Add([]byte("‮gnp.txt.exe"))

	f.Fuzz(func(t *testing.T, in []byte) {
		c := fuzzgen.New(in)
		pod := c.Pod()
		events := c.Events(pod, 4)
		facts := PodFacts{Pod: pod, Events: events}

		podBefore := pod.DeepCopy()
		// eventsBefore preserves nil-vs-empty: c.Events returns a nil slice when it
		// draws zero events, and reflect.DeepEqual treats nil and a non-nil
		// zero-length slice as unequal. Building eventsBefore unconditionally with
		// make() would make the purity check below fail on every such input,
		// regardless of whether any detector actually touched anything.
		var eventsBefore []corev1.Event
		if events != nil {
			eventsBefore = make([]corev1.Event, len(events))
			for i := range events {
				eventsBefore[i] = *events[i].DeepCopy()
			}
		}

		findings := Run(DefaultDetectors(fuzzgen.Base), []PodFacts{facts})

		if !reflect.DeepEqual(pod, podBefore) {
			t.Errorf("a detector mutated the pod it was handed; detectors must be pure")
		}
		if !reflect.DeepEqual(events, eventsBefore) {
			t.Errorf("a detector mutated the events it was handed; detectors must be pure")
		}

		again := Run(DefaultDetectors(fuzzgen.Base), []PodFacts{facts})
		if !reflect.DeepEqual(findings, again) {
			t.Errorf("the detector set is not deterministic:\nfirst:  %+v\nsecond: %+v", findings, again)
		}

		for i, fd := range findings {
			where := fmt.Sprintf("finding[%d]", i)
			fuzzgen.AssertSafe(t, where+".pod", fd.Pod)
			fuzzgen.AssertSafe(t, where+".issue", fd.Issue)
			fuzzgen.AssertSafe(t, where+".reason", fd.Reason)
			fuzzgen.AssertSafe(t, where+".evidence", fd.Evidence)
			fuzzgen.AssertSafe(t, where+".container", fd.Container)
			fuzzgen.AssertBounded(t, where+".evidence", fd.Evidence, evidenceBudget)
			fuzzgen.AssertBounded(t, where+".container", fd.Container, safetext.MaxLine)
			if fd.Resources != nil {
				fuzzgen.AssertSafe(t, where+".resources.container", fd.Resources.Container)
				fuzzgen.AssertSafe(t, where+".resources.memLimit", fd.Resources.MemLimit)
				fuzzgen.AssertSafe(t, where+".resources.cpuLimit", fd.Resources.CPULimit)
			}
		}
	})
}
