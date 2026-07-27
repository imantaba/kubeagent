// Package mcp serves kubeagent's read-only diagnosis over the Model Context
// Protocol, so another agent can call it as a deterministic tool.
//
// The server owns no cluster logic: every tool runs the same scan pipeline the
// CLI runs and projects the result into this package's json-tagged view types.
// It is strictly read-only toward the cluster (get/list/watch only), it never
// calls an LLM, and no code path in it can reach the --fix remediation writer.
package mcp

// Config is everything the server needs to talk to a cluster. It carries no
// credential of its own: the kubeconfig path is resolved by internal/cluster
// and is never echoed back to a caller, because a path can name a customer, a
// cluster and an environment.
type Config struct {
	// Kubeconfig is an explicit kubeconfig path, or "" for the usual resolution.
	Kubeconfig string
	// Context is the kubeconfig context to use, or "" for the current context.
	Context string
	// AllowContextSwitch registers list_contexts and lets a tool call name a
	// different context. Off by default: a server started against one cluster
	// should not be talked into another one.
	AllowContextSwitch bool
	// Logs enables the log-tail enrichment that scan --logs performs.
	Logs bool
}
