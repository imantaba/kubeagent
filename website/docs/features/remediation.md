# Remediation (`--fix`)

`scan --fix` proposes — and, after you confirm, applies — safe, reversible fixes
for a small set of problems `kubeagent` already detects.

!!! warning
    `--fix` is the **only** feature that writes to your cluster. Without it,
    `kubeagent` is strictly read-only. Every write is **deterministic** (never
    decided by `--explain` or any model), drawn from a **fixed allowlist**, and
    applied only **after a per-action confirmation**.

## The actions

Only these actions are ever planned or applied — nothing outside the allowlist:

| Action | Proposed when | What it does | `kubectl` equivalent |
|--------|---------------|--------------|----------------------|
| `RolloutUndo` | a Deployment is **degraded** (Ready < Desired) because its newest rollout cannot pull its image, and a prior revision exists | rolls the Deployment back to its previous revision | `kubectl -n <ns> rollout undo deployment/<name>` |
| `Uncordon` | a node is cordoned (`SchedulingDisabled`) with no `NoExecute` taint | makes the node schedulable again | `kubectl uncordon <node>` |

A rollout that is stuck on `ImagePullBackOff` but whose previous revision is still
serving (`Ready == Desired`) is **not** rolled back — the app is not down, so the
image is left for you to fix forward.

An accidental cordon is uncordoned; a deliberate drain (which carries a
`NoExecute` taint) is left alone.

## Guard rails

- **Opt-in.** Writes happen only with `--fix`; the default is read-only.
- **Per-action confirmation.** Each fix prints its target, reason, and `kubectl`
  equivalent, then prompts `Apply? [y/N]` — the default is **No**.
- **Protected namespaces.** Remediations never target `kube-system`,
  `kube-public`, or `kube-node-lease`.
- **Apply-time re-check.** Just before writing, the problem is re-verified against
  live cluster state; if it has already resolved, the action is skipped with no
  write.
- **Single write per action.** One API update — nothing cascading.
- **Never model-decided.** The plan is pure, deterministic logic. `--explain` is
  never consulted, and no model ever influences a write.

## Preview and non-interactive

```bash
# show what would be done — write nothing, never prompt
./kubeagent scan --fix --dry-run

# apply every proposed fix without prompting (CI / scripted use)
./kubeagent scan --fix --yes
```

`--fix --dry-run` against a cluster with a bad rollout and a cordoned node: it runs the
normal scan, then lists the proposed fixes — a `RolloutUndo` and an `Uncordon` — and
exits without writing anything:

![kubeagent scan --fix --dry-run](../assets/fix-dry-run.gif)

## The preview is a contract

Every proposed fix includes a curated `will change:` diff — the revision line,
a per-container image change (`image (name): old → new`), and a count-only
`N other template fields changed` line for anything else — computed at plan
time, before `Apply?` is shown. **Only safe, structural fields are diffed: env
values and template contents are never shown.**

`Apply` is bound to that preview. Just before writing, kubeagent re-checks the
live revision and pod-template hash. If the cluster has moved — a new rollout
landed, or the target revision is gone — the action is refused with
`state changed since preview … no write made` and rendered as `skipped:`. To
retry, re-run the scan so a fresh preview is computed against current state.

## Example

```text
Proposed fix: shop/web (Deployment) — roll back to the previous revision
  reason: newest rollout cannot pull its image; a prior revision (1) exists
  will change:
    revision: 2 → 1
    image (web): registry.example.com/web:v2 → registry.example.com/web:v1
  kubectl equivalent: kubectl -n shop rollout undo deployment/web
  Apply? [y/N] y
  applied: rolled back shop/web to revision 1 (pod template restored)

Proposed fix: node/worker-1 — uncordon the node (make it schedulable)
  reason: node is cordoned (SchedulingDisabled)
  will change:
    spec.unschedulable: true → false
  kubectl equivalent: kubectl uncordon worker-1
  Apply? [y/N] y
  applied: uncordoned node worker-1
```

When nothing is safely fixable, `kubeagent` says so and writes nothing:

```text
No automatic remediations available.
```

## JSON output (`--output json`)

With `--output json`, the remediation plan is included in the scan result as
`remediationPlan` — an array of proposed actions, each with `status: "proposed"`.
This is the foundation for the coming audit log.

```json
{
  "remediationPlan": [
    {
      "kind": "RolloutUndo",
      "target": "shop/web (Deployment)",
      "summary": "roll back to the previous revision",
      "reason": "newest rollout cannot pull its image; a prior revision (1) exists",
      "kubectlEquivalent": "kubectl -n shop rollout undo deployment/web",
      "changes": [
        { "field": "revision", "from": "2", "to": "1" },
        { "field": "image (web)", "from": "registry.example.com/web:v2", "to": "registry.example.com/web:v1" }
      ],
      "status": "proposed"
    }
  ]
}
```
