# `--fix` / `--rollback`: retry a write that hits a conflict — design

Date: 2026-10-07. Status: approved direction, spec for planning.

## Summary

Every `--fix` and `--rollback` write reads an object, runs its safety checks,
and saves it with `Update`. `Update` carries the `resourceVersion` the read
returned. If anything else changed the object in between, the API server
refuses the save with a 409 Conflict. kubeagent does not try again, so the
action ends as `ERROR` and the audit log records `"disposition":"error"`.

That happened in the nightly chaos matrix (k3s, v1.34, scenario 9b, run
37599174800). `--rollback` ran two seconds after `--fix`. The Deployment
controller was still writing the Deployment it had just been handed, so the
version moved under kubeagent and the save failed:

```text
--rollback outcome: ERROR: update deployment: Operation cannot be fulfilled on
deployments.apps "web": the object has been modified; please apply your
changes to the latest version and try again
```

kind cells and earlier k3s runs passed. The failure depends on timing, not on
the distribution.

This slice makes each of the four write paths retry on a conflict, re-reading
the object and re-running every safety check on the fresh copy each time. A
harmless update by someone else no longer fails the action. A real change by
someone else is still refused, exactly as today.

## Decisions (locked with the operator)

1. **Retry inside `internal/remediate`, not in the CLI.** The package that
   owns the write rules owns the retry. `--fix` and `--rollback` both call
   `remediate.Apply`, so one change covers both.
2. **Keep `Update`; do not switch to a patch.** A patch carries no version and
   never conflicts — which is exactly what makes it wrong here. The version on
   `Update` is what guarantees the save lands only on the copy the checks
   approved. A patch would throw that guarantee away.
3. **Every try re-runs every check.** Read, guards, permission check
   (`Preflight`), save — the whole body is retried, never only the save. A
   fresh copy that fails a guard is `refused`, and nothing is written.
4. **Only a conflict is retried.** A 403, a timeout, a not-found, or any other
   error fails at once, as today.
5. **Bounded: `retry.DefaultRetry`.** client-go's own recommended setting: 5
   tries, about 10 ms apart. If all 5 conflict, the action fails with the same
   error text as today.

## Design

### Where

`internal/remediate/remediate.go`, the four functions `Apply` dispatches to:

| Function | Object | Guards re-run on each try |
|---|---|---|
| `applyRolloutUndo` | Deployment | current/target revision bond, target still exists |
| `applyRolloutForward` | Deployment | target revision exists, template not already equal, per-container image drift |
| `applyUncordon` | Node | still unschedulable, no `NoExecute` taint |
| `applyCordon` | Node | not already cordoned |

The protected-namespace check stays outside the loop: it reads only the
`Action`, which a retry cannot change.

### How

Each function's Get → guards → `Preflight` → mutate → `Update` body moves into
a closure passed to `k8s.io/client-go/util/retry.RetryOnConflict(retry.DefaultRetry, …)`.
The package is part of `k8s.io/client-go`, already a direct dependency at
v0.36.2, so `go.mod` does not change.

- A guard or a denied preflight that stops the action sets the `Result` and
  returns `nil` from the closure, which ends the loop with no write.
- A failed `Get` returns its error. It is not a conflict, so the loop ends at
  once.
- A failed `Update` returns the **raw** error, so `RetryOnConflict` can see it
  is a conflict. The `update deployment: %w` / `update node: %w` wrap happens
  once, after the loop, so the error text an operator sees is byte-identical
  to today's.

### Why this does not weaken the guard rail

The preview an operator approves and the first read inside `Apply` can
already be minutes apart: the `[y/N]` prompt waits for a person. The guards,
not the timing, are the bond between preview and write — that is why they
exist. A retry adds milliseconds to a window that is already open-ended, and
every try passes the same guards on the newest copy. Nothing is ever written
to a copy the guards did not check.

`Preflight` runs again on each try. It is a `SelfSubjectAccessReview` — a
virtual resource the API server evaluates and never stores — so a retry adds
at most four of them and changes no cluster state.

### What stays the same

- CLI output and the `Result` fields, including every `Detail` string.
- One audit record per action. The record shows the outcome, not the number
  of tries.
- No new flag, no new environment variable.
- No JSON shape changes; no `schemaVersion` moves.
- `Plan`, `Inverse`, `Preflight` and the read-only commands are untouched.

## Testing (TDD, fake clientset)

All tests live in `internal/remediate/remediate_test.go` and use client-go's
fake clientset with `PrependReactor` on `update`, as the existing preflight
tests already do on `create selfsubjectaccessreviews`.

1. **Conflict once, then success — one test per write function.** The reactor
   returns `apierrors.NewConflict` on the first `update` and passes through
   after. Expect `Applied`, no `Err`, and the object written.
2. **Conflict, and the fresh copy now fails a guard.** The reactor returns a
   conflict and, before returning, changes the stored object so the guard
   fails (for `applyRolloutUndo`, a new revision; for `applyUncordon`, the
   node already schedulable). Expect `Refused`, no `Err`, and no change to the
   object beyond what the reactor made.
3. **Conflict every time.** Expect `Err` that is a conflict, its text starting
   with `update deployment:` (or `update node:`), and exactly 5 `update`
   attempts.
4. **Non-conflict error.** The reactor returns a server timeout. Expect `Err`
   and exactly 1 `update` attempt.

Each new test fails on `main` today (no retry) before the change, except test
4, which pins current behaviour so the change cannot widen what is retried.

The chaos harness needs no change: scenario 9b is the end-to-end check, and
it already asserts the rollback lands.

## Docs

- `website/docs/features/remediation.md`, in "The preview is a contract": one
  short paragraph saying a conflict is retried a few times, each try re-runs
  the same checks, and a fresh copy that fails them is refused.
- `CHANGELOG.md`, under `[Unreleased]`, a `### Fixed` entry.

No new Go concept for `docs/go-concepts.md`: closures handed to a helper
already appear in the bounded worker pool entry.

## Out of scope

- Retrying the read-only commands. They issue no `Update`.
- Changing the backoff, or making it configurable.
- Server-side apply or patches.
