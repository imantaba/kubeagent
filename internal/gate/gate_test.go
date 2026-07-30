package gate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/jsonschema"
	"github.com/imantaba/kubeagent/internal/scan"
)

// degraded builds a workload with one critical detector finding.
func degraded(ns, name, kind string) inventory.Workload {
	return inventory.Workload{
		Namespace: ns, Name: name, Kind: kind, Desired: 1, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{
			Pod: ns + "/" + name + "-abc", Issue: "CrashLoopBackOff", Reason: "crashes on start",
		}},
	}
}

func TestDecideHealthyClusterPasses(t *testing.T) {
	v := Decide(scan.Result{}, Options{FailOn: findings.Critical})
	if v.Code != CodePass {
		t.Fatalf("Code = %d, want %d", v.Code, CodePass)
	}
	if v.Verdict != "pass" {
		t.Errorf("Verdict = %q, want pass", v.Verdict)
	}
	if len(v.Failing) != 0 {
		t.Errorf("Failing = %+v, want empty", v.Failing)
	}
}

func TestDecideCriticalFindingFails(t *testing.T) {
	res := scan.Result{Inventory: inventory.Result{
		Workloads: []inventory.Workload{degraded("prod", "api", "Deployment")}}}

	v := Decide(res, Options{FailOn: findings.Critical})
	if v.Code != CodeFail {
		t.Fatalf("Code = %d, want %d", v.Code, CodeFail)
	}
	if v.Verdict != "fail" {
		t.Errorf("Verdict = %q, want fail", v.Verdict)
	}
	if len(v.Failing) != 1 {
		t.Fatalf("Failing has %d entries, want 1: %+v", len(v.Failing), v.Failing)
	}
}

func TestDecideWarningDoesNotFailAtCriticalThreshold(t *testing.T) {
	res := scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{{
		Namespace: "prod", Name: "api", Kind: "Deployment",
		Desired: 3, Ready: 1, Status: "Degraded",
	}}}}

	v := Decide(res, Options{FailOn: findings.Critical})
	if v.Code != CodePass {
		t.Fatalf("Code = %d, want %d (a warning must not trip --fail-on critical)", v.Code, CodePass)
	}
	if len(v.Reported) != 1 {
		t.Errorf("Reported has %d entries, want the warning to still be reported: %+v", len(v.Reported), v.Reported)
	}
}

func TestDecideWarningFailsAtWarningThreshold(t *testing.T) {
	res := scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{{
		Namespace: "prod", Name: "api", Kind: "Deployment",
		Desired: 3, Ready: 1, Status: "Degraded",
	}}}}

	v := Decide(res, Options{FailOn: findings.Warning})
	if v.Code != CodeFail {
		t.Fatalf("Code = %d, want %d", v.Code, CodeFail)
	}
}

func TestDecideScopeExcludesOtherWorkloads(t *testing.T) {
	res := scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{
		degraded("prod", "api", "Deployment"),
		degraded("staging", "worker", "Deployment"),
	}}}

	v := Decide(res, Options{
		FailOn: findings.Critical, ScopeKind: "Deployment", ScopeName: "api", ScopeNamespace: "prod",
	})
	if v.Code != CodeFail {
		t.Fatalf("Code = %d, want %d", v.Code, CodeFail)
	}
	if len(v.Failing) != 1 {
		t.Fatalf("Failing has %d entries, want only the in-scope one: %+v", len(v.Failing), v.Failing)
	}
	if v.Failing[0].Namespace != "prod" {
		t.Errorf("failing finding is in %q, want prod", v.Failing[0].Namespace)
	}
	if len(v.Reported) != 1 || v.Reported[0].Namespace != "staging" {
		t.Errorf("Reported = %+v, want the out-of-scope staging finding", v.Reported)
	}
}

func TestDecideScopedRunPassesWhenOnlyOtherWorkloadsAreBroken(t *testing.T) {
	res := scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{
		degraded("staging", "worker", "Deployment"),
	}}}

	v := Decide(res, Options{
		FailOn: findings.Critical, ScopeKind: "Deployment", ScopeName: "api", ScopeNamespace: "prod",
	})
	if v.Code != CodePass {
		t.Fatalf("Code = %d, want %d — an unrelated namespace must not fail this deploy", v.Code, CodePass)
	}
}

func TestDecidePartialReadIsInconclusive(t *testing.T) {
	res := scan.Result{PartialReads: []scan.ReadFailure{{
		Resource: "events", Reason: "forbidden",
	}}}

	v := Decide(res, Options{FailOn: findings.Critical})
	if v.Code != CodeInconclusive {
		t.Fatalf("Code = %d, want %d", v.Code, CodeInconclusive)
	}
	if v.Verdict != "inconclusive" {
		t.Errorf("Verdict = %q, want inconclusive", v.Verdict)
	}
	if len(v.Inconclusive) != 1 || v.Inconclusive[0].Waived {
		t.Errorf("Inconclusive = %+v, want one unwaived blindspot", v.Inconclusive)
	}
}

func TestDecideWaivedPartialReadStillPasses(t *testing.T) {
	res := scan.Result{PartialReads: []scan.ReadFailure{{
		Resource: "leases", Reason: "forbidden",
	}}}

	v := Decide(res, Options{FailOn: findings.Critical, AllowPartialRead: []string{"leases"}})
	if v.Code != CodePass {
		t.Fatalf("Code = %d, want %d", v.Code, CodePass)
	}
	if len(v.Inconclusive) != 1 || !v.Inconclusive[0].Waived {
		t.Errorf("Inconclusive = %+v, want the waiver recorded, not dropped", v.Inconclusive)
	}
}

func TestDecideWaiverIsPerResource(t *testing.T) {
	res := scan.Result{PartialReads: []scan.ReadFailure{
		{Resource: "leases", Reason: "forbidden"},
		{Resource: "events", Reason: "forbidden"},
	}}

	v := Decide(res, Options{FailOn: findings.Critical, AllowPartialRead: []string{"leases"}})
	if v.Code != CodeInconclusive {
		t.Fatalf("Code = %d, want %d — waiving leases must not waive events", v.Code, CodeInconclusive)
	}
}

func TestDecideInconclusiveBeatsFail(t *testing.T) {
	res := scan.Result{
		Inventory:    inventory.Result{Workloads: []inventory.Workload{degraded("prod", "api", "Deployment")}},
		PartialReads: []scan.ReadFailure{{Resource: "events", Reason: "forbidden"}},
	}

	v := Decide(res, Options{FailOn: findings.Critical})
	if v.Code != CodeInconclusive {
		t.Fatalf("Code = %d, want %d — a blind run must not report a confident failure", v.Code, CodeInconclusive)
	}
}

func TestDecideTimeoutBeatsEverything(t *testing.T) {
	res := scan.Result{
		Inventory:    inventory.Result{Workloads: []inventory.Workload{degraded("prod", "api", "Deployment")}},
		PartialReads: []scan.ReadFailure{{Resource: "events", Reason: "forbidden"}},
	}

	v := Decide(res, Options{
		FailOn: findings.Critical, TimedOut: true,
		TimeoutDetail: "1/3 replicas updated, 2 unavailable",
	})
	if v.Code != CodeTimeout {
		t.Fatalf("Code = %d, want %d", v.Code, CodeTimeout)
	}
	if v.Verdict != "timeout" {
		t.Errorf("Verdict = %q, want timeout", v.Verdict)
	}
	if v.Detail != "1/3 replicas updated, 2 unavailable" {
		t.Errorf("Detail = %q, want the last observed rollout state", v.Detail)
	}
}

func TestDecideTimeoutBeatsInconclusiveAlone(t *testing.T) {
	res := scan.Result{PartialReads: []scan.ReadFailure{{Resource: "events", Reason: "forbidden"}}}

	v := Decide(res, Options{FailOn: findings.Critical, TimedOut: true})
	if v.Code != CodeTimeout {
		t.Fatalf("Code = %d, want %d", v.Code, CodeTimeout)
	}
}

func TestDecideWaiverForAResourceThatNeverFailedIsInert(t *testing.T) {
	v := Decide(scan.Result{}, Options{
		FailOn: findings.Critical, AllowPartialRead: []string{"networkpolicies"},
	})
	if v.Code != CodePass {
		t.Fatalf("Code = %d, want %d", v.Code, CodePass)
	}
	if len(v.Inconclusive) != 0 {
		t.Errorf("Inconclusive = %v, want empty: a waiver must not invent a blind spot", v.Inconclusive)
	}
}

func TestDecideScopeStringNamesTheWorkload(t *testing.T) {
	v := Decide(scan.Result{}, Options{
		FailOn: findings.Critical, ScopeKind: "Deployment", ScopeName: "api", ScopeNamespace: "prod",
	})
	if v.Scope != "Deployment/api in prod" {
		t.Errorf("Scope = %q, want \"Deployment/api in prod\"", v.Scope)
	}
}

func TestDecideUnscopedRunSaysCluster(t *testing.T) {
	v := Decide(scan.Result{}, Options{FailOn: findings.Critical})
	if v.Scope != "cluster" {
		t.Errorf("Scope = %q, want \"cluster\"", v.Scope)
	}
}

func TestDecideNeverReturnsNilSlices(t *testing.T) {
	v := Decide(scan.Result{}, Options{FailOn: findings.Critical})
	if v.Failing == nil || v.Reported == nil || v.Inconclusive == nil {
		t.Fatalf("nil slice in verdict: %+v — --output json must emit [] not null", v)
	}
}

func TestDecideStampsTheSchemaVersion(t *testing.T) {
	v := Decide(scan.Result{}, Options{FailOn: findings.Critical})
	if v.SchemaVersion != jsonschema.GateVersion {
		t.Errorf("SchemaVersion = %q, want %q", v.SchemaVersion, jsonschema.GateVersion)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"schemaVersion":"`+jsonschema.GateVersion+`"`) {
		t.Errorf("verdict JSON has no schemaVersion:\n%s", raw)
	}
}
