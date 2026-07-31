package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/policy"
)

func policyView() *PolicyView {
	return &PolicyView{
		Rules: 3,
		Violations: []policy.Violation{
			{
				RuleID: "registry-allowlist", Level: policy.LevelCritical,
				Kind: "Pod", Namespace: "prod", Name: "web",
				Message:  "image is not from an allowed registry",
				Evidence: "docker.example.net/app:1.0",
			},
			{
				RuleID: "prod-deployments-need-a-pdb", Level: policy.LevelWarning,
				Kind: "Deployment", Namespace: "prod", Name: "api",
				Message: "no PodDisruptionBudget covers this Deployment",
			},
		},
		NotEvaluated: []policy.Unevaluated{{
			RuleID: "storage-class-is-encrypted", Level: policy.LevelInfo,
			Kind:   "StorageClass",
			Reason: "kubeagent could not read this kind, so the rule was not evaluated",
		}},
	}
}

// The whole point of the nil-able field: a scan with no --policy renders and
// encodes exactly what it did before this sub-project.
func TestNoPolicyRendersNothingAndEncodesNoKey(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "POLICY") {
		t.Error("a scan with no policy printed a POLICY section")
	}

	buf.Reset()
	if err := PrintInventory(Input{}, "json", &buf); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, present := doc["policy"]; present {
		t.Error("a scan with no policy emitted a policy key")
	}
}

func TestPolicySectionRendersViolationsAndBlindRules(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Policy: policyView()}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"POLICY",
		"3 rules",
		"1 critical",
		"1 warning",
		"registry-allowlist",
		"Pod prod/web",
		"image is not from an allowed registry",
		"docker.example.net/app:1.0",
		"prod-deployments-need-a-pdb",
		"Deployment prod/api",
		"not evaluated",
		"storage-class-is-encrypted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("POLICY section is missing %q\n%s", want, out)
		}
	}
}

// A rule that matched nothing is not a violation and must not be listed. The
// section only appears when there is something to say.
func TestPolicySectionIsSilentWhenEverythingPassed(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Policy: &PolicyView{Rules: 4}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "POLICY") {
		t.Fatalf("a clean policy run must still say it ran\n%s", out)
	}
	if !strings.Contains(out, "4 rules") || !strings.Contains(out, "no violations") {
		t.Errorf("a clean policy run must name the rule count and the verdict\n%s", out)
	}
}

// A cluster-scoped violation has no namespace. "Node /worker-1" would be a
// rendering bug that reads as a missing value.
func TestClusterScopedViolationRendersWithoutANamespace(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Policy: &PolicyView{Rules: 1, Violations: []policy.Violation{{
		RuleID: "nodes-are-labelled", Level: policy.LevelInfo,
		Kind: "Node", Name: "worker-1", Message: "no topology label",
	}}}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Node worker-1") {
		t.Errorf("cluster-scoped violation rendered oddly\n%s", out)
	}
	if strings.Contains(out, "Node /worker-1") {
		t.Errorf("an empty namespace leaked a stray separator\n%s", out)
	}
}

func TestPolicyJSONCarriesTheWholeView(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{Policy: policyView()}, "json", &buf); err != nil {
		t.Fatal(err)
	}
	var doc ScanReport
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Policy == nil {
		t.Fatal("policy view missing from the JSON document")
	}
	if doc.Policy.Rules != 3 || len(doc.Policy.Violations) != 2 || len(doc.Policy.NotEvaluated) != 1 {
		t.Fatalf("policy view = %#v", doc.Policy)
	}
	if doc.Policy.Violations[0].Evidence != "docker.example.net/app:1.0" {
		t.Errorf("evidence did not survive the round trip: %q", doc.Policy.Violations[0].Evidence)
	}
}

// A policy file's path is a credential. Nothing in the rendered document may
// carry one, in either format.
func TestNoPolicyPathReachesTheReport(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		var buf bytes.Buffer
		if err := PrintInventory(Input{Policy: policyView()}, format, &buf); err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{"/etc/", "/home/", ".yaml", ".yml"} {
			if strings.Contains(buf.String(), needle) {
				t.Errorf("%s output contains %q — a policy path must never be rendered", format, needle)
			}
		}
	}
}
