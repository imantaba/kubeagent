package gitops

import (
	"testing"
	"time"
)

func TestShortAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "0s"},
		{45 * time.Second, "45s"},
		{12 * time.Minute, "12m"},
		{5 * time.Hour, "5h"},
		{72 * time.Hour, "3d"},
	}
	for _, tt := range tests {
		if got := shortAge(tt.d); got != tt.want {
			t.Errorf("shortAge(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestDurText(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{time.Hour, "1h"},
		{90 * time.Minute, "90m"},
		{30 * time.Second, "30s"},
		{0, "0s"},
		{-time.Hour, "0s"},
	}
	for _, tt := range tests {
		if got := durText(tt.d); got != tt.want {
			t.Errorf("durText(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestAgeOf(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	if _, known := ageOf(time.Time{}, now); known {
		t.Error("a zero timestamp must not be a known age")
	}
	d, known := ageOf(now.Add(-3*time.Hour), now)
	if !known || d != 3*time.Hour {
		t.Errorf("ageOf(3h ago) = %v, %v; want 3h, true", d, known)
	}
	// Clock skew: a stamp in the future is not staleness.
	d, known = ageOf(now.Add(time.Hour), now)
	if !known || d != 0 {
		t.Errorf("ageOf(future) = %v, %v; want 0, true", d, known)
	}
}

func TestByAge(t *testing.T) {
	const thr = time.Hour
	if got := byAge(0, false, thr); got != StatePending {
		t.Errorf("unknown age = %q, want pending — a missing timestamp is never stale", got)
	}
	if got := byAge(thr, true, thr); got != StatePending {
		t.Errorf("exactly at the threshold = %q, want pending", got)
	}
	if got := byAge(thr+time.Nanosecond, true, thr); got != StateStale {
		t.Errorf("one nanosecond past the threshold = %q, want stale", got)
	}
}

func TestConditionReadsNoMessage(t *testing.T) {
	obj := map[string]any{
		"status": map[string]any{
			"conditions": []any{
				map[string]any{
					"type":               "Ready",
					"status":             "False",
					"reason":             "BuildFailed",
					"message":            "failed to clone https://tok3n@git.example/org/repo",
					"lastTransitionTime": "2026-07-27T09:00:00Z",
				},
			},
		},
	}
	status, reason, changed, found := condition(obj, "Ready")
	if !found || status != "False" || reason != "BuildFailed" {
		t.Fatalf("condition() = %q, %q, %v, %v", status, reason, changed, found)
	}
	if want := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC); !changed.Equal(want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if _, _, _, found := condition(obj, "Stalled"); found {
		t.Error("absent condition reported as found")
	}
}
