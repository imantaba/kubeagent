// Package remediate plans and applies safe, reversible, opt-in fixes for problems
// kubeagent detects. Planning is pure; applying performs a single guarded write
// via client-go. No remediation is ever decided by an LLM.
package remediate

import (
	"context"
	"fmt"
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

// Change is one previewed field change, e.g. {"image (web)", "web:v2", "web:v1"}.
// From/To are always safe display values (revisions, image refs, booleans, counts) —
// never env values or raw template content. A count-only line (e.g. "2 other
// template fields changed") leaves From/To empty.
type Change struct {
	Field string `json:"field"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

// Action is one proposed, allowlisted remediation. Never free-form; never LLM-decided.
type Action struct {
	Kind              string // "RolloutUndo" | "Uncordon"
	Namespace         string
	Name              string   // workload name (a Deployment in v1)
	Target            string   // display target, e.g. "shop/web (Deployment)" or "node/worker-1"
	Summary           string   // one-line human description
	Reason            string   // why it's proposed
	KubectlEquivalent string   // shown for audit only; NOT how it executes
	Changes           []Change // the previewed field-level diff (rendered + JSON)
	CurrentRevision   int      // RolloutUndo: revision current at preview time (0 for Uncordon)
	TargetRevision    int      // RolloutUndo: revision the rollback lands on (0 for Uncordon)
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
		cur, target := planTarget(w.Namespace, w.Name, replicaSets)
		if target == nil {
			continue
		}
		targetRev := revFromAnnotations(target.Annotations)
		actions = append(actions, Action{
			Kind:              "RolloutUndo",
			Namespace:         w.Namespace,
			Name:              w.Name,
			Target:            w.Namespace + "/" + w.Name + " (Deployment)",
			Summary:           "roll back to the previous revision",
			Reason:            "newest rollout cannot pull its image; a prior revision (" + strconv.Itoa(targetRev) + ") exists",
			KubectlEquivalent: "kubectl -n " + w.Namespace + " rollout undo deployment/" + w.Name,
			Changes:           templateChanges(*cur, *target),
			CurrentRevision:   revFromAnnotations(cur.Annotations),
			TargetRevision:    targetRev,
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
			Changes:           []Change{{Field: "spec.unschedulable", From: "true", To: "false"}},
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

// planTarget returns the deployment's current (highest-revision) owned ReplicaSet
// and the rollback target — the highest revision strictly below current whose pod
// template differs — or nils if there is no current or no differing prior revision.
// This is the same selection rule Apply's pickTarget uses, so what Plan previews is
// what Apply lands on.
func planTarget(namespace, deployment string, replicaSets []appsv1.ReplicaSet) (cur, target *appsv1.ReplicaSet) {
	for i := range replicaSets {
		rs := &replicaSets[i]
		if rs.Namespace != namespace || !ownedBy(*rs, deployment) || revFromAnnotations(rs.Annotations) == 0 {
			continue
		}
		if cur == nil || revFromAnnotations(rs.Annotations) > revFromAnnotations(cur.Annotations) {
			cur = rs
		}
	}
	if cur == nil {
		return nil, nil
	}
	curRev := revFromAnnotations(cur.Annotations)
	for i := range replicaSets {
		rs := &replicaSets[i]
		if rs.Namespace != namespace || !ownedBy(*rs, deployment) {
			continue
		}
		r := revFromAnnotations(rs.Annotations)
		if r == 0 || r >= curRev {
			continue
		}
		if templatesEqual(rs.Spec.Template, cur.Spec.Template) {
			continue
		}
		if target == nil || r > revFromAnnotations(target.Annotations) {
			target = rs
		}
	}
	return cur, target
}

// templateChanges renders the curated preview diff between the current and target
// templates: the revision line, per-container image changes, and a count-only line
// for any other differences. Never prints template contents.
func templateChanges(cur, target appsv1.ReplicaSet) []Change {
	curRev, targetRev := revFromAnnotations(cur.Annotations), revFromAnnotations(target.Annotations)
	changes := []Change{{Field: "revision", From: strconv.Itoa(curRev), To: strconv.Itoa(targetRev)}}
	targetImages := map[string]string{}
	for _, c := range target.Spec.Template.Spec.Containers {
		targetImages[c.Name] = c.Image
	}
	for _, c := range cur.Spec.Template.Spec.Containers {
		if to, ok := targetImages[c.Name]; ok && to != c.Image {
			changes = append(changes, Change{Field: "image (" + c.Name + ")", From: c.Image, To: to})
		}
	}
	if n := otherChangeCount(cur.Spec.Template, target.Spec.Template); n > 0 {
		field := strconv.Itoa(n) + " other template field"
		if n > 1 {
			field += "s"
		}
		changes = append(changes, Change{Field: field + " changed"})
	}
	return changes
}

// otherChangeCount counts template differences beyond container images, comparing
// with pod-template-hash stripped and images neutralized (they are reported
// separately). Each differing aspect counts once; contents are never exposed.
func otherChangeCount(a, b corev1.PodTemplateSpec) int {
	ac, bc := a.DeepCopy(), b.DeepCopy()
	delete(ac.Labels, "pod-template-hash")
	delete(bc.Labels, "pod-template-hash")
	for i := range ac.Spec.Containers {
		ac.Spec.Containers[i].Image = ""
	}
	for i := range bc.Spec.Containers {
		bc.Spec.Containers[i].Image = ""
	}
	n := 0
	if !apiequality.Semantic.DeepEqual(ac.Labels, bc.Labels) {
		n++
	}
	if !apiequality.Semantic.DeepEqual(ac.Annotations, bc.Annotations) {
		n++
	}
	if len(ac.Spec.Containers) != len(bc.Spec.Containers) || len(ac.Spec.InitContainers) != len(bc.Spec.InitContainers) {
		n++
	} else {
		for i := range ac.Spec.Containers {
			if !apiequality.Semantic.DeepEqual(ac.Spec.Containers[i], bc.Spec.Containers[i]) {
				n++
			}
		}
		for i := range ac.Spec.InitContainers {
			if !apiequality.Semantic.DeepEqual(ac.Spec.InitContainers[i], bc.Spec.InitContainers[i]) {
				n++
			}
		}
	}
	podA, podB := ac.Spec.DeepCopy(), bc.Spec.DeepCopy()
	podA.Containers, podB.Containers = nil, nil
	podA.InitContainers, podB.InitContainers = nil, nil
	if !apiequality.Semantic.DeepEqual(podA, podB) {
		n++
	}
	return n
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
	curRev, targetRev := revFromAnnotations(dep.Annotations), revFromAnnotations(target.Annotations)
	if curRev != a.CurrentRevision || targetRev != a.TargetRevision {
		res.Detail = fmt.Sprintf(
			"state changed since preview (revision %d is now current and the rollback would land on %d; previewed %d → %d) — re-run kubeagent scan --fix; no write made",
			curRev, targetRev, a.CurrentRevision, a.TargetRevision)
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
