# kubeagent — Design: `--fix` remediation (opt-in, guard-railed)

**Status:** approved design (pre-implementation)
**Date:** 2026-06-30

## Goal

Turn kubeagent from diagnose-and-explain into diagnose-and-**solve** — beginning
with a single, safe, reversible remediation behind an opt-in `--fix` flag. The
real deliverable is the **guard-railed action framework** (opt-in, per-action
confirmation, dry-run, allowlist, protected namespaces, re-verify, audit) built
around one proven action; further fixes are added later, each validated against
the chaos suite.

## The invariant change (deliberate, central)

kubeagent's cardinal rule has been: *"READ-ONLY. Only `List`/`Get`. Never create,
update, patch, or delete."* This feature amends it to:

> **READ-ONLY by default.** The only cluster writes are guard-railed remediations
> behind the opt-in `--fix` flag, confirmed per action, drawn from a fixed
> allowlist. With `--fix` absent, behavior is byte-identical to today.

`CLAUDE.md` and `README.md` are updated to state this. No remediation is ever
decided by the LLM: `--explain` remains read-only and is never in the fix path;
remediations are computed **deterministically**.

## Decisions (from brainstorming)

- **v1 scope:** exactly one remediation — `RolloutUndo` — plus the full framework.
- **Approval:** `scan --fix` proposes each fix and prompts `Apply? [y/N]` (default
  No) per action. `--dry-run` prints proposals only (never prompts or writes).
  `--yes` applies all proposals without prompting (automation opt-in).
- **Execution:** via **client-go** (kubeagent has no kubectl runtime dependency);
  the `kubectl …` equivalent is shown for audit only.

## CLI / UX

```text
kubeagent scan                      # unchanged, read-only
kubeagent scan --fix                # propose + per-action [y/N] confirm + apply
kubeagent scan --fix --dry-run      # propose only; never prompt or write
kubeagent scan --fix --yes          # apply all proposals without prompting
```

Flag rules: `--dry-run` and `--yes` are meaningful only with `--fix`; if both are
given, `--dry-run` wins (preview, no writes — the safe interpretation). `--fix`
runs after the normal scan/diagnose/inventory, on the same collected state.

Output per fixable finding:

```text
Proposed fix: chaos-rollout/web (Deployment) — ImagePullBackOff
  reason: newest rollout pulls a missing image; a prior good revision (rev 2) exists
  action: roll back to the previous revision
  kubectl equivalent: kubectl -n chaos-rollout rollout undo deployment/web
  Apply? [y/N]
```

After applying: an audit line of exactly what was done, plus a re-verify note.

## Architecture — `internal/remediate` (new package)

Separates planning (pure, testable) from execution (writes).

```go
// Action is one proposed, allowlisted remediation. No LLM, no free-form commands.
type Action struct {
    Kind              string // "RolloutUndo" (the only kind in v1)
    Namespace         string
    Name              string // workload name (a Deployment in v1)
    Summary           string // one-line human description
    Reason            string // why it's proposed (the finding + precondition)
    KubectlEquivalent string // shown for audit only; NOT how it executes
}

// Plan inspects diagnosed workloads and returns the safe, allowlisted, precondition-
// satisfied remediations. Pure: it reads the inventory + the collected ReplicaSets,
// writes nothing.
func Plan(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet) []Action

// Result records what Apply did, for the audit line and re-verify.
type Result struct {
    Action   Action
    Applied  bool
    Detail   string // e.g. "rolled back web to revision 2 (template restored)"
    Err      error
}

// Apply performs the action's single write via client-go and re-verifies.
func Apply(ctx context.Context, client kubernetes.Interface, a Action) Result
```

## The v1 remediation: `RolloutUndo`

**Trigger (in `Plan`).** A workload where ALL hold:
1. `Kind == "Deployment"`.
2. It has a finding with `Issue == "ImagePullBackOff"` or `"ErrImagePull"` (the
   newest rollout can't pull its image).
3. Its namespace is **not** protected (see Guards).
4. A **prior revision exists** to roll back to: among the Deployment's owned
   ReplicaSets, there is one with a lower `deployment.kubernetes.io/revision`
   than the current and a pod template that differs from the current.

**Mechanics (in `Apply`, client-go — mirrors `kubectl rollout undo`).**
1. Get the Deployment and list its owned ReplicaSets.
2. Identify the current revision (max `revision` annotation) and the target =
   the highest revision strictly below it whose template differs.
3. Set `Deployment.Spec.Template` to the target ReplicaSet's
   `Spec.Template` (clearing the `pod-template-hash` label the controller manages)
   and `Update` the Deployment. This single Update is the only write.
4. Re-verify: confirm the Deployment's `.spec.template` now matches the target and
   a new rollout has started; report it.

**Reversible:** the rollback is itself a rollout; running `--fix` again (or a
manual `rollout undo`) redoes it. If no valid target is found at apply time
(state changed since Plan), `Apply` makes no write and reports a skip.

## Guards (baked into every action)

- **Protected namespaces:** `Plan` never proposes an action whose target is in
  `kube-system`, `kube-public`, or `kube-node-lease`; `Apply` re-checks and
  refuses. (v1 only targets Deployments, so control-plane static pods are out of
  scope anyway.)
- **Allowlist:** only `Kind == "RolloutUndo"` is ever planned or applied. An
  unknown kind in `Apply` is a no-op error.
- **Preconditions re-checked at apply time** against live state (the prior
  revision still exists and differs); otherwise no write.
- **Re-verify + audit:** every applied action prints exactly what it changed
  (the client-go op and the kubectl equivalent). Nothing about actions is sent to
  `--explain`.
- **Default deny:** without `--fix`, no `remediate` code path runs and no client
  write method is ever called.

## Wiring (`main.go`)

Add `fs.Bool` flags `--fix`, `--dry-run`, `--yes`. After the existing
scan/diagnose/inventory steps, when `*fix` is set:

```go
actions := remediate.Plan(result.Workloads, inputs.ReplicaSets)
for _, a := range actions {
    // print proposal
    if *dryRun { continue }
    if !*yes && !confirm("Apply? [y/N] ") { continue }
    res := remediate.Apply(context.Background(), client, a)
    // print audit + re-verify
}
```

`confirm` reads a `y`/`Y` line from stdin (anything else = No). The report is
printed first (unchanged); `--fix` proposals/actions print after it.

## Testing (TDD)

- **`Plan` (pure, fake workloads + ReplicaSets):** Deployment + ImagePullBackOff +
  a prior differing revision → one `RolloutUndo`; no finding → none; no prior
  revision → none; protected namespace → none; non-Deployment kind → none.
- **`Apply` (fake clientset):** with a 2-revision Deployment whose current
  template is "broken", `Apply` restores the prior template (assert
  `Deployment.Spec.Template` equals the target RS template, hash label cleared);
  precondition gone at apply time → no write, skip result; unknown Action kind →
  no-op error.
- **`main` (flag wiring):** `--fix`/`--dry-run`/`--yes` parse; `--fix --dry-run`
  writes nothing (fake clientset records no Update); `--yes` applies without a
  prompt; without `--fix` the remediate path never runs.
- **Live acceptance (chaos suite):** chaos scenario 9 (faulty rollout) — after
  injection, `scan --fix --yes` rolls the Deployment back and it recovers. A new
  `chaos/run.sh` check (or a documented manual step) asserts this.

## Out of scope (explicit non-goals)

- Any remediation other than `RolloutUndo` (OOM-limit bumps, uncordon, pod
  restart/delete, NetworkPolicy/LB/DNS fixes) — added later, one at a time.
- LLM-decided or free-form actions; sending action context to `--explain`.
- Multi-step or cluster-wide orchestration; rollback of partially-applied changes
  beyond the single Deployment update (the action is itself a single, reversible
  rollout).
- A `kubectl` runtime dependency (execution is client-go only).
