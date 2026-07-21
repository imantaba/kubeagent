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
	case "RestartLoop", "ProbeFailure":
		return "medium"
	default:
		return "high"
	}
}

// ForRootCause returns the confidence of a root-cause attribution from its cause
// type: node and PVC are evidence-backed ("high"); a shared registry is a
// statistical inference ("medium"). Empty or unrecognized input returns "".
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

// Annotate stamps Confidence on every finding of every workload — a single choke
// point covering all finding producers. Mutates in place; idempotent.
func Annotate(workloads []inventory.Workload) {
	for i := range workloads {
		for j := range workloads[i].Findings {
			workloads[i].Findings[j].Confidence = ForIssue(workloads[i].Findings[j].Issue)
		}
	}
}
