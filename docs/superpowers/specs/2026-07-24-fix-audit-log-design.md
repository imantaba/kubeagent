# `--fix` audit log (design)

**Status:** approved · **Date:** 2026-07-24 · **Type:** write-path hardening (Theme D,
trustworthy remediation — slice 2: audit log)

## Problem

Slice 1 (v0.51.0) made the `--fix` *plan* durable (JSON `remediationPlan`, status
`proposed`) and bound `Apply` to the preview. What happened to each action at run time
is still ephemeral — the applied/refused/skipped/error lines scroll past on stdout and
are gone. A production remediation contract needs the accountability half: a **durable,
append-only, machine-readable record of every remediation outcome** — what kubeagent
proposed and what became of it — that survives the run and can be read back by later
slices (RBAC-preflight, rollback).

## Scope decisions (locked)

| Decision | Choice |
|----------|--------|
| Destination | **`--audit-log <path>` file flag** (with `--fix`); unset ⇒ no audit file, stdout unchanged |
| Format | **JSON Lines** — one self-contained JSON object per line (append-only, jq/grep-able) |
| Coverage | **Every disposition** — `dry-run`, `declined`, `applied`, `refused`, `error` |

## Architecture

A new small package `internal/audit` owns the durable record and its writer.
`remediate` owns the *action + its result*; `audit` owns *what happened to it* — a
distinct responsibility, and keeping them separate lets slices 3/4 read/write audit
records without importing the write-path apply logic. `main.go` opens the file and
passes an `*audit.Writer` into `runFixes`.

### 1. `internal/audit` (new package)

```go
// Record is one durable audit entry: what was proposed and what became of it.
// Every field is a safe display value — Changes reuses the slice-1 remediate.Change
// (revisions / images / counts only) and Detail is remediate.Result.Detail. No pod
// specs, env, or secrets are ever recorded.
type Record struct {
	Time        string             `json:"time"`                 // RFC3339 UTC
	Kind        string             `json:"kind"`                 // RolloutUndo | Uncordon
	Namespace   string             `json:"namespace,omitempty"`  // "" for a node
	Name        string             `json:"name"`
	Target      string             `json:"target"`               // e.g. "shop/web (Deployment)"
	Changes     []remediate.Change `json:"changes,omitempty"`
	Disposition string             `json:"disposition"`          // dry-run|declined|applied|refused|error
	Detail      string             `json:"detail,omitempty"`
}

// RecordFor builds a Record from an action, its disposition, a detail string, and a
// clock. Pure — no I/O.
func RecordFor(a remediate.Action, disposition, detail string, now time.Time) Record

// Writer appends JSON-Lines records to an underlying writer (the open audit file, or
// any io.Writer in tests). One record per line.
type Writer struct { w io.Writer }
func NewWriter(w io.Writer) *Writer
func (a *Writer) Log(r Record) error   // json.Marshal(r) + "\n"; returns a write/marshal error
```

`RecordFor` maps `a.Kind/Namespace/Name/Target/Changes` straight through, stamps
`now.UTC().Format(time.RFC3339)`, and sets `Disposition`/`Detail` from its args.

### 2. `remediate` — distinguish "refused" from other non-applied outcomes

`Result` gains one boolean:

```go
	Refused bool // set on the guarded no-write refusals (drift bond, no differing target,
	             // protected namespace, apply-time precondition) — Applied is false, Err is nil
```

Set `Refused = true` at each existing refusal return in `applyRolloutUndo`
(the `target == nil` "no differing prior revision" case and the revision-drift bond)
and `applyUncordon` (the "no longer a safe uncordon target" precondition), plus the
protected-namespace guard. These are the "no write made" paths. No behavior changes —
only the flag is added. This gives `runFixes` a clean, testable disposition signal
instead of parsing `Detail` text.

### 3. `main.go` — flag + wiring

- New flag: `--audit-log <path>` — help: `"with --fix: append a JSON-lines audit record
  per action to this file"`. Add to the usage string inside the `--fix` group.
- In `runScan`, when `*fix` and `--audit-log` is set: open the file **fail-fast before
  any apply** —
  `os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)`; a failure returns an
  error from `run` (if we were asked for a trail and can't write it, do not proceed to
  mutate). `defer f.Close()`. Wrap in `audit.NewWriter(f)`. When unset, pass `nil`.
  Mode `0o600` — an audit trail should not be world-readable.
- `runFixes` gains an `*audit.Writer` parameter (nil ⇒ no logging). After it determines
  each action's disposition it calls `auditw.Log(audit.RecordFor(a, disposition,
  detail, time.Now()))` (guarded by `auditw != nil`). Disposition/detail per branch:
  - dry-run branch → `("dry-run", "")`
  - user typed not-`y` → `("declined", "")`
  - after `remediate.Apply`: `res.Err != nil` → `("error", res.Err.Error())`;
    `res.Applied` → `("applied", res.Detail)`; `res.Refused` → `("refused", res.Detail)`;
    else → `("refused", res.Detail)` (fallback: any non-applied, non-error is a refusal).
  A `Log` error is surfaced to stderr as a warning and does not abort the loop (the
  write already happened or didn't; losing one audit line must not crash remediation) —
  but the fail-fast open above makes this path rare.

### 4. No change to stdout or the report

The human-readable apply lines and the JSON `remediationPlan` are unchanged. The audit
log is a separate sink. Golden snapshot untouched.

## Global constraints

- **Writes stay guard-railed and opt-in.** No new write paths; the only remediation-
  behavior change is the additive `Result.Refused` flag. The audit file is written
  **after** each action's outcome is known; logging never changes what is applied.
- **No secrets in the audit record.** Only revisions, image refs, counts, booleans
  (via `remediate.Change`), and our own `Detail`/`Target` strings — never env, specs,
  or secrets.
- **Append-only, 0o600.** JSON Lines; the file is opened `O_APPEND`; one line per action.
- **Fail fast on an unwritable audit path** (before any mutation); a mid-run `Log`
  error warns but does not abort.
- **No new dependency. No RBAC/Helm change** (chart PATCH; `--fix` runs with the
  operator's kubeconfig, audit file is operator-side).
- **Golden snapshot unchanged.** No `Co-Authored-By: Claude` trailer. **TDD.**
  gofmt-clean.

## Out of scope (YAGNI)

Log rotation / retention / max-size; reading the audit log back (slices 3/4);
an `auditLog` array in the JSON report; syslog / webhook / stdout sinks; per-record
signing or hashing; multi-writer locking (single-process CLI, `O_APPEND` is atomic for
short lines); recording non-`--fix` diagnostics.

## Testing

- **`RecordFor` (pure):** each disposition maps correctly; `Changes`/`Target`/`Kind`
  pass through; `Time` is RFC3339 UTC (inject a fixed `now`); a node action has empty
  `Namespace`.
- **`Writer.Log`:** writes exactly one line per call, each line is a standalone JSON
  object that round-trips via `json.Unmarshal`; two calls produce two parseable lines;
  a failing underlying writer surfaces the error.
- **`remediate` refusal flag:** `Refused` true on the drift bond, the no-target case,
  the protected-namespace guard, and the uncordon precondition; false on a clean apply.
  (Extend the existing Apply tests; assert `Applied`/`Refused`/`Err` together.)
- **`runFixes` end-to-end (fake clientset + `audit.Writer` over a `bytes.Buffer`):**
  one record per action with the right disposition across dry-run, declined (stdin "n"),
  applied, refused (drift setup), and error (injected reactor); records absent when the
  writer is nil; every emitted line parses as JSON with the expected `disposition`.
- **Live gate:** full chaos suite — the fix scenarios run `--fix ... --audit-log <tmp>`
  and the harness confirms the file gains a well-formed record.

## Release

- **Gate:** touches the `--fix` write path (new `runFixes` param, `Result.Refused`) →
  **FULL CHAOS GATE** (`./chaos/run.sh --recreate`).
- **Version:** minor **v0.51.0 → v0.52.0**.
- **Chart:** **PATCH** — no RBAC/Helm/template change.

## Files touched

- **Create:** `internal/audit/audit.go` (+ `audit_test.go`) — `Record`, `RecordFor`,
  `Writer`, `NewWriter`, `Log`.
- **Modify:** `internal/remediate/remediate.go` (+ test) — the `Result.Refused` flag on
  the refusal returns.
- **Modify:** `main.go` (+ `main_test.go`) — `--audit-log` flag, fail-fast file open,
  `runFixes` audit-writer param + per-disposition logging.
- **Docs:** `website/docs/features/remediation.md`, `README.md`, `CHANGELOG.md`,
  `website/docs/roadmap.md` (Theme-D slice-2 shipped bullet).
