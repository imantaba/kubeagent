# Policy packs

[Policy as code](policy.md) lets you write your own rules. A **policy pack**
is a rule set kubeagent ships pre-written, compiled into the binary, and
evaluated by the exact same engine — so you get a working set of checks with
no file of your own to author.

```bash
kubeagent policy packs                       # list what ships
kubeagent policy packs --print reliability   # print one, to read or fork
kubeagent scan --policy-pack reliability     # evaluate it against a cluster
```

```text
$ kubeagent policy packs
  reliability    14 rules — probes, resource requests and limits, replica counts, disruption budgets and image tags

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
kubeagent: unknown policy pack "nope" (want reliability)
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
schema version 1.2, `gate` at 1.1. Shipping the `reliability` pack inside the
binary changes nothing about what an existing command line does; the rules
only run once `--policy-pack` names them.

When it is used, a pack joins the same evaluation `--policy` already drives:
pack rules and file rules populate one rule list, and their violations render
in the same `POLICY` section and the same JSON `policy` key. There is no
separate, pack-shaped output to learn.

## No rule is critical

`gate` fails a build on a `critical` finding by default (`--fail-on
critical`). None of the fourteen `reliability` rules is `critical` — the
pack's own header comment says this is deliberate — so turning on
`--policy-pack reliability` in a pipeline that passed yesterday cannot make it
fail today. Raising `--fail-on warning` (or lower) is the explicit, separate
act that makes these rules block a build; enabling the pack does not do it for
you.

## The fourteen rules

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

## Two semantics a rule author must know

These follow directly from how the [general policy evaluator](policy.md)
works, and every one of the fourteen `reliability` rules is written with them
in mind.

**`[*]` produces one slot per element, and every slot must satisfy the
assertion.** `reliability.deploy-memory-limit` asserts
`spec.template.spec.containers[*].resources.limits.memory` exists. A
Deployment with several containers where only one lacks a memory limit still
violates the rule: one object yields at most one violation per rule, from the
first slot that fails, but it takes only one failing slot to produce it.
Setting the limit on every container but one is not "mostly compliant."

**`exists` violates on an absent field; every other operator skips it.**
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
rbac print` — already report. The kinds the fourteen `reliability` rules
select (`Deployment`, `StatefulSet`, `DaemonSet`, `CronJob`,
`PersistentVolumeClaim`) are all inside the policy engine's selectable kinds,
which are pinned to the same core rules `rbacprofile` already grants. Turning
on `--policy-pack reliability` asks the cluster for nothing a `scan` without
it did not already ask for. See [Least-privilege RBAC](rbac.md).

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
mine.yaml: rule id "reliability.deploy-readiness-probe" is already defined in pack:reliability
```

Change the ids in the fork — or drop `--policy-pack reliability` once you are
running the fork instead of the original — before combining the two.

## Not in this slice

Deliberately absent:

- **Security and cost packs.** `reliability` is the first pack; the registry
  has room for more, but this slice ships exactly one.
- **Operator-contributed packs at run time.** The registry is curated and
  compiled into the binary, the same as `known-issues`; there is no way to add
  a pack without a kubeagent release.
- **A pack on by default.** `--policy-pack` is opt-in on every command that
  accepts it; nothing runs unless it is named.
- **Any change to the evaluator.** `internal/policy` is unchanged — a pack is
  YAML data read by the same `Load`/`Evaluate` a `--policy` file already used.
