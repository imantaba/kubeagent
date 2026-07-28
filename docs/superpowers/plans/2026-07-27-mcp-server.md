# MCP Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose kubeagent's deterministic, read-only diagnosis to other AI agents as an MCP server (`kubeagent mcp`), with four tools, a machine-readable honesty contract, and no path to `--fix`.

**Architecture:** A new `internal/mcp` package wraps the existing `internal/scan.Evaluate` seam and serves it over stdio using `github.com/modelcontextprotocol/go-sdk` v1.6.1. The server owns no cluster logic of its own: it connects once at startup (eager validation), calls `scan.Evaluate` per request (no cache), and *projects* the result into its own json-tagged view structs. Two supporting refactors land first — `internal/redact` (credential redaction, lifted out of `internal/alert`) and `internal/advisory` (the `--operators`/`--drift`/`--capacity` block lifted out of `main.go`, returning structured degradations instead of stderr warnings) — so the CLI and the MCP server share one implementation and one honesty story.

**Tech Stack:** Go 1.26, standard-library `flag`, `github.com/modelcontextprotocol/go-sdk/mcp` v1.6.1, `github.com/google/jsonschema-go/jsonschema` (explicit input schemas with enums), client-go fake clientset for tests.

## Global Constraints

Copied from the spec's "Global constraints" section. Every task's requirements implicitly include this list.

- **Read-only toward the cluster, always.** The MCP server issues only get/list/watch calls. No tool, argument, or code path may reach `--fix`, `internal/remediate`, or any create/update/patch/delete verb. `internal/mcp` must never import `internal/remediate`.
- **No LLM calls from the server.** The MCP server never calls the Anthropic API. `internal/mcp` must never import `internal/explain`. The caller *is* the model; kubeagent stays the deterministic tool.
- **Kubeconfigs and endpoint URLs are credentials.** No log line, error string, tool result, or documentation example may carry more than `scheme://host`. Kubeconfig **paths** are never echoed back to the caller. All error text crossing the MCP boundary goes through `redact.Error`.
- **No API key anywhere in an argument.** No tool takes a token, key, or bearer credential as an argument.
- **Standard-library `flag` only.** No Cobra. The `mcp` subcommand parses with its own `flag.FlagSet`, matching `watch`.
- **Absence must never read as zero.** Every tool result carries a `coverage` block naming what ran, what was skipped and why, and what was read only partially. A model reading the JSON must be able to tell "no findings" from "did not look".
- **Project only from json-tagged structs.** `scan.Result` and `scan.Options` carry **no** json tags and must never be marshalled directly. Every value crossing the MCP boundary lives in a struct defined in `internal/mcp` with explicit json tags.
- **Deterministic output.** Findings are sorted by a total order (severity, then namespace, then name, then kind, then reason) so identical clusters produce identical payloads.
- **The golden scan fixture stays byte-identical.** `internal/report/testdata/golden-scan.txt` must not change in this branch. If it changes, the refactor leaked into CLI output and is wrong.
- **A default `kubeagent scan` issues no extra API call** after the advisory extraction. The advisory path builds dynamic clients lazily, only when `--operators` or `--drift` is set.
- **Every commit is authored solely by the human.** No `Co-Authored-By: Claude` trailer, no Claude/Claude Code/Anthropic attribution in commit messages, code, comments, docs, or the changelog.
- **No secrets, private IPs, or internal hostnames** in code, tests, fixtures, or docs. Use `<PLACEHOLDER>`, `example.com`, or RFC-5737 documentation addresses.
- **Go on PATH:** `export PATH=$PATH:/usr/local/go/bin` before any `go` command.

## Measured go-sdk v1.6.1 behaviours

Every item here was verified by compiling and running a probe against v1.6.1. Do not
re-derive them from documentation, and do not assume the opposite of any of them.

- `mcpsdk.AddTool[In, Out]` infers the input schema from the Go type; a `jsonschema:"…"`
  struct tag becomes the property `description`. A json tag with `,omitempty` makes the
  property **optional**; without it, **required**.
- Passing an explicit `InputSchema: &jsonschema.Schema{…}` is honoured, which is the only
  way to publish an `enum`. `jsonschema.Schema.Enum` is `[]any`.
- Argument validation — including enum membership — runs **before** the handler. Bad
  arguments produce `CallToolResult{IsError: true}` with SDK-generated text such as
  `validating "arguments": validating root: required: missing properties: ["name"]`.
- Only an **unknown tool name** is a JSON-RPC-level error (`calling "tools/call": unknown
  tool "kubeagent_fix"`); everything else is a tool result with `IsError` set.
- A handler-returned `error` becomes `IsError: true` with `err.Error()` as the text — so
  every error string must already be redacted.
- **A panic in a handler kills the process.** The SDK does not recover; the process exits
  with status 2. Hence the `guard` wrapper.
- `CallToolResult.StructuredContent` is `any` and carries the typed output.
- `mcpsdk.NewInMemoryTransports()` returns two connected transports for in-process tests.
- Tools are listed **alphabetically** by name.
- Over stdio, `Run` returns `"server is closing: EOF"` when the client closes stdin with
  requests in flight, and `errors.Is(err, io.EOF)` is **false** (the error is not wrapped).
- **Responses arrive out of order** over stdio — the SDK serves requests concurrently. Any
  script driving it must match responses by `id`, never by line position.

## File Structure

**New packages**

| File | Responsibility |
|------|----------------|
| `internal/redact/redact.go` | `URL(string) string` and `Error(error) string` — the only credential-redaction implementation in the tree. |
| `internal/advisory/advisory.go` | Runs the optional `--operators` / `--drift` / `--capacity` sections and reports structured `Degradation`s instead of printing warnings. Also `ClusterPods` (cluster-wide pod fallback). |
| `internal/mcp/config.go` | `Config` (kubeconfig, context, allow-context-switch, logs) and `Serve`. |
| `internal/mcp/coverage.go` | The honesty contract: `Coverage`, `SkippedCheck`, `PartialRead`, and their builders. |
| `internal/mcp/view.go` | json-tagged view types (`Finding`) and the projections from every internal issue type into them, plus the total sort and the finding cap. |
| `internal/mcp/server.go` | Server construction, tool registration, the panic guard, eager startup validation. |
| `internal/mcp/triage.go` | `kubeagent_triage` handler. |
| `internal/mcp/inspect.go` | `kubeagent_inspect` handler. |
| `internal/mcp/advisory.go` | `kubeagent_advisory` handler. |
| `internal/mcp/contexts.go` | `list_contexts` handler (registered only when context switching is allowed). |

**Modified**

| File | Change |
|------|--------|
| `internal/alert/url.go`, `internal/alert/url_test.go` | Redaction helpers move out; `resolveURL`/`sanitizeErr` stay. |
| `internal/gitops/gitops.go`, `internal/operators/operators.go`, `internal/watch/*.go`, `internal/alert/sink.go` | Call `redact.URL` / `redact.Error`. |
| `main.go` | Advisory block extracted; `mcp` subcommand added; usage string extended. |
| `internal/scan/scan.go` | New `ReadFailure` type and `Result.PartialReads`, recorded where collector errors are currently discarded. |
| `internal/inventory/inventory.go` | `humanAge` exported as `HumanAge`. |
| `internal/cluster/client.go` | New `Contexts(kubeconfigPath)`. |
| `internal/collect/collect.go` | New `ObjectEvents`. |
| `chaos/run.sh` | Scenario 19. |
| `README.md`, `CHANGELOG.md`, `CLAUDE.md`, `website/docs/features/mcp.md`, `website/mkdocs.yml`, `website/docs/roadmap.md` | Documentation. |

---

### Task 1: `internal/redact` — one redaction implementation

**Files:**
- Create: `internal/redact/redact.go`
- Create: `internal/redact/redact_test.go`
- Modify: `internal/alert/url.go` (delete `RedactURL` and `RedactError`; keep `resolveURL`, `sanitizeErr`)
- Modify: `internal/alert/url_test.go` (delete the two moved tests; keep `TestResolveURL`, `TestResolveURL_ErrorsNeverEchoTheURL`, `TestSanitizeErr_StripsTheURLNetHTTPEmbeds` and the `slackish` const they use)
- Modify: `internal/alert/sink.go:190,194,196`
- Modify: `internal/gitops/gitops.go:175`
- Modify: `internal/operators/operators.go:155`
- Modify: `internal/watch/watch.go:194,271`
- Modify: `internal/watch/metrics.go:181`
- Modify: `internal/watch/cluster.go:199`

**Interfaces:**
- Consumes: nothing (first task).
- Produces:
  - `func redact.URL(raw string) string` — returns `scheme://host`, or `"(redacted)"` when the input does not parse into both.
  - `func redact.Error(err error) string` — unwraps `*url.Error` recursively, redacting the URL at each level; returns `""` for a nil error.

**Why this task exists:** the MCP server must redact every error string it returns, and `internal/mcp` importing `internal/alert` (a webhook-sending package) to get a string helper would be backwards. The helpers move to a leaf package that imports only `errors` and `net/url`, so nothing can cycle.

- [ ] **Step 1: Write the failing test**

Create `internal/redact/redact_test.go`:

```go
package redact

import (
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"strips path query and fragment", "https://hooks.example.com/services/T000/B000/XXXX?tok=abc#frag", "https://hooks.example.com"},
		{"keeps port", "http://alerts.example.com:8080/ingest", "http://alerts.example.com:8080"},
		{"userinfo is dropped with the rest", "https://user:pass@alerts.example.com/hook", "https://alerts.example.com"},
		{"unparseable", "://nonsense", "(redacted)"},
		{"no host", "file:///etc/kubeagent/token", "(redacted)"},
		{"empty", "", "(redacted)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := URL(tt.raw); got != tt.want {
				t.Errorf("URL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestError_URLErrorKeepsSchemeHostAndUnderlyingCause(t *testing.T) {
	inner := errors.New("connection refused")
	err := &url.Error{Op: "Post", URL: "https://hooks.example.com/services/T000/SECRET", Err: inner}

	got := Error(err)
	want := "Post https://hooks.example.com: connection refused"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestError_NestedURLErrorsAreRedactedAtEveryLevel(t *testing.T) {
	inner := &url.Error{Op: "Get", URL: "https://inner.example.com/a/SECRET", Err: errors.New("timeout")}
	outer := &url.Error{Op: "Post", URL: "https://outer.example.com/b/SECRET", Err: fmt.Errorf("wrapped: %w", inner)}

	got := Error(outer)
	want := "Post https://outer.example.com: Get https://inner.example.com: timeout"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestError_NilIsEmpty(t *testing.T) {
	if got := Error(nil); got != "" {
		t.Errorf("Error(nil) = %q, want empty", got)
	}
}

func TestError_PlainErrorPassesThrough(t *testing.T) {
	if got := Error(errors.New("boom")); got != "boom" {
		t.Errorf("Error() = %q, want %q", got, "boom")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/redact/
```

Expected: FAIL — `undefined: URL`, `undefined: Error`.

- [ ] **Step 3: Create the package**

Create `internal/redact/redact.go`:

```go
// Package redact turns credential-bearing values into safe-to-log strings.
//
// Endpoint URLs are credentials: a Slack or Teams webhook URL is a bearer
// token in path form, and a Git remote or API server URL can carry userinfo
// and query secrets. Nothing outside this package should format a URL or a
// URL-carrying error for a log line, a metric label, a CLI warning, or a
// tool result — call URL or Error instead.
package redact

import (
	"errors"
	"net/url"
)

// URL reduces a URL to scheme://host, dropping the path, query, fragment and
// any userinfo. Anything that does not parse into both a scheme and a host is
// reported as "(redacted)" rather than echoed.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "(redacted)"
	}
	return u.Scheme + "://" + u.Host
}

// Error renders an error safely. net/http embeds the full request URL in the
// *url.Error it returns, so a bare err.Error() would leak the whole webhook
// path; Error walks the chain and redacts the URL at every level while keeping
// the operation and the underlying cause, which are what a reader needs.
func Error(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + " " + URL(ue.URL) + ": " + Error(ue.Err)
	}
	return err.Error()
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./internal/redact/
```

Expected: PASS (5 tests).

- [ ] **Step 5: Delete the old helpers and move their tests**

In `internal/alert/url.go`, delete the `RedactURL` and `RedactError` functions and their doc comments. Keep the package doc, `resolveURL`, and `sanitizeErr`. `sanitizeErr` now delegates:

```go
func sanitizeErr(err error) string {
	return redact.Error(err)
}
```

Add `"github.com/imantaba/kubeagent/internal/redact"` to the imports and drop `"errors"` / `"net/url"` if nothing else in the file uses them (`resolveURL` uses `net/url`, so keep that one).

In `internal/alert/url_test.go`, delete `TestRedactURL` and `TestRedactError_URLErrorKeepsSchemeHostAndUnderlyingCause`. Keep everything else, including the `slackish` const.

- [ ] **Step 6: Update every call site**

Replace `alert.RedactURL(` with `redact.URL(` and `alert.RedactError(` with `redact.Error(` at:

- `internal/alert/sink.go:190,194,196` (in-package: `RedactURL(` → `redact.URL(`, `RedactError(` → `redact.Error(`)
- `internal/gitops/gitops.go:175`
- `internal/operators/operators.go:155`
- `internal/watch/watch.go:194,271`
- `internal/watch/metrics.go:181`
- `internal/watch/cluster.go:199`

Find any stragglers:

```bash
grep -rn 'RedactURL\|RedactError' --include='*.go' .
```

Expected: no matches.

- [ ] **Step 7: Run the full suite**

```bash
go build ./... && go test ./...
```

Expected: all packages PASS. `internal/gitops/gitops_test.go:140` and `internal/watch/watch_test.go:769` assert on redacted message text — they must still pass unchanged; if either fails, the delegation changed behaviour and is wrong.

- [ ] **Step 8: Commit**

```bash
git add internal/redact internal/alert internal/gitops internal/operators internal/watch
git commit -m "refactor(redact): lift URL and error redaction into its own package"
```

---

### Task 2: `internal/advisory` — structured degradations instead of stderr warnings

**Files:**
- Create: `internal/advisory/advisory.go`
- Create: `internal/advisory/advisory_test.go`
- Modify: `main.go:191-234` (replace the advisory block), `main.go:606-616` (delete `enabledFlagNames`), `main.go:629-642` (delete `resolveResourcePods`)
- Modify: `main_test.go:343-392` (the three `resolveResourcePods` tests move to `internal/advisory/advisory_test.go` as `ClusterPods` tests)

**Interfaces:**
- Consumes: `redact.Error` (Task 1).
- Produces:
  - `type advisory.Degradation struct { Sections []string; Subject string; Reason string }` — `Sections` holds machine names (`"operators"`, `"drift"`, `"capacity"`), `Subject` is the human phrase the CLI prints (`"--operators/--drift"`, `"pod metrics"`), `Reason` is already redacted.
  - `type advisory.Options struct { Operators bool; Drift bool; DriftAge time.Duration; Capacity bool; Namespace string }`
  - `type advisory.Result struct { Operators *operators.Report; GitOps *gitops.Report; Capacity *capacity.Report; MetricsAvailable bool; Degradations []Degradation }`
  - `type advisory.DynFactory func() (dynamic.Interface, discovery.DiscoveryInterface, error)`
  - `func advisory.Assess(ctx context.Context, client kubernetes.Interface, dyn DynFactory, in Inputs, opts Options, now time.Time) Result`
  - `type advisory.Inputs struct { Deployments []appsv1.Deployment; StatefulSets []appsv1.StatefulSet; DaemonSets []appsv1.DaemonSet; Jobs []batchv1.Job; CronJobs []batchv1.CronJob; ReplicaSets []appsv1.ReplicaSet; Nodes []corev1.Node; Pods []corev1.Pod }`
  - `func advisory.FlagNames(operators, drift bool) string`
  - `func advisory.ClusterPods(ctx context.Context, client kubernetes.Interface, namespace string, scoped []corev1.Pod) ([]corev1.Pod, error)`

**Why this task exists:** `main.go` prints `kubeagent: warning: …` to stderr when an advisory section cannot run. An MCP caller has no stderr, and a section that silently vanished from a JSON payload is exactly the "absence reads as zero" failure the honesty contract forbids. Moving the block into a package that *returns* its degradations lets the CLI keep printing them and lets MCP put them in `coverage`.

- [ ] **Step 1: Write the failing test**

Create `internal/advisory/advisory_test.go`:

```go
package advisory

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func pod(ns, name string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func TestFlagNames(t *testing.T) {
	tests := []struct {
		operators, drift bool
		want             string
	}{
		{true, true, "--operators/--drift"},
		{true, false, "--operators"},
		{false, true, "--drift"},
	}
	for _, tt := range tests {
		if got := FlagNames(tt.operators, tt.drift); got != tt.want {
			t.Errorf("FlagNames(%v, %v) = %q, want %q", tt.operators, tt.drift, got, tt.want)
		}
	}
}

func TestClusterPods_ClusterScopeReturnsScopedUnchanged(t *testing.T) {
	client := fake.NewSimpleClientset()
	scoped := []corev1.Pod{pod("a", "one")}

	got, err := ClusterPods(context.Background(), client, "", scoped)
	if err != nil {
		t.Fatalf("ClusterPods() error = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Name != "one" {
		t.Errorf("ClusterPods() = %v, want the scoped slice unchanged", got)
	}
}

func TestClusterPods_NamespacedFetchesEveryNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "one"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "two"}},
	)
	scoped := []corev1.Pod{pod("a", "one")}

	got, err := ClusterPods(context.Background(), client, "a", scoped)
	if err != nil {
		t.Fatalf("ClusterPods() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Errorf("ClusterPods() returned %d pods, want 2 (the whole cluster)", len(got))
	}
}

func TestClusterPods_ListFailureFallsBackToScopedAndReportsWhy(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pods is forbidden")
	})
	scoped := []corev1.Pod{pod("a", "one")}

	got, err := ClusterPods(context.Background(), client, "a", scoped)
	if err == nil {
		t.Fatal("ClusterPods() error = nil, want the list failure reported")
	}
	if len(got) != 1 || got[0].Name != "one" {
		t.Errorf("ClusterPods() = %v, want the scoped slice as the fallback", got)
	}
}

func TestAssess_NothingEnabledDoesNothing(t *testing.T) {
	called := false
	dyn := func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
		called = true
		return nil, nil, errors.New("must not be called")
	}

	got := Assess(context.Background(), fake.NewSimpleClientset(), dyn, Inputs{}, Options{}, time.Now())

	if called {
		t.Error("Assess() built dynamic clients with no advisory section enabled; a default scan must issue no extra API call")
	}
	if got.Operators != nil || got.GitOps != nil || got.Capacity != nil {
		t.Errorf("Assess() = %+v, want all reports nil", got)
	}
	if len(got.Degradations) != 0 {
		t.Errorf("Assess() degradations = %v, want none", got.Degradations)
	}
}

func TestAssess_DynamicClientFailureDegradesBothSectionsOnce(t *testing.T) {
	dyn := func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
		return nil, nil, errors.New("no CRD access")
	}

	got := Assess(context.Background(), fake.NewSimpleClientset(), dyn, Inputs{},
		Options{Operators: true, Drift: true}, time.Now())

	if len(got.Degradations) != 1 {
		t.Fatalf("Assess() degradations = %v, want exactly one", got.Degradations)
	}
	d := got.Degradations[0]
	if d.Subject != "--operators/--drift" {
		t.Errorf("Subject = %q, want %q", d.Subject, "--operators/--drift")
	}
	if len(d.Sections) != 2 || d.Sections[0] != "operators" || d.Sections[1] != "drift" {
		t.Errorf("Sections = %v, want [operators drift]", d.Sections)
	}
	if d.Reason != "no CRD access" {
		t.Errorf("Reason = %q, want %q", d.Reason, "no CRD access")
	}
	if got.Operators != nil || got.GitOps != nil {
		t.Error("Assess() produced a report despite the client failure")
	}
}

func TestAssess_CapacityRunsWithoutMetricsAndSaysSo(t *testing.T) {
	// The fake clientset serves no metrics API, which collect.PodMetrics
	// reports as "not available" rather than as an error.
	dyn := func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
		return nil, nil, errors.New("must not be called")
	}

	got := Assess(context.Background(), fake.NewSimpleClientset(), dyn, Inputs{},
		Options{Capacity: true}, time.Now())

	if got.Capacity == nil {
		t.Fatal("Capacity report is nil; the section still runs from requests and limits without metrics")
	}
	if got.MetricsAvailable {
		t.Error("MetricsAvailable = true with no metrics API; a consumer would read the headroom as usage-backed")
	}
	if len(got.Degradations) != 0 {
		t.Errorf("Degradations = %v, want none — an absent metrics-server is normal, not an error",
			got.Degradations)
	}
}

var _ kubernetes.Interface = (*fake.Clientset)(nil)
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/advisory/
```

Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write the package**

Create `internal/advisory/advisory.go`:

```go
// Package advisory runs kubeagent's optional advisory sections — operator
// health, GitOps drift and capacity — and reports what it could not run.
//
// The sections are opt-in and each depends on API access the core scan does
// not need: CRDs for operators and drift, metrics-server for capacity. When
// that access is missing the section is skipped, and a skipped section that
// simply vanishes from the output is indistinguishable from a section that
// found nothing. Assess therefore returns a Degradation for every section it
// could not fully run, so the CLI can print a warning and the MCP server can
// put the same fact in its coverage block.
package advisory

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/capacity"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/gitops"
	"github.com/imantaba/kubeagent/internal/operators"
	"github.com/imantaba/kubeagent/internal/redact"
)

// Degradation records an advisory section that could not run, or could only
// run partially. Sections holds machine-readable section names; Subject is the
// phrase a human-facing warning uses; Reason is already redacted.
type Degradation struct {
	Sections []string
	Subject  string
	Reason   string
}

// Options selects which advisory sections to run.
type Options struct {
	Operators bool
	Drift     bool
	DriftAge  time.Duration
	Capacity  bool
	Namespace string
}

// Inputs carries the workload objects the scan already listed, so the advisory
// sections re-use them instead of listing again.
type Inputs struct {
	Deployments  []appsv1.Deployment
	StatefulSets []appsv1.StatefulSet
	DaemonSets   []appsv1.DaemonSet
	Jobs         []batchv1.Job
	CronJobs     []batchv1.CronJob
	ReplicaSets  []appsv1.ReplicaSet
	Nodes        []corev1.Node
	Pods         []corev1.Pod
}

// Result carries one pointer per section — nil means the section did not run —
// plus the reasons for anything missing.
type Result struct {
	Operators *operators.Report
	GitOps    *gitops.Report
	Capacity  *capacity.Report
	// MetricsAvailable reports whether metrics-server answered. The capacity
	// report is still produced without it, from requests and limits alone, so
	// this flag is the only way a consumer can tell a headroom figure backed
	// by real usage from one backed by declared requests.
	MetricsAvailable bool
	Degradations     []Degradation
}

// DynFactory builds the dynamic and discovery clients the CRD-reading sections
// need. It is a function rather than a pair of clients so that a scan with no
// advisory section enabled never builds them, and so tests can fail them.
type DynFactory func() (dynamic.Interface, discovery.DiscoveryInterface, error)

// FlagNames renders the enabled CRD-reading flags for a human-facing warning.
func FlagNames(operators, drift bool) string {
	switch {
	case operators && drift:
		return "--operators/--drift"
	case operators:
		return "--operators"
	default:
		return "--drift"
	}
}

// Assess runs the enabled advisory sections.
func Assess(ctx context.Context, client kubernetes.Interface, dyn DynFactory, in Inputs, opts Options, now time.Time) Result {
	var res Result

	if opts.Operators || opts.Drift {
		sections := []string{}
		if opts.Operators {
			sections = append(sections, "operators")
		}
		if opts.Drift {
			sections = append(sections, "drift")
		}
		dynClient, disco, err := dyn()
		if err != nil {
			res.Degradations = append(res.Degradations, Degradation{
				Sections: sections,
				Subject:  FlagNames(opts.Operators, opts.Drift),
				Reason:   redact.Error(err),
			})
		} else {
			adapters := gitops.Adapters()
			if opts.Operators {
				adapters = operators.Adapters()
			}
			fetched := collect.OperatorResources(ctx, disco, dynClient, adapters, opts.Namespace)
			if opts.Operators {
				rep := operators.Assess(fetched)
				res.Operators = &rep
			}
			if opts.Drift {
				rep := gitops.Assess(fetched, now, opts.DriftAge)
				res.GitOps = &rep
			}
		}
	}

	if opts.Capacity {
		// collect.PodMetrics reports an absent or forbidden metrics-server
		// through its second return value, not an error: that case is normal
		// and non-fatal. A non-nil error here means the response was
		// unparseable, which is worth warning about.
		podUsage, available, err := collect.PodMetrics(ctx, client)
		res.MetricsAvailable = available
		if err != nil {
			res.Degradations = append(res.Degradations, Degradation{
				Sections: []string{"capacity"},
				Subject:  "pod metrics",
				Reason:   redact.Error(err),
			})
		}
		templates := capacity.Templates(in.Deployments, in.StatefulSets, in.DaemonSets, in.Jobs, in.CronJobs)
		rep := capacity.Assess(in.Nodes, in.Pods, in.ReplicaSets, templates, podUsage, opts.Namespace)
		res.Capacity = &rep
	}

	return res
}

// ClusterPods returns the pods capacity headroom should be computed from. A
// namespaced scan still needs every pod in the cluster, because headroom is a
// node-level fact: computing it from one namespace's pods would report the
// other namespaces' consumption as free. When the cluster-wide list fails it
// falls back to the scoped pods and returns the error so the caller can say so.
func ClusterPods(ctx context.Context, client kubernetes.Interface, namespace string, scoped []corev1.Pod) ([]corev1.Pod, error) {
	if namespace == "" {
		return scoped, nil
	}
	all, err := collect.AllPods(ctx, client)
	if err != nil {
		return scoped, err
	}
	return all, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./internal/advisory/
```

Expected: PASS.

- [ ] **Step 5: Rewire `main.go`**

Replace `main.go:191-234` (the `operatorRep`/`gitopsRep`/`capacityRep` block) with:

```go
	advRes := advisory.Assess(context.Background(), client,
		func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
			return cluster.NewDynamicClients(*kubeconfig, *contextName)
		},
		advisory.Inputs{
			Deployments:  res.Inputs.Deployments,
			StatefulSets: res.Inputs.StatefulSets,
			DaemonSets:   res.Inputs.DaemonSets,
			Jobs:         res.Inputs.Jobs,
			CronJobs:     res.Inputs.CronJobs,
			ReplicaSets:  res.Inputs.ReplicaSets,
			Nodes:        nodes,
			Pods:         resourcePods,
		},
		advisory.Options{
			Operators: *operatorsFlag,
			Drift:     *driftFlag,
			DriftAge:  *driftAge,
			Capacity:  *capacityFlag,
			Namespace: namespace,
		}, time.Now())

	for _, d := range advRes.Degradations {
		fmt.Fprintf(os.Stderr, "kubeagent: warning: %s unavailable: %s\n", d.Subject, d.Reason)
	}

	operatorRep := advRes.Operators
	gitopsRep := advRes.GitOps
	capacityRep := advRes.Capacity
```

Two warning strings existed before this change and both must survive byte-identically apart from the redaction:

- the CRD one was `"kubeagent: warning: %s unavailable: %v\n"` with `enabledFlagNames(...)` and the error — now `%s` with `d.Reason`;
- the metrics one was `"kubeagent: warning: pod metrics unavailable: %v\n"` — now covered by `Subject == "pod metrics"`, producing the same sentence.

`resourcePods` must be computed *before* this block (it already is). Replace the `resolveResourcePods` call with:

```go
	resourcePods, podsErr := advisory.ClusterPods(context.Background(), client, namespace, scopedPods)
	if podsErr != nil {
		fmt.Fprintf(os.Stderr,
			"kubeagent: warning: cluster-wide pod list unavailable: %s; "+
				"capacity headroom and the resources summary will be computed from "+
				"namespace %q only, overstating free capacity across the whole cluster\n",
			redact.Error(podsErr), namespace)
	}
```

Then delete `enabledFlagNames` (`main.go:606-616`) and `resolveResourcePods` (`main.go:629-642`), and remove now-unused imports (`capacity`, `gitops`, `operators`, `collect` may still be used elsewhere — let the compiler decide).

Move the three `resolveResourcePods` tests out of `main_test.go:343-392`; the `ClusterPods` tests written in Step 1 already cover the same three cases, so delete the originals rather than duplicating them.

- [ ] **Step 6: Prove the CLI did not change**

```bash
go build ./... && go test ./...
git diff --stat internal/report/testdata/golden-scan.txt
```

Expected: all tests PASS, and the `git diff --stat` prints **nothing** — the golden fixture is untouched. If `TestGoldenScanOutput` fails, the refactor changed CLI output; fix the refactor, do not regenerate the golden file.

- [ ] **Step 7: Prove a default scan issues no extra API call**

The `TestAssess_NothingEnabledDoesNothing` test above is the guard: it fails if `Assess` builds dynamic clients when no section is enabled. Confirm it is still passing:

```bash
go test ./internal/advisory/ -run TestAssess_NothingEnabledDoesNothing -v
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/advisory main.go main_test.go
git commit -m "refactor(advisory): extract the optional sections and report structured degradations"
```

---

### Task 3: `scan.PartialReads` — make a denied list distinguishable from an empty one

**Files:**
- Modify: `internal/scan/scan.go` (add `ReadFailure`, add `Result.PartialReads`, record at every site that currently discards a collector error)
- Modify: `internal/scan/scan_test.go` (new tests)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type scan.ReadFailure struct { Resource string; Reason string }` — `Resource` is the plural lowercase resource name (`"pods"`, `"events"`, `"networkpolicies"`, …); `Reason` is `err.Error()`.
  - `scan.Result.PartialReads []ReadFailure` — appended in collector call order; nil when everything read cleanly.

**Why this task exists:** `scan.Evaluate` discards roughly eighteen collector errors with `x, _ := collect.X(...)`. That is defensible for a CLI that degrades gracefully, but it means an RBAC-denied list and a genuinely empty list produce identical output. The MCP honesty contract promises `coverage.partial` — which cannot be honest while the information is thrown away at the source. This change is additive: the CLI does not read the new field, so its output is unchanged.

- [ ] **Step 1: Write the failing test**

Append to `internal/scan/scan_test.go`:

```go
func TestEvaluate_RecordsDeniedListsAsPartialReads(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("networkpolicies is forbidden: User cannot list resource")
	})

	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil (a denied optional list must degrade, not fail)", err)
	}

	var found *ReadFailure
	for i := range res.PartialReads {
		if res.PartialReads[i].Resource == "networkpolicies" {
			found = &res.PartialReads[i]
		}
	}
	if found == nil {
		t.Fatalf("PartialReads = %v, want an entry for networkpolicies", res.PartialReads)
	}
	if found.Reason == "" {
		t.Error("PartialReads entry has an empty Reason; a caller cannot tell why the read failed")
	}
}

func TestEvaluate_CleanClusterHasNoPartialReads(t *testing.T) {
	client := fake.NewSimpleClientset()

	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	if len(res.PartialReads) != 0 {
		t.Errorf("PartialReads = %v, want none on a cluster that answered every list", res.PartialReads)
	}
}
```

Add whatever imports the file is missing (`errors`, `k8s.io/apimachinery/pkg/runtime`, `k8stesting "k8s.io/client-go/testing"`).

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/scan/ -run TestEvaluate_RecordsDeniedListsAsPartialReads
```

Expected: FAIL — `res.PartialReads undefined`.

- [ ] **Step 3: Add the type and the recording helper**

In `internal/scan/scan.go`, next to `Result`:

```go
// ReadFailure records a collector call that failed. A scan degrades rather
// than aborting when an optional list is denied, so without this record an
// RBAC-denied list and a genuinely empty one produce the same output.
type ReadFailure struct {
	Resource string
	Reason   string
}
```

Add the field to `Result` (keep it last so the existing field order is untouched):

```go
	// PartialReads names the collector calls that failed. Empty means every
	// list this scan attempted answered successfully.
	PartialReads []ReadFailure
```

Inside `Evaluate`, declare the recorder once, immediately after `res` is created:

```go
	note := func(resource string, err error) {
		if err != nil {
			res.PartialReads = append(res.PartialReads, ReadFailure{Resource: resource, Reason: err.Error()})
		}
	}
```

- [ ] **Step 4: Record at every discarding site**

Rewrite each `x, _ := collect.X(...)` in `Evaluate` to capture and note the error. There are roughly eighteen, currently at lines 154, 155, 189, 193, 194, 198, 225, 226, 228, 230, 234, 235, 242, 244, 245, 252, 260 and 274. Find them all:

```bash
grep -n ', _ := collect\.' internal/scan/scan.go
```

Each becomes the two-line form, with the resource name matching the Kubernetes plural for what the collector lists:

```go
	nps, npErr := collect.NetworkPolicies(ctx, client, namespace)
	note("networkpolicies", npErr)
```

Use these resource names: `pods`, `events`, `nodes`, `deployments`, `replicasets`, `statefulsets`, `daemonsets`, `jobs`, `cronjobs`, `services`, `endpointslices`, `ingresses`, `persistentvolumeclaims`, `poddisruptionbudgets`, `horizontalpodautoscalers`, `resourcequotas`, `networkpolicies`, `validatingwebhookconfigurations`, `secrets`, `configmaps`, `storageclasses`, `ingressclasses` — pick the one matching each call. Where two calls read the same resource with different selectors (the event collectors), use the same name for both; duplicates in `PartialReads` are correct and informative.

Do **not** change any control flow: every call still proceeds with whatever the collector returned, exactly as before.

- [ ] **Step 5: Run the tests**

```bash
go test ./internal/scan/ ./internal/report/
```

Expected: PASS, including `TestGoldenScanOutput` — the CLI reads none of this.

- [ ] **Step 6: Confirm the golden fixture is untouched**

```bash
git diff --stat internal/report/testdata/golden-scan.txt
```

Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add internal/scan
git commit -m "feat(scan): record failed collector reads so a denied list is distinguishable from an empty one"
```

---

### Task 4: `internal/mcp` foundations — config, coverage, and the view projections

No MCP SDK yet. This task is pure Go: the json-tagged types every tool result is built from, and the projections that fill them. Getting this right in isolation is what keeps the later tasks small.

**Files:**
- Create: `internal/mcp/config.go`
- Create: `internal/mcp/coverage.go`
- Create: `internal/mcp/coverage_test.go`
- Create: `internal/mcp/view.go`
- Create: `internal/mcp/view_test.go`
- Modify: `internal/inventory/inventory.go` (rename `humanAge` → `HumanAge`; 4 call sites)
- Modify: `internal/inventory/inventory_test.go:39-40` (two references)

**Interfaces:**
- Consumes: `scan.ReadFailure` (Task 3).
- Produces:
  - `type mcp.Config struct { Kubeconfig string; Context string; AllowContextSwitch bool; Logs bool }`
  - `type mcp.Coverage struct` with json tags `context`, `namespaceScope`, `collectedAt`, `checksRun`, `checksSkipped`, `partial`, `metricsServer`
  - `type mcp.SkippedCheck struct { Check string; Why string }` → `check`, `why`
  - `type mcp.PartialRead struct { Resource string; Why string }` → `resource`, `why`
  - `func newCoverage(contextName, namespace string, now time.Time) *Coverage`
  - `func (c *Coverage) markRun(checks ...string)`
  - `func (c *Coverage) markSkipped(check, why string)`
  - `func (c *Coverage) markPartial(reads []scan.ReadFailure)`
  - `type mcp.Finding struct` with json tags `severity`, `kind`, `namespace`, `name`, `reason`, `detail,omitempty`, `confidence,omitempty`, `remediationHint,omitempty`
  - `const mcp.MaxFindings = 50`
  - `func sortFindings(f []Finding)`
  - `func capFindings(f []Finding) ([]Finding, int)`
  - `func findingsFromResult(res scan.Result) []Finding`
  - `func inventory.HumanAge(t, now time.Time) string`

**The bug this task guards:** this project has twice shipped a json tag on the wrong struct — a tag added to `report.Input` when `inventoryReport` is what `encoding/json` actually marshals. The MCP analogue is projecting straight from `scan.Result`, whose 20 fields carry **no** json tags at all: marshalling it would emit Go field names and every unexported detail the caller must not see. Every value here is copied field by field into a struct declared in this file. Never marshal a type from `internal/scan`.

- [ ] **Step 1: Export `inventory.HumanAge`**

`humanAge` is at `internal/inventory/inventory.go:88`. Rename the declaration to `HumanAge` and give it a doc comment:

```go
// HumanAge renders the gap between t and now as a compact duration such as
// "3d", "5h" or "12m". A negative gap (a clock skew, or an object stamped in
// the future) reads as zero rather than a negative string.
func HumanAge(t, now time.Time) string {
```

Update all callers:

```bash
export PATH=$PATH:/usr/local/go/bin
grep -rn 'humanAge(' --include='*.go' internal/
```

Expected before: 4 matches (`inventory.go:85`, `inventory.go:88`, `inventory.go:338`, and `inventory_test.go:39-40`). Rewrite each to `HumanAge(`. Then:

```bash
grep -rn 'humanAge(' --include='*.go' internal/ ; go test ./internal/inventory/
```

Expected: no matches, tests PASS.

- [ ] **Step 2: Write the failing coverage test**

Create `internal/mcp/coverage_test.go`:

```go
package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/scan"
)

var fixedNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

func TestNewCoverage_EmptySlicesMarshalAsArraysNotNull(t *testing.T) {
	cov := newCoverage("kind-example", "", fixedNow)

	blob, err := json.Marshal(cov)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{"checksRun", "checksSkipped", "partial"} {
		v, ok := got[key]
		if !ok {
			t.Errorf("coverage is missing %q; a caller cannot tell an empty list from an absent one", key)
			continue
		}
		if _, isSlice := v.([]any); !isSlice {
			t.Errorf("coverage.%s = %v (%T), want a JSON array", key, v, v)
		}
	}
	if got["namespaceScope"] != "all namespaces" {
		t.Errorf("namespaceScope = %v, want %q", got["namespaceScope"], "all namespaces")
	}
	if got["collectedAt"] != "2026-07-27T12:00:00Z" {
		t.Errorf("collectedAt = %v, want RFC3339 UTC", got["collectedAt"])
	}
	if got["metricsServer"] != "not-checked" {
		t.Errorf("metricsServer = %v, want %q — a check that never ran must not claim absence",
			got["metricsServer"], "not-checked")
	}
}

func TestCoverage_NamespaceScopeNamesTheNamespace(t *testing.T) {
	cov := newCoverage("kind-example", "payments", fixedNow)
	if cov.NamespaceScope != "payments" {
		t.Errorf("NamespaceScope = %q, want %q", cov.NamespaceScope, "payments")
	}
}

func TestCoverage_MarkPartialCarriesResourceAndReason(t *testing.T) {
	cov := newCoverage("kind-example", "", fixedNow)
	cov.markPartial([]scan.ReadFailure{{Resource: "networkpolicies", Reason: "forbidden"}})

	if len(cov.Partial) != 1 {
		t.Fatalf("Partial = %v, want one entry", cov.Partial)
	}
	if cov.Partial[0].Resource != "networkpolicies" || cov.Partial[0].Why != "forbidden" {
		t.Errorf("Partial[0] = %+v, want {networkpolicies forbidden}", cov.Partial[0])
	}
}

func TestCoverage_MarkRunAndMarkSkippedAccumulate(t *testing.T) {
	cov := newCoverage("kind-example", "", fixedNow)
	cov.markRun("workloads", "services")
	cov.markSkipped("logs", "not requested")

	if len(cov.ChecksRun) != 2 || cov.ChecksRun[0] != "workloads" {
		t.Errorf("ChecksRun = %v, want [workloads services]", cov.ChecksRun)
	}
	if len(cov.ChecksSkipped) != 1 || cov.ChecksSkipped[0].Check != "logs" || cov.ChecksSkipped[0].Why != "not requested" {
		t.Errorf("ChecksSkipped = %v, want one {logs, not requested}", cov.ChecksSkipped)
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

```bash
go test ./internal/mcp/
```

Expected: FAIL — the package does not exist.

- [ ] **Step 4: Write `config.go` and `coverage.go`**

Create `internal/mcp/config.go`:

```go
// Package mcp serves kubeagent's read-only diagnosis over the Model Context
// Protocol, so another agent can call it as a deterministic tool.
//
// The server owns no cluster logic: every tool runs the same scan pipeline the
// CLI runs and projects the result into this package's json-tagged view types.
// It is strictly read-only toward the cluster (get/list/watch only), it never
// calls an LLM, and no code path in it can reach the --fix remediation writer.
package mcp

// Config is everything the server needs to talk to a cluster. It carries no
// credential of its own: the kubeconfig path is resolved by internal/cluster
// and is never echoed back to a caller, because a path can name a customer, a
// cluster and an environment.
type Config struct {
	// Kubeconfig is an explicit kubeconfig path, or "" for the usual resolution.
	Kubeconfig string
	// Context is the kubeconfig context to use, or "" for the current context.
	Context string
	// AllowContextSwitch registers list_contexts and lets a tool call name a
	// different context. Off by default: a server started against one cluster
	// should not be talked into another one.
	AllowContextSwitch bool
	// Logs enables the log-tail enrichment that scan --logs performs.
	Logs bool
}
```

Create `internal/mcp/coverage.go`:

```go
package mcp

import (
	"time"

	"github.com/imantaba/kubeagent/internal/scan"
)

// SkippedCheck names a check that did not run, and why.
type SkippedCheck struct {
	Check string `json:"check"`
	Why   string `json:"why"`
}

// PartialRead names a resource kubeagent tried and failed to list.
type PartialRead struct {
	Resource string `json:"resource"`
	Why      string `json:"why"`
}

// Coverage is the honesty contract every tool result carries. A model reading
// JSON treats an absent key as zero, so "no findings" and "never looked" must
// not produce the same payload: ChecksRun says what was examined,
// ChecksSkipped says what was not and why, and Partial says which lists came
// back incomplete.
type Coverage struct {
	Context        string         `json:"context"`
	NamespaceScope string         `json:"namespaceScope"`
	CollectedAt    string         `json:"collectedAt"`
	ChecksRun      []string       `json:"checksRun"`
	ChecksSkipped  []SkippedCheck `json:"checksSkipped"`
	Partial        []PartialRead  `json:"partial"`
	// MetricsServer is "available", "absent" or "not-checked". A tool that
	// never queries metrics reports "not-checked" rather than "absent", which
	// would assert a fact nothing tested.
	MetricsServer string `json:"metricsServer"`
}

// newCoverage starts a coverage block with non-nil slices, so an empty list
// marshals as [] rather than null.
func newCoverage(contextName, namespace string, now time.Time) *Coverage {
	scope := namespace
	if scope == "" {
		scope = "all namespaces"
	}
	return &Coverage{
		Context:        contextName,
		NamespaceScope: scope,
		CollectedAt:    now.UTC().Format(time.RFC3339),
		ChecksRun:      []string{},
		ChecksSkipped:  []SkippedCheck{},
		Partial:        []PartialRead{},
		MetricsServer:  "not-checked",
	}
}

func (c *Coverage) markRun(checks ...string) {
	c.ChecksRun = append(c.ChecksRun, checks...)
}

func (c *Coverage) markSkipped(check, why string) {
	c.ChecksSkipped = append(c.ChecksSkipped, SkippedCheck{Check: check, Why: why})
}

// markPartial copies the scan's failed reads into the coverage block.
func (c *Coverage) markPartial(reads []scan.ReadFailure) {
	for _, r := range reads {
		c.Partial = append(c.Partial, PartialRead{Resource: r.Resource, Why: r.Reason})
	}
}
```

- [ ] **Step 5: Run the coverage tests**

```bash
go test ./internal/mcp/
```

Expected: PASS (4 tests).

- [ ] **Step 6: Write the failing view test**

Create `internal/mcp/view_test.go`:

```go
package mcp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/pvchealth"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

func TestFindingsFromResult_DetectorFindingIsCriticalWithARemediationHint(t *testing.T) {
	res := scan.Result{
		Inventory: inventory.Result{
			Workloads: []inventory.Workload{{
				Namespace: "payments", Name: "api", Kind: "Deployment", Desired: 3, Ready: 0, Status: "Degraded",
				Findings: []diagnose.Finding{{
					Pod: "payments/api-abc", Issue: "CrashLoopBackOff",
					Reason: "container exits immediately", Evidence: "restart count 7",
					Container: "api", Confidence: "high",
				}},
			}},
		},
	}

	got := findingsFromResult(res)

	if len(got) != 1 {
		t.Fatalf("findingsFromResult() = %d findings, want 1", len(got))
	}
	f := got[0]
	if f.Severity != "critical" {
		t.Errorf("Severity = %q, want %q", f.Severity, "critical")
	}
	if f.Kind != "Pod" || f.Namespace != "payments" || f.Name != "api-abc" {
		t.Errorf("got %s %s/%s, want Pod payments/api-abc", f.Kind, f.Namespace, f.Name)
	}
	if f.Reason != "CrashLoopBackOff" {
		t.Errorf("Reason = %q, want the issue name", f.Reason)
	}
	if f.Confidence != "high" {
		t.Errorf("Confidence = %q, want %q", f.Confidence, "high")
	}
	if f.RemediationHint == "" {
		t.Error("RemediationHint is empty; every detector finding has a deterministic next step")
	}
}

func TestFindingsFromResult_DegradedWorkloadWithoutADetectorFindingStillReports(t *testing.T) {
	res := scan.Result{
		Inventory: inventory.Result{
			Workloads: []inventory.Workload{{
				Namespace: "payments", Name: "worker", Kind: "Deployment",
				Desired: 3, Ready: 1, Status: "Degraded",
			}},
		},
	}

	got := findingsFromResult(res)

	if len(got) != 1 {
		t.Fatalf("findingsFromResult() = %d findings, want 1 workload-level finding", len(got))
	}
	if got[0].Severity != "warning" || got[0].Kind != "Deployment" || got[0].Name != "worker" {
		t.Errorf("got %+v, want a warning on Deployment worker", got[0])
	}
	if got[0].Detail != "1/3 ready" {
		t.Errorf("Detail = %q, want %q", got[0].Detail, "1/3 ready")
	}
}

func TestFindingsFromResult_CoversEveryAttentionClassNotJustWorkloads(t *testing.T) {
	res := scan.Result{
		ServiceIssues: []svchealth.Issue{{
			Namespace: "payments", Name: "api", Type: "ClusterIP",
			Problem: "no endpoints", Detail: "selector matches no pods",
		}},
		PVCIssues: []pvchealth.Issue{{
			Namespace: "payments", Name: "data", Phase: "Pending", Reason: "no provisioner",
		}},
	}

	got := findingsFromResult(res)

	kinds := map[string]bool{}
	for _, f := range got {
		kinds[f.Kind] = true
	}
	if !kinds["Service"] || !kinds["PersistentVolumeClaim"] {
		t.Errorf("kinds = %v, want both Service and PersistentVolumeClaim; a triage payload that "+
			"reports only workloads silently drops every other class the CLI treats as degrading", kinds)
	}
}

func TestFindingsFromResult_ExpectedServiceIssuesAreNotFindings(t *testing.T) {
	res := scan.Result{
		ServiceIssues: []svchealth.Issue{{
			Namespace: "kube-system", Name: "headless", Problem: "no endpoints", Expected: true,
		}},
	}

	if got := findingsFromResult(res); len(got) != 0 {
		t.Errorf("findingsFromResult() = %v, want none — the CLI does not treat expected issues as attention", got)
	}
}

func TestSortFindings_TotalOrderIsDeterministic(t *testing.T) {
	in := []Finding{
		{Severity: "warning", Kind: "Service", Namespace: "b", Name: "two", Reason: "x"},
		{Severity: "critical", Kind: "Pod", Namespace: "b", Name: "one", Reason: "x"},
		{Severity: "warning", Kind: "Service", Namespace: "a", Name: "three", Reason: "x"},
		{Severity: "warning", Kind: "Ingress", Namespace: "a", Name: "three", Reason: "x"},
	}
	sortFindings(in)

	want := []string{"critical/b/one/Pod", "warning/a/three/Ingress", "warning/a/three/Service", "warning/b/two/Service"}
	for i, w := range want {
		got := fmt.Sprintf("%s/%s/%s/%s", in[i].Severity, in[i].Namespace, in[i].Name, in[i].Kind)
		if got != w {
			t.Errorf("position %d = %q, want %q", i, got, w)
		}
	}
}

func TestCapFindings_TruncatesAndReportsHowMany(t *testing.T) {
	in := make([]Finding, MaxFindings+7)
	for i := range in {
		in[i] = Finding{Severity: "warning", Kind: "Pod", Namespace: "n", Name: fmt.Sprintf("p%03d", i)}
	}

	got, omitted := capFindings(in)

	if len(got) != MaxFindings {
		t.Errorf("len = %d, want %d", len(got), MaxFindings)
	}
	if omitted != 7 {
		t.Errorf("omitted = %d, want 7", omitted)
	}
}

func TestFinding_JSONKeysAreTheDocumentedOnes(t *testing.T) {
	blob, err := json.Marshal(Finding{Severity: "critical", Kind: "Pod", Namespace: "n", Name: "p", Reason: "r"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"severity":"critical","kind":"Pod","namespace":"n","name":"p","reason":"r"}`
	if string(blob) != want {
		t.Errorf("Marshal() = %s, want %s", blob, want)
	}
}
```

- [ ] **Step 7: Run it to verify it fails**

```bash
go test ./internal/mcp/ -run TestFindings
```

Expected: FAIL — `undefined: findingsFromResult`.

- [ ] **Step 8: Write `view.go`**

Create `internal/mcp/view.go`. `splitNamespacedName` handles `diagnose.Finding.Pod`, which is the string `"namespace/name"`:

```go
package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/remediation"
	"github.com/imantaba/kubeagent/internal/scan"
)

// MaxFindings caps a tool result. A model pays for every token it reads, and a
// thousand-finding payload from a broken cluster is worse than fifty findings
// plus an honest count of what was dropped.
const MaxFindings = 50

// Finding is one problem, flattened for a model to read. It is deliberately a
// separate type from diagnose.Finding and from every *health.Issue: those are
// internal shapes that change with the detectors, while this one is the
// published contract.
type Finding struct {
	// Severity is "critical" (a detector matched a concrete failure mode) or
	// "warning" (a health check flagged something that needs a human).
	Severity        string `json:"severity"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	Reason          string `json:"reason"`
	Detail          string `json:"detail,omitempty"`
	Confidence      string `json:"confidence,omitempty"`
	RemediationHint string `json:"remediationHint,omitempty"`
}

func splitNamespacedName(s string) (namespace, name string) {
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

func fromDiagnose(f diagnose.Finding) Finding {
	ns, name := splitNamespacedName(f.Pod)
	detail := f.Reason
	if f.Evidence != "" {
		detail = strings.TrimSpace(detail + " (" + f.Evidence + ")")
	}
	return Finding{
		Severity:        "critical",
		Kind:            "Pod",
		Namespace:       ns,
		Name:            name,
		Reason:          f.Issue,
		Detail:          detail,
		Confidence:      f.Confidence,
		RemediationHint: remediation.For(f).NextStep,
	}
}

func fromWorkload(w inventory.Workload) Finding {
	return Finding{
		Severity:  "warning",
		Kind:      w.Kind,
		Namespace: w.Namespace,
		Name:      w.Name,
		Reason:    w.Status,
		Detail:    fmt.Sprintf("%d/%d ready", w.Ready, w.Desired),
	}
}

// findingsFromResult projects every attention-worthy class scan.Result carries
// into one flat list. The classes mirror the text report's hasAttention
// expression: leaving one out would make a triage result say "healthy" about a
// cluster the CLI calls degraded. Two of hasAttention's inputs — credential
// lint and disk usage — are computed by the CLI outside scan.Evaluate; the
// triage handler declares those as skipped checks rather than pretending they
// were clean.
func findingsFromResult(res scan.Result) []Finding {
	out := []Finding{}

	for _, w := range res.Inventory.Workloads {
		if len(w.Findings) == 0 {
			out = append(out, fromWorkload(w))
			continue
		}
		for _, f := range w.Findings {
			out = append(out, fromDiagnose(f))
		}
	}

	for _, i := range res.ServiceIssues {
		if i.Expected {
			continue
		}
		out = append(out, Finding{Severity: "warning", Kind: "Service", Namespace: i.Namespace,
			Name: i.Name, Reason: i.Problem, Detail: i.Detail})
	}
	for _, i := range res.IngressIssues {
		if i.Expected {
			continue
		}
		out = append(out, Finding{Severity: "warning", Kind: "Ingress", Namespace: i.Namespace,
			Name: i.Ingress, Reason: i.Problem, Detail: i.Detail})
	}
	for _, i := range res.PVCIssues {
		out = append(out, Finding{Severity: "warning", Kind: "PersistentVolumeClaim", Namespace: i.Namespace,
			Name: i.Name, Reason: i.Reason, Detail: i.Detail})
	}
	for _, i := range res.StuckTerminating {
		out = append(out, Finding{Severity: "warning", Kind: i.Kind, Namespace: i.Namespace,
			Name: i.Name, Reason: "stuck terminating", Detail: i.Reason})
	}
	for _, i := range res.PDBIssues {
		out = append(out, Finding{Severity: "warning", Kind: "PodDisruptionBudget", Namespace: i.Namespace,
			Name: i.Name, Reason: i.Category, Detail: i.Reason})
	}
	for _, i := range res.HPAIssues {
		out = append(out, Finding{Severity: "warning", Kind: "HorizontalPodAutoscaler", Namespace: i.Namespace,
			Name: i.Name, Reason: i.Category, Detail: i.Reason})
	}
	for _, i := range res.WebhookIssues {
		out = append(out, Finding{Severity: "warning", Kind: i.Kind, Namespace: "",
			Name: i.Config, Reason: i.Problem, Detail: i.Reason})
	}
	for _, i := range res.QuotaIssues {
		out = append(out, Finding{Severity: "warning", Kind: "ResourceQuota", Namespace: i.Namespace,
			Name: i.Quota, Reason: i.Severity, Detail: fmt.Sprintf("%s %s/%s used", i.Resource, i.Used, i.Hard)})
	}
	sortFindings(out)
	return out
}

// severityRank orders severities without depending on their spelling sorting
// the right way (alphabetically "critical" < "warning" is a coincidence).
func severityRank(s string) int {
	if s == "critical" {
		return 0
	}
	return 1
}

// sortFindings imposes a total order, so two scans of an unchanged cluster
// produce byte-identical payloads and a caller can diff them.
func sortFindings(f []Finding) {
	sort.SliceStable(f, func(a, b int) bool {
		x, y := f[a], f[b]
		if ra, rb := severityRank(x.Severity), severityRank(y.Severity); ra != rb {
			return ra < rb
		}
		if x.Namespace != y.Namespace {
			return x.Namespace < y.Namespace
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Reason < y.Reason
	})
}

// capFindings truncates to MaxFindings and reports how many were dropped.
func capFindings(f []Finding) ([]Finding, int) {
	if len(f) <= MaxFindings {
		return f, 0
	}
	return f[:MaxFindings], len(f) - MaxFindings
}
```

**If a field name in this file does not compile**, read the issue type and use its real field — do not invent one. The issue types live in `internal/svchealth`, `internal/ingresshealth`, `internal/pvchealth`, `internal/termhealth`, `internal/pdbhealth`, `internal/hpahealth`, `internal/webhookhealth`, `internal/quotahealth`, and the credential warning type is on `scan.Result.CredentialWarnings`. Report any field you had to change in your task report so later tasks use the same names.

- [ ] **Step 9: Run the view tests**

```bash
go test ./internal/mcp/ -v
```

Expected: PASS, all tests in both files.

- [ ] **Step 10: Commit**

```bash
git add internal/mcp internal/inventory
git commit -m "feat(mcp): coverage contract and json-tagged finding projections"
```

---

### Task 5: the SDK, the server skeleton, and `kubeagent_triage`

The first vertical slice: a real MCP server, one tool, driven end to end in a test over an in-memory transport.

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `internal/mcp/server.go`
- Create: `internal/mcp/server_test.go`
- Create: `internal/mcp/triage.go`
- Create: `internal/mcp/triage_test.go`

**Interfaces:**
- Consumes: `Config`, `Coverage`, `newCoverage`, `Finding`, `findingsFromResult`, `capFindings`, `MaxFindings` (Task 4); `scan.Result.PartialReads` (Task 3).
- Produces:
  - `func mcp.Serve(ctx context.Context, cfg Config, version string) error` — connects, validates, serves over stdio until the client disconnects.
  - `func newServer(cfg Config, version string, client kubernetes.Interface, now func() time.Time) *mcpsdk.Server` — the testable constructor; takes an already-built clientset.
  - `type mcp.Cluster struct { Context string; Version string; Nodes int }` → `context`, `version`, `nodes`
  - `type mcp.TriageInput struct { Namespace string; Context string }` → `namespace,omitempty`, `context,omitempty`
  - `type mcp.TriageOutput struct { Verdict string; Cluster Cluster; Findings []Finding; FindingsOmitted int; Coverage *Coverage }` → `verdict`, `cluster`, `findings`, `findingsOmitted,omitempty`, `coverage`
  - `type mcp.toolHandler[In, Out any] func(context.Context, *mcpsdk.CallToolRequest, In) (*mcpsdk.CallToolResult, Out, error)`
  - `func guard[In, Out any](name string, h toolHandler[In, Out]) toolHandler[In, Out]` — panic-recovering wrapper
  - `func contextLabel(name string) string`

**Import naming:** this package is called `mcp` and so is the SDK's. Alias the SDK import in every file that needs it:

```go
mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
```

- [ ] **Step 1: Add the dependency**

```bash
export PATH=$PATH:/usr/local/go/bin
go get github.com/modelcontextprotocol/go-sdk@v1.6.1
go mod tidy
git diff go.mod
```

Expected: `github.com/modelcontextprotocol/go-sdk v1.6.1` as a direct require, plus these indirect additions — `github.com/google/jsonschema-go`, `github.com/segmentio/encoding`, `github.com/segmentio/asm`, `github.com/yosida95/uritemplate/v3` — and bumps of `golang.org/x/oauth2` (0.34.0 → 0.35.0) and `golang.org/x/sys` (0.40.0 → 0.41.0). Nothing else. If `go mod tidy` pulls anything beyond that list, stop and report it — the spec's dependency budget was measured, not estimated.

- [ ] **Step 2: Write the failing server test**

Create `internal/mcp/server_test.go`:

```go
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
```

- [ ] **Step 3: Run it to verify it fails**

```bash
go test ./internal/mcp/ -run TestServer
```

Expected: FAIL — `undefined: newServer`.

- [ ] **Step 4: Write `server.go`**

Create `internal/mcp/server.go`:

```go
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

// toolHandler is the shape the SDK's AddTool expects.
type toolHandler[In, Out any] func(context.Context, *mcpsdk.CallToolRequest, In) (*mcpsdk.CallToolResult, Out, error)

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
				// The panic value can carry anything, including a URL from a
				// client library, so it is redacted like any other error.
				err = fmt.Errorf("%s failed unexpectedly: %s", name, redact.Error(fmt.Errorf("%v", r)))
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
```

`registerInspect` and `registerAdvisory` do not exist yet. Add these two temporary no-op stubs at the bottom of `server.go` so the package compiles; Tasks 6 and 7 replace them with the real registrations in their own files:

```go
// Replaced in the tasks that implement these tools.
func registerInspect(*mcpsdk.Server, Config, kubernetes.Interface, func() time.Time)  {}
func registerAdvisory(*mcpsdk.Server, Config, kubernetes.Interface, func() time.Time) {}
```

Because of those stubs, `TestServer_ExposesExactlyTheReadOnlyTools` will report only `kubeagent_triage` at the end of this task. **Change the test's `want` to `[]string{"kubeagent_triage"}` now, and restore the other two names in Tasks 6 and 7** as each tool lands. Do not delete the assertion.

- [ ] **Step 5: Write the failing triage test**

Create `internal/mcp/triage_test.go`:

```go
package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func callTriage(t *testing.T, cs *mcpsdk.ClientSession, args map[string]any) TriageOutput {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "kubeagent_triage", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	var out TriageOutput
	blob, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("Marshal(structured) error = %v", err)
	}
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return out
}

func crashingPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "api-abc"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "api",
				RestartCount: 7,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff", Message: "back-off restarting failed container",
				}},
			}},
		},
	}
}

func TestTriage_HealthyClusterIsExplicitlyHealthy(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callTriage(t, cs, map[string]any{})

	if out.Verdict != "healthy" {
		t.Errorf("Verdict = %q, want %q", out.Verdict, "healthy")
	}
	if out.Findings == nil {
		t.Error("Findings is null; an empty finding list must marshal as [] so a caller can tell it apart from absent")
	}
	if out.Coverage == nil {
		t.Fatal("Coverage is nil; every result carries the honesty contract")
	}
	if out.Coverage.Context != "kind-example" {
		t.Errorf("coverage.context = %q, want %q", out.Coverage.Context, "kind-example")
	}
	if len(out.Coverage.ChecksRun) == 0 {
		t.Error("coverage.checksRun is empty; a healthy verdict with no declared checks is unfalsifiable")
	}
	if out.Coverage.MetricsServer != "not-checked" {
		t.Errorf("coverage.metricsServer = %q, want %q — triage never queries metrics",
			out.Coverage.MetricsServer, "not-checked")
	}
}

func TestTriage_CrashLoopIsACriticalFinding(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(crashingPod()))

	out := callTriage(t, cs, map[string]any{})

	if out.Verdict != "degraded" {
		t.Fatalf("Verdict = %q, want %q", out.Verdict, "degraded")
	}
	if len(out.Findings) == 0 {
		t.Fatal("Findings is empty on a crash-looping pod")
	}
	f := out.Findings[0]
	if f.Severity != "critical" || f.Reason != "CrashLoopBackOff" {
		t.Errorf("Findings[0] = %+v, want a critical CrashLoopBackOff", f)
	}
	if f.RemediationHint == "" {
		t.Error("RemediationHint is empty; the caller gets the deterministic next step, not an invented one")
	}
}

func TestTriage_NamespaceArgumentIsReflectedInCoverage(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callTriage(t, cs, map[string]any{"namespace": "payments"})

	if out.Coverage.NamespaceScope != "payments" {
		t.Errorf("coverage.namespaceScope = %q, want %q", out.Coverage.NamespaceScope, "payments")
	}
}

func TestTriage_ContextArgumentIsRejectedUnlessSwitchingIsAllowed(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_triage", Arguments: map[string]any{"context": "kind-other"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded; a server started without --allow-context-switch must not be " +
			"talked into another cluster")
	}
}

func TestTriage_SkippedChecksAreDeclaredNotSilent(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callTriage(t, cs, map[string]any{})

	declared := map[string]bool{}
	for _, s := range out.Coverage.ChecksSkipped {
		declared[s.Check] = true
		if s.Why == "" {
			t.Errorf("checksSkipped entry %q has an empty reason", s.Check)
		}
	}
	for _, want := range []string{"credential-lint", "disk-usage", "security", "certificates"} {
		if !declared[want] {
			t.Errorf("coverage.checksSkipped does not mention %q; the CLI reports it and triage does not, "+
				"so its absence must be stated rather than implied clean", want)
		}
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

```bash
go test ./internal/mcp/ -run TestTriage
```

Expected: FAIL — `undefined: TriageOutput`.

- [ ] **Step 7: Write `triage.go`**

Create `internal/mcp/triage.go`:

```go
package mcp

import (
	"context"
	"errors"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/scan"
)

// Cluster identifies what was looked at.
type Cluster struct {
	Context string `json:"context"`
	Version string `json:"version"`
	Nodes   int    `json:"nodes"`
}

// TriageInput is the kubeagent_triage argument object.
type TriageInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"limit the scan to one namespace; omit to scan the whole cluster"`
	Context   string `json:"context,omitempty" jsonschema:"kubeconfig context to use; only accepted when the server was started with --allow-context-switch"`
}

// TriageOutput is the kubeagent_triage result.
type TriageOutput struct {
	// Verdict is "healthy" or "degraded".
	Verdict         string    `json:"verdict"`
	Cluster         Cluster   `json:"cluster"`
	Findings        []Finding `json:"findings"`
	FindingsOmitted int       `json:"findingsOmitted,omitempty"`
	Coverage        *Coverage `json:"coverage"`
}

// errContextSwitchDisabled is returned verbatim to the caller, so it explains
// the fix without naming a kubeconfig path or a server address.
var errContextSwitchDisabled = errors.New(
	"this server was started without --allow-context-switch, so it only answers for the cluster it was started against")

func registerTriage(s *mcpsdk.Server, cfg Config, client kubernetes.Interface, now func() time.Time) {
	tool := &mcpsdk.Tool{
		Name: "kubeagent_triage",
		Description: "Run kubeagent's deterministic read-only diagnosis over a cluster or one namespace and " +
			"return a verdict, the findings that support it, and a coverage block stating what was and was " +
			"not examined. Read-only: this never changes cluster state.",
	}
	mcpsdk.AddTool(s, tool, guard("kubeagent_triage",
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in TriageInput) (*mcpsdk.CallToolResult, TriageOutput, error) {
			if in.Context != "" && !cfg.AllowContextSwitch {
				return nil, TriageOutput{}, errContextSwitchDisabled
			}

			res, err := scan.Evaluate(ctx, client, scan.Options{
				Namespace:       in.Namespace,
				IncludeCron:     true,
				IncludeRestarts: true,
				Logs:            cfg.Logs,
			})
			if err != nil {
				return nil, TriageOutput{}, errors.New("scanning the cluster: " + redact.Error(err))
			}

			cov := newCoverage(contextLabel(cfg.Context), in.Namespace, now())
			cov.markRun("workloads", "pod-diagnosis", "services", "ingresses", "persistentvolumeclaims",
				"terminating", "poddisruptionbudgets", "horizontalpodautoscalers", "webhooks", "resourcequotas")
			cov.markSkipped("credential-lint", "not run by triage; use the kubeagent CLI")
			cov.markSkipped("disk-usage", "not run by triage; it needs node stats the server does not request")
			cov.markSkipped("security", "not run by triage; call kubeagent_advisory with section \"security\"")
			cov.markSkipped("certificates", "not run by triage; call kubeagent_advisory with section \"certificates\"")
			cov.markPartial(res.PartialReads)
			if cfg.Logs {
				cov.markRun("log-tails")
			} else {
				cov.markSkipped("log-tails", "the server was started without --logs")
			}

			findings, omitted := capFindings(findingsFromResult(res))
			verdict := "healthy"
			if len(findings) > 0 {
				verdict = "degraded"
			}

			return nil, TriageOutput{
				Verdict: verdict,
				Cluster: Cluster{
					Context: contextLabel(cfg.Context),
					Version: platform.Detect(res.Nodes, nil, nil, nil).KubeVersion,
					Nodes:   len(res.Nodes),
				},
				Findings:        findings,
				FindingsOmitted: omitted,
				Coverage:        cov,
			}, nil
		}))
}
```

Note on `Cluster.Version`: `platform.Detect` derives the version from the node objects the scan already listed, so this costs no extra API call. It reports major.minor (`"v1.34"`), not the full patch version — that is the honest thing the node objects carry.

- [ ] **Step 8: Run the tests**

```bash
go test ./internal/mcp/ -v
```

Expected: PASS. If `TestTriage_HealthyClusterIsExplicitlyHealthy` fails on `Findings is null`, `findingsFromResult` returned a nil slice — it must return `[]Finding{}` (Task 4 initialises it that way).

- [ ] **Step 9: Prove the read-only invariant structurally**

```bash
go list -deps ./internal/mcp | grep -E 'kubeagent/internal/(remediate|explain)'
```

Expected: no output. If either package appears, an import crossed a hard line — remove it.

- [ ] **Step 10: Commit**

```bash
git add go.mod go.sum internal/mcp
git commit -m "feat(mcp): serve kubeagent_triage over the Model Context Protocol"
```

---

### Task 6: `kubeagent_inspect` and `collect.ObjectEvents`

Triage answers "what is broken?". Inspect answers "tell me about this one thing" — the drill-down a model reaches for after triage names something.

**Files:**
- Create: `internal/mcp/inspect.go`
- Create: `internal/mcp/inspect_test.go`
- Modify: `internal/mcp/server.go` (delete the `registerInspect` stub)
- Modify: `internal/mcp/server_test.go` (add `kubeagent_inspect` back to the expected tool list)
- Modify: `internal/collect/collect.go` (add `ObjectEvents`)
- Modify: `internal/collect/collect_test.go` (add its tests)

**Interfaces:**
- Consumes: `newServer`, `guard`, `contextLabel`, `errContextSwitchDisabled` (Task 5); `Finding`, `Coverage` (Task 4); `inventory.HumanAge` (Task 4).
- Produces:
  - `func collect.ObjectEvents(ctx context.Context, client kubernetes.Interface, namespace, name string) ([]corev1.Event, error)`
  - `type mcp.Event struct { Type, Reason, Message string; Count int32; Age string }` → `type`, `reason`, `message`, `count`, `age`
  - `type mcp.InspectInput struct { Kind, Namespace, Name, Context string }`
  - `type mcp.InspectOutput struct { Found bool; Kind, Namespace, Name, Status string; Desired, Ready int; Image string; Pods []inventory.PodRow; Findings []Finding; Events []Event; Coverage *Coverage }`
  - `func registerInspect(s *mcpsdk.Server, cfg Config, client kubernetes.Interface, now func() time.Time)`

**Why `inventory.PodRow` is reused rather than re-projected:** `PodRow` is already part of kubeagent's published JSON contract — it is what `scan --output json` emits — and it is fully json-tagged. Re-declaring it here would create two shapes that must be kept in sync. `scan.Result` is the type that must never be marshalled, because it has no tags at all.

- [ ] **Step 1: Write the failing collector test**

Append to `internal/collect/collect_test.go`:

```go
func TestObjectEvents_ReturnsOnlyTheNamedObjectsEvents(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "payments", Name: "e1"},
			InvolvedObject: corev1.ObjectReference{Namespace: "payments", Name: "api-abc"},
			Reason:         "BackOff", Message: "back-off restarting failed container", Type: "Warning", Count: 5,
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "payments", Name: "e2"},
			InvolvedObject: corev1.ObjectReference{Namespace: "payments", Name: "other-pod"},
			Reason:         "Pulled", Message: "image pulled", Type: "Normal", Count: 1,
		},
	)

	got, err := ObjectEvents(context.Background(), client, "payments", "api-abc")
	if err != nil {
		t.Fatalf("ObjectEvents() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ObjectEvents() returned %d events, want 1 — the fake clientset ignores field "+
			"selectors, so the filter must also be applied client-side", len(got))
	}
	if got[0].Reason != "BackOff" {
		t.Errorf("Reason = %q, want %q", got[0].Reason, "BackOff")
	}
}

func TestObjectEvents_NoEventsIsNotAnError(t *testing.T) {
	got, err := ObjectEvents(context.Background(), fake.NewSimpleClientset(), "payments", "api-abc")
	if err != nil {
		t.Fatalf("ObjectEvents() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("ObjectEvents() = %v, want none", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -run TestObjectEvents
```

Expected: FAIL — `undefined: ObjectEvents`.

- [ ] **Step 3: Implement `ObjectEvents`**

Add to `internal/collect/collect.go`, alongside the other event collectors:

```go
// ObjectEvents lists the events attached to one object. The field selector is
// what a real API server uses to do the filtering server-side; the loop repeats
// it client-side because client-go's fake clientset ignores field selectors, so
// without it every test would see every event in the namespace.
func ObjectEvents(ctx context.Context, client kubernetes.Interface, namespace, name string) ([]corev1.Event, error) {
	list, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + name,
	})
	if err != nil {
		return nil, err
	}
	out := make([]corev1.Event, 0, len(list.Items))
	for _, e := range list.Items {
		if e.InvolvedObject.Name == name {
			out = append(out, e)
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run the collector tests**

```bash
go test ./internal/collect/
```

Expected: PASS.

- [ ] **Step 5: Write the failing inspect test**

Create `internal/mcp/inspect_test.go`:

```go
package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func callInspect(t *testing.T, cs *mcpsdk.ClientSession, args map[string]any) InspectOutput {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "kubeagent_inspect", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	var out InspectOutput
	blob, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return out
}

func TestInspect_PodReturnsItsFindingsAndEvents(t *testing.T) {
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "payments", Name: "e1"},
		InvolvedObject: corev1.ObjectReference{Namespace: "payments", Name: "api-abc"},
		Reason:         "BackOff", Message: "back-off restarting failed container", Type: "Warning", Count: 5,
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(crashingPod(), event))

	out := callInspect(t, cs, map[string]any{"kind": "pod", "namespace": "payments", "name": "api-abc"})

	if !out.Found {
		t.Fatal("Found = false, want the pod to be found")
	}
	if len(out.Findings) == 0 {
		t.Error("Findings is empty on a crash-looping pod")
	}
	if len(out.Events) == 0 {
		t.Fatal("Events is empty; inspect exists to surface the events triage summarises away")
	}
	if out.Events[0].Reason != "BackOff" || out.Events[0].Count != 5 {
		t.Errorf("Events[0] = %+v, want the BackOff event with count 5", out.Events[0])
	}
	if out.Events[0].Age == "" {
		t.Error("Events[0].Age is empty; a model cannot judge relevance without it")
	}
}

func TestInspect_MissingObjectIsNotFoundNotAnError(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callInspect(t, cs, map[string]any{"kind": "deployment", "namespace": "payments", "name": "ghost"})

	if out.Found {
		t.Error("Found = true for an object that does not exist")
	}
	if out.Findings == nil || out.Events == nil || out.Pods == nil {
		t.Error("a not-found result must still carry empty lists, so absent and empty stay distinguishable")
	}
	if out.Coverage == nil {
		t.Error("Coverage is nil; every result carries the honesty contract")
	}
}

func TestInspect_UnknownKindIsRejectedBeforeTheHandlerRuns(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "kubeagent_inspect",
		Arguments: map[string]any{"kind": "secret", "namespace": "payments", "name": "creds"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded for kind=secret; the published schema's enum must reject it")
	}
}

func TestInspect_MissingRequiredArgumentIsRejected(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_inspect", Arguments: map[string]any{"kind": "pod"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded without a name; name is required")
	}
	text := firstText(res)
	if !strings.Contains(text, "name") {
		t.Errorf("error text = %q, want it to name the missing property", text)
	}
}

// firstText returns the first text block of a tool result, for asserting on
// SDK-generated validation messages.
func firstText(res *mcpsdk.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
```

- [ ] **Step 6: Run it to verify it fails**

```bash
go test ./internal/mcp/ -run TestInspect
```

Expected: FAIL — `undefined: InspectOutput`.

- [ ] **Step 7: Write `inspect.go`**

Create `internal/mcp/inspect.go`. The input schema is written out explicitly rather than inferred, because an inferred schema cannot express an enum, and an enum is what stops a caller inventing `kind: "secret"` and getting a confusing runtime error instead of a clear schema rejection. Validation — including the enum — runs before the handler.

```go
package mcp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/scan"
)

// inspectKinds is both the published enum and the accepted set.
var inspectKinds = []any{"pod", "deployment", "statefulset", "daemonset", "replicaset", "job", "cronjob"}

// Event is one Kubernetes event, flattened.
type Event struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Count   int32  `json:"count"`
	Age     string `json:"age"`
}

// InspectInput is the kubeagent_inspect argument object.
type InspectInput struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Context   string `json:"context,omitempty"`
}

// InspectOutput is the kubeagent_inspect result.
type InspectOutput struct {
	Found     bool               `json:"found"`
	Kind      string             `json:"kind"`
	Namespace string             `json:"namespace"`
	Name      string             `json:"name"`
	Status    string             `json:"status,omitempty"`
	Desired   int                `json:"desired,omitempty"`
	Ready     int                `json:"ready,omitempty"`
	Image     string             `json:"image,omitempty"`
	Pods      []inventory.PodRow `json:"pods"`
	Findings  []Finding          `json:"findings"`
	Events    []Event            `json:"events"`
	Coverage  *Coverage          `json:"coverage"`
}

func registerInspect(s *mcpsdk.Server, cfg Config, client kubernetes.Interface, now func() time.Time) {
	tool := &mcpsdk.Tool{
		Name: "kubeagent_inspect",
		Description: "Inspect one workload or pod: its status, its pods, kubeagent's findings for it, and " +
			"its recent Kubernetes events. Read-only: this never changes cluster state.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"kind": {
					Type:        "string",
					Enum:        inspectKinds,
					Description: "the kind of object to inspect",
				},
				"namespace": {
					Type:        "string",
					Description: "the object's namespace",
				},
				"name": {
					Type:        "string",
					Description: "the object's name",
				},
				"context": {
					Type: "string",
					Description: "kubeconfig context to use; only accepted when the server was started " +
						"with --allow-context-switch",
				},
			},
			Required: []string{"kind", "namespace", "name"},
		},
	}

	mcpsdk.AddTool(s, tool, guard("kubeagent_inspect",
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in InspectInput) (*mcpsdk.CallToolResult, InspectOutput, error) {
			if in.Context != "" && !cfg.AllowContextSwitch {
				return nil, InspectOutput{}, errContextSwitchDisabled
			}

			res, err := scan.Evaluate(ctx, client, scan.Options{
				Namespace:       in.Namespace,
				IncludeCron:     true,
				IncludeRestarts: true,
				Logs:            cfg.Logs,
			})
			if err != nil {
				return nil, InspectOutput{}, errors.New("scanning the namespace: " + redact.Error(err))
			}

			cov := newCoverage(contextLabel(cfg.Context), in.Namespace, now())
			cov.markRun("workloads", "pod-diagnosis", "events")
			cov.markPartial(res.PartialReads)

			out := InspectOutput{
				Kind:      in.Kind,
				Namespace: in.Namespace,
				Name:      in.Name,
				Pods:      []inventory.PodRow{},
				Findings:  []Finding{},
				Events:    []Event{},
				Coverage:  cov,
			}

			for _, w := range res.Inventory.Workloads {
				if w.Namespace != in.Namespace || w.Name != in.Name ||
					!strings.EqualFold(w.Kind, in.Kind) {
					continue
				}
				out.Found = true
				out.Kind = w.Kind
				out.Status = w.Status
				out.Desired = w.Desired
				out.Ready = w.Ready
				out.Image = w.Image
				out.Pods = append(out.Pods, w.Pods...)
				for _, f := range w.Findings {
					out.Findings = append(out.Findings, fromDiagnose(f))
				}
				break
			}

			// Events are read for the named object whether or not the scan
			// found a workload for it: "the object is gone but its events
			// explain why" is exactly the case a drill-down must answer.
			events, evErr := collect.ObjectEvents(ctx, client, in.Namespace, in.Name)
			if evErr != nil {
				cov.Partial = append(cov.Partial, PartialRead{Resource: "events", Why: redact.Error(evErr)})
			}
			for _, e := range events {
				out.Events = append(out.Events, Event{
					Type:    e.Type,
					Reason:  e.Reason,
					Message: e.Message,
					Count:   e.Count,
					Age:     inventory.HumanAge(eventTime(e), now()),
				})
			}

			sortFindings(out.Findings)
			return nil, out, nil
		}))
}
```

Add the timestamp helper at the bottom of the same file — an event carries up to three timestamps and only one of them is reliably set:

```go
// eventTime picks the most recent timestamp an event carries. Series events
// set EventTime, repeated events set LastTimestamp, and one-shot events set
// only FirstTimestamp.
func eventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}
```

Add `corev1 "k8s.io/api/core/v1"` to the imports.

- [ ] **Step 8: Register it and restore the tool-list assertion**

Delete the `registerInspect` stub from `server.go`. In `server_test.go`, change `TestServer_ExposesExactlyTheReadOnlyTools`' `want` to:

```go
	want := []string{"kubeagent_inspect", "kubeagent_triage"}
```

- [ ] **Step 9: Run the tests**

```bash
go test ./internal/mcp/ ./internal/collect/ -v
```

Expected: PASS.

- [ ] **Step 10: Confirm the dependency is now direct**

```bash
grep -n 'jsonschema-go' go.mod
```

Expected: `github.com/google/jsonschema-go` now appears in the direct `require` block, not the indirect one (run `go mod tidy` if it has not moved). That is intended: explicit enum schemas need it, and the spec's dependency section accounts for it.

- [ ] **Step 11: Commit**

```bash
git add internal/mcp internal/collect go.mod go.sum
git commit -m "feat(mcp): add kubeagent_inspect with per-object events"
```

---

### Task 7: `kubeagent_advisory`

The opt-in sections — operators, GitOps drift, capacity, security posture, certificate expiry — behind one tool with an explicit section list, so a caller pays for only what it asks for.

**Files:**
- Create: `internal/mcp/advisory.go`
- Create: `internal/mcp/advisory_test.go`
- Modify: `internal/mcp/server.go` (delete the `registerAdvisory` stub)
- Modify: `internal/mcp/server_test.go` (restore `kubeagent_advisory` in the expected tool list)

**Interfaces:**
- Consumes: `advisory.Assess`, `advisory.Options`, `advisory.Inputs`, `advisory.ClusterPods`, `advisory.Degradation` (Task 2); `guard`, `contextLabel`, `errContextSwitchDisabled` (Task 5).
- Produces:
  - `type mcp.AdvisoryInput struct { Sections []string; Namespace string; Context string }`
  - `type mcp.AdvisoryOutput struct { Requested []string; Sections map[string]any; Coverage *Coverage }` → `requested`, `sections`, `coverage`
  - `func registerAdvisory(s *mcpsdk.Server, cfg Config, client kubernetes.Interface, now func() time.Time)`

**The two sources, and why there are two:** `operators`, `drift` and `capacity` come from `advisory.Assess`. `security` and `certificates` are computed inside `scan.Evaluate` and only when `Options.Security` / `Options.Certs` are set — so asking for them means a second `scan.Evaluate` call with those options on. That is one extra scan per advisory call that requests them, and it is the honest cost: there is no way to get those two sections from a scan that did not ask for them.

- [ ] **Step 1: Write the failing test**

Create `internal/mcp/advisory_test.go`:

```go
package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes/fake"
)

func callAdvisory(t *testing.T, cs *mcpsdk.ClientSession, args map[string]any) AdvisoryOutput {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "kubeagent_advisory", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	var out AdvisoryOutput
	blob, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return out
}

func TestAdvisory_OnlyTheRequestedSectionsAppear(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callAdvisory(t, cs, map[string]any{"sections": []any{"capacity"}})

	if len(out.Requested) != 1 || out.Requested[0] != "capacity" {
		t.Errorf("Requested = %v, want [capacity]", out.Requested)
	}
	if _, ok := out.Sections["capacity"]; !ok {
		t.Errorf("Sections = %v, want a capacity entry", out.Sections)
	}
	if _, ok := out.Sections["security"]; ok {
		t.Error("Sections contains security, which was not requested")
	}
}

func TestAdvisory_RequestedSectionsAreDedupedAndSorted(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callAdvisory(t, cs, map[string]any{"sections": []any{"security", "capacity", "security"}})

	want := []string{"capacity", "security"}
	if len(out.Requested) != len(want) {
		t.Fatalf("Requested = %v, want %v", out.Requested, want)
	}
	for i := range want {
		if out.Requested[i] != want[i] {
			t.Errorf("Requested = %v, want %v", out.Requested, want)
		}
	}
}

func TestAdvisory_SectionsMapIsNeverNull(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_advisory", Arguments: map[string]any{"sections": []any{}},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	blob, _ := json.Marshal(res.StructuredContent)
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if raw["sections"] == nil {
		t.Error("sections is null; an empty section map must marshal as {} so absent and empty stay distinct")
	}
}

func TestAdvisory_UnknownSectionIsRejectedBeforeTheHandlerRuns(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_advisory", Arguments: map[string]any{"sections": []any{"fix"}},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() accepted section \"fix\"; the enum must reject anything outside the five sections")
	}
}

func TestAdvisory_CapacityWithoutMetricsReportsMetricsServerAbsent(t *testing.T) {
	// The fake clientset serves no metrics API. The capacity report is still
	// produced, from requests and limits, so the only honest signal that the
	// headroom is not usage-backed is coverage.metricsServer.
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callAdvisory(t, cs, map[string]any{"sections": []any{"capacity"}})

	if out.Coverage == nil {
		t.Fatal("Coverage is nil")
	}
	if out.Coverage.MetricsServer != "absent" {
		t.Errorf("coverage.metricsServer = %q, want %q", out.Coverage.MetricsServer, "absent")
	}
}

func TestAdvisory_MetricsServerStaysNotCheckedWhenCapacityIsNotRequested(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callAdvisory(t, cs, map[string]any{"sections": []any{"security"}})

	if out.Coverage.MetricsServer != "not-checked" {
		t.Errorf("coverage.metricsServer = %q, want %q — nothing queried metrics, so claiming "+
			"\"absent\" would assert an untested fact", out.Coverage.MetricsServer, "not-checked")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
go test ./internal/mcp/ -run TestAdvisory
```

Expected: FAIL — `undefined: AdvisoryOutput`.

- [ ] **Step 3: Write `advisory.go`**

Create `internal/mcp/advisory.go`:

```go
package mcp

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/advisory"
	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/scan"
)

// advisorySections is both the published enum and the accepted set.
var advisorySections = []any{"operators", "drift", "capacity", "security", "certificates"}

// AdvisoryInput is the kubeagent_advisory argument object.
type AdvisoryInput struct {
	Sections  []string `json:"sections"`
	Namespace string   `json:"namespace,omitempty"`
	Context   string   `json:"context,omitempty"`
}

// AdvisoryOutput carries one entry per requested section. Requested echoes what
// was asked for, so a caller comparing it with the keys of Sections can see at
// a glance that nothing was dropped on the floor.
type AdvisoryOutput struct {
	Requested []string       `json:"requested"`
	Sections  map[string]any `json:"sections"`
	Coverage  *Coverage      `json:"coverage"`
}

// normalizeSections dedupes and sorts, so the same request always produces the
// same payload regardless of the order the caller listed them in.
func normalizeSections(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func registerAdvisory(s *mcpsdk.Server, cfg Config, client kubernetes.Interface, now func() time.Time) {
	tool := &mcpsdk.Tool{
		Name: "kubeagent_advisory",
		Description: "Run kubeagent's opt-in advisory sections: operator health, GitOps drift, scheduling " +
			"capacity, security posture, and certificate expiry. Each section costs extra API reads, so " +
			"request only what you need. Read-only: this never changes cluster state.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"sections": {
					Type:        "array",
					Description: "which advisory sections to run",
					Items:       &jsonschema.Schema{Type: "string", Enum: advisorySections},
				},
				"namespace": {
					Type:        "string",
					Description: "limit to one namespace; omit for the whole cluster",
				},
				"context": {
					Type: "string",
					Description: "kubeconfig context to use; only accepted when the server was started " +
						"with --allow-context-switch",
				},
			},
			Required: []string{"sections"},
		},
	}

	mcpsdk.AddTool(s, tool, guard("kubeagent_advisory",
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in AdvisoryInput) (*mcpsdk.CallToolResult, AdvisoryOutput, error) {
			if in.Context != "" && !cfg.AllowContextSwitch {
				return nil, AdvisoryOutput{}, errContextSwitchDisabled
			}

			want := map[string]bool{}
			requested := normalizeSections(in.Sections)
			for _, s := range requested {
				want[s] = true
			}

			cov := newCoverage(contextLabel(cfg.Context), in.Namespace, now())
			out := AdvisoryOutput{Requested: requested, Sections: map[string]any{}, Coverage: cov}

			// One scan feeds every section: the advisory assessors need the
			// workload objects it already listed. security and certificates
			// are computed inside Evaluate and only when asked for.
			res, err := scan.Evaluate(ctx, client, scan.Options{
				Namespace:       in.Namespace,
				IncludeCron:     true,
				IncludeRestarts: true,
				Security:        want["security"],
				Certs:           want["certificates"],
				CertWarnDays:    30,
			})
			if err != nil {
				return nil, AdvisoryOutput{}, errors.New("scanning the cluster: " + redact.Error(err))
			}
			cov.markPartial(res.PartialReads)

			if want["security"] {
				cov.markRun("security")
				out.Sections["security"] = res.SecurityIssues
			}
			if want["certificates"] {
				cov.markRun("certificates")
				if res.Certificates == nil {
					cov.markSkipped("certificates", "no certificate data was returned for this scope")
				} else {
					out.Sections["certificates"] = res.Certificates
				}
			}

			if want["operators"] || want["drift"] || want["capacity"] {
				pods, podsErr := advisory.ClusterPods(ctx, client, in.Namespace, res.Inputs.Pods)
				if podsErr != nil {
					cov.Partial = append(cov.Partial, PartialRead{
						Resource: "pods (cluster-wide)",
						Why: redact.Error(podsErr) +
							"; headroom is computed from namespace " + in.Namespace +
							" only and overstates free capacity",
					})
				}

				adv := advisory.Assess(ctx, client,
					func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
						return cluster.NewDynamicClients(cfg.Kubeconfig, cfg.Context)
					},
					advisory.Inputs{
						Deployments:  res.Inputs.Deployments,
						StatefulSets: res.Inputs.StatefulSets,
						DaemonSets:   res.Inputs.DaemonSets,
						Jobs:         res.Inputs.Jobs,
						CronJobs:     res.Inputs.CronJobs,
						ReplicaSets:  res.Inputs.ReplicaSets,
						Nodes:        res.Nodes,
						Pods:         pods,
					},
					advisory.Options{
						Operators: want["operators"],
						Drift:     want["drift"],
						DriftAge:  24 * time.Hour,
						Capacity:  want["capacity"],
						Namespace: in.Namespace,
					}, now())

				// A degradation whose section still produced a report is a
				// partial read; one whose report is nil is a skipped check.
				reports := map[string]any{}
				if adv.Operators != nil {
					reports["operators"] = adv.Operators
				}
				if adv.GitOps != nil {
					reports["drift"] = adv.GitOps
				}
				if adv.Capacity != nil {
					reports["capacity"] = adv.Capacity
				}
				for _, d := range adv.Degradations {
					for _, section := range d.Sections {
						if _, produced := reports[section]; produced {
							cov.Partial = append(cov.Partial, PartialRead{Resource: section, Why: d.Reason})
						} else {
							cov.markSkipped(section, d.Reason)
						}
					}
				}
				for _, section := range []string{"operators", "drift", "capacity"} {
					if !want[section] {
						continue
					}
					if rep, ok := reports[section]; ok {
						cov.markRun(section)
						out.Sections[section] = rep
					}
				}

				if want["capacity"] {
					cov.MetricsServer = "absent"
					if adv.MetricsAvailable {
						cov.MetricsServer = "available"
					}
				}
			}

			return nil, out, nil
		}))
}
```

`DriftAge` is fixed at 24h here, matching the CLI's `--drift-age` default; the tool deliberately does not expose it as an argument, because a caller tuning a drift threshold is making a policy decision the operator owns.

- [ ] **Step 4: Register it and restore the tool-list assertion**

Delete the `registerAdvisory` stub from `server.go`. In `server_test.go`, restore:

```go
	want := []string{"kubeagent_advisory", "kubeagent_inspect", "kubeagent_triage"}
```

- [ ] **Step 5: Run the tests**

```bash
go test ./internal/mcp/ -v
```

Expected: PASS, including `TestServer_ExposesExactlyTheReadOnlyTools` with all three names in alphabetical order.

- [ ] **Step 6: Commit**

```bash
git add internal/mcp
git commit -m "feat(mcp): add kubeagent_advisory for the opt-in sections"
```

---

### Task 8: `list_contexts` and honouring `context` when switching is allowed

Until now the `context` argument is only ever rejected. This task makes it work — behind an explicit opt-in — and adds the tool that makes it usable.

**Files:**
- Modify: `internal/cluster/client.go` (add `ContextInfo` and `Contexts`)
- Create: `internal/cluster/contexts_test.go`
- Create: `internal/mcp/contexts.go`
- Create: `internal/mcp/contexts_test.go`
- Modify: `internal/mcp/server.go` (add the `clientFactory` parameter, the `clientFor` helper, conditional registration)
- Modify: `internal/mcp/triage.go`, `internal/mcp/inspect.go`, `internal/mcp/advisory.go` (use `clientFor` instead of the inline rejection)
- Modify: `internal/mcp/server_test.go` (the `connect` helper gains a factory)

**Interfaces:**
- Consumes: `redact.URL` (Task 1); `newServer`, `guard` (Task 5).
- Produces:
  - `type cluster.ContextInfo struct { Name, Cluster, Server string; Current bool }`
  - `func cluster.Contexts(kubeconfigPath string) ([]ContextInfo, error)` — `Server` is already reduced to `scheme://host`
  - `type mcp.clientFactory func(contextName string) (kubernetes.Interface, error)`
  - `func clientFor(cfg Config, base kubernetes.Interface, switchTo clientFactory, requested string) (kubernetes.Interface, string, error)` — returns the client and the context label to report
  - `func newServer(cfg Config, version string, client kubernetes.Interface, switchTo clientFactory, now func() time.Time) *mcpsdk.Server` — **signature change**
  - `type mcp.ContextsOutput struct { Contexts []ContextView; Current string }` → `contexts`, `current`
  - `type mcp.ContextView struct { Name, Cluster, Server string; Current bool }` → `name`, `cluster`, `server`, `current`

- [ ] **Step 1: Write the failing `cluster.Contexts` test**

Create `internal/cluster/contexts_test.go`. The fixture is written by the test, so nothing real is checked in:

```go
package cluster

import (
	"os"
	"path/filepath"
	"testing"
)

const kubeconfigFixture = `apiVersion: v1
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

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(kubeconfigFixture), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func TestContexts_ListsEveryContextSortedWithTheCurrentOneMarked(t *testing.T) {
	got, err := Contexts(writeKubeconfig(t))
	if err != nil {
		t.Fatalf("Contexts() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Contexts() returned %d contexts, want 2", len(got))
	}
	if got[0].Name != "prod" || got[1].Name != "staging" {
		t.Errorf("names = %q, %q; want them sorted", got[0].Name, got[1].Name)
	}
	if got[0].Current {
		t.Error("prod is marked current; staging is current-context")
	}
	if !got[1].Current {
		t.Error("staging is not marked current")
	}
	if got[1].Cluster != "staging-cluster" {
		t.Errorf("Cluster = %q, want %q", got[1].Cluster, "staging-cluster")
	}
}

func TestContexts_ServerIsReducedToSchemeAndHost(t *testing.T) {
	got, err := Contexts(writeKubeconfig(t))
	if err != nil {
		t.Fatalf("Contexts() error = %v", err)
	}
	for _, c := range got {
		if c.Server != "https://staging.example.com:6443" && c.Server != "https://prod.example.com:6443" {
			t.Errorf("Server = %q; an API server URL may carry no more than scheme://host", c.Server)
		}
	}
}

func TestContexts_MissingFileErrorDoesNotEchoThePath(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "customer-acme-prod-kubeconfig")

	_, err := Contexts(secret)
	if err == nil {
		t.Fatal("Contexts() error = nil, want a load failure")
	}
	if filepath.Base(secret) != "" && contains(err.Error(), filepath.Base(secret)) {
		t.Errorf("error = %q; a kubeconfig path names a customer and an environment and must not be echoed", err)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || len(haystack) > 0 && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cluster/ -run TestContexts
```

Expected: FAIL — `undefined: Contexts`.

- [ ] **Step 3: Implement `cluster.Contexts`**

Add to `internal/cluster/client.go`:

```go
// ContextInfo describes one kubeconfig context, with the API server URL
// already reduced to scheme://host.
type ContextInfo struct {
	Name    string
	Cluster string
	Server  string
	Current bool
}

// Contexts lists the contexts a kubeconfig defines. It deliberately never
// includes the kubeconfig path, not in the result and not in its errors: a
// path like ~/.kube/customer-acme-prod names a customer, a cluster and an
// environment, and this list is served to a remote caller.
func Contexts(kubeconfigPath string) ([]ContextInfo, error) {
	path, err := resolveKubeconfig(kubeconfigPath)
	if err != nil {
		return nil, errors.New("locating the kubeconfig")
	}
	raw, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, errors.New("loading the kubeconfig")
	}

	out := make([]ContextInfo, 0, len(raw.Contexts))
	for name, c := range raw.Contexts {
		info := ContextInfo{Name: name, Cluster: c.Cluster, Current: name == raw.CurrentContext}
		if cl, ok := raw.Clusters[c.Cluster]; ok {
			info.Server = redact.URL(cl.Server)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
```

Add `"errors"`, `"sort"` and `"github.com/imantaba/kubeagent/internal/redact"` to the imports. `internal/redact` imports only `errors` and `net/url`, so there is no cycle.

- [ ] **Step 4: Run it**

```bash
go test ./internal/cluster/
```

Expected: PASS.

- [ ] **Step 5: Add the client factory and `clientFor`**

In `internal/mcp/server.go`, add:

```go
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
```

Change `newServer` to take the factory and pass it down, and register `list_contexts` only when switching is allowed:

```go
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
```

And in `Serve`, pass the real factory:

```go
	s := newServer(cfg, version, client, func(contextName string) (kubernetes.Interface, error) {
		return cluster.NewClient(cfg.Kubeconfig, contextName)
	}, time.Now)
```

- [ ] **Step 6: Rewire the three handlers**

In `triage.go`, `inspect.go` and `advisory.go`: add `switchTo clientFactory` to each `register*` signature, and replace

```go
			if in.Context != "" && !cfg.AllowContextSwitch {
				return nil, TriageOutput{}, errContextSwitchDisabled
			}
```

with

```go
			client, contextName, err := clientFor(cfg, base, switchTo, in.Context)
			if err != nil {
				return nil, TriageOutput{}, err
			}
```

Rename each closure's captured clientset parameter from `client` to `base` so the per-call `client` shadows nothing, use `contextName` wherever the handler currently calls `contextLabel(cfg.Context)`, and use the per-call `client` in the `scan.Evaluate` and `collect.ObjectEvents` calls. In `advisory.go`, the `cluster.NewDynamicClients` closure takes `contextName` instead of `cfg.Context`.

- [ ] **Step 7: Update the test harness**

In `internal/mcp/server_test.go`, `connect` gains the factory. Give it one that returns the same fake, so a switch-allowed server is testable:

```go
	srv := newServer(cfg, "test", client,
		func(string) (kubernetes.Interface, error) { return client, nil },
		func() time.Time { return fixedNow })
```

- [ ] **Step 8: Write the failing `list_contexts` test**

Create `internal/mcp/contexts_test.go`:

```go
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
```

- [ ] **Step 9: Run it to verify it fails**

```bash
go test ./internal/mcp/ -run TestListContexts
```

Expected: FAIL — `undefined: registerContexts` (or the tool is absent).

- [ ] **Step 10: Write `contexts.go`**

Create `internal/mcp/contexts.go`:

```go
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
```

`cluster.Contexts` already returns path-free errors, so passing `err.Error()` through is safe here — that is why it was written that way rather than relying on the caller to remember.

- [ ] **Step 11: Run the whole package**

```bash
go test ./internal/mcp/ ./internal/cluster/ -v
```

Expected: PASS. `TestServer_ExposesExactlyTheReadOnlyTools` still asserts three tools, because the default `Config{}` has `AllowContextSwitch` false.

- [ ] **Step 12: Commit**

```bash
git add internal/mcp internal/cluster
git commit -m "feat(mcp): opt-in context switching with list_contexts"
```

---

### Task 9: the `kubeagent mcp` subcommand

**Files:**
- Modify: `main.go:59-68` (subcommand dispatch), the usage string, and a new `runMCP` function
- Modify: `main_test.go` (usage assertions)

**Interfaces:**
- Consumes: `mcp.Serve`, `mcp.Config` (Tasks 5-8).
- Produces: the `kubeagent mcp` subcommand.

- [ ] **Step 1: Write the failing test**

Add to `main_test.go`:

```go
func TestUsage_MentionsTheMCPSubcommand(t *testing.T) {
	err := run([]string{"kubeagent"})
	if err == nil {
		t.Fatal("run() with no subcommand error = nil, want the usage error")
	}
	if !strings.Contains(err.Error(), "kubeagent mcp") {
		t.Errorf("usage = %q, want it to list the mcp subcommand", err)
	}
}
```

Match the existing usage-assertion style in `main_test.go`; if the entry point there is named something other than `run`, use whatever the file already calls.

- [ ] **Step 2: Run it to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestUsage_MentionsTheMCPSubcommand
```

Expected: FAIL — the usage string has no `kubeagent mcp` line.

- [ ] **Step 3: Add the subcommand**

In the dispatch at `main.go:59-68`, add a case beside `version` and `watch`:

```go
	case "mcp":
		return runMCP(args[2:])
```

Add `runMCP` next to the `watch` runner, using its own `flag.FlagSet` — the standard library only, matching every other subcommand:

```go
// runMCP serves kubeagent's read-only diagnosis over the Model Context
// Protocol on stdin/stdout, for an AI agent to call as a tool.
func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current context)")
	allowSwitch := fs.Bool("allow-context-switch", false,
		"let tool calls name a different kubeconfig context, and expose list_contexts")
	logs := fs.Bool("logs", false, "enrich findings with a short log tail from failing containers")
	if err := fs.Parse(args); err != nil {
		return err
	}

	return mcp.Serve(context.Background(), mcp.Config{
		Kubeconfig:         *kubeconfig,
		Context:            *contextName,
		AllowContextSwitch: *allowSwitch,
		Logs:               *logs,
	}, version)
}
```

Import `"github.com/imantaba/kubeagent/internal/mcp"`.

- [ ] **Step 4: Extend the usage string**

In the single usage `fmt.Errorf` string, add the `mcp` line next to `watch`, keeping the existing alignment exactly:

```
  kubeagent mcp [--kubeconfig path] [--context name] [--allow-context-switch] [--logs]
```

and a one-line description in the same style as the others: `serve read-only diagnosis to an AI agent over the Model Context Protocol (stdio)`.

- [ ] **Step 5: Run the tests**

```bash
go build ./... && go test ./...
```

Expected: all PASS.

- [ ] **Step 6: Drive it by hand over stdio**

```bash
go build -o kubeagent .
{ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 2; } | ./kubeagent mcp --context kind-kubeagent-smoke
```

Expected: three JSON lines on stdout, one of them listing exactly `kubeagent_advisory`, `kubeagent_inspect`, `kubeagent_triage`. The `sleep 2` is required: closing stdin while a request is in flight makes the server exit with `server is closing: EOF` before it answers.

If no cluster is available, this step is expected to fail at startup with a connection error — that is the eager validation working. Note it in the task report and move on.

- [ ] **Step 7: Commit**

```bash
git add main.go main_test.go
git commit -m "feat(cli): add the kubeagent mcp subcommand"
```

---

### Task 10: documentation

**Files:**
- Create: `website/docs/features/mcp.md`
- Modify: `website/mkdocs.yml` (nav)
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `website/docs/roadmap.md`
- Modify: `CLAUDE.md` (invariants)

**Sibling doc surfaces, checked deliberately** — this project has twice shipped a feature whose sibling docs went stale:

| Surface | Action |
|---------|--------|
| `website/docs/features/mcp.md` | **Create** — the feature page. |
| `website/mkdocs.yml` nav | **Modify** — a page absent from the nav is unreachable. |
| `README.md` | **Modify** — the feature list and a usage line. |
| `CHANGELOG.md` | **Modify** — under `## [Unreleased]`. |
| `website/docs/roadmap.md` | **Modify** — Theme G bullet and the v0.5x milestone row. |
| `CLAUDE.md` | **Modify** — the read-only invariant now has a second documented long-lived process. |
| `website/docs/quickstart.md` | **No change** — it walks through `scan`, whose output is unchanged. |
| `deploy/README.md`, `deploy/helm/**` | **No change** — they document the in-cluster watch daemon. The MCP server runs beside a developer's agent over stdio, is not deployed in-cluster, and adds no RBAC. |
| `internal/report/testdata/golden-scan.txt` | **No change** — must stay byte-identical. |

- [ ] **Step 1: Write the feature page**

Create `website/docs/features/mcp.md` following the structure of the existing feature pages (read `website/docs/features/watch-mode.md` first and match its heading depth, admonition style, and code-fence conventions). Cover:

1. **What it is** — `kubeagent mcp` serves the same deterministic diagnosis the CLI runs, over the Model Context Protocol on stdio, so an agent can call it as a tool.
2. **Why an agent should trust it** — the answers are computed by detectors, not generated; kubeagent never calls an LLM from this path.
3. **Read-only, structurally** — get/list/watch only; no tool reaches `--fix`; no tool name contains a write verb; the server cannot be talked into a write.
4. **The four tools** — a table of `kubeagent_triage`, `kubeagent_inspect`, `kubeagent_advisory`, and `list_contexts` (conditional), with their arguments.
5. **The coverage block** — show a real `coverage` object and explain that `checksSkipped` exists so a model can tell "clean" from "never looked".
6. **Configuration** — a client config snippet:

```json
{
  "mcpServers": {
    "kubeagent": {
      "command": "kubeagent",
      "args": ["mcp", "--context", "my-cluster"]
    }
  }
}
```

7. **Context switching** — off by default; `--allow-context-switch` also registers `list_contexts`.
8. **Freshness** — every call runs a fresh scan; there is no cache, so an agent never reasons about a stale cluster.
9. **What it does not do** — no remediation, no `--explain`, no `watch`-style streaming.

No real cluster names, hostnames, or IPs anywhere: use `my-cluster` and `example.com`.

- [ ] **Step 2: Add it to the nav**

In `website/mkdocs.yml`, under `nav: Features:`, add the page after `watch-mode`:

```yaml
      - MCP server: features/mcp.md
```

- [ ] **Step 3: Build the site strictly**

```bash
(cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml)
```

Expected: "Documentation built", exit 0, and **no `WARNING` line mentioning `mcp.md`**. The red "Material for MkDocs 2.0" banner is cosmetic. A warning about a page not in the nav means Step 2 was missed.

- [ ] **Step 4: Update the README**

Add the MCP server to the feature list and a short usage block in the established style:

```bash
kubeagent mcp --context my-cluster
```

with one sentence: it serves kubeagent's read-only diagnosis to an AI agent over the Model Context Protocol, and it can never write to the cluster.

- [ ] **Step 5: Update the changelog**

Under `## [Unreleased]` → `### Added` in `CHANGELOG.md`:

```markdown
- **MCP server (`kubeagent mcp`)** — serves kubeagent's deterministic, read-only
  diagnosis to AI agents over the Model Context Protocol on stdio. Four tools:
  `kubeagent_triage` (verdict plus findings), `kubeagent_inspect` (one workload,
  with its events), `kubeagent_advisory` (the opt-in operator, drift, capacity,
  security and certificate sections), and `list_contexts` (only when started
  with `--allow-context-switch`). Every result carries a `coverage` block
  naming what ran, what was skipped and why, and what was read only partially,
  so a model can tell "nothing is wrong" from "nothing was checked". No tool
  can reach `--fix`, and the server never calls an LLM.
```

Under `### Changed`, note the two refactors — redaction moved to `internal/redact`, and the CLI's advisory sections now report structured degradations — stating that CLI output is unchanged.

- [ ] **Step 6: Update the roadmap**

In `website/docs/roadmap.md`: mark the MCP server shipped in the Theme G bullet (~line 392) and in the **v0.5x** milestone row (~line 431), matching how already-shipped items in that table are marked. Leave krew, the CI/CD gate, the TUI and the dashboard as the remaining Theme G work.

- [ ] **Step 7: Update `CLAUDE.md`**

In the **Invariants** section, extend the concurrency exception note: `internal/watch` is no longer the only long-lived process. Add that `kubeagent mcp` is a long-lived server too, that it is **strictly read-only** toward the cluster and never calls an LLM, and that `internal/mcp` must never import `internal/remediate` or `internal/explain`.

Also add a line to the Roadmap section recording that Theme G slice 1 (MCP server) has shipped.

- [ ] **Step 8: Scan for leaked values**

```bash
grep -rniE '([0-9]{1,3}\.){3}[0-9]{1,3}|\.local\b|Bearer |sk-ant|kubeconfig: /' \
  website/docs/features/mcp.md README.md CHANGELOG.md
```

Expected: no real addresses or credentials. Documentation IP examples, if any, must be RFC 5737 (`192.0.2.0/24`).

- [ ] **Step 9: Commit**

```bash
git add website README.md CHANGELOG.md CLAUDE.md
git commit -m "docs: document the MCP server"
```

---

### Task 11: chaos scenario 19 — the MCP server against a real broken cluster

Every other test in this branch runs against a fake clientset. This one drives the real binary over real stdio against a real Kind cluster with real outages injected, which is the only way to know the protocol handshake, the eager validation and the projections all work together.

**Files:**
- Modify: `chaos/run.sh` (add `scenario_19_mcp`, register it in `run_scenarios`)

**Interfaces:**
- Consumes: the `kubeagent mcp` subcommand (Task 9).
- Produces: chaos scenario 19.

**Harness conventions to follow exactly** (read `scenario_18_capacity()` at `chaos/run.sh:871` first and mirror its shape):

- The cluster is `kubeagent-chaos` and the context is `kind-kubeagent-chaos`. **Every `kubectl` call is pinned with `--context "$CTX"`.** No command in this scenario may touch any other context.
- Use the existing `log()` and `record()` helpers; `record` takes the scenario name and the pass/fail verdict the same way scenario 18 does.
- Scenarios are registered in the `local all=(…)` array in `run_scenarios()` (~line 965). Add `19_mcp` **before** `01_etcd`, which stays last deliberately.

**Two protocol facts this scenario must respect, both measured:**

1. **Hold stdin open.** Closing stdin while requests are in flight makes the server exit with `server is closing: EOF` before it answers. The driving pipeline ends with a `sleep`.
2. **Match responses by `id`, never by line position.** The SDK serves requests concurrently and responses arrive out of order — a probe observed id 3 before id 2.

- [ ] **Step 1: Write the scenario**

Add to `chaos/run.sh`, immediately after `scenario_18_capacity()`:

```bash
# 19 — MCP server: drive the real stdio protocol against the chaos cluster.
scenario_19_mcp() {
  log "scenario 19: MCP server over stdio"

  # A crash-looping pod so triage has something real to find.
  kubectl --context "$CTX" create namespace chaos-mcp --dry-run=client -o yaml |
    kubectl --context "$CTX" apply -f -
  kubectl --context "$CTX" -n chaos-mcp run mcp-crasher \
    --image=busybox --restart=Always -- /bin/sh -c 'exit 1' >/dev/null 2>&1 || true

  # Give it long enough to reach CrashLoopBackOff.
  local waited=0
  while [ "$waited" -lt 90 ]; do
    if kubectl --context "$CTX" -n chaos-mcp get pod mcp-crasher \
      -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null |
      grep -q CrashLoopBackOff; then
      break
    fi
    sleep 5
    waited=$((waited + 5))
  done

  local out="$RESULTS/scenario-19-mcp.jsonl"
  {
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"chaos","version":"0"}}}'
    printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
    printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
    printf '%s\n' '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"kubeagent_triage","arguments":{"namespace":"chaos-mcp"}}}'
    # Hold stdin open: closing it with requests in flight kills the server
    # before it answers ("server is closing: EOF").
    sleep 10
  } | ./kubeagent mcp --context "$CTX" >"$out" 2>"$RESULTS/scenario-19-mcp.err" || true

  # Responses arrive out of order — the SDK serves requests concurrently — so
  # every assertion below selects its response by id, never by line number.
  local verdict="pass"

  local tools
  tools=$(python3 -c '
import json, sys
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if msg.get("id") == 2:
        print(" ".join(sorted(t["name"] for t in msg["result"]["tools"])))
        break
' "$out")
  if [ "$tools" != "kubeagent_advisory kubeagent_inspect kubeagent_triage" ]; then
    log "scenario 19: unexpected tool list: $tools"
    verdict="fail"
  fi
  case "$tools" in
    *fix* | *apply* | *delete* | *patch* | *create*)
      log "scenario 19: a tool name contains a write verb: $tools"
      verdict="fail"
      ;;
  esac

  local triage
  triage=$(python3 -c '
import json, sys
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if msg.get("id") == 3:
        out = msg["result"]["structuredContent"]
        print(out["verdict"], len(out["findings"]), out["coverage"]["context"], sep="|")
        break
' "$out")
  local got_verdict got_findings got_context
  IFS="|" read -r got_verdict got_findings got_context <<<"$triage"

  if [ "$got_verdict" != "degraded" ]; then
    log "scenario 19: verdict=$got_verdict, want degraded"
    verdict="fail"
  fi
  if [ "${got_findings:-0}" -lt 1 ]; then
    log "scenario 19: findings=$got_findings, want at least one"
    verdict="fail"
  fi
  if [ "$got_context" != "$CTX" ]; then
    log "scenario 19: coverage.context=$got_context, want $CTX"
    verdict="fail"
  fi

  kubectl --context "$CTX" delete namespace chaos-mcp --wait=false >/dev/null 2>&1 || true

  record "19_mcp" "$verdict"
}
```

Match `record`'s real argument order and the `$RESULTS` variable name to what scenario 18 uses — if they differ, follow the file, not this snippet.

- [ ] **Step 2: Register it**

In `run_scenarios()`, add `19_mcp` to the `local all=(…)` array immediately before `01_etcd`.

- [ ] **Step 3: Run only this scenario**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
./chaos/run.sh --recreate --only 19
```

Expected: scenario 19 green. This takes several minutes (cluster creation plus the crash-loop wait) — run it in the background and watch the log.

If it fails on an empty `$out`, read `$RESULTS/scenario-19-mcp.err`: `server is closing: EOF` means the `sleep 10` is too short or was dropped.

- [ ] **Step 4: Commit**

```bash
git add chaos/run.sh
git commit -m "test(chaos): scenario 19 drives the MCP server over stdio"
```

---

## Full-gate note for the release

This branch touches `internal/collect`, `internal/cluster` and `main.go`, so the release gate is the **full chaos gate**, not a smoke test:

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
./chaos/run.sh --recreate
```

Every scenario must be green before the release is cut. It does not touch `--fix`, RBAC, `nodes/proxy`, the watch daemon or the Helm templates, so the chart version takes a **patch** bump only.
