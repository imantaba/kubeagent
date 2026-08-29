// Package netpolicy annotates workloads with the names of NetworkPolicies that
// select their pods — a root-cause hint for a degraded workload with no known
// detector cause. It is pure; the caller supplies workloads, pod labels, and
// policies.
package netpolicy

import (
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate sets w.NetworkPolicies for each flagged workload whose pods are
// selected by one or more NetworkPolicies in the same namespace and which has
// no detector finding that already explains it. podLabels maps
// "namespace/podName" to that pod's labels. It mutates the slice elements in
// place.
//
// "No finding that already explains it" is deliberately not the same as "no
// finding at all". A workload whose only findings are probe failures still
// gets the hint, because a blocked pod is precisely how a NetworkPolicy
// usually presents: traffic to the pod is dropped, the readiness probe times
// out, and a ProbeFailure is recorded. Suppressing on any finding meant the
// hint survived only when nothing else was wrong, which for a network block is
// close to never. Every other finding kind still suppresses: an OOMKilled
// workload has a cause, and in a cluster that uses policies at all most pods
// are selected by one, so an unconditional hint would be noise on everything.
func Annotate(workloads []inventory.Workload, podLabels map[string]map[string]string, policies []networkingv1.NetworkPolicy) {
	for i := range workloads {
		w := workloads[i]
		if !w.Flagged() || !onlyProbeFailures(w.Findings) {
			continue
		}
		if names := selectingPolicies(w, podLabels, policies); len(names) > 0 {
			workloads[i].NetworkPolicies = names
		}
	}
}

// onlyProbeFailures reports whether every finding is a probe failure — which is
// vacuously true for a workload with no findings at all, the original
// finding-less case. "ProbeFailure" is the kind internal/diagnose's probe
// detector emits; it is one of the sixteen kinds internal/knownissues
// documents.
func onlyProbeFailures(findings []diagnose.Finding) bool {
	for _, f := range findings {
		if f.Issue != "ProbeFailure" {
			return false
		}
	}
	return true
}

// selectingPolicies returns the sorted, de-duplicated names of NetworkPolicies in
// the workload's namespace whose podSelector matches any of its pods.
func selectingPolicies(w inventory.Workload, podLabels map[string]map[string]string, policies []networkingv1.NetworkPolicy) []string {
	set := map[string]struct{}{}
	for _, p := range policies {
		if p.Namespace != w.Namespace {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)
		if err != nil {
			continue // malformed selector — skip defensively
		}
		for _, pr := range w.Pods {
			if sel.Matches(labels.Set(podLabels[w.Namespace+"/"+pr.Name])) {
				set[p.Name] = struct{}{}
				break
			}
		}
	}
	if len(set) == 0 {
		return nil
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
