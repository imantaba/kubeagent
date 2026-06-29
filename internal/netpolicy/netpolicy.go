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

	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate sets w.NetworkPolicies for each workload that is flagged with no
// detector finding and whose pods are selected by one or more NetworkPolicies in
// the same namespace. podLabels maps "namespace/podName" to that pod's labels.
// It mutates the slice elements in place.
func Annotate(workloads []inventory.Workload, podLabels map[string]map[string]string, policies []networkingv1.NetworkPolicy) {
	for i := range workloads {
		w := workloads[i]
		if !w.Flagged() || len(w.Findings) > 0 {
			continue
		}
		if names := selectingPolicies(w, podLabels, policies); len(names) > 0 {
			workloads[i].NetworkPolicies = names
		}
	}
}

// selectingPolicies returns the sorted, de-duplicated names of NetworkPolicies in
// the workload's namespace whose podSelector matches any of its pods.
func selectingPolicies(w inventory.Workload, podLabels map[string]map[string]string, policies []networkingv1.NetworkPolicy) []string {
	set := map[string]bool{}
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
				set[p.Name] = true
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
