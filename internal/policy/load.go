package policy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// Document is one policy file's bytes plus the name to use in an error. That
// name reaches stderr only: a policy path is a filesystem path, and
// filesystem paths are credentials — none may appear in JSON, SARIF or the
// HTML report.
type Document struct {
	Source string
	Data   []byte
}

// Aux names the auxiliary reads an evaluation needs beyond the selected kinds.
type Aux struct {
	Namespaces bool // any rule uses match.namespaceLabels
	PDBs       bool // any rule asserts hasPodDisruptionBudget
	HPAs       bool // any rule asserts hasHorizontalPodAutoscaler
}

// ruleIDPattern keeps a rule id to characters that are safe as a SARIF rule
// id, as a value in a JSON document, and as a line of terminal output.
var ruleIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Load decodes and validates every document, returning the rules in file
// order. It is fail-fast: the first problem stops the load, and no cluster
// call has happened yet, so a malformed policy costs nothing but the read.
//
// Taking every document at once is what lets a duplicate rule id be caught
// across files rather than only within one.
func Load(docs []Document) ([]Rule, error) {
	var out []Rule
	seen := map[string]string{} // rule id -> the document that defined it
	for _, doc := range docs {
		var rules []Rule
		if err := yaml.UnmarshalStrict(doc.Data, &rules); err != nil {
			return nil, fmt.Errorf("%s: invalid YAML: %w", doc.Source, err)
		}
		for i, r := range rules {
			if r.ID == "" {
				return nil, fmt.Errorf("%s: rule %d has no id", doc.Source, i+1)
			}
			if !ruleIDPattern.MatchString(r.ID) {
				return nil, fmt.Errorf("%s: rule id %q may use only letters, digits, dot, dash and underscore", doc.Source, r.ID)
			}
			if where, dup := seen[r.ID]; dup {
				return nil, fmt.Errorf("%s: rule id %q is already defined in %s", doc.Source, r.ID, where)
			}
			if err := validate(&r); err != nil {
				return nil, fmt.Errorf("%s: rule %q: %w", doc.Source, r.ID, err)
			}
			seen[r.ID] = doc.Source
			// Sanitize at ingress: the message reaches a terminal, a JSON
			// document and an HTML report.
			r.Message = safetext.Line(r.Message)
			out = append(out, r)
		}
	}
	return out, nil
}

// validate checks one rule. The error names no file — Load adds the file and
// the rule id.
func validate(r *Rule) error {
	if !KindSelectable(r.Match.Kind) {
		return fmt.Errorf("kind %q is not one of the kinds a policy may select", r.Match.Kind)
	}
	// A Node has no namespace, so either selector could only ever match
	// nothing. Silently matching nothing is worse than refusing to load.
	if len(r.Match.NamespaceLabels) > 0 || len(r.Match.Namespaces) > 0 {
		if namespaced, _ := KindNamespaced(r.Match.Kind); !namespaced {
			return fmt.Errorf("kind %q is cluster-scoped, so namespaces and namespaceLabels can never match", r.Match.Kind)
		}
	}
	hasPath := r.Assert.Path != ""
	hasRelation := r.Assert.Relation != ""
	if hasPath == hasRelation {
		return fmt.Errorf("assert must set exactly one of path and relation")
	}
	if hasRelation {
		if err := validateRelation(r); err != nil {
			return err
		}
	} else if err := validatePath(r); err != nil {
		return err
	}
	if !ValidLevel(r.Level) {
		return fmt.Errorf("unknown level %q", r.Level)
	}
	if strings.TrimSpace(r.Message) == "" {
		return fmt.Errorf("message is empty")
	}
	return nil
}

func validateRelation(r *Rule) error {
	if !ValidRelation(r.Assert.Relation) {
		return fmt.Errorf("unknown relation %q", r.Assert.Relation)
	}
	if r.Assert.Op != "" {
		return fmt.Errorf("relation %q takes no op", r.Assert.Relation)
	}
	if len(r.Assert.Values) > 0 {
		return fmt.Errorf("relation %q takes no values", r.Assert.Relation)
	}
	if !RelationValidForKind(r.Assert.Relation, r.Match.Kind) {
		return fmt.Errorf("relation %q does not apply to kind %q", r.Assert.Relation, r.Match.Kind)
	}
	return nil
}

func validatePath(r *Rule) error {
	if !ValidOp(r.Assert.Op) {
		return fmt.Errorf("unknown operator %q", r.Assert.Op)
	}
	segs, err := parsePath(r.Assert.Path)
	if err != nil {
		return err
	}
	if r.Match.Kind == "ConfigMap" && readsConfigMapContents(segs) {
		return fmt.Errorf("a ConfigMap policy may not read data or binaryData — a violation would carry the contents as evidence")
	}
	return validateArity(r.Assert.Op, r.Assert.Values)
}

// readsConfigMapContents reports whether a path reaches into a ConfigMap's
// operator-supplied contents. Both the bare key and any subpath are refused: a
// ConfigMap routinely holds a token or a connection string that nobody
// remembered was not a Secret.
func readsConfigMapContents(segs []segment) bool {
	if len(segs) == 0 {
		return false
	}
	// The first segment decides it, whatever the rest spells. Matching on the
	// raw string would miss data["key"] and data[*], which read exactly the
	// same contents as data.key.
	return segs[0].key == "data" || segs[0].key == "binaryData"
}

func validateArity(op Op, values []string) error {
	switch op {
	case OpExists, OpNotExists:
		if len(values) > 0 {
			return fmt.Errorf("operator %q takes no values", op)
		}
	case OpIn, OpNotIn, OpMatches, OpNotMatches:
		if len(values) == 0 {
			return fmt.Errorf("operator %q needs at least one value", op)
		}
	case OpGt, OpGte, OpLt, OpLte:
		if len(values) != 1 {
			return fmt.Errorf("operator %q takes exactly one value, got %d", op, len(values))
		}
	}
	return nil
}

// Kinds returns every kind the rules select, sorted and deduplicated. The
// caller reads the cluster in this order, so the order is part of the
// determinism contract.
func Kinds(rules []Rule) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		if !seen[r.Match.Kind] {
			seen[r.Match.Kind] = true
			out = append(out, r.Match.Kind)
		}
	}
	sort.Strings(out)
	return out
}

// Needs reports which auxiliary reads the rules require. Skipping an unneeded
// read is not cleverness — it is not asking the API server for something no
// rule will look at.
func Needs(rules []Rule) Aux {
	var a Aux
	for _, r := range rules {
		if len(r.Match.NamespaceLabels) > 0 {
			a.Namespaces = true
		}
		switch r.Assert.Relation {
		case RelationHasPDB:
			a.PDBs = true
		case RelationHasHPA:
			a.HPAs = true
		}
	}
	return a
}
