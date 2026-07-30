package sarif

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/gate"
)

func sampleVerdict() gate.Verdict {
	return gate.Verdict{
		Verdict: "fail", Code: gate.CodeFail, FailOn: findings.Critical, Scope: "cluster",
		Failing: []findings.Finding{{
			Level: findings.Critical, Kind: "Pod", Namespace: "payments", Name: "worker-7d9c6f6b8-x2z4q",
			Issue:  "CrashLoopBackOff",
			Reason: "Container repeatedly crashes after starting (restartCount=5)",
			Owner:  "Deployment/worker",
		}},
		Reported: []findings.Finding{{
			Level: findings.Warning, Kind: "Service", Namespace: "payments", Name: "worker",
			Issue: "no endpoints", Reason: "selector matches 0 pods",
		}},
		Inconclusive: []gate.Blindspot{},
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	got, err := Render(sampleVerdict(), "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	path := filepath.Join("testdata", "golden-gate.sarif.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Render output differs from %s.\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	first, err := Render(sampleVerdict(), "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	second, err := Render(sampleVerdict(), "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("two renders of the same verdict differ; an unchanged cluster must produce byte-identical SARIF")
	}
}

// doc is the subset of SARIF the tests assert on.
type doc struct {
	Schema  string `json:"$schema"`
	Version string `json:"version"`
	Runs    []struct {
		Tool struct {
			Driver struct {
				Name           string `json:"name"`
				Version        string `json:"version"`
				InformationURI string `json:"informationUri"`
				Rules          []struct {
					ID                   string                 `json:"id"`
					Name                 string                 `json:"name"`
					ShortDescription     struct{ Text string }  `json:"shortDescription"`
					DefaultConfiguration struct{ Level string } `json:"defaultConfiguration"`
				} `json:"rules"`
			} `json:"driver"`
		} `json:"tool"`
		Results []struct {
			RuleID    string                `json:"ruleId"`
			Level     string                `json:"level"`
			Message   struct{ Text string } `json:"message"`
			Locations []struct {
				PhysicalLocation struct {
					ArtifactLocation struct{ URI string } `json:"artifactLocation"`
				} `json:"physicalLocation"`
			} `json:"locations"`
		} `json:"results"`
		Invocations []struct {
			ExecutionSuccessful            bool `json:"executionSuccessful"`
			ToolConfigurationNotifications []struct {
				Descriptor struct{ ID string }   `json:"descriptor"`
				Level      string                `json:"level"`
				Message    struct{ Text string } `json:"message"`
			} `json:"toolConfigurationNotifications"`
		} `json:"invocations"`
	} `json:"runs"`
}

func parse(t *testing.T, b []byte) doc {
	t.Helper()
	var d doc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("rendered SARIF does not parse as JSON: %v\n%s", err, b)
	}
	return d
}

func TestRenderCarriesTheRequiredTopLevelFields(t *testing.T) {
	b, err := Render(sampleVerdict(), "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	d := parse(t, b)
	if d.Version != "2.1.0" {
		t.Errorf("version = %q, want 2.1.0", d.Version)
	}
	if d.Schema != "https://json.schemastore.org/sarif-2.1.0.json" {
		t.Errorf("$schema = %q", d.Schema)
	}
	if len(d.Runs) != 1 {
		t.Fatalf("runs has %d entries, want 1", len(d.Runs))
	}
	drv := d.Runs[0].Tool.Driver
	if drv.Name != "kubeagent" {
		t.Errorf("driver.name = %q, want kubeagent", drv.Name)
	}
	if drv.Version != "v0.65.0" {
		t.Errorf("driver.version = %q, want the version passed in", drv.Version)
	}
	if drv.InformationURI != "https://github.com/imantaba/kubeagent" {
		t.Errorf("driver.informationUri = %q", drv.InformationURI)
	}
}

func TestRenderIncludesBothFailingAndReportedFindings(t *testing.T) {
	b, err := Render(sampleVerdict(), "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	d := parse(t, b)
	if len(d.Runs[0].Results) != 2 {
		t.Fatalf("results has %d entries, want 2 (the failing finding and the reported one)", len(d.Runs[0].Results))
	}
}

func TestRenderMapsLevels(t *testing.T) {
	b, err := Render(sampleVerdict(), "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	d := parse(t, b)
	byRule := map[string]string{}
	for _, r := range d.Runs[0].Results {
		byRule[r.RuleID] = r.Level
	}
	if byRule["CrashLoopBackOff"] != "error" {
		t.Errorf("critical rendered as %q, want error", byRule["CrashLoopBackOff"])
	}
	if byRule["no endpoints"] != "warning" {
		t.Errorf("warning rendered as %q, want warning", byRule["no endpoints"])
	}
}

func TestRenderUsesSyntheticClusterURIWithNoRegion(t *testing.T) {
	b, err := Render(sampleVerdict(), "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	d := parse(t, b)
	uri := d.Runs[0].Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI
	if uri != "k8s://payments/Pod/worker-7d9c6f6b8-x2z4q" {
		t.Errorf("artifact URI = %q, want k8s://payments/Pod/worker-7d9c6f6b8-x2z4q", uri)
	}
	if bytes.Contains(b, []byte(`"region"`)) {
		t.Error("rendered SARIF carries a region; a cluster object has no line number to point at")
	}
}

// A cluster-scoped finding really does reach here with an empty namespace:
// findings.Flatten records webhook issues with Namespace: "".
func TestRenderClusterScopedFindingOmitsTheNamespaceSegment(t *testing.T) {
	v := gate.Verdict{
		Verdict: "fail", Code: gate.CodeFail, FailOn: findings.Critical, Scope: "cluster",
		Failing: []findings.Finding{{
			Level: findings.Warning, Kind: "ValidatingWebhookConfiguration", Namespace: "", Name: "policy",
			Issue: "NoMatchingService", Reason: "webhook points at a service that does not exist",
		}},
		Reported: []findings.Finding{}, Inconclusive: []gate.Blindspot{},
	}
	b, err := Render(v, "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	d := parse(t, b)
	uri := d.Runs[0].Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI
	if uri != "k8s://ValidatingWebhookConfiguration/policy" {
		t.Errorf("artifact URI = %q, want k8s://ValidatingWebhookConfiguration/policy", uri)
	}
}

func TestRenderMapsInfoToNote(t *testing.T) {
	v := gate.Verdict{
		Verdict: "fail", Code: gate.CodeFail, FailOn: findings.Info, Scope: "cluster",
		Failing: []findings.Finding{{
			Level: findings.Info, Kind: "Pod", Namespace: "payments", Name: "worker",
			Issue: "Informational", Reason: "nothing is wrong, this is a note",
		}},
		Reported: []findings.Finding{}, Inconclusive: []gate.Blindspot{},
	}
	b, err := Render(v, "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	d := parse(t, b)
	if got := d.Runs[0].Results[0].Level; got != "note" {
		t.Errorf("level = %q, want note", got)
	}
}

func TestRenderOnlyDeclaresRulesThatFired(t *testing.T) {
	b, err := Render(sampleVerdict(), "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	d := parse(t, b)
	rules := d.Runs[0].Tool.Driver.Rules
	if len(rules) != 2 {
		t.Fatalf("driver.rules has %d entries, want exactly the 2 that fired", len(rules))
	}
	if rules[0].ID != "CrashLoopBackOff" || rules[1].ID != "no endpoints" {
		t.Errorf("rules = %q, %q; want them sorted by id", rules[0].ID, rules[1].ID)
	}
	if rules[0].DefaultConfiguration.Level != "error" {
		t.Errorf("CrashLoopBackOff defaultConfiguration.level = %q, want error", rules[0].DefaultConfiguration.Level)
	}
}

func TestRenderDeclaresEachRuleOnceEvenWhenItFiresTwice(t *testing.T) {
	v := sampleVerdict()
	v.Failing = append(v.Failing, findings.Finding{
		Level: findings.Critical, Kind: "Pod", Namespace: "payments", Name: "worker-7d9c6f6b8-aaaaa",
		Issue: "CrashLoopBackOff", Reason: "Container repeatedly crashes after starting (restartCount=2)",
	})
	b, err := Render(v, "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	d := parse(t, b)
	if len(d.Runs[0].Tool.Driver.Rules) != 2 {
		t.Errorf("driver.rules has %d entries, want 2 distinct rules for 3 results", len(d.Runs[0].Tool.Driver.Rules))
	}
}

func TestRenderCleanRunIsExecutionSuccessful(t *testing.T) {
	b, err := Render(sampleVerdict(), "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	d := parse(t, b)
	if !d.Runs[0].Invocations[0].ExecutionSuccessful {
		t.Error("a run with no blind spots must be executionSuccessful: true even when it found problems")
	}
}

func TestRenderPartialReadIsANotificationAndFailsTheInvocation(t *testing.T) {
	v := sampleVerdict()
	v.Verdict, v.Code = "inconclusive", gate.CodeInconclusive
	v.Inconclusive = []gate.Blindspot{{Resource: "pods", Reason: "forbidden"}}

	b, err := Render(v, "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	inv := parse(t, b).Runs[0].Invocations[0]
	if inv.ExecutionSuccessful {
		t.Error("executionSuccessful must be false when kubeagent could not see the cluster; an upload must not look clean")
	}
	if len(inv.ToolConfigurationNotifications) != 1 {
		t.Fatalf("toolConfigurationNotifications has %d entries, want 1", len(inv.ToolConfigurationNotifications))
	}
	n := inv.ToolConfigurationNotifications[0]
	if n.Descriptor.ID != "partial-read" || n.Level != "error" {
		t.Errorf("notification = %+v, want a partial-read error", n)
	}
	if n.Message.Text != "could not read pods: forbidden" {
		t.Errorf("notification message = %q", n.Message.Text)
	}
}

func TestRenderWaivedBlindSpotDoesNotFailTheInvocation(t *testing.T) {
	v := sampleVerdict()
	v.Inconclusive = []gate.Blindspot{{Resource: "leases", Reason: "forbidden", Waived: true}}

	b, err := Render(v, "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	inv := parse(t, b).Runs[0].Invocations[0]
	if !inv.ExecutionSuccessful {
		t.Error("an explicitly waived blind spot must not mark the invocation unsuccessful")
	}
	if len(inv.ToolConfigurationNotifications) != 1 {
		t.Errorf("a waived blind spot must still be reported, as a notification")
	}
}

func TestRenderTimeoutFailsTheInvocation(t *testing.T) {
	v := sampleVerdict()
	v.Verdict, v.Code = "timeout", gate.CodeTimeout

	b, err := Render(v, "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if parse(t, b).Runs[0].Invocations[0].ExecutionSuccessful {
		t.Error("a timed-out rollout never produced a judgement; executionSuccessful must be false")
	}
}

func TestRenderEndsWithANewline(t *testing.T) {
	b, err := Render(sampleVerdict(), "v0.65.0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Error("rendered SARIF must end in a newline so a shell redirect produces a well-formed file")
	}
}

func TestRenderIgnoresTheGateSchemaVersion(t *testing.T) {
	v := gate.Verdict{Verdict: "pass", Code: 0, Scope: "cluster"}
	without, err := Render(v, "v0.0.0-test")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	v.SchemaVersion = "1.0"
	with, err := Render(v, "v0.0.0-test")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Equal(without, with) {
		t.Error("the gate's schemaVersion changed the SARIF output; SARIF is versioned by OASIS")
	}
}
