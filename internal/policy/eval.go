package policy

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// evidenceLimit caps how much of a failing value is quoted back. A report is
// meant to be read; a 4 KiB annotation pasted into a terminal is not.
const evidenceLimit = 120

// unreadableReason is kubeagent's own words for why a rule did not run. It
// never quotes the API server: a refusal reason is kubeagent's to phrase.
const unreadableReason = "kubeagent could not read this kind, so the rule was not evaluated"

// unreadableSupportReason covers the second way a rule can go unevaluated: the
// selected kind was read, but the objects the rule compares against were not.
// A hasPodDisruptionBudget rule with no PDB list would otherwise report every
// workload as uncovered, which is a fabricated violation rather than a blind
// spot — the same failure as a silent pass, only louder.
const unreadableSupportReason = "kubeagent could not read the objects this rule compares against, so the rule was not evaluated"

// unreadableFor reports kubeagent's own reason why a rule cannot run, or "" if
// it can. It never quotes the API server.
func unreadableFor(r Rule, in Inputs) string {
	if in.Unreadable[r.Match.Kind] {
		return unreadableReason
	}
	if len(r.Match.NamespaceLabels) > 0 && in.Unreadable["Namespace"] {
		return unreadableSupportReason
	}
	switch r.Assert.Relation {
	case RelationHasPDB:
		if in.Unreadable["PodDisruptionBudget"] {
			return unreadableSupportReason
		}
	case RelationHasHPA:
		if in.Unreadable["HorizontalPodAutoscaler"] {
			return unreadableSupportReason
		}
	}
	return ""
}

// Evaluate applies every rule to the objects it was handed and returns the
// violations plus the rules that could not be evaluated at all.
//
// A rule whose kind was unreadable is reported as not evaluated, never as
// passing: a refused read is a blind spot, and a blind spot that renders as a
// clean bill of health is worse than no check.
//
// One resource produces at most one violation per rule — the first failing
// slot wins. Output is sorted, so two runs over the same cluster render the
// same bytes.
func Evaluate(rules []Rule, in Inputs) (violations []Violation, notEvaluated []Unevaluated) {
	nsLabels := namespaceLabelIndex(in.Namespaces)

	for _, r := range rules {
		if reason := unreadableFor(r, in); reason != "" {
			notEvaluated = append(notEvaluated, Unevaluated{
				RuleID: r.ID,
				Level:  r.Level,
				Kind:   r.Match.Kind,
				Reason: reason,
			})
			continue
		}
		// The loader already accepted this path, so a parse failure here
		// would be a bug rather than bad input. Skip rather than panic.
		var segs []segment
		if r.Assert.Path != "" {
			parsed, err := parsePath(r.Assert.Path)
			if err != nil {
				continue
			}
			segs = parsed
		}
		for _, obj := range in.Objects[r.Match.Kind] {
			if obj == nil || !matches(r.Match, obj, nsLabels) {
				continue
			}
			if v, ok := check(r, segs, obj, in); ok {
				violations = append(violations, v)
			}
		}
	}

	sort.SliceStable(violations, func(i, j int) bool {
		a, b := violations[i], violations[j]
		switch {
		case a.RuleID != b.RuleID:
			return a.RuleID < b.RuleID
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		case a.Namespace != b.Namespace:
			return a.Namespace < b.Namespace
		default:
			return a.Name < b.Name
		}
	})
	sort.SliceStable(notEvaluated, func(i, j int) bool {
		return notEvaluated[i].RuleID < notEvaluated[j].RuleID
	})
	return violations, notEvaluated
}

// check applies one rule to one already-matched object, returning the
// violation if there is one.
func check(r Rule, segs []segment, obj *unstructured.Unstructured, in Inputs) (Violation, bool) {
	if r.Assert.Relation != "" {
		if relationHolds(r.Assert.Relation, obj, in) {
			return Violation{}, false
		}
		// A relation violation has no field to quote.
		return violationFor(r, obj, ""), true
	}

	// walk rather than resolve: the answer is the FIRST slot that violates, so
	// the traversal stops there. A wildcard over a large object names one slot
	// per element, and materializing all of them to read one is work no
	// verdict depends on.
	var (
		anySlot   bool
		violation Violation
		violated  bool
	)
	walk(obj.Object, segs, func(s Slot) bool {
		anySlot = true
		ok, skip := checkOp(r.Assert.Op, s, r.Assert.Values)
		if skip || ok {
			return true
		}
		violation, violated = violationFor(r, obj, evidence(s)), true
		return false
	})
	if violated {
		return violation, true
	}
	if !anySlot && r.Assert.Op == OpExists {
		// Nothing to assert about. `exists` still violates — something was
		// required and there is nowhere it could be.
		return violationFor(r, obj, ""), true
	}
	return Violation{}, false
}

func violationFor(r Rule, obj *unstructured.Unstructured, ev string) Violation {
	return Violation{
		RuleID:    r.ID,
		Level:     r.Level,
		Kind:      r.Match.Kind,
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
		Message:   r.Message,
		Evidence:  ev,
	}
}

// evidence renders the failing value for a human, sanitized at ingress and
// capped. An absent slot has nothing to show.
//
// Note the ordering: matching already happened, on the RAW value. Sanitizing
// before matching would let a control character spliced mid-word evade a glob.
func evidence(s Slot) string {
	if !s.Present {
		return ""
	}
	raw, ok := stringOf(s.Value)
	if !ok {
		return ""
	}
	clean := []rune(safetext.Line(raw))
	if len(clean) > evidenceLimit {
		clean = clean[:evidenceLimit]
	}
	return string(clean)
}

// matches reports whether a rule's match block selects this object.
func matches(m Match, obj *unstructured.Unstructured, nsLabels map[string]map[string]string) bool {
	if len(m.Namespaces) > 0 {
		ns := obj.GetNamespace()
		found := false
		for _, want := range m.Namespaces {
			if ns == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(m.Labels) > 0 && !subset(m.Labels, obj.GetLabels()) {
		return false
	}
	if len(m.NamespaceLabels) > 0 {
		labels, known := nsLabels[obj.GetNamespace()]
		if !known {
			// The Namespace was not read, so kubeagent cannot say the
			// selector matches. Matching blindly would invent a finding.
			return false
		}
		if !subset(m.NamespaceLabels, labels) {
			return false
		}
	}
	return true
}

// namespaceLabelIndex maps a namespace name to its labels, once, rather than
// re-scanning the namespace list for every object.
func namespaceLabelIndex(namespaces []*unstructured.Unstructured) map[string]map[string]string {
	out := make(map[string]map[string]string, len(namespaces))
	for _, ns := range namespaces {
		if ns == nil {
			continue
		}
		out[ns.GetName()] = ns.GetLabels()
	}
	return out
}
