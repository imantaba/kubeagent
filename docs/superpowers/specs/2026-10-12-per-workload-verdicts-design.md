# Local `--investigate`: one model call per workload — design

Date: 2026-10-12. Status: approved direction, spec for planning.

## Summary

Local verdict mode sends every flagged workload — up to 10 — to the model in
one prompt and asks for one verdict row per workload. A purpose-tuned small
model fails at that shape. In a live chaos run it found the right cause for
1 of 45 workloads. It wrote one or two rows where five were asked for, and it
wrote a summary claiming the workloads "share one upstream cause" when they
did not.

The same model, given the same evidence one workload at a time, got 6 of 11
right, 2 nearly right, and said "I can't tell" on the other 3. It made
nothing up. The model learned a shape where several workloads in one prompt
usually means one shared cause, so the multi-workload prompt is outside what
it can do.

This slice changes the shape of the call, not the model. kubeagent makes one
`/chat/completions` call per flagged workload that the rules did not decide,
each prompt holding that one workload and only its evidence. The model's
summary is no longer rendered; shared-cause lines come from the rules alone,
as they already do.

Nothing else moves. The reads, the read budget, the system prompt, verdict
contract v1, the size bounds and the tool-loop mode are unchanged. `scan`
stays at schema 1.8 and no dependency is added.

## Decisions (locked with the operator)

1. **One call per undecided workload.** Grouping workloads that share a
   candidate into one call was rejected: it brings back the multi-workload
   shape the model fails on. Retraining for the multi-workload shape was
   rejected for now: weeks of work with no evidence it would help.
2. **No call for a rule-decided workload.** A rule row's cause never comes
   from the model. The row prints the rule's own evidence sentence.
3. **The row is pinned to the workload kubeagent asked about.** The workload
   name the model writes is ignored.
4. **The model's summary is not rendered.** Rules write the shared lines.
5. **Calls run one at a time, in report order, under the existing 300-second
   budget.** A failed call costs that workload's row and nothing else.
6. **Flagged workloads beyond the gather's 10 get no row.** They had no
   evidence; the model could only guess from the inventory line.

## Design

### The gather keeps its reads, and remembers which workload each belongs to

`gatherEvidence` makes the same reads in the same order under the same budget
of 8, and returns the same trail and the same `hypothesis.Reads`. What
changes is the bundle: instead of one string, it returns one evidence string
per scoped workload, in scope order.

- A workload's evidence holds its own events read, the describes of its own
  surviving node and PVC candidates, and its own log-cause reads.
- A describe is still read once. When two workloads share a node or a PVC,
  the second workload's evidence carries the same text the first one got.
  Today's global dedupe kept that text out of the second workload's view;
  each prompt now holds only one workload, so each needs it.
- A read that was never made because the budget ran out is simply absent,
  as today.
- Per-read capping (`maxReadBytes`, 4 KiB) and the section format
  (`== label ==\n<content>\n\n`) are unchanged.

The trail does not change: one entry per read actually made, in the order it
was made.

### Each prompt holds one workload

`buildVerdictPrompt` builds the prompt for a single workload:

- **inventory**: `explain.BuildInventoryPrompt` called with that one
  workload and only the service issues in its namespace (still capped at
  10), its `--explain` closing line stripped as today. The cluster-health
  and resource header lines stay.
- **candidates**: that workload's capped trace and its rule result, rendered
  by the existing `renderCandidates` over a one-element slice.
- **evidence**: that workload's evidence string.
- The closing line is unchanged.

The 64 KiB whole-prompt bound and its evidence-cutting logic apply to each
prompt. `verdictSystemPrompt` is unchanged — the model was trained on it and
a test pins its injection-posture sentences.

### Which workloads get a call

For each scoped workload, in scope order:

- **Rule-decided** (`hypothesis.Result.Decided`): no call. It renders as a
  rule row with the rule's evidence sentence as its rationale.
- **Not decided**: one call.

With no undecided workload, no call is made. A scan with no flagged workload
— only a degraded cluster or service issues — makes no call either, because
the only thing the model could add there is a summary, and summaries are no
longer rendered. If the rules decided nothing, there is no section, as when
the model fails today.

### Reading a reply

Each reply is parsed with the existing lenient `parseVerdicts`.

- The **first row** in `verdicts` is taken as the verdict for the workload
  that call was about. Its `workload` field is ignored, so a name the model
  shortened (`chaos-c-initcfg` for `chaos-c-initcfg/migrate`) still lands on
  the right row, and a reply can never place a row on any other workload.
- Any further rows are ignored.
- `summary` is ignored.
- A reply with no rows counts as no verdict for that workload.

The model row renders exactly as today through `modelRow`: cause and
rationale capped at 512 runes and passed through `safetext.Line`,
confidence outside `low`/`medium`/`high` shown as `unstated`. The rendered
workload name is kubeagent's own `namespace/name`, never the model's string.

### Rendering

`Root-cause verdicts:` then one row per scoped workload that has one, in
scope order: a rule row for a decided workload, a model row for an undecided
workload whose call returned a row. At most 10 rows, as today (the scope is
already capped at 10). Then the rules' shared lines from `hypothesis.Shared`,
unchanged. No model summary follows.

`ruleRow` no longer takes a model row. Its rationale is always the rule's
evidence sentence.

### Calls, the 400 retry, and the time budget

- Calls run one at a time, in scope order. Local servers usually serve one
  request at a time, so running them together would not finish sooner.
- The first call that gets a 400 with `response_format` set is retried once
  without it, as today. Every later call in the same run is sent without
  `response_format` from the start.
- All calls share the 300-second context the CLI already sets. A call that
  hits the deadline fails like any other failed call, and every workload
  after it is skipped without a call. Measured on CPU, one call takes about
  10 seconds, so 10 calls fit with room to spare.
- `Report.Truncated` is true when any call's `finish_reason` was `length`.

### When some or all calls fail

A call fails when the request errors, the endpoint returns a non-2xx status,
the reply does not parse, or the reply has no row. Skipped calls after a
deadline count as failed.

- **No call failed:** the report renders; no notice.
- **Some failed, at least one model row rendered:** the report renders with
  the rows it has. `Investigate` returns a nil error and sets a new field,
  `Report.Notice`, to
  `model gave no verdict for <n> of <m> workloads: <first failure reason>`,
  where `m` is the number of undecided workloads. The CLI prints it on
  stderr as `--investigate: <notice>`.
- **Every call failed:** behaves as a failed call does today. `Investigate`
  returns the error of the first failure with the rules-only report; the CLI
  prints `--investigate: <reason>; rule-decided verdicts rendered without
  the model`, or just the reason when no rule decided anything.

`Report.Notice` is text-only like `Truncated`: never marshalled to JSON, so
no schema moves. The tool-loop client never sets it.

### What does not change

- Read-only toward the cluster; never fatal to the scan.
- The 8-read budget, the 10-workload scope, every size bound in the
  diagnostics table.
- `verdictSystemPrompt`, verdict contract v1's JSON shape, `verdictSchema`.
- `internal/hypothesis` — no change at all.
- The Anthropic tool-loop mode and `--explain`.
- `scan` schema 1.8.

## Docs

- `website/docs/features/diagnostics.md`, "Local verdict mode": replace
  "makes exactly one call" and "then asks once" with one call per workload
  the rules did not decide, at most 10, one at a time; describe per-workload
  evidence, the pinned row, the dropped summary, partial failure and the
  new notice; drop the paragraph about rows for workloads outside the scope.
  Update "Verdict contract v1": `summary` is still part of the shape and is
  not rendered, and the `workload` field is not used to place the row.
  Update "Model guidance": a prompt now holds one workload.
- `website/docs/roadmap.md` and `CLAUDE.md`: the local verdict mode
  paragraphs say "exactly one `/chat/completions` call"; change them to one
  call per undecided workload.
- `CHANGELOG.md`: an Unreleased entry.

## Testing (TDD)

All in `internal/investigate`, with an `httptest` endpoint that counts
requests and answers per prompt, and the client-go fake clientset.

1. Three undecided workloads → three requests, each user message naming
   exactly one workload in its inventory and candidates sections.
2. One rule-decided and two undecided → two requests; the decided row is a
   rule row with the rule's evidence sentence.
3. All decided → zero requests.
4. No flagged workload, degraded cluster → zero requests, empty report.
5. A reply whose `workload` is a shortened or wrong name → the row renders
   under the workload the call was about.
6. A reply with two rows → only the first renders.
7. A reply's `summary` never appears in the narrative; the shared lines do.
8. Second of three calls returns 500 → two model rows, nil error,
   `Report.Notice` = `model gave no verdict for 1 of 3 workloads: …`.
9. Every call fails → error returned with the rules-only report.
10. A 400 on the first call → retried without `response_format`; the second
    call's request body has no `response_format`.
11. Context deadline hit during call one of three → no further requests;
    every call counts as failed, so the error path runs.
12. Two workloads sharing a PVC candidate → one PVC read in the trail, the
    describe text in both prompts.
13. A workload's evidence holds only its own reads — another workload's
    events never appear in its prompt.
14. `Truncated` true when any one reply has `finish_reason: length`.
15. `internal/cli`: `runModelPath` prints `--investigate: <notice>` for a
    report with a nil error and a non-empty `Notice`.

Existing tests that assert a single call, the model summary, or rows for
workloads outside the scope change to the new behavior; each such change is
named in the plan.

## Out of scope

- No log-cause read for `RestartLoop` and `Init:CrashLoopBackOff`
  (`crashFamily`). A separate fix.
- A live exam slice for the verdict model. A separate change in the training
  repository, done after this one.
- Retraining the model.
