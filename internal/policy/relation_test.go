package policy

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func workload(kind, namespace, name string, templateLabels map[string]string) *unstructured.Unstructured {
	labels := map[string]any{}
	for k, v := range templateLabels {
		labels[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"kind":     kind,
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"template": map[string]any{"metadata": map[string]any{"labels": labels}},
		},
	}}
}

func pdb(namespace string, selector map[string]any) *unstructured.Unstructured {
	spec := map[string]any{}
	if selector != nil {
		spec["selector"] = selector
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"kind":     "PodDisruptionBudget",
		"metadata": map[string]any{"name": "pdb", "namespace": namespace},
		"spec":     spec,
	}}
}

func hpa(namespace, targetKind, targetName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"kind":     "HorizontalPodAutoscaler",
		"metadata": map[string]any{"name": "hpa", "namespace": namespace},
		"spec": map[string]any{
			"scaleTargetRef": map[string]any{"kind": targetKind, "name": targetName},
		},
	}}
}

func TestHasPodDisruptionBudget(t *testing.T) {
	dep := workload("Deployment", "prod", "web", map[string]string{"app": "web", "tier": "front"})

	cases := []struct {
		name string
		pdbs []*unstructured.Unstructured
		want bool
	}{
		{"no PDBs at all", nil, false},
		{"matching subset selector", []*unstructured.Unstructured{
			pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web"}}),
		}, true},
		{"selector needs a label the workload lacks", []*unstructured.Unstructured{
			pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web", "zone": "b"}}),
		}, false},
		{"right selector, wrong namespace", []*unstructured.Unstructured{
			pdb("staging", map[string]any{"matchLabels": map[string]any{"app": "web"}}),
		}, false},
		{"empty matchLabels covers everything in the namespace", []*unstructured.Unstructured{
			pdb("prod", map[string]any{"matchLabels": map[string]any{}}),
		}, true},
		{"no selector at all is ignored", []*unstructured.Unstructured{
			pdb("prod", nil),
		}, false},
		{"matchExpressions alone does not cover — kubeagent does not evaluate them", []*unstructured.Unstructured{
			pdb("prod", map[string]any{"matchExpressions": []any{
				map[string]any{"key": "app", "operator": "In", "values": []any{"web"}},
			}}),
		}, false},
		{"one of several matches", []*unstructured.Unstructured{
			pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "api"}}),
			pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web"}}),
		}, true},
	}
	for _, c := range cases {
		got := relationHolds(RelationHasPDB, dep, Inputs{PDBs: c.pdbs})
		if got != c.want {
			t.Errorf("%s: relationHolds = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHasHorizontalPodAutoscaler(t *testing.T) {
	dep := workload("Deployment", "prod", "web", nil)

	cases := []struct {
		name string
		hpas []*unstructured.Unstructured
		want bool
	}{
		{"none", nil, false},
		{"exact target", []*unstructured.Unstructured{hpa("prod", "Deployment", "web")}, true},
		{"wrong name", []*unstructured.Unstructured{hpa("prod", "Deployment", "api")}, false},
		{"wrong kind", []*unstructured.Unstructured{hpa("prod", "StatefulSet", "web")}, false},
		{"wrong namespace", []*unstructured.Unstructured{hpa("staging", "Deployment", "web")}, false},
	}
	for _, c := range cases {
		got := relationHolds(RelationHasHPA, dep, Inputs{HPAs: c.hpas})
		if got != c.want {
			t.Errorf("%s: relationHolds = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestWorkloadWithNoPodTemplateLabelsIsCoveredOnlyByAnEmptySelector: a
// Deployment whose pod template sets no labels can only be covered by a
// selector that requires nothing.
func TestWorkloadWithNoPodTemplateLabelsIsCoveredOnlyByAnEmptySelector(t *testing.T) {
	dep := workload("Deployment", "prod", "web", nil)
	if relationHolds(RelationHasPDB, dep, Inputs{PDBs: []*unstructured.Unstructured{
		pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web"}}),
	}}) {
		t.Error("a workload with no template labels must not be covered by a label selector")
	}
	if !relationHolds(RelationHasPDB, dep, Inputs{PDBs: []*unstructured.Unstructured{
		pdb("prod", map[string]any{"matchLabels": map[string]any{}}),
	}}) {
		t.Error("an empty selector covers everything in the namespace")
	}
}

func TestUnknownRelationNeverHolds(t *testing.T) {
	dep := workload("Deployment", "prod", "web", nil)
	if relationHolds(Relation("hasNetworkPolicy"), dep, Inputs{}) {
		t.Error("an unknown relation must not hold")
	}
}
