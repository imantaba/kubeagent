# Policy as code

`kubeagent scan --policy` and `kubeagent gate --policy` evaluate
organization-specific checks written in a YAML file, alongside the built-in
detectors. A rule says *which objects to look at* and *one thing that must be
true of them*.

Policy evaluation is **read-only**. A rule can never write to the cluster;
there is no `--fix` path from a policy, and `--policy` grants kubeagent no
access it does not already have — the kinds a rule may select are exactly the
kinds a plain `kubeagent scan` already reads.

Writing rules from scratch is not the only way in: kubeagent also ships
curated **policy packs** — rule sets compiled into the binary, evaluated by
this same engine, with no file of your own to write. See [Policy
packs](policy-packs.md).

## The one thing to know first

**An operator other than `exists` skips a field nobody set.**

A rule like "the memory limit must be under 4Gi" does **not** catch a container
that sets no memory limit at all. `lte` has nothing to compare, so that
container is skipped — kubeagent will not turn an unset field into an
accusation.

Writing "every container must set a memory limit, and it must be under 4Gi" is
therefore genuinely **two rules**:

```yaml
- id: memory-limit-set
  level: critical
  message: container sets no memory limit
  match:
    kind: Pod
  assert:
    path: spec.containers[*].resources.limits.memory
    op: exists

- id: memory-limit-bounded
  level: warning
  message: container memory limit is above 4Gi
  match:
    kind: Pod
  assert:
    path: spec.containers[*].resources.limits.memory
    op: lte
    values: ["4Gi"]
```

This is the single thing most likely to trip a rule author. If a rule reports
nothing and you expected it to fire, check whether the field is set at all.

## What a rule looks like

```yaml
- id: registry-allowlist
  level: critical
  message: image is not from an allowed registry
  match:
    kind: Pod
    namespaceLabels:
      environment: production
  assert:
    path: spec.containers[*].image
    op: matches
    values:
      - "registry.example.com/*"
      - "registry.example.org/library/*"
```

| field | meaning |
| --- | --- |
| `id` | the rule's name; appears as `policy/<id>` in `gate` and SARIF |
| `level` | `critical`, `warning` or `info` |
| `message` | what a violation says; kubeagent never invents wording |
| `match.kind` | one kind, written bare (`Deployment`, not `apps/v1 Deployment`) |
| `match.namespaces` | exact namespace names |
| `match.labels` | labels on the object; every pair must match |
| `match.namespaceLabels` | labels on the object's **namespace** |
| `assert.path` | the field to look at |
| `assert.op` | one of the ten operators |
| `assert.values` | what to compare against |
| `assert.relation` | used instead of `path`/`op` for the two relations |

## Paths

A path is written exactly as the field appears in `kubectl get -o yaml`:

```text
metadata.name
spec.replicas
spec.containers[*].image
spec.template.spec.containers[*].resources.limits.cpu
metadata.labels["app.kubernetes.io/name"]
```

`[*]` iterates a list. The bracket-quoted form addresses a map key verbatim,
which is how you reach a label or annotation key containing dots and slashes.

**`[*]` produces one slot per element, even for elements that lack the rest of
the path.** On a Pod with three containers where only one sets a CPU limit,
`spec.containers[*].resources.limits.cpu` resolves to three slots — one value
and two absent — so an `exists` rule reports that Pod. It does not pass because
"at least one was found". A path that never reaches a list at all — the field
is simply unset — resolves the same way, to a single absent slot; a path whose
list exists but has no elements resolves to zero slots. `exists` violates on
either: something was required and there is nowhere it could be. Every other
operator, `notExists` included, treats both the same as an ordinary absent
field — see the operator table below.

Every slot must satisfy the assertion, and one object produces at most one
violation per rule: the first slot that fails.

## Operators

| operator | true when | absent field |
| --- | --- | --- |
| `exists` | the field is set to something non-null | **violation** |
| `notExists` | the field is unset or null | satisfied |
| `in` | the value is one of `values` | skipped |
| `notIn` | the value is none of `values` | skipped |
| `matches` | the value matches one of the glob `values` | skipped |
| `notMatches` | the value matches none of them | skipped |
| `gt` `gte` `lt` `lte` | numeric or quantity comparison against `values[0]` | skipped |

Globs use `*` for any run of bytes, including `/`, and `?` for exactly one
byte — so `?` against a multi-byte UTF-8 character consumes only its first
byte, not the whole character. `registry.example.com/*` matches
`registry.example.com/team/app:1.0`.

Comparisons understand plain numbers and Kubernetes quantities, so `500m`,
`2Gi` and `1.5` all work. A value that parses as neither is skipped rather
than guessed at.

## Relations

Two assertions compare an object against other objects instead of against a
field of its own:

```yaml
- id: pdb-required
  level: warning
  message: no PodDisruptionBudget covers this Deployment
  match:
    kind: Deployment
    namespaceLabels:
      environment: production
  assert:
    relation: hasPodDisruptionBudget
```

`hasPodDisruptionBudget` applies to `Deployment`, `StatefulSet`, `ReplicaSet`
and `DaemonSet`. `hasHorizontalPodAutoscaler` applies to `Deployment`,
`StatefulSet` and `ReplicaSet` only — a DaemonSet runs one pod per node and
cannot scale horizontally, so asserting the HPA relation against one is
rejected at load time rather than silently never firing.

## Running it

```bash
# one file, a directory of files, or both — the flag is repeatable
kubeagent scan --policy policies/production.yaml
kubeagent scan --policy policies/
kubeagent gate --policy policies/ --fail-on warning

# check a file before a cluster is involved: no kubeconfig, no cluster call
kubeagent policy validate policies/production.yaml
```

A directory contributes its `.yaml` and `.yml` files in name order, and is not
searched recursively. A file named on the command line is read whatever it is
called.

In `scan`, violations appear in their own `POLICY` section. **A violation never
changes the cluster verdict**: the verdict is kubeagent's judgement about
cluster health, and a rule about required labels is not cluster health.

In `gate`, violations are ordinary findings at their declared level and cross
the existing `--fail-on` threshold, so CI enforcement needs no extra flag. In
SARIF, the rule id is `policy/<id>`.

## A rule that could not be evaluated is not a pass

If kubeagent cannot read a kind a rule selects — an RBAC denial, a resource the
cluster does not serve — the rule is reported as **not evaluated**, never as
satisfied. In `gate`, an unevaluated rule at or above `--fail-on` **fails the
build: exit `1`**, the same code an ordinary failing finding gets. That stays
true even when the read failure behind it also shows up as a blind spot in
`--output json`'s `inconclusive` list — a policy grants no new RBAC, so the
kind a rule could not read is often a kind the scan itself could not read
either, and `--allow-partial-read` does not change the exit code here: waiving
the read failure cannot turn a rule that never ran into anything less than a
failure. See [Why exit 2 exists, and is not
opt-in](ci-gate.md#why-exit-2-exists-and-is-not-opt-in). The same applies when
the supporting list a relation compares against cannot be read: without the
PodDisruptionBudget list, `hasPodDisruptionBudget` would report every workload
as uncovered, which is a fabricated violation rather than a silent pass, and
equally wrong.

## What a rule may not do

- **Secrets are not selectable.** No rule can name `Secret` as its kind.
- **A ConfigMap's contents are not readable.** A path beginning `data` or
  `binaryData` on a `ConfigMap` is a load error, in every spelling — a
  violation would carry the value as evidence into a report designed to be
  forwarded.
- **A policy cannot write.** There is no remediation path from a rule.

## Selectable kinds

`ConfigMap`, `CronJob`, `DaemonSet`, `Deployment`, `EndpointSlice`,
`HorizontalPodAutoscaler`, `Ingress`, `IngressClass`, `Job`,
`MutatingWebhookConfiguration`, `Namespace`, `NetworkPolicy`, `Node`,
`PersistentVolume`, `PersistentVolumeClaim`, `Pod`, `PodDisruptionBudget`,
`ReplicaSet`, `ResourceQuota`, `Service`, `StatefulSet`, `StorageClass`,
`ValidatingWebhookConfiguration`.

These are exactly the kinds kubeagent already reads, which is why `--policy`
needs no RBAC beyond `deploy/rbac.yaml`. See
[Least-privilege RBAC](rbac.md).
