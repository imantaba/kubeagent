package investigate

import (
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/inventory"
)

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
