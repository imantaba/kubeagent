# Curated Policy Packs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a curated reliability rule pack compiled into the binary, listed and printable by `kubeagent policy packs`, and run opt-in by `scan --policy-pack` / `gate --policy-pack`.

**Architecture:** A pack is data, not code. A new stdlib-only package `internal/policypack` embeds the pack YAML and hands out bytes; `internal/cli` turns those bytes into a `policy.Document` and passes it to the existing `policy.Load` and `policy.Evaluate`. Nothing in `internal/policy` changes — no new operator, no new relation, no new selectable kind — so a pack violation is an ordinary policy violation on the path that already ships.

**Tech Stack:** Go 1.26, `embed` (standard library), Cobra/pflag for the CLI, `sigs.k8s.io/yaml` (already vendored, used only inside `internal/policy`).

**Source spec:** `docs/superpowers/specs/2026-08-06-detector-library-policy-packs-design.md` (commit 3ea8bc5 on main).

## Global Constraints

Every task's requirements implicitly include this section.

- **NO NEW DEPENDENCY**: `go.mod` and `go.sum` must not change. `embed` is standard library; `sigs.k8s.io/yaml` is already vendored and is used only by `internal/policy`, not by `internal/policypack`.
- **`internal/policypack` must import NOTHING from kubeagent and nothing outside the standard library** — it joins `internal/jsonschema`, `internal/dashboard`, `internal/baseline`, `internal/glob` and `internal/knownissues`. `imports_test.go` enforces both halves. Its TEST files may import `internal/policy` freely (`internal/policy` does not import `policypack`, so there is no cycle); nothing is added to the non-test import set.
- **READ-ONLY toward the cluster**: a policy rule cannot write, and there is no `--fix` path from a rule. **Separately and additionally: no LLM call.** Never blur the two, and never let help text, docs or a commit message suggest a pack is related to `--explain`, which is the model path.
- **CREDENTIALS**: no secrets, credentials, private IPs or internal hostnames anywhere — code, tests, fixtures, docs, help text, rule messages. The pack YAML names no real host, image, namespace or workload; the one registry example is `registry.example.com`. Documentation IPs are RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains RFC 2606 (`example.com`/`.org`/`.net`). URLs are credentials — nothing beyond `scheme://host`, and the project's own `https://k8sproject.top` links are the only permitted host. `Document.Source` for a pack is `pack:<name>`, never a filesystem path.
- **`internal/report/testdata/golden-scan.txt` must stay BYTE-IDENTICAL.** A pack is opt-in, so a plain scan renders exactly as before. Do NOT regenerate the demo GIF or `website/docs/quickstart.md`.
- **NO JSON document changes**: no new field on `report.PolicyView` or `policy.Violation`, no ninth versioned document, and none of the eight `schemaVersion` values moves (scan stays 1.2, gate stays 1.1). **Do NOT run any test with `-update`.**
- **NO RBAC change**: every rule selects a kind already in `policy.SelectableKinds()`, which is pinned to `rbacprofile.coreRules` by `TestSelectableKindsMatchesRBACProfileCore`. `policy.Load` enforces this, so a rule selecting anything else fails the pack's Load test.
- **NO rule in the pack may be level `critical`.** `gate`'s default is `--fail-on critical`, so adding `--policy-pack` to a pipeline must not fail a build that passed yesterday. A test asserts this.
- **Flags are declared per command, never as persistent flags.** Every command sets `SilenceErrors` and `SilenceUsage`; validation lives in `RunE`, not in Cobra's `Args`/`MarkFlagsMutuallyExclusive` helpers. Use `Args: cobra.ArbitraryArgs` — NOT `ValidArgs`, which would make Cobra validate and reword the error.
- **TDD**: write the failing test first, watch it fail, then implement. No cluster and no fake clientset are needed anywhere in this plan — policy evaluation is pure and takes unstructured objects built in the test.
- Go lives at `/usr/local/go/bin`. `go test` runs with `-p 2` locally, never `-short`.
- Every commit needs `git commit -s` (DCO enforced on main), authored solely by the human — **NO `Co-Authored-By` trailer and no AI attribution of any kind, anywhere.**

## DANGER

**NEVER run `./chaos/run.sh` in any form** — a run takes ~40 minutes and injects real outages. No task in this plan creates, deletes, or touches any cluster; nothing here needs one.

## Two evaluator semantics the rule author must get right

These are already-shipped behaviour of `internal/policy`. Getting them wrong produces rules that look right and check nothing.

1. **`[*]` produces one slot per element, and EVERY slot must satisfy the assertion.** A Deployment with three containers where only one sets a memory limit **violates** — it does not pass because one was found. One object yields at most one violation per rule (the first failing slot wins). See `internal/policy/path.go`'s `Slot` doc comment.

2. **`exists` violates on an absent field; every other operator SKIPS on an absent field.** So a rule that must catch "the field is missing" uses `exists`. A rule using `matches`/`in`/`gte` silently passes an object where the field is absent. This is why the `spec.replicas` test fixture below sets `replicas: 1` explicitly rather than omitting it — an omitted `replicas` would make the `gte` rule skip, and the test would prove nothing.

A third consequence worth stating: an **empty** container list yields **zero** slots, so `exists` still violates through `check`'s `!anySlot` branch, but `gte`/`matches` do not.

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/policypack/policypack.go` (create) | `Pack` type, `All`/`Lookup`/`Bytes`, the `//go:embed` directive and the pack table. Stdlib only. |
| `internal/policypack/packs/reliability.yaml` (create) | The fourteen reliability rules. Data, embedded at build time. |
| `internal/policypack/imports_test.go` (create) | The two walls: no kubeagent import, stdlib only. Copied from `internal/baseline/imports_test.go`. |
| `internal/policypack/policypack_test.go` (create) | Package-local unit tests — no `internal/policy` import. |
| `internal/policypack/packs_test.go` (create) | The tests that need the loader: `Load` succeeds, id prefix, no `critical`, credential scan. |
| `internal/policypack/rules_test.go` (create) | The per-rule behavioural table — fourteen rules × a violating and a satisfying fixture. |
| `internal/cli/policy.go` (modify) | `packDocuments`, the `packs` subcommand with `--print`, and the pack parameter threaded through `loadPolicy` and `evaluatePolicy`. |
| `internal/cli/policy_test.go` (modify) | Tests for the new verb, `--print`, and the unknown-name error. |
| `internal/cli/scan.go` (modify) | `--policy-pack` flag and field. |
| `internal/cli/gate.go` (modify) | `--policy-pack` flag and field. |
| `internal/cli/root.go` (modify) | `usageError()` gains `policy packs` and the two `--policy-pack` spellings. |
| `internal/cli/surface_test.go` (modify) | `--policy-pack` joins the flags `fleet` refuses. |
| `website/docs/features/policy-packs.md` (create) | The feature page. |
| `website/docs/features/policy.md`, `website/mkdocs.yml`, `website/docs/roadmap.md`, `website/docs/compatibility.md`, `CHANGELOG.md`, `CLAUDE.md` (modify) | Pointers, nav, roadmap, the read-only command list, changelog, project invariants. |

---

### Task 1: `internal/policypack` — the package, the pack, and its walls

Creates the branch, the stdlib-only package, the embedded YAML with all fourteen rules, and the tests that need no loader.

**Files:**
- Create: `internal/policypack/policypack.go`
- Create: `internal/policypack/packs/reliability.yaml`
- Create: `internal/policypack/imports_test.go`
- Create: `internal/policypack/policypack_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces, for Tasks 2–5:
  - `type Pack struct { Name string; Summary string }`
  - `func All() []Pack` — every pack, sorted by `Name`
  - `func Lookup(name string) (Pack, bool)` — exact match, no case folding
  - `func Bytes(name string) ([]byte, bool)` — the pack's YAML, a fresh copy each call
  - `func Names() []string` — every pack name, sorted (used by the CLI's unknown-name error)

- [ ] **Step 1: Cut the branch**

```bash
export PATH=$PATH:/usr/local/go/bin
cd /home/ubuntu/git/kubeagent
git checkout main
git checkout -b policy-packs
git log --oneline -1   # expect 3ea8bc5 spec: curated policy packs …
```

- [ ] **Step 2: Write the failing wall tests**

Create `internal/policypack/imports_test.go`. This is `internal/baseline/imports_test.go` with the package name and the prose changed — read that file and follow it exactly rather than inventing a variant.

```go
package policypack

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulePath is kubeagent's module path. Any import beginning with it is an
// import of kubeagent.
const modulePath = "github.com/imantaba/kubeagent"

// TestNoKubeagentImport is the structural half of this package's contract.
// internal/policypack holds curated rule data that reaches a report and a
// gate; the design makes reaching internal/remediate or internal/explain
// impossible by construction rather than by a rule someone has to remember,
// by forbidding every kubeagent import. It is the same class as
// internal/baseline, internal/glob, internal/knownissues, internal/dashboard
// and internal/jsonschema.
//
// Only non-test files are walked: a test may import internal/policy to prove
// the pack loads without weakening what the shipped package can reach.
func TestNoKubeagentImport(t *testing.T) {
	for _, path := range packageFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, p := range importsOf(t, path) {
			if strings.HasPrefix(p, modulePath) {
				t.Errorf("%s imports %s — internal/policypack must import nothing from kubeagent", path, p)
			}
		}
	}
}

// TestStdlibOnly is the second half: internal/policypack may import nothing
// outside the standard library either, so go.mod can never move because of
// this package. The convention Go itself uses is that a module path's first
// segment contains a dot; a standard-library import path never does.
func TestStdlibOnly(t *testing.T) {
	for _, path := range packageFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, p := range importsOf(t, path) {
			first, _, _ := strings.Cut(p, "/")
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %s — internal/policypack must import only the standard library", path, p)
			}
		}
	}
}

func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, imp := range f.Imports {
		p, uerr := strconv.Unquote(imp.Path.Value)
		if uerr != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// packageFiles lists this package's Go files. The test binary runs with the
// package directory as its working directory, so a glob is enough — no walk,
// and no dependency on where the repository is checked out.
func packageFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing package files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found — the guard tests would pass vacuously")
	}
	return files
}
```

- [ ] **Step 3: Write the failing package unit tests**

Create `internal/policypack/policypack_test.go`:

```go
package policypack

import (
	"bytes"
	"sort"
	"testing"
)

func TestAllIsSortedByName(t *testing.T) {
	packs := All()
	if len(packs) == 0 {
		t.Fatal("no packs — every later assertion would pass vacuously")
	}
	names := make([]string, len(packs))
	for i, p := range packs {
		names[i] = p.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("All() = %v, want sorted by name", names)
	}
}

func TestEveryPackHasASummary(t *testing.T) {
	for _, p := range All() {
		if p.Summary == "" {
			t.Errorf("pack %q has no summary — the listing would print a bare name", p.Name)
		}
	}
}

func TestLookupIsExact(t *testing.T) {
	if _, ok := Lookup("reliability"); !ok {
		t.Fatal(`Lookup("reliability") = false, want the shipped pack`)
	}
	// No case folding and no fuzzy match: the name is the join between the
	// listing and the flag, so a near miss must be refused rather than guessed.
	for _, miss := range []string{"Reliability", "RELIABILITY", "reliabilit", "", "security"} {
		if _, ok := Lookup(miss); ok {
			t.Errorf("Lookup(%q) = true, want false", miss)
		}
	}
}

func TestNamesMatchesAll(t *testing.T) {
	packs := All()
	names := Names()
	if len(names) != len(packs) {
		t.Fatalf("Names() has %d entries, All() has %d", len(names), len(packs))
	}
	for i, p := range packs {
		if names[i] != p.Name {
			t.Errorf("Names()[%d] = %q, want %q", i, names[i], p.Name)
		}
	}
}

func TestBytesReturnsACopy(t *testing.T) {
	first, ok := Bytes("reliability")
	if !ok {
		t.Fatal(`Bytes("reliability") = false, want the shipped pack`)
	}
	if len(first) == 0 {
		t.Fatal("the pack is empty")
	}
	original := append([]byte(nil), first...)
	// A caller that mutates what it was handed must not reach the next caller.
	for i := range first {
		first[i] = 'x'
	}
	second, _ := Bytes("reliability")
	if !bytes.Equal(second, original) {
		t.Error("Bytes returned a view of the embedded pack — mutating it changed what the next caller sees")
	}
}

func TestBytesMissReturnsFalse(t *testing.T) {
	if _, ok := Bytes("no-such-pack"); ok {
		t.Error(`Bytes("no-such-pack") = true, want false`)
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test ./internal/policypack/`
Expected: FAIL — the package does not exist (`no Go files in .../internal/policypack`).

- [ ] **Step 5: Write the embedded pack**

Create `internal/policypack/packs/reliability.yaml` exactly as follows. Transcribe it; do not paraphrase a message or renumber an id.

```yaml
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

- id: reliability.deploy-liveness-probe
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].livenessProbe
    op: exists
  level: info
  message: a container has no liveness probe, so a wedged process is never restarted

- id: reliability.statefulset-readiness-probe
  match:
    kind: StatefulSet
  assert:
    path: spec.template.spec.containers[*].readinessProbe
    op: exists
  level: warning
  message: a container has no readiness probe, so an ordered rollout cannot tell when a replica is ready

- id: reliability.daemonset-readiness-probe
  match:
    kind: DaemonSet
  assert:
    path: spec.template.spec.containers[*].readinessProbe
    op: exists
  level: info
  message: a container has no readiness probe, so a node-local failure stays invisible

- id: reliability.deploy-memory-limit
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].resources.limits.memory
    op: exists
  level: warning
  message: a container has no memory limit, so a leak is bounded only by the node

- id: reliability.statefulset-memory-limit
  match:
    kind: StatefulSet
  assert:
    path: spec.template.spec.containers[*].resources.limits.memory
    op: exists
  level: warning
  message: a container has no memory limit, so a leak is bounded only by the node

- id: reliability.deploy-cpu-request
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].resources.requests.cpu
    op: exists
  level: info
  message: a container has no CPU request, so the scheduler places it as if it were free

- id: reliability.deploy-memory-request
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].resources.requests.memory
    op: exists
  level: info
  message: a container has no memory request, so the scheduler places it as if it were free

- id: reliability.deploy-image-not-latest
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].image
    op: notMatches
    values:
      - "*:latest"
  level: warning
  message: a container image is pinned to the latest tag, so a restart can change the code that runs

- id: reliability.deploy-image-tagged
  match:
    kind: Deployment
  assert:
    path: spec.template.spec.containers[*].image
    op: matches
    values:
      - "*:*"
  level: info
  message: a container image carries no tag or digest, so the runtime resolves it to latest

- id: reliability.deploy-replicas-min-two
  match:
    kind: Deployment
  assert:
    path: spec.replicas
    op: gte
    values:
      - "2"
  level: warning
  message: the Deployment runs a single replica, so any node drain is an outage

- id: reliability.deploy-pdb
  match:
    kind: Deployment
  assert:
    relation: hasPodDisruptionBudget
  level: warning
  message: no PodDisruptionBudget covers this Deployment, so a drain can evict every replica at once

- id: reliability.cronjob-concurrency-policy
  match:
    kind: CronJob
  assert:
    path: spec.concurrencyPolicy
    op: in
    values:
      - Forbid
      - Replace
  level: info
  message: the CronJob allows concurrent runs, so a slow job can pile up on itself

- id: reliability.pvc-storage-class
  match:
    kind: PersistentVolumeClaim
  assert:
    path: spec.storageClassName
    op: exists
  level: info
  message: the claim names no storage class, so it binds only if a default class exists
```

- [ ] **Step 6: Write the package**

Create `internal/policypack/policypack.go`:

```go
// Package policypack holds kubeagent's curated policy packs: rule sets
// compiled into the binary and run by name, so an operator with only a
// container image or a krew install has them.
//
// A pack is data, not code. The bytes here are handed to internal/policy,
// which is the only thing that parses or evaluates them — which is what makes
// a curated rule incapable of writing anything, panicking a scan, or widening
// RBAC. There is no --fix path from a rule.
//
// It holds no client and no context, issues no cluster call and makes no model
// call — two separate promises, and neither implies the other. In particular a
// pack has nothing to do with --explain, which is the model path.
//
// The package imports nothing from kubeagent and nothing outside the standard
// library, which puts it in the same class as internal/jsonschema,
// internal/dashboard, internal/baseline, internal/glob and internal/knownissues
// and makes reaching internal/remediate or internal/explain impossible by
// construction rather than by rule. internal/policypack/imports_test.go
// enforces both halves. The consequence is that this package cannot parse its
// own YAML, so it does not claim to know how many rules a pack holds: the
// caller counts by loading (see internal/cli/policy.go). A number that cannot
// be wrong beats a number that has to be checked.
package policypack

import (
	"embed"
	"sort"
)

//go:embed packs/*.yaml
var files embed.FS

// Pack is one curated rule set, compiled into the binary.
type Pack struct {
	// Name is how an operator selects the pack: `--policy-pack <name>`.
	// Lookup is exact — it is the join between the listing and the flag.
	Name string

	// Summary is one line for the listing: lowercase, no trailing period.
	Summary string

	// file is the embedded path. Unexported: a caller gets bytes, never a
	// path, so nothing downstream can learn where the pack lives.
	file string
}

// packs is the registry, in name order.
var packs = []Pack{
	{
		Name:    "reliability",
		Summary: "probes, resource requests and limits, replica counts, disruption budgets and image tags",
		file:    "packs/reliability.yaml",
	},
}

// All returns every pack, sorted by name. The slice is fresh, so a caller may
// sort, filter or truncate what it is handed without any of it reaching the
// next caller.
func All() []Pack {
	out := make([]Pack, len(packs))
	copy(out, packs)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns every pack name, sorted.
func Names() []string {
	out := make([]string, 0, len(packs))
	for _, p := range All() {
		out = append(out, p.Name)
	}
	return out
}

// Lookup finds a pack by name. The match is exact: no case folding and no
// fuzzy match, for the same reason `known-issues` refuses one — a near miss
// that silently resolved would run rules the operator did not ask for.
func Lookup(name string) (Pack, bool) {
	for _, p := range packs {
		if p.Name == name {
			return p, true
		}
	}
	return Pack{}, false
}

// Bytes returns a pack's YAML. The result is a fresh copy: a caller that
// mutates what it was handed must not change what the next caller sees.
func Bytes(name string) ([]byte, bool) {
	p, ok := Lookup(name)
	if !ok {
		return nil, false
	}
	data, err := files.ReadFile(p.file)
	if err != nil {
		// The file is embedded at build time, so this cannot happen without a
		// registry entry naming a file that is not there — which the package's
		// own tests catch before a build ships.
		return nil, false
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, true
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/policypack/ -v`
Expected: PASS — `TestNoKubeagentImport`, `TestStdlibOnly`, `TestAllIsSortedByName`, `TestEveryPackHasASummary`, `TestLookupIsExact`, `TestNamesMatchesAll`, `TestBytesReturnsACopy`, `TestBytesMissReturnsFalse`.

- [ ] **Step 8: Verify go.mod did not move**

Run: `git status --short go.mod go.sum`
Expected: no output. If either file changed, something outside the standard library was imported — fix it before committing.

- [ ] **Step 9: Commit**

```bash
git add internal/policypack/
git commit -s -m "feat: internal/policypack — the curated reliability pack, embedded

A pack is data, not code: the bytes here are handed to internal/policy,
which is the only thing that parses or evaluates them. That is what keeps
a curated rule incapable of writing, panicking a scan, or widening RBAC.

The package imports nothing from kubeagent and nothing outside the
standard library, joining internal/baseline, internal/glob and
internal/knownissues. It therefore cannot count its own rules, and does
not claim to — the caller counts by loading."
```

---

### Task 2: The pack loads, and keeps its promises

The tests that need the loader. `policy.Load` already refuses an invalid id, a non-selectable kind, a malformed assert, an unknown level, an empty message, and a duplicate id across documents — so one `Load` succeeds assertion covers all of that. **Do not re-assert any of it**; a redundant re-assertion is a defect a reviewer should reject.

These tests assert what `Load` does *not*.

**Files:**
- Create: `internal/policypack/packs_test.go`

**Interfaces:**
- Consumes: `policypack.All`, `policypack.Bytes` (Task 1); `policy.Document{Source,Data}`, `policy.Load([]Document) ([]Rule, error)`, `policy.LevelCritical` (already in the tree).
- Produces: `func loadPack(t *testing.T, name string) []policy.Rule` — used by Task 3's rule table.

- [ ] **Step 1: Write the failing tests**

Create `internal/policypack/packs_test.go`:

```go
package policypack_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/policy"
	"github.com/imantaba/kubeagent/internal/policypack"
)

// loadPack loads one pack through the real loader. Source is "pack:<name>",
// which is what the CLI uses too: a pack has no filesystem path, so there is
// none to leak into an error.
func loadPack(t *testing.T, name string) []policy.Rule {
	t.Helper()
	data, ok := policypack.Bytes(name)
	if !ok {
		t.Fatalf("Bytes(%q) = false, want the shipped pack", name)
	}
	rules, err := policy.Load([]policy.Document{{Source: "pack:" + name, Data: data}})
	if err != nil {
		t.Fatalf("loading pack %q: %v", name, err)
	}
	return rules
}

// TestEveryPackLoads is the one assertion that covers most of the contract.
// policy.Load already refuses an empty or malformed rule id, a kind that is
// not selectable, a cluster-scoped kind carrying namespace selectors, an
// assert that sets both or neither of path and relation, an unknown level and
// an empty message — and sigs.k8s.io/yaml's UnmarshalStrict refuses an unknown
// key, so a typo in the pack fails here rather than being ignored. Re-asserting
// any of that separately would be testing Load.
//
// The selectable-kind half is also the RBAC promise: the kinds a rule may
// select are exactly the kinds rbacprofile's core rules already grant, so a
// pack can never require a grant kubeagent does not already ask for.
func TestEveryPackLoads(t *testing.T) {
	for _, p := range policypack.All() {
		t.Run(p.Name, func(t *testing.T) {
			if rules := loadPack(t, p.Name); len(rules) == 0 {
				t.Error("the pack loaded but holds no rules")
			}
		})
	}
}

// TestRuleIDsCarryTheirPackPrefix keeps a pack's ids in its own namespace, so
// giving --policy-pack and --policy together cannot collide by accident. Load
// detects a collision across documents; this is what makes one unlikely.
func TestRuleIDsCarryTheirPackPrefix(t *testing.T) {
	for _, p := range policypack.All() {
		t.Run(p.Name, func(t *testing.T) {
			prefix := p.Name + "."
			for _, r := range loadPack(t, p.Name) {
				if !strings.HasPrefix(r.ID, prefix) {
					t.Errorf("rule id %q does not start with %q", r.ID, prefix)
				}
			}
		})
	}
}

// TestNoPackRuleIsCritical is the promise that adding a pack to a pipeline
// cannot fail a build that passed yesterday: gate's default is
// --fail-on critical, and no curated rule reaches that level. An operator who
// wants these to block raises --fail-on warning, which is an explicit act.
func TestNoPackRuleIsCritical(t *testing.T) {
	for _, p := range policypack.All() {
		t.Run(p.Name, func(t *testing.T) {
			for _, r := range loadPack(t, p.Name) {
				if r.Level == policy.LevelCritical {
					t.Errorf("rule %q is critical — adding this pack to a gate would fail it at default settings", r.ID)
				}
			}
		})
	}
}

// hostish matches anything that looks like a URL or a bare IPv4 address.
var hostish = regexp.MustCompile(`://|\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// TestPackCarriesNoHostOrAddress is the credential wall. A violation's message
// reaches a terminal, a JSON document, a SARIF upload and an HTML report — all
// of them forwarded artifacts — so no rule may carry a host, a URL or an
// address. The rules assert about shapes, not about anyone's infrastructure.
func TestPackCarriesNoHostOrAddress(t *testing.T) {
	for _, p := range policypack.All() {
		t.Run(p.Name, func(t *testing.T) {
			data, _ := policypack.Bytes(p.Name)
			if loc := hostish.FindString(string(data)); loc != "" {
				t.Errorf("the pack carries %q — a rule may not name a host or an address", loc)
			}
			for _, r := range loadPack(t, p.Name) {
				if loc := hostish.FindString(r.Message); loc != "" {
					t.Errorf("rule %q message carries %q", r.ID, loc)
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/policypack/ -run 'TestEveryPackLoads|TestRuleIDs|TestNoPackRule|TestPackCarriesNo' -v`

Expected: FAIL if any rule in Task 1's YAML is malformed. If they pass on the first run, that is legitimate — Task 1 wrote the data these tests describe, and the TDD cycle for this task is "the tests must be able to fail". Prove they can by temporarily changing one rule's `level` to `critical` in `packs/reliability.yaml`, running `go test ./internal/policypack/ -run TestNoPackRuleIsCritical` and watching it fail, then reverting the change. Record in the report that you did this.

- [ ] **Step 3: Run the whole package and confirm green**

Run: `go test ./internal/policypack/ -v`
Expected: PASS, all tests from Tasks 1 and 2.

- [ ] **Step 4: Commit**

```bash
git add internal/policypack/packs_test.go
git commit -s -m "test: the pack loads, keeps its prefix, and never reaches critical

policy.Load already refuses a bad id, a non-selectable kind, a malformed
assert, an unknown level and a duplicate — so one Load assertion covers
all of it, and the rest of these tests assert only what Load does not:
the pack-name prefix that keeps ids from colliding with an operator's
own, the absence of any critical rule, and the credential wall over
every rule message."
```

---

### Task 3: Every rule is proved, not asserted

A table that drives each of the fourteen rules through the real evaluator with an object that violates it and one that satisfies it. A rule that checks nothing — a typo'd path, a wrong operator — passes Task 2 and fails here.

**Files:**
- Create: `internal/policypack/rules_test.go`

**Interfaces:**
- Consumes: `loadPack` (Task 2); `policy.Evaluate(rules []Rule, in Inputs) ([]Violation, []Unevaluated)`, `policy.InputsFrom(objects map[string][]*unstructured.Unstructured, unreadable map[string]bool) Inputs`.
- Note: `InputsFrom` wires `Inputs.PDBs` from `objects["PodDisruptionBudget"]` itself, so the PDB case supplies the budget through the objects map like any other kind — it does not build `Inputs` by hand.
- Produces: nothing later tasks consume.

- [ ] **Step 1: Write the failing test**

Create `internal/policypack/rules_test.go`:

```go
package policypack_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/policy"
)

// The fixtures below use one namespace and one label set throughout. Neither
// names anything real: a rule is about a shape, not about infrastructure.
const (
	fixtureNamespace = "app"
	fixtureImage     = "registry.example.com/team/app:1.0"
)

// goodContainer satisfies every container-level rule in the pack. Each case
// below starts from it and removes or changes exactly the one thing its rule
// is about, so a case can only fail for its own reason.
func goodContainer() map[string]any {
	return map[string]any{
		"name":           "app",
		"image":          fixtureImage,
		"readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/readyz", "port": int64(8080)}},
		"livenessProbe":  map[string]any{"httpGet": map[string]any{"path": "/healthz", "port": int64(8080)}},
		"resources": map[string]any{
			"limits":   map[string]any{"memory": "512Mi"},
			"requests": map[string]any{"cpu": "100m", "memory": "256Mi"},
		},
	}
}

// containerWithout returns a good container with one field removed. The path
// is walked so a nested field can be removed too, which is what the resource
// rules need: containerWithout("resources", "limits", "memory").
func containerWithout(t *testing.T, path ...string) map[string]any {
	t.Helper()
	c := goodContainer()
	m := c
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			t.Fatalf("containerWithout(%v): %q is not a map", path, k)
		}
		m = next
	}
	delete(m, path[len(path)-1])
	return c
}

// containerWithImage returns a good container with a different image.
func containerWithImage(image string) map[string]any {
	c := goodContainer()
	c["image"] = image
	return c
}

// workload builds a Deployment, StatefulSet or DaemonSet around one container.
// replicas is set by default so the replica rule's `gte` has something to
// compare — an absent field makes every operator except exists SKIP, and a
// fixture that made the rule skip would prove nothing.
func workload(kind, name string, c map[string]any, replicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"replicas": replicas,
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
				"spec":     map[string]any{"containers": []any{c}},
			},
		},
	}}
}

func deployment(name string, c map[string]any) *unstructured.Unstructured {
	return workload("Deployment", name, c, 2)
}

// pdb builds a PodDisruptionBudget selecting the pod template labels workload
// stamps. coveredByPDB reads spec.selector.matchLabels and compares against
// spec.template.metadata.labels, in the same namespace.
func pdb(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "PodDisruptionBudget",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
		},
	}}
}

func cronJob(name, concurrencyPolicy string) *unstructured.Unstructured {
	spec := map[string]any{"schedule": "*/5 * * * *"}
	if concurrencyPolicy != "" {
		spec["concurrencyPolicy"] = concurrencyPolicy
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec":       spec,
	}}
}

func pvc(name, storageClass string) *unstructured.Unstructured {
	spec := map[string]any{"accessModes": []any{"ReadWriteOnce"}}
	if storageClass != "" {
		spec["storageClassName"] = storageClass
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec":       spec,
	}}
}

// ruleCase is one rule proved from both sides.
type ruleCase struct {
	id   string
	kind string // the Objects key the rule selects
	// violating must produce exactly one violation of this rule.
	violating *unstructured.Unstructured
	// satisfying must produce none.
	satisfying *unstructured.Unstructured
	// support is extra objects added to the SATISFYING run only, keyed by
	// kind. Only the PDB rule needs it: the point of that case is that the
	// budget is absent in one run and present in the other.
	support map[string][]*unstructured.Unstructured
}

// TestEveryReliabilityRuleFiresAndPasses drives each rule through the real
// evaluator, alone, against an object that must violate it and one that must
// not. A rule with a typo'd path or the wrong operator loads cleanly and
// checks nothing; this is what catches that.
func TestEveryReliabilityRuleFiresAndPasses(t *testing.T) {
	rules := loadPack(t, "reliability")
	byID := map[string]policy.Rule{}
	for _, r := range rules {
		byID[r.ID] = r
	}

	cases := []ruleCase{
		{
			id:         "reliability.deploy-readiness-probe",
			kind:       "Deployment",
			violating:  deployment("no-readiness", containerWithout(t, "readinessProbe")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:         "reliability.deploy-liveness-probe",
			kind:       "Deployment",
			violating:  deployment("no-liveness", containerWithout(t, "livenessProbe")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:         "reliability.statefulset-readiness-probe",
			kind:       "StatefulSet",
			violating:  workload("StatefulSet", "no-readiness", containerWithout(t, "readinessProbe"), 2),
			satisfying: workload("StatefulSet", "ok", goodContainer(), 2),
		},
		{
			id:         "reliability.daemonset-readiness-probe",
			kind:       "DaemonSet",
			violating:  workload("DaemonSet", "no-readiness", containerWithout(t, "readinessProbe"), 1),
			satisfying: workload("DaemonSet", "ok", goodContainer(), 1),
		},
		{
			id:         "reliability.deploy-memory-limit",
			kind:       "Deployment",
			violating:  deployment("no-mem-limit", containerWithout(t, "resources", "limits", "memory")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:         "reliability.statefulset-memory-limit",
			kind:       "StatefulSet",
			violating:  workload("StatefulSet", "no-mem-limit", containerWithout(t, "resources", "limits", "memory"), 2),
			satisfying: workload("StatefulSet", "ok", goodContainer(), 2),
		},
		{
			id:         "reliability.deploy-cpu-request",
			kind:       "Deployment",
			violating:  deployment("no-cpu-request", containerWithout(t, "resources", "requests", "cpu")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:         "reliability.deploy-memory-request",
			kind:       "Deployment",
			violating:  deployment("no-mem-request", containerWithout(t, "resources", "requests", "memory")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:         "reliability.deploy-image-not-latest",
			kind:       "Deployment",
			violating:  deployment("floating", containerWithImage("registry.example.com/team/app:latest")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:   "reliability.deploy-image-tagged",
			kind: "Deployment",
			// No colon at all, so no tag and no digest.
			violating:  deployment("untagged", containerWithImage("registry.example.com/team/app")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:   "reliability.deploy-replicas-min-two",
			kind: "Deployment",
			// replicas is set explicitly: gte SKIPS an absent field, so a
			// fixture that omitted it would make the rule pass for the wrong
			// reason.
			violating:  workload("Deployment", "single", goodContainer(), 1),
			satisfying: workload("Deployment", "paired", goodContainer(), 2),
		},
		{
			id:         "reliability.deploy-pdb",
			kind:       "Deployment",
			violating:  deployment("unbudgeted", goodContainer()),
			satisfying: deployment("budgeted", goodContainer()),
			support: map[string][]*unstructured.Unstructured{
				"PodDisruptionBudget": {pdb("web")},
			},
		},
		{
			id:         "reliability.cronjob-concurrency-policy",
			kind:       "CronJob",
			violating:  cronJob("piles-up", "Allow"),
			satisfying: cronJob("serialized", "Forbid"),
		},
		{
			id:         "reliability.pvc-storage-class",
			kind:       "PersistentVolumeClaim",
			violating:  pvc("classless", ""),
			satisfying: pvc("fast", "standard"),
		},
	}

	if len(cases) != len(rules) {
		t.Fatalf("%d cases for %d rules — every rule must be proved from both sides", len(cases), len(rules))
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			r, ok := byID[tc.id]
			if !ok {
				t.Fatalf("the pack has no rule %q", tc.id)
			}
			only := []policy.Rule{r}

			// The violating side.
			objects := map[string][]*unstructured.Unstructured{tc.kind: {tc.violating}}
			violations, notEvaluated := policy.Evaluate(only, policy.InputsFrom(objects, nil))
			if len(notEvaluated) != 0 {
				t.Fatalf("nothing was unreadable, got %#v", notEvaluated)
			}
			if len(violations) != 1 {
				t.Fatalf("violating object produced %d violations, want 1", len(violations))
			}
			if violations[0].RuleID != tc.id {
				t.Errorf("violation is from rule %q, want %q", violations[0].RuleID, tc.id)
			}
			if violations[0].Level == policy.LevelCritical {
				t.Errorf("violation is critical — no pack rule may be")
			}

			// The satisfying side.
			objects = map[string][]*unstructured.Unstructured{tc.kind: {tc.satisfying}}
			for k, v := range tc.support {
				objects[k] = append(objects[k], v...)
			}
			violations, notEvaluated = policy.Evaluate(only, policy.InputsFrom(objects, nil))
			if len(notEvaluated) != 0 {
				t.Fatalf("nothing was unreadable, got %#v", notEvaluated)
			}
			if len(violations) != 0 {
				t.Errorf("satisfying object produced %d violations, want 0: %#v", len(violations), violations)
			}
		})
	}
}

// TestWildcardRequiresEverySlot pins the semantic the container rules depend
// on: a wildcard produces one slot per element and every slot must satisfy the
// assertion, so a Deployment where only ONE of two containers sets a memory
// limit violates. Collapsing to "a value was found somewhere" would silently
// pass it, and every probe and resource rule in the pack would become
// decorative.
func TestWildcardRequiresEverySlot(t *testing.T) {
	rules := loadPack(t, "reliability")
	var r policy.Rule
	for _, candidate := range rules {
		if candidate.ID == "reliability.deploy-memory-limit" {
			r = candidate
		}
	}
	if r.ID == "" {
		t.Fatal("the pack has no reliability.deploy-memory-limit rule")
	}

	mixed := deployment("mixed", goodContainer())
	containers := []any{goodContainer(), containerWithout(t, "resources", "limits", "memory")}
	containers[1].(map[string]any)["name"] = "sidecar"
	spec := mixed.Object["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	template["spec"].(map[string]any)["containers"] = containers

	objects := map[string][]*unstructured.Unstructured{"Deployment": {mixed}}
	violations, _ := policy.Evaluate([]policy.Rule{r}, policy.InputsFrom(objects, nil))
	if len(violations) != 1 {
		t.Fatalf("a Deployment where one of two containers sets a memory limit produced %d violations, want 1", len(violations))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/policypack/ -run TestEveryReliabilityRuleFiresAndPasses -v`

Expected: PASS if Task 1's YAML is correct. As in Task 2, the TDD obligation is to prove the test can fail: temporarily change `reliability.deploy-memory-limit`'s path in `packs/reliability.yaml` to `spec.template.spec.containers[*].resources.limits.cpu`, run the test, watch the `reliability.deploy-memory-limit` subtest fail on the satisfying side, then revert. Record this in the report.

If a subtest fails without your having introduced a fault, the rule is wrong — fix `packs/reliability.yaml`, not the test.

- [ ] **Step 3: Run the whole package**

Run: `go test ./internal/policypack/ -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/policypack/rules_test.go
git commit -s -m "test: prove each reliability rule from both sides

A rule with a typo'd path or the wrong operator loads cleanly and checks
nothing. Each of the fourteen now runs alone against an object that must
violate it and one that must not, and a separate test pins the wildcard
semantic the probe and resource rules depend on: every slot must satisfy,
so one container out of two setting a memory limit still violates."
```

---

### Task 4: `kubeagent policy packs` — list and print

The discovery surface. Lists what ships and prints a pack's YAML so an operator can fork a rule instead of arguing with it. Contacts nothing: no cluster, no kubeconfig, no network, no model.

**Files:**
- Modify: `internal/cli/policy.go`
- Modify: `internal/cli/policy_test.go`
- Modify: `internal/cli/root.go` (the `usageError()` string)

**Interfaces:**
- Consumes: `policypack.All`, `policypack.Bytes`, `policypack.Names` (Task 1); `policy.Load` (in the tree).
- Produces, for Task 5: `func packDocuments(names []string) ([]policy.Document, error)`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/cli/policy_test.go`:

```go
func TestPolicyPacksListsWhatShips(t *testing.T) {
	var buf bytes.Buffer
	if err := runPolicyPacks(nil, "", &buf); err != nil {
		t.Fatalf("runPolicyPacks: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "reliability") {
		t.Errorf("the listing does not name the reliability pack:\n%s", out)
	}
	// The count comes from loading, so it cannot drift from the file.
	if !strings.Contains(out, "14 rules") {
		t.Errorf("the listing does not carry a rule count:\n%s", out)
	}
	if !strings.Contains(out, "kubeagent policy packs --print") {
		t.Errorf("the listing does not say how to print one:\n%s", out)
	}
}

func TestPolicyPacksPrintEmitsLoadableYAML(t *testing.T) {
	var buf bytes.Buffer
	if err := runPolicyPacks(nil, "reliability", &buf); err != nil {
		t.Fatalf("runPolicyPacks --print: %v", err)
	}
	// What is printed must be what the flag would run: load it back.
	rules, err := policy.Load([]policy.Document{{Source: "stdin", Data: buf.Bytes()}})
	if err != nil {
		t.Fatalf("the printed pack does not load: %v", err)
	}
	if len(rules) != 14 {
		t.Errorf("printed pack has %d rules, want 14", len(rules))
	}
}

func TestPolicyPacksPrintUnknownNameIsRefused(t *testing.T) {
	var buf bytes.Buffer
	err := runPolicyPacks(nil, "no-such-pack", &buf)
	if err == nil {
		t.Fatal("an unknown pack name was accepted")
	}
	if !strings.Contains(err.Error(), `"no-such-pack"`) {
		t.Errorf("the error does not quote the name given: %v", err)
	}
	if !strings.Contains(err.Error(), "reliability") {
		t.Errorf("the error does not name the packs that do exist: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused name still wrote to stdout: %q", buf.String())
	}
}

func TestPolicyPacksRefusesPositionalArguments(t *testing.T) {
	var buf bytes.Buffer
	err := runPolicyPacks([]string{"reliability"}, "", &buf)
	if err == nil {
		t.Fatal("a positional argument was accepted; the pack name goes to --print")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Errorf("the error is not a usage error: %v", err)
	}
}

func TestPackDocumentsCarryNoFilesystemPath(t *testing.T) {
	docs, err := packDocuments([]string{"reliability"})
	if err != nil {
		t.Fatalf("packDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("packDocuments returned %d documents, want 1", len(docs))
	}
	if docs[0].Source != "pack:reliability" {
		t.Errorf("Source = %q, want %q — a pack has no path, so none may leak", docs[0].Source, "pack:reliability")
	}
}

func TestPackDocumentsRefusesAnUnknownName(t *testing.T) {
	if _, err := packDocuments([]string{"no-such-pack"}); err == nil {
		t.Fatal("an unknown pack name was accepted")
	}
}

// policy.Load treats empty or nil YAML as a valid, empty document rather
// than an error, so nothing downstream of requirePackBytes would ever catch
// a broken embed on its own. This drives the guard directly with empty
// bytes, since no shipped pack is ever actually empty: there is no way to
// reach this branch through packDocuments or runPolicyPacks against the
// real embedded registry.
func TestRequirePackBytesRefusesEmptyBytes(t *testing.T) {
	_, err := requirePackBytes("reliability", nil)
	if err == nil {
		t.Fatal("empty pack bytes were accepted")
	}
	if !strings.Contains(err.Error(), `"reliability"`) {
		t.Errorf("the error does not name the pack: %v", err)
	}
}

func TestRequirePackBytesAcceptsNonEmptyBytes(t *testing.T) {
	data, err := requirePackBytes("reliability", []byte("- id: x\n"))
	if err != nil {
		t.Fatalf("requirePackBytes: %v", err)
	}
	if string(data) != "- id: x\n" {
		t.Errorf("requirePackBytes changed the bytes: got %q", data)
	}
}

// The two surfaces that can miss a pack name — packDocuments and
// runPolicyPacks --print — must describe the same miss the same way, so a
// future edit to one cannot quietly drift from the other.
func TestUnknownPackErrorIsIdenticalAcrossBothSurfaces(t *testing.T) {
	_, docErr := packDocuments([]string{"no-such-pack"})
	var buf bytes.Buffer
	printErr := runPolicyPacks(nil, "no-such-pack", &buf)
	if docErr == nil || printErr == nil {
		t.Fatal("expected both surfaces to refuse the unknown name")
	}
	if docErr.Error() != printErr.Error() {
		t.Errorf("packDocuments error = %q, runPolicyPacks --print error = %q, want identical",
			docErr.Error(), printErr.Error())
	}
}
```

Ensure `internal/cli/policy_test.go` imports `bytes`, `strings`, `testing` and `github.com/imantaba/kubeagent/internal/policy` — add whichever are missing.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestPolicyPacks|TestPackDocuments' -v`
Expected: FAIL — `undefined: runPolicyPacks`, `undefined: packDocuments`, `undefined: requirePackBytes`.

- [ ] **Step 3: Implement**

Add to `internal/cli/policy.go` (and add `"github.com/imantaba/kubeagent/internal/policypack"` to its import block):

```go
// unknownPackErr reports a pack name that policypack does not have, naming
// the packs that do exist so the operator can pick a real one. packDocuments
// and runPolicyPacks's --print both reach an unknown name through
// policypack.Bytes's ok return; sharing the wording keeps the two from
// quietly drifting apart on how they describe the same miss.
func unknownPackErr(name string) error {
	return fmt.Errorf("unknown policy pack %q (want %s)", name, strings.Join(policypack.Names(), ", "))
}

// requirePackBytes turns a pack's raw bytes into either the bytes unchanged
// or an error naming the pack. policy.Load treats empty or nil YAML as a
// valid, empty document, not an error — so an empty result passed through
// unchecked would silently run, list, or print zero rules under the pack's
// own name instead of failing loudly. No pack that ships is ever empty; this
// only fires if a registry entry and its embedded file drift apart.
func requirePackBytes(name string, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("pack %q: embedded pack is empty (broken build)", name)
	}
	return data, nil
}

// packDocuments turns pack names into documents internal/policy can load.
// Source is "pack:<name>", not a path: a pack has no filesystem location, so
// there is none to reach an error message, a JSON document or a report.
//
// An unknown name is refused rather than skipped. Silently ignoring it would
// run fewer rules than the operator asked for and say nothing. So is a name
// that resolves but whose embedded bytes are empty — see requirePackBytes.
func packDocuments(names []string) ([]policy.Document, error) {
	var out []policy.Document
	for _, name := range names {
		data, ok := policypack.Bytes(name)
		if !ok {
			return nil, unknownPackErr(name)
		}
		data, err := requirePackBytes(name, data)
		if err != nil {
			return nil, err
		}
		out = append(out, policy.Document{Source: "pack:" + name, Data: data})
	}
	return out, nil
}

// runPolicyPacks lists the curated packs, or prints one when printName names
// it. It contacts nothing: no cluster, no kubeconfig, no network, and no model
// — the packs are compiled into the binary.
//
// The rule count is computed by loading rather than stored beside the name, so
// it cannot disagree with the file it describes.
func runPolicyPacks(args []string, printName string, w io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: %s policy packs [--print name]", invokedAs)
	}
	if printName != "" {
		data, ok := policypack.Bytes(printName)
		if !ok {
			return unknownPackErr(printName)
		}
		data, err := requirePackBytes(printName, data)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	for _, p := range policypack.All() {
		data, err := requirePackBytes(p.Name, mustPackBytes(p.Name))
		if err != nil {
			return err
		}
		rules, err := policy.Load([]policy.Document{{Source: "pack:" + p.Name, Data: data}})
		if err != nil {
			// The packs ship with the binary and their tests load every one,
			// so this is unreachable outside a broken build.
			return fmt.Errorf("pack %q: %w", p.Name, err)
		}
		fmt.Fprintf(w, "  %-14s %s — %s\n", p.Name, plural(len(rules), "rule", "rules"), p.Summary)
	}
	fmt.Fprintf(w, "\nPrint one to fork it:\n  %s policy packs --print <name>\n", invokedAs)
	return nil
}

// mustPackBytes reads a pack that policypack.All just named, so the lookup
// itself cannot miss. Returning nil rather than panicking on the impossible
// case means the caller — requirePackBytes — can turn a broken build into a
// load error that names the pack, rather than a bare panic or (since
// policy.Load treats empty or nil YAML as a valid, empty document) a
// healthy-looking zero.
func mustPackBytes(name string) []byte {
	data, _ := policypack.Bytes(name)
	return data
}
```

Register the subcommand inside `newPolicyCommand`, after the `validate` subcommand:

```go
	packs := &cobra.Command{
		Use:           "packs",
		Short:         "List the curated policy packs compiled into this binary",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	var printName string
	packs.Flags().StringVar(&printName, "print", "", "print this pack's rules as YAML instead of listing")
	packs.RunE = func(cmd *cobra.Command, args []string) error {
		return runPolicyPacks(args, printName, os.Stdout)
	}
	cmd.AddCommand(packs)
```

Update the bare `policy` command's `RunE` error in the same file so it names both verbs:

```go
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("usage: %s policy validate <file>… | %s policy packs [--print name]", invokedAs, invokedAs)
		},
```

- [ ] **Step 4: Update the top-level usage error**

In `internal/cli/root.go`, inside `usageError()`, replace

```
| %[1]s policy validate <file>… |
```

with

```
| %[1]s policy validate <file>… | %[1]s policy packs [--print name] |
```

`TestUsageErrorNamesEveryCommand` walks the real command tree and requires every command to appear, so skipping this fails the suite.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/cli/ -run 'TestPolicyPacks|TestPackDocuments|TestRequirePackBytes|TestUnknownPackError|TestUsageError' -v`
Expected: PASS.

Then the package: `go test ./internal/cli/`
Expected: PASS.

- [ ] **Step 6: Exercise the binary by hand**

```bash
go build -o /tmp/kubeagent-packs . 
/tmp/kubeagent-packs policy packs;                     echo "exit=$?"   # listing, exit 0
/tmp/kubeagent-packs policy packs --print reliability | head -20; echo "exit=$?"  # YAML, exit 0
/tmp/kubeagent-packs policy packs --print nope;        echo "exit=$?"   # refusal, exit 1
/tmp/kubeagent-packs policy packs reliability;         echo "exit=$?"   # usage error, exit 1
/tmp/kubeagent-packs policy;                           echo "exit=$?"   # names both verbs, exit 1
rm -f /tmp/kubeagent-packs
```

Paste the observed output into the task report. No cluster is contacted by any of these.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/policy.go internal/cli/policy_test.go internal/cli/root.go
git commit -s -m "feat: kubeagent policy packs — list the curated packs, print one to fork

The listing counts a pack's rules by loading it, so the number cannot
disagree with the file it describes. --print emits the YAML an operator
can copy into their own --policy file.

Contacts nothing: no cluster, no kubeconfig, no network, no model call."
```

---

### Task 5: `--policy-pack` on `scan` and `gate`

Wires a pack into the single evaluation path both commands already share, so neither can run a pack the other would reject.

**Files:**
- Modify: `internal/cli/policy.go` (`loadPolicy`, `evaluatePolicy`)
- Modify: `internal/cli/scan.go` (field at ~line 58, flag at ~line 108, call at ~line 264)
- Modify: `internal/cli/gate.go` (field at ~line 60, flag at ~line 85, call at ~line 162)
- Modify: `internal/cli/root.go` (`usageError()` — both commands' flag lists)
- Modify: `internal/cli/surface_test.go` (`TestFleetRefusesTheFlagsItDoesNotOffer`)
- Modify: `internal/cli/scan_test.go`, `internal/cli/gate_test.go` (flag-parsing tests)

**Interfaces:**
- Consumes: `packDocuments(names []string) ([]policy.Document, error)` (Task 4).
- Produces: `evaluatePolicy(ctx context.Context, paths, packs []string, kubeconfig, contextName, namespace string) (*report.PolicyView, error)`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/cli/policy_test.go`:

```go
func TestLoadPolicyPutsPacksBeforeFiles(t *testing.T) {
	rules, err := loadPolicy(nil, []string{"reliability"})
	if err != nil {
		t.Fatalf("loadPolicy: %v", err)
	}
	if len(rules) != 14 {
		t.Fatalf("loaded %d rules from the pack alone, want 14", len(rules))
	}
	if !strings.HasPrefix(rules[0].ID, "reliability.") {
		t.Errorf("first rule is %q, want a pack rule first", rules[0].ID)
	}
}

func TestLoadPolicyRejectsAFileThatDuplicatesAPackRuleID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mine.yaml")
	body := []byte("- id: reliability.deploy-pdb\n" +
		"  match:\n    kind: Deployment\n" +
		"  assert:\n    path: spec.replicas\n    op: exists\n" +
		"  level: info\n  message: mine\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	_, err := loadPolicy([]string{path}, []string{"reliability"})
	if err == nil {
		t.Fatal("a file reusing a pack rule id was accepted")
	}
	// Load's text is `%s: rule id %q is already defined in %s` — the second
	// %s is the document that defined it FIRST. Packs load first, so the
	// error reads "<your file>: rule id … is already defined in
	// pack:reliability": the pack is the fixed thing.
	if !strings.Contains(err.Error(), "pack:reliability") {
		t.Errorf("the error does not say which pack the id came from: %v", err)
	}
}
```

Add to `internal/cli/scan_test.go` (follow the file's existing flag-parsing test style — find the test that asserts `--policy` reaches `o.policyPaths` and mirror it):

```go
func TestScanPolicyPackFlagReachesItsField(t *testing.T) {
	o, err := parseScanFlags([]string{"--policy-pack", "reliability", "--policy-pack", "other"})
	if err != nil {
		t.Fatalf("parseScanFlags: %v", err)
	}
	want := []string{"reliability", "other"}
	if !slices.Equal(o.policyPackNames, want) {
		t.Errorf("policyPackNames = %v, want %v — the flag is repeatable", o.policyPackNames, want)
	}
}
```

Add the same for gate in `internal/cli/gate_test.go`, using that file's own parse helper:

```go
func TestGatePolicyPackFlagReachesItsField(t *testing.T) {
	o, err := parseGateFlags([]string{"--policy-pack", "reliability"})
	if err != nil {
		t.Fatalf("parseGateFlags: %v", err)
	}
	if !slices.Equal(o.policyPackNames, []string{"reliability"}) {
		t.Errorf("policyPackNames = %v, want [reliability]", o.policyPackNames)
	}
}
```

If the parse helper in either file is named differently, use the real name — read the file rather than assuming. Add `slices`, `os`, `path/filepath` to imports as needed.

In `internal/cli/surface_test.go`, add `"--policy-pack"` to the list in `TestFleetRefusesTheFlagsItDoesNotOffer`:

```go
	for _, flag := range []string{"--logs", "--disk-usage", "--certs", "--capacity",
		"--explain", "--investigate", "--fix", "--rollback", "--policy", "--policy-pack"} {
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestLoadPolicy|PolicyPackFlag|TestFleetRefuses' -v`
Expected: FAIL — `loadPolicy` takes one argument, and `policyPackNames` is undefined.

- [ ] **Step 3: Thread packs through the evaluation path**

In `internal/cli/policy.go`, replace `loadPolicy` and `evaluatePolicy`:

```go
// loadPolicy reads and validates the packs named by --policy-pack and the
// files named by --policy, as one rule set.
//
// Packs load FIRST, and deliberately: policy.Load reports a duplicate id
// against the document that defined it earlier, so a collision reads as "your
// file reuses a pack's id" rather than the other way round. The pack is the
// fixed thing.
//
// Both go into the SAME document slice, which is what lets that collision be
// caught at all — Load detects duplicates across documents, not only within
// one.
func loadPolicy(paths, packs []string) ([]policy.Rule, error) {
	docs, err := packDocuments(packs)
	if err != nil {
		return nil, err
	}
	fileDocs, err := policyDocuments(paths)
	if err != nil {
		return nil, err
	}
	return policy.Load(append(docs, fileDocs...))
}
```

```go
// evaluatePolicy is the whole policy path, and the only one. scan and gate
// both call it, so neither can load a policy the other would reject, and
// neither can drop the unreadable set — which is the difference between "the
// rule passed" and "the rule never ran".
//
// Returns nil when neither --policy nor --policy-pack was given, so a run
// without them renders exactly the bytes it rendered before either existed.
//
// Read-only toward the cluster: ReadPlan names the kinds, collect.PolicyObjects
// lists them, and nothing here writes. There is no --fix path from a policy,
// and a curated pack is a policy like any other. Separately: no model call.
func evaluatePolicy(ctx context.Context, paths, packs []string, kubeconfig, contextName, namespace string) (*report.PolicyView, error) {
	if len(paths) == 0 && len(packs) == 0 {
		return nil, nil
	}
	rules, err := loadPolicy(paths, packs)
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

- [ ] **Step 4: Add the flag to both commands**

In `internal/cli/scan.go`, beside `policyPaths []string` in the options struct:

```go
	policyPackNames        []string
```

beside the `--policy` flag declaration:

```go
	f.StringArrayVar(&o.policyPackNames, "policy-pack", nil, "evaluate a curated rule pack compiled into kubeagent (repeatable; see `kubeagent policy packs`)")
```

and at the call site:

```go
	policyView, err := evaluatePolicy(context.Background(), o.policyPaths, o.policyPackNames, o.kubeconfig, o.contextName, o.namespace)
```

In `internal/cli/gate.go`, the same three edits — field beside `policyPaths`, flag beside the `--policy` declaration with the identical help string, and:

```go
	pv, err := evaluatePolicy(ctx, o.policyPaths, o.policyPackNames, o.kubeconfig, o.contextName, o.namespace)
```

- [ ] **Step 5: Update the top-level usage error**

In `internal/cli/root.go`, inside `usageError()`, add `[--policy-pack name (repeatable)]` immediately after `[--policy path (repeatable)]` in **both** the `scan` and the `gate` flag lists.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/cli/ -v -run 'TestLoadPolicy|PolicyPackFlag|TestFleetRefuses|TestUsageError'`
Expected: PASS.

Then the whole suite: `go test -p 2 ./...`
Expected: PASS, every package.

- [ ] **Step 7: Confirm the golden file and the schemas did not move**

```bash
git status --short internal/report/testdata/golden-scan.txt website/docs/schemas/ go.mod go.sum
```

Expected: no output. A pack is opt-in, so a plain scan renders the same bytes and no `schemaVersion` moves. If any of these changed, stop and report it — do not run any test with `-update`.

- [ ] **Step 8: Commit**

```bash
git add internal/cli/policy.go internal/cli/policy_test.go internal/cli/scan.go internal/cli/scan_test.go internal/cli/gate.go internal/cli/gate_test.go internal/cli/root.go internal/cli/surface_test.go
git commit -s -m "feat: scan --policy-pack and gate --policy-pack

Packs and files load as one rule set through the single path scan and
gate already share, so neither can run a pack the other would reject.
Packs load first, so a collision reads as the operator's file reusing a
pack id rather than the reverse.

Opt-in: without the flag nothing changes, so a plain scan renders the
bytes it always did and no schemaVersion moves."
```

---

### Task 6: Documentation

**Files:**
- Create: `website/docs/features/policy-packs.md`
- Modify: `website/mkdocs.yml` (nav), `website/docs/features/policy.md` (pointer), `website/docs/roadmap.md`, `website/docs/compatibility.md` (the read-only command list), `CHANGELOG.md`, `CLAUDE.md`

**Interfaces:**
- Consumes: everything above. Produces nothing.

- [ ] **Step 1: Write the feature page**

Create `website/docs/features/policy-packs.md`. Cover, in this order:

1. What it is and the three commands (`policy packs`, `--print`, `--policy-pack`), with the real output you captured in Task 4 Step 6 — do not invent output.
2. **Guarantees**, as two separate promises, never merged: it is read-only toward the cluster (a rule cannot write; there is no `--fix` path from a rule); and separately and additionally, it makes no LLM call — `--explain` is the model path and a pack is not a smaller version of it. `policy packs` itself contacts nothing at all: no cluster, no kubeconfig, no network.
3. **Opt-in, and what that buys**: without `--policy-pack` a scan renders exactly the bytes it did before, no scan-JSON key appears, and no `schemaVersion` moves.
4. **No rule is critical**, and why: `gate` fails on critical by default, so adding a pack to a pipeline cannot fail a build that passed yesterday. `--fail-on warning` is the explicit act that makes them block.
5. **The fourteen rules**, as the table from the spec: id, kind, assertion, level.
6. **Two semantics a rule author must know** — `[*]` requires every slot, and `exists` violates on absence while every other operator skips. Include the documented `deploy-image-tagged` limitation: a registry with a port and no tag (`registry.example.com:5000/app`) contains a colon and passes.
7. **RBAC**: a pack needs no grant beyond what `rbac print` already reports, because the kinds a rule may select are pinned to `rbacprofile`'s core rules.
8. **Forking a pack**: `kubeagent policy packs --print reliability > mine.yaml`, edit, then `--policy mine.yaml`. Note that ids must be changed or the file will collide with the pack if both are given.
9. **Not in this slice**: security and cost packs; operator-contributed packs at run time; a pack on by default; any evaluator change.

Every example uses placeholders and `example.com`-family hosts. The only permitted host in a link is `https://k8sproject.top`.

- [ ] **Step 2: Wire the page into the site**

In `website/mkdocs.yml`, add `Policy packs: features/policy-packs.md` to the nav immediately after the existing `Policy` entry. Match the surrounding indentation exactly.

In `website/docs/features/policy.md`, add a short pointer near the top — one or two sentences saying kubeagent ships a curated pack that needs no file, linking to `policy-packs.md`.

In `website/docs/compatibility.md`, add `policy packs` to the list of commands that issue only `get`, `list` and `watch` calls (line ~176). `policy packs` in fact issues none at all, which the feature page says; the compatibility list is about the ceiling.

- [ ] **Step 3: Update the roadmap**

In `website/docs/roadmap.md`, mark post-1.0 item 3's second half — the curated community detector library — as shipped in its slice-1 form, in the style the neighbouring shipped rows use.

- [ ] **Step 4: Update CHANGELOG.md**

Under `## [Unreleased]`, in an `### Added` block:

```markdown
- `kubeagent policy packs` lists the curated policy packs compiled into the
  binary, and `--print <name>` emits one as YAML to fork. It contacts nothing:
  no cluster, no kubeconfig, no network, and no model call.
- `scan --policy-pack <name>` and `gate --policy-pack <name>` evaluate a
  curated pack (repeatable, and combinable with `--policy`). The first pack,
  `reliability`, carries fourteen rules covering probes, resource requests and
  limits, replica counts, disruption-budget coverage and image tags. No rule in
  it is `critical`, so adding it to a pipeline cannot fail a build that passed
  yesterday; `--fail-on warning` makes them block.
- `internal/policypack` holds the packs and imports nothing from kubeagent and
  nothing outside the standard library, joining `internal/baseline`,
  `internal/glob` and `internal/knownissues`.
```

Do not touch any released section, and do not run `scripts/bump-version.sh` — releasing is a separate step.

- [ ] **Step 5: Update CLAUDE.md**

Two edits:

1. In the invariants section, add `internal/policypack` to the list of packages that import nothing from kubeagent and are stdlib-only, in the style used for `internal/glob` and `internal/knownissues`: name `internal/policypack/imports_test.go` as enforcing both halves, and state the two separate promises (no cluster call; no model call). Say explicitly that it cannot parse its own YAML, which is why it stores no rule count and the caller counts by loading.
2. Add a roadmap bullet recording that post-1.0 item 3's second half shipped slice 1 — the curated reliability pack — noting it is opt-in, adds no `schemaVersion` move, and ships no `critical` rule.

- [ ] **Step 6: Build the docs**

```bash
cd /home/ubuntu/git/kubeagent/website
/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml
cd /home/ubuntu/git/kubeagent
```

Expected: `Documentation built in …`, exit 0, and **no `WARNING` line naming your pages**. The red "Material for MkDocs 2.0" banner is cosmetic.

**The `cd` back to the repository root is not optional** — the shell's working directory persists between commands and has drifted into `website/` before, which makes a later `go build ./...` report `matched no packages`.

- [ ] **Step 7: Final verification on the whole branch**

```bash
export PATH=$PATH:/usr/local/go/bin
cd /home/ubuntu/git/kubeagent
go build ./...
go test -p 2 ./...
gofmt -l internal/
go vet ./...
git status --short go.mod go.sum internal/report/testdata/golden-scan.txt website/docs/schemas/
```

Expected: build clean, tests pass, `gofmt -l` prints nothing, `vet` clean, and the last command prints nothing.

- [ ] **Step 8: Commit**

```bash
git add website/docs/features/policy-packs.md website/docs/features/policy.md website/mkdocs.yml website/docs/roadmap.md website/docs/compatibility.md CHANGELOG.md CLAUDE.md
git commit -s -m "docs: curated policy packs

Two promises stated separately: a pack is read-only toward the cluster
because a rule has no write path, and separately it makes no model call.
Also records why no rule is critical — a gate that passed yesterday must
not fail because a pack was added."
```

---

## Self-Review

**Spec coverage.** Every section of the spec maps to a task: the package and its walls (Task 1), the `Load`-complement tests (Task 2), the per-rule behavioural table (Task 3), `policy packs` and `--print` (Task 4), `--policy-pack` on both commands (Task 5), the docs page and the contracts section (Task 6). The spec's "contracts this slice does not move" is checked mechanically in Task 5 Step 7 and Task 6 Step 7, not merely asserted in prose.

**Deviation from the spec, deliberate and recorded:** the spec's `Pack` struct listed a `RuleCount` field with a test pinning it against the loaded rules. This plan drops the field — a stdlib-only package cannot parse its own YAML, so the number would be hand-maintained, and the CLI counts by loading instead (Task 4). The spec was amended to match before this plan was written; both now say the same thing.

**Placeholder scan.** No "TBD", no "add appropriate error handling", no "similar to Task N". The full pack YAML, every test body, and every command line appear in full.

**Type consistency.** `Pack{Name, Summary, file}`, `All`, `Names`, `Lookup`, `Bytes` are defined in Task 1 and used under those exact names in Tasks 2–5. `loadPack` is defined in Task 2 and used in Task 3. `packDocuments` is defined in Task 4 and used in Task 5. `loadPolicy` and `evaluatePolicy` gain their `packs` parameter in Task 5, and both call sites are named with their line numbers.

**One judgement call for the executing controller:** Tasks 2 and 3 write tests against data Task 1 already wrote, so their first run may pass rather than fail. Each carries an explicit fault-injection step to prove the test can fail, with the exact edit to make and revert. A reviewer should check the task report records that step actually being run — a test that has never been seen red is a test that has not been tested.
