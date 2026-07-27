package mcp

import (
	"context"
	"fmt"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/redact"
)

// toolHandler is the shape the SDK's AddTool expects. It is a type alias
// (not a distinct defined type) for mcpsdk.ToolHandlerFor: AddTool is generic
// over ToolHandlerFor[In, Out], and a same-shaped-but-distinct defined type
// is not assignable to it without an explicit conversion at every call site.
// The alias keeps guard's return value passable to AddTool directly.
type toolHandler[In, Out any] = mcpsdk.ToolHandlerFor[In, Out]

// guard turns a panic in a handler into an error result. The SDK does not
// recover: a panicking handler unwinds through the per-request goroutine and
// takes the whole process down, leaving every other session with a dead pipe.
// A long-lived server serving a model that can send anything cannot afford
// that, so every handler is wrapped.
func guard[In, Out any](name string, h toolHandler[In, Out]) toolHandler[In, Out] {
	return func(ctx context.Context, req *mcpsdk.CallToolRequest, in In) (res *mcpsdk.CallToolResult, out Out, err error) {
		defer func() {
			if r := recover(); r != nil {
				var zero Out
				res, out = nil, zero
				// The panic value can carry anything, including a *url.Error
				// from a client library. When it is itself an error, it is
				// passed to redact.Error directly, so the unwrap chain
				// survives and redact.Error's errors.As lookup can still find
				// and redact the URL inside it; flattening it with %v first
				// (as fmt.Errorf("%v", r) would) discards that chain and lets
				// the URL's full path and query through unredacted. A
				// non-error panic value has no chain to preserve, so it is
				// only formatted.
				reason := fmt.Sprintf("%v", r)
				if e, ok := r.(error); ok {
					reason = redact.Error(e)
				}
				err = fmt.Errorf("%s failed unexpectedly: %s", name, reason)
			}
		}()
		return h(ctx, req, in)
	}
}

// contextLabel names the kubeconfig context a result came from. An empty
// configured context means "whatever the kubeconfig's current-context is";
// saying so is honest, and it avoids reading the kubeconfig just to print a
// label.
func contextLabel(name string) string {
	if name == "" {
		return "(current context)"
	}
	return name
}

// newServer builds the server around an already-connected clientset. Serve is
// the production entry point; this constructor exists so tests can drive the
// whole protocol against a fake clientset and a fixed clock.
func newServer(cfg Config, version string, client kubernetes.Interface, now func() time.Time) *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "kubeagent", Version: version}, nil)
	registerTriage(s, cfg, client, now)
	registerInspect(s, cfg, client, now)
	registerAdvisory(s, cfg, client, now)
	return s
}

// Serve connects to the cluster, validates the connection, and serves MCP over
// stdio until the client disconnects.
//
// The connection is validated eagerly. A server that starts happily and then
// fails every tool call teaches the calling model that kubeagent is unreliable;
// failing at startup puts the error where a human will read it.
func Serve(ctx context.Context, cfg Config, version string) error {
	client, err := cluster.NewClient(cfg.Kubeconfig, cfg.Context)
	if err != nil {
		return fmt.Errorf("connecting to the cluster: %s", redact.Error(err))
	}
	if _, err := client.Discovery().ServerVersion(); err != nil {
		return fmt.Errorf("reaching the API server: %s", redact.Error(err))
	}

	s := newServer(cfg, version, client, time.Now)
	return s.Run(ctx, &mcpsdk.StdioTransport{})
}
