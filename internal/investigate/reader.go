package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// toolCall is one model-requested read (backend-agnostic; the Anthropic backend
// translates tool_use blocks into these).
type toolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// toolResult answers a toolCall. IsError marks a denied or failed read; the loop
// feeds it back so the model can adapt.
type toolResult struct {
	ID      string
	Content string
	IsError bool
}

// Reader executes an allowed tool call via read-only client-go calls, rendering
// only structured fields — never IPs, env, secret data, container args, or logs.
type Reader struct {
	client kubernetes.Interface
}

func (r Reader) execute(ctx context.Context, c toolCall, scope *Scope) toolResult {
	switch c.Name {
	case "describe":
		return r.describe(ctx, c, scope)
	case "get_events":
		return r.getEvents(ctx, c, scope)
	case "get_related":
		return r.getRelated(ctx, c, scope)
	default:
		return errResult(c.ID, fmt.Sprintf("unknown tool %q", c.Name))
	}
}

func errResult(id, msg string) toolResult { return toolResult{ID: id, Content: msg, IsError: true} }

func okResult(id, content string) toolResult { return toolResult{ID: id, Content: content} }

type describeInput struct{ Kind, Namespace, Name string }

func (r Reader) describe(ctx context.Context, c toolCall, scope *Scope) toolResult {
	var in describeInput
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return errResult(c.ID, "invalid input: "+err.Error())
	}
	kind := normKind(in.Kind)
	if !scope.Allowed(kind, nsFor(kind, in.Namespace), in.Name) {
		return errResult(c.ID, fmt.Sprintf("%s %s/%s is not in scope for this investigation", kind, in.Namespace, in.Name))
	}
	switch kind {
	case "pod":
		p, err := r.client.CoreV1().Pods(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return errResult(c.ID, err.Error())
		}
		return okResult(c.ID, describePod(p))
	case "deployment", "replicaset", "statefulset", "daemonset", "job":
		return r.describeWorkload(ctx, c.ID, kind, in.Namespace, in.Name)
	case "node":
		n, err := r.client.CoreV1().Nodes().Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return errResult(c.ID, err.Error())
		}
		return okResult(c.ID, describeNode(n))
	case "pvc":
		pvc, err := r.client.CoreV1().PersistentVolumeClaims(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return errResult(c.ID, err.Error())
		}
		return okResult(c.ID, describePVC(pvc))
	default:
		return errResult(c.ID, fmt.Sprintf("kind %q is not supported for describe", in.Kind))
	}
}

// nsFor returns "" for cluster-scoped kinds so scope lookups match the seeded keys.
func nsFor(kind, ns string) string {
	if kind == "node" {
		return ""
	}
	return ns
}

func describePod(p *corev1.Pod) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pod %s/%s: phase=%s node=%s\n", p.Namespace, p.Name, p.Status.Phase, p.Spec.NodeName)
	for _, cond := range p.Status.Conditions {
		fmt.Fprintf(&b, "  condition %s=%s", cond.Type, cond.Status)
		if cond.Reason != "" {
			fmt.Fprintf(&b, " (%s)", cond.Reason)
		}
		b.WriteString("\n")
	}
	for _, cs := range p.Status.ContainerStatuses {
		fmt.Fprintf(&b, "  container %s: ready=%t restarts=%d", cs.Name, cs.Ready, cs.RestartCount)
		if cs.State.Waiting != nil {
			fmt.Fprintf(&b, " waiting=%s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
		if cs.State.Terminated != nil {
			fmt.Fprintf(&b, " terminated=%s (exit %d)", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (r Reader) describeWorkload(ctx context.Context, id, kind, ns, name string) toolResult {
	var b strings.Builder
	switch kind {
	case "deployment":
		d, err := r.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return errResult(id, err.Error())
		}
		fmt.Fprintf(&b, "deployment %s/%s: ready=%d/%d updated=%d available=%d\n",
			ns, name, d.Status.ReadyReplicas, d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas)
		for _, cnd := range d.Status.Conditions {
			fmt.Fprintf(&b, "  condition %s=%s (%s): %s\n", cnd.Type, cnd.Status, cnd.Reason, cnd.Message)
		}
	case "replicaset":
		rs, err := r.client.AppsV1().ReplicaSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return errResult(id, err.Error())
		}
		fmt.Fprintf(&b, "replicaset %s/%s: ready=%d/%d available=%d\n", ns, name,
			rs.Status.ReadyReplicas, rs.Status.Replicas, rs.Status.AvailableReplicas)
	case "statefulset":
		ss, err := r.client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return errResult(id, err.Error())
		}
		fmt.Fprintf(&b, "statefulset %s/%s: ready=%d/%d\n", ns, name, ss.Status.ReadyReplicas, ss.Status.Replicas)
	case "daemonset":
		ds, err := r.client.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return errResult(id, err.Error())
		}
		fmt.Fprintf(&b, "daemonset %s/%s: ready=%d desired=%d available=%d unavailable=%d\n", ns, name,
			ds.Status.NumberReady, ds.Status.DesiredNumberScheduled, ds.Status.NumberAvailable, ds.Status.NumberUnavailable)
	case "job":
		j, err := r.client.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return errResult(id, err.Error())
		}
		fmt.Fprintf(&b, "job %s/%s: active=%d succeeded=%d failed=%d\n", ns, name, j.Status.Active, j.Status.Succeeded, j.Status.Failed)
	}
	return okResult(id, b.String())
}

func describeNode(n *corev1.Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, "node %s: unschedulable=%t\n", n.Name, n.Spec.Unschedulable)
	for _, cond := range n.Status.Conditions {
		fmt.Fprintf(&b, "  condition %s=%s (%s): %s\n", cond.Type, cond.Status, cond.Reason, cond.Message)
	}
	for _, t := range n.Spec.Taints {
		fmt.Fprintf(&b, "  taint %s=%s:%s\n", t.Key, t.Value, t.Effect)
	}
	return b.String()
}

func describePVC(p *corev1.PersistentVolumeClaim) string {
	sc := ""
	if p.Spec.StorageClassName != nil {
		sc = *p.Spec.StorageClassName
	}
	return fmt.Sprintf("pvc %s/%s: phase=%s storageClass=%s volume=%s\n",
		p.Namespace, p.Name, p.Status.Phase, sc, p.Spec.VolumeName)
}

type eventsInput struct{ Namespace, Name string }

func (r Reader) getEvents(ctx context.Context, c toolCall, scope *Scope) toolResult {
	var in eventsInput
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return errResult(c.ID, "invalid input: "+err.Error())
	}
	if !scope.HasName(in.Namespace, in.Name) {
		return errResult(c.ID, fmt.Sprintf("%s/%s is not in scope for this investigation", in.Namespace, in.Name))
	}
	evs, err := r.client.CoreV1().Events(in.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + in.Name,
	})
	if err != nil {
		return errResult(c.ID, err.Error())
	}
	if len(evs.Items) == 0 {
		return okResult(c.ID, fmt.Sprintf("no events for %s/%s", in.Namespace, in.Name))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "events for %s/%s:\n", in.Namespace, in.Name)
	for _, e := range evs.Items {
		fmt.Fprintf(&b, "  %s: %s (x%d)\n", e.Reason, e.Message, e.Count)
	}
	return okResult(c.ID, b.String())
}

type relatedInput struct{ Namespace, Name, Relation string }

func (r Reader) getRelated(ctx context.Context, c toolCall, scope *Scope) toolResult {
	var in relatedInput
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return errResult(c.ID, "invalid input: "+err.Error())
	}
	// The source is always the named pod, which must already be in scope.
	if !scope.Allowed("pod", in.Namespace, in.Name) {
		return errResult(c.ID, fmt.Sprintf("pod %s/%s is not in scope for this investigation", in.Namespace, in.Name))
	}
	p, err := r.client.CoreV1().Pods(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return errResult(c.ID, err.Error())
	}
	switch in.Relation {
	case "owner":
		if len(p.OwnerReferences) == 0 {
			return okResult(c.ID, fmt.Sprintf("pod %s/%s has no owner", in.Namespace, in.Name))
		}
		var b strings.Builder
		for _, o := range p.OwnerReferences {
			scope.Add(o.Kind, in.Namespace, o.Name)
			fmt.Fprintf(&b, "owner of %s: %s %s\n", in.Name, o.Kind, o.Name)
		}
		return okResult(c.ID, b.String())
	case "node":
		if p.Spec.NodeName == "" {
			return okResult(c.ID, fmt.Sprintf("pod %s/%s is not scheduled to a node", in.Namespace, in.Name))
		}
		scope.Add("node", "", p.Spec.NodeName)
		return okResult(c.ID, fmt.Sprintf("node of %s: %s\n", in.Name, p.Spec.NodeName))
	case "pvc":
		var names []string
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				n := v.PersistentVolumeClaim.ClaimName
				scope.Add("pvc", in.Namespace, n)
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			return okResult(c.ID, fmt.Sprintf("pod %s/%s has no PersistentVolumeClaims", in.Namespace, in.Name))
		}
		return okResult(c.ID, fmt.Sprintf("PVCs of %s: %s\n", in.Name, strings.Join(names, ", ")))
	default:
		return errResult(c.ID, fmt.Sprintf("unknown relation %q (want owner|node|pvc)", in.Relation))
	}
}
