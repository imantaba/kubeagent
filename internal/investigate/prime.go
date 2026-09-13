package investigate

import (
	"fmt"
	"strings"

	"github.com/imantaba/kubeagent/internal/hypothesis"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// renderTrace renders each flagged workload's deterministic root-cause
// hypothesis trace for the investigation's opening message, or "" when no
// workload carries one. It lives here and not in internal/explain on purpose:
// explain.BuildInventoryPrompt, the base prompt both flags share, already
// names a node in its degraded-cluster section when the cluster is
// Degraded. What the trace adds is a per-workload attribution tying a
// named node to a named workload as its cause, and the tool loop adds
// direct node reads (describe on a node, get_related's node hop) on top —
// exposure --explain has no equivalent of, which is why the two egress
// boundaries stay separate.
// explain.BuildInventoryPrompt and --explain's payload are unchanged.
func renderTrace(workloads []inventory.Workload) string {
	var b strings.Builder
	for _, w := range workloads {
		writeWorkloadTrace(&b, w, 0)
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\nThe deterministic pass already evaluated these root-cause hypotheses:\n" +
		b.String() +
		"\nVerify each attributed cause with the tools before relying on it, and spend the rest of the budget on what the deterministic pass could not explain — the workloads with no attributed cause and the findings behind the ruled-out candidates."
}

// maxCandidatesPerWorkload bounds how many trace entries local verdict
// mode's prompt shows per workload; renderTrace (the tool loop's primer)
// passes 0 and stays unlimited.
const maxCandidatesPerWorkload = 8

// writeWorkloadTrace writes one workload's candidate lines for the tool
// loop's primer. limit 0 means unlimited; a positive limit cuts after that
// many entries and marks the cut.
func writeWorkloadTrace(b *strings.Builder, w inventory.Workload, limit int) {
	if len(w.RootCauseTrace) == 0 {
		return
	}
	writeTraceHeading(b, w)
	for i, h := range w.RootCauseTrace {
		if limit > 0 && i == limit {
			b.WriteString("    " + truncationMarker + "\n")
			break
		}
		writeCandidateLine(b, h)
	}
}

// writeTraceHeading writes the "- ns/name (Kind) [confidence: x]:" line.
func writeTraceHeading(b *strings.Builder, w inventory.Workload) {
	fmt.Fprintf(b, "- %s/%s (%s)", w.Namespace, w.Name, w.Kind)
	if w.RootCauseConfidence != "" {
		fmt.Fprintf(b, " [confidence: %s]", w.RootCauseConfidence)
	}
	b.WriteString(":\n")
}

// writeCandidateLine writes one "considered …" line.
func writeCandidateLine(b *strings.Builder, h inventory.Hypothesis) {
	fmt.Fprintf(b, "    considered %s: %s — %s\n",
		h.Cause, strings.ReplaceAll(string(h.Verdict), "_", " "), h.Reason)
}

// writeWorkloadCandidates writes one workload's candidate lines for local
// verdict mode, capped at maxCandidatesPerWorkload. When r is not nil, each
// shown candidate the rules re-checked is followed by its fresh-read line,
// and a decided workload ends with the decided-by-rules line. r.Decisions
// holds one entry per non-ruled-out candidate in trace order, so a cursor
// over it stays aligned with the trace.
// Decide walks the whole trace, so the decided-by-rules line may name a
// candidate that sits past the cap and was not shown.
func writeWorkloadCandidates(b *strings.Builder, w inventory.Workload, r *hypothesis.Result) {
	if len(w.RootCauseTrace) == 0 {
		return
	}
	writeTraceHeading(b, w)
	next := 0
	for i, h := range w.RootCauseTrace {
		if i == maxCandidatesPerWorkload {
			b.WriteString("    " + truncationMarker + "\n")
			break
		}
		writeCandidateLine(b, h)
		if h.Verdict == inventory.VerdictRuledOut {
			continue
		}
		if r != nil && next < len(r.Decisions) {
			d := r.Decisions[next]
			fmt.Fprintf(b, "      fresh read: %s — %s\n", d.Outcome, d.Evidence)
		}
		next++
	}
	if r != nil && r.Decided {
		fmt.Fprintf(b, "    decided by rules: %s — %s\n", r.Cause, r.Outcome)
	}
}

// renderCandidates renders the per-workload candidate lines for local
// verdict mode's prompt — capped, and without renderTrace's wrapper, whose
// "verify with the tools" instruction would be false in a mode with no
// tools. results[i] is scoped[i]'s rule result; a nil or short results
// slice renders the candidate lines alone. "" when no workload carries a
// trace.
func renderCandidates(scoped []inventory.Workload, results []hypothesis.Result) string {
	var b strings.Builder
	for i, w := range scoped {
		var r *hypothesis.Result
		if i < len(results) {
			r = &results[i]
		}
		writeWorkloadCandidates(&b, w, r)
	}
	return b.String()
}
