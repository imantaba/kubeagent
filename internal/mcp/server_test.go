package mcp

import (
	"context"
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
	want := []string{"kubeagent_triage"}
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
