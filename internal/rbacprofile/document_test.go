package rbacprofile

import (
	"encoding/json"
	"strings"
	"testing"
)

// The rbac outputs were bare JSON arrays, which can never carry a version
// field. Both are wrapped, and these are the wrappers.
func TestRulesDocumentShape(t *testing.T) {
	raw, err := json.Marshal(RulesDocument{
		SchemaVersion: "1.0",
		RoleName:      "kubeagent",
		Rules:         []Rule{{APIGroup: "", Resources: []string{"pods"}, Verbs: []string{"get", "list"}}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"schemaVersion":"1.0"`, `"roleName":"kubeagent"`, `"rules":[`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("document is missing %s:\n%s", want, raw)
		}
	}
}

func TestCheckDocumentShape(t *testing.T) {
	raw, err := json.Marshal(CheckDocument{
		SchemaVersion: "1.0",
		Features:      []FeatureStatus{{Name: "core", Summary: "list pods", Allowed: true}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"schemaVersion":"1.0"`, `"features":[`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("document is missing %s:\n%s", want, raw)
		}
	}
}
