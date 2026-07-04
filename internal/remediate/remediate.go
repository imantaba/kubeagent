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
	Kind              string // "RolloutUndo" | "Uncordon"
	Namespace         string
	Name              string // workload name (a Deployment in v1)
	Target            string // display target, e.g. "shop/web (Deployment)" or "node/worker-1"
	Summary           string // one-line human description
	Reason            string // why it's proposed
	KubectlEquivalent string // shown for audit only; NOT how it executes
}

// Plan returns the safe, allowlisted, precondition-satisfied remediations for the
// diagnosed workloads. Pure: reads only, mutates nothing.
func Plan(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, nodes []corev1.Node) []Action {
	var actions []Action
	for _, w := range workloads {
		if w.Kind != "Deployment" || protectedNamespaces[w.Namespace] {
			continue
		}
		if !hasImagePullFinding(w) {
			continue
		}
		if w.Ready >= w.Desired {
			continue // still meeting its replica target (e.g. previous revision serving) — not an outage
		}
		prev := previousRevision(w.Namespace, w.Name, replicaSets)
		if prev == "" {
			continue
		}
		actions = append(actions, Action{
			Kind:              "RolloutUndo",
			Namespace:         w.Namespace,
			Name:              w.Name,
			Target:            w.Namespace + "/" + w.Name + " (Deployment)",
			Summary:           "roll back to the previous revision",
			Reason:            "newest rollout cannot pull its image; a prior revision (" + prev + ") exists",
			KubectlEquivalent: "kubectl -n " + w.Namespace + " rollout undo deployment/" + w.Name,
		})
	}
	for _, n := range nodes {
		if !n.Spec.Unschedulable || hasNoExecuteTaint(n) {
			continue
		}
		actions = append(actions, Action{
			Kind:              "Uncordon",
			Name:              n.Name,
			Target:            "node/" + n.Name,
			Summary:           "uncordon the node (make it schedulable)",
			Reason:            "node is cordoned (SchedulingDisabled)",
			KubectlEquivalent: "kubectl uncordon " + n.Name,
		})
	}
	return actions
}

// hasNoExecuteTaint reports whether the node carries any NoExecute taint (an active
// drain / NotReady / pressure) — a signal not to fight by uncordoning.
func hasNoExecuteTaint(n corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
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

// Apply performs an allowlisted remediation's single guarded write via client-go.
func Apply(ctx context.Context, client kubernetes.Interface, a Action) Result {
	switch a.Kind {
	case "RolloutUndo":
		return applyRolloutUndo(ctx, client, a)
	case "Uncordon":
		return applyUncordon(ctx, client, a)
	default:
		return Result{Action: a, Err: fmt.Errorf("unknown action kind %q", a.Kind)}
	}
}

func applyRolloutUndo(ctx context.Context, client kubernetes.Interface, a Action) Result {
	res := Result{Action: a}
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

func applyUncordon(ctx context.Context, client kubernetes.Interface, a Action) Result {
	res := Result{Action: a}
	n, err := client.CoreV1().Nodes().Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		res.Err = fmt.Errorf("get node: %w", err)
		return res
	}
	// apply-time precondition: still cordoned and still no NoExecute taint
	if !n.Spec.Unschedulable || hasNoExecuteTaint(*n) {
		res.Detail = "node is no longer a safe uncordon target (already schedulable or NoExecute-tainted); no write made"
		return res
	}
	n.Spec.Unschedulable = false
	if _, err := client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		res.Err = fmt.Errorf("update node: %w", err)
		return res
	}
	res.Applied = true
	res.Detail = "uncordoned node " + a.Name
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
