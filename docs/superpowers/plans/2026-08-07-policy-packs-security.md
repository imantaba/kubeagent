# Curated policy packs slice 2 — the `security` pack — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a second kubeagent-curated policy pack, `security`, of twenty-three rules over workload pod templates, compiled into `internal/policypack` beside `reliability` and evaluated by the existing `--policy` engine.

**Architecture:** A pack is data, not code. This slice adds one embedded YAML file and one registry entry; `internal/policy` is not touched by any task. The four existing pack-level tests already iterate `policypack.All()`, so the new pack inherits them the moment its registry entry lands — the new test code is per-rule coverage, not contract coverage.

**Tech Stack:** Go 1.26, `embed`, `sigs.k8s.io/yaml` (already a direct dependency, used by `internal/policy/load.go`), `k8s.io/apimachinery`'s `unstructured` for test fixtures.

**Spec:** [docs/superpowers/specs/2026-08-07-policy-packs-security-design.md](../specs/2026-08-07-policy-packs-security-design.md) — read it; it is the requirements.

## Global Constraints

Every task's requirements implicitly include this section.

- **READ-ONLY toward the cluster:** `get`/`list` only, no `--fix` path from a rule. **SEPARATELY and ADDITIONALLY: no LLM call.** These are two promises, not one. Never blur them, and never let a comment, doc line, help string or commit message suggest a pack is related to `--explain`, which is the model path.
- **NO ENGINE CHANGE.** `internal/policy` is not touched by any task. No new operator, no new relation, no new selectable kind. If a rule seems to need one, the rule is wrong, not the engine.
- **`internal/policypack` stays stdlib-only** (`embed` + `sort`) and imports nothing from kubeagent. `internal/policypack/imports_test.go` enforces both halves and must not be edited.
- **NO NEW DEPENDENCY.** `go.mod` and `go.sum` must not change. Verify at the end of every task with `git diff --stat main -- go.mod go.sum` — must be empty.
- **NO SCHEMA MOVES.** `scan` stays 1.2, `gate` stays 1.1, and the other six do not move. **Never run any test with `-update`** in this slice, in any task, for any reason.
- **`internal/report/testdata/golden-scan.txt` must stay BYTE-IDENTICAL** — the golden scan runs no policy, so it cannot move. Do NOT regenerate the demo GIF or `website/docs/quickstart.md`.
- **NO CRITICAL RULE.** `TestNoPackRuleIsCritical` is generic over `policypack.All()` and must not be edited or weakened. Every explicit-bad-value rule is `warning`; every `-unset` rule is `info`.
- **NO DOT IN ANY RULE MESSAGE**, and no `"://"` and no dotted quad anywhere in the YAML. The messages in this plan are already dot-free — copy them verbatim rather than rewording.
- **CREDENTIALS:** no secrets, credentials, private IPs or internal hostnames anywhere — code, tests, fixtures, docs, help text. Documentation IPs are RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains RFC 2606 (`example.com`/`.org`/`.net`). URLs are credentials — nothing beyond `scheme://host`, and the project's own `https://k8sproject.top` links are the only permitted host. **A curated rule must never name a registry hostname** — that is exactly what `--print` and forking are for.
- **TDD:** write the failing test first, watch it fail, then implement.
- `go test` runs with `-p 2` locally, never `-short`. Put Go on PATH first: `export PATH=$PATH:/usr/local/go/bin`.
- Every commit needs `git commit -s` (DCO is enforced on `main`), authored solely by the repository owner — **no `Co-Authored-By` trailer and no AI attribution of any kind**, anywhere.
- **DANGER: never run `./chaos/run.sh` in any form.** It takes ~40 minutes and injects real cluster outages. No cluster is needed for this slice.

---

## File Structure

| File | Status | Responsibility | Task |
| --- | --- | --- | --- |
| `internal/policypack/packs/security.yaml` | create | The twenty-three curated rules plus the pack header comment | 1, 2 |
| `internal/policypack/policypack.go` | modify | One registry entry, keeping `packs` sorted by name | 1 |
| `internal/policypack/security_rules_test.go` | create | The hardened fixtures, the kind-distribution test, one fires-and-passes case per rule, and the pairing-principle test | 2, 3, 4 |
| `internal/cli/policy_test.go` | modify | Widen the two listing/refusal tests to assert the second pack | 5 |
| `website/docs/features/policy-packs.md` | modify | Restructure for two packs | 6 |
| `CHANGELOG.md` | modify | `### Added` under `[Unreleased]` | 6 |
| `CLAUDE.md` | modify | The curated-packs bullet | 6 |
| `website/docs/roadmap.md` | modify | The curated-packs entry in the post-1.0 row | 6 |

Not touched by any task: `internal/policy/**`, `internal/policypack/packs_test.go`, `internal/policypack/policypack_test.go`, `internal/policypack/imports_test.go`, `internal/policypack/rules_test.go`, `go.mod`, `go.sum`, everything under `deploy/`.

## Cross-Task Notes the Implementers Must Know

1. **Tasks 1 and 2 write the same file.** Task 1 lands the header comment and the eighteen `Deployment` rules; Task 2 **appends** five more. Task 2's implementer must not rewrite, reorder, renumber or reword anything Task 1 landed.
2. **There is no per-rule coverage until Task 3.** After Tasks 1 and 2 the pack is green on the four inherited generic tests (`TestEveryPackLoads`, `TestRuleIDsCarryTheirPackPrefix`, `TestNoPackRuleIsCritical`, `TestPackCarriesNoHostOrAddress`) plus Task 2's distribution test. That is the **expected** state, not a gap to fix early. Do not write per-rule cases in Task 1 or 2.
3. **The absent-slot rule is the trap in Task 3.** `checkOp` (`internal/policy/op.go:30-38`) gives only `exists` and `notExists` an opinion about absence; **every other operator returns `skip` on an absent slot**. A fixture whose field is simply missing therefore makes a `notIn` or `gt` rule say nothing — which is *not* a pass. Every value-rule case must set the field **explicitly** to the bad value.
4. **Boolean values are compared as text.** `stringOf` (`internal/policy/op.go:109-125`) renders a Go `bool` with `strconv.FormatBool`, so a `notIn` against `"true"` works against a boolean field — but `Assert.Values` is `[]string`, so the YAML value must be the **quoted** string `"true"`, never bare `true`, which would fail to decode.
5. **`validateArity`** (`internal/policy/load.go:149-165`) refuses `values` on `exists`/`notExists`, requires at least one for `in`/`notIn`/`matches`/`notMatches`, and requires **exactly one** for `gt`/`gte`/`lt`/`lte`.
6. **`[*]` is universally quantified and an absent cursor propagates as exactly one absent slot.** A container with no `ports` list yields one absent slot for `containers[*].ports[*].hostPort`, which `notExists` is satisfied by.

---

## Task 1: The security pack's eighteen Deployment rules

**Files:**
- Create: `internal/policypack/packs/security.yaml`
- Modify: `internal/policypack/policypack.go:48-54` (the `packs` registry slice)

**Interfaces:**
- Consumes: `policypack.Bytes(name)`, `policypack.All()`, `policypack.Lookup(name)` — all existing, unchanged.
- Produces: a pack named `security` whose embedded file is `packs/security.yaml`, holding eighteen rules with ids prefixed `security.`. Task 2 appends to the same file. Task 3 loads it with the existing `loadPack(t, "security")` helper from `internal/policypack/packs_test.go`. Task 5 asserts the listing shows `security` and its final rule count of **23**.

- [ ] **Step 1: Cut the branch**

```bash
cd /home/ubuntu/git/kubeagent
export PATH=$PATH:/usr/local/go/bin
git checkout main
git checkout -b policy-pack-security
git log --oneline -1   # must be the spec commit
```

- [ ] **Step 2: Add the registry entry — this is the failing test**

`policypack.Bytes("security")` reads the embedded `packs/security.yaml`, which does not exist yet, so it returns `false` and the existing `loadPack` helper fatals. That is the red state; no new test file is needed to produce it.

Edit `internal/policypack/policypack.go`. The `packs` slice currently holds one entry; append the second, **keeping the slice in name order** — `reliability` sorts before `security`, so appending is correct, and `TestAllIsSortedByName` pins it:

```go
// packs is the registry, in name order.
var packs = []Pack{
	{
		Name:    "reliability",
		Summary: "probes, resource requests and limits, replica counts, disruption budgets and image tags",
		file:    "packs/reliability.yaml",
	},
	{
		Name:    "security",
		Summary: "privileged containers, host namespaces and paths, root filesystems, capabilities and service account tokens",
		file:    "packs/security.yaml",
	},
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -p 2 ./internal/policypack/`

Expected: FAIL, with `TestEveryPackLoads/security` reporting `Bytes("security") = false, want the shipped pack`.

- [ ] **Step 4: Write the pack file**

Create `internal/policypack/packs/security.yaml` with exactly this content. The header comment follows `reliability.yaml`'s established shape; the eighteen rules follow the spec's §7 table in order.

```yaml
# kubeagent security pack.
#
# Every rule here is about a workload's own hardening: what a container may do
# to its node, whether it runs as root, whether its root filesystem is
# writable, and which of the node's namespaces the pod shares.
#
# Where a field's absence is itself unsafe there are TWO rules — an `exists`
# rule at info for the unset case, and a value rule at warning for the
# explicitly bad one. Every operator except `exists` and `notExists` skips an
# absent field, so one rule alone could only ever catch one of the two. Where
# absence is the safe default — privileged, hostNetwork, an added capability —
# one value rule is enough, and a second would accuse a compliant workload.
#
# No rule is `critical`, deliberately. `gate` fails on critical by default, so
# adding this pack to a pipeline must not fail a build that passed yesterday.
# An operator who wants these to block raises --fail-on warning.
#
# Three things this pack cannot say, for three properties the grammar does not
# have. With no existential quantifier it cannot require that
# `capabilities.drop` includes ALL. With no OR it cannot accept a hardening
# field set at pod level in place of the container-level rule, so a workload
# hardened only at pod level will report violations. And RBAC bindings,
# service account objects and Secrets are not kinds a policy may select at
# all, so nothing here can reach them.
#
# Rule ids are namespaced with the pack name so they cannot collide with an
# operator's own rules when both are given.

- id: security.deploy-privileged
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].securityContext.privileged
    op: notIn
    values:
      - "true"
  level: warning
  message: a container runs privileged, so it has full access to the host

- id: security.deploy-privilege-escalation-unset
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].securityContext.allowPrivilegeEscalation
    op: exists
  level: info
  message: a container does not set allowPrivilegeEscalation, so the default lets a process gain more privileges than its parent

- id: security.deploy-privilege-escalation
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].securityContext.allowPrivilegeEscalation
    op: notIn
    values:
      - "true"
  level: warning
  message: a container explicitly allows privilege escalation

- id: security.deploy-run-as-non-root-unset
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].securityContext.runAsNonRoot
    op: exists
  level: info
  message: a container does not set runAsNonRoot, so nothing stops it running as root

- id: security.deploy-run-as-non-root
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].securityContext.runAsNonRoot
    op: notIn
    values:
      - "false"
  level: warning
  message: a container sets runAsNonRoot to false, so it may run as root

- id: security.deploy-run-as-root-uid
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].securityContext.runAsUser
    op: gt
    values:
      - "0"
  level: warning
  message: a container is pinned to user id 0, which is root

- id: security.deploy-read-only-root-unset
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].securityContext.readOnlyRootFilesystem
    op: exists
  level: info
  message: a container does not set readOnlyRootFilesystem, so its root filesystem is writable

- id: security.deploy-read-only-root
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].securityContext.readOnlyRootFilesystem
    op: notIn
    values:
      - "false"
  level: warning
  message: a container sets readOnlyRootFilesystem to false, so its root filesystem is writable

- id: security.deploy-added-capabilities
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].securityContext.capabilities.add[*]
    op: notIn
    values:
      - ALL
      - SYS_ADMIN
      - SYS_MODULE
      - SYS_PTRACE
      - NET_ADMIN
      - NET_RAW
      - DAC_READ_SEARCH
  level: warning
  message: a container adds a Linux capability that grants host-level power

- id: security.deploy-host-path-volume
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.volumes[*].hostPath
    op: notExists
  level: warning
  message: a volume mounts a path from the host, so a container can reach the node filesystem

- id: security.deploy-host-port
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].ports[*].hostPort
    op: notExists
  level: warning
  message: a container binds a port directly on the node

- id: security.deploy-host-network
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.hostNetwork
    op: notIn
    values:
      - "true"
  level: warning
  message: the pod shares the host network namespace, so it bypasses network policy

- id: security.deploy-host-pid
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.hostPID
    op: notIn
    values:
      - "true"
  level: warning
  message: the pod shares the host process namespace, so it can see and signal node processes

- id: security.deploy-host-ipc
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.hostIPC
    op: notIn
    values:
      - "true"
  level: warning
  message: the pod shares the host IPC namespace, so it can reach shared memory on the node

- id: security.deploy-seccomp-unset
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.securityContext.seccompProfile.type
    op: exists
  level: info
  message: the pod sets no seccomp profile, so its containers run with syscalls unfiltered

- id: security.deploy-seccomp-unconfined
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.securityContext.seccompProfile.type
    op: notIn
    values:
      - Unconfined
  level: warning
  message: the pod sets its seccomp profile to Unconfined, so syscalls are unfiltered

- id: security.deploy-service-account-unset
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.serviceAccountName
    op: exists
  level: info
  message: the workload names no service account, so it uses the namespace default

- id: security.deploy-automount-token-unset
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.automountServiceAccountToken
    op: exists
  level: info
  message: the workload does not set automountServiceAccountToken, so an API token is mounted into every container
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -p 2 ./internal/policypack/ -v -run 'TestEveryPackLoads|TestRuleIDsCarryTheirPackPrefix|TestNoPackRuleIsCritical|TestPackCarriesNoHostOrAddress|TestAllIsSortedByName'`

Expected: PASS, with a `security` subtest under each of the four generic tests.

- [ ] **Step 6: Verify the whole tree and the dependency constraint**

```bash
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./...
git diff --stat main -- go.mod go.sum          # must be empty
git diff --stat main -- internal/report/testdata/golden-scan.txt   # must be empty
```

- [ ] **Step 7: Commit**

```bash
git add internal/policypack/packs/security.yaml internal/policypack/policypack.go
git commit -s -m "feat(policypack): the security pack's eighteen Deployment rules

Where a field's absence is itself unsafe the pack pairs an exists rule at
info with a value rule at warning, because every operator except exists
and notExists skips an absent slot and one rule alone could only ever
catch one of the two cases. Where absence is the safe default a single
value rule ships, since the exists half would accuse a compliant
workload.

Nothing is critical, so the pack cannot fail a pipeline that passed
yesterday. The four generic tests over policypack.All() cover the new
pack unchanged."
```

---

## Task 2: The five spillover rules, and the kind-distribution test

**Files:**
- Modify: `internal/policypack/packs/security.yaml` (append only)
- Create: `internal/policypack/security_rules_test.go`

**Interfaces:**
- Consumes: `loadPack(t, "security")` from `internal/policypack/packs_test.go:15`, which returns `[]policy.Rule`; `policy.Rule` has fields `ID`, `Match.Kind`, `Assert`, `Level`, `Message`.
- Produces: `internal/policypack/security_rules_test.go` in package `policypack_test`, holding `TestSecurityPackKindDistribution`. Tasks 3 and 4 add fixtures and further tests to this same file.

**Do not** rewrite, reorder, renumber or reword any rule Task 1 landed. Append the five new rules to the end of the file.

- [ ] **Step 1: Write the failing test**

Create `internal/policypack/security_rules_test.go`:

```go
package policypack_test

import (
	"testing"
)

// TestSecurityPackKindDistribution pins the pack's scope decision: the full
// rule set runs against Deployment, and only the two host-escape claims spill
// over to the kinds where they bite as hard. It is not a cross-product, and it
// deliberately never selects Pod or Job — a controller-owned pod repeats its
// workload's violation once per replica, and a report naming one Deployment is
// actionable in a way that a report naming forty pods is not.
func TestSecurityPackKindDistribution(t *testing.T) {
	byKind := map[string]int{}
	for _, r := range loadPack(t, "security") {
		byKind[r.Match.Kind]++
	}

	want := map[string]int{
		"Deployment":  18,
		"StatefulSet": 2,
		"DaemonSet":   2,
		"CronJob":     1,
	}
	for kind, n := range want {
		if byKind[kind] != n {
			t.Errorf("%d rules select %s, want %d", byKind[kind], kind, n)
		}
	}
	for kind := range byKind {
		if _, ok := want[kind]; !ok {
			t.Errorf("the pack selects %s, which is not one of the four workload kinds it is scoped to", kind)
		}
	}

	total := 0
	for _, n := range byKind {
		total += n
	}
	if total != 23 {
		t.Errorf("the pack holds %d rules, want 23", total)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -p 2 ./internal/policypack/ -run TestSecurityPackKindDistribution -v`

Expected: FAIL with `0 rules select StatefulSet, want 2`, `0 rules select DaemonSet, want 2`, `0 rules select CronJob, want 1`, and `the pack holds 18 rules, want 23`.

- [ ] **Step 3: Append the five spillover rules**

Append to the end of `internal/policypack/packs/security.yaml`:

```yaml
- id: security.statefulset-privileged
  match:
    kind: StatefulSet
  assert:
    path: spec.template.spec.containers[*].securityContext.privileged
    op: notIn
    values:
      - "true"
  level: warning
  message: a container runs privileged, so it has full access to the host

- id: security.statefulset-host-path-volume
  match:
    kind: StatefulSet
  assert:
    path: spec.template.spec.volumes[*].hostPath
    op: notExists
  level: warning
  message: a volume mounts a path from the host, so a container can reach the node filesystem

- id: security.daemonset-privileged
  match:
    kind: DaemonSet
  assert:
    path: spec.template.spec.containers[*].securityContext.privileged
    op: notIn
    values:
      - "true"
  level: warning
  message: a container runs privileged, so it has full access to the host

- id: security.daemonset-host-path-volume
  match:
    kind: DaemonSet
  assert:
    path: spec.template.spec.volumes[*].hostPath
    op: notExists
  level: warning
  message: a volume mounts a path from the host, so a container can reach the node filesystem

- id: security.cronjob-privileged
  match:
    kind: CronJob
  assert:
    path: spec.jobTemplate.spec.template.spec.containers[*].securityContext.privileged
    op: notIn
    values:
      - "true"
  level: warning
  message: a container runs privileged, so it has full access to the host
```

Note the CronJob path: a CronJob's pod template lives one level deeper, at `spec.jobTemplate.spec.template.spec`, not `spec.template.spec`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -p 2 ./internal/policypack/ -v`

Expected: PASS, including all four generic tests over `policypack.All()` with a `security` subtest each, and `TestSecurityPackKindDistribution`.

- [ ] **Step 5: Verify the dependency constraint**

```bash
go build ./... && go vet ./... && gofmt -l internal/
git diff --stat main -- go.mod go.sum   # must be empty
```

- [ ] **Step 6: Commit**

```bash
git add internal/policypack/packs/security.yaml internal/policypack/security_rules_test.go
git commit -s -m "feat(policypack): the security pack's five spillover rules

privileged and hostPath spill from Deployment to StatefulSet and
DaemonSet, and privileged to CronJob through the deeper
spec.jobTemplate.spec.template.spec path. That is selective spillover
where the risk bites, not a cross-product, and the pack still selects
neither Pod nor Job.

The distribution is pinned by a test rather than left to review."
```

---

## Task 3: One fires-and-passes case per rule

**Files:**
- Modify: `internal/policypack/security_rules_test.go`

**Interfaces:**
- Consumes: from `internal/policypack/rules_test.go` (same package, already in the tree) the constants `fixtureNamespace = "app"` and `fixtureImage = "registry.example.com/team/app:1.0"`, and the `ruleCase` struct:
  ```go
  type ruleCase struct {
      id         string
      kind       string
      violating  *unstructured.Unstructured
      satisfying *unstructured.Unstructured
      support    map[string][]*unstructured.Unstructured
  }
  ```
  Reuse both — **do not redeclare them**, and do not redeclare `goodContainer`, `containerWithout`, `containerWithImage`, `workload`, `deployment`, `pdb`, `cronJob` or `pvc`, which already exist in the same package. Every helper this task adds carries a distinct name.
- Consumes: `policy.Evaluate(rules []policy.Rule, in policy.Inputs) (violations []policy.Violation, notEvaluated []policy.Unevaluated)` and `policy.InputsFrom(objects map[string][]*unstructured.Unstructured, unreadable map[string]bool) policy.Inputs`.
- Produces: the hardened fixture helpers listed in Step 3, which Task 4 reuses.

**The trap in this task:** `checkOp` gives only `exists` and `notExists` an opinion about absence; every other operator **skips** an absent slot. A case whose field is simply missing therefore proves nothing about a `notIn` or `gt` rule. Every value-rule case below sets the field **explicitly** to the bad value, and that is why.

- [ ] **Step 1: Write the failing test — the fixtures**

Append to `internal/policypack/security_rules_test.go`. Add `"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"` and `"github.com/imantaba/kubeagent/internal/policy"` to the file's import block.

```go
// hardenedContainer satisfies every container-level rule in the security pack.
// Each case below starts from it and changes or removes exactly the one thing
// its rule is about, so a case can only fail for its own reason.
//
// capabilities carries drop but no add: an absent add[*] is one absent slot,
// which notIn skips — which is the correct satisfying side, since the rule is
// about a capability that was added, not about one that was not dropped.
func hardenedContainer() map[string]any {
	return map[string]any{
		"name":  "app",
		"image": fixtureImage,
		"ports": []any{map[string]any{"containerPort": int64(8080)}},
		"securityContext": map[string]any{
			"privileged":               false,
			"allowPrivilegeEscalation": false,
			"runAsNonRoot":             true,
			"runAsUser":                int64(1000),
			"readOnlyRootFilesystem":   true,
			"capabilities":             map[string]any{"drop": []any{"ALL"}},
		},
	}
}

// containerMinus returns a hardened container with one field removed, walking
// the path so a nested field can be removed too:
// containerMinus(t, "securityContext", "runAsNonRoot").
func containerMinus(t *testing.T, path ...string) map[string]any {
	t.Helper()
	c := hardenedContainer()
	m := c
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			t.Fatalf("containerMinus(%v): %q is not a map", path, k)
		}
		m = next
	}
	delete(m, path[len(path)-1])
	return c
}

// containerWithSecurityContext returns a hardened container with one
// securityContext field set explicitly to v. Setting it explicitly is the
// whole point: every operator except exists and notExists SKIPS an absent
// slot, so a fixture that merely omitted the field would make the rule say
// nothing, which is not a pass.
func containerWithSecurityContext(t *testing.T, key string, v any) map[string]any {
	t.Helper()
	c := hardenedContainer()
	sc, ok := c["securityContext"].(map[string]any)
	if !ok {
		t.Fatal("hardenedContainer has no securityContext map")
	}
	sc[key] = v
	return c
}

// containerWithAddedCapability returns a hardened container that adds one
// Linux capability.
func containerWithAddedCapability(t *testing.T, name string) map[string]any {
	t.Helper()
	c := hardenedContainer()
	sc, ok := c["securityContext"].(map[string]any)
	if !ok {
		t.Fatal("hardenedContainer has no securityContext map")
	}
	caps, ok := sc["capabilities"].(map[string]any)
	if !ok {
		t.Fatal("hardenedContainer has no capabilities map")
	}
	caps["add"] = []any{name}
	return c
}

// containerWithHostPort returns a hardened container whose port is bound on
// the node itself.
func containerWithHostPort() map[string]any {
	c := hardenedContainer()
	c["ports"] = []any{map[string]any{"containerPort": int64(8080), "hostPort": int64(8080)}}
	return c
}

// hardenedPodSpec satisfies every pod-level rule in the security pack. It
// carries a volume deliberately: a spec with no volumes at all would satisfy
// the hostPath rule vacuously, so the satisfying side proves that an ordinary
// volume passes rather than that no volume was looked at.
func hardenedPodSpec(c map[string]any) map[string]any {
	return map[string]any{
		"serviceAccountName":           "app",
		"automountServiceAccountToken": false,
		"hostNetwork":                  false,
		"hostPID":                      false,
		"hostIPC":                      false,
		"securityContext": map[string]any{
			"seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"volumes":    []any{map[string]any{"name": "cache", "emptyDir": map[string]any{}}},
		"containers": []any{c},
	}
}

// podSpecWith returns a hardened pod spec with one top-level field set
// explicitly to v.
func podSpecWith(c map[string]any, key string, v any) map[string]any {
	spec := hardenedPodSpec(c)
	spec[key] = v
	return spec
}

// podSpecWithout returns a hardened pod spec with one top-level field removed.
func podSpecWithout(c map[string]any, key string) map[string]any {
	spec := hardenedPodSpec(c)
	delete(spec, key)
	return spec
}

// podSpecWithSeccomp returns a hardened pod spec whose seccomp profile type is
// set explicitly to v.
func podSpecWithSeccomp(c map[string]any, v string) map[string]any {
	spec := hardenedPodSpec(c)
	spec["securityContext"] = map[string]any{"seccompProfile": map[string]any{"type": v}}
	return spec
}

// podSpecWithHostPathVolume returns a hardened pod spec carrying a second
// volume that mounts a path from the node.
func podSpecWithHostPathVolume(c map[string]any) map[string]any {
	spec := hardenedPodSpec(c)
	spec["volumes"] = []any{
		map[string]any{"name": "cache", "emptyDir": map[string]any{}},
		map[string]any{"name": "host-root", "hostPath": map[string]any{"path": "/"}},
	}
	return spec
}

// hardenedWorkload builds a Deployment, StatefulSet or DaemonSet around one
// pod spec.
func hardenedWorkload(kind, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"replicas": int64(2),
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
				"spec":     spec,
			},
		},
	}}
}

// hardenedDeployment is the shorthand the eighteen Deployment cases use.
func hardenedDeployment(name string, spec map[string]any) *unstructured.Unstructured {
	return hardenedWorkload("Deployment", name, spec)
}

// hardenedCronJob wraps a pod spec in the batch/v1 shape, whose template lives
// one level deeper than a Deployment's: spec.jobTemplate.spec.template.spec.
func hardenedCronJob(name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"schedule": "*/5 * * * *",
			"jobTemplate": map[string]any{
				"spec": map[string]any{
					"template": map[string]any{
						"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
						"spec":     spec,
					},
				},
			},
		},
	}}
}

// evaluateOne runs exactly one rule against exactly one object and returns its
// violations, failing the test if anything was unreadable.
func evaluateOne(t *testing.T, r policy.Rule, kind string, obj *unstructured.Unstructured) []policy.Violation {
	t.Helper()
	objects := map[string][]*unstructured.Unstructured{kind: {obj}}
	violations, notEvaluated := policy.Evaluate([]policy.Rule{r}, policy.InputsFrom(objects, nil))
	if len(notEvaluated) != 0 {
		t.Fatalf("nothing was unreadable, got %#v", notEvaluated)
	}
	return violations
}

// securityRule finds one rule in the pack by id.
func securityRule(t *testing.T, rules []policy.Rule, id string) policy.Rule {
	t.Helper()
	for _, r := range rules {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("the pack has no rule %q", id)
	return policy.Rule{}
}
```

- [ ] **Step 2: Write the failing test — the twenty-three cases**

Append to the same file:

```go
// TestEverySecurityRuleFiresAndPasses drives each rule through the real
// evaluator, alone, against an object that must violate it and one that must
// not. A rule with a typo'd path or the wrong operator loads cleanly and
// checks nothing; this is what catches that.
func TestEverySecurityRuleFiresAndPasses(t *testing.T) {
	rules := loadPack(t, "security")

	cases := []ruleCase{
		{
			id:         "security.deploy-privileged",
			kind:       "Deployment",
			violating:  hardenedDeployment("privileged", hardenedPodSpec(containerWithSecurityContext(t, "privileged", true))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-privilege-escalation-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("escalation-unset", hardenedPodSpec(containerMinus(t, "securityContext", "allowPrivilegeEscalation"))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-privilege-escalation",
			kind:       "Deployment",
			violating:  hardenedDeployment("escalates", hardenedPodSpec(containerWithSecurityContext(t, "allowPrivilegeEscalation", true))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-run-as-non-root-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("non-root-unset", hardenedPodSpec(containerMinus(t, "securityContext", "runAsNonRoot"))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-run-as-non-root",
			kind:       "Deployment",
			violating:  hardenedDeployment("may-be-root", hardenedPodSpec(containerWithSecurityContext(t, "runAsNonRoot", false))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:   "security.deploy-run-as-root-uid",
			kind: "Deployment",
			// runAsUser is set explicitly: gt SKIPS an absent field, so a
			// fixture that omitted it would make the rule pass for the wrong
			// reason.
			violating:  hardenedDeployment("uid-zero", hardenedPodSpec(containerWithSecurityContext(t, "runAsUser", int64(0)))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-read-only-root-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("read-only-unset", hardenedPodSpec(containerMinus(t, "securityContext", "readOnlyRootFilesystem"))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-read-only-root",
			kind:       "Deployment",
			violating:  hardenedDeployment("writable-root", hardenedPodSpec(containerWithSecurityContext(t, "readOnlyRootFilesystem", false))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-added-capabilities",
			kind:       "Deployment",
			violating:  hardenedDeployment("sys-admin", hardenedPodSpec(containerWithAddedCapability(t, "SYS_ADMIN"))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:   "security.deploy-host-path-volume",
			kind: "Deployment",
			// The satisfying side carries an emptyDir volume, so it proves an
			// ordinary volume passes rather than that no volume was looked at.
			violating:  hardenedDeployment("host-root", podSpecWithHostPathVolume(hardenedContainer())),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:   "security.deploy-host-port",
			kind: "Deployment",
			// The satisfying side declares a containerPort, so it proves a
			// port without hostPort passes rather than that no port existed.
			violating:  hardenedDeployment("node-port", hardenedPodSpec(containerWithHostPort())),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-host-network",
			kind:       "Deployment",
			violating:  hardenedDeployment("host-network", podSpecWith(hardenedContainer(), "hostNetwork", true)),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-host-pid",
			kind:       "Deployment",
			violating:  hardenedDeployment("host-pid", podSpecWith(hardenedContainer(), "hostPID", true)),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-host-ipc",
			kind:       "Deployment",
			violating:  hardenedDeployment("host-ipc", podSpecWith(hardenedContainer(), "hostIPC", true)),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-seccomp-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("seccomp-unset", podSpecWithout(hardenedContainer(), "securityContext")),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-seccomp-unconfined",
			kind:       "Deployment",
			violating:  hardenedDeployment("unconfined", podSpecWithSeccomp(hardenedContainer(), "Unconfined")),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-service-account-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("default-sa", podSpecWithout(hardenedContainer(), "serviceAccountName")),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-automount-token-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("automount-unset", podSpecWithout(hardenedContainer(), "automountServiceAccountToken")),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.statefulset-privileged",
			kind:       "StatefulSet",
			violating:  hardenedWorkload("StatefulSet", "privileged", hardenedPodSpec(containerWithSecurityContext(t, "privileged", true))),
			satisfying: hardenedWorkload("StatefulSet", "ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.statefulset-host-path-volume",
			kind:       "StatefulSet",
			violating:  hardenedWorkload("StatefulSet", "host-root", podSpecWithHostPathVolume(hardenedContainer())),
			satisfying: hardenedWorkload("StatefulSet", "ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.daemonset-privileged",
			kind:       "DaemonSet",
			violating:  hardenedWorkload("DaemonSet", "privileged", hardenedPodSpec(containerWithSecurityContext(t, "privileged", true))),
			satisfying: hardenedWorkload("DaemonSet", "ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.daemonset-host-path-volume",
			kind:       "DaemonSet",
			violating:  hardenedWorkload("DaemonSet", "host-root", podSpecWithHostPathVolume(hardenedContainer())),
			satisfying: hardenedWorkload("DaemonSet", "ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.cronjob-privileged",
			kind:       "CronJob",
			violating:  hardenedCronJob("privileged", hardenedPodSpec(containerWithSecurityContext(t, "privileged", true))),
			satisfying: hardenedCronJob("ok", hardenedPodSpec(hardenedContainer())),
		},
	}

	if len(cases) != len(rules) {
		t.Fatalf("%d cases for %d rules — every rule must be proved from both sides", len(cases), len(rules))
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			r := securityRule(t, rules, tc.id)

			violations := evaluateOne(t, r, tc.kind, tc.violating)
			if len(violations) != 1 {
				t.Fatalf("violating object produced %d violations, want 1", len(violations))
			}
			if violations[0].RuleID != tc.id {
				t.Errorf("violation is from rule %q, want %q", violations[0].RuleID, tc.id)
			}
			if violations[0].Level == policy.LevelCritical {
				t.Errorf("violation is critical — no pack rule may be")
			}

			if violations := evaluateOne(t, r, tc.kind, tc.satisfying); len(violations) != 0 {
				t.Errorf("satisfying object produced %d violations, want 0: %#v", len(violations), violations)
			}
		})
	}
}
```

- [ ] **Step 3: Run the test to verify it fails, then passes**

The fixtures and the cases land together, so this test does not have a red phase of its own — Tasks 1 and 2 already proved the pack loads, and this task's job is to prove each rule actually checks something. Run it and read every subtest:

Run: `go test -p 2 ./internal/policypack/ -run TestEverySecurityRuleFiresAndPasses -v`

Expected: PASS, 23 subtests.

**If a subtest fails on the violating side with `produced 0 violations, want 1`, do not "fix" it by changing the rule.** First check whether the fixture left the field absent: `notIn` and `gt` skip an absent slot, so the fixture is wrong, not the rule.

- [ ] **Step 4: Prove the test can actually fail**

A fires-and-passes table that would pass against a broken pack is worthless. Prove it discriminates, then undo:

```bash
# Temporarily break one rule's path, confirm the suite goes red, then restore.
sed -i 's|path: spec.template.spec.hostPID|path: spec.template.spec.hostPidTypo|' internal/policypack/packs/security.yaml
go test -p 2 ./internal/policypack/ -run TestEverySecurityRuleFiresAndPasses 2>&1 | tail -5
git checkout internal/policypack/packs/security.yaml
go test -p 2 ./internal/policypack/ -run TestEverySecurityRuleFiresAndPasses
```

Expected: the first run FAILS on `security.deploy-host-pid` with `violating object produced 0 violations, want 1`; the second PASSES. The `git checkout` must run — confirm with `git diff --stat internal/policypack/packs/security.yaml` being empty before committing.

- [ ] **Step 5: Verify the whole tree**

```bash
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./...
git diff --stat main -- go.mod go.sum   # must be empty
```

- [ ] **Step 6: Commit**

```bash
git add internal/policypack/security_rules_test.go
git commit -s -m "test(policypack): prove every security rule from both sides

Twenty-three cases, each starting from a hardened fixture and breaking
exactly one thing. A rule with a typo'd path or the wrong operator loads
cleanly and checks nothing, which is what this catches.

Every value-rule case sets its field explicitly to the bad value rather
than omitting it: notIn and gt skip an absent slot, so an omitted field
would make the rule say nothing, and saying nothing is not passing. The
hostPath and hostPort fixtures carry an ordinary volume and an ordinary
containerPort on the satisfying side for the same reason."
```

---

## Task 4: The pairing-principle test

**Files:**
- Modify: `internal/policypack/security_rules_test.go`

**Interfaces:**
- Consumes: `hardenedContainer()`, `containerMinus(t, path...)`, `containerWithSecurityContext(t, key, v)`, `hardenedPodSpec(c)`, `podSpecWithout(c, key)`, `podSpecWithSeccomp(c, v)`, `hardenedDeployment(name, spec)`, `evaluateOne(t, r, kind, obj)` and `securityRule(t, rules, id)` — all from Task 3.
- Produces: nothing later tasks consume.

This is the test that catches someone later "simplifying" a pair into one rule. For each of the four paired properties it asserts the division of labour in **both** directions: the `-unset` rule fires on a missing field while the value rule stays silent, and the value rule fires on an explicit bad value while the `-unset` rule stays silent.

- [ ] **Step 1: Write the failing test**

Append to `internal/policypack/security_rules_test.go`:

```go
// TestPairedRulesDivideTheWork pins the pack's pairing principle. Four
// properties are unsafe when absent AND unsafe when set to the wrong value,
// and no single rule can catch both: exists says nothing about a bad value,
// and notIn SKIPS an absent slot. So each ships as a pair, and each half must
// cover exactly the case the other cannot.
//
// Asserting both directions is the point. A single-direction test would still
// pass if someone collapsed a pair into one rule, which is precisely the
// change this exists to fail.
func TestPairedRulesDivideTheWork(t *testing.T) {
	rules := loadPack(t, "security")

	cases := []struct {
		property string
		unsetID  string
		valueID  string
		// unset is missing the field entirely.
		unset *unstructured.Unstructured
		// bad sets the field explicitly to the unsafe value.
		bad *unstructured.Unstructured
	}{
		{
			property: "allowPrivilegeEscalation",
			unsetID:  "security.deploy-privilege-escalation-unset",
			valueID:  "security.deploy-privilege-escalation",
			unset:    hardenedDeployment("unset", hardenedPodSpec(containerMinus(t, "securityContext", "allowPrivilegeEscalation"))),
			bad:      hardenedDeployment("bad", hardenedPodSpec(containerWithSecurityContext(t, "allowPrivilegeEscalation", true))),
		},
		{
			property: "runAsNonRoot",
			unsetID:  "security.deploy-run-as-non-root-unset",
			valueID:  "security.deploy-run-as-non-root",
			unset:    hardenedDeployment("unset", hardenedPodSpec(containerMinus(t, "securityContext", "runAsNonRoot"))),
			bad:      hardenedDeployment("bad", hardenedPodSpec(containerWithSecurityContext(t, "runAsNonRoot", false))),
		},
		{
			property: "readOnlyRootFilesystem",
			unsetID:  "security.deploy-read-only-root-unset",
			valueID:  "security.deploy-read-only-root",
			unset:    hardenedDeployment("unset", hardenedPodSpec(containerMinus(t, "securityContext", "readOnlyRootFilesystem"))),
			bad:      hardenedDeployment("bad", hardenedPodSpec(containerWithSecurityContext(t, "readOnlyRootFilesystem", false))),
		},
		{
			property: "seccompProfile",
			unsetID:  "security.deploy-seccomp-unset",
			valueID:  "security.deploy-seccomp-unconfined",
			unset:    hardenedDeployment("unset", podSpecWithout(hardenedContainer(), "securityContext")),
			bad:      hardenedDeployment("bad", podSpecWithSeccomp(hardenedContainer(), "Unconfined")),
		},
	}

	if len(cases) != 4 {
		t.Fatalf("%d paired properties, want 4", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.property, func(t *testing.T) {
			unsetRule := securityRule(t, rules, tc.unsetID)
			valueRule := securityRule(t, rules, tc.valueID)

			// The unset object: only the exists half may fire.
			if got := evaluateOne(t, unsetRule, "Deployment", tc.unset); len(got) != 1 {
				t.Errorf("%s produced %d violations on an object missing %s, want 1", tc.unsetID, len(got), tc.property)
			}
			if got := evaluateOne(t, valueRule, "Deployment", tc.unset); len(got) != 0 {
				t.Errorf("%s produced %d violations on an object missing %s, want 0 — a value operator must skip an absent slot", tc.valueID, len(got), tc.property)
			}

			// The explicitly-bad object: only the value half may fire.
			if got := evaluateOne(t, valueRule, "Deployment", tc.bad); len(got) != 1 {
				t.Errorf("%s produced %d violations on an object setting %s to the unsafe value, want 1", tc.valueID, len(got), tc.property)
			}
			if got := evaluateOne(t, unsetRule, "Deployment", tc.bad); len(got) != 0 {
				t.Errorf("%s produced %d violations on an object that DOES set %s, want 0", tc.unsetID, len(got), tc.property)
			}

			// The levels are part of the principle, not decoration: the unset
			// half is advisory, the explicit-bad half is a warning.
			if unsetRule.Level != policy.LevelInfo {
				t.Errorf("%s is %q, want info", tc.unsetID, unsetRule.Level)
			}
			if valueRule.Level != policy.LevelWarning {
				t.Errorf("%s is %q, want warning", tc.valueID, valueRule.Level)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test -p 2 ./internal/policypack/ -run TestPairedRulesDivideTheWork -v`

Expected: PASS, 4 subtests.

- [ ] **Step 3: Prove the test can actually fail**

```bash
# Temporarily change one unset rule's level, confirm the suite goes red, then restore.
sed -i '/^- id: security.deploy-run-as-non-root-unset$/,/^$/ s/^  level: info$/  level: warning/' internal/policypack/packs/security.yaml
go test -p 2 ./internal/policypack/ -run TestPairedRulesDivideTheWork 2>&1 | tail -5
git checkout internal/policypack/packs/security.yaml
go test -p 2 ./internal/policypack/ -run TestPairedRulesDivideTheWork
```

Expected: the first run FAILS with `security.deploy-run-as-non-root-unset is "warning", want info`; the second PASSES. Confirm `git diff --stat internal/policypack/packs/security.yaml` is empty before committing.

- [ ] **Step 4: Verify the whole tree**

```bash
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./...
git diff --stat main -- go.mod go.sum   # must be empty
```

- [ ] **Step 5: Commit**

```bash
git add internal/policypack/security_rules_test.go
git commit -s -m "test(policypack): pin the pairing principle in both directions

Four properties are unsafe absent and unsafe when set wrong, and no
single rule can catch both: exists says nothing about a bad value, and
notIn skips an absent slot. Each half must cover exactly the case the
other cannot, and the levels — info for unset, warning for explicitly
bad — are part of the division rather than decoration.

Asserting both directions is what makes collapsing a pair into one rule
fail the suite."
```

---

## Task 5: Widen the CLI listing and refusal tests

**Files:**
- Modify: `internal/cli/policy_test.go:235-251` (`TestPolicyPacksListsWhatShips`) and `internal/cli/policy_test.go:267-283` (`TestPolicyPacksPrintUnknownNameIsRefused`)

**Interfaces:**
- Consumes: `runPolicyPacks(cmd, printName string, w io.Writer) error` — existing, unchanged. Call it as `runPolicyPacks(nil, "", &buf)` to list and `runPolicyPacks(nil, "security", &buf)` to print.
- Produces: nothing later tasks consume.

**Nothing here is broken today.** Both tests use `strings.Contains`, so both already pass beside a second pack. They are widened anyway, because a listing test that would pass with the new pack **missing** is not testing the listing.

- [ ] **Step 1: Write the failing assertions**

Replace `TestPolicyPacksListsWhatShips` in `internal/cli/policy_test.go` with:

```go
func TestPolicyPacksListsWhatShips(t *testing.T) {
	var buf bytes.Buffer
	if err := runPolicyPacks(nil, "", &buf); err != nil {
		t.Fatalf("runPolicyPacks: %v", err)
	}
	out := buf.String()
	// Every shipped pack must appear. Asserting one pack would still pass with
	// a second one missing from the registry, which is the failure this is for.
	for _, want := range []string{"reliability", "security"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not name the %s pack:\n%s", want, out)
		}
	}
	// The counts come from loading, so they cannot drift from the files.
	for _, want := range []string{"14 rules", "23 rules"} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing does not carry the %q count:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "kubeagent policy packs --print") {
		t.Errorf("the listing does not say how to print one:\n%s", out)
	}
}
```

Then, in `TestPolicyPacksPrintUnknownNameIsRefused`, replace the single-pack assertion:

```go
	if !strings.Contains(err.Error(), "reliability") {
		t.Errorf("the error does not name the packs that do exist: %v", err)
	}
```

with one that requires every shipped pack to be named:

```go
	for _, want := range []string{"reliability", "security"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name the %s pack, so it does not name the packs that do exist: %v", want, err)
		}
	}
```

- [ ] **Step 2: Run the tests to verify they pass**

Run: `go test -p 2 ./internal/cli/ -run 'TestPolicyPacks' -v`

Expected: PASS. All four `TestPolicyPacks*` tests are green; nothing in `internal/cli` needed a code change, because `runPolicyPacks` already iterates the registry.

- [ ] **Step 3: Prove the widened assertions can actually fail**

```bash
# Temporarily hide the security pack from the registry, confirm the CLI tests
# go red, then restore.
sed -i 's|^\t\t\tfile:    "packs/security.yaml",$|\t\t\tfile:    "packs/absent.yaml",|' internal/policypack/policypack.go
go test -p 2 ./internal/cli/ -run TestPolicyPacksListsWhatShips 2>&1 | tail -5
git checkout internal/policypack/policypack.go
go test -p 2 ./internal/cli/ -run TestPolicyPacks
```

Expected: the first run FAILS; the second PASSES. Confirm `git diff --stat internal/policypack/policypack.go` is empty before committing.

- [ ] **Step 4: Verify the whole tree**

```bash
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./...
git diff --stat main -- go.mod go.sum   # must be empty
```

- [ ] **Step 5: Commit**

```bash
git add internal/cli/policy_test.go
git commit -s -m "test(cli): assert the listing and the refusal name every shipped pack

Both tests used strings.Contains on one pack name, so both already
passed beside a second pack — and would still pass with the second pack
missing from the registry. That is not testing the listing."
```

---

## Task 6: Documentation

**Files:**
- Modify: `website/docs/features/policy-packs.md`
- Modify: `CHANGELOG.md` (under `## [Unreleased]`)
- Modify: `CLAUDE.md` (the post-1.0 curated-packs bullet)
- Modify: `website/docs/roadmap.md:559` (the post-1.0 row's curated-packs clause)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing.

**Do not** add a `(vX.Y.Z)` parenthetical to the CLAUDE.md bullet. That parenthetical is added **exclusively** by the later `release: vX.Y.Z` commit, never by a docs commit.

- [ ] **Step 1: Restructure `website/docs/features/policy-packs.md`**

The page is written for exactly one pack. Restructure it — this is not an append. Make these changes:

**a. The intro command block** (currently lines 8–12) gains the security pack and the blocking recipe:

```bash
kubeagent policy packs                       # list what ships
kubeagent policy packs --print reliability   # print one, to read or fork
kubeagent scan --policy-pack reliability     # evaluate it against a cluster
kubeagent scan --policy-pack security        # or the other one, or both

# Nothing in a pack is critical, so a pack cannot fail a gate by default.
# This is the explicit act that makes it block:
kubeagent gate --policy-pack security --fail-on warning
```

**b. The sample listing output** (currently lines 14–20) gains the second line:

```text
$ kubeagent policy packs
  reliability    14 rules — probes, resource requests and limits, replica counts, disruption budgets and image tags
  security       23 rules — privileged containers, host namespaces and paths, root filesystems, capabilities and service account tokens

Print one to fork it:
  kubeagent policy packs --print <name>
```

**c. The unknown-name refusal example** (currently lines 57–60) now names both packs:

```text
$ kubeagent policy packs --print nope
kubeagent: unknown policy pack "nope" (want reliability, security)
```

Run `go build -o /tmp/kubeagent-doccheck . && /tmp/kubeagent-doccheck policy packs && /tmp/kubeagent-doccheck policy packs --print nope` and paste the **actual** bytes, so the page cannot drift from the binary. Delete the temporary binary afterwards.

**d. "Opt-in, and what that buys"** (currently lines 76–88): change "Shipping the `reliability` pack inside the binary" to "Shipping a pack inside the binary", so the sentence holds for both.

**e. "No rule is critical"** (currently lines 90–98): widen it from the reliability pack to the whole registry, and give the security pack its escalation explicitly. Replace the section body with the following. **Note the four-backtick outer fence below is this plan's quoting device only** — what goes into the page is its contents, with the inner three-backtick `bash` block kept as a real fenced block:

````markdown
`gate` fails a build on a `critical` finding by default (`--fail-on
critical`). **No rule in any shipped pack is `critical`** — each pack's own
header comment says this is deliberate — so turning on `--policy-pack` in a
pipeline that passed yesterday cannot make it fail today. A test over the
whole registry keeps it that way, so it is a property of the pack format
rather than of the two packs that happen to ship.

Read that as "opt-in to blocking", not as "not meant to block." Raising
`--fail-on` is the explicit, separate act:

```bash
kubeagent gate --policy-pack security --fail-on warning
```

Every explicitly-bad-value rule in the `security` pack is a `warning`, so that
one flag makes the pack block a build. Every "field is unset" rule is `info`
and stays advisory even then.
````

**f. Rename "The fourteen rules"** (currently line 100) to `## The reliability pack — fourteen rules`, leaving its table and its lead-in sentence untouched.

**g. Add a new `## The security pack — twenty-three rules` section** immediately after the reliability table, before "Two semantics a rule author must know":

```markdown
## The security pack — twenty-three rules

Paths are shortened below: `T` is `spec.template.spec`, except for
`security.cronjob-privileged`, whose pod template lives one level deeper at
`spec.jobTemplate.spec.template.spec`.

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
| `security.cronjob-privileged` | CronJob | `containers[*].securityContext.privileged` notIn `true` | warning |

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
2. **Hardening set at *pod* level does not satisfy a *container*-level rule.**
   The grammar has no OR, so a Deployment that sets `runAsNonRoot` once in
   `spec.template.spec.securityContext` still reports
   `security.deploy-run-as-non-root-unset` for each container. This is the
   pack's most likely false positive. If your workloads harden at pod level,
   fork the pack and move those paths.
3. **`capabilities.drop` cannot be required to include `ALL`.** That needs an
   existential quantifier — "some element equals ALL" — and `[*]` is
   universally quantified with no existential counterpart. The pack checks
   what was *added* instead, against a fixed list of seven host-level
   capabilities.
4. **RBAC bindings, service account objects and Secrets are unreachable.**
   None is a kind a policy may select. A workload's *reference* to a service
   account is reachable, and `security.deploy-service-account-unset` is that
   rule; the object it names is not. `Secret` is absent deliberately — a
   violation carries evidence, and evidence drawn from a Secret would be
   secret material rendered into a report, a JSON document and a SARIF upload.
5. **The added-capability list is curated, not exhaustive.** A capability
   outside the seven passes. Fork the pack to extend it.

A registry allowlist is also not a rule kubeagent can curate: it does not know
which registry is yours, and a shipped rule naming one would be wrong for
everyone else. `--print` and forking are the answer.
```

**h. "RBAC"** (currently lines 149–165): widen it. Replace "The kinds the fourteen `reliability` rules select (`Deployment`, `StatefulSet`, `DaemonSet`, `CronJob`, `PersistentVolumeClaim`)" with "The kinds the shipped rules select (`Deployment`, `StatefulSet`, `DaemonSet`, `CronJob`, `PersistentVolumeClaim`)", change "Turning on `--policy-pack reliability`" to "Turning on `--policy-pack`", and replace the request-volume paragraph's count sentence with one that covers both packs:

```markdown
It does add request volume, though: evaluating any policy — a pack included —
builds its own dynamic client and lists every kind the loaded rules touch,
independently of whatever `scan`'s typed collectors already read. For
`reliability` that is six `List` calls, one each for the five kinds its rules
select plus `PodDisruptionBudget` for the one relation rule. For `security` it
is four, one per workload kind, since it has no relation rule. That extra,
uncached read is how `--policy` has always evaluated a rule set; a pack does
not change it. See [Least-privilege RBAC](rbac.md).
```

**i. "Two semantics a rule author must know"** (currently lines 122–147): change "every one of the fourteen `reliability` rules is written with them in mind" to "every rule in both shipped packs is written with them in mind". The rest of that section stays as it is.

**j. "Not in this slice"** (currently lines 191–204): **narrow** the first bullet; do not delete it. Retraction is not deletion. Replace:

```markdown
- **Security and cost packs.** `reliability` is the first pack; the registry
  has room for more, but this slice ships exactly one.
```

with:

```markdown
- **A cost pack.** `reliability` and `security` ship; a cost pack does not.
  Most cost claims are thresholds, and a threshold is cluster-specific, so
  picking a curated default is a decision of its own rather than a third
  transcription of this one.
```

The other three bullets in that section stay exactly as they are.

- [ ] **Step 2: Build the docs**

```bash
cd /home/ubuntu/git/kubeagent/website
/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/mkdocs build --strict -f mkdocs.yml
cd /home/ubuntu/git/kubeagent
```

Expected: exit 0, "Documentation built", and no `WARNING` line naming `features/policy-packs.md`. The red "Material for MkDocs 2.0" banner is cosmetic. **Return to the repository root afterwards** — the shell's working directory persists between commands.

- [ ] **Step 3: Add the CHANGELOG entry**

Under `## [Unreleased]` in `CHANGELOG.md`, add:

```markdown
### Added

- A second curated policy pack, `security`: twenty-three rules over workload
  pod templates covering privileged containers, privilege escalation, running
  as root, writable root filesystems, added Linux capabilities, `hostPath`
  volumes, host ports, the three host namespaces, seccomp profiles and service
  account token mounting. Run it with `kubeagent scan --policy-pack security`
  or `kubeagent gate --policy-pack security`, print it with `kubeagent policy
  packs --print security`, and combine it with `--policy` or with the
  `reliability` pack. Four properties that are unsafe both when unset and when
  set wrong ship as a pair of rules — an `info` rule for the unset case and a
  `warning` rule for the explicit one — because every operator except `exists`
  and `notExists` skips an absent field, so one rule could only ever catch one
  of the two. No rule is `critical`, so adding the pack to a pipeline that
  passed yesterday cannot fail it today; `--fail-on warning` is the explicit
  act that makes it block. Nothing versioned moves: `scan` stays at schema
  version 1.2 and `gate` at 1.1, the evaluator is unchanged, and the pack needs
  no RBAC grant a plain `scan` did not already have.
```

- [ ] **Step 4: Update `CLAUDE.md`**

In the post-1.0 curated-packs bullet, change the pack description to cover both packs and narrow the remaining-work sentence. Replace the sentence

> `kubeagent policy packs`: a kubeagent-curated `reliability` pack of fourteen rules, compiled into `internal/policypack` and evaluated by the existing `--policy` engine via `scan --policy-pack`/`gate --policy-pack`.

with

> `kubeagent policy packs`: a kubeagent-curated `reliability` pack of fourteen rules and, since slice 2, a `security` pack of twenty-three rules over workload pod templates, both compiled into `internal/policypack` and evaluated by the existing `--policy` engine via `scan --policy-pack`/`gate --policy-pack`. The `security` pack pairs an `info` "field unset" rule with a `warning` "field set wrong" rule for the four properties that are unsafe either way, because every operator except `exists` and `notExists` skips an absent slot; where absence is the safe default a single value rule ships.

and, in the same bullet's closing sentence, narrow

> The remaining post-1.0 work is the rest of the curated-packs item's second half — security and cost packs, and a pack contributed by someone other than kubeagent itself — plus other baseline dimensions.

to

> The remaining post-1.0 work is the rest of the curated-packs item's second half — a cost pack, and a pack contributed by someone other than kubeagent itself — plus other baseline dimensions.

Add **no** `(vX.Y.Z)` parenthetical.

- [ ] **Step 5: Update `website/docs/roadmap.md`**

In the post-1.0 row (line 559), the curated-packs clause currently reads:

> **curated policy packs slice 1 shipped** (`kubeagent policy packs` — a kubeagent-curated `reliability` pack of fourteen rules, compiled into the binary and evaluated by the existing `--policy` engine via `scan --policy-pack`/`gate --policy-pack`; opt-in, no rule `critical`, no `schemaVersion` move), the second half of this item's first form — plus other baseline dimensions, more packs, and community contribution at run time, still ahead

Replace it with:

> **curated policy packs slices 1 and 2 shipped** (`kubeagent policy packs` — a kubeagent-curated `reliability` pack of fourteen rules and a `security` pack of twenty-three rules over workload pod templates, compiled into the binary and evaluated by the existing `--policy` engine via `scan --policy-pack`/`gate --policy-pack`; opt-in, no rule `critical`, no `schemaVersion` move, and no RBAC grant a plain `scan` did not already have), the second half of this item's first form — plus other baseline dimensions, a cost pack, and community contribution at run time, still ahead

- [ ] **Step 6: Verify nothing outside docs moved**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 -count=1 ./...
git diff --stat main -- go.mod go.sum                              # must be empty
git diff --stat main -- internal/report/testdata/golden-scan.txt   # must be empty
git diff --stat main -- website/docs/quickstart.md                 # must be empty
git status --short   # no stray /tmp/kubeagent-doccheck, no untracked files
```

- [ ] **Step 7: Commit**

```bash
git add website/docs/features/policy-packs.md CHANGELOG.md CLAUDE.md website/docs/roadmap.md
git commit -s -m "docs: the security pack

The policy-packs page was written for exactly one pack, so this
restructures it rather than appending: per-pack rule tables, a
no-critical section that is about the registry rather than about
reliability, and an RBAC section that names both packs' List counts.

The security pack's five real gaps are written down rather than worked
around — a bare Pod is unchecked, pod-level hardening does not satisfy a
container-level rule, capabilities.drop cannot be required to include
ALL, RBAC bindings and Secrets are unreachable, and the added-capability
list is curated rather than exhaustive. Each comes from a property the
grammar does not have.

The 'not in this slice' bullet naming security and cost packs is
narrowed to the cost pack rather than deleted."
```

---

## Verification Before the Whole-Branch Review

```bash
cd /home/ubuntu/git/kubeagent
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && gofmt -l internal/     # gofmt prints nothing
go test -p 2 -count=1 ./...                              # every package ok
bash scripts/dco-check.sh main HEAD                      # every commit signed off
git diff --stat main -- go.mod go.sum                    # empty
git diff --stat main -- internal/report/testdata/golden-scan.txt   # empty
git diff --stat main -- website/docs/schemas/            # empty — no schema moved
git diff --stat main -- internal/policy/                 # empty — no engine change
git diff --name-only main                                # exactly the eight files in File Structure
```

The whole-branch review runs on the most capable model. Point it at the spec's §8 (the two separate promises), §10 (nothing versioned moves) and §14 (the five accepted limits) as the properties to check the branch against.

## Self-Review

**Spec coverage.** §1 the pack and its four commands → Tasks 1, 2, 6. §3 the grammar constraints → encoded in the rule set (Tasks 1, 2) and stated in Task 6's "What the security pack cannot say". §4 the pairing principle → Tasks 1 and 4, and Task 6's "Why four properties get two rules each". §5 no `critical` and the blocking recipe → the inherited `TestNoPackRuleIsCritical`, Task 4's level assertions, and Task 6 steps 1a and 1e. §6 kind scope → Task 2's `TestSecurityPackKindDistribution`. §7 the 23 rules and their messages → Tasks 1 and 2, verbatim. §8 the two promises → the pack header (Task 1) and the page's existing Guarantees section, which already states them and needs no edit. §9 RBAC → Task 6 step 1h. §10 nothing versioned moves → the per-task and pre-review verification blocks. §11 tests → Tasks 3, 4, 5. §12 docs → Task 6. §13/§14 → Task 6 steps 1g and 1j.

**Placeholder scan.** No "TBD", no "add a test for X", no "similar to Task N". Every code step carries the actual content. The two `sed` mutation steps (Tasks 3 and 4) and the registry mutation (Task 5) each name the exact command and the exact `git checkout` that undoes it.

**Type consistency.** `hardenedContainer`, `containerMinus`, `containerWithSecurityContext`, `containerWithAddedCapability`, `containerWithHostPort`, `hardenedPodSpec`, `podSpecWith`, `podSpecWithout`, `podSpecWithSeccomp`, `podSpecWithHostPathVolume`, `hardenedWorkload`, `hardenedDeployment`, `hardenedCronJob`, `evaluateOne` and `securityRule` are defined once in Task 3 and used with the same signatures in Task 4. None collides with a name already in `internal/policypack/rules_test.go`. `ruleCase` is reused from that file rather than redeclared. The rule ids in Tasks 1, 2, 3, 4 and 6 are the same twenty-three strings, and the pack's rule count is `23` everywhere it appears (Task 2's distribution test, Task 5's listing assertion, Task 6's heading, table and CHANGELOG).
