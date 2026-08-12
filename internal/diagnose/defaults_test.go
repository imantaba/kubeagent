package diagnose

import (
	"fmt"
	"testing"
	"time"
)

func TestDefaultDetectorsOrder(t *testing.T) {
	// The order is the order findings are reported in. A reordering is a
	// user-visible output change, so it is pinned here as well as in the report
	// package's golden test.
	want := []string{
		"diagnose.CrashLoopDetector",
		"diagnose.ImagePullDetector",
		"diagnose.OOMKilledDetector",
		"diagnose.PendingDetector",
		"diagnose.VolumeAttachDetector",
		"diagnose.VolumeMountDetector",
		"diagnose.RestartLoopDetector",
		"diagnose.InitContainerDetector",
		"diagnose.ProbeFailureDetector",
		"diagnose.ConfigErrorDetector",
		"diagnose.ContainerStartErrorDetector",
	}
	got := DefaultDetectors(time.Now())
	if len(got) != len(want) {
		t.Fatalf("got %d detectors, want %d", len(got), len(want))
	}
	for i, d := range got {
		if name := fmt.Sprintf("%T", d); name != want[i] {
			t.Errorf("detector %d = %s, want %s", i, name, want[i])
		}
	}
}

func TestDefaultDetectorsInjectsTheClock(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	// All three clock-reading detectors must receive the same instant. A
	// detector left at the zero value still returns findings, so nothing else
	// in the suite would notice the omission — only this check.
	var sawRestart, sawCrash, sawStart bool
	for _, d := range DefaultDetectors(now) {
		switch v := d.(type) {
		case RestartLoopDetector:
			sawRestart = true
			if !v.Now.Equal(now) {
				t.Errorf("RestartLoopDetector.Now = %v, want %v", v.Now, now)
			}
		case CrashLoopDetector:
			sawCrash = true
			if !v.Now.Equal(now) {
				t.Errorf("CrashLoopDetector.Now = %v, want %v", v.Now, now)
			}
		case ContainerStartErrorDetector:
			sawStart = true
			if !v.Now.Equal(now) {
				t.Errorf("ContainerStartErrorDetector.Now = %v, want %v", v.Now, now)
			}
		}
	}
	if !sawRestart {
		t.Error("no RestartLoopDetector in the default set — the injected clock is unreachable")
	}
	if !sawCrash {
		t.Error("no CrashLoopDetector in the default set — the injected clock is unreachable")
	}
	if !sawStart {
		t.Error("no ContainerStartErrorDetector in the default set — the injected clock is unreachable")
	}
}
