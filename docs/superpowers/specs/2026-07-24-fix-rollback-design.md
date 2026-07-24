# `--fix` rollback (design)

**Status:** approved · **Date:** 2026-07-24 · **Type:** write-path hardening (Theme D,
trustworthy remediation — slice 4, final: rollback)

## Problem

`--fix` can now preview precisely what it will change (slice 1), record every outcome
durably (slice 2), and prove the operator may perform the write (slice 3). What it
cannot do is **undo**. An operator who applies a rollback and then decides it was the
wrong call has no supported way back — and the audit log that knows exactly what
happened is write-only.

This slice closes the arc: `kubeagent scan --rollback --audit-log <path>` reads the most
recent **applied** record, derives the deterministic inverse action, and runs it through
the **same guard rails** as any fix — curated preview diff, drift bond, RBAC preflight,
per-action confirmation, audit record. It is the undo button that makes `--fix` safe to
use confidently.

## Scope decisions (locked)

| Decision | Choice |
|----------|--------|
| Source | **Read the `--audit-log` applied records** — works across runs (undo yesterday's fix), making the slice-2 log load-bearing |
| Scope per invocation | **The last applied action only** — re-run to walk back further, one confirmed step at a time |
| Inverse derivation | **Structured record fields** (`fromRevision`/`toRevision`) → a normal allowlisted Action through every guard rail; audited with a new **`rollback`** disposition |

## Architecture

```
audit log (JSONL) ──ReadLast(applied)──▶ Record ──(main adapts)──▶ remediate.Inverse(...)
                                                                        │
                                                    Action{RolloutForward|Cordon}
                                                                        │
                          preview diff → [y/N] → drift bond → RBAC preflight → write
                                                                        │
                                                        audit: disposition "rollback"
```

### 1. `internal/audit` — structured rollback data + a reader

`Record` gains two fields (both omitted for Uncordon, whose inverse needs no data):

```go
	FromRevision int `json:"fromRevision,omitempty"` // RolloutUndo: revision before the fix
	ToRevision   int `json:"toRevision,omitempty"`   // RolloutUndo: revision the fix landed on
```

`RecordFor` populates them from the Action's `CurrentRevision` / `TargetRevision`
(which slice 1 already carries).

New reader — streams the JSONL file and returns the newest matching record:

```go
// ReadLast scans the audit file and returns the most recent record satisfying want.
// Malformed lines are skipped (a truncated tail must not break rollback); found is
// false when no record matches.
func ReadLast(path string, want func(Record) bool) (rec Record, found bool, err error)
```

Rollback calls it with `func(r Record) bool { return r.Disposition == "applied" }`.

### 2. `internal/remediate` — two new allowlisted inverse kinds

**Import direction matters:** `audit` imports `remediate` today, so `Inverse` must NOT
take an `audit.Record` (that would create a cycle). It takes plain values; `main.go`
adapts the record:

```go
// Inverse returns the deterministic undo of a previously applied remediation. Pure:
// no I/O, never LLM-decided. It errors when the kind is unknown or when a RolloutUndo
// record predates structured rollback data (pre-v0.54 audit files).
func Inverse(kind, namespace, name string, fromRevision, toRevision int) (Action, error)
```

| Applied kind | Inverse kind | Meaning |
|--------------|--------------|---------|
| `RolloutUndo` | **`RolloutForward`** | restore the deployment to `fromRevision` (the revision it had before the fix) |
| `Uncordon` | **`Cordon`** | set `spec.unschedulable = true` again |

The returned Action carries the same fields a planned Action does — `Target`, `Summary`
("roll forward to the pre-fix revision" / "re-cordon the node"), `Reason` ("undo the fix
applied at …"), `KubectlEquivalent`, `Changes` (`revision: 4 → 5`, or
`spec.unschedulable: false → true`), and for RolloutForward `CurrentRevision =
toRevision`, `TargetRevision = fromRevision` so the **drift bond applies unchanged**: if
the cluster moved since the fix, the rollback refuses rather than clobbering.

**Backward compatibility:** a `RolloutUndo` record without `fromRevision` (written by
v0.52–v0.53) yields an error — `"this audit record predates structured rollback data
(kubeagent < v0.54); cannot derive a safe rollback"`. No parsing of display strings.

`Apply` gains the two kinds:

- `applyRolloutForward` — reuses the existing revision machinery (`ownedBy`,
  `revFromAnnotations`, `templatesEqual`): find the owned ReplicaSet at
  `TargetRevision`, verify the deployment is still at `CurrentRevision` (drift bond),
  run the **RBAC preflight** (`update apps/deployments` in the namespace), then restore
  that ReplicaSet's pod template (`pod-template-hash` stripped) exactly as
  `applyRolloutUndo` does. Missing target revision → `Refused` ("revision N no longer
  exists; no write made").
- `applyCordon` — get the node; if it is already `Unschedulable`, `Refused` ("node is
  already cordoned; no write made"); otherwise **RBAC preflight** (`update nodes`), then
  set `Unschedulable = true`.

Both honour the protected-namespace guard and produce `Result{Applied|Refused|
PreflightDenied|Err}` exactly like the existing kinds.

### 3. `main.go` — the `--rollback` flag

- `--rollback` (bool): "undo the most recent applied fix recorded in --audit-log
  (requires --audit-log)".
- Preconditions, checked before any cluster work: `--rollback` requires `--audit-log`;
  `--rollback` and `--fix` are **mutually exclusive** (error if both).
- Flow (`runRollback`, a sibling of `runFixes`):
  1. `audit.ReadLast(path, applied)` → no record → `"no applied remediation found in
     <path>; nothing to roll back"` (clean exit, not an error).
  2. `remediate.Inverse(...)` → on error (unknown kind / pre-v0.54 record), print it and
     stop.
  3. Print the proposal exactly like a fix — target, reason, `will change:` diff,
     kubectl equivalent — then `--dry-run` (report + audit `dry-run`), `[y/N]` unless
     `--yes` (decline → audit `declined`).
  4. `remediate.Apply` → dispositions: applied → **`rollback`**; `PreflightDenied` →
     `preflight`; `Err` → `error`; otherwise → `refused`. Same Err-first ordering.
- The audit writer is the same one `--fix` uses (the file named by `--audit-log`), so a
  rollback appends to the same trail it read from.

### 4. Vocabulary

Audit dispositions become `dry-run | declined | applied | refused | preflight | error |
rollback`. A successful undo is `rollback` (not `applied`), so the trail distinguishes
"kubeagent fixed something" from "kubeagent undid a fix".

## Global constraints

- **Never LLM-decided.** `Inverse` is a pure deterministic mapping over an allowlist;
  the model is not consulted anywhere in this path.
- **Same guard rails.** Rollback writes go through the identical preview → confirm →
  drift bond → RBAC preflight → audit chain as fixes. No new bypass.
- **Fail closed / refuse on drift.** If the cluster moved since the fix (revision
  changed, node already cordoned, target revision gone), refuse with no write.
- **No secrets** in records or output (only revisions, names, counts).
- **No new dependency. No RBAC/Helm change** — the inverse actions use the same
  `update deployments` / `update nodes` verbs already required (chart **PATCH**).
- **Golden snapshot unchanged.** **No `Co-Authored-By: Claude` trailer.** **TDD.**
  gofmt-clean.

## Out of scope (YAGNI)

Undoing more than one action per invocation (re-run to walk back); run-ids or
`--rollback-since <window>`; rollback of a rollback beyond what re-running naturally
does; reading audit logs from another host/format; a `--rollback-last` in-memory
same-run mode; new action kinds beyond the two inverses; compaction/rotation of the
audit file.

## Testing

- **`audit.ReadLast`:** returns the newest matching record among several; skips
  non-`applied` dispositions; skips malformed/truncated lines; `found == false` on an
  empty file or no match; a missing file returns an error.
- **`audit.RecordFor`:** populates `fromRevision`/`toRevision` from the Action; omits
  them (zero) for Uncordon.
- **`remediate.Inverse` (pure):** RolloutUndo(from 5, to 4) → `Action{Kind:
  "RolloutForward", CurrentRevision: 4, TargetRevision: 5}` with a `revision: 4 → 5`
  change; Uncordon → `Action{Kind: "Cordon"}` with `spec.unschedulable: false → true`;
  a RolloutUndo record with `fromRevision == 0` → error mentioning the version; unknown
  kind → error.
- **`Apply` for the new kinds (fake clientset):** RolloutForward with permission and no
  drift restores the target template (assert the image is back); drift (deployment no
  longer at `CurrentRevision`) → `Refused`, **zero writes**; missing target revision →
  `Refused`; denied preflight → `PreflightDenied`, zero writes. Cordon: sets
  `Unschedulable` true; already-cordoned → `Refused`, zero writes; denied preflight →
  zero writes.
- **`runRollback` end-to-end (fake clientset + a temp audit file):** apply a fix, read
  the record back, roll it back → the deployment is at the pre-fix revision and the
  audit file has one `rollback` line; `--dry-run` reports and writes nothing (audit
  `dry-run`); declining writes nothing (audit `declined`); no applied record → the
  "nothing to roll back" message with no write; a pre-v0.54 record (no `fromRevision`)
  → the version-refusal message with no write.
- **`main` preconditions:** `--rollback` without `--audit-log` errors; `--rollback` with
  `--fix` errors.
- **Live gate:** full chaos suite; the faulty-rollout scenario applies a fix with an
  audit log and then rolls it back, confirming the deployment returns to its pre-fix
  image.

## Release

- **Gate:** touches the `--fix` write path (new action kinds, new write paths) →
  **FULL CHAOS GATE** (`./chaos/run.sh --recreate`).
- **Version:** minor **v0.53.0 → v0.54.0**.
- **Chart: PATCH** — no RBAC/Helm/template change (same verbs).

## Files touched

- **Modify:** `internal/audit/audit.go` (+ test) — `FromRevision`/`ToRevision`,
  `RecordFor` population, `ReadLast`.
- **Modify:** `internal/remediate/remediate.go` (+ test) — `Inverse`,
  `applyRolloutForward`, `applyCordon`, `Apply` dispatch for the two kinds.
- **Modify:** `main.go` (+ `main_test.go`) — `--rollback` flag, preconditions,
  `runRollback`, the `rollback` disposition.
- **Modify:** `chaos/run.sh` — extend the faulty-rollout scenario with an apply-then-
  rollback check.
- **Docs:** `website/docs/features/remediation.md`, `README.md`, `CHANGELOG.md`,
  `website/docs/roadmap.md` (Theme-D slice-4 shipped bullet; Theme D complete).
