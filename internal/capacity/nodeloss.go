package capacity

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// nodeLoss removes the largest included node and tries to place its non-DaemonSet
// pods on the remaining included nodes by first-fit-decreasing.
//
// The heuristic is one-sided sound. A successful pass is a constructive placement
// and therefore proves the requests fit; a failed pass proves nothing, because a
// different packing may still succeed. Callers must render the failure as "may not
// fit", never as "does not fit".
//
// This is resource arithmetic only: it ignores affinity and anti-affinity, topology
// spread constraints, PVC zoning, and PodDisruptionBudgets.
func nodeLoss(included []nodeCapacity, pods []corev1.Pod) *NodeLoss {
	if len(included) == 0 {
		return nil
	}
	victim := largestNode(included)
	if len(included) == 1 {
		return &NodeLoss{Node: victim.name, SingleNode: true}
	}

	type slot struct {
		cpu, mem int64
	}
	remaining := make([]*slot, 0, len(included)-1)
	for _, n := range included {
		if n.name == victim.name {
			continue
		}
		remaining = append(remaining, &slot{cpu: n.freeCPU(), mem: n.freeMem()})
	}

	evictees := make([]corev1.Pod, 0, len(pods))
	for _, p := range pods {
		if p.Spec.NodeName != victim.name || terminal(p) || ownedByDaemonSet(p) {
			continue
		}
		evictees = append(evictees, p)
	}
	// Decreasing by CPU, then memory, then namespace/name so equal pods keep a
	// stable order and the reported blocker never varies between runs.
	sort.Slice(evictees, func(a, b int) bool {
		ca, ma := podRequests(evictees[a])
		cb, mb := podRequests(evictees[b])
		switch {
		case ca != cb:
			return ca > cb
		case ma != mb:
			return ma > mb
		case evictees[a].Namespace != evictees[b].Namespace:
			return evictees[a].Namespace < evictees[b].Namespace
		default:
			return evictees[a].Name < evictees[b].Name
		}
	})

	placed := 0
	for _, p := range evictees {
		cpu, mem := podRequests(p)
		fitted := false
		for _, s := range remaining {
			if s.cpu >= cpu && s.mem >= mem {
				s.cpu -= cpu
				s.mem -= mem
				fitted = true
				break
			}
		}
		if !fitted {
			return &NodeLoss{
				Node:       victim.name,
				Placed:     placed,
				Blocker:    ownerLabel(p),
				BlockerCPU: formatMilliCPU(cpu),
			}
		}
		placed++
	}
	return &NodeLoss{Node: victim.name, Fits: true, Placed: placed}
}

// largestNode picks the included node with the most allocatable CPU, breaking ties
// by name ascending so the row is deterministic on a uniform cluster.
func largestNode(included []nodeCapacity) nodeCapacity {
	best := included[0]
	for _, n := range included[1:] {
		if n.cpuAlloc > best.cpuAlloc || (n.cpuAlloc == best.cpuAlloc && n.name < best.name) {
			best = n
		}
	}
	return best
}

// ownerLabel names a pod by its controller owner, e.g. "StatefulSet/prod/db", or
// by the pod itself when it has no owner. It does not resolve ReplicaSet up to
// Deployment — the blocker line names the object first-fit could not place, and
// Task 4's roll-up is a separate concern with different inputs.
func ownerLabel(p corev1.Pod) string {
	if o := controllerOwner(p.OwnerReferences); o != nil {
		return fmt.Sprintf("%s/%s/%s", o.Kind, p.Namespace, o.Name)
	}
	return fmt.Sprintf("Pod/%s/%s", p.Namespace, p.Name)
}
