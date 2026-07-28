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

// clientFactory builds a clientset for a named kubeconfig context. It exists
// so tests can drive context switching without a kubeconfig on disk.
type clientFactory func(contextName string) (kubernetes.Interface, error)

// clientFor picks the clientset a call should use and the context label its
// result should report. The error it returns crosses the MCP boundary, so it
// names the requested context and nothing else — never a kubeconfig path,
// never an API server address.
func clientFor(cfg Config, base kubernetes.Interface, switchTo clientFactory, requested string) (kubernetes.Interface, string, error) {
	if requested == "" {
		return base, contextLabel(cfg.Context), nil
	}
	if !cfg.AllowContextSwitch {
		return nil, "", errContextSwitchDisabled
	}
	client, err := switchTo(requested)
	if err != nil {
		return nil, "", fmt.Errorf("connecting to context %q", requested)
	}
	return client, requested, nil
}

// newServer builds the server around an already-connected clientset. Serve is
// the production entry point; this constructor exists so tests can drive the
// whole protocol against a fake clientset and a fixed clock.
func newServer(cfg Config, version string, client kubernetes.Interface, switchTo clientFactory, now func() time.Time) *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "kubeagent", Version: version}, nil)
	registerTriage(s, cfg, client, switchTo, now)
	registerInspect(s, cfg, client, switchTo, now)
	registerAdvisory(s, cfg, client, switchTo, now)
	if cfg.AllowContextSwitch {
		registerContexts(s, cfg)
	}
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
		// redact.Error only special-cases *url.Error (see internal/redact); a
		// kubeconfig-load failure from internal/cluster's restConfig is a
		// plain fmt.Errorf wrapping the kubeconfig path and context, so it is
		// not a *url.Error and passes through unredacted, path intact. That
		// is intended on this one operator-facing path: the process exits
		// here before it ever starts serving, and this error goes to stderr,
		// not into any tool result or protocol message. See the disclosure in
		// website/docs/features/mcp.md ("The startup error on stderr is not
		// redacted") — do not mistake this for an oversight.
		return fmt.Errorf("connecting to the cluster: %s", redact.Error(err))
	}
	if _, err := client.Discovery().ServerVersion(); err != nil {
		return fmt.Errorf("reaching the API server: %s", redact.Error(err))
	}

	s := newServer(cfg, version, client, func(contextName string) (kubernetes.Interface, error) {
		return cluster.NewClient(cfg.Kubeconfig, contextName)
	}, time.Now)
	return s.Run(ctx, &mcpsdk.StdioTransport{})
}
