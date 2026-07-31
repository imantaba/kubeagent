package findings

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/policy"
	"github.com/imantaba/kubeagent/internal/pvchealth"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

func TestLevelOrdering(t *testing.T) {
	if !(Info < Warning && Warning < Critical) {
		t.Fatalf("want Info < Warning < Critical, got %d %d %d", Info, Warning, Critical)
	}
}

func TestLevelString(t *testing.T) {
	for _, tc := range []struct {
		level Level
		want  string
	}{
		{Info, "info"},
		{Warning, "warning"},
		{Critical, "critical"},
	} {
		if got := tc.level.String(); got != tc.want {
			t.Errorf("Level(%d).String() = %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestLevelMarshalJSON(t *testing.T) {
	b, err := json.Marshal(Critical)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `"critical"` {
		t.Errorf("Marshal(Critical) = %s, want \"critical\"", b)
	}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Level
	}{
		{"info", Info},
		{"warning", Warning},
		{"critical", Critical},
	} {
		got, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q): unexpected error %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseRejectsUnknown(t *testing.T) {
	if _, err := Parse("fatal"); err == nil {
		t.Fatal("Parse(\"fatal\"): want an error, got nil")
	}
}

func TestFlattenDiagnoseFindingIsCriticalAndCarriesItsOwner(t *testing.T) {
	res := scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{{
		Namespace: "prod", Name: "api", Kind: "Deployment", Desired: 3, Ready: 1,
		Status: "Degraded",
		Findings: []diagnose.Finding{{
			Pod:      "prod/api-5f9c7d8b4-nk2wv",
			Issue:    "CrashLoopBackOff",
			Reason:   "Container repeatedly crashes after starting",
			Evidence: "restartCount=4",
		}},
	}}}}

	got := Flatten(res)
	if len(got) != 1 {
		t.Fatalf("Flatten returned %d findings, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Level != Critical {
		t.Errorf("Level = %v, want Critical", f.Level)
	}
	if f.Kind != "Pod" || f.Namespace != "prod" || f.Name != "api-5f9c7d8b4-nk2wv" {
		t.Errorf("object = %s %s/%s, want Pod prod/api-5f9c7d8b4-nk2wv", f.Kind, f.Namespace, f.Name)
	}
	if f.Issue != "CrashLoopBackOff" {
		t.Errorf("Issue = %q, want CrashLoopBackOff", f.Issue)
	}
	if f.Reason != "Container repeatedly crashes after starting (restartCount=4)" {
		t.Errorf("Reason = %q, want the reason with the evidence appended", f.Reason)
	}
	if f.Owner != "Deployment/api" {
		t.Errorf("Owner = %q, want Deployment/api", f.Owner)
	}
}

func TestFlattenFlaggedWorkloadWithoutFindingsIsWarning(t *testing.T) {
	res := scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{{
		Namespace: "prod", Name: "api", Kind: "Deployment",
		Desired: 3, Ready: 1, Status: "Degraded",
	}}}}

	got := Flatten(res)
	if len(got) != 1 {
		t.Fatalf("Flatten returned %d findings, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Level != Warning {
		t.Errorf("Level = %v, want Warning", f.Level)
	}
	if f.Kind != "Deployment" || f.Name != "api" {
		t.Errorf("object = %s %s, want Deployment api", f.Kind, f.Name)
	}
	if f.Issue != "Degraded" {
		t.Errorf("Issue = %q, want the workload Status", f.Issue)
	}
	if f.Reason != "1/3 ready" {
		t.Errorf("Reason = %q, want \"1/3 ready\"", f.Reason)
	}
	if f.Owner != "Deployment/api" {
		t.Errorf("Owner = %q, want Deployment/api", f.Owner)
	}
}

func TestFlattenSkipsHealthyWorkloads(t *testing.T) {
	res := scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{{
		Namespace: "prod", Name: "api", Kind: "Deployment",
		Desired: 3, Ready: 3, Status: "Running",
	}}}}

	if got := Flatten(res); len(got) != 0 {
		t.Fatalf("Flatten returned %d findings for a healthy workload, want 0: %+v", len(got), got)
	}
}

func TestFlattenIncludesHealthIssuesAsWarnings(t *testing.T) {
	res := scan.Result{
		ServiceIssues: []svchealth.Issue{{
			Namespace: "prod", Name: "api", Problem: "no endpoints", Detail: "selector matches 0 pods",
		}},
		PVCIssues: []pvchealth.Issue{{
			Namespace: "prod", Name: "data", Reason: "Pending", Detail: "no matching PersistentVolume",
		}},
	}

	got := Flatten(res)
	if len(got) != 2 {
		t.Fatalf("Flatten returned %d findings, want 2: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Level != Warning {
			t.Errorf("%s %s/%s: Level = %v, want Warning", f.Kind, f.Namespace, f.Name, f.Level)
		}
	}
}

func TestFlattenSkipsExpectedServiceIssues(t *testing.T) {
	res := scan.Result{ServiceIssues: []svchealth.Issue{{
		Namespace: "prod", Name: "headless", Problem: "no endpoints", Expected: true,
	}}}

	if got := Flatten(res); len(got) != 0 {
		t.Fatalf("Flatten returned %d findings for an expected issue, want 0: %+v", len(got), got)
	}
}

func TestFlattenSortsCriticalFirstThenByObject(t *testing.T) {
	res := scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{
		{Namespace: "zeta", Name: "w", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded"},
		{Namespace: "alpha", Name: "w", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded"},
		{Namespace: "mid", Name: "w", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Findings: []diagnose.Finding{{Pod: "mid/w-1", Issue: "OOMKilled", Reason: "killed"}},
		},
	}}}

	got := Flatten(res)
	if len(got) != 3 {
		t.Fatalf("Flatten returned %d findings, want 3: %+v", len(got), got)
	}
	if got[0].Level != Critical {
		t.Errorf("first finding Level = %v, want Critical", got[0].Level)
	}
	if got[1].Namespace != "alpha" || got[2].Namespace != "zeta" {
		t.Errorf("warnings ordered %q then %q, want alpha then zeta", got[1].Namespace, got[2].Namespace)
	}
}

func TestFlattenIsDeterministic(t *testing.T) {
	res := scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{
		{Namespace: "b", Name: "x", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded"},
		{Namespace: "a", Name: "y", Kind: "StatefulSet", Desired: 2, Ready: 1, Status: "Degraded"},
	}}}

	first, second := Flatten(res), Flatten(res)
	if len(first) != len(second) {
		t.Fatalf("two Flatten calls returned %d and %d findings", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("finding %d differs between calls: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestFromPolicyMapsLevelsAndPrefixesTheIssue(t *testing.T) {
	got := FromPolicy([]policy.Violation{
		{RuleID: "registry-allowlist", Level: policy.LevelCritical, Kind: "Pod",
			Namespace: "prod", Name: "web", Message: "image is not from an allowed registry",
			Evidence: "docker.example.net/app:1.0"},
		{RuleID: "pdb-required", Level: policy.LevelWarning, Kind: "Deployment",
			Namespace: "prod", Name: "api", Message: "no PodDisruptionBudget covers this Deployment"},
		{RuleID: "zone-label", Level: policy.LevelInfo, Kind: "Node",
			Name: "worker-1", Message: "no topology label"},
	}, nil)

	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3", len(got))
	}
	wantLevels := []Level{Critical, Warning, Info}
	for i, want := range wantLevels {
		if got[i].Level != want {
			t.Errorf("finding %d level = %v, want %v", i, got[i].Level, want)
		}
	}
	if got[0].Issue != "policy/registry-allowlist" {
		t.Errorf("Issue = %q, want the policy/ prefix", got[0].Issue)
	}
	if !strings.Contains(got[0].Reason, "image is not from an allowed registry") ||
		!strings.Contains(got[0].Reason, "docker.example.net/app:1.0") {
		t.Errorf("Reason = %q, want the message and the evidence", got[0].Reason)
	}
	if got[1].Reason != "no PodDisruptionBudget covers this Deployment" {
		t.Errorf("a violation with no evidence gained a suffix: %q", got[1].Reason)
	}
	if got[2].Namespace != "" || got[2].Name != "worker-1" {
		t.Errorf("cluster-scoped violation = %#v", got[2])
	}
}

// A rule kubeagent could not run must reach the gate as a finding at the
// rule's own level. Dropping it — or demoting it to info — would let a
// refused read pass a build that the operator wrote a critical rule to stop.
func TestFromPolicyTurnsAnUnevaluatedRuleIntoAFindingAtItsOwnLevel(t *testing.T) {
	got := FromPolicy(nil, []policy.Unevaluated{{
		RuleID: "storage-encrypted", Level: policy.LevelCritical, Kind: "StorageClass",
		Reason: "kubeagent could not read this kind, so the rule was not evaluated",
	}})

	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	f := got[0]
	if f.Level != Critical {
		t.Errorf("Level = %v, want Critical — a blind rule keeps its own severity", f.Level)
	}
	if f.Issue != "policy/storage-encrypted" {
		t.Errorf("Issue = %q", f.Issue)
	}
	if f.Kind != "StorageClass" {
		t.Errorf("Kind = %q", f.Kind)
	}
	if f.Name != "" {
		t.Errorf("Name = %q, want empty — no object was evaluated", f.Name)
	}
	if !strings.Contains(f.Reason, "not evaluated") {
		t.Errorf("Reason = %q, want kubeagent's own words", f.Reason)
	}
}

func TestFromPolicyWithNothingReturnsNothing(t *testing.T) {
	if got := FromPolicy(nil, nil); len(got) != 0 {
		t.Errorf("got %d findings from no policy results", len(got))
	}
}
