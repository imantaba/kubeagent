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
	for _, d := range DefaultDetectors(now) {
		if rl, ok := d.(RestartLoopDetector); ok {
			if !rl.Now.Equal(now) {
				t.Errorf("RestartLoopDetector.Now = %v, want %v", rl.Now, now)
			}
			return
		}
	}
	t.Fatal("no RestartLoopDetector in the default set — the injected clock is unreachable")
}
