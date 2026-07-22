package confidence

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func TestForIssue(t *testing.T) {
	medium := []string{"RestartLoop", "ProbeFailure"}
	high := []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "OOMKilled",
		"Unschedulable", "VolumeAttachError", "Init:CrashLoopBackOff", "Init:ImagePullBackOff",
		"Init:OOMKilled", "FailedCreate", "JobFailed", "CreateContainerConfigError", "SomeFutureDirectDetector"}
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

func TestAnnotate_StampsEveryFinding(t *testing.T) {
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
	// idempotent
	Annotate(ws)
	if ws[0].Findings[0].Confidence != "medium" || ws[0].Findings[1].Confidence != "high" {
		t.Error("Annotate must be idempotent")
	}
}
