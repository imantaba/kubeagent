# Local-model (offline) `--explain` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `--explain` talk to a local OpenAI-compatible model endpoint (`KUBEAGENT_EXPLAIN_ENDPOINT`) instead of Anthropic — no API key, nothing leaving the network.

**Architecture:** Add a second `summarizer` backend (`openaiSummarizer`, plain `net/http` → `/chat/completions`) behind the existing seam, a single `NewFromConfig(model, endpoint, apiKey)` selector, and `main.go` wiring that routes to the local backend when the endpoint env is set. The prompt, deterministic core, and Anthropic path are unchanged.

**Tech Stack:** Go 1.26 standard library (`net/http`, `encoding/json`), `httptest` for deterministic tests. No new dependency.

## Global Constraints

- **Opt-in; offline core unchanged.** No API call without `--explain`; `scan`/`--suggest`/golden output byte-identical.
- **No new dependency** — OpenAI-compatible client is `net/http` + `encoding/json`.
- **Selection:** `KUBEAGENT_EXPLAIN_ENDPOINT` set → local backend (no `ANTHROPIC_API_KEY` needed, but `--model`/`KUBEAGENT_MODEL` required); unset → Anthropic as today.
- **Auth:** `Authorization: Bearer <KUBEAGENT_EXPLAIN_API_KEY>` only when that env is set; else no auth header.
- Same `systemPrompt` + `buildInventoryPrompt` to the local model; still one call (no agentic loop).
- Gate: touches `internal/explain` + `main.go` → **LIGHTWEIGHT** (`httptest` unit tests). **Minor** bump v0.48.0 → **v0.49.0**; **chart PATCH** (no Helm/deploy/RBAC change).
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

---

### Task 1: The OpenAI-compatible backend + selector

**Files:**
- Create: `internal/explain/local.go`
- Test: `internal/explain/local_test.go`
- Modify: `internal/explain/explain.go` (`NewFromConfig`; `New` delegates)

**Interfaces:**
- Consumes: the existing `summarizer` interface (`summarize(ctx, prompt) (string, error)`), the `systemPrompt` const, `anthropicSummarizer`, `DefaultModel`, `Client`.
- Produces: `openaiSummarizer` (implements `summarizer`); `func NewFromConfig(model, endpoint, apiKey string) *Client`. Used by Task 2 (`main.go`).

- [ ] **Step 1: Write the failing tests**

Create `internal/explain/local_test.go`:

```go
package explain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
)

func TestOpenAISummarizer_PostsAndParses(t *testing.T) {
	var gotBody []byte
	var gotAuth, gotPath, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"two nodes NotReady"}}]}`)
	}))
	defer srv.Close()

	o := openaiSummarizer{endpoint: srv.URL, model: "llama3.1", apiKey: "sekret", http: srv.Client()}
	out, err := o.summarize(context.Background(), "PROMPT-BODY")
	if err != nil {
		t.Fatal(err)
	}
	if out != "two nodes NotReady" {
		t.Errorf("out = %q, want the response content", out)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("auth = %q, want Bearer sekret", gotAuth)
	}
	var req chatRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "llama3.1" {
		t.Errorf("model = %q", req.Model)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v, want [system,user]", req.Messages)
	}
	if req.Messages[0].Content != systemPrompt {
		t.Error("system message is not systemPrompt")
	}
	if req.Messages[1].Content != "PROMPT-BODY" {
		t.Error("user message is not the prompt")
	}
}

func TestOpenAISummarizer_NoAuthHeaderWhenNoKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	o := openaiSummarizer{endpoint: srv.URL, model: "m", apiKey: "", http: srv.Client()}
	if _, err := o.summarize(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("expected no Authorization header, got %q", gotAuth)
	}
}

func TestOpenAISummarizer_Errors(t *testing.T) {
	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "model overloaded")
	}))
	defer srv500.Close()
	if _, err := (openaiSummarizer{endpoint: srv500.URL, model: "m", http: srv500.Client()}).summarize(context.Background(), "p"); err == nil {
		t.Error("want an error on a 500 response")
	}

	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srvEmpty.Close()
	if _, err := (openaiSummarizer{endpoint: srvEmpty.URL, model: "m", http: srvEmpty.Client()}).summarize(context.Background(), "p"); err == nil {
		t.Error("want an error when the response has no choices")
	}
}

func TestNewFromConfig_LocalBackendEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"local says degraded"}}]}`)
	}))
	defer srv.Close()

	c := NewFromConfig("llama3.1", srv.URL, "")
	out, err := c.ExplainInventory(context.Background(),
		clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 1, NodesTotal: 2}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "local says degraded" {
		t.Errorf("out = %q, want the local endpoint's content", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ -run 'OpenAISummarizer|NewFromConfig' 2>&1 | head`
Expected: compile failure — `openaiSummarizer`, `chatRequest`, `NewFromConfig` undefined.

- [ ] **Step 3: Create `internal/explain/local.go`**

```go
// OpenAI-compatible /chat/completions backend for --explain, so the summary can be
// produced by a local model (Ollama, vLLM, llama.cpp, LM Studio, …) with no API key
// and no data leaving the caller's network. Read-only; one call, same prompt.
package explain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// openaiSummarizer talks to an OpenAI-compatible /chat/completions endpoint.
type openaiSummarizer struct {
	endpoint string // e.g. "http://localhost:11434/v1"
	model    string // e.g. "llama3.1"
	apiKey   string // optional bearer token ("" = no auth header)
	http     *http.Client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []chatMessage `json:"messages"`
}
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

func (o openaiSummarizer) summarize(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:  o.model,
		Stream: false,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(o.endpoint, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling local explain endpoint: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("local explain endpoint returned %d: %s", resp.StatusCode, snippet(raw))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("parsing local explain response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("local explain endpoint returned no choices")
	}
	return cr.Choices[0].Message.Content, nil
}

// snippet trims an endpoint's error body for inclusion in an error message.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
```

- [ ] **Step 4: Add `NewFromConfig` and delegate `New` (in `explain.go`)**

In `internal/explain/explain.go`, add `NewFromConfig` next to `New`, and make `New` delegate:

```go
// New returns a Client backed by the Anthropic API (empty model falls back to
// DefaultModel). The SDK reads ANTHROPIC_API_KEY.
func New(model string) *Client {
	return NewFromConfig(model, "", "")
}

// NewFromConfig returns a Client using the local OpenAI-compatible endpoint when
// endpoint is non-empty, otherwise the Anthropic backend. apiKey is the optional
// bearer token for the local endpoint (ignored by the Anthropic path).
func NewFromConfig(model, endpoint, apiKey string) *Client {
	if endpoint != "" {
		return &Client{s: openaiSummarizer{endpoint: endpoint, model: model, apiKey: apiKey, http: http.DefaultClient}}
	}
	if model == "" {
		model = DefaultModel
	}
	return &Client{s: anthropicSummarizer{client: anthropic.NewClient(), model: model}}
}
```

(Add `"net/http"` to `explain.go`'s imports if not already present — it is needed for `http.DefaultClient`. Remove the old body of `New` that constructed the `anthropicSummarizer` directly, since `NewFromConfig` now owns that.)

- [ ] **Step 5: Run the explain suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ 2>&1 | tail -5`
Expected: PASS — the four new tests and all existing explain tests (the fake-summarizer tests construct `&Client{s: fake}` directly and are unaffected; `New`'s behavior is unchanged for the Anthropic path).

- [ ] **Step 6: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/explain/
git add internal/explain/local.go internal/explain/local_test.go internal/explain/explain.go
git commit -m "feat(explain): add an OpenAI-compatible local-model backend"
```

---

### Task 2: `main.go` wiring

**Files:**
- Modify: `main.go` (endpoint/api-key envs, relaxed precondition, model selection, `firstNonEmpty` helper, `NewFromConfig` call, `--explain` flag help)
- Test: `main_test.go` (two precondition tests)

**Interfaces:**
- Consumes: `explain.NewFromConfig(model, endpoint, apiKey string) *Client` (Task 1); `explain.ResolveModel`; the existing `*explainFlag`, `*model` flags.
- Produces: end-to-end local-model routing for `--explain`.

- [ ] **Step 1: Write the failing tests**

Add to `main_test.go` (the `strings` import already present):

```go
func TestRun_ExplainNeedsKeyOrEndpoint(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := run([]string{"scan", "--explain"})
	if err == nil || !strings.Contains(err.Error(), "KUBEAGENT_EXPLAIN_ENDPOINT") {
		t.Fatalf("want the key-or-endpoint error, got %v", err)
	}
}

func TestRun_ExplainLocalNeedsModel(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "http://localhost:11434/v1")
	t.Setenv("KUBEAGENT_MODEL", "")
	err := run([]string{"scan", "--explain"})
	if err == nil || !strings.Contains(err.Error(), "needs --model") {
		t.Fatalf("want the needs-model error, got %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run 'TestRun_Explain' 2>&1 | tail`
Expected: FAIL — `TestRun_ExplainNeedsKeyOrEndpoint` currently errors with the OLD message (`ANTHROPIC_API_KEY environment variable`, no mention of `KUBEAGENT_EXPLAIN_ENDPOINT`); `TestRun_ExplainLocalNeedsModel` errors with the OLD message too (endpoint set but the old check only looks at `ANTHROPIC_API_KEY`).

- [ ] **Step 3: Replace the precondition + add model selection**

In `main.go`, replace the existing block:

```go
	// --explain needs an API key; check before running a full scan.
	if *explainFlag && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("--explain needs the ANTHROPIC_API_KEY environment variable")
	}
```

with:

```go
	// --explain needs Anthropic, or a local OpenAI-compatible endpoint; check before scanning.
	explainEndpoint := os.Getenv("KUBEAGENT_EXPLAIN_ENDPOINT")
	if *explainFlag && explainEndpoint == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("--explain needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model")
	}
	var explainModel string
	if explainEndpoint != "" {
		explainModel = firstNonEmpty(*model, os.Getenv("KUBEAGENT_MODEL")) // no Anthropic default for a local model
		if *explainFlag && explainModel == "" {
			return fmt.Errorf("--explain with KUBEAGENT_EXPLAIN_ENDPOINT needs --model (or KUBEAGENT_MODEL) set to the local model name")
		}
	} else {
		explainModel = explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))
	}
```

- [ ] **Step 4: Route the call to `NewFromConfig` + add the helper + flag help**

In `main.go`, change the `--explain` call site from:

```go
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).ExplainInventory(ctx, health, &summary, &facts, serviceIssues, result.Workloads)
```

to:

```go
		explanation, err = explain.NewFromConfig(explainModel, explainEndpoint, os.Getenv("KUBEAGENT_EXPLAIN_API_KEY")).ExplainInventory(ctx, health, &summary, &facts, serviceIssues, result.Workloads)
```

Add the helper (near the other small helpers in `main.go`):

```go
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
```

Update the `--explain` flag help string from
`"summarize findings via one Claude API call (needs ANTHROPIC_API_KEY)"` to
`"summarize findings via one LLM call (needs ANTHROPIC_API_KEY, or KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model)"`.

- [ ] **Step 5: Run the main + explain suites**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test . ./internal/explain/ 2>&1 | tail -5`
Expected: PASS (both new precondition tests and all existing tests).

- [ ] **Step 6: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w main.go
git add main.go main_test.go
git commit -m "feat(main): route --explain to a local model when KUBEAGENT_EXPLAIN_ENDPOINT is set"
```

---

### Task 3: Documentation

**Files:**
- Modify: the `--explain` doc (find it: `grep -rln -- '--explain' website/docs`), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs).

- [ ] **Step 1: Update the docs**

- The `--explain` feature doc: add a "local / offline model" note — set `KUBEAGENT_EXPLAIN_ENDPOINT` (e.g. `http://localhost:11434/v1` for Ollama) and `--explain` uses that OpenAI-compatible endpoint instead of Anthropic: no `ANTHROPIC_API_KEY`, nothing leaves the network. Note `--model`/`KUBEAGENT_MODEL` names the local model, and `KUBEAGENT_EXPLAIN_API_KEY` is an optional bearer token. The prompt (ranked, grounded) and offline core are unchanged.

- `README.md`: in the `--explain` section, mention the local-model option (`KUBEAGENT_EXPLAIN_ENDPOINT`, no key, on-network).

- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`:

  ```
  - **Local-model `--explain`.** Set `KUBEAGENT_EXPLAIN_ENDPOINT` (an OpenAI-compatible
    `/chat/completions` URL — Ollama, vLLM, llama.cpp, LM Studio) and `--explain` runs
    against that local model: no `ANTHROPIC_API_KEY`, and nothing leaves the network.
    `--model`/`KUBEAGENT_MODEL` names the local model; `KUBEAGENT_EXPLAIN_API_KEY` is an
    optional bearer token. Theme-C (principled intelligence) — offline/local explain.
  ```

- `website/docs/roadmap.md`: add a Shipped bullet after the ranked-`--explain` entry, tagged **Theme-C** (offline/local explain), noting `--explain` now runs against a local OpenAI-compatible model with no key/no egress; link to the `--explain` doc.

- [ ] **Step 2: Verify the docs build**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (venv fallback: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, no page WARNINGs.

- [ ] **Step 3: Run the whole suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add website/ README.md CHANGELOG.md
git commit -m "docs: document local-model (offline) --explain"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the `release` skill owns this. Touches only `internal/explain` + `main.go` (opt-in LLM path; offline core unchanged) → **LIGHTWEIGHT** gate. Validation is the deterministic `httptest` unit tests (no real model); if an Ollama/OpenAI-compatible endpoint is reachable, a live `KUBEAGENT_EXPLAIN_ENDPOINT=… scan --explain --model <m>` confirms the round trip. **Minor** bump **v0.48.0 → v0.49.0**; **chart PATCH** (no Helm/template change — the bump script's default patch is correct; do NOT override). Hold for the user's explicit "run release and push".
```
