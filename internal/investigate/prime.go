package investigate

import (
	"fmt"
	"strings"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// renderTrace renders each flagged workload's deterministic root-cause
// hypothesis trace for the investigation's opening message, or "" when no
// workload carries one. It lives here and not in internal/explain on purpose:
// the trace names nodes, and --explain's payload deliberately excludes node
// names, while --investigate already surfaces node names through its tools —
// the two egress boundaries differ and must stay separate.
// explain.BuildInventoryPrompt and --explain's payload are unchanged.
func renderTrace(workloads []inventory.Workload) string {
	var b strings.Builder
	for _, w := range workloads {
		if len(w.RootCauseTrace) == 0 {
			continue
		}
		fmt.Fprintf(&b, "- %s/%s (%s)", w.Namespace, w.Name, w.Kind)
		if w.RootCauseConfidence != "" {
			fmt.Fprintf(&b, " [confidence: %s]", w.RootCauseConfidence)
		}
		b.WriteString(":\n")
		for _, h := range w.RootCauseTrace {
			fmt.Fprintf(&b, "    considered %s: %s — %s\n",
				h.Cause, strings.ReplaceAll(string(h.Verdict), "_", " "), h.Reason)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\nThe deterministic pass already evaluated these root-cause hypotheses:\n" +
		b.String() +
		"\nVerify each attributed cause with the tools before relying on it, and spend the rest of the budget on what the deterministic pass could not explain — the workloads with no attributed cause and the findings behind the ruled-out candidates."
}
