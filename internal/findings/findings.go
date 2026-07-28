// Package findings owns kubeagent's severity model for the CI gate: an ordered
// Level, the finding-kind -> level table, and the projection of a scan.Result
// into one flat, deterministically ordered list.
//
// Severity is assigned ad hoc elsewhere in the tree (internal/mcp/view.go,
// internal/watch/issues.go, internal/report, internal/gitops,
// internal/quotahealth). This package does not replace those: internal/gate is
// its only consumer for now, because migrating the others would change the MCP
// tool payloads shipped in v0.63.0 and regenerate the golden report fixture.
// The table below deliberately mirrors internal/mcp/view.go so the two agree.
//
// Pure: no cluster calls, no LLM calls.
package findings

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Level is a finding's severity, ordered so a threshold comparison is a plain
// >= and does not depend on how the names happen to sort.
type Level int

const (
	// Info is reserved: no detector emits it yet, but --fail-on info must have
	// a meaning, and adding an informational class later must not renumber the
	// levels above it.
	Info Level = iota
	Warning
	Critical
)

func (l Level) String() string {
	switch l {
	case Critical:
		return "critical"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// MarshalJSON emits the spelling, not the ordinal: the JSON is a published
// contract and must not change if a level is inserted between two others.
func (l Level) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

// Parse turns a --fail-on value into a Level.
func Parse(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return Info, nil
	case "warning":
		return Warning, nil
	case "critical":
		return Critical, nil
	}
	return Info, fmt.Errorf("unknown level %q (want critical, warning or info)", s)
}

// Finding is one problem with a severity attached. Owner names the workload the
// finding hangs off ("Deployment/api"), which is what lets the gate scope a
// post-deploy verify to one rollout: a diagnose.Finding carries only a pod name
// and no controller reference.
type Finding struct {
	Level     Level  `json:"level"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Issue     string `json:"issue"`
	Reason    string `json:"reason"`
	Owner     string `json:"owner,omitempty"`
}
