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
		{"Init:CrashLoopBackOff", "wait-db", "an init container is failing", "kubectl -n shop logs web-abc -c wait-db --previous"},
		{"Init:OOMKilled", "wait-db", "an init container is failing", "kubectl -n shop logs web-abc -c wait-db --previous"},
		{"Init:ImagePullBackOff", "wait-db", "init container's image can't be pulled", "kubectl -n shop describe pod web-abc"},
		{"FailedCreate", "", "the controller can't create pods", "kubectl -n shop get events --field-selector reason=FailedCreate"},
		{"JobFailed", "", "exhausted its retries", "kubectl -n shop logs web-abc --previous"},
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

func TestFor_OmitsContainerWhenEmpty(t *testing.T) {
	f := diagnose.Finding{Issue: "CrashLoopBackOff", Pod: "shop/web-abc"} // no Container
	if got := For(f).Command; got != "kubectl -n shop logs web-abc --previous" {
		t.Fatalf("Command = %q, want no -c flag", got)
	}
}

func TestFor_CommandsAreNeverMutating(t *testing.T) {
	bad := []string{"delete", "apply", "edit", "patch", "scale", "rollout", "cordon", "drain", "create ", "replace"}
	issues := []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "OOMKilled", "Unschedulable",
		"CreateContainerConfigError", "ProbeFailure", "VolumeAttachError", "Init:CrashLoopBackOff", "Init:OOMKilled",
		"Init:ImagePullBackOff", "FailedCreate", "JobFailed", "RestartLoop", "whatever-default"}
	for _, iss := range issues {
		cmd := For(diagnose.Finding{Issue: iss, Pod: "ns/pod", Container: "c"}).Command
		for _, b := range bad {
			if strings.Contains(cmd, b) {
				t.Errorf("%s: command %q contains a mutating verb %q", iss, cmd, b)
			}
		}
	}
}
