package capacity

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/imantaba/kubeagent/internal/resources"
)

// buildHeadroom reduces the included nodes to the four headroom rows. It returns a
// block even when nothing is included, because the exclusion list is itself the
// answer in that case.
func buildHeadroom(included []nodeCapacity, excluded []NodeExclusion, total int, pods []corev1.Pod) *Headroom {
	h := &Headroom{
		IncludedNodes: len(included),
		TotalNodes:    total,
		Excluded:      excluded,
		FreeCPU:       formatMilliCPU(0),
		FreeMemory:    formatBytes(0),
	}
	if len(included) == 0 {
		return h
	}

	var freeCPU, freeMem int64
	cpuWinner, memWinner := 0, 0
	tightest, tightestPct, tightestRes := 0, -1, "CPU"
	for i, n := range included {
		freeCPU += n.freeCPU()
		freeMem += n.freeMem()
		if n.freeCPU() > included[cpuWinner].freeCPU() {
			cpuWinner = i
		}
		if n.freeMem() > included[memWinner].freeMem() {
			memWinner = i
		}
		cpuPct, memPct := ratio(n.cpuReq, n.cpuAlloc), ratio(n.memReq, n.memAlloc)
		pct, res := cpuPct, "CPU"
		if memPct > cpuPct {
			pct, res = memPct, "memory"
		}
		if pct > tightestPct {
			tightest, tightestPct, tightestRes = i, pct, res
		}
	}

	h.FreeCPU = formatMilliCPU(freeCPU)
	h.FreeMemory = formatBytes(freeMem)
	h.LargestCPUFit = fitOf(included[cpuWinner])
	if memWinner != cpuWinner {
		h.LargestMemFit = fitOf(included[memWinner])
	}
	h.TightestNode = &TightNode{
		Node: included[tightest].name, Resource: tightestRes, Pct: tightestPct,
	}
	// h.NodeLoss is filled by Task 3, which adds nodeloss.go and the one call line
	// here. The pods parameter exists for it and is unused in this task — an unused
	// function parameter is legal Go and no placeholder is needed.
	return h
}

func fitOf(n nodeCapacity) *NodeFit {
	return &NodeFit{
		Node:   n.name,
		CPU:    formatMilliCPU(n.freeCPU()),
		Memory: formatBytes(n.freeMem()),
	}
}

// ratio is percent of whole, 0 when whole is not positive — the same guard
// internal/resources applies.
func ratio(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	return int(part * 100 / whole)
}

// formatMilliCPU renders milli-cores through the shared formatter so the CAPACITY
// section and the Resources block never disagree about the same number.
func formatMilliCPU(milli int64) string {
	return resources.FormatCPU(*resource.NewMilliQuantity(milli, resource.DecimalSI))
}

func formatBytes(b int64) string {
	return resources.FormatMem(*resource.NewQuantity(b, resource.BinarySI))
}
