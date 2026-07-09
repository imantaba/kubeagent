// Package diskusage flags node root filesystems and PVCs at or over a usage
// threshold, from parsed kubelet /stats/summary data. Pure: the caller supplies
// the per-node summaries. Read-only, opt-in (the collection needs nodes/proxy).
package diskusage

import "sort"

// NodeSummary is the slice of a node's kubelet /stats/summary that we consume.
type NodeSummary struct {
	Node    string
	FSUsed  int64
	FSCap   int64
	Volumes []PVCVolume
}

// PVCVolume is one pod volume backed by a PVC.
type PVCVolume struct {
	Namespace string
	Name      string
	Used      int64
	Cap       int64
}

// VolumeUsage is one node fs or PVC's usage.
type VolumeUsage struct {
	Kind          string  `json:"kind"` // "node" | "pvc"
	Node          string  `json:"node,omitempty"`
	Namespace     string  `json:"namespace,omitempty"`
	Name          string  `json:"name"`
	UsedBytes     int64   `json:"usedBytes"`
	CapacityBytes int64   `json:"capacityBytes"`
	Ratio         float64 `json:"ratio"`
}

// Report is the disk-usage picture. Over holds node+PVC volumes at/over the
// threshold (for display), highest ratio first. Nodes holds every node's fs
// ratio (for the daemon gauge), regardless of threshold.
type Report struct {
	Over      []VolumeUsage `json:"over"`
	Nodes     []VolumeUsage `json:"nodes,omitempty"`
	Threshold float64       `json:"threshold"`
}

// Assess flags volumes whose used/capacity ratio is >= threshold. Volumes with
// zero capacity are skipped.
func Assess(stats []NodeSummary, threshold float64) Report {
	rep := Report{Over: []VolumeUsage{}, Threshold: threshold}
	for _, s := range stats {
		if s.FSCap > 0 {
			r := ratio(s.FSUsed, s.FSCap)
			node := VolumeUsage{Kind: "node", Node: s.Node, Name: s.Node, UsedBytes: s.FSUsed, CapacityBytes: s.FSCap, Ratio: r}
			rep.Nodes = append(rep.Nodes, node)
			if r >= threshold {
				rep.Over = append(rep.Over, node)
			}
		}
		for _, v := range s.Volumes {
			if v.Cap <= 0 {
				continue
			}
			r := ratio(v.Used, v.Cap)
			if r >= threshold {
				rep.Over = append(rep.Over, VolumeUsage{
					Kind: "pvc", Namespace: v.Namespace, Name: v.Name,
					UsedBytes: v.Used, CapacityBytes: v.Cap, Ratio: r,
				})
			}
		}
	}
	sort.SliceStable(rep.Over, func(i, j int) bool {
		if rep.Over[i].Ratio != rep.Over[j].Ratio {
			return rep.Over[i].Ratio > rep.Over[j].Ratio
		}
		return rep.Over[i].Name < rep.Over[j].Name
	})
	return rep
}

func ratio(used, capacity int64) float64 {
	return float64(used) / float64(capacity)
}
