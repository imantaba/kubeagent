package cli

import (
	"context"

	"github.com/spf13/cobra"

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

// bindMCPFlags declares mcp's flags on cmd, writing into o. Flag names,
// defaults and usage strings are unchanged from the standard-library
// FlagSet this replaces.
func bindMCPFlags(cmd *cobra.Command, o *mcpOptions) {
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	f.StringVar(&o.contextName, "context", "", "kubeconfig context to use (default: current context)")
	f.BoolVar(&o.allowContextSwitch, "allow-context-switch", false,
		"let tool calls name a different kubeconfig context, and expose list_contexts")
	f.BoolVar(&o.logs, "logs", false, "enrich findings with a short log tail from failing containers")
}

// parseMCPFlags parses mcp's command line without running it. It builds a
// throwaway command so the flag declarations have exactly one home.
func parseMCPFlags(args []string) (mcpOptions, error) {
	var o mcpOptions
	cmd := &cobra.Command{Use: "mcp", SilenceErrors: true, SilenceUsage: true}
	bindMCPFlags(cmd, &o)
	if err := cmd.Flags().Parse(Normalize(args, longFlagLookup(cmd))); err != nil {
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

// newMCPCommand builds `kubeagent mcp`.
func newMCPCommand() *cobra.Command {
	var o mcpOptions
	cmd := &cobra.Command{
		Use:           "mcp",
		Short:         "Serve read-only diagnosis over the Model Context Protocol",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMCPOpts(o)
		},
	}
	bindMCPFlags(cmd, &o)
	return cmd
}
