package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// connect wires a server built over client to an in-process MCP client. Every
// context switch resolves back to the same clientset, and a switch can never
// fail: callers that need to prove a switch actually changed which cluster is
// read, or that a factory failure is handled safely, must use connectWith
// instead.
func connect(t *testing.T, cfg Config, client kubernetes.Interface) *mcpsdk.ClientSession {
	t.Helper()
	return connectWith(t, cfg, client, func(string) (kubernetes.Interface, error) { return client, nil })
}

// connectWith is connect with the clientFactory exposed, so a test can supply
// a factory that returns a distinct clientset per context name or fails with
// a chosen error. It exists so context-switching and factory-failure tests
// don't have to hand-roll the transport wiring connect already does.
func connectWith(t *testing.T, cfg Config, client kubernetes.Interface, switchTo clientFactory) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()

	srv := newServer(cfg, "test", client, switchTo, func() time.Time { return fixedNow })
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

// TestClientFor_SwitchingContextsActuallyReadsADifferentCluster proves a
// context switch consults a different clientset, not merely a different
// label. connect's hardcoded factory returns the same clientset for every
// context name, so no test built on it can catch the worst failure mode this
// tool has: a clientFor that returned base while labelling the result with
// the requested context. Here the "kind-other" clientset carries a
// crash-looping pod the base one does not, so the two calls' verdicts and
// findings — not just Cluster.Context — must differ.
func TestClientFor_SwitchingContextsActuallyReadsADifferentCluster(t *testing.T) {
	healthy := fake.NewSimpleClientset()
	crashing := fake.NewSimpleClientset(crashingPod())

	switchTo := func(contextName string) (kubernetes.Interface, error) {
		if contextName == "kind-other" {
			return crashing, nil
		}
		return nil, fmt.Errorf("connectWith test: unexpected switch target %q", contextName)
	}

	cs := connectWith(t, Config{Context: "kind-example", AllowContextSwitch: true}, healthy, switchTo)

	baseline := callTriage(t, cs, map[string]any{})
	if baseline.Verdict != "healthy" {
		t.Fatalf("baseline Verdict = %q, want %q — no context argument must read the base clientset, "+
			"which has no crash-looping pod", baseline.Verdict, "healthy")
	}
	if len(baseline.Findings) != 0 {
		t.Fatalf("baseline Findings = %+v, want none", baseline.Findings)
	}

	switched := callTriage(t, cs, map[string]any{"context": "kind-other"})
	if switched.Verdict != "degraded" {
		t.Fatalf("switched Verdict = %q, want %q — context %q's clientset has a crash-looping pod",
			switched.Verdict, "degraded", "kind-other")
	}
	if len(switched.Findings) == 0 || switched.Findings[0].Reason != "CrashLoopBackOff" {
		t.Fatalf("switched Findings = %+v, want a CrashLoopBackOff finding sourced from kind-other's "+
			"clientset", switched.Findings)
	}
	if switched.Cluster.Context != "kind-other" {
		t.Errorf("switched Cluster.Context = %q, want %q", switched.Cluster.Context, "kind-other")
	}
}

// TestClientFor_FactoryFailureNeverLeaksTheUnderlyingError drives clientFor's
// factory-error branch — unreachable through connect's hardcoded,
// never-failing factory — for all three read-only tools. clientFor
// deliberately discards switchTo's error and reports only the requested
// context name, because a real cluster.NewClient failure wraps the
// kubeconfig path (internal/cluster/client.go's restConfig), and
// redact.Error only special-cases *url.Error, so that string would pass
// through unredacted if a regression started wrapping it instead of
// discarding it. This proves the discard holds at every call site, not just
// one.
func TestClientFor_FactoryFailureNeverLeaksTheUnderlyingError(t *testing.T) {
	const sentinelPath = "/nonexistent/kubeagent-test/secret-cluster.kubeconfig"
	const requestedContext = "kind-other"

	failingSwitch := func(contextName string) (kubernetes.Interface, error) {
		return nil, fmt.Errorf("loading kubeconfig %q (context %q): open %s: no such file or directory",
			sentinelPath, contextName, sentinelPath)
	}

	tests := []struct {
		name string
		args map[string]any
	}{
		{"kubeagent_triage", map[string]any{"context": requestedContext}},
		{"kubeagent_inspect", map[string]any{
			"kind": "pod", "namespace": "payments", "name": "api-abc", "context": requestedContext}},
		{"kubeagent_advisory", map[string]any{"sections": []any{"security"}, "context": requestedContext}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := connectWith(t, Config{Context: "kind-example", AllowContextSwitch: true},
				fake.NewSimpleClientset(), failingSwitch)

			res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
			}
			if !res.IsError {
				t.Fatalf("CallTool() succeeded although the factory failed; want an error result")
			}

			text := firstText(res)
			if !strings.Contains(text, requestedContext) {
				t.Errorf("error text = %q, want it to name the requested context %q", text, requestedContext)
			}
			if strings.Contains(text, sentinelPath) {
				t.Errorf("error text = %q, leaks the kubeconfig path sentinel %q", text, sentinelPath)
			}
		})
	}
}
