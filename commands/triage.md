---
description: Sweep a cluster or namespace with kubeagent, inspect every serious finding, and report what was and was not checked.
argument-hint: "[namespace]"
---

Triage the cluster with kubeagent. Namespace scope: $1 (empty means all namespaces).

Follow the `triaging-a-cluster` skill for the workflow and the
`reading-kubeagent-findings` skill for how to read what comes back.

1. Preflight: confirm `kubeagent` is on PATH. If not, stop and give the install
   commands.
2. Call `kubeagent_triage`, passing `namespace` only if $1 is non-empty.
3. Read the `coverage` block before the findings.
4. Call `kubeagent_inspect` on every `critical` finding, and on each `warning`
   finding inside the namespace scope, passing that finding's `namespace` and
   `name` and its `kind` **lowercased** — a finding says `Pod`, the tool takes
   `pod`. Only `pod`, `deployment`, `statefulset`, `daemonset`, `replicaset`,
   `job` and `cronjob` are inspectable; a Service, Ingress, PVC,
   PodDisruptionBudget, HPA or ResourceQuota finding is not, so report it from
   its own `reason` and `detail`. `critical` and `warning` are the only two
   severities kubeagent emits.
5. Do not call `kubeagent_advisory` unless a finding points at a specific
   section.

Report, in this order:

- The verdict, in one sentence.
- Findings ranked by severity, each with what `kubeagent_inspect` added — the
  pod state and the events that explain it.
- **What was not checked.** Name the skipped checks that matter for the user's
  question, and every entry in `coverage.partial` as a blind spot rather than a
  clean result.

Do not remediate. If a fix is obvious, give the user the `kubeagent scan --fix`
command to run themselves and say that it confirms each action.
