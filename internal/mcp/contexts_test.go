package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes/fake"
)

func toolNames(t *testing.T, cs *mcpsdk.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	var out []string
	for _, tool := range res.Tools {
		out = append(out, tool.Name)
	}
	return out
}

func TestListContexts_IsNotRegisteredByDefault(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	for _, name := range toolNames(t, cs) {
		if name == "list_contexts" {
			t.Fatal("list_contexts is registered on a server started without --allow-context-switch; " +
				"a caller must not even learn which other clusters exist")
		}
	}
}

func TestListContexts_IsRegisteredWhenSwitchingIsAllowed(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example", AllowContextSwitch: true}, fake.NewSimpleClientset())

	found := false
	for _, name := range toolNames(t, cs) {
		if name == "list_contexts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tools = %v, want list_contexts among them", toolNames(t, cs))
	}
}

func TestTriage_ContextArgumentIsAcceptedWhenSwitchingIsAllowed(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example", AllowContextSwitch: true}, fake.NewSimpleClientset())

	out := callTriage(t, cs, map[string]any{"context": "kind-other"})

	if out.Coverage.Context != "kind-other" {
		t.Errorf("coverage.context = %q, want %q — the result must name the cluster it actually read",
			out.Coverage.Context, "kind-other")
	}
}
