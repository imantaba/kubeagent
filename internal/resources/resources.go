// Package resources computes a cluster-wide CPU and memory summary: allocatable
// capacity, reserved (pod requests) and limits, and optional live usage from
// metrics-server. It is pure — the caller supplies nodes, pods, and usage.
package resources

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Line is one resource's cluster accounting. Quantities are human-readable
// strings; percentages are integers of allocatable (0 when allocatable is 0).
type Line struct {
	Allocatable string `json:"allocatable"`
	Requests    string `json:"requests"`
	Limits      string `json:"limits"`
	Usage       string `json:"usage,omitempty"` // "" when metrics unavailable
	RequestsPct int    `json:"requestsPct"`
	LimitsPct   int    `json:"limitsPct"`
	UsagePct    int    `json:"usagePct,omitempty"`
}

// Summary is the cluster-wide CPU and memory picture.
type Summary struct {
	CPU              Line `json:"cpu"`
	Memory           Line `json:"memory"`
	MetricsAvailable bool `json:"metricsAvailable"`
}

// Summarize aggregates node allocatable, pod requests/limits (over non-terminal
// pods only — terminal pods reserve nothing), and optional per-node usage into a
// cluster Summary. nil/empty usage yields MetricsAvailable=false.
func Summarize(nodes []corev1.Node, pods []corev1.Pod, usage map[string]corev1.ResourceList) Summary {
	var cpuAlloc, memAlloc, cpuReq, cpuLim, memReq, memLim, cpuUse, memUse resource.Quantity
	for _, n := range nodes {
		cpuAlloc.Add(n.Status.Allocatable[corev1.ResourceCPU])
		memAlloc.Add(n.Status.Allocatable[corev1.ResourceMemory])
	}
	for _, p := range pods {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			cpuReq.Add(c.Resources.Requests[corev1.ResourceCPU])
			cpuLim.Add(c.Resources.Limits[corev1.ResourceCPU])
			memReq.Add(c.Resources.Requests[corev1.ResourceMemory])
			memLim.Add(c.Resources.Limits[corev1.ResourceMemory])
		}
	}
	available := len(usage) > 0
	for _, u := range usage {
		cpuUse.Add(u[corev1.ResourceCPU])
		memUse.Add(u[corev1.ResourceMemory])
	}
	return Summary{
		MetricsAvailable: available,
		CPU:              cpuLine(cpuAlloc, cpuReq, cpuLim, cpuUse, available),
		Memory:           memLine(memAlloc, memReq, memLim, memUse, available),
	}
}

func cpuLine(alloc, req, lim, use resource.Quantity, available bool) Line {
	a := alloc.MilliValue()
	l := Line{
		Allocatable: formatCPU(alloc),
		Requests:    formatCPU(req),
		Limits:      formatCPU(lim),
		RequestsPct: pct(req.MilliValue(), a),
		LimitsPct:   pct(lim.MilliValue(), a),
	}
	if available {
		l.Usage = formatCPU(use)
		l.UsagePct = pct(use.MilliValue(), a)
	}
	return l
}

func memLine(alloc, req, lim, use resource.Quantity, available bool) Line {
	a := alloc.Value()
	l := Line{
		Allocatable: formatMem(alloc),
		Requests:    formatMem(req),
		Limits:      formatMem(lim),
		RequestsPct: pct(req.Value(), a),
		LimitsPct:   pct(lim.Value(), a),
	}
	if available {
		l.Usage = formatMem(use)
		l.UsagePct = pct(use.Value(), a)
	}
	return l
}

func pct(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	return int(part * 100 / whole)
}

// formatCPU renders a quantity as cores with one decimal, e.g. "8.0".
func formatCPU(q resource.Quantity) string {
	return fmt.Sprintf("%.1f", float64(q.MilliValue())/1000)
}

// formatMem renders a quantity in Gi (or Mi below 1Gi), rounded, e.g. "16Gi".
func formatMem(q resource.Quantity) string {
	b := q.Value()
	if b >= 1<<30 {
		return fmt.Sprintf("%.0fGi", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
}
