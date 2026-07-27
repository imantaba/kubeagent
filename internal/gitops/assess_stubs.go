package gitops

import "time"

// Temporary stubs; Task 2 replaces assessArgo and Task 3 replaces the Flux pair.
func assessArgo(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessment{State: StateUnknown}
}

func assessKustomization(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessment{State: StateUnknown}
}

func assessHelmRelease(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessment{State: StateUnknown}
}
