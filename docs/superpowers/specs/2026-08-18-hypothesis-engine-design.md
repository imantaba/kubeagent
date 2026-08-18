# Hypothesis engine, smart `--investigate`, and the chaos corpus — design

Date: 2026-08-18
Status: approved (approach A of three considered)

## Problem

kubeagent's root-cause attribution (`internal/rootcause`) collapses many
findings into one cause, but it discards the evidence of everything it
rejected: each rule short-circuits on first match and throws away the
candidates it evaluated. Three consequences:

1. **No trust trace.** An operator sees `↳ likely caused by node worker-3
   (NotReady)` but cannot see what else was considered and why it was
   rejected. The first time an attribution is wrong, there is no way to see
   its work — which is when tools lose their users.
2. **`--investigate` starts from zero.** The agentic loop re-discovers, with
   its bounded 8 tool calls, facts the deterministic pass already computed
   and threw away.
3. **No training data.** A future tiny local classifier ("what failure is
   this?") needs labeled examples. The chaos harness injects known faults —
   ground truth by construction — but captures nothing reusable.

## Goals

- Keep every candidate the attribution rules evaluate, with a verdict and a
  reason, and surface that trace to operators (flag in text, always in JSON).
- Make `--investigate` measurably smarter per tool call: start it from the
  deterministic trace, give it a privacy-preserving log-classification tool
  and one new relation hop, and close a known error-string leak.
- Start the labeled corpus: one machine-readable row per chaos scenario,
  fault label included, produced by the nightly matrix.

## Non-goals

- No trained model in this design. The corpus is the prerequisite; training
  is its own future project.
- No new Go classifier interface. `logscan.Classify` and the hypothesis
  engine are the seams a future model implements against; an interface with
  one implementation is invented abstraction.
- No change to attribution behavior: `RootCause` strings, precedence, and
  default text output stay byte-identical.
- No trace in `findings.Finding` (gate/fleet/policy consumers), matching the
  existing deliberate omission of Confidence and RootCause there (R192).
  Revisitable when a consumer needs it.
- No new user-facing knobs on the investigate loop; bounds stay
  `maxToolCalls=8`, `maxTurns=6`.

## Section 1 — Hypothesis engine (`internal/rootcause` refactor)

New type, defined in `internal/inventory` next to `Workload` (rootcause
imports inventory, so the type lives downstream of its producer):

```go
// Hypothesis is one candidate cause the attribution pass evaluated for a
// flagged workload, kept whether or not it won.
type Hypothesis struct {
    Cause   string `json:"cause"`   // same format as RootCause: "<cause> (<detail>)"
    Kind    string `json:"kind"`    // "node" | "pvc" | "registry"
    Verdict string `json:"verdict"` // "attributed" | "ruled_out" | "outranked"
    Reason  string `json:"reason"`  // one evidence sentence
}
```

`inventory.Workload` gains `RootCauseTrace []Hypothesis
`json:"rootCauseTrace,omitempty"`` and `RootCauseConfidence string
`json:"rootCauseConfidence,omitempty"``.

Each rule records every candidate it evaluates, per flagged workload:

- **node** (`Annotate`): every down node is a candidate. A pod scheduled on
  it → `attributed`. No pod on it → `ruled_out`, reason "no pod of this
  workload is scheduled on it".
- **pvc** (`AnnotatePVC`): every independently-diagnosed broken PVC in the
  workload's namespace is a candidate. Mounted by a pod and no higher cause →
  `attributed`. Mounted but a node cause already won → `outranked`, reason
  names the winning cause. Not mounted → `ruled_out`, reason "not mounted by
  this workload's pods".
- **registry** (`AnnotateRegistry`): the workload's pull-failure registry
  host is a candidate. Group ≥ 2 → `attributed`. Group of 1 → `ruled_out`,
  reason "only workload failing to pull from this host; threshold is 2".
  Image undeterminable → `ruled_out`, reason "image reference
  undeterminable". Evidence matched but node/PVC won → `outranked`.

`outranked` is the trust-critical verdict: the evidence matched, but
precedence chose better evidence. It is distinct from `ruled_out`, where the
evidence did not match.

Invariants preserved:

- `RootCause` selection is unchanged: same rules, same structural precedence
  (node > PVC > registry via the `w.RootCause != ""` guard), same string
  format `<cause> (<detail>)` that `report.go`'s `rootCauseNode` parses.
- The package stays pure and deterministic: no client, no context, no I/O.
  Trace ordering is kind precedence (node, pvc, registry), then sorted
  `Cause` within a kind.
- `confidence.ForRootCause`'s answer is now also *stored* at annotation time
  into `RootCauseConfidence` (scan.go's annotation chain), fixing the
  asymmetry where root-cause confidence existed only inside the text
  renderer. The renderer keeps calling `ForRootCause` for text; JSON carries
  the stored field.

Noise bound: candidates are only recorded for flagged workloads, and only
for causes that exist (down nodes present, broken PVCs present, pull
findings present). A healthy cluster records no trace at all; `omitempty`
keeps every existing JSON document byte-identical.

## Section 2 — Surfacing

- **`scan --why`** (new boolean flag, scan command only): after each flagged
  workload's `↳ likely caused by …` line (or where it would be), print one
  indented line per hypothesis:

  ```
  · considered node worker-3 (NotReady): attributed — pod web-abc is scheduled on it
  · considered registry ghcr.io: outranked — node worker-3 (NotReady) is the stronger cause
  · considered PVC data-0 (ProvisioningFailed): ruled out — not mounted by this workload's pods
  ```

  Without `--why`, text output is byte-identical to today: the golden test
  (`internal/report/golden_test.go`) does not regenerate, and no demo-GIF or
  quickstart refresh is needed.
- **JSON always carries the trace** when it is non-empty: `rootCauseTrace`
  and `rootCauseConfidence` on the workload row, both `omitempty`. Scan
  schema **1.3 → 1.4**, additive; regenerate via
  `go test ./internal/schemadoc -run TestSchemaDrift -update`. The
  `verdict` values register in `internal/schemadoc`'s enum map.
- **`--why` and JSON**: `--why` affects text rendering only; JSON output is
  identical with or without it (the trace is always there when non-empty).
- **gate/fleet**: `findings.Flatten` continues to drop the trace (see
  Non-goals).
- **MCP/HTML**: both render what the shared shapes carry; no renderer-side
  change. `internal/htmlreport` never reads the new fields in this design.

## Section 3 — Smart `--investigate`

Four changes, all inside existing seams of `internal/investigate`:

1. **Trace-primed opening.** The first user message appends, after
   `explain.BuildInventoryPrompt`'s output, a rendering of each flagged
   workload's `RootCauseTrace` plus an instruction: verify the attributed
   causes, and investigate what the deterministic pass could not explain.
   The rendering is a new pure function in `internal/investigate` (not in
   `explain`): the trace names nodes, and `--explain`'s payload deliberately
   excludes node names, while investigate already surfaces node names
   through its tools — the two boundaries differ and must stay separate.
   `BuildInventoryPrompt` and `--explain`'s payload are unchanged.
2. **New tool `get_log_causes`.** Input: pod + container (scope-checked via
   the existing `Scope`). It fetches the previous-instance tail via
   `collect.PreviousLogs` (the same bounded 25-line read `--logs` uses),
   classifies with `logscan.Classify`, and returns **only the Cause string
   after `redact.Addresses`**. The raw excerpt never crosses the model
   boundary — this matches the existing policy split (LogCause may cross
   after redaction; LogExcerpt never does). A Forbidden/Unauthorized read
   returns a sanitized refusal naming the `pods/log` permission. The tool
   works whether or not `--logs` was passed; it reads on demand.
3. **New hop and kind.** `get_related` learns `service`: from a pod, the
   Services in its namespace whose selector matches the pod's labels.
   `Service` joins the describable kinds (`Scope.normKind`, a
   `describeService` rendering name, type, ports, selector, and
   ready-endpoint count). One hop, scope-guarded like every other.
4. **Close the error leak.** `Reader`'s failed client-go reads currently
   return raw `err.Error()` to the model (documented gap in `reader.go`);
   route them through `redact.Error` before they become a `toolResult`, the
   same helper the CLI's `enrichmentFailure` already uses.

Unchanged: loop bounds, the Anthropic-only CLI gate, the findings-seeded
scope closure, the every-tool_use-gets-a-tool_result contract, and the
never-fatal rule (a model-path failure still reduces to one stderr notice
and a deterministic report at exit 0).

## Section 4 — Chaos corpus (data engine)

- New `capture()` helper in `chaos/run.sh` beside `record()`: same
  never-fail contract (`assert.sh` helpers must never return non-zero under
  `set -euo pipefail`), same redaction path (`redact_nodes`/`redact_needles`
  before any byte leaves the process in portable mode), same
  mktemp-plus-trap lifecycle as `$ASSERTLOG`.
- One JSONL row per scenario:

  ```json
  {"scenario": "5. coredns", "fault": "coredns-corefile-broken",
   "k8s": "v1.34", "distro": "kind", "rc": 0,
   "assertions": ["PASS\tCluster: Degraded named", "..."],
   "skipped": false, "skip_reason": ""}
  ```

  The scenario label comes from the existing `scenario_title()`; assertion
  lines come from `$ASSERTLOG`, which is already scenario-labeled and
  redaction-safe. Skipped scenarios (capability-gated, or scenario 02 which
  never scans) write `skipped: true` with the reason — a partial run can
  never masquerade as a full corpus.
- The corpus file (`chaos-corpus-<minor>-<distro>.jsonl`) is written next to
  the results report, uploaded as a nightly artifact by
  `chaos-matrix.yml`, and the workflow's post-run credential-grep step
  extends to cover it (including the scenario-10 `AKIAIOSFODNN7EXAMPLE`
  allowance and the scenario-20 certificate/token checks).
- The corpus format is the training contract. No Go code consumes it in
  this design.

## Error handling

- Hypothesis recording can never fail a scan: it is pure computation on
  values already in memory.
- `get_log_causes` failures (RBAC, missing previous instance, fetch error)
  are tool results, not loop errors: sanitized, named, and counted against
  the call budget like any other call.
- `capture()` failures must not abort the chaos run: same contract as
  `record()`, errors swallowed after a stderr note.

## Testing

- **rootcause:** table-driven unit tests with fake workloads assert (a) the
  chosen `RootCause` is byte-identical to today's for every existing test
  case, and (b) exact expected traces, including `outranked` cases and
  ordering. TDD: trace tests written first.
- **report:** unit tests for `--why` rendering; the golden scan test must
  pass *unmodified* (proof the default output did not move).
- **schema:** bump `jsonschema` scan version to 1.4, regenerate, and the
  drift test classifies the change additive (MINOR).
- **investigate:** the existing fake-conversation pattern covers the primed
  prompt content, the `get_log_causes` tool (result shape, redaction,
  RBAC refusal), the service hop, and the sanitized error path.
- **chaos:** one local kind run of the full suite proving `capture()`
  writes a well-formed corpus (row count = scenario count, skips flagged,
  no credential material), before the nightly picks it up.

## Slices

Three independently releasable slices, in order:

1. **Hypothesis engine + `--why` + schema 1.4** — sections 1 and 2.
2. **Smart `--investigate`** — section 3 (depends on slice 1's trace).
3. **Chaos corpus** — section 4 (no production Go code).

Each slice: feature branch, SDD pipeline, whole-branch review, `release`
skill.
