# kubeagent v2 — Design: `--explain`

**Status:** approved design (pre-implementation)
**Date:** 2026-06-21

## Goal

Add an optional `--explain` flag to `kubeagent scan`. When set, after the
deterministic detectors run, kubeagent makes **one** Claude API call that turns
the structured findings into a plain-English summary plus suggested next steps.

`--explain` is purely additive: the deterministic core stays fully usable
**offline and with no API key**. Nothing about the existing scan/diagnose/report
behavior changes when the flag is absent.

## Invariants preserved

- **Read-only against the cluster.** The Claude call is outbound network I/O, not
  a cluster mutation. Detectors still only `List`/`Get`.
- **Sequential.** `--explain` adds one blocking call at the end of the pipeline.
  No goroutines.
- **stdlib `flag`** for the CLI — no Cobra.
- **Exit codes** unchanged: `0` ran successfully, `1` tool failed.

## New dependency

- `github.com/anthropics/anthropic-sdk-go` — the official Go SDK. This is the
  first third-party dependency beyond `client-go`. Chosen over hand-rolled
  `net/http` for idiomatic, typed requests and built-in error/retry handling.

## Architecture

One new stage hangs off the end of the existing pipeline, only when `--explain`
is set:

```
cluster → collect → diagnose ─┬─────────────────────────► report
                              └─ (if --explain) explain ──┘
```

### New package: `internal/explain`

Mirrors the isolation of the other stage packages.

- `buildPrompt(findings []diagnose.Finding) string` — a **pure function**,
  unit-tested with fake findings exactly like the detectors. Builds a compact
  prompt from the structured findings.
- A small client type wrapping the SDK, exposing
  `Explain(ctx context.Context, findings []diagnose.Finding) (string, error)`.
  The SDK call sits behind a **one-method interface seam** so tests use a fake
  and never touch the network — consistent with "I/O packages are tested without
  a real cluster."

## The Claude call

- **Model:** `claude-opus-4-8` (current default).
- **Request:** non-streaming, `max_tokens: 1024`. The output is a short paragraph
  plus a few next steps, so streaming is unnecessary.
- **System prompt:** instructs the model to act as a Kubernetes SRE, explain the
  findings in plain English, suggest concrete next steps, be concise, and
  **respond with only the explanation, no preamble.** The final-answer-only
  instruction prevents Opus 4.8 from leaking reasoning into the visible response
  when extended/adaptive thinking is not enabled.
- **Timeout:** the call runs under a `context.WithTimeout` (default **30s**) so a
  hung network cannot wedge the CLI.
- **What is sent:** only the structured findings (`pod`, `issue`, `reason`,
  `evidence`). Never raw pod specs, environment variables, or secrets. This keeps
  the payload bounded and avoids leaking sensitive cluster data. The README
  documents this egress behavior.

## Wiring & flags (`main.go`)

- Add `explain := fs.Bool("explain", false, "summarize findings via one Claude API call (needs ANTHROPIC_API_KEY)")`.
- **Fail fast, before the scan:** if `--explain` is set and `ANTHROPIC_API_KEY`
  is empty, return a clear error immediately — same spirit as the existing
  up-front `--output` validation, so we don't run a full scan and then fail.
- **Zero findings ⇒ skip the call.** No findings means nothing to explain; we
  don't spend an API call. Output is the usual "No issues found. ✅".

## Output (`internal/report`)

`Print` gains an `explanation string` parameter (`""` when not requested):

- **text:** deterministic report as today; if `explanation != ""`, append a
  `── Explanation ──` separator followed by the summary.
- **json:** **backward-compatible.** When `explanation == ""`, emit the bare
  findings array exactly as v1.1 does. Only when an explanation is present does it
  emit the wrapper object:

  ```json
  {
    "findings": [ { "pod": "...", "issue": "...", "reason": "...", "evidence": "..." } ],
    "explanation": "..."
  }
  ```

## Testing (TDD)

- `buildPrompt` — pure unit tests with fake findings.
- `explain.Explain` orchestration — fake summarizer via the interface seam:
  verifies the skip-when-empty path and error wrapping. No network.
- `report.Print` — table cases for the text-append path and both JSON shapes
  (bare array when no explanation; wrapper object when present).
- `main` arg tests — `--explain` without `ANTHROPIC_API_KEY` errors fast;
  `--explain --output json` is accepted.

## Docs

- `docs/go-concepts.md` **#13** — one new concept: `context.WithTimeout` for
  bounding a network call (with a note on pulling in a third-party module). One
  concept, in the established "simple example first, then the kubeagent example"
  style.
- `README.md` — usage example for `--explain` plus the data-egress note.
- `docs/design.md` — mark v2 shipped in the Roadmap section.

## Out of scope (explicit non-goals)

- Streaming the explanation.
- Sending raw pod manifests, events, or logs to the API (only structured
  findings go out).
- Any caching, batching, or multi-call orchestration — v2 is exactly one call.
- Configurable model/prompt via flags — fixed for v2.
```
