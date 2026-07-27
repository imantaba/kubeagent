// Package capacity reports scheduling headroom and structurally wrong workload
// shapes. It is pure: the caller supplies nodes, pods, ReplicaSets, and an optional
// per-pod usage sample. It issues no API calls and is advisory only — nothing here
// produces a Finding or changes the cluster verdict.
//
// Two numbers this package deliberately never produces. It never renders money:
// no cluster publishes prices, so every figure is cores and GiB. And it never
// claims a peak: metrics-server returns a single sample of roughly a 30-second
// average and retains no history, so a usage reading is attached as context to a
// workload some structural rule already flagged, and never selects one by itself.
package capacity

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuleName identifies a structural right-sizing rule. The constant is the JSON and
// lookup key; the report owns the human label.
type RuleName string

const (
	// RuleNoRequests: a container declares neither a CPU nor a memory request.
	RuleNoRequests RuleName = "noRequests"
	// RuleLimitNoRequest: a container sets a limit for a resource but no request
	// for it, so Kubernetes defaults the request to the limit.
	RuleLimitNoRequest RuleName = "limitNoRequest"
	// RuleNeverSchedulable: a container request exceeds the largest included node's
	// allocatable, so the pod can never be placed.
	RuleNeverSchedulable RuleName = "neverSchedulable"
)

// maxOwnersPerRule caps enumeration per rule; the remainder is reported as a count,
// never silently dropped. Matches the internal/gitops cap.
const maxOwnersPerRule = 20

// Report is the advisory capacity view. Either half may be nil.
type Report struct {
	Headroom    *Headroom    `json:"headroom,omitempty"`
	RightSizing *RightSizing `json:"rightSizing,omitempty"`
}

// Headroom is the scheduling picture over included nodes only.
type Headroom struct {
	IncludedNodes int             `json:"includedNodes"`
	TotalNodes    int             `json:"totalNodes"`
	FreeCPU       string          `json:"freeCPU"`
	FreeMemory    string          `json:"freeMemory"`
	LargestCPUFit *NodeFit        `json:"largestCPUFit,omitempty"`
	LargestMemFit *NodeFit        `json:"largestMemFit,omitempty"`
	TightestNode  *TightNode      `json:"tightestNode,omitempty"`
	NodeLoss      *NodeLoss       `json:"nodeLoss,omitempty"`
	Excluded      []NodeExclusion `json:"excluded,omitempty"`
}

// NodeFit is one node's free capacity. CPU and Memory always come from the SAME
// node: a pod lands on one node, so a cross-node maximum would describe a shape
// nothing can schedule.
type NodeFit struct {
	Node   string `json:"node"`
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// TightNode is the node closest to full, by whichever of its two ratios is higher.
type TightNode struct {
	Node     string `json:"node"`
	Resource string `json:"resource"` // "CPU" or "memory"
	Pct      int    `json:"pct"`
}

// NodeLoss is the first-fit-decreasing result of removing the largest included
// node. Fits=true is a constructive proof the requests fit; Fits=false is not a
// proof they do not, which is why the report says "may not fit".
type NodeLoss struct {
	Node       string `json:"node"`
	SingleNode bool   `json:"singleNode"`
	Fits       bool   `json:"fits"`
	Placed     int    `json:"placed"`
	Blocker    string `json:"blocker,omitempty"`
	BlockerCPU string `json:"blockerCPU,omitempty"`
}

// NodeExclusion names a node whose capacity was not counted, and why.
type NodeExclusion struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// RightSizing carries the structural rules that matched, plus sample coverage.
type RightSizing struct {
	Rules            []Rule `json:"rules,omitempty"`
	MetricsAvailable bool   `json:"metricsAvailable"`
	PodsReporting    int    `json:"podsReporting"`
	PodsTotal        int    `json:"podsTotal"`
}

// Rule is one structural rule and the owners that matched it.
type Rule struct {
	Name      RuleName `json:"name"`
	Owners    []Owner  `json:"owners"`
	Truncated int      `json:"truncated,omitempty"`
}

// Owner is one flagged workload. Detail is rule-specific; Observed is the attached
// usage sample and is empty when metrics-server did not answer for its pods.
type Owner struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Detail     string `json:"detail,omitempty"`
	Observed   string `json:"observed,omitempty"`
	BestEffort bool   `json:"bestEffort,omitempty"`
}

// nodeCapacity is one included node's arithmetic, in milli-cores and bytes.
type nodeCapacity struct {
	name     string
	cpuAlloc int64
	memAlloc int64
	cpuReq   int64
	memReq   int64
}

func (n nodeCapacity) freeCPU() int64 { return max64(0, n.cpuAlloc-n.cpuReq) }
func (n nodeCapacity) freeMem() int64 { return max64(0, n.memAlloc-n.memReq) }

// classifyNodes splits nodes into the included set (Ready, schedulable, untainted)
// and the excluded set with a reason, and accumulates each included node's pod
// requests. Pods on excluded nodes are ignored: their requests sit on capacity the
// section has already disclaimed. Both slices are sorted by node name.
//
// Exactly one reason is reported per excluded node, in the order checked here, so
// output never varies between runs.
func classifyNodes(nodes []corev1.Node, pods []corev1.Pod) ([]nodeCapacity, []NodeExclusion) {
	var included []nodeCapacity
	var excluded []NodeExclusion
	byName := map[string]int{}
	for _, n := range nodes {
		if reason := excludeReason(n); reason != "" {
			excluded = append(excluded, NodeExclusion{Node: n.Name, Reason: reason})
			continue
		}
		byName[n.Name] = len(included)
		included = append(included, nodeCapacity{
			name:     n.Name,
			cpuAlloc: n.Status.Allocatable.Cpu().MilliValue(),
			memAlloc: n.Status.Allocatable.Memory().Value(),
		})
	}
	for _, p := range pods {
		i, ok := byName[p.Spec.NodeName]
		if !ok || terminal(p) {
			continue
		}
		cpu, mem := podRequests(p)
		included[i].cpuReq += cpu
		included[i].memReq += mem
	}
	sort.Slice(included, func(a, b int) bool { return included[a].name < included[b].name })
	sort.Slice(excluded, func(a, b int) bool { return excluded[a].Node < excluded[b].Node })
	return included, excluded
}

// excludeReason returns "" when the node is usable for ordinary scheduling.
func excludeReason(n corev1.Node) string {
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
			break
		}
	}
	if !ready {
		return "NotReady"
	}
	if n.Spec.Unschedulable {
		return "cordoned"
	}
	for _, t := range n.Spec.Taints {
		switch t.Effect {
		case corev1.TaintEffectNoSchedule:
			return "NoSchedule taint"
		case corev1.TaintEffectNoExecute:
			return "NoExecute taint"
		}
	}
	return ""
}

// podRequests sums a pod's container requests as milli-cores and bytes.
func podRequests(p corev1.Pod) (cpu, mem int64) {
	for _, c := range p.Spec.Containers {
		cpu += c.Resources.Requests.Cpu().MilliValue()
		mem += c.Resources.Requests.Memory().Value()
	}
	return cpu, mem
}

// terminal reports whether a pod has stopped and so reserves nothing.
func terminal(p corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ownedByDaemonSet reports whether the pod's controller owner is a DaemonSet.
// DaemonSet pods do not rehome when their node is removed, so node-loss arithmetic
// excludes them.
func ownedByDaemonSet(p corev1.Pod) bool {
	o := controllerOwner(p.OwnerReferences)
	return o != nil && o.Kind == "DaemonSet"
}

// controllerOwner returns the controller ownerReference, or the first reference
// when none is marked controller, or nil when there are none.
func controllerOwner(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}
