// Package remediate plans and applies safe, reversible, opt-in fixes for problems
// kubeagent detects. Planning is pure; applying performs a single guarded write
// via client-go. No remediation is ever decided by an LLM.
package remediate

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
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
	Action          Action
	Applied         bool
	Refused         bool // a guarded no-write refusal (drift, no target, unsafe precondition); Applied false, Err nil
	PreflightDenied bool // the RBAC preflight refused this action; Applied false, Err nil, no write
	Detail          string
	Err             error
}

// Preflight asks the API server whether the current credentials may perform the write
// this Action implies (verb=update on its resource/namespace/name) via a
// SelfSubjectAccessReview. Returns (allowed, humanReason, err): err != nil means the
// SSAR call itself failed (callers fail closed and do not write); allowed==false means
// not permitted and reason explains it in plain language.
func Preflight(ctx context.Context, client kubernetes.Interface, a Action) (bool, string, error) {
	var group, resource, ns string
	switch a.Kind {
	case "RolloutUndo", "RolloutForward":
		group, resource, ns = "apps", "deployments", a.Namespace
	case "Uncordon", "Cordon":
		group, resource, ns = "", "nodes", ""
	default:
		return false, "", fmt.Errorf("unknown action kind %q", a.Kind)
	}
	ssar := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb: "update", Group: group, Resource: resource, Namespace: ns, Name: a.Name,
			},
		},
	}
	resp, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, ssar, metav1.CreateOptions{})
	if err != nil {
		return false, "", err
	}
	if resp.Status.Allowed {
		return true, "", nil
	}
	if ns == "" {
		return false, fmt.Sprintf("you lack permission to update %s (RBAC)", resource), nil
	}
	return false, fmt.Sprintf("you lack permission to update %s in namespace %q (RBAC)", resource, ns), nil
}

// Apply performs an allowlisted remediation's single guarded write via client-go.
func Apply(ctx context.Context, client kubernetes.Interface, a Action) Result {
	switch a.Kind {
	case "RolloutUndo":
		return applyRolloutUndo(ctx, client, a)
	case "Uncordon":
		return applyUncordon(ctx, client, a)
	case "RolloutForward":
		return applyRolloutForward(ctx, client, a)
	case "Cordon":
		return applyCordon(ctx, client, a)
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
		res.Refused = true
		return res
	}
	curRev, targetRev := revFromAnnotations(dep.Annotations), revFromAnnotations(target.Annotations)
	if curRev != a.CurrentRevision || targetRev != a.TargetRevision {
		res.Detail = fmt.Sprintf(
			"state changed since preview (revision %d is now current and the rollback would land on %d; previewed %d → %d) — re-run kubeagent scan --fix; no write made",
			curRev, targetRev, a.CurrentRevision, a.TargetRevision)
		res.Refused = true
		return res
	}
	allowed, reason, err := Preflight(ctx, client, a)
	if err != nil {
		res.Err = fmt.Errorf("permission preflight failed: %w", err)
		return res
	}
	if !allowed {
		res.PreflightDenied = true
		res.Detail = reason + "; no write attempted"
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
		res.Refused = true
		return res
	}
	allowed, reason, err := Preflight(ctx, client, a)
	if err != nil {
		res.Err = fmt.Errorf("permission preflight failed: %w", err)
		return res
	}
	if !allowed {
		res.PreflightDenied = true
		res.Detail = reason + "; no write attempted"
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

// Inverse returns the deterministic undo of a previously applied remediation, from the
// plain values an audit record carries (this package must not import internal/audit —
// audit imports remediate). Pure: no I/O, never LLM-decided. The returned Action flows
// through the same guard rails as any planned action.
func Inverse(kind, namespace, name string, fromRevision, toRevision int) (Action, error) {
	switch kind {
	case "RolloutUndo":
		if fromRevision == 0 || toRevision == 0 {
			return Action{}, fmt.Errorf("this audit record predates structured rollback data (kubeagent < v0.54); cannot derive a safe rollback")
		}
		return Action{
			Kind:              "RolloutForward",
			Namespace:         namespace,
			Name:              name,
			Target:            namespace + "/" + name + " (Deployment)",
			Summary:           "roll forward to the pre-fix revision",
			Reason:            fmt.Sprintf("undo the fix that rolled %s/%s back from revision %d to %d", namespace, name, fromRevision, toRevision),
			KubectlEquivalent: fmt.Sprintf("kubectl -n %s rollout undo deployment/%s --to-revision=%d", namespace, name, fromRevision),
			Changes: []Change{{
				Field: "revision",
				From:  strconv.Itoa(toRevision),
				To:    strconv.Itoa(fromRevision),
			}},
			CurrentRevision: toRevision,   // where the fix left it
			TargetRevision:  fromRevision, // where we are restoring to
		}, nil
	case "Uncordon":
		return Action{
			Kind:              "Cordon",
			Name:              name,
			Target:            "node/" + name,
			Summary:           "re-cordon the node (make it unschedulable)",
			Reason:            "undo the fix that uncordoned node " + name,
			KubectlEquivalent: "kubectl cordon " + name,
			Changes:           []Change{{Field: "spec.unschedulable", From: "false", To: "true"}},
		}, nil
	default:
		return Action{}, fmt.Errorf("no inverse defined for action kind %q", kind)
	}
}

// applyRolloutForward restores a Deployment to the revision it had before a fix, using
// a content-based drift bond (Amendment 2026-07-25): find the target RS by TargetRevision,
// verify the Deployment template is not already at the target, verify the Deployment's
// per-container images still match the post-fix images recorded on the Action's Changes,
// then the RBAC preflight, then the single write.
//
// CurrentRevision is kept on the Action for display/audit but is NOT a precondition —
// Kubernetes assigns a brand-new revision after a template-restore fix, so the numeric
// bond can never match.
func applyRolloutForward(ctx context.Context, client kubernetes.Interface, a Action) Result {
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

	// 1. Find the restore target by TargetRevision (= fromRevision; survives the fix).
	var target *appsv1.ReplicaSet
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if ownedBy(*rs, a.Name) && revFromAnnotations(rs.Annotations) == a.TargetRevision {
			target = rs
			break
		}
	}
	if target == nil {
		res.Detail = fmt.Sprintf("revision %d no longer exists; no write made", a.TargetRevision)
		res.Refused = true
		return res
	}

	// 2. Nothing to undo: Deployment template is already the target template.
	if templatesEqual(dep.Spec.Template, target.Spec.Template) {
		res.Detail = "already at the pre-fix revision; no write made"
		res.Refused = true
		return res
	}

	// 3. Drift: the Deployment's current per-container images must still match the
	// post-fix images recorded in the Action's Changes.  Any mismatch means a third
	// party has changed something since the fix — refuse rather than clobber.
	const imagePrefix = "image ("
	for _, c := range a.Changes {
		if !strings.HasPrefix(c.Field, imagePrefix) {
			continue
		}
		// Parse "image (<containerName>)" → containerName
		name := strings.TrimSuffix(strings.TrimPrefix(c.Field, imagePrefix), ")")
		want := c.To // what the fix left as the post-fix image
		// Find the container by name in the current Deployment template.
		var actual string
		for _, container := range dep.Spec.Template.Spec.Containers {
			if container.Name == name {
				actual = container.Image
				break
			}
		}
		if actual != want {
			res.Detail = fmt.Sprintf(
				"state changed since the fix (container %q is now %s; the fix left it at %s) — no write made",
				name, actual, want)
			res.Refused = true
			return res
		}
	}

	allowed, reason, err := Preflight(ctx, client, a)
	if err != nil {
		res.Err = fmt.Errorf("permission preflight failed: %w", err)
		return res
	}
	if !allowed {
		res.PreflightDenied = true
		res.Detail = reason + "; no write attempted"
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
	res.Detail = fmt.Sprintf("rolled %s/%s forward to revision %d (pre-fix pod template restored)",
		a.Namespace, a.Name, a.TargetRevision)
	return res
}

// applyCordon re-cordons a node that a previous fix uncordoned.
func applyCordon(ctx context.Context, client kubernetes.Interface, a Action) Result {
	res := Result{Action: a}
	n, err := client.CoreV1().Nodes().Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		res.Err = fmt.Errorf("get node: %w", err)
		return res
	}
	if n.Spec.Unschedulable {
		res.Detail = "node is already cordoned; no write made"
		res.Refused = true
		return res
	}
	allowed, reason, err := Preflight(ctx, client, a)
	if err != nil {
		res.Err = fmt.Errorf("permission preflight failed: %w", err)
		return res
	}
	if !allowed {
		res.PreflightDenied = true
		res.Detail = reason + "; no write attempted"
		return res
	}
	n.Spec.Unschedulable = true
	if _, err := client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		res.Err = fmt.Errorf("update node: %w", err)
		return res
	}
	res.Applied = true
	res.Detail = "re-cordoned node " + a.Name
	return res
}
