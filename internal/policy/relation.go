package policy

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// relationHolds reports whether a cross-resource relation is true of obj. A
// rule asserting a relation violates when this returns false.
//
// Both relations are deliberately shallow: they answer "is there an object of
// this kind pointing at this workload", not "does that object do anything
// useful". A PodDisruptionBudget with maxUnavailable: 100% still counts as
// covering the Deployment — whether the budget is meaningful is a separate
// rule an operator can write with a path assertion.
func relationHolds(rel Relation, obj *unstructured.Unstructured, in Inputs) bool {
	switch rel {
	case RelationHasPDB:
		return coveredByPDB(obj, in.PDBs)
	case RelationHasHPA:
		return targetedByHPA(obj, in.HPAs)
	default:
		return false
	}
}

// coveredByPDB reports whether any PodDisruptionBudget in the workload's
// namespace selects its pod template.
//
// Only matchLabels is evaluated. A selector that relies solely on
// matchExpressions does not cover the workload — kubeagent says "no PDB
// covers this" rather than guessing at set-based semantics it does not
// implement.
func coveredByPDB(obj *unstructured.Unstructured, pdbs []*unstructured.Unstructured) bool {
	ns := obj.GetNamespace()
	labels := podTemplateLabels(obj)
	for _, p := range pdbs {
		if p == nil || p.GetNamespace() != ns {
			continue
		}
		selector, found, err := unstructured.NestedMap(p.Object, "spec", "selector")
		if err != nil || !found {
			// A PDB with no selector selects nothing kubeagent can reason
			// about; ignore it rather than treat it as universal.
			continue
		}
		if exprs, ok := selector["matchExpressions"].([]any); ok && len(exprs) > 0 {
			continue
		}
		match, _, err := unstructured.NestedStringMap(p.Object, "spec", "selector", "matchLabels")
		if err != nil {
			continue
		}
		// An empty selector matches every pod in the namespace.
		if subset(match, labels) {
			return true
		}
	}
	return false
}

// targetedByHPA reports whether any HorizontalPodAutoscaler in the workload's
// namespace scales it.
func targetedByHPA(obj *unstructured.Unstructured, hpas []*unstructured.Unstructured) bool {
	ns, kind, name := obj.GetNamespace(), obj.GetKind(), obj.GetName()
	for _, h := range hpas {
		if h == nil || h.GetNamespace() != ns {
			continue
		}
		targetKind, _, err := unstructured.NestedString(h.Object, "spec", "scaleTargetRef", "kind")
		if err != nil {
			continue
		}
		targetName, _, err := unstructured.NestedString(h.Object, "spec", "scaleTargetRef", "name")
		if err != nil {
			continue
		}
		if targetKind == kind && targetName == name {
			return true
		}
	}
	return false
}

// podTemplateLabels returns the labels a workload stamps on the pods it
// creates — what a PodDisruptionBudget actually selects.
func podTemplateLabels(obj *unstructured.Unstructured) map[string]string {
	labels, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		return nil
	}
	return labels
}

// subset reports whether every key/value in want appears in have. An empty
// want is a subset of anything.
func subset(want, have map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
