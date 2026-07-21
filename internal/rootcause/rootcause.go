// Package rootcause attributes a flagged workload's failure to a hard-down node
// (NotReady or kubelet-not-heartbeating) when the workload has a pod placed on
// that node — collapsing many disconnected findings toward one root cause. Pure
// and read-only; the caller supplies the workloads and the down-node list.
// Mirrors netpolicy/rollout.Annotate.
package rootcause

import (
	"fmt"
	"sort"
	"strings"

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

// AnnotateRegistry sets w.RootCause = "registry <host> (<N> workloads failing to
// pull)" on each flagged, not-yet-attributed workload whose image-pull failure
// shares a registry host with at least one other such workload — the shared
// signature of a registry outage, expired pull credentials, or rate limiting.
// Pure and deterministic (hosts processed in sorted order). Call after Annotate:
// node attribution wins, and a node-attributed workload is excluded from the
// group count too.
func AnnotateRegistry(workloads []inventory.Workload) {
	groups := map[string][]int{}
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() || w.RootCause != "" || w.Image == "" || !hasPullFailure(*w) {
			continue
		}
		host := registryHost(w.Image)
		groups[host] = append(groups[host], i)
	}
	hosts := make([]string, 0, len(groups))
	for h := range groups {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		members := groups[host]
		if len(members) < 2 {
			continue
		}
		cause := fmt.Sprintf("registry %s (%d workloads failing to pull)", host, len(members))
		for _, i := range members {
			workloads[i].RootCause = cause
		}
	}
}

// hasPullFailure reports whether the workload carries an image-pull finding.
func hasPullFailure(w inventory.Workload) bool {
	for _, f := range w.Findings {
		if f.Issue == "ImagePullBackOff" || f.Issue == "ErrImagePull" {
			return true
		}
	}
	return false
}

// registryHost extracts the registry host from a container image reference using
// the standard rules: the first path segment is a registry iff it contains "." or
// ":" or is "localhost"; otherwise the image lives on Docker Hub ("docker.io").
func registryHost(image string) string {
	seg, _, found := strings.Cut(image, "/")
	if !found || (!strings.ContainsAny(seg, ".:") && seg != "localhost") {
		return "docker.io"
	}
	return seg
}
