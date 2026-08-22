package investigate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/inventory"
)

func TestRenderTraceBytesPinned(t *testing.T) {
	w := inventory.Workload{Namespace: "shop", Name: "web", Kind: "Deployment", RootCauseConfidence: "high",
		RootCauseTrace: []inventory.Hypothesis{
			{Cause: "node worker-1 (NotReady)", Verdict: inventory.VerdictAttributed, Reason: "pod web-abc is scheduled on it"},
			{Cause: "registry ghcr.io", Verdict: inventory.VerdictRuledOut, Reason: "only workload failing to pull from this host; threshold is 2"},
		}}
	got := renderTrace([]inventory.Workload{w})
	want := "\n\nThe deterministic pass already evaluated these root-cause hypotheses:\n" +
		"- shop/web (Deployment) [confidence: high]:\n" +
		"    considered node worker-1 (NotReady): attributed — pod web-abc is scheduled on it\n" +
		"    considered registry ghcr.io: ruled out — only workload failing to pull from this host; threshold is 2\n" +
		"\nVerify each attributed cause with the tools before relying on it, and spend the rest of the budget on what the deterministic pass could not explain — the workloads with no attributed cause and the findings behind the ruled-out candidates."
	if got != want {
		t.Errorf("renderTrace bytes moved:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestRenderTrace_EmptyWhenNoWorkloadCarriesATrace(t *testing.T) {
	workloads := []inventory.Workload{
		{Kind: "Deployment", Namespace: "shop", Name: "web"},
	}
	if got := renderTrace(workloads); got != "" {
		t.Errorf("want empty string for traceless workloads, got %q", got)
	}
	if got := renderTrace(nil); got != "" {
		t.Errorf("want empty string for nil workloads, got %q", got)
	}
}

func TestRenderTrace_RendersEveryHypothesisWithSpacedVerdicts(t *testing.T) {
	workloads := []inventory.Workload{
		{
			Kind: "Deployment", Namespace: "shop", Name: "web",
			RootCauseConfidence: "high",
			RootCauseTrace: []inventory.Hypothesis{
				{Cause: "node worker-3 (NotReady)", Kind: "node", Verdict: inventory.VerdictAttributed, Reason: "pod web-abc is scheduled on it"},
				{Cause: "PVC data-0 (ProvisioningFailed)", Kind: "pvc", Verdict: inventory.VerdictRuledOut, Reason: "not mounted by this workload's pods"},
				{Cause: "registry registry.example.com", Kind: "registry", Verdict: inventory.VerdictOutranked, Reason: "node worker-3 (NotReady) is the stronger cause"},
			},
		},
	}
	got := renderTrace(workloads)
	for _, want := range []string{
		"- shop/web (Deployment) [confidence: high]:",
		"considered node worker-3 (NotReady): attributed — pod web-abc is scheduled on it",
		"considered PVC data-0 (ProvisioningFailed): ruled out — not mounted by this workload's pods",
		"considered registry registry.example.com: outranked — node worker-3 (NotReady) is the stronger cause",
		"Verify each attributed cause",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trace missing %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ruled_out") {
		t.Errorf("verdict underscores must render as spaces, got:\n%s", got)
	}
}

func TestRenderTrace_SkipsTracelessWorkloadsButKeepsOthers(t *testing.T) {
	workloads := []inventory.Workload{
		{Kind: "Deployment", Namespace: "shop", Name: "healthy"},
		{
			Kind: "StatefulSet", Namespace: "shop", Name: "db",
			RootCauseTrace: []inventory.Hypothesis{
				{Cause: "node worker-1 (NotReady)", Kind: "node", Verdict: inventory.VerdictAttributed, Reason: "pod db-0 is scheduled on it"},
			},
		},
	}
	got := renderTrace(workloads)
	if !strings.Contains(got, "- shop/db (StatefulSet):") {
		t.Errorf("traced workload missing, got:\n%s", got)
	}
	if strings.Contains(got, "healthy") {
		t.Errorf("traceless workload must not appear, got:\n%s", got)
	}
}

func TestRenderCandidatesCapsAndOmitsWrapper(t *testing.T) {
	var trace []inventory.Hypothesis
	for i := 0; i < 9; i++ {
		trace = append(trace, inventory.Hypothesis{
			Cause:   fmt.Sprintf("node worker-%d (NotReady)", i),
			Verdict: inventory.VerdictOutranked,
			Reason:  "a stronger cause exists",
		})
	}
	w := inventory.Workload{Namespace: "shop", Name: "web", Kind: "Deployment", RootCauseTrace: trace}
	got := renderCandidates([]inventory.Workload{w})
	if n := strings.Count(got, "considered "); n != maxCandidatesPerWorkload {
		t.Errorf("rendered %d candidates, want the cap %d:\n%s", n, maxCandidatesPerWorkload, got)
	}
	if !strings.Contains(got, "    "+truncationMarker+"\n") {
		t.Errorf("a capped trace must carry the truncation marker:\n%s", got)
	}
	if strings.Contains(got, "Verify each attributed cause with the tools") {
		t.Errorf("renderCandidates must not carry the tool-loop wrapper (local mode has no tools):\n%s", got)
	}
	if strings.Contains(got, "deterministic pass already evaluated") {
		t.Errorf("renderCandidates must not carry renderTrace's header:\n%s", got)
	}
	if !strings.Contains(got, "- shop/web (Deployment):\n") {
		t.Errorf("workload heading missing:\n%s", got)
	}
}

func TestRenderCandidatesEmptyTrace(t *testing.T) {
	w := inventory.Workload{Namespace: "shop", Name: "web", Kind: "Deployment"}
	if got := renderCandidates([]inventory.Workload{w}); got != "" {
		t.Errorf("no trace must render nothing, got %q", got)
	}
}
