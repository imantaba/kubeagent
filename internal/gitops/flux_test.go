package gitops

import (
	"strings"
	"testing"
	"time"
)

var fluxNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// fluxObj builds a Kustomization/HelmRelease fixture. As with the Argo fixture,
// every must-never-render field is present so the detail assertions double as a
// leak test.
func fluxObj(suspend bool, conds []map[string]any, attempted, applied string) map[string]any {
	spec := map[string]any{
		"path":      "./overlays/prod",
		"sourceRef": map[string]any{"kind": "GitRepository", "name": "tok3n-repo"},
	}
	if suspend {
		spec["suspend"] = true
	}
	status := map[string]any{}
	if len(conds) > 0 {
		items := make([]any, 0, len(conds))
		for _, c := range conds {
			items = append(items, c)
		}
		status["conditions"] = items
	}
	if attempted != "" {
		status["lastAttemptedRevision"] = attempted
	}
	if applied != "" {
		status["lastAppliedRevision"] = applied
	}
	return map[string]any{"spec": spec, "status": status}
}

func cond(condType, status, reason, at string) map[string]any {
	return map[string]any{
		"type":               condType,
		"status":             status,
		"reason":             reason,
		"message":            "failed to clone https://tok3n@git.example/org/repo",
		"lastTransitionTime": at,
	}
}

const (
	revA = "main@sha1:a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"
	revB = "main@sha1:9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"
)

func TestAssessKustomization(t *testing.T) {
	const thr = time.Hour
	ready := func(status, reason, at string) []map[string]any {
		return []map[string]any{cond("Ready", status, reason, at)}
	}
	tests := []struct {
		name      string
		obj       map[string]any
		wantState State
		wantIn    []string
	}{
		{
			name:      "synced",
			obj:       fluxObj(false, ready("True", "ReconciliationSucceeded", "2026-07-27T11:00:00Z"), revA, revA),
			wantState: StateSynced,
		},
		{
			name:      "suspended will not self-heal",
			obj:       fluxObj(true, ready("True", "ReconciliationSucceeded", "2026-07-15T12:00:00Z"), revA, revA),
			wantState: StateBlocked,
			wantIn:    []string{"suspended"},
		},
		{
			name: "stalled will not self-heal",
			obj: fluxObj(false, []map[string]any{
				cond("Ready", "False", "BuildFailed", "2026-07-27T11:59:00Z"),
				cond("Stalled", "True", "InvalidPath", "2026-07-27T11:59:00Z"),
			}, revA, revB),
			wantState: StateBlocked,
			wantIn:    []string{"stalled", "InvalidPath"},
		},
		{
			name:      "not ready, younger than the threshold",
			obj:       fluxObj(false, ready("False", "BuildFailed", "2026-07-27T11:58:00Z"), revA, revB),
			wantState: StatePending,
			wantIn:    []string{"attempted a1b2c3d", "applied 9f8e7d6", "not ready 2m", "BuildFailed"},
		},
		{
			name:      "not ready for days",
			obj:       fluxObj(false, ready("False", "BuildFailed", "2026-07-24T12:00:00Z"), revA, revB),
			wantState: StateStale,
			wantIn:    []string{"not ready 3d", "BuildFailed"},
		},
		{
			name:      "not ready with no usable timestamp is never stale",
			obj:       fluxObj(false, ready("False", "BuildFailed", ""), revA, revB),
			wantState: StatePending,
			wantIn:    []string{"age unknown"},
		},
		{
			name:      "ready but the newest revision has not landed",
			obj:       fluxObj(false, ready("True", "ReconciliationSucceeded", "2026-07-24T12:00:00Z"), revA, revB),
			wantState: StateStale,
			wantIn:    []string{"attempted a1b2c3d", "applied 9f8e7d6", "unchanged 3d"},
		},
		{
			name:      "nothing has ever landed",
			obj:       fluxObj(false, ready("True", "ReconciliationSucceeded", "2026-07-27T11:55:00Z"), revA, ""),
			wantState: StatePending,
			wantIn:    []string{"attempted a1b2c3d", "applied none"},
		},
		{
			name:      "no conditions at all",
			obj:       fluxObj(false, nil, "", ""),
			wantState: StateUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assessKustomization(tt.obj, fluxNow, thr)
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q (detail %q)", got.State, tt.wantState, got.Detail)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("Detail = %q, want it to contain %q", got.Detail, want)
				}
			}
			for _, leak := range []string{"tok3n", "git.example", "overlays/prod", "failed to clone", "sha1"} {
				if strings.Contains(got.Detail, leak) {
					t.Errorf("Detail = %q leaks %q", got.Detail, leak)
				}
			}
		})
	}
}

func TestAssessHelmReleaseSkipsRevisionComparison(t *testing.T) {
	const thr = time.Hour
	// HelmRelease v2 has no status.lastAppliedRevision. An object carrying only
	// lastAttemptedRevision must still read as synced when Ready is True —
	// inventing a mismatch signal would flag every healthy release.
	obj := fluxObj(false, []map[string]any{
		cond("Ready", "True", "InstallSucceeded", "2026-07-24T12:00:00Z"),
	}, revA, "")
	if got := assessHelmRelease(obj, fluxNow, thr); got.State != StateSynced {
		t.Errorf("State = %q, want synced (detail %q)", got.State, got.Detail)
	}
	// The same object read as a Kustomization does report the mismatch.
	if got := assessKustomization(obj, fluxNow, thr); got.State != StateStale {
		t.Errorf("Kustomization State = %q, want stale", got.State)
	}
}

func TestAssessHelmReleaseConditions(t *testing.T) {
	const thr = time.Hour
	tests := []struct {
		name      string
		obj       map[string]any
		wantState State
	}{
		{"suspended", fluxObj(true, []map[string]any{cond("Ready", "True", "InstallSucceeded", "2026-07-27T11:00:00Z")}, "", ""), StateBlocked},
		{"stalled", fluxObj(false, []map[string]any{cond("Stalled", "True", "RetriesExceeded", "2026-07-27T11:00:00Z")}, "", ""), StateBlocked},
		{"not ready for days", fluxObj(false, []map[string]any{cond("Ready", "False", "UpgradeFailed", "2026-07-24T12:00:00Z")}, "", ""), StateStale},
		{"not ready, minutes old", fluxObj(false, []map[string]any{cond("Ready", "False", "UpgradeFailed", "2026-07-27T11:58:00Z")}, "", ""), StatePending},
		{"ready", fluxObj(false, []map[string]any{cond("Ready", "True", "UpgradeSucceeded", "2026-07-27T11:00:00Z")}, "", ""), StateSynced},
		{"no conditions", fluxObj(false, nil, "", ""), StateUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := assessHelmRelease(tt.obj, fluxNow, thr); got.State != tt.wantState {
				t.Errorf("State = %q, want %q (detail %q)", got.State, tt.wantState, got.Detail)
			}
		})
	}
}
