# Policy-as-Code Custom Checks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator add organization-specific checks to kubeagent from a YAML policy file, without forking the binary and without giving a rule any way to write to the cluster.

**Architecture:** A new pure package `internal/policy` owns the whole rule language: a strict loader, a dotted-path resolver over `map[string]any` objects, one glob matcher, a closed set of ten operators, two cross-resource relations, and an `Evaluate` function that turns rules plus objects into sorted violations. Nothing in that package holds a client, a context, or a file handle. Around it, `internal/collect` grows one kind-to-lister switch that returns `*unstructured.Unstructured`, `internal/scan` issues one dedicated read per selected kind, and the four presentation surfaces (`scan` text, `scan --output json`, `scan --output html`, `gate`) render what `Evaluate` produced. The `policy validate` command exercises the loader alone — no cluster, no kubeconfig.

**Tech Stack:** Go 1.26, `sigs.k8s.io/yaml` (already a direct require), `k8s.io/apimachinery` (`unstructured`, `runtime.DefaultUnstructuredConverter`, `api/resource.ParseQuantity`), `github.com/spf13/cobra`, the project's own `internal/safetext`, `internal/jsonschema`, `internal/rbacprofile`.

**Source spec:** [docs/superpowers/specs/2026-07-31-policy-as-code-design.md](../specs/2026-07-31-policy-as-code-design.md) (committed `263cbf8`). Where this plan and the spec disagree, the plan's Deviations section says which governs and why.

**Branch:** `policy-as-code`, already cut off `main` at `263cbf8` and checked out. Never implement on `main`.

## Global Constraints

Every task's requirements implicitly include this section. Values are verbatim.

- **DCO.** Every commit needs a `Signed-off-by` trailer matching its author (`git commit -s`) because `main` enforces DCO. Verify with `scripts/dco-check.sh main HEAD`. **No `Co-Authored-By` trailer and no AI attribution anywhere** — commits, code, docs, changelog, PR text.
- **NO NEW DEPENDENCY.** `go.mod` and `go.sum` must not change. `sigs.k8s.io/yaml` is already a direct require (currently used only by `krew_manifest_test.go`) and `k8s.io/apimachinery` is already in use. Adding any module is a plan violation.
- **`internal/policy` is PURE:** no client, no context, no I/O beyond the bytes it is handed. It must never import `internal/remediate`, `internal/explain`, `internal/investigate`, `internal/report`, `internal/scan` or `internal/findings`. The `findings` → `scan` → `policy` chain is why `policy` defines its own `Level` type instead of reusing `findings.Level`.
- **Read-only toward the cluster:** `get`/`list` only. Policy has **no `--fix` integration** and can never write.
- **`internal/report/testdata/golden-scan.txt` stays BYTE-IDENTICAL** (the golden run passes no `--policy`). A second golden fixture covers a policy run. **Do not** regenerate the demo GIF or the quickstart.
- **Tests:** `go test` runs with `-p 2` and never `-short` (full parallelism trips a known Go linker panic on `internal/advisory`). CI's `go test -race ./...` must stay green.
- **schemaVersion:** `jsonschema.ScanVersion` 1.0 → 1.1 and `GateVersion` 1.0 → 1.1, regenerated with `go test ./internal/schemadoc -run TestSchemaDrift -update`, which must classify **both** as additive/MINOR. The gate's added field is `Verdict.PolicyNotEvaluated` (Task 12).
- **Sanitize at ingress** via `internal/safetext.Line`: `Rule.Message` at **load** time, `Violation.Evidence` at **evaluation** time (truncated to **120 runes**). **Matching runs on the RAW value** — sanitizing before matching would let a control character spliced mid-word evade a glob.
- **No secrets, private IPs or internal hostnames anywhere** — including policy examples, testdata policy files, fuzz seed corpora and doc examples. RFC 5737 IPs (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`), RFC 2606 domains (`example.com`, `example.org`). **Filesystem paths are credentials:** a policy path may appear on **stderr and nowhere else** — never in JSON, SARIF or the HTML report.
- **Blocked/refusal reasons are kubeagent's own words**, never the API server's.
- **Flags are declared per command, never persistent:** `--policy` goes on `scan` and `gate` only, as `StringArrayVar` (not `StringSliceVar`, which splits on commas). Every command sets `SilenceErrors` and `SilenceUsage`; validation lives in `RunE`, not Cobra's `Args`/`MarkFlagsMutuallyExclusive` helpers. Usage and error text uses `invokedAs` from `argv[0]`, never a hardcoded `"kubeagent"`.
- **`internal/fuzzgen` is test-only;** no non-test file may import it.
- **Detectors stay pure functions.**
- **Go lives at `/usr/local/go/bin`** — `export PATH=$PATH:/usr/local/go/bin` before any `go` command.

## Sequencing Requirement

Tasks 1–8 complete and unit-test **all of `internal/policy`** before any surface wiring begins in Task 9. `scan`, `gate`, `htmlreport` and `policy validate` all consume the same evaluator; a semantics change after wiring means re-reviewing every surface.

## Named Risks

1. **Slot semantics (Task 3).** A wildcard must produce **one slot per list element even when that element lacks the rest of the path**. On a Pod with three containers where only one sets a CPU limit, `spec.containers[*].resources.limits.cpu` resolves to **three** slots — one present, two absent — so `exists` violates. A test that only exercises a single-container Pod cannot tell the correct implementation from the wrong one. `TestWildcardYieldsOneSlotPerElementIncludingAbsentOnes` is a first-class named test in Task 3, and the Task 3 reviewer is pointed at it explicitly.
2. **Security exclusions (Tasks 1, 6, 9).** `Secret` must not be a selectable kind, and a `path` beginning `data.` or `binaryData.` on a `ConfigMap` must be a **load error**. Without the second, a policy file landed in a repo, run under `gate --output sarif` and uploaded to a code-scanning dashboard, becomes an exfiltration channel. Both need explicit negative tests: `TestSecretIsNotSelectable` (Task 1), `TestConfigMapDataPathIsALoadError` (Task 6), `TestByKindRefusesSecret` (Task 9).

## Deviations From the Spec

- **The gate's added field is named here, not in the spec.** The spec says `gate` gains a field and bumps `GateVersion`, without naming it. Task 12 makes it `Verdict.PolicyNotEvaluated` (`policyNotEvaluated`, `omitempty`) and deliberately adds nothing for violations, which reach the verdict as ordinary findings. Task 15 runs `TestSchemaDrift` first and follows its verdict; if it reports the gate document unchanged, the field was not wired — that is a bug in Task 12, not a reason to skip the bump.
- **`Evaluate` returns `[]Unevaluated`, not `[]string`.** `internal/cli/gate.go:176` calls `gate.Decide(scanRes, opts)`, so `Decide` owns the flattening and the CLI never sees a `[]findings.Finding` it could augment. Carrying `Level` on the value lets `findings.Flatten` emit a synthetic finding per unevaluated rule, and the existing `--fail-on` machinery makes `gate` fail with **zero changes to `internal/gate`**.
- **Map keys containing dots ARE addressable.** The spec calls this "an accepted limitation". It is not acceptable: `app.kubernetes.io/name` and `topology.kubernetes.io/zone` are the label keys operators most want rules about, and a dotted path cannot reach either. Task 4 adds a bracket-quoted segment — `metadata.labels["app.kubernetes.io/name"]` — parsed by a scanner rather than `strings.Split`. The exclusion check in Task 6 runs on the parsed segments as a result, so `data["token"]` and `data[*]` on a ConfigMap are load errors exactly as `data.token` is.
- **`report.PolicyView` is one nil-able field, not two parallel slices.** One additive JSON field (`policy`), and it lets a clean run render "0 violations across N rules" instead of rendering nothing.
- **One new load error beyond the spec:** `namespaceLabels` on a cluster-scoped kind. A `Node` has no namespace, so the selector could never match; silently matching nothing is worse than refusing to load.
- **Policy reads are dedicated, not reused from scan's existing collectors.** 21 of the 23 selectable kinds are already read in scan's phase 1, but `ConfigMap` and `IngressClass` are not read at all, and `vwc`/`mwc` are gated on `opts.Namespace == ""` while `tlsSecrets` is gated on `opts.Certs`. Reusing them would make a webhook policy silently evaluate nothing under `--namespace`. Accepted cost: a policy selecting `Pod` issues a second pod list.

## File Structure

**New files:**

| File | Responsibility |
|---|---|
| `internal/policy/policy.go` | Types (`Level`, `Op`, `Relation`, `Rule`, `Match`, `Assert`, `Violation`, `Unevaluated`, `Inputs`) and the selectable-kind table. |
| `internal/policy/glob.go` | The one wildcard matcher. `*` matches any run including `/`; `?` matches exactly one byte. |
| `internal/policy/path.go` | Dotted-path resolution over `map[string]any` into an ordered `[]Slot`. |
| `internal/policy/op.go` | The closed operator set: `Check(op, slot, values) (ok, skip bool)`. |
| `internal/policy/relation.go` | `hasPodDisruptionBudget`, `hasHorizontalPodAutoscaler`. |
| `internal/policy/load.go` | Strict YAML decode + validation: `Load`, `LoadPaths`, `Kinds`, `Needs`. |
| `internal/policy/eval.go` | `Evaluate`: match, assert, sort, sanitize. |
| `internal/policy/fuzz_test.go` | Four fuzz targets. |
| `internal/policy/testdata/` | Valid and malformed policy fixtures; fuzz seed corpora. |
| `internal/collect/bykind.go` | `ByKind` — the kind→lister switch, converting to `*unstructured.Unstructured`. |
| `internal/cli/policy.go` | The `policy validate` command. |
| `website/docs/features/policy.md` | The feature page. |

**Modified files:** `internal/scan/scan.go` (policy reads + evaluation), `internal/report/report.go` (`PolicyView`, JSON field, text section), `internal/findings/findings.go` (violation + unevaluated mapping), `internal/htmlreport/htmlreport.go` (policy section), `internal/cli/scan.go` and `internal/cli/gate.go` (`--policy`), `internal/cli/root.go` (register `policy`), `internal/cli/usage.go` (usage text), `internal/rbacprofile/profile.go` (feature entry), `internal/jsonschema/jsonschema.go` (version), `internal/schemadoc/schemadoc.go` (`policy.Level` enum), `internal/fuzzgen/imports_test.go` (generalized import invariant), `website/mkdocs.yml`, `website/docs/roadmap.md`, `CHANGELOG.md`, `CLAUDE.md`, `docs/go-concepts.md`.

---

### Task 1: Policy types and the selectable-kind table

**Files:**
- Create: `internal/policy/policy.go`
- Test: `internal/policy/policy_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `Level` (`LevelCritical`/`LevelWarning`/`LevelInfo`), `Op` (ten constants), `Relation` (two constants), `Rule`, `Match`, `Assert`, `Violation`, `Unevaluated`, `Inputs`; `SelectableKinds() []string`, `KindSelectable(kind string) bool`, `KindNamespaced(kind string) (namespaced bool, known bool)`, `RelationValidForKind(r Relation, kind string) bool`, `ValidLevel(l Level) bool`, `ValidOp(o Op) bool`, `ValidRelation(r Relation) bool`.

- [ ] **Step 1: Write the failing test**

Create `internal/policy/policy_test.go`:

```go
package policy

import (
	"sort"
	"testing"
)

// TestSecretIsNotSelectable pins the spec's first security exclusion. A
// violation carries evidence, and evidence drawn from a Secret would be secret
// material rendered into a report, a JSON document and a SARIF upload.
func TestSecretIsNotSelectable(t *testing.T) {
	if KindSelectable("Secret") {
		t.Fatal("Secret must never be a selectable kind — a violation would carry secret material as evidence")
	}
	for _, k := range SelectableKinds() {
		if k == "Secret" {
			t.Fatal("SelectableKinds lists Secret")
		}
	}
}

func TestSelectableKindsIsSortedAndHasNoDuplicates(t *testing.T) {
	kinds := SelectableKinds()
	if len(kinds) != 23 {
		t.Fatalf("want 23 selectable kinds, got %d", len(kinds))
	}
	if !sort.StringsAreSorted(kinds) {
		t.Errorf("SelectableKinds must be sorted for deterministic output: %v", kinds)
	}
	seen := map[string]bool{}
	for _, k := range kinds {
		if seen[k] {
			t.Errorf("duplicate kind %q", k)
		}
		seen[k] = true
	}
}

func TestKindNamespacedReportsAPIScope(t *testing.T) {
	cases := []struct {
		kind       string
		namespaced bool
	}{
		{"Pod", true},
		{"Deployment", true},
		{"PodDisruptionBudget", true},
		{"Node", false},
		{"Namespace", false},
		{"PersistentVolume", false},
		{"StorageClass", false},
		{"IngressClass", false},
		{"ValidatingWebhookConfiguration", false},
	}
	for _, c := range cases {
		got, known := KindNamespaced(c.kind)
		if !known {
			t.Errorf("%s: not a known kind", c.kind)
			continue
		}
		if got != c.namespaced {
			t.Errorf("%s: namespaced = %v, want %v", c.kind, got, c.namespaced)
		}
	}
	if _, known := KindNamespaced("Widget"); known {
		t.Error("Widget must not be a known kind")
	}
}

func TestRelationValidForKind(t *testing.T) {
	cases := []struct {
		rel  Relation
		kind string
		want bool
	}{
		{RelationHasPDB, "Deployment", true},
		{RelationHasPDB, "StatefulSet", true},
		{RelationHasPDB, "ReplicaSet", true},
		{RelationHasPDB, "DaemonSet", true},
		{RelationHasPDB, "Pod", false},
		{RelationHasHPA, "Deployment", true},
		{RelationHasHPA, "StatefulSet", true},
		{RelationHasHPA, "ReplicaSet", true},
		{RelationHasHPA, "DaemonSet", false}, // a DaemonSet runs one pod per node; it cannot scale horizontally
		{RelationHasHPA, "Pod", false},
	}
	for _, c := range cases {
		if got := RelationValidForKind(c.rel, c.kind); got != c.want {
			t.Errorf("RelationValidForKind(%q, %q) = %v, want %v", c.rel, c.kind, got, c.want)
		}
	}
}

func TestValidatorsRejectUnknownValues(t *testing.T) {
	if !ValidLevel(LevelCritical) || !ValidLevel(LevelWarning) || !ValidLevel(LevelInfo) {
		t.Error("the three levels must validate")
	}
	if ValidLevel(Level("fatal")) {
		t.Error("fatal is not a level")
	}
	if !ValidOp(OpExists) || !ValidOp(OpNotMatches) || !ValidOp(OpLte) {
		t.Error("declared operators must validate")
	}
	if ValidOp(Op("regex")) {
		t.Error("regex is not an operator")
	}
	if !ValidRelation(RelationHasPDB) || !ValidRelation(RelationHasHPA) {
		t.Error("declared relations must validate")
	}
	if ValidRelation(Relation("hasNetworkPolicy")) {
		t.Error("hasNetworkPolicy is not a relation")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -run 'TestSecret|TestSelectable|TestKindNamespaced|TestRelationValid|TestValidators' -v
```

Expected: FAIL — the package does not exist (`no Go files in .../internal/policy`).

- [ ] **Step 3: Write the implementation**

Create `internal/policy/policy.go`:

```go
// Package policy evaluates operator-authored rules against Kubernetes objects.
//
// It is pure: no client, no context, no I/O beyond the bytes it is handed. A
// caller reads the cluster and hands the objects in; this package decides what
// violates and returns values. That is what makes a policy rule incapable of
// writing anything — there is nothing here to write with.
//
// It must never import internal/remediate, internal/explain,
// internal/investigate, internal/report, internal/scan or internal/findings.
// internal/findings imports internal/scan and internal/scan imports this
// package, so importing findings would close a cycle — which is why Level is
// declared here rather than reused from internal/findings.
package policy

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Level is how loudly a violation is reported. It mirrors findings.Level in
// spelling but is a distinct type: see the package comment for why.
type Level string

const (
	LevelCritical Level = "critical"
	LevelWarning  Level = "warning"
	LevelInfo     Level = "info"
)

// Op is one of the closed set of comparisons a rule may make. The set is
// closed on purpose: an expression language would be a second thing to fuzz,
// to sandbox and to version.
type Op string

const (
	OpExists     Op = "exists"
	OpNotExists  Op = "notExists"
	OpIn         Op = "in"
	OpNotIn      Op = "notIn"
	OpMatches    Op = "matches"
	OpNotMatches Op = "notMatches"
	OpGt         Op = "gt"
	OpGte        Op = "gte"
	OpLt         Op = "lt"
	OpLte        Op = "lte"
)

// Relation is a claim about another resource in the cluster rather than about
// a field of the matched object.
type Relation string

const (
	RelationHasPDB Relation = "hasPodDisruptionBudget"
	RelationHasHPA Relation = "hasHorizontalPodAutoscaler"
)

// Rule is one policy check, as written in a policy file.
type Rule struct {
	ID      string `json:"id"`
	Match   Match  `json:"match"`
	Assert  Assert `json:"assert"`
	Level   Level  `json:"level"`
	Message string `json:"message"`
}

// Match narrows which resources a rule applies to.
type Match struct {
	Kind            string            `json:"kind"`
	Namespaces      []string          `json:"namespaces,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	NamespaceLabels map[string]string `json:"namespaceLabels,omitempty"`
}

// Assert is one claim. Exactly one of (Path+Op) and Relation is set; the
// loader rejects a rule that sets both or neither.
type Assert struct {
	Path     string   `json:"path,omitempty"`
	Op       Op       `json:"op,omitempty"`
	Values   []string `json:"values,omitempty"`
	Relation Relation `json:"relation,omitempty"`
}

// Violation is one resource failing one rule. A resource produces at most one
// violation per rule: the first failing slot wins.
type Violation struct {
	RuleID    string `json:"ruleId"`
	Level     Level  `json:"level"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Message   string `json:"message"`
	Evidence  string `json:"evidence,omitempty"`
}

// Unevaluated is a rule that could not run because the kind it selects was not
// readable. It carries Level so a caller can decide severity without holding
// the rule set — internal/gate never sees the rules.
//
// A refused read is never a pass.
type Unevaluated struct {
	RuleID string `json:"ruleId"`
	Level  Level  `json:"level"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// Inputs is everything Evaluate needs. The caller does every read; this
// package does none.
type Inputs struct {
	// Objects maps a selectable kind to the objects read for it.
	Objects map[string][]*unstructured.Unstructured
	// Namespaces backs match.namespaceLabels.
	Namespaces []*unstructured.Unstructured
	// PDBs and HPAs back the two relations.
	PDBs []*unstructured.Unstructured
	HPAs []*unstructured.Unstructured
	// Unreadable names kinds whose read was refused or failed. Rules
	// selecting them are reported as not evaluated, never as passing.
	Unreadable map[string]bool
}

// kindInfo is what the evaluator and the loader need to know about a kind.
type kindInfo struct {
	namespaced bool
}

// selectableKinds is exactly the set internal/rbacprofile's core rules already
// grant. Keeping the two in step means a policy file never needs an RBAC grant
// kubeagent does not already ask for, so `rbac print` keeps telling the truth.
//
// Secret is deliberately absent: a violation carries evidence, and evidence
// from a Secret would be secret material rendered into a report, a JSON
// document and a SARIF upload. Event and Lease are absent as carrying no
// policy value.
var selectableKinds = map[string]kindInfo{
	// core/v1
	"ConfigMap":             {namespaced: true},
	"Namespace":             {namespaced: false},
	"Node":                  {namespaced: false},
	"PersistentVolume":      {namespaced: false},
	"PersistentVolumeClaim": {namespaced: true},
	"Pod":                   {namespaced: true},
	"ResourceQuota":         {namespaced: true},
	"Service":               {namespaced: true},
	// apps/v1
	"DaemonSet":   {namespaced: true},
	"Deployment":  {namespaced: true},
	"ReplicaSet":  {namespaced: true},
	"StatefulSet": {namespaced: true},
	// batch/v1
	"CronJob": {namespaced: true},
	"Job":     {namespaced: true},
	// discovery.k8s.io/v1
	"EndpointSlice": {namespaced: true},
	// networking.k8s.io/v1
	"Ingress":       {namespaced: true},
	"IngressClass":  {namespaced: false},
	"NetworkPolicy": {namespaced: true},
	// storage.k8s.io/v1
	"StorageClass": {namespaced: false},
	// policy/v1
	"PodDisruptionBudget": {namespaced: true},
	// autoscaling/v2
	"HorizontalPodAutoscaler": {namespaced: true},
	// admissionregistration.k8s.io/v1
	"MutatingWebhookConfiguration":   {namespaced: false},
	"ValidatingWebhookConfiguration": {namespaced: false},
}

// SelectableKinds returns every kind a rule may select, sorted. Callers read
// the cluster in this order, so sorting keeps the read order deterministic.
func SelectableKinds() []string {
	out := make([]string, 0, len(selectableKinds))
	for k := range selectableKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// KindSelectable reports whether a rule may select this kind.
func KindSelectable(kind string) bool {
	_, ok := selectableKinds[kind]
	return ok
}

// KindNamespaced reports whether a kind is namespaced, and whether it is known
// at all. The loader uses it to refuse namespaceLabels on a cluster-scoped
// kind, where the selector could never match anything.
func KindNamespaced(kind string) (namespaced, known bool) {
	info, ok := selectableKinds[kind]
	return info.namespaced, ok
}

// relationKinds names the workload kinds each relation is meaningful for. A
// DaemonSet runs one pod per node and cannot scale horizontally, so it is
// absent from the HPA list.
var relationKinds = map[Relation]map[string]bool{
	RelationHasPDB: {"Deployment": true, "StatefulSet": true, "ReplicaSet": true, "DaemonSet": true},
	RelationHasHPA: {"Deployment": true, "StatefulSet": true, "ReplicaSet": true},
}

// RelationValidForKind reports whether a relation may be asserted about a kind.
// A mismatch is a load error, not a silent pass.
func RelationValidForKind(r Relation, kind string) bool {
	return relationKinds[r][kind]
}

var validOps = map[Op]bool{
	OpExists: true, OpNotExists: true,
	OpIn: true, OpNotIn: true,
	OpMatches: true, OpNotMatches: true,
	OpGt: true, OpGte: true, OpLt: true, OpLte: true,
}

// ValidOp reports whether an operator is one of the ten.
func ValidOp(o Op) bool { return validOps[o] }

// ValidRelation reports whether a relation is one of the two.
func ValidRelation(r Relation) bool { _, ok := relationKinds[r]; return ok }

// ValidLevel reports whether a level is one of the three.
func ValidLevel(l Level) bool {
	return l == LevelCritical || l == LevelWarning || l == LevelInfo
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -p 2 -v
```

Expected: PASS, all five tests.

- [ ] **Step 5: Verify no new dependency and no forbidden import**

```bash
git diff --stat go.mod go.sum          # must print nothing
go list -deps ./internal/policy | grep -E 'kubeagent/internal/(remediate|explain|investigate|report|scan|findings)'
```

Expected: the first prints nothing; the second prints nothing and exits 1.

- [ ] **Step 6: Commit**

```bash
git add internal/policy/policy.go internal/policy/policy_test.go
git commit -s -m "policy: add rule types and the selectable-kind table

The 23 selectable kinds are exactly those internal/rbacprofile's core rules
already grant, so a policy file never needs an RBAC grant kubeagent does not
already ask for. Secret is deliberately excluded: a violation carries evidence,
and evidence drawn from a Secret would be secret material in a report."
```

---

### Task 2: The glob matcher

**Files:**
- Create: `internal/policy/glob.go`
- Test: `internal/policy/glob_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `globMatch(pattern, s string) bool` (unexported; used by `op.go` for `matches`/`notMatches`).

Why hand-rolled rather than `path.Match`: `path.Match` will not let `*` cross a `/`, so `registry.example.com/*` would fail to match `registry.example.com/team/app:1.0` — the single most obvious rule an operator will write. This is **not** a ReDoS concern; Go's `regexp` is RE2 and linear-time. The reasons are authoring ergonomics and the `/` behaviour.

- [ ] **Step 1: Write the failing test**

Create `internal/policy/glob_test.go`:

```go
package policy

import "testing"

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		want    bool
		why     string
	}{
		{"", "", true, "empty pattern matches empty string"},
		{"", "x", false, "empty pattern matches nothing else"},
		{"*", "", true, "star matches the empty string"},
		{"*", "anything/at/all:1.0", true, "star matches everything"},
		{"nginx", "nginx", true, "literal"},
		{"nginx", "nginxx", false, "literal is anchored at both ends"},
		{"registry.example.com/*", "registry.example.com/team/app:1.0", true, "star crosses a slash — path.Match would not"},
		{"registry.example.com/*", "quay.example.org/team/app:1.0", false, "different registry"},
		{"*/app:*", "registry.example.com/team/app:1.0", true, "two stars"},
		{"?", "a", true, "question mark matches one byte"},
		{"?", "", false, "question mark needs a byte"},
		{"?", "ab", false, "question mark matches exactly one"},
		{"a?c", "abc", true, "question mark mid-pattern"},
		{"a.c", "abc", false, "dot is a literal, not a regexp metacharacter"},
		{"a.c", "a.c", true, "dot matches itself"},
		{"a[bc]d", "abd", false, "brackets are literals"},
		{"a[bc]d", "a[bc]d", true, "brackets match themselves"},
		{"**", "abc", true, "adjacent stars collapse"},
		{"*a*b*c*", "xxaxxbxxcxx", true, "many stars backtrack correctly"},
		{"*a*b*c*", "xxaxxcxxbxx", false, "order still matters"},
		{"prod-*", "prod-", true, "trailing star may match nothing"},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.input); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v (%s)", c.pattern, c.input, got, c.want, c.why)
		}
	}
}

// TestGlobMatchIsLinearOnPathologicalInput guards the matcher against the
// exponential backtracking a naive recursive implementation shows. This must
// finish instantly, not in geologic time.
func TestGlobMatchIsLinearOnPathologicalInput(t *testing.T) {
	pattern := "*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*b"
	input := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if globMatch(pattern, input) {
		t.Error("no trailing b, so no match")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -run TestGlob -v
```

Expected: FAIL with `undefined: globMatch`.

- [ ] **Step 3: Write the implementation**

Create `internal/policy/glob.go`:

```go
package policy

// globMatch reports whether s matches pattern. Two metacharacters, and only
// two: `*` matches any run of bytes including the empty run and including `/`,
// and `?` matches exactly one byte. Every other byte — `.`, `[`, `\` — is a
// literal.
//
// The standard library's path.Match will not let `*` cross a `/`, which breaks
// the most obvious rule an operator will write:
//
//	registry.example.com/*  against  registry.example.com/team/app:1.0
//
// Hence this. It is iterative, allocates nothing, and backtracks to the last
// star rather than recursing, so a pattern full of stars stays linear in the
// length of s rather than exponential.
func globMatch(pattern, s string) bool {
	var (
		p, i       int // cursors into pattern and s
		starP      = -1
		starI      int
	)
	for i < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case p < len(pattern) && pattern[p] == '*':
			// Remember where the star was, try matching zero bytes first.
			starP = p
			starI = i
			p++
		case starP >= 0:
			// Mismatch after a star: let the star swallow one more byte.
			p = starP + 1
			starI++
			i = starI
		default:
			return false
		}
	}
	// Trailing stars may match the empty run.
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -p 2 -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/policy/glob.go internal/policy/glob_test.go
git commit -s -m "policy: add the wildcard matcher

path.Match will not let * cross a /, which breaks registry.example.com/*
against registry.example.com/team/app:1.0 — the most obvious rule an operator
will write. Iterative with star backtracking, so a star-heavy pattern stays
linear rather than exponential."
```

---

### Task 3: Dotted-path resolution into slots

> **Reviewer: this is the highest-risk task in the plan.** The wildcard must
> produce **one slot per list element even when that element lacks the rest of
> the path**. Verify `TestWildcardYieldsOneSlotPerElementIncludingAbsentOnes`
> exists, exercises a **three**-container Pod where only one container sets the
> field, and asserts three slots in container order — not one. An
> implementation that returns only the present values passes every
> single-container test and is wrong.

**Files:**
- Create: `internal/policy/path.go`
- Test: `internal/policy/path_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `Slot{Present bool; Value any}`, `parsePath(path string) ([]segment, error)`, `segment{key string; wildcard bool}`, `resolve(obj map[string]any, segs []segment) []Slot`.

**Semantics (from the spec's "Resolution yields slots, not values"):**

| Situation | Result |
|---|---|
| Cursor slot is absent | exactly **one** absent successor (arity preserved) |
| Cursor value is not a `map[string]any` | one absent successor |
| Key missing, or present with an explicit `nil` | one absent successor |
| Plain segment, key present | one present successor |
| `[*]` on a `[]any` of N elements | **N** present successors, in list order |
| `[*]` on an empty list | **zero** successors |
| `[*]` on a non-list | one absent successor |

- [ ] **Step 1: Write the failing test**

Create `internal/policy/path_test.go`:

```go
package policy

import "testing"

// podWithContainers builds the unstructured shape
// runtime.DefaultUnstructuredConverter produces for a Pod: every container is
// a map, and a container that sets no CPU limit simply has no
// resources.limits.cpu key.
func podWithContainers(cpuLimits ...string) map[string]any {
	containers := make([]any, 0, len(cpuLimits))
	for i, lim := range cpuLimits {
		c := map[string]any{"name": string(rune('a'+i)), "image": "app:1.0"}
		if lim != "" {
			c["resources"] = map[string]any{"limits": map[string]any{"cpu": lim}}
		}
		containers = append(containers, c)
	}
	return map[string]any{
		"kind":     "Pod",
		"metadata": map[string]any{"name": "web", "namespace": "prod"},
		"spec":     map[string]any{"containers": containers},
	}
}

func mustParse(t *testing.T, path string) []segment {
	t.Helper()
	segs, err := parsePath(path)
	if err != nil {
		t.Fatalf("parsePath(%q): %v", path, err)
	}
	return segs
}

// TestWildcardYieldsOneSlotPerElementIncludingAbsentOnes is the load-bearing
// test of this package. A Pod with three containers where only the middle one
// sets a CPU limit must resolve to THREE slots — absent, present, absent — in
// container order. Anything that returns a single present slot makes
// `exists` on spec.containers[*].resources.limits.cpu silently pass a Pod
// where two of three containers are unlimited, which is the exact bug this
// rule exists to catch.
func TestWildcardYieldsOneSlotPerElementIncludingAbsentOnes(t *testing.T) {
	obj := podWithContainers("", "500m", "")
	slots := resolve(obj, mustParse(t, "spec.containers[*].resources.limits.cpu"))

	if len(slots) != 3 {
		t.Fatalf("got %d slots, want 3 (one per container, present or not): %#v", len(slots), slots)
	}
	if slots[0].Present {
		t.Errorf("container 0 sets no CPU limit; slot must be absent, got %#v", slots[0].Value)
	}
	if !slots[1].Present || slots[1].Value != "500m" {
		t.Errorf("container 1 sets 500m; got present=%v value=%#v", slots[1].Present, slots[1].Value)
	}
	if slots[2].Present {
		t.Errorf("container 2 sets no CPU limit; slot must be absent, got %#v", slots[2].Value)
	}
}

func TestWildcardOnEveryContainerPresent(t *testing.T) {
	obj := podWithContainers("100m", "200m", "300m")
	slots := resolve(obj, mustParse(t, "spec.containers[*].resources.limits.cpu"))
	if len(slots) != 3 {
		t.Fatalf("got %d slots, want 3", len(slots))
	}
	for i, want := range []string{"100m", "200m", "300m"} {
		if !slots[i].Present || slots[i].Value != want {
			t.Errorf("slot %d = (%v, %#v), want (true, %q)", i, slots[i].Present, slots[i].Value, want)
		}
	}
}

func TestWildcardOnEmptyListYieldsZeroSlots(t *testing.T) {
	obj := podWithContainers()
	slots := resolve(obj, mustParse(t, "spec.containers[*].image"))
	if len(slots) != 0 {
		t.Fatalf("an empty list must yield zero slots, got %d", len(slots))
	}
}

func TestWildcardOnMissingListYieldsOneAbsentSlot(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{}}
	slots := resolve(obj, mustParse(t, "spec.containers[*].image"))
	if len(slots) != 1 || slots[0].Present {
		t.Fatalf("want one absent slot, got %#v", slots)
	}
}

func TestWildcardOnNonListYieldsOneAbsentSlot(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"containers": "not-a-list"}}
	slots := resolve(obj, mustParse(t, "spec.containers[*].image"))
	if len(slots) != 1 || slots[0].Present {
		t.Fatalf("want one absent slot, got %#v", slots)
	}
}

func TestPlainPathResolution(t *testing.T) {
	obj := podWithContainers("100m")
	cases := []struct {
		path    string
		present bool
		value   any
	}{
		{"metadata.name", true, "web"},
		{"metadata.namespace", true, "prod"},
		{"metadata.uid", false, nil},
		{"metadata.name.deeper", false, nil}, // cursor is a string, not a map
		{"spec", true, nil},                  // present, value is the map itself
	}
	for _, c := range cases {
		slots := resolve(obj, mustParse(t, c.path))
		if len(slots) != 1 {
			t.Errorf("%s: got %d slots, want 1", c.path, len(slots))
			continue
		}
		if slots[0].Present != c.present {
			t.Errorf("%s: present = %v, want %v", c.path, slots[0].Present, c.present)
			continue
		}
		if c.value != nil && slots[0].Value != c.value {
			t.Errorf("%s: value = %#v, want %#v", c.path, slots[0].Value, c.value)
		}
	}
}

// TestExplicitNullIsAbsent: YAML `key: null` decodes to a nil interface value.
// Treating it as present would make `exists` pass on a field that holds
// nothing.
func TestExplicitNullIsAbsent(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"nodeName": nil}}
	slots := resolve(obj, mustParse(t, "spec.nodeName"))
	if len(slots) != 1 || slots[0].Present {
		t.Fatalf("an explicit null must be absent, got %#v", slots)
	}
}

// TestNestedWildcardsMultiply: two wildcards on the same path produce the
// cross-product, still one slot per leaf position.
func TestNestedWildcardsMultiply(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"ports": []any{
			map[string]any{"containerPort": int64(80)},
			map[string]any{"containerPort": int64(443)},
		}},
		map[string]any{"ports": []any{
			map[string]any{"containerPort": int64(8080)},
		}},
		map[string]any{}, // no ports at all
	}}}
	slots := resolve(obj, mustParse(t, "spec.containers[*].ports[*].containerPort"))
	if len(slots) != 4 {
		t.Fatalf("got %d slots, want 4 (2 + 1 + 1 absent): %#v", len(slots), slots)
	}
	if !slots[0].Present || slots[0].Value != int64(80) {
		t.Errorf("slot 0 = %#v, want 80", slots[0])
	}
	if !slots[2].Present || slots[2].Value != int64(8080) {
		t.Errorf("slot 2 = %#v, want 8080", slots[2])
	}
	if slots[3].Present {
		t.Errorf("the container with no ports must contribute one absent slot, got %#v", slots[3])
	}
}

func TestParsePathAcceptsValidPaths(t *testing.T) {
	for _, p := range []string{
		"metadata.name",
		"spec.containers[*].image",
		"spec.containers[*].ports[*].containerPort",
		"spec",
		`metadata.labels["app.kubernetes.io/name"]`,
		`metadata.annotations["example.com/owner"]`,
		`metadata.labels["tier"].nested`,
	} {
		if _, err := parsePath(p); err != nil {
			t.Errorf("parsePath(%q) = %v, want no error", p, err)
		}
	}
}

// A Kubernetes label key contains dots and a slash, so splitting on dots
// cannot reach it at all. This is the case the quoted form exists for, and a
// grammar that accepts the spelling but resolves it as three nested lookups
// would find nothing and report every object as compliant.
func TestQuotedKeyIsOneLookupNotThree(t *testing.T) {
	segs, err := parsePath(`metadata.labels["app.kubernetes.io/name"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3 (metadata, labels, the whole key)", len(segs))
	}
	if segs[2].key != "app.kubernetes.io/name" {
		t.Errorf("key = %q, want the literal label key", segs[2].key)
	}
	if segs[2].wildcard {
		t.Error(`["key"] is a lookup, never a wildcard`)
	}

	obj := map[string]any{"metadata": map[string]any{
		"labels": map[string]any{"app.kubernetes.io/name": "checkout"},
	}}
	slots := resolve(obj, segs)
	if len(slots) != 1 || !slots[0].Present || slots[0].Value != "checkout" {
		t.Fatalf("resolve = %#v, want one present slot holding the label value", slots)
	}
}

func TestParsePathRejectsMalformedPaths(t *testing.T) {
	for _, p := range []string{
		"",                        // empty
		".",                       // empty segment
		"metadata..name",          // empty segment
		".metadata",               // leading dot
		"metadata.",               // trailing dot
		"spec.containers[0].image", // an index is not supported; only [*]
		"spec.containers[].image",  // empty bracket
		"spec.containers[*",        // unterminated
		"spec.containers*].image",  // stray bracket
		"spec.[*].image",           // wildcard with no key
		"[*].image",                // wildcard with no key, at the start
		`metadata.labels["unterminated`,
		`metadata.labels[""]`,      // empty key
		`metadata.labels["a"b"]`,   // a quote inside the key
		`metadata.labels[app]`,     // unquoted bracket key
	} {
		if _, err := parsePath(p); err == nil {
			t.Errorf("parsePath(%q) = nil error, want a rejection", p)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -run 'TestWildcard|TestPlainPath|TestExplicitNull|TestNestedWildcards|TestParsePath' -v
```

Expected: FAIL with `undefined: parsePath`, `undefined: resolve`, `undefined: segment`.

- [ ] **Step 3: Write the implementation**

Create `internal/policy/path.go`:

```go
package policy

import (
	"fmt"
	"strings"
)

// Slot is one position a path resolved to. A path resolves to an ordered list
// of slots, not to a list of values: a wildcard produces one slot per list
// element EVEN WHEN that element lacks the rest of the path, and that slot is
// absent.
//
// This is the whole reason the type exists. On a Pod with three containers
// where only one sets a CPU limit, spec.containers[*].resources.limits.cpu
// resolves to three slots — one present, two absent — so `exists` violates.
// Collapsing to "the values that were found" would silently pass that Pod.
type Slot struct {
	Present bool
	Value   any
}

// segment is one dotted component of a path, optionally with a [*] suffix.
type segment struct {
	key      string
	wildcard bool
}

// parsePath splits a path into segments. Three forms:
//
//	name          a field
//	name[*]       every element of a list field
//	["literal"]   a map key written verbatim
//
// The bracket-quoted form is not a convenience. Kubernetes label and
// annotation keys contain dots and slashes, so app.kubernetes.io/name cannot
// be reached by splitting on dots at all — metadata.labels["app.kubernetes.io/name"]
// is the only way to write it, and label rules are the most common thing an
// operator wants a policy for.
//
// An index like [0] is rejected rather than silently ignored: a policy that
// reads like it pins the first container but does not would be worse than one
// that refuses to load.
func parsePath(path string) ([]segment, error) {
	if path == "" {
		return nil, fmt.Errorf("path is empty")
	}
	var segs []segment
	for i := 0; i < len(path); {
		switch {
		case path[i] == '.':
			return nil, fmt.Errorf("path %q has an empty segment", path)
		case strings.HasPrefix(path[i:], "[*]"):
			return nil, fmt.Errorf("path %q: [*] must follow a field name", path)
		case path[i] == '[':
			key, next, err := parseQuotedKey(path, i)
			if err != nil {
				return nil, err
			}
			segs = append(segs, segment{key: key})
			i = next
		default:
			j := i
			for j < len(path) && path[j] != '.' && path[j] != '[' {
				j++
			}
			seg := segment{key: path[i:j]}
			if strings.ContainsRune(seg.key, ']') {
				return nil, fmt.Errorf("path %q: stray bracket in %q", path, seg.key)
			}
			i = j
			// [*] binds to the field name it follows. ["key"] does not — it is
			// its own segment, handled on the next pass.
			if strings.HasPrefix(path[i:], "[*]") {
				seg.wildcard = true
				i += 3
			}
			segs = append(segs, seg)
		}
		// Between segments: a dot separator, or a bracket that opens the next
		// segment. Anything else is a path the operator did not mean to write.
		if i < len(path) && path[i] == '.' {
			i++
			if i == len(path) {
				return nil, fmt.Errorf("path %q has an empty segment", path)
			}
		}
	}
	return segs, nil
}

// parseQuotedKey reads a ["literal"] segment starting at i and returns the key
// and the index just past the closing bracket. The key is taken verbatim:
// there are no escapes, because a Kubernetes map key cannot contain a double
// quote and inventing an escape syntax for a character that cannot occur would
// be a rule to get wrong for nothing.
func parseQuotedKey(path string, i int) (string, int, error) {
	rest := path[i:]
	if !strings.HasPrefix(rest, `["`) {
		return "", 0, fmt.Errorf(`path %q: expected ["key"] or [*], got %q`, path, rest)
	}
	end := strings.Index(rest[2:], `"]`)
	if end < 0 {
		return "", 0, fmt.Errorf(`path %q: unterminated ["key"]`, path)
	}
	key := rest[2 : 2+end]
	if key == "" {
		return "", 0, fmt.Errorf(`path %q: ["..."] key is empty`, path)
	}
	if strings.ContainsRune(key, '"') {
		return "", 0, fmt.Errorf(`path %q: a double quote inside ["..."] is not supported`, path)
	}
	return key, i + 2 + end + 2, nil
}

// absent is the zero slot. Named so the propagation rules below read as rules.
var absent = Slot{}

// resolve walks segs over obj and returns the ordered slots the path names.
//
// Arity is the invariant: an absent cursor propagates as exactly one absent
// successor, a plain segment maps one cursor to one successor, and a wildcard
// maps one cursor to one successor per list element (zero for an empty list).
func resolve(obj map[string]any, segs []segment) []Slot {
	cur := []Slot{{Present: true, Value: any(obj)}}
	for _, seg := range segs {
		next := make([]Slot, 0, len(cur))
		for _, s := range cur {
			next = append(next, step(s, seg)...)
		}
		cur = next
	}
	return cur
}

// step maps one cursor slot to its successors for one segment.
func step(s Slot, seg segment) []Slot {
	if !s.Present {
		// An absent slot stays exactly one absent slot, wildcard or not.
		// Collapsing it here would lose the arity the caller depends on.
		return []Slot{absent}
	}
	m, ok := s.Value.(map[string]any)
	if !ok {
		return []Slot{absent}
	}
	v, ok := m[seg.key]
	if !ok || v == nil {
		// A key present with an explicit null holds nothing; `exists` must
		// not pass on it.
		return []Slot{absent}
	}
	if !seg.wildcard {
		return []Slot{{Present: true, Value: v}}
	}
	list, ok := v.([]any)
	if !ok {
		return []Slot{absent}
	}
	out := make([]Slot, 0, len(list))
	for _, e := range list {
		out = append(out, Slot{Present: true, Value: e})
	}
	// An empty list yields zero slots: there is nothing to assert about.
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -p 2 -v
```

Expected: PASS, including `TestWildcardYieldsOneSlotPerElementIncludingAbsentOnes`.

- [ ] **Step 5: Prove the test can fail**

Temporarily change `step`'s absent branch to `return nil` (drop the slot instead of propagating it), re-run, and confirm `TestWildcardYieldsOneSlotPerElementIncludingAbsentOnes` fails with "got 1 slots, want 3". Then revert the change and re-run to green. Report both outcomes in the task report — this is the evidence that the named test actually discriminates.

- [ ] **Step 6: Commit**

```bash
git add internal/policy/path.go internal/policy/path_test.go
git commit -s -m "policy: resolve dotted paths into slots, not values

A wildcard produces one slot per list element even when that element lacks the
rest of the path, so exists on spec.containers[*].resources.limits.cpu
violates for a Pod where only one of three containers sets a limit. Collapsing
to the values that happened to be found would silently pass it."
```

---

### Task 4: The closed operator set

**Files:**
- Create: `internal/policy/op.go`
- Test: `internal/policy/op_test.go`

**Interfaces:**
- Consumes: `Slot` and `Op` (Tasks 1, 3), `globMatch` (Task 2).
- Produces: `checkOp(op Op, s Slot, values []string) (ok, skip bool)`, `stringOf(v any) (string, bool)`, `compareNumeric(a, b string) (cmp int, ok bool)`.

**Operator table (the spec's "Absence is a first-class case"):**

| Operator | Absent slot | Present, non-scalar | Unparseable comparison |
|---|---|---|---|
| `exists` | **violation** | — | — |
| `notExists` | satisfied | violation | — |
| every other | **skipped** | skipped | skipped |

`skip == true` means "this slot has nothing to say"; the caller moves to the next slot without recording a violation.

- [ ] **Step 1: Write the failing test**

Create `internal/policy/op_test.go`:

```go
package policy

import (
	"math"
	"strings"
	"testing"
)

func present(v any) Slot { return Slot{Present: true, Value: v} }

func TestCheckOpAbsenceTable(t *testing.T) {
	cases := []struct {
		op       Op
		wantOK   bool
		wantSkip bool
	}{
		{OpExists, false, false},   // absent fails exists — a violation
		{OpNotExists, true, false}, // absent satisfies notExists
		{OpIn, false, true},
		{OpNotIn, false, true},
		{OpMatches, false, true},
		{OpNotMatches, false, true},
		{OpGt, false, true},
		{OpGte, false, true},
		{OpLt, false, true},
		{OpLte, false, true},
	}
	for _, c := range cases {
		ok, skip := checkOp(c.op, absent, []string{"1"})
		if ok != c.wantOK || skip != c.wantSkip {
			t.Errorf("checkOp(%s, absent) = (%v, %v), want (%v, %v)", c.op, ok, skip, c.wantOK, c.wantSkip)
		}
	}
}

func TestCheckOpPresence(t *testing.T) {
	if ok, skip := checkOp(OpExists, present("x"), nil); !ok || skip {
		t.Errorf("exists on a present slot = (%v, %v), want (true, false)", ok, skip)
	}
	if ok, skip := checkOp(OpNotExists, present("x"), nil); ok || skip {
		t.Errorf("notExists on a present slot = (%v, %v), want (false, false)", ok, skip)
	}
}

func TestCheckOpSetMembership(t *testing.T) {
	vals := []string{"Always", "IfNotPresent"}
	if ok, _ := checkOp(OpIn, present("Always"), vals); !ok {
		t.Error("Always is in the set")
	}
	if ok, _ := checkOp(OpIn, present("Never"), vals); ok {
		t.Error("Never is not in the set")
	}
	if ok, _ := checkOp(OpNotIn, present("Never"), vals); !ok {
		t.Error("Never satisfies notIn")
	}
	if ok, _ := checkOp(OpNotIn, present("Always"), vals); ok {
		t.Error("Always fails notIn")
	}
}

func TestCheckOpGlobMatching(t *testing.T) {
	vals := []string{"registry.example.com/*", "quay.example.org/*"}
	if ok, _ := checkOp(OpMatches, present("registry.example.com/team/app:1.0"), vals); !ok {
		t.Error("an allowlisted registry must match")
	}
	if ok, _ := checkOp(OpMatches, present("docker.example.net/app:1.0"), vals); ok {
		t.Error("an unlisted registry must not match")
	}
	if ok, _ := checkOp(OpNotMatches, present("docker.example.net/app:1.0"), vals); !ok {
		t.Error("an unlisted registry satisfies notMatches")
	}
}

// An annotation value can be hundreds of kilobytes and comes from whoever
// wrote the workload. globMatch is quadratic in the worst case, so an
// unbounded call would let that author stall a scan. Over the cap the slot is
// skipped, never silently reported as matching or as not matching — both would
// be a judgement kubeagent did not actually make.
func TestCheckOpSkipsAValueTooLongToMatchSafely(t *testing.T) {
	long := strings.Repeat("a", maxMatchLen+1)
	atCap := strings.Repeat("a", maxMatchLen)

	for _, op := range []Op{OpMatches, OpNotMatches} {
		ok, skip := checkOp(op, present(long), []string{"a*"})
		if !skip {
			t.Errorf("%s on a %d-byte value: skip=false, want true", op, len(long))
		}
		if ok {
			t.Errorf("%s on an over-cap value returned ok=true; a skipped slot decides nothing", op)
		}
		// One byte under the cap still evaluates: the cap must not quietly
		// disable matching for ordinary values.
		if _, skip := checkOp(op, present(atCap), []string{"a*"}); skip {
			t.Errorf("%s on a %d-byte value was skipped; the cap is inclusive", op, len(atCap))
		}
	}

	// Only the glob operators are capped. Equality is a byte compare and costs
	// nothing, so capping it would drop a comparison that was safe to make.
	if _, skip := checkOp(OpIn, present(long), []string{long}); skip {
		t.Error("in was skipped on a long value; only the glob operators are capped")
	}
}

// A float64 carries 53 bits of integer precision. Above that two distinct
// int64 values round to the same float, and a comparison that went through a
// float would return a verdict rather than a skip — the one failure mode this
// package must not have, because a wrong answer is worse than no answer.
func TestCheckOpComparesLargeIntegersExactly(t *testing.T) {
	// 2^53 and 2^53+1: distinct as int64, identical as float64.
	const lo = int64(1) << 53
	const hi = lo + 1

	if ok, skip := checkOp(OpGt, present(hi), []string{strconv.FormatInt(lo, 10)}); !ok || skip {
		t.Errorf("gt %d vs %d: ok=%v skip=%v, want ok=true skip=false", hi, lo, ok, skip)
	}
	if ok, skip := checkOp(OpLt, present(lo), []string{strconv.FormatInt(hi, 10)}); !ok || skip {
		t.Errorf("lt %d vs %d: ok=%v skip=%v, want ok=true skip=false", lo, hi, ok, skip)
	}
	// Equality at the same magnitude must still hold.
	if ok, skip := checkOp(OpGte, present(hi), []string{strconv.FormatInt(hi, 10)}); !ok || skip {
		t.Errorf("gte %d vs itself: ok=%v skip=%v, want ok=true skip=false", hi, ok, skip)
	}
	// The int64 boundary itself, where ParseInt stops and the float path takes
	// over: still a comparison, never a panic.
	if _, skip := checkOp(OpGt, present(int64(math.MaxInt64)), []string{"9223372036854775806"}); skip {
		t.Error("gt at MaxInt64 was skipped; both sides parse as integers")
	}
}

func TestCheckOpNumericAndQuantityComparison(t *testing.T) {
	cases := []struct {
		op       Op
		got      any
		want     string
		wantOK   bool
		wantSkip bool
		why      string
	}{
		{OpLte, "4Gi", "4Gi", true, false, "equal quantities"},
		{OpLte, "8Gi", "4Gi", false, false, "8Gi exceeds 4Gi"},
		{OpLt, "500m", "1", true, false, "millicores against a whole core"},
		{OpGte, int64(3), "2", true, false, "plain integers"},
		{OpGt, float64(1.5), "2", false, false, "plain floats"},
		{OpGte, "3", "2", true, false, "numeric strings"},
		{OpLte, "not-a-number", "4Gi", false, true, "an unparseable field must not become a false accusation"},
		{OpLte, "4Gi", "not-a-number", false, true, "an unparseable threshold skips too"},
		{OpLte, math.NaN(), "4", false, true, "NaN is not comparable"},
		{OpLte, math.Inf(1), "4", false, true, "Inf is not comparable"},
	}
	for _, c := range cases {
		ok, skip := checkOp(c.op, present(c.got), []string{c.want})
		if ok != c.wantOK || skip != c.wantSkip {
			t.Errorf("checkOp(%s, %#v, %q) = (%v, %v), want (%v, %v) — %s",
				c.op, c.got, c.want, ok, skip, c.wantOK, c.wantSkip, c.why)
		}
	}
}

func TestCheckOpSkipsNonScalarValues(t *testing.T) {
	for _, v := range []any{
		map[string]any{"a": "b"},
		[]any{"a"},
	} {
		for _, op := range []Op{OpIn, OpMatches, OpGt} {
			if ok, skip := checkOp(op, present(v), []string{"a"}); ok || !skip {
				t.Errorf("checkOp(%s, %#v) = (%v, %v), want (false, true)", op, v, ok, skip)
			}
		}
	}
}

// TestCheckOpSkipsWhenValuesAreMissing is defence in depth: the loader makes
// this unreachable in production, but a fuzz target calls checkOp directly.
func TestCheckOpSkipsWhenValuesAreMissing(t *testing.T) {
	for _, op := range []Op{OpIn, OpNotIn, OpMatches, OpNotMatches, OpGt, OpGte, OpLt, OpLte} {
		if ok, skip := checkOp(op, present("x"), nil); ok || !skip {
			t.Errorf("checkOp(%s, present, nil values) = (%v, %v), want (false, true)", op, ok, skip)
		}
	}
}

func TestStringOf(t *testing.T) {
	cases := []struct {
		in     any
		want   string
		wantOK bool
	}{
		{"x", "x", true},
		{true, "true", true},
		{false, "false", true},
		{int64(7), "7", true},
		{float64(1.5), "1.5", true},
		{float64(2), "2", true},
		{math.NaN(), "", false},
		{math.Inf(-1), "", false},
		{map[string]any{}, "", false},
		{[]any{}, "", false},
		{nil, "", false},
	}
	for _, c := range cases {
		got, ok := stringOf(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("stringOf(%#v) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -run 'TestCheckOp|TestStringOf' -v
```

Expected: FAIL with `undefined: checkOp`, `undefined: stringOf`.

- [ ] **Step 3: Write the implementation**

Create `internal/policy/op.go`:

```go
package policy

import (
	"math"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
)

// maxMatchLen bounds what the glob operators will look at. Every value a
// policy realistically matches on — an image reference, a label value, a
// storage class name — is far below this; the cap exists for the annotation
// nobody expected. Do not remove it on the belief that globMatch is linear:
// it is not, and glob.go says so.
const maxMatchLen = 4096

// checkOp applies one operator to one slot.
//
// ok reports whether the slot satisfies the assertion. skip reports that the
// slot had nothing to say — an absent value under an operator other than
// exists/notExists, a non-scalar value, or a comparison neither side of which
// parses as a number or a Kubernetes quantity. A skipped slot is not a
// violation: a policy must never turn a field it cannot read into an
// accusation.
func checkOp(op Op, s Slot, values []string) (ok, skip bool) {
	// exists and notExists are the only operators that have an opinion about
	// absence itself.
	switch op {
	case OpExists:
		return s.Present, false
	case OpNotExists:
		return !s.Present, false
	}
	if !s.Present {
		return false, true
	}
	// Defence in depth: the loader enforces arity, so production never gets
	// here with no values. A fuzz target calling checkOp directly can.
	if len(values) == 0 {
		return false, true
	}
	got, isScalar := stringOf(s.Value)
	if !isScalar {
		return false, true
	}
	switch op {
	case OpIn:
		for _, v := range values {
			if got == v {
				return true, false
			}
		}
		return false, false
	case OpNotIn:
		for _, v := range values {
			if got == v {
				return false, false
			}
		}
		return true, false
	case OpMatches, OpNotMatches:
		// globMatch is O(len(pattern) * len(got)) in the worst case — a single
		// star followed by a long partly-matching literal run. `got` comes from
		// the cluster and an annotation value can reach hundreds of kilobytes,
		// so an unbounded call is a workload author's denial of service against
		// a scan. Over the cap the slot is skipped, the same answer a non-scalar
		// gets: kubeagent declines to judge rather than guessing.
		if len(got) > maxMatchLen {
			return false, true
		}
		for _, v := range values {
			if globMatch(v, got) {
				return op == OpMatches, false
			}
		}
		return op == OpNotMatches, false
	case OpGt, OpGte, OpLt, OpLte:
		cmp, cmpOK := compareNumeric(got, values[0])
		if !cmpOK {
			return false, true
		}
		switch op {
		case OpGt:
			return cmp > 0, false
		case OpGte:
			return cmp >= 0, false
		case OpLt:
			return cmp < 0, false
		default:
			return cmp <= 0, false
		}
	}
	// An operator the loader should have rejected. Say nothing rather than
	// accuse.
	return false, true
}

// stringOf renders a scalar unstructured value as text, reporting false for
// anything that is not a scalar.
//
// runtime.DefaultUnstructuredConverter produces exactly five leaf types —
// string, bool, int64, float64 and nil — plus map[string]any and []any for
// the interior. Non-finite floats are refused: formatting a NaN and then
// comparing it produces nonsense, and an integer conversion of one overflows,
// which is the bug class the fuzz campaign already found in the DNS health
// parser.
func stringOf(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return "", false
		}
		return strconv.FormatFloat(t, 'g', -1, 64), true
	default:
		return "", false
	}
}

// compareNumeric compares two textual values, returning -1, 0 or 1, and
// whether the comparison was possible at all.
//
// Whole integers first, compared exactly. Past 2^53 a float64 can no longer
// represent every integer, so 9007199254740993 and 9007199254740992 round to
// the same value and gt would answer "no" — a confident wrong verdict, which
// is worse than the skip an unreadable value gets. Then plain numbers, so 3.5
// vs 2 does not need a quantity round-trip; then Kubernetes quantities, so
// 500m vs 1 and 8Gi vs 4Gi compare correctly. If either side fails all three,
// the caller skips.
func compareNumeric(a, b string) (int, bool) {
	if ai, err := strconv.ParseInt(a, 10, 64); err == nil {
		if bi, err := strconv.ParseInt(b, 10, 64); err == nil {
			switch {
			case ai < bi:
				return -1, true
			case ai > bi:
				return 1, true
			default:
				return 0, true
			}
		}
	}

	af, aErr := strconv.ParseFloat(a, 64)
	bf, bErr := strconv.ParseFloat(b, 64)
	if aErr == nil && bErr == nil {
		if math.IsNaN(af) || math.IsNaN(bf) || math.IsInf(af, 0) || math.IsInf(bf, 0) {
			return 0, false
		}
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		default:
			return 0, true
		}
	}
	aq, aErr := resource.ParseQuantity(a)
	if aErr != nil {
		return 0, false
	}
	bq, bErr := resource.ParseQuantity(b)
	if bErr != nil {
		return 0, false
	}
	return aq.Cmp(bq), true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -p 2 -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/policy/op.go internal/policy/op_test.go
git commit -s -m "policy: add the closed operator set

Ten operators, no expression language. Absence is a first-class case: only
exists and notExists have an opinion about it, and every other operator skips
an absent slot rather than accusing. An unparseable field or threshold skips
too — a policy must never turn a field it cannot read into a false accusation."
```

---

### Task 5: The two cross-resource relations

**Files:**
- Create: `internal/policy/relation.go`
- Test: `internal/policy/relation_test.go`

**Interfaces:**
- Consumes: `Relation`, `Inputs` (Task 1).
- Produces: `relationHolds(rel Relation, obj *unstructured.Unstructured, in Inputs) bool`.

A relation is asserted positively and violated when it does **not** hold. There is no inverse form — `notHasPodDisruptionBudget` is not a thing, because "this Deployment must not be covered by a PDB" is not a check anyone wants.

**Documented limitations, each with a test:**
- A PDB with no `spec.selector` at all is ignored.
- An **empty** `spec.selector.matchLabels` covers every workload in its namespace (that is what an empty selector means in Kubernetes).
- `matchExpressions` are **not** evaluated. A PDB whose selector relies solely on them does not cover the workload — reported honestly rather than guessed at.

- [ ] **Step 1: Write the failing test**

Create `internal/policy/relation_test.go`:

```go
package policy

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func workload(kind, namespace, name string, templateLabels map[string]string) *unstructured.Unstructured {
	labels := map[string]any{}
	for k, v := range templateLabels {
		labels[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"kind":     kind,
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"template": map[string]any{"metadata": map[string]any{"labels": labels}},
		},
	}}
}

func pdb(namespace string, selector map[string]any) *unstructured.Unstructured {
	spec := map[string]any{}
	if selector != nil {
		spec["selector"] = selector
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"kind":     "PodDisruptionBudget",
		"metadata": map[string]any{"name": "pdb", "namespace": namespace},
		"spec":     spec,
	}}
}

func hpa(namespace, targetKind, targetName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"kind":     "HorizontalPodAutoscaler",
		"metadata": map[string]any{"name": "hpa", "namespace": namespace},
		"spec": map[string]any{
			"scaleTargetRef": map[string]any{"kind": targetKind, "name": targetName},
		},
	}}
}

func TestHasPodDisruptionBudget(t *testing.T) {
	dep := workload("Deployment", "prod", "web", map[string]string{"app": "web", "tier": "front"})

	cases := []struct {
		name string
		pdbs []*unstructured.Unstructured
		want bool
	}{
		{"no PDBs at all", nil, false},
		{"matching subset selector", []*unstructured.Unstructured{
			pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web"}}),
		}, true},
		{"selector needs a label the workload lacks", []*unstructured.Unstructured{
			pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web", "zone": "b"}}),
		}, false},
		{"right selector, wrong namespace", []*unstructured.Unstructured{
			pdb("staging", map[string]any{"matchLabels": map[string]any{"app": "web"}}),
		}, false},
		{"empty matchLabels covers everything in the namespace", []*unstructured.Unstructured{
			pdb("prod", map[string]any{"matchLabels": map[string]any{}}),
		}, true},
		{"no selector at all is ignored", []*unstructured.Unstructured{
			pdb("prod", nil),
		}, false},
		{"matchExpressions alone does not cover — kubeagent does not evaluate them", []*unstructured.Unstructured{
			pdb("prod", map[string]any{"matchExpressions": []any{
				map[string]any{"key": "app", "operator": "In", "values": []any{"web"}},
			}}),
		}, false},
		{"one of several matches", []*unstructured.Unstructured{
			pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "api"}}),
			pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web"}}),
		}, true},
	}
	for _, c := range cases {
		got := relationHolds(RelationHasPDB, dep, Inputs{PDBs: c.pdbs})
		if got != c.want {
			t.Errorf("%s: relationHolds = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHasHorizontalPodAutoscaler(t *testing.T) {
	dep := workload("Deployment", "prod", "web", nil)

	cases := []struct {
		name string
		hpas []*unstructured.Unstructured
		want bool
	}{
		{"none", nil, false},
		{"exact target", []*unstructured.Unstructured{hpa("prod", "Deployment", "web")}, true},
		{"wrong name", []*unstructured.Unstructured{hpa("prod", "Deployment", "api")}, false},
		{"wrong kind", []*unstructured.Unstructured{hpa("prod", "StatefulSet", "web")}, false},
		{"wrong namespace", []*unstructured.Unstructured{hpa("staging", "Deployment", "web")}, false},
	}
	for _, c := range cases {
		got := relationHolds(RelationHasHPA, dep, Inputs{HPAs: c.hpas})
		if got != c.want {
			t.Errorf("%s: relationHolds = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestWorkloadWithNoPodTemplateLabelsIsCoveredOnlyByAnEmptySelector: a
// Deployment whose pod template sets no labels can only be covered by a
// selector that requires nothing.
func TestWorkloadWithNoPodTemplateLabelsIsCoveredOnlyByAnEmptySelector(t *testing.T) {
	dep := workload("Deployment", "prod", "web", nil)
	if relationHolds(RelationHasPDB, dep, Inputs{PDBs: []*unstructured.Unstructured{
		pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web"}}),
	}}) {
		t.Error("a workload with no template labels must not be covered by a label selector")
	}
	if !relationHolds(RelationHasPDB, dep, Inputs{PDBs: []*unstructured.Unstructured{
		pdb("prod", map[string]any{"matchLabels": map[string]any{}}),
	}}) {
		t.Error("an empty selector covers everything in the namespace")
	}
}

func TestUnknownRelationNeverHolds(t *testing.T) {
	dep := workload("Deployment", "prod", "web", nil)
	if relationHolds(Relation("hasNetworkPolicy"), dep, Inputs{}) {
		t.Error("an unknown relation must not hold")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -run 'TestHas|TestWorkloadWithNo|TestUnknownRelation' -v
```

Expected: FAIL with `undefined: relationHolds`.

- [ ] **Step 3: Write the implementation**

Create `internal/policy/relation.go`:

```go
package policy

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// relationHolds reports whether a cross-resource relation is true of obj. A
// rule asserting a relation violates when this returns false.
//
// Both relations are deliberately shallow: they answer "is there an object of
// this kind pointing at this workload", not "does that object do anything
// useful". A PodDisruptionBudget with maxUnavailable: 100% still counts as
// covering the Deployment — whether the budget is meaningful is a separate
// rule an operator can write with a path assertion.
func relationHolds(rel Relation, obj *unstructured.Unstructured, in Inputs) bool {
	switch rel {
	case RelationHasPDB:
		return coveredByPDB(obj, in.PDBs)
	case RelationHasHPA:
		return targetedByHPA(obj, in.HPAs)
	default:
		return false
	}
}

// coveredByPDB reports whether any PodDisruptionBudget in the workload's
// namespace selects its pod template.
//
// Only matchLabels is evaluated. A selector that relies solely on
// matchExpressions does not cover the workload — kubeagent says "no PDB
// covers this" rather than guessing at set-based semantics it does not
// implement.
func coveredByPDB(obj *unstructured.Unstructured, pdbs []*unstructured.Unstructured) bool {
	ns := obj.GetNamespace()
	labels := podTemplateLabels(obj)
	for _, p := range pdbs {
		if p == nil || p.GetNamespace() != ns {
			continue
		}
		selector, found, err := unstructured.NestedMap(p.Object, "spec", "selector")
		if err != nil || !found {
			// A PDB with no selector selects nothing kubeagent can reason
			// about; ignore it rather than treat it as universal.
			continue
		}
		if exprs, ok := selector["matchExpressions"].([]any); ok && len(exprs) > 0 {
			continue
		}
		match, _, err := unstructured.NestedStringMap(p.Object, "spec", "selector", "matchLabels")
		if err != nil {
			continue
		}
		// An empty selector matches every pod in the namespace.
		if subset(match, labels) {
			return true
		}
	}
	return false
}

// targetedByHPA reports whether any HorizontalPodAutoscaler in the workload's
// namespace scales it.
func targetedByHPA(obj *unstructured.Unstructured, hpas []*unstructured.Unstructured) bool {
	ns, kind, name := obj.GetNamespace(), obj.GetKind(), obj.GetName()
	for _, h := range hpas {
		if h == nil || h.GetNamespace() != ns {
			continue
		}
		targetKind, _, err := unstructured.NestedString(h.Object, "spec", "scaleTargetRef", "kind")
		if err != nil {
			continue
		}
		targetName, _, err := unstructured.NestedString(h.Object, "spec", "scaleTargetRef", "name")
		if err != nil {
			continue
		}
		if targetKind == kind && targetName == name {
			return true
		}
	}
	return false
}

// podTemplateLabels returns the labels a workload stamps on the pods it
// creates — what a PodDisruptionBudget actually selects.
func podTemplateLabels(obj *unstructured.Unstructured) map[string]string {
	labels, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		return nil
	}
	return labels
}

// subset reports whether every key/value in want appears in have. An empty
// want is a subset of anything.
func subset(want, have map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -p 2 -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/policy/relation.go internal/policy/relation_test.go
git commit -s -m "policy: add the hasPodDisruptionBudget and hasHorizontalPodAutoscaler relations

Only matchLabels is evaluated: a PDB selector relying solely on
matchExpressions does not cover the workload, so kubeagent reports what it
actually checked rather than guessing at semantics it does not implement."
```

---

### Task 6: The strict loader

**Files:**
- Create: `internal/policy/load.go`
- Create: `internal/policy/testdata/valid.yaml`, `internal/policy/testdata/second.yaml`
- Test: `internal/policy/load_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces:
  - `Document{Source string; Data []byte}`
  - `Load(docs []Document) ([]Rule, error)`
  - `Kinds(rules []Rule) []string` — sorted, deduplicated selected kinds
  - `Aux{Namespaces, PDBs, HPAs bool}`
  - `Needs(rules []Rule) Aux`

**Purity note:** `Load` takes bytes, not paths — `internal/policy` performs no I/O. Walking a `--policy` argument into `[]Document` lives in `internal/cli` (Task 14), where the file system already is. Taking a **slice** of documents rather than one at a time is what lets the loader detect a duplicate rule id **across** files.

**Load errors.** Each names the offending file and, where it has one, the rule id. Loading is fail-fast, **before any cluster call**:

| Condition | Message fragment |
|---|---|
| YAML does not parse | `p.yaml: invalid YAML: …` |
| Unknown field (`UnmarshalStrict`) | `p.yaml: invalid YAML: unknown field "sevrity"` |
| Empty `id` | `p.yaml: rule 2 has no id` |
| `id` outside `[A-Za-z0-9._-]` | `p.yaml: rule id "reg allow!" may use only letters, digits, dot, dash and underscore` |
| Duplicate `id` across all documents | `second.yaml: rule id "registry-allowlist" is already defined in valid.yaml` |
| Unknown `kind` | `p.yaml: rule "x": kind "Secret" is not one of the kinds a policy may select` |
| `namespaces` or `namespaceLabels` on a cluster-scoped kind | `p.yaml: rule "x": kind "Node" is cluster-scoped, so namespaces and namespaceLabels can never match` |
| Both `path` and `relation`, or neither | `p.yaml: rule "x": assert must set exactly one of path and relation` |
| Unknown `op` | `p.yaml: rule "x": unknown operator "regex"` |
| Bad path syntax | `p.yaml: rule "x": path "spec.containers[0].image": only the [*] wildcard is supported, got "[0]"` |
| Wrong `values` arity | `p.yaml: rule "x": operator "lte" takes exactly one value, got 2` |
| `data`/`binaryData` path on a ConfigMap | `p.yaml: rule "x": a ConfigMap policy may not read data or binaryData — a violation would carry the contents as evidence` |
| Unknown `relation` | `p.yaml: rule "x": unknown relation "hasNetworkPolicy"` |
| Relation on an invalid kind | `p.yaml: rule "x": relation "hasHorizontalPodAutoscaler" does not apply to kind "DaemonSet"` |
| Unknown `level` | `p.yaml: rule "x": unknown level "fatal"` |
| Empty `message` | `p.yaml: rule "x": message is empty` |

- [ ] **Step 1: Write the fixtures**

Create `internal/policy/testdata/valid.yaml`:

```yaml
- id: registry-allowlist
  match:
    kind: Pod
    namespaceLabels:
      tier: prod
  assert:
    path: spec.containers[*].image
    op: matches
    values: ["registry.example.com/*"]
  level: critical
  message: image comes from a registry outside the allowlist

- id: prod-deployments-need-a-pdb
  match:
    kind: Deployment
    namespaceLabels:
      tier: prod
  assert:
    relation: hasPodDisruptionBudget
  level: warning
  message: no PodDisruptionBudget covers this Deployment

- id: memory-limit-is-set
  match:
    kind: Pod
    namespaces: [prod]
  assert:
    path: spec.containers[*].resources.limits.memory
    op: exists
  level: warning
  message: container sets no memory limit
```

Create `internal/policy/testdata/second.yaml`:

```yaml
- id: registry-allowlist
  match:
    kind: Pod
  assert:
    path: spec.containers[*].image
    op: matches
    values: ["quay.example.org/*"]
  level: info
  message: duplicate id, used to prove cross-file detection
```

- [ ] **Step 2: Write the failing test**

Create `internal/policy/load_test.go`:

```go
package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return Document{Source: name, Data: data}
}

func TestLoadAcceptsAValidPolicy(t *testing.T) {
	rules, err := Load([]Document{readFixture(t, "valid.yaml")})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3", len(rules))
	}
	if rules[0].ID != "registry-allowlist" || rules[0].Assert.Op != OpMatches {
		t.Errorf("rule 0 = %#v", rules[0])
	}
	if rules[1].Assert.Relation != RelationHasPDB {
		t.Errorf("rule 1 relation = %q", rules[1].Assert.Relation)
	}
	if rules[2].Level != LevelWarning {
		t.Errorf("rule 2 level = %q", rules[2].Level)
	}
}

func TestLoadDetectsADuplicateIDAcrossFiles(t *testing.T) {
	_, err := Load([]Document{readFixture(t, "valid.yaml"), readFixture(t, "second.yaml")})
	if err == nil {
		t.Fatal("want an error for a duplicate rule id")
	}
	msg := err.Error()
	for _, want := range []string{"second.yaml", "registry-allowlist", "valid.yaml"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
}

// TestConfigMapDataPathIsALoadError pins the spec's second security exclusion.
// Without it, a policy file landed in a repo, run under `gate --output sarif`
// and uploaded to a code-scanning dashboard, is an exfiltration channel.
func TestConfigMapDataPathIsALoadError(t *testing.T) {
	// Every spelling of the same read, including the two bracket forms a
	// string-prefix check would miss.
	for _, path := range []string{
		"data", "data.token", "binaryData", "binaryData.blob",
		`data["token"]`, "data[*]", `binaryData["blob"]`,
	} {
		doc := Document{Source: "cm.yaml", Data: []byte(`
- id: read-configmap
  match: {kind: ConfigMap}
  assert: {path: ` + path + `, op: exists}
  level: info
  message: reads a ConfigMap value
`)}
		_, err := Load([]Document{doc})
		if err == nil {
			t.Errorf("path %q on a ConfigMap must be a load error", path)
			continue
		}
		if !strings.Contains(err.Error(), "evidence") {
			t.Errorf("path %q: error %q should explain that a violation would carry the contents as evidence", path, err)
		}
	}
	// The same path on another kind is fine — only ConfigMap holds
	// operator-supplied data under those keys.
	doc := Document{Source: "pod.yaml", Data: []byte(`
- id: pod-data
  match: {kind: Pod}
  assert: {path: data.whatever, op: notExists}
  level: info
  message: not a ConfigMap, so not restricted
`)}
	if _, err := Load([]Document{doc}); err != nil {
		t.Errorf("a data. path on a Pod is not restricted: %v", err)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring the error must contain
	}{
		{"not yaml", "\t- : :\n  ::", "invalid YAML"},
		{"unknown field", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
  sevrity: high
`, "sevrity"},
		{"no id", `
- match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
`, "no id"},
		{"bad id charset", `
- id: "reg allow!"
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
`, "letters, digits"},
		{"unknown kind", `
- id: x
  match: {kind: Secret}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
`, "not one of the kinds"},
		{"namespaceLabels on a cluster-scoped kind", `
- id: x
  match: {kind: Node, namespaceLabels: {tier: prod}}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
`, "cluster-scoped"},
		{"both path and relation", `
- id: x
  match: {kind: Deployment}
  assert: {path: metadata.name, op: exists, relation: hasPodDisruptionBudget}
  level: info
  message: m
`, "exactly one"},
		{"neither path nor relation", `
- id: x
  match: {kind: Deployment}
  assert: {}
  level: info
  message: m
`, "exactly one"},
		{"unknown operator", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: regex, values: ["a"]}
  level: info
  message: m
`, "unknown operator"},
		{"bad path", `
- id: x
  match: {kind: Pod}
  assert: {path: "spec.containers[0].image", op: exists}
  level: info
  message: m
`, "[*]"},
		{"exists takes no values", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists, values: ["a"]}
  level: info
  message: m
`, "takes no values"},
		{"in needs values", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: in}
  level: info
  message: m
`, "at least one value"},
		{"lte takes exactly one value", `
- id: x
  match: {kind: Pod}
  assert: {path: spec.containers[*].resources.limits.memory, op: lte, values: ["1Gi", "2Gi"]}
  level: info
  message: m
`, "exactly one value"},
		{"unknown relation", `
- id: x
  match: {kind: Deployment}
  assert: {relation: hasNetworkPolicy}
  level: info
  message: m
`, "unknown relation"},
		{"relation on an invalid kind", `
- id: x
  match: {kind: DaemonSet}
  assert: {relation: hasHorizontalPodAutoscaler}
  level: info
  message: m
`, "does not apply to kind"},
		{"relation takes no values", `
- id: x
  match: {kind: Deployment}
  assert: {relation: hasPodDisruptionBudget, values: ["a"]}
  level: info
  message: m
`, "takes no values"},
		{"unknown level", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: fatal
  message: m
`, "unknown level"},
		{"empty message", `
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: info
  message: ""
`, "message is empty"},
	}
	for _, c := range cases {
		_, err := Load([]Document{{Source: "p.yaml", Data: []byte(c.yaml)}})
		if err == nil {
			t.Errorf("%s: want an error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not contain %q", c.name, err, c.want)
		}
		if !strings.Contains(err.Error(), "p.yaml") {
			t.Errorf("%s: error %q does not name the file", c.name, err)
		}
	}
}

// TestLoadSanitizesTheMessage: a message is operator-authored, but it ends up
// on a terminal, so it is sanitized at ingress like any other untrusted line.
func TestLoadSanitizesTheMessage(t *testing.T) {
	// A YAML double-quoted scalar interprets a backslash-u escape, so the
	// message arrives carrying a real ESC byte.
	src := "- id: x\n" +
		"  match: {kind: Pod}\n" +
		"  assert: {path: metadata.name, op: exists}\n" +
		"  level: info\n" +
		"  message: \"bad\\u001b[2Jmessage\"\n"
	rules, err := Load([]Document{{Source: "p.yaml", Data: []byte(src)}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.ContainsRune(rules[0].Message, '\x1b') {
		t.Errorf("message was not sanitized: %q", rules[0].Message)
	}
}

func TestKindsAndNeeds(t *testing.T) {
	rules, err := Load([]Document{readFixture(t, "valid.yaml")})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	kinds := Kinds(rules)
	if len(kinds) != 2 || kinds[0] != "Deployment" || kinds[1] != "Pod" {
		t.Errorf("Kinds = %v, want [Deployment Pod] sorted and deduplicated", kinds)
	}
	need := Needs(rules)
	if !need.Namespaces {
		t.Error("namespaceLabels is used, so Namespaces must be needed")
	}
	if !need.PDBs {
		t.Error("hasPodDisruptionBudget is used, so PDBs must be needed")
	}
	if need.HPAs {
		t.Error("no rule uses hasHorizontalPodAutoscaler, so HPAs must not be needed")
	}
}

func TestNeedsIsEmptyForAPathOnlyPolicy(t *testing.T) {
	rules, err := Load([]Document{{Source: "p.yaml", Data: []byte(`
- id: x
  match: {kind: Pod}
  assert: {path: metadata.name, op: exists}
  level: info
  message: m
`)}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if need := Needs(rules); need.Namespaces || need.PDBs || need.HPAs {
		t.Errorf("Needs = %#v, want all false", need)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -run 'TestLoad|TestConfigMapData|TestKindsAndNeeds|TestNeedsIsEmpty' -v
```

Expected: FAIL with `undefined: Load`, `undefined: Document`, `undefined: Kinds`, `undefined: Needs`.

- [ ] **Step 4: Write the implementation**

Create `internal/policy/load.go`:

```go
package policy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// Document is one policy file's bytes plus the name to use in an error. That
// name reaches stderr only: a policy path is a filesystem path, and
// filesystem paths are credentials — none may appear in JSON, SARIF or the
// HTML report.
type Document struct {
	Source string
	Data   []byte
}

// Aux names the auxiliary reads an evaluation needs beyond the selected kinds.
type Aux struct {
	Namespaces bool // any rule uses match.namespaceLabels
	PDBs       bool // any rule asserts hasPodDisruptionBudget
	HPAs       bool // any rule asserts hasHorizontalPodAutoscaler
}

// ruleIDPattern keeps a rule id to characters that are safe as a SARIF rule
// id, as a value in a JSON document, and as a line of terminal output.
var ruleIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Load decodes and validates every document, returning the rules in file
// order. It is fail-fast: the first problem stops the load, and no cluster
// call has happened yet, so a malformed policy costs nothing but the read.
//
// Taking every document at once is what lets a duplicate rule id be caught
// across files rather than only within one.
func Load(docs []Document) ([]Rule, error) {
	var out []Rule
	seen := map[string]string{} // rule id -> the document that defined it
	for _, doc := range docs {
		var rules []Rule
		if err := yaml.UnmarshalStrict(doc.Data, &rules); err != nil {
			return nil, fmt.Errorf("%s: invalid YAML: %w", doc.Source, err)
		}
		for i, r := range rules {
			if r.ID == "" {
				return nil, fmt.Errorf("%s: rule %d has no id", doc.Source, i+1)
			}
			if !ruleIDPattern.MatchString(r.ID) {
				return nil, fmt.Errorf("%s: rule id %q may use only letters, digits, dot, dash and underscore", doc.Source, r.ID)
			}
			if where, dup := seen[r.ID]; dup {
				return nil, fmt.Errorf("%s: rule id %q is already defined in %s", doc.Source, r.ID, where)
			}
			if err := validate(&r); err != nil {
				return nil, fmt.Errorf("%s: rule %q: %w", doc.Source, r.ID, err)
			}
			seen[r.ID] = doc.Source
			// Sanitize at ingress: the message reaches a terminal, a JSON
			// document and an HTML report.
			r.Message = safetext.Line(r.Message)
			out = append(out, r)
		}
	}
	return out, nil
}

// validate checks one rule. The error names no file — Load adds the file and
// the rule id.
func validate(r *Rule) error {
	if !KindSelectable(r.Match.Kind) {
		return fmt.Errorf("kind %q is not one of the kinds a policy may select", r.Match.Kind)
	}
	// A Node has no namespace, so either selector could only ever match
	// nothing. Silently matching nothing is worse than refusing to load.
	if len(r.Match.NamespaceLabels) > 0 || len(r.Match.Namespaces) > 0 {
		if namespaced, _ := KindNamespaced(r.Match.Kind); !namespaced {
			return fmt.Errorf("kind %q is cluster-scoped, so namespaces and namespaceLabels can never match", r.Match.Kind)
		}
	}
	hasPath := r.Assert.Path != ""
	hasRelation := r.Assert.Relation != ""
	if hasPath == hasRelation {
		return fmt.Errorf("assert must set exactly one of path and relation")
	}
	if hasRelation {
		if err := validateRelation(r); err != nil {
			return err
		}
	} else if err := validatePath(r); err != nil {
		return err
	}
	if !ValidLevel(r.Level) {
		return fmt.Errorf("unknown level %q", r.Level)
	}
	if strings.TrimSpace(r.Message) == "" {
		return fmt.Errorf("message is empty")
	}
	return nil
}

func validateRelation(r *Rule) error {
	if !ValidRelation(r.Assert.Relation) {
		return fmt.Errorf("unknown relation %q", r.Assert.Relation)
	}
	if r.Assert.Op != "" {
		return fmt.Errorf("relation %q takes no op", r.Assert.Relation)
	}
	if len(r.Assert.Values) > 0 {
		return fmt.Errorf("relation %q takes no values", r.Assert.Relation)
	}
	if !RelationValidForKind(r.Assert.Relation, r.Match.Kind) {
		return fmt.Errorf("relation %q does not apply to kind %q", r.Assert.Relation, r.Match.Kind)
	}
	return nil
}

func validatePath(r *Rule) error {
	if !ValidOp(r.Assert.Op) {
		return fmt.Errorf("unknown operator %q", r.Assert.Op)
	}
	segs, err := parsePath(r.Assert.Path)
	if err != nil {
		return err
	}
	if r.Match.Kind == "ConfigMap" && readsConfigMapContents(segs) {
		return fmt.Errorf("a ConfigMap policy may not read data or binaryData — a violation would carry the contents as evidence")
	}
	return validateArity(r.Assert.Op, r.Assert.Values)
}

// readsConfigMapContents reports whether a path reaches into a ConfigMap's
// operator-supplied contents. Both the bare key and any subpath are refused: a
// ConfigMap routinely holds a token or a connection string that nobody
// remembered was not a Secret.
func readsConfigMapContents(segs []segment) bool {
	if len(segs) == 0 {
		return false
	}
	// The first segment decides it, whatever the rest spells. Matching on the
	// raw string would miss data["key"] and data[*], which read exactly the
	// same contents as data.key.
	return segs[0].key == "data" || segs[0].key == "binaryData"
}

func validateArity(op Op, values []string) error {
	switch op {
	case OpExists, OpNotExists:
		if len(values) > 0 {
			return fmt.Errorf("operator %q takes no values", op)
		}
	case OpIn, OpNotIn, OpMatches, OpNotMatches:
		if len(values) == 0 {
			return fmt.Errorf("operator %q needs at least one value", op)
		}
	case OpGt, OpGte, OpLt, OpLte:
		if len(values) != 1 {
			return fmt.Errorf("operator %q takes exactly one value, got %d", op, len(values))
		}
	}
	return nil
}

// Kinds returns every kind the rules select, sorted and deduplicated. The
// caller reads the cluster in this order, so the order is part of the
// determinism contract.
func Kinds(rules []Rule) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		if !seen[r.Match.Kind] {
			seen[r.Match.Kind] = true
			out = append(out, r.Match.Kind)
		}
	}
	sort.Strings(out)
	return out
}

// Needs reports which auxiliary reads the rules require. Skipping an unneeded
// read is not cleverness — it is not asking the API server for something no
// rule will look at.
func Needs(rules []Rule) Aux {
	var a Aux
	for _, r := range rules {
		if len(r.Match.NamespaceLabels) > 0 {
			a.Namespaces = true
		}
		switch r.Assert.Relation {
		case RelationHasPDB:
			a.PDBs = true
		case RelationHasHPA:
			a.HPAs = true
		}
	}
	return a
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -p 2 -v
git diff --stat go.mod go.sum          # must print nothing
```

Expected: PASS; no dependency change (`sigs.k8s.io/yaml` is already a direct require).

- [ ] **Step 6: Commit**

```bash
git add internal/policy/load.go internal/policy/load_test.go internal/policy/testdata/valid.yaml internal/policy/testdata/second.yaml
git commit -s -m "policy: add the strict loader

Load takes bytes, not paths, so internal/policy performs no I/O; walking a
--policy argument into documents lives in the CLI. Every rejection names the
file and the rule id, on stderr only. A ConfigMap policy may not read data or
binaryData: a violation carries evidence, so such a rule under
gate --output sarif would turn a code-scanning dashboard into an exfiltration
channel."
```

---

### Task 7: Evaluate

**Files:**
- Create: `internal/policy/eval.go`
- Test: `internal/policy/eval_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–6.
- Produces: `Evaluate(rules []Rule, in Inputs) (violations []Violation, notEvaluated []Unevaluated)`.

**Rules of the evaluation:**
- A rule whose kind is in `in.Unreadable` is **not evaluated** — never silently passed. A refused read is a blind spot, and `gate` must be able to fail on it.
- One resource produces **at most one violation per rule**: the first failing slot wins, then the loop moves to the next resource.
- Zero slots: `exists` violates (there is nothing where something was required); every other operator says nothing.
- `Evidence` is the failing slot's value, sanitized through `safetext.Line` and truncated to **120 runes**. An absent slot has no evidence.
- Output is sorted by `(RuleID, Kind, Namespace, Name)` so two runs over the same cluster render the same bytes.

- [ ] **Step 1: Write the failing test**

Create `internal/policy/eval_test.go`:

```go
package policy

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func pod(namespace, name string, labels map[string]string, images ...string) *unstructured.Unstructured {
	containers := make([]any, 0, len(images))
	for i, img := range images {
		containers = append(containers, map[string]any{"name": string(rune('a' + i)), "image": img})
	}
	meta := map[string]any{"name": name, "namespace": namespace}
	if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		meta["labels"] = l
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"kind":     "Pod",
		"metadata": meta,
		"spec":     map[string]any{"containers": containers},
	}}
}

func namespaceObj(name string, labels map[string]string) *unstructured.Unstructured {
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"kind":     "Namespace",
		"metadata": map[string]any{"name": name, "labels": l},
	}}
}

func registryRule() Rule {
	return Rule{
		ID:      "registry-allowlist",
		Match:   Match{Kind: "Pod"},
		Assert:  Assert{Path: "spec.containers[*].image", Op: OpMatches, Values: []string{"registry.example.com/*"}},
		Level:   LevelCritical,
		Message: "image comes from a registry outside the allowlist",
	}
}

func TestEvaluateFlagsAViolatingResource(t *testing.T) {
	in := Inputs{Objects: map[string][]*unstructured.Unstructured{"Pod": {
		pod("prod", "good", nil, "registry.example.com/team/app:1.0"),
		pod("prod", "bad", nil, "docker.example.net/app:1.0"),
	}}}
	violations, notEvaluated := Evaluate([]Rule{registryRule()}, in)
	if len(notEvaluated) != 0 {
		t.Fatalf("nothing was unreadable, got %#v", notEvaluated)
	}
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want 1: %#v", len(violations), violations)
	}
	v := violations[0]
	if v.RuleID != "registry-allowlist" || v.Kind != "Pod" || v.Namespace != "prod" || v.Name != "bad" {
		t.Errorf("violation = %#v", v)
	}
	if v.Level != LevelCritical {
		t.Errorf("level = %q, want critical", v.Level)
	}
	if v.Evidence != "docker.example.net/app:1.0" {
		t.Errorf("evidence = %q, want the offending image", v.Evidence)
	}
}

// TestOneViolationPerResourcePerRule: a Pod with two bad images reports once,
// not twice. A report that lists the same rule against the same resource
// repeatedly buries the other findings.
func TestOneViolationPerResourcePerRule(t *testing.T) {
	in := Inputs{Objects: map[string][]*unstructured.Unstructured{"Pod": {
		pod("prod", "bad", nil, "docker.example.net/a:1.0", "docker.example.net/b:1.0"),
	}}}
	violations, _ := Evaluate([]Rule{registryRule()}, in)
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want exactly 1", len(violations))
	}
	if violations[0].Evidence != "docker.example.net/a:1.0" {
		t.Errorf("evidence = %q, want the FIRST failing slot", violations[0].Evidence)
	}
}

// TestPartiallyAbsentWildcardViolatesUnderExists is the evaluation-level twin
// of Task 3's slot test: three containers, one memory limit, one violation.
func TestPartiallyAbsentWildcardViolatesUnderExists(t *testing.T) {
	p := pod("prod", "web", nil, "app:1.0", "app:1.0", "app:1.0")
	containers, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
	c0 := containers[0].(map[string]any)
	c0["resources"] = map[string]any{"limits": map[string]any{"memory": "1Gi"}}
	_ = unstructured.SetNestedSlice(p.Object, containers, "spec", "containers")

	rule := Rule{
		ID:      "memory-limit-is-set",
		Match:   Match{Kind: "Pod"},
		Assert:  Assert{Path: "spec.containers[*].resources.limits.memory", Op: OpExists},
		Level:   LevelWarning,
		Message: "container sets no memory limit",
	}
	violations, _ := Evaluate([]Rule{rule}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Pod": {p}},
	})
	if len(violations) != 1 {
		t.Fatalf("two of three containers set no memory limit; want 1 violation, got %d", len(violations))
	}
	if violations[0].Evidence != "" {
		t.Errorf("an absent slot has no evidence, got %q", violations[0].Evidence)
	}
}

func TestZeroSlotsViolateOnlyUnderExists(t *testing.T) {
	// A Pod with no containers at all: spec.containers[*].image resolves to
	// zero slots.
	empty := pod("prod", "empty", nil)
	in := Inputs{Objects: map[string][]*unstructured.Unstructured{"Pod": {empty}}}

	existsRule := Rule{ID: "r", Match: Match{Kind: "Pod"},
		Assert: Assert{Path: "spec.containers[*].image", Op: OpExists},
		Level:  LevelInfo, Message: "no image"}
	if v, _ := Evaluate([]Rule{existsRule}, in); len(v) != 1 {
		t.Errorf("exists over zero slots must violate, got %d violations", len(v))
	}

	for _, op := range []Op{OpNotExists, OpIn, OpMatches, OpLte} {
		r := Rule{ID: "r", Match: Match{Kind: "Pod"},
			Assert: Assert{Path: "spec.containers[*].image", Op: op, Values: []string{"x"}},
			Level:  LevelInfo, Message: "m"}
		if op == OpNotExists {
			r.Assert.Values = nil
		}
		if v, _ := Evaluate([]Rule{r}, in); len(v) != 0 {
			t.Errorf("%s over zero slots must not violate, got %d", op, len(v))
		}
	}
}

func TestUnreadableKindIsReportedNotEvaluated(t *testing.T) {
	in := Inputs{Unreadable: map[string]bool{"Pod": true}}
	violations, notEvaluated := Evaluate([]Rule{registryRule()}, in)
	if len(violations) != 0 {
		t.Errorf("an unreadable kind produces no violations, got %d", len(violations))
	}
	if len(notEvaluated) != 1 {
		t.Fatalf("got %d unevaluated rules, want 1", len(notEvaluated))
	}
	u := notEvaluated[0]
	if u.RuleID != "registry-allowlist" || u.Kind != "Pod" || u.Level != LevelCritical {
		t.Errorf("unevaluated = %#v", u)
	}
	if u.Reason == "" {
		t.Error("an unevaluated rule must carry kubeagent's own reason")
	}
}

// The selected kind read fine but the list the rule compares against did not.
// Reporting every Deployment as uncovered would be a fabricated violation —
// the mirror image of a silent pass, and just as wrong.
func TestUnreadableSupportingListIsReportedNotEvaluated(t *testing.T) {
	dep := workload("Deployment", "prod", "web", map[string]string{"app": "web"})
	pdbRule := Rule{
		ID:      "prod-deployments-need-a-pdb",
		Match:   Match{Kind: "Deployment"},
		Assert:  Assert{Relation: RelationHasPDB},
		Level:   LevelWarning,
		Message: "no PodDisruptionBudget covers this Deployment",
	}
	nsRule := registryRule()
	nsRule.Match.NamespaceLabels = map[string]string{"tier": "prod"}

	cases := []struct {
		name       string
		rule       Rule
		objects    map[string][]*unstructured.Unstructured
		unreadable map[string]bool
	}{
		{"pdb list refused", pdbRule,
			map[string][]*unstructured.Unstructured{"Deployment": {dep}},
			map[string]bool{"PodDisruptionBudget": true}},
		{"namespace list refused", nsRule,
			map[string][]*unstructured.Unstructured{"Pod": {pod("prod", "a", nil, "docker.example.net/x:1.0")}},
			map[string]bool{"Namespace": true}},
	}
	for _, c := range cases {
		violations, notEvaluated := Evaluate([]Rule{c.rule}, Inputs{
			Objects: c.objects, Unreadable: c.unreadable,
		})
		if len(violations) != 0 {
			t.Errorf("%s: got %d violations, want 0 — an unread list is not evidence", c.name, len(violations))
		}
		if len(notEvaluated) != 1 {
			t.Fatalf("%s: got %d unevaluated rules, want 1", c.name, len(notEvaluated))
		}
		if notEvaluated[0].Reason != unreadableSupportReason {
			t.Errorf("%s: reason = %q", c.name, notEvaluated[0].Reason)
		}
	}
}

func TestMatchNarrowsByNamespaceLabelAndNamespaceLabels(t *testing.T) {
	pods := []*unstructured.Unstructured{
		pod("prod", "a", map[string]string{"app": "web"}, "docker.example.net/x:1.0"),
		pod("prod", "b", map[string]string{"app": "api"}, "docker.example.net/x:1.0"),
		pod("staging", "c", map[string]string{"app": "web"}, "docker.example.net/x:1.0"),
	}
	namespaces := []*unstructured.Unstructured{
		namespaceObj("prod", map[string]string{"tier": "prod"}),
		namespaceObj("staging", map[string]string{"tier": "dev"}),
	}

	byNamespace := registryRule()
	byNamespace.Match.Namespaces = []string{"prod"}
	byLabel := registryRule()
	byLabel.Match.Labels = map[string]string{"app": "web"}
	byNamespaceLabel := registryRule()
	byNamespaceLabel.Match.NamespaceLabels = map[string]string{"tier": "prod"}

	cases := []struct {
		name  string
		rule  Rule
		names []string
	}{
		{"namespaces", byNamespace, []string{"a", "b"}},
		{"labels", byLabel, []string{"a", "c"}},
		{"namespaceLabels", byNamespaceLabel, []string{"a", "b"}},
	}
	for _, c := range cases {
		violations, _ := Evaluate([]Rule{c.rule}, Inputs{
			Objects:    map[string][]*unstructured.Unstructured{"Pod": pods},
			Namespaces: namespaces,
		})
		var got []string
		for _, v := range violations {
			got = append(got, v.Name)
		}
		if strings.Join(got, ",") != strings.Join(c.names, ",") {
			t.Errorf("%s: matched %v, want %v", c.name, got, c.names)
		}
	}
}

// TestNamespaceLabelsWithNoNamespaceObjectMatchesNothing: if the Namespace
// read was skipped or refused, a namespaceLabels rule must not match blindly.
func TestNamespaceLabelsWithNoNamespaceObjectMatchesNothing(t *testing.T) {
	rule := registryRule()
	rule.Match.NamespaceLabels = map[string]string{"tier": "prod"}
	violations, _ := Evaluate([]Rule{rule}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Pod": {
			pod("prod", "a", nil, "docker.example.net/x:1.0"),
		}},
	})
	if len(violations) != 0 {
		t.Errorf("with no Namespace objects, a namespaceLabels rule matches nothing, got %d", len(violations))
	}
}

func TestRelationViolationCarriesNoEvidence(t *testing.T) {
	dep := workload("Deployment", "prod", "web", map[string]string{"app": "web"})
	rule := Rule{
		ID:      "prod-deployments-need-a-pdb",
		Match:   Match{Kind: "Deployment"},
		Assert:  Assert{Relation: RelationHasPDB},
		Level:   LevelWarning,
		Message: "no PodDisruptionBudget covers this Deployment",
	}
	violations, _ := Evaluate([]Rule{rule}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Deployment": {dep}},
	})
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want 1", len(violations))
	}
	if violations[0].Evidence != "" {
		t.Errorf("a relation violation has no field to quote, got %q", violations[0].Evidence)
	}
	// With a covering PDB it holds and there is no violation.
	violations, _ = Evaluate([]Rule{rule}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Deployment": {dep}},
		PDBs:    []*unstructured.Unstructured{pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web"}})},
	})
	if len(violations) != 0 {
		t.Errorf("a covering PDB satisfies the relation, got %d violations", len(violations))
	}
}

func TestEvidenceIsSanitizedAndTruncated(t *testing.T) {
	long := strings.Repeat("x", 300)
	p := pod("prod", "bad", nil, "docker.example.net/\x1b[2J"+long)
	violations, _ := Evaluate([]Rule{registryRule()}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Pod": {p}},
	})
	if len(violations) != 1 {
		t.Fatalf("got %d violations, want 1", len(violations))
	}
	ev := violations[0].Evidence
	if strings.ContainsRune(ev, '\x1b') {
		t.Errorf("evidence was not sanitized: %q", ev)
	}
	if n := len([]rune(ev)); n > 120 {
		t.Errorf("evidence is %d runes, want at most 120", n)
	}
}

// TestMatchingRunsOnTheRawValue: sanitizing before matching would let a
// control character spliced mid-word evade a glob. The image below is NOT
// registry.example.com/app once the escape is accounted for, and must still
// be reported.
func TestMatchingRunsOnTheRawValue(t *testing.T) {
	p := pod("prod", "sneaky", nil, "registry.example.com\x1b/../docker.example.net/app:1.0")
	rule := registryRule()
	rule.Assert.Values = []string{"registry.example.com/*"}
	violations, _ := Evaluate([]Rule{rule}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Pod": {p}},
	})
	if len(violations) != 1 {
		t.Fatalf("the raw image does not match the allowlist glob; want 1 violation, got %d", len(violations))
	}
}

func TestEvaluateOutputIsSorted(t *testing.T) {
	pods := []*unstructured.Unstructured{
		pod("prod", "z", nil, "docker.example.net/x:1.0"),
		pod("alpha", "a", nil, "docker.example.net/x:1.0"),
		pod("prod", "a", nil, "docker.example.net/x:1.0"),
	}
	ruleB := registryRule()
	ruleB.ID = "b-rule"
	ruleA := registryRule()
	ruleA.ID = "a-rule"

	violations, _ := Evaluate([]Rule{ruleB, ruleA}, Inputs{
		Objects: map[string][]*unstructured.Unstructured{"Pod": pods},
	})
	var got []string
	for _, v := range violations {
		got = append(got, v.RuleID+"/"+v.Namespace+"/"+v.Name)
	}
	want := []string{
		"a-rule/alpha/a", "a-rule/prod/a", "a-rule/prod/z",
		"b-rule/alpha/a", "b-rule/prod/a", "b-rule/prod/z",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("order = %v\nwant  = %v", got, want)
	}
}

func TestEvaluateHandlesNilInputsWithoutPanicking(t *testing.T) {
	if v, n := Evaluate(nil, Inputs{}); len(v) != 0 || len(n) != 0 {
		t.Errorf("no rules means no output, got %d/%d", len(v), len(n))
	}
	if v, _ := Evaluate([]Rule{registryRule()}, Inputs{}); len(v) != 0 {
		t.Errorf("no objects means no violations, got %d", len(v))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -run TestEvaluate -v
```

Expected: FAIL with `undefined: Evaluate`.

- [ ] **Step 3: Write the implementation**

Create `internal/policy/eval.go`:

```go
package policy

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// evidenceLimit caps how much of a failing value is quoted back. A report is
// meant to be read; a 4 KiB annotation pasted into a terminal is not.
const evidenceLimit = 120

// unreadableReason is kubeagent's own words for why a rule did not run. It
// never quotes the API server: a refusal reason is kubeagent's to phrase.
const unreadableReason = "kubeagent could not read this kind, so the rule was not evaluated"

// unreadableSupportReason covers the second way a rule can go unevaluated: the
// selected kind was read, but the objects the rule compares against were not.
// A hasPodDisruptionBudget rule with no PDB list would otherwise report every
// workload as uncovered, which is a fabricated violation rather than a blind
// spot — the same failure as a silent pass, only louder.
const unreadableSupportReason = "kubeagent could not read the objects this rule compares against, so the rule was not evaluated"

// unreadableFor reports kubeagent's own reason why a rule cannot run, or "" if
// it can. It never quotes the API server.
func unreadableFor(r Rule, in Inputs) string {
	if in.Unreadable[r.Match.Kind] {
		return unreadableReason
	}
	if len(r.Match.NamespaceLabels) > 0 && in.Unreadable["Namespace"] {
		return unreadableSupportReason
	}
	switch r.Assert.Relation {
	case RelationHasPDB:
		if in.Unreadable["PodDisruptionBudget"] {
			return unreadableSupportReason
		}
	case RelationHasHPA:
		if in.Unreadable["HorizontalPodAutoscaler"] {
			return unreadableSupportReason
		}
	}
	return ""
}

// Evaluate applies every rule to the objects it was handed and returns the
// violations plus the rules that could not be evaluated at all.
//
// A rule whose kind was unreadable is reported as not evaluated, never as
// passing: a refused read is a blind spot, and a blind spot that renders as a
// clean bill of health is worse than no check.
//
// One resource produces at most one violation per rule — the first failing
// slot wins. Output is sorted, so two runs over the same cluster render the
// same bytes.
func Evaluate(rules []Rule, in Inputs) (violations []Violation, notEvaluated []Unevaluated) {
	nsLabels := namespaceLabelIndex(in.Namespaces)

	for _, r := range rules {
		if reason := unreadableFor(r, in); reason != "" {
			notEvaluated = append(notEvaluated, Unevaluated{
				RuleID: r.ID,
				Level:  r.Level,
				Kind:   r.Match.Kind,
				Reason: reason,
			})
			continue
		}
		// The loader already accepted this path, so a parse failure here
		// would be a bug rather than bad input. Skip rather than panic.
		var segs []segment
		if r.Assert.Path != "" {
			parsed, err := parsePath(r.Assert.Path)
			if err != nil {
				continue
			}
			segs = parsed
		}
		for _, obj := range in.Objects[r.Match.Kind] {
			if obj == nil || !matches(r.Match, obj, nsLabels) {
				continue
			}
			if v, ok := check(r, segs, obj, in); ok {
				violations = append(violations, v)
			}
		}
	}

	sort.SliceStable(violations, func(i, j int) bool {
		a, b := violations[i], violations[j]
		switch {
		case a.RuleID != b.RuleID:
			return a.RuleID < b.RuleID
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		case a.Namespace != b.Namespace:
			return a.Namespace < b.Namespace
		default:
			return a.Name < b.Name
		}
	})
	sort.SliceStable(notEvaluated, func(i, j int) bool {
		return notEvaluated[i].RuleID < notEvaluated[j].RuleID
	})
	return violations, notEvaluated
}

// check applies one rule to one already-matched object, returning the
// violation if there is one.
func check(r Rule, segs []segment, obj *unstructured.Unstructured, in Inputs) (Violation, bool) {
	if r.Assert.Relation != "" {
		if relationHolds(r.Assert.Relation, obj, in) {
			return Violation{}, false
		}
		// A relation violation has no field to quote.
		return violationFor(r, obj, ""), true
	}

	slots := resolve(obj.Object, segs)
	if len(slots) == 0 {
		// Nothing to assert about. `exists` still violates — something was
		// required and there is nowhere it could be.
		if r.Assert.Op == OpExists {
			return violationFor(r, obj, ""), true
		}
		return Violation{}, false
	}
	for _, s := range slots {
		ok, skip := checkOp(r.Assert.Op, s, r.Assert.Values)
		if skip || ok {
			continue
		}
		return violationFor(r, obj, evidence(s)), true
	}
	return Violation{}, false
}

func violationFor(r Rule, obj *unstructured.Unstructured, ev string) Violation {
	return Violation{
		RuleID:    r.ID,
		Level:     r.Level,
		Kind:      r.Match.Kind,
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
		Message:   r.Message,
		Evidence:  ev,
	}
}

// evidence renders the failing value for a human, sanitized at ingress and
// capped. An absent slot has nothing to show.
//
// Note the ordering: matching already happened, on the RAW value. Sanitizing
// before matching would let a control character spliced mid-word evade a glob.
func evidence(s Slot) string {
	if !s.Present {
		return ""
	}
	raw, ok := stringOf(s.Value)
	if !ok {
		return ""
	}
	clean := []rune(safetext.Line(raw))
	if len(clean) > evidenceLimit {
		clean = clean[:evidenceLimit]
	}
	return string(clean)
}

// matches reports whether a rule's match block selects this object.
func matches(m Match, obj *unstructured.Unstructured, nsLabels map[string]map[string]string) bool {
	if len(m.Namespaces) > 0 {
		ns := obj.GetNamespace()
		found := false
		for _, want := range m.Namespaces {
			if ns == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(m.Labels) > 0 && !subset(m.Labels, obj.GetLabels()) {
		return false
	}
	if len(m.NamespaceLabels) > 0 {
		labels, known := nsLabels[obj.GetNamespace()]
		if !known {
			// The Namespace was not read, so kubeagent cannot say the
			// selector matches. Matching blindly would invent a finding.
			return false
		}
		if !subset(m.NamespaceLabels, labels) {
			return false
		}
	}
	return true
}

// namespaceLabelIndex maps a namespace name to its labels, once, rather than
// re-scanning the namespace list for every object.
func namespaceLabelIndex(namespaces []*unstructured.Unstructured) map[string]map[string]string {
	out := make(map[string]map[string]string, len(namespaces))
	for _, ns := range namespaces {
		if ns == nil {
			continue
		}
		out[ns.GetName()] = ns.GetLabels()
	}
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -p 2 -v
go vet ./internal/policy/
```

Expected: PASS; `go vet` clean.

- [ ] **Step 5: Commit**

```bash
git add internal/policy/eval.go internal/policy/eval_test.go
git commit -s -m "policy: add Evaluate

A rule whose kind was unreadable is reported as not evaluated, never as
passing — a refused read that renders as a clean bill of health is worse than
no check at all. One resource yields at most one violation per rule, evidence
is sanitized and capped at 120 runes, and the output is sorted so two runs
over the same cluster render the same bytes."
```

---

### Task 8: Fuzz targets and the generalized import invariant

**Files:**
- Create: `internal/policy/fuzz_test.go`
- Modify: `internal/fuzzgen/imports_test.go`
- Modify: `.github/workflows/fuzz.yml`

**Interfaces:**
- Consumes: `Load`, `Evaluate`, `parsePath`, `resolve`, `globMatch`.
- Produces: nothing importable — four `go test -fuzz` targets and one strengthened invariant test.

Four targets, matching the seven the tree already carries (`FuzzRedactURL`, `FuzzRedactError`, `FuzzDetectors`, `FuzzParseReadyz`, `FuzzCertAssess`, `FuzzParseResponses`, `FuzzClassify`): `FuzzLoadPolicy`, `FuzzEvaluatePolicy`, `FuzzResolvePath`, `FuzzGlob`. Seeds go in `f.Add` calls so a plain `go test` replays them; a real campaign runs nightly.

- [ ] **Step 1: Write the fuzz targets**

Create `internal/policy/fuzz_test.go`:

```go
package policy

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// FuzzLoadPolicy asserts that no byte sequence in a policy file can panic the
// loader, and that anything it accepts is internally consistent — a rule that
// loads must be one Evaluate can run without reaching for a value the loader
// promised was there.
func FuzzLoadPolicy(f *testing.F) {
	f.Add("- id: x\n  match: {kind: Pod}\n  assert: {path: metadata.name, op: exists}\n  level: info\n  message: m\n")
	f.Add("- id: r\n  match: {kind: Deployment}\n  assert: {relation: hasPodDisruptionBudget}\n  level: warning\n  message: m\n")
	f.Add("[]")
	f.Add("")
	f.Add("- id: x\n  match: {kind: ConfigMap}\n  assert: {path: data.token, op: exists}\n  level: info\n  message: m\n")
	f.Add("- id: \x00\n  match: {kind: Pod}\n")

	f.Fuzz(func(t *testing.T, src string) {
		rules, err := Load([]Document{{Source: "fuzz.yaml", Data: []byte(src)}})
		if err != nil {
			if rules != nil {
				t.Errorf("Load returned rules alongside an error")
			}
			return
		}
		for _, r := range rules {
			if r.ID == "" {
				t.Errorf("an accepted rule has no id")
			}
			if !KindSelectable(r.Match.Kind) {
				t.Errorf("an accepted rule selects %q, which is not selectable", r.Match.Kind)
			}
			if r.Match.Kind == "Secret" {
				t.Errorf("Secret was accepted as a selectable kind")
			}
			if r.Match.Kind == "ConfigMap" && readsConfigMapContents(r.Assert.Path) {
				t.Errorf("a ConfigMap contents path was accepted: %q", r.Assert.Path)
			}
			if safetext.Line(r.Message) != r.Message {
				t.Errorf("an accepted rule's message is not sanitized: %q", r.Message)
			}
			hasPath := r.Assert.Path != ""
			hasRelation := r.Assert.Relation != ""
			if hasPath == hasRelation {
				t.Errorf("an accepted rule sets both or neither of path and relation")
			}
			if hasPath {
				if _, perr := parsePath(r.Assert.Path); perr != nil {
					t.Errorf("an accepted rule has an unparseable path %q: %v", r.Assert.Path, perr)
				}
			}
		}
	})
}

// FuzzEvaluatePolicy asserts that no combination of a loadable policy and
// hostile object text can panic a scan, that evidence never carries a raw byte
// from the cluster to a terminal, and that evaluation is deterministic.
func FuzzEvaluatePolicy(f *testing.F) {
	f.Add("- id: x\n  match: {kind: Pod}\n  assert: {path: spec.containers[*].image, op: matches, values: [\"registry.example.com/*\"]}\n  level: critical\n  message: m\n",
		"registry.example.com/app:1.0", "prod", "tier")
	f.Add("- id: x\n  match: {kind: Pod}\n  assert: {path: spec.containers[*].resources.limits.cpu, op: exists}\n  level: warning\n  message: m\n",
		"\x1b[2Japp", "\x00ns", "")

	f.Fuzz(func(t *testing.T, src, image, namespace, nsLabel string) {
		rules, err := Load([]Document{{Source: "fuzz.yaml", Data: []byte(src)}})
		if err != nil {
			return
		}
		obj := &unstructured.Unstructured{Object: map[string]any{
			"kind": "Pod",
			"metadata": map[string]any{
				"name":      "fuzz",
				"namespace": namespace,
				"labels":    map[string]any{"app": image},
			},
			"spec": map[string]any{"containers": []any{
				map[string]any{"name": "a", "image": image},
				map[string]any{"name": "b"},
			}},
		}}
		ns := &unstructured.Unstructured{Object: map[string]any{
			"kind":     "Namespace",
			"metadata": map[string]any{"name": namespace, "labels": map[string]any{"tier": nsLabel}},
		}}
		in := Inputs{
			Objects:    map[string][]*unstructured.Unstructured{"Pod": {obj}},
			Namespaces: []*unstructured.Unstructured{ns},
		}

		violations, notEvaluated := Evaluate(rules, in)

		seen := map[string]bool{}
		for _, v := range violations {
			if safetext.Line(v.Evidence) != v.Evidence {
				t.Errorf("evidence carries an unsanitized byte: %q", v.Evidence)
			}
			if n := len([]rune(v.Evidence)); n > evidenceLimit {
				t.Errorf("evidence is %d runes, over the %d cap", n, evidenceLimit)
			}
			if safetext.Line(v.Message) != v.Message {
				t.Errorf("message carries an unsanitized byte: %q", v.Message)
			}
			key := v.RuleID + "\x1f" + v.Kind + "\x1f" + v.Namespace + "\x1f" + v.Name
			if seen[key] {
				t.Errorf("a resource produced two violations for one rule: %s", key)
			}
			seen[key] = true
		}
		for _, u := range notEvaluated {
			if u.Reason == "" {
				t.Errorf("an unevaluated rule carries no reason")
			}
		}

		// Determinism: the same inputs must produce the same output, in the
		// same order, or the rendered report is a function of map iteration.
		again, againNot := Evaluate(rules, in)
		if len(again) != len(violations) || len(againNot) != len(notEvaluated) {
			t.Fatalf("Evaluate is not deterministic: %d/%d then %d/%d",
				len(violations), len(notEvaluated), len(again), len(againNot))
		}
		for i := range violations {
			if again[i] != violations[i] {
				t.Fatalf("violation %d differs between runs: %#v vs %#v", i, violations[i], again[i])
			}
		}
	})
}

// FuzzResolvePath asserts that no path string can panic the resolver, and
// pins the arity invariant: a path with no wildcard always resolves to
// exactly one slot, present or absent.
func FuzzResolvePath(f *testing.F) {
	f.Add("metadata.name")
	f.Add("spec.containers[*].image")
	f.Add("spec.containers[*].ports[*].containerPort")
	f.Add("")
	f.Add("...")
	f.Add("a[0].b")
	f.Add("\x00.\x00")

	obj := map[string]any{
		"metadata": map[string]any{"name": "web", "labels": map[string]any{"app": "web"}},
		"spec": map[string]any{"containers": []any{
			map[string]any{"image": "app:1.0"},
			map[string]any{},
		}},
	}

	f.Fuzz(func(t *testing.T, path string) {
		segs, err := parsePath(path)
		if err != nil {
			return
		}
		slots := resolve(obj, segs)
		wildcards := 0
		for _, s := range segs {
			if s.wildcard {
				wildcards++
			}
		}
		if wildcards == 0 && len(slots) != 1 {
			t.Errorf("path %q has no wildcard but resolved to %d slots", path, len(slots))
		}
		for _, s := range slots {
			if !s.Present && s.Value != nil {
				t.Errorf("an absent slot carries a value: %#v", s.Value)
			}
		}
	})
}

// FuzzGlob asserts that no pattern can panic or hang the matcher, and pins two
// identities: `*` matches everything, and a metacharacter-free pattern matches
// itself.
func FuzzGlob(f *testing.F) {
	f.Add("registry.example.com/*", "registry.example.com/team/app:1.0")
	f.Add("*", "")
	f.Add("*a*a*a*a*a*a*a*b", strings.Repeat("a", 64))
	f.Add("?", "\x00")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, pattern, s string) {
		globMatch(pattern, s)
		if !globMatch("*", s) {
			t.Errorf("* must match %q", s)
		}
		if !strings.ContainsAny(s, "*?") && !globMatch(s, s) {
			t.Errorf("a metacharacter-free pattern must match itself: %q", s)
		}
	})
}
```

- [ ] **Step 2: Run the fuzz targets in seed-replay mode**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -p 2 -run Fuzz -v
```

Expected: PASS — a plain `go test` replays every `f.Add` seed.

- [ ] **Step 3: Run a short real campaign on each target**

```bash
export PATH=$PATH:/usr/local/go/bin
for target in FuzzLoadPolicy FuzzEvaluatePolicy FuzzResolvePath FuzzGlob; do
  go test ./internal/policy/ -run "^$target$" -fuzz "^$target$" -fuzztime 60s || break
done
```

Expected: each finishes with `elapsed: 1m0s` and no crasher. If one finds a crasher, `go test` writes it under `internal/policy/testdata/fuzz/<target>/` — **fix the bug, keep the crasher file, and commit both**. That corpus entry is the regression test.

- [ ] **Step 4: Generalize the import-invariant test**

`internal/fuzzgen/imports_test.go` holds the tree's only import-invariant test. It already walks the repository with `go/parser` in `parser.ImportsOnly` mode, skipping `.git .github website chaos docs deploy`. Keep that walk. Add this table above `TestNoProductionImport` and the check below inside the existing per-import loop:

```go
// constrained lists packages whose import graph is part of kubeagent's
// security contract, and what each may never reach. The rule is about
// capability: a package that cannot import internal/remediate cannot write to
// a cluster, and one that cannot import internal/explain cannot make a model
// call, no matter what a future edit does inside it.
//
// internal/schemadoc is deliberately the inverse case and is absent here: it
// imports the surface packages to name the document roots, so it transitively
// reaches remediate and explain. The invariants constrain what these packages
// import, not who imports them.
var constrained = map[string][]string{
	"internal/fuzzgen": {"internal/remediate", "internal/explain"},
	"internal/safetext": {"internal/remediate", "internal/explain"},
	"internal/parallel": {"internal/remediate", "internal/explain"},
	"internal/mcp":      {"internal/remediate", "internal/explain"},
	"internal/gate":     {"internal/remediate", "internal/explain"},
	"internal/rbacprofile": {"internal/remediate", "internal/explain"},
	"internal/tui": {
		"internal/remediate", "internal/explain",
		"internal/investigate", "internal/report",
	},
	// internal/policy is pure: it is handed bytes and objects and returns
	// values. It may not reach report/scan/findings either — findings imports
	// scan and scan imports policy, so a policy import of findings would close
	// a cycle, which is why policy declares its own Level type.
	"internal/policy": {
		"internal/remediate", "internal/explain", "internal/investigate",
		"internal/report", "internal/scan", "internal/findings",
	},
}
```

Inside the loop over each file's imports, alongside the existing `p == self` check, replace the fuzzgen-scoped remediate/explain check with the table-driven one:

```go
		slash := filepath.ToSlash(path)
		for dir, forbidden := range constrained {
			if !strings.Contains(slash, "/"+dir+"/") {
				continue
			}
			for _, bad := range forbidden {
				if strings.HasSuffix(p, "/"+bad) {
					t.Errorf("%s imports %s — %s may never reach it", path, p, dir)
				}
			}
		}
```

- [ ] **Step 5: Verify the invariant test actually fails when violated**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/fuzzgen/ -p 2 -run TestNoProductionImport -v
# Prove it discriminates: temporarily add a blank import of
# "github.com/imantaba/kubeagent/internal/report" to internal/policy/eval.go,
# re-run, confirm the failure names internal/policy, then revert.
```

Expected: PASS on the clean tree; FAIL with `internal/policy may never reach it` while the temporary import is in place. Report both outcomes.

- [ ] **Step 6: Register the four targets in the nightly campaign**

`.github/workflows/fuzz.yml` lists one entry per fuzz target. Append four entries in exactly the shape the existing seven use, with package `./internal/policy` and names `FuzzLoadPolicy`, `FuzzEvaluatePolicy`, `FuzzResolvePath`, `FuzzGlob`. Then check the file parses:

```bash
python3 -c "import yaml,sys; d=yaml.safe_load(open('.github/workflows/fuzz.yml')); print('ok')"
grep -c 'internal/policy' .github/workflows/fuzz.yml   # must print 4
```

- [ ] **Step 7: Full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
git diff --stat go.mod go.sum   # must print nothing
```

Expected: everything green, `go.mod`/`go.sum` untouched.

- [ ] **Step 8: Commit**

```bash
git add internal/policy/fuzz_test.go internal/fuzzgen/imports_test.go .github/workflows/fuzz.yml
git commit -s -m "policy: fuzz the loader, the evaluator, the path resolver and the glob

Four targets assert that no policy file and no object text can panic a scan,
that evidence never carries a raw byte from the cluster to a terminal, and
that evaluation is deterministic. The tree's single import-invariant test
becomes a table so internal/policy joins the constrained set: it may never
reach remediate, explain, investigate, report, scan or findings."
```

---

### Task 9: `collect.ByKind` — one read per selected kind

**Files:**
- Create: `internal/collect/bykind.go`
- Create: `internal/collect/bykind_test.go`

**Interfaces:**
- Consumes: `policy.SelectableKinds()` (test-only, for the parity assertion).
- Produces:
  - `func ByKind(ctx context.Context, dyn dynamic.Interface, kind, namespace string) ([]*unstructured.Unstructured, error)`
  - `func KindGVR(kind string) (schema.GroupVersionResource, bool)`

Policy rules select kinds the built-in detectors never read (`NetworkPolicy`,
`ResourceQuota`, `StorageClass`, the webhook configurations), so `scan` needs a
generic read. It already carries a `dynamic.Interface` — `OperatorResources`
takes one — so this is a table plus a `List` call, not new plumbing.

**Why unstructured rather than the typed clientset:** a typed read means 23
distinct call sites and 23 conversions. A dynamic read is one. It also gives
`internal/policy` exactly the shape its paths are written against: the field
names as they appear in `kubectl get -o yaml`.

- [ ] **Step 1: Write the failing tests**

Create `internal/collect/bykind_test.go`:

```go
package collect

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/policy"
)

// dynamicForKinds builds a fake dynamic client that knows the list kinds for
// every kind ByKind can read, so a test can hand it any selectable object.
func dynamicForKinds(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	lists := map[schema.GroupVersionResource]string{}
	for _, kind := range policy.SelectableKinds() {
		gvr, ok := KindGVR(kind)
		if !ok {
			continue
		}
		lists[gvr] = kind + "List"
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objs...)
}

func unstructuredPod(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
	}}
}

func TestByKindListsANamespacedKind(t *testing.T) {
	dyn := dynamicForKinds(unstructuredPod("prod", "web"), unstructuredPod("dev", "api"))

	all, err := ByKind(context.Background(), dyn, "Pod", "")
	if err != nil {
		t.Fatalf("ByKind: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d pods across all namespaces, want 2", len(all))
	}

	scoped, err := ByKind(context.Background(), dyn, "Pod", "prod")
	if err != nil {
		t.Fatalf("ByKind scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].GetName() != "web" {
		t.Fatalf("namespace scoping did not apply: %#v", scoped)
	}
}

// A cluster-scoped kind has no namespace to scope to. Passing one must not
// produce an empty list — that would silently turn "every Node" into "no
// Nodes" whenever the operator ran a namespaced scan.
func TestByKindIgnoresNamespaceOnAClusterScopedKind(t *testing.T) {
	node := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]any{"name": "worker-1"},
	}}
	dyn := dynamicForKinds(node)

	got, err := ByKind(context.Background(), dyn, "Node", "prod")
	if err != nil {
		t.Fatalf("ByKind: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d nodes, want 1 — a namespace must not scope a cluster-scoped kind", len(got))
	}
}

// Secret is not readable through this path at all. policy.Load already refuses
// it, but ByKind is an exported function in a package other code can call, so
// the refusal is enforced here too rather than only one layer up.
func TestByKindRefusesSecret(t *testing.T) {
	dyn := dynamicForKinds()
	got, err := ByKind(context.Background(), dyn, "Secret", "")
	if err == nil {
		t.Fatal("ByKind read Secret; it must refuse")
	}
	if got != nil {
		t.Errorf("ByKind returned objects alongside the refusal: %#v", got)
	}
	for _, action := range dyn.Actions() {
		t.Errorf("ByKind issued an API call for Secret: %#v", action)
	}
}

func TestByKindRefusesAnUnknownKind(t *testing.T) {
	if _, err := ByKind(context.Background(), dynamicForKinds(), "Frobnicator", ""); err == nil {
		t.Fatal("ByKind accepted a kind it has no GVR for")
	}
}

// A refused read must reach the caller as an error, never as an empty list —
// an empty list is indistinguishable from "nothing is wrong", which is exactly
// the silent pass the policy surface must not produce.
func TestByKindSurfacesAForbiddenReadAsAnError(t *testing.T) {
	dyn := dynamicForKinds()
	dyn.PrependReactor("list", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", nil)
	})

	got, err := ByKind(context.Background(), dyn, "NetworkPolicy", "")
	if err == nil {
		t.Fatal("a forbidden list returned no error")
	}
	if got != nil {
		t.Errorf("a forbidden list returned objects: %#v", got)
	}
}

// The GVR table and policy's selectable-kind table must name the same kinds.
// They live in different packages on purpose — policy is pure and holds no
// client — so nothing but this test keeps them from drifting. A kind policy
// can select but collect cannot read would be reported "not evaluated" on
// every cluster, forever.
func TestGVRTableMatchesTheSelectableKinds(t *testing.T) {
	for _, kind := range policy.SelectableKinds() {
		if _, ok := KindGVR(kind); !ok {
			t.Errorf("policy can select %q but collect has no GVR for it", kind)
		}
	}
	for kind := range kindGVRs {
		if !policy.KindSelectable(kind) {
			t.Errorf("collect can read %q but policy cannot select it", kind)
		}
	}
	if len(kindGVRs) != len(policy.SelectableKinds()) {
		t.Errorf("table sizes differ: collect has %d, policy has %d", len(kindGVRs), len(policy.SelectableKinds()))
	}
}
```

**Note on this test file's import of `internal/policy`:** it is test-only and
one-directional — `internal/collect` has no production import of `policy`, and
`policy` imports neither. The parity assertion is the whole point: two tables
that must agree, kept honest by a test rather than by a shared package that
would drag a client into `policy`.

- [ ] **Step 2: Run the tests to watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -p 2 -run 'ByKind|GVRTable' -v
```

Expected: FAIL — `undefined: ByKind`, `undefined: KindGVR`, `undefined: kindGVRs`.

- [ ] **Step 3: Implement `ByKind`**

Create `internal/collect/bykind.go`:

```go
package collect

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// kindGVRs maps every kind a policy rule may select to the resource to list and
// whether that resource is namespaced. The set is exactly the one
// internal/rbacprofile already grants read access to, so a policy rule can
// never need a permission the shipped ClusterRole does not carry — and
// `kubeagent rbac print` keeps describing what kubeagent actually reads.
//
// Secret is absent by design and must stay absent: a rule that could read a
// Secret and quote it as evidence would turn a policy file into an
// exfiltration channel through any report the operator forwards.
var kindGVRs = map[string]kindGVR{
	// core/v1
	"ConfigMap":             {gvr("", "v1", "configmaps"), true},
	"Namespace":             {gvr("", "v1", "namespaces"), false},
	"Node":                  {gvr("", "v1", "nodes"), false},
	"PersistentVolume":      {gvr("", "v1", "persistentvolumes"), false},
	"PersistentVolumeClaim": {gvr("", "v1", "persistentvolumeclaims"), true},
	"Pod":                   {gvr("", "v1", "pods"), true},
	"ResourceQuota":         {gvr("", "v1", "resourcequotas"), true},
	"Service":               {gvr("", "v1", "services"), true},
	// apps/v1
	"DaemonSet":   {gvr("apps", "v1", "daemonsets"), true},
	"Deployment":  {gvr("apps", "v1", "deployments"), true},
	"ReplicaSet":  {gvr("apps", "v1", "replicasets"), true},
	"StatefulSet": {gvr("apps", "v1", "statefulsets"), true},
	// batch/v1
	"CronJob": {gvr("batch", "v1", "cronjobs"), true},
	"Job":     {gvr("batch", "v1", "jobs"), true},
	// discovery.k8s.io/v1
	"EndpointSlice": {gvr("discovery.k8s.io", "v1", "endpointslices"), true},
	// networking.k8s.io/v1
	"Ingress":       {gvr("networking.k8s.io", "v1", "ingresses"), true},
	"IngressClass":  {gvr("networking.k8s.io", "v1", "ingressclasses"), false},
	"NetworkPolicy": {gvr("networking.k8s.io", "v1", "networkpolicies"), true},
	// storage.k8s.io/v1
	"StorageClass": {gvr("storage.k8s.io", "v1", "storageclasses"), false},
	// policy/v1
	"PodDisruptionBudget": {gvr("policy", "v1", "poddisruptionbudgets"), true},
	// autoscaling/v2
	"HorizontalPodAutoscaler": {gvr("autoscaling", "v2", "horizontalpodautoscalers"), true},
	// admissionregistration.k8s.io/v1
	"MutatingWebhookConfiguration":   {gvr("admissionregistration.k8s.io", "v1", "mutatingwebhookconfigurations"), false},
	"ValidatingWebhookConfiguration": {gvr("admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations"), false},
}

type kindGVR struct {
	resource   schema.GroupVersionResource
	namespaced bool
}

func gvr(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

// KindGVR reports the resource to list for a kind, and whether the kind is one
// ByKind will read at all.
func KindGVR(kind string) (schema.GroupVersionResource, bool) {
	e, ok := kindGVRs[kind]
	if !ok {
		return schema.GroupVersionResource{}, false
	}
	return e.resource, true
}

// ByKind lists every object of one kind, optionally scoped to a namespace.
// Read-only: a List call and nothing else.
//
// A namespace is ignored for a cluster-scoped kind rather than producing an
// empty list, so a namespaced scan still evaluates rules about Nodes and
// StorageClasses.
//
// An error means the kind could not be read — refused, unreachable, or timed
// out. The caller must treat that as "not evaluated" and never as "no
// violations": the difference between the two is the whole value of the
// policy surface. The error's text is a boolean to the caller and is never
// rendered; an API error can carry a request URL, and a URL is a credential.
func ByKind(ctx context.Context, dyn dynamic.Interface, kind, namespace string) ([]*unstructured.Unstructured, error) {
	if kind == "Secret" {
		// Belt and braces: policy.Load refuses Secret at load time. This is the
		// second lock, so no future caller can reach a Secret through here.
		return nil, fmt.Errorf("kubeagent never reads Secret objects")
	}
	e, ok := kindGVRs[kind]
	if !ok {
		return nil, fmt.Errorf("kubeagent does not read the kind %q", kind)
	}

	var ri dynamic.ResourceInterface = dyn.Resource(e.resource)
	if e.namespaced && namespace != "" {
		ri = dyn.Resource(e.resource).Namespace(namespace)
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests to watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -p 2 -run 'ByKind|GVRTable' -v
```

Expected: PASS, all six.

- [ ] **Step 5: Prove the parity test discriminates**

```bash
export PATH=$PATH:/usr/local/go/bin
# Temporarily comment out the "StorageClass" line in kindGVRs, re-run, and
# confirm TestGVRTableMatchesTheSelectableKinds fails naming StorageClass.
# Then restore it.
go test ./internal/collect/ -p 2 -run TestGVRTableMatchesTheSelectableKinds -v
```

Expected while the line is commented out: FAIL with `policy can select
"StorageClass" but collect has no GVR for it` and a size mismatch. Report both
runs.

- [ ] **Step 6: Full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
git diff --stat go.mod go.sum   # must print nothing
```

- [ ] **Step 7: Commit**

```bash
git add internal/collect/bykind.go internal/collect/bykind_test.go
git commit -s -m "collect: read any selectable kind through the dynamic client

ByKind lists one kind as unstructured objects so policy paths can be written
the way fields appear in kubectl get -o yaml. The GVR table names exactly the
kinds rbacprofile already grants, so no policy rule can need a permission the
shipped ClusterRole lacks; a test asserts that table against the one in
internal/policy. Secret is refused before any call is issued, and a refused
read surfaces as an error rather than an empty list."
```

---

### Task 10: Bounded reads for a policy run

**Files:**
- Modify: `internal/collect/bykind.go` (add `PolicyObjects`)
- Modify: `internal/collect/bykind_test.go` (add its tests)
- Modify: `internal/scan/workers.go` (export `Workers`)
- Modify: `internal/policy/policy.go` (add `ReadPlan`, `InputsFrom`)
- Modify: `internal/policy/policy_test.go` (add their tests)

**Interfaces:**
- Consumes: `ByKind`, `policy.Kinds`, `policy.Needs`, `parallel.Do`.
- Produces:
  - `func collect.PolicyObjects(ctx context.Context, dyn dynamic.Interface, kinds []string, namespace string, workers int) (map[string][]*unstructured.Unstructured, map[string]bool)`
  - `func scan.Workers() int`
  - `func policy.ReadPlan(rules []Rule) []string`
  - `func policy.InputsFrom(objects map[string][]*unstructured.Unstructured, unreadable map[string]bool) Inputs`

Two surfaces run policy — `scan` and `gate` — and each needs the same three
steps: work out which kinds to read, read them, assemble `Inputs`. Written
twice, they will diverge, and the way they diverge is that one of them forgets
to populate `Unreadable` and starts reporting refused reads as passes. So the
three steps become three named functions here, and Tasks 12 and 14 call them.

**A note on sequencing:** the plan's rule is that `internal/policy` is finished
before any surface wiring. `ReadPlan` and `InputsFrom` are the deliberate
exception, added here rather than in Task 6 because they exist only to serve
the callers. Both are pure — one takes rules and returns strings, the other
takes maps and returns a struct. Neither touches evaluation semantics, so no
earlier task's tests change.

- [ ] **Step 1: Write the failing tests for the policy helpers**

Append to `internal/policy/policy_test.go`:

```go
func TestReadPlanCoversSelectedAndSupportingKinds(t *testing.T) {
	rules := []Rule{
		{ID: "a", Match: Match{Kind: "Pod"}, Assert: Assert{Path: "metadata.name", Op: OpExists}},
		{ID: "b", Match: Match{Kind: "Deployment"}, Assert: Assert{Relation: RelationHasPDB}},
		{ID: "c", Match: Match{Kind: "Deployment"}, Assert: Assert{Relation: RelationHasHPA}},
		{ID: "d", Match: Match{Kind: "Pod", NamespaceLabels: map[string]string{"tier": "prod"}},
			Assert: Assert{Path: "metadata.name", Op: OpExists}},
	}
	got := ReadPlan(rules)
	want := []string{"Deployment", "HorizontalPodAutoscaler", "Namespace", "Pod", "PodDisruptionBudget"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ReadPlan = %v, want %v", got, want)
	}
}

// A rule set that needs no supporting list must not make kubeagent ask for
// one. Reading a PDB list nothing looks at is a permission kubeagent did not
// need and an API call it did not have to make.
func TestReadPlanAsksForNothingItDoesNotNeed(t *testing.T) {
	rules := []Rule{{ID: "a", Match: Match{Kind: "Pod"}, Assert: Assert{Path: "metadata.name", Op: OpExists}}}
	got := ReadPlan(rules)
	if strings.Join(got, ",") != "Pod" {
		t.Errorf("ReadPlan = %v, want just Pod", got)
	}
	if len(ReadPlan(nil)) != 0 {
		t.Error("no rules must plan no reads")
	}
}

// A kind that is both selected and supporting is read once.
func TestReadPlanDeduplicatesASelectedSupportingKind(t *testing.T) {
	rules := []Rule{
		{ID: "a", Match: Match{Kind: "PodDisruptionBudget"}, Assert: Assert{Path: "spec.minAvailable", Op: OpExists}},
		{ID: "b", Match: Match{Kind: "Deployment"}, Assert: Assert{Relation: RelationHasPDB}},
	}
	got := ReadPlan(rules)
	want := []string{"Deployment", "PodDisruptionBudget"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ReadPlan = %v, want %v", got, want)
	}
}

func TestInputsFromRoutesTheSupportingLists(t *testing.T) {
	ns := namespaceObj("prod", map[string]string{"tier": "prod"})
	p := pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web"}})
	h := hpa("prod", "Deployment", "web")
	objects := map[string][]*unstructured.Unstructured{
		"Namespace":               {ns},
		"PodDisruptionBudget":     {p},
		"HorizontalPodAutoscaler": {h},
		"Pod":                     {pod("prod", "a", nil, "docker.example.net/x:1.0")},
	}
	in := InputsFrom(objects, map[string]bool{"Node": true})

	if len(in.Namespaces) != 1 || len(in.PDBs) != 1 || len(in.HPAs) != 1 {
		t.Fatalf("supporting lists not routed: %d namespaces, %d pdbs, %d hpas",
			len(in.Namespaces), len(in.PDBs), len(in.HPAs))
	}
	if len(in.Objects["Pod"]) != 1 {
		t.Error("selected objects did not survive")
	}
	if !in.Unreadable["Node"] {
		t.Error("the unreadable set did not survive — a refused read would render as a pass")
	}
	// The supporting kinds stay in Objects too: a rule may select them.
	if len(in.Objects["PodDisruptionBudget"]) != 1 {
		t.Error("a supporting kind must remain selectable")
	}
}

func TestInputsFromToleratesNilMaps(t *testing.T) {
	in := InputsFrom(nil, nil)
	if len(in.Objects) != 0 || len(in.Unreadable) != 0 {
		t.Error("nil inputs must produce empty ones, not a panic")
	}
}
```

`hpa` is the helper Task 5 added to `helpers_test.go`; `namespaceObj`, `pdb`
and `pod` are the ones Tasks 5 and 7 added. `strings` is already imported by
this file.

- [ ] **Step 2: Run them to watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -p 2 -run 'ReadPlan|InputsFrom' -v
```

Expected: FAIL — `undefined: ReadPlan`, `undefined: InputsFrom`.

- [ ] **Step 3: Implement the two helpers**

Append to `internal/policy/policy.go`:

```go
// ReadPlan returns every kind an evaluation of these rules must read: the
// kinds the rules select, plus the supporting lists their matches and
// relations compare against. Sorted and deduplicated, so the read order — and
// with it the report — does not depend on the order rules were written in.
//
// Nothing is read speculatively. A rule set with no relations and no
// namespaceLabels plans exactly the kinds it selects, which is also the RBAC
// it needs.
func ReadPlan(rules []Rule) []string {
	seen := map[string]bool{}
	for _, k := range Kinds(rules) {
		seen[k] = true
	}
	aux := Needs(rules)
	if aux.Namespaces {
		seen["Namespace"] = true
	}
	if aux.PDBs {
		seen["PodDisruptionBudget"] = true
	}
	if aux.HPAs {
		seen["HorizontalPodAutoscaler"] = true
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// InputsFrom assembles Inputs from what the caller read. The supporting lists
// are looked up by kind rather than passed separately, so a caller cannot
// populate Objects and forget PDBs — and cannot drop the unreadable set, which
// is the difference between "no violations" and "not checked".
//
// A supporting kind stays in Objects as well: a rule may legitimately select
// PodDisruptionBudget and assert something about it.
func InputsFrom(objects map[string][]*unstructured.Unstructured, unreadable map[string]bool) Inputs {
	if objects == nil {
		objects = map[string][]*unstructured.Unstructured{}
	}
	if unreadable == nil {
		unreadable = map[string]bool{}
	}
	return Inputs{
		Objects:    objects,
		Namespaces: objects["Namespace"],
		PDBs:       objects["PodDisruptionBudget"],
		HPAs:       objects["HorizontalPodAutoscaler"],
		Unreadable: unreadable,
	}
}
```

- [ ] **Step 4: Run them to watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policy/ -p 2 -v
```

Expected: PASS, the whole package.

- [ ] **Step 5: Export the worker cap**

The policy reads belong in the same bounded pool the scan's own reads use —
one knob, not two. Append to `internal/scan/workers.go`:

```go
// Workers is the cap the scan's own reads run under, exported so a surface
// that adds reads of its own — the policy evaluation in `scan` and in `gate` —
// runs them under the same bound and the same KUBEAGENT_SCAN_WORKERS knob.
// An operator who turns kubeagent down to one worker means all of it.
func Workers() int { return scanWorkers() }
```

- [ ] **Step 6: Write the failing tests for `PolicyObjects`**

Append to `internal/collect/bykind_test.go`:

```go
func TestPolicyObjectsReadsEveryKindItIsGiven(t *testing.T) {
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"namespace": "prod", "name": "web"},
	}}
	dyn := dynamicForKinds(unstructuredPod("prod", "web"), dep)

	objs, unreadable := PolicyObjects(context.Background(), dyn,
		[]string{"Deployment", "Pod"}, "", 8)

	if len(unreadable) != 0 {
		t.Errorf("nothing was refused, got %v", unreadable)
	}
	if len(objs["Pod"]) != 1 || len(objs["Deployment"]) != 1 {
		t.Fatalf("got %d pods and %d deployments, want 1 each",
			len(objs["Pod"]), len(objs["Deployment"]))
	}
}

// A refused kind must land in the unreadable set and must NOT appear in the
// object map with an empty list. An empty list evaluates to "no violations",
// which is the silent pass this whole surface exists to avoid.
func TestPolicyObjectsSeparatesARefusedKindFromAnEmptyOne(t *testing.T) {
	dyn := dynamicForKinds()
	dyn.PrependReactor("list", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", nil)
	})

	objs, unreadable := PolicyObjects(context.Background(), dyn,
		[]string{"NetworkPolicy", "Pod"}, "", 8)

	if !unreadable["NetworkPolicy"] {
		t.Error("a refused read was not recorded as unreadable")
	}
	if _, present := objs["NetworkPolicy"]; present {
		t.Error("a refused kind must be absent from the object map, not empty in it")
	}
	if unreadable["Pod"] {
		t.Error("one refusal marked an unrelated kind unreadable")
	}
	if _, present := objs["Pod"]; !present {
		t.Error("an empty but readable kind must be present with an empty list")
	}
}

// The reads overlap, so the result must not depend on which one answered
// first. One worker and eight must produce the same map.
func TestPolicyObjectsIsIndependentOfTheSchedule(t *testing.T) {
	kinds := []string{"Deployment", "NetworkPolicy", "Pod", "Service"}
	objects := []runtime.Object{unstructuredPod("prod", "web"), unstructuredPod("dev", "api")}

	one, unreadableOne := PolicyObjects(context.Background(), dynamicForKinds(objects...), kinds, "", 1)
	many, unreadableMany := PolicyObjects(context.Background(), dynamicForKinds(objects...), kinds, "", 8)

	if len(one) != len(many) || len(unreadableOne) != len(unreadableMany) {
		t.Fatalf("worker count changed the result: %d/%d vs %d/%d",
			len(one), len(unreadableOne), len(many), len(unreadableMany))
	}
	for kind := range one {
		if len(one[kind]) != len(many[kind]) {
			t.Errorf("%s: %d objects with one worker, %d with eight", kind, len(one[kind]), len(many[kind]))
		}
	}
}

func TestPolicyObjectsWithNoKindsReadsNothing(t *testing.T) {
	dyn := dynamicForKinds()
	objs, unreadable := PolicyObjects(context.Background(), dyn, nil, "", 8)
	if len(objs) != 0 || len(unreadable) != 0 {
		t.Errorf("got %d kinds and %d refusals for an empty plan", len(objs), len(unreadable))
	}
	if len(dyn.Actions()) != 0 {
		t.Errorf("an empty plan issued %d API calls", len(dyn.Actions()))
	}
}
```

- [ ] **Step 7: Run them to watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -p 2 -run PolicyObjects -v
```

Expected: FAIL — `undefined: PolicyObjects`.

- [ ] **Step 8: Implement `PolicyObjects`**

Append to `internal/collect/bykind.go` (and add
`"github.com/imantaba/kubeagent/internal/parallel"` to its imports):

```go
// PolicyObjects reads every kind in the plan, concurrently and bounded by
// workers, and returns the objects keyed by kind alongside the set of kinds
// that could not be read.
//
// Each read writes only its own slot and returns only its own error, so the
// result is a function of the cluster and not of which read answered first —
// the same construction the scan's phase-1 reads use.
//
// A kind that could not be read is absent from the map AND present in the
// unreadable set. That distinction is the point: an empty list means "read it,
// found none", and an absent kind means "did not find out". Rendering the
// second as the first turns a blind spot into a clean bill of health.
//
// The error itself is deliberately discarded. Any failure — refused,
// unreachable, timed out — means the same thing to a policy run, and an API
// error can carry a request URL, which is a credential.
func PolicyObjects(ctx context.Context, dyn dynamic.Interface, kinds []string,
	namespace string, workers int) (map[string][]*unstructured.Unstructured, map[string]bool) {

	read := make([][]*unstructured.Unstructured, len(kinds))
	errs := parallel.Do(ctx, workers, len(kinds), func(ctx context.Context, i int) error {
		var err error
		read[i], err = ByKind(ctx, dyn, kinds[i], namespace)
		return err
	})

	objects := make(map[string][]*unstructured.Unstructured, len(kinds))
	unreadable := map[string]bool{}
	for i, kind := range kinds {
		if errs[i] != nil {
			unreadable[kind] = true
			continue
		}
		objects[kind] = read[i]
	}
	return objects, unreadable
}
```

- [ ] **Step 9: Run them to watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -p 2 -v
go test ./internal/collect/ -race -run PolicyObjects
```

Expected: PASS both. The race run matters — this is the only new concurrency
in the sub-project.

- [ ] **Step 10: Full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
git diff --stat go.mod go.sum   # must print nothing
```

- [ ] **Step 11: Commit**

```bash
git add internal/collect/bykind.go internal/collect/bykind_test.go \
        internal/scan/workers.go internal/policy/policy.go internal/policy/policy_test.go
git commit -s -m "policy: plan, read and assemble the inputs for an evaluation

ReadPlan names every kind an evaluation must read, including the supporting
lists relations and namespaceLabels compare against, and nothing else.
PolicyObjects reads them through the same bounded worker pool the scan's own
reads use, keeping a refused kind out of the object map and in the unreadable
set so it can never render as a pass. InputsFrom assembles the two into
policy.Inputs, so scan and gate cannot assemble them differently."
```

---

### Task 11: The report surface — `POLICY` in text and JSON

**Files:**
- Modify: `internal/report/report.go`
- Modify: `internal/report/report_test.go`
- Create: `internal/report/policy_test.go`
- Create: `internal/report/testdata/golden-scan-policy.txt` (generated, Step 6)

**Interfaces:**
- Consumes: `policy.Violation`, `policy.Unevaluated`, `policy.Level*`.
- Produces:
  - `type report.PolicyView struct { Rules int; Violations []policy.Violation; NotEvaluated []policy.Unevaluated }`
  - `Input.Policy *PolicyView`, `ScanReport.Policy *PolicyView` (`json:"policy,omitempty"`)

One nil-able field on `Input` and one on `ScanReport`, exactly like
`Capacity` and `GitOps`. A scan with no `--policy` leaves it nil, prints
nothing, and emits no `policy` key — which is what keeps
`testdata/golden-scan.txt` byte-identical.

**Ordering:** the section renders in the order `policy.Evaluate` returned —
sorted by rule id, then kind, namespace, name. Text and JSON therefore agree,
and neither is a function of which read answered first. The header carries the
severity counts so a critical is visible without reading the list.

**Paths are absent by construction.** `PolicyView` has no field for the file a
rule came from. The only place a policy path may appear is stderr (Task 14).

- [ ] **Step 1: Write the failing tests**

Create `internal/report/policy_test.go`:

```go
package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/policy"
)

func policyView() *PolicyView {
	return &PolicyView{
		Rules: 3,
		Violations: []policy.Violation{
			{
				RuleID: "registry-allowlist", Level: policy.LevelCritical,
				Kind: "Pod", Namespace: "prod", Name: "web",
				Message:  "image is not from an allowed registry",
				Evidence: "docker.example.net/app:1.0",
			},
			{
				RuleID: "prod-deployments-need-a-pdb", Level: policy.LevelWarning,
				Kind: "Deployment", Namespace: "prod", Name: "api",
				Message: "no PodDisruptionBudget covers this Deployment",
			},
		},
		NotEvaluated: []policy.Unevaluated{{
			RuleID: "storage-class-is-encrypted", Level: policy.LevelInfo,
			Kind:   "StorageClass",
			Reason: "kubeagent could not read this kind, so the rule was not evaluated",
		}},
	}
}

// The whole point of the nil-able field: a scan with no --policy renders and
// encodes exactly what it did before this sub-project.
func TestNoPolicyRendersNothingAndEncodesNoKey(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "POLICY") {
		t.Error("a scan with no policy printed a POLICY section")
	}

	buf.Reset()
	if err := PrintInventory(Input{}, "json", &buf); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, present := doc["policy"]; present {
		t.Error("a scan with no policy emitted a policy key")
	}
}

func TestPolicySectionRendersViolationsAndBlindRules(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Policy: policyView()}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"POLICY",
		"3 rules",
		"1 critical",
		"1 warning",
		"registry-allowlist",
		"Pod prod/web",
		"image is not from an allowed registry",
		"docker.example.net/app:1.0",
		"prod-deployments-need-a-pdb",
		"Deployment prod/api",
		"not evaluated",
		"storage-class-is-encrypted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("POLICY section is missing %q\n%s", want, out)
		}
	}
}

// A rule that matched nothing is not a violation and must not be listed. The
// section only appears when there is something to say.
func TestPolicySectionIsSilentWhenEverythingPassed(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Policy: &PolicyView{Rules: 4}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "POLICY") {
		t.Fatalf("a clean policy run must still say it ran\n%s", out)
	}
	if !strings.Contains(out, "4 rules") || !strings.Contains(out, "no violations") {
		t.Errorf("a clean policy run must name the rule count and the verdict\n%s", out)
	}
}

// A cluster-scoped violation has no namespace. "Node /worker-1" would be a
// rendering bug that reads as a missing value.
func TestClusterScopedViolationRendersWithoutANamespace(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Policy: &PolicyView{Rules: 1, Violations: []policy.Violation{{
		RuleID: "nodes-are-labelled", Level: policy.LevelInfo,
		Kind: "Node", Name: "worker-1", Message: "no topology label",
	}}}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Node worker-1") {
		t.Errorf("cluster-scoped violation rendered oddly\n%s", out)
	}
	if strings.Contains(out, "Node /worker-1") {
		t.Errorf("an empty namespace leaked a stray separator\n%s", out)
	}
}

func TestPolicyJSONCarriesTheWholeView(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{Policy: policyView()}, "json", &buf); err != nil {
		t.Fatal(err)
	}
	var doc ScanReport
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Policy == nil {
		t.Fatal("policy view missing from the JSON document")
	}
	if doc.Policy.Rules != 3 || len(doc.Policy.Violations) != 2 || len(doc.Policy.NotEvaluated) != 1 {
		t.Fatalf("policy view = %#v", doc.Policy)
	}
	if doc.Policy.Violations[0].Evidence != "docker.example.net/app:1.0" {
		t.Errorf("evidence did not survive the round trip: %q", doc.Policy.Violations[0].Evidence)
	}
}

// A policy file's path is a credential. Nothing in the rendered document may
// carry one, in either format.
func TestNoPolicyPathReachesTheReport(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		var buf bytes.Buffer
		if err := PrintInventory(Input{Policy: policyView()}, format, &buf); err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{"/etc/", "/home/", ".yaml", ".yml"} {
			if strings.Contains(buf.String(), needle) {
				t.Errorf("%s output contains %q — a policy path must never be rendered", format, needle)
			}
		}
	}
}
```

- [ ] **Step 2: Run them to watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ -p 2 -run Policy -v
```

Expected: FAIL — `undefined: PolicyView`, `in.Policy undefined`.

- [ ] **Step 3: Add the view and the two fields**

In `internal/report/report.go`, add the import
`"github.com/imantaba/kubeagent/internal/policy"`, then the type beside
`InvestigationView`:

```go
// PolicyView is the outcome of a policy run: how many rules ran, what they
// found, and which of them could not run at all.
//
// NotEvaluated is not a footnote. A rule whose kind kubeagent may not read has
// not passed — it has not been checked — and a document that renders the two
// the same way is worse than one that omits the check entirely.
//
// There is deliberately no field for the file a rule came from: a filesystem
// path is a credential, and this document is written to be forwarded.
type PolicyView struct {
	Rules        int                  `json:"rules"`
	Violations   []policy.Violation   `json:"violations,omitempty"`
	NotEvaluated []policy.Unevaluated `json:"notEvaluated,omitempty"`
}
```

Add to `Input`, beside `Capacity`:

```go
	// Policy is the custom-check view (opt-in --policy). Nil when the flag is
	// absent, so a default scan's text and JSON are unchanged.
	Policy *PolicyView
```

Add to `ScanReport`, after `QuotaIssues`:

```go
	Policy *PolicyView `json:"policy,omitempty"`
```

and to the `ScanReport{...}` literal in `PrintInventory`'s JSON branch:

```go
			Policy: in.Policy,
```

- [ ] **Step 4: Render the text section**

Add the call in `PrintInventory`'s text branch, immediately after the
`printCapacity` call and before `printBlindSpots`:

```go
	if err := printPolicy(in.Policy, w); err != nil {
		return err
	}
```

And the renderer, beside `printCapacity`:

```go
// printPolicy renders the operator's own checks. Unlike the advisory sections
// it prints even when it found nothing: the operator asked for these rules by
// name, and silence would be indistinguishable from the flag not working.
func printPolicy(v *PolicyView, w io.Writer) error {
	if v == nil {
		return nil
	}

	var critical, warning, info int
	for _, vi := range v.Violations {
		switch vi.Level {
		case policy.LevelCritical:
			critical++
		case policy.LevelWarning:
			warning++
		case policy.LevelInfo:
			info++
		}
	}

	verdict := "no violations"
	if len(v.Violations) > 0 {
		parts := make([]string, 0, 3)
		if critical > 0 {
			parts = append(parts, fmt.Sprintf("%d critical", critical))
		}
		if warning > 0 {
			parts = append(parts, fmt.Sprintf("%d warning", warning))
		}
		if info > 0 {
			parts = append(parts, fmt.Sprintf("%d info", info))
		}
		verdict = strings.Join(parts, ", ")
	}
	if _, err := fmt.Fprintf(w, "POLICY  (%d %s, %s)\n",
		v.Rules, plural(v.Rules, "rule", "rules"), verdict); err != nil {
		return err
	}

	for _, vi := range v.Violations {
		if _, err := fmt.Fprintf(w, "  ✗ %s  %s  %s\n", vi.Level, vi.RuleID, policyTarget(vi)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      %s\n", vi.Message); err != nil {
			return err
		}
		if vi.Evidence != "" {
			if _, err := fmt.Fprintf(w, "      value: %s\n", vi.Evidence); err != nil {
				return err
			}
		}
	}
	for _, u := range v.NotEvaluated {
		if _, err := fmt.Fprintf(w, "  ⚠ not evaluated  %s  %s\n", u.RuleID, u.Kind); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      %s\n", u.Reason); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintln(w)
	return err
}

// policyTarget names the offending object. A cluster-scoped kind has no
// namespace, so it gets no separator rather than an empty one.
func policyTarget(v policy.Violation) string {
	if v.Namespace == "" {
		return v.Kind + " " + v.Name
	}
	return v.Kind + " " + v.Namespace + "/" + v.Name
}
```

`plural` is the existing helper at `internal/report/report.go:489` —
`plural(n int, one, many string) string`. Do not add another.

- [ ] **Step 5: Run the tests to watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ -p 2 -run Policy -v
```

Expected: PASS, all six.

- [ ] **Step 6: Add the second golden fixture**

The existing golden must not move; a policy run gets its own. Append to
`internal/report/golden_test.go`:

```go
const goldenPolicyPath = "testdata/golden-scan-policy.txt"

// goldenPolicyInput is the same broad snapshot with a policy run attached, so
// the POLICY section is pinned byte-for-byte without touching golden-scan.txt.
// A scan with no --policy renders the original, which is why that file stays
// byte-identical through this whole sub-project.
func goldenPolicyInput(now time.Time) Input {
	in := goldenInput(now)
	in.Policy = &PolicyView{
		Rules: 4,
		Violations: []policy.Violation{
			{
				RuleID: "images-come-from-the-allowlist", Level: policy.LevelCritical,
				Kind: "Pod", Namespace: "shop", Name: "checkout-7d9f",
				Message:  "image is not from an allowed registry",
				Evidence: "docker.example.net/checkout:1.4.2",
			},
			{
				RuleID: "nodes-carry-a-topology-label", Level: policy.LevelInfo,
				Kind: "Node", Name: "worker-2",
				Message: "no topology.kubernetes.io/zone label",
			},
			{
				RuleID: "prod-deployments-need-a-pdb", Level: policy.LevelWarning,
				Kind: "Deployment", Namespace: "shop", Name: "checkout",
				Message: "no PodDisruptionBudget covers this Deployment",
			},
		},
		NotEvaluated: []policy.Unevaluated{{
			RuleID: "storage-classes-are-encrypted", Level: policy.LevelWarning,
			Kind:   "StorageClass",
			Reason: "kubeagent could not read this kind, so the rule was not evaluated",
		}},
	}
	return in
}

func TestGoldenPolicyScanOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(goldenPolicyInput(goldenNow), "text", &buf); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if *update {
		if err := os.WriteFile(goldenPolicyPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPolicyPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("policy scan text output changed:\n%s\n\n"+
			"If this change is intended, run:\n"+
			"  go test ./internal/report -run TestGoldenPolicyScanOutput -update",
			firstDiff(string(want), string(got)))
	}
}
```

Add `"github.com/imantaba/kubeagent/internal/policy"` to that file's imports.
Note that the failure message deliberately does **not** mention the demo GIF or
the quickstart: neither renders a policy run.

Generate the fixture, then read it:

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report -run TestGoldenPolicyScanOutput -update
go test ./internal/report -p 2 -run Golden -v
cat internal/report/testdata/golden-scan-policy.txt
```

Expected: both golden tests PASS. Read the generated POLICY section and confirm
it is legible — the levels line up, the cluster-scoped Node row has no stray
`/`, and the not-evaluated rule reads as unchecked rather than as clean.

- [ ] **Step 7: Prove the original golden did not move**

```bash
git status --short internal/report/testdata/golden-scan.txt   # must print nothing
git diff --stat internal/report/testdata/golden-scan.txt      # must print nothing
```

Expected: no output from either. If `golden-scan.txt` changed, something
rendered for a nil `Policy` — stop and fix that before continuing; the demo GIF
and quickstart depend on this file.

- [ ] **Step 8: Full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
git diff --stat go.mod go.sum   # must print nothing
```

- [ ] **Step 9: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go \
        internal/report/policy_test.go internal/report/golden_test.go \
        internal/report/testdata/golden-scan-policy.txt
git commit -s -m "report: render policy violations in text and JSON

PolicyView is one nil-able field on Input and one on ScanReport, so a scan
with no --policy renders and encodes exactly what it did before and
golden-scan.txt stays byte-identical. Rules that could not be evaluated are
listed as unchecked rather than folded into the pass count, and the view
carries no field for the file a rule came from — a path is a credential and
this document is written to be forwarded. A second golden fixture pins the
POLICY section."
```

---

### Task 12: The gate surface — policy findings fail a build

**Files:**
- Modify: `internal/findings/findings.go`
- Modify: `internal/findings/findings_test.go`
- Modify: `internal/gate/gate.go`
- Modify: `internal/gate/gate_test.go`
- Modify: `internal/sarif/sarif.go`
- Modify: `internal/sarif/sarif_test.go`

**Interfaces:**
- Consumes: `policy.Violation`, `policy.Unevaluated`, `policy.Level*`.
- Produces:
  - `func findings.FromPolicy(violations []policy.Violation, notEvaluated []policy.Unevaluated) []Finding`
  - `gate.Options.PolicyViolations []policy.Violation`
  - `gate.Options.PolicyNotEvaluated []policy.Unevaluated`

A policy violation is a finding like any other: it flows through
`--fail-on`, through `--wait-for` scoping, into the JSON verdict and into
SARIF. No new verdict field, no new exit code.

**`Issue` is `policy/<ruleID>`.** `internal/sarif` uses `Finding.Issue` as the
SARIF rule id verbatim, so the prefix is what keeps an operator's rule named
`OOMKilled` from colliding with the detector of that name in a code-scanning
dashboard. The rule-id charset the loader enforces (Task 6) is what makes the
result a valid SARIF id.

**A rule that could not be evaluated becomes a failing finding at its own
level**, not a `Blindspot`. The two mechanisms would contradict each other —
`Inconclusive` yields exit 2, a failing finding yields exit 1 — and the spec
chose the failure. The reasoning: `--allow-partial-read` exists to waive a
blind spot the operator accepted, and an operator who wrote a rule did not
accept not running it.

**`gate.Verdict` gains one field: `policyNotEvaluated`.** Violations need no
field — they land in the existing `failing` and `reported` arrays like any
finding. Rules that could not run do need one: a CI consumer reading
`verdict.json` must be able to tell "the rule was checked and the cluster
violated it" from "the rule never ran" without parsing English out of a
finding's reason, because only the second means the gate is not enforcing what
the operator wrote. The field is additive and `omitempty`, so a verdict from a
run with no `--policy` is byte-identical to today's. This is the gate shape
change the spec's `GateVersion` 1.0 → 1.1 bump refers to; Task 15 confirms it
with `TestSchemaDrift`.

- [ ] **Step 1: Write the failing tests for `FromPolicy`**

Append to `internal/findings/findings_test.go`:

```go
func TestFromPolicyMapsLevelsAndPrefixesTheIssue(t *testing.T) {
	got := FromPolicy([]policy.Violation{
		{RuleID: "registry-allowlist", Level: policy.LevelCritical, Kind: "Pod",
			Namespace: "prod", Name: "web", Message: "image is not from an allowed registry",
			Evidence: "docker.example.net/app:1.0"},
		{RuleID: "pdb-required", Level: policy.LevelWarning, Kind: "Deployment",
			Namespace: "prod", Name: "api", Message: "no PodDisruptionBudget covers this Deployment"},
		{RuleID: "zone-label", Level: policy.LevelInfo, Kind: "Node",
			Name: "worker-1", Message: "no topology label"},
	}, nil)

	if len(got) != 3 {
		t.Fatalf("got %d findings, want 3", len(got))
	}
	wantLevels := []Level{Critical, Warning, Info}
	for i, want := range wantLevels {
		if got[i].Level != want {
			t.Errorf("finding %d level = %v, want %v", i, got[i].Level, want)
		}
	}
	if got[0].Issue != "policy/registry-allowlist" {
		t.Errorf("Issue = %q, want the policy/ prefix", got[0].Issue)
	}
	if !strings.Contains(got[0].Reason, "image is not from an allowed registry") ||
		!strings.Contains(got[0].Reason, "docker.example.net/app:1.0") {
		t.Errorf("Reason = %q, want the message and the evidence", got[0].Reason)
	}
	if got[1].Reason != "no PodDisruptionBudget covers this Deployment" {
		t.Errorf("a violation with no evidence gained a suffix: %q", got[1].Reason)
	}
	if got[2].Namespace != "" || got[2].Name != "worker-1" {
		t.Errorf("cluster-scoped violation = %#v", got[2])
	}
}

// A rule kubeagent could not run must reach the gate as a finding at the
// rule's own level. Dropping it — or demoting it to info — would let a
// refused read pass a build that the operator wrote a critical rule to stop.
func TestFromPolicyTurnsAnUnevaluatedRuleIntoAFindingAtItsOwnLevel(t *testing.T) {
	got := FromPolicy(nil, []policy.Unevaluated{{
		RuleID: "storage-encrypted", Level: policy.LevelCritical, Kind: "StorageClass",
		Reason: "kubeagent could not read this kind, so the rule was not evaluated",
	}})

	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	f := got[0]
	if f.Level != Critical {
		t.Errorf("Level = %v, want Critical — a blind rule keeps its own severity", f.Level)
	}
	if f.Issue != "policy/storage-encrypted" {
		t.Errorf("Issue = %q", f.Issue)
	}
	if f.Kind != "StorageClass" {
		t.Errorf("Kind = %q", f.Kind)
	}
	if f.Name != "" {
		t.Errorf("Name = %q, want empty — no object was evaluated", f.Name)
	}
	if !strings.Contains(f.Reason, "not evaluated") {
		t.Errorf("Reason = %q, want kubeagent's own words", f.Reason)
	}
}

func TestFromPolicyWithNothingReturnsNothing(t *testing.T) {
	if got := FromPolicy(nil, nil); len(got) != 0 {
		t.Errorf("got %d findings from no policy results", len(got))
	}
}
```

Add `"github.com/imantaba/kubeagent/internal/policy"` to that file's imports.

- [ ] **Step 2: Run them to watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/findings/ -p 2 -run FromPolicy -v
```

Expected: FAIL — `undefined: FromPolicy`.

- [ ] **Step 3: Implement `FromPolicy`**

Append to `internal/findings/findings.go` (and add the `policy` import):

```go
// policyLevel maps a policy level onto the gate's severity ordering. The two
// types stay separate on purpose: internal/policy may not import this package
// (findings imports scan, scan imports policy), and a policy file's vocabulary
// should not be a Go ordinal.
func policyLevel(l policy.Level) Level {
	switch l {
	case policy.LevelCritical:
		return Critical
	case policy.LevelWarning:
		return Warning
	default:
		return Info
	}
}

// FromPolicy projects a policy run into findings.
//
// Issue is "policy/<ruleID>": internal/sarif uses Issue as the SARIF rule id
// verbatim, and without the prefix an operator's rule named "OOMKilled" would
// merge with the detector of that name in a code-scanning dashboard.
//
// A rule that could not be evaluated becomes a finding at its own level rather
// than a Blindspot. --allow-partial-read exists to waive a blind spot the
// operator accepted; an operator who wrote a rule did not accept not running
// it. It carries no Name, because no object was examined.
func FromPolicy(violations []policy.Violation, notEvaluated []policy.Unevaluated) []Finding {
	out := make([]Finding, 0, len(violations)+len(notEvaluated))

	for _, v := range violations {
		reason := v.Message
		if v.Evidence != "" {
			reason = strings.TrimSpace(reason + " (" + v.Evidence + ")")
		}
		out = append(out, Finding{
			Level: policyLevel(v.Level), Kind: v.Kind,
			Namespace: v.Namespace, Name: v.Name,
			Issue: "policy/" + v.RuleID, Reason: reason,
		})
	}
	for _, u := range notEvaluated {
		out = append(out, Finding{
			Level: policyLevel(u.Level), Kind: u.Kind,
			Issue: "policy/" + u.RuleID, Reason: u.Reason,
		})
	}
	return out
}
```

- [ ] **Step 4: Run them to watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/findings/ -p 2 -v
```

Expected: PASS.

- [ ] **Step 5: Write the failing gate tests**

Append to `internal/gate/gate_test.go`:

```go
func TestPolicyViolationFailsTheGate(t *testing.T) {
	v := Decide(scan.Result{}, Options{
		FailOn: findings.Warning,
		PolicyViolations: []policy.Violation{{
			RuleID: "registry-allowlist", Level: policy.LevelCritical, Kind: "Pod",
			Namespace: "prod", Name: "web", Message: "image is not from an allowed registry",
		}},
	})
	if v.Code != CodeFail {
		t.Fatalf("exit code = %d, want %d", v.Code, CodeFail)
	}
	if len(v.Failing) != 1 || v.Failing[0].Issue != "policy/registry-allowlist" {
		t.Fatalf("failing = %#v", v.Failing)
	}
}

// Below --fail-on it is reported, not failed — the same contract every other
// finding class has.
func TestPolicyViolationBelowTheThresholdIsReportedOnly(t *testing.T) {
	v := Decide(scan.Result{}, Options{
		FailOn: findings.Critical,
		PolicyViolations: []policy.Violation{{
			RuleID: "zone-label", Level: policy.LevelInfo, Kind: "Node",
			Name: "worker-1", Message: "no topology label",
		}},
	})
	if v.Code != CodePass {
		t.Fatalf("exit code = %d, want %d", v.Code, CodePass)
	}
	if len(v.Reported) != 1 {
		t.Fatalf("reported = %#v", v.Reported)
	}
}

// The whole reason the surface exists: a rule that could not run must not read
// as a rule that passed.
func TestUnevaluatedRuleFailsTheGate(t *testing.T) {
	v := Decide(scan.Result{}, Options{
		FailOn: findings.Warning,
		PolicyNotEvaluated: []policy.Unevaluated{{
			RuleID: "storage-encrypted", Level: policy.LevelCritical, Kind: "StorageClass",
			Reason: "kubeagent could not read this kind, so the rule was not evaluated",
		}},
	})
	if v.Code != CodeFail {
		t.Fatalf("exit code = %d, want %d — an unevaluated rule is not a pass", v.Code, CodeFail)
	}
	if len(v.Failing) != 1 || v.Failing[0].Issue != "policy/storage-encrypted" {
		t.Fatalf("failing = %#v", v.Failing)
	}
	// The verdict must also say it as data: a consumer cannot be asked to parse
	// English out of a finding to learn that a rule never ran.
	if len(v.PolicyNotEvaluated) != 1 || v.PolicyNotEvaluated[0].RuleID != "storage-encrypted" {
		t.Fatalf("PolicyNotEvaluated = %#v", v.PolicyNotEvaluated)
	}
}

// --wait-for narrows the gate to one rollout. A policy violation elsewhere in
// the cluster is reported but must not fail that rollout's gate, exactly as a
// detector finding elsewhere does not.
func TestPolicyViolationOutOfScopeDoesNotFailAScopedGate(t *testing.T) {
	v := Decide(scan.Result{}, Options{
		FailOn:    findings.Warning,
		ScopeKind: "Deployment", ScopeName: "api", ScopeNamespace: "prod",
		PolicyViolations: []policy.Violation{{
			RuleID: "registry-allowlist", Level: policy.LevelCritical, Kind: "Pod",
			Namespace: "other", Name: "web", Message: "image is not from an allowed registry",
		}},
	})
	if v.Code != CodePass {
		t.Fatalf("exit code = %d, want %d", v.Code, CodePass)
	}
	if len(v.Reported) != 1 {
		t.Fatalf("an out-of-scope violation must still be reported: %#v", v.Reported)
	}
}

func TestNoPolicyLeavesTheVerdictUnchanged(t *testing.T) {
	v := Decide(scan.Result{}, Options{FailOn: findings.Warning})
	if v.Code != CodePass || len(v.Failing) != 0 || len(v.Reported) != 0 {
		t.Fatalf("a gate with no policy changed: %#v", v)
	}
	if v.PolicyNotEvaluated != nil {
		t.Errorf("PolicyNotEvaluated = %#v, want nil so the JSON key stays absent", v.PolicyNotEvaluated)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "policyNotEvaluated") {
		t.Errorf("a no-policy verdict encoded the new key:\n%s", out)
	}
}
```

Add `"github.com/imantaba/kubeagent/internal/policy"` to that file's imports.

- [ ] **Step 6: Run them to watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/gate/ -p 2 -run Policy -v
```

Expected: FAIL — `unknown field PolicyViolations in struct literal`.

- [ ] **Step 7: Wire the gate**

Add to `gate.Options`, after `AllowPartialRead`:

```go
	// PolicyViolations and PolicyNotEvaluated are the outcome of the --policy
	// run, both empty when no policy file was given. They join the flattened
	// findings, so --fail-on and --wait-for scoping apply to them unchanged.
	PolicyViolations   []policy.Violation
	PolicyNotEvaluated []policy.Unevaluated
```

Add to `gate.Verdict`, after `Blindspots`:

```go
	// PolicyNotEvaluated lists the rules kubeagent could not run. Violations
	// need no field of their own — they are findings, and they are already in
	// Failing or Reported. A rule that never ran is different in kind: it is the
	// one outcome where the verdict understates what the operator asked for, and
	// a consumer must be able to see it as data rather than by reading English
	// out of a finding's reason. Each also appears in Failing or Reported at its
	// own level; this field says which of them never ran.
	PolicyNotEvaluated []policy.Unevaluated `json:"policyNotEvaluated,omitempty"`
```

Populate it in `Decide`, alongside the existing `Blindspots` assignment:

```go
	v.PolicyNotEvaluated = opts.PolicyNotEvaluated
```

In `Decide`, replace the `for _, f := range findings.Flatten(res) {` loop
header with a flattening step that includes the policy results:

```go
	all := findings.Flatten(res)
	all = append(all, findings.FromPolicy(opts.PolicyViolations, opts.PolicyNotEvaluated)...)
	findings.Sort(all)

	for _, f := range all {
```

The body of the loop is unchanged. `findings.Sort` is stable and the same
total order `Flatten` already applied, so a gate with no policy produces the
identical list it did before.

Add the `policy` import to `internal/gate/gate.go`.

**Invariant check:** `internal/gate` must never import `internal/remediate` or
`internal/explain`. It now imports `internal/policy`, which imports neither —
the table in `internal/fuzzgen/imports_test.go` (Task 8) covers both packages.

- [ ] **Step 8: Run the gate tests to watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/gate/ -p 2 -v
```

Expected: PASS, including every pre-existing test — the no-policy path is
untouched.

- [ ] **Step 9: Give an object-less finding a clean SARIF URI**

An unevaluated rule has no object, so `Finding.Name` is empty and
`artifactURI` would render a trailing slash. In `internal/sarif/sarif.go`,
replace `artifactURI` with:

```go
// artifactURI names the object a finding is about. A finding with no object —
// a policy rule that could not be evaluated against anything — names its kind
// alone rather than a URI with a trailing empty segment.
func artifactURI(f findings.Finding) string {
	switch {
	case f.Name == "":
		return fmt.Sprintf("k8s://%s", f.Kind)
	case f.Namespace == "":
		return fmt.Sprintf("k8s://%s/%s", f.Kind, f.Name)
	}
	return fmt.Sprintf("k8s://%s/%s/%s", f.Namespace, f.Kind, f.Name)
}
```

Append to `internal/sarif/sarif_test.go`:

```go
func TestPolicyFindingsRenderAsSARIFRules(t *testing.T) {
	v := gate.Decide(scan.Result{}, gate.Options{
		FailOn: findings.Warning,
		PolicyViolations: []policy.Violation{{
			RuleID: "registry-allowlist", Level: policy.LevelCritical, Kind: "Pod",
			Namespace: "prod", Name: "web", Message: "image is not from an allowed registry",
		}},
		PolicyNotEvaluated: []policy.Unevaluated{{
			RuleID: "storage-encrypted", Level: policy.LevelCritical, Kind: "StorageClass",
			Reason: "kubeagent could not read this kind, so the rule was not evaluated",
		}},
	})
	out, err := Render(v, "test")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)

	for _, want := range []string{
		`"policy/registry-allowlist"`,
		`"policy/storage-encrypted"`,
		`k8s://prod/Pod/web`,
		`k8s://StorageClass`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("SARIF is missing %q\n%s", want, doc)
		}
	}
	if strings.Contains(doc, `k8s://StorageClass/"`) {
		t.Error("an object-less finding rendered a trailing empty path segment")
	}
	// A policy file's path is a credential and must not reach a document that
	// gets uploaded to a code-scanning dashboard.
	for _, needle := range []string{".yaml", ".yml", "/etc/", "/home/"} {
		if strings.Contains(doc, needle) {
			t.Errorf("SARIF contains %q — a policy path must never be rendered", needle)
		}
	}
}
```

Add the `policy` import to that test file if it is not already there.

- [ ] **Step 10: Run the SARIF tests**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/sarif/ -p 2 -v
```

Expected: PASS. If a golden SARIF fixture under `internal/sarif/testdata`
changed, the `artifactURI` edit altered an existing finding's URI — that would
mean an existing finding has an empty `Name`, which is a real bug worth
reporting rather than regenerating around. Stop and report it.

- [ ] **Step 11: Full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
git diff --stat go.mod go.sum   # must print nothing
```

- [ ] **Step 12: Commit**

```bash
git add internal/findings/findings.go internal/findings/findings_test.go \
        internal/gate/gate.go internal/gate/gate_test.go \
        internal/sarif/sarif.go internal/sarif/sarif_test.go
git commit -s -m "gate: fail a build on a policy violation

Policy results become findings and flow through --fail-on, --wait-for scoping,
the JSON verdict and SARIF unchanged; the issue is policy/<ruleID> so an
operator's rule cannot collide with a detector of the same name in a
code-scanning dashboard. A rule kubeagent could not evaluate fails at its own
level rather than becoming a waivable blind spot: --allow-partial-read waives
what the operator accepted, and writing a rule is not accepting that it might
not run."
```

---

### Task 13: The HTML surface — a POLICY section in the shareable report

**Files:**
- Modify: `internal/htmlreport/htmlreport.go`
- Modify: `internal/htmlreport/report.html.tmpl`
- Modify: `internal/htmlreport/htmlreport_test.go`
- Modify: `internal/htmlreport/golden_test.go`
- Regenerate: `internal/htmlreport/testdata/golden-report.html`

**Interfaces:**
- Consumes: `report.Input.Policy *report.PolicyView` (Task 11).
- Produces: nothing other packages call. `Render`'s signature is unchanged.

The HTML report is a **scan** surface, so policy appears the way it appears in
the text report: **its own section, outside the findings table and outside the
severity tally.** A violation never moves the cluster verdict (spec, "Severity
and the cluster verdict"), and the header pills are that verdict's tally. Rows
in the findings table would be counted there. The gate is the surface where
violations become findings; that is Task 12 and it is a different document.

**The section renders nothing when `in.Report.Policy` is nil.** A scan with no
`--policy` produces the same bytes it produced before this task — that is gate
evidence item 1.

**No path, ever.** The section names rule ids, kinds and objects. The policy
file's path is not in `report.PolicyView`, so there is nothing to leak here by
accident; the test asserts it anyway, because this document is the one written
to be forwarded.

`safeReason` is **not** reused. It exists to classify an API server's error
text, which kubeagent does not control. An `Unevaluated.Reason` is kubeagent's
own sentence, fixed at three constants in Task 7, and re-classifying a string
this binary wrote would be theatre.

- [ ] **Step 1: Write the failing tests**

Append to `internal/htmlreport/htmlreport_test.go`:

```go
func policyInput() Input {
	in := Input{Version: "test", Namespace: "prod"}
	in.Report.Now = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	in.Report.Policy = &report.PolicyView{
		Rules: 4,
		Violations: []policy.Violation{
			{RuleID: "registry-allowlist", Level: policy.LevelCritical, Kind: "Pod",
				Namespace: "prod", Name: "web-7d9f", Message: "image is not from an allowed registry",
				Evidence: "docker.example.net/app:1.0"},
			{RuleID: "pdb-required", Level: policy.LevelWarning, Kind: "Deployment",
				Namespace: "prod", Name: "api", Message: "no PodDisruptionBudget covers this Deployment"},
			{RuleID: "zone-label", Level: policy.LevelInfo, Kind: "Node", Name: "worker-1",
				Message: "no topology label"},
		},
		NotEvaluated: []policy.Unevaluated{{
			RuleID: "storage-encrypted", Level: policy.LevelCritical, Kind: "StorageClass",
			Reason: "kubeagent could not read this kind, so the rule was not evaluated",
		}},
	}
	return in
}

func TestPolicySectionRendersRulesViolationsAndUnevaluated(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, policyInput()); err != nil {
		t.Fatal(err)
	}
	doc := buf.String()

	for _, want := range []string{
		"Policy", "registry-allowlist", "web-7d9f", "image is not from an allowed registry",
		"docker.example.net/app:1.0", "pdb-required", "zone-label", "worker-1",
		"storage-encrypted", "not evaluated",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the policy section is missing %q", want)
		}
	}
}

// A violation is not a cluster finding: it must not be counted in the header
// pills, which tally kubeagent's own judgement about cluster health.
func TestPolicyViolationsDoNotEnterTheSeverityTally(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, policyInput()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "0 critical") {
		t.Error("a critical policy violation was counted in the header tally")
	}
}

// Gate evidence item 1 in miniature: with no policy the document is what it
// was before this task existed.
func TestNoPolicyRendersNoPolicySection(t *testing.T) {
	in := policyInput()
	in.Report.Policy = nil

	var buf bytes.Buffer
	if err := Render(&buf, in); err != nil {
		t.Fatal(err)
	}
	// The stylesheet always carries a .policy rule, so match the section
	// element and its content rather than the bare word.
	for _, absent := range []string{
		`<section class="policy">`, "<h2>Policy</h2>", "rules evaluated",
		"registry-allowlist", "storage-encrypted",
	} {
		if strings.Contains(buf.String(), absent) {
			t.Errorf("a scan with no --policy rendered %q", absent)
		}
	}
}

// The document is written to be forwarded. A policy file lives at a path on the
// operator's machine, and that path is a credential.
func TestPolicySectionCarriesNoPath(t *testing.T) {
	in := policyInput()
	in.Report.Policy.Violations[0].Evidence = "/etc/kubeagent/policies/prod.yaml"

	var buf bytes.Buffer
	if err := Render(&buf, in); err != nil {
		t.Fatal(err)
	}
	// Evidence is cluster text, not a path — but if a future change ever routes
	// a path into it, this document must not be the place it surfaces.
	for _, needle := range []string{"/etc/", "/home/", "kubeconfig"} {
		if strings.Contains(buf.String(), needle) {
			t.Errorf("the document rendered %q", needle)
		}
	}
}
```

Add `"github.com/imantaba/kubeagent/internal/policy"` and
`"github.com/imantaba/kubeagent/internal/report"` to that file's imports if
they are not already there.

`TestPolicySectionCarriesNoPath` will fail until Step 3 adds the filter. That
is deliberate: the assertion is the requirement, not the implementation.

- [ ] **Step 2: Run them to watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/htmlreport/ -p 2 -run Policy -v
```

Expected: FAIL — `in.Report.Policy` undefined until Task 11 is in the tree
(it is; this task follows it), then FAIL on the missing section text.

- [ ] **Step 3: Add the view types and populate them**

In `internal/htmlreport/htmlreport.go`, add to `view` after `Blind`:

```go
	// Policy is nil unless --policy was given, so a scan without it renders the
	// same bytes it rendered before the flag existed.
	Policy *policyView
```

and add the two types after `blindSpot`:

```go
// policyView is the POLICY section. It is separate from Findings on purpose: a
// violation is a statement about an organization's rules, not about cluster
// health, and the header pills tally cluster health.
type policyView struct {
	Rules        int
	Violations   []policyRow
	NotEvaluated []policyRow
}

// policyRow is one line of the section. Target is the object for a violation
// and empty for an unevaluated rule, which examined no object at all.
type policyRow struct {
	RuleID, Level, Kind, Target, Message, Evidence string
}
```

Add to `newView`, after the `blind` loop:

```go
	if p := in.Report.Policy; p != nil {
		pv := &policyView{Rules: p.Rules}
		for _, x := range p.Violations {
			target := x.Name
			if x.Namespace != "" {
				target = x.Namespace + "/" + x.Name
			}
			pv.Violations = append(pv.Violations, policyRow{
				RuleID: x.RuleID, Level: string(x.Level), Kind: x.Kind,
				Target: target, Message: x.Message, Evidence: noPath(x.Evidence),
			})
		}
		for _, x := range p.NotEvaluated {
			pv.NotEvaluated = append(pv.NotEvaluated, policyRow{
				RuleID: x.RuleID, Level: string(x.Level), Kind: x.Kind, Message: x.Reason,
			})
		}
		v.Policy = pv
	}
```

and the filter, next to `safeReason`:

```go
// pathLike is what this document will not print. A filesystem path identifies
// the machine kubeagent ran on, and this file is written to be forwarded.
const pathLike = "the value was withheld: it looks like a filesystem path"

// noPath drops evidence that looks like a path. Evidence is cluster text, so in
// practice it never is one; the check is here because the cost of being wrong
// is a leak in the one artifact designed to leave the operator's control, and
// the cost of being right is one line of a report a reader can also get from
// --output text.
func noPath(evidence string) string {
	if strings.HasPrefix(evidence, "/") || strings.HasPrefix(evidence, "~") ||
		strings.Contains(evidence, "kubeconfig") {
		return pathLike
	}
	return evidence
}
```

**Note for the implementer:** `policyView.Rules` is set but the template below
prints it in the heading; do not drop the field.

- [ ] **Step 4: Add the template section**

In `internal/htmlreport/report.html.tmpl`, insert **between** the blind-spots
section and `<section class="findings">`:

```html
{{- if .Policy}}
<section class="policy">
  <h2>Policy</h2>
  <p>{{.Policy.Rules}} rules evaluated from the policy files given on the command line.</p>
{{- if .Policy.Violations}}
  <table>
    <thead><tr><th>Level</th><th>Rule</th><th>Kind</th><th>Object</th><th>Message</th><th>Evidence</th></tr></thead>
    <tbody>
{{- range .Policy.Violations}}
      <tr class="{{.Level}}"><td class="sev">{{.Level}}</td><td class="mono">{{.RuleID}}</td><td>{{.Kind}}</td><td class="mono">{{.Target}}</td><td>{{.Message}}</td><td class="mono">{{.Evidence}}</td></tr>
{{- end}}
    </tbody>
  </table>
{{- else}}
  <p class="empty">No violations.</p>
{{- end}}
{{- if .Policy.NotEvaluated}}
  <p>The following rules were <strong>not evaluated</strong>, so nothing here says whether the cluster satisfies them.</p>
  <ul>
{{- range .Policy.NotEvaluated}}
    <li><span class="mono">{{.RuleID}}</span> ({{.Level}}, {{.Kind}}) &mdash; {{.Message}}</li>
{{- end}}
  </ul>
{{- end}}
</section>
{{- end}}
```

Add one rule to the stylesheet, beside the `.blind` rule:

```css
  .policy { background: var(--panel); border-left: 3px solid var(--muted); padding: .75rem 1rem; }
```

The severity classes on the rows reuse the existing `.critical`/`.warning`/
`.info` colours. They are outside the findings table, so the radio-button
severity filter — which selects inside `.findings` — does not touch them.

- [ ] **Step 5: Run the tests to watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/htmlreport/ -p 2 -run Policy -v
```

Expected: PASS.

- [ ] **Step 6: Extend the golden fixture**

The golden document is the one comprehensive snapshot of every section, and
`TestGoldenInputCoversEverySection` exists to stop it losing one. Add policy to
`goldenInput()` in `internal/htmlreport/golden_test.go` — inside the
`Report` literal, beside `Now`:

```go
			Policy: &report.PolicyView{
				Rules: 3,
				Violations: []policy.Violation{
					{RuleID: "registry-allowlist", Level: policy.LevelCritical, Kind: "Pod",
						Namespace: "shop", Name: "checkout-7d9f",
						Message: "image is not from an allowed registry",
						Evidence: "docker.example.net/checkout:2.1"},
					{RuleID: "pdb-required", Level: policy.LevelWarning, Kind: "Deployment",
						Namespace: "shop", Name: "checkout",
						Message: "no PodDisruptionBudget covers this Deployment"},
				},
				NotEvaluated: []policy.Unevaluated{{
					RuleID: "storage-encrypted", Level: policy.LevelCritical, Kind: "StorageClass",
					Reason: "kubeagent could not read this kind, so the rule was not evaluated",
				}},
			},
```

and extend the coverage assertion:

```go
	if in.Report.Policy == nil || len(in.Report.Policy.Violations) == 0 ||
		len(in.Report.Policy.NotEvaluated) == 0 {
		t.Fatal("goldenInput must populate the policy section, violations and unevaluated rules")
	}
```

Then regenerate and inspect:

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/htmlreport -p 2 -run TestGoldenHTMLReport -update
git diff --stat internal/htmlreport/testdata/golden-report.html
grep -c 'registry-allowlist' internal/htmlreport/testdata/golden-report.html   # 1
```

Read the diff. It must be **only** the new `<section class="policy">` block and
the one CSS rule — every pre-existing line unchanged. If any other line moved,
the section was inserted in the wrong place; fix it rather than accepting the
fixture.

This is the only golden regeneration in the whole sub-project. The
**report** package's `golden-scan.txt` is not regenerated (Task 11 added a
second fixture there instead), so the demo GIF and the quickstart stay as they
are.

- [ ] **Step 7: Full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
git diff --stat internal/report/testdata/golden-scan.txt   # must print nothing
git diff --stat go.mod go.sum                              # must print nothing
```

- [ ] **Step 8: Commit**

```bash
git add internal/htmlreport/htmlreport.go internal/htmlreport/report.html.tmpl \
        internal/htmlreport/htmlreport_test.go internal/htmlreport/golden_test.go \
        internal/htmlreport/testdata/golden-report.html
git commit -s -m "htmlreport: render the policy section

Policy gets its own section rather than rows in the findings table: a violation
is a statement about an organization's rules, not about cluster health, and the
header pills tally cluster health. Rules that could not be evaluated are named
in full, because a section that showed only violations would read as a pass.
Evidence that looks like a filesystem path is withheld — this is the one
artifact written to be forwarded off the machine kubeagent ran on."
```

---

### Task 14: `kubeagent policy validate` — check a file before a cluster exists

**Files:**
- Create: `internal/cli/policy.go`
- Create: `internal/cli/policy_test.go`
- Modify: `internal/cli/root.go` (usage text and the command tree)
- Modify: `internal/cli/root_test.go`

**Interfaces:**
- Consumes: `policy.Document`, `policy.Load`, `policy.Kinds` (Task 6).
- Produces:
  - `func policyDocuments(paths []string) ([]policy.Document, error)`
  - `func runPolicyValidate(args []string, w io.Writer) error`
  - `func newPolicyCommand() *cobra.Command`

**The filesystem lives here, not in `internal/policy`.** `policy.Load` takes
bytes and a source name. Reading files, walking a directory, and deciding what
counts as a policy file are CLI concerns, and keeping them out of
`internal/policy` is what lets that package stay pure enough for `gate` and
`mcp` to import.

`policy validate` follows `kubeagent schema` exactly: no cluster, no
kubeconfig, `Args: cobra.ArbitraryArgs` with hand-rolled argument handling so
the error wording stays kubeagent's. It exists so CI can reject a bad policy
file before anything touches a cluster.

**The path may reach stderr and nowhere else.** `Main` prints a returned error
to stderr, which is the operator's own channel — that is the accepted carve-out
(same shape as `mcp`'s startup connection check). Nothing this task writes to
**stdout** carries a path: the success line is a count, and it is the only
thing printed on success.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/policy_test.go`:

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validPolicy = `- id: registry-allowlist
  level: critical
  message: image is not from an allowed registry
  match:
    kind: Pod
  assert:
    path: spec.containers[*].image
    op: matches
    values: ["registry.example.com/*"]
`

const secondPolicy = `- id: zone-label
  level: info
  message: no topology label
  match:
    kind: Node
  assert:
    path: metadata.labels["topology.kubernetes.io/zone"]
    op: exists
`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPolicyValidateReportsRulesAndKinds(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.yaml", validPolicy)
	b := writeFile(t, dir, "b.yaml", secondPolicy)

	var out bytes.Buffer
	if err := runPolicyValidate([]string{a, b}, &out); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "2 rules") || !strings.Contains(got, "2 kinds") {
		t.Errorf("output = %q, want a rule and kind count", got)
	}
}

// A count is all stdout gets. The path stays on stderr, where Main puts a
// returned error, because a filesystem path names the machine kubeagent ran on.
func TestPolicyValidateStdoutCarriesNoPath(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "rules.yaml", validPolicy)

	var out bytes.Buffer
	if err := runPolicyValidate([]string{p}, &out); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{dir, "rules.yaml", ".yaml", "/"} {
		if strings.Contains(out.String(), needle) {
			t.Errorf("stdout contains %q:\n%s", needle, out.String())
		}
	}
}

func TestPolicyValidateRejectsABadFileAndNamesIt(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "broken.yaml", "- id: no-level\n  match:\n    kind: Pod\n")

	var out bytes.Buffer
	err := runPolicyValidate([]string{p}, &out)
	if err == nil {
		t.Fatal("a policy with no level validated")
	}
	// The error is stderr-bound, so it may name the file — and must, or the
	// operator cannot tell which of five files is wrong.
	if !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("error = %v, want the offending file named", err)
	}
	if out.Len() != 0 {
		t.Errorf("a failed validation printed to stdout: %q", out.String())
	}
}

func TestPolicyValidateWithNoArgumentsIsAUsageError(t *testing.T) {
	var out bytes.Buffer
	err := runPolicyValidate(nil, &out)
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

func TestPolicyDocumentsReadsADirectoryInNameOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.yaml", secondPolicy)
	writeFile(t, dir, "a.yml", validPolicy)
	writeFile(t, dir, "notes.txt", "not a policy")

	docs, err := policyDocuments([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d documents, want 2 (the .txt must be skipped)", len(docs))
	}
	if filepath.Base(docs[0].Source) != "a.yml" || filepath.Base(docs[1].Source) != "b.yaml" {
		t.Errorf("documents out of name order: %q, %q", docs[0].Source, docs[1].Source)
	}
}

// A directory with nothing in it is far more likely a wrong path than an
// operator asking for zero rules, and silently evaluating nothing is the
// failure mode this whole sub-project exists to prevent.
func TestPolicyDocumentsRejectsADirectoryWithNoPolicyFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "notes.txt", "not a policy")

	if _, err := policyDocuments([]string{dir}); err == nil {
		t.Fatal("an empty policy directory loaded without error")
	}
}

// A named file is taken at its word whatever it is called: the operator typed
// the name, so kubeagent does not second-guess the extension.
func TestPolicyDocumentsAcceptsANamedFileWithAnyExtension(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "rules.policy", validPolicy)

	docs, err := policyDocuments([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
}

func TestPolicyDocumentsNamesAMissingPath(t *testing.T) {
	_, err := policyDocuments([]string{filepath.Join(t.TempDir(), "absent.yaml")})
	if err == nil || !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("err = %v, want the missing path named", err)
	}
}
```

- [ ] **Step 2: Run them to watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli/ -p 2 -run Policy -v
```

Expected: FAIL — `undefined: runPolicyValidate`, `undefined: policyDocuments`.

- [ ] **Step 3: Implement**

Create `internal/cli/policy.go`:

```go
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/policy"
)

// policyDocuments reads every --policy path into a document internal/policy can
// load. The filesystem stops here: internal/policy takes bytes and a name, which
// is what keeps it importable by gate and mcp.
//
// A path may be a file or a directory. A named file is read whatever it is
// called — the operator typed the name. A directory contributes its .yaml and
// .yml entries in name order, and only those: a directory is a place other
// things live too, and reading a README as a policy would be an error message
// about YAML instead of about the mistake.
//
// The walk is not recursive. A nested directory is a structure kubeagent would
// have to invent a meaning for, and "the files I can see in this directory" is
// the meaning an operator already has.
func policyDocuments(paths []string) ([]policy.Document, error) {
	var out []policy.Document
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("--policy %s: %w", p, err)
		}
		if !info.IsDir() {
			doc, err := readPolicyFile(p)
			if err != nil {
				return nil, err
			}
			out = append(out, doc)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, fmt.Errorf("--policy %s: %w", p, err)
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			switch strings.ToLower(filepath.Ext(e.Name())) {
			case ".yaml", ".yml":
				names = append(names, e.Name())
			}
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("--policy %s: no .yaml or .yml files in this directory", p)
		}
		// Name order, so the rule order — and the report — does not depend on
		// what the filesystem happens to return.
		sort.Strings(names)
		for _, n := range names {
			doc, err := readPolicyFile(filepath.Join(p, n))
			if err != nil {
				return nil, err
			}
			out = append(out, doc)
		}
	}
	return out, nil
}

// readPolicyFile reads one file into a document. Source is the path as the
// operator wrote it, because it reaches only an error on stderr, where naming
// the file is the whole point.
func readPolicyFile(path string) (policy.Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return policy.Document{}, fmt.Errorf("--policy %s: %w", path, err)
	}
	return policy.Document{Source: path, Data: data}, nil
}

// loadPolicy reads and validates the files named by --policy. Both scan and
// gate call it, so neither can load a policy the other would reject.
func loadPolicy(paths []string) ([]policy.Rule, error) {
	docs, err := policyDocuments(paths)
	if err != nil {
		return nil, err
	}
	return policy.Load(docs)
}

// runPolicyValidate checks policy files and prints a count. It contacts
// nothing: no cluster, no kubeconfig, no LLM. The count is all stdout gets —
// the paths stay in the error, which Main writes to stderr.
func runPolicyValidate(args []string, w io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s policy validate <file>…", invokedAs)
	}
	rules, err := loadPolicy(args)
	if err != nil {
		return err
	}
	kinds := policy.Kinds(rules)
	fmt.Fprintf(w, "%s, %s\n",
		plural(len(rules), "rule", "rules"), plural(len(kinds), "kind", "kinds"))
	return nil
}

// newPolicyCommand builds `kubeagent policy validate`. Like `schema`, it keeps
// its own argument handling rather than cobra.MinimumNArgs(1), which would
// reword the usage error runPolicyValidate produces.
func newPolicyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "policy",
		Short:         "Work with policy files",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("usage: %s policy validate <file>…", invokedAs)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:           "validate <file>…",
		Short:         "Validate policy files without contacting a cluster",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPolicyValidate(args, os.Stdout)
		},
	})
	return cmd
}
```

**`plural` note:** `internal/cli` has no `plural` helper — the one at
`internal/report/report.go:489` is unexported and `internal/cli` must not grow
a dependency on `internal/report` for a word ending. Add this small one to
`internal/cli/policy.go`:

```go
// plural picks the singular or plural spelling for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
```

Before adding it, run `grep -rn "func plural" internal/cli/` — if `internal/cli`
already has one, use that and add nothing.

- [ ] **Step 4: Run the tests to watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli/ -p 2 -run Policy -v
```

Expected: PASS.

- [ ] **Step 5: Register the command and extend the usage text**

In `internal/cli/root.go`, add `newPolicyCommand()` to the `root.AddCommand(...)`
call, between `newRBACCommand()` and `newCompletionCommand()`.

In `usageError()`, insert the new alternative immediately before
`| %[1]s schema [name]`:

```text
 | %[1]s policy validate <file>…
```

The `--policy` flag itself is added to the `scan` and `gate` alternatives in
Task 15 — the flag does not exist yet, and usage text must never name a flag
the binary rejects.

- [ ] **Step 6: Assert the command is reachable**

Append to `internal/cli/root_test.go`:

```go
func TestPolicyValidateIsReachableWithNoKubeconfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rules.yaml")
	if err := os.WriteFile(p, []byte(validPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	// No kubeconfig anywhere: policy validate must not read one. This is gate
	// evidence item 3, asserted in a unit test as well as on a real machine.
	t.Setenv("KUBECONFIG", filepath.Join(dir, "does-not-exist"))
	t.Setenv("HOME", dir)

	if err := Run([]string{"policy", "validate", p}); err != nil {
		t.Fatalf("policy validate: %v", err)
	}
}

func TestUsageNamesPolicyValidate(t *testing.T) {
	if !strings.Contains(usageError().Error(), "policy validate") {
		t.Error("the usage line does not name policy validate")
	}
}
```

Add `"os"` and `"path/filepath"` to that file's imports if they are not
already there.

- [ ] **Step 7: Full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
```

Every pre-existing `internal/cli` test must still pass — in particular any
test that asserts on the full usage string, which now has one more
alternative. If one fails on the added text, update its expectation; if one
fails on anything else, stop and report it.

- [ ] **Step 8: Commit**

```bash
git add internal/cli/policy.go internal/cli/policy_test.go \
        internal/cli/root.go internal/cli/root_test.go
git commit -s -m "cli: add kubeagent policy validate

Reads policy files, validates them, and prints a count. It contacts nothing —
no cluster, no kubeconfig, no LLM — so CI can reject a bad policy file before
anything touches a cluster, the same shape kubeagent schema already has.

The filesystem stops in internal/cli: internal/policy takes bytes and a source
name, which is what keeps it importable by gate and mcp. A policy path reaches
stderr, where naming the offending file is the point, and never stdout: the
success line is a count."
```

---

### Task 15: `--policy` on `scan` and `gate`

**Files:**
- Modify: `internal/cli/policy.go`
- Modify: `internal/cli/policy_test.go`
- Modify: `internal/cli/scan.go`
- Modify: `internal/cli/gate.go`
- Modify: `internal/cli/root.go` (usage text)
- Modify: `internal/cli/scan_test.go`
- Modify: `internal/cli/gate_test.go`

**Interfaces:**
- Consumes: `loadPolicy` (Task 14), `policy.ReadPlan` / `policy.InputsFrom` /
  `policy.Evaluate` (Tasks 7, 10), `collect.PolicyObjects` (Task 10),
  `scan.Workers` (Task 10), `report.PolicyView` (Task 11),
  `gate.Options.PolicyViolations` / `.PolicyNotEvaluated` (Task 12).
- Produces:
  - `func evaluatePolicy(ctx context.Context, paths []string, kubeconfig, contextName, namespace string) (*report.PolicyView, error)`

**One function, two callers.** `scan` and `gate` must never be able to load,
read, or evaluate a policy differently — in particular neither may forget to
populate `Inputs.Unreadable`, which is the difference between "the rule passed"
and "the rule never ran". `evaluatePolicy` is that single path, and it returns
`*report.PolicyView` so there is one shape to render and one shape to map into
findings.

`internal/cli` may import `internal/report`; `internal/gate` may not, and does
not — the CLI does the mapping and hands `gate.Decide` two slices.

**`--policy` is repeatable and goes on `scan` and `gate` only.** `StringArrayVar`,
not `StringSliceVar`: a path can contain a comma, and `StringSliceVar` would
split it into two paths that do not exist. This is the same reasoning
`--allow-partial-read` already carries at `internal/cli/gate.go:67`.

**A policy failure is fatal, not a warning.** If the file will not load or the
dynamic client will not build, `scan` returns the error rather than printing a
report without the policy section. A report that silently omits the rules the
operator asked for reads as "your rules are satisfied".

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/policy_test.go`:

```go
func TestEvaluatePolicyWithNoPathsReturnsNil(t *testing.T) {
	got, err := evaluatePolicy(context.Background(), nil, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %#v, want nil so no section renders and no JSON key appears", got)
	}
}

// A file that will not load must stop the command. A scan that printed a
// report with no policy section would read as "your rules are satisfied".
func TestEvaluatePolicyFailsOnABadFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "broken.yaml", "- id: no-level\n  match:\n    kind: Pod\n")

	if _, err := evaluatePolicy(context.Background(), []string{p}, "", "", ""); err == nil {
		t.Fatal("a bad policy file did not stop the command")
	}
}
```

Append to `internal/cli/scan_test.go`:

```go
func TestScanRegistersPolicyAsARepeatableFlag(t *testing.T) {
	cmd := newScanCommand()
	f := cmd.Flags().Lookup("policy")
	if f == nil {
		t.Fatal("scan has no --policy flag")
	}
	// stringArray, not stringSlice: a path may contain a comma.
	if f.Value.Type() != "stringArray" {
		t.Errorf("--policy is %s, want stringArray so a comma in a path is not a separator", f.Value.Type())
	}
}
```

Append to `internal/cli/gate_test.go`:

```go
func TestGateRegistersPolicyAsARepeatableFlag(t *testing.T) {
	cmd := newGateCommand()
	f := cmd.Flags().Lookup("policy")
	if f == nil {
		t.Fatal("gate has no --policy flag")
	}
	if f.Value.Type() != "stringArray" {
		t.Errorf("--policy is %s, want stringArray", f.Value.Type())
	}
}

// --policy is declared per command, never as a persistent flag. The commands
// that take no policy must reject it rather than accept and ignore it.
func TestPolicyIsNotAPersistentFlag(t *testing.T) {
	for _, name := range []string{"watch", "mcp", "tui", "version", "schema"} {
		if err := Run([]string{name, "--policy", "x.yaml"}); err == nil {
			t.Errorf("%s accepted --policy", name)
		}
	}
}
```

- [ ] **Step 2: Run them to watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli/ -p 2 -run 'Policy' -v
```

Expected: FAIL — `undefined: evaluatePolicy`, and `scan has no --policy flag`.

- [ ] **Step 3: Implement `evaluatePolicy`**

Append to `internal/cli/policy.go` (and add the imports it needs:
`context`, `internal/cluster`, `internal/collect`, `internal/policy`,
`internal/report`, `internal/scan`):

```go
// evaluatePolicy is the whole --policy path, and the only one. scan and gate
// both call it, so neither can load a policy the other would reject, and
// neither can drop the unreadable set — which is the difference between "the
// rule passed" and "the rule never ran".
//
// Returns nil when no --policy was given, so a run without the flag renders
// exactly the bytes it rendered before the flag existed.
//
// Read-only toward the cluster: ReadPlan names the kinds, collect.PolicyObjects
// lists them, and nothing here writes. There is no --fix path from a policy.
func evaluatePolicy(ctx context.Context, paths []string, kubeconfig, contextName, namespace string) (*report.PolicyView, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	rules, err := loadPolicy(paths)
	if err != nil {
		return nil, err
	}
	// A dynamic client, because a policy selects kinds the typed collectors do
	// not cover. Same construction scan already uses for the advisory reads.
	dyn, _, err := cluster.NewDynamicClients(kubeconfig, contextName)
	if err != nil {
		return nil, err
	}
	objects, unreadable := collect.PolicyObjects(ctx, dyn, policy.ReadPlan(rules), namespace, scan.Workers())
	violations, notEvaluated := policy.Evaluate(rules, policy.InputsFrom(objects, unreadable))
	return &report.PolicyView{
		Rules: len(rules), Violations: violations, NotEvaluated: notEvaluated,
	}, nil
}
```

- [ ] **Step 4: Wire `scan`**

Add the field to `scanOptions`:

```go
	policyPaths []string
```

Register the flag beside the others in the flag block (after `--capacity`, so
the help text groups it with the other opt-in sections):

```go
	f.StringArrayVar(&o.policyPaths, "policy", nil, "evaluate organization-specific checks from this policy file or directory (repeatable)")
```

In `runScan`, immediately after the `res.PartialReads = append(...)` line that
follows `advisory.Assess`, add:

```go
	policyView, err := evaluatePolicy(context.Background(), o.policyPaths, o.kubeconfig, o.contextName, o.namespace)
	if err != nil {
		return err
	}
```

and set `Policy: policyView` on the `report.Input` literal `runScan` builds.

**Find that literal** with `grep -n "report.Input{" internal/cli/scan.go` — there
may be more than one construction path (text vs html). Set the field on every
one, so `--output html` is not silently policy-free.

- [ ] **Step 5: Wire `gate`**

Add the field to `gateOptions`:

```go
	policyPaths []string
```

Register the flag beside `--allow-partial-read`:

```go
	f.StringArrayVar(&o.policyPaths, "policy", nil, "evaluate organization-specific checks from this policy file or directory (repeatable)")
```

In `runGateOpts`, immediately after the `opts := gate.Options{...}` line:

```go
	// Exit 4 for a bad policy file: it is bad input, in the same class as a bad
	// flag, and nothing was attempted against the cluster. Exit 1 would claim
	// kubeagent looked and found problems.
	pv, err := evaluatePolicy(ctx, o.policyPaths, o.kubeconfig, o.contextName, o.namespace)
	if err != nil {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
	if pv != nil {
		opts.PolicyViolations, opts.PolicyNotEvaluated = pv.Violations, pv.NotEvaluated
	}
```

Place it **before** the `--wait-for` block, so a policy file with a typo fails
fast instead of after a rollout wait that can take minutes.

**Careful with `err`:** `runGateOpts` already declares `err` above via
`level, err := findings.Parse(...)`. Use `=` where the plan's snippet would
redeclare, or Go will reject the shadow. Build after this step and read the
compiler.

- [ ] **Step 6: Extend the usage text**

In `internal/cli/root.go`, add `[--policy path (repeatable)]` to the `scan`
alternative — after `[--capacity]` — and to the `gate` alternative, after
`[--allow-partial-read resource (repeatable)]`.

- [ ] **Step 7: Run the tests**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./internal/cli/ -p 2
```

Expected: PASS. If a test asserting the full usage string fails on the added
text, update its expectation.

- [ ] **Step 8: Prove the no-policy path is untouched**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./... -p 2
git diff --stat internal/report/testdata/golden-scan.txt   # must print nothing
git diff --stat go.mod go.sum                              # must print nothing
```

`golden-scan.txt` not moving is the unit-test half of gate evidence item 1; the
byte-identity check against a `main` binary on a live cluster is the other half
and runs in the gate, not here.

- [ ] **Step 9: Commit**

```bash
git add internal/cli/policy.go internal/cli/policy_test.go internal/cli/scan.go \
        internal/cli/gate.go internal/cli/root.go internal/cli/scan_test.go \
        internal/cli/gate_test.go
git commit -s -m "cli: add --policy to scan and gate

One function loads, reads and evaluates, so the two commands cannot diverge —
in particular neither can drop the unreadable set, which is the difference
between a rule that passed and a rule that never ran. --policy is repeatable
and declared per command, never persistent, and takes a string array rather
than a string slice because a path may contain a comma.

A policy that will not load stops the command. A report that silently omitted
the rules the operator asked for would read as 'your rules are satisfied'."
```

---

### Task 16: RBAC feature entry and the schema version bumps

**Files:**
- Modify: `internal/rbacprofile/profile.go`
- Modify: `internal/rbacprofile/profile_test.go`
- Modify: `internal/jsonschema/jsonschema.go`
- Modify: `internal/schemadoc/schemadoc.go`
- Regenerate: `website/docs/schemas/*.json`

**Interfaces:**
- Consumes: `policy.Level*` (Task 1), `report.PolicyView` (Task 11),
  `gate.Verdict.PolicyNotEvaluated` (Task 12).
- Produces: a `policy` entry in `rbacprofile.Features`; `ScanVersion` and
  `GateVersion` at `1.1`.

**The `policy` feature ships no manifest and no new grant.** The 23 selectable
kinds are exactly the kinds `rbacprofile.coreRules` already grants (Task 1's
`TestSelectableKindsMatchesRBACProfileCore` pins that), so a policy needs nothing core
does not already have. The entry exists so `kubeagent rbac print` and
`rbac check` name the feature at all — a feature absent from the table is a
feature an operator cannot ask about.

`ScanOnly: true`: the watch daemon has no `--policy`, so the chart gates
nothing for it.

- [ ] **Step 1: Write the failing RBAC test**

Append to `internal/rbacprofile/profile_test.go`:

```go
// The policy feature must cost nothing: the kinds a rule may select are the
// kinds core already grants, so a shipped ClusterRole that grew a rule because
// of this feature means the selectable-kind table drifted from coreRules.
func TestPolicyFeatureAddsNoRules(t *testing.T) {
	var f *Feature
	for i := range Features {
		if Features[i].Name == "policy" {
			f = &Features[i]
		}
	}
	if f == nil {
		t.Fatal("no policy feature in the table")
	}
	if len(f.Rules) != 0 {
		t.Errorf("the policy feature declares %d rules; it must be covered by core", len(f.Rules))
	}
	if f.CoveredBy != "core" {
		t.Errorf("CoveredBy = %q, want core", f.CoveredBy)
	}
	if f.Manifest != "" || f.RoleName != "" || f.HelmCondition != "" {
		t.Errorf("the policy feature must ship no manifest and gate nothing in the chart: %#v", f)
	}
	if !f.ScanOnly {
		t.Error("ScanOnly = false; the watch daemon has no --policy")
	}
}
```

- [ ] **Step 2: Run it to watch it fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/rbacprofile/ -p 2 -run TestPolicyFeature -v
```

Expected: FAIL — "no policy feature in the table".

- [ ] **Step 3: Add the feature entry**

Append to `rbacprofile.Features`, after the `logs` entry:

```go
	{
		Name:      "policy",
		Flag:      "--policy",
		Summary:   "organization-specific checks from a policy file; reads only kinds core already grants",
		CoveredBy: "core",
		ScanOnly:  true,
	},
```

- [ ] **Step 4: Run the RBAC suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/rbacprofile/ -p 2
git diff --stat deploy/ deploy/helm/                # must print nothing
```

Nothing under `deploy/` may move: a feature that grants nothing changes no
manifest. If a generated manifest did change, the selectable-kind table has
drifted from `coreRules` — stop and report it rather than committing the
regenerated manifest.

- [ ] **Step 5: Add the enum and bump the versions**

In `internal/schemadoc/schemadoc.go`, add to `enums` (and import
`internal/policy`):

```go
	"policy.Level": {
		string(policy.LevelInfo), string(policy.LevelWarning), string(policy.LevelCritical),
	},
```

No `overrides` entry: `policy.Level` is a named string type, so reflection
already documents it as a string. `findings.Level` needs one because it is an
int with a custom marshaler.

In `internal/jsonschema/jsonschema.go`:

```go
	ScanVersion  = "1.1"
	GateVersion  = "1.1"
```

Both surfaces gained a field: `scan` gained `policy`, `gate` gained
`policyNotEvaluated`. Both are `omitempty`, so both are additive.

- [ ] **Step 6: Regenerate and read what the drift test says**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/schemadoc -p 2 -run TestSchemaDrift -update
git diff --stat website/docs/schemas/
```

The drift test classifies each change. It **must** report both as **additive
(MINOR)**. If it reports either as breaking, stop: something removed or
retyped a field, which no task in this plan intends. If it reports the **gate**
document unchanged, Task 12's `Verdict.PolicyNotEvaluated` was not wired —
fix that rather than reverting the bump.

Record the drift test's exact classification in the task report.

- [ ] **Step 7: Confirm the running binary agrees**

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o /tmp/kubeagent-policy . \
  && /tmp/kubeagent-policy schema | grep -E 'scan|gate' \
  && /tmp/kubeagent-policy schema scan | grep -c 'policy' \
  && /tmp/kubeagent-policy rbac print --features policy >/dev/null \
  && rm -f /tmp/kubeagent-policy
```

`schema` must list `scan` and `gate` at v1.1, and `rbac print --features policy`
must succeed — that is the operator-facing proof the feature name is real.

- [ ] **Step 8: Full suite and commit**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
git add internal/rbacprofile/profile.go internal/rbacprofile/profile_test.go \
        internal/jsonschema/jsonschema.go internal/schemadoc/schemadoc.go \
        website/docs/schemas/
git commit -s -m "schema: publish the policy fields at scan v1.1 and gate v1.1

scan's document gains policy, gate's gains policyNotEvaluated; both are
omitempty, so a run without --policy encodes neither and every existing
consumer is unaffected. The drift test classifies both as additive.

internal/rbacprofile gains a policy feature so rbac print and rbac check can
name it. It grants nothing: the kinds a rule may select are exactly the kinds
core already reads, so no shipped manifest changes."
```

---

### Task 17: Documentation

**Files:**
- Create: `website/docs/features/policy.md`
- Modify: `website/mkdocs.yml`
- Modify: `website/docs/roadmap.md`
- Modify: `CHANGELOG.md`
- Modify: `CLAUDE.md`
- Modify: `docs/go-concepts.md` (**on disk only — gitignored, never staged**)

**Interfaces:** none. This task adds no Go code.

**Every example uses RFC 2606 domains and RFC 5737 addresses.** No real
registry, no real cluster name, no internal hostname, no path that names a real
machine. A doc example is copied verbatim by readers, which makes it the most
widely distributed text in the repository.

**`docs/go-concepts.md` is gitignored** (`.gitignore:43`). Edit it on disk and
do **not** `git add` it. Never `git add -A` or `git add .` anywhere in this
plan.

- [ ] **Step 1: Write the feature page**

Create `website/docs/features/policy.md`:

````markdown
# Policy as code

`kubeagent scan --policy` and `kubeagent gate --policy` evaluate
organization-specific checks written in a YAML file, alongside the built-in
detectors. A rule says *which objects to look at* and *one thing that must be
true of them*.

Policy evaluation is **read-only**. A rule can never write to the cluster;
there is no `--fix` path from a policy, and `--policy` grants kubeagent no
access it does not already have — the kinds a rule may select are exactly the
kinds a plain `kubeagent scan` already reads.

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
"at least one was found".

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

Globs use `*` for any run of characters, including `/`, and `?` for exactly
one. `registry.example.com/*` matches `registry.example.com/team/app:1.0`.

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

`hasPodDisruptionBudget` and `hasHorizontalPodAutoscaler` apply to
`Deployment`, `StatefulSet` and `ReplicaSet`.

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
build**. The same applies when the supporting list a relation compares against
cannot be read: without the PodDisruptionBudget list, `hasPodDisruptionBudget`
would report every workload as uncovered, which is a fabricated violation
rather than a silent pass, and equally wrong.

## What a rule may not do

- **Secrets are not selectable.** No rule can name `Secret` as its kind.
- **A ConfigMap's contents are not readable.** A path beginning `data` or
  `binaryData` on a `ConfigMap` is a load error, in every spelling — a
  violation would carry the value as evidence into a report designed to be
  forwarded.
- **A policy cannot write.** There is no remediation path from a rule.

## Selectable kinds

`Pod`, `Service`, `ServiceAccount`, `ConfigMap`, `Namespace`, `Node`,
`PersistentVolume`, `PersistentVolumeClaim`, `Deployment`, `StatefulSet`,
`DaemonSet`, `ReplicaSet`, `Job`, `CronJob`, `EndpointSlice`, `Ingress`,
`IngressClass`, `NetworkPolicy`, `StorageClass`, `PodDisruptionBudget`,
`HorizontalPodAutoscaler`, `ValidatingWebhookConfiguration`,
`MutatingWebhookConfiguration`.

These are exactly the kinds kubeagent already reads, which is why `--policy`
needs no RBAC beyond `deploy/rbac.yaml`. See
[Least-privilege RBAC](rbac.md).
````

**Verify the kind list** against `internal/policy/policy.go`'s
`SelectableKinds` before committing — the two must agree, and the doc is the
one a reader trusts.

- [ ] **Step 2: Add the nav entry**

In `website/mkdocs.yml`, after the `Shell completion` line:

```yaml
      - Policy as code: features/policy.md
```

- [ ] **Step 3: Build the site**

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
(cd website && mkdocs build --strict -f mkdocs.yml)
```

Expected: "Documentation built", exit 0, and **no `WARNING` line naming
`policy.md`**. A warning about a link or a nav entry is a real error here —
`--strict` is what the release gate runs.

- [ ] **Step 4: Stamp the roadmap**

In `website/docs/roadmap.md`, extend the Theme H paragraph. Replace the
sentence "The rest of Theme H — the v1.0 production contract — remains ahead."
with:

```markdown
  Slice 7 — policy as code — has shipped: `scan --policy` and `gate --policy`
  evaluate organization-specific checks from a YAML file, so an operator no
  longer has to fork kubeagent to add a check its detectors do not make. A
  rule names one kind and asserts one thing; a wildcard path yields one slot
  per list element, so "every container sets a memory limit" is not satisfied
  by one container out of three; Secrets are not selectable and a ConfigMap's
  contents are not readable; and a rule kubeagent could not evaluate is
  reported as not evaluated and **fails a gate** rather than passing quietly
  — see [Policy as code](features/policy.md). The rest of Theme H — the
  cross-version/distro chaos matrix and the v1.0 production contract — remains
  ahead.
```

- [ ] **Step 5: Add the changelog entry**

Under `## [Unreleased]` in `CHANGELOG.md`, in an `### Added` block:

```markdown
- **Policy as code.** `kubeagent scan --policy FILE|DIR` and
  `kubeagent gate --policy FILE|DIR` evaluate organization-specific checks
  written in YAML, alongside the built-in detectors — "every production
  Deployment must be covered by a PodDisruptionBudget", "no image may come from
  a registry outside the allowlist" — without forking kubeagent. A rule names
  one kind and asserts one thing about it, from a closed set of ten operators
  and two relations. `kubeagent policy validate FILE…` checks a file with no
  cluster and no kubeconfig, so CI can reject a bad policy before a deploy.
  Evaluation is strictly read-only and adds no RBAC: the selectable kinds are
  exactly the kinds a plain scan already reads. Secrets are not selectable and
  a ConfigMap's `data` is not readable. A rule kubeagent could not evaluate is
  reported as **not evaluated** and fails a gate rather than passing quietly.
  See [Policy as code](https://k8sproject.top/features/policy/).
```

Under `### Changed`:

```markdown
- `scan`'s JSON document is schema version **1.1** (added `policy`) and
  `gate`'s is **1.1** (added `policyNotEvaluated`). Both additions are
  `omitempty`, so a run without `--policy` encodes neither and every existing
  consumer is unaffected.
```

- [ ] **Step 6: Update `CLAUDE.md`**

Two edits, both in the **Invariants** section:

1. Extend the list of packages that must never import `internal/remediate` or
   `internal/explain` with `internal/policy`, and state its stronger rule:

```markdown
  `internal/policy` (the `--policy` evaluator) is a sixth case and the most
  constrained: it is **pure** — no client, no context, no I/O beyond the bytes
  it is handed — and must never import `internal/remediate`, `internal/explain`,
  `internal/investigate`, `internal/report`, `internal/scan` or
  `internal/findings`. `internal/findings` imports `internal/scan`, which
  imports `internal/policy`, so the last three are a cycle rather than a
  preference; that is why `policy` defines its own `Level` type. A policy can
  never write to the cluster: there is no `--fix` path from a rule
  (see [website/docs/features/policy.md](website/docs/features/policy.md)).
```

2. In the same paragraph, record the two invariants the spec names that are
   about capability rather than imports:

```markdown
  A policy can never require a grant beyond `core`: the kinds a rule may select
  are exactly the kinds `rbacprofile.coreRules` already grants, pinned by
  `TestSelectableKindsMatchesRBACProfileCore`, so `--policy` changes no RBAC manifest.
  `Secret` is not a selectable kind and a `ConfigMap` path beginning `data` or
  `binaryData` is a load error — a violation carries its evidence into a report
  designed to be forwarded.
```

3. In the six-JSON-documents paragraph, note that `scan` is at 1.1 and `gate`
   at 1.1.

4. Also add to the **Roadmap** section, after the slice 6 sentence:

```markdown
  Slice 7 — policy as code — has shipped (v0.74.0): `scan --policy` and
  `gate --policy` evaluate operator-written YAML rules, `kubeagent policy
  validate` checks a file with no cluster, and a rule that could not be
  evaluated fails a gate instead of passing quietly.
```

Leave the version number as `v0.74.0` only if that is what the release
actually cuts; the release skill's bump script is the authority.

- [ ] **Step 7: Add the Go concept entry**

Add a numbered section to `docs/go-concepts.md`, in the established style —
**a plain everyday example first, then the kubeagent example**, and **no Python
comparisons**.

Three things about placement, all of which the file itself will tell you:

- The last numbered section is **25** (`A function value stored in a struct
  field`), so this one is **26**. Confirm with
  `grep -n '^## ' docs/go-concepts.md | tail -3` before writing — if a section
  26 already exists, use the next free number.
- The file ends with a `## Coming later` section. Insert **before** it, not at
  the end of the file.
- Sections 20 and 22–25 introduce the kubeagent half with an inline
  **`**In kubeagent:**`** paragraph, not a `### In kubeagent` subheading.
  §21 uses the subheading form; do not copy that one.

The concept this sub-project introduces is the **type switch over `any`**:

````markdown
## 26. Type switches: asking a value what it is

A variable of type `any` can hold anything. To use it you have to ask what it
actually is, and Go's answer is the **type switch**:

```go
func describe(v any) string {
	switch x := v.(type) {
	case string:
		return "a string of " + strconv.Itoa(len(x)) + " characters"
	case int:
		return "the number " + strconv.Itoa(x)
	case bool:
		if x {
			return "yes"
		}
		return "no"
	default:
		return "something else"
	}
}
```

`x` has a different type in each branch — a `string` in the first, an `int` in
the second — which is what makes the switch useful rather than just a test.

The single-type form is a **type assertion**, and its two-value shape never
panics:

```go
s, ok := v.(string)   // ok is false if v is not a string
```

**In kubeagent:** `internal/policy` walks a Kubernetes object that has been
decoded into `map[string]any` — nested maps, lists and scalars, with no Go
struct anywhere. Every step down a path is a question about what it just found:

```go
m, ok := s.Value.(map[string]any)
if !ok {
	return []Slot{absent}   // not a map, so the path cannot continue
}
```

and turning a found value into text for a comparison is a type switch:

```go
func stringOf(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	}
	return "", false
}
```

The `default` case — or, as here, falling off the end — matters as much as the
others. A policy rule that met a value it did not understand and guessed would
produce a confident, wrong answer about someone's cluster; returning `false`
lets the caller skip it instead.
````

**Do not stage this file.**

- [ ] **Step 8: Commit**

```bash
git add website/docs/features/policy.md website/mkdocs.yml \
        website/docs/roadmap.md CHANGELOG.md CLAUDE.md
git commit -s -m "docs: document policy as code

The feature page leads with the operator-absence rule, because a rule author
who does not know that lte skips an unset field will write one rule where two
are needed and believe the check is enforced. Also records what a policy may
not do — no Secrets, no ConfigMap contents, no writes — and that a rule
kubeagent could not evaluate fails a gate rather than passing."
git status --short   # docs/go-concepts.md must show as untracked/ignored, not staged
```

---

## Gate

Run this **after** the whole-branch review closes, before
`superpowers:finishing-a-development-branch`. It is the **full chaos gate**, not
the lightweight smoke: this branch touches `internal/collect` and evaluates
against real cluster state.

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY          # keys never reach the shell; --explain scenarios skip
./chaos/run.sh --recreate        # long-running: run in the background, watch the log
```

Every scenario must be green. Scenario 4 (NetworkPolicy causality) is a known
cold-cluster flake — re-run `./chaos/run.sh --recreate` before treating it as a
regression.

Three pieces of evidence are specific to this sub-project and are **not** part
of the chaos suite. Run them by hand on the chaos cluster while it is up, and
record each result in the ledger.

### Evidence 1: a scan with no `--policy` is byte-identical to `main`

The unit half is `golden-scan.txt` not moving. This is the other half: the same
cluster, the same flags, two binaries.

```bash
export PATH=$PATH:/usr/local/go/bin
CTX=kind-kubeagent-chaos

go build -o /tmp/ka-new .
git stash list                                   # tree must be clean before the next line
git worktree add /tmp/ka-main-wt main \
  && (cd /tmp/ka-main-wt && go build -o /tmp/ka-main .)

for out in text json html; do
  /tmp/ka-new  scan --context $CTX --output $out > /tmp/new.$out
  /tmp/ka-main scan --context $CTX --output $out > /tmp/main.$out
  if cmp -s /tmp/new.$out /tmp/main.$out; then echo "$out IDENTICAL"; else echo "$out DIFFERS"; diff /tmp/new.$out /tmp/main.$out | head -40; fi
done

git worktree remove /tmp/ka-main-wt && rm -f /tmp/ka-new /tmp/ka-main /tmp/new.* /tmp/main.*
```

All three must print `IDENTICAL`. A timestamp or a duration field would defeat
this comparison — if one appears in the diff, it is pre-existing and predates
this branch; confirm that by running the two `main` builds against each other
before concluding anything about the policy work.

### Evidence 2: a refused kind is reported as not evaluated, and `gate` fails

This is the invariant the whole "a refused read is never a pass" section exists
for, and the only way to prove it is to take a grant away.

```bash
export PATH=$PATH:/usr/local/go/bin
CTX=kind-kubeagent-chaos
kubectl --context $CTX create namespace policy-gate --dry-run=client -o yaml | kubectl --context $CTX apply -f -
kubectl --context $CTX -n policy-gate create serviceaccount narrow

# Deliberately narrow: pods only. A rule selecting StorageClass cannot be evaluated.
kubectl --context $CTX create clusterrole narrow-reader \
  --verb=get,list --resource=pods --dry-run=client -o yaml | kubectl --context $CTX apply -f -
kubectl --context $CTX create clusterrolebinding narrow-reader \
  --clusterrole=narrow-reader --serviceaccount=policy-gate:narrow \
  --dry-run=client -o yaml | kubectl --context $CTX apply -f -

cat > /tmp/policy-gate.yaml <<'YAML'
- id: storageclass-encrypted
  level: critical
  message: StorageClass does not declare encryption
  match:
    kind: StorageClass
  assert:
    path: parameters.encrypted
    op: exists
YAML

# A kubeconfig for the narrowed identity. The token never leaves this shell and
# the file is deleted below.
kubectl --context $CTX create token narrow -n policy-gate --duration=10m > /tmp/narrow.token
SERVER=$(kubectl --context $CTX config view --minify -o jsonpath='{.clusters[0].cluster.server}')
kubectl --context $CTX config view --raw --minify -o yaml > /tmp/narrow.kubeconfig
kubectl --kubeconfig /tmp/narrow.kubeconfig config set-credentials narrow --token="$(cat /tmp/narrow.token)"
kubectl --kubeconfig /tmp/narrow.kubeconfig config set-context --current --user=narrow

go build -o /tmp/ka-new .
/tmp/ka-new scan --kubeconfig /tmp/narrow.kubeconfig --policy /tmp/policy-gate.yaml
/tmp/ka-new gate --kubeconfig /tmp/narrow.kubeconfig --policy /tmp/policy-gate.yaml --fail-on critical
echo "gate exit: $?"

rm -f /tmp/narrow.token /tmp/narrow.kubeconfig /tmp/policy-gate.yaml /tmp/ka-new
kubectl --context $CTX delete clusterrolebinding narrow-reader
kubectl --context $CTX delete clusterrole narrow-reader
kubectl --context $CTX delete namespace policy-gate
```

Required:

- `scan` prints the rule under **not evaluated**, naming the kind and
  kubeagent's own refusal wording — never the API server's message.
- `gate` exits **1** (a failure), not **2** (inconclusive) and not **0**.
- Neither output contains `/tmp/narrow.kubeconfig` or `/tmp/policy-gate.yaml`.
  A path may appear on **stderr** from a connection failure and nowhere else;
  here the connection succeeds, so no path should appear at all.

The `SERVER` variable is captured only to confirm the minified kubeconfig kept
its cluster entry; do not echo it, and do not paste any part of the token or
the kubeconfig into the ledger, the task report, or a commit message.

### Evidence 3: `policy validate` runs with no kubeconfig at all

`policy validate` must be usable in a CI container that has never seen a
cluster. The unit test
`TestPolicyValidateIsReachableWithNoKubeconfig` covers it; this is the same
claim against the real binary.

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o /tmp/ka-new .
cat > /tmp/policy-ok.yaml <<'YAML'
- id: registry-allowlist
  level: critical
  message: image is not from an allowed registry
  match:
    kind: Pod
  assert:
    path: spec.containers[*].image
    op: matches
    values: ["registry.example.com/*"]
YAML

env -i PATH=/usr/bin:/bin HOME=/nonexistent KUBECONFIG=/nonexistent \
  /tmp/ka-new policy validate /tmp/policy-ok.yaml
echo "exit: $?"

rm -f /tmp/ka-new /tmp/policy-ok.yaml
```

Required: exit **0**, a rule/kind count on stdout, and **no path** in that
output — `/tmp/policy-ok.yaml` must not appear.

### Recording the gate

Append one ledger line per item — `gate: evidence 1 (byte identity) — text,
json, html all identical` and so on — plus the chaos suite's scenario tally.
A gate whose result lives only in a scrollback buffer did not happen.

---

## Execution

Execute with **`superpowers:subagent-driven-development`**: a fresh implementer
subagent per task, an independent task reviewer after each, and a whole-branch
review at the end.

- Implementers and task reviewers run on **sonnet**. Tasks 1–7 are transcription
  plus testing — the plan carries the complete code — and take the cheapest tier
  that reliably compiles Go. Tasks 9–16 are integration work across existing
  files and take a standard tier.
- The **whole-branch review runs on opus**, not the session default.
- Every Critical and Important finding is fixed before merge. Minor findings go
  in the ledger for the whole-branch pass to triage.
- Tasks 1–8 must all be complete before Task 9 starts. `internal/policy`'s
  semantics are consumed by four surfaces; changing them after wiring means
  re-reviewing every surface (see **Sequencing Requirement**).
- Then the **Gate** above, then
  `superpowers:finishing-a-development-branch`, then the `release` skill.
