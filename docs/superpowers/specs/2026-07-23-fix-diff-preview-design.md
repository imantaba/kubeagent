# `--fix` plan/diff preview + preview→apply contract (design)

**Status:** approved · **Date:** 2026-07-23 · **Type:** write-path hardening (Theme D,
trustworthy remediation — slice 1: plan/dry-run/diff preview)

## Problem

`--fix` today proposes an action as prose ("roll back to the previous revision") and,
on confirmation, applies it. Two gaps for a production remediation contract:

1. **The preview doesn't show what will change.** The operator approves a sentence,
   not a change. For a RolloutUndo they can't see which revision or which image the
   rollback lands on; `--dry-run` (the CI use case) is equally opaque.
2. **The preview is advisory, not a contract.** `Apply` re-derives its target at
   apply time; if the cluster moved between preview and confirmation, what's applied
   can differ from what was approved — silently.

This slice makes the preview a first-class contract: every proposed action shows a
**curated field-level diff** of exactly what will change, and `Apply` **refuses to
write** if the cluster state no longer matches what was previewed. It is the
foundation the Theme-D audit-log and RBAC-preflight slices attach to.

## Scope decisions (locked)

| Decision | Choice |
|----------|--------|
| Diff content | **Curated field-level diff** — revision, per-container images, safe count of other template changes; never env values or raw template content (no-secrets rule holds by construction) |
| Preview→apply | **Bound: refuse on drift** — Action carries the promised revisions; Apply refuses (no write) if current or target revision moved since preview |
| Machine-readable | **Yes** — JSON output gains `remediationPlan` (proposed actions with diffs) when `--fix` is set |

## Architecture

All logic lives in `internal/remediate` (the existing guardrail home); `main.go`
renders the diff in the proposal and passes the plan to the report; `internal/report`
adds the JSON view. The diff is computed **purely at plan time** from data `Plan`
already receives — no new reads, `Plan` stays pure.

### 1. `internal/remediate` — diff + contract

```go
// Change is one previewed field change, e.g. {"image (web)", "web:v2", "web:v1"}.
// From/To are always safe display values (revisions, image refs, booleans, counts) —
// never env values or raw template content.
type Change struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}
```

`Action` gains:

```go
	Changes         []Change // the previewed field-level diff (rendered + JSON)
	CurrentRevision int      // RolloutUndo: the revision current at preview time (0 for Uncordon)
	TargetRevision  int      // RolloutUndo: the revision the rollback lands on (0 for Uncordon)
```

**Plan-time diff (RolloutUndo).** From the ReplicaSet list `Plan` already receives:
current = the highest-revision RS owned by the Deployment; target = the highest
revision strictly below it **whose pod template differs** (semantic compare with
`pod-template-hash` stripped — the same rule `pickTarget` uses at apply time; this
replaces `previousRevision`'s bare second-highest pick, fixing a latent plan/apply
mismatch). Changes rendered:

- `revision: <cur> → <target>`
- per differing container: `image (<name>): <curImage> → <targetImage>`
- if the templates differ beyond images: one safe line `other template fields: <N>
  changed` (count from the semantic compare; contents never printed)

If no differing prior revision exists, no action is proposed (as today).

**Plan-time diff (Uncordon).** Static: `spec.unschedulable: true → false`.

**The preview→apply bond (`applyRolloutUndo`).** After the existing re-derivation
(`Get` deployment, `List` ReplicaSets, `pickTarget`), add the contract check —
refuse with no write unless BOTH hold:

- `revFromAnnotations(dep.Annotations) == a.CurrentRevision` (no new rollout since
  preview), and
- `revFromAnnotations(target.Annotations) == a.TargetRevision` (the rollback still
  lands where the preview promised).

On mismatch: `res.Detail = "state changed since preview (revision <N> is now
current, previewed <M>) — re-run kubeagent scan --fix; no write made"`, `Applied`
stays false, no error (a refusal, not a failure). ReplicaSet templates are immutable,
so matching revisions ⇒ matching templates — revision equality is a sufficient bond.

**Uncordon** keeps its existing apply-time precondition (still cordoned, no
NoExecute taint) — that already is its bond; nothing else can drift.

### 2. `main.go` — render the diff, plan once

- `runScan` computes `actions := remediate.Plan(...)` **once** when `*fix` is set
  (before `report.PrintInventory`), passes the actions to the report input AND to
  `runFixes` (which no longer calls `Plan` itself).
- `runFixes` prints a `will change:` block in each proposal:

```
Proposed fix: shop/web (Deployment) — roll back to the previous revision
  reason: newest rollout cannot pull its image; a prior revision (4) exists
  will change:
    revision: 5 → 4
    image (web): registry.example.com/web:v2 → registry.example.com/web:v1
  kubectl equivalent: kubectl -n shop rollout undo deployment/web
  Apply? [y/N]
```

  The refusal path prints as `skipped: state changed since preview …` via the
  existing Result.Detail plumbing (no new UI states).

### 3. `internal/report` — JSON `remediationPlan`

- `Input` gains `RemediationPlan []remediate.Action` (nil unless `--fix`).
- JSON output gains, only when the slice is non-nil:

```json
"remediationPlan": [{
  "kind": "RolloutUndo",
  "target": "shop/web (Deployment)",
  "summary": "roll back to the previous revision",
  "reason": "newest rollout cannot pull its image; a prior revision (4) exists",
  "kubectlEquivalent": "kubectl -n shop rollout undo deployment/web",
  "changes": [
    {"field": "revision", "from": "5", "to": "4"},
    {"field": "image (web)", "from": "registry.example.com/web:v2", "to": "registry.example.com/web:v1"}
  ],
  "status": "proposed"
}]
```

  Rendered via a small view struct in `report` (status is the literal `"proposed"`
  in this slice — apply outcomes become durable in the audit-log slice). The **text
  report is unchanged** (the fix preview lives in `runFixes`' own section) — the
  golden snapshot is untouched.

## Global constraints

- **Writes stay guard-railed and opt-in.** No new write paths; `Apply`'s writes only
  get *stricter* (the bond can only refuse writes that would have happened before).
  Protected namespaces, per-action confirmation, `--dry-run`/`--yes` all unchanged.
- **No secrets in output.** The diff renders only revisions, image refs, booleans,
  and counts — never env values, args, or raw template YAML.
- **`Plan` stays pure** (no I/O); the diff is computed from already-collected data.
- **No new dependency. No RBAC change** (`--fix` runs with the operator's
  kubeconfig; the in-cluster daemon never fixes).
- **Golden snapshot unchanged** (text report untouched).
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Full YAML diff / redaction layer; audit log (next slice); RBAC preflight; rollback of
an applied fix; new action kinds; server-side dry-run (`dryRun=All`); apply outcomes
in JSON; re-diff-and-reprompt on drift (refusal is the v1 contract).

## Testing

- **`Plan` diffs (pure, fake objects):** RolloutUndo — image change rendered per
  container; multi-container with one image change; templates differing beyond
  images → the `other template fields: N changed` line (and never their contents);
  target selection skips a same-template higher RS (the pickTarget alignment);
  fewer than 2 revisions → no action. Uncordon — the static unschedulable change.
  Assert `CurrentRevision`/`TargetRevision` populated.
- **The bond (fake clientset):** plan against rev 5→4, then bump the deployment to
  rev 6 (new RS) before `Apply` → refusal Detail, `Applied == false`, and **zero
  Update calls** (assert via the fake's action recorder). Matching state → applies
  exactly as today. Target RS deleted → existing "no differing prior revision"
  refusal still holds.
- **`runFixes` output:** the `will change:` block renders each Change; dry-run
  unchanged otherwise.
- **`report`:** JSON contains `remediationPlan` with changes + `"proposed"` when
  set; key absent when nil; text output byte-identical (golden passes).
- **Live gate:** full chaos suite — its fix scenarios exercise the preview + bond
  end-to-end on a Kind cluster.

## Release

- **Gate:** touches the `--fix` write path → **FULL CHAOS GATE**
  (`./chaos/run.sh --recreate`).
- **Version:** minor **v0.50.0 → v0.51.0**.
- **Chart:** **PATCH** — no RBAC/Helm/template change.

## Files touched

- **Modify:** `internal/remediate/remediate.go` (+ `remediate_test.go`) — `Change`,
  enriched `Action`, plan-time diff, aligned target selection, the apply-time bond.
- **Modify:** `main.go` (+ `main_test.go`) — plan-once wiring, `will change:` block.
- **Modify:** `internal/report/report.go` (+ test) — `RemediationPlan` JSON view.
- **Docs:** `website/docs/features/remediation.md`, `README.md`, `CHANGELOG.md`,
  `website/docs/roadmap.md` (Theme-D first shipped bullet).
