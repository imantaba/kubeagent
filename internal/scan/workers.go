package scan

import (
	"os"
	"strconv"
)

const (
	// defaultScanWorkers is how many of the scan's independent reads may be in
	// flight at once. Eight overlaps the scan's two dozen phase-1 reads into a
	// handful of round trips while staying well inside what one API server
	// answers comfortably. A worker cap is self-limiting in a way a request rate
	// is not: when the API server slows down, every worker is blocked on its own
	// in-flight request, so kubeagent slows down with it.
	defaultScanWorkers = 8

	minScanWorkers = 1
	maxScanWorkers = 64
)

// scanWorkers returns the scan's worker cap. KUBEAGENT_SCAN_WORKERS overrides
// the default; a value that does not parse is ignored, and a value outside
// 1..64 is clamped to the nearer bound. It never fails — a bad knob degrades to
// a working scan, not to an error.
//
// Under `kubeagent watch` the daemon runs one goroutine per cluster, so the
// effective cap across the process is this number times the number of clusters
// being watched.
func scanWorkers() int {
	s := os.Getenv("KUBEAGENT_SCAN_WORKERS")
	if s == "" {
		return defaultScanWorkers
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return defaultScanWorkers
	}
	if n < minScanWorkers {
		return minScanWorkers
	}
	if n > maxScanWorkers {
		return maxScanWorkers
	}
	return n
}
