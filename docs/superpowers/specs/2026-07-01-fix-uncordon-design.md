# kubeagent — Design: `--fix` Uncordon remediation

**Status:** approved design (pre-implementation)
**Date:** 2026-07-01

## Goal

Add a second remediation to the opt-in, guard-railed `--fix` framework:
**`Uncordon`** — make a mistakenly-cordoned node schedulable again. This runs on
the existing rails (opt-in, per-action confirmation, dry-run, allowlist,
apply-time precondition re-check, single write) established by the v1
`RolloutUndo` action.

## Decision (from brainstorming)

Uncordon is the cleanest next remediation: deterministic, reversible, and
kubeagent already detects cordoned nodes. It was chosen over CoreDNS-Corefile
restore (kubeagent lacks a trusted source of "good" and would clobber custom DNS
config) and OOM-limit raise (needs a value judgment).

## Invariants / constraints (unchanged framework)

- **READ-ONLY by default.** Uncordon runs only under `--fix`; the only write is a
  single `Nodes().Update`. Never LLM-decided.
- **Allowlist grows to `{RolloutUndo, Uncordon}`** — nothing else is ever planned
  or applied. `Apply` switches on `Action.Kind`; an unknown kind is a no-op error.
- **No new Go module dependency** (`k8s.io/api/core/v1` already present).
- Without `--fix`, behavior is byte-identical to today.

## Trigger (in `Plan`)

Propose an `Uncordon` for a `corev1.Node` where BOTH hold:
1. `Node.Spec.Unschedulable == true` (cordoned — kubeagent already reports this as
   `SchedulingDisabled` in `clusterhealth`).
2. The node has **no `NoExecute` taint**. The auto
   `node.kubernetes.io/unschedulable:NoSchedule` taint (always present on a
   cordoned node) is expected and ignored; a `NoExecute` taint means workloads are
   being actively evicted (a deliberate drain, or `NotReady`/pressure), so we do
   NOT propose uncordoning it. This isolates the "accidentally cordoned but
   otherwise healthy" case.

Per-action `[y/N]` confirmation (default No) is the primary human gate for "was
this cordon intentional (maintenance)?" — the harness proposes; the operator
decides.

## Action

```go
Action{
    Kind:              "Uncordon",
    Namespace:         "",              // nodes are cluster-scoped
    Name:              "<node>",
    Target:            "node/<node>",   // display (see Target field below)
    Summary:           "uncordon the node (make it schedulable)",
    Reason:            "node is cordoned (SchedulingDisabled)",
    KubectlEquivalent: "kubectl uncordon node/<node>",
}
```

## Apply (client-go)

For `Kind == "Uncordon"`:
1. `Nodes().Get(name)`.
2. Re-check the precondition against live state: still `Spec.Unschedulable == true`
   AND still no `NoExecute` taint; if not, return a **no-write skip** Result.
3. Set `node.Spec.Unschedulable = false` and `Nodes().Update(...)`. This single
   Update is the only write.
4. Result detail: `uncordoned node <name>`. Reversible via `kubectl cordon`.

The namespace guard (`protectedNamespaces`) is not applicable to node actions
(nodes have no namespace); the allowlist + precondition + confirmation are the
guards.

## Framework changes (small)

- **`Action` gains a `Target string`** display field. `runFixes` prints
  `a.Target` instead of the hardcoded `%s/%s (Deployment)`. The existing
  `RolloutUndo` planner sets `Target = "<ns>/<name> (Deployment)"`.
- **`Plan` signature:** `Plan(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, nodes []corev1.Node) []Action` — it now also emits `Uncordon` actions. Existing callers/tests pass `nodes` (nil ⇒ no Uncordon actions, so RolloutUndo tests are unaffected once they add the third arg).
- **`Apply`** switches on `Kind`: `RolloutUndo` (unchanged) vs `Uncordon` (new helper).
- **`main.go`:** `nodes` (already fetched for `clusterhealth`/`resources`) is passed into `runFixes` → `Plan`.

## Testing (TDD)

- **`Plan` (pure):** a cordoned node with no `NoExecute` taint → one `Uncordon`;
  a schedulable node → none; a cordoned node WITH a `NoExecute` taint → none;
  the ignored `unschedulable:NoSchedule` taint alone still yields an `Uncordon`;
  a workload `RolloutUndo` and a node `Uncordon` are both emitted together.
- **`Apply` (fake clientset):** uncordons a cordoned node (`Spec.Unschedulable`
  is `false` after Update); precondition gone at apply time (node already
  schedulable, or gained a `NoExecute` taint) → no write, skip Result; unknown
  kind still errors.
- **`main` `runFixes`:** `--dry-run` records no `update` verb on a cordoned-node
  fixture; `--yes` applies (node becomes schedulable). Flag parsing unchanged.
- **Live acceptance:** `kubectl cordon` a Kind worker → `scan --fix --yes` →
  proposes and applies `Uncordon`, node schedulable again.

## Docs

- `README.md` `### Remediation (--fix, opt-in)`: note the second action
  (`Uncordon`) alongside `RolloutUndo`.
- `CHANGELOG.md` `[Unreleased] → Added`: the `Uncordon` remediation.
- `chaos/README.md`: an uncordon acceptance note (chaos #3 is the match).
- `CLAUDE.md`: unchanged (the invariant already covers "fixed allowlist"; the
  allowlist simply grows).

## Out of scope (explicit non-goals)

- Distinguishing intentional maintenance cordons from accidental ones beyond the
  `NoExecute`-taint heuristic (the per-action confirmation is the real gate).
- Uncordoning nodes with `NoExecute` taints, draining/re-scheduling logic, or any
  node action other than clearing `Spec.Unschedulable`.
- CoreDNS-restore, OOM-limit, or other remediations (separate future cycles).
- Any LLM involvement in planning or applying.
