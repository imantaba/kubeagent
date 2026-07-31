package cli

import (
	"context"
	"flag"

	"github.com/imantaba/kubeagent/internal/mcp"
)

// runMCP serves kubeagent's read-only diagnosis over the Model Context
// Protocol on stdin/stdout, for an AI agent to call as a tool. The protocol
// owns stdout, so nothing here may print to it; the underlying mcp.Serve
// connects to the cluster and validates it eagerly before serving.
func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current context)")
	allowSwitch := fs.Bool("allow-context-switch", false,
		"let tool calls name a different kubeconfig context, and expose list_contexts")
	logs := fs.Bool("logs", false, "enrich findings with a short log tail from failing containers")
	if err := fs.Parse(args); err != nil {
		return err
	}

	return mcp.Serve(context.Background(), mcp.Config{
		Kubeconfig:         *kubeconfig,
		Context:            *contextName,
		AllowContextSwitch: *allowSwitch,
		Logs:               *logs,
	}, version)
}
