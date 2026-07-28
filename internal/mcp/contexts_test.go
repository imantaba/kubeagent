package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes/fake"
)

// contextsKubeconfigFixture mirrors the fixture internal/cluster/contexts_test.go
// uses: two contexts, one of whose cluster server URL carries both a path and
// a query token, so a test can assert the tool result keeps only scheme://host.
const contextsKubeconfigFixture = `apiVersion: v1
kind: Config
current-context: staging
clusters:
  - name: staging-cluster
    cluster:
      server: https://staging.example.com:6443/some/path?token=<PLACEHOLDER>
  - name: prod-cluster
    cluster:
      server: https://prod.example.com:6443
contexts:
  - name: staging
    context:
      cluster: staging-cluster
      user: staging-user
  - name: prod
    context:
      cluster: prod-cluster
      user: prod-user
users:
  - name: staging-user
    user: {}
  - name: prod-user
    user: {}
`

// writeKubeconfig writes contents to a fresh temp file and returns its path.
// Nothing here is checked in: t.TempDir() is removed after the test.
func writeKubeconfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

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

// callListContexts invokes list_contexts over the in-process transport and
// returns the raw result alongside its marshalled StructuredContent bytes, so
// callers can assert on the JSON a caller actually receives rather than on a
// Go value — this project has twice shipped a bug where a json tag sat on a
// struct that was not the one actually marshalled.
func callListContexts(t *testing.T, cs *mcpsdk.ClientSession) (*mcpsdk.CallToolResult, []byte) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "list_contexts"})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		return res, nil
	}
	blob, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("Marshal(structured) error = %v", err)
	}
	return res, blob
}

// TestListContexts_ServerURLIsReducedAndListIsSortedWithCurrentMarked drives
// list_contexts against a real kubeconfig on disk (AllowContextSwitch: true
// is what registers the tool at all) and asserts, against the marshalled
// JSON, that: the API server URL a caller sees is scheme://host only, never
// the path or query token the fixture cluster's server carries; the contexts
// come back sorted by name; and current names the kubeconfig's
// current-context. cluster.Contexts already applies redact.URL — this is the
// end-to-end proof that the reduction survives all the way through the tool
// boundary a model actually reads.
func TestListContexts_ServerURLIsReducedAndListIsSortedWithCurrentMarked(t *testing.T) {
	path := writeKubeconfig(t, contextsKubeconfigFixture)
	cs := connect(t, Config{Kubeconfig: path, AllowContextSwitch: true}, fake.NewSimpleClientset())

	res, blob := callListContexts(t, cs)
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	body := string(blob)

	if !strings.Contains(body, "https://staging.example.com:6443") {
		t.Errorf("marshalled result = %s, want the bare scheme://host %q", body, "https://staging.example.com:6443")
	}
	for _, leak := range []string{"/some/path", "token=", "PLACEHOLDER"} {
		if strings.Contains(body, leak) {
			t.Errorf("marshalled result = %s, leaks %q from the API server URL's path/query", body, leak)
		}
	}

	var out ContextsOutput
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(out.Contexts) != 2 || out.Contexts[0].Name != "prod" || out.Contexts[1].Name != "staging" {
		t.Fatalf("Contexts = %+v, want exactly [prod, staging] sorted by name", out.Contexts)
	}
	if out.Current != "staging" {
		t.Errorf("Current = %q, want %q — the kubeconfig's current-context", out.Current, "staging")
	}
}

// TestListContexts_EmptyContextsMarshalAsEmptyArrayNotNull covers a valid
// kubeconfig that defines no contexts at all: a caller reading JSON treats an
// absent/null key as zero, so "contexts": null would be indistinguishable
// from "the tool did not run" rather than "the kubeconfig has none".
func TestListContexts_EmptyContextsMarshalAsEmptyArrayNotNull(t *testing.T) {
	path := writeKubeconfig(t, "apiVersion: v1\nkind: Config\n")
	cs := connect(t, Config{Kubeconfig: path, AllowContextSwitch: true}, fake.NewSimpleClientset())

	res, blob := callListContexts(t, cs)
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	if !strings.Contains(string(blob), `"contexts":[]`) {
		t.Errorf("marshalled result = %s, want the literal `\"contexts\":[]`, not null, for a kubeconfig "+
			"defining no contexts", blob)
	}
}

// TestListContexts_MissingKubeconfigErrorDoesNotLeakThePath points the server
// at a kubeconfig path that does not exist and asserts the error a caller
// receives is path-free. A kubeconfig path is a credential in this project —
// it names a customer, a cluster and an environment (see cluster.Contexts'
// own doc comment) — and must never reach an MCP caller, whether it entered
// the message through cluster.Contexts or through contexts.go building the
// message with err.Error() around it.
func TestListContexts_MissingKubeconfigErrorDoesNotLeakThePath(t *testing.T) {
	const sentinelPath = "/nonexistent/kubeagent-test/secret-cluster.kubeconfig"
	cs := connect(t, Config{Kubeconfig: sentinelPath, AllowContextSwitch: true}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "list_contexts"})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded against a nonexistent kubeconfig; want an error result")
	}

	text := firstText(res)
	if strings.Contains(text, sentinelPath) {
		t.Errorf("error text = %q, leaks the kubeconfig path sentinel %q", text, sentinelPath)
	}
}
