package pdbhealth

import (
	"testing"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// pdb builds a PDB with a minAvailable rule and the given status counts.
func pdb(ns, name string, minAvail int, expected, desired, current, allowed int32) policyv1.PodDisruptionBudget {
	m := intstr.FromInt(minAvail)
	return policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &m},
		Status: policyv1.PodDisruptionBudgetStatus{
			ExpectedPods: expected, DesiredHealthy: desired,
			CurrentHealthy: current, DisruptionsAllowed: allowed,
		},
	}
}

func find(issues []Issue, name string) (Issue, bool) {
	for _, i := range issues {
		if i.Name == name {
			return i, true
		}
	}
	return Issue{}, false
}

func TestAssess_Unsatisfiable(t *testing.T) {
	// minAvailable 3 covering all 3 pods → can never allow a disruption.
	got := Assess([]policyv1.PodDisruptionBudget{pdb("shop", "api", 3, 3, 3, 3, 0)})
	is, ok := find(got, "api")
	if !ok || is.Category != "unsatisfiable" {
		t.Fatalf("want unsatisfiable, got %+v", got)
	}
	if is.Rule != "minAvailable: 3" {
		t.Errorf("rule = %q, want minAvailable: 3", is.Rule)
	}
	if is.Reason != "covers all 3 pods — no voluntary eviction can ever proceed; every node drain will hang" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_Blocking(t *testing.T) {
	// disruptionsAllowed 0 with only 1/2 guarded pods healthy.
	got := Assess([]policyv1.PodDisruptionBudget{pdb("shop", "cache", 2, 3, 2, 1, 0)})
	is, ok := find(got, "cache")
	if !ok || is.Category != "blocking" {
		t.Fatalf("want blocking, got %+v", got)
	}
	if is.Reason != "blocking evictions with only 1/2 guarded pods healthy" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_UnsatisfiableEvenWhenDegraded(t *testing.T) {
	// minAvailable == replicas is unsatisfiable regardless of current health;
	// it must NOT be reclassified as "blocking" just because a pod is down.
	got := Assess([]policyv1.PodDisruptionBudget{pdb("shop", "hot", 2, 2, 2, 1, 0)})
	is, ok := find(got, "hot")
	if !ok || is.Category != "unsatisfiable" {
		t.Fatalf("a degraded minAvailable-of-N PDB must stay unsatisfiable, got %+v", got)
	}
}

func TestAssess_Stale(t *testing.T) {
	// selector matches no pods.
	got := Assess([]policyv1.PodDisruptionBudget{pdb("ops", "legacy", 1, 0, 0, 0, 0)})
	is, ok := find(got, "legacy")
	if !ok || is.Category != "stale" {
		t.Fatalf("want stale, got %+v", got)
	}
	if is.Reason != "selector matches no pods (stale?)" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_MaxUnavailableZeroRuleString(t *testing.T) {
	// maxUnavailable 0 on a multi-replica workload → unsatisfiable, rule names maxUnavailable.
	mu := intstr.FromInt(0)
	p := policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "mu"},
		Spec:       policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &mu},
		Status:     policyv1.PodDisruptionBudgetStatus{ExpectedPods: 2, DesiredHealthy: 2, CurrentHealthy: 2, DisruptionsAllowed: 0},
	}
	is, ok := find(Assess([]policyv1.PodDisruptionBudget{p}), "mu")
	if !ok || is.Category != "unsatisfiable" || is.Rule != "maxUnavailable: 0" {
		t.Fatalf("want unsatisfiable maxUnavailable: 0, got %+v", is)
	}
}

func TestAssess_NotFlagged(t *testing.T) {
	cases := []policyv1.PodDisruptionBudget{
		pdb("a", "singleton", 1, 1, 1, 1, 0), // single replica → excluded by expectedPods>1 guard
		pdb("a", "healthy23", 2, 3, 2, 3, 1),  // minAvailable 2 of 3, disruptionsAllowed 1
		pdb("a", "atfloor", 2, 3, 2, 2, 0), // minAvailable 2 of 3, exactly at floor (current==desired, allowed 0) → benign
	}
	if got := Assess(cases); len(got) != 0 {
		t.Fatalf("expected nothing flagged, got %+v", got)
	}
}

func TestAssess_StaleBeatsBlocking(t *testing.T) {
	// expectedPods 0 AND currentHealthy < desiredHealthy → reported as stale (precedence).
	got := Assess([]policyv1.PodDisruptionBudget{pdb("a", "p", 1, 0, 1, 0, 0)})
	if is, _ := find(got, "p"); is.Category != "stale" {
		t.Fatalf("stale must win precedence, got %+v", got)
	}
}

func TestAssess_SortedByNamespaceName(t *testing.T) {
	got := Assess([]policyv1.PodDisruptionBudget{
		pdb("b", "z", 2, 2, 2, 2, 0),
		pdb("a", "y", 2, 2, 2, 2, 0),
		pdb("a", "x", 2, 2, 2, 2, 0),
	})
	if len(got) != 3 || got[0].Name != "x" || got[1].Name != "y" || got[2].Name != "z" {
		t.Fatalf("not sorted by (ns,name): %+v", got)
	}
}
