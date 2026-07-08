// Package nodereserve reports each node's aggregate kubelet resource
// reservation, observed as Capacity - Allocatable (kube-reserved +
// system-reserved + eviction-hard combined). It warns when a node reserves no
// memory, a kubelet configuration that lets OS/kubelet memory pressure
// destabilise the node. Pure: the caller supplies the nodes. Read-only.
package nodereserve

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// NodeReservation is one node's observed reservation. Reserved amounts are
// human-readable strings ("200m", "800Mi", "0"). Warning is set when the node
// reserves no memory.
type NodeReservation struct {
	Name        string `json:"name"`
	Role        string `json:"role,omitempty"`
	CPUReserved string `json:"cpuReserved"`
	MemReserved string `json:"memReserved"`
	Warning     bool   `json:"warning"`
}

// Report is the per-node reservation picture. WarnCount is the number of nodes
// with Warning set.
type Report struct {
	Nodes     []NodeReservation `json:"nodes"`
	WarnCount int               `json:"warnCount"`
}

// Assess computes reserved cpu/memory for each node as Capacity - Allocatable
// (clamped at 0) and flags nodes that reserve no memory.
func Assess(nodes []corev1.Node) Report {
	rep := Report{Nodes: make([]NodeReservation, 0, len(nodes))}
	for _, n := range nodes {
		cpuRes := reserved(n.Status.Capacity[corev1.ResourceCPU], n.Status.Allocatable[corev1.ResourceCPU])
		memRes := reserved(n.Status.Capacity[corev1.ResourceMemory], n.Status.Allocatable[corev1.ResourceMemory])
		warn := memRes.Value() == 0
		if warn {
			rep.WarnCount++
		}
		rep.Nodes = append(rep.Nodes, NodeReservation{
			Name:        n.Name,
			Role:        role(n),
			CPUReserved: fmtCPU(cpuRes),
			MemReserved: fmtMem(memRes),
			Warning:     warn,
		})
	}
	return rep
}

// reserved returns capacity - allocatable, clamped to zero on a negative delta.
func reserved(capacity, allocatable resource.Quantity) resource.Quantity {
	out := capacity.DeepCopy()
	out.Sub(allocatable)
	if out.Sign() < 0 {
		return resource.Quantity{}
	}
	return out
}

// role classifies the node from its node-role labels.
func role(n corev1.Node) string {
	for k := range n.Labels {
		if k == "node-role.kubernetes.io/control-plane" || k == "node-role.kubernetes.io/master" {
			return "control-plane"
		}
	}
	return "worker"
}

// fmtCPU renders reserved cpu as millicores ("200m") or "0".
func fmtCPU(q resource.Quantity) string {
	m := q.MilliValue()
	if m <= 0 {
		return "0"
	}
	return fmt.Sprintf("%dm", m)
}

// fmtMem renders reserved memory in Gi/Mi ("1Gi", "800Mi") or "0".
func fmtMem(q resource.Quantity) string {
	b := q.Value()
	if b <= 0 {
		return "0"
	}
	if b >= 1<<30 {
		return fmt.Sprintf("%.0fGi", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
}
