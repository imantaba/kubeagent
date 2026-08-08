# Cost policy pack Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a third kubeagent-curated policy pack, `cost` — sixteen rules, every one at `info`, over seven kinds — evaluated by the existing `--policy` engine via `scan --policy-pack cost` and `gate --policy-pack cost`.

**Architecture:** A YAML file compiled into the stdlib-only `internal/policypack` by the existing `go:embed`, plus one registry entry. No engine change: `internal/policy` is not touched by any task. Four generic pack tests are inherited the moment the registry entry lands; four cost-specific tests are added on top.

**Tech Stack:** Go 1.26, `go:embed`, `sigs.k8s.io/yaml` (already a dependency, used by `internal/policy`), `k8s.io/apimachinery` unstructured objects in tests.

**Source of truth:** `docs/superpowers/specs/2026-08-08-policy-packs-cost-design.md`. Its rule table and its sixteen messages are copied byte-for-byte below; do not re-derive or reword either.

## Global Constraints

Every task's requirements implicitly include this section.

- **READ-ONLY toward the cluster:** `get`/`list` only, no `--fix` path from a rule. **Separately and additionally: no LLM call.** Two promises, never blurred into one. No comment, doc line, help string or commit message may suggest a pack is related to `--explain`, which is the model path.
- **A third promise, new to this pack: kubeagent has no prices.** No billing data, no instance types, no node cost, no cloud API. No rule, message, comment or doc line may imply the pack knows what anything costs.
- **NO ENGINE CHANGE.** `internal/policy` is not touched by any task. No new operator, no new relation, no new selectable kind. If a rule seems to need one, the rule is wrong, not the engine.
- **`internal/policypack` STAYS STDLIB-ONLY** (`embed` + `sort`) and imports nothing from kubeagent. `internal/policypack/imports_test.go` enforces both halves and must not be edited.
- **NO NEW DEPENDENCY:** `go.mod` and `go.sum` must not change. Verify with `git diff --stat main -- go.mod go.sum` at the end of every task: must be empty.
- **NO SCHEMA MOVES:** `scan` stays 1.2, `gate` stays 1.1, and the other six do not move. **Never run any test with `-update`** in this slice, in any task, for any reason.
- `internal/report/testdata/golden-scan.txt` must stay **byte-identical** — the golden scan runs no policy, so it cannot move. Do **not** regenerate the demo GIF or `website/docs/quickstart.md`.
- **NO `critical` RULE.** `TestNoPackRuleIsCritical` is generic over `policypack.All()` and must not be edited or weakened. In this pack every rule is `info` — there is no `warning` rule either.
- **NO DOT IN ANY RULE MESSAGE**, and no `://` and no dotted quad anywhere in the YAML. The sixteen messages below are already dot-free — use them verbatim rather than rewording. Thresholds are spelled as words (*eight*, *thirty-two gibibytes*) so no decimal point can creep in.
- **CREDENTIALS:** no secrets, credentials, private IPs or internal hostnames anywhere — code, tests, fixtures, docs, help text. Documentation IPs are RFC 5737; example domains RFC 2606 (`example.com`/`.org`/`.net`). URLs are credentials — nothing beyond `scheme://host`, and the project's own `https://k8sproject.top` links are the only permitted host. **A curated rule must never name a registry hostname** — that is exactly what `--print` and forking are for.
- **TDD:** write the failing test first, watch it fail, then implement. `go test` runs with `-p 2` locally, **never `-short`**.
- Every commit needs `git commit -s` (DCO enforced on `main`), authored solely by the repository owner — **no `Co-Authored-By` and no AI attribution of any kind.**
- **DANGER: never run `./chaos/run.sh` in any form** — it takes ~40 minutes and injects real outages. No cluster is needed for this slice. Do not create, delete or touch any cluster.

## File Structure

```
internal/policypack/packs/cost.yaml       (new)   16 rules + header comment
internal/policypack/policypack.go         (edit)  one registry entry, FIRST in the slice
internal/policypack/cost_rules_test.go    (new)   fixtures + 4 cost-specific tests
internal/policypack/security_rules_test.go (edit) one rename: securityRule -> packRule
internal/cli/policy_test.go               (edit)  widen 2 assertions
website/docs/features/policy-packs.md     (edit)  restructure for three packs
CHANGELOG.md                              (edit)  [Unreleased] ### Added
CLAUDE.md                                 (edit)  curated-packs bullet
website/docs/roadmap.md                   (edit)  curated-packs item
```

**Not touched by any task:** `internal/policy/**`, `internal/policypack/packs_test.go`, `internal/policypack/policypack_test.go`, `internal/policypack/imports_test.go`, `internal/policypack/rules_test.go`, `internal/cli/policy.go`, `go.mod`, `go.sum`, anything under `deploy/`, `internal/report/testdata/`.

## Branch

Cut `policy-pack-cost` off `main` at the spec commit as the first step of Task 1.

---

## Reference: the sixteen rules

`T` is shorthand for `spec.template.spec` in this table only — the YAML spells every path in full. CronJob container paths live one level deeper, at `spec.jobTemplate.spec.template.spec`.

Every rule is `level: info`.

| id | kind | path | op | values |
| --- | --- | --- | --- | --- |
| `cost.deploy-ephemeral-storage-limit` | Deployment | `T.containers[*].resources.limits.ephemeral-storage` | exists | — |
| `cost.deploy-large-cpu-request` | Deployment | `T.containers[*].resources.requests.cpu` | lte | `"8"` |
| `cost.deploy-large-memory-request` | Deployment | `T.containers[*].resources.requests.memory` | lte | `"32Gi"` |
| `cost.statefulset-cpu-request` | StatefulSet | `T.containers[*].resources.requests.cpu` | exists | — |
| `cost.statefulset-memory-request` | StatefulSet | `T.containers[*].resources.requests.memory` | exists | — |
| `cost.daemonset-cpu-request` | DaemonSet | `T.containers[*].resources.requests.cpu` | exists | — |
| `cost.daemonset-memory-request` | DaemonSet | `T.containers[*].resources.requests.memory` | exists | — |
| `cost.daemonset-ephemeral-storage-limit` | DaemonSet | `T.containers[*].resources.limits.ephemeral-storage` | exists | — |
| `cost.cronjob-cpu-request` | CronJob | `spec.jobTemplate.spec.template.spec.containers[*].resources.requests.cpu` | exists | — |
| `cost.cronjob-memory-request` | CronJob | `spec.jobTemplate.spec.template.spec.containers[*].resources.requests.memory` | exists | — |
| `cost.cronjob-successful-history` | CronJob | `spec.successfulJobsHistoryLimit` | lte | `"10"` |
| `cost.cronjob-failed-history` | CronJob | `spec.failedJobsHistoryLimit` | lte | `"10"` |
| `cost.cronjob-active-deadline` | CronJob | `spec.jobTemplate.spec.activeDeadlineSeconds` | exists | — |
| `cost.job-backoff-limit` | Job | `spec.backoffLimit` | lte | `"10"` |
| `cost.hpa-max-replicas` | HorizontalPodAutoscaler | `spec.maxReplicas` | lte | `"50"` |
| `cost.pvc-large-storage` | PersistentVolumeClaim | `spec.resources.requests.storage` | lte | `"1Ti"` |

Kind distribution: Deployment 3, StatefulSet 2, DaemonSet 3, CronJob 5, Job 1, HorizontalPodAutoscaler 1, PersistentVolumeClaim 1. Total 16.
Level distribution: 16 `info`, 0 `warning`, 0 `critical`.

Nine rules use `exists`; seven use `lte`. There are no other operators in this pack.

---

## Reference: two facts that shape every task

**1. `lte` compares quantities numerically, not lexically.** `internal/policy/op.go`'s `compareNumeric` tries `strconv.ParseInt`, then `strconv.ParseFloat`, then `resource.ParseQuantity` with `Cmp`. So `16` correctly exceeds `8`, and `512Mi` is correctly below `32Gi`. This is why the pack may ship thresholds at all, and Task 3 machine-checks it.

**2. Every operator except `exists`/`notExists` SKIPS an absent slot** (`op.go:30-38` returns `(ok=false, skip=true)`). Two consequences the implementer must hold on to:

- The pack ships **zero paired rules** and that is correct rather than incomplete. A threshold fires only on a value someone wrote; where the API defines a default (`successfulJobsHistoryLimit` 3, `failedJobsHistoryLimit` 1, `backoffLimit` 6) absence *is* the safe value.
- **In a test, an absent field is not a violation — it is a skip, which is not a pass either.** Every threshold case must set the field **explicitly** to a value past the threshold. A case that merely omits the field proves nothing and is a defect.

---

### Task 1: The pack YAML and its registry entry

**Files:**
- Create: `internal/policypack/packs/cost.yaml`
- Modify: `internal/policypack/policypack.go` (the `packs` registry slice, around line 48)

**Interfaces:**
- Consumes: `type Pack struct { Name, Summary string; file string }` and the `//go:embed packs/*.yaml` directive, both already in `internal/policypack/policypack.go`. `Pack` stores **no rule count** — a stdlib-only package has no YAML decoder, so the count is computed by callers that load the pack.
- Produces: the name `"cost"` resolvable through `policypack.Lookup`, `policypack.Bytes`, `policypack.All` and `policypack.Names`. Later tasks load it with `loadPack(t, "cost")`.

- [ ] **Step 1: Cut the branch**

```bash
export PATH=$PATH:/usr/local/go/bin
cd /home/ubuntu/git/kubeagent
git checkout main
git checkout -b policy-pack-cost
git status --short   # must be empty
```

- [ ] **Step 2: Record that `cost` does not resolve yet**

The four pack-level tests in `internal/policypack/packs_test.go` are generic over `policypack.All()`, so there is nothing to write to make them cover `cost` — they cover it the moment the registry entry lands, as a subtest that does not exist until then. So this task's failing-first evidence is not a red test but the CLI refusing the name:

```bash
go run . policy packs --print cost ; echo "exit=$?"
```

Expected **before** Steps 3 and 4: a refusal naming `"cost"` as unknown and listing `reliability` and `security`, at a non-zero exit. Paste that output into your report — it is what Step 5 turns green.

**Do not add a temporary test to manufacture a red run.** A test written only to be deleted is noise in the diff, and Step 5 is the real gate.

- [ ] **Step 3: Write the pack file**

Create `internal/policypack/packs/cost.yaml`. Start with this header comment **verbatim** — it is the pack's own statement of what it is and what it cannot do, in the same voice as `reliability.yaml` and `security.yaml`:

```yaml
# kubeagent cost pack.
#
# Every rule here is about a workload's claim on the cluster: what it reserves,
# what it may grow to, and what it leaves behind. A cluster is provisioned for
# the sum of its requests, so an unset request and an oversized one are both
# money, in opposite directions.
#
# kubeagent has no prices. There is no billing data here, no instance types, no
# node cost and no cloud API. The pack names shapes that usually cost money; it
# cannot tell you what anything costs, and no rule here claims to.
#
# Every rule is `info` — not `warning`, which the other two packs use. A cost
# finding is budget-dependent in a way a security finding is not: "privileged is
# bad" holds in every cluster, "sixteen CPUs is too many" does not. So this pack
# cannot fail a gate at any --fail-on above info. A pack that cannot know must
# not accuse.
#
# The thresholds are set as outliers, generously enough that an ordinary
# workload never trips one, and every threshold message asks you to confirm the
# size is deliberate rather than calling it waste. An operator who disagrees
# with a number runs `kubeagent policy packs --print cost > cost.yaml`, edits
# it, and passes --policy cost.yaml.
#
# There are no paired rules here, unlike the security pack. Every operator
# except `exists` and `notExists` skips an absent field, which is exactly what
# a threshold wants: a workload that sets no CPU request is never accused of
# setting a large one, and where the API defines a default — three successful
# runs kept, one failed run kept, six retries — absence is already the safe
# value.
#
# What this pack does not say, because the rule grammar has no cross-field
# relation: a container that sets a limit and no request has its request
# defaulted to the limit, reserving the ceiling rather than the expected use.
# That is probably the largest single cost defect in a typical cluster and no
# rule here can name it. A Deployment with no CPU or memory request is also
# unsaid when this pack runs alone — that `exists` question belongs to the
# reliability pack, deliberately, so the two do not duplicate.
#
# Rule ids are namespaced with the pack name so they cannot collide with an
# operator's own rules when both are given.
```

Then the sixteen rules, in the table's order. Each has this exact shape — here is the first, in full, as the template for the other fifteen:

```yaml
- id: cost.deploy-ephemeral-storage-limit
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].resources.limits.ephemeral-storage
    op: exists
  level: info
  message: a container has no ephemeral-storage limit, so local disk use is bounded only by the node
```

A threshold rule adds a `values` list of exactly one entry, quoted so YAML does not read it as a number:

```yaml
- id: cost.deploy-large-cpu-request
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].resources.requests.cpu
    op: lte
    values:
      - "8"
  level: info
  message: a container reserves more than eight CPUs, so confirm the size is deliberate
```

**The remaining fourteen, with their exact messages.** Use the id, kind, path, op and values from the reference table above, and these messages **verbatim** — every one is dot-free, which `TestPackCarriesNoHostOrAddress` requires:

```
cost.deploy-large-memory-request
  a container reserves more than thirty-two gibibytes of memory, so confirm the size is deliberate

cost.statefulset-cpu-request
  a container has no CPU request, so the scheduler cannot size it and the cluster is provisioned for a guess

cost.statefulset-memory-request
  a container has no memory request, so the scheduler cannot size it and the cluster is provisioned for a guess

cost.daemonset-cpu-request
  a container has no CPU request, and this workload is scheduled once per node

cost.daemonset-memory-request
  a container has no memory request, and this workload is scheduled once per node

cost.daemonset-ephemeral-storage-limit
  a container has no ephemeral-storage limit, and this workload writes on every node

cost.cronjob-cpu-request
  a container has no CPU request, so every run is placed as if it were free

cost.cronjob-memory-request
  a container has no memory request, so every run is placed as if it were free

cost.cronjob-successful-history
  the CronJob keeps more than ten successful runs, so completed Jobs and their pods accumulate

cost.cronjob-failed-history
  the CronJob keeps more than ten failed runs, so completed Jobs and their pods accumulate

cost.cronjob-active-deadline
  the job template sets no activeDeadlineSeconds, so a run that hangs occupies its request until something else stops it

cost.job-backoff-limit
  the Job retries more than ten times, so a run that cannot succeed still consumes its request on every attempt

cost.hpa-max-replicas
  the autoscaler may reach more than fifty replicas, so confirm the ceiling is deliberate

cost.pvc-large-storage
  the claim reserves more than one tebibyte, so confirm the size is deliberate
```

The two ephemeral-storage paths contain a hyphen: `resources.limits.ephemeral-storage`. That parses correctly — `internal/policy/path.go`'s `parsePath` reads a plain segment to the next `.` or `[`, so a hyphen is an ordinary byte. Do not quote or bracket it.

- [ ] **Step 4: Add the registry entry — FIRST in the slice, not appended**

In `internal/policypack/policypack.go`, the `packs` slice is documented as "the registry, in name order" and `TestAllIsSortedByName` checks it. `cost` sorts **before** `reliability` and `security`, so the new entry goes at the **top** of the slice:

```go
// packs is the registry, in name order.
var packs = []Pack{
	{
		Name:    "cost",
		Summary: "resource requests and limits, retention and history limits, autoscaler ceilings and claim sizes",
		file:    "packs/cost.yaml",
	},
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

Do not touch the `reliability` or `security` entries.

- [ ] **Step 5: Run the whole package and watch the four generic tests now cover cost**

```bash
go test ./internal/policypack -v 2>&1 | grep -E "^(=== RUN|--- (PASS|FAIL)|ok|FAIL)" | head -40
```

Expected: `TestEveryPackLoads/cost`, `TestRuleIDsCarryTheirPackPrefix/cost`, `TestNoPackRuleIsCritical/cost` and `TestPackCarriesNoHostOrAddress/cost` all appear and all PASS. If `TestPackCarriesNoHostOrAddress/cost` fails, a message contains a dot — find it and fix the message rather than the test.

**Expected state at the end of this task:** the four inherited tests are green and there is **no per-rule coverage yet**. That is the planned state, not a gap to fix early — Tasks 2 and 3 add it.

- [ ] **Step 6: Verify the untouchables and commit**

```bash
git diff --stat main -- go.mod go.sum internal/policy internal/report/testdata   # must be empty
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./internal/policypack ./internal/policy ./internal/cli
git add internal/policypack/packs/cost.yaml internal/policypack/policypack.go
git commit -s -m "policypack: the cost pack, sixteen rules at info

Nine exists rules and seven lte thresholds over seven kinds: what a workload
reserves, what it may grow to, and what it leaves behind.

Every rule is info rather than warning, so the pack cannot fail a gate at any
--fail-on above info. A cost finding is budget-dependent in a way a security
finding is not, and a pack that cannot know must not accuse. The thresholds
are outliers and their messages ask whether the size is deliberate.

No paired rules, unlike the security pack: every operator except exists and
notExists skips an absent slot, so a threshold fires only on a value someone
wrote, and where the API defines a default absence is already the safe value.

kubeagent has no prices — no billing data, no instance types, no node cost.
The pack names shapes that usually cost money and claims nothing more."
```

---

### Task 2: Fixtures, the kind-distribution test, and the nine `exists` cases

**Files:**
- Create: `internal/policypack/cost_rules_test.go`
- Modify: `internal/policypack/security_rules_test.go` (one rename, described in Step 1)

**Interfaces:**
- Consumes, from `internal/policypack/rules_test.go` (same test package `policypack_test`, already in the tree — do **not** redefine these):
  - `const fixtureNamespace = "app"` and `const fixtureImage = "registry.example.com/team/app:1.0"`
  - `func workload(kind, name string, c map[string]any, replicas int64) *unstructured.Unstructured` — builds `apps/v1` `spec.template.spec.containers[c]` with `spec.replicas`
  - `func deployment(name string, c map[string]any) *unstructured.Unstructured` — `workload("Deployment", name, c, 2)`
  - `type ruleCase struct { id, kind string; violating, satisfying *unstructured.Unstructured; support map[string][]*unstructured.Unstructured }`
- Consumes, from `internal/policypack/packs_test.go`: `func loadPack(t *testing.T, name string) []policy.Rule`
- Consumes, from `internal/policypack/security_rules_test.go`: `func evaluateOne(t *testing.T, r policy.Rule, kind string, obj *unstructured.Unstructured) []policy.Violation`
- Consumes, from `internal/policy`: `policy.Rule` with fields `ID string`, `Match Match` (field `Kind string`), `Assert Assert` (fields `Path string`, `Op Op`, `Values []string`), `Level Level`, `Message string`; constants `policy.LevelInfo`, `policy.LevelWarning`, `policy.LevelCritical`, `policy.OpExists`, `policy.OpLte`.
- Produces, for Tasks 3 and 4: `packRule`, `sizedContainer`, `containerWithoutResource`, `containerRequesting`, `cronJobFrom`/`goodCronJob`/`cronJobWithContainer`/`cronJobWithHistory`/`cronJobWithoutDeadline`, `costJob`, `hpa`, `claim`. There is deliberately no limits-side container helper: every `lte` rule in the pack targets `requests` or a bare numeric field, and the two rules that read `resources.limits` use `exists`, which `containerWithoutResource` already serves.

- [ ] **Step 1: Rename `securityRule` to `packRule`**

`internal/policypack/security_rules_test.go` defines a helper that finds a rule by id in a slice. It is not security-specific — the cost tests need the identical thing, and a copy-pasted `costRule` twin would be verbatim duplication of a logic block. Rename it instead, at its definition (around line 240) and at its three call sites:

```go
// packRule finds one rule in a pack by id.
func packRule(t *testing.T, rules []policy.Rule, id string) policy.Rule {
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

Nothing else in that file changes: `hardenedContainer`, `hardenedDeployment`, `hardenedCronJob`, `evaluateOne` and every security case stay exactly as they are.

```bash
grep -rn "securityRule" internal/    # must return nothing when the rename is done
go test -p 2 -count=1 ./internal/policypack
```

- [ ] **Step 2: Write the failing kind-distribution test**

Create `internal/policypack/cost_rules_test.go` starting with:

```go
package policypack_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/policy"
)

// TestCostPackKindDistribution pins the pack's scope decision: three workload
// kinds carry resource rules, CronJob carries five because it has both a pod
// template and its own retention knobs, and three more kinds are here because
// each one is a direct claim on spend — a Job's retry budget, an autoscaler's
// ceiling, a claim's size. It also pins the level split, which is the pack's
// central promise: every rule is info, so no --fail-on above info can be
// failed by adding this pack to a pipeline.
func TestCostPackKindDistribution(t *testing.T) {
	rules := loadPack(t, "cost")

	byKind := map[string]int{}
	byLevel := map[policy.Level]int{}
	for _, r := range rules {
		byKind[r.Match.Kind]++
		byLevel[r.Level]++
	}

	wantKind := map[string]int{
		"Deployment":              3,
		"StatefulSet":             2,
		"DaemonSet":               3,
		"CronJob":                 5,
		"Job":                     1,
		"HorizontalPodAutoscaler": 1,
		"PersistentVolumeClaim":   1,
	}
	for kind, n := range wantKind {
		if byKind[kind] != n {
			t.Errorf("%d rules select %s, want %d", byKind[kind], kind, n)
		}
	}
	for kind := range byKind {
		if _, ok := wantKind[kind]; !ok {
			t.Errorf("the pack selects %s, which is not one of the seven kinds it is scoped to", kind)
		}
	}

	if len(rules) != 16 {
		t.Errorf("the pack holds %d rules, want 16", len(rules))
	}

	if byLevel[policy.LevelInfo] != 16 {
		t.Errorf("%d rules are info, want 16", byLevel[policy.LevelInfo])
	}
	if n := byLevel[policy.LevelWarning] + byLevel[policy.LevelCritical]; n != 0 {
		t.Errorf("%d rules are above info — the cost pack must not be able to fail a gate above --fail-on info", n)
	}
}
```

- [ ] **Step 3: Run it and watch it fail**

```bash
go test ./internal/policypack -run TestCostPackKindDistribution -v
```

Expected: PASS, because Task 1 already landed the sixteen rules. **If it fails, Task 1's YAML is wrong** — fix the YAML, not this test. Record the result either way; this test is the check on Task 1, so a pass here is the evidence, not a missing failing-first step.

- [ ] **Step 4: Write the fixtures**

Append to `internal/policypack/cost_rules_test.go`. These are the cost pack's own fixtures: `goodContainer()` in `rules_test.go` sets no `ephemeral-storage` limit, so it would violate two cost rules and cannot be reused.

```go
// sizedContainer satisfies every container-level rule in the cost pack: it
// requests a modest amount of CPU and memory and bounds its local disk. Each
// case below starts from it and changes or removes exactly the one thing its
// rule is about, so a case can only fail for its own reason.
func sizedContainer() map[string]any {
	return map[string]any{
		"name":  "app",
		"image": fixtureImage,
		"resources": map[string]any{
			"limits":   map[string]any{"memory": "512Mi", "ephemeral-storage": "1Gi"},
			"requests": map[string]any{"cpu": "100m", "memory": "256Mi"},
		},
	}
}

// containerWithoutResource returns a sized container with one resources entry
// removed: containerWithoutResource(t, "requests", "cpu").
func containerWithoutResource(t *testing.T, group, key string) map[string]any {
	t.Helper()
	c := sizedContainer()
	res, ok := c["resources"].(map[string]any)
	if !ok {
		t.Fatal("the sized container has no resources map")
	}
	g, ok := res[group].(map[string]any)
	if !ok {
		t.Fatalf("the sized container has no resources.%s map", group)
	}
	delete(g, key)
	return c
}

// containerRequesting returns a sized container asking for a different amount
// of one resource. Setting the field explicitly is the whole point: every
// operator except exists and notExists SKIPS an absent slot, so a fixture that
// merely omitted the field would make a threshold rule say nothing, which is
// not a pass.
func containerRequesting(t *testing.T, key, value string) map[string]any {
	t.Helper()
	c := sizedContainer()
	res, ok := c["resources"].(map[string]any)
	if !ok {
		t.Fatal("the sized container has no resources map")
	}
	g, ok := res["requests"].(map[string]any)
	if !ok {
		t.Fatal("the sized container has no resources.requests map")
	}
	g[key] = value
	return c
}

// cronJobKnobs is the four things the five CronJob rules read. goodCronJobSpec
// fills every one with a safe value and each case changes exactly one; a nil
// history or deadline leaves that field ABSENT, which is what the
// activeDeadlineSeconds case needs and what every threshold case must avoid.
type cronJobKnobs struct {
	container             map[string]any
	successfulHistory     any
	failedHistory         any
	activeDeadlineSeconds any
}

func goodCronJobSpec() cronJobKnobs {
	return cronJobKnobs{
		container:             sizedContainer(),
		successfulHistory:     int64(3),
		failedHistory:         int64(1),
		activeDeadlineSeconds: int64(600),
	}
}

// cronJobFrom wraps the knobs in the batch/v1 shape, whose pod template lives
// one level deeper than a Deployment's: spec.jobTemplate.spec.template.spec.
func cronJobFrom(name string, k cronJobKnobs) *unstructured.Unstructured {
	jobSpec := map[string]any{
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
			"spec":     map[string]any{"containers": []any{k.container}},
		},
	}
	if k.activeDeadlineSeconds != nil {
		jobSpec["activeDeadlineSeconds"] = k.activeDeadlineSeconds
	}
	spec := map[string]any{
		"schedule":    "*/5 * * * *",
		"jobTemplate": map[string]any{"spec": jobSpec},
	}
	if k.successfulHistory != nil {
		spec["successfulJobsHistoryLimit"] = k.successfulHistory
	}
	if k.failedHistory != nil {
		spec["failedJobsHistoryLimit"] = k.failedHistory
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec":       spec,
	}}
}

func goodCronJob(name string) *unstructured.Unstructured {
	return cronJobFrom(name, goodCronJobSpec())
}

func cronJobWithContainer(name string, c map[string]any) *unstructured.Unstructured {
	k := goodCronJobSpec()
	k.container = c
	return cronJobFrom(name, k)
}

func cronJobWithHistory(name string, successful, failed any) *unstructured.Unstructured {
	k := goodCronJobSpec()
	k.successfulHistory, k.failedHistory = successful, failed
	return cronJobFrom(name, k)
}

func cronJobWithoutDeadline(name string) *unstructured.Unstructured {
	k := goodCronJobSpec()
	k.activeDeadlineSeconds = nil
	return cronJobFrom(name, k)
}

// costJob builds a batch/v1 Job with an explicit backoffLimit. The field is
// always set: absent, lte would skip, and the API's own default of six is
// already within the threshold anyway.
func costJob(name string, backoffLimit int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"backoffLimit": backoffLimit,
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
				"spec":     map[string]any{"containers": []any{sizedContainer()}},
			},
		},
	}}
}

// hpa builds an autoscaling/v2 HorizontalPodAutoscaler with an explicit
// ceiling. maxReplicas is required by the API, so it is never absent in
// practice and never absent here.
func hpa(name string, maxReplicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling/v2",
		"kind":       "HorizontalPodAutoscaler",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"minReplicas":    int64(2),
			"maxReplicas":    maxReplicas,
			"scaleTargetRef": map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "web"},
		},
	}}
}

// claim builds a PersistentVolumeClaim with an explicit request. rules_test.go
// already has a pvc helper for the reliability pack's storage-class rule; that
// one sets no size, which is the one field this pack reads.
func claim(name, storage string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"accessModes":      []any{"ReadWriteOnce"},
			"storageClassName": "standard",
			"resources":        map[string]any{"requests": map[string]any{"storage": storage}},
		},
	}}
}
```

- [ ] **Step 5: Write the failing fires-and-passes test with the nine `exists` cases**

Append to `internal/policypack/cost_rules_test.go`:

```go
// TestEveryCostRuleFiresAndPasses drives each rule through the real evaluator,
// alone, against an object that must violate it and one that must not. A rule
// with a typo'd path or the wrong operator loads cleanly and checks nothing;
// this is what catches that.
func TestEveryCostRuleFiresAndPasses(t *testing.T) {
	rules := loadPack(t, "cost")

	cases := []ruleCase{
		{
			id:         "cost.deploy-ephemeral-storage-limit",
			kind:       "Deployment",
			violating:  deployment("unbounded-disk", containerWithoutResource(t, "limits", "ephemeral-storage")),
			satisfying: deployment("sized", sizedContainer()),
		},
		{
			id:         "cost.statefulset-cpu-request",
			kind:       "StatefulSet",
			violating:  workload("StatefulSet", "no-cpu", containerWithoutResource(t, "requests", "cpu"), 2),
			satisfying: workload("StatefulSet", "sized", sizedContainer(), 2),
		},
		{
			id:         "cost.statefulset-memory-request",
			kind:       "StatefulSet",
			violating:  workload("StatefulSet", "no-memory", containerWithoutResource(t, "requests", "memory"), 2),
			satisfying: workload("StatefulSet", "sized", sizedContainer(), 2),
		},
		{
			id:         "cost.daemonset-cpu-request",
			kind:       "DaemonSet",
			violating:  workload("DaemonSet", "no-cpu", containerWithoutResource(t, "requests", "cpu"), 2),
			satisfying: workload("DaemonSet", "sized", sizedContainer(), 2),
		},
		{
			id:         "cost.daemonset-memory-request",
			kind:       "DaemonSet",
			violating:  workload("DaemonSet", "no-memory", containerWithoutResource(t, "requests", "memory"), 2),
			satisfying: workload("DaemonSet", "sized", sizedContainer(), 2),
		},
		{
			id:         "cost.daemonset-ephemeral-storage-limit",
			kind:       "DaemonSet",
			violating:  workload("DaemonSet", "unbounded-disk", containerWithoutResource(t, "limits", "ephemeral-storage"), 2),
			satisfying: workload("DaemonSet", "sized", sizedContainer(), 2),
		},
		{
			id:         "cost.cronjob-cpu-request",
			kind:       "CronJob",
			violating:  cronJobWithContainer("no-cpu", containerWithoutResource(t, "requests", "cpu")),
			satisfying: goodCronJob("sized"),
		},
		{
			id:         "cost.cronjob-memory-request",
			kind:       "CronJob",
			violating:  cronJobWithContainer("no-memory", containerWithoutResource(t, "requests", "memory")),
			satisfying: goodCronJob("sized"),
		},
		{
			id:         "cost.cronjob-active-deadline",
			kind:       "CronJob",
			violating:  cronJobWithoutDeadline("no-deadline"),
			satisfying: goodCronJob("bounded"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			r := packRule(t, rules, tc.id)

			if got := evaluateOne(t, r, tc.kind, tc.violating); len(got) != 1 {
				t.Errorf("%s produced %d violations on the violating object, want 1", tc.id, len(got))
			}
			if got := evaluateOne(t, r, tc.kind, tc.satisfying); len(got) != 0 {
				t.Errorf("%s produced %d violations on the satisfying object, want 0", tc.id, len(got))
			}
		})
	}
}
```

Task 3 appends its seven threshold cases to the same `cases` slice and adds the count assertion — do **not** add a `len(cases)` check here, it would only have to be edited in the next task.

- [ ] **Step 6: Run it**

```bash
go test ./internal/policypack -run TestEveryCostRuleFiresAndPasses -v
```

Expected: nine subtests, all PASS. A failing subtest means Task 1's path for that rule is wrong — fix the YAML.

- [ ] **Step 7: Verify the untouchables and commit**

```bash
git diff --stat main -- go.mod go.sum internal/policy internal/report/testdata   # must be empty
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./internal/policypack ./internal/policy ./internal/cli
git add internal/policypack/cost_rules_test.go internal/policypack/security_rules_test.go
git commit -s -m "policypack: cost fixtures, kind distribution, and the nine exists rules

Each case starts from a container that satisfies every rule in the pack and
breaks exactly one thing, so a case can only fail for its own reason. The
reliability pack's goodContainer sets no ephemeral-storage limit, so the cost
pack needs its own.

The kind-distribution test pins the level split as well as the kind split:
sixteen info and nothing above it is the pack's central promise, and a rule
that drifted to warning would make adding the pack able to fail a gate.

securityRule becomes packRule — it was never security-specific, and a
copy-pasted twin for the cost tests would be duplication rather than reuse."
```

---

### Task 3: The seven threshold cases and the quantity-comparison test

**Files:**
- Modify: `internal/policypack/cost_rules_test.go`

**Interfaces:**
- Consumes: everything Task 2 produced, plus `packRule` and `evaluateOne`.
- Produces: nothing new for later tasks.

- [ ] **Step 1: Append the seven threshold cases to the existing `cases` slice**

In `TestEveryCostRuleFiresAndPasses`, add these **after** the nine `exists` cases, before the closing `}` of the slice literal:

```go
		{
			id:         "cost.deploy-large-cpu-request",
			kind:       "Deployment",
			violating:  deployment("big-cpu", containerRequesting(t, "cpu", "16")),
			satisfying: deployment("sized", sizedContainer()),
		},
		{
			id:         "cost.deploy-large-memory-request",
			kind:       "Deployment",
			violating:  deployment("big-memory", containerRequesting(t, "memory", "64Gi")),
			satisfying: deployment("sized", sizedContainer()),
		},
		{
			id:         "cost.cronjob-successful-history",
			kind:       "CronJob",
			violating:  cronJobWithHistory("many-successes", int64(50), int64(1)),
			satisfying: goodCronJob("few-successes"),
		},
		{
			id:         "cost.cronjob-failed-history",
			kind:       "CronJob",
			violating:  cronJobWithHistory("many-failures", int64(3), int64(50)),
			satisfying: goodCronJob("few-failures"),
		},
		{
			id:         "cost.job-backoff-limit",
			kind:       "Job",
			violating:  costJob("many-retries", 50),
			satisfying: costJob("few-retries", 6),
		},
		{
			id:         "cost.hpa-max-replicas",
			kind:       "HorizontalPodAutoscaler",
			violating:  hpa("wide", 200),
			satisfying: hpa("narrow", 10),
		},
		{
			id:         "cost.pvc-large-storage",
			kind:       "PersistentVolumeClaim",
			violating:  claim("big", "4Ti"),
			satisfying: claim("small", "20Gi"),
		},
```

Every violating object above sets its field **explicitly**. That is not stylistic: `lte` skips an absent slot, so a case that merely omitted the field would produce zero violations and the subtest would fail — or worse, would pass for the wrong reason if someone then flipped the expectation. Note the two CronJob history cases each hold the *other* limit at its safe value, so neither can pass because of the wrong field.

- [ ] **Step 2: Add the count assertion**

Immediately after the `cases` slice literal and before the loop:

```go
	if len(cases) != 16 {
		t.Fatalf("%d cases, want one per rule in a sixteen-rule pack", len(cases))
	}
```

- [ ] **Step 3: Run it and watch all sixteen pass**

```bash
go test ./internal/policypack -run TestEveryCostRuleFiresAndPasses -v 2>&1 | grep -cE "^    --- PASS"
```

Expected: `16`.

- [ ] **Step 4: Write the failing quantity-comparison test**

Append to `internal/policypack/cost_rules_test.go`:

```go
// TestCostThresholdsCompareQuantitiesNotStrings machine-checks the fact the
// whole threshold half of this pack rests on: internal/policy's compareNumeric
// falls through ParseInt and ParseFloat to resource.ParseQuantity, so a
// threshold compares quantities rather than bytes.
//
// Both cases below are chosen because a lexical comparison gets them WRONG in
// opposite directions, so neither can pass by accident:
//
//	"16" sorts BEFORE "8", so a lexical lte would let sixteen CPUs through
//	"512Mi" sorts AFTER "32Gi", so a lexical lte would accuse half a gibibyte
//
// cost.hpa-max-replicas is a third instance of the same property — "200" sorts
// before "50" — and its own case above already covers it.
func TestCostThresholdsCompareQuantitiesNotStrings(t *testing.T) {
	rules := loadPack(t, "cost")

	cpu := packRule(t, rules, "cost.deploy-large-cpu-request")
	if got := evaluateOne(t, cpu, "Deployment", deployment("sixteen", containerRequesting(t, "cpu", "16"))); len(got) != 1 {
		t.Errorf(`a request of 16 CPUs produced %d violations of a "lte 8" rule, want 1 — the threshold is comparing bytes, not quantities`, len(got))
	}

	memory := packRule(t, rules, "cost.deploy-large-memory-request")
	if got := evaluateOne(t, memory, "Deployment", deployment("half-a-gibibyte", containerRequesting(t, "memory", "512Mi"))); len(got) != 0 {
		t.Errorf(`a request of 512Mi produced %d violations of a "lte 32Gi" rule, want 0 — the threshold is comparing bytes, not quantities`, len(got))
	}
}
```

- [ ] **Step 5: Run it**

```bash
go test ./internal/policypack -run TestCostThresholdsCompareQuantitiesNotStrings -v
```

Expected: PASS. A failure here means either the YAML threshold values are wrong or `compareNumeric` does not behave as the spec verified — in the second case **stop and report**, do not change the engine.

- [ ] **Step 6: Verify the untouchables and commit**

```bash
git diff --stat main -- go.mod go.sum internal/policy internal/report/testdata   # must be empty
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./internal/policypack
git add internal/policypack/cost_rules_test.go
git commit -s -m "policypack: the seven cost thresholds, proved from both sides

Every threshold case sets its field explicitly. An absent slot makes lte skip,
which is not a violation and is not a pass either, so a case that merely
omitted the field would prove nothing.

The quantity test picks the two comparisons a lexical implementation gets
wrong in opposite directions: 16 sorts before 8, and 512Mi sorts after 32Gi.
Neither can pass by accident, which is what makes the pack's threshold half
machine-checked rather than asserted in prose."
```

---

### Task 4: The no-pairing test

**Files:**
- Modify: `internal/policypack/cost_rules_test.go`

**Interfaces:**
- Consumes: `loadPack`, and `policy.Rule`'s `ID`, `Match.Kind`, `Assert.Path` and `Assert.Op` fields.
- Produces: nothing.

- [ ] **Step 1: Write the failing test**

Append to `internal/policypack/cost_rules_test.go`. This is the inverse of `TestPairedRulesDivideTheWork` in `security_rules_test.go`: there, four properties deliberately carry two rules each; here, none does.

```go
// TestCostShipsNoPairedRules pins the decision that the cost pack needs none of
// the security pack's exists/value pairs, and pins the reason: every operator
// except exists and notExists skips an absent slot, so a threshold fires only
// on a value someone actually wrote. A workload that sets no CPU request is
// never accused of setting a large one, and where the API defines a default —
// three successful runs kept, one failed run kept, six retries — absence is
// already the safe value.
//
// It is the inverse of the security pack's TestPairedRulesDivideTheWork. A
// later edit that introduces a pair here has to delete this test, which makes
// it a decision rather than a drift.
func TestCostShipsNoPairedRules(t *testing.T) {
	type slot struct{ kind, path string }
	seen := map[slot]string{}

	for _, r := range loadPack(t, "cost") {
		s := slot{kind: r.Match.Kind, path: r.Assert.Path}
		if first, ok := seen[s]; ok {
			t.Errorf("%s and %s both assert %s on %s — the cost pack ships no paired rules", first, r.ID, r.Assert.Path, r.Match.Kind)
			continue
		}
		seen[s] = r.ID
	}

	// The pack uses exactly two operators. A third would be a new claim shape
	// that this test's reasoning has not been checked against.
	for _, r := range loadPack(t, "cost") {
		if r.Assert.Op != policy.OpExists && r.Assert.Op != policy.OpLte {
			t.Errorf("rule %q uses %q — the cost pack asserts only exists and lte", r.ID, r.Assert.Op)
		}
	}
}
```

- [ ] **Step 2: Run it**

```bash
go test ./internal/policypack -run TestCostShipsNoPairedRules -v
```

Expected: PASS. A failure names the duplicate pair or the unexpected operator.

- [ ] **Step 3: Run the whole package**

```bash
go test -p 2 -count=1 ./internal/policypack -v 2>&1 | grep -E "^(--- FAIL|FAIL|ok)"
```

Expected: `ok`.

- [ ] **Step 4: Verify the untouchables and commit**

```bash
git diff --stat main -- go.mod go.sum internal/policy internal/report/testdata   # must be empty
git add internal/policypack/cost_rules_test.go
git commit -s -m "policypack: pin that the cost pack ships no paired rules

The security pack pairs an exists rule with a value rule for four properties
that are unsafe both unset and set wrong. The cost pack needs no pair, and
this is the test that keeps it that way: adding one means deleting this test,
which makes it a decision rather than a drift.

It also pins the two-operator vocabulary. A third operator would be a claim
shape whose interaction with the absent-slot rule has not been checked."
```

---

### Task 5: Widen the CLI listing and refusal tests

**Files:**
- Modify: `internal/cli/policy_test.go`

**Interfaces:**
- Consumes: `runPolicyPacks(args []string, printName string, w io.Writer) error` — already in `internal/cli/policy.go`, unchanged by this slice.
- Produces: nothing.

`internal/cli/policy.go` is **not** modified. The listing already enumerates `policypack.All()` and the refusal already names `policypack.Names()`, so both pick the new pack up with no code change — these tests are what prove it.

No usage string moves either: `internal/cli/root.go` spells the flag as `--policy-pack name (repeatable)` and names no pack.

- [ ] **Step 1: Widen `TestPolicyPacksListsWhatShips`**

Around line 243, two string slices grow. Replace:

```go
	for _, want := range []string{"reliability", "security"} {
```

with:

```go
	for _, want := range []string{"cost", "reliability", "security"} {
```

and replace:

```go
	for _, want := range []string{"14 rules", "23 rules"} {
```

with:

```go
	for _, want := range []string{"14 rules", "16 rules", "23 rules"} {
```

- [ ] **Step 2: Widen `TestPolicyPacksPrintUnknownNameIsRefused`**

Around line 283, replace:

```go
	for _, want := range []string{"reliability", "security"} {
```

with:

```go
	for _, want := range []string{"cost", "reliability", "security"} {
```

- [ ] **Step 3: Add a cost round-trip to the print test**

`TestPolicyPacksPrintEmitsLoadableYAML` currently prints `reliability` and asserts 14 rules. Add a second case for `cost` immediately after it, as its own test, so the reliability assertion stays untouched:

```go
func TestPolicyPacksPrintEmitsALoadableCostPack(t *testing.T) {
	var buf bytes.Buffer
	if err := runPolicyPacks(nil, "cost", &buf); err != nil {
		t.Fatalf("runPolicyPacks: %v", err)
	}
	rules, err := policy.Load([]policy.Document{{Source: "pack:cost", Data: buf.Bytes()}})
	if err != nil {
		t.Fatalf("the printed pack does not load back: %v", err)
	}
	if len(rules) != 16 {
		t.Errorf("printed pack has %d rules, want 16", len(rules))
	}
}
```

Match the imports and the `policy.Load` call shape used by `TestPolicyPacksPrintEmitsLoadableYAML` a few lines above — read it first and copy its form exactly rather than guessing.

- [ ] **Step 4: Run the package**

```bash
go test -p 2 -count=1 ./internal/cli -run "TestPolicyPacks" -v 2>&1 | grep -E "^(--- |ok|FAIL)"
```

Expected: every `TestPolicyPacks*` subtest PASS.

- [ ] **Step 5: Verify the untouchables and commit**

```bash
git diff --stat main -- go.mod go.sum internal/policy internal/cli/policy.go internal/cli/root.go   # must be empty
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./...
git add internal/cli/policy_test.go
git commit -s -m "cli: assert the cost pack in the listing and the refusal

internal/cli/policy.go is unchanged: the listing already enumerates every pack
and the refusal already names every pack, so both pick a third one up with no
code change. These assertions are what prove that rather than assume it."
```

---

### Task 6: Documentation

**Files:**
- Modify: `website/docs/features/policy-packs.md`
- Modify: `CHANGELOG.md`
- Modify: `CLAUDE.md`
- Modify: `website/docs/roadmap.md`

**Interfaces:** none — documentation only.

- [ ] **Step 1: Restructure `website/docs/features/policy-packs.md` for three packs**

**Restructure, do not append.** The page is currently written for two packs and four specific things go stale:

1. Around line 103, a sentence reading *"rather than of the two packs that happen to ship"* — reword so it does not count the packs at all, or says three.
2. Around line 239, a clause reading *"every rule in both shipped packs is written with them"* — same.
3. The `## Not in this slice` section at line 306 has a bullet beginning *"**A cost pack.** `reliability` and `security` ship; a cost pack does not."* — **narrow it, do not delete it.** Retraction is not deletion: the bullet becomes the one thing that genuinely remains, a pack contributed by someone other than kubeagent itself.
4. The two existing pack sections are headed `## The reliability pack — fourteen rules` and `## The security pack — twenty-three rules`. Add `## The cost pack — sixteen rules` in the same shape, placed after the security section.

The new section must carry, in the page's existing voice:

- A one-line statement of what the pack is about: a workload's claim on the cluster — what it reserves, what it may grow to, what it leaves behind.
- The full sixteen-rule table, matching the shape of the other two pack tables on the page (read them first and copy the column layout exactly).
- **A "Why every cost rule is `info`" subsection.** The other two packs carry `warning` rules; this one does not, so it cannot fail a gate at any `--fail-on` above `info`. The reason is that a cost finding is budget-dependent in a way a security finding is not.
- **A "What the cost pack cannot say" subsection**, following the precedent set by `### What the security pack cannot say`. Three gaps, from the spec:
  1. *limits set without requests* is not expressible — the grammar has no cross-field relation, and this is probably the largest single cost defect in a typical cluster.
  2. A Deployment with no CPU or memory request is unsaid when `cost` runs alone — that `exists` question belongs to `reliability`, deliberately, so the two packs do not duplicate. Run both and the question is covered.
  3. Absence across a namespace is not expressible — a rule asserts over objects that exist, so "this namespace has no `ResourceQuota`" cannot be a rule, and neither can "`minReplicas` equals `maxReplicas`", which compares two fields to each other.
- **A "kubeagent has no prices" statement.** No billing data, no instance types, no node cost, no cloud API. The pack names shapes that usually cost money and claims nothing more. This must read as its own claim, not folded into either of the two below.
- The existing `## Guarantees` section carries the read-only and no-LLM promises. **Do not blur them into one another and do not blur either into the no-prices claim** — three separate statements, none implying another. Check that whatever you add keeps the page's existing separation intact.

- [ ] **Step 2: Check the whole page for stale counts**

```bash
grep -n "two packs\|both packs\|both shipped\|second pack\|reliability and security" website/docs/features/policy-packs.md
```

Every hit must either be correct for three packs or be fixed.

- [ ] **Step 3: Add the CHANGELOG entry**

Under `## [Unreleased]` → `### Added` in `CHANGELOG.md`, in the style of the existing pack entries. It must say: a `cost` pack of sixteen rules over seven kinds; every rule `info`, so adding it cannot fail a gate at the default `--fail-on critical` or at `--fail-on warning`; opt-in, so omitting `--policy-pack` renders the same bytes as before; no schema move (`scan` stays 1.2, `gate` stays 1.1). Do **not** add a version heading — the release commit does that.

- [ ] **Step 4: Update `CLAUDE.md`'s curated-packs bullet**

The bullet currently describes the `reliability` and `security` packs. It gains the `cost` pack: sixteen rules over seven kinds, every one `info`, no paired rules — with the reason, which is that a threshold operator skips an absent slot, so absence is either safe or already reliability's question. State the no-prices claim as its own sentence.

Then narrow the bullet's closing "remaining post-1.0 work" sentence: it currently names the cost pack, the outside-contributed pack, and other baseline dimensions. After this slice it names only the outside-contributed pack and other baseline dimensions.

**Do not add a `(vX.Y.Z)` parenthetical.** That is added exclusively by the later `release:` commit, never by a docs commit.

- [ ] **Step 5: Update `website/docs/roadmap.md`**

Find the curated-policy-packs item and give it the same treatment: the cost pack ships, and the remaining work narrows to the outside-contributed pack.

- [ ] **Step 6: Build the site strictly**

```bash
cd /home/ubuntu/git/kubeagent/website
/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/mkdocs build --strict -f mkdocs.yml
cd /home/ubuntu/git/kubeagent
```

Expected: exit 0, no `WARNING` lines naming your pages. The red "Material for MkDocs 2.0" banner is cosmetic. **The bash working directory persists between calls and has drifted into `website/` before — the `cd` back is not optional.**

- [ ] **Step 7: Verify the untouchables and commit**

```bash
git diff --stat main -- go.mod go.sum internal/ deploy/ website/docs/quickstart.md   # must be empty
go test -p 2 -count=1 ./...
git add website/docs/features/policy-packs.md CHANGELOG.md CLAUDE.md website/docs/roadmap.md
git commit -s -m "docs: the cost policy pack

Sixteen rules over seven kinds, every one info, so adding the pack to a
pipeline cannot fail it at the default --fail-on critical or at --fail-on
warning. Opt-in: omitting the flag renders the same bytes as before, and no
schema version moves.

The page is restructured for three packs rather than appended to, and the
Not-in-this-slice bullet narrows to the one thing that genuinely remains
rather than being deleted.

Three claims are kept separate because none implies another: a rule can never
write to the cluster; a pack makes no LLM call; and kubeagent has no prices —
no billing data, no instance types, no node cost. The pack names shapes that
usually cost money and claims nothing more."
```

---

## Self-review notes

Recorded for the executing agent, from checking this plan against the spec:

- **Spec coverage.** All sixteen rules land in Task 1. All four planned tests land: kind distribution (Task 2), fires-and-passes ×16 (Tasks 2 and 3), no pairing (Task 4), quantity comparison (Task 3). Every constraint in the spec's "Constraints inherited" section appears in Global Constraints above. The spec's three "what it does not do" statements are carried into Task 1's YAML header and Task 6's docs.
- **One deliberate strengthening of the spec.** The spec sketched the quantity test as "`500m` passes `lte 8` while `16` fails it". The `500m` half does not discriminate: a lexical implementation would also let it pass, and so would a skip. Task 3 replaces it with `512Mi` against `lte "32Gi"`, which a lexical implementation gets wrong in the opposite direction. The spec's requirement — that `16` fails — is still met.
- **One deliberate addition.** Task 2 renames `securityRule` to `packRule`. The spec does not mention it; a `costRule` twin would be verbatim duplication of a logic block, which the review rubric treats as a defect.
- **Type consistency.** `packRule`, `evaluateOne`, `loadPack`, `ruleCase`, `workload`, `deployment`, `fixtureNamespace` and `fixtureImage` are used with the signatures recorded in each task's Interfaces block, all read out of the tree rather than guessed. New helpers introduced in Task 2 are used with identical signatures in Tasks 3 and 4.
- **Task 1 Step 2 is deliberately loose.** There is no clean way to watch a generic-over-`All()` test fail for a pack that has no registry entry, because the subtest does not exist yet. Step 5 is the real evidence and the task says so.

## Version

This slice releases as **v1.13.0** (minor: a new user-facing pack). The release is a separate step after merge, run with the `release` skill — not part of any task here.
