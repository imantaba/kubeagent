# Smart `--investigate` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `--investigate` measurably smarter per tool call — prime the loop with the deterministic hypothesis trace, add a privacy-preserving log-classification tool and a Service hop, and close the known error-string leak — implementing Section 3 of `docs/superpowers/specs/2026-08-18-hypothesis-engine-design.md`.

**Architecture:** Four changes, all inside existing seams of `internal/investigate`: (1) a new pure function `renderTrace` appends each flagged workload's `RootCauseTrace` to the loop's first user message; (2) a new tool `get_log_causes` fetches the bounded previous-log tail via `collect.PreviousLogs`, classifies it with `logscan.Classify`, and returns **only** the redacted Cause string; (3) `get_related` learns the `service` relation and `describe` learns the Service kind; (4) every failed client-go read in `reader.go` reduces through `redact.Error` before reaching the model. No schema moves, no CLI change, no new loop bounds.

**Tech Stack:** Go 1.26; client-go fake clientset for tests; existing packages `internal/collect`, `internal/logscan`, `internal/redact`, `internal/safetext`, `internal/inventory`, `internal/explain`. `k8s.io/apimachinery/pkg/labels` is inside an existing dependency.

## Global Constraints

Every task's requirements implicitly include this section.

- **Read-only toward the cluster** — get/list only; `GetLogs` is a get. SEPARATELY: `internal/investigate` IS the model path (Anthropic-only). Never blur "read-only" with "makes no external calls"; they are two promises.
- **The trace-priming function lives in `internal/investigate`, never in `internal/explain`.** The trace names nodes; `--explain`'s payload deliberately excludes node names, while `--investigate` already surfaces them through its tools. `explain.BuildInventoryPrompt` and `--explain`'s payload stay byte-unchanged.
- **A raw log line never crosses the model boundary.** Only the classified `logscan.Clue.Cause` (a fixed vocabulary) crosses, after `redact.Addresses`. `Clue.Excerpt` is discarded deliberately — this matches the existing report policy split (LogCause may cross after redaction; LogExcerpt never does).
- **Loop bounds unchanged:** `maxToolCalls = 8`, `maxTurns = 6`. No new user-facing knobs. The never-fatal rule, the Anthropic-only CLI gate, the findings-seeded scope closure, and the every-tool_use-gets-a-tool_result contract are all untouched. `internal/cli` is untouched in this slice.
- **NO SCHEMA MOVES.** The spec's "scan 1.3 → 1.4" note belongs to slice 1 (scan is already past it); this slice adds no JSON field to any of the eight versioned documents. Never run any test with `-update`, for any reason.
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.** Do NOT regenerate the demo GIF or `website/docs/quickstart.md`.
- **NO NEW DEPENDENCY:** `go.mod` and `go.sum` must not change (`k8s.io/apimachinery/pkg/labels` is within an existing module). Verify `git diff --stat main -- go.mod go.sum` is empty at the end of every task.
- **Package walls:** `internal/investigate` may import `explain`, `collect`, `logscan`, `redact`, `safetext`, `inventory` — no wall forbids these and none creates a cycle. `internal/tui` must never import `internal/investigate` (untouched here).
- **Untrusted API text is sanitized at ingress** (`sanitize` = `safetext.Line` then `redact.Addresses`, with its documented ordering rationale); matching decisions run on the RAW value. The `get_related` arms use `safetext.Line` alone, NOT `sanitize`, so both sinks (rendered text and scope key) agree and an IPv4-lookalike object name survives — preserve that split exactly.
- **Retraction is not deletion.** `reader.go`'s known-gap doc paragraph and `diagnostics.md`'s "No logs" bullet are NARROWED to the new truth, never deleted.
- **Credentials rule:** synthetic names only in tests and docs (`shop/web-abc`, `worker-1`, `frontend`, `registry.example.com`); `10.96.x.x`/`10.244.x.x` are this repo's established synthetic fixture conventions; RFC 5737 IPs and RFC 2606 domains elsewhere. No real infra identifier anywhere. No secrets, no kubeconfig paths, nothing beyond `scheme://host` in anything kubeagent emits.
- **TDD:** write the failing test first, watch it fail, then implement. `go test` runs with `-p 2`, never `-short`. `gofmt` clean.
- **Every commit `git commit -s`** (DCO), authored solely by the human — NO `Co-Authored-By` and no AI attribution of any kind. Commit messages must not cite a path under `docs/testing/` or a scenario record ID.
- **DANGER: never run `./chaos/run.sh` in any form.** No cluster is needed for this slice.

---

### Task 1: Trace-primed opening message

**Files:**
- Create: `internal/investigate/prime.go`
- Create: `internal/investigate/prime_test.go`
- Modify: `internal/investigate/investigate.go:166-167` (the `firstUser` expression)
- Modify: `internal/investigate/investigate_test.go` (two new tests + one new import)

**Interfaces:**
- Consumes: `inventory.Workload.RootCauseTrace []inventory.Hypothesis`, `inventory.Workload.RootCauseConfidence string`, `inventory.Hypothesis{Cause string; Kind string; Verdict inventory.Verdict; Reason string}`, verdict constants `inventory.VerdictAttributed`/`VerdictRuledOut`/`VerdictOutranked` (values `"attributed"`/`"ruled_out"`/`"outranked"`) — all shipped by slice 1.
- Produces: `func renderTrace(workloads []inventory.Workload) string` (package-private; returns `""` when no workload carries a trace). Later tasks do not call it; only `Investigate` does.

- [ ] **Step 1: Write the failing tests**

Create `internal/investigate/prime_test.go`:

```go
package investigate

import (
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/inventory"
)

func TestRenderTrace_EmptyWhenNoWorkloadCarriesATrace(t *testing.T) {
	workloads := []inventory.Workload{
		{Kind: "Deployment", Namespace: "shop", Name: "web"},
	}
	if got := renderTrace(workloads); got != "" {
		t.Errorf("want empty string for traceless workloads, got %q", got)
	}
	if got := renderTrace(nil); got != "" {
		t.Errorf("want empty string for nil workloads, got %q", got)
	}
}

func TestRenderTrace_RendersEveryHypothesisWithSpacedVerdicts(t *testing.T) {
	workloads := []inventory.Workload{
		{
			Kind: "Deployment", Namespace: "shop", Name: "web",
			RootCauseConfidence: "high",
			RootCauseTrace: []inventory.Hypothesis{
				{Cause: "node worker-3 (NotReady)", Kind: "node", Verdict: inventory.VerdictAttributed, Reason: "pod web-abc is scheduled on it"},
				{Cause: "PVC data-0 (ProvisioningFailed)", Kind: "pvc", Verdict: inventory.VerdictRuledOut, Reason: "not mounted by this workload's pods"},
				{Cause: "registry registry.example.com", Kind: "registry", Verdict: inventory.VerdictOutranked, Reason: "node worker-3 (NotReady) is the stronger cause"},
			},
		},
	}
	got := renderTrace(workloads)
	for _, want := range []string{
		"- shop/web (Deployment) [confidence: high]:",
		"considered node worker-3 (NotReady): attributed — pod web-abc is scheduled on it",
		"considered PVC data-0 (ProvisioningFailed): ruled out — not mounted by this workload's pods",
		"considered registry registry.example.com: outranked — node worker-3 (NotReady) is the stronger cause",
		"Verify each attributed cause",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trace missing %q\ngot:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ruled_out") {
		t.Errorf("verdict underscores must render as spaces, got:\n%s", got)
	}
}

func TestRenderTrace_SkipsTracelessWorkloadsButKeepsOthers(t *testing.T) {
	workloads := []inventory.Workload{
		{Kind: "Deployment", Namespace: "shop", Name: "healthy"},
		{
			Kind: "StatefulSet", Namespace: "shop", Name: "db",
			RootCauseTrace: []inventory.Hypothesis{
				{Cause: "node worker-1 (NotReady)", Kind: "node", Verdict: inventory.VerdictAttributed, Reason: "pod db-0 is scheduled on it"},
			},
		},
	}
	got := renderTrace(workloads)
	if !strings.Contains(got, "- shop/db (StatefulSet):") {
		t.Errorf("traced workload missing, got:\n%s", got)
	}
	if strings.Contains(got, "healthy") {
		t.Errorf("traceless workload must not appear, got:\n%s", got)
	}
}
```

Append to `internal/investigate/investigate_test.go` (add `"github.com/imantaba/kubeagent/internal/explain"` to its imports):

```go
// TestInvestigate_PrimesFirstUserWithTrace proves the spec's trace-primed
// opening: the first user message carries the deterministic hypothesis trace
// after the inventory prompt and before the closing instruction.
func TestInvestigate_PrimesFirstUserWithTrace(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	client := fake.NewSimpleClientset(pod)

	var gotFirstUser string
	c := &Client{newConversation: func(system, firstUser string, specs []toolSpec) conversation {
		gotFirstUser = firstUser
		return &fakeConv{t: t, replies: []reply{{Text: "done", Done: true}}}
	}}

	wl := []inventory.Workload{{
		Kind: "Deployment", Namespace: "shop", Name: "web",
		Pods:     []inventory.PodRow{{Name: "web-abc"}},
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "Pending", Reason: "node down", Evidence: "NotReady"}},
		RootCauseTrace: []inventory.Hypothesis{
			{Cause: "node worker-3 (NotReady)", Kind: "node", Verdict: inventory.VerdictAttributed, Reason: "pod web-abc is scheduled on it"},
		},
		RootCauseConfidence: "high",
	}}
	if _, err := c.Investigate(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, wl, client); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotFirstUser, "considered node worker-3 (NotReady): attributed — pod web-abc is scheduled on it") {
		t.Errorf("first user message missing the hypothesis trace:\n%s", gotFirstUser)
	}
	if !strings.HasSuffix(gotFirstUser, "Investigate the findings with the read-only tools, then explain.") {
		t.Errorf("first user message must still end with the investigate instruction:\n%s", gotFirstUser)
	}
}

// TestInvestigate_FirstUserUnchangedWithoutTrace pins the no-trace case to the
// pre-slice bytes: priming must cost nothing when there is nothing to prime.
func TestInvestigate_FirstUserUnchangedWithoutTrace(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	client := fake.NewSimpleClientset(pod)

	var gotFirstUser string
	c := &Client{newConversation: func(system, firstUser string, specs []toolSpec) conversation {
		gotFirstUser = firstUser
		return &fakeConv{t: t, replies: []reply{{Text: "done", Done: true}}}
	}}

	wl := []inventory.Workload{{
		Kind: "Deployment", Namespace: "shop", Name: "web",
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "ImagePullBackOff", Reason: "bad tag", Evidence: "ErrImagePull"}},
	}}
	if _, err := c.Investigate(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, wl, client); err != nil {
		t.Fatal(err)
	}
	want := explain.BuildInventoryPrompt(clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, wl) +
		"\n\nInvestigate the findings with the read-only tools, then explain."
	if gotFirstUser != want {
		t.Errorf("a traceless run's first message must be byte-identical to the pre-slice shape:\n%s", gotFirstUser)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -p 2 ./internal/investigate/ -run 'TestRenderTrace|TestInvestigate_PrimesFirstUser|TestInvestigate_FirstUserUnchanged' -v`
Expected: compile FAILURE — `undefined: renderTrace`.

- [ ] **Step 3: Implement `renderTrace` and wire it in**

Create `internal/investigate/prime.go`:

```go
package investigate

import (
	"fmt"
	"strings"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// renderTrace renders each flagged workload's deterministic root-cause
// hypothesis trace for the investigation's opening message, or "" when no
// workload carries one. It lives here and not in internal/explain on purpose:
// the trace names nodes, and --explain's payload deliberately excludes node
// names, while --investigate already surfaces node names through its tools —
// the two egress boundaries differ and must stay separate.
// explain.BuildInventoryPrompt and --explain's payload are unchanged.
func renderTrace(workloads []inventory.Workload) string {
	var b strings.Builder
	for _, w := range workloads {
		if len(w.RootCauseTrace) == 0 {
			continue
		}
		fmt.Fprintf(&b, "- %s/%s (%s)", w.Namespace, w.Name, w.Kind)
		if w.RootCauseConfidence != "" {
			fmt.Fprintf(&b, " [confidence: %s]", w.RootCauseConfidence)
		}
		b.WriteString(":\n")
		for _, h := range w.RootCauseTrace {
			fmt.Fprintf(&b, "    considered %s: %s — %s\n",
				h.Cause, strings.ReplaceAll(string(h.Verdict), "_", " "), h.Reason)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\nThe deterministic pass already evaluated these root-cause hypotheses:\n" +
		b.String() +
		"\nVerify each attributed cause with the tools before relying on it, and spend the rest of the budget on what the deterministic pass could not explain — the workloads with no attributed cause and the findings behind the ruled-out candidates."
}
```

In `internal/investigate/investigate.go`, change the `firstUser` expression (currently lines 166-167):

```go
	firstUser := explain.BuildInventoryPrompt(cluster, summary, facts, serviceIssues, workloads) +
		renderTrace(workloads) +
		"\n\nInvestigate the findings with the read-only tools, then explain."
```

Nothing else in `investigate.go` changes in this task.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -p 2 ./internal/investigate/ -v`
Expected: ALL PASS (the new tests and every pre-existing test).

- [ ] **Step 5: Full verification and commit**

Run: `go build ./... && gofmt -l internal/ && go test -p 2 ./... && git diff --stat main -- go.mod go.sum`
Expected: build ok, no gofmt output, all packages pass, empty go.mod/go.sum diff.

```bash
git add internal/investigate/prime.go internal/investigate/prime_test.go internal/investigate/investigate.go internal/investigate/investigate_test.go
git commit -s -m "feat(investigate): prime the opening message with the root-cause hypothesis trace"
```

---

### Task 2: Close the error leak — `redact.Error` on every failed read

Ordered before Tasks 3 and 4 so their new arms land on the corrected pattern.

**Files:**
- Modify: `internal/investigate/reader.go` (ten `err.Error()` sites + the doc-comment gap paragraph at lines 42-47)
- Test: `internal/investigate/reader_test.go` (one new table-driven test)

**Interfaces:**
- Consumes: `redact.Error(err error) string` (already imported in `reader.go`): walks any `*url.Error` chain, reducing the URL at every level to `scheme://host`; a non-`*url.Error` returns `err.Error()` unchanged.
- Produces: nothing new — same `toolResult` shapes, error content reduced.

**The exact ten sites** (all are `errResult(<id>, err.Error())` following a failed client-go call; replace `err.Error()` with `redact.Error(err)` at each, and ONLY at these — the `"invalid input: "+err.Error()` sites after `json.Unmarshal` are NOT client-go reads: their input is the model's own bytes and a JSON syntax error carries no URL, and `redact.Error` would pass it through unchanged anyway):

1. `describe` pod arm — `reader.go:112`
2. `describe` node arm — `reader.go:120`
3. `describe` pvc arm — `reader.go:126`
4. `describeWorkload` deployment arm — `reader.go:181`
5. `describeWorkload` replicaset arm — `reader.go:191`
6. `describeWorkload` statefulset arm — `reader.go:198`
7. `describeWorkload` daemonset arm — `reader.go:204`
8. `describeWorkload` job arm — `reader.go:211`
9. `getEvents` list — `reader.go:253`
10. `getRelated` pod get — `reader.go:279`

Representative before/after (site 1):

```go
	// before
	if err != nil {
		return errResult(c.ID, err.Error())
	}
	// after
	if err != nil {
		return errResult(c.ID, redact.Error(err))
	}
```

- [ ] **Step 1: Write the failing test**

Append to `internal/investigate/reader_test.go`. Add these imports: `"errors"`, `"net/url"`, `"k8s.io/apimachinery/pkg/runtime"`, `k8stesting "k8s.io/client-go/testing"`.

```go
// TestReader_FailedReads_ReduceViaRedactError proves the reader.go doc
// comment's closed gap: a failed client-go read reaches the model as
// op + scheme://host + cause — never the request path or query.
func TestReader_FailedReads_ReduceViaRedactError(t *testing.T) {
	failure := &url.Error{
		Op:  "Get",
		URL: "https://10.96.0.1:6443/api/v1/namespaces/shop/pods/web-abc?timeout=30s",
		Err: errors.New("connection refused"),
	}
	tests := []struct {
		name     string
		resource string // the fake clientset resource the reactor intercepts
		verb     string
		call     toolCall
	}{
		{"describe pod", "pods", "get", call("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "web-abc"})},
		{"describe node", "nodes", "get", call("describe", map[string]string{"kind": "node", "namespace": "", "name": "worker-1"})},
		{"describe pvc", "persistentvolumeclaims", "get", call("describe", map[string]string{"kind": "pvc", "namespace": "shop", "name": "data-0"})},
		{"describe deployment", "deployments", "get", call("describe", map[string]string{"kind": "deployment", "namespace": "shop", "name": "web"})},
		{"describe replicaset", "replicasets", "get", call("describe", map[string]string{"kind": "replicaset", "namespace": "shop", "name": "web-rs"})},
		{"describe statefulset", "statefulsets", "get", call("describe", map[string]string{"kind": "statefulset", "namespace": "shop", "name": "db"})},
		{"describe daemonset", "daemonsets", "get", call("describe", map[string]string{"kind": "daemonset", "namespace": "shop", "name": "logger"})},
		{"describe job", "jobs", "get", call("describe", map[string]string{"kind": "job", "namespace": "shop", "name": "migrate"})},
		{"get_events", "events", "list", call("get_events", map[string]string{"namespace": "shop", "name": "web-abc"})},
		{"get_related", "pods", "get", call("get_related", map[string]string{"namespace": "shop", "name": "web-abc", "relation": "owner"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor(tt.verb, tt.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, failure
			})
			s := NewScope(nil)
			s.Add("pod", "shop", "web-abc")
			s.Add("node", "", "worker-1")
			s.Add("pvc", "shop", "data-0")
			s.Add("deployment", "shop", "web")
			s.Add("replicaset", "shop", "web-rs")
			s.Add("statefulset", "shop", "db")
			s.Add("daemonset", "shop", "logger")
			s.Add("job", "shop", "migrate")
			r := Reader{client: client}
			res := r.execute(context.Background(), tt.call, s)
			if !res.IsError {
				t.Fatalf("expected an error result, got %+v", res)
			}
			if !strings.Contains(res.Content, "https://10.96.0.1:6443") || !strings.Contains(res.Content, "connection refused") {
				t.Errorf("want op + scheme://host + cause to survive, got %q", res.Content)
			}
			for _, leaked := range []string{"/api/v1/namespaces", "timeout=30s"} {
				if strings.Contains(res.Content, leaked) {
					t.Errorf("request path/query leaked into the tool result: %q", res.Content)
				}
			}
		})
	}
}
```

(`10.96.0.1:6443` is this repo's established synthetic in-cluster API address fixture.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -p 2 ./internal/investigate/ -run TestReader_FailedReads_ReduceViaRedactError -v`
Expected: FAIL on every subtest — the raw `err.Error()` carries `/api/v1/namespaces` and `timeout=30s`.

- [ ] **Step 3: Apply the ten-site sweep**

Replace `err.Error()` with `redact.Error(err)` at exactly the ten sites listed above. `redact` is already imported.

- [ ] **Step 4: Narrow the doc-comment gap paragraph (retraction is not deletion)**

Replace the second paragraph of the `Reader` doc comment (`reader.go:42-47`) with:

```go
// That does not make a tool result address-free. When a client-go Get or List
// call in this file fails, the error reaches the model through redact.Error,
// which walks any *url.Error chain and reduces the URL at every level to
// scheme://host: the request path and query are dropped, and the API server's
// address survives only in that reduced form -- the same shape the CLI's own
// enrichment-failure notice uses. What redact.Error does not rewrite is error
// text outside a *url.Error -- an API status message, say -- which is the
// cluster's own text and can quote whatever it likes. The unfiltered
// err.Error() gap this paragraph used to record is closed; what remains is
// the scheme://host reduction itself and that non-URL cause text.
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -p 2 ./internal/investigate/ -v`
Expected: ALL PASS, including the pre-existing egress tests (`TestReader_DescribePod_URLResidualSurvivesRedaction` and friends are untouched — they exercise successful reads, not failures).

- [ ] **Step 6: Full verification and commit**

Run: `go build ./... && gofmt -l internal/ && go test -p 2 ./... && git diff --stat main -- go.mod go.sum`
Expected: build ok, no gofmt output, all pass, empty dependency diff.

```bash
git add internal/investigate/reader.go internal/investigate/reader_test.go
git commit -s -m "fix(investigate): reduce failed-read errors via redact.Error before they reach the model"
```

---

### Task 3: The `get_log_causes` tool

**Files:**
- Modify: `internal/investigate/reader.go` (new input type + two functions, one `execute` case, first doc-comment paragraph narrowed)
- Modify: `internal/investigate/tools.go` (fourth `toolSpec`)
- Modify: `internal/investigate/investigate.go` (one `label` case; one sentence of `investigateSuffix`)
- Modify: `internal/investigate/tools_test.go` (one table row)
- Modify: `internal/investigate/investigate_test.go` (the `len(specs) != 3` assertion becomes `!= 4` — touch NOTHING else in that file; Task 1's tests stay as committed)
- Test: `internal/investigate/reader_test.go` (unit tests for `logCauseResult`, integration tests through `execute`)

**Interfaces:**
- Consumes: `collect.PreviousLogs(ctx, client, ns, pod, container) (string, bool, error)` — the bounded 25-line previous-instance tail `--logs` uses; `(_, false, nil)` means no previous instance. `logscan.Classify(log string) logscan.Clue` — `Clue{Signature, Excerpt, Cause string}`; zero `Clue` for empty or placeholder-only logs; every non-empty `Cause` is a fixed string, with fallback `"last output before exit (no signature in the last 25 lines)"`.
- Produces: `func logCauseResult(id, namespace, pod, container, log string, ok bool, err error) toolResult` (pure, fully unit-testable) and `func (r Reader) getLogCauses(ctx context.Context, c toolCall, scope *Scope) toolResult` (thin fetch wrapper). Tool name `"get_log_causes"`, required inputs `namespace`, `pod`, `container`.

The split exists because the fake clientset's `GetLogs` serves a fixed `"fake logs"` body — reactors can inject errors but never content — so every content-dependent arm is tested through the pure helper, and the fake-body integration test doubles as the excerpt-never-crosses proof.

- [ ] **Step 1: Write the failing tests**

Append to `internal/investigate/reader_test.go`. Add imports: `apierrors "k8s.io/apimachinery/pkg/api/errors"`, `"k8s.io/apimachinery/pkg/runtime/schema"` (`"errors"`, `runtime`, `k8stesting` are present from Task 2).

```go
// logCauseResult is pure so every arm is testable: the fake clientset's
// GetLogs serves a fixed body, so content-dependent arms cannot be driven
// through it.
func TestLogCauseResult_RefusalNamesPodsLogPermission(t *testing.T) {
	for _, err := range []error{
		apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "web-abc", errors.New("no access")),
		apierrors.NewUnauthorized("no bearer token"),
	} {
		res := logCauseResult("t1", "shop", "web-abc", "app", "", false, err)
		if !res.IsError {
			t.Fatalf("expected an error result for %v", err)
		}
		want := "reading the previous log of shop/web-abc was refused: this identity lacks the pods/log get permission"
		if res.Content != want {
			t.Errorf("content = %q, want %q", res.Content, want)
		}
	}
}

func TestLogCauseResult_OtherErrorReducesViaRedactError(t *testing.T) {
	err := &url.Error{
		Op:  "Get",
		URL: "https://10.96.0.1:6443/api/v1/namespaces/shop/pods/web-abc/log?previous=true",
		Err: errors.New("connection refused"),
	}
	res := logCauseResult("t1", "shop", "web-abc", "app", "", false, err)
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(res.Content, "https://10.96.0.1:6443") || strings.Contains(res.Content, "/api/v1/") {
		t.Errorf("want scheme://host only, got %q", res.Content)
	}
}

func TestLogCauseResult_NoPreviousInstance(t *testing.T) {
	res := logCauseResult("t1", "shop", "web-abc", "app", "", false, nil)
	if res.IsError {
		t.Fatalf("no previous instance is not an error: %q", res.Content)
	}
	want := `no previous-instance log for shop/web-abc container "app" (nothing was refused; the container may not have restarted)`
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
}

// TestLogCauseResult_ExcerptNeverCrosses is the boundary proof: a classified
// log returns ONLY the fixed-vocabulary cause. No line of the log itself —
// addresses, tokens, anything — reaches the result.
func TestLogCauseResult_ExcerptNeverCrosses(t *testing.T) {
	log := "dial tcp 10.96.0.10:6379: connect: connection refused\ntoken=not-a-real-routing-key"
	res := logCauseResult("t1", "shop", "web-abc", "app", log, true, nil)
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if !strings.HasPrefix(res.Content, "log cause: ") {
		t.Errorf("want a classified cause, got %q", res.Content)
	}
	for _, leaked := range []string{"10.96.0.10", "dial tcp", "not-a-real-routing-key", "token="} {
		if strings.Contains(res.Content, leaked) {
			t.Errorf("raw log content crossed the boundary: %q in %q", leaked, res.Content)
		}
	}
}

func TestLogCauseResult_FallbackCauseOnly(t *testing.T) {
	res := logCauseResult("t1", "shop", "web-abc", "app", "something odd happened", true, nil)
	want := "log cause: last output before exit (no signature in the last 25 lines)"
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
	if strings.Contains(res.Content, "something odd happened") {
		t.Errorf("the last line itself must not cross: %q", res.Content)
	}
}

func TestLogCauseResult_UnclassifiableLog(t *testing.T) {
	// A placeholder-only log classifies to the zero Clue (Cause == "").
	log := "unable to retrieve container logs for containerd://0123456789abcdef"
	res := logCauseResult("t1", "shop", "web-abc", "app", log, true, nil)
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	want := `the previous log of shop/web-abc container "app" has no classifiable output`
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
}

func TestReader_GetLogCauses_OutOfScopeIsRefused(t *testing.T) {
	r := Reader{client: fake.NewSimpleClientset()}
	s := NewScope(nil)
	res := r.execute(context.Background(), call("get_log_causes", map[string]string{"namespace": "shop", "pod": "web-abc", "container": "app"}), s)
	if !res.IsError || !strings.Contains(res.Content, "not in scope") {
		t.Errorf("out-of-scope pod must be refused, got %+v", res)
	}
}

// TestReader_GetLogCauses_WiredThroughExecute drives the real path: the fake
// clientset serves its fixed "fake logs" body, which classifies to the
// fallback cause — and the body itself must not appear in the result.
func TestReader_GetLogCauses_WiredThroughExecute(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_log_causes", map[string]string{"namespace": "shop", "pod": "web-abc", "container": "app"}), s)
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if res.Content != "log cause: last output before exit (no signature in the last 25 lines)" {
		t.Errorf("content = %q", res.Content)
	}
	if strings.Contains(res.Content, "fake logs") {
		t.Errorf("the raw log body crossed the boundary: %q", res.Content)
	}
}

func TestReader_GetLogCauses_ForbiddenViaReactor(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "web-abc", errors.New("no access"))
	})
	r := Reader{client: client}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_log_causes", map[string]string{"namespace": "shop", "pod": "web-abc", "container": "app"}), s)
	if !res.IsError || !strings.Contains(res.Content, "pods/log get permission") {
		t.Errorf("a forbidden read must name the missing permission, got %+v", res)
	}
	if strings.Contains(res.Content, "no access") {
		t.Errorf("the API error's own text must not pass through the refusal arm: %q", res.Content)
	}
}
```

In `internal/investigate/tools_test.go`, add one row to the hand-maintained table in `TestToolSpecs_RequiredMatchesWhatTheExecutorDereferences`, following the existing per-row comment style:

```go
		// getLogCauses dereferences namespace, pod and container: the scope
		// check needs namespace+pod, and PreviousLogs needs all three.
		{"get_log_causes", []string{"namespace", "pod", "container"}},
```

In `internal/investigate/investigate_test.go`, change ONLY the spec-count assertion in `TestInvestigate_RunsLoopAndReturnsReport`:

```go
		if len(specs) != 4 {
			t.Errorf("expected 4 tool specs, got %d", len(specs))
		}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -p 2 ./internal/investigate/ -run 'TestLogCauseResult|TestReader_GetLogCauses|TestToolSpecs|TestInvestigate_RunsLoop' -v`
Expected: compile FAILURE — `undefined: logCauseResult`.

- [ ] **Step 3: Implement**

In `internal/investigate/reader.go`, add imports `"github.com/imantaba/kubeagent/internal/collect"`, `"github.com/imantaba/kubeagent/internal/logscan"`, and `apierrors "k8s.io/apimachinery/pkg/api/errors"`, then add (after `getRelated`):

```go
// logCausesInput is the wire shape of a get_log_causes call.
type logCausesInput struct{ Namespace, Pod, Container string }

// getLogCauses classifies the previous-instance log tail of an in-scope pod's
// container and returns only the classified cause. The raw excerpt never
// crosses the model boundary: logscan.Clue.Excerpt is deliberately discarded,
// matching the report's policy split — a LogCause may cross after redaction, a
// LogExcerpt never does.
func (r Reader) getLogCauses(ctx context.Context, c toolCall, scope *Scope) toolResult {
	var in logCausesInput
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return errResult(c.ID, "invalid input: "+err.Error())
	}
	if !scope.Allowed("pod", in.Namespace, in.Pod) {
		return errResult(c.ID, fmt.Sprintf("pod %s/%s is not in scope for this investigation", in.Namespace, in.Pod))
	}
	log, ok, err := collect.PreviousLogs(ctx, r.client, in.Namespace, in.Pod, in.Container)
	return logCauseResult(c.ID, in.Namespace, in.Pod, in.Container, log, ok, err)
}

// logCauseResult turns one PreviousLogs answer into a tool result. It is split
// from the fetch so every arm is unit-testable: the fake clientset serves a
// fixed body for GetLogs, so only errors can be injected through it. The
// refusal arm's text is fixed — the API error's own message never passes
// through it — and the other-error arm reduces via redact.Error like every
// failed read in this file.
func logCauseResult(id, namespace, pod, container, log string, ok bool, err error) toolResult {
	switch {
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return errResult(id, fmt.Sprintf("reading the previous log of %s/%s was refused: this identity lacks the pods/log get permission", namespace, pod))
	case err != nil:
		return errResult(id, redact.Error(err))
	case !ok:
		return okResult(id, fmt.Sprintf("no previous-instance log for %s/%s container %q (nothing was refused; the container may not have restarted)", namespace, pod, container))
	}
	clue := logscan.Classify(log)
	if clue.Cause == "" {
		return okResult(id, fmt.Sprintf("the previous log of %s/%s container %q has no classifiable output", namespace, pod, container))
	}
	return okResult(id, "log cause: "+redact.Addresses(clue.Cause))
}
```

Add the `execute` case (between `get_related` and `default`):

```go
	case "get_log_causes":
		return r.getLogCauses(ctx, c, scope)
```

Narrow the FIRST paragraph of the `Reader` doc comment: change `— never env, secret data, container args, or logs —` to `— never env, secret data, container args, or a raw log line (get_log_causes reads a bounded previous-instance tail but returns only its classified cause) —`.

In `internal/investigate/tools.go`, append the fourth spec to `toolSpecs()`:

```go
		{
			Name:        "get_log_causes",
			Description: "Classify the previous-instance log tail (last 25 lines) of an in-scope pod's container into a plain-language crash cause. Returns only the classified cause string — never a raw log line.",
			Properties: map[string]any{
				"namespace": prop("the pod's namespace"),
				"pod":       prop("the pod's name"),
				"container": prop("the container name within the pod"),
			},
			Required: []string{"namespace", "pod", "container"},
		},
```

In `internal/investigate/investigate.go`, add a `label` case (before `default`):

```go
	case "get_log_causes":
		return fmt.Sprintf("log causes %s/%s container %s", m["namespace"], m["pod"], m["container"])
```

And extend `investigateSuffix`'s first sentence — change `describe an object, list its events, or resolve a related object (owner, node, PVC).` to `describe an object, list its events, resolve a related object (owner, node, PVC), or classify a crashed container's previous log into a cause.` (`TestInvestigateSuffix_HasFixedBudgetInstruction`'s four required phrases are untouched.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -p 2 ./internal/investigate/ -v`
Expected: ALL PASS.

- [ ] **Step 5: Full verification and commit**

Run: `go build ./... && gofmt -l internal/ && go test -p 2 ./... && git diff --stat main -- go.mod go.sum`
Expected: build ok, no gofmt output, all pass (including `plugin_manifest_test.go` and `internal/cli/plugin_flags_test.go` — no shipped skill or command names the investigate tool set), empty dependency diff.

```bash
git add internal/investigate/reader.go internal/investigate/reader_test.go internal/investigate/tools.go internal/investigate/tools_test.go internal/investigate/investigate.go internal/investigate/investigate_test.go
git commit -s -m "feat(investigate): add the get_log_causes tool (classified cause only, never a raw log line)"
```

---

### Task 4: The Service hop and the Service kind

**Files:**
- Modify: `internal/investigate/reader.go` (a `describe` case, a `describeService` method, a `getRelated` case, the `getRelated` default-arm message)
- Modify: `internal/investigate/tools.go` (describe + get_related descriptions and props)
- Modify: `internal/investigate/investigate.go` (one `investigateSuffix` parenthetical)
- Test: `internal/investigate/reader_test.go`

**Interfaces:**
- Consumes: `scope.Add(kind, namespace, name)` and `scope.Allowed(...)` (normKind lowercases, so `"service"` needs no normKind change); `labels.SelectorFromSet` / `labels.Set` from `k8s.io/apimachinery/pkg/labels` (inside an existing dependency — go.mod does not move); `redact.Error` (Task 2's pattern); `apierrors.IsNotFound` (import present from Task 3).
- Produces: relation value `"service"` on get_related; kind value `"service"` on describe. No new tool — the spec count stays 4 and `tools_test.go`'s table gains no row (both tools' Required lists are unchanged).

- [ ] **Step 1: Write the failing tests**

Append to `internal/investigate/reader_test.go`. Add import `"k8s.io/apimachinery/pkg/util/intstr"`.

```go
func TestReader_DescribeService_RendersStructuredShape(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: "shop"},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.96.14.203",
			Selector:  map[string]string{"tier": "frontend", "app": "web"},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(8080)},
			},
		},
	}
	ep := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: "shop"},
		Subsets: []corev1.EndpointSubset{{
			Addresses:         []corev1.EndpointAddress{{IP: "10.244.1.5"}, {IP: "10.244.2.6"}},
			NotReadyAddresses: []corev1.EndpointAddress{{IP: "10.244.3.7"}},
		}},
	}
	r := Reader{client: fake.NewSimpleClientset(svc, ep)}
	s := NewScope(nil)
	s.Add("service", "shop", "frontend")
	res := r.execute(context.Background(), call("describe", map[string]string{"kind": "service", "namespace": "shop", "name": "frontend"}), s)
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	for _, want := range []string{
		"service shop/frontend: type=ClusterIP",
		"port http 80/TCP -> 8080",
		"selector: app=web,tier=frontend", // sorted keys
		"ready endpoints: 2",              // not-ready addresses excluded
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("describe service missing %q, got:\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "10.96.14.203") {
		t.Errorf("ClusterIP must never render: %q", res.Content)
	}
	if strings.Contains(res.Content, "10.244.") {
		t.Errorf("endpoint addresses must never render: %q", res.Content)
	}
}

func TestReader_DescribeService_NoEndpointsObjectIsZero(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: "shop"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
	}
	r := Reader{client: fake.NewSimpleClientset(svc)}
	s := NewScope(nil)
	s.Add("service", "shop", "frontend")
	res := r.execute(context.Background(), call("describe", map[string]string{"kind": "service", "namespace": "shop", "name": "frontend"}), s)
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	for _, want := range []string{"selector: (none)", "ready endpoints: 0 (no Endpoints object)"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("missing %q, got:\n%s", want, res.Content)
		}
	}
}

func TestReader_DescribeService_OutOfScopeIsRefused(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: "shop"}}
	r := Reader{client: fake.NewSimpleClientset(svc)}
	res := r.execute(context.Background(), call("describe", map[string]string{"kind": "service", "namespace": "shop", "name": "frontend"}), NewScope(nil))
	if !res.IsError || !strings.Contains(res.Content, "not in scope") {
		t.Errorf("an out-of-scope service must be refused, got %+v", res)
	}
}

// TestReader_GetRelated_Service_AddsMatchesToScope proves the hop and its
// load-bearing guard: labels.SelectorFromSet of an EMPTY selector matches
// every pod, so a selectorless Service must be skipped explicitly.
func TestReader_GetRelated_Service_AddsMatchesToScope(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop", Labels: map[string]string{"app": "web"}}}
	matching := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: "shop"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "web"}},
	}
	other := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "shop"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "db"}},
	}
	selectorless := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "shop"},
	}
	r := Reader{client: fake.NewSimpleClientset(pod, matching, other, selectorless)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_related", map[string]string{"namespace": "shop", "name": "web-abc", "relation": "service"}), s)
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if !strings.Contains(res.Content, "Services selecting web-abc: frontend") {
		t.Errorf("matching service missing, got %q", res.Content)
	}
	for _, absent := range []string{"backend", "external"} {
		if strings.Contains(res.Content, absent) {
			t.Errorf("%q must not match, got %q", absent, res.Content)
		}
	}
	if !s.Allowed("service", "shop", "frontend") {
		t.Error("the matching service must enter scope")
	}
	for _, name := range []string{"backend", "external"} {
		if s.Allowed("service", "shop", name) {
			t.Errorf("service %q must not enter scope", name)
		}
	}
}

func TestReader_GetRelated_Service_NoMatchIsNotAnError(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop", Labels: map[string]string{"app": "web"}}}
	other := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "shop"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "db"}},
	}
	r := Reader{client: fake.NewSimpleClientset(pod, other)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_related", map[string]string{"namespace": "shop", "name": "web-abc", "relation": "service"}), s)
	if res.IsError {
		t.Fatalf("no match is not an error: %q", res.Content)
	}
	if res.Content != "no Service in shop selects pod web-abc" {
		t.Errorf("content = %q", res.Content)
	}
}

func TestReader_GetRelated_UnknownRelationNamesService(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_related", map[string]string{"namespace": "shop", "name": "web-abc", "relation": "secrets"}), s)
	if !res.IsError || !strings.Contains(res.Content, "want owner|node|pvc|service") {
		t.Errorf("the default arm must name the full relation set, got %+v", res)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -p 2 ./internal/investigate/ -run 'TestReader_DescribeService|TestReader_GetRelated_Service|TestReader_GetRelated_UnknownRelation' -v`
Expected: FAIL — describe returns `kind "service" is not supported for describe`, get_related returns `unknown relation "service" (want owner|node|pvc)`.

- [ ] **Step 3: Implement**

In `internal/investigate/reader.go`, add imports `"sort"` and `"k8s.io/apimachinery/pkg/labels"`.

Add a `describe` case (between `pvc` and `default`):

```go
	case "service":
		svc, err := r.client.CoreV1().Services(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return errResult(c.ID, redact.Error(err))
		}
		return okResult(c.ID, r.describeService(ctx, svc))
```

Add the method (after `describePVC`):

```go
// describeService renders a Service's structured shape: type, ports, selector
// and the count of ready endpoints behind it. It never renders an address
// kubeagent chose: no ClusterIP, no LoadBalancer ingress address, no endpoint
// IP. Port names, selector keys and values, and targetPort are API-validated
// syntax, so none pass through sanitize — the same reasoning as describePod's
// pod and node names. It is a method, not a free function like the other
// describe renderers, because the ready-endpoint count needs a second read.
func (r Reader) describeService(ctx context.Context, svc *corev1.Service) string {
	var b strings.Builder
	fmt.Fprintf(&b, "service %s/%s: type=%s\n", svc.Namespace, svc.Name, svc.Spec.Type)
	for _, p := range svc.Spec.Ports {
		fmt.Fprintf(&b, "  port %s %d/%s -> %s\n", p.Name, p.Port, p.Protocol, p.TargetPort.String())
	}
	if len(svc.Spec.Selector) == 0 {
		b.WriteString("  selector: (none)\n")
	} else {
		pairs := make([]string, 0, len(svc.Spec.Selector))
		for k, v := range svc.Spec.Selector {
			pairs = append(pairs, k+"="+v)
		}
		sort.Strings(pairs)
		fmt.Fprintf(&b, "  selector: %s\n", strings.Join(pairs, ","))
	}
	ep, err := r.client.CoreV1().Endpoints(svc.Namespace).Get(ctx, svc.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		b.WriteString("  ready endpoints: 0 (no Endpoints object)\n")
	case err != nil:
		fmt.Fprintf(&b, "  ready endpoints: unknown (%s)\n", redact.Error(err))
	default:
		ready := 0
		for _, ss := range ep.Subsets {
			ready += len(ss.Addresses)
		}
		fmt.Fprintf(&b, "  ready endpoints: %d\n", ready)
	}
	return b.String()
}
```

Add a `getRelated` case (between `pvc` and `default`):

```go
	case "service":
		svcs, err := r.client.CoreV1().Services(in.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return errResult(c.ID, redact.Error(err))
		}
		var names []string
		for _, svc := range svcs.Items {
			// A selectorless Service selects no pods, but
			// labels.SelectorFromSet of an empty set matches everything —
			// skip it explicitly or every Service in the namespace matches.
			if len(svc.Spec.Selector) == 0 {
				continue
			}
			if !labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(p.Labels)) {
				continue
			}
			// svc.Name is the listed object's own metadata.name, which the
			// API server validates — unlike the owner/node/pvc arms above,
			// whose names come from unvalidated fields of another object —
			// so no safetext.Line here. Both sinks get the same value.
			scope.Add("service", in.Namespace, svc.Name)
			names = append(names, svc.Name)
		}
		if len(names) == 0 {
			return okResult(c.ID, fmt.Sprintf("no Service in %s selects pod %s", in.Namespace, in.Name))
		}
		sort.Strings(names)
		return okResult(c.ID, fmt.Sprintf("Services selecting %s: %s\n", in.Name, strings.Join(names, ", ")))
```

Change the `getRelated` default arm to:

```go
		return errResult(c.ID, fmt.Sprintf("unknown relation %q (want owner|node|pvc|service)", in.Relation))
```

In `internal/investigate/tools.go`:
- describe Description: `(pod, deployment, replicaset, statefulset, daemonset, job, node, or pvc)` → `(pod, deployment, replicaset, statefulset, daemonset, job, node, pvc, or service)`.
- describe kind prop: `"one of: pod, deployment, replicaset, statefulset, daemonset, job, node, pvc"` → `"one of: pod, deployment, replicaset, statefulset, daemonset, job, node, pvc, service"`.
- get_related Description: `the owners its ownerReferences name, its node, or its PersistentVolumeClaims.` → `the owners its ownerReferences name, its node, its PersistentVolumeClaims, or the Services whose selectors match its labels.`
- get_related relation prop: `"one of: owner, node, pvc"` → `"one of: owner, node, pvc, service"`.

In `internal/investigate/investigate.go`, change `investigateSuffix`'s parenthetical `(owner, node, PVC)` to `(owner, node, PVC, Service)`.

No `normKind` change (`Scope.normKind` lowercases and `"service"` needs no alias), no `label` change (`get_related`'s label already renders any relation), no `nsFor` change (a Service is namespaced).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -p 2 ./internal/investigate/ -v`
Expected: ALL PASS.

- [ ] **Step 5: Full verification and commit**

Run: `go build ./... && gofmt -l internal/ && go test -p 2 ./... && git diff --stat main -- go.mod go.sum`
Expected: build ok, no gofmt output, all pass, empty dependency diff.

```bash
git add internal/investigate/reader.go internal/investigate/reader_test.go internal/investigate/tools.go internal/investigate/investigate.go
git commit -s -m "feat(investigate): teach get_related the service hop and describe the Service kind"
```

---

### Task 5: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md` (the "Agentic investigation (`--investigate`)" section, lines ~1236-1345)
- Modify: `CHANGELOG.md` (`## [Unreleased]`)
- Modify: `CLAUDE.md` (the roadmap's hypothesis-engine bullet)
- Modify: `website/docs/roadmap.md` (the post-1.0 remaining-work sentence, currently naming "a trace-primed `--investigate`" as still ahead, around line 572)

**Interfaces:**
- Consumes: the shipped behavior of Tasks 1-4 (four tools; trace priming; `redact.Error` on failed reads; classified-cause-only log egress).
- Produces: nothing for later tasks — this is the slice's public record.

**Honesty requirements that bind every edit in this task:**
- **The "No logs" bullet is REWRITTEN, not deleted** — retraction is not deletion. `--investigate` now genuinely fetches a bounded log tail on demand; the surviving promise is that no raw log line leaves the process.
- Never blur read-only with makes-no-external-calls; the trace priming deliberately places node names in the prompt (a sanctioned boundary difference from `--explain` — say so).
- No `(vX.Y.Z)` parenthetical in CLAUDE.md — the `release:` commit adds it, never a docs commit.

- [ ] **Step 1: Update `website/docs/features/diagnostics.md`**

In the "Agentic investigation (`--investigate`)" section, apply exactly these changes:

1. The intro sentence listing what the model can do gains the two new capabilities: it can describe a flagged object, list its events, hop to related resources (the owners its ownerReferences name, its node, its PersistentVolumeClaims, or the Services whose selectors match its labels), and classify a crashed container's previous-log tail into a plain-language cause.

2. Add a new paragraph (or bullet, matching the section's structure) after the intro, before "Reachable scope": **Trace-primed.** The loop's first message now carries the deterministic root-cause hypothesis trace — the same per-workload `considered … : attributed/ruled out/outranked` lines `scan --why` prints — with an instruction to verify the attributed causes with the tools and spend the budget on what the deterministic pass could not explain. This is a difference from `--explain` by design: `--explain`'s payload excludes node names, while `--investigate`'s prompt and tools already surface them.

3. "the three tools gate differently" → "the four tools gate differently", and extend the gating description: `describe`'s kind list gains `service`; `get_related` offers `owner`, `node`, `pvc` or `service`; `get_log_causes` reads only a pod already in scope (same gate as `describe pod`); `get_events`' name-only gating is unchanged.

4. The finding-families paragraph: a Service named by a Service issue is now describable, but it enters scope **only** via the `service` hop from an in-scope pod — scope seeding is unchanged, so a Service-issues-only run still begins with nothing reachable.

5. REWRITE the "No logs" bullet as: **Logs never cross raw.** `get_log_causes` reads the same bounded previous-instance tail (last 25 lines) that `--logs` uses, on demand and whether or not `--logs` was passed — but only the classified cause crosses the model boundary: a fixed-vocabulary string, address-redacted. No raw log line ever leaves the process; the excerpt `--logs` renders locally is deliberately discarded here. A refused read returns a fixed refusal naming the `pods/log` permission.

6. The "Bounded egress" bullet's enumeration of what leaves the process gains: the hypothesis trace (cause, verdict, reason and confidence per flagged workload), the classified log cause, and — for a failed read — the API server address reduced to `scheme://host` plus the error's non-URL cause text (no request path or query; previously the raw error string).

7. Everything else in the section (Anthropic-only, Supersedes `--explain`, Capped at 8 reads / 6 turns, Never writes, Model selection) stays as is.

- [ ] **Step 2: Update `CHANGELOG.md` under `## [Unreleased]`**

```markdown
### Added

- `scan --investigate` is smarter per tool call (the hypothesis engine's
  second slice). The loop's first message now carries the deterministic
  root-cause hypothesis trace — the same considered/attributed/ruled-out
  lines `scan --why` prints — with an instruction to verify the attributed
  causes and spend the budget on what the deterministic pass could not
  explain. A new `get_log_causes` tool classifies the bounded
  previous-instance log tail (the same 25-line read `--logs` uses) into a
  plain-language cause: only the classified, address-redacted cause string
  crosses the model boundary, never a raw log line, and a refused read names
  the `pods/log` permission. `get_related` learns the `service` relation
  (the Services whose selectors match the pod's labels) and `describe`
  learns the Service kind (type, ports, selector, ready-endpoint count —
  never a ClusterIP or endpoint address). Loop bounds are unchanged
  (8 reads, 6 turns), and no JSON schema moves.

### Fixed

- A failed read inside the `--investigate` loop no longer returns the raw
  client-go error to the model. The error now reduces through the same
  redaction the CLI's enrichment-failure notice uses: any URL in the error
  chain is cut to `scheme://host`, dropping the request path and query that
  the previous behavior leaked.
```

- [ ] **Step 3: Update `CLAUDE.md` and `website/docs/roadmap.md`**

- CLAUDE.md: in the roadmap section's hypothesis-engine bullet, record that slice 2 has shipped — the trace-primed `--investigate`, the `get_log_causes` tool (classified cause only; a raw log line never crosses), the Service hop/kind, and failed reads reduced via `redact.Error` — and narrow the bullet's remaining-work clause to the chaos correctness corpus (plus whatever it already names beyond this spec). NO version parenthetical.
- roadmap.md: the post-1.0 row that ends "…and the hypothesis engine's remaining slices (a trace-primed `--investigate`, the chaos correctness corpus), still ahead" narrows to name only the chaos correctness corpus as still ahead, moving the trace-primed `--investigate` into the shipped clause with one line on what it added.

- [ ] **Step 4: Build the docs strictly**

Run: `cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/mkdocs build --strict -f mkdocs.yml; cd /home/ubuntu/git/kubeagent`
Expected: exit 0, no WARNING lines about the edited pages. (The working directory persists between shell calls — always cd back to the repo root.)

- [ ] **Step 5: Full verification and commit**

Run: `go build ./... && go test -p 2 ./... && git diff --stat main -- go.mod go.sum internal/report/testdata/golden-scan.txt website/docs/quickstart.md`
Expected: all pass; the diff-stat is empty (no dependency, golden or quickstart movement anywhere in the slice).

```bash
git add website/docs/features/diagnostics.md CHANGELOG.md CLAUDE.md website/docs/roadmap.md
git commit -s -m "docs: record smart --investigate (trace priming, get_log_causes, service hop, redacted errors)"
```
