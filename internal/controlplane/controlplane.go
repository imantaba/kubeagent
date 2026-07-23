// Package controlplane classifies the apiserver /readyz?verbose response into an
// advisory control-plane / etcd health probe. Pure and read-only: the caller
// (collect) does the HTTP GET and passes the status code and body here. Mirrors
// the nodehealth classify helper for kubelet /healthz.
package controlplane

import "strings"

// Probe is the apiserver /readyz classification.
type Probe struct {
	Status string   `json:"status"`           // "ok" | "unhealthy" | "forbidden" | "unreachable"
	Failed []string `json:"failed,omitempty"` // failing check names when unhealthy
}

// ParseReadyz classifies an HTTP status code and /readyz?verbose body into a Probe.
// 200 is ok; 401/403 is forbidden (grant missing); code 0 (no HTTP status) is
// unreachable; any other code means not-ready — the failing checks are the names
// immediately after each "[-]" line of the verbose body.
func ParseReadyz(code int, body []byte) Probe {
	switch {
	case code == 200:
		return Probe{Status: "ok"}
	case code == 401 || code == 403:
		return Probe{Status: "forbidden"}
	case code == 0:
		return Probe{Status: "unreachable"}
	default:
		return Probe{Status: "unhealthy", Failed: failedChecks(body)}
	}
}

// failedChecks extracts the check name from each "[-]<name> …" line of a verbose
// /readyz body, in order. Returns nil when there are none (a generic not-ready).
func failedChecks(body []byte) []string {
	var failed []string
	for _, ln := range strings.Split(string(body), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "[-]") {
			if fields := strings.Fields(ln[3:]); len(fields) > 0 {
				failed = append(failed, fields[0])
			}
		}
	}
	return failed
}
