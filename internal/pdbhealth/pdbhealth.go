// Package pdbhealth flags PodDisruptionBudgets that will block a node drain — a
// PDB that can never allow a voluntary eviction, one whose selector matches no
// pods, or one blocking evictions on an already-degraded workload. Pure and
// read-only: the caller supplies the PDB objects; every count comes from the
// PDB's own status. Advisory (never affects the cluster verdict).
package pdbhealth

import (
	"fmt"
	"sort"

	policyv1 "k8s.io/api/policy/v1"
)

// Issue is one PodDisruptionBudget that will block a node drain.
type Issue struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Rule      string `json:"rule"`     // "minAvailable: 3" | "maxUnavailable: 0"
	Category  string `json:"category"` // "stale" | "unsatisfiable" | "blocking"
	Reason    string `json:"reason"`
}

// Assess flags PDBs that will block a node drain, sorted by (Namespace, Name).
// A benign PDB (a healthy at-floor budget, a single-replica singleton, or one
// that currently allows a disruption) is not flagged.
func Assess(pdbs []policyv1.PodDisruptionBudget) []Issue {
	var out []Issue
	for _, p := range pdbs {
		rule := ruleString(p)
		if rule == "" {
			continue // neither minAvailable nor maxUnavailable set — nothing to say
		}
		if cat, reason, ok := classify(p); ok {
			out = append(out, Issue{Namespace: p.Namespace, Name: p.Name, Rule: rule, Category: cat, Reason: reason})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ruleString renders the PDB's rule ("minAvailable: 3" / "maxUnavailable: 0").
// Exactly one of the two is set on a valid PDB; if neither is, returns "".
func ruleString(p policyv1.PodDisruptionBudget) string {
	switch {
	case p.Spec.MinAvailable != nil:
		return "minAvailable: " + p.Spec.MinAvailable.String()
	case p.Spec.MaxUnavailable != nil:
		return "maxUnavailable: " + p.Spec.MaxUnavailable.String()
	default:
		return ""
	}
}

// classify returns the first matching category (stale → unsatisfiable → blocking)
// for a PDB, or ok=false when it is benign. All counts come from status.
func classify(p policyv1.PodDisruptionBudget) (category, reason string, ok bool) {
	s := p.Status
	switch {
	case s.ExpectedPods == 0:
		return "stale", "selector matches no pods (stale?)", true
	case s.ExpectedPods > 1 && s.DesiredHealthy >= s.ExpectedPods:
		return "unsatisfiable", fmt.Sprintf("covers all %d pods — no voluntary eviction can ever proceed; every node drain will hang", s.ExpectedPods), true
	case s.DisruptionsAllowed == 0 && s.CurrentHealthy < s.DesiredHealthy:
		return "blocking", fmt.Sprintf("blocking evictions with only %d/%d guarded pods healthy", s.CurrentHealthy, s.DesiredHealthy), true
	default:
		return "", "", false
	}
}
