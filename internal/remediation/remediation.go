// Package remediation maps a diagnosed finding to a deterministic, reviewed next
// step — a concise cause direction and a read-only kubectl command to investigate.
// Never LLM-decided; the command is printed for the operator, never run.
package remediation

import (
	"fmt"
	"strings"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

// Suggestion is a deterministic next step for a finding.
type Suggestion struct {
	NextStep string // concise cause direction / what to do
	Command  string // a read-only kubectl command to investigate ("" when N/A)
}

// For returns the suggestion for a finding, keyed on its Issue. An unrecognized
// Issue gets a safe generic describe suggestion.
func For(f diagnose.Finding) Suggestion {
	ns, pod := splitPod(f.Pod)
	switch f.Issue {
	case "CrashLoopBackOff", "RestartLoop":
		return Suggestion{"starts then crashes — inspect the crash output", logsCmd(ns, pod, f.Container)}
	case "ImagePullBackOff", "ErrImagePull":
		return Suggestion{"the image can't be pulled — verify the tag exists and the registry credentials", describeCmd(ns, pod)}
	case "OOMKilled":
		return Suggestion{"the container exceeded its memory limit — raise the limit or fix the leak", describeCmd(ns, pod)}
	case "Unschedulable":
		return Suggestion{"no node can place the pod — check resource requests, taints, and affinity", describeCmd(ns, pod)}
	case "CreateContainerConfigError":
		return Suggestion{"a referenced ConfigMap or Secret is missing — create it or fix the reference", describeCmd(ns, pod)}
	case "ProbeFailure":
		return Suggestion{"the probe keeps failing — check the probe config and the app's health endpoint", describeCmd(ns, pod)}
	case "VolumeAttachError":
		return Suggestion{"the volume can't attach — check the PVC/PV binding and the CSI driver", describeCmd(ns, pod)}
	case "Init:CrashLoopBackOff", "Init:ImagePullBackOff", "Init:OOMKilled":
		return Suggestion{"an init container is failing — the pod cannot start until it succeeds", logsCmd(ns, pod, f.Container)}
	case "FailedCreate":
		return Suggestion{"the controller can't create pods — check for quota, LimitRange, or a rejecting admission webhook", eventsCmd(ns, "FailedCreate")}
	case "JobFailed":
		return Suggestion{"the Job exhausted its retries — inspect the failed pod's logs", logsCmd(ns, pod, "")}
	default:
		return Suggestion{"inspect the object for details", describeCmd(ns, pod)}
	}
}

func splitPod(p string) (ns, name string) {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

func logsCmd(ns, pod, container string) string {
	c := ""
	if container != "" {
		c = " -c " + container
	}
	return fmt.Sprintf("kubectl -n %s logs %s%s --previous", ns, pod, c)
}

func describeCmd(ns, pod string) string {
	return fmt.Sprintf("kubectl -n %s describe pod %s", ns, pod)
}

func eventsCmd(ns, reason string) string {
	return fmt.Sprintf("kubectl -n %s get events --field-selector reason=%s", ns, reason)
}
