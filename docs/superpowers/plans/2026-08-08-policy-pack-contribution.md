# Policy Pack Contribution Path Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put in place the route by which a policy pack written by someone
outside kubeagent can land — a documented path, admission criteria enforced by
`go test` at the one layer nothing checks today, and an honest written
statement that nobody has walked it yet.

**Architecture:** **This slice adds no production Go code at all.**
`internal/policypack/policypack.go` gains no registry entry and no struct
field; `internal/policy` is untouched; `internal/cli` is untouched; no pack
ships. Everything added is one new test file
(`internal/policypack/registry_test.go`) plus documentation. `policy.Load`
already validates every rule and the four generic tests already validate every
*registered* pack — neither can see the registry itself, because `Load` is
handed bytes and never learns where they came from, and every generic test
iterates `All()`, so anything absent from `All()` is invisible to all of them.
The three new tests close exactly that layer.

**Tech Stack:** Go 1.26, standard library only (`io/fs`, `regexp`, `strings`,
`unicode`, `unicode/utf8`, `testing`). MkDocs for the website.

**Source of truth:** `docs/superpowers/specs/2026-08-08-policy-pack-contribution-design.md`
(committed on `main` as `c3c659b`). Its decisions and rationale are settled —
do not re-derive or reword them.

**Slice numbering:** the repository's own record (`CLAUDE.md`,
`website/docs/roadmap.md`) numbers the curated-pack slices 1 (`reliability`),
2 (`security`), 3 (`cost`). This is **slice 4**. Use that number in every
document; do not introduce letters.

## Global Constraints

Every task's requirements implicitly include this section.

- **Read-only toward the cluster.** A rule can only read a field; there is no
  `--fix` path from a rule. **Separately and additionally: a pack makes no LLM
  call.** These are two promises and must never be blurred into one. No
  comment, doc line, help string or commit message may suggest a pack is
  related to `--explain`, which is the model path.
- **kubeagent has no prices.** No billing data, no instance types, no node
  cost, no cloud API. Nothing written in this slice may imply otherwise.
- **No engine change.** `internal/policy` is not touched by any task. No new
  operator, no new relation, no new selectable kind.
- **No production Go change.** `internal/policypack/policypack.go` is modified
  only *temporarily*, inside a falsification step, and reverted with
  `git checkout` before the commit. Its committed content is byte-identical to
  `main` at the end of every task.
- **`internal/policypack` stays stdlib-only** and imports nothing from
  kubeagent. `internal/policypack/imports_test.go` enforces both halves and
  **must not be edited**. Both its guards skip files ending `_test.go`, so the
  new test file has no wall interaction — but it must still import only the
  standard library.
- **No new dependency.** `go.mod` and `go.sum` must not change. Verify with
  `git diff --stat main -- go.mod go.sum` at the end of every task: must be
  empty.
- **No schema moves.** `scan` stays at 1.2, `gate` at 1.1, and the other six do
  not move. **Never run any test with `-update`, in any task, for any reason.**
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.** Do not
  regenerate the demo GIF and do not touch `website/docs/quickstart.md`.
- **No `critical` rule.** `TestNoPackRuleIsCritical` is generic over
  `policypack.All()` and must not be edited or weakened.
- **Credentials.** No secrets, credentials, private IPs or internal hostnames
  anywhere — code, tests, fixtures, docs, help text. Documentation IPs are
  RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example
  domains are RFC 2606 (`example.com`, `example.org`, `example.net`). URLs are
  credentials: nothing beyond `scheme://host`, except the project's own
  `https://k8sproject.top/...` and `https://github.com/imantaba/kubeagent/...`
  links, both of which already appear throughout the website. A curated rule
  must never name a registry hostname.
- **TDD.** Write the failing test first, watch it fail, then implement.
  `go test` runs with `-p 2` locally, **never `-short`**.
- **Every commit is signed off** (`git commit -s`; DCO is enforced on `main`)
  and authored solely by the repository owner. **No `Co-Authored-By` trailer
  and no AI attribution of any kind, anywhere** — not in a commit message, not
  in code, not in documentation.
- **DANGER: never run `./chaos/run.sh` in any form.** It takes about forty
  minutes and injects real outages into a cluster. No cluster is needed for any
  part of this slice. Do not create, delete or touch any cluster.

**Environment:** Go lives outside the default `PATH` —
`export PATH=$PATH:/usr/local/go/bin`. The bash working directory persists
between calls, so `cd /home/ubuntu/git/kubeagent` when in doubt. The only
working mkdocs on this machine is
`/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/mkdocs`.

---

## File Structure

**Created**

- `internal/policypack/registry_test.go` — `package policypack` (internal, not
  `policypack_test`). Holds all three new tests plus two package-level
  declarations they share. One file, one responsibility: the invariants of the
  registry itself, as opposed to the invariants of a pack's rules
  (`packs_test.go`) or of the accessor functions (`policypack_test.go`).

**Modified**

- `website/docs/features/policy-packs.md` — a new `## Contributing a pack`
  section between `## Forking a pack` and `## Not in this slice`; the
  `## Not in this slice` list rewritten.
- `CONTRIBUTING.md` — a pointer to the canonical section, plus four bounded
  factual corrections in three locations.
- `website/docs/roadmap.md` — the bundled "community contribution at run time"
  clause split into its two distinct ideas.
- `CHANGELOG.md` — `### Added` and `### Fixed` entries under `## [Unreleased]`.
- `CLAUDE.md` — the post-1.0 curated-packs bullet records slice 4; the
  remaining-work sentence narrows.

**Not touched by any task:** `internal/policy/`,
`internal/policypack/policypack.go` (committed content),
`internal/policypack/packs/`, `internal/policypack/imports_test.go`,
`internal/policypack/packs_test.go`, `internal/policypack/policypack_test.go`,
`internal/policypack/rules_test.go`, `internal/policypack/security_rules_test.go`,
`internal/policypack/cost_rules_test.go`, `internal/cli/`, `go.mod`, `go.sum`,
`deploy/`, `website/docs/schemas/`,
`internal/report/testdata/golden-scan.txt`, `website/docs/quickstart.md`.

---

## Interfaces already verified in the tree

Use these verbatim. Do not re-derive them and do not go looking for
alternatives.

**`internal/policypack/policypack.go`** (read-only for this slice):

```go
//go:embed packs/*.yaml
var files embed.FS

// Pack is one curated rule set, compiled into the binary.
type Pack struct {
	Name    string // how an operator selects the pack: `--policy-pack <name>`
	Summary string // one line for the listing: lowercase, no trailing period
	file    string // the embedded path — UNEXPORTED
}

// packs is the registry, in name order.
var packs = []Pack{
	{Name: "cost", Summary: "…", file: "packs/cost.yaml"},
	{Name: "reliability", Summary: "…", file: "packs/reliability.yaml"},
	{Name: "security", Summary: "…", file: "packs/security.yaml"},
}

func All() []Pack
func Names() []string
func Lookup(name string) (Pack, bool)
func Bytes(name string) ([]byte, bool)
```

- A registry entry's `file` field holds the **full embedded path including the
  `packs/` prefix** — `"packs/cost.yaml"`, not `"cost.yaml"`. So the
  comparison against a directory entry is `"packs/" + entry.Name()`.
- Listing the embed is `fs.ReadDir(files, "packs")` from `io/fs` — standard
  library.

**Package placement.** The new file is `package policypack` — *internal*, not
`policypack_test` — because it needs both the unexported `files` embed handle
and the unexported `packs` slice with its unexported `file` field.
`internal/policypack/policypack_test.go` is already `package policypack`, so
the new file joins that package. Names already declared there, which must not
be redeclared: `TestAllIsSortedByName`, `TestEveryPackHasASummary`,
`TestLookupIsExact`, `TestNamesMatchesAll`, `TestBytesReturnsACopy`,
`TestBytesMissReturnsFalse`.

`internal/policypack/packs_test.go` and `rules_test.go` are
`package policypack_test` — a **different** package. Their helper
`loadPack(t, name)` is **not in scope** and must not be called from the new
file.

**What `internal/policy/load.go`'s `Load` already validates** — none of the
three new tests may re-assert any of it: rule id non-empty; id charset
`^[A-Za-z0-9._-]+$`; duplicate ids across the whole document set; selectable
kind; the cluster-scoped namespace-selector guard; exactly one of `path` and
`relation`; relation validity and applicability; level validity; non-empty
message; path parseability; the ConfigMap `data`/`binaryData` refusal;
operator arity. `sigs.k8s.io/yaml`'s `UnmarshalStrict` additionally refuses an
unknown key.

**The four generic per-pack tests** in `packs_test.go`, all looping over
`policypack.All()`: `TestEveryPackLoads`, `TestRuleIDsCarryTheirPackPrefix`,
`TestNoPackRuleIsCritical`, `TestPackCarriesNoHostOrAddress`. A registry entry
naming a file that does not exist is already caught by `TestEveryPackLoads`
(`Bytes` returns false → `t.Fatalf`), so the new orphan test only needs the
other direction.

**`internal/cli/policy.go`'s `runPolicyPacks`** renders the listing with the
format string `"  %-14s %s — %s\n"`. That `14` is what Task 2's length bound
mirrors. The test cannot reach the format string — `internal/policypack` may
import nothing from kubeagent — so the constant is duplicated with a comment
naming `internal/cli/policy.go` as the site it mirrors.

---

## The falsification requirement (Tasks 1–3)

**Read this before starting Task 1. It is the heart of the first three tasks
and the most likely place to go wrong.**

All three invariants **already hold** on the shipping tree. Each new test
therefore **PASSES on its first run**. That is the expected state, not a gap
to fix and not a reason to change the test.

But a test that has never been seen to fail is not evidence of anything — an
assertion with a typo'd field, an inverted condition or an empty loop passes
identically. So each of Tasks 1–3 proves its test by **temporary
falsification**:

1. Write the test.
2. Run it. Expect **PASS**. Report that you saw PASS.
3. Break the invariant, exactly as the task's steps specify.
4. Run it. Expect **FAIL**, with the message the task's steps quote. Report
   the actual failure output you saw.
5. Revert the break with `git checkout` or `rm` — never by hand-editing back.
6. Run it a third time. Expect **PASS**.
7. Run `git status --short` and confirm the **only** change is the new test
   file.
8. Commit.

**Report what you actually saw at each run.** Never manufacture a red run,
never claim a failure you did not observe, and never paste an invented
message. If a break does not produce the failure you expected, that is a real
finding about the test — say so and fix the test, do not paper over it.

**The reviewer checks the committed diff for leftovers**: a stray
`internal/policypack/packs/orphan.yaml`, a fourth entry in the `packs` slice,
or any modification to `policypack.go`. Finding one is a Critical.

**One deliberate deviation from the spec's Testing section.** The spec
describes Task 2's three breaks as successive mutations of one temporary
fourth registry entry. The steps below instead `git checkout` back to a clean
`policypack.go` between each break, and apply breaks two and three to the
*first existing* entry. Same three failures, and each break is isolated — but
it also means no temporary fourth entry survives past the step that created
it, which is exactly the leftover the reviewer is told to look for. Follow the
steps as written.

---

## Task 1: The orphan-embedded-file test

**Files:**
- Create: `internal/policypack/registry_test.go`
- Read for context: `internal/policypack/policypack.go`

**Interfaces:**
- Consumes: the unexported `files embed.FS` and `packs []Pack` from
  `internal/policypack/policypack.go`; `Pack.file`, the unexported field.
- Produces: the file `internal/policypack/registry_test.go`, which Tasks 2 and
  3 **append to**. Task 1 owns its import block; later tasks add imports to it
  but must not remove or reorder Task 1's.

- [ ] **Step 1: Cut the branch**

```bash
cd /home/ubuntu/git/kubeagent
export PATH=$PATH:/usr/local/go/bin
git checkout main
git status --short          # must be empty
git checkout -b policy-pack-contribution
git log --oneline -2        # the plan commit, then the spec commit c3c659b
```

The branch is cut off `main` at its tip, which is the plan commit — the spec
and this plan are both already on `main`, as this project's convention has it.

- [ ] **Step 2: Write the test**

Create `internal/policypack/registry_test.go` with exactly this content:

```go
package policypack

import (
	"io/fs"
	"testing"
)

// TestEveryEmbeddedPackIsRegistered closes the one gap no per-pack test can
// see.
//
// `//go:embed packs/*.yaml` compiles every file under packs/ into the binary,
// but every other test over the packs — here and in package policypack_test —
// iterates All(). A file with no registry entry is therefore invisible to all
// of them while still travelling in every release artifact: its rules are
// never loaded, never validated and never run, and nothing reports it.
//
// That is the mistake a contributed pack is most likely to make — adding the
// YAML and forgetting the five-line registry entry — which is why it is
// checked here rather than left to review.
//
// Only this direction needs checking. The reverse, a registry entry naming a
// file that is not embedded, already fails TestEveryPackLoads in
// packs_test.go, where Bytes returns false.
func TestEveryEmbeddedPackIsRegistered(t *testing.T) {
	entries, err := fs.ReadDir(files, "packs")
	if err != nil {
		t.Fatalf("reading the embedded packs directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no embedded packs — every assertion below would pass vacuously")
	}

	// Built from the registry, keyed by embedded path, so the same file named
	// by two entries is caught too: one pack's bytes would then ship under two
	// names, and the second name's rule ids would carry the first pack's
	// prefix.
	registered := make(map[string]string, len(packs))
	for _, p := range packs {
		if other, dup := registered[p.file]; dup {
			t.Errorf("packs %q and %q both name %s — one file cannot be two packs", other, p.Name, p.file)
		}
		registered[p.file] = p.Name
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := "packs/" + e.Name()
		if _, ok := registered[path]; !ok {
			t.Errorf("%s is embedded in the binary but no registry entry names it — it ships unlisted, unloaded and untested", path)
		}
	}
}
```

- [ ] **Step 3: Run it — expect PASS**

```bash
cd /home/ubuntu/git/kubeagent
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policypack -run TestEveryEmbeddedPackIsRegistered -v
```

Expected: **PASS**. All three shipped packs are registered, so the invariant
already holds. This is the expected state — do not change the test to make it
fail. Report that you saw PASS.

- [ ] **Step 4: Break the invariant**

Create `internal/policypack/packs/orphan.yaml` with exactly this content:

```yaml
# Temporary falsification fixture for TestEveryEmbeddedPackIsRegistered
# This file is deleted in the next step and must never be committed
- id: orphan.example
  match:
    kind: Deployment
  assert:
    path: spec.replicas
    op: exists
  level: info
  message: this file is a temporary falsification fixture
```

- [ ] **Step 5: Run it — expect FAIL**

```bash
cd /home/ubuntu/git/kubeagent
go test ./internal/policypack -run TestEveryEmbeddedPackIsRegistered -v
```

Expected: **FAIL**, with a message of the form:

```
    registry_test.go:NN: packs/orphan.yaml is embedded in the binary but no registry entry names it — it ships unlisted, unloaded and untested
```

Report the actual output you saw. If it passes here, the test is broken —
investigate and fix the test, do not proceed.

- [ ] **Step 6: Revert the break**

```bash
cd /home/ubuntu/git/kubeagent
rm internal/policypack/packs/orphan.yaml
git status --short
```

Expected: `git status --short` shows exactly one line,
`?? internal/policypack/registry_test.go`. Nothing else.

- [ ] **Step 7: Run the whole package — expect PASS**

```bash
cd /home/ubuntu/git/kubeagent
go test -p 2 -count=1 ./internal/policypack/... -v
```

Expected: **PASS**, every test in the package.

- [ ] **Step 8: Verify nothing else moved**

```bash
cd /home/ubuntu/git/kubeagent
gofmt -l internal/policypack/
go vet ./internal/policypack/...
git diff --stat main -- go.mod go.sum internal/policy/ internal/policypack/policypack.go internal/policypack/packs/
```

Expected: `gofmt` prints nothing, `go vet` is silent, and the `git diff --stat`
is **empty**.

- [ ] **Step 9: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/policypack/registry_test.go
git commit -s -m "test(policypack): fail an embedded pack with no registry entry

go:embed packs/*.yaml compiles every file under packs/ into the binary, but
every other test over the packs iterates All(). A file with no registry entry
is therefore invisible to all of them while still travelling in every release
artifact: its rules are never loaded, never validated and never run, and
nothing reports it.

That is the mistake a contributed pack is most likely to make — adding the
YAML and forgetting the registry entry — so it is checked rather than left to
review. Two entries naming the same file fail too: one pack's bytes would
otherwise ship under two names.

Proven by falsification: a temporary packs/orphan.yaml makes the test name the
file and fail, and the test passes again once it is removed."
```

---

## Task 2: The pack-name test

**Files:**
- Modify: `internal/policypack/registry_test.go` — **append**, do not rewrite

**Interfaces:**
- Consumes: the unexported `packs []Pack` slice; the file created by Task 1.
- Produces: package-level `nameWidth` (an `int` constant, value 14),
  `packNamePattern` (a `*regexp.Regexp`) and `validPackName(name string) bool`.
  Task 3 does not use them, but must not redeclare them.

**Do not** rewrite, rename, reword or reorder `TestEveryEmbeddedPackIsRegistered`
or its comment. Append below it, and add `regexp` to the existing import block
in sorted position.

- [ ] **Step 1: Append the declarations and the two tests**

The import block becomes:

```go
import (
	"io/fs"
	"regexp"
	"testing"
)
```

Append below `TestEveryEmbeddedPackIsRegistered`:

```go
// nameWidth is the widest pack name the listing can align. It mirrors the
// column width in internal/cli/policy.go, whose runPolicyPacks renders each
// row with "  %-14s %s — %s\n": a longer name pushes its own row's summary
// out of line with every other row.
//
// The constant is duplicated rather than shared because internal/policypack
// may import nothing from kubeagent. A maintainer who changes one is told
// here where the other lives.
const nameWidth = 14

// packNamePattern is the shape a pack name may take. The name is the value an
// operator types after --policy-pack and the join between the listing and the
// flag, so it stays to lowercase letters, digits and interior hyphens: no
// empty name, no uppercase, no whitespace, no dot, no slash, and no leading,
// trailing or doubled hyphen.
var packNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// validPackName reports whether a name is usable as a --policy-pack value and
// fits the listing column. Both halves matter and neither implies the other:
// the pattern is about what an operator can type, the width about what the
// listing can align.
func validPackName(name string) bool {
	return packNamePattern.MatchString(name) && len(name) <= nameWidth
}

// TestPackNamesAreUniqueAndCLISafe checks the registry itself, which no
// per-pack test can reach.
//
// Uniqueness is not cosmetic. Lookup returns the first match and stops, while
// All lists every entry — so a duplicate name silently shadows a pack that is
// still advertised in the listing. It is also what makes
// TestRuleIDsCarryTheirPackPrefix sufficient to rule out a rule-id collision
// between two packs: with unique names, prefixed ids cannot collide across
// packs; without them, they can.
func TestPackNamesAreUniqueAndCLISafe(t *testing.T) {
	if len(packs) == 0 {
		t.Fatal("no packs — every assertion below would pass vacuously")
	}
	seen := make(map[string]bool, len(packs))
	for _, p := range packs {
		if seen[p.Name] {
			t.Errorf("two registry entries are named %q — Lookup returns the first and stops, so the second is unreachable while still listed", p.Name)
		}
		seen[p.Name] = true
		if !validPackName(p.Name) {
			t.Errorf("pack name %q is not usable as a --policy-pack value: want lowercase letters, digits and interior hyphens, at most %d bytes", p.Name, nameWidth)
		}
	}
}

// TestPackNameRulesRefuseHostileNames proves validPackName actually refuses
// what it claims to. Without it the pattern could be `^.*$` and the width
// bound could be missing entirely, and TestPackNamesAreUniqueAndCLISafe would
// still pass on the three shipped names.
func TestPackNameRulesRefuseHostileNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		why  string
	}{
		{"", "no name at all"},
		{"Security", "a leading uppercase letter"},
		{"COST", "all uppercase"},
		{"two words", "whitespace, which the shell would split"},
		{"cost.pack", "a dot"},
		{"team/cost", "a slash"},
		{"-cost", "a leading hyphen, which reads as a flag"},
		{"cost-", "a trailing hyphen"},
		{"cost--pack", "an empty segment"},
		{"cost_pack", "an underscore"},
		{"compliance-cis1", "fifteen bytes, one over the listing column"},
	} {
		if validPackName(tc.name) {
			t.Errorf("validPackName accepts %q (%s), which is not usable as a pack name", tc.name, tc.why)
		}
	}
	for _, name := range []string{"cost", "reliability", "security", "supply-chain", "cis1"} {
		if !validPackName(name) {
			t.Errorf("validPackName refuses %q, which is a well-formed pack name", name)
		}
	}
}
```

- [ ] **Step 2: Run both tests — expect PASS**

```bash
cd /home/ubuntu/git/kubeagent
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policypack -run 'TestPackName' -v
```

Expected: **PASS** for both. The three shipped names are unique and
well-formed, so the invariant already holds. Report that you saw PASS.

Note: `TestPackNameRulesRefuseHostileNames` is meaningful on its first run —
it exercises names the registry does not contain — so it is not subject to the
"already holds" caveat. `TestPackNamesAreUniqueAndCLISafe` is, and Steps 3–8
falsify it.

- [ ] **Step 3: Break it — a duplicate name**

Temporarily append a fourth entry to the `packs` slice in
`internal/policypack/policypack.go`, immediately before the closing `}` of the
slice literal:

```go
	{
		Name:    "cost",
		Summary: "a temporary falsification entry",
		file:    "packs/cost.yaml",
	},
```

- [ ] **Step 4: Run it — expect FAIL**

```bash
cd /home/ubuntu/git/kubeagent
go test ./internal/policypack -run TestPackNamesAreUniqueAndCLISafe -v
```

Expected: **FAIL**, with a message of the form:

```
    registry_test.go:NN: two registry entries are named "cost" — Lookup returns the first and stops, so the second is unreachable while still listed
```

Report the actual output. (`TestEveryEmbeddedPackIsRegistered` from Task 1
also fails here, on the two-entries-name-one-file branch — that is correct and
expected, and confirms Task 1's second check works too. Note it in your
report.)

- [ ] **Step 5: Break it differently — an uppercase name**

Revert first, then re-break:

```bash
cd /home/ubuntu/git/kubeagent
git checkout -- internal/policypack/policypack.go
```

Now change the **first** registry entry's `Name` from `"cost"` to `"Cost"`
(one character), and run:

```bash
go test ./internal/policypack -run TestPackNamesAreUniqueAndCLISafe -v
```

Expected: **FAIL**, with:

```
    registry_test.go:NN: pack name "Cost" is not usable as a --policy-pack value: want lowercase letters, digits and interior hyphens, at most 14 bytes
```

Report the actual output.

- [ ] **Step 6: Break it a third way — a fifteen-byte name**

Revert, then re-break:

```bash
cd /home/ubuntu/git/kubeagent
git checkout -- internal/policypack/policypack.go
```

Now change the first registry entry's `Name` from `"cost"` to
`"compliance-cis1"` (fifteen bytes, well-formed in shape), and run:

```bash
go test ./internal/policypack -run TestPackNamesAreUniqueAndCLISafe -v
```

Expected: **FAIL**, with the same "not usable as a --policy-pack value"
message naming `"compliance-cis1"` — proving the width bound fires
independently of the pattern. Report the actual output.

- [ ] **Step 7: Revert and confirm clean**

```bash
cd /home/ubuntu/git/kubeagent
git checkout -- internal/policypack/policypack.go
git status --short
git diff --stat main -- internal/policypack/policypack.go
```

Expected: `git status --short` shows only
`M  internal/policypack/registry_test.go` (or ` M`), and the `git diff --stat`
against `main` for `policypack.go` is **empty**.

- [ ] **Step 8: Run the whole package — expect PASS**

```bash
cd /home/ubuntu/git/kubeagent
go test -p 2 -count=1 ./internal/policypack/... -v
gofmt -l internal/policypack/
go vet ./internal/policypack/...
git diff --stat main -- go.mod go.sum internal/policy/ internal/policypack/policypack.go internal/policypack/packs/
```

Expected: all tests **PASS**, `gofmt` prints nothing, `go vet` is silent, the
`git diff --stat` is **empty**.

- [ ] **Step 9: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/policypack/registry_test.go
git commit -s -m "test(policypack): fail a duplicate or unusable pack name

A pack name is the value an operator types after --policy-pack and the join
between the listing and the flag, and nothing checked it.

Uniqueness is not cosmetic: Lookup returns the first match and stops while All
lists every entry, so a duplicate name silently shadows a pack that is still
advertised. It is also what makes TestRuleIDsCarryTheirPackPrefix sufficient
to rule out a rule-id collision between two packs — with unique names,
prefixed ids cannot collide; without them, they can.

The shape rule keeps a name to lowercase letters, digits and interior hyphens,
and the width bound mirrors the %-14s column in internal/cli/policy.go, which
a longer name pushes out of alignment. The constant is duplicated because this
package may import nothing from kubeagent; a comment names the other site.

Proven by falsification: a duplicate entry, an uppercase name and a
fifteen-byte name each fail, and the tests pass again once reverted."
```

---

## Task 3: The summary-shape test

**Files:**
- Modify: `internal/policypack/registry_test.go` — **append**, do not rewrite

**Interfaces:**
- Consumes: the unexported `packs []Pack` slice; the file created by Task 1 and
  extended by Task 2.
- Produces: nothing later tasks consume.

**Do not** rewrite, rename, reword or reorder anything Tasks 1 and 2 wrote.
Append below, and add `strings`, `unicode` and `unicode/utf8` to the existing
import block in sorted position.

- [ ] **Step 1: Append the test**

The import block becomes:

```go
import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)
```

Append below `TestPackNameRulesRefuseHostileNames`:

```go
// TestPackSummaryShape enforces the rule Pack's own doc comment already states
// — "one line for the listing: lowercase, no trailing period" — and which
// nothing checked.
//
// TestEveryPackHasASummary in policypack_test.go covers non-emptiness and
// stays as it is; this covers the shape. The multi-line case is the one that
// breaks the listing outright, since runPolicyPacks prints one row per pack;
// the other two are the house style a contributor is asked to match, and the
// fix for either is to reword.
func TestPackSummaryShape(t *testing.T) {
	if len(packs) == 0 {
		t.Fatal("no packs — every assertion below would pass vacuously")
	}
	for _, p := range packs {
		if strings.ContainsAny(p.Summary, "\n\r") {
			t.Errorf("pack %q has a multi-line summary — the listing prints one row per pack: %q", p.Name, p.Summary)
		}
		if strings.TrimSpace(p.Summary) != p.Summary {
			t.Errorf("pack %q summary has leading or trailing whitespace, which the listing would render as a ragged column: %q", p.Name, p.Summary)
		}
		if strings.HasSuffix(p.Summary, ".") {
			t.Errorf("pack %q summary ends in a period — it is a phrase in a row, not a sentence: %q", p.Name, p.Summary)
		}
		// An empty summary decodes to utf8.RuneError, which is not upper, so
		// this reports nothing for it — non-emptiness is
		// TestEveryPackHasASummary's job and stays there.
		if r, _ := utf8.DecodeRuneInString(p.Summary); unicode.IsUpper(r) {
			t.Errorf("pack %q summary begins with an uppercase letter — it is a phrase in a row, not a sentence: %q", p.Name, p.Summary)
		}
	}
}
```

- [ ] **Step 2: Run it — expect PASS**

```bash
cd /home/ubuntu/git/kubeagent
export PATH=$PATH:/usr/local/go/bin
go test ./internal/policypack -run TestPackSummaryShape -v
```

Expected: **PASS**. All three shipped summaries are one lowercase line with no
trailing period, so the invariant already holds. Report that you saw PASS.

- [ ] **Step 3: Break it — a trailing period**

In `internal/policypack/policypack.go`, append a single `.` to the first
registry entry's `Summary` value, then run:

```bash
cd /home/ubuntu/git/kubeagent
go test ./internal/policypack -run TestPackSummaryShape -v
```

Expected: **FAIL**, with:

```
    registry_test.go:NN: pack "cost" summary ends in a period — it is a phrase in a row, not a sentence: "…"
```

Report the actual output.

- [ ] **Step 4: Break it — a leading capital**

Revert, then re-break:

```bash
cd /home/ubuntu/git/kubeagent
git checkout -- internal/policypack/policypack.go
```

Change the first registry entry's `Summary` so it begins `Resource` instead of
`resource` (one character), and run:

```bash
go test ./internal/policypack -run TestPackSummaryShape -v
```

Expected: **FAIL**, with the "begins with an uppercase letter" message. Report
the actual output.

- [ ] **Step 5: Break it — an embedded newline**

Revert, then re-break:

```bash
cd /home/ubuntu/git/kubeagent
git checkout -- internal/policypack/policypack.go
```

Insert a literal `\n` escape into the middle of the first registry entry's
`Summary` string, and run:

```bash
go test ./internal/policypack -run TestPackSummaryShape -v
```

Expected: **FAIL**, with the "multi-line summary" message. Report the actual
output.

- [ ] **Step 6: Revert and confirm clean**

```bash
cd /home/ubuntu/git/kubeagent
git checkout -- internal/policypack/policypack.go
git status --short
git diff --stat main -- internal/policypack/policypack.go
```

Expected: only `internal/policypack/registry_test.go` is modified, and the
`git diff --stat` for `policypack.go` is **empty**.

- [ ] **Step 7: Run the full suite — expect PASS**

```bash
cd /home/ubuntu/git/kubeagent
go build ./...
go vet ./...
gofmt -l internal/
go test -p 2 -count=1 ./... 2>&1 | tail -40
git diff --stat main -- go.mod go.sum internal/policy/ internal/policypack/policypack.go internal/policypack/packs/ internal/report/testdata/golden-scan.txt
```

Expected: build clean, vet silent, `gofmt` prints nothing, **every package
passes**, and the `git diff --stat` is **empty**.

- [ ] **Step 8: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/policypack/registry_test.go
git commit -s -m "test(policypack): fail a summary the listing cannot align

Pack's doc comment already states the rule — one line for the listing,
lowercase, no trailing period — and nothing checked it. A multi-line summary
breaks the listing outright, since runPolicyPacks prints one row per pack; the
other two are the house style a contributor is asked to match.

TestEveryPackHasASummary keeps covering non-emptiness and is unchanged; an
empty summary decodes to utf8.RuneError, which is not upper, so this test
reports nothing for it.

Proven by falsification: a trailing period, a leading capital and an embedded
newline each fail, and the test passes again once reverted."
```

---

## Task 4: The `Contributing a pack` documentation section

**Files:**
- Modify: `website/docs/features/policy-packs.md` — insert a new section
  between `## Forking a pack` (line 363) and `## Not in this slice` (line 387);
  rewrite the `## Not in this slice` list.

**Interfaces:**
- Consumes: the seven checks Tasks 1–3 and the existing `packs_test.go`
  enforce. The three new test names are `TestEveryEmbeddedPackIsRegistered`,
  `TestPackNamesAreUniqueAndCLISafe` and `TestPackSummaryShape`; the four
  existing ones are `TestEveryPackLoads`, `TestRuleIDsCarryTheirPackPrefix`,
  `TestNoPackRuleIsCritical` and `TestPackCarriesNoHostOrAddress`.
- Produces: the anchor `#contributing-a-pack`, which Task 5's pointer links to.

**Binding requirements for this task:**

1. **The section must not blur the two promises.** Read-only toward the
   cluster and making no LLM call are separate claims. If the section mentions
   either, it must not merge them into "makes no external calls".
2. **It must not imply a pack knows prices.** kubeagent has no billing data, no
   instance types, no node cost, no cloud API.
3. **It must not suggest a pack is related to `--explain`,** which is the model
   path.
4. **It must never name a registry hostname**, or any URL beyond
   `scheme://host`, other than the project's own `k8sproject.top` and
   `github.com/imantaba/kubeagent` links.
5. **The `## Not in this slice` bullet reading "A pack contributed by someone
   other than kubeagent itself" is NARROWED, NOT DELETED.** Retraction is not
   deletion. What was absent was the *path*; the path now exists, so the bullet
   leaves that list — but the fact that no outside pack has used it survives,
   moved into the new section as its final point. A reviewer will check that
   the sentence still exists somewhere on the page.
6. **The "Operator-contributed packs at run time" bullet stays EXACTLY as it
   is**, byte for byte. That capability is still genuinely absent and this
   slice does not add it.

- [ ] **Step 1: Insert the new section**

After the line `Change the ids in the fork — or drop `--policy-pack
reliability` once you are` … `running the fork instead of the original —
before combining the two.` and **before** `## Not in this slice`, insert:

````markdown
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
````

- [ ] **Step 2: Rewrite `## Not in this slice`**

Replace the whole `## Not in this slice` block with:

```markdown
## Not in this slice

Deliberately absent:

- **Operator-contributed packs at run time.** The registry is curated and
  compiled into the binary, the same as `known-issues`; there is no way to add
  a pack without a kubeagent release.
- **A pack on by default.** `--policy-pack` is opt-in on every command that
  accepts it; nothing runs unless it is named.
- **Any change to the evaluator.** `internal/policy` is unchanged — a pack is
  YAML data read by the same `Load`/`Evaluate` a `--policy` file already used.
```

The removed bullet's content is not lost: it is the final paragraph of
**Two limits worth knowing before you start**, above. What changed is that the
*path* is no longer absent — only its use is.

- [ ] **Step 3: Check for a stale "two packs" or "three packs" claim**

```bash
cd /home/ubuntu/git/kubeagent
grep -n 'two packs\|three packs\|both packs\|the two shipped' website/docs/features/policy-packs.md
```

If any hit describes the *number of packs*, leave it alone — this slice adds
no pack, so every such count is still correct. Report what you found either
way.

- [ ] **Step 4: Build the site**

```bash
cd /home/ubuntu/git/kubeagent/website
/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/mkdocs build --strict -f mkdocs.yml
cd /home/ubuntu/git/kubeagent
```

Expected: exit 0 and no `WARNING` line naming `policy-packs.md`. The red
"Material for MkDocs 2.0" banner is cosmetic — judge by the exit code and the
absence of page warnings. **`cd` back to the repository root**, as shown — the
working directory persists between commands.

- [ ] **Step 5: Verify nothing else moved**

```bash
cd /home/ubuntu/git/kubeagent
git status --short
git diff --stat main -- go.mod go.sum internal/ deploy/ website/docs/schemas/ website/docs/quickstart.md
```

Expected: `git status --short` shows only
`website/docs/features/policy-packs.md`, and the `git diff --stat` is
**empty**.

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add website/docs/features/policy-packs.md
git commit -s -m "docs: the route for a pack written outside kubeagent

Names the route — issue first, YAML, registry entry, go test, pull request —
the seven checks that gate it, and the judgment criteria no test can reach:
whether each rule is true of the kind it selects, whether the subject belongs,
and whether the pack is honest about its own gaps.

Acceptance stays curatorial: the criteria are necessary, not sufficient, and a
pack that ships is kubeagent's curation whoever wrote it. Attribution lives in
the pack's header comment, which --print emits; there is no author field,
because a two-tier listing would tell an operator to trust some shipped rules
less than others.

The 'Not in this slice' bullet about an outside-contributed pack is narrowed
rather than dropped: what was absent was the path, and the path now exists.
That no pack has yet come through it survives as the section's closing
paragraph. Operator-contributed packs at run time stay listed as absent —
the registry is still compiled into the binary."
```

---

## Task 5: CONTRIBUTING.md — the pointer and four bounded corrections

**Files:**
- Modify: `CONTRIBUTING.md` — four locations, named below.

**Interfaces:**
- Consumes: the `#contributing-a-pack` anchor Task 4 created.
- Produces: nothing later tasks consume.

**This task is BOUNDED.** It corrects the statements named below and adds one
pointer. It is **not** a re-audit of `CONTRIBUTING.md`: do not rewrite prose
that is merely dated in tone, do not restructure sections, and do not correct
anything not listed here. If you find a fifth false statement, **report it in
your task report** and leave it alone — widening the scope is the controller's
call, not yours.

- [ ] **Step 1: Correction A — the layout claim (lines 57-58)**

This is false: the Cobra CLI shipped in v0.73.0. Replace:

```markdown
- `main.go` — flag parsing and subcommand dispatch (standard-library `flag`
  only; no Cobra).
```

with:

```markdown
- `main.go` — only the `version` symbol the release workflow stamps with
  `-ldflags`. The CLI is a Cobra command tree in `internal/cli`, one file per
  command; flags are declared per command and never as persistent flags.
```

- [ ] **Step 2: Correction B and C — invariant 1 (line 23)**

Two false things in one sentence: the command list omits `rbac` and `fleet`,
and `--fix` is not the single write path — `scan --rollback` undoes an applied
fix from the audit log, and `rbac check` issues the one POST outside
remediation. Replace:

```markdown
1. **Read-only by default.** `scan`, `watch`, `mcp`, `gate`, and `tui` issue
   only `get`/`list`/`watch` against the cluster. The single exception is the
   opt-in `--fix` flag, whose writes come from a fixed allowlist, refuse
   protected namespaces, require a per-action confirmation, and re-verify
   afterwards.
```

with:

```markdown
1. **Read-only by default.** `scan`, `watch`, `mcp`, `gate`, `tui`, `rbac` and
   `fleet` issue only `get`/`list`/`watch` against the cluster. Two opt-in
   flags write, and only those two: `scan --fix`, whose writes come from a
   fixed allowlist, refuse protected namespaces, require a per-action
   confirmation and re-verify afterwards; and `scan --rollback`, which undoes
   the most recent applied fix recorded in `--audit-log`. The two are mutually
   exclusive. One documented carve-out is not a write at all: `rbac check`
   creates `SelfSubjectAccessReview` objects — a virtual resource the API
   server evaluates and never persists, the same API `kubectl auth can-i` uses
   — which makes it the only POST outside remediation and changes no cluster
   state.
```

- [ ] **Step 3: Correction D — invariant 2's import wall**

The list is far longer than two packages now. Replace:

```markdown
2. **No LLM call decides a write.** Remediation is chosen by deterministic
   code. `internal/mcp` and `internal/gate` must never import
   `internal/remediate` or `internal/explain`.
```

with:

```markdown
2. **No LLM call decides a write.** Remediation is chosen by deterministic
   code, and a read-only surface may never import `internal/remediate` or
   `internal/explain`. That list has grown well past the first two:
   `internal/mcp`, `internal/gate` (with `internal/findings`,
   `internal/sarif`, `internal/rolloutwait`), `internal/tui`,
   `internal/rbacprofile`, `internal/policy`, `internal/parallel`,
   `internal/fleet` and `internal/fleetfile` all carry the wall. Several
   packages go further and import nothing from kubeagent at all —
   `internal/jsonschema`, `internal/dashboard`, `internal/baseline`,
   `internal/glob`, `internal/knownissues` and `internal/policypack` — which
   makes the reach impossible by construction rather than by rule. Each wall
   is enforced by a test in its own package; adding a package means deciding
   which one it inherits.
```

- [ ] **Step 4: Add the pointer**

In `## Before you start`, after the `**Read [docs/design.md](docs/design.md).**`
bullet and before the `**Security problems do not go in issues.**` bullet,
insert:

```markdown
- **Contributing a policy pack?** There is a documented route with its own
  admission criteria, enforced by `go test ./internal/policypack` — see
  [Contributing a pack](https://k8sproject.top/features/policy-packs/#contributing-a-pack).
```

- [ ] **Step 5: Verify nothing else moved**

```bash
cd /home/ubuntu/git/kubeagent
git status --short
git diff --stat main -- go.mod go.sum internal/ website/ deploy/
```

Expected: `git status --short` shows only `CONTRIBUTING.md`, and the
`git diff --stat` is **empty**.

- [ ] **Step 6: Re-read your own diff**

```bash
cd /home/ubuntu/git/kubeagent
git diff CONTRIBUTING.md
```

Confirm four changed regions and nothing else. If the diff shows a fifth,
revert it — the task is bounded.

- [ ] **Step 7: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add CONTRIBUTING.md
git commit -s -m "docs: point a pack contributor at the route, and fix what CONTRIBUTING claimed

Adds the pointer to the documented pack-contribution route, and corrects four
statements a contributor would have acted on:

- main.go has not parsed flags since v0.73.0; the CLI is a Cobra tree in
  internal/cli, one file per command.
- The read-only list omitted rbac and fleet, and --fix is not the single write
  path: scan --rollback undoes an applied fix from the audit log, and rbac
  check issues the one POST outside remediation — a SelfSubjectAccessReview,
  which the API server evaluates and never persists.
- The import wall named two packages; it now covers a dozen, six of which
  import nothing from kubeagent at all.

Bounded to what was demonstrably false — not a re-audit of the file."
```

---

## Task 6: roadmap, changelog and project notes

**Files:**
- Modify: `website/docs/roadmap.md` — the post-1.0 row's closing clause
- Modify: `CHANGELOG.md` — under `## [Unreleased]`
- Modify: `CLAUDE.md` — the post-1.0 curated-packs bullet and the
  remaining-work sentence

**Interfaces:**
- Consumes: everything Tasks 1–5 landed.
- Produces: nothing.

**Binding requirements for this task:**

1. **No `(vX.Y.Z)` version parenthetical anywhere.** That is added
   **exclusively** by the later `release: vX.Y.Z` commit, never by a docs
   commit. Write "Slice 4 has since shipped" with no parenthetical.
2. **The roadmap's bundled clause is split.** "community contribution at run
   time" runs two distinct ideas together — *who authors a pack* (answered by
   this slice) and *whether a pack can be loaded without a kubeagent release*
   (deliberately still absent). Split it so each can be true or false on its
   own. This clears a Minor deferred from the cost-pack slice.
3. **Do not blur the two promises**, do not imply a pack knows prices, and do
   not suggest a pack is related to `--explain`.

- [ ] **Step 1: `website/docs/roadmap.md` — two edits on line 559**

The **post-1.0** table row is one very long line. Make two exact string
replacements in it and change nothing else on the line.

**1a — the header phrase.** Replace:

```
**curated policy packs slices 1, 2 and 3 shipped**
```

with:

```
**curated policy packs complete**
```

**1b — the closing clause.** Replace:

```
no `schemaVersion` move, and no RBAC grant a plain `scan` did not already have), the second half of this item's first form — plus other baseline dimensions and community contribution at run time, still ahead
```

with:

```
no `schemaVersion` move, and no RBAC grant a plain `scan` did not already have; slice 4 adds the route for a pack written outside kubeagent, its admission criteria machine-checked at the registry layer — where neither the rule loader nor the per-pack tests can see — though no outside pack has yet come through it), the second half of this item's first form — plus other baseline dimensions, and loading a pack into an installed binary without a kubeagent release, still ahead
```

The bundled clause is now split: who authors a pack is answered, and loading
one without a kubeagent release is named as its own outstanding item.

Verify afterwards that the row still has the same number of `|` cell
separators it had before — a stray pipe would break the table:

```bash
cd /home/ubuntu/git/kubeagent
awk 'NR==559 {n=gsub(/\|/,"|"); print "pipes:", n}' website/docs/roadmap.md
```

Expected: `pipes: 5`, unchanged from before the edit.

- [ ] **Step 2: `CHANGELOG.md` — the entries**

Under `## [Unreleased]` (currently empty, immediately above
`## [1.13.0] - 2026-08-08`), insert:

```markdown
### Added

- A documented route for contributing a policy pack, with admission criteria
  enforced by `go test ./internal/policypack` rather than by review. Three new
  checks cover the registry, the one layer nothing could see: every embedded
  `packs/*.yaml` must have a registry entry — one that does not would ship
  inside the binary while being invisible to `kubeagent policy packs`, to
  `--policy-pack` and to every other test — no two packs may share a name, a
  name must be usable as a `--policy-pack` value and fit the listing column,
  and a summary must be a single line the listing can align. Seven checks now
  run over every pack, so a contributor can predict every failure before
  opening a pull request. Acceptance stays curatorial: the criteria are
  necessary, not sufficient. No pack ships in this release, no existing command
  line behaves differently, and nothing versioned moves.

### Fixed

- Four statements in `CONTRIBUTING.md` that no longer described the code: the
  claim that `main.go` parses flags with the standard library and uses no Cobra
  (the CLI has been a Cobra tree in `internal/cli` since v0.73.0), a read-only
  command list missing `rbac` and `fleet`, the claim that `--fix` is the single
  write path (`scan --rollback` also writes, and `rbac check` issues the one
  POST outside remediation), and an import-wall list naming two packages where
  a dozen carry the wall.
```

- [ ] **Step 3: `CLAUDE.md` — record slice 4**

At the **end** of the post-1.0 curated-policy-packs bullet — after
`(see [website/docs/features/policy-packs.md](website/docs/features/policy-packs.md)).`
and before the next `- **Post-1.0 — fleet-scale, slice 2 …**` bullet — append,
at the bullet's indentation:

```markdown
  Slice 4 has since shipped, and **curated policy packs are complete**: there
  is now a documented route for a pack written outside kubeagent, with the
  admission criteria machine-checked at the layer nothing else can see.
  `policy.Load` validates every rule and the four generic tests validate every
  *registered* pack, but neither can see the registry — `Load` is handed bytes
  and never learns where they came from, and every generic test iterates
  `All()`, so anything absent from `All()` is invisible to all of them.
  `internal/policypack/registry_test.go` closes it: every embedded
  `packs/*.yaml` must have a registry entry, no two entries may share a name or
  a file, a name must match `^[a-z0-9]+(-[a-z0-9]+)*$` and fit the `%-14s`
  listing column in `internal/cli/policy.go`, and a summary must be one line
  with no trailing period and no leading capital. Acceptance stays curatorial —
  the criteria are necessary, not sufficient, and a pack that ships is
  kubeagent's curation whoever wrote it; attribution lives in the pack's header
  comment, which `--print` emits, and there is no author field, because a
  two-tier listing would tell an operator to trust some shipped rules less than
  others. The slice adds **no production Go code**: the registry gains no entry
  and no field, and no pack ships. Loading a pack into an installed binary
  without a kubeagent release remains deliberately absent, and no outside pack
  has yet come through the route
  (see [website/docs/features/policy-packs.md](website/docs/features/policy-packs.md)).
```

- [ ] **Step 4: `CLAUDE.md` — narrow the remaining-work sentence**

At the end of the `internal/fleetfile` bullet, replace:

```markdown
  The remaining post-1.0 work is the rest of the curated-packs item's second
  half — a pack contributed by someone other than kubeagent itself — plus
  other baseline dimensions.
```

with:

```markdown
  The remaining post-1.0 work is other baseline dimensions.
```

- [ ] **Step 5: Build the site**

```bash
cd /home/ubuntu/git/kubeagent/website
/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/mkdocs build --strict -f mkdocs.yml
cd /home/ubuntu/git/kubeagent
```

Expected: exit 0, no `WARNING` naming `roadmap.md`. **`cd` back to the
repository root**, as shown.

- [ ] **Step 6: Full verification**

```bash
cd /home/ubuntu/git/kubeagent
export PATH=$PATH:/usr/local/go/bin
go build ./...
go vet ./...
gofmt -l internal/
go test -p 2 -count=1 ./... 2>&1 | tail -40
git status --short
git diff --stat main -- go.mod go.sum internal/policy/ internal/policypack/policypack.go internal/policypack/packs/ internal/report/testdata/golden-scan.txt website/docs/schemas/ website/docs/quickstart.md deploy/
```

Expected: build clean, vet silent, `gofmt` prints nothing, **every package
passes**, `git status --short` shows only the three files this task modifies,
and the `git diff --stat` is **empty**.

- [ ] **Step 7: Confirm no version parenthetical leaked in**

```bash
cd /home/ubuntu/git/kubeagent
git diff CLAUDE.md website/docs/roadmap.md CHANGELOG.md | grep -n 'v1\.14\|(v1\.' || echo "clean: no version parenthetical"
```

Expected: `clean: no version parenthetical`. The `(vX.Y.Z)` is the release
commit's job.

- [ ] **Step 8: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add website/docs/roadmap.md CHANGELOG.md CLAUDE.md
git commit -s -m "docs: record the pack contribution path, and split a bundled roadmap clause

The roadmap's 'community contribution at run time' ran two distinct ideas
together — who authors a pack, and whether a pack can be loaded without a
kubeagent release. This slice answers the first and deliberately does not
answer the second, so the clause is split and each stands on its own.

CLAUDE.md records slice 4 and narrows the remaining post-1.0 work to other
baseline dimensions. The version parenthetical is left for the release commit.

CHANGELOG carries the route and the three registry checks under Added, and the
four corrected CONTRIBUTING statements under Fixed."
```

---

## Definition of done

Before handing the branch to the whole-branch review:

- [ ] Six commits on `policy-pack-contribution`, all signed off. Verify with
      `bash scripts/dco-check.sh main HEAD`.
- [ ] `go build ./...`, `go vet ./...` clean; `gofmt -l internal/` prints
      nothing.
- [ ] `go test -p 2 -count=1 ./...` — every package passes.
- [ ] `mkdocs build --strict` exits 0 with no warning naming a page this branch
      touched.
- [ ] `git diff --stat main -- go.mod go.sum internal/policy/
      internal/policypack/policypack.go internal/policypack/packs/
      internal/policypack/imports_test.go internal/policypack/packs_test.go
      internal/cli/ internal/report/testdata/golden-scan.txt
      website/docs/schemas/ website/docs/quickstart.md deploy/` is **empty**.
- [ ] `git diff main --stat` names exactly six files:
      `internal/policypack/registry_test.go`,
      `website/docs/features/policy-packs.md`, `CONTRIBUTING.md`,
      `website/docs/roadmap.md`, `CHANGELOG.md`, `CLAUDE.md`.
- [ ] No `internal/policypack/packs/orphan.yaml` anywhere in the diff or the
      working tree.
- [ ] No `Co-Authored-By` trailer and no AI attribution in any commit message.
- [ ] No `(vX.Y.Z)` parenthetical in the `CLAUDE.md` diff.
