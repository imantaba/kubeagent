package gate

import (
	"bytes"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/findings"
)

func render(t *testing.T, v Verdict) string {
	t.Helper()
	var buf bytes.Buffer
	if err := RenderText(&buf, v); err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	return buf.String()
}

func TestRenderTextPass(t *testing.T) {
	got := render(t, Verdict{
		Verdict: "pass", Code: CodePass, FailOn: findings.Critical, Scope: "cluster",
		Failing: []findings.Finding{}, Reported: []findings.Finding{}, Inconclusive: []Blindspot{},
	})
	want := "GATE: pass — nothing at or above critical (scope: cluster)\n"
	if got != want {
		t.Errorf("RenderText =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderTextFailListsEachFailingFinding(t *testing.T) {
	got := render(t, Verdict{
		Verdict: "fail", Code: CodeFail, FailOn: findings.Critical, Scope: "Deployment/api in prod",
		Failing: []findings.Finding{{
			Level: findings.Critical, Kind: "Pod", Namespace: "prod", Name: "api-5f9c7d8b4-nk2wv",
			Issue: "CrashLoopBackOff", Reason: "Container repeatedly crashes after starting",
		}},
		Reported: []findings.Finding{}, Inconclusive: []Blindspot{},
	})
	for _, want := range []string{
		"GATE: fail — 1 finding at or above critical (scope: Deployment/api in prod)",
		"critical  Pod prod/api-5f9c7d8b4-nk2wv  CrashLoopBackOff",
		"Container repeatedly crashes after starting",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderText output missing %q; got:\n%s", want, got)
		}
	}
}

func TestRenderTextPluralizesFindings(t *testing.T) {
	f := findings.Finding{Level: findings.Critical, Kind: "Pod", Namespace: "prod", Name: "a", Issue: "OOMKilled"}
	got := render(t, Verdict{
		Verdict: "fail", Code: CodeFail, FailOn: findings.Critical, Scope: "cluster",
		Failing: []findings.Finding{f, f}, Reported: []findings.Finding{}, Inconclusive: []Blindspot{},
	})
	if !strings.Contains(got, "2 findings at or above critical") {
		t.Errorf("want a plural count; got:\n%s", got)
	}
}

func TestRenderTextNamesBelowThresholdFindingsAsNotCounted(t *testing.T) {
	got := render(t, Verdict{
		Verdict: "pass", Code: CodePass, FailOn: findings.Critical, Scope: "Deployment/api in prod",
		Failing: []findings.Finding{},
		Reported: []findings.Finding{
			{Level: findings.Critical, Kind: "Pod", Namespace: "staging", Name: "w-1", Issue: "OOMKilled"},
			{Level: findings.Warning, Kind: "Service", Namespace: "staging", Name: "svc", Issue: "no endpoints"},
		},
		Inconclusive: []Blindspot{},
	})
	if !strings.Contains(got, "not counted (below --fail-on): 2 findings") {
		t.Errorf("want an explicit not-counted line; got:\n%s", got)
	}
}

// TestRenderTextNotCountedLineIsIndependentOfTheHeaderScope pins that the
// "not counted (below --fail-on)" line and the header's "(scope: …)" carry two
// different meanings — the former about --fail-on, the latter about the
// namespace/workload scope — and each renders on its own regardless of the
// other's value.
func TestRenderTextNotCountedLineIsIndependentOfTheHeaderScope(t *testing.T) {
	got := render(t, Verdict{
		Verdict: "pass", Code: CodePass, FailOn: findings.Critical, Scope: "cluster",
		Failing: []findings.Finding{},
		Reported: []findings.Finding{
			{Level: findings.Warning, Kind: "Pod", Namespace: "staging", Name: "w-1", Issue: "OOMKilled"},
		},
		Inconclusive: []Blindspot{},
	})
	for _, want := range []string{
		"(scope: cluster)",
		"not counted (below --fail-on): 1 finding",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderText output missing %q; got:\n%s", want, got)
		}
	}
}

func TestRenderTextInconclusiveNamesTheBlindSpot(t *testing.T) {
	got := render(t, Verdict{
		Verdict: "inconclusive", Code: CodeInconclusive, FailOn: findings.Critical, Scope: "cluster",
		Failing: []findings.Finding{}, Reported: []findings.Finding{},
		Inconclusive: []Blindspot{{Resource: "events", Reason: "forbidden"}},
	})
	for _, want := range []string{
		"GATE: inconclusive",
		"could not read events: forbidden",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderText output missing %q; got:\n%s", want, got)
		}
	}
}

func TestRenderTextMarksWaivedBlindSpots(t *testing.T) {
	got := render(t, Verdict{
		Verdict: "pass", Code: CodePass, FailOn: findings.Critical, Scope: "cluster",
		Failing: []findings.Finding{}, Reported: []findings.Finding{},
		Inconclusive: []Blindspot{{Resource: "leases", Reason: "forbidden", Waived: true}},
	})
	if !strings.Contains(got, "could not read leases: forbidden (waived)") {
		t.Errorf("a waived blind spot must still be printed, marked waived; got:\n%s", got)
	}
}

func TestRenderTextTimeoutShowsTheLastObservedState(t *testing.T) {
	got := render(t, Verdict{
		Verdict: "timeout", Code: CodeTimeout, FailOn: findings.Critical, Scope: "Deployment/api in prod",
		Detail:  "1/3 replicas updated, 2 unavailable",
		Failing: []findings.Finding{}, Reported: []findings.Finding{}, Inconclusive: []Blindspot{},
	})
	want := "GATE: timeout — the rollout did not settle (scope: Deployment/api in prod)\n" +
		"  last observed: 1/3 replicas updated, 2 unavailable\n"
	if got != want {
		t.Errorf("RenderText =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderTextTimeoutWithoutDetailOmitsTheLine(t *testing.T) {
	got := render(t, Verdict{
		Verdict: "timeout", Code: CodeTimeout, FailOn: findings.Critical, Scope: "cluster",
		Failing: []findings.Finding{}, Reported: []findings.Finding{}, Inconclusive: []Blindspot{},
	})
	if strings.Contains(got, "last observed") {
		t.Errorf("an empty Detail must not print a bare label; got:\n%s", got)
	}
}
