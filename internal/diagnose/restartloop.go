package diagnose

import (
	"fmt"
	"time"

	"github.com/imantaba/kubeagent/internal/safetext"
)

const (
	// RestartThreshold is how many restarts make a container "restarting
	// repeatedly" rather than one that hit a bad moment. It is exported because
	// internal/rollouthealth asks the same question of a workload's pod rows —
	// reading the number from here is what keeps the two from drifting apart.
	RestartThreshold = 3
	restartRecency   = 10 * time.Minute
)

// RestartLoopDetector flags a currently-Running container that keeps exiting with
// a non-OOM error and restarting — a flapping pod the point-in-time detectors
// (CrashLoopBackOff fires only while Waiting; OOMKilled only for memory kills)
// miss. It reads the durable RestartCount + LastTerminationState, so it fires on
// every reconcile in a Running window rather than only during a crash instant.
type RestartLoopDetector struct{ Now time.Time }

func (d RestartLoopDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		run := cs.State.Running
		if run == nil {
			continue // not currently Running — the Waiting/CrashLoopBackOff cases are covered elsewhere
		}
		if d.Now.Sub(run.StartedAt.Time) > restartRecency {
			continue // recovered: has run stably past the window
		}
		if int(cs.RestartCount) < RestartThreshold {
			continue
		}
		term := cs.LastTerminationState.Terminated
		if term == nil || term.ExitCode == 0 || term.Reason == "OOMKilled" {
			continue // no prior error termination, a graceful exit, or OOM (OOMKilledDetector covers it)
		}
		age := d.Now.Sub(term.FinishedAt.Time).Truncate(time.Second)
		return &Finding{
			Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
			Issue:     "RestartLoop",
			Reason:    "Container keeps exiting with an error and restarting",
			Evidence:  fmt.Sprintf("container %q, %d restarts, last exit %d (%s), %s ago", cs.Name, cs.RestartCount, term.ExitCode, safetext.Line(term.Reason), age),
			Container: cs.Name,
		}
	}
	return nil
}
