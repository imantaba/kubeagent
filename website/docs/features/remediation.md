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
| `RolloutUndo` | a Deployment's newest rollout cannot pull its image and a prior revision exists | rolls the Deployment back to its previous revision | `kubectl -n <ns> rollout undo deployment/<name>` |
| `Uncordon` | a node is cordoned (`SchedulingDisabled`) with no `NoExecute` taint | makes the node schedulable again | `kubectl uncordon <node>` |

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

## Example

```text
Proposed fix: chaos-rollout/web (Deployment) — roll back to the previous revision
  reason: newest rollout cannot pull its image; a prior revision (1) exists
  kubectl equivalent: kubectl -n chaos-rollout rollout undo deployment/web
  Apply? [y/N] y
  applied: rolled back chaos-rollout/web to revision 1 (pod template restored)

Proposed fix: node/worker-1 — uncordon the node (make it schedulable)
  reason: node is cordoned (SchedulingDisabled)
  kubectl equivalent: kubectl uncordon worker-1
  Apply? [y/N] y
  applied: uncordoned node worker-1
```

When nothing is safely fixable, `kubeagent` says so and writes nothing:

```text
No automatic remediations available.
```
