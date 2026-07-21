// Package rootcause attributes a flagged workload's failure to a hard-down node
// (NotReady or kubelet-not-heartbeating) when the workload has a pod placed on
// that node — collapsing many disconnected findings toward one root cause. Pure
// and read-only; the caller supplies the workloads and the down-node list.
// Mirrors netpolicy/rollout.Annotate.
package rootcause

import (
	"sort"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate sets w.RootCause on each flagged workload that has a pod on a hard-down
// node. It mutates the slice elements in place. When several down nodes host the
// workload's pods, the node whose name sorts first is chosen (deterministic).
func Annotate(workloads []inventory.Workload, down []clusterhealth.DownNode) {
	if len(down) == 0 {
		return
	}
	reasonByNode := make(map[string]string, len(down))
	names := make([]string, 0, len(down))
	for _, d := range down {
		if _, seen := reasonByNode[d.Name]; !seen {
			names = append(names, d.Name)
		}
		reasonByNode[d.Name] = d.Reason
	}
	sort.Strings(names)
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() {
			continue
		}
		on := podNodes(*w)
		for _, name := range names {
			if on[name] {
				workloads[i].RootCause = "node " + name + " (" + reasonByNode[name] + ")"
				break
			}
		}
	}
}

// podNodes is the set of nodes this workload's pods are placed on.
func podNodes(w inventory.Workload) map[string]bool {
	on := make(map[string]bool, len(w.Pods))
	for _, p := range w.Pods {
		if p.Node != "" {
			on[p.Node] = true
		}
	}
	return on
}
