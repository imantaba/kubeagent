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
//
// The selector is read with unstructured.NestedFieldNoCopy, not NestedMap:
// NestedMap deep-copies the whole subtree via runtime.DeepCopyJSON, which
// panics on any value it does not recognize as valid decoded JSON — a bare
// Go int, for instance, as opposed to the int64 the API server's own decoder
// produces. p.Object came from a caller kubeagent does not control (and, in
// a fuzz test, from a generator that is free to place a plain int anywhere),
// so a hostile or malformed selector must fall through to "does not cover",
// never panic.
func coveredByPDB(obj *unstructured.Unstructured, pdbs []*unstructured.Unstructured) bool {
	ns := obj.GetNamespace()
	labels := podTemplateLabels(obj)
	for _, p := range pdbs {
		if p == nil || p.GetNamespace() != ns {
			continue
		}
		sel, found, err := unstructured.NestedFieldNoCopy(p.Object, "spec", "selector")
		if err != nil || !found {
			// A PDB with no selector selects nothing kubeagent can reason
			// about; ignore it rather than treat it as universal.
			continue
		}
		selector, ok := sel.(map[string]any)
		if !ok {
			// spec.selector is present but not an object — nothing to match.
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
//
// Only scaleTargetRef.kind and .name are compared; apiVersion is not read at
// all. An HPA targeting {apiVersion: "custom.example.com/v1", kind:
// "Deployment", name: "web"} would therefore be reported as covering an
// apps/v1 Deployment named "web", even though its scaleTargetRef names a
// different API group. That is acceptable here because RelationValidForKind
// restricts this relation to Deployment, StatefulSet and ReplicaSet — three
// fixed, built-in apps/v1 kinds a rule can select — so there is no other API
// group in play for kubeagent to disambiguate against in practice.
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
