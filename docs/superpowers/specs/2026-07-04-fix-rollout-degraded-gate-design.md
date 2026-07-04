# kubeagent — Design: `--fix` RolloutUndo degraded-only gate

**Status:** approved design (pre-implementation)
**Date:** 2026-07-04

## Goal

Make the opt-in `--fix` `RolloutUndo` remediation smarter and more conservative:
propose a rollback **only when the Deployment is actually degraded** (fewer ready
replicas than desired), not when the previous revision is still fully serving.

## Motivation (real production case)

A `scan --fix --dry-run` against a live cluster proposed rolling back
`troweb-cms-production/the-importer`, which was **`1/1 Running`** — fully
available. The Deployment had two ReplicaSets live: the previous revision (5)
serving `1/1`, and a new revision (6) stuck in `ImagePullBackOff` (a bad image
ref / registry auth). The rollout was *stuck* but nothing was *down*, so an
automatic rollback was unwanted — it would abandon the operator's intended new
image for a non-emergency.

The current trigger checks only "has an ImagePull/ErrImagePull finding" **and**
"a prior revision exists" — it never checks whether the Deployment is losing
availability. That is the flaw this design fixes.

## Decision (from brainstorming)

- **Gate on `Ready < Desired`** (numeric availability) — chosen over gating on
  `Status == "Degraded"` (couples to one string label) and over correlating the
  failing pod to the newest ReplicaSet (more rigorous but over-engineered for
  this case; YAGNI). The numeric check is the exact signal `Workload.Flagged()`
  already uses.
- **Stay silent** for a stuck-but-serving rollout — `--fix` proposes nothing for
  it. The `ImagePullBackOff` still surfaces in the normal scan and `--explain`
  output, so visibility is unchanged; only the rollback *proposal* is withheld.

## Invariants / constraints (unchanged framework)

- **READ-ONLY by default.** This only makes `--fix` propose *less*; no new write
  path, no change to `Apply`, guard rails intact (allowlist, protected
  namespaces, apply-time precondition re-check, per-action confirmation, never
  LLM-decided).
- **Allowlist unchanged** — `{RolloutUndo, Uncordon}`. Uncordon is untouched.
- **No new Go module dependency.** `Ready`/`Desired` are already fields on
  `inventory.Workload` passed into `Plan`.
- `--fix` stays fully decoupled from `--explain`.

## The change (`internal/remediate/remediate.go`)

In `Plan`'s Deployment loop, add one condition. `RolloutUndo` is proposed only
when **all** hold:

1. `w.Kind == "Deployment"`
2. `!protectedNamespaces[w.Namespace]`
3. `hasImagePullFinding(w)` (`ImagePullBackOff` or `ErrImagePull`)
4. **`w.Ready < w.Desired`** — the Deployment is degraded (new)
5. `previousRevision(...) != ""`

A Deployment meeting its replica target (`Ready == Desired`, e.g. `1/1`) — the
still-serving stuck-rollout case — yields no action. A `Desired == 0`
(scaled-to-zero) Deployment also yields no action, since `0 < 0` is false.

### Behavior matrix

| State | Ready/Desired | Before | After |
|-------|---------------|--------|-------|
| Stuck rollout, prior revision still serving | `1/1` | proposes rollback | **silent** |
| Rollout dropped old pods, new can't pull | `0/1`, `2/3` | proposes rollback | proposes rollback |
| Healthy | `n/n` | silent | silent |

No change to `Apply`, to the `Action` fields, to the printed proposal format, or
to which findings appear in the scan.

## Testing (TDD)

Unit tests in `internal/remediate/remediate_test.go`:

- The existing "proposes RolloutUndo" fixtures build `Ready=0, Desired=0`; update
  them to a genuinely degraded Deployment (`Desired: 1, Ready: 0`) so they still
  trigger under the new gate. This affects `TestPlan_ProposesRolloutUndo`,
  `TestPlan_ErrImagePullAlsoTriggers`, and `TestPlan_EmitsBothRolloutUndoAndUncordon`
  (and the `dep` helper, which should set the degraded state).
- **New:** a Deployment with an ImagePull finding **and** a prior revision but
  `Ready == Desired` (e.g. `1/1`) → **zero actions** (the the-importer case).
- Keep unchanged: protected-namespace skip, non-Deployment skip, no-prior-revision
  skip, and all `Uncordon` / `Apply` tests.

## Chaos harness (`chaos/README.md`)

The documented `RolloutUndo` acceptance recipe currently injects a bad image into
a 3-replica Deployment that stays **`2/2 Running`** (still serving) — which under
this change would *correctly* no longer propose a rollback. Update the recipe so
the rollout is genuinely degraded (`kubectl patch` the Deployment strategy to
`maxSurge=0, maxUnavailable=1` before setting the bad image, so the failing new
pod replaces the old one → `0/1`), keeping the acceptance meaningful. Scenario 9's
automated scan assertion (detects `ImagePullBackOff`) is unaffected.

## Docs

- `website/docs/features/remediation.md`: note RolloutUndo fires only for a
  **degraded** Deployment; a still-serving stuck rollout is left alone.
- `README.md`: same one-line qualification wherever `--fix`/RolloutUndo is
  described.
- `CHANGELOG.md` `[Unreleased] → Changed`: the degraded-only gate.

## Out of scope (explicit non-goals)

- Any change to `Uncordon` or the allowlist.
- Fix-forward suggestions (correcting the image/auth) — that is `--explain`'s
  job, never a `--fix` write.
- Correlating the failing pod to a specific ReplicaSet revision.
- Any change to what data the scan collects (the availability fields already
  exist).
