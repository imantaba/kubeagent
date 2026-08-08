# `kubeagent mcp` Without a Current Context — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `kubeagent mcp --allow-context-switch` start when the kubeconfig names contexts but marks none current, and make the shipped triage skill's preflight check capability rather than presence.

**Architecture:** Two independent slices. Slice 1 (Tasks 1–3) is Go: `Serve` degrades to a nil `base` clientset in exactly one narrow case, and `clientFor` returns a new sentinel error when a call names no context and there is no default cluster. No new type, no new package, no schema change. Slice 2 (Task 4) is shipped-plugin text only: the skill checks its own tool list first and shells out only to explain an empty one.

**Tech Stack:** Go 1.26, `github.com/modelcontextprotocol/go-sdk/mcp`, `k8s.io/client-go` (fake clientset in tests), Cobra CLI, MkDocs Material for the website.

**Spec:** [docs/superpowers/specs/2026-08-08-mcp-no-current-context-design.md](../specs/2026-08-08-mcp-no-current-context-design.md), committed at `53c0e80`.

## Global Constraints

- Go lives at `/usr/local/go/bin` — every task's commands assume `export PATH=$PATH:/usr/local/go/bin`.
- **No `Co-Authored-By: Claude` trailer** and no Claude/Anthropic attribution of any kind on any commit, in any file, in any comment.
- `git add` by explicit path only. Never `git add -A` or `git add .`.
- **Read-only.** No task may add a cluster write, and `internal/mcp` must never import `internal/remediate` or `internal/explain`. No task adds an import to `internal/mcp` at all — every package it needs (`cluster`, `redact`, `kubernetes`, `mcpsdk`) is already imported by the file being edited.
- **No LLM call** anywhere in `internal/mcp`.
- **No `schemaVersion` moves.** No field, type or enum value on any of the eight versioned JSON documents changes. Do not run `go test ./internal/schemadoc -run TestSchemaDrift -update`.
- **Errors crossing the MCP boundary name no kubeconfig path and no API server address.** The single documented exception is `Serve`'s startup-failure return, which goes to stderr — its existing comment block at `internal/mcp/server.go:109-117` must survive edits verbatim.
- Every error message string is given verbatim in this plan. Do not reword them; tests assert on their content.
- Run `go build ./...` and `go test ./...` before every commit.

---

## File Structure

| File | Change | Responsible for |
|---|---|---|
| `internal/mcp/triage.go` | Modify (add one `var` beside `errContextSwitchDisabled` at line 39-42) | The sentinel error text a caller reads |
| `internal/mcp/server.go` | Modify (`clientFor` at 72-84, `Serve` at 106-128) | The nil-base branch and the degraded-start decision |
| `internal/mcp/server_test.go` | Modify (append tests) | `clientFor`'s table, the degrade predicate's table, the end-to-end protocol test |
| `internal/mcp/contexts_test.go` | Modify (add one fixture + one test) | `list_contexts` still answers, with `current` empty |
| `website/docs/features/mcp.md` | Modify (Context switching section, ~line 128-135) | The user-facing description of the degraded start |
| `CHANGELOG.md` | Modify (`## [Unreleased]`) | Both slices' release notes |
| `skills/triaging-a-cluster/SKILL.md` | Modify (Step 0, lines 12-35) | The three-way capability preflight |
| `commands/triage.md`, `commands/why.md`, `commands/preflight.md` | Modify (line 11 each) | Deferring to the skill's Step 0 |
| `website/docs/features/claude-plugin.md` | Modify (lines 25-26) | The `/mcp`-shows-failed troubleshooting line |

---

### Task 1: The nil-base branch in `clientFor`

`clientFor` currently returns `base` unconditionally when a call names no context. After Task 2, `base` can be nil. This task makes that safe and gives the caller a message that tells it what to do — and it is independently testable, because `newServer` already accepts any `kubernetes.Interface`, including nil.

**Files:**
- Modify: `internal/mcp/triage.go:39-42` (add a second sentinel below the existing one)
- Modify: `internal/mcp/server.go:72-84` (`clientFor`)
- Test: `internal/mcp/server_test.go` (append), `internal/mcp/contexts_test.go` (append)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `var errNoDefaultContext error` in package `mcp` — Task 2 does not reference it by name, but its behaviour is what Task 2's degraded start relies on. `clientFor`'s signature is unchanged: `clientFor(cfg Config, base kubernetes.Interface, switchTo clientFactory, requested string) (kubernetes.Interface, string, error)`.

**Background the implementer needs:**
- `base` is an interface value. A nil interface (`var base kubernetes.Interface`, or the literal `nil` at a call site) compares `== nil`. A typed nil pointer stored in an interface does **not**. Task 2 is written to only ever store a literal nil, so `base == nil` is the correct and sufficient check here.
- Test helpers already in the package: `connect(t, cfg, client)` and `connectWith(t, cfg, client, switchTo)` in `server_test.go:22-51`; `callTriage(t, cs, args)` and `crashingPod()` in `triage_test.go`; `firstText(res)` used in `server_test.go:254`; `writeKubeconfig(t, contents)` and `callListContexts(t, cs)` in `contexts_test.go:46` and `:109`.

- [ ] **Step 1: Write the failing test for `clientFor`**

Append to `internal/mcp/server_test.go`:

```go
// TestClientFor_NilBaseRefusesUnnamedCallsOnly covers the server that started
// without a default cluster: the kubeconfig named contexts but marked none
// current, so Serve passed a nil base (see startableWithoutDefaultContext). A
// call that names no context has nothing to read and must be told how to fix
// that; a call that names one must still work, because switchTo never
// consulted base in the first place.
func TestClientFor_NilBaseRefusesUnnamedCallsOnly(t *testing.T) {
	present := fake.NewSimpleClientset()
	switched := fake.NewSimpleClientset()
	switchTo := func(string) (kubernetes.Interface, error) { return switched, nil }
	cfg := Config{AllowContextSwitch: true}

	tests := []struct {
		name      string
		base      kubernetes.Interface
		requested string
		wantErr   error
		wantLabel string
	}{
		{"unnamed call with a default cluster", present, "", nil, "(current context)"},
		{"unnamed call without a default cluster", nil, "", errNoDefaultContext, ""},
		{"named call with a default cluster", present, "kind-other", nil, "kind-other"},
		{"named call without a default cluster", nil, "kind-other", nil, "kind-other"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, label, err := clientFor(cfg, tc.base, switchTo, tc.requested)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if client != nil {
					t.Errorf("client = %v, want nil alongside an error", client)
				}
				return
			}
			if client == nil {
				t.Fatal("client = nil, want a usable clientset")
			}
			if label != tc.wantLabel {
				t.Errorf("label = %q, want %q", label, tc.wantLabel)
			}
		})
	}
}

// TestErrNoDefaultContext_NamesTheFixAndNoPath pins the message a model reads.
// It crosses the MCP boundary, so it must name list_contexts and the context
// argument — the two things that resolve it — and must not name a kubeconfig
// path or an API server address, because nothing in this package may put one
// on the protocol stream.
func TestErrNoDefaultContext_NamesTheFixAndNoPath(t *testing.T) {
	got := errNoDefaultContext.Error()

	for _, want := range []string{"no current context", "list_contexts", "context"} {
		if !strings.Contains(got, want) {
			t.Errorf("error = %q, want it to mention %q", got, want)
		}
	}
	for _, leak := range []string{"kubeconfig path", "/", "https://", "--"} {
		if strings.Contains(got, leak) {
			t.Errorf("error = %q, contains %q; this string crosses the MCP boundary and must "+
				"name neither a path, a URL, nor a CLI flag a model might try to run", got, leak)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run 'TestClientFor_NilBase|TestErrNoDefaultContext' 2>&1 | head -20
```

Expected: FAIL — `undefined: errNoDefaultContext`.

- [ ] **Step 3: Add the sentinel**

In `internal/mcp/triage.go`, directly below the existing `errContextSwitchDisabled` block (which ends at line 42), add:

```go
// errNoDefaultContext is returned verbatim to the caller, like
// errContextSwitchDisabled above. The server started without a default
// cluster because the kubeconfig marks no context current (see
// startableWithoutDefaultContext in server.go), so a call naming no context
// has nothing to read. The message names the fix — list_contexts, then the
// context argument — without naming a kubeconfig path, a server address, or
// a CLI flag a model might be tempted to run on the operator's behalf.
var errNoDefaultContext = errors.New(
	"this kubeconfig has no current context, so there is no default cluster: call list_contexts and pass one of its names as context")
```

- [ ] **Step 4: Add the nil-base branch**

In `internal/mcp/server.go`, replace `clientFor`'s first branch (lines 73-75):

```go
	if requested == "" {
		return base, contextLabel(cfg.Context), nil
	}
```

with:

```go
	if requested == "" {
		// A nil base means Serve started without a default cluster. base is
		// only ever the literal nil there, never a typed nil pointer stored
		// in the interface, so this comparison is the whole check.
		if base == nil {
			return nil, "", errNoDefaultContext
		}
		return base, contextLabel(cfg.Context), nil
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run 'TestClientFor_NilBase|TestErrNoDefaultContext' -v 2>&1 | tail -20
```

Expected: PASS, all four subtests plus the message test.

- [ ] **Step 6: Write the failing end-to-end protocol test**

Append to `internal/mcp/server_test.go`:

```go
// TestServer_NilBaseIsUsableOverTheProtocol drives the real MCP transport
// against a server built with no default cluster. This is the shape a live
// client sees after Serve degrades: the tools are all registered, a call
// naming a context works normally, and a call naming none comes back as a
// tool-level error result carrying the sentinel's text — not a transport
// failure and not a panic.
func TestServer_NilBaseIsUsableOverTheProtocol(t *testing.T) {
	crashing := fake.NewSimpleClientset(crashingPod())
	switchTo := func(string) (kubernetes.Interface, error) { return crashing, nil }
	cs := connectWith(t, Config{AllowContextSwitch: true}, nil, switchTo)

	named := callTriage(t, cs, map[string]any{"context": "kind-other"})
	if named.Verdict != "degraded" {
		t.Fatalf("named-context Verdict = %q, want %q — a named call must work with no default "+
			"cluster, because it never consulted one", named.Verdict, "degraded")
	}
	if named.Cluster.Context != "kind-other" {
		t.Errorf("named-context Cluster.Context = %q, want %q", named.Cluster.Context, "kind-other")
	}

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "kubeagent_triage"})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() with no context succeeded although there is no default cluster")
	}
	if text := firstText(res); !strings.Contains(text, "list_contexts") {
		t.Errorf("error text = %q, want it to point the caller at list_contexts", text)
	}
}
```

- [ ] **Step 7: Run it to verify it passes**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run TestServer_NilBaseIsUsableOverTheProtocol -v 2>&1 | tail -15
```

Expected: PASS. It should pass immediately — Step 4 is what makes it pass, and Steps 6-7 prove the branch holds through the real transport rather than only in a direct call. If it fails with a nil-pointer panic instead of an error result, `guard` is doing its job but something reached the clientset anyway: report that rather than working around it.

- [ ] **Step 8: Write the failing `list_contexts` test**

Append to `internal/mcp/contexts_test.go`. Put the fixture constant directly below `contextsKubeconfigFixture` (which ends at line 42):

```go
// noCurrentKubeconfigFixture names two contexts and marks neither current —
// the posture of an operator with several production clusters who does not
// want a stray kubectl to hit one. It is the case that made kubeagent mcp
// exit at startup.
const noCurrentKubeconfigFixture = `apiVersion: v1
kind: Config
clusters:
  - name: staging-cluster
    cluster:
      server: https://staging.example.com:6443
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
```

and this test at the end of the file:

```go
// TestListContexts_AnswersWithNoCurrentContextAndNoDefaultCluster is the
// reason the degraded start exists at all: list_contexts is the tool that
// resolves "there is no current context", so it must work on exactly the
// server that condition produces — one built with a nil base. It reads the
// kubeconfig directly and never touches the clientset, so it answers in full,
// with current empty rather than absent.
func TestListContexts_AnswersWithNoCurrentContextAndNoDefaultCluster(t *testing.T) {
	path := writeKubeconfig(t, noCurrentKubeconfigFixture)
	cs := connectWith(t, Config{Kubeconfig: path, AllowContextSwitch: true}, nil,
		func(string) (kubernetes.Interface, error) { return fake.NewSimpleClientset(), nil })

	res, blob := callListContexts(t, cs)
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}

	var out ContextsOutput
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", blob, err)
	}
	if out.Current != "" {
		t.Errorf("current = %q, want empty — this kubeconfig marks no context current", out.Current)
	}
	if len(out.Contexts) != 2 {
		t.Fatalf("contexts = %+v, want both of the kubeconfig's two", out.Contexts)
	}
	if out.Contexts[0].Name != "prod" || out.Contexts[1].Name != "staging" {
		t.Errorf("contexts = %+v, want them sorted by name", out.Contexts)
	}
	for _, c := range out.Contexts {
		if c.Current {
			t.Errorf("context %q is marked current, want none marked", c.Name)
		}
	}
}
```

`internal/mcp/contexts_test.go` needs `"k8s.io/client-go/kubernetes"` added to its import block for the `switchTo` literal's return type.

- [ ] **Step 9: Run it**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run TestListContexts_AnswersWithNoCurrent -v 2>&1 | tail -15
```

Expected: PASS immediately. This one is a regression guard rather than a
red-first test, and deliberately so: `registerContexts` reads the kubeconfig
directly and never touches the clientset, which is the property the whole
degraded start rests on. The test exists to fail if a future edit gives
`list_contexts` a clientset dependency — at which point the degraded server
would stop being able to answer the one question it exists to answer.

- [ ] **Step 10: Run the whole suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... 2>&1 | grep -v '^ok' | head -20; echo "exit: ${PIPESTATUS[0]}"
```

Expected: no FAIL lines. (`grep -v` exiting 1 on no matches is not a test failure — read the `exit:` line, which reports `go test`'s own status.)

- [ ] **Step 11: Commit**

```bash
git add internal/mcp/triage.go internal/mcp/server.go internal/mcp/server_test.go internal/mcp/contexts_test.go
git commit -m "fix(mcp): refuse an unnamed call when there is no default cluster

clientFor returned base unconditionally for a call that named no context.
The next commit lets Serve start with a nil base, so that path needs an
answer: a sentinel, defined beside errContextSwitchDisabled and returned
verbatim, naming list_contexts and the context argument without naming a
kubeconfig path or a server address.

Named calls are untouched — switchTo never consulted base — and
list_contexts reads the kubeconfig directly, so both work with no default
cluster at all."
```

---

### Task 2: Degrade instead of exiting

**Files:**
- Modify: `internal/mcp/server.go:100-128` (`Serve`, plus one new unexported function above it)
- Test: `internal/mcp/server_test.go` (append)

**Interfaces:**
- Consumes: `errNoDefaultContext` and `clientFor`'s nil-base branch from Task 1 — this task does not name the sentinel, but its correctness depends on that branch existing.
- Produces: `func startableWithoutDefaultContext(cfg Config) bool` in package `mcp`. Nothing later calls it; it is referenced by name in Task 1's comments and in Task 3's docs.

**Background the implementer needs:**
- `cluster.Contexts(kubeconfigPath string) ([]cluster.ContextInfo, error)` is at `internal/cluster/client.go:142`. `ContextInfo` has `Name`, `Cluster`, `Server`, `Current`. It is contractually path-free in both its result and its errors, and it already computes `Current: name == raw.CurrentContext`. `internal/cluster` is already imported by `server.go`.
- `cluster.NewClient` returns `(*kubernetes.Clientset, error)`. Assigning a successful one into a `kubernetes.Interface` variable is fine. **Never** assign the failed `client` value into that variable — on the degraded path the variable must stay the literal nil the `var` declaration gave it, or `clientFor`'s `base == nil` check silently fails and every unnamed call panics instead of returning the sentinel.
- The comment block at lines 109-117 explains why the startup error is deliberately unredacted. It moves into the new `default:` branch verbatim — do not shorten it.

- [ ] **Step 1: Write the failing predicate test**

Append to `internal/mcp/server_test.go`:

```go
// oneCurrentKubeconfig, noContextsKubeconfig: the two fixtures the degrade
// predicate must reject. The one it accepts, noCurrentKubeconfigFixture,
// lives in contexts_test.go beside the fixture it was derived from.
const oneCurrentKubeconfig = `apiVersion: v1
kind: Config
current-context: staging
clusters:
  - name: staging-cluster
    cluster:
      server: https://staging.example.com:6443
contexts:
  - name: staging
    context:
      cluster: staging-cluster
      user: staging-user
users:
  - name: staging-user
    user: {}
`

const noContextsKubeconfig = `apiVersion: v1
kind: Config
clusters: []
contexts: []
users: []
`

// TestStartableWithoutDefaultContext_OnlyTheOneNarrowCase pins the trigger.
// Degrading is a real loss of a startup check, so it must fire for exactly
// one condition — switching allowed, no context requested, and a kubeconfig
// that names contexts but marks none current. Every other startup failure
// still exits with the operator-facing error on stderr, which is the honest
// answer for a missing kubeconfig, a typo'd --context, or a cluster that is
// simply unreachable.
func TestStartableWithoutDefaultContext_OnlyTheOneNarrowCase(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		cfg      Config
		want     bool
	}{
		{
			name:     "contexts with none current, switching allowed",
			contents: noCurrentKubeconfigFixture,
			cfg:      Config{AllowContextSwitch: true},
			want:     true,
		},
		{
			name:     "a context is current",
			contents: oneCurrentKubeconfig,
			cfg:      Config{AllowContextSwitch: true},
			want:     false,
		},
		{
			name:     "the kubeconfig names no contexts at all",
			contents: noContextsKubeconfig,
			cfg:      Config{AllowContextSwitch: true},
			want:     false,
		},
		{
			name:     "switching is not allowed, so no call could name a context",
			contents: noCurrentKubeconfigFixture,
			cfg:      Config{},
			want:     false,
		},
		{
			name:     "the operator named a context explicitly, so its failure is theirs to see",
			contents: noCurrentKubeconfigFixture,
			cfg:      Config{AllowContextSwitch: true, Context: "staging"},
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Kubeconfig = writeKubeconfig(t, tc.contents)
			if got := startableWithoutDefaultContext(cfg); got != tc.want {
				t.Errorf("startableWithoutDefaultContext() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("the kubeconfig cannot be read", func(t *testing.T) {
		cfg := Config{AllowContextSwitch: true, Kubeconfig: filepath.Join(t.TempDir(), "absent")}
		if startableWithoutDefaultContext(cfg) {
			t.Error("startableWithoutDefaultContext() = true for an unreadable kubeconfig, want false — " +
				"a path that does not resolve is a configuration error the operator must see")
		}
	})
}
```

`internal/mcp/server_test.go` needs `"path/filepath"` added to its import block.

- [ ] **Step 2: Run it to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run TestStartableWithoutDefaultContext 2>&1 | head -10
```

Expected: FAIL — `undefined: startableWithoutDefaultContext`.

- [ ] **Step 3: Add the predicate**

In `internal/mcp/server.go`, directly above `Serve`'s doc comment (line 100), add:

```go
// startableWithoutDefaultContext reports whether a failed startup connection
// is the one case worth serving anyway: the server may switch contexts, the
// operator named none explicitly, and the kubeconfig names contexts but marks
// none of them current. There is then no default cluster — but every named
// context is still reachable, and list_contexts, the tool that exists to pick
// one, would be unreachable if the process exited here.
//
// The second half is answered with cluster.Contexts, not by matching
// clientcmd's error text, which is not a contract kubeagent can rely on.
// cluster.Contexts is also contractually path-free, so nothing it returns can
// leak into a later tool result.
//
// Everything else still exits: a missing or unreadable kubeconfig, one naming
// zero contexts, an unreachable API server, and a --context that does not
// resolve. Those are configuration or reachability failures an operator needs
// to see, and turning them into a server that starts and refuses every call
// is exactly the behaviour the eager check exists to prevent.
func startableWithoutDefaultContext(cfg Config) bool {
	if !cfg.AllowContextSwitch || cfg.Context != "" {
		return false
	}
	infos, err := cluster.Contexts(cfg.Kubeconfig)
	if err != nil || len(infos) == 0 {
		return false
	}
	for _, info := range infos {
		if info.Current {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run it to verify it passes**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run TestStartableWithoutDefaultContext -v 2>&1 | tail -15
```

Expected: PASS, all six subtests.

- [ ] **Step 5: Wire it into `Serve`**

Replace the body of `Serve` (`internal/mcp/server.go:106-128`) with:

```go
func Serve(ctx context.Context, cfg Config, version string) error {
	// base is nil when the server starts without a default cluster. It is only
	// ever the literal nil below — never the failed *kubernetes.Clientset —
	// because clientFor tests it with base == nil, which a typed nil pointer
	// stored in an interface would defeat.
	var base kubernetes.Interface

	client, err := cluster.NewClient(cfg.Kubeconfig, cfg.Context)
	switch {
	case err == nil:
		if _, err := client.Discovery().ServerVersion(); err != nil {
			return fmt.Errorf("reaching the API server: %s", redact.Error(err))
		}
		base = client
	case startableWithoutDefaultContext(cfg):
		// Serve on with no default cluster: every tool still answers for a
		// context the caller names, and list_contexts — the tool that supplies
		// those names — never needed a clientset at all. An unnamed call gets
		// errNoDefaultContext, which says exactly that. Nothing is printed
		// here: this is not a failure, and stdout is the protocol stream.
	default:
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

	s := newServer(cfg, version, base, func(contextName string) (kubernetes.Interface, error) {
		return cluster.NewClient(cfg.Kubeconfig, contextName)
	}, time.Now)
	return s.Run(ctx, &mcpsdk.StdioTransport{})
}
```

Also extend `Serve`'s doc comment (lines 100-105) so it stops claiming the connection is always validated:

```go
// Serve connects to the cluster and serves MCP over stdio until the client
// disconnects.
//
// The connection is validated eagerly. A server that starts happily and then
// fails every tool call teaches the calling model that kubeagent is unreliable;
// failing at startup puts the error where a human will read it. The one
// exception is startableWithoutDefaultContext's case, where there is no
// default cluster to validate but every named context still works.
```

- [ ] **Step 6: Verify the whole package still passes**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./internal/mcp 2>&1 | tail -5
```

Expected: `ok  	github.com/imantaba/kubeagent/internal/mcp`.

- [ ] **Step 7: Verify by hand against a real kubeconfig with no current context**

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o /tmp/kubeagent-nocurrent .
cat > /tmp/kubeagent-nocurrent.kubeconfig <<'EOF'
apiVersion: v1
kind: Config
clusters:
  - name: a-cluster
    cluster:
      server: https://a.example.com:6443
contexts:
  - name: a
    context:
      cluster: a-cluster
      user: a-user
users:
  - name: a-user
    user: {}
EOF
KUBECONFIG=/tmp/kubeagent-nocurrent.kubeconfig timeout 5 /tmp/kubeagent-nocurrent mcp --allow-context-switch </dev/null; echo "exit: $?"
```

Expected: no `connecting to the cluster:` error. Exit 0 or 124 (the `timeout`), both meaning the process served rather than refusing to start. Before this task it printed `connecting to the cluster: loading kubeconfig ...: invalid configuration: no configuration has been provided` and exited 1.

Then check the refusal path still exits:

```bash
KUBECONFIG=/tmp/absent-kubeagent-test.kubeconfig timeout 5 /tmp/kubeagent-nocurrent mcp --allow-context-switch </dev/null; echo "exit: $?"
```

Expected: `connecting to the cluster: ...` on stderr, exit 1.

Clean up: `rm -f /tmp/kubeagent-nocurrent /tmp/kubeagent-nocurrent.kubeconfig`.

- [ ] **Step 8: Run the whole suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... 2>&1 | grep -v '^ok' | head -20; echo "exit: ${PIPESTATUS[0]}"
```

Expected: no FAIL lines.

- [ ] **Step 9: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "feat(mcp): start without a default cluster when no context is current

A kubeconfig that names contexts but marks none current made kubeagent mcp
exit at startup — even under --allow-context-switch, which makes
list_contexts, the tool that exists to pick a context, unreachable in
exactly the case it was built for.

Serve now degrades in that one narrow case: switching allowed, no --context
given, and the kubeconfig parses into at least one context with none
current. The base clientset stays nil, all four tools stay registered, and
a call naming a context works normally. cluster.Contexts answers the
question, so nothing depends on clientcmd's error text.

Every other startup failure still exits with the same message: a missing or
unreadable kubeconfig, one naming zero contexts, an unreachable API server,
a --context that does not resolve."
```

---

### Task 3: Document the degraded start

**Files:**
- Modify: `website/docs/features/mcp.md` (the "Context switching" section, lines 128-135)
- Modify: `CHANGELOG.md` (`## [Unreleased]`)

**Interfaces:**
- Consumes: the behaviour Tasks 1 and 2 shipped. No code.
- Produces: nothing later tasks reference.

- [ ] **Step 1: Extend the Context switching section**

In `website/docs/features/mcp.md`, after the paragraph ending "…accept a `context` argument naming any context in the same kubeconfig." (line 135), add:

```markdown
`--allow-context-switch` also changes what happens at startup when your
kubeconfig marks **no** context as current — the usual posture when you hold
several production kubeconfigs and do not want a stray `kubectl` to reach one.
Without the flag, the server exits: there is no default cluster and no way to
name one. With it, the server starts anyway, with no default cluster. All four
tools stay registered, `list_contexts` answers as usual with an empty
`current`, and a call naming a `context` works normally. A call naming none is
refused with a message telling the caller to list the contexts and pick one.

Nothing else degrades. A kubeconfig that cannot be read, one naming no
contexts at all, an API server that cannot be reached, and a `--context` that
does not resolve all still exit at startup with the error below — a server
that starts and then fails every call is worse than one that refuses to start.
```

- [ ] **Step 2: Add the changelog entry**

In `CHANGELOG.md`, under `## [Unreleased]`, add a `### Fixed` section below the existing `### Added` block (which ends with the "See [Claude Code plugin]…" line):

```markdown
### Fixed

- **`kubeagent mcp` no longer exits when the kubeconfig marks no context as
  current.** Under `--allow-context-switch` — and only there — the server now
  starts without a default cluster instead of refusing to start. That flag
  registers `list_contexts`, the tool whose whole purpose is to pick a context,
  and it was unreachable in exactly the case it was built for. All four tools
  stay registered, a call naming a `context` works normally, and a call naming
  none is refused with a message pointing at `list_contexts`. Every other
  startup failure still exits with the same operator-facing error: a missing or
  unreadable kubeconfig, one naming zero contexts, an unreachable API server, a
  `--context` that does not resolve. No `schemaVersion` moves and no import-graph
  invariant changes. See [MCP server](website/docs/features/mcp.md).
```

- [ ] **Step 3: Verify the docs build and the suite is green**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./... 2>&1 | grep -v '^ok' | head -10; echo "exit: ${PIPESTATUS[0]}"
grep -c '^## \[Unreleased\]' CHANGELOG.md
```

Expected: no FAIL lines, and exactly `1` for the `Unreleased` heading count.

- [ ] **Step 4: Commit**

```bash
git add website/docs/features/mcp.md CHANGELOG.md
git commit -m "docs: the degraded MCP start when no kubeconfig context is current

Names what --allow-context-switch now changes at startup, and — as
importantly — what it does not: every other startup failure still exits."
```

---

### Task 4: Preflight capability, not presence

`command -v kubeagent` passed on a binary built before `kubeagent mcp` existed. That is what the live-client failure actually was, and the current Step 0 cannot say it — it can only tell the user to install kubeagent, which they had already done.

**Files:**
- Modify: `skills/triaging-a-cluster/SKILL.md:12-35` (Step 0)
- Modify: `commands/triage.md:11`, `commands/why.md:11`, `commands/preflight.md:11`
- Modify: `website/docs/features/claude-plugin.md:25-26`
- Modify: `CHANGELOG.md` (`## [Unreleased]`, the `### Fixed` section Task 3 created)
- Test: `plugin_manifest_test.go` already covers this — no new test file.

**Interfaces:**
- Consumes: Task 2's behaviour — the third preflight branch is only accurate because a no-current-context kubeconfig no longer kills the server.
- Produces: nothing later tasks reference.

**Background the implementer needs:**
- `TestShippedDocsNameOnlyRegisteredTools` (`plugin_manifest_test.go:210`) requires every file in `pluginDocFiles` to name **at least one** registered MCP tool, and to name **no** unregistered one. The registered set is `kubeagent_triage`, `kubeagent_inspect`, `kubeagent_advisory`, `list_contexts`, derived by reading `internal/mcp/*.go`. Keep the new text inside that vocabulary.
- The probe is real: on a pre-Cobra build `kubeagent mcp --help` exits 1; on the current binary it exits 0 without touching a cluster. That is the discriminator, verified by hand.
- Do not name a version floor. `kubeagent mcp --help` asks the binary, and stays true as releases move.

- [ ] **Step 1: Replace Step 0 of the skill**

In `skills/triaging-a-cluster/SKILL.md`, replace everything from `## Step 0: preflight` (line 12) up to but not including `## Step 1: always start with kubeagent_triage` (line 37) with the following — note the outer fence here is four backticks
because the replacement text itself contains fenced blocks; write the inner
three-backtick blocks into the skill, not the outer fence:

````markdown
## Step 0: preflight

The plugin cannot ship kubeagent's binary, so the MCP server may not be
running. Check **capability, not presence** — and check it in your own tool
list first, which costs no command:

**Is `kubeagent_triage` among the tools available to you?**

If it is, the server is running and connected. Go to Step 1. Do not shell out
to check anything.

If it is not, no diagnosis is possible and every step below would fail. Shell
out only to find out *why*, in this order.

```bash
command -v kubeagent
```

**Nothing printed** — kubeagent is not installed. Stop and give the user all
three install paths:

```bash
go install github.com/imantaba/kubeagent@latest
```

```bash
kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml
```

Or a prebuilt binary from <https://github.com/imantaba/kubeagent/releases>.

**A path printed** — kubeagent is installed, but "installed" is not "recent
enough to serve MCP". Ask the binary:

```bash
kubeagent mcp --help
```

**That failed** — the binary predates the MCP server, so the plugin's tools can
never appear no matter how many times it is reloaded. Tell the user to
**upgrade** it. The three commands above are the same ones, but the diagnosis
is not: reinstalling is the fix, and being told to install something they
already have is not.

**Both succeeded** — the binary is fine and the server itself failed to start
or connect. The usual causes are a kubeconfig Claude Code cannot read and an
API server it cannot reach. Tell the user to check `/mcp` for the server's
error and stop there.

Do not attempt the diagnosis with the tools missing, and do not substitute
`kubectl` for them. Reporting a cluster healthy because you could not look at
it is the worst outcome available here.
````

- [ ] **Step 2: Point the three commands at it**

In `commands/triage.md`, replace lines 11-12:

```markdown
1. Preflight: confirm `kubeagent` is on PATH. If not, stop and give the install
   commands.
```

with:

```markdown
1. Preflight: follow the `triaging-a-cluster` skill's Step 0 — confirm
   `kubeagent_triage` is in your tool list, and shell out only if it is not.
```

In `commands/why.md`, replace line 11:

```markdown
1. Preflight: confirm `kubeagent` is on PATH.
```

with:

```markdown
1. Preflight: follow the `triaging-a-cluster` skill's Step 0 — confirm
   `kubeagent_inspect` is in your tool list, and shell out only if it is not.
```

In `commands/preflight.md`, replace line 11:

```markdown
1. Preflight: confirm `kubeagent` is on PATH.
```

with:

```markdown
1. Preflight: follow the `triaging-a-cluster` skill's Step 0 — confirm
   `kubeagent_triage` is in your tool list, and shell out only if it is not.
```

- [ ] **Step 3: Fix the plugin doc's troubleshooting line**

In `website/docs/features/claude-plugin.md`, replace lines 25-26:

```markdown
Confirm the server connected with `/mcp`. If it shows as failed, the usual
cause is `kubeagent` not being on the `PATH` Claude Code sees.
```

with:

```markdown
Confirm the server connected with `/mcp`. If it shows as failed, there are two
common causes and they need different fixes: `kubeagent` is not on the `PATH`
Claude Code sees, or it is there but predates the MCP server. `kubeagent mcp
--help` tells you which — it fails on a binary too old to serve, and reinstalling
over the top fixes that. The `triaging-a-cluster` skill runs this same check
before its first tool call.
```

- [ ] **Step 4: Extend the changelog entry**

In `CHANGELOG.md`, add a second bullet to the `### Fixed` section under `## [Unreleased]`:

```markdown
- **The shipped `triaging-a-cluster` skill preflights capability, not presence.**
  `command -v kubeagent` passes on a binary built before `kubeagent mcp`
  existed, which is a plugin whose skills load and whose tools never appear.
  The skill now checks whether the `kubeagent_*` tools are in the model's own
  tool list — no subprocess — and shells out only to explain an empty one:
  nothing installed, a binary too old to serve MCP (`kubeagent mcp --help`
  fails), or a server that could not connect. The three commands defer to it
  rather than restating a check that is now three-way.
```

- [ ] **Step 5: Run the manifest tests**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run 'TestPlugin|TestMarketplace|TestShippedDocs' -v 2>&1 | tail -20
```

Expected: PASS for all four. `TestShippedDocsNameOnlyRegisteredTools` is the one that matters — it proves the rewritten text names only tools `internal/mcp` registers, and that every shipped file still names at least one.

- [ ] **Step 6: Run the whole suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... 2>&1 | grep -v '^ok' | head -20; echo "exit: ${PIPESTATUS[0]}"
```

Expected: no FAIL lines.

- [ ] **Step 7: Commit**

```bash
git add skills/triaging-a-cluster/SKILL.md commands/triage.md commands/why.md commands/preflight.md website/docs/features/claude-plugin.md CHANGELOG.md
git commit -m "fix(plugin): preflight the MCP tools, not the binary's presence

command -v kubeagent passes on a build that predates the mcp subcommand —
the plugin's skills load, its tools never appear, and the skill's preflight
records a pass. That was the live-client failure.

Step 0 now checks the model's own tool list first, at no subprocess cost,
and shells out only to explain an empty one: not installed, installed but
too old to serve MCP (kubeagent mcp --help fails), or a server that could
not connect. The three commands defer to it instead of restating a check
that is now three-way."
```

---

## Self-Review

**1. Spec coverage.**

| Spec section | Task |
|---|---|
| Slice 1 — trigger, both halves required | Task 2, Step 3 (`startableWithoutDefaultContext`) + Step 1's six-case table |
| Slice 1 — second half via `cluster.Contexts`, not error strings | Task 2, Step 3 |
| Slice 1 — degraded means `base` is nil, `newServer` unchanged | Task 2, Step 5 |
| Slice 1 — `list_contexts` works, `current` empty | Task 1, Steps 8-9 |
| Slice 1 — `clientFor` grows one branch, sentinel mirrors `errContextSwitchDisabled` | Task 1, Steps 3-4 |
| Slice 1 — named calls untouched | Task 1, Step 1 table rows 3-4, and Step 6's protocol test |
| Slice 1 — no `schemaVersion`, read-only, no LLM call, import wall | Global Constraints; no task adds an import to `internal/mcp` |
| Slice 1 — startup carve-out preserved | Task 2, Step 5 (comment block moved verbatim into `default:`) |
| Slice 1 — testing: `clientFor` table, predicate against temp kubeconfigs, end-to-end | Task 1 Steps 1/6/8, Task 2 Step 1 |
| Slice 2 — tool-list check first, three-way shell-out | Task 4, Step 1 |
| Slice 2 — the three commands defer to the skill | Task 4, Step 2 |
| Slice 2 — no version floor named | Task 4, Step 1 (`kubeagent mcp --help`) |
| Slice 2 — `TestShippedDocsNameOnlyRegisteredTools` still holds | Task 4, Step 5 |
| Out of scope: CLI-command-line pinning test, degrading on an unreachable API server, manifest change | No task touches any of them |

The spec's `ServerVersion` clarification ("the probe runs only when `NewClient` succeeded") is realised by Task 2 Step 5's `case err == nil:` branch. Documentation is not in the spec's task list but is required by the repo's conventions — Tasks 3 and 4 carry it.

**2. Placeholder scan.** Every code block is complete. Every error string, comment, and doc paragraph is given in full. No "TBD", no "similar to Task N", no "add error handling".

**3. Type consistency.** `startableWithoutDefaultContext(cfg Config) bool` is spelled identically in Task 1's comment, Task 2's implementation, Task 2's test, and Task 3's prose. `errNoDefaultContext` is spelled identically in Tasks 1 and 2. `clientFor`'s signature is unchanged. `noCurrentKubeconfigFixture` is defined once (Task 1, Step 8, in `contexts_test.go`) and consumed by Task 2's table — both files are package `mcp`, so the reference resolves. Task 2's own two fixtures (`oneCurrentKubeconfig`, `noContextsKubeconfig`) are defined in `server_test.go` and used only there.
