package capacity

import (
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// buildRightSizing applies the three structural rules and rolls the matches up by
// owner. Every rule is provable from the pod spec alone; no usage data is read
// here, and none of these rules can be satisfied or suppressed by a usage sample.
//
// namespace, when non-empty, scopes the enumeration only — the caller still passes
// the cluster-wide pod list, because included carries cluster-wide arithmetic.
func buildRightSizing(pods []corev1.Pod, replicaSets []appsv1.ReplicaSet,
	included []nodeCapacity, namespace string) *RightSizing {

	rsIndex := deploymentIndex(replicaSets)
	// key -> owner, so replicas of one workload collapse to a single row.
	matches := map[RuleName]map[string]Owner{
		RuleNoRequests:       {},
		RuleLimitNoRequest:   {},
		RuleNeverSchedulable: {},
	}

	var biggest nodeCapacity
	for _, n := range included {
		if n.cpuAlloc > biggest.cpuAlloc {
			biggest.cpuAlloc = n.cpuAlloc
		}
		if n.memAlloc > biggest.memAlloc {
			biggest.memAlloc = n.memAlloc
		}
	}

	for _, p := range pods {
		if terminal(p) || (namespace != "" && p.Namespace != namespace) {
			continue
		}
		o := ownerOf(p, rsIndex)
		key := o.Kind + "/" + o.Namespace + "/" + o.Name

		if noRequests, allBare := noRequestContainers(p); noRequests {
			e := o
			e.BestEffort = allBare
			// A pod already recorded without BestEffort must not be upgraded by a
			// sibling replica that happens to be bare; take the weaker claim.
			if prev, ok := matches[RuleNoRequests][key]; ok && !prev.BestEffort {
				e.BestEffort = false
			}
			matches[RuleNoRequests][key] = e
		}
		if detail := limitWithoutRequest(p); detail != "" {
			e := o
			e.Detail = detail
			if _, ok := matches[RuleLimitNoRequest][key]; !ok {
				matches[RuleLimitNoRequest][key] = e
			}
		}
		if len(included) > 0 {
			if detail := exceedsLargestNode(p, biggest); detail != "" {
				e := o
				e.Detail = detail
				if _, ok := matches[RuleNeverSchedulable][key]; !ok {
					matches[RuleNeverSchedulable][key] = e
				}
			}
		}
	}

	// Fixed rule order — a literal slice, never a range over the map.
	order := []RuleName{RuleNoRequests, RuleLimitNoRequest, RuleNeverSchedulable}
	var rules []Rule
	for _, name := range order {
		owners := make([]Owner, 0, len(matches[name]))
		for _, o := range matches[name] {
			owners = append(owners, o)
		}
		if len(owners) == 0 {
			continue
		}
		sort.Slice(owners, func(a, b int) bool {
			if owners[a].Namespace != owners[b].Namespace {
				return owners[a].Namespace < owners[b].Namespace
			}
			return owners[a].Name < owners[b].Name
		})
		r := Rule{Name: name}
		if len(owners) > maxOwnersPerRule {
			r.Truncated = len(owners) - maxOwnersPerRule
			owners = owners[:maxOwnersPerRule]
		}
		r.Owners = owners
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		return nil
	}
	return &RightSizing{Rules: rules}
}

// noRequestContainers reports whether any container declares neither a CPU nor a
// memory request, and whether every container is like that — the second is what
// makes the pod BestEffort.
func noRequestContainers(p corev1.Pod) (any_, all bool) {
	all = len(p.Spec.Containers) > 0
	for _, c := range p.Spec.Containers {
		bare := c.Resources.Requests.Cpu().IsZero() && c.Resources.Requests.Memory().IsZero()
		if bare {
			any_ = true
		} else {
			all = false
		}
	}
	return any_, all && any_
}

// limitWithoutRequest returns a detail string when a container sets a limit for a
// resource but no request for it. Kubernetes then defaults the request to the
// limit, so the workload reserves the full limit cluster-wide.
func limitWithoutRequest(p corev1.Pod) string {
	for _, c := range p.Spec.Containers {
		if !c.Resources.Limits.Cpu().IsZero() && c.Resources.Requests.Cpu().IsZero() {
			return "lim " + formatMilliCPU(c.Resources.Limits.Cpu().MilliValue()) + " cores"
		}
		if !c.Resources.Limits.Memory().IsZero() && c.Resources.Requests.Memory().IsZero() {
			return "lim " + formatBytes(c.Resources.Limits.Memory().Value())
		}
	}
	return ""
}

// exceedsLargestNode returns a detail string when a container requests more of a
// resource than the largest included node can ever offer.
func exceedsLargestNode(p corev1.Pod, biggest nodeCapacity) string {
	for _, c := range p.Spec.Containers {
		if cpu := c.Resources.Requests.Cpu().MilliValue(); cpu > biggest.cpuAlloc {
			return fmt.Sprintf("req %s cores > largest node (%s cores)",
				formatMilliCPU(cpu), formatMilliCPU(biggest.cpuAlloc))
		}
		if mem := c.Resources.Requests.Memory().Value(); mem > biggest.memAlloc {
			return fmt.Sprintf("req %s > largest node (%s)",
				formatBytes(mem), formatBytes(biggest.memAlloc))
		}
	}
	return ""
}

// deploymentIndex maps "namespace/replicaset" to its owning Deployment's name, so a
// pod can be rolled up two levels. ReplicaSets with no Deployment owner are absent
// from the map and the pod rolls up to the ReplicaSet itself.
func deploymentIndex(replicaSets []appsv1.ReplicaSet) map[string]string {
	idx := make(map[string]string, len(replicaSets))
	for _, rs := range replicaSets {
		if o := controllerOwner(rs.OwnerReferences); o != nil && o.Kind == "Deployment" {
			idx[rs.Namespace+"/"+rs.Name] = o.Name
		}
	}
	return idx
}

// ownerOf resolves a pod to the workload a human would name: through a ReplicaSet
// to its Deployment when possible, else the direct controller, else the pod.
func ownerOf(p corev1.Pod, rsIndex map[string]string) Owner {
	o := controllerOwner(p.OwnerReferences)
	if o == nil {
		return Owner{Kind: "Pod", Namespace: p.Namespace, Name: p.Name}
	}
	if o.Kind == "ReplicaSet" {
		if dep, ok := rsIndex[p.Namespace+"/"+o.Name]; ok {
			return Owner{Kind: "Deployment", Namespace: p.Namespace, Name: dep}
		}
	}
	return Owner{Kind: o.Kind, Namespace: p.Namespace, Name: o.Name}
}
