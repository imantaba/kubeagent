# kubeagent v3 — Phase A Plan: model selection for `--explain`

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the user choose the Claude model used by `--explain`, with precedence `--model` flag › `KUBEAGENT_MODEL` env › `claude-opus-4-8`.

**Architecture:** A pure `explain.ResolveModel(flag, env)` implements the precedence; `explain.New(model)` takes the resolved model and the SDK adapter passes `anthropic.Model(model)` to the request; `main` adds a `--model` flag (default empty so the env can win) and resolves it. The model is only used when `--explain` is set; an unknown model is rejected by the API at call time.

**Tech Stack:** Go 1.26, stdlib `flag`, existing `github.com/anthropics/anthropic-sdk-go`. No new dependency.

## Global Constraints

- Module path: `github.com/imantaba/kubeagent`; Go 1.26.
- **Read-only** against the cluster; **sequential** (no goroutines); CLI uses standard-library `flag` only.
- **Model precedence:** `--model` flag › `KUBEAGENT_MODEL` env › default `claude-opus-4-8`.
- **`--explain` stays additive:** no behavior change when the flag is absent; the model is consulted only on the `--explain` path.
- No new third-party module.
- Each task keeps `go build ./...` and `go test ./...` green.

---

## File Structure

- `internal/explain/explain.go` — **modify.** Add `DefaultModel` const + `ResolveModel`; change `New()` → `New(model string)`; give the SDK adapter a `model` field.
- `internal/explain/explain_test.go` — **modify.** Add `TestResolveModel` (the existing fake-based tests are unaffected — they construct `&Client{s: fake}`).
- `main.go` — **modify.** Add the `--model` flag, resolve precedence, pass to `explain.New`; update the usage string.
- `main_test.go` — **modify.** Add a test that `--model` is a recognized flag (still fails fast on a missing key).
- `README.md` — **modify.** Document `--model` + `KUBEAGENT_MODEL` precedence.

---

## Task 1: `explain` — model parameter + precedence helper

**Files:**
- Modify: `internal/explain/explain.go`
- Modify: `internal/explain/explain_test.go`
- Modify: `main.go` (one call site, to keep the build green)

**Interfaces:**
- Produces: `const DefaultModel = "claude-opus-4-8"`; `func ResolveModel(flagVal, envVal string) string`; `func New(model string) *Client` (was `New()`).
- Unchanged: `(*Client).Explain(ctx, findings) (string, error)`, the `summarizer` seam.

- [ ] **Step 1: Write the failing test**

Add to `internal/explain/explain_test.go`:

```go
func TestResolveModel(t *testing.T) {
	cases := []struct {
		name, flag, env, want string
	}{
		{"flag wins over env and default", "claude-opus-4-8", "claude-sonnet-4-6", "claude-opus-4-8"},
		{"env used when flag empty", "", "claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"default when both empty", "", "", DefaultModel},
		{"flag wins when env empty", "claude-haiku-4-5", "", "claude-haiku-4-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveModel(tc.flag, tc.env); got != tc.want {
				t.Errorf("ResolveModel(%q, %q) = %q, want %q", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/explain/ -run TestResolveModel 2>&1 | tail -8
```
Expected: FAIL — compile error: undefined `ResolveModel` (and `DefaultModel`).

- [ ] **Step 3: Update the implementation**

In `internal/explain/explain.go`, add the const + helper near the top (after the imports / `systemPrompt`):

```go
// DefaultModel is used when neither --model nor KUBEAGENT_MODEL is set.
const DefaultModel = "claude-opus-4-8"

// ResolveModel picks the model by precedence: flag, then env, then DefaultModel.
func ResolveModel(flagVal, envVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if envVal != "" {
		return envVal
	}
	return DefaultModel
}
```

Change `New` to take a model and default an empty value:

```go
// New returns a Client backed by the Anthropic API, using the given model
// (empty falls back to DefaultModel). The SDK reads ANTHROPIC_API_KEY.
func New(model string) *Client {
	if model == "" {
		model = DefaultModel
	}
	return &Client{s: anthropicSummarizer{client: anthropic.NewClient(), model: model}}
}
```

Give the adapter a `model` field and use it in the request:

```go
// anthropicSummarizer is the real summarizer, backed by the Anthropic SDK.
type anthropicSummarizer struct {
	client anthropic.Client
	model  string
}

func (a anthropicSummarizer) summarize(ctx context.Context, prompt string) (string, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 1024,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(tb.Text)
		}
	}
	return out.String(), nil
}
```

(`Explain`, `buildPrompt`, the `summarizer` interface, and `Client` are unchanged.)

- [ ] **Step 4: Keep the build green — update the `main.go` call site**

In `main.go`, change the explain call from:

```go
		explanation, err = explain.New().Explain(ctx, findings)
```
to:
```go
		explanation, err = explain.New(explain.DefaultModel).Explain(ctx, findings)
```
(Task 2 replaces `explain.DefaultModel` with the resolved model.)

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go build ./...
go test ./internal/explain/ -v 2>&1 | tail -20
go vet ./internal/explain/
```
Expected: module builds; all explain tests pass (the 5 existing + `TestResolveModel`); vet clean.

- [ ] **Step 6: Commit**

```bash
git add internal/explain/ main.go
git commit -m "feat(explain): make the model selectable (New(model) + ResolveModel)" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: `main` — `--model` flag + precedence wiring + docs

**Files:**
- Modify: `main.go`
- Modify: `main_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `explain.ResolveModel(flag, env)` and `explain.New(model)` (Task 1).

- [ ] **Step 1: Add the failing test**

Add to `main_test.go` (it already imports `strings`):

```go
func TestRun_ModelFlagIsRecognized(t *testing.T) {
	// --model must be a known flag: with it set and no API key, the error is
	// the fail-fast key error, NOT "flag provided but not defined".
	t.Setenv("ANTHROPIC_API_KEY", "")
	err := run([]string{"scan", "--explain", "--model", "claude-sonnet-4-6"})
	if err == nil {
		t.Fatal("expected the fail-fast API-key error")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected ANTHROPIC_API_KEY error (proves --model parsed), got: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestRun_ModelFlagIsRecognized 2>&1 | tail -8
```
Expected: FAIL — `--model` isn't defined yet, so `flag.Parse` returns "flag provided but not defined: -model", which does not contain "ANTHROPIC_API_KEY".

- [ ] **Step 3: Add the flag and resolve precedence in `main.go`**

Add the flag alongside the others in `run` (after the `explainFlag` line):

```go
	model := fs.String("model", "", "Claude model for --explain (default: $KUBEAGENT_MODEL or claude-opus-4-8)")
```

Change the explain call (from Task 1's placeholder) to resolve precedence:

```go
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).Explain(ctx, findings)
```

Update the usage string to include `[--model name]`:

```go
		return fmt.Errorf("usage: kubeagent scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json] [--explain] [--model name]")
```

(`os` is already imported.)

- [ ] **Step 4: Run the suite + build**

```bash
go test ./... 2>&1
go vet ./... 2>&1
go build -o kubeagent .
ANTHROPIC_API_KEY= ./kubeagent scan --explain --model claude-sonnet-4-6 2>&1 | head -2
```
Expected: all packages PASS (incl. `TestRun_ModelFlagIsRecognized`); vet clean; binary builds; the last command prints the `--explain needs the ANTHROPIC_API_KEY environment variable` error (proving `--model` parsed and fail-fast still wins).

- [ ] **Step 5: Document in `README.md`**

In the Usage code block, add this example immediately after the `./kubeagent scan --explain` example:

```bash
# choose the model (default: claude-opus-4-8; or set KUBEAGENT_MODEL)
./kubeagent scan --explain --model claude-sonnet-4-6
```

And add this note immediately after the existing egress blockquote (the `> --explain sends only ...` paragraph):

```markdown
> Model precedence for `--explain`: the `--model` flag, then the
> `KUBEAGENT_MODEL` environment variable, then the default `claude-opus-4-8`.
```

- [ ] **Step 6: Commit**

```bash
git add main.go main_test.go README.md
git commit -m "feat: add --model flag and KUBEAGENT_MODEL for --explain" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-review

- **Spec coverage (model selection):** `ResolveModel` precedence flag›env›default ✅ (Task 1); `--model` flag + `KUBEAGENT_MODEL` ✅ (Task 2); default `claude-opus-4-8` ✅ (`DefaultModel`); model only used on `--explain` path ✅; unknown model left to the API ✅ (no pre-validation); README documents it ✅.
- **Placeholder scan:** none — every step has complete code/commands.
- **Type consistency:** `New(model string)` (Task 1) matches the Task 1 main call-site update and the Task 2 call; `ResolveModel(flagVal, envVal string) string` matches its Task 2 use; the adapter's new `model` field is used via `anthropic.Model(a.model)`.
- **Scope:** Phase A only — no inventory/cluster-health changes here; those are Phases B–D.
