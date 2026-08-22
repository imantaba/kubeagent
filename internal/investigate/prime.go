package investigate

import (
	"fmt"
	"strings"

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

// writeWorkloadTrace writes one workload's candidate lines. limit 0 means
// unlimited; a positive limit cuts after that many entries and marks the cut.
func writeWorkloadTrace(b *strings.Builder, w inventory.Workload, limit int) {
	if len(w.RootCauseTrace) == 0 {
		return
	}
	fmt.Fprintf(b, "- %s/%s (%s)", w.Namespace, w.Name, w.Kind)
	if w.RootCauseConfidence != "" {
		fmt.Fprintf(b, " [confidence: %s]", w.RootCauseConfidence)
	}
	b.WriteString(":\n")
	for i, h := range w.RootCauseTrace {
		if limit > 0 && i == limit {
			b.WriteString("    " + truncationMarker + "\n")
			break
		}
		fmt.Fprintf(b, "    considered %s: %s — %s\n",
			h.Cause, strings.ReplaceAll(string(h.Verdict), "_", " "), h.Reason)
	}
}

// renderCandidates renders the same per-workload candidate lines for local
// verdict mode's prompt — capped, and without renderTrace's wrapper, whose
// "verify with the tools" instruction would be false in a mode with no
// tools. "" when no workload carries a trace.
func renderCandidates(workloads []inventory.Workload) string {
	var b strings.Builder
	for _, w := range workloads {
		writeWorkloadTrace(&b, w, maxCandidatesPerWorkload)
	}
	return b.String()
}
