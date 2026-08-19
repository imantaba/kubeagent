// Package confidence classifies findings and root-cause attributions by how
// directly the observed signal implies the diagnosis: "high" for a state
// Kubernetes itself asserts, "medium" for a kubeagent heuristic or inference.
// Pure and deterministic; informational only (never affects priority or the
// cluster verdict).
package confidence

import (
	"strings"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// ForIssue returns the confidence level of a finding by its Issue string.
// kubeagent heuristics are "medium"; every other (direct-read) issue is "high",
// so a new direct-state detector needs no change here.
func ForIssue(issue string) string {
	switch issue {
	// ContainerStartError is medium for a reason the other two do not share:
	// the start failure itself is a direct read, but the finding does not say
	// why the container did not start, and the kubelet reason it quotes covers
	// several unrelated causes.
	case "RestartLoop", "ProbeFailure", "ContainerStartError":
		return "medium"
	default:
		return "high"
	}
}

// ForRootCause returns the confidence of a root-cause attribution from its cause
// type: node and PVC are evidence-backed ("high"); a shared registry is a
// statistical inference ("medium"). Empty or unrecognized input returns "" —
// and at internal/report's only call site that renders unmarked, the same as
// "high", because a tag is added only when the level is non-empty and not
// "high".
func ForRootCause(rootCause string) string {
	switch {
	case strings.HasPrefix(rootCause, "node "):
		return "high"
	case strings.HasPrefix(rootCause, "PVC "):
		return "high"
	case strings.HasPrefix(rootCause, "registry "):
		return "medium"
	default:
		return ""
	}
}

// Annotate fills Confidence on every finding of every workload that does not
// already carry one — a single choke point covering every finding producer
// that leaves the field empty. A producer that knows better than the issue
// string wins: internal/rollouthealth sets Confidence itself on its
// RolloutStuck findings (one level per arm, since the same issue string
// covers a controller-set condition on a Deployment and an inferred, wedged
// counter on a StatefulSet or DaemonSet), and Annotate leaves those alone.
// Mutates in place. Idempotent for a different reason than a stamp would be:
// a second call fills nothing, because the first call already left no empty
// field behind.
//
// This only works because every producer that sets its own Confidence runs
// before Annotate — internal/scan/scan.go calls rollouthealth.Annotate ahead
// of confidence.Annotate. Reordering that would silently fall back to
// ForIssue's answer for those findings instead of failing loudly.
//
// Annotate also stores the attribution confidence on the workload row
// (RootCauseConfidence), the same word internal/report renders as a tag, so
// the JSON document carries it too; an empty or unrecognized RootCause
// stores nothing.
func Annotate(workloads []inventory.Workload) {
	for i := range workloads {
		if c := ForRootCause(workloads[i].RootCause); c != "" {
			workloads[i].RootCauseConfidence = c
		}
		for j := range workloads[i].Findings {
			if workloads[i].Findings[j].Confidence == "" {
				workloads[i].Findings[j].Confidence = ForIssue(workloads[i].Findings[j].Issue)
			}
		}
	}
}
