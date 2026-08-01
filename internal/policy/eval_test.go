package policy

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func pod(namespace, name string, labels map[string]string, images ...string) *unstructured.Unstructured {
	containers := make([]any, 0, len(images))
	for i, img := range images {
		containers = append(containers, map[string]any{"name": string(rune('a' + i)), "image": img})
	}
	meta := map[string]any{"name": name, "namespace": namespace}
	if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		meta["labels"] = l
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"kind":     "Pod",
		"metadata": meta,
		"spec":     map[string]any{"containers": containers},
	}}
}

func namespaceObj(name string, labels map[string]string) *unstructured.Unstructured {
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"kind":     "Namespace",
		"metadata": map[string]any{"name": name, "labels": l},
	}}
}

func registryRule() Rule {
	return Rule{
		ID:      "registry-allowlist",
		Match:   Match{Kind: "Pod"},
		Assert:  Assert{Path: "spec.containers[*].image", Op: OpMatches, Values: []string{"registry.example.com/*"}},
		Level:   LevelCritical,
		Message: "image comes from a registry outside the allowlist",
	}
}

func TestEvaluateFlagsAViolatingResource(t *testing.T) {
	in := Inputs{Objects: map[string][]*unstructured.Unstructured{"Pod": {
		pod("prod", "good", nil, "registry.example.com/team/app:1.0"),
		pod("prod", "bad", nil, "docker.example.net/app:1.0"),
	}}}
	violations, notEvaluated := Evaluate([]Rule{registryRule()}, in)
	if len(notEvaluated) != 0 {
		t.Fatalf("nothing was unreadable, got %#v", notEvaluated)
	}
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want 1: %#v", len(violations), violations)
	}
	v := violations[0]
	if v.RuleID != "registry-allowlist" || v.Kind != "Pod" || v.Namespace != "prod" || v.Name != "bad" {
		t.Errorf("violation = %#v", v)
	}
	if v.Level != LevelCritical {
		t.Errorf("level = %q, want critical", v.Level)
	}
	if v.Evidence != "docker.example.net/app:1.0" {
		t.Errorf("evidence = %q, want the offending image", v.Evidence)
	}
}

// TestOneViolationPerResourcePerRule: a Pod with two bad images reports once,
// not twice. A report that lists the same rule against the same resource
// repeatedly buries the other findings.
func TestOneViolationPerResourcePerRule(t *testing.T) {
	in := Inputs{Objects: map[string][]*unstructured.Unstructured{"Pod": {
		pod("prod", "bad", nil, "docker.example.net/a:1.0", "docker.example.net/b:1.0"),
	}}}
	violations, _ := Evaluate([]Rule{registryRule()}, in)
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want exactly 1", len(violations))
	}
	if violations[0].Evidence != "docker.example.net/a:1.0" {
		t.Errorf("evidence = %q, want the FIRST failing slot", violations[0].Evidence)
	}
}

// TestPartiallyAbsentWildcardViolatesUnderExists is the evaluation-level twin
// of Task 3's slot test: three containers, one memory limit, one violation.
func TestPartiallyAbsentWildcardViolatesUnderExists(t *testing.T) {
	p := pod("prod", "web", nil, "app:1.0", "app:1.0", "app:1.0")
	containers, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
	c0 := containers[0].(map[string]any)
	c0["resources"] = map[string]any{"limits": map[string]any{"memory": "1Gi"}}
	_ = unstructured.SetNestedSlice(p.Object, containers, "spec", "containers")

	rule := Rule{
		ID:      "memory-limit-is-set",
		Match:   Match{Kind: "Pod"},
		Assert:  Assert{Path: "spec.containers[*].resources.limits.memory", Op: OpExists},
		Level:   LevelWarning,
		Message: "container sets no memory limit",
	}
	violations, _ := Evaluate([]Rule{rule}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Pod": {p}},
	})
	if len(violations) != 1 {
		t.Fatalf("two of three containers set no memory limit; want 1 violation, got %d", len(violations))
	}
	if violations[0].Evidence != "" {
		t.Errorf("an absent slot has no evidence, got %q", violations[0].Evidence)
	}
}

func TestZeroSlotsViolateOnlyUnderExists(t *testing.T) {
	// A Pod with no containers at all: spec.containers[*].image resolves to
	// zero slots.
	empty := pod("prod", "empty", nil)
	in := Inputs{Objects: map[string][]*unstructured.Unstructured{"Pod": {empty}}}

	existsRule := Rule{ID: "r", Match: Match{Kind: "Pod"},
		Assert: Assert{Path: "spec.containers[*].image", Op: OpExists},
		Level:  LevelInfo, Message: "no image"}
	if v, _ := Evaluate([]Rule{existsRule}, in); len(v) != 1 {
		t.Errorf("exists over zero slots must violate, got %d violations", len(v))
	}

	for _, op := range []Op{OpNotExists, OpIn, OpMatches, OpLte} {
		r := Rule{ID: "r", Match: Match{Kind: "Pod"},
			Assert: Assert{Path: "spec.containers[*].image", Op: op, Values: []string{"x"}},
			Level:  LevelInfo, Message: "m"}
		if op == OpNotExists {
			r.Assert.Values = nil
		}
		if v, _ := Evaluate([]Rule{r}, in); len(v) != 0 {
			t.Errorf("%s over zero slots must not violate, got %d", op, len(v))
		}
	}
}

func TestUnreadableKindIsReportedNotEvaluated(t *testing.T) {
	in := Inputs{Unreadable: map[string]bool{"Pod": true}}
	violations, notEvaluated := Evaluate([]Rule{registryRule()}, in)
	if len(violations) != 0 {
		t.Errorf("an unreadable kind produces no violations, got %d", len(violations))
	}
	if len(notEvaluated) != 1 {
		t.Fatalf("got %d unevaluated rules, want 1", len(notEvaluated))
	}
	u := notEvaluated[0]
	if u.RuleID != "registry-allowlist" || u.Kind != "Pod" || u.Level != LevelCritical {
		t.Errorf("unevaluated = %#v", u)
	}
	if u.Reason == "" {
		t.Error("an unevaluated rule must carry kubeagent's own reason")
	}
}

// The selected kind read fine but the list the rule compares against did not.
// Reporting every Deployment as uncovered would be a fabricated violation —
// the mirror image of a silent pass, and just as wrong.
func TestUnreadableSupportingListIsReportedNotEvaluated(t *testing.T) {
	dep := workload("Deployment", "prod", "web", map[string]string{"app": "web"})
	pdbRule := Rule{
		ID:      "prod-deployments-need-a-pdb",
		Match:   Match{Kind: "Deployment"},
		Assert:  Assert{Relation: RelationHasPDB},
		Level:   LevelWarning,
		Message: "no PodDisruptionBudget covers this Deployment",
	}
	nsRule := registryRule()
	nsRule.Match.NamespaceLabels = map[string]string{"tier": "prod"}

	cases := []struct {
		name       string
		rule       Rule
		objects    map[string][]*unstructured.Unstructured
		unreadable map[string]bool
	}{
		{"pdb list refused", pdbRule,
			map[string][]*unstructured.Unstructured{"Deployment": {dep}},
			map[string]bool{"PodDisruptionBudget": true}},
		{"namespace list refused", nsRule,
			map[string][]*unstructured.Unstructured{"Pod": {pod("prod", "a", nil, "docker.example.net/x:1.0")}},
			map[string]bool{"Namespace": true}},
	}
	for _, c := range cases {
		violations, notEvaluated := Evaluate([]Rule{c.rule}, Inputs{
			Objects: c.objects, Unreadable: c.unreadable,
		})
		if len(violations) != 0 {
			t.Errorf("%s: got %d violations, want 0 — an unread list is not evidence", c.name, len(violations))
		}
		if len(notEvaluated) != 1 {
			t.Fatalf("%s: got %d unevaluated rules, want 1", c.name, len(notEvaluated))
		}
		if notEvaluated[0].Reason != unreadableSupportReason {
			t.Errorf("%s: reason = %q", c.name, notEvaluated[0].Reason)
		}
	}
}

func TestMatchNarrowsByNamespaceLabelAndNamespaceLabels(t *testing.T) {
	pods := []*unstructured.Unstructured{
		pod("prod", "a", map[string]string{"app": "web"}, "docker.example.net/x:1.0"),
		pod("prod", "b", map[string]string{"app": "api"}, "docker.example.net/x:1.0"),
		pod("staging", "c", map[string]string{"app": "web"}, "docker.example.net/x:1.0"),
	}
	namespaces := []*unstructured.Unstructured{
		namespaceObj("prod", map[string]string{"tier": "prod"}),
		namespaceObj("staging", map[string]string{"tier": "dev"}),
	}

	byNamespace := registryRule()
	byNamespace.Match.Namespaces = []string{"prod"}
	byLabel := registryRule()
	byLabel.Match.Labels = map[string]string{"app": "web"}
	byNamespaceLabel := registryRule()
	byNamespaceLabel.Match.NamespaceLabels = map[string]string{"tier": "prod"}

	cases := []struct {
		name  string
		rule  Rule
		names []string
	}{
		{"namespaces", byNamespace, []string{"a", "b"}},
		{"labels", byLabel, []string{"a", "c"}},
		{"namespaceLabels", byNamespaceLabel, []string{"a", "b"}},
	}
	for _, c := range cases {
		violations, _ := Evaluate([]Rule{c.rule}, Inputs{
			Objects:    map[string][]*unstructured.Unstructured{"Pod": pods},
			Namespaces: namespaces,
		})
		var got []string
		for _, v := range violations {
			got = append(got, v.Name)
		}
		if strings.Join(got, ",") != strings.Join(c.names, ",") {
			t.Errorf("%s: matched %v, want %v", c.name, got, c.names)
		}
	}
}

// TestNamespaceLabelsWithNoNamespaceObjectMatchesNothing: if the Namespace
// read was skipped or refused, a namespaceLabels rule must not match blindly.
func TestNamespaceLabelsWithNoNamespaceObjectMatchesNothing(t *testing.T) {
	rule := registryRule()
	rule.Match.NamespaceLabels = map[string]string{"tier": "prod"}
	violations, _ := Evaluate([]Rule{rule}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Pod": {
			pod("prod", "a", nil, "docker.example.net/x:1.0"),
		}},
	})
	if len(violations) != 0 {
		t.Errorf("with no Namespace objects, a namespaceLabels rule matches nothing, got %d", len(violations))
	}
}

func TestRelationViolationCarriesNoEvidence(t *testing.T) {
	dep := workload("Deployment", "prod", "web", map[string]string{"app": "web"})
	rule := Rule{
		ID:      "prod-deployments-need-a-pdb",
		Match:   Match{Kind: "Deployment"},
		Assert:  Assert{Relation: RelationHasPDB},
		Level:   LevelWarning,
		Message: "no PodDisruptionBudget covers this Deployment",
	}
	violations, _ := Evaluate([]Rule{rule}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Deployment": {dep}},
	})
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want 1", len(violations))
	}
	if violations[0].Evidence != "" {
		t.Errorf("a relation violation has no field to quote, got %q", violations[0].Evidence)
	}
	// With a covering PDB it holds and there is no violation.
	violations, _ = Evaluate([]Rule{rule}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Deployment": {dep}},
		PDBs:    []*unstructured.Unstructured{pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web"}})},
	})
	if len(violations) != 0 {
		t.Errorf("a covering PDB satisfies the relation, got %d violations", len(violations))
	}
}

func TestEvidenceIsSanitizedAndTruncated(t *testing.T) {
	long := strings.Repeat("x", 300)
	p := pod("prod", "bad", nil, "docker.example.net/\x1b[2J"+long)
	violations, _ := Evaluate([]Rule{registryRule()}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Pod": {p}},
	})
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want 1", len(violations))
	}
	ev := violations[0].Evidence
	if strings.ContainsRune(ev, '\x1b') {
		t.Errorf("evidence was not sanitized: %q", ev)
	}
	if n := len([]rune(ev)); n > 120 {
		t.Errorf("evidence is %d runes, want at most 120", n)
	}
}

// TestMatchingRunsOnTheRawValue: sanitizing before matching would let a
// control character spliced mid-word evade a glob. The image below is NOT
// registry.example.com/app once the escape is accounted for, and must still
// be reported.
func TestMatchingRunsOnTheRawValue(t *testing.T) {
	p := pod("prod", "sneaky", nil, "registry.example.com\x1b/../docker.example.net/app:1.0")
	rule := registryRule()
	rule.Assert.Values = []string{"registry.example.com/*"}
	violations, _ := Evaluate([]Rule{rule}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Pod": {p}},
	})
	if len(violations) != 1 {
		t.Fatalf("the raw image does not match the allowlist glob; want 1 violation, got %d", len(violations))
	}
}

func TestEvaluateOutputIsSorted(t *testing.T) {
	pods := []*unstructured.Unstructured{
		pod("prod", "z", nil, "docker.example.net/x:1.0"),
		pod("alpha", "a", nil, "docker.example.net/x:1.0"),
		pod("prod", "a", nil, "docker.example.net/x:1.0"),
	}
	ruleB := registryRule()
	ruleB.ID = "b-rule"
	ruleA := registryRule()
	ruleA.ID = "a-rule"

	violations, _ := Evaluate([]Rule{ruleB, ruleA}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Pod": pods},
	})
	var got []string
	for _, v := range violations {
		got = append(got, v.RuleID+"/"+v.Namespace+"/"+v.Name)
	}
	want := []string{
		"a-rule/alpha/a", "a-rule/prod/a", "a-rule/prod/z",
		"b-rule/alpha/a", "b-rule/prod/a", "b-rule/prod/z",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("order = %v\nwant  = %v", got, want)
	}
}

func TestEvaluateHandlesNilInputsWithoutPanicking(t *testing.T) {
	if v, n := Evaluate(nil, Inputs{}); len(v) != 0 || len(n) != 0 {
		t.Errorf("no rules means no output, got %d/%d", len(v), len(n))
	}
	if v, _ := Evaluate([]Rule{registryRule()}, Inputs{}); len(v) != 0 {
		t.Errorf("no objects means no violations, got %d", len(v))
	}
}
