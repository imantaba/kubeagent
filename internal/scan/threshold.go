package scan

import (
	"os"
	"time"
)

// defaultTerminatingThreshold is how long a resource may sit in Terminating
// before termhealth.Assess flags it as stuck.
const defaultTerminatingThreshold = 2 * time.Minute

// terminatingThreshold returns the stuck-terminating threshold.
// KUBEAGENT_TERMINATING_THRESHOLD overrides the default; a value that does not
// parse as a Go duration, or that parses to zero or negative, falls back to the
// default rather than disabling the check — a threshold of zero would flag
// every deletion in flight. It never fails — a bad knob degrades to a working
// scan, not to an error.
func terminatingThreshold() time.Duration {
	s := os.Getenv("KUBEAGENT_TERMINATING_THRESHOLD")
	if s == "" {
		return defaultTerminatingThreshold
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return defaultTerminatingThreshold
	}
	return d
}
