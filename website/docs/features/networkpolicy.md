# NetworkPolicy hints

When a workload is degraded with no detector finding — for example, pods are
`Running` but never become `Ready` — `scan` names the NetworkPolicies whose
`podSelector` matches the pods of that workload.

## What you see

```text
  ⚠ NetworkPolicy: pods selected by deny-all — may be blocking traffic
```

The policy names are shown inline with the workload entry and are included in
what is sent to `--explain`.

## Scope and limitations

This is a **hint, not a verdict**. `kubeagent` does not analyze policy rules or
know what traffic the pod needs. It points you at the policies to inspect — the
investigation is yours to complete.

Only policy names are sent to the model. Policy rules, CIDR blocks, and port
lists are not included.

Checks are read-only and namespace-scoped.

!!! note
    Some CNIs — for example, **kindnet** (the default in `kind` clusters) — do
    not enforce NetworkPolicies at all. If your cluster uses such a CNI,
    NetworkPolicy hints will still be reported, but the policies have no runtime
    effect. Check your CNI's documentation to confirm enforcement support. The
    [Platform facts](platform-facts.md) line can help you identify which CNI is
    in use.
