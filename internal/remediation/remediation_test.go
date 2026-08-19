package remediation

import (
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

func TestFor_TableAndCommands(t *testing.T) {
	cases := []struct {
		issue, container, wantStepSub, wantCmd string
	}{
		{"CrashLoopBackOff", "web", "inspect the crash output", "kubectl -n shop logs web-abc -c web --previous"},
		{"RestartLoop", "web", "inspect the crash output", "kubectl -n shop logs web-abc -c web --previous"},
		{"ImagePullBackOff", "", "the image can't be pulled", "kubectl -n shop describe pod web-abc"},
		{"ErrImagePull", "", "the image can't be pulled", "kubectl -n shop describe pod web-abc"},
		{"OOMKilled", "", "exceeded its memory limit", "kubectl -n shop describe pod web-abc"},
		{"Unschedulable", "", "no node can place the pod", "kubectl -n shop describe pod web-abc"},
		{"CreateContainerConfigError", "", "referenced ConfigMap or Secret is missing", "kubectl -n shop describe pod web-abc"},
		{"ProbeFailure", "", "the probe keeps failing", "kubectl -n shop describe pod web-abc"},
		{"VolumeAttachError", "", "the volume can't attach", "kubectl -n shop describe pod web-abc"},
		{"VolumeMountError", "", "a mounted ConfigMap or Secret is missing", "kubectl -n shop describe pod web-abc"},
		{"Init:CrashLoopBackOff", "wait-db", "an init container is failing", "kubectl -n shop logs web-abc -c wait-db --previous"},
		{"Init:OOMKilled", "wait-db", "an init container is failing", "kubectl -n shop logs web-abc -c wait-db --previous"},
		{"Init:ImagePullBackOff", "wait-db", "init container's image can't be pulled", "kubectl -n shop describe pod web-abc"},
		{"Init:ErrImagePull", "wait-db", "init container's image can't be pulled", "kubectl -n shop describe pod web-abc"},
		{"Init:CreateContainerConfigError", "", "referenced ConfigMap or Secret is missing", "kubectl -n shop describe pod web-abc"},
		{"FailedCreate", "", "the controller can't create pods", "kubectl -n shop get events --field-selector reason=FailedCreate"},
		{"JobFailed", "", "exhausted its retries", "kubectl -n shop logs job/web-abc"},
		// RolloutStuck names a Deployment, a StatefulSet or a DaemonSet, and
		// its Kind is empty (only the JobFailed producer sets one) — so the
		// command is one that needs no kind.
		{"RolloutStuck", "", "the rollout is wedged", "kubectl -n shop get events --field-selector involvedObject.name=web-abc"},
		{"SomethingNew", "", "inspect the object for details", "kubectl -n shop describe pod web-abc"},
	}
	for _, tc := range cases {
		f := diagnose.Finding{Issue: tc.issue, Pod: "shop/web-abc", Container: tc.container}
		got := For(f)
		if !strings.Contains(got.NextStep, tc.wantStepSub) {
			t.Errorf("%s: NextStep %q, want it to contain %q", tc.issue, got.NextStep, tc.wantStepSub)
		}
		if got.Command != tc.wantCmd {
			t.Errorf("%s: Command = %q, want %q", tc.issue, got.Command, tc.wantCmd)
		}
	}
}

// A CronJob-sourced JobFailed finding carries the CronJob's name in Pod, and
// no Job by that name exists — the failed run is named "<name>-<minute>" — so
// the job/ log command could never resolve. The suggestion addresses the
// CronJob itself instead.
func TestFor_JobFailedFromCronJob(t *testing.T) {
	f := diagnose.Finding{Issue: "JobFailed", Kind: "CronJob", Pod: "shop/nightly"}
	got := For(f)
	if want := "inspect that run and the schedule"; !strings.Contains(got.NextStep, want) {
		t.Errorf("NextStep = %q, want it to contain %q", got.NextStep, want)
	}
	if want := "kubectl -n shop describe cronjob nightly"; got.Command != want {
		t.Errorf("Command = %q, want %q", got.Command, want)
	}
}

// Every Kind other than "CronJob" — "Job" from the standalone-Job constructor,
// or empty from a producer that predates the field — keeps the Job-addressed
// log command, so nothing that worked before degrades.
func TestFor_JobFailedKeepsJobCommandForOtherKinds(t *testing.T) {
	for _, kind := range []string{"", "Job"} {
		f := diagnose.Finding{Issue: "JobFailed", Kind: kind, Pod: "shop/web-abc"}
		got := For(f)
		if want := "kubectl -n shop logs job/web-abc"; got.Command != want {
			t.Errorf("Kind %q: Command = %q, want %q", kind, got.Command, want)
		}
		if want := "exhausted its retries"; !strings.Contains(got.NextStep, want) {
			t.Errorf("Kind %q: NextStep = %q, want it to contain %q", kind, got.NextStep, want)
		}
	}
}

func TestFor_OmitsContainerWhenEmpty(t *testing.T) {
	f := diagnose.Finding{Issue: "CrashLoopBackOff", Pod: "shop/web-abc"} // no Container
	if got := For(f).Command; got != "kubectl -n shop logs web-abc --previous" {
		t.Fatalf("Command = %q, want no -c flag", got)
	}
}

func TestFor_CommandsAreNeverMutating(t *testing.T) {
	bad := []string{"delete", "apply", "edit", "patch", "scale", "rollout", "cordon", "drain", "create ", "replace"}
	issues := []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "OOMKilled", "Unschedulable",
		"CreateContainerConfigError", "ProbeFailure", "VolumeAttachError", "VolumeMountError", "Init:CrashLoopBackOff", "Init:OOMKilled",
		"Init:ImagePullBackOff", "Init:ErrImagePull", "Init:CreateContainerConfigError", "FailedCreate", "JobFailed", "RestartLoop", "RolloutStuck", "whatever-default"}
	for _, iss := range issues {
		cmd := For(diagnose.Finding{Issue: iss, Pod: "ns/pod", Container: "c"}).Command
		for _, b := range bad {
			if strings.Contains(cmd, b) {
				t.Errorf("%s: command %q contains a mutating verb %q", iss, cmd, b)
			}
		}
	}
}
