// Package nodehealth turns per-node kubelet /healthz probe results into an
// advisory report of nodes whose kubelet is reachable but reporting unhealthy.
// Pure and read-only: the caller (scan) does the probing.
package nodehealth

// Probe is one node's kubelet /healthz classification.
type Probe struct {
	Node   string `json:"node"`
	Status string `json:"status"` // "ok" | "unhealthy" | "forbidden" | "unreachable"
	Detail string `json:"detail,omitempty"`
}

// Issue is one node flagged unhealthy.
type Issue struct {
	Node   string `json:"node"`
	Detail string `json:"detail,omitempty"`
}

// Report is the advisory kubelet-health result.
type Report struct {
	Unhealthy []Issue `json:"unhealthy,omitempty"`
	Probed    int     `json:"probed"`
	Forbidden int     `json:"forbidden"`
}

// Assess collapses per-node probes into the report: the unhealthy nodes plus the
// probed/forbidden counts (used for the daemon gauge and the missing-grant hint).
func Assess(probes []Probe) Report {
	rep := Report{Probed: len(probes)}
	for _, p := range probes {
		switch p.Status {
		case "unhealthy":
			rep.Unhealthy = append(rep.Unhealthy, Issue{Node: p.Node, Detail: p.Detail})
		case "forbidden":
			rep.Forbidden++
		}
	}
	return rep
}
