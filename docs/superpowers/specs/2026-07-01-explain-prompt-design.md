# kubeagent — Design: `--explain` prompt improvement

**Status:** approved design (pre-implementation)
**Date:** 2026-07-01

## Goal

Make `--explain` produce consistently high-quality, actionable, **facts-grounded**
output. Today the system prompt is one line ("be concise"), so the output shape
drifts run-to-run and — observed in the chaos `--explain` report — the model can
confidently misattribute a root cause when the input is messy. This replaces the
system prompt with a senior-SRE persona, a prescribed per-issue structure, and an
anti-hallucination guardrail; it lightly frames the facts with P1/P2 priority.

## Decision (from brainstorming)

- **Consistent per-issue structure** (chosen over sharpened free-form): the model
  addresses issues in priority order and, for each, gives a one-line root cause,
  read-only "Check" commands, and an exact "Fix".
- **Anti-hallucination guardrail** baked in: explain using ONLY the provided
  facts; when ambiguous, name the most likely cause AND what to check — never
  present a guess as certain.

## Invariants / constraints (unchanged)

- `--explain` stays **opt-in and read-only**; it makes one Claude API call and
  sends only **structured facts** — never raw pod specs, pod IPs, env values, or
  secrets. (Node names in the cluster section remain infrastructure identifiers,
  as today.)
- `--explain` is **entirely separate from `--fix`** — no coupling; no action
  context ever reaches the model.
- No new Go module dependency. Only `internal/explain` changes (+ a short
  README/CHANGELOG note).

## Component 1 — system prompt (`internal/explain/explain.go`)

Replace the `systemPrompt` const with:

```text
You are a senior Kubernetes SRE reviewing a read-only cluster scan. Explain what
is wrong and exactly how to fix it, using ONLY the facts provided — do not invent
causes, resources, or values that are not given.

Address issues in priority order: cluster / kube-system problems (P1) before
workload problems (P2). For EACH issue use this structure:

**<namespace/name> — <the issue>**
- Root cause: one line, from the facts. If the facts are ambiguous, name the most
  likely cause AND what to check — never present a guess as certain.
- Check: 1–3 read-only commands to confirm (kubectl get/describe/logs).
- Fix: the exact command(s) or concrete change to resolve it.

Be tight — no preamble, no restating the input, no generic advice. If a finding
is expected (e.g. a scaled-to-zero workload), say it needs no action. Prefer
"likely"/"check" over false certainty.
```

## Component 2 — prompt facts framing (`buildInventoryPrompt`)

Keep the structured facts as-is, with two small framing changes so the model
orders correctly and stays grounded:

- Label the cluster/system block **P1** — e.g. the degraded-cluster header becomes
  `Cluster health (P1): DEGRADED — …`.
- Label the workloads block **P2** — the `These Kubernetes workloads need
  attention:` line becomes `Workload problems (P2):`.
- Align the closing instruction to the required structure — the final
  `Explain what is going wrong and suggest concrete next steps.` becomes
  `Explain each problem and its fix using the required structure.`

No change to which facts are sent or to the resources/service-issues rendering.

## Component 3 — token budget

`MaxTokens` in `anthropicSummarizer.summarize` goes from `1024` → `2048`: a
structured per-issue answer across several findings can exceed 1024 and be
truncated mid-fix.

## Testing (TDD)

The model's output is not unit-testable; the **prompt is**. `systemPrompt` and
`buildInventoryPrompt` are package-internal, so tests in package `explain` assert
them directly:

- `systemPrompt` contains the structure directives (`Root cause`, `Check`,
  `Fix`), the P1-before-P2 ordering instruction, and the anti-hallucination
  guardrail wording ("ONLY the facts" / "do not invent" / "never present a guess
  as certain").
- `buildInventoryPrompt` emits the `(P1)` label on a degraded-cluster section and
  the `Workload problems (P2):` label when workloads are present; the existing
  prompt-content tests (facts included: workload ns/name, findings, resources,
  service issues, netpolicy) still pass.
- `ExplainInventory`'s skip path (healthy + no workloads + no service issues →
  `""`, no API call) is unchanged and still covered.

Real-world output quality is validated by re-running `kubeagent scan --explain`
(the operator's API key) and comparing to the prior chaos `--explain` report —
outside the unit suite.

## Docs

- `README.md` `--explain` note: mention the output is structured (root cause →
  check → fix, priority-ordered) and grounded strictly in the scan's facts.
- `CHANGELOG.md` `[Unreleased] → Changed`: the improved `--explain` prompt.

## Out of scope (explicit non-goals)

- Enriching the facts beyond the P1/P2 labels, or changing what data is collected.
- Any `--fix` interaction or LLM-in-the-fix-path.
- Multi-call, streaming, few-shot examples, or model changes.
