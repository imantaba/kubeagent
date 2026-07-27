package mcp

import (
	"time"

	"github.com/imantaba/kubeagent/internal/scan"
)

// SkippedCheck names a check that did not run, and why.
type SkippedCheck struct {
	Check string `json:"check"`
	Why   string `json:"why"`
}

// PartialRead names a resource kubeagent tried and failed to list.
type PartialRead struct {
	Resource string `json:"resource"`
	Why      string `json:"why"`
}

// Coverage is the honesty contract every tool result carries. A model reading
// JSON treats an absent key as zero, so "no findings" and "never looked" must
// not produce the same payload: ChecksRun says what was examined,
// ChecksSkipped says what was not and why, and Partial says which lists came
// back incomplete.
type Coverage struct {
	Context        string         `json:"context"`
	NamespaceScope string         `json:"namespaceScope"`
	CollectedAt    string         `json:"collectedAt"`
	ChecksRun      []string       `json:"checksRun"`
	ChecksSkipped  []SkippedCheck `json:"checksSkipped"`
	Partial        []PartialRead  `json:"partial"`
	// MetricsServer is "available", "absent" or "not-checked". A tool that
	// never queries metrics reports "not-checked" rather than "absent", which
	// would assert a fact nothing tested.
	MetricsServer string `json:"metricsServer"`
}

// newCoverage starts a coverage block with non-nil slices, so an empty list
// marshals as [] rather than null.
func newCoverage(contextName, namespace string, now time.Time) *Coverage {
	scope := namespace
	if scope == "" {
		scope = "all namespaces"
	}
	return &Coverage{
		Context:        contextName,
		NamespaceScope: scope,
		CollectedAt:    now.UTC().Format(time.RFC3339),
		ChecksRun:      []string{},
		ChecksSkipped:  []SkippedCheck{},
		Partial:        []PartialRead{},
		MetricsServer:  "not-checked",
	}
}

func (c *Coverage) markRun(checks ...string) {
	c.ChecksRun = append(c.ChecksRun, checks...)
}

func (c *Coverage) markSkipped(check, why string) {
	c.ChecksSkipped = append(c.ChecksSkipped, SkippedCheck{Check: check, Why: why})
}

// markPartial copies the scan's failed reads into the coverage block.
func (c *Coverage) markPartial(reads []scan.ReadFailure) {
	for _, r := range reads {
		c.Partial = append(c.Partial, PartialRead{Resource: r.Resource, Why: r.Reason})
	}
}
