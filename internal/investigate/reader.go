package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/safetext"
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
// only structured fields — never env, secret data, container args, or logs —
// and never an address it chose: no PodIP, HostIP, or ClusterIP field is ever
// printed. A condition, waiting/terminated, or event Reason and Message is
// free text the API server does not validate, so it passes through sanitize on
// its way out (see describePod), which catches a network address embedded in
// that text. It does not catch an arbitrary URL: the cluster's own text can
// still carry one, path and all.
//
// That does not make a tool result address-free. When a client-go Get or List
// call in this file fails, the error is returned to the model as err.Error(),
// unfiltered by sanitize or by anything else -- a *url.Error from a failed
// call names the API server's own host:port in its dial failure, and the
// request path it was reaching for besides. This is a known gap: no decision
// in this package closes it.
type Reader struct {
	client kubernetes.Interface
}

// sanitize prepares one free-text field (a condition, waiting/terminated, or
// event Reason or Message) read from the API server for a tool result.
//
// The order is safetext.Line first, then redact.Addresses -- never the
// reverse, and not for the reason the project's usual "match on the raw
// value" rule would suggest. That rule exists so a hostile control character
// spliced mid-word cannot evade a detector's raw-value signature match; taken
// literally here it would argue for redacting first. The real reason for
// this order is the opposite: a Unicode formatting character (category Cf,
// e.g. U+202E) can sit inside an address and split it, which breaks
// redact.Addresses' regexp. Sanitizing first repairs the split -- Line drops
// the character before the regexp ever runs -- so the address is caught;
// redacting first tests the still-split text, misses it, and only then has
// Line strip the character that was hiding it, leaving the address in the
// clear. Truncating before matching is safe here, unlike the raw-value rule's
// usual concern: text past safetext.MaxLine is discarded, never rendered, so
// an address in the dropped tail leaks nothing either way.
//
// sanitize does not catch everything: redact.Addresses matches a bracketed
// IPv6 address with its port, a dotted-quad IPv4 address with or without one,
// or a dotted DNS name with its port -- never an arbitrary URL, so a registry
// address quoted inside an image-pull failure keeps its scheme and path
// intact. The DNS alternative needs that dot: a single-label service host
// with a port ("redis:6379") passes through as well (R248).
func sanitize(s string) string {
	return redact.Addresses(safetext.Line(s))
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

// describePod renders a pod's structured status: phase, node, conditions and
// container states. It never renders an address kubeagent chose -- no PodIP,
// HostIP, or ClusterIP appears here. A condition's Reason and a waiting or
// terminated container's Reason and Message are free text the kubelet wrote,
// not validated by the API server, so each passes through sanitize before
// reaching the returned string. sanitize catches a network address embedded
// in that text, but not an arbitrary URL: a registry address quoted inside an
// image-pull failure, for example, can still reach the model with its scheme
// and path intact -- the cluster's own text, quoted rather than chosen by
// kubeagent.
func describePod(p *corev1.Pod) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pod %s/%s: phase=%s node=%s\n", p.Namespace, p.Name, p.Status.Phase, p.Spec.NodeName)
	for _, cond := range p.Status.Conditions {
		fmt.Fprintf(&b, "  condition %s=%s", cond.Type, cond.Status)
		if cond.Reason != "" {
			fmt.Fprintf(&b, " (%s)", sanitize(cond.Reason))
		}
		b.WriteString("\n")
	}
	for _, cs := range p.Status.ContainerStatuses {
		fmt.Fprintf(&b, "  container %s: ready=%t restarts=%d", cs.Name, cs.Ready, cs.RestartCount)
		if cs.State.Waiting != nil {
			fmt.Fprintf(&b, " waiting=%s: %s", sanitize(cs.State.Waiting.Reason), sanitize(cs.State.Waiting.Message))
		}
		if cs.State.Terminated != nil {
			fmt.Fprintf(&b, " terminated=%s (exit %d)", sanitize(cs.State.Terminated.Reason), cs.State.Terminated.ExitCode)
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
			fmt.Fprintf(&b, "  condition %s=%s (%s): %s\n", cnd.Type, cnd.Status, sanitize(cnd.Reason), sanitize(cnd.Message))
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
		fmt.Fprintf(&b, "  condition %s=%s (%s): %s\n", cond.Type, cond.Status, sanitize(cond.Reason), sanitize(cond.Message))
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
		fmt.Fprintf(&b, "  %s: %s (x%d)\n", sanitize(e.Reason), sanitize(e.Message), e.Count)
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
		// Kind and Name go through safetext.Line, not this file's sanitize
		// helper. sanitize also runs redact.Addresses, and of its three
		// alternatives only the dotted-quad IPv4 one has an optional port --
		// the bracketed IPv6 alternative requires a port besides its
		// brackets, and the dotted-DNS-name alternative requires one too. A
		// DNS-1123 object name can never contain the ':' either of those two
		// need, so the IPv4 alternative is the only one that can ever match
		// it, and it can match with no port present. A legal name that looks
		// like an IPv4 address, e.g. "192.0.2.1", would therefore be
		// rewritten to "<redacted>", breaking the scope match. Both places
		// below are built from the same sanitized values.
		for _, o := range p.OwnerReferences {
			kind, name := safetext.Line(o.Kind), safetext.Line(o.Name)
			scope.Add(kind, in.Namespace, name)
			fmt.Fprintf(&b, "owner of %s: %s %s\n", in.Name, kind, name)
		}
		return okResult(c.ID, b.String())
	case "node":
		if p.Spec.NodeName == "" {
			return okResult(c.ID, fmt.Sprintf("pod %s/%s is not scheduled to a node", in.Namespace, in.Name))
		}
		// safetext.Line, not sanitize, for the same reason as the owner arm
		// above: a legal node name that looks like an IPv4 address would be
		// rewritten by redact.Addresses, breaking the scope match. Both
		// sinks get the same sanitized value.
		node := safetext.Line(p.Spec.NodeName)
		scope.Add("node", "", node)
		return okResult(c.ID, fmt.Sprintf("node of %s: %s\n", in.Name, node))
	case "pvc":
		var names []string
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				// safetext.Line, not sanitize — same rule as the owner and
				// node arms above, same sanitized value in both sinks.
				n := safetext.Line(v.PersistentVolumeClaim.ClaimName)
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
