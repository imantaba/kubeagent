# `--investigate` — agentic read-only follow-up reads (design)

**Status:** approved · **Date:** 2026-07-23 · **Type:** LLM-path agent loop (Theme C,
principled intelligence — slice 3: agentic investigation)

## Problem

`--explain` (v0.48–0.49) makes **one** model call over the findings the scan already
collected. It cannot chase a cause it can't already see: when a CrashLoop's root cause
lives in an event on the owning ReplicaSet, or in the pod's node condition, or in a
sibling PVC, the one-shot summary can only say "check X" — it can't go look.

`--investigate` closes that gap. It runs the scan, then hands the model a small set of
**read-only tools** and lets it **choose** which follow-up reads to make — describe a
pod, get an object's events, hop to a related resource — gathering evidence across the
finding's resource graph before it concludes. It is a genuine agentic loop (the model
drives), but bounded on every axis: a fixed read allowlist, a findings-scoped reachable
set, hard iteration caps, and the same structured-only egress discipline as `--explain`.
It **never writes** — get/list only.

## Behavior (approved)

- `kubeagent scan --investigate` runs the normal scan, then a bounded tool-use loop, then
  prints an **`Investigation`** report section: a one-line evidence trail (what it
  consulted) followed by the same `Fix first:` ranked narrative `--explain` produces,
  grounded in the evidence it gathered and the deterministic `remediation.For` commands.
- `--investigate` **supersedes** `--explain`: if both are passed, the investigation runs
  (it is the superset) and no separate `Explanation` section is emitted.
- **Anthropic-only in v1.** The loop needs reliable `tool_use`; local OpenAI-compatible
  endpoints are not dependable at it yet. `--investigate` requires `ANTHROPIC_API_KEY`
  and errors clearly if only `KUBEAGENT_EXPLAIN_ENDPOINT` is set.
- Model selection reuses `--model` / `KUBEAGENT_MODEL` / `DefaultModel` (same as
  `--explain`'s Anthropic path).
- Like `--explain`, it is skipped (empty section) when the cluster is healthy and there
  are no workload or service findings — there is nothing to investigate.

## Scope decisions (locked)

| Decision | Choice |
|----------|--------|
| Mechanism | Model-driven **tool-use loop**, Anthropic-only in v1 |
| Read tools | `describe` · `get_events` · `get_related` (owner/PVC/node) — **no logs** |
| Reach | **Findings-scoped** — the closure of the scan findings |
| Bounds | **Fixed baked-in caps**, no flags/env |
| Flag/output | Standalone **`--investigate`**, own section, supersedes `--explain` |

Logs are deliberately **out of scope for v1**: they are the highest-value follow-up but
the highest secret-leak risk (tokens, connection strings printed to stdout). Every tool
in v1 returns structured, scrubbable fields only. A `--investigate-logs` slice with a
proven scrubber can come later.

## Architecture

A new package `internal/investigate/`. `explain` is left **untouched** (`--explain` stays
byte-identical); `investigate` reuses `explain.buildInventoryPrompt` for the opening
bundle and `remediation.For` for grounding. The loop lives in its own package because a
tool-use conversation is a multi-turn message exchange — a different shape from
`explain`'s single-shot `summarizer` interface.

```
scan → inventory → (── --investigate ──) investigate.Investigate(ctx, health, summary,
                                            facts, serviceIssues, workloads, client)
                                          → (narrative, evidenceTrail, error)
                                          → report "Investigation" section
```

### 1. `tools.go` — the read-only tool surface

A fixed allowlist of **three** tool specs (`anthropic.ToolParam`, each with a JSON input
schema). Nothing else is ever offered to the model:

| Tool | Input | Returns (structured text) |
|------|-------|---------------------------|
| `describe` | `{kind, namespace, name}` | phase/conditions, container states & reasons, restart counts, owner refs, node — the same field set the scan renders; never the raw spec/env |
| `get_events` | `{namespace, name}` | recent events for that object (`involvedObject.name=`), `reason: message (count, age)` lines |
| `get_related` | `{namespace, name, relation}` | `relation ∈ {owner, pvc, node}`: the owning workload, a bound PVC, or the host node — key facts only |

`kind` for `describe`/`get_related` is validated against a small enum
(`pod|deployment|replicaset|statefulset|daemonset|job|node|pvc`); an unknown value
returns a tool-result error, not a panic.

### 2. `scope.go` — findings closure & guard

Before the loop, compute the **reachable set** from the scan findings — a set of
`{kind, namespace, name}` keys:

- each flagged workload, its pods, their owner refs, their PVCs (from pod volumes), and
  the node each pod runs on;
- events are reachable for any object already in the set;
- `kube-system` / cluster objects are reachable **only** when a P1 (cluster/system)
  finding put them in scope.

Every tool call is validated against this set. An out-of-scope call returns a
**tool-result error** (`"<kind> <ns>/<name> is not in scope for this investigation"`) —
the model adapts and tries something else; it is not a hard failure. This is the read
analog of `remediate`'s `protectedNamespaces` allowlist discipline.

### 3. `reader.go` — execute an allowed call

Dispatches a validated tool call to a client-go `Get`/`List` and renders **structured
fields only**, reusing the scan's secret-free rendering discipline (the same fields
`buildInventoryPrompt` already emits). Never dumps a raw spec, env vars, secret data, or
container args; **no logs at all** in v1. All reads are `get`/`list` on resources the base
RBAC already grants (pods, nodes, events, persistentvolumeclaims, and the workload
kinds) — so **no new RBAC rule is required**.

### 4. `investigate.go` — the loop

A new backend interface for the multi-turn exchange (only Anthropic implements it; tests
use a fake):

```go
// converse advances one model turn: given the running message list and the fixed
// tool set, it returns the next assistant reply (text and/or tool calls) and the stop
// reason. The Anthropic implementation wraps Messages.New with Tools; the fake scripts
// a fixed sequence of turns for deterministic tests.
type conversation interface {
    converse(ctx context.Context, messages []turn, tools []toolSpec) (reply, error)
}
```

Loop:

1. Seed the conversation with an **investigation system prompt** (the `--explain`
   Fix-first structure plus: "You may call the provided read-only tools to gather more
   evidence about a finding before concluding. Use only the facts you observe.") and a
   first user message = `explain.buildInventoryPrompt(...)` + "Investigate the findings,
   then explain."
2. Call `converse`. If the reply has `tool_use` blocks → validate + execute each via the
   reader, append the `tool_result` blocks, and continue. If it stops with `end_turn` →
   the reply text is the narrative.
3. Enforce caps: `maxToolCalls = 8`, `maxTurns = 6`, and the scan's context deadline. On
   hitting a cap, send one final "conclude now from what you have" user turn and take the
   resulting text (never loop unbounded).
4. Record an **evidence trail**: the ordered list of executed calls
   (`describe pod web-abc`, `events web`, `related web-abc→node`), surfaced in the report.

Return `(narrative string, trail []string, error)`. Errors from the model or a fully
failed loop are surfaced (non-fatal to the scan — the deterministic output already
printed).

### 5. Wiring — `main.go`

- `--investigate` bool flag; usage note: "agentic read-only investigation of findings via
  a bounded tool-use loop (needs ANTHROPIC_API_KEY; supersedes --explain)".
- Precondition: if `--investigate` and `ANTHROPIC_API_KEY == ""` → error
  (`"--investigate needs ANTHROPIC_API_KEY (local endpoints do not support the tool-use
  loop yet)"`). If both `--investigate` and a local `KUBEAGENT_EXPLAIN_ENDPOINT` are set,
  the endpoint is ignored for investigation and this is noted.
- If `--investigate`, run the loop and attach its result to the report; skip the
  `--explain` path even if `--explain` was also passed.

### 6. `report` — the `Investigation` section

- **Text:** a heading `Investigation`, then `consulted: <trail joined by " · ">`, then the
  narrative. Rendered only when `--investigate` produced text.
- **JSON:** an `investigation` object `{ "consulted": [...], "narrative": "..." }`.
- The golden snapshot does **not** include `--investigate` output (it needs a live model);
  a report-level unit test covers the section rendering from a fixed `Investigation`
  struct, mirroring how the explanation field is tested.

## Global constraints

- **Read-only.** get/list only; no writes, ever. `--investigate` is opt-in; without it,
  nothing here runs and the deterministic scan is byte-identical.
- **Structured-only egress.** Tool results carry only the structured fields the scan
  already renders — never raw specs, env, secret data, or logs. An egress test asserts no
  pod IP and no secret env value can appear in a tool result.
- **Bounded on every axis.** Fixed read allowlist (3 tools), findings-scoped reachable
  set, `maxToolCalls=8` / `maxTurns=6` / scan deadline. No user-facing knobs in v1.
- **Anthropic-only in v1.** Requires `ANTHROPIC_API_KEY`; errors clearly otherwise.
- **No new dependency.** Uses the existing `anthropic-sdk-go` tool-use API and client-go.
- **No new RBAC.** All reads are on resources the base ClusterRole already grants.
- **`explain` untouched.** `buildInventoryPrompt`, `systemPrompt`, and the `--explain`
  path are reused, not modified. Golden snapshot unchanged.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Log reads / a log scrubber (the next slice); writes or any `--fix` interaction; local /
OpenAI-compatible tool-use backend; cluster-wide or namespace-wide reads (findings-scoped
only); user-tunable caps or flags; parallel tool calls (serial loop is enough for a
bounded budget); the `watch` daemon (it does not call the LLM); persisting or caching
investigations; re-running the scan mid-loop (the loop reads live objects, it does not
re-diagnose).

## Testing

Fully deterministic — no live model, no real cluster:

- **`reader` (fake clientset):** each tool renders the expected structured fields for a
  fake pod/workload/event/PVC/node; asserts **no** pod IP, node internal IP, env value, or
  secret data appears in the output (egress guard); unknown `kind` → tool-result error.
- **`scope` (fake findings):** the reachable set is exactly the finding closure; an
  out-of-scope `{kind,ns,name}` is rejected; a `kube-system` object is reachable only when
  a P1 finding placed it in scope.
- **`investigate` loop (fake `conversation` + fake clientset):**
  - a scripted turn sequence (tool_use `describe` → tool_use `get_events` → `end_turn`
    text) drives the loop; assert the reader was invoked for each requested call, the
    `tool_result` blocks were appended, the evidence trail lists both calls, and the final
    narrative is returned.
  - an out-of-scope tool call → the loop feeds back the scope error and continues (does not
    abort).
  - `maxToolCalls` reached → the loop stops requesting tools, sends the "conclude now"
    turn, and returns that text.
  - a `converse` error → surfaced as an error; the scan output is unaffected.
- **`main` preconditions (`run(...)`):** `--investigate` with no `ANTHROPIC_API_KEY` → the
  precondition error; `--investigate` supersedes `--explain` (both set → investigation
  path taken). Use `t.Setenv`, clearing `KUBEAGENT_EXPLAIN_ENDPOINT`.
- **`report`:** the `Investigation` section renders from a fixed struct in both text and
  JSON; absent when empty.
- **Live validation (manual, keyed):** the chaos suite can't drive `--investigate` without
  a key, so unit tests are the gate. Before release, run `--investigate` with a real key
  against a Kind cluster with one injected fault (e.g. a bad-image CrashLoop) and confirm
  the loop consults the pod's events and concludes with a grounded fix.

## Release

- **Gate:** touches `internal/investigate` (new), `main.go`, `internal/report`, and adds
  client-go **read** paths on already-granted resources (no `nodes/proxy`, no
  `collect`/`cluster` change, no writes, no Helm template change) → **LIGHTWEIGHT**
  (deterministic unit tests + the keyed live smoke above).
- **Version:** **minor** v0.49.0 → **v0.50.0**.
- **Chart:** **PATCH** — no new RBAC rule, no template/values change (the base ClusterRole
  already grants every read the loop makes).

## Files touched

- **Create:** `internal/investigate/{tools.go, scope.go, reader.go, investigate.go}`
  (+ their `_test.go`).
- **Modify:** `main.go` (+ `main_test.go`) — `--investigate` flag, precondition, supersede
  logic, loop call + report wiring.
- **Modify:** `internal/report/report.go` (+ test) — the `Investigation` section (text +
  JSON).
- **Docs:** `website/docs/features/diagnostics.md` (the `--investigate` subsection),
  `README.md`, `CHANGELOG.md` (`[Unreleased] / ### Added`), `website/docs/roadmap.md`
  (Theme-C shipped bullet).
