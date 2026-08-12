package diagnose

import "time"

// DefaultDetectors returns the detector set every kubeagent command runs, in the
// order findings are reported.
//
// now is a parameter rather than a time.Now() call because three detectors in
// this set read a clock and no other does: CrashLoopDetector and
// RestartLoopDetector both measure how long ago a container last exited, and
// ContainerStartErrorDetector measures how long a pod has been failing to start.
// Injecting the instant keeps the whole set a pure function of its inputs,
// which is what the determinism property in FuzzDetectors and the report
// package's golden test both depend on.
//
// ContainerStartErrorDetector is registered last because it says the least: it
// reports that a container did not start without claiming to know why. Order is
// report order and nothing else — Run applies every detector to every pod and
// collects all findings, so a catch-all cannot shadow a specific detector.
func DefaultDetectors(now time.Time) []Detector {
	return []Detector{
		CrashLoopDetector{Now: now},
		ImagePullDetector{},
		OOMKilledDetector{},
		PendingDetector{},
		VolumeAttachDetector{},
		VolumeMountDetector{},
		RestartLoopDetector{Now: now},
		InitContainerDetector{},
		ProbeFailureDetector{},
		ConfigErrorDetector{},
		ContainerStartErrorDetector{Now: now},
	}
}
