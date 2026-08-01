package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return Document{Source: name, Data: data}
}

func TestLoadAcceptsAValidPolicy(t *testing.T) {
	rules, err := Load([]Document{readFixture(t, "valid.yaml")})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3", len(rules))
	}
	if rules[0].ID != "registry-allowlist" || rules[0].Assert.Op != OpMatches {
		t.Errorf("rule 0 = %#v", rules[0])
	}
	if rules[1].Assert.Relation != RelationHasPDB {
		t.Errorf("rule 1 relation = %q", rules[1].Assert.Relation)
	}
	if rules[2].Level != LevelWarning {
		t.Errorf("rule 2 level = %q", rules[2].Level)
	}
}

func TestLoadDetectsADuplicateIDAcrossFiles(t *testing.T) {
	_, err := Load([]Document{readFixture(t, "valid.yaml"), readFixture(t, "second.yaml")})
	if err == nil {
		t.Fatal("want an error for a duplicate rule id")
	}
	msg := err.Error()
	for _, want := range []string{"second.yaml", "registry-allowlist", "valid.yaml"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
}

// TestConfigMapDataPathIsALoadError pins the spec's second security exclusion.
// Without it, a policy file landed in a repo, run under `gate --output sarif`
// and uploaded to a code-scanning dashboard, is an exfiltration channel.
func TestConfigMapDataPathIsALoadError(t *testing.T) {
	// Every spelling of the same read, including the two bracket forms a
	// string-prefix check would miss.
	for _, path := range []string{
		"data", "data.token", "binaryData", "binaryData.blob",
		`data["token"]`, "data[*]", `binaryData["blob"]`,
	} {
		// path is single-quoted: several spellings contain [ and ], which
		// are flow indicators and must be quoted inside a flow mapping —
		// unquoted, YAML fails to parse the document at all, which would
		// test nothing.
		doc := Document{Source: "cm.yaml", Data: []byte(`
- id: read-configmap
  match: {kind: ConfigMap}
  assert: {path: '` + path + `', op: exists}
  level: info
  message: reads a ConfigMap value
`)}
		_, err := Load([]Document{doc})
		if err == nil {
			t.Errorf("path %q on a ConfigMap must be a load error", path)
			continue
		}
		if !strings.Contains(err.Error(), "evidence") {
			t.Errorf("path %q: error %q should explain that a violation would carry the contents as evidence", path, err)
		}
	}
	// The same path on another kind is fine — only ConfigMap holds
	// operator-supplied data under those keys.
	doc := Document{Source: "pod.yaml", Data: []byte(`
- id: pod-data
  match: {kind: Pod}
  assert: {path: data.whatever, op: notExists}
  level: info
  message: not a ConfigMap, so not restricted
`)}
	if _, err := Load([]Document{doc}); err != nil {
		t.Errorf("a data. path on a Pod is not restricted: %v", err)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring the error must contain
	}{
		{"not yaml", "\t- : :\n  ::", "invalid YAML"},
		{"unknown field", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
  sevrity: high
`, "sevrity"},
		{"no id", `
- match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
`, "no id"},
		{"bad id charset", `
- id: "reg allow!"
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
`, "letters, digits"},
		{"unknown kind", `
- id: x
  match: {kind: Secret}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
`, "not one of the kinds"},
		{"namespaceLabels on a cluster-scoped kind", `
- id: x
  match: {kind: Node, namespaceLabels: {tier: prod}}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
`, "cluster-scoped"},
		{"both path and relation", `
- id: x
  match: {kind: Deployment}
  assert: {path: metadata.name, op: exists, relation: hasPodDisruptionBudget}
  level: info
  message: m
`, "exactly one"},
		{"neither path nor relation", `
- id: x
  match: {kind: Deployment}
  assert: {}
  level: info
  message: m
`, "exactly one"},
		{"unknown operator", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: regex, values: ["a"]}
  level: info
  message: m
`, "unknown operator"},
		{"bad path", `
- id: x
  match: {kind: Pod}
  assert: {path: "spec.containers[0].image", op: exists}
  level: info
  message: m
`, "[*]"},
		{"exists takes no values", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists, values: ["a"]}
  level: info
  message: m
`, "takes no values"},
		{"in needs values", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: in}
  level: info
  message: m
`, "at least one value"},
		{"lte takes exactly one value", `
- id: x
  match: {kind: Pod}
  assert: {path: "spec.containers[*].resources.limits.memory", op: lte, values: ["1Gi", "2Gi"]}
  level: info
  message: m
`, "exactly one value"},
		{"unknown relation", `
- id: x
  match: {kind: Deployment}
  assert: {relation: hasNetworkPolicy}
  level: info
  message: m
`, "unknown relation"},
		{"relation on an invalid kind", `
- id: x
  match: {kind: DaemonSet}
  assert: {relation: hasHorizontalPodAutoscaler}
  level: info
  message: m
`, "does not apply to kind"},
		{"relation takes no values", `
- id: x
  match: {kind: Deployment}
  assert: {relation: hasPodDisruptionBudget, values: ["a"]}
  level: info
  message: m
`, "takes no values"},
		{"unknown level", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: fatal
  message: m
`, "unknown level"},
		{"empty message", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: info
  message: ""
`, "message is empty"},
	}
	for _, c := range cases {
		_, err := Load([]Document{{Source: "p.yaml", Data: []byte(c.yaml)}})
		if err == nil {
			t.Errorf("%s: want an error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not contain %q", c.name, err, c.want)
		}
		if !strings.Contains(err.Error(), "p.yaml") {
			t.Errorf("%s: error %q does not name the file", c.name, err)
		}
	}
}

// TestLoadSanitizesTheMessage: a message is operator-authored, but it ends up
// on a terminal, so it is sanitized at ingress like any other untrusted line.
func TestLoadSanitizesTheMessage(t *testing.T) {
	// A YAML double-quoted scalar interprets a backslash-u escape, so the
	// message arrives carrying a real ESC byte.
	src := "- id: x\n" +
		"  match: {kind: Pod}\n" +
		"  assert: {path: metadata.name, op: exists}\n" +
		"  level: info\n" +
		"  message: \"bad\\u001b[2Jmessage\"\n"
	rules, err := Load([]Document{{Source: "p.yaml", Data: []byte(src)}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.ContainsRune(rules[0].Message, '\x1b') {
		t.Errorf("message was not sanitized: %q", rules[0].Message)
	}
}

func TestKindsAndNeeds(t *testing.T) {
	rules, err := Load([]Document{readFixture(t, "valid.yaml")})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	kinds := Kinds(rules)
	if len(kinds) != 2 || kinds[0] != "Deployment" || kinds[1] != "Pod" {
		t.Errorf("Kinds = %v, want [Deployment Pod] sorted and deduplicated", kinds)
	}
	need := Needs(rules)
	if !need.Namespaces {
		t.Error("namespaceLabels is used, so Namespaces must be needed")
	}
	if !need.PDBs {
		t.Error("hasPodDisruptionBudget is used, so PDBs must be needed")
	}
	if need.HPAs {
		t.Error("no rule uses hasHorizontalPodAutoscaler, so HPAs must not be needed")
	}
}

func TestNeedsIsEmptyForAPathOnlyPolicy(t *testing.T) {
	rules, err := Load([]Document{{Source: "p.yaml", Data: []byte(`
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
`)}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if need := Needs(rules); need.Namespaces || need.PDBs || need.HPAs {
		t.Errorf("Needs = %#v, want all false", need)
	}
}
