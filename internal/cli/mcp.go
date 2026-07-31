package cli

import (
	"context"
	"flag"

	"github.com/imantaba/kubeagent/internal/mcp"
)

// mcpOptions is `kubeagent mcp`'s parsed command line. One field per flag, in
// declaration order. It exists so flag wiring is testable without a cluster:
// parseMCPFlags is pure, and runMCPOpts does the I/O.
type mcpOptions struct {
	kubeconfig         string
	contextName        string
	allowContextSwitch bool
	logs               bool
}

// parseMCPFlags parses `kubeagent mcp`'s command line. Pure: it contacts no
// cluster and writes nothing.
func parseMCPFlags(args []string) (mcpOptions, error) {
	var o mcpOptions
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&o.contextName, "context", "", "kubeconfig context to use (default: current context)")
	fs.BoolVar(&o.allowContextSwitch, "allow-context-switch", false,
		"let tool calls name a different kubeconfig context, and expose list_contexts")
	fs.BoolVar(&o.logs, "logs", false, "enrich findings with a short log tail from failing containers")
	if err := fs.Parse(args); err != nil {
		return mcpOptions{}, err
	}
	return o, nil
}

// runMCPOpts serves kubeagent's read-only diagnosis over the Model Context
// Protocol on stdin/stdout, for an AI agent to call as a tool. The protocol
// owns stdout, so nothing here may print to it; the underlying mcp.Serve
// connects to the cluster and validates it eagerly before serving. o is the
// already-parsed command line, as produced by parseMCPFlags.
func runMCPOpts(o mcpOptions) error {
	return mcp.Serve(context.Background(), mcp.Config{
		Kubeconfig:         o.kubeconfig,
		Context:            o.contextName,
		AllowContextSwitch: o.allowContextSwitch,
		Logs:               o.logs,
	}, version)
}

func runMCP(args []string) error {
	o, err := parseMCPFlags(args)
	if err != nil {
		return err
	}
	return runMCPOpts(o)
}
