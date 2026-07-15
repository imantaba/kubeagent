// Package nodereserve reports each node's aggregate kubelet resource reservation
// for cpu, memory, and ephemeral-storage, observed as Capacity - Allocatable
// (kube-reserved + system-reserved + eviction-hard combined). It warns when a node
// reserves no memory or no ephemeral-storage, kubelet configurations that let
// OS/kubelet pressure destabilise the node. Pure: the caller supplies the nodes. Read-only.
package nodereserve

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// NodeReservation is one node's observed reservation. Reserved amounts are
// human-readable strings ("200m", "800Mi", "0", or "—" when the resource is not
// reported). Warning is set when the node reserves no memory; NoEphemeral/NoCPU
// when it reserves no ephemeral-storage / cpu.
type NodeReservation struct {
	Name              string `json:"name"`
	Role              string `json:"role,omitempty"`
	CPUReserved       string `json:"cpuReserved"`
	MemReserved       string `json:"memReserved"`
	EphemeralReserved string `json:"ephemeralReserved"`
	Warning           bool   `json:"warning"`
	NoEphemeral       bool   `json:"noEphemeralReserve,omitempty"`
	NoCPU             bool   `json:"noCPUReserve,omitempty"`
}

// Report is the per-node reservation picture. WarnCount is the number of nodes
// reserving no memory (unchanged; drives the daemon gauge). EphemeralNone/CPUNone
// count nodes reserving none of that resource; EphemeralReporting is the number of
// nodes that report ephemeral-storage capacity at all.
type Report struct {
	Nodes              []NodeReservation `json:"nodes"`
	WarnCount          int               `json:"warnCount"`
	EphemeralNone      int               `json:"ephemeralNone"`
	CPUNone            int               `json:"cpuNone"`
	EphemeralReporting int               `json:"ephemeralReporting"`
}

// Assess computes reserved cpu/memory/ephemeral-storage for each node as
// Capacity - Allocatable (clamped at 0) and flags nodes that reserve none of
// memory (Warning), ephemeral-storage (NoEphemeral), or cpu (NoCPU).
func Assess(nodes []corev1.Node) Report {
	rep := Report{Nodes: make([]NodeReservation, 0, len(nodes))}
	for _, n := range nodes {
		cpuRes := reserved(n.Status.Capacity[corev1.ResourceCPU], n.Status.Allocatable[corev1.ResourceCPU])
		memRes := reserved(n.Status.Capacity[corev1.ResourceMemory], n.Status.Allocatable[corev1.ResourceMemory])

		nr := NodeReservation{
			Name:        n.Name,
			Role:        role(n),
			CPUReserved: fmtCPU(cpuRes),
			MemReserved: fmtMem(memRes),
			Warning:     memRes.Value() == 0,
			NoCPU:       cpuRes.MilliValue() == 0,
		}
		if nr.Warning {
			rep.WarnCount++
		}
		if nr.NoCPU {
			rep.CPUNone++
		}

		if capEph, ok := n.Status.Capacity[corev1.ResourceEphemeralStorage]; ok {
			ephRes := reserved(capEph, n.Status.Allocatable[corev1.ResourceEphemeralStorage])
			nr.EphemeralReserved = fmtMem(ephRes)
			nr.NoEphemeral = ephRes.Value() == 0
			rep.EphemeralReporting++
			if nr.NoEphemeral {
				rep.EphemeralNone++
			}
		} else {
			nr.EphemeralReserved = "—"
		}

		rep.Nodes = append(rep.Nodes, nr)
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
