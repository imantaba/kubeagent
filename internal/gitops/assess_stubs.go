package gitops

import "time"

// Temporary stubs; Task 3 replaces the Flux pair.
func assessKustomization(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessment{State: StateUnknown}
}

func assessHelmRelease(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessment{State: StateUnknown}
}
