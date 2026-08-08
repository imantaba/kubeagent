# Curated policy packs, slice 2: the `security` pack

**Date:** 2026-08-07
**Status:** approved
**Roadmap item:** post-1.0, curated policy packs — second half
**Predecessor:** slice 1 shipped in v1.9.0 (`internal/policypack`, the
`reliability` pack, `--policy-pack`, `kubeagent policy packs`)

## 1. What this slice ships

A second curated rule pack, `security`, compiled into the binary beside
`reliability` and evaluated by the same `--policy` engine:

```bash
kubeagent policy packs                    # now lists two
kubeagent policy packs --print security   # print it, to read or fork
kubeagent scan --policy-pack security     # evaluate it against a cluster
kubeagent gate --policy-pack security --fail-on warning   # make it block
```

Twenty-three rules over workload pod templates, covering the host-escape and
container-hardening surface the policy grammar can actually reach.

Nothing else moves. `internal/policy` is untouched — a pack is YAML data read
by the `Load`/`Evaluate` a `--policy` file already used. `internal/policypack`
stays stdlib-only. No new dependency, no schema version change, no RBAC
manifest change.

## 2. Why security is the first of the three remaining items

CLAUDE.md names the remaining work for this roadmap item as "security and cost
packs, and a pack contributed by someone other than kubeagent itself." That is
three slices, not one. Security goes first for three reasons:

1. It is the pack operators ask for.
2. It carries the only genuine design tension of the three — whether a curated
   pack may ship a `critical` rule — and resolving that decides the contract
   the cost pack inherits.
3. It is the pack the grammar constrains hardest. Working out which claims are
   expressible, which need two rules, and which are out of reach establishes
   the shape the cost pack then reuses.

Slice B (the `cost` pack) and slice C (the contribution path) are out of scope
here and are sketched in §13 only so the boundary is clear.

## 3. The grammar, and what it forbids

Verified by reading `internal/policy`, not assumed. Everything in this section
is a hard constraint on what the pack can say.

**Selectable kinds — 23, pinned to `rbacprofile.coreRules`** by
`TestSelectableKindsMatchesRBACProfileCore`. Present and useful here:
`Deployment`, `StatefulSet`, `DaemonSet`, `Job`, `CronJob`, `Pod`, `Ingress`,
`NetworkPolicy`, the two webhook-configuration kinds, `PersistentVolume`,
`StorageClass`.

**Absent, and the absence is the point:**

- `Role`, `RoleBinding`, `ClusterRole`, `ClusterRoleBinding` — a pack cannot
  check who may do what. RBAC-as-policy is not reachable, and adding it would
  widen the selectable set, which moves an RBAC manifest and is a far larger
  slice than this one.
- `ServiceAccount` — the object itself is unreachable. A workload's *reference*
  to one (`spec.template.spec.serviceAccountName`) is reachable, and that is
  what rule 17 uses.
- `Secret` — deliberately not selectable. A violation carries evidence, and
  evidence drawn from a Secret would be secret material rendered into a
  terminal, a JSON document, a SARIF upload and an HTML report. A security pack
  is exactly the thing that would reach for this; it may not.
- `ConfigMap` is selectable, but a path beginning `data` or `binaryData` is a
  **load error** (`readsConfigMapContents`), for the same evidence reason — a
  ConfigMap routinely holds a token nobody remembered was not a Secret. No rule
  in this pack selects `ConfigMap`.
- `LimitRange` — not granted by core, so not selectable. Relevant to slice B,
  not here.

**Ten operators:** `exists`, `notExists`, `in`, `notIn`, `matches`,
`notMatches`, `gt`, `gte`, `lt`, `lte`. Numeric comparison is real and goes
through `resource.ParseQuantity`, so `500m` vs `1` and `8Gi` vs `4Gi` compare
correctly. This slice uses only one comparison (`gt` on `runAsUser`); it
matters much more to slice B.

**Two relations only:** `hasPodDisruptionBudget`, `hasHorizontalPodAutoscaler`.
There is no "this namespace has a NetworkPolicy" relation, so the single most
requested cluster-level security claim is not expressible. It is not being
added: a new relation is engine work, and this slice changes no engine.

**Four semantics that shape every rule below:**

1. **Every operator except `exists`/`notExists` skips an absent slot.** A
   policy must never turn a field it cannot read into an accusation. So `in`,
   `notIn` and the comparisons say nothing at all about a field that is not
   set. Most security fields are optional pointers. This is the single most
   shaping fact in the slice, and §4 is the response to it.
2. **`[*]` is universally quantified.** A path resolves to one slot per list
   element, and every slot must satisfy the assertion. One failing container
   violates. There is no existential quantifier, which is why "`capabilities.drop`
   must include `ALL`" is **not expressible** and is not attempted.
3. **No OR, and no cross-field comparison.** `runAsNonRoot` set at pod level
   cannot satisfy a container-level rule, and "limits without requests" cannot
   be written at all.
4. **A rule message may contain no dot and no `://`.** Enforced by
   `TestPackCarriesNoHostOrAddress`, which is deliberately stricter than the
   rule it protects. Every message below is written dot-free.

## 4. The pairing principle

`exists` catches an unset field; a value operator catches an explicitly bad
one; neither catches both. The pack pairs them — but only where an **absent
field is itself unsafe**. Where absence is the safe default, the `exists` half
would accuse a compliant workload, so only the value rule ships.

| property | absent means | rules |
| --- | --- | --- |
| `allowPrivilegeEscalation` | Kubernetes default is **true** — unsafe | paired |
| `runAsNonRoot` | nothing stops root — unsafe | paired |
| `readOnlyRootFilesystem` | writable root filesystem — unsafe | paired |
| `seccompProfile.type` | unconfined — unsafe | paired |
| `privileged` | false — **safe** | value only |
| `hostNetwork`, `hostPID`, `hostIPC` | false — **safe** | value only |
| `runAsUser` | the image's own user applies, which the `runAsNonRoot` pair already covers | value only |
| `capabilities.add` | no capability added — **safe** | value only |
| `hostPath`, `hostPort` | absent is the safe case | `notExists` only |
| `serviceAccountName` | the namespace default is used | `exists` only, `info` |
| `automountServiceAccountToken` | a token is mounted — unsafe, but an explicit `true` is a legitimate choice | `exists` only, `info` |

Four paired properties produce eight rules; the six value-only properties, the
two `notExists` properties and the two `exists`-only properties produce one
each. Eighteen rules on `Deployment`, which is what §7 lists.

## 5. Severity: nothing is `critical`

No rule in the pack is `critical`, and `TestNoPackRuleIsCritical` — which
already loops over `policypack.All()`, not over `reliability` alone — stands
unchanged. Adding `--policy-pack security` to a pipeline that passed yesterday
cannot fail it today.

The reasoning is not merely consistency with `reliability`:

- **Severity is a property of the cluster, not of the rule.** A `hostPath`
  mount is critical on a shared multi-tenant cluster and routine on a
  single-tenant CI runner. A pack compiled into the binary is asserting
  something about a cluster it has never seen.
- **A compiled-in pack cannot be tuned in place.** An operator who disagrees
  with a severity must fork with `--print`. A severity set too high is
  therefore more expensive to live with than one set too low.
- **`warning` is not silence.** `scan` renders it, and
  `gate --policy-pack security --fail-on warning` blocks a build. That flag is
  the operator's explicit, one-flag escalation.

Because "cannot block by default" reads too easily as "not meant to block," the
docs page ships the blocking recipe explicitly:

```bash
kubeagent gate --policy-pack security --fail-on warning
```

Every explicit-bad-value rule is `warning`. Every `-unset` rule is `info`.

## 6. Kind scope: workload pod templates only

Rules select `Deployment`, `StatefulSet`, `DaemonSet` and `CronJob`. They do
**not** select `Pod`.

This follows the `reliability` pack's precedent exactly — it selects no `Pod`
either. The reasons are the same and they are good ones: the pack's purpose is
to catch a problem before a workload goes live, a controller-owned pod
duplicates its workload's violation once per replica, and a report that names
one Deployment is actionable in a way that a report naming forty pods is not.

The accepted cost, stated in the docs: a hand-applied bare `Pod` that no
controller owns is invisible to this pack. `kubeagent scan`'s own detectors see
that pod; the policy pack does not.

Distribution follows `reliability`'s shape — one kind carries the bulk, with
selective spillover where the risk is highest, never a cross-product:

- **Deployment: 18 rules** — the full set.
- **StatefulSet and DaemonSet: 2 rules each** — `privileged` and `hostPath`,
  the two host-escape claims that matter as much on a StatefulSet as on a
  Deployment, and that a node agent shipped as a DaemonSet is most likely to
  carry.
- **CronJob: 1 rule** — `privileged`, reached through
  `spec.jobTemplate.spec.template.spec`, a different path prefix from every
  other rule in the pack.

`Job` is not selected. A Job created by a CronJob inherits the CronJob's
template, and a standalone Job is the same shape as a bare Pod — out of scope
for the same reason.

## 7. The twenty-three rules

`T` abbreviates `spec.template.spec` throughout. The single CronJob rule has a
different prefix and spells its path out in full.

### Deployment (18)

| id | path | op | values | level |
| --- | --- | --- | --- | --- |
| `security.deploy-privileged` | `T.containers[*].securityContext.privileged` | `notIn` | `true` | warning |
| `security.deploy-privilege-escalation-unset` | `T.containers[*].securityContext.allowPrivilegeEscalation` | `exists` | — | info |
| `security.deploy-privilege-escalation` | `T.containers[*].securityContext.allowPrivilegeEscalation` | `notIn` | `true` | warning |
| `security.deploy-run-as-non-root-unset` | `T.containers[*].securityContext.runAsNonRoot` | `exists` | — | info |
| `security.deploy-run-as-non-root` | `T.containers[*].securityContext.runAsNonRoot` | `notIn` | `false` | warning |
| `security.deploy-run-as-root-uid` | `T.containers[*].securityContext.runAsUser` | `gt` | `0` | warning |
| `security.deploy-read-only-root-unset` | `T.containers[*].securityContext.readOnlyRootFilesystem` | `exists` | — | info |
| `security.deploy-read-only-root` | `T.containers[*].securityContext.readOnlyRootFilesystem` | `notIn` | `false` | warning |
| `security.deploy-added-capabilities` | `T.containers[*].securityContext.capabilities.add[*]` | `notIn` | `ALL`, `SYS_ADMIN`, `SYS_MODULE`, `SYS_PTRACE`, `NET_ADMIN`, `NET_RAW`, `DAC_READ_SEARCH` | warning |
| `security.deploy-host-path-volume` | `T.volumes[*].hostPath` | `notExists` | — | warning |
| `security.deploy-host-port` | `T.containers[*].ports[*].hostPort` | `notExists` | — | warning |
| `security.deploy-host-network` | `T.hostNetwork` | `notIn` | `true` | warning |
| `security.deploy-host-pid` | `T.hostPID` | `notIn` | `true` | warning |
| `security.deploy-host-ipc` | `T.hostIPC` | `notIn` | `true` | warning |
| `security.deploy-seccomp-unset` | `T.securityContext.seccompProfile.type` | `exists` | — | info |
| `security.deploy-seccomp-unconfined` | `T.securityContext.seccompProfile.type` | `notIn` | `Unconfined` | warning |
| `security.deploy-service-account-unset` | `T.serviceAccountName` | `exists` | — | info |
| `security.deploy-automount-token-unset` | `T.automountServiceAccountToken` | `exists` | — | info |

### Spillover (5)

| id | kind | path | op | values | level |
| --- | --- | --- | --- | --- | --- |
| `security.statefulset-privileged` | StatefulSet | `T.containers[*].securityContext.privileged` | `notIn` | `true` | warning |
| `security.statefulset-host-path-volume` | StatefulSet | `T.volumes[*].hostPath` | `notExists` | — | warning |
| `security.daemonset-privileged` | DaemonSet | `T.containers[*].securityContext.privileged` | `notIn` | `true` | warning |
| `security.daemonset-host-path-volume` | DaemonSet | `T.volumes[*].hostPath` | `notExists` | — | warning |
| `security.cronjob-privileged` | CronJob | `spec.jobTemplate.spec.template.spec.containers[*].securityContext.privileged` | `notIn` | `true` | warning |

### Messages

Every message, verbatim. Each is dot-free and scheme-free, as
`TestPackCarriesNoHostOrAddress` requires.

| id | message |
| --- | --- |
| `security.deploy-privileged` | a container runs privileged, so it has full access to the host |
| `security.deploy-privilege-escalation-unset` | a container does not set allowPrivilegeEscalation, so the default lets a process gain more privileges than its parent |
| `security.deploy-privilege-escalation` | a container explicitly allows privilege escalation |
| `security.deploy-run-as-non-root-unset` | a container does not set runAsNonRoot, so nothing stops it running as root |
| `security.deploy-run-as-non-root` | a container sets runAsNonRoot to false, so it may run as root |
| `security.deploy-run-as-root-uid` | a container is pinned to user id 0, which is root |
| `security.deploy-read-only-root-unset` | a container does not set readOnlyRootFilesystem, so its root filesystem is writable |
| `security.deploy-read-only-root` | a container sets readOnlyRootFilesystem to false, so its root filesystem is writable |
| `security.deploy-added-capabilities` | a container adds a Linux capability that grants host-level power |
| `security.deploy-host-path-volume` | a volume mounts a path from the host, so a container can reach the node filesystem |
| `security.deploy-host-port` | a container binds a port directly on the node |
| `security.deploy-host-network` | the pod shares the host network namespace, so it bypasses network policy |
| `security.deploy-host-pid` | the pod shares the host process namespace, so it can see and signal node processes |
| `security.deploy-host-ipc` | the pod shares the host IPC namespace, so it can reach shared memory on the node |
| `security.deploy-seccomp-unset` | the pod sets no seccomp profile, so its containers run with syscalls unfiltered |
| `security.deploy-seccomp-unconfined` | the pod sets its seccomp profile to Unconfined, so syscalls are unfiltered |
| `security.deploy-service-account-unset` | the workload names no service account, so it uses the namespace default |
| `security.deploy-automount-token-unset` | the workload does not set automountServiceAccountToken, so an API token is mounted into every container |
| `security.statefulset-privileged` | a container runs privileged, so it has full access to the host |
| `security.statefulset-host-path-volume` | a volume mounts a path from the host, so a container can reach the node filesystem |
| `security.daemonset-privileged` | a container runs privileged, so it has full access to the host |
| `security.daemonset-host-path-volume` | a volume mounts a path from the host, so a container can reach the node filesystem |
| `security.cronjob-privileged` | a container runs privileged, so it has full access to the host |

Repeated messages across kinds follow the `reliability` precedent, where the
memory-limit message is shared by its Deployment and StatefulSet rules.

### Two rules whose semantics deserve a note

**`security.deploy-run-as-root-uid` uses `gt 0`, not `notIn ["0"]`.** The
comparison operators parse both sides as numbers, so `gt 0` is satisfied by any
positive uid and violated by `0`. An absent `runAsUser` skips, which is
correct: an unset uid is the `runAsNonRoot` rules' business, not this one's.

**`security.deploy-host-port` carries two wildcards.** A container with no
`ports` list produces exactly one absent slot — `walkFrom` propagates an absent
cursor as one absent successor, wildcard or not — which `notExists` is
satisfied by. The rule fires only on a `hostPort` that is genuinely set.

## 8. Guarantees

Two separate promises. Neither implies the other, and neither may be stated in
a way that blurs into the other.

**Read-only toward the cluster.** Evaluating the pack issues `get`/`list` only.
A rule can read a field and nothing else; there is no `--fix` path from a rule,
and a curated pack is a policy like any other. `internal/policy` is pure — no
client, no context, no I/O beyond the bytes it is handed — which is what makes
this structural rather than a rule someone must remember.

**Separately: no LLM call.** Loading, listing, printing and evaluating a pack
contact no model. `--explain` is the model path; a pack is not a smaller
version of one, and no comment, doc line, help string or commit message in this
slice may suggest otherwise.

`kubeagent policy packs` and `--print` contact nothing at all: no cluster, no
kubeconfig, no network. The bytes are compiled into the binary.

## 9. RBAC: no manifest moves

Every kind the pack selects — `Deployment`, `StatefulSet`, `DaemonSet`,
`CronJob` — is already inside `internal/policy`'s selectable set, which is
pinned to exactly the kinds `rbacprofile.coreRules` grants. Turning on
`--policy-pack security` asks for no permission a plain `kubeagent scan` did
not already have, and `kubeagent rbac print` keeps telling the truth.

It does add request volume, as any policy evaluation does: the policy path
builds its own dynamic client and lists each kind the loaded rules touch,
independently of `scan`'s typed collectors. For this pack that is four `List`
calls — one per selected kind, and no relation rule, so no supporting read.
That is how `--policy` has always worked; the pack does not change it.

## 10. Compatibility: nothing versioned moves

- **No `schemaVersion` changes.** `scan` stays at 1.2, `gate` at 1.1, and the
  other six documents do not move. A pack's violations render in the `policy`
  key that already exists, in the shape it already has.
- **`--policy-pack` is opt-in.** A command line that does not name the pack
  renders byte-identical output to today's.
- **`internal/report/testdata/golden-scan.txt` cannot move** — the golden scan
  runs no policy. The demo GIF and `website/docs/quickstart.md` are untouched.
- **No new dependency.** `go.mod` and `go.sum` do not change.
- **`internal/policypack` stays stdlib-only** (`embed`, `sort`) and imports
  nothing from kubeagent. `internal/policypack/imports_test.go` enforces both
  halves and is unchanged.

## 11. Tests

**Free, from the four existing generic tests.** `TestEveryPackLoads`,
`TestRuleIDsCarryTheirPackPrefix`, `TestNoPackRuleIsCritical` and
`TestPackCarriesNoHostOrAddress` all iterate `policypack.All()`, so they cover
the new pack the moment its registry entry lands — no edit needed. Between
them they already assert that every rule id is `security.`-prefixed, every kind
is selectable, every level is valid, every message is non-empty, no key is
misspelled (`UnmarshalStrict`), no rule is `critical`, and no message or byte of
the file names a host or an address.

**New: `internal/policypack/security_rules_test.go`,** on
`rules_test.go`'s established pattern — a `hardenedContainer()` fixture that
satisfies every container-level rule, helpers that remove or change exactly one
field, and one case per rule asserting it fires on the broken object and passes
on the hardened one. A case that could pass for the wrong reason is the failure
mode this pattern exists to prevent: a fixture whose field is absent makes every
operator except `exists` **skip**, which is not a pass, so each value-rule case
must set the field explicitly to the bad value.

**Also new: a test that the pairing principle holds** — for each of the four
paired properties, the `-unset` rule fires on an object missing the field and
the value rule does not, and vice versa. This is the assertion that catches
someone later "simplifying" a pair into one rule.

**`internal/policypack/policypack_test.go`** needs no change: `TestAllIsSortedByName`
already covers the new entry, and `reliability` sorts before `security`.

**CLI-level:** nothing here *breaks*. `TestPolicyPacksListsWhatShips` asserts
with `strings.Contains` on `reliability` and `14 rules`, and
`TestPolicyPacksPrintUnknownNameIsRefused` asserts the refusal names
`reliability` — both stay true beside a second pack. Both are nonetheless
widened to assert the `security` line and its own rule count, because a listing
test that would pass with the new pack missing is not testing the listing.

## 12. Documentation to update

- **`website/docs/features/policy-packs.md`** — the page is currently written
  for exactly one pack and needs restructuring, not appending. Specifically:
  the `policy packs` sample output gains a `security` line; the "The fourteen
  rules" heading becomes per-pack; "No rule is critical" must widen from "none
  of the fourteen `reliability` rules" to the whole registry; the RBAC section
  must widen the same way; and the "Not in this slice" bullet that reads
  "Security and cost packs" must be **narrowed to name only the cost pack**,
  not deleted — a retraction is not a deletion.
- **`CHANGELOG.md`** — an `### Added` entry under `[Unreleased]`.
- **`CLAUDE.md`** — the post-1.0 curated-packs bullet gains the second pack and
  narrows its remaining-work sentence to the cost pack and the contribution
  path. The `(vX.Y.Z)` parenthetical is added by the release commit only.
- **`website/docs/roadmap.md`** — the curated-packs item.
- **`internal/policypack/packs/security.yaml`'s own header comment** — the
  no-critical rationale, the pairing principle, and the id-namespacing note, in
  the style `reliability.yaml`'s header established.

## 13. Not in this slice

- **The `cost` pack.** Slice B. It is a different problem — most cost claims
  are thresholds, and a threshold is cluster-specific — and it overlaps
  `reliability`'s requests-and-limits rules in a way that needs its own
  decision.
- **The contribution path.** Slice C, and mostly already served: `--policy
  <path>` evaluates anyone's YAML today, `--print` forks a pack into one, and
  the four generic `All()` tests already gate any pack added to the registry.
  What remains is a written contribution process, not a feature.
- **Any engine change.** No new operator, no new relation, no new selectable
  kind. In particular the two most-requested security claims that would need
  one — "this namespace has a default-deny NetworkPolicy" and
  "`capabilities.drop` includes `ALL`" — stay unexpressible and are documented
  as such.
- **A pack on by default.** `--policy-pack` remains opt-in.
- **`Pod` and `Job` selection.** §6.
- **A registry allowlist rule.** kubeagent does not know which registry is
  yours; a curated rule naming one would be both wrong and a hostname in a
  shipped file. Forking with `--print` is the answer.

## 14. Accepted limits, to be written down rather than worked around

Each of these is a real gap. The design's response is to state it in the docs
page and the pack header, in the same voice the `reliability` page already uses
for its `*:*` image-glob limit.

1. A bare `Pod` no controller owns is not checked.
2. `runAsNonRoot` and the other container-level fields set at **pod** level do
   not satisfy the container-level rules, because the grammar has no OR. A
   workload hardened only at pod level will report violations. This is the
   most likely false-positive source in the pack and must be named prominently.
3. `capabilities.drop` cannot be checked at all.
4. RBAC bindings, service-account objects and Secrets are unreachable.
5. `security.deploy-added-capabilities` uses a fixed seven-name list. A
   capability outside it passes. The list is a curated starting point, not an
   exhaustive one, and forking is how it gets extended.
