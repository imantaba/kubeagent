# Ranked `--explain` remediation, grounded on the deterministic core — design

**Status:** approved · **Date:** 2026-07-23 · **Type:** LLM-path enrichment (Theme C, principled intelligence — slice 1)

## Problem

`--explain` (v2) makes one Claude API call that turns findings into a plain-English
per-issue summary (Root cause / Check / Fix, P1-before-P2). Its "Fix" is entirely
model-authored — the commands are invented, not grounded in kubeagent's reviewed
logic. Separately, `--suggest` (v0.42.0) produces a **deterministic, reviewed**
next-step + read-only command per finding (`remediation.For`), explicitly designed
as "the deterministic remediation core a later Theme-C slice hands to `--explain`
for LLM ranking and phrasing — the LLM ranks; it never invents the remediation."
This slice closes that loop: it injects the deterministic suggestions into the
`--explain` prompt, anchors the model's Fix to the reviewed command (never
substitute/invent), and adds an explicit ranked remediation order. The result is a
trustworthy, prioritized on-call plan.

## Behavior (approved)

`--explain` output gains two things:

1. A leading **`Fix first:`** ranked list — the issues in remediation order
   (most blocking / highest-impact first; P1 cluster/kube-system before P2
   workloads), each line `N. <namespace/name>: <one-phrase action>`.
2. Each per-issue **Fix** is grounded on the deterministic command: the model uses
   the provided, pre-reviewed command verbatim (it may add a namespace/flag already
   shown, sequence multiple provided commands, and phrase for on-call), and never
   substitutes or invents a different command.

The offline deterministic core (`scan`, `--suggest`) is unchanged. `--explain`
stays opt-in and requires an API key.

## Design

### 1. Inject the deterministic suggestion into the prompt

`internal/explain/explain.go`'s `buildInventoryPrompt` currently emits, per workload
finding:

```
    issue: <Issue> — <Reason> (<Evidence>)
```

(plus optional `log cause` / `container resources` sub-lines). Add, immediately
after those, one line built from `remediation.For(f)`:

```go
	s := remediation.For(f)
	fmt.Fprintf(&b, "      suggested fix (deterministic, pre-reviewed — do not substitute): %s | run: %s\n", s.NextStep, s.Command)
```

- `remediation.For` always returns a non-empty `NextStep` and `Command` (it has a
  safe generic default), so the line is always present for a workload finding.
- **Imports:** `explain` gains `"github.com/imantaba/kubeagent/internal/remediation"`.
  No import cycle: `remediation` imports only `diagnose`; `explain` already imports
  `inventory` (which imports `diagnose`), so adding `remediation` is acyclic.
- This applies to **workload findings only** (the ones `--suggest`/`remediation.For`
  cover). Service issues and the other Assess lists keep their existing prompt
  lines unchanged (they are not `diagnose.Finding`s and have no
  `remediation.For` suggestion).
- **Privacy unchanged:** `remediation.For` produces read-only `kubectl` command
  strings templated from the finding's namespace/name/container — no secrets, no
  new sensitive data enters the prompt.

### 2. System-prompt additions

Two additions to the `systemPrompt` const, keeping the existing per-issue structure:

- **Ranked list (new, at the top of the required output):**
  > Begin your response with a `Fix first:` section — a numbered list ranking the
  > issues in the order they should be remediated (most blocking / highest-impact
  > first; cluster / kube-system P1 issues before workload P2 issues), each line
  > `N. <namespace/name>: <one-phrase action>`. Then give the per-issue detail
  > below.

- **Strict grounding (added to the `Fix:` bullet's instruction):**
  > Fix: use the provided deterministic, pre-reviewed command for this issue
  > **verbatim** — you may add a namespace or flag already shown, sequence multiple
  > provided commands, and phrase it for on-call, but **never substitute or invent
  > a different command**. When the provided command is a generic `describe`, keep
  > it and say what to look for in the output.

The rest of the system prompt (use-only-given-facts, tightness, expected-state
handling, "likely"/"check" over false certainty) is unchanged.

### 3. No other wiring

`--explain` already exists (flag, model resolution, the `ExplainInventory` call,
the fake-summarizer seam). This slice changes only the prompt content — no new
flag, no `Options`/`Result` field, no `main.go`/`scan`/`watch`/report change, no
new RBAC/collector. The `ExplainInventory` skip-when-healthy short-circuit is
unchanged.

## Global constraints

- **Opt-in; offline core unchanged.** The deterministic `scan`/`--suggest` output
  is untouched; only the `--explain` prompt is enriched. No API call happens
  without `--explain`.
- **Gate:** touches only `internal/explain` → **LIGHTWEIGHT** (unit-tested via the
  fake summarizer + the pure `buildInventoryPrompt`). **Patch/minor** version bump
  v0.47.0 → **v0.48.0** (a user-facing `--explain` behavior improvement — minor);
  **chart PATCH** (no Helm/deploy/RBAC change).
- **LLM ranks, never invents** — the deterministic command is the source of truth
  for the Fix; the model ranks, sequences, and phrases.
- **Deterministic tests** — `buildInventoryPrompt` is a pure function and the
  `systemPrompt` is a const, so the prompt content (the injected suggestion + the
  ranking/grounding instructions) is asserted without any network call.
- `remediation`, `diagnose`, `inventory`, `scan`, `report`, `--fix`, the watch
  daemon, and the golden snapshot stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Grounding the Assess-list issues (service/PDB/HPA/webhook/quota) — they carry their
own prescriptive Detail and are not `diagnose.Finding`s (a later slice could add a
`remediation.ForIssue`); an `--investigate` read-only follow-up-reads mode (the
next Theme-C slice); local-model explain; changing the deterministic `--suggest`
output or `remediation.For` itself; a machine-readable ranked-plan JSON (the ranked
list is prose for on-call); multi-call / agentic explain (still one API call).

## Testing

- **`buildInventoryPrompt` (pure, no network):**
  - a workload with a `CrashLoopBackOff` finding (`Pod: "shop/web-abc"`,
    `Container: "web"`) → the prompt contains
    `suggested fix (deterministic, pre-reviewed — do not substitute):` and the exact
    `remediation.For` command `kubectl -n shop logs web-abc -c web --previous`.
  - a workload with an `ImagePullBackOff` finding → the prompt contains that
    finding's deterministic `describe pod` command.
  - the suggestion line appears once per finding, after the `issue:` line.
- **`systemPrompt` const:** contains `Fix first` (the ranked-list instruction) and
  the grounding phrase (`verbatim` and `never substitute or invent`).
- **Existing explain tests (fake summarizer):** still pass — `ExplainInventory`
  returns the fake output for a degraded cluster / workloads, and returns `""`
  (no API call) when healthy with no workloads/service issues.

## Files touched

- **Modify:** `internal/explain/explain.go` (+ test) — `buildInventoryPrompt` suggestion injection; `systemPrompt` ranked-list + grounding additions.
- **Docs:** `website/docs/features/` (the `--explain` / explain doc), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
