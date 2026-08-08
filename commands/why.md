---
description: Root-cause one Kubernetes object with kubeagent - its findings, its events, and the one advisory section its failure implies.
argument-hint: "<kind>/<name> [-n namespace]"
---

Explain why this object is unhealthy: $ARGUMENTS

Follow the `triaging-a-cluster` skill and the `reading-kubeagent-findings`
skill.

1. Preflight: follow the `triaging-a-cluster` skill's Step 0 — confirm
   `kubeagent_inspect` is in your tool list, and shell out only if it is not.
2. Parse the argument into a kind and a name. Valid kinds are `pod`,
   `deployment`, `statefulset`, `daemonset`, `replicaset`, `job`, and
   `cronjob`. If the namespace was not given with `-n`, ask for it rather than
   guessing — `kubeagent_inspect` requires it.
3. Call `kubeagent_inspect` with that kind, namespace, and name.
4. Read its findings and its recent Kubernetes events together. The events
   usually carry the sentence the finding summarises.
5. Call `kubeagent_advisory` for **at most one** section, and only if a finding
   points at it: `certificates` for TLS expiry, `capacity` for scheduling or
   resource pressure, `operators` for an unhealthy custom resource, `drift` for
   divergence from Git, `security` for a privileged or host-mounted pod.

Report:

- What is wrong, quoting the finding's `reason` and `detail` rather than
  strengthening them.
- The evidence: the events and container state that support it.
- kubeagent's `remediationHint` if there is one, labelled as kubeagent's. Any
  further suggestion of your own, labelled as yours.
- Anything `coverage` says was not checked that bears on this object.

Do not remediate.
