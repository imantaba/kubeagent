package confidence

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func TestForIssue(t *testing.T) {
	medium := []string{"RestartLoop", "ProbeFailure", "ContainerStartError"}
	high := []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "OOMKilled",
		"Unschedulable", "VolumeAttachError", "Init:CrashLoopBackOff", "Init:ImagePullBackOff",
		"Init:OOMKilled", "FailedCreate", "JobFailed", "CreateContainerConfigError", "RolloutStuck", "SomeFutureDirectDetector"}
	for _, iss := range medium {
		if got := ForIssue(iss); got != "medium" {
			t.Errorf("ForIssue(%q) = %q, want medium", iss, got)
		}
	}
	for _, iss := range high {
		if got := ForIssue(iss); got != "high" {
			t.Errorf("ForIssue(%q) = %q, want high (default)", iss, got)
		}
	}
}

func TestForRootCause(t *testing.T) {
	cases := map[string]string{
		"node worker-2 (NotReady)":                       "high",
		"PVC reports-data (ProvisioningFailed)":          "high",
		"registry ghcr.io (2 workloads failing to pull)": "medium",
		"":               "",
		"something else": "",
	}
	for rc, want := range cases {
		if got := ForRootCause(rc); got != want {
			t.Errorf("ForRootCause(%q) = %q, want %q", rc, got, want)
		}
	}
}

func TestAnnotate_FillsEveryEmptyFinding(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "cache", Findings: []diagnose.Finding{
		{Issue: "RestartLoop"}, {Issue: "CrashLoopBackOff"},
	}}}
	Annotate(ws)
	if ws[0].Findings[0].Confidence != "medium" {
		t.Errorf("RestartLoop confidence = %q, want medium", ws[0].Findings[0].Confidence)
	}
	if ws[0].Findings[1].Confidence != "high" {
		t.Errorf("CrashLoopBackOff confidence = %q, want high", ws[0].Findings[1].Confidence)
	}
	// idempotent: a second call fills nothing, because the first call already
	// left no empty field behind.
	Annotate(ws)
	if ws[0].Findings[0].Confidence != "medium" || ws[0].Findings[1].Confidence != "high" {
		t.Error("Annotate must be idempotent")
	}
}

// TestAnnotate_PresetConfidenceSurvives is R189's regression fixture: a
// producer (e.g. internal/rollouthealth, on a RolloutStuck finding) that has
// already set Confidence knows better than the issue string, and Annotate
// must leave it alone rather than overwrite it with ForIssue's answer.
func TestAnnotate_PresetConfidenceSurvives(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "api", Findings: []diagnose.Finding{
		{Issue: "RolloutStuck", Confidence: "medium"}, // preset by a producer
		{Issue: "OOMKilled"},                          // left empty: still filled from ForIssue
	}}}
	Annotate(ws)
	if got := ws[0].Findings[0].Confidence; got != "medium" {
		t.Errorf("preset Confidence = %q, want medium (unchanged)", got)
	}
	if got := ws[0].Findings[1].Confidence; got != "high" {
		t.Errorf("empty Confidence = %q, want high (filled from ForIssue)", got)
	}
}

func TestAnnotate_StoresRootCauseConfidence(t *testing.T) {
	ws := []inventory.Workload{
		{Namespace: "shop", Name: "api", RootCause: "node worker-2 (NotReady)"},
		{Namespace: "shop", Name: "web", RootCause: "registry ghcr.io (2 workloads failing to pull)"},
		{Namespace: "shop", Name: "db", RootCause: "PVC data-0 (ProvisioningFailed)"},
		{Namespace: "shop", Name: "cache"}, // no attribution
	}
	Annotate(ws)
	for i, want := range []string{"high", "medium", "high", ""} {
		if got := ws[i].RootCauseConfidence; got != want {
			t.Errorf("ws[%d].RootCauseConfidence = %q, want %q", i, got, want)
		}
	}
	// Idempotent: a second call writes the same values.
	Annotate(ws)
	if ws[0].RootCauseConfidence != "high" || ws[3].RootCauseConfidence != "" {
		t.Error("Annotate must be idempotent for RootCauseConfidence")
	}
}
