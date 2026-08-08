# Curated policy packs, slice C — the pack contribution path

**Date:** 2026-08-08
**Status:** approved, ready for an implementation plan
**Roadmap item:** post-1.0 curated policy packs, the second half's remaining
clause — "a pack contributed by someone other than kubeagent itself"

## Goal

Put in place everything a project needs so that a policy pack written by
someone outside kubeagent can land: a documented route, machine-checked
admission criteria at the layer no existing test covers, and an honest written
statement of what the path does not yet include.

## What this slice does not ship

**A fourth pack.** No outside contributor exists. A pack kubeagent wrote and
labelled "contributed" would be a false statement about its own provenance,
and this project does not ship those. The deliverable is the route; the docs
say plainly that nobody has walked it yet.

**A run-time plugin path.** The registry stays compiled into the binary, the
same as `known-issues`. A contributed pack ships on a kubeagent release. That
limit is deliberate and is stated, not glossed.

**Any change to `internal/policy`.** The evaluator is untouched: no new
operator, no new relation, no new selectable kind.

**Any production Go change at all.** `internal/policypack/policypack.go` gains
no registry entry and no struct field. Everything this slice adds is test code
and documentation. That is worth stating up front because it bounds the risk:
nothing an operator runs today behaves differently afterwards.

## Ground verified before designing

Read directly, not taken on trust:

- `internal/policypack/policypack.go` — `Pack{Name, Summary, file}`, a
  three-entry registry, `//go:embed packs/*.yaml`, and `All`/`Names`/`Lookup`/
  `Bytes` all returning fresh copies. The package is stdlib-only (`embed`,
  `sort`) and imports nothing from kubeagent.
- `internal/policypack/packs_test.go` — the four generic per-pack tests, all
  looping over `policypack.All()`: `TestEveryPackLoads`,
  `TestRuleIDsCarryTheirPackPrefix`, `TestNoPackRuleIsCritical`,
  `TestPackCarriesNoHostOrAddress`.
- `internal/policypack/policypack_test.go` — `TestAllIsSortedByName`,
  `TestEveryPackHasASummary` (non-emptiness only), `TestLookupIsExact`,
  `TestNamesMatchesAll`, `TestBytesReturnsACopy`, `TestBytesMissReturnsFalse`.
  This file is `package policypack`, so it can already reach unexported state.
- `internal/policypack/imports_test.go` — both halves of the wall, and both
  skip `_test.go` files. A new test file therefore has no wall interaction.
- `internal/policy/load.go` — `Load` validates rule id non-emptiness, id
  charset, duplicate ids **across the whole document set**, selectable kind,
  the cluster-scoped namespace-selector guard, exactly one of path and
  relation, relation validity and applicability, level validity, non-empty
  message, path parseability, the ConfigMap contents refusal, and operator
  arity. It sanitizes each message through `safetext.Line` at ingress.
- `internal/cli/policy.go` — `runPolicyPacks` renders the listing with
  `"  %-14s %s — %s\n"`, computing the rule count by loading rather than
  storing it. `unknownPackErr` joins `policypack.Names()`. No non-test file in
  `internal/cli` names a pack.
- `CONTRIBUTING.md` and `website/docs/features/policy-packs.md` — the two
  surfaces a contributor reads.

**The gap this establishes.** `policy.Load` validates every rule thoroughly,
and the four generic tests validate every *registered* pack. Neither can see
the registry itself: `Load` is handed bytes and never learns where they came
from, and every generic test iterates `All()`, so anything absent from `All()`
is invisible to all of them. The registry is the layer a contributed pack
actually lands in, and it is unchecked.

## Decisions

### 1. Deliverable: documentation plus registry admission tests

The criteria a contributed pack must meet become a `go test` run, not a
maintainer's memory. A contribution's gate is then the same thing CI already
runs on every pull request.

A `kubeagent policy packs --check <candidate>` verb was considered and
rejected for this slice: it is a new CLI surface with new help text and
completion output, and the test suite is already the gate that decides whether
a pull request merges. Convenience for a contributor's pre-push loop does not
justify a new command.

### 2. Three registry invariants become tests

A new file, `internal/policypack/registry_test.go`, in `package policypack` —
internal, because two of the three need the unexported `files` embed handle
and the unexported `packs` slice.

**A. No orphan embedded file.** Every entry under `packs/` in the embedded
filesystem must be named by exactly one registry entry's `file` field.

Today a contributor can add `packs/mine.yaml`, forget the registry entry, and
the YAML is compiled into the shipped binary while being invisible to
`kubeagent policy packs`, to `--policy-pack`, and to all four generic tests —
which iterate `All()` and therefore never see it. Nothing anywhere reports
this. The file's rules are never loaded, never validated, and never run, but
they travel in every release artifact.

**B. Pack names are unique and CLI-safe.** No two registry entries share a
`Name`. Every `Name` matches `^[a-z0-9]+(-[a-z0-9]+)*$`.

Uniqueness is not cosmetic: `Lookup` returns the first match and stops, and
`All()` lists both, so a duplicate name silently shadows a pack. It is also
what makes the existing `TestRuleIDsCarryTheirPackPrefix` sufficient to rule
out a rule-id collision between two packs — with unique names, prefixed ids
cannot collide across packs; without them, they can.

The shape rule is because the name is the value an operator types after
`--policy-pack`, and the join between the listing and the flag. The pattern
admits `cost`, `reliability`, `security`, and a future hyphenated name; it
refuses an empty name, uppercase, whitespace, a dot, a slash, and a leading or
trailing hyphen.

**Name length is bounded at 14 bytes**, as part of the same test.
`internal/cli/policy.go` renders the listing with `%-14s` on the name, so a
longer name pushes that row's summary out of alignment with every other row.
The `14` is necessarily duplicated: a test in a package that may import
nothing from kubeagent cannot reach the format string. The test carries a
comment naming `internal/cli/policy.go` as the site the constant mirrors, so a
maintainer who changes one is told where the other is.

**C. Summary shape.** A `Summary` is one line, does not end in a period, and
does not begin with an uppercase letter.

`policypack.go`'s doc comment already states this rule — "one line for the
listing: lowercase, no trailing period" — and nothing checks it.
`TestEveryPackHasASummary` checks only non-emptiness and stays as it is; the
new test adds the shape. A multi-line summary breaks the listing outright; the
other two are the stated house style, and a contributor who trips the check
rewords.

**Deliberately not enforced: the file name matching the pack name.** All three
shipped packs happen to satisfy `packs/<name>.yaml`, but a registry entry
pointing at the wrong pack's file is already caught by
`TestRuleIDsCarryTheirPackPrefix`, whose ids would carry the other pack's
prefix. A second check for the same failure is not worth a rule a contributor
has to obey.

### 3. Acceptance is curatorial; credit lives in the pack header

The criteria are **necessary, not sufficient**. A maintainer still judges
whether each rule is true, whether the pack's subject belongs in kubeagent,
and whether the pack is honest about what it cannot say. A pack that ships is
kubeagent's curation whoever wrote it — kubeagent's name is on every rule an
operator runs by name.

**Attribution goes in the pack's own header comment**, which
`kubeagent policy packs --print <name>` already emits verbatim. No `Author` or
`Origin` field is added to `Pack`, and the listing does not distinguish
contributed packs from kubeagent's own. A provenance field was considered and
rejected: it would tell an operator to trust some shipped rules less than
others, which is the opposite of what accepting a pack means. If kubeagent
would not vouch for a pack, kubeagent does not merge it.

### 4. Bounded correction of CONTRIBUTING.md

`CONTRIBUTING.md` is the file a pack contributor reads first, and parts of it
are now false. This slice corrects the statements a contributor would act on,
by name — not the whole file:

- **Layout.** It says `main.go` does "flag parsing and subcommand dispatch
  (standard-library `flag` only; no Cobra)". Untrue since v0.73.0: `main.go`
  holds only the `version` symbol the release workflow stamps, and the CLI is
  a Cobra command tree in `internal/cli`, one file per command.
- **Invariant 1, the read-only list.** It names `scan`, `watch`, `mcp`,
  `gate` and `tui`, omitting `rbac` and `fleet`. It also does not mention that
  `rbac check` creates `SelfSubjectAccessReview` objects — a virtual resource
  the API server evaluates and never persists — which is the sole POST outside
  remediation and should be named where a contributor will see it.
- **Invariant 2, the import wall.** It names only `internal/mcp` and
  `internal/gate` as packages that may not import `internal/remediate` or
  `internal/explain`. The real list is considerably longer, and a contributor
  adding a package needs to know the rule generalizes.

A full audit of the file was considered and rejected: it turns a
contribution-path slice into a documentation sweep. What is corrected is what
is demonstrably false.

## File structure

**Created**

- `internal/policypack/registry_test.go` — `package policypack`. Three tests:
  `TestEveryEmbeddedPackIsRegistered`, `TestPackNamesAreUniqueAndCLISafe`,
  `TestPackSummaryShape`.

**Modified**

- `website/docs/features/policy-packs.md` — new `## Contributing a pack`
  section; the `## Not in this slice` list rewritten.
- `CONTRIBUTING.md` — a pointer to the canonical section, plus the three
  factual corrections above.
- `website/docs/roadmap.md` — the bundled "community contribution at run time"
  clause split into its two ideas.
- `CHANGELOG.md` — an `### Added` entry under `## [Unreleased]`.
- `CLAUDE.md` — the post-1.0 curated-packs bullet records slice C and narrows
  its remaining-work sentence.

**Not touched:** `internal/policy/`, `internal/policypack/policypack.go`,
`internal/policypack/packs/`, `internal/cli/`, `go.mod`, `go.sum`,
`deploy/`, `website/docs/schemas/`,
`internal/report/testdata/golden-scan.txt`.

## Documentation content

### `website/docs/features/policy-packs.md` — `## Contributing a pack`

Placed after `## Forking a pack` and before `## Not in this slice`, because
forking is the thing a would-be contributor does first and the natural place
to arrive from.

It covers, in this order:

1. **The route.** Open an issue first — a pack is a subject, not a patch, and
   agreeing the subject belongs comes before the YAML. Then: write the pack
   under `internal/policypack/packs/`, add its registry entry to the sorted
   slice in `policypack.go`, run `go test ./internal/policypack`, and open a
   pull request with a `CHANGELOG.md` entry.
2. **What the tests check**, as a list naming each of the seven — the four
   existing per-pack tests and the three new registry ones — with what each
   refuses. A contributor should be able to predict every failure before
   running anything.
3. **What no test can check**, the judgment criteria: is every rule *true* of
   the kind it selects; does the subject belong in kubeagent; is every message
   a single dot-free clause; does the pack carry an honest section naming what
   it cannot say, the precedent all three shipped packs follow; and is every
   rule's level right, with nothing `critical`.
4. **Curation.** The criteria are necessary, not sufficient. A shipped pack is
   kubeagent's curation whoever wrote it. Attribution goes in the pack's header
   comment, which `--print` shows.
5. **The two limits.** A contributed pack ships on a kubeagent release, not at
   run time — the registry is compiled in. And no pack has yet come through
   this path: `reliability`, `security` and `cost` are all kubeagent's own.

### `## Not in this slice`, rewritten

- The bullet reading "**A pack contributed by someone other than kubeagent
  itself.** `reliability`, `security` and `cost` are all kubeagent's own
  curation; a pack authored outside the project does not exist yet" is
  **narrowed, not deleted** — retraction is not deletion. What was absent was
  the *path*; the path now exists. The fact that no outside pack has used it
  is a statement about the world, and it moves into the new section as point
  5 rather than remaining listed as a missing capability.
- "**Operator-contributed packs at run time**" stays exactly as it is. It is a
  genuinely absent capability and this slice does not add it.
- "**A pack on by default**" and "**Any change to the evaluator**" stay as
  they are; both remain true.

### `website/docs/roadmap.md`

The clause "community contribution at run time" bundles two distinct ideas —
who authors a pack, and whether a pack can be loaded without a kubeagent
release. This slice answers the first and deliberately does not answer the
second, so the sentence is split so each can be true or false on its own.

## Constraints

Every one of these is inherited and non-negotiable.

- **Read-only toward the cluster.** A rule can only read a field; there is no
  `--fix` path from a rule. **Separately and additionally: no LLM call.** Two
  promises, never blurred into one. No comment, doc line, help string or commit
  message may suggest a pack is related to `--explain`, which is the model path.
- **kubeagent has no prices.** The third promise, from the cost pack: no
  billing data, no instance types, no node cost, no cloud API. Nothing written
  here may imply otherwise.
- **No engine change.** `internal/policy` is not touched by any task.
- **`internal/policypack` stays stdlib-only** and imports nothing from
  kubeagent. `imports_test.go` enforces both halves and must not be edited.
  The new test file is a `_test.go`, which both guards skip, and it imports
  only the standard library and `testing` regardless.
- **No new dependency.** `go.mod` and `go.sum` must not change.
- **No schema moves.** `scan` stays at 1.2, `gate` at 1.1, and the other six do
  not move. **Never run any test with `-update`, in any task, for any reason.**
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.** Do not
  regenerate the demo GIF or `website/docs/quickstart.md`.
- **No `critical` rule.** `TestNoPackRuleIsCritical` is generic over
  `policypack.All()` and must not be edited or weakened.
- **Credentials.** No secrets, credentials, private IPs or internal hostnames
  anywhere — code, tests, fixtures, docs, help text. Documentation IPs are
  RFC 5737; example domains RFC 2606. URLs are credentials: nothing beyond
  `scheme://host`, and the project's own `https://k8sproject.top` links are the
  only permitted host. A curated rule must never name a registry hostname.
- **TDD.** Write the failing test first, watch it fail, then implement.
  `go test` runs with `-p 2` locally, never `-short`.
- **Every commit is signed off** (`git commit -s`; DCO is enforced on `main`)
  and authored solely by the repository owner. No `Co-Authored-By` trailer and
  no AI attribution of any kind, anywhere.

## Testing

All three new invariants **already hold** on the shipping tree. A test written
against a satisfied invariant passes on its first run, which is not evidence
that it works — it is the failure mode that hid a no-op assertion in an earlier
slice.

So each test is proven by temporary falsification, and the implementer reports
what it actually saw:

- **Orphan file:** create `internal/policypack/packs/orphan.yaml` holding a
  syntactically valid but unregistered rule list, run the test, watch it name
  the orphan, delete the file.
- **Duplicate and malformed name:** temporarily add a fourth entry to `packs`
  duplicating an existing `Name`, run, watch it fail; change it to a name with
  an uppercase letter, run, watch it fail; change it to a fifteen-byte name,
  run, watch it fail; revert.
- **Summary shape:** temporarily give one entry a summary with a trailing
  period, run, watch it fail; then a leading capital; then an embedded
  newline; revert.

Reverting is part of the task, and the task's commit must contain **only** the
new test file. `git status` must be clean of the temporary edits before
committing, and the reviewer checks the diff for a stray `packs/orphan.yaml`
or a fourth registry entry.

Beyond that: `go build ./...`, `go vet ./...`, `gofmt -l internal/` and
`go test -p 2 -count=1 ./...` all clean, and
`git diff --stat main -- go.mod go.sum internal/policy/ internal/policypack/policypack.go internal/report/testdata/golden-scan.txt website/docs/schemas/`
empty at the end of every task.

The website build must pass `mkdocs build --strict` once the documentation
tasks land.

## Execution

Branch `policy-pack-contribution`, cut off `main` at the spec commit.
Subagent-driven: a fresh implementer plus an independent reviewer per task on
a mid model, and the whole-branch review on the most capable model.

**Danger, restated for every subagent:** never run `./chaos/run.sh` in any
form. It takes about forty minutes and injects real outages into a cluster. No
cluster is needed for any part of this slice.
