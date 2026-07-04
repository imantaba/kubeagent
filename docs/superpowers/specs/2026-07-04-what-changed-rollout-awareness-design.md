# kubeagent — Design: "what changed" rollout awareness

**Status:** approved design (pre-implementation)
**Date:** 2026-07-04

## Goal

When a Deployment is flagged (degraded or carrying a finding), tell the operator
**what changed and when** — correlating the problem with its most recent rollout.
Most production incidents trace to a recent change; surfacing "degraded since the
rollout to revision 6, 4 days ago, which introduced image X" turns a symptom list
into a lead. Deterministic, read-only, grounded in data kubeagent already
collects.

## Decision (from brainstorming)

- **Content: timing + image delta.** For a flagged Deployment, show the current
  rollout's revision and age, plus the image change vs the previous revision
  (image segment omitted when unchanged). Purely factual.
- **Recency-gated.** Annotate only when the current rollout is recent (within a
  hardcoded **7-day** window). A flagged Deployment whose last rollout was months
  ago gets no line — its problem almost certainly is not that rollout. Keeps
  signal high and avoids implying a stale rollout caused a fresh incident.
- **No causal claim.** State what changed and when; never assert the rollout
  *caused* the symptom. The human (or `--explain`) connects the dots.

## Invariants / constraints (unchanged)

- **READ-ONLY.** Uses `inputs.ReplicaSets`, already collected — no new API calls,
  no extra egress, no writes. Independent of `--fix`.
- **Structured-facts-only for `--explain`.** The rollout fact (revision, age,
  image) is already in the report; feeding it to the prompt adds no sensitive
  data.
- **No new Go module dependency.** `k8s.io/api/apps/v1` and the revision
  annotation parsing are already used by `internal/remediate`.

## Component 1 — data model (`internal/inventory`)

Add a struct and an optional field on `Workload`, alongside the existing
`NetworkPolicies` hint field (so an annotator package can set it without an
import cycle):

```go
type RolloutChange struct {
    Revision string `json:"revision"`           // e.g. "6"
    Since    string `json:"since"`              // e.g. "4d ago"
    OldImage string `json:"oldImage,omitempty"` // prior revision's image
    NewImage string `json:"newImage,omitempty"` // current revision's image
}
```

On `Workload`:

```go
Rollout *inventory.RolloutChange `json:"rollout,omitempty"`
```

`nil` when there is no recent rollout to report.

## Component 2 — the annotator (`internal/rollout`)

A pure function mirroring `netpolicy.Annotate`:

```go
func Annotate(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, now time.Time)
```

For each workload where `w.Flagged()` and `w.Kind == "Deployment"`:

1. Find the **current rollout**: the ReplicaSet owned by this Deployment
   (OwnerReference Kind `Deployment`, matching name, same namespace) with the
   highest `deployment.kubernetes.io/revision`.
2. **Recency gate:** if the current ReplicaSet's `CreationTimestamp` is older
   than `now - 7*24h`, leave `w.Rollout` nil and continue.
3. Set `w.Rollout`:
   - `Revision` = the current revision string.
   - `Since` = `inventory.HumanSince(currentRS.CreationTimestamp, now)` (e.g.
     "4d ago").
   - `OldImage`/`NewImage` = the first-container image of the previous-revision
     ReplicaSet and the current one, **only if they differ**; both left empty
     when unchanged or when there is no prior revision.

Pure and deterministic: `now` is injected. No mutation beyond setting `w.Rollout`.

The revision parsing (`deployment.kubernetes.io/revision`, owner matching,
highest/second-highest revision) reuses the same logic pattern as
`remediate.previousRevision`.

## Component 3 — wiring (`main.go`)

One call, right after `netpolicy.Annotate`:

```go
rollout.Annotate(result.Workloads, inputs.ReplicaSets, time.Now())
```

`inputs.ReplicaSets` is already fetched by `collect.CollectInventory`.

## Component 4 — render (`internal/report`)

In `printWorkload`, after the findings / NetworkPolicy lines, when
`wl.Rollout != nil` print one neutral, factual line:

```
    ↳ changed: rollout to revision 6, 4d ago · image nginx:1.27-alpine → nginx:does-not-exist-9999
```

The `· image A → B` segment is omitted when `OldImage`/`NewImage` are empty
(unchanged image). JSON output carries the structured `rollout` field
automatically.

## Component 5 — `--explain` integration (`internal/explain`)

`buildInventoryPrompt` includes the rollout-change fact for a workload when
present — e.g. a line like `recent change: rolled out to revision 6 4d ago,
image nginx:1.27-alpine → nginx:does-not-exist-9999`. This lets the summary lead
with the likely trigger. Stays structured-facts-only (revision / age / image —
all already surfaced in the report); the egress-guard test must still pass.

## Testing (TDD)

- **`internal/rollout` (pure, fake ReplicaSets):**
  - recent rollout with an image change → `Rollout` set with revision, age, and
    Old/New image.
  - rollout older than the 7-day window → `Rollout` nil.
  - recent rollout, image unchanged → `Rollout` set with revision/age, no image
    delta.
  - flagged Deployment with a single revision (no prior) → revision/age, no image
    delta.
  - non-Deployment workload, or a not-flagged Deployment → `Rollout` nil.
  - both `RolloutUndo`-style bad rollout and a healthy one present → only the
    flagged one annotated.
- **`internal/report`:** a workload with `Rollout` set renders the `changed:`
  line (with and without the image segment); JSON includes the `rollout` field.
- **`internal/explain`:** `buildInventoryPrompt` includes the rollout fact when
  present; the existing egress-guard / skip-path tests still pass.
- **`main`:** wiring compiles and `rollout.Annotate` runs in the pipeline (no
  behavior change when there are no recent rollouts).

Real-world validation: `scan` against a cluster with a recent bad rollout (the
degraded-gate chaos recipe already produces one) shows the `changed:` line.

## Docs

- `README.md`: note the "what changed" line under the scan/diagnostics
  description.
- `CHANGELOG.md` `[Unreleased] → Added`: rollout-change awareness.
- `website/docs/features/diagnostics.md`: a short note on the recent-rollout
  correlation (no new nav entry — keep it on the existing diagnostics page).

## Out of scope (explicit non-goals)

- StatefulSet / DaemonSet history (ControllerRevisions) — a separate later
  effort.
- Multi-container image diffs (first container only in v1).
- Diffing config / env / resources / replicas — image + timing only.
- Any causal assertion ("this rollout caused the failure").
- A user-tunable recency window (hardcode 7 days; revisit if asked).
- Any `--fix` interaction.
