package mcp

import (
	"context"
	"errors"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/imantaba/kubeagent/internal/cluster"
)

// ContextView is one kubeconfig context as a caller sees it.
type ContextView struct {
	Name    string `json:"name"`
	Cluster string `json:"cluster"`
	// Server is scheme://host only. An API server URL can carry a path and a
	// query, and a full one is enough to start probing.
	Server  string `json:"server"`
	Current bool   `json:"current"`
}

// ContextsInput is empty: listing contexts takes no arguments.
type ContextsInput struct{}

// ContextsOutput is the list_contexts result.
type ContextsOutput struct {
	Contexts []ContextView `json:"contexts"`
	Current  string        `json:"current"`
}

func registerContexts(s *mcpsdk.Server, cfg Config) {
	tool := &mcpsdk.Tool{
		Name: "list_contexts",
		Description: "List the kubeconfig contexts this server may switch between. Read-only: this never " +
			"changes cluster state and never reveals credentials or kubeconfig paths.",
	}
	mcpsdk.AddTool(s, tool, guard("list_contexts",
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ ContextsInput) (*mcpsdk.CallToolResult, ContextsOutput, error) {
			infos, err := cluster.Contexts(cfg.Kubeconfig)
			if err != nil {
				// err.Error() is safe here only because cluster.Contexts is
				// contractually path-free: it discards the underlying error and
				// returns a fixed message. See its doc comment before changing
				// either side — a kubeconfig path reaching this string would
				// cross the MCP boundary.
				return nil, ContextsOutput{}, errors.New("listing kubeconfig contexts: " + err.Error())
			}
			out := ContextsOutput{Contexts: []ContextView{}}
			for _, i := range infos {
				out.Contexts = append(out.Contexts, ContextView{
					Name: i.Name, Cluster: i.Cluster, Server: i.Server, Current: i.Current,
				})
				if i.Current {
					out.Current = i.Name
				}
			}
			return nil, out, nil
		}))
}
