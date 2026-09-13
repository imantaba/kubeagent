// Package hypothesis re-checks the root-cause candidates a workload carries
// against the objects local verdict mode already read, and decides each
// candidate as confirmed, refuted or unverified with one evidence sentence.
//
// The package is pure. It imports only the standard library, k8s.io/api and
// internal/inventory; it holds no client and no context, issues no cluster
// call and makes no model call. No API text reaches an evidence sentence:
// every sentence is a fixed string, or a fixed string plus a literal from a
// closed list, or a fixed prefix plus a failed-read message the gather has
// already reduced and sanitized.
package hypothesis

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// Reads is what the gather read fresh, keyed the way the rules look it up.
// A key in Failed names a read that was attempted and did not return an
// object; its value is the reduced, sanitized error. A key absent from both
// the object map and Failed was never read — the budget was spent first.
type Reads struct {
	Nodes  map[string]*corev1.Node                  // by node name
	PVCs   map[string]*corev1.PersistentVolumeClaim // by "namespace/name"
	Events map[string][]corev1.Event                // by "namespace/pod"
	Failed map[string]string                        // "node/<name>", "pvc/<ns>/<name>", "events/<ns>/<pod>"
}

// Outcome is what a fresh read said about one candidate.
type Outcome string

const (
	Confirmed  Outcome = "confirmed"
	Refuted    Outcome = "refuted"
	Unverified Outcome = "unverified"
)

// Decision is one candidate's outcome and evidence sentence.
type Decision struct {
	Candidate inventory.Hypothesis
	Outcome   Outcome
	Evidence  string
}

// Result is one workload's row decision plus every candidate decision.
type Result struct {
	Workload  string // "namespace/name"
	Decided   bool   // a rule fixed the cause
	Cause     string // the deciding candidate's text, verbatim
	Outcome   Outcome
	Evidence  string
	GroupKey  string // set only when Decided && Outcome == Confirmed
	GroupText string
	Decisions []Decision // every non-ruled-out candidate, in trace order
}

// Decide re-checks every candidate the deterministic pass did not rule out,
// in trace order, and decides the row: the first confirmed candidate wins;
// with none, the first unverified candidate wins; with only refuted
// candidates the row is left to the model.
func Decide(w inventory.Workload, reads Reads) Result {
	r := Result{Workload: w.Namespace + "/" + w.Name}
	for _, h := range w.RootCauseTrace {
		if h.Verdict == inventory.VerdictRuledOut {
			continue
		}
		outcome, evidence := check(w, h, reads)
		r.Decisions = append(r.Decisions, Decision{Candidate: h, Outcome: outcome, Evidence: evidence})
	}
	for _, d := range r.Decisions {
		if d.Outcome == Confirmed {
			return decided(r, d, w, reads)
		}
	}
	for _, d := range r.Decisions {
		if d.Outcome == Unverified {
			return decided(r, d, w, reads)
		}
	}
	return r
}

// decided fills the row from the deciding candidate. Only a confirmed row
// joins a group: an unverified cause is a guess and must not be counted
// as shared.
func decided(r Result, d Decision, w inventory.Workload, reads Reads) Result {
	r.Decided = true
	r.Cause = d.Candidate.Cause
	r.Outcome = d.Outcome
	r.Evidence = d.Evidence
	if d.Outcome == Confirmed {
		r.GroupKey, r.GroupText = group(w, d.Candidate, reads)
	}
	return r
}
