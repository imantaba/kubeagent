package diagnose

import "time"

// DefaultDetectors returns the detector set every kubeagent command runs, in the
// order findings are reported.
//
// now is a parameter rather than a time.Now() call because RestartLoopDetector
// and CrashLoopDetector both measure how long ago a container last exited, and
// they are the only detectors in this set that read a clock. Injecting the
// instant keeps the whole set a pure function of its inputs, which is what the
// determinism property in FuzzDetectors and the report package's golden test
// both depend on.
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
	}
}
