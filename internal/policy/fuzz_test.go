package policy

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// FuzzLoadPolicy asserts that no byte sequence in a policy file can panic the
// loader, and that anything it accepts is internally consistent — a rule that
// loads must be one Evaluate can run without reaching for a value the loader
// promised was there.
func FuzzLoadPolicy(f *testing.F) {
	f.Add("- id: x\n  match: {kind: Pod}\n  assert: {path: metadata.name, op: exists}\n  level: info\n  message: m\n")
	f.Add("- id: r\n  match: {kind: Deployment}\n  assert: {relation: hasPodDisruptionBudget}\n  level: warning\n  message: m\n")
	f.Add("[]")
	f.Add("")
	f.Add("- id: x\n  match: {kind: ConfigMap}\n  assert: {path: data.token, op: exists}\n  level: info\n  message: m\n")
	f.Add("- id: \x00\n  match: {kind: Pod}\n")

	f.Fuzz(func(t *testing.T, src string) {
		rules, err := Load([]Document{{Source: "fuzz.yaml", Data: []byte(src)}})
		if err != nil {
			if rules != nil {
				t.Errorf("Load returned rules alongside an error")
			}
			return
		}
		for _, r := range rules {
			if r.ID == "" {
				t.Errorf("an accepted rule has no id")
			}
			if !KindSelectable(r.Match.Kind) {
				t.Errorf("an accepted rule selects %q, which is not selectable", r.Match.Kind)
			}
			if r.Match.Kind == "Secret" {
				t.Errorf("Secret was accepted as a selectable kind")
			}
			if safetext.Line(r.Message) != r.Message {
				t.Errorf("an accepted rule's message is not sanitized: %q", r.Message)
			}
			hasPath := r.Assert.Path != ""
			hasRelation := r.Assert.Relation != ""
			if hasPath == hasRelation {
				t.Errorf("an accepted rule sets both or neither of path and relation")
			}
			if hasPath {
				segs, perr := parsePath(r.Assert.Path)
				if perr != nil {
					t.Errorf("an accepted rule has an unparseable path %q: %v", r.Assert.Path, perr)
				} else if r.Match.Kind == "ConfigMap" && readsConfigMapContents(segs) {
					t.Errorf("a ConfigMap contents path was accepted: %q", r.Assert.Path)
				}
			}
		}
	})
}

// FuzzEvaluatePolicy asserts that no combination of a loadable policy and
// hostile object text can panic a scan, that evidence never carries a raw byte
// from the cluster to a terminal, and that evaluation is deterministic.
func FuzzEvaluatePolicy(f *testing.F) {
	f.Add("- id: x\n  match: {kind: Pod}\n  assert: {path: \"spec.containers[*].image\", op: matches, values: [\"registry.example.com/*\"]}\n  level: critical\n  message: m\n",
		"registry.example.com/app:1.0", "prod", "tier")
	f.Add("- id: x\n  match: {kind: Pod}\n  assert: {path: \"spec.containers[*].resources.limits.cpu\", op: exists}\n  level: warning\n  message: m\n",
		"\x1b[2Japp", "\x00ns", "")

	f.Fuzz(func(t *testing.T, src, image, namespace, nsLabel string) {
		rules, err := Load([]Document{{Source: "fuzz.yaml", Data: []byte(src)}})
		if err != nil {
			return
		}
		obj := &unstructured.Unstructured{Object: map[string]any{
			"kind": "Pod",
			"metadata": map[string]any{
				"name":      "fuzz",
				"namespace": namespace,
				"labels":    map[string]any{"app": image},
			},
			"spec": map[string]any{"containers": []any{
				map[string]any{"name": "a", "image": image},
				map[string]any{"name": "b"},
			}},
		}}
		ns := &unstructured.Unstructured{Object: map[string]any{
			"kind":     "Namespace",
			"metadata": map[string]any{"name": namespace, "labels": map[string]any{"tier": nsLabel}},
		}}
		in := Inputs{
			Objects:    map[string][]*unstructured.Unstructured{"Pod": {obj}},
			Namespaces: []*unstructured.Unstructured{ns},
		}

		violations, notEvaluated := Evaluate(rules, in)

		seen := map[string]bool{}
		for _, v := range violations {
			if safetext.Line(v.Evidence) != v.Evidence {
				t.Errorf("evidence carries an unsanitized byte: %q", v.Evidence)
			}
			if n := len([]rune(v.Evidence)); n > evidenceLimit {
				t.Errorf("evidence is %d runes, over the %d cap", n, evidenceLimit)
			}
			if safetext.Line(v.Message) != v.Message {
				t.Errorf("message carries an unsanitized byte: %q", v.Message)
			}
			key := v.RuleID + "\x1f" + v.Kind + "\x1f" + v.Namespace + "\x1f" + v.Name
			if seen[key] {
				t.Errorf("a resource produced two violations for one rule: %s", key)
			}
			seen[key] = true
		}
		for _, u := range notEvaluated {
			if u.Reason == "" {
				t.Errorf("an unevaluated rule carries no reason")
			}
		}

		// Determinism: the same inputs must produce the same output, in the
		// same order, or the rendered report is a function of map iteration.
		again, againNot := Evaluate(rules, in)
		if len(again) != len(violations) || len(againNot) != len(notEvaluated) {
			t.Fatalf("Evaluate is not deterministic: %d/%d then %d/%d",
				len(violations), len(notEvaluated), len(again), len(againNot))
		}
		for i := range violations {
			if again[i] != violations[i] {
				t.Fatalf("violation %d differs between runs: %#v vs %#v", i, violations[i], again[i])
			}
		}
	})
}

// FuzzResolvePath asserts that no path string can panic the resolver, and
// pins the arity invariant: a path with no wildcard always resolves to
// exactly one slot, present or absent.
func FuzzResolvePath(f *testing.F) {
	f.Add("metadata.name")
	f.Add("spec.containers[*].image")
	f.Add("spec.containers[*].ports[*].containerPort")
	f.Add("")
	f.Add("...")
	f.Add("a[0].b")
	f.Add("\x00.\x00")

	obj := map[string]any{
		"metadata": map[string]any{"name": "web", "labels": map[string]any{"app": "web"}},
		"spec": map[string]any{"containers": []any{
			map[string]any{"image": "app:1.0"},
			map[string]any{},
		}},
	}

	f.Fuzz(func(t *testing.T, path string) {
		segs, err := parsePath(path)
		if err != nil {
			return
		}
		slots := resolve(obj, segs)
		wildcards := 0
		for _, s := range segs {
			if s.wildcard {
				wildcards++
			}
		}
		if wildcards == 0 && len(slots) != 1 {
			t.Errorf("path %q has no wildcard but resolved to %d slots", path, len(slots))
		}
		for _, s := range slots {
			if !s.Present && s.Value != nil {
				t.Errorf("an absent slot carries a value: %#v", s.Value)
			}
		}
	})
}
