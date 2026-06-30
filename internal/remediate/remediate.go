// Package remediate plans and applies safe, reversible, opt-in fixes for problems
// kubeagent detects. Planning is pure; applying performs a single guarded write
// via client-go. No remediation is ever decided by an LLM.
package remediate

import (
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

const revisionAnno = "deployment.kubernetes.io/revision"

// protectedNamespaces are never targeted by a remediation.
var protectedNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
}

// Action is one proposed, allowlisted remediation. Never free-form; never LLM-decided.
type Action struct {
	Kind              string // "RolloutUndo" (the only kind in v1)
	Namespace         string
	Name              string // workload name (a Deployment in v1)
	Summary           string // one-line human description
	Reason            string // why it's proposed
	KubectlEquivalent string // shown for audit only; NOT how it executes
}

// Plan returns the safe, allowlisted, precondition-satisfied remediations for the
// diagnosed workloads. Pure: reads only, mutates nothing.
func Plan(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet) []Action {
	var actions []Action
	for _, w := range workloads {
		if w.Kind != "Deployment" || protectedNamespaces[w.Namespace] {
			continue
		}
		if !hasImagePullFinding(w) {
			continue
		}
		prev := previousRevision(w.Namespace, w.Name, replicaSets)
		if prev == "" {
			continue
		}
		actions = append(actions, Action{
			Kind:              "RolloutUndo",
			Namespace:         w.Namespace,
			Name:              w.Name,
			Summary:           "roll back to the previous revision",
			Reason:            "newest rollout cannot pull its image; a prior revision (" + prev + ") exists",
			KubectlEquivalent: "kubectl -n " + w.Namespace + " rollout undo deployment/" + w.Name,
		})
	}
	return actions
}

func hasImagePullFinding(w inventory.Workload) bool {
	for _, f := range w.Findings {
		if f.Issue == "ImagePullBackOff" || f.Issue == "ErrImagePull" {
			return true
		}
	}
	return false
}

// previousRevision returns the revision just below the current (max) one, among the
// ReplicaSets owned by the named Deployment in the namespace, or "" if there is no
// prior revision to roll back to.
func previousRevision(namespace, deployment string, replicaSets []appsv1.ReplicaSet) string {
	var revs []int
	for _, rs := range replicaSets {
		if rs.Namespace == namespace && ownedBy(rs, deployment) {
			if r := revFromAnnotations(rs.Annotations); r > 0 {
				revs = append(revs, r)
			}
		}
	}
	if len(revs) < 2 {
		return ""
	}
	sort.Sort(sort.Reverse(sort.IntSlice(revs)))
	return strconv.Itoa(revs[1])
}

func ownedBy(rs appsv1.ReplicaSet, deployment string) bool {
	for _, o := range rs.OwnerReferences {
		if o.Kind == "Deployment" && o.Name == deployment {
			return true
		}
	}
	return false
}

func revFromAnnotations(anno map[string]string) int {
	if v, ok := anno[revisionAnno]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}
