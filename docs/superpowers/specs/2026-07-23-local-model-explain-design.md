# Local-model (offline) `--explain` — design

**Status:** approved · **Date:** 2026-07-23 · **Type:** LLM-path backend (Theme C, principled intelligence — slice: local/offline explain)

## Problem

`--explain` today makes one call to the Anthropic API and requires `ANTHROPIC_API_KEY`
— so it is unavailable in air-gapped or key-less environments, and it sends the
(structured, secret-free) summary to a third party. Many teams run a local LLM
(Ollama, vLLM, llama.cpp, LM Studio) that exposes the OpenAI-compatible
`/chat/completions` API. This adds a **local-model backend**: when
`KUBEAGENT_EXPLAIN_ENDPOINT` is set, `--explain` talks to that endpoint instead —
no Anthropic key, and nothing leaves the local network. The deterministic offline
core (`scan`, `--suggest`) is unchanged, and the LLM prompt (Fix-first ranking +
deterministic-command grounding) is identical.

## Behavior (approved)

- Default (no `KUBEAGENT_EXPLAIN_ENDPOINT`): `--explain` uses the Anthropic backend
  exactly as today (needs `ANTHROPIC_API_KEY`).
- With `KUBEAGENT_EXPLAIN_ENDPOINT` set (e.g. `http://localhost:11434/v1`):
  `--explain` POSTs to `<endpoint>/chat/completions` (OpenAI-compatible) and does
  **not** require `ANTHROPIC_API_KEY`. It **does** require `--model` (or
  `KUBEAGENT_MODEL`) to name the local model (e.g. `llama3.1`) — there is no
  universal default.
- `KUBEAGENT_EXPLAIN_API_KEY`, when set, is sent as `Authorization: Bearer <key>`;
  when unset, no auth header is sent (local Ollama needs none).

## Design

### 1. `internal/explain/local.go` — the OpenAI-compatible backend (new file)

Plain `net/http` + `encoding/json` (no new dependency — the chat-completions
request/response is a few fields). Implements the existing `summarizer` interface.

```go
// openaiSummarizer talks to an OpenAI-compatible /chat/completions endpoint
// (Ollama, vLLM, llama.cpp, LM Studio, …). Read-only: it sends the same structured
// prompt as the Anthropic backend to a model on the caller's own network.
type openaiSummarizer struct {
	endpoint string       // e.g. "http://localhost:11434/v1"
	model    string       // e.g. "llama3.1"
	apiKey   string       // optional bearer token ("" = no auth header)
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
	body, _ := json.Marshal(chatRequest{
		Model:  o.model,
		Stream: false,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		},
	})
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
```

- `snippet(raw []byte) string` — a small helper trimming the body to ~200 runes for
  the error message (no secret risk; it's the endpoint's error text). Lives in this
  file.
- The `1<<20` read cap bounds a misbehaving endpoint.

### 2. `explain.NewFromConfig` — backend selection

Add one exported constructor that chooses the backend; keep `New` for back-compat.

```go
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

`New(model string) *Client` becomes `return NewFromConfig(model, "", "")` (its only
caller is `main.go`; the tests construct `&Client{s: fake}` directly and are
unaffected).

### 3. `main.go` — wiring

- Read the two envs near the existing `--explain` handling:
  `endpoint := os.Getenv("KUBEAGENT_EXPLAIN_ENDPOINT")`,
  `explainKey := os.Getenv("KUBEAGENT_EXPLAIN_API_KEY")`.
- Replace the precondition (currently: `--explain` needs `ANTHROPIC_API_KEY`) with:
  ```go
  if *explainFlag {
      if endpoint == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
          return fmt.Errorf("--explain needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model")
      }
  }
  ```
- Compute the model and require it for the local path (no Anthropic default):
  ```go
  var explainModel string
  if endpoint != "" {
      explainModel = firstNonEmpty(*model, os.Getenv("KUBEAGENT_MODEL")) // no DefaultModel fallback for local
      if *explainFlag && explainModel == "" {
          return fmt.Errorf("--explain with KUBEAGENT_EXPLAIN_ENDPOINT needs --model (or KUBEAGENT_MODEL) set to the local model name")
      }
  } else {
      explainModel = explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))
  }
  ```
  (`firstNonEmpty(a, b string) string` — a tiny local helper: returns `a` if
  non-empty else `b`. Add it in `main.go` if not already present.)
- Build the client via `explain.NewFromConfig(explainModel, endpoint, explainKey)`
  at the existing `--explain` call site (replacing `explain.New(explain.ResolveModel(...))`).
- Usage string: extend the `--explain` note to mention `KUBEAGENT_EXPLAIN_ENDPOINT`.
  The `--explain` flag help stays accurate ("summarize findings via one LLM call").

### 4. No prompt / core change

`systemPrompt` and `buildInventoryPrompt` are unchanged — the local model receives
the identical prompt (including the Fix-first ranking and deterministic-command
grounding). The offline `scan`/`--suggest`/golden output is unchanged. `--explain`
is still one call (no agentic loop — that is the separate `--investigate` slice).

## Global constraints

- **Opt-in; offline core unchanged.** No API call without `--explain`. The
  deterministic scan is byte-identical.
- **No new dependency** — the OpenAI-compatible client is `net/http` +
  `encoding/json`. (Keeps "one fast binary, minimal dependencies".)
- **Privacy** — with a local endpoint, the summary goes only to a model on the
  caller's network; no Anthropic key required. The prompt already excludes secrets,
  pod IPs, and env values (unchanged).
- **Gate:** touches `internal/explain` (new backend) + `main.go` (wiring) →
  **LIGHTWEIGHT** (`httptest`-server unit tests; no cluster, no real model).
  **Minor** bump v0.48.0 → **v0.49.0**; **chart PATCH** (no Helm/deploy/RBAC change).
- **Deterministic tests** — the local backend is tested against an in-process
  `httptest.Server`; no network, no model.
- `buildInventoryPrompt`, `systemPrompt`, `scan`, `report`, `--fix`, watch, and the
  golden snapshot stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Streaming responses (`stream:false` — one shot is enough for a summary);
model-capability negotiation / retries / backoff (one call, surface errors);
`--investigate` agentic follow-up reads (the next Theme-C slice); a local backend
for the `watch` daemon (the daemon does not call `--explain`); auto-discovery of a
local endpoint (explicit env only); temperature/token knobs (defaults are fine —
the endpoint's own config governs); Ollama-native `/api/generate` (Ollama already
speaks OpenAI-compatible).

## Testing

- **`openaiSummarizer.summarize` (against `httptest.Server`):**
  - the server records the request and returns
    `{"choices":[{"message":{"role":"assistant","content":"two nodes NotReady"}}]}`
    → `summarize` returns `"two nodes NotReady"`.
  - the recorded request body's messages are `[{role:system, content:<systemPrompt>},
    {role:user, content:<prompt>}]` and `model` is the configured model.
  - path is `<endpoint>/chat/completions`; `Content-Type: application/json`.
  - with `apiKey` set → the request carries `Authorization: Bearer <key>`; with
    `apiKey == ""` → no `Authorization` header.
  - a `500` response → error containing the status; a `200` with `{"choices":[]}` →
    a "no choices" error; a `200` with non-JSON → a parse error.
- **`NewFromConfig` selection (end-to-end via `httptest`):**
  - `NewFromConfig("llama3.1", server.URL, "").ExplainInventory(ctx, degradedCluster,
    …)` hits the test server and returns its content (proves the local backend is
    selected and wired through `ExplainInventory`).
  - `NewFromConfig("m", "", "")` yields a Client whose summarizer is the Anthropic
    one — assert indirectly: it does not hit the test server (or simply that
    construction succeeds; the Anthropic path is not unit-tested for network).
- **`main` preconditions (`run(...)`):**
  - `--explain` with no `ANTHROPIC_API_KEY` and no `KUBEAGENT_EXPLAIN_ENDPOINT` →
    the "needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT" error.
  - `--explain` with `KUBEAGENT_EXPLAIN_ENDPOINT` set but no `--model`/`KUBEAGENT_MODEL`
    → the "needs --model" error. (Use `t.Setenv`.)
- **Existing explain tests** (fake summarizer) — unchanged and passing.

## Files touched

- **Create:** `internal/explain/local.go` (+ `internal/explain/local_test.go`).
- **Modify:** `internal/explain/explain.go` — `NewFromConfig`; `New` delegates.
- **Modify:** `main.go` (+ `main_test.go`) — endpoint/api-key envs, relaxed precondition, model selection, `NewFromConfig` call, usage string.
- **Docs:** `website/docs/features/` (the `--explain` doc — local-model option), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
