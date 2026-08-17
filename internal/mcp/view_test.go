package mcp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/hpahealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/pdbhealth"
	"github.com/imantaba/kubeagent/internal/pvchealth"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/termhealth"
)

func TestFindingsFromResult_DetectorFindingIsCriticalWithARemediationHint(t *testing.T) {
	res := scan.Result{
		Inventory: inventory.Result{
			Workloads: []inventory.Workload{{
				Namespace: "payments", Name: "api", Kind: "Deployment", Desired: 3, Ready: 0, Status: "Degraded",
				Findings: []diagnose.Finding{{
					Pod: "payments/api-abc", Issue: "CrashLoopBackOff",
					Reason: "container exits immediately", Evidence: "restart count 7",
					Container: "api", Confidence: "high",
				}},
			}},
		},
	}

	got := findingsFromResult(res)

	if len(got) != 1 {
		t.Fatalf("findingsFromResult() = %d findings, want 1", len(got))
	}
	f := got[0]
	if f.Severity != "critical" {
		t.Errorf("Severity = %q, want %q", f.Severity, "critical")
	}
	if f.Kind != "Pod" || f.Namespace != "payments" || f.Name != "api-abc" {
		t.Errorf("got %s %s/%s, want Pod payments/api-abc", f.Kind, f.Namespace, f.Name)
	}
	if f.Reason != "CrashLoopBackOff" {
		t.Errorf("Reason = %q, want the issue name", f.Reason)
	}
	if f.Confidence != "high" {
		t.Errorf("Confidence = %q, want %q", f.Confidence, "high")
	}
	if f.RemediationHint == "" {
		t.Error("RemediationHint is empty; every detector finding has a deterministic next step")
	}
}

func TestFindingsFromResult_DegradedWorkloadWithoutADetectorFindingStillReports(t *testing.T) {
	res := scan.Result{
		Inventory: inventory.Result{
			Workloads: []inventory.Workload{{
				Namespace: "payments", Name: "worker", Kind: "Deployment",
				Desired: 3, Ready: 1, Status: "Degraded",
			}},
		},
	}

	got := findingsFromResult(res)

	if len(got) != 1 {
		t.Fatalf("findingsFromResult() = %d findings, want 1 workload-level finding", len(got))
	}
	if got[0].Severity != "warning" || got[0].Kind != "Deployment" || got[0].Name != "worker" {
		t.Errorf("got %+v, want a warning on Deployment worker", got[0])
	}
	if got[0].Detail != "1/3 ready" {
		t.Errorf("Detail = %q, want %q", got[0].Detail, "1/3 ready")
	}
}

func TestFindingsFromResult_HealthyRestartedWorkloadProducesNoFindings(t *testing.T) {
	res := scan.Result{
		Inventory: inventory.Result{
			Workloads: []inventory.Workload{{
				Namespace: "payments", Name: "api", Kind: "Deployment",
				Desired: 3, Ready: 3, Status: "Running", Restarts: 4,
			}},
		},
	}

	got := findingsFromResult(res)

	if len(got) != 0 {
		t.Errorf("findingsFromResult() = %+v, want none — a healthy workload that merely "+
			"restarted in the past is not a warning a model should act on", got)
	}
}

func TestFindingsFromResult_CoversEveryAttentionClassNotJustWorkloads(t *testing.T) {
	res := scan.Result{
		ServiceIssues: []svchealth.Issue{{
			Namespace: "payments", Name: "api", Type: "ClusterIP",
			Problem: "no endpoints", Detail: "selector matches no pods",
		}},
		PVCIssues: []pvchealth.Issue{{
			Namespace: "payments", Name: "data", Phase: "Pending", Reason: "no provisioner",
		}},
	}

	got := findingsFromResult(res)

	kinds := map[string]bool{}
	for _, f := range got {
		kinds[f.Kind] = true
	}
	if !kinds["Service"] || !kinds["PersistentVolumeClaim"] {
		t.Errorf("kinds = %v, want both Service and PersistentVolumeClaim; a triage payload that "+
			"reports only workloads silently drops every other class the CLI treats as degrading", kinds)
	}
}

// TestFindingsFromResult_UsesCamelCaseFindingVocabulary asserts that
// stuck-terminating, PDB and HPA issues surface in the MCP tool result's
// Reason field with the CamelCase spelling shared by gate JSON and the watch
// daemon's /issues — not the raw lowercase reason/category the source
// packages carry.
func TestFindingsFromResult_UsesCamelCaseFindingVocabulary(t *testing.T) {
	res := scan.Result{
		StuckTerminating: []termhealth.Issue{
			{Kind: "Namespace", Name: "legacy-ns", Age: "2h", Reason: "stuck in Terminating"},
		},
		PDBIssues: []pdbhealth.Issue{
			{Namespace: "shop", Name: "unsat", Category: "unsatisfiable", Reason: "r1"},
			{Namespace: "shop", Name: "stl", Category: "stale", Reason: "r2"},
			{Namespace: "shop", Name: "blk", Category: "blocking", Reason: "r3"},
			{Namespace: "shop", Name: "sgl", Category: "singleton", Reason: "r4"},
		},
		HPAIssues: []hpahealth.Issue{
			{Namespace: "shop", Name: "unable-hpa", Category: "unable", Reason: "r5"},
			{Namespace: "shop", Name: "metrics-hpa", Category: "metrics", Reason: "r6"},
			{Namespace: "shop", Name: "capped-hpa", Category: "capped", Reason: "r7"},
			{Namespace: "shop", Name: "disabled-hpa", Category: "disabled", Reason: "r8"},
			{Namespace: "shop", Name: "ambiguous-hpa", Category: "ambiguous", Reason: "r9"},
		},
	}

	got := findingsFromResult(res)
	want := map[string]string{
		"legacy-ns":     "StuckTerminating",
		"unsat":         "PDBUnsatisfiable",
		"stl":           "PDBStale",
		"blk":           "PDBBlocked",
		"sgl":           "PDBSingleton",
		"unable-hpa":    "HPAUnableToScale",
		"metrics-hpa":   "HPAMetricsFailed",
		"capped-hpa":    "HPACapped",
		"disabled-hpa":  "HPAScalingDisabled",
		"ambiguous-hpa": "HPAAmbiguousSelector",
	}
	if len(got) != len(want) {
		t.Fatalf("findingsFromResult() = %d findings, want %d: %+v", len(got), len(want), got)
	}
	seen := make(map[string]bool, len(got))
	for _, f := range got {
		wantReason, ok := want[f.Name]
		if !ok {
			t.Errorf("unexpected finding for %s: %+v", f.Name, f)
			continue
		}
		if f.Reason != wantReason {
			t.Errorf("%s: Reason = %q, want %q", f.Name, f.Reason, wantReason)
		}
		seen[f.Name] = true
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("missing finding for %s", name)
		}
	}
}

func TestFindingsFromResult_ExpectedServiceIssuesAreNotFindings(t *testing.T) {
	res := scan.Result{
		ServiceIssues: []svchealth.Issue{{
			Namespace: "kube-system", Name: "headless", Problem: "no endpoints", Expected: true,
		}},
	}

	if got := findingsFromResult(res); len(got) != 0 {
		t.Errorf("findingsFromResult() = %v, want none — the CLI does not treat expected issues as attention", got)
	}
}

func TestSortFindings_TotalOrderIsDeterministic(t *testing.T) {
	in := []Finding{
		{Severity: "warning", Kind: "Service", Namespace: "b", Name: "two", Reason: "x"},
		{Severity: "critical", Kind: "Pod", Namespace: "b", Name: "one", Reason: "x"},
		{Severity: "warning", Kind: "Service", Namespace: "a", Name: "three", Reason: "x"},
		{Severity: "warning", Kind: "Ingress", Namespace: "a", Name: "three", Reason: "x"},
	}
	sortFindings(in)

	want := []string{"critical/b/one/Pod", "warning/a/three/Ingress", "warning/a/three/Service", "warning/b/two/Service"}
	for i, w := range want {
		got := fmt.Sprintf("%s/%s/%s/%s", in[i].Severity, in[i].Namespace, in[i].Name, in[i].Kind)
		if got != w {
			t.Errorf("position %d = %q, want %q", i, got, w)
		}
	}
}

func TestCapFindings_TruncatesAndReportsHowMany(t *testing.T) {
	in := make([]Finding, MaxFindings+7)
	for i := range in {
		in[i] = Finding{Severity: "warning", Kind: "Pod", Namespace: "n", Name: fmt.Sprintf("p%03d", i)}
	}

	got, omitted := capFindings(in)

	if len(got) != MaxFindings {
		t.Errorf("len = %d, want %d", len(got), MaxFindings)
	}
	if omitted != 7 {
		t.Errorf("omitted = %d, want 7", omitted)
	}
}

func TestCapFindings_SortedCriticalsSurviveTruncationAheadOfWarnings(t *testing.T) {
	in := make([]Finding, 0, MaxFindings+5)
	for i := 0; i < 5; i++ {
		in = append(in, Finding{Severity: "warning", Kind: "Pod", Namespace: "n", Name: fmt.Sprintf("w%03d", i)})
	}
	for i := 0; i < MaxFindings; i++ {
		in = append(in, Finding{Severity: "critical", Kind: "Pod", Namespace: "n", Name: fmt.Sprintf("c%03d", i)})
	}

	sortFindings(in)
	got, omitted := capFindings(in)

	if omitted != 5 {
		t.Fatalf("omitted = %d, want 5", omitted)
	}
	if len(got) != MaxFindings {
		t.Fatalf("len(got) = %d, want %d", len(got), MaxFindings)
	}
	for _, f := range got {
		if f.Severity != "critical" {
			t.Errorf("got a %q finding in the surviving set: %+v; sorted criticals must survive "+
				"truncation ahead of warnings", f.Severity, f)
		}
	}
}

func TestFinding_JSONKeysAreTheDocumentedOnes(t *testing.T) {
	blob, err := json.Marshal(Finding{Severity: "critical", Kind: "Pod", Namespace: "n", Name: "p", Reason: "r"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"severity":"critical","kind":"Pod","namespace":"n","name":"p","reason":"r"}`
	if string(blob) != want {
		t.Errorf("Marshal() = %s, want %s", blob, want)
	}
}
