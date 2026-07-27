package mcp

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// connect wires a server built over client to an in-process MCP client.
func connect(t *testing.T, cfg Config, client kubernetes.Interface) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()

	srv := newServer(cfg, "test", client, func() time.Time { return fixedNow })
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()

	go func() {
		if err := srv.Run(ctx, serverTransport); err != nil {
			t.Logf("server stopped: %v", err)
		}
	}()

	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestServer_ExposesExactlyTheReadOnlyTools(t *testing.T) {
	cs := connect(t, Config{}, fake.NewSimpleClientset())

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}

	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	want := []string{"kubeagent_advisory", "kubeagent_inspect", "kubeagent_triage"}
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tools = %v, want %v (the SDK lists tools alphabetically)", got, want)
		}
	}
}

func TestServer_NoToolNameSuggestsAWriteVerb(t *testing.T) {
	cs := connect(t, Config{}, fake.NewSimpleClientset())

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}

	banned := []string{"fix", "apply", "delete", "patch", "create", "update", "restart", "scale", "remediate", "rollback"}
	for _, tool := range res.Tools {
		for _, verb := range banned {
			if strings.Contains(strings.ToLower(tool.Name), verb) {
				t.Errorf("tool %q contains the write verb %q; the MCP server is read-only and must not "+
					"advertise a mutating capability", tool.Name, verb)
			}
		}
	}
}

func TestServer_UnknownToolIsAProtocolError(t *testing.T) {
	cs := connect(t, Config{}, fake.NewSimpleClientset())

	_, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "kubeagent_fix"})
	if err == nil {
		t.Fatal("CallTool(kubeagent_fix) error = nil, want an unknown-tool error")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("error = %q, want it to name the unknown tool", err)
	}
}

// TestGuard_PanicWithURLErrorRedactsPathAndQuery drives guard directly with a
// handler that panics with a *url.Error, the same shape net/http returns when
// a client library's request fails. redact.Error's own contract keeps
// scheme://host and drops everything else — guard's recover path must not
// bypass that by flattening the panic value with %v before redacting, which
// discards the error chain errors.As needs to find the *url.Error at all.
func TestGuard_PanicWithURLErrorRedactsPathAndQuery(t *testing.T) {
	h := guard("kubeagent_test",
		func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
			panic(&url.Error{
				Op:  "Post",
				URL: "https://alerts.example.com/hooks/FAKE-TOKEN?token=FAKE-SECRET",
				Err: errors.New("connection refused"),
			})
		})

	res, out, err := h(context.Background(), nil, struct{}{})
	if err == nil {
		t.Fatal("err = nil, want the panic surfaced as an error")
	}
	if res != nil {
		t.Errorf("res = %+v, want nil on a recovered panic", res)
	}
	if out != (struct{}{}) {
		t.Errorf("out = %+v, want the zero value on a recovered panic", out)
	}

	got := err.Error()
	if !strings.Contains(got, "https://alerts.example.com") {
		t.Errorf("error = %q, want it to keep scheme://host %q", got, "https://alerts.example.com")
	}
	for _, leak := range []string{"hooks", "FAKE-TOKEN", "FAKE-SECRET", "token="} {
		if strings.Contains(got, leak) {
			t.Errorf("error = %q, leaks %q from the panicking *url.Error's path/query; "+
				"guard must redact a panic value the same way it redacts any other error", got, leak)
		}
	}
}

// TestGuard_PanicWithPlainValueProducesWrappedMessage covers the fallback
// path: a panic value that is not an error (the common case — a string or a
// runtime error from a nil map write, an index out of range, and so on) has
// no unwrap chain to preserve, so it is only formatted. This must keep
// working after the URL-error fix, and must never itself panic — a panic
// escaping guard's own recover kills the process.
func TestGuard_PanicWithPlainValueProducesWrappedMessage(t *testing.T) {
	h := guard("kubeagent_test",
		func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, struct{}, error) {
			panic("boom")
		})

	_, _, err := h(context.Background(), nil, struct{}{})
	if err == nil {
		t.Fatal("err = nil, want the panic surfaced as an error")
	}
	want := "kubeagent_test failed unexpectedly: boom"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
