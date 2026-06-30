// Package remediate plans and applies safe, reversible, opt-in fixes for problems
// kubeagent detects. Planning is pure; applying performs a single guarded write
// via client-go. No remediation is ever decided by an LLM.
package remediate

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

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

// Result records what Apply did, for the audit line.
type Result struct {
	Action  Action
	Applied bool
	Detail  string
	Err     error
}

// Apply performs the action's single guarded write via client-go and reports it.
func Apply(ctx context.Context, client kubernetes.Interface, a Action) Result {
	res := Result{Action: a}
	if a.Kind != "RolloutUndo" {
		res.Err = fmt.Errorf("unknown action kind %q", a.Kind)
		return res
	}
	if protectedNamespaces[a.Namespace] {
		res.Err = fmt.Errorf("refusing to act in protected namespace %q", a.Namespace)
		return res
	}
	dep, err := client.AppsV1().Deployments(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		res.Err = fmt.Errorf("get deployment: %w", err)
		return res
	}
	rsList, err := client.AppsV1().ReplicaSets(a.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		res.Err = fmt.Errorf("list replicasets: %w", err)
		return res
	}
	target := pickTarget(dep, rsList.Items)
	if target == nil {
		res.Detail = "no differing prior revision to roll back to (state changed); no write made"
		return res
	}
	// Roll back: restore the target revision's pod template. The controller manages
	// the pod-template-hash label, so drop it from the Deployment spec.
	tpl := *target.Spec.Template.DeepCopy()
	delete(tpl.Labels, "pod-template-hash")
	dep.Spec.Template = tpl
	if _, err := client.AppsV1().Deployments(a.Namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		res.Err = fmt.Errorf("update deployment: %w", err)
		return res
	}
	res.Applied = true
	res.Detail = fmt.Sprintf("rolled back %s/%s to revision %d (pod template restored)",
		a.Namespace, a.Name, revFromAnnotations(target.Annotations))
	return res
}

// pickTarget returns the owned ReplicaSet with the highest revision strictly below
// the Deployment's current revision whose pod template differs from the current
// one. nil if none.
func pickTarget(dep *appsv1.Deployment, replicaSets []appsv1.ReplicaSet) *appsv1.ReplicaSet {
	curRev := revFromAnnotations(dep.Annotations)
	if curRev == 0 {
		return nil // no current-revision annotation: can't safely identify a prior revision; skip
	}
	var best *appsv1.ReplicaSet
	for i := range replicaSets {
		rs := &replicaSets[i]
		if rs.Namespace != dep.Namespace || !ownedBy(*rs, dep.Name) {
			continue
		}
		r := revFromAnnotations(rs.Annotations)
		if r >= curRev {
			continue
		}
		if templatesEqual(rs.Spec.Template, dep.Spec.Template) {
			continue
		}
		if best == nil || r > revFromAnnotations(best.Annotations) {
			best = rs
		}
	}
	return best
}

func templatesEqual(a, b corev1.PodTemplateSpec) bool {
	ac, bc := a.DeepCopy(), b.DeepCopy()
	delete(ac.Labels, "pod-template-hash")
	delete(bc.Labels, "pod-template-hash")
	return apiequality.Semantic.DeepEqual(ac, bc)
}
