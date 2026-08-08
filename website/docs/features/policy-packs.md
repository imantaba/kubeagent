# Policy packs

[Policy as code](policy.md) lets you write your own rules. A **policy pack**
is a rule set kubeagent ships pre-written, compiled into the binary, and
evaluated by the exact same engine — so you get a working set of checks with
no file of your own to author.

```bash
kubeagent policy packs                       # list what ships
kubeagent policy packs --print reliability   # print one, to read or fork
kubeagent scan --policy-pack reliability     # evaluate it against a cluster
kubeagent scan --policy-pack security        # or another one, or all three

# Nothing in a pack is critical, so a pack cannot fail a gate by default.
# This is the explicit act that makes it block:
kubeagent gate --policy-pack security --fail-on warning
```

```text
$ kubeagent policy packs
  cost           16 rules — resource requests and limits, retention and history limits, autoscaler ceilings and claim sizes
  reliability    14 rules — probes, resource requests and limits, replica counts, disruption budgets and image tags
  security       23 rules — privileged containers, host namespaces and paths, root filesystems, capabilities and service account tokens

Print one to fork it:
  kubeagent policy packs --print <name>
```

`--print` writes the pack's YAML to stdout, unmodified:

```text
$ kubeagent policy packs --print reliability
# kubeagent reliability pack.
#
# Every rule here catches, before a workload goes live, a failure kubeagent's
# detectors diagnose after it does: a container with no readiness probe, no
# memory limit, a floating tag, a single replica with no disruption budget.
#
# No rule is `critical`, deliberately. `gate` fails on critical by default, so
# adding this pack to a pipeline must not fail a build that passed yesterday.
# An operator who wants these to block raises --fail-on warning.
#
# Rule ids are namespaced with the pack name so they cannot collide with an
# operator's own rules when both are given.

- id: reliability.deploy-readiness-probe
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].readinessProbe
    op: exists
  level: warning
  message: a container has no readiness probe, so its Service sends traffic before it can serve

# … the remaining thirteen rules follow the same shape. The complete list is
# in the table below, or run the command yourself for the full YAML.
```

`--policy-pack <name>` on `scan` or `gate` evaluates a pack against the
current cluster. It is repeatable, and combinable with `--policy`.

An unknown name is refused, naming what does exist:

```text
$ kubeagent policy packs --print nope
kubeagent: unknown policy pack "nope" (want cost, reliability, security)
```

## Guarantees

`policy packs` itself — the list and `--print` — contacts nothing at all: no
cluster, no kubeconfig, no network. It reads only what is compiled into the
binary.

Evaluating a pack (`--policy-pack` on `scan` or `gate`) is **read-only toward
the cluster**: a rule can only read a field, and there is no `--fix` path from
a rule, exactly as for a `--policy` file. Separately, a pack **makes no LLM
call**. Those are two separate promises — read-only describes what evaluation
does to the cluster, no-model-call describes what it does with the result —
and neither implies the other. `--explain` is the model path; a pack is not a
smaller version of it.

## Opt-in, and what that buys

`--policy-pack` is opt-in, exactly like `--policy`: leave it off, and `scan`
renders exactly the bytes it rendered before this slice shipped. No `policy`
key appears in `--output json`, and no `schemaVersion` moves — `scan` stays at
schema version 1.2, `gate` at 1.1. Shipping a pack inside the
binary changes nothing about what an existing command line does; the rules
only run once `--policy-pack` names them.

When it is used, a pack joins the same evaluation `--policy` already drives:
pack rules and file rules populate one rule list, and their violations render
in the same `POLICY` section and the same JSON `policy` key. There is no
separate, pack-shaped output to learn.

## No rule is critical

`gate` fails a build on a `critical` finding by default (`--fail-on
critical`). **No rule in any shipped pack is `critical`** — each pack's own
header comment says this is deliberate — so turning on `--policy-pack` in a
pipeline that passed yesterday cannot make it fail today. A test over the
whole registry keeps it that way, so it is a property of the pack format
rather than of the packs that happen to ship.

Read that as "opt-in to blocking", not as "not meant to block." Raising
`--fail-on` is the explicit, separate act:

```bash
kubeagent gate --policy-pack security --fail-on warning
```

Every explicitly-bad-value rule in the `security` pack is a `warning`, so that
one flag makes the pack block a build. Every "field is unset" rule is `info`
and stays advisory even then.

## The reliability pack — fourteen rules

Paths are shortened below; every `containers[*]` is
`spec.template.spec.containers[*]`.

| id | kind | assertion | level |
| --- | --- | --- | --- |
| `reliability.deploy-readiness-probe` | Deployment | `containers[*].readinessProbe` exists | warning |
| `reliability.deploy-liveness-probe` | Deployment | `containers[*].livenessProbe` exists | info |
| `reliability.statefulset-readiness-probe` | StatefulSet | `containers[*].readinessProbe` exists | warning |
| `reliability.daemonset-readiness-probe` | DaemonSet | `containers[*].readinessProbe` exists | info |
| `reliability.deploy-memory-limit` | Deployment | `containers[*].resources.limits.memory` exists | warning |
| `reliability.statefulset-memory-limit` | StatefulSet | `containers[*].resources.limits.memory` exists | warning |
| `reliability.deploy-cpu-request` | Deployment | `containers[*].resources.requests.cpu` exists | info |
| `reliability.deploy-memory-request` | Deployment | `containers[*].resources.requests.memory` exists | info |
| `reliability.deploy-image-not-latest` | Deployment | `containers[*].image` notMatches `*:latest` | warning |
| `reliability.deploy-image-tagged` | Deployment | `containers[*].image` matches `*:*` | info |
| `reliability.deploy-replicas-min-two` | Deployment | `spec.replicas` gte `2` | warning |
| `reliability.deploy-pdb` | Deployment | relation `hasPodDisruptionBudget` | warning |
| `reliability.cronjob-concurrency-policy` | CronJob | `spec.concurrencyPolicy` in `Forbid`, `Replace` | info |
| `reliability.pvc-storage-class` | PersistentVolumeClaim | `spec.storageClassName` exists | info |

## The security pack — twenty-three rules

Paths are shortened below: `T` is `spec.template.spec`. The one `CronJob` rule
spells its path in full, because its pod template lives one level deeper.

| id | kind | assertion | level |
| --- | --- | --- | --- |
| `security.deploy-privileged` | Deployment | `T.containers[*].securityContext.privileged` notIn `true` | warning |
| `security.deploy-privilege-escalation-unset` | Deployment | `T.containers[*].securityContext.allowPrivilegeEscalation` exists | info |
| `security.deploy-privilege-escalation` | Deployment | `T.containers[*].securityContext.allowPrivilegeEscalation` notIn `true` | warning |
| `security.deploy-run-as-non-root-unset` | Deployment | `T.containers[*].securityContext.runAsNonRoot` exists | info |
| `security.deploy-run-as-non-root` | Deployment | `T.containers[*].securityContext.runAsNonRoot` notIn `false` | warning |
| `security.deploy-run-as-root-uid` | Deployment | `T.containers[*].securityContext.runAsUser` gt `0` | warning |
| `security.deploy-read-only-root-unset` | Deployment | `T.containers[*].securityContext.readOnlyRootFilesystem` exists | info |
| `security.deploy-read-only-root` | Deployment | `T.containers[*].securityContext.readOnlyRootFilesystem` notIn `false` | warning |
| `security.deploy-added-capabilities` | Deployment | `T.containers[*].securityContext.capabilities.add[*]` notIn seven host-level capabilities | warning |
| `security.deploy-host-path-volume` | Deployment | `T.volumes[*].hostPath` notExists | warning |
| `security.deploy-host-port` | Deployment | `T.containers[*].ports[*].hostPort` notExists | warning |
| `security.deploy-host-network` | Deployment | `T.hostNetwork` notIn `true` | warning |
| `security.deploy-host-pid` | Deployment | `T.hostPID` notIn `true` | warning |
| `security.deploy-host-ipc` | Deployment | `T.hostIPC` notIn `true` | warning |
| `security.deploy-seccomp-unset` | Deployment | `T.securityContext.seccompProfile.type` exists | info |
| `security.deploy-seccomp-unconfined` | Deployment | `T.securityContext.seccompProfile.type` notIn `Unconfined` | warning |
| `security.deploy-service-account-unset` | Deployment | `T.serviceAccountName` exists | info |
| `security.deploy-automount-token-unset` | Deployment | `T.automountServiceAccountToken` exists | info |
| `security.statefulset-privileged` | StatefulSet | `T.containers[*].securityContext.privileged` notIn `true` | warning |
| `security.statefulset-host-path-volume` | StatefulSet | `T.volumes[*].hostPath` notExists | warning |
| `security.daemonset-privileged` | DaemonSet | `T.containers[*].securityContext.privileged` notIn `true` | warning |
| `security.daemonset-host-path-volume` | DaemonSet | `T.volumes[*].hostPath` notExists | warning |
| `security.cronjob-privileged` | CronJob | `spec.jobTemplate.spec.template.spec.containers[*].securityContext.privileged` notIn `true` | warning |

### Why four properties get two rules each

`exists` catches a field nobody set. A value operator catches a field someone
set to the wrong thing. Neither catches both, because **every operator except
`exists` and `notExists` skips an absent field** — so a single `notIn` rule is
silent about a workload that never set the field at all.

The pack pairs them only where **an absent field is itself unsafe**:

| property | absent means | rules |
| --- | --- | --- |
| `allowPrivilegeEscalation` | the Kubernetes default is `true` — unsafe | paired |
| `runAsNonRoot` | nothing stops the container running as root — unsafe | paired |
| `readOnlyRootFilesystem` | a writable root filesystem — unsafe | paired |
| `seccompProfile.type` | unconfined — unsafe | paired |
| `privileged`, `hostNetwork`, `hostPID`, `hostIPC`, `capabilities.add` | the safe value — safe | one value rule |
| `hostPath`, `hostPort` | the safe case | one `notExists` rule |

Where absence is the safe default, a second rule would report a compliant
workload, so only the value rule ships. The `-unset` half of each pair is
`info` and the value half is `warning`, which is why raising `--fail-on
warning` blocks on the explicit misconfiguration without also blocking on
every unset optional field.

### What the security pack cannot say

Five real gaps. They are written down rather than worked around, because each
comes from a property the rule grammar does not have — and adding one would be
an engine change, not a pack change.

1. **A bare `Pod` that no controller owns is not checked.** Every rule selects
   a workload kind. `kubeagent scan`'s own detectors still see that pod; the
   pack does not. A controller-owned pod would otherwise repeat its workload's
   violation once per replica.
2. **Hardening set at one level does not satisfy a rule written for the
   other.** The grammar has no OR, so each path names exactly one level and
   cannot also accept the other. Both directions are real. A Deployment that
   sets `runAsNonRoot` once in `spec.template.spec.securityContext` still
   reports `security.deploy-run-as-non-root-unset` for each container. And a
   Deployment whose containers each set their own `seccompProfile` still
   reports `security.deploy-seccomp-unset`, because that pair reads the
   pod-level field — Kubernetes accepts a seccomp profile at either level, so
   that workload is hardened and the rule still fires. This is the pack's most
   likely source of false positives. If your workloads harden at the other
   level, fork the pack and move those paths.
3. **`capabilities.drop` cannot be required to include `ALL`.** That needs an
   existential quantifier — "some element equals ALL" — and `[*]` is
   universally quantified with no existential counterpart. The pack checks
   what was *added* instead, against a fixed list of seven host-level
   capabilities.
4. **RBAC bindings, service account objects and Secrets are unreachable.**
   None is a kind a policy may select. A workload's *reference* to a service
   account is reachable, and `security.deploy-service-account-unset` is that
   rule; the object it names is not.
   `security.deploy-automount-token-unset` is bounded the same way: it reads
   the workload's own field, and cannot see that the service account behind it
   may already have opted out.
   `Secret` is absent deliberately — a violation carries evidence, and
   evidence drawn from a Secret would be secret material rendered into a
   report, a JSON document and a SARIF upload.
5. **The added-capability list is curated, not exhaustive.** A capability
   outside the seven passes. Fork the pack to extend it.

A registry allowlist is also not a rule kubeagent can curate: it does not know
which registry is yours, and a shipped rule naming one would be wrong for
everyone else. `--print` and forking are the answer.

## The cost pack — sixteen rules

The `cost` pack is about a workload's claim on the cluster — what it
reserves, what it may grow to, and what it leaves behind.

Paths are shortened below: `T` is `spec.template.spec`. The two `CronJob`
rules that read a container spell their path in full, because the pod
template lives one level deeper; the three `CronJob` rules that read the
CronJob's own fields need no shortening.

| id | kind | assertion | level |
| --- | --- | --- | --- |
| `cost.deploy-ephemeral-storage-limit` | Deployment | `T.containers[*].resources.limits.ephemeral-storage` exists | info |
| `cost.deploy-large-cpu-request` | Deployment | `T.containers[*].resources.requests.cpu` lte `8` | info |
| `cost.deploy-large-memory-request` | Deployment | `T.containers[*].resources.requests.memory` lte `32Gi` | info |
| `cost.statefulset-cpu-request` | StatefulSet | `T.containers[*].resources.requests.cpu` exists | info |
| `cost.statefulset-memory-request` | StatefulSet | `T.containers[*].resources.requests.memory` exists | info |
| `cost.daemonset-cpu-request` | DaemonSet | `T.containers[*].resources.requests.cpu` exists | info |
| `cost.daemonset-memory-request` | DaemonSet | `T.containers[*].resources.requests.memory` exists | info |
| `cost.daemonset-ephemeral-storage-limit` | DaemonSet | `T.containers[*].resources.limits.ephemeral-storage` exists | info |
| `cost.cronjob-cpu-request` | CronJob | `spec.jobTemplate.spec.template.spec.containers[*].resources.requests.cpu` exists | info |
| `cost.cronjob-memory-request` | CronJob | `spec.jobTemplate.spec.template.spec.containers[*].resources.requests.memory` exists | info |
| `cost.cronjob-successful-history` | CronJob | `spec.successfulJobsHistoryLimit` lte `10` | info |
| `cost.cronjob-failed-history` | CronJob | `spec.failedJobsHistoryLimit` lte `10` | info |
| `cost.cronjob-active-deadline` | CronJob | `spec.jobTemplate.spec.activeDeadlineSeconds` exists | info |
| `cost.job-backoff-limit` | Job | `spec.backoffLimit` lte `10` | info |
| `cost.hpa-max-replicas` | HorizontalPodAutoscaler | `spec.maxReplicas` lte `50` | info |
| `cost.pvc-large-storage` | PersistentVolumeClaim | `spec.resources.requests.storage` lte `1Ti` | info |

### Why every cost rule is `info`

`reliability` and `security` each carry `warning` rules; `cost` does not —
all sixteen are `info`. The difference is not caution, it is scope:
`security`'s "privileged is bad" holds in every cluster, but `cost`'s "eight
CPUs is too many" does not — a request that is generous in one cluster is
unremarkable in another, so a cost finding is budget-dependent in a way a
security finding is not. A pack that cannot know a cluster's budget must not
accuse it of overspending, only ask for confirmation. The consequence is
mechanical: `cost` cannot fail a gate at any `--fail-on` above `info` — not
even `--fail-on warning`, which is enough to make `security`'s value rules
block.

### What the cost pack cannot say

Three real gaps. They are written down rather than worked around, for the
same reason the security pack's five are: each comes from a property the rule
grammar does not have, and adding one would be an engine change, not a pack
change.

1. **A limit set with no matching request is unsaid.** Kubernetes defaults an
   unset request to the limit, so a container that sets a memory limit and no
   memory request reserves the ceiling rather than the expected use — probably
   the largest single cost defect in a typical cluster. The grammar has no
   cross-field relation, so no rule here can compare a container's own request
   to its own limit.
2. **A Deployment with no CPU or memory request is unsaid when `cost` runs
   alone.** That `exists` question is already `reliability.deploy-cpu-request`
   and `reliability.deploy-memory-request`; `cost` asks it only for
   StatefulSet, DaemonSet and CronJob, the three kinds `reliability` does not
   cover it for, so `reliability` and `cost` never report the same gap under
   two ids. Run both and the question is covered.
3. **Absence across a whole namespace, and a comparison between two fields on
   the same object, are both unexpressed.** A rule asserts over objects that
   exist, so "this namespace has no `ResourceQuota`" cannot be a rule — there
   is no object to fail against. The same grammar has no way to compare one
   field to another on the same object, so "`minReplicas` equals
   `maxReplicas`" cannot be a rule either.

### kubeagent has no prices

kubeagent has no prices. There is no billing data, no instance types, no node
cost and no cloud API anywhere in the binary. The pack names shapes that
usually cost money — an oversized request, an unbounded autoscaler ceiling, a
retention window nobody trimmed — and claims nothing beyond that: it cannot
tell you what anything costs, in any currency, and no rule here says it can.
This is a third, separate claim from the two in **Guarantees**, above: it
says nothing about whether a rule can write to the cluster or whether
evaluating a pack calls a model.

## Two semantics a rule author must know

These follow directly from how the [general policy evaluator](policy.md)
works, and every rule in every shipped pack is written with them
in mind.

**`[*]` produces one slot per element, and every slot must satisfy the
assertion.** `reliability.deploy-memory-limit` asserts
`spec.template.spec.containers[*].resources.limits.memory` exists. A
Deployment with several containers where only one lacks a memory limit still
violates the rule: one object yields at most one violation per rule, from the
first slot that fails, but it takes only one failing slot to produce it.
Setting the limit on every container but one is not "mostly compliant."

**`exists` violates on an absent field; `notExists` is satisfied by one; every
other operator skips it.**
`reliability.deploy-image-tagged` asserts `containers[*].image` **matches**
`*:*` — looking for a colon, which is where a tag or digest would be. It
catches a bare `image: nginx`. It does **not** catch
`image: registry.example.com:5000/app` — a private registry host with a port
and no tag at all — because the host:port colon alone satisfies the glob. That
is a documented, accepted limitation of a glob-based check: it recognizes the
shape of a tag, not the field, since the API does not expose "image has a tag"
as a boolean of its own. Pair it with `reliability.deploy-image-not-latest`
(`notMatches` `*:latest`) for the case it does catch, and do not rely on
either rule alone to prove every image is pinned.

## RBAC

A pack needs no grant beyond what a plain `kubeagent scan` — and `kubeagent
rbac print` — already report. The kinds the shipped rules
select (`Deployment`, `StatefulSet`, `DaemonSet`, `CronJob`, `Job`,
`HorizontalPodAutoscaler`, `PersistentVolumeClaim`) are all inside the policy
engine's selectable kinds, which are pinned to the same core rules
`rbacprofile` already grants. Turning on `--policy-pack` asks for no
permission a plain `scan` did not already have.

It does add request volume, though: evaluating any policy — a pack included —
builds its own dynamic client and lists every kind the loaded rules touch,
independently of whatever `scan`'s typed collectors already read. For
`reliability` that is six `List` calls, one each for the five kinds its rules
select plus `PodDisruptionBudget` for the one relation rule. For `security` it
is four, one per workload kind, since it has no relation rule. For `cost` it
is seven, one per kind its rules select, since it has no relation rule
either. That extra, uncached read is how `--policy` has always evaluated a
rule set; a pack does not change it. See [Least-privilege RBAC](rbac.md).

## Forking a pack

Print a pack to a file, edit it, and run it as an ordinary `--policy` file:

```bash
kubeagent policy packs --print reliability > mine.yaml
# edit mine.yaml — remove rules, change a level, tighten a value
kubeagent scan --policy mine.yaml
```

`--policy-pack` and `--policy` may both be given, and pack rules load first —
so a duplicate id is reported against the pack as the earlier definition,
reading as "your file reuses a pack's id" rather than the reverse. A freshly
forked file keeps every id from the original pack until you change them, so
running the fork **alongside** the pack it came from collides:

```text
$ kubeagent scan --policy-pack reliability --policy mine.yaml
kubeagent: mine.yaml: rule id "reliability.deploy-readiness-probe" is already defined in pack:reliability
```

Change the ids in the fork — or drop `--policy-pack reliability` once you are
running the fork instead of the original — before combining the two.

## Contributing a pack

A pack is a subject, not a patch. **Open an issue first** and agree the subject
belongs in kubeagent before writing any YAML — review will ask why *this* set
of rules, and that is easier to answer before the rules exist.

Then:

```bash
# 1. write the pack
$EDITOR internal/policypack/packs/<name>.yaml

# 2. add its entry to the registry slice in policypack.go, keeping it sorted
$EDITOR internal/policypack/policypack.go

# 3. the gate
go test ./internal/policypack
```

Open a pull request with a `CHANGELOG.md` entry under `## [Unreleased]`, the
same as any other change.
[CONTRIBUTING.md](https://github.com/imantaba/kubeagent/blob/main/CONTRIBUTING.md)
carries the sign-off and commit-message conventions.

### What the tests check

Seven assertions run over every registered pack, so every failure is
predictable before you push:

| check | refuses |
|-------|---------|
| the pack loads | anything the policy loader rejects — an unknown key, a malformed rule id, a kind that is not selectable, an unknown level, an empty message |
| ids carry the pack prefix | a rule id not beginning `<pack>.`, which is what keeps `--policy-pack` and `--policy` from colliding when both are given |
| no rule is critical | a `critical` rule, which would fail a gate at its default `--fail-on critical` the day the pack was added |
| no host or address | `://` or a bare IPv4 address anywhere in the YAML, and **any** dot in a rule message |
| every embedded file is registered | a `packs/*.yaml` with no registry entry — it would ship inside the binary while being invisible to the listing, to `--policy-pack` and to every other test |
| names are unique and usable | a duplicate name, anything outside lowercase letters, digits and interior hyphens, or a name too long for the listing column |
| summary shape | a multi-line summary, a trailing period, or a leading capital |

The last three are about the registry rather than the rules, and they exist
because nothing else can see it: the loader is handed bytes and never learns
where they came from, and every other test iterates the registered packs — so
anything missing from the registry is invisible to all of them.

### What no test can check

These are the review, and they are why acceptance is not automatic:

- **Is every rule true of the kind it selects?** A path that does not exist on
  that kind makes every operator except `exists` and `notExists` skip the slot
  — the rule runs, reports nothing, and looks like a pass. Check each path
  against the API type, not against memory.
- **Does the subject belong?** `reliability`, `security` and `cost` are three
  questions an operator already asks of a workload. A pack encoding one
  organisation's house style is a fork, not a pack — see
  [Forking a pack](#forking-a-pack).
- **Is every message a single clause with no dot?** The dot ban is mechanical;
  a message that still reads well under it is not.
- **Is every level right?** Nothing is `critical`. Beyond that, a rule firing
  on an explicitly wrong value is usually `warning`, and one firing on an unset
  field is usually `info` — each shipped pack explains its own choice in its
  header comment.
- **Does the pack say what it cannot say?** All three shipped packs carry a
  section naming their own gaps. Claiming only what you deliver is the house
  style here, not a nicety.

### Acceptance is curatorial

Passing the tests is **necessary, not sufficient**. A maintainer still reads
every rule, and a pack that ships is kubeagent's curation whoever wrote it —
kubeagent's name is on every rule an operator runs by name. If kubeagent would
not vouch for a pack, kubeagent does not merge it.

Attribution goes in the pack's own header comment, which
`kubeagent policy packs --print <name>` emits verbatim. There is no author
field in the listing, and a contributed pack is not marked as one: a two-tier
listing would tell an operator to trust some shipped rules less than others,
which is the opposite of what accepting a pack means.

### Two limits worth knowing before you start

**A contributed pack ships on a kubeagent release.** The registry is compiled
into the binary, the same as `known-issues`; there is no way to add a pack to
an installed kubeagent. If you need rules today rather than next release, fork
one instead — [Forking a pack](#forking-a-pack) needs no release and no pull
request.

**Nobody has walked this path yet.** `reliability`, `security` and `cost` are
all kubeagent's own curation; a pack authored outside the project does not
exist yet. The route above is written and enforced, but it has not been used.

## Not in this slice

Deliberately absent:

- **Operator-contributed packs at run time.** The registry is curated and
  compiled into the binary, the same as `known-issues`; there is no way to add
  a pack without a kubeagent release.
- **A pack on by default.** `--policy-pack` is opt-in on every command that
  accepts it; nothing runs unless it is named.
- **Any change to the evaluator.** `internal/policy` is unchanged — a pack is
  YAML data read by the same `Load`/`Evaluate` a `--policy` file already used.
