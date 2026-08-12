package diagnose

import (
	"fmt"
	"time"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// containerStartDwell is how long a brand-new pod may sit in one of the start
// reasons below before it is reported. Container creation is not instant, and
// a scan run two seconds after a deploy must not describe an ordinary startup
// as a failure. A container that has already restarted skips the wait — the
// kubelet has tried more than once, so the wait would prove nothing.
const containerStartDwell = 60 * time.Second

// containerStartReasons is the closed set of kubelet waiting reasons this
// detector answers for: the container was created but could not be started.
//
// It is closed on purpose. The four image-family reasons — InvalidImageName,
// ErrImageNeverPull, RegistryUnavailable, SignatureValidationFailed — are
// deliberately absent: those are pull failures and belong beside
// ImagePullDetector, not here. That is a known gap recorded on purpose, not an
// oversight.
var containerStartReasons = map[string]bool{
	"RunContainerError":    true,
	"CreateContainerError": true,
	"PreStartHookError":    true,
	"PostStartHookError":   true,
	"StartError":           true,
}

// ContainerStartErrorDetector flags a main container the kubelet created but
// could not start, for a reason no other detector owns.
//
// It exists because the alternative was silence. A container OOM-killed during
// startup, for instance, reports waiting.Reason=RunContainerError with the OOM
// recorded only in lastState.terminated.Reason=StartError — so OOMKilledDetector,
// which matches on the reason OOMKilled, sees nothing, and the workload renders
// as failing with no stated cause underneath it.
//
// This detector says the least of any in the set, and is registered last for
// that reason. It reports that the container did not start and quotes the
// kubelet verbatim; it does not claim to know why. That is what makes it
// medium confidence rather than high.
//
// Main containers only. The same failure on an init container is a different
// diagnosis — nothing in the pod has run yet — and InitContainerDetector
// reports it as Init:<reason> with the failing container's position.
type ContainerStartErrorDetector struct{ Now time.Time }

func (d ContainerStartErrorDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		w := cs.State.Waiting
		// Matched on the raw reason: sanitizing first would let a control
		// character spliced mid-word evade the set.
		if w == nil || !containerStartReasons[w.Reason] {
			continue
		}
		if cs.RestartCount == 0 && d.Now.Sub(facts.Pod.CreationTimestamp.Time) < containerStartDwell {
			continue // too young to call — see containerStartDwell
		}
		return &Finding{
			Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
			Issue:     "ContainerStartError",
			Reason:    "the container image was resolved but the container could not be started",
			Evidence:  fmt.Sprintf("container %q: %s: %s", cs.Name, safetext.Line(w.Reason), safetext.Line(w.Message)),
			Container: cs.Name,
		}
	}
	return nil
}
