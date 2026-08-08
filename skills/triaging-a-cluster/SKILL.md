---
name: triaging-a-cluster
description: Use when asked to diagnose a Kubernetes cluster, namespace, or workload with kubeagent - establishes the tool order, when to escalate, when to stop, and the rule that nothing here ever writes to the cluster.
---

# Triaging a cluster with kubeagent

kubeagent's MCP tools return findings computed by detectors, not generated text.
Your job is to call them in the right order, read what comes back honestly, and
stop when there is nothing left to learn.

## Step 0: preflight

The plugin cannot ship kubeagent's binary. Before the first call, check it is
installed:

```bash
command -v kubeagent
```

If that produces nothing, stop and tell the user to install it, offering all
three paths:

```bash
go install github.com/imantaba/kubeagent@latest
```

```bash
kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml
```

Or download a prebuilt binary from
<https://github.com/imantaba/kubeagent/releases>.

Do not attempt the diagnosis without it. The MCP tools will simply fail.

## Step 1: always start with kubeagent_triage

Call `kubeagent_triage` first, every time. Pass `namespace` if the user named
one; omit it for a whole-cluster sweep.

Never open with `kubeagent_inspect`. You do not yet know what is broken, so you
would be guessing at an object name, and a guess costs a call and returns
nothing.

If the user named a cluster rather than a namespace, call `list_contexts` first
to find its exact context name, then pass that as `context`.

## Step 2: read coverage before findings

Read the `coverage` block before you read a single finding. An empty `findings`
list means "no finding was produced", which is not the same as "the cluster is
healthy" — the checks that would have produced one may never have run.

The `reading-kubeagent-findings` skill covers how. Follow it. This is the single
most common way to report a cluster healthy when it is not.

## Step 3: escalate from the findings, not from intuition

Every finding carries a `severity` of `critical` (a detector matched a concrete
failure mode) or `warning` (a health check flagged something that needs a
human). Those are the only two values.

Call `kubeagent_inspect` on every `critical` finding, using the `namespace` and
`name` the finding already gave you. It returns that object's status, its pods,
kubeagent's findings for it, and its recent Kubernetes events.

**Lowercase the kind first.** A finding reports `"kind": "Pod"`; `kubeagent_inspect`
accepts `pod`. Passing the finding's spelling through unchanged is rejected before
the call reaches the cluster.

`kubeagent_inspect` takes seven kinds and no others: `pod`, `deployment`,
`statefulset`, `daemonset`, `replicaset`, `job`, `cronjob`. Most of kubeagent's
Service, Ingress, PVC, PodDisruptionBudget, HPA and quota findings are
`warning`s, and **none of those six kinds can be inspected** — the call fails
the schema. Report those findings from the `reason` and `detail` they already
carry, and inspect the workload behind them if the user's question is about one.
Do not skip them wholesale; that is how a real problem gets dismissed as noise.

Otherwise inspect a `warning` when it sits inside the scope the user asked about.

Do not inspect objects no finding pointed at. Do not invent names.

## Step 4: call advisory sections only when a finding points at one

`kubeagent_advisory` sections each cost extra API reads, so they are opt-in.
Call one when a finding implies it, not speculatively:

| Finding is about | Section |
|---|---|
| Certificate expiry or TLS | `certificates` |
| Pending pods, scheduling pressure, resource limits | `capacity` |
| Unhealthy operator custom resources | `operators` |
| Live state diverging from Git | `drift` |
| Privileged pods, host mounts, weak security context | `security` |

Requesting all five on a healthy cluster wastes reads and produces noise you
will then have to explain away.

## Step 5: stop

Stop when either is true:

- The verdict is `healthy` and `coverage.partial` is empty. Say so and stop.
  Continuing to dig produces noise, not confidence.
- You have inspected every `critical` finding, plus the `warning` findings in
  the user's scope, and reported them.

Then write the report: the verdict, the findings ranked by severity, and — always
— what was not checked, in the user's words rather than as a raw JSON dump.

## Never write to the cluster

kubeagent has a guard-railed `--fix` mode. It is not reachable from here, and
that is deliberate. No MCP tool can write, and this skill does not shell out to
the CLI to work around that.

If the user wants remediation, give them the command to run themselves:

```bash
kubeagent scan --namespace <ns> --fix
```

Tell them it will ask for confirmation per action and re-verify afterwards. Do
not run it for them. Do not run `kubectl delete`, `kubectl patch`, `kubectl
rollout restart`, or any other mutation as a substitute.
