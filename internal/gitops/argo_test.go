package gitops

import (
	"strings"
	"testing"
	"time"
)

var argoNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// argoApp builds an Application fixture. Every field a real Application carries
// that must never be rendered is present, so any test asserting on Detail also
// proves the leak boundary holds.
func argoApp(sync, phase, finishedAt string, automated bool) map[string]any {
	spec := map[string]any{
		"source": map[string]any{
			"repoURL":        "https://tok3n@git.example/org/repo.git",
			"targetRevision": "main",
			"path":           "overlays/prod",
		},
	}
	if automated {
		spec["syncPolicy"] = map[string]any{"automated": map[string]any{"prune": true}}
	}
	status := map[string]any{
		"sync": map[string]any{
			"status":   sync,
			"revision": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
		},
	}
	if phase != "" || finishedAt != "" {
		op := map[string]any{"message": "one or more objects failed to apply: https://tok3n@git.example"}
		if phase != "" {
			op["phase"] = phase
		}
		if finishedAt != "" {
			op["finishedAt"] = finishedAt
		}
		status["operationState"] = op
	}
	return map[string]any{"spec": spec, "status": status}
}

func TestAssessArgo(t *testing.T) {
	const thr = time.Hour
	tests := []struct {
		name      string
		obj       map[string]any
		wantState State
		wantIn    []string // substrings the detail must contain
	}{
		{
			name:      "synced",
			obj:       argoApp("Synced", "Succeeded", "2026-07-27T11:00:00Z", true),
			wantState: StateSynced,
		},
		{
			name:      "out of sync with auto-sync, young",
			obj:       argoApp("OutOfSync", "Succeeded", "2026-07-27T11:56:00Z", true),
			wantState: StatePending,
			wantIn:    []string{"OutOfSync a1b2c3d", "last synced 4m ago"},
		},
		{
			name:      "out of sync with auto-sync, past the threshold",
			obj:       argoApp("OutOfSync", "Succeeded", "2026-07-21T12:00:00Z", true),
			wantState: StateStale,
			wantIn:    []string{"OutOfSync a1b2c3d", "last synced 6d ago"},
		},
		{
			name:      "out of sync without auto-sync will not self-heal",
			obj:       argoApp("OutOfSync", "Succeeded", "2026-07-21T12:00:00Z", false),
			wantState: StateBlocked,
			wantIn:    []string{"auto-sync off", "last synced 6d ago"},
		},
		{
			name:      "last sync operation failed",
			obj:       argoApp("OutOfSync", "Failed", "2026-07-27T11:59:00Z", true),
			wantState: StateBlocked,
			wantIn:    []string{"last sync failed"},
		},
		{
			name:      "operation error is also blocked",
			obj:       argoApp("OutOfSync", "Error", "2026-07-27T11:59:00Z", true),
			wantState: StateBlocked,
			wantIn:    []string{"last sync failed"},
		},
		{
			name:      "no operationState at all: differs but the age is unknowable",
			obj:       argoApp("OutOfSync", "", "", true),
			wantState: StatePending,
			wantIn:    []string{"age unknown"},
		},
		{
			name:      "a failed operation in the past does not taint a synced app",
			obj:       argoApp("Synced", "Failed", "2026-07-21T12:00:00Z", true),
			wantState: StateSynced,
		},
		{
			name:      "unreported sync status",
			obj:       argoApp("Unknown", "", "", true),
			wantState: StateUnknown,
		},
		{
			name:      "empty object",
			obj:       map[string]any{},
			wantState: StateUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assessArgo(tt.obj, argoNow, thr)
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q (detail %q)", got.State, tt.wantState, got.Detail)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("Detail = %q, want it to contain %q", got.Detail, want)
				}
			}
			for _, leak := range []string{"tok3n", "git.example", "overlays/prod", "failed to apply"} {
				if strings.Contains(got.Detail, leak) {
					t.Errorf("Detail = %q leaks %q", got.Detail, leak)
				}
			}
		})
	}
}
