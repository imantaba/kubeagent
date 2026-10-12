package investigate

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/hypothesis"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// Size bounds for local verdict mode's evidence pre-fetch. The global read
// budget is maxToolCalls — the same 8 the tool loop enforces — so neither
// mode can out-read the other.
const (
	maxReadBytes       = 4096
	maxGatherWorkloads = 10
	truncationMarker   = "[truncated by kubeagent]"
)

// flaggedScope returns the first maxGatherWorkloads flagged workloads in
// report order — the rows the operator sees first are the rows the model
// judges.
func flaggedScope(workloads []inventory.Workload) []inventory.Workload {
	var scoped []inventory.Workload
	for _, w := range workloads {
		if !w.Flagged() {
			continue
		}
		scoped = append(scoped, w)
		if len(scoped) == maxGatherWorkloads {
			break
		}
	}
	return scoped
}

// gatherEvidence is local verdict mode's deterministic evidence pre-fetch:
// kubeagent chooses the reads, in report order, under the tool loop's global
// budget. Per workload, in order: the events of its first finding's pod (the
// workload name when there is no finding), a describe per surviving node or
// PVC candidate (registry candidates have nothing to read), and a classified
// previous-log cause per crash-family finding (deduped per container). A
// describe is read once: a later workload with the same node or PVC gets a
// copy of the same section text, at no budget cost, even after the budget
// is spent — each prompt holds one workload, so each needs it. It returns
// the evidence trail — byte-for-byte the tool loop's label() formats, one
// entry per read actually made —, one evidence string per scoped workload
// (evidence[i] belongs to scoped[i]; "" when no read reached it), and the
// objects it read, keyed for the rules. A failed read still consumes budget
// (refusal is evidence) and renders as a reduced error, never a raw
// client-go message; the same reduced error, passed through safetext.Line,
// is what the rules see under Reads.Failed.
func gatherEvidence(ctx context.Context, client kubernetes.Interface, scoped []inventory.Workload) ([]string, []string, hypothesis.Reads) {
	var (
		trail []string
		spent int
	)
	evidence := make([]string, len(scoped))
	fresh := hypothesis.Reads{
		Nodes:  map[string]*corev1.Node{},
		PVCs:   map[string]*corev1.PersistentVolumeClaim{},
		Events: map[string][]corev1.Event{},
		Failed: map[string]string{},
	}
	described := map[string]string{} // describe key → the section it rendered
	seenLog := map[string]bool{}
	for i, w := range scoped {
		var b strings.Builder
		if spent < maxToolCalls {
			name := w.Name
			if len(w.Findings) > 0 {
				if p := podPart(w.Findings[0].Pod); p != "" {
					name = p
				}
			}
			var content string
			items, err := listEvents(ctx, client, w.Namespace, name)
			if err != nil {
				fresh.Failed["events/"+w.Namespace+"/"+name] = safetext.Line(redact.Error(err))
				content = "read failed: " + redact.Error(err)
			} else {
				fresh.Events[w.Namespace+"/"+name] = items
				content = formatEvents(w.Namespace, name, items)
			}
			appendRead(&b, &trail, &spent, fmt.Sprintf("events %s/%s", w.Namespace, name), content)
		}

		own := map[string]bool{} // describes already in this workload's evidence
		for _, h := range w.RootCauseTrace {
			if h.Verdict == inventory.VerdictRuledOut || h.Object == "" {
				continue
			}
			if h.Kind != "node" && h.Kind != "pvc" {
				continue // registry: no object to read
			}
			ns := ""
			if h.Kind == "pvc" {
				ns = w.Namespace
			}
			key := h.Kind + "/" + ns + "/" + h.Object
			if own[key] {
				continue
			}
			if sec, ok := described[key]; ok {
				own[key] = true
				b.WriteString(sec)
				continue
			}
			if spent >= maxToolCalls {
				continue // a later candidate may still be a cached describe
			}
			own[key] = true
			var content string
			switch h.Kind {
			case "node":
				n, err := client.CoreV1().Nodes().Get(ctx, h.Object, metav1.GetOptions{})
				if err != nil {
					fresh.Failed["node/"+h.Object] = safetext.Line(redact.Error(err))
					content = "read failed: " + redact.Error(err)
				} else {
					fresh.Nodes[h.Object] = n
					content = describeNode(n)
				}
			case "pvc":
				pvc, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, h.Object, metav1.GetOptions{})
				if err != nil {
					fresh.Failed["pvc/"+ns+"/"+h.Object] = safetext.Line(redact.Error(err))
					content = "read failed: " + redact.Error(err)
				} else {
					fresh.PVCs[ns+"/"+h.Object] = pvc
					content = describePVC(pvc)
				}
			}
			described[key] = appendRead(&b, &trail, &spent, fmt.Sprintf("describe %s %s/%s", h.Kind, ns, h.Object), content)
		}

		for _, f := range w.Findings {
			if spent >= maxToolCalls {
				break
			}
			if !crashFamily(f.Issue) || f.Container == "" {
				continue
			}
			pod := podPart(f.Pod)
			if pod == "" {
				continue
			}
			key := w.Namespace + "/" + pod + "/" + f.Container
			if seenLog[key] {
				continue
			}
			seenLog[key] = true
			log, ok, err := collect.PreviousLogs(ctx, client, w.Namespace, pod, f.Container)
			res := logCauseResult("", w.Namespace, pod, f.Container, log, ok, err)
			appendRead(&b, &trail, &spent, fmt.Sprintf("log causes %s/%s container %s", w.Namespace, pod, f.Container), res.Content)
		}
		evidence[i] = b.String()
	}
	return trail, evidence, fresh
}

// appendRead records one completed read: one trail entry, one budget unit,
// one evidence section, which it also returns so a describe can be copied
// into a later workload's evidence. Content arrives already reduced (never a
// raw error) and is capped at maxReadBytes here; trailing newlines are
// normalized so a section is always exactly "== label ==\n<content>\n\n".
func appendRead(b *strings.Builder, trail *[]string, spent *int, label, content string) string {
	*trail = append(*trail, label)
	*spent++
	sec := "== " + label + " ==\n" + strings.TrimRight(capContent(content), "\n") + "\n\n"
	b.WriteString(sec)
	return sec
}

// capContent bounds one read's contribution to the prompt. A cut lands on
// the last full line inside the cap and is marked, so the model never sees a
// half-written line and a truncated read is visible as such.
func capContent(s string) string {
	if len(s) <= maxReadBytes {
		return s
	}
	cut := s[:maxReadBytes]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + "\n" + truncationMarker
}

// podPart extracts the pod name from a finding's "namespace/name" Pod field.
func podPart(pod string) string {
	if _, name, ok := strings.Cut(pod, "/"); ok {
		return name
	}
	return ""
}

// crashFamily reports whether an issue kind implies a crashed container
// whose previous-instance log tail can be classified.
func crashFamily(issue string) bool {
	return issue == "CrashLoopBackOff" || issue == "ContainerStartError" || issue == "OOMKilled"
}
