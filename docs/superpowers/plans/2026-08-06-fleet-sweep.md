# Fleet Sweep (`kubeagent fleet`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `kubeagent fleet` — a one-shot, read-only command that sweeps many
kubeconfig contexts in bounded parallel, runs the same evaluation `kubeagent gate`
already runs against each, and prints one row per cluster worst-first with a
gate-compatible exit code.

**Architecture:** Per cluster the pipeline is exactly three calls —
`cluster.NewClient` (in `internal/cli`), `scan.Evaluate`, then the pure
`gate.Decide`. A new `internal/fleet` package reduces each `gate.Verdict` to a
counts-and-issue-kinds summary, sorts the summaries by one total order, and
derives the fleet verdict. No new diagnosis logic exists anywhere in this plan.
A second new package, `internal/glob`, is extracted from `internal/policy`'s
unexported `globMatch` so both `--policy` and `fleet --match` share one matcher.

**Tech Stack:** Go 1.26, Cobra (`internal/cli`), client-go (fake clientset in
tests), `internal/parallel` for the bounded pool, `internal/jsonschema` +
`internal/schemadoc` for the versioned JSON document. No new dependency.

---

## Global Constraints

Every task's requirements implicitly include this section.

- **NO NEW DEPENDENCY:** `go.mod` and `go.sum` must not change.
- **Package walls, both machine-enforced by an `imports_test.go` this plan adds:**
  - `internal/fleet` must never import `internal/remediate` or `internal/explain`
    (it inherits `internal/gate`'s wall).
  - `internal/glob` must import **nothing from kubeagent** and **nothing outside
    the standard library** — it joins `internal/jsonschema`, `internal/dashboard`
    and `internal/baseline` in the strictest class.
- **READ-ONLY toward the cluster:** `get`/`list` only. No `--fix` path.
  Separately — **never blur read-only into "makes no external calls"**:
  `kubeagent fleet` also makes **no LLM calls**. That is a second, stronger
  promise, not a restatement of the first.
- **CREDENTIALS:** no secrets, credentials, private IPs or internal hostnames
  anywhere — code, tests, fixtures, docs, help text, schema descriptions, seed
  corpora. Documentation IPs must be RFC 5737 (`192.0.2.0/24`,
  `198.51.100.0/24`, `203.0.113.0/24`); example domains RFC 2606
  (`example.com`/`.org`/`.net`). **URLs are credentials:** nothing beyond
  `scheme://host` in any forwarded artifact. **Kubeconfig paths and filesystem
  paths are credentials** — a connection failure naming the path on **stderr**
  is the one accepted carve-out, and it lives in `internal/cli`, never in
  `internal/fleet`. **Kubernetes node names, namespace names, pod names and
  workload names must NEVER reach the fleet report** — the report names clusters
  and issue kinds only. `Unreachable.Reason` comes from a fixed vocabulary,
  never `err.Error()`.
- **The eighth versioned JSON document enters at `schemaVersion` 1.0.** `scan`
  stays 1.2 and `gate` stays 1.1 — neither moves. Regenerate schemas with
  `go test ./internal/schemadoc -run TestSchemaDrift -update`. A **reviewer must
  never run any test with `-update`**.
- `internal/report/testdata/golden-scan.txt` must stay **BYTE-IDENTICAL**. Do
  **not** regenerate the demo GIF or `website/docs/quickstart.md`.
- **TDD:** write the failing test first, watch it fail, then implement. Pure
  functions unit-tested with fake objects; I/O packages use client-go's fake
  clientset.
- Go lives at `/usr/local/go/bin`. `go test` runs with `-p 2` locally, never
  `-short`.
- Every commit needs `git commit -s` (DCO enforced on `main`), authored solely
  by the human — **no `Co-Authored-By` trailer and no AI attribution of any
  kind, anywhere** (commits, code, comments, docs, changelog).
- Flags are declared **per command, never as persistent flags**. Every command
  sets `SilenceErrors` and `SilenceUsage`; validation lives in `RunE`, not in
  Cobra's `Args`/`MarkFlagsMutuallyExclusive` helpers, which would reword the
  messages.

## DANGER — do not touch any cluster

**NEVER run `./chaos/run.sh` in any form.** A run takes ~40 minutes and injects
real outages into a real cluster. **No task in this plan creates, deletes, or
touches any cluster** — every test in this plan uses client-go's fake clientset
or plain Go values. Do not run `kind`, `k3d`, `kubectl`, or `helm`.

## Verified interfaces — use these verbatim, do not re-derive

```go
scan.Evaluate(ctx context.Context, client kubernetes.Interface, opts scan.Options) (scan.Result, error)  // internal/scan/scan.go:166
gate.Decide(res scan.Result, opts gate.Options) gate.Verdict                                             // internal/gate/gate.go:172, PURE
parallel.Do[T any](ctx context.Context, workers, n int, fn func(context.Context, int) T) []T             // internal/parallel/parallel.go:33, INDEX-ordered
cluster.Contexts(kubeconfigPath string) ([]cluster.ContextInfo, error)                                   // internal/cluster/client.go:142, contractually PATH-FREE, sorted by name
cluster.NewClient(kubeconfig, contextName string) (*kubernetes.Clientset, error)                          // see internal/cli/watch.go:47-64
```

Gate exit codes (`internal/gate/gate.go:23-27`): `CodePass=0`, `CodeFail=1`,
`CodeInconclusive=2`, `CodeTimeout=3`, `CodeUsage=4`.

`gate.Verdict` fields used here (`internal/gate/gate.go:78-104`): `Verdict string`,
`Code int`, `FailOn findings.Level`, `Failing []findings.Finding`,
`Reported []findings.Finding`, `Inconclusive []gate.Blindspot`.

`findings.Finding` (`internal/findings/findings.go:73-81`):
`{Level Level; Kind, Namespace, Name, Issue, Reason, Owner string}`.
`findings.Level` is `Info`(0) / `Warning`(1) / `Critical`(2); `findings.Parse(s)`
maps `"critical"`/`"warning"`/`"info"`.

## Two spec ambiguities, resolved here

The spec ([docs/superpowers/specs/2026-08-06-fleet-sweep-design.md](../specs/2026-08-06-fleet-sweep-design.md))
leaves two points open. Both are settled below and every task follows these
rulings, not a re-reading of the spec.

1. **The spec's sample output line `[…12 more passing]` is documentation
   elision, not program output.** The spec's own prose says the command "prints
   one row per cluster", and its `TopIssues` comment reasons about the column
   "at 300 rows". The text renderer therefore prints **every** selected cluster
   as one row, with no elision and no `[…N more passing]` line. Do not emit that
   string.
2. **Selection lives in `internal/cli`, not in `internal/fleet`.** The spec's
   architecture puts kubeconfig enumeration and client construction in
   `internal/cli`, and its Testing section tests selection "through the pure
   flag-parsing path `internal/cli` already uses". So `internal/cli` imports
   `internal/glob` and `internal/fleet` does not. The spec's import list for
   `internal/fleet` names `internal/glob`; that line is superseded.

## File Structure

**Created**

| File | Responsibility |
|---|---|
| `internal/glob/glob.go` | The two-metacharacter matcher, moved from `internal/policy`, exported as `Match`. |
| `internal/glob/glob_test.go` | The existing table + blow-up tests, moved unchanged apart from package and call name. |
| `internal/glob/fuzz_test.go` | `FuzzGlob`, moved out of `internal/policy/fuzz_test.go`. |
| `internal/glob/testdata/fuzz/FuzzGlob/7a18e8a7330619a0` | The one seed corpus entry, moved. |
| `internal/glob/imports_test.go` | The stdlib-only + nothing-from-kubeagent wall. |
| `internal/fleet/fleet.go` | Package doc, `Target`, `Options`, `Report`, `ClusterSummary`, `Unreachable`, `Sweep`. |
| `internal/fleet/summarize.go` | Pure: `summarize`, `topIssues`, `rank`, `sortSummaries`, `decide`. |
| `internal/fleet/render.go` | `RenderText(w, Report)` and `RenderJSON(w, Report)`. |
| `internal/fleet/fleet_test.go` | `Sweep` over fake clientsets, determinism, end-to-end credential markers. |
| `internal/fleet/summarize_test.go` | Pure unit tests for the summarize/sort/verdict core. |
| `internal/fleet/render_test.go` | Exact-bytes text tests and a JSON round-trip. |
| `internal/fleet/imports_test.go` | The never-`remediate`/never-`explain` wall. |
| `internal/cli/fleet.go` | Flags, the pure `selectContexts`, client construction, `runFleetOpts`, `newFleetCommand`. |
| `internal/cli/fleet_test.go` | Pure selection and flag-parsing tests. |
| `website/docs/schemas/fleet-v1.json` | Generated — never hand-edited. |
| `website/docs/features/fleet.md` | The feature page. |

**Modified**

| File | Change |
|---|---|
| `internal/policy/glob.go` | Deleted (moved). |
| `internal/policy/glob_test.go` | Deleted (moved). |
| `internal/policy/op.go` | `globMatch(...)` → `glob.Match(...)`, plus the two comments naming it. |
| `internal/policy/op_test.go` | One comment naming `globMatch`. |
| `internal/policy/fuzz_test.go` | `FuzzGlob` removed. |
| `internal/policy/testdata/fuzz/FuzzGlob/` | Removed (moved). |
| `.github/workflows/fuzz.yml` | Matrix entry `./internal/policy`/`FuzzGlob` → `./internal/glob`. |
| `internal/jsonschema/jsonschema.go` | `FleetVersion = "1.0"`. |
| `internal/schemadoc/schemadoc.go` | One `Documents` entry for `fleet`. |
| `internal/cli/root.go` | Register `newFleetCommand()`. |
| `internal/cli/surface_test.go` | A `fleet` flag-surface table. |
| `website/mkdocs.yml` | Nav entry for the feature page. |
| `website/docs/features/json-schema.md` | Seven documents → eight. |
| `website/docs/roadmap.md` | Post-1.0 item 2, slice 1 shipped. |
| `CLAUDE.md` | The two new packages and their walls; the eighth document. |
| `CHANGELOG.md` | `[Unreleased]` entry. |
| `docs/go-concepts.md` | One entry (gitignored — never staged). |

---

### Task 1: Extract `internal/glob`

`internal/policy`'s `globMatch` is exactly the matcher `fleet --match` needs, and
it is unexported. Promote it to a stdlib-only leaf package both callers import.
Copying it instead would duplicate a function whose doc comment carries a
load-bearing complexity caveat.

**Files:**

- Create: `internal/glob/glob.go` (moved from `internal/policy/glob.go`)
- Create: `internal/glob/glob_test.go` (moved from `internal/policy/glob_test.go`)
- Create: `internal/glob/fuzz_test.go`
- Create: `internal/glob/testdata/fuzz/FuzzGlob/7a18e8a7330619a0` (moved)
- Create: `internal/glob/imports_test.go`
- Modify: `internal/policy/op.go:13`, `internal/policy/op.go:61-62`, `internal/policy/op.go:72`
- Modify: `internal/policy/op_test.go:76`
- Modify: `internal/policy/fuzz_test.go` (remove `FuzzGlob`)
- Modify: `.github/workflows/fuzz.yml:49-50`

**Interfaces:**

- Consumes: nothing from earlier tasks.
- Produces: `glob.Match(pattern, s string) bool` in package
  `github.com/imantaba/kubeagent/internal/glob`. Task 6 imports it.

- [ ] **Step 1: Move the three files with git, so history follows them**

```bash
export PATH=$PATH:/usr/local/go/bin
mkdir -p internal/glob/testdata/fuzz
git mv internal/policy/glob.go internal/glob/glob.go
git mv internal/policy/glob_test.go internal/glob/glob_test.go
git mv internal/policy/testdata/fuzz/FuzzGlob internal/glob/testdata/fuzz/FuzzGlob
```

- [ ] **Step 2: Rewrite `internal/glob/glob.go`'s package clause, name and doc comment**

Change line 1 from `package policy` to the block below, and rename the function.
The body (lines 22-57 of the original) is unchanged — do not retype it, only
change the signature line and the comment above it.

```go
// Package glob is a two-metacharacter matcher for names the standard library's
// path.Match cannot handle.
//
// It imports nothing from kubeagent and nothing outside the standard library —
// in the same class as internal/jsonschema, internal/dashboard and
// internal/baseline. internal/glob/imports_test.go enforces both halves. It has
// two callers with nothing else in common: a --policy rule matching an image
// reference, and `kubeagent fleet --match` matching a kubeconfig context name.
package glob

// Match reports whether s matches pattern. Two metacharacters, and only two:
// `*` matches any run of bytes including the empty run and including `/`, and
// `?` matches exactly one byte. Every other byte — `.`, `[`, `\` — is a
// literal.
//
// The standard library's path.Match will not let `*` cross a `/`, which breaks
// the most obvious rule an operator will write:
//
//	registry.example.com/*  against  registry.example.com/team/app:1.0
//
// and the most obvious context name an OpenShift kubeconfig will hold, where a
// single context name carries several slashes.
//
// Hence this. It is iterative, allocates nothing, and backtracks to the last
// star rather than recursing — so it cannot grow the stack and cannot go
// exponential the way a naive recursive translation can. But it is not
// linear: worst case is O(len(pattern) * len(s)), realized by a single star
// followed by a long, almost-matching literal run, where each mismatch
// re-scans nearly the whole literal. A caller must not hand this an unbounded
// value — internal/policy's checkOp caps the compared value at maxMatchLen
// before it reaches here, and `fleet --match` compares kubeconfig context
// names, which the operator wrote. Do not remove that cap on the mistaken
// belief that this function is linear.
func Match(pattern, s string) bool {
```

- [ ] **Step 3: Rewrite `internal/glob/glob_test.go`'s package clause and call sites**

Change line 1 to `package glob`. Then rename every `globMatch(` call to `Match(`
and every mention of `globMatch` in a test name or message to `Match`
(`TestGlobMatch` → `TestMatch`, `TestGlobMatchHasNoCatastrophicBlowup` →
`TestMatchHasNoCatastrophicBlowup`). Change nothing else — the case table and
the 2s deadline stay exactly as they are, so a green run proves the extraction
preserved behaviour.

- [ ] **Step 4: Create `internal/glob/fuzz_test.go` with `FuzzGlob` moved verbatim**

```go
package glob

import (
	"strings"
	"testing"
)

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
		Match(pattern, s)
		if !Match("*", s) {
			t.Errorf("* must match %q", s)
		}
		if !strings.ContainsAny(s, "*?") && !Match(s, s) {
			t.Errorf("a metacharacter-free pattern must match itself: %q", s)
		}
	})
}
```

- [ ] **Step 5: Remove `FuzzGlob` from `internal/policy/fuzz_test.go`**

Delete the whole `FuzzGlob` function (`internal/policy/fuzz_test.go:180-199` —
comment at 180, `func` at 183, closing brace at 199),
comment included). Leave the other three fuzz targets alone. `strings` is still
used elsewhere in that file, so the import block does not change — confirm with
the build in Step 8 rather than by eye.

- [ ] **Step 6: Create `internal/glob/imports_test.go`**

Modelled on `internal/baseline/imports_test.go`, which is the established form of
this wall.

```go
package glob

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulePath is this repository's module path. Any import that begins with it
// is a kubeagent package.
const modulePath = "github.com/imantaba/kubeagent"

// TestNoKubeagentImport pins the wall: internal/glob imports nothing from
// kubeagent. It is a leaf both internal/policy and internal/cli depend on, and
// a leaf that reached back into kubeagent could grow a cycle or — worse — reach
// internal/remediate. Keeping the wall at "nothing from kubeagent" makes that
// impossible by construction rather than by rule.
func TestNoKubeagentImport(t *testing.T) {
	for _, file := range packageFiles(t) {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		for _, imp := range importsOf(t, file) {
			if strings.HasPrefix(imp, modulePath) {
				t.Errorf("%s imports %s; internal/glob must import nothing from kubeagent", file, imp)
			}
		}
	}
}

// TestStdlibOnly pins the second half: no third-party dependency either. A
// standard-library import path has no dot in its first segment; every module
// path does, because it starts with a hostname.
func TestStdlibOnly(t *testing.T) {
	for _, file := range packageFiles(t) {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		for _, imp := range importsOf(t, file) {
			if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s; internal/glob must import nothing outside the standard library", file, imp)
			}
		}
	}
}

// importsOf returns the import paths one file declares.
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var out []string
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquoting %s in %s: %v", spec.Path.Value, path, err)
		}
		out = append(out, p)
	}
	return out
}

// packageFiles lists this package's Go files. It fatals on an empty result so a
// guard above can never pass vacuously.
func packageFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found; the guards would pass vacuously")
	}
	return files
}
```

- [ ] **Step 7: Run the tests and watch `internal/policy` fail to build**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... 2>&1 | head
```

Expected: FAIL — `internal/policy/op.go:72: undefined: globMatch`. That is the
compiler proving the move happened.

- [ ] **Step 8: Point `internal/policy` at the new package**

In `internal/policy/op.go`, add the import and change the one call site:

```go
import (
	"math"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/imantaba/kubeagent/internal/glob"
)
```

Line 72 becomes:

```go
			if glob.Match(v, got) {
```

Then update the two comments that name the old function. `internal/policy/op.go:13`:

```go
// storage class name — is far below this; the cap exists for the annotation
// nobody expected. Do not remove it on the belief that glob.Match is linear:
// it is not, and internal/glob says so.
```

`internal/policy/op.go:61-62`:

```go
		// glob.Match is O(len(pattern) * len(got)) in the worst case — a single
		// star followed by a long partly-matching literal run. `got` comes from
```

And `internal/policy/op_test.go:76`:

```go
// wrote the workload. glob.Match is quadratic in the worst case, so an
```

- [ ] **Step 9: Run the full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./internal/glob/... ./internal/policy/...
```

Expected: PASS, including the moved `TestMatch`, `TestMatchHasNoCatastrophicBlowup`,
`TestNoKubeagentImport`, `TestStdlibOnly`, and the replayed `FuzzGlob` seed.

- [ ] **Step 10: Update the nightly fuzz matrix**

In `.github/workflows/fuzz.yml`, change lines 49-50 from

```yaml
          - package: ./internal/policy
            target: FuzzGlob
```

to

```yaml
          - package: ./internal/glob
            target: FuzzGlob
```

Leave every other matrix entry alone. This is the whole cost of the extraction:
one matrix line, because the workflow's crasher-upload path is derived from
`matrix.package`.

- [ ] **Step 11: Confirm nothing else in the repository names the old function**

```bash
grep -rn 'globMatch' --include='*.go' --include='*.yml' --include='*.md' . || echo "clean"
```

Expected: `clean`.

- [ ] **Step 12: Commit**

```bash
git add internal/glob internal/policy/op.go internal/policy/op_test.go \
  internal/policy/fuzz_test.go .github/workflows/fuzz.yml
git add -u internal/policy
git commit -s -m "refactor: extract internal/glob from internal/policy

path.Match will not let * cross a /, which breaks both the most obvious
policy rule an operator writes and the most obvious OpenShift kubeconfig
context name. internal/policy already solved that with an unexported
globMatch; kubeagent fleet --match needs the same matcher, so promote it to
a stdlib-only leaf both callers import rather than copy a function whose
doc comment carries a load-bearing complexity caveat.

The table test, the blow-up test and FuzzGlob move unchanged, so a green
run proves the extraction preserved behaviour."
```

---

### Task 2: `internal/fleet` types and the pure summarize / sort / verdict core

The whole of fleet's judgement is pure: a `gate.Verdict` in, a `ClusterSummary`
out; a slice of summaries in, one total order out; the set of outcomes in, one
fleet verdict out. Build and test that first, with no cluster and no client
anywhere in sight.

**Files:**

- Create: `internal/fleet/fleet.go` (types only in this task)
- Create: `internal/fleet/summarize.go`
- Create: `internal/fleet/summarize_test.go`
- Create: `internal/fleet/imports_test.go`
- Modify: `internal/jsonschema/jsonschema.go:28-32`

**Interfaces:**

- Consumes: `gate.Verdict`, `findings.Finding`, `findings.Level` (see the
  verified-interfaces block above).
- Produces, for Tasks 3-6:

```go
type Target struct {
	Name   string
	Client kubernetes.Interface
}

type Options struct {
	FailOn         findings.Level
	Scan           scan.Options
	Workers        int
	ClusterTimeout time.Duration
}

type Report struct {
	SchemaVersion string           `json:"schemaVersion"`
	Verdict       string           `json:"verdict"`
	Code          int              `json:"exitCode"`
	FailOn        findings.Level   `json:"failOn"`
	Clusters      []ClusterSummary `json:"clusters"`
	Unreachable   []Unreachable    `json:"unreachable"`
}

type ClusterSummary struct {
	Context    string   `json:"context"`
	Verdict    string   `json:"verdict"`
	Critical   int      `json:"critical"`
	Warning    int      `json:"warning"`
	Info       int      `json:"info"`
	Blindspots int      `json:"blindspots"`
	TopIssues  []string `json:"topIssues,omitempty"`
}

type Unreachable struct {
	Context string `json:"context"`
	Reason  string `json:"reason"`
}

const (
	ReasonUnreachable = "connecting to the cluster"
	ReasonTimedOut    = "timed out"
)

func summarize(context string, v gate.Verdict) ClusterSummary   // pure
func sortSummaries(s []ClusterSummary)                          // in place, total order
func decide(clusters []ClusterSummary, unreachable []Unreachable) (string, int)
```

  Task 6 also uses `jsonschema.FleetVersion`.

- [ ] **Step 1: Add the schema version constant**

In `internal/jsonschema/jsonschema.go`, alongside the existing constants at
lines 28-32:

```go
	// FleetVersion is `kubeagent fleet --output json`. It enters the contract at
	// 1.0 as the eighth document; no existing surface moves for it.
	FleetVersion = "1.0"
```

- [ ] **Step 2: Write the failing tests for `summarize`**

Create `internal/fleet/summarize_test.go`:

```go
package fleet

import (
	"reflect"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/gate"
)

func finding(level findings.Level, issue string) findings.Finding {
	return findings.Finding{Level: level, Kind: "Pod", Namespace: "ns", Name: "pod", Issue: issue}
}

func TestSummarizeCountsByLevelAcrossFailingAndReported(t *testing.T) {
	v := gate.Verdict{
		Verdict: "fail",
		Failing: []findings.Finding{
			finding(findings.Critical, "CrashLoopBackOff"),
			finding(findings.Critical, "CrashLoopBackOff"),
		},
		Reported: []findings.Finding{
			finding(findings.Warning, "Unschedulable"),
			finding(findings.Info, "Unschedulable"),
		},
		Inconclusive: []gate.Blindspot{{Resource: "nodes", Reason: "forbidden"}},
	}

	got := summarize("example-context", v)

	want := ClusterSummary{
		Context:    "example-context",
		Verdict:    "fail",
		Critical:   2,
		Warning:    1,
		Info:       1,
		Blindspots: 1,
		TopIssues:  []string{"CrashLoopBackOff", "Unschedulable"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("summarize() = %+v, want %+v", got, want)
	}
}

// A pass carries no issues at all, and TopIssues must be nil rather than an
// empty slice so `omitempty` drops the key from the JSON document.
func TestSummarizeOfAPassIsAllZeroAndOmitsTopIssues(t *testing.T) {
	got := summarize("example-context", gate.Verdict{Verdict: "pass"})

	if got.TopIssues != nil {
		t.Errorf("TopIssues = %v, want nil so omitempty drops the key", got.TopIssues)
	}
	if got.Critical+got.Warning+got.Info+got.Blindspots != 0 {
		t.Errorf("summarize() = %+v, want every count zero", got)
	}
}

// Three, and the fourth-most-common kind is dropped rather than truncating the
// column at render time — the cap belongs to the document, not to one renderer.
func TestSummarizeCapsTopIssuesAtThreeMostFrequentFirst(t *testing.T) {
	var fs []findings.Finding
	for i := 0; i < 4; i++ {
		fs = append(fs, finding(findings.Critical, "aaa"))
	}
	for i := 0; i < 3; i++ {
		fs = append(fs, finding(findings.Critical, "bbb"))
	}
	for i := 0; i < 2; i++ {
		fs = append(fs, finding(findings.Critical, "ccc"))
	}
	fs = append(fs, finding(findings.Critical, "ddd"))

	got := summarize("example-context", gate.Verdict{Verdict: "fail", Failing: fs})

	want := []string{"aaa", "bbb", "ccc"}
	if !reflect.DeepEqual(got.TopIssues, want) {
		t.Errorf("TopIssues = %v, want %v", got.TopIssues, want)
	}
}

// Equal counts must not order by map iteration, which Go randomizes per run.
func TestSummarizeBreaksTopIssueTiesByNameAscending(t *testing.T) {
	fs := []findings.Finding{
		finding(findings.Critical, "zebra"),
		finding(findings.Critical, "alpha"),
		finding(findings.Critical, "mike"),
	}

	for i := 0; i < 50; i++ {
		got := summarize("example-context", gate.Verdict{Verdict: "fail", Failing: fs})
		want := []string{"alpha", "mike", "zebra"}
		if !reflect.DeepEqual(got.TopIssues, want) {
			t.Fatalf("run %d: TopIssues = %v, want %v", i, got.TopIssues, want)
		}
	}
}

// The report names clusters and issue kinds. A namespace, pod, workload or node
// name reaching it would be a credential leak, and the defence is structural:
// summarize reads Level and Issue and nothing else. This test proves the
// structure holds by marking every other field and looking for the markers.
func TestSummarizeCarriesNoObjectName(t *testing.T) {
	const marker = "MARKERVALUE"
	v := gate.Verdict{
		Verdict: "fail",
		Failing: []findings.Finding{{
			Level:     findings.Critical,
			Kind:      marker + "Kind",
			Namespace: marker + "Namespace",
			Name:      marker + "Name",
			Issue:     "CrashLoopBackOff",
			Reason:    marker + "Reason",
			Owner:     marker + "Owner",
		}},
		Inconclusive: []gate.Blindspot{{Resource: marker + "Resource", Reason: marker + "BlindReason"}},
	}

	got := summarize("example-context", v)

	rendered := strings.Join(append([]string{got.Context, got.Verdict}, got.TopIssues...), " ")
	if strings.Contains(rendered, marker) {
		t.Errorf("summary %+v carries a marker; the report must name clusters and issue kinds only", got)
	}
}
```

- [ ] **Step 3: Run them and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/fleet/
```

Expected: FAIL — `undefined: summarize`, `undefined: ClusterSummary`.

- [ ] **Step 4: Write `internal/fleet/fleet.go` — package doc and types**

```go
// Package fleet sweeps many clusters in one read-only pass and reduces each to
// a one-line verdict.
//
// The per-cluster pipeline is exactly the one `kubeagent gate` runs —
// scan.Evaluate then the pure gate.Decide — so a fleet sweep and a
// single-cluster gate can never disagree about the same cluster. fleet adds no
// diagnosis of its own.
//
// Two separate promises, and they are not restatements of each other. First,
// fleet is read-only toward every cluster it touches: get and list only, no
// write of any kind, and no --fix path. Second and additionally, fleet makes no
// LLM call. The package accordingly imports neither internal/remediate nor
// internal/explain, which internal/fleet/imports_test.go enforces.
//
// The report names kubeconfig context names and issue kinds. It never names a
// node, namespace, pod or workload, and that is structural rather than
// filtered: a summary is counts plus issue kinds, a shape an object name cannot
// fit into. Nor does it ever carry a kubeconfig path — the one accepted place a
// path may appear is stderr, from internal/cli, and this package writes no
// errors of its own.
package fleet

import (
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/scan"
)

// Target is one cluster to sweep. The caller builds the client, because
// building it needs a kubeconfig path and a kubeconfig path is a credential
// this package must never hold.
type Target struct {
	Name   string
	Client kubernetes.Interface
}

// Options configures a sweep.
type Options struct {
	// FailOn is the level at or above which a finding fails a cluster.
	FailOn findings.Level

	// Scan is the per-cluster read, handed in whole rather than rebuilt here so
	// that fleet judges exactly the check set `kubeagent gate` judges. Sharing
	// the constructor makes that a structural fact rather than a copied list
	// two commands must be kept in step by hand.
	Scan scan.Options

	// Workers bounds how many clusters are read at once. Sweep clamps it to
	// 1..64, so a zero value is 1 rather than an error.
	Workers int

	// ClusterTimeout is the per-cluster budget. A cluster that overruns it is
	// unreachable with ReasonTimedOut, and the other clusters keep going.
	ClusterTimeout time.Duration
}

// Report is `kubeagent fleet --output json` verbatim.
type Report struct {
	SchemaVersion string           `json:"schemaVersion"`
	Verdict       string           `json:"verdict"`
	Code          int              `json:"exitCode"`
	FailOn        findings.Level   `json:"failOn"`
	Clusters      []ClusterSummary `json:"clusters"`
	Unreachable   []Unreachable    `json:"unreachable"`
}

// ClusterSummary is one cluster's outcome: counts and issue kinds, never object
// names.
type ClusterSummary struct {
	Context    string `json:"context"`
	Verdict    string `json:"verdict"`
	Critical   int    `json:"critical"`
	Warning    int    `json:"warning"`
	Info       int    `json:"info"`
	Blindspots int    `json:"blindspots"`

	// TopIssues is at most three issue kinds, most frequent first, ties broken
	// by kind name ascending so the slice is deterministic. It is a signpost,
	// not an inventory: the operator runs `scan` against that one context for
	// the full list.
	TopIssues []string `json:"topIssues,omitempty"`
}

// Unreachable is a cluster that was selected and could not be judged.
//
// Unreachable is not the same as refused. A cluster kubeagent reached but was
// not allowed to read fully gets an ordinary ClusterSummary with a non-zero
// Blindspots count and an inconclusive verdict, because scan.Evaluate returned
// and gate.Decide recorded the refusal. Unreachable is only for a cluster that
// produced no scan.Result at all.
type Unreachable struct {
	Context string `json:"context"`

	// Reason is drawn from the fixed vocabulary below and is never an
	// err.Error(), which can carry a server URL or a filesystem path. The
	// underlying error still reaches the operator on stderr, from internal/cli.
	Reason string `json:"reason"`
}

// The fixed Unreachable.Reason vocabulary.
const (
	ReasonUnreachable = "connecting to the cluster"
	ReasonTimedOut    = "timed out"
)
```

- [ ] **Step 5: Write `internal/fleet/summarize.go`**

```go
package fleet

import (
	"sort"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/gate"
)

// maxTopIssues caps the issue kinds a summary carries. Three, because the
// column has to stay readable at three hundred rows and because the
// fourth-most-common kind has never been what makes an operator open a cluster.
const maxTopIssues = 3

// summarize reduces one cluster's gate verdict to its fleet row. It is pure,
// and it reads only Level and Issue off each finding — which is what keeps a
// namespace, pod, workload or node name out of the report by construction.
func summarize(context string, v gate.Verdict) ClusterSummary {
	s := ClusterSummary{
		Context:    context,
		Verdict:    v.Verdict,
		Blindspots: len(v.Inconclusive),
	}

	counts := map[string]int{}
	for _, f := range append(append([]findings.Finding{}, v.Failing...), v.Reported...) {
		switch f.Level {
		case findings.Critical:
			s.Critical++
		case findings.Warning:
			s.Warning++
		default:
			s.Info++
		}
		counts[f.Issue]++
	}
	s.TopIssues = topIssues(counts)
	return s
}

// topIssues returns at most maxTopIssues kinds, most frequent first, ties by
// name ascending. Go randomizes map iteration order per run, so the tiebreak is
// not a nicety: without it the same cluster would render differently twice.
func topIssues(counts map[string]int) []string {
	if len(counts) == 0 {
		return nil
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if counts[kinds[i]] != counts[kinds[j]] {
			return counts[kinds[i]] > counts[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	if len(kinds) > maxTopIssues {
		kinds = kinds[:maxTopIssues]
	}
	return kinds
}
```

- [ ] **Step 6: Run the tests and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 -run 'TestSummarize' ./internal/fleet/
```

Expected: PASS (5 tests).

- [ ] **Step 7: Write the failing tests for the sort order and the fleet verdict**

Append to `internal/fleet/summarize_test.go`:

```go
// One total order, used by both renderers and by the JSON document: verdict
// rank first, then critical, warning and info counts descending, then context
// name ascending. Context names are unique within a kubeconfig, so the last
// tiebreak makes the order total — two runs over the same fleet cannot differ.
func TestSortSummariesIsWorstFirstAndTotal(t *testing.T) {
	in := []ClusterSummary{
		{Context: "b-pass", Verdict: "pass"},
		{Context: "a-fail-low", Verdict: "fail", Critical: 1},
		{Context: "z-inconclusive", Verdict: "inconclusive", Blindspots: 1},
		{Context: "a-pass", Verdict: "pass"},
		{Context: "b-fail-high", Verdict: "fail", Critical: 4},
		{Context: "c-fail-tie", Verdict: "fail", Critical: 1},
	}

	sortSummaries(in)

	want := []string{"z-inconclusive", "b-fail-high", "a-fail-low", "c-fail-tie", "a-pass", "b-pass"}
	var got []string
	for _, s := range in {
		got = append(got, s.Context)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// inconclusive outranks fail, mirroring gate.Decide's own switch, where the
// blind case is evaluated before the failing case. When kubeagent could not see
// enough, a "fail" may understate what is actually wrong — so the honest fleet
// answer is that the run could not judge. Inverting this would let one
// unreachable cluster hide behind another cluster's failure.
func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		name        string
		clusters    []ClusterSummary
		unreachable []Unreachable
		wantVerdict string
		wantCode    int
	}{
		{"everything passed", []ClusterSummary{{Verdict: "pass"}, {Verdict: "pass"}}, nil, "pass", 0},
		{"one failing", []ClusterSummary{{Verdict: "pass"}, {Verdict: "fail"}}, nil, "fail", 1},
		{"one inconclusive", []ClusterSummary{{Verdict: "pass"}, {Verdict: "inconclusive"}}, nil, "inconclusive", 2},
		{"inconclusive outranks fail", []ClusterSummary{{Verdict: "fail"}, {Verdict: "inconclusive"}}, nil, "inconclusive", 2},
		{"unreachable outranks fail", []ClusterSummary{{Verdict: "fail"}}, []Unreachable{{Context: "x"}}, "inconclusive", 2},
		{"nothing selected is not a pass to invent", nil, nil, "pass", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, code := decide(tc.clusters, tc.unreachable)
			if verdict != tc.wantVerdict || code != tc.wantCode {
				t.Errorf("decide() = %q/%d, want %q/%d", verdict, code, tc.wantVerdict, tc.wantCode)
			}
		})
	}
}
```

- [ ] **Step 8: Run them and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 -run 'TestSortSummaries|TestDecide' ./internal/fleet/
```

Expected: FAIL — `undefined: sortSummaries`, `undefined: decide`.

- [ ] **Step 9: Add the sort and the verdict to `internal/fleet/summarize.go`**

```go
// rank orders verdicts worst first. Unreachable is rank 0 and is not a
// ClusterSummary verdict — the text renderer synthesises a row at that rank for
// each unreachable cluster, which is why the constant lives here rather than in
// the renderer.
func rank(verdict string) int {
	switch verdict {
	case "unreachable":
		return 0
	case "inconclusive":
		return 1
	case "fail":
		return 2
	default: // pass
		return 3
	}
}

// sortSummaries puts the worst cluster first, in place. The last tiebreak is
// the context name, which is unique within a kubeconfig — so the order is
// total and two runs over the same fleet render identical bytes.
func sortSummaries(s []ClusterSummary) {
	sort.Slice(s, func(i, j int) bool {
		a, b := s[i], s[j]
		if ra, rb := rank(a.Verdict), rank(b.Verdict); ra != rb {
			return ra < rb
		}
		if a.Critical != b.Critical {
			return a.Critical > b.Critical
		}
		if a.Warning != b.Warning {
			return a.Warning > b.Warning
		}
		if a.Info != b.Info {
			return a.Info > b.Info
		}
		return a.Context < b.Context
	})
}

// decide derives the fleet verdict and its exit code.
//
// inconclusive outranks fail, mirroring gate.Decide's own switch at
// internal/gate/gate.go:229-240, where the blind case is evaluated before the
// failing case. The reasoning carries over exactly: when kubeagent could not
// see enough, a "fail" may understate what is actually wrong.
func decide(clusters []ClusterSummary, unreachable []Unreachable) (string, int) {
	failing := false
	for _, c := range clusters {
		switch c.Verdict {
		case "inconclusive":
			return "inconclusive", gate.CodeInconclusive
		case "fail":
			failing = true
		}
	}
	if len(unreachable) > 0 {
		return "inconclusive", gate.CodeInconclusive
	}
	if failing {
		return "fail", gate.CodeFail
	}
	return "pass", gate.CodePass
}
```

- [ ] **Step 10: Run them and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/fleet/
```

Expected: PASS.

- [ ] **Step 11: Add the package wall**

Create `internal/fleet/imports_test.go`. It reuses the `importsOf` /
`packageFiles` machinery from `internal/baseline/imports_test.go` — the helpers
are per-package by design, so copying them is the established form here rather
than a shared test utility, which would need a non-test package to live in.

```go
package fleet

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// banned is the wall internal/fleet inherits from internal/gate, whose pipeline
// it runs. internal/remediate is the only package that writes to a cluster and
// internal/explain is the only one that calls a model — so keeping both out is
// what makes "read-only toward the cluster" and "makes no LLM call" two
// separate, checkable promises rather than one slogan.
var banned = []string{
	"github.com/imantaba/kubeagent/internal/remediate",
	"github.com/imantaba/kubeagent/internal/explain",
}

func TestNoRemediateOrExplainImport(t *testing.T) {
	for _, file := range packageFiles(t) {
		for _, imp := range importsOf(t, file) {
			for _, b := range banned {
				if imp == b {
					t.Errorf("%s imports %s; internal/fleet must never import it", file, b)
				}
			}
		}
	}
}

// importsOf returns the import paths one file declares.
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var out []string
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquoting %s in %s: %v", spec.Path.Value, path, err)
		}
		out = append(out, p)
	}
	return out
}

// packageFiles lists this package's Go files, test files included — a test that
// reached into internal/remediate would be just as much a hole in the wall. It
// fatals on an empty result so the guard can never pass vacuously.
func packageFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found; the guard would pass vacuously")
	}
	return files
}
```

Note the two helpers are byte-identical to `internal/glob/imports_test.go`'s.
That duplication is deliberate and unavoidable: a Go test helper cannot be
shared across packages without a non-test package to hold it, and
`internal/baseline/imports_test.go` already established this form. Do not
extract them.

- [ ] **Step 12: Run the package**

```bash
export PATH=$PATH:/usr/local/go/bin
go vet ./internal/fleet/ && go test -p 2 ./internal/fleet/ ./internal/jsonschema/
```

Expected: PASS.

- [ ] **Step 13: Commit**

```bash
git add internal/fleet/fleet.go internal/fleet/summarize.go \
  internal/fleet/summarize_test.go internal/fleet/imports_test.go \
  internal/jsonschema/jsonschema.go
git commit -s -m "feat(fleet): the fleet report types and its pure judgement core

summarize reduces one cluster's gate verdict to counts plus at most three
issue kinds. That shape is the credential defence: a namespace, pod,
workload or node name cannot fit into a count, so the report names clusters
and issue kinds by construction rather than by a filter someone has to
remember to apply.

decide mirrors gate.Decide's own switch, where inconclusive outranks fail:
when kubeagent could not see enough, a fail may understate what is wrong.
Inverting it at fleet scope would let one unreachable cluster hide behind
another cluster's failure."
```

---

### Task 3: `fleet.Sweep` — the bounded, deterministic read

**Files:**

- Modify: `internal/fleet/fleet.go` (append `Sweep`)
- Create: `internal/fleet/fleet_test.go`

**Interfaces:**

- Consumes: `Target`, `Options`, `Report`, `summarize`, `sortSummaries`,
  `decide`, `ReasonUnreachable`, `ReasonTimedOut` from Task 2.
- Produces: `func Sweep(ctx context.Context, targets []Target, opts Options) Report`.
  Task 4 renders its result; Task 6 calls it.

- [ ] **Step 1: Write the failing tests**

Create `internal/fleet/fleet_test.go`:

```go
package fleet

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/jsonschema"
)

// crashingPod is a pod in CrashLoopBackOff — the signal every detector suite
// agrees on, so it makes a cluster fail without depending on any one detector's
// thresholds. Names are markers, so TestSweepCarriesNoObjectName can look for
// them in the rendered report.
func crashingPod(marker string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: marker + "-pod", Namespace: marker + "-ns"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         marker + "-container",
				RestartCount: 9,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "CrashLoopBackOff",
					Message: "back-off restarting failed container",
				}},
			}},
		},
	}
}

func healthyClient() kubernetes.Interface { return fake.NewSimpleClientset() }

func TestSweepSummarizesEveryTarget(t *testing.T) {
	rep := Sweep(context.Background(), []Target{
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("alpha"))},
		{Name: "example-b", Client: healthyClient()},
	}, Options{FailOn: findings.Critical, Workers: 2, ClusterTimeout: 30 * time.Second})

	if rep.SchemaVersion != jsonschema.FleetVersion {
		t.Errorf("SchemaVersion = %q, want %q", rep.SchemaVersion, jsonschema.FleetVersion)
	}
	if len(rep.Clusters) != 2 {
		t.Fatalf("Clusters = %d, want 2", len(rep.Clusters))
	}
	if len(rep.Unreachable) != 0 {
		t.Errorf("Unreachable = %v, want none — both fake clients answer", rep.Unreachable)
	}
	if rep.Clusters[0].Context != "example-a" || rep.Clusters[0].Verdict != "fail" {
		t.Errorf("worst cluster = %+v, want example-a failing first", rep.Clusters[0])
	}
	if rep.Verdict != "fail" || rep.Code != 1 {
		t.Errorf("verdict = %q/%d, want fail/1", rep.Verdict, rep.Code)
	}
	if rep.FailOn != findings.Critical {
		t.Errorf("FailOn = %v, want critical echoed back", rep.FailOn)
	}
}

// The pool must not make the report a function of which cluster answered first.
// parallel.Do is index-ordered and sortSummaries is total, so the same input
// must produce byte-identical output every time.
func TestSweepIsDeterministic(t *testing.T) {
	targets := []Target{
		{Name: "example-c", Client: fake.NewSimpleClientset(crashingPod("gamma"))},
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("alpha"))},
		{Name: "example-b", Client: healthyClient()},
		{Name: "example-d", Client: healthyClient()},
	}
	opts := Options{FailOn: findings.Critical, Workers: 4, ClusterTimeout: 30 * time.Second}

	first := Sweep(context.Background(), targets, opts)
	for i := 0; i < 20; i++ {
		got := Sweep(context.Background(), targets, opts)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs from run 0:\n got %+v\nwant %+v", i, got, first)
		}
	}
}

// The whole report — every string it carries — must be free of node, namespace,
// pod, workload and container names.
func TestSweepCarriesNoObjectName(t *testing.T) {
	rep := Sweep(context.Background(), []Target{
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("MARKERVALUE"))},
	}, Options{FailOn: findings.Critical, Workers: 1, ClusterTimeout: 30 * time.Second})

	var sb strings.Builder
	sb.WriteString(rep.SchemaVersion + " " + rep.Verdict)
	for _, c := range rep.Clusters {
		sb.WriteString(" " + c.Context + " " + c.Verdict + " " + strings.Join(c.TopIssues, " "))
	}
	for _, u := range rep.Unreachable {
		sb.WriteString(" " + u.Context + " " + u.Reason)
	}
	if strings.Contains(sb.String(), "MARKERVALUE") {
		t.Errorf("report carries an object name: %q", sb.String())
	}
}

// A nil client is the shape internal/cli hands in when it could not build one.
// The cluster must be named as a blind spot, never silently dropped — one
// missing row out of three hundred is invisible.
func TestSweepNamesAClusterItCouldNotRead(t *testing.T) {
	rep := Sweep(context.Background(), []Target{
		{Name: "example-a", Client: healthyClient()},
		{Name: "example-unreachable", Client: nil},
	}, Options{FailOn: findings.Critical, Workers: 2, ClusterTimeout: 30 * time.Second})

	if len(rep.Clusters) != 1 {
		t.Errorf("Clusters = %d, want only the reachable one", len(rep.Clusters))
	}
	want := []Unreachable{{Context: "example-unreachable", Reason: ReasonUnreachable}}
	if !reflect.DeepEqual(rep.Unreachable, want) {
		t.Errorf("Unreachable = %+v, want %+v", rep.Unreachable, want)
	}
	if rep.Verdict != "inconclusive" || rep.Code != 2 {
		t.Errorf("verdict = %q/%d, want inconclusive/2 — an unread cluster is not a pass",
			rep.Verdict, rep.Code)
	}
}

// A cancelled context is the shape a per-cluster timeout takes. The reason must
// come from the fixed vocabulary, not from err.Error(), which can carry a
// server URL or a filesystem path.
func TestSweepReportsATimeoutFromTheFixedVocabulary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rep := Sweep(ctx, []Target{{Name: "example-a", Client: healthyClient()}},
		Options{FailOn: findings.Critical, Workers: 1, ClusterTimeout: time.Nanosecond})

	if len(rep.Unreachable) != 1 {
		t.Fatalf("Unreachable = %+v, want one entry", rep.Unreachable)
	}
	if rep.Unreachable[0].Reason != ReasonTimedOut {
		t.Errorf("Reason = %q, want %q", rep.Unreachable[0].Reason, ReasonTimedOut)
	}
}

// Zero targets is a legal sweep — internal/cli refuses an empty selection
// before it gets here, so this only pins that Sweep does not panic and does not
// invent a verdict it did not measure.
func TestSweepOfNothingIsAPassWithEmptySlices(t *testing.T) {
	rep := Sweep(context.Background(), nil, Options{FailOn: findings.Critical})

	if rep.Verdict != "pass" || rep.Code != 0 {
		t.Errorf("verdict = %q/%d, want pass/0", rep.Verdict, rep.Code)
	}
	if rep.Clusters == nil || rep.Unreachable == nil {
		t.Errorf("Clusters = %v, Unreachable = %v — both must be empty slices so the "+
			"JSON document has [] rather than null", rep.Clusters, rep.Unreachable)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 -run TestSweep ./internal/fleet/
```

Expected: FAIL — `undefined: Sweep`.

- [ ] **Step 3: Append `Sweep` to `internal/fleet/fleet.go`**

Add these imports to the file's import block: `context`, `errors`,
`github.com/imantaba/kubeagent/internal/gate`,
`github.com/imantaba/kubeagent/internal/jsonschema`,
`github.com/imantaba/kubeagent/internal/parallel`.

```go
// maxWorkers bounds the pool. Three hundred clusters at once would be three
// hundred TLS handshakes and three hundred concurrent API server conversations
// from one process — the cap is what makes "fleet-scale" a bounded read rather
// than a thundering herd the operator finds out about from their control plane.
const maxWorkers = 64

// Sweep reads every target and returns the fleet report.
//
// Determinism is preserved by construction, not by discipline: parallel.Do
// returns results in index order rather than completion order, each closure
// writes only its own result, and the sequential pass afterwards sorts by a
// total order. The rendered bytes are never a function of which cluster
// answered first.
//
// A target whose read fails is named in Unreachable rather than dropped. At
// fleet size that matters more than at one: one missing row out of three
// hundred is invisible, so the count reaches the header line and the verdict
// too.
func Sweep(ctx context.Context, targets []Target, opts Options) Report {
	workers := opts.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > maxWorkers {
		workers = maxWorkers
	}

	type outcome struct {
		verdict gate.Verdict
		err     error
	}

	results := parallel.Do(ctx, workers, len(targets), func(ctx context.Context, i int) outcome {
		t := targets[i]
		if t.Client == nil {
			return outcome{err: errClientUnavailable}
		}
		if opts.ClusterTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, opts.ClusterTimeout)
			defer cancel()
		}
		res, err := scan.Evaluate(ctx, t.Client, opts.Scan)
		if err != nil {
			return outcome{err: err}
		}
		return outcome{verdict: gate.Decide(res, gate.Options{FailOn: opts.FailOn})}
	})

	rep := Report{
		SchemaVersion: jsonschema.FleetVersion,
		FailOn:        opts.FailOn,
		Clusters:      []ClusterSummary{},
		Unreachable:   []Unreachable{},
	}
	for i, r := range results {
		if r.err != nil {
			rep.Unreachable = append(rep.Unreachable, Unreachable{
				Context: targets[i].Name,
				Reason:  reasonFor(r.err),
			})
			continue
		}
		rep.Clusters = append(rep.Clusters, summarize(targets[i].Name, r.verdict))
	}

	sortSummaries(rep.Clusters)
	sort.Slice(rep.Unreachable, func(i, j int) bool {
		return rep.Unreachable[i].Context < rep.Unreachable[j].Context
	})
	rep.Verdict, rep.Code = decide(rep.Clusters, rep.Unreachable)
	return rep
}

// errClientUnavailable stands in for the target internal/cli could not build a
// client for. It never reaches the report — reasonFor maps it to the fixed
// vocabulary — and it carries no path of its own.
var errClientUnavailable = errors.New("no client")

// reasonFor maps a read failure to the fixed Unreachable.Reason vocabulary.
// Deliberately not err.Error(): a client-go error routinely carries the API
// server URL, and a wrapped one can carry a kubeconfig path. Either would put a
// credential into a document written to be forwarded. The operator still gets
// the underlying error, on stderr, from internal/cli.
func reasonFor(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ReasonTimedOut
	}
	return ReasonUnreachable
}
```

Add `"sort"` to the import block as well.

- [ ] **Step 4: Run them and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/fleet/
```

Expected: PASS.

- [ ] **Step 5: Check the race detector, since this is the one concurrent path**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -race -p 2 -run TestSweep ./internal/fleet/
```

Expected: PASS with no race report.

- [ ] **Step 6: Commit**

```bash
git add internal/fleet/fleet.go internal/fleet/fleet_test.go
git commit -s -m "feat(fleet): the bounded, deterministic sweep

Per cluster the pipeline is exactly gate's: scan.Evaluate then the pure
gate.Decide, with a per-cluster timeout so one wedged control plane cannot
stall the other 299. parallel.Do returns results in index order and the
sequential pass afterwards sorts by a total order, so the rendered bytes
are never a function of which cluster answered first.

A cluster that produced no scan.Result is named in Unreachable with a
reason from a fixed vocabulary, never err.Error() — a client-go error
routinely carries the API server URL and a wrapped one can carry a
kubeconfig path."
```

---

### Task 4: The two renderers

**Files:**

- Create: `internal/fleet/render.go`
- Create: `internal/fleet/render_test.go`

**Interfaces:**

- Consumes: `Report`, `ClusterSummary`, `Unreachable`, `rank` from Tasks 2-3.
- Produces: `func RenderText(w io.Writer, rep Report) error` and
  `func RenderJSON(w io.Writer, rep Report) error`. Task 6 calls both.

Layout rules, pinned here because the spec's sample is illustrative:

- The `CLUSTER` column is as wide as the widest context name or the header,
  whichever is larger — an OpenShift context name can be forty characters.
- The `VERDICT` column is `len("inconclusive")` = 12 wide.
- `CRIT`/`WARN`/`INFO` are right-aligned in 4, two spaces between columns.
- An unreachable row leaves the three count cells blank and puts its reason in
  `TOP ISSUES`.
- Trailing whitespace is trimmed from every line, so a passing row with no
  issues does not end in spaces.
- **Every** selected cluster gets a row. There is no elision and no
  `[…N more passing]` line — see "Two spec ambiguities, resolved here".

- [ ] **Step 1: Write the failing text-renderer test**

Create `internal/fleet/render_test.go`:

```go
package fleet

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/findings"
)

func sampleReport() Report {
	rep := Report{
		SchemaVersion: "1.0",
		FailOn:        findings.Critical,
		Clusters: []ClusterSummary{
			{Context: "example-staging-2", Verdict: "inconclusive", Warning: 1, Blindspots: 2},
			{Context: "example-eu-1", Verdict: "fail", Critical: 4, Warning: 2,
				TopIssues: []string{"CrashLoopBackOff", "ImagePullBackOff"}},
			{Context: "example-us-3", Verdict: "fail", Critical: 1, Warning: 5, Info: 1,
				TopIssues: []string{"Unschedulable"}},
			{Context: "example-eu-2", Verdict: "pass"},
		},
		Unreachable: []Unreachable{{Context: "example-ap-1", Reason: ReasonUnreachable}},
	}
	rep.Verdict, rep.Code = decide(rep.Clusters, rep.Unreachable)
	return rep
}

func TestRenderTextIsExactBytes(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderText(&buf, sampleReport()); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}

	want := strings.Join([]string{
		"FLEET  5 clusters, 2 failing, 1 unreachable",
		"",
		"CLUSTER            VERDICT       CRIT  WARN  INFO  TOP ISSUES",
		"example-ap-1       unreachable                     connecting to the cluster",
		"example-staging-2  inconclusive     0     1     0  (2 blind spots)",
		"example-eu-1       fail             4     2     0  CrashLoopBackOff, ImagePullBackOff",
		"example-us-3       fail             1     5     1  Unschedulable",
		"example-eu-2       pass             0     0     0",
		"",
		"verdict: inconclusive (exit 2)",
		"",
	}, "\n")

	if got := buf.String(); got != want {
		t.Errorf("RenderText() =\n%s\nwant\n%s", got, want)
	}
}

// One blind spot, not "1 blind spots".
func TestRenderTextSingularBlindSpot(t *testing.T) {
	rep := Report{
		Verdict:  "inconclusive",
		Code:     2,
		Clusters: []ClusterSummary{{Context: "example-a", Verdict: "inconclusive", Blindspots: 1}},
	}
	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	if !strings.Contains(buf.String(), "(1 blind spot)") {
		t.Errorf("output = %q, want a singular blind spot", buf.String())
	}
}

// A blind spot and issue kinds are not exclusive; both belong in the column.
func TestRenderTextShowsIssuesAndBlindSpotsTogether(t *testing.T) {
	rep := Report{
		Verdict: "inconclusive",
		Code:    2,
		Clusters: []ClusterSummary{{
			Context: "example-a", Verdict: "inconclusive", Critical: 1, Blindspots: 3,
			TopIssues: []string{"OOMKilled"},
		}},
	}
	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	if !strings.Contains(buf.String(), "OOMKilled (3 blind spots)") {
		t.Errorf("output = %q, want both the issue kind and the blind-spot count", buf.String())
	}
}

// A context name can be far wider than the header, and the columns must still
// line up rather than run into each other.
func TestRenderTextWidensForALongContextName(t *testing.T) {
	long := "default/api-example-com:6443/kube:admin"
	rep := Report{
		Verdict:  "pass",
		Clusters: []ClusterSummary{{Context: long, Verdict: "pass"}, {Context: "example-a", Verdict: "pass"}},
	}
	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.HasPrefix(line, "CLUSTER") || strings.HasPrefix(line, long) || strings.HasPrefix(line, "example-a") {
			if !strings.Contains(line[len(long):], "  ") {
				t.Errorf("line %q does not keep two spaces after the widest name", line)
			}
		}
	}
}

// There is no elision. The spec's sample line is documentation shorthand, not
// program output — three hundred clusters means three hundred rows.
func TestRenderTextElidesNothing(t *testing.T) {
	rep := Report{Verdict: "pass"}
	for i := 0; i < 40; i++ {
		rep.Clusters = append(rep.Clusters, ClusterSummary{
			Context: "example-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Verdict: "pass",
		})
	}
	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	if strings.Contains(buf.String(), "more passing") {
		t.Error("output elides passing clusters; every selected cluster gets a row")
	}
	if got := strings.Count(buf.String(), "\n  ") + strings.Count(buf.String(), "example-"); got < 40 {
		t.Errorf("counted %d cluster rows, want 40", got)
	}
}

func TestRenderJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sampleReport()); err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}

	var got Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if got.Verdict != "inconclusive" || got.Code != 2 {
		t.Errorf("verdict = %q/%d, want inconclusive/2", got.Verdict, got.Code)
	}
	if len(got.Unreachable) != 1 {
		t.Errorf("Unreachable = %+v, want the one unreachable cluster as its own array — a "+
			"consumer filtering clusters[] must not have to know some entries have no counts",
			got.Unreachable)
	}
	// A passing cluster carries no topIssues key at all.
	if strings.Contains(buf.String(), `"topIssues": []`) {
		t.Error("an empty topIssues array reached the document; omitempty must drop the key")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 -run 'TestRender' ./internal/fleet/
```

Expected: FAIL — `undefined: RenderText`, `undefined: RenderJSON`.

- [ ] **Step 3: Write `internal/fleet/render.go`**

```go
package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// verdictWidth is the width of the VERDICT column: the longest word that can
// appear in it. "unreachable" is 11 and "inconclusive" is 12.
const verdictWidth = 12

// RenderJSON writes the report as the versioned JSON document.
//
// It keeps Unreachable as its own array rather than interleaving it, because a
// consumer filtering clusters[] for failures must not have to know that some
// entries have no counts. The text renderer makes the opposite choice for the
// opposite reason.
func RenderJSON(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// RenderText writes the one-row-per-cluster table, worst first.
//
// Unreachable clusters are interleaved into the same table at rank 0 with their
// reason in the TOP ISSUES column, rather than listed separately: a reader
// scanning rows top-down should not have to find a second table below the fold
// to learn that a cluster went unjudged.
//
// Every selected cluster gets a row. Three hundred clusters is three hundred
// rows — the summary that makes that readable is the row itself, not a cut-off.
func RenderText(w io.Writer, rep Report) error {
	rows := make([]row, 0, len(rep.Clusters)+len(rep.Unreachable))
	for _, u := range rep.Unreachable {
		rows = append(rows, row{context: u.Context, verdict: "unreachable", detail: u.Reason})
	}
	for _, c := range rep.Clusters {
		rows = append(rows, row{
			context: c.Context,
			verdict: c.Verdict,
			crit:    fmt.Sprint(c.Critical),
			warn:    fmt.Sprint(c.Warning),
			info:    fmt.Sprint(c.Info),
			detail:  detailOf(c),
		})
	}

	width := len("CLUSTER")
	for _, r := range rows {
		if len(r.context) > width {
			width = len(r.context)
		}
	}

	failing := 0
	for _, c := range rep.Clusters {
		if c.Verdict == "fail" {
			failing++
		}
	}

	fmt.Fprintf(w, "FLEET  %d clusters, %d failing, %d unreachable\n\n",
		len(rep.Clusters)+len(rep.Unreachable), failing, len(rep.Unreachable))

	writeRow(w, width, row{context: "CLUSTER", verdict: "VERDICT",
		crit: "CRIT", warn: "WARN", info: "INFO", detail: "TOP ISSUES"})
	for _, r := range rows {
		writeRow(w, width, r)
	}

	_, err := fmt.Fprintf(w, "\nverdict: %s (exit %d)\n", rep.Verdict, rep.Code)
	return err
}

// row is one rendered line. The count cells are strings rather than ints so an
// unreachable cluster can leave them blank — it has no counts, and printing 0
// would claim kubeagent looked and found nothing.
type row struct {
	context, verdict, crit, warn, info, detail string
}

func writeRow(w io.Writer, width int, r row) {
	line := fmt.Sprintf("%-*s  %-*s  %4s  %4s  %4s  %s",
		width, r.context, verdictWidth, r.verdict, r.crit, r.warn, r.info, r.detail)
	fmt.Fprintln(w, strings.TrimRight(line, " "))
}

// detailOf builds the TOP ISSUES cell for a judged cluster. A blind-spot count
// is appended rather than replacing the issue kinds: a cluster can perfectly
// well have both, and showing only one would hide the other.
func detailOf(c ClusterSummary) string {
	detail := strings.Join(c.TopIssues, ", ")
	if c.Blindspots == 0 {
		return detail
	}
	noun := "blind spots"
	if c.Blindspots == 1 {
		noun = "blind spot"
	}
	blind := fmt.Sprintf("(%d %s)", c.Blindspots, noun)
	if detail == "" {
		return blind
	}
	return detail + " " + blind
}
```

- [ ] **Step 4: Run them and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/fleet/
```

Expected: PASS. If `TestRenderTextIsExactBytes` fails on whitespace, the
expected string in the test is the contract — fix the renderer, not the test,
unless the mismatch is the header row's own padding, in which case check
`verdictWidth` against `len("VERDICT")`.

- [ ] **Step 5: Commit**

```bash
git add internal/fleet/render.go internal/fleet/render_test.go
git commit -s -m "feat(fleet): the text and JSON renderers

The two present unreachable clusters differently and deliberately. JSON
keeps them as their own array, because a consumer filtering clusters[] for
failures must not have to know some entries have no counts. Text
interleaves them into the one table at rank 0 with the reason in the TOP
ISSUES column, because a reader scanning rows top-down should not have to
find a second table below the fold to learn a cluster went unjudged.

Count cells are strings so an unreachable row can leave them blank:
printing 0 would claim kubeagent looked and found nothing."
```

---

### Task 5: Publish the eighth versioned JSON document

**Files:**

- Modify: `internal/schemadoc/schemadoc.go` (one `Documents` entry)
- Create: `website/docs/schemas/fleet-v1.json` (generated — never hand-written)

**Interfaces:**

- Consumes: `fleet.Report` and `jsonschema.FleetVersion` from Tasks 2-3.
- Produces: `kubeagent schema fleet` and the published schema file. Task 7
  documents them.

- [ ] **Step 1: Read the existing `Documents` table**

```bash
export PATH=$PATH:/usr/local/go/bin
sed -n '/^var Documents/,/^}/p' internal/schemadoc/schemadoc.go
```

Match the shape of the `baseline` entry exactly — same field order, same
sentence style in `Description`.

- [ ] **Step 2: Add the `fleet` entry**

Append to `var Documents`, after the `baseline` entry:

```go
	{
		Name: "fleet", Surface: "fleet", Version: jsonschema.FleetVersion,
		Root:        reflect.TypeOf(fleet.Report{}),
		Title:       "kubeagent fleet report",
		Description: "The document written by `kubeagent fleet --output json`: one summary per selected cluster, worst first, plus the clusters that could not be judged. A summary carries counts and issue kinds — it never names a node, namespace, pod or workload.",
	},
```

Add the import `"github.com/imantaba/kubeagent/internal/fleet"` to the file.

- [ ] **Step 3: Watch the drift test fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/schemadoc/
```

Expected: FAIL — `TestSchemaDrift` reports that `website/docs/schemas/fleet-v1.json`
does not exist yet.

- [ ] **Step 4: Generate the schema**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/schemadoc -run TestSchemaDrift -update
```

This is the only place in this plan where `-update` is correct. Do not run it
in any other task, and a **reviewer must never run it at all**.

- [ ] **Step 5: Read the generated file and check it for credentials**

```bash
cat website/docs/schemas/fleet-v1.json
grep -nE '10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.|/home/|/Users/|kubeconfig' website/docs/schemas/fleet-v1.json || echo "clean"
```

Expected: `clean`. Every description comes from the Go doc comments written in
Tasks 2-3, so a leak here means a doc comment needs fixing — fix the comment and
regenerate, never hand-edit the JSON.

- [ ] **Step 6: Confirm nothing else moved**

```bash
export PATH=$PATH:/usr/local/go/bin
git status --short website/docs/schemas/
go test -p 2 ./internal/schemadoc/ ./internal/jsonschema/
```

Expected: only `website/docs/schemas/fleet-v1.json` is new; `scan-v1.json` and
`gate-v1.json` are untouched, because neither version moved. Tests PASS.

- [ ] **Step 7: Confirm the command can print it**

```bash
export PATH=$PATH:/usr/local/go/bin
go run . schema fleet | head -5
go run . schema | grep fleet
```

Expected: the schema prints, and `fleet` appears in the list — `internal/cli`'s
schema command enumerates `schemadoc.Documents`, so no code change is needed
there.

- [ ] **Step 8: Commit**

```bash
git add internal/schemadoc/schemadoc.go website/docs/schemas/fleet-v1.json
git commit -s -m "feat(fleet): publish fleet as the eighth versioned JSON document

It enters the contract at 1.0. Nothing existing moves: scan stays at 1.2
and gate stays at 1.1, because neither type changed shape. kubeagent schema
fleet prints it from the running binary with no cluster and no kubeconfig."
```

---

### Task 6: `kubeagent fleet` — the command

**Files:**

- Create: `internal/cli/fleet.go`
- Create: `internal/cli/fleet_test.go`
- Modify: `internal/cli/root.go:118` (registration)
- Modify: `internal/cli/surface_test.go` (a `fleet` flag table)

**Interfaces:**

- Consumes: `glob.Match` (Task 1); `fleet.Target`, `fleet.Options`,
  `fleet.Report`, `fleet.Sweep`, `fleet.RenderText`, `fleet.RenderJSON`
  (Tasks 2-4); the existing `gateScanOptions(namespace string) scan.Options`
  (`internal/cli/gate.go:40`), `exitError` (`internal/cli/root.go:52`),
  `Normalize`, `longFlagLookup`, `envInt`, `envDuration`, `envOr`.
- Produces: `newFleetCommand() *cobra.Command`, registered on the root.

Selection rules, all validated in `RunE` and all exit 4:

| Input | Result |
|---|---|
| `--match` without `--all-contexts` | usage error |
| `--context` together with `--all-contexts` | usage error |
| neither `--context` nor `--all-contexts` | the kubeconfig's current context |
| neither, and no current context | usage error |
| `--context NAME` not in the kubeconfig | usage error naming it |
| `--all-contexts --match GLOB` matching nothing | usage error |
| a `cluster.NewClient` failure | usage error (exit 4), naming the context and — on stderr only — the kubeconfig path |

- [ ] **Step 1: Write the failing selection tests**

Create `internal/cli/fleet_test.go`:

```go
package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/cluster"
)

func contexts() []cluster.ContextInfo {
	return []cluster.ContextInfo{
		{Name: "example-eu-1"},
		{Name: "example-eu-2", Current: true},
		{Name: "example-us-3"},
		{Name: "staging-1"},
	}
}

func TestSelectContexts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wanted      []string
		allContexts bool
		match       string
		want        []string
		wantErr     string
	}{
		{name: "no selection is the current context", want: []string{"example-eu-2"}},
		{name: "explicit contexts, in the order given",
			wanted: []string{"example-us-3", "example-eu-1"},
			want:   []string{"example-us-3", "example-eu-1"}},
		{name: "all contexts, sorted", allContexts: true,
			want: []string{"example-eu-1", "example-eu-2", "example-us-3", "staging-1"}},
		{name: "a glob filter", allContexts: true, match: "example-eu-*",
			want: []string{"example-eu-1", "example-eu-2"}},
		{name: "a glob crossing a slash is still one pattern", allContexts: true, match: "*-us-*",
			want: []string{"example-us-3"}},
		{name: "match without all-contexts", match: "example-*",
			wantErr: "--match needs --all-contexts"},
		{name: "context with all-contexts", wanted: []string{"example-eu-1"}, allContexts: true,
			wantErr: "--context and --all-contexts cannot be combined"},
		{name: "an unknown context", wanted: []string{"nowhere"},
			wantErr: `unknown context "nowhere"`},
		{name: "a glob matching nothing", allContexts: true, match: "nowhere-*",
			wantErr: "no context matches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectContexts(contexts(), tc.wanted, tc.allContexts, tc.match)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("selectContexts() error = nil, want %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectContexts() error = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("selectContexts() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A kubeconfig with no current context and no explicit selection has nothing to
// sweep — that is bad input, not an empty pass.
func TestSelectContextsWithNoCurrentContext(t *testing.T) {
	_, err := selectContexts([]cluster.ContextInfo{{Name: "example-a"}}, nil, false, "")
	if err == nil || !strings.Contains(err.Error(), "no context selected") {
		t.Errorf("error = %v, want it to say no context is selected", err)
	}
}

// Every selection error is exit 4 — bad input, before any cluster was touched.
func TestSelectContextsErrorsAreUsageErrors(t *testing.T) {
	_, err := selectContexts(contexts(), nil, false, "example-*")
	if got := exitCodeFor(err); got != 4 {
		t.Errorf("exit code = %d, want 4", got)
	}
}

func TestParseFleetFlags(t *testing.T) {
	o, err := parseFleetFlags([]string{
		"--all-contexts", "--match", "example-*", "--fail-on", "warning",
		"--workers", "3", "--cluster-timeout", "90s", "--output", "json", "-n", "example-ns",
	})
	if err != nil {
		t.Fatalf("parseFleetFlags() error = %v", err)
	}
	if !o.allContexts || o.match != "example-*" || o.failOn != "warning" ||
		o.workers != 3 || o.clusterTimeout.String() != "1m30s" ||
		o.output != "json" || o.namespace != "example-ns" {
		t.Errorf("parsed = %+v, want every flag to reach its field", o)
	}
}

// The single-dash long-flag spelling the standard library accepted still works,
// because Normalize rewrites it before pflag sees it.
func TestParseFleetFlagsAcceptsSingleDashLongNames(t *testing.T) {
	o, err := parseFleetFlags([]string{"-output", "json", "-fail-on", "info"})
	if err != nil {
		t.Fatalf("parseFleetFlags() error = %v", err)
	}
	if o.output != "json" || o.failOn != "info" {
		t.Errorf("parsed = %+v, want the single-dash spelling to reach the fields", o)
	}
}

func TestParseFleetFlagsRejectsAnUnknownOutput(t *testing.T) {
	o, err := parseFleetFlags([]string{"--output", "yaml"})
	if err != nil {
		t.Fatalf("parseFleetFlags() error = %v", err)
	}
	if err := validateFleetOptions(o); err == nil || !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error = %v, want it to name the unsupported format", err)
	} else if exitCodeFor(err) != 4 {
		t.Errorf("exit code = %d, want 4", exitCodeFor(err))
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 -run 'Fleet|SelectContexts' ./internal/cli/
```

Expected: FAIL — `undefined: selectContexts`, `undefined: parseFleetFlags`.

- [ ] **Step 3: Write `internal/cli/fleet.go`**

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/fleet"
	"github.com/imantaba/kubeagent/internal/gate"
	"github.com/imantaba/kubeagent/internal/glob"
)

// fleetOptions is one field per flag, so parsing stays pure and testable and
// the flag surface can be asserted in a table.
type fleetOptions struct {
	kubeconfig     string
	contexts       []string
	allContexts    bool
	match          string
	failOn         string
	workers        int
	clusterTimeout time.Duration
	output         string
	namespace      string
}

func bindFleetFlags(cmd *cobra.Command, o *fleetOptions) {
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	cmd.Flags().StringArrayVar(&o.contexts, "context", nil, "kubeconfig context to sweep (repeatable)")
	cmd.Flags().BoolVar(&o.allContexts, "all-contexts", false, "sweep every context the kubeconfig defines")
	cmd.Flags().StringVar(&o.match, "match", "", "with --all-contexts: only contexts whose name matches this glob")
	cmd.Flags().StringVar(&o.failOn, "fail-on", "critical", "severity that fails the sweep: critical, warning or info")
	cmd.Flags().IntVar(&o.workers, "workers", envInt("KUBEAGENT_FLEET_WORKERS", 8), "clusters read concurrently")
	cmd.Flags().DurationVar(&o.clusterTimeout, "cluster-timeout", envDuration("KUBEAGENT_FLEET_CLUSTER_TIMEOUT", 60*time.Second), "per-cluster budget")
	cmd.Flags().StringVar(&o.output, "output", "text", "output format: text or json")
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "namespace to judge (default: all namespaces)")
}

// parseFleetFlags is pure: it builds a throwaway command, normalizes the
// single-dash long-flag spelling pflag would reject, and parses. It touches no
// kubeconfig and no cluster, which is what makes the flag surface testable
// without one.
func parseFleetFlags(args []string) (fleetOptions, error) {
	var o fleetOptions
	cmd := &cobra.Command{Use: "fleet"}
	bindFleetFlags(cmd, &o)
	if err := cmd.Flags().Parse(Normalize(args, longFlagLookup(cmd))); err != nil {
		return o, &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
	return o, nil
}

// validateFleetOptions checks the values that do not need a kubeconfig.
func validateFleetOptions(o fleetOptions) error {
	if o.output != "text" && o.output != "json" {
		return &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("unsupported output format %q: use text or json", o.output)}
	}
	if _, err := findings.Parse(o.failOn); err != nil {
		return &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("unsupported --fail-on %q: use critical, warning or info", o.failOn)}
	}
	return nil
}

// selectContexts resolves the flag combination to the ordered list of context
// names to sweep. It is pure over the kubeconfig's context list, so every
// selection rule is unit-testable without a kubeconfig.
//
// Every failure here is exit 4: bad input, discovered before any cluster was
// touched. A selection that resolves to nothing is one of them — an empty sweep
// reporting "pass" would be the worst possible answer, because it looks like
// good news.
func selectContexts(all []cluster.ContextInfo, wanted []string, allContexts bool, match string) ([]string, error) {
	usage := func(format string, a ...any) error {
		return &exitError{code: gate.CodeUsage, msg: fmt.Sprintf(format, a...)}
	}

	switch {
	case match != "" && !allContexts:
		return nil, usage("--match needs --all-contexts: it filters the contexts a sweep would otherwise take all of")
	case len(wanted) > 0 && allContexts:
		return nil, usage("--context and --all-contexts cannot be combined: pick the contexts or take them all")
	}

	known := make(map[string]bool, len(all))
	for _, c := range all {
		known[c.Name] = true
	}

	if len(wanted) > 0 {
		for _, name := range wanted {
			if !known[name] {
				return nil, usage("unknown context %q: the kubeconfig does not define it", name)
			}
		}
		return wanted, nil
	}

	if !allContexts {
		for _, c := range all {
			if c.Current {
				return []string{c.Name}, nil
			}
		}
		return nil, usage("no context selected and the kubeconfig names no current context: pass --context or --all-contexts")
	}

	var selected []string
	for _, c := range all {
		if match == "" || glob.Match(match, c.Name) {
			selected = append(selected, c.Name)
		}
	}
	if len(selected) == 0 {
		return nil, usage("no context matches --match %q", match)
	}
	sort.Strings(selected)
	return selected, nil
}

// buildFleetTargets connects to each selected context.
//
// A client that cannot be built is fatal, the same ruling buildTargets makes for
// the watch daemon: an operator who asked for three hundred clusters and
// silently got two hundred and ninety is worse off than one whose sweep refused
// to start. This is the one place a kubeconfig path may be named — on stderr,
// the operator's own channel — and it is why the path never reaches
// internal/fleet at all.
func buildFleetTargets(kubeconfig string, names []string) ([]fleet.Target, error) {
	targets := make([]fleet.Target, 0, len(names))
	for _, name := range names {
		client, err := cluster.NewClient(kubeconfig, name)
		if err != nil {
			return nil, &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("connecting to context %q: %v", name, err)}
		}
		targets = append(targets, fleet.Target{Name: name, Client: client})
	}
	return targets, nil
}

func runFleetOpts(o fleetOptions) error {
	if err := validateFleetOptions(o); err != nil {
		return err
	}
	level, _ := findings.Parse(o.failOn) // validated just above

	all, err := cluster.Contexts(o.kubeconfig)
	if err != nil {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
	names, err := selectContexts(all, o.contexts, o.allContexts, o.match)
	if err != nil {
		return err
	}
	targets, err := buildFleetTargets(o.kubeconfig, names)
	if err != nil {
		return err
	}

	rep := fleet.Sweep(context.Background(), targets, fleet.Options{
		FailOn:         level,
		Scan:           gateScanOptions(o.namespace),
		Workers:        o.workers,
		ClusterTimeout: o.clusterTimeout,
	})

	render := fleet.RenderText
	if o.output == "json" {
		render = fleet.RenderJSON
	}
	if err := render(os.Stdout, rep); err != nil {
		return err
	}
	if rep.Code != gate.CodePass {
		return &exitError{code: rep.Code}
	}
	return nil
}

func newFleetCommand() *cobra.Command {
	var o fleetOptions
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Sweep many clusters and report one verdict per cluster",
		Long: "Sweep every selected kubeconfig context in bounded parallel, running the same\n" +
			"evaluation `kubeagent gate` runs against each, and print one row per cluster\n" +
			"worst first. Read-only toward every cluster: get and list only, no writes, and\n" +
			"no model call. The report names contexts and issue kinds — never a node,\n" +
			"namespace, pod or workload.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetOpts(o)
		},
	}
	bindFleetFlags(cmd, &o)
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	})
	return cmd
}
```

- [ ] **Step 4: Check the helpers this file assumes actually exist**

```bash
export PATH=$PATH:/usr/local/go/bin
grep -n 'func envInt\|func envDuration\|func longFlagLookup\|func gateScanOptions\|func exitCodeFor' internal/cli/*.go
grep -n 'func Parse' internal/findings/findings.go
```

Both `envDur` and `envDuration` exist in `internal/cli/helpers.go` — read both
and use whichever the surrounding commands use for a per-operation budget.
`findings.Parse` returns `(Level, error)`, which is what the code above assumes.
If any signature differs from what is written here, the tree governs: adapt the
code, do not change the tree to match the plan.

- [ ] **Step 5: Register the command**

In `internal/cli/root.go:118`, add `newFleetCommand()` to the `AddCommand` call,
after `newGateCommand()`:

```go
	root.AddCommand(newVersionCommand(), newSchemaCommand(), newMCPCommand(), newTUICommand(), newScanCommand(),
		newWatchCommand(), newGateCommand(), newFleetCommand(), newRBACCommand(), newPolicyCommand(),
		newBaselineCommand(), newCompletionCommand())
```

Shell completion needs no further change: `kubeagent completion <shell>`
generates from the command tree, so registering the command is what puts `fleet`
and its flags into every completion script.

- [ ] **Step 6: Run the tests and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./internal/cli/
```

Expected: PASS.

- [ ] **Step 7: Add the flag-surface table**

In `internal/cli/surface_test.go`, following the shape of the existing `scan`
and `gate` tables (see `internal/cli/surface_test.go:47` and `:193`), add:

```go
func TestFleetFlagSurface(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want func(fleetOptions) bool
	}{
		{"kubeconfig", []string{"--kubeconfig", "/nonexistent/kubeconfig"}, func(o fleetOptions) bool { return o.kubeconfig == "/nonexistent/kubeconfig" }},
		{"context", []string{"--context", "example-a"}, func(o fleetOptions) bool { return len(o.contexts) == 1 && o.contexts[0] == "example-a" }},
		{"repeated context", []string{"--context", "example-a", "--context", "example-b"}, func(o fleetOptions) bool { return len(o.contexts) == 2 }},
		{"all-contexts", []string{"--all-contexts"}, func(o fleetOptions) bool { return o.allContexts }},
		{"match", []string{"--match", "example-*"}, func(o fleetOptions) bool { return o.match == "example-*" }},
		{"fail-on", []string{"--fail-on", "warning"}, func(o fleetOptions) bool { return o.failOn == "warning" }},
		{"workers", []string{"--workers", "16"}, func(o fleetOptions) bool { return o.workers == 16 }},
		{"cluster-timeout", []string{"--cluster-timeout", "2m"}, func(o fleetOptions) bool { return o.clusterTimeout == 2*time.Minute }},
		{"output", []string{"--output", "json"}, func(o fleetOptions) bool { return o.output == "json" }},
		{"namespace shorthand", []string{"-n", "example-ns"}, func(o fleetOptions) bool { return o.namespace == "example-ns" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseFleetFlags(tc.args)
			if err != nil {
				t.Fatalf("parseFleetFlags(%v) error = %v", tc.args, err)
			}
			if !tc.want(o) {
				t.Errorf("parseFleetFlags(%v) = %+v; the flag did not reach its field", tc.args, o)
			}
		})
	}
}

// The flags fleet deliberately does not offer. Multiplying a proxied per-node
// read by three hundred clusters is a shape this command will not have, and
// --fix, --rollback, --explain and --investigate would each break a promise the
// package doc makes.
func TestFleetRefusesTheFlagsItDoesNotOffer(t *testing.T) {
	for _, flag := range []string{"--logs", "--disk-usage", "--certs", "--capacity",
		"--explain", "--investigate", "--fix", "--rollback", "--policy"} {
		t.Run(flag, func(t *testing.T) {
			if _, err := parseFleetFlags([]string{flag}); err == nil {
				t.Errorf("parseFleetFlags(%q) error = nil, want an unknown-flag error", flag)
			}
		})
	}
}
```

- [ ] **Step 8: Run the whole suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
```

Expected: PASS everywhere, `internal/report`'s golden test included — nothing in
this task touches the scan report.

- [ ] **Step 9: Confirm the help text carries no credential**

```bash
export PATH=$PATH:/usr/local/go/bin
go run . fleet --help
```

Expected: no path other than the literal `$KUBECONFIG or ~/.kube/config`
placeholder, no host name, no example beyond `example-*`.

- [ ] **Step 10: Commit**

```bash
git add internal/cli/fleet.go internal/cli/fleet_test.go internal/cli/root.go internal/cli/surface_test.go
git commit -s -m "feat(fleet): kubeagent fleet, the command

Selection is a pure function over the kubeconfig's context list, so every
rule is unit-testable without a kubeconfig, and every selection failure is
exit 4 — bad input, found before any cluster is touched. A selection that
resolves to nothing is one of them: an empty sweep reporting pass would be
the worst possible answer, because it looks like good news.

A client that cannot be built is fatal, the same ruling the watch daemon
makes. That is also the one place a kubeconfig path may be named, on
stderr, which is why the path never reaches internal/fleet at all."
```

---

### Task 7: Documentation

**Files:**

- Create: `website/docs/features/fleet.md`
- Modify: `website/mkdocs.yml` (nav)
- Modify: `website/docs/features/json-schema.md` (seven → eight)
- Modify: `website/docs/roadmap.md` (post-1.0 item 2, slice 1)
- Modify: `CLAUDE.md` (the two new packages and their walls)
- Modify: `CHANGELOG.md` (`[Unreleased]`)
- Modify: `docs/go-concepts.md` (**gitignored — edit it, never stage it**)

**Interfaces:**

- Consumes: everything from Tasks 1-6. Produces no code.

- [ ] **Step 1: Write `website/docs/features/fleet.md`**

Follow the shape of `website/docs/features/ci-gate.md` — a one-paragraph what,
a worked example, a flag table, the exit codes, and a "What the report may name"
section. Required content:

- The command, its purpose, and that the per-cluster pipeline is exactly gate's.
- **Two separate promises, stated as two:** read-only toward every cluster
  (`get`/`list` only, no writes, no `--fix` path); and, separately and
  additionally, **no LLM call**.
- The sample output, copied from the spec's What-ships block but with **every**
  passing cluster shown — the spec's `[…12 more passing]` line is documentation
  shorthand and must not appear as though the command prints it.
- The flag table from the spec's Flags section, verbatim.
- The exit-code table from the spec's Verdict section, verbatim, including the
  sentence that **`inconclusive` outranks `fail`** and why.
- A "What the report may name" section: context names and issue kinds, and the
  explicit list of what it never names — a kubeconfig or filesystem path, a full
  API server URL, a node name, a namespace, pod or workload name — plus the
  reason it is structural rather than filtered.
- The `Unreachable` vs refused distinction, in one short paragraph.
- A link to `schemas/fleet-v1.json` and a note that `kubeagent schema fleet`
  prints it with no cluster and no kubeconfig.
- Every example context name must be an `example-*` name and every domain must
  be RFC 2606. No IP addresses at all; if one is unavoidable, RFC 5737.

- [ ] **Step 2: Add the nav entry**

In `website/mkdocs.yml`, at the end of the Features list, after the
`Restart-rate baseline: features/baseline.md` entry (currently the last one):

```yaml
      - Fleet sweep: features/fleet.md
```

- [ ] **Step 3: Update the schema count**

In `website/docs/features/json-schema.md`, change every "seven" that counts the
documents to "eight" and add `fleet` to the table of surfaces with version
`1.0`. Find them with:

```bash
grep -n 'seven\|Seven' website/docs/features/json-schema.md
```

State explicitly that `scan` stays at 1.2 and `gate` stays at 1.1 — the eighth
document moved neither.

- [ ] **Step 4: Update the roadmap**

In `website/docs/roadmap.md`, at post-1.0 item 2 (fleet-scale, near line 559),
record that slice 1 has shipped: `kubeagent fleet` sweeps every selected context
in bounded parallel and reports one verdict per cluster, and name what is
deliberately still ahead (cross-cluster correlation — "the same image is failing
in all three" — which this slice makes possible and does not attempt).

- [ ] **Step 5: Update `CLAUDE.md`**

Two edits in the Invariants section:

1. Add `internal/glob` to the sentence listing the packages that import nothing
   from kubeagent, noting it is stdlib-only alongside `internal/baseline`, that
   `internal/glob/imports_test.go` enforces both halves, and that
   `internal/policy` and `internal/cli` are its two callers.
2. Add `internal/fleet` as the next case in the read-only list, stating both
   promises separately — read-only toward the cluster (`get`/`list` only) **and**
   no LLM calls — that it must never import `internal/remediate` or
   `internal/explain`, that its report names contexts and issue kinds and never
   an object name, and linking
   [website/docs/features/fleet.md](website/docs/features/fleet.md).

Then update the versioned-documents paragraph: seven documents become **eight**,
`fleet.Report` enters at **1.0**, and `scan` stays 1.2 / `gate` stays 1.1.

- [ ] **Step 6: Add the `[Unreleased]` changelog entry**

Under `## [Unreleased]` → `### Added`:

```markdown
- **`kubeagent fleet` — sweep many clusters in one read-only pass.** Selects
  kubeconfig contexts with `--context` (repeatable) or `--all-contexts` plus an
  optional `--match` glob, reads them through a bounded worker pool
  (`--workers`, default 8, `KUBEAGENT_FLEET_WORKERS`) with a per-cluster budget
  (`--cluster-timeout`, default 60s), and prints one row per cluster worst
  first. Each cluster runs exactly the evaluation `kubeagent gate` runs, so the
  two can never disagree. Exit codes match `gate`'s, and `inconclusive`
  outranks `fail` for the same reason it does there. `--output json` writes the
  eighth versioned document, `fleet` at schema 1.0; `scan` stays at 1.2 and
  `gate` at 1.1. The report names contexts and issue kinds — never a node,
  namespace, pod or workload.
```

Under `### Changed`:

```markdown
- `internal/policy`'s unexported glob matcher moved to `internal/glob`, a
  stdlib-only leaf `internal/policy` and `internal/cli` now share. Behaviour is
  unchanged: the table test, the blow-up test and `FuzzGlob` moved with it.
```

- [ ] **Step 7: Add the Go-concepts entry (gitignored — do not stage it)**

`docs/go-concepts.md` is a running cheat-sheet for a developer learning Go. Add
one entry in the established style — **a plain everyday example first, then the
kubeagent example** — on **promoting an unexported function to its own package**:
why Go makes that a rename (`globMatch` → `glob.Match`), why the tests move with
it unchanged, and what "leaf package" buys you. No comparison to any other
language. One example is enough.

- [ ] **Step 8: Build the docs**

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml; cd ..
```

Expected: exit 0, "Documentation built", and no `WARNING` naming
`features/fleet.md`. The red "Material for MkDocs 2.0" banner is cosmetic.

- [ ] **Step 9: Sweep the new documentation for credentials**

```bash
grep -rnE '10\.[0-9]|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.|/home/|/Users/|https?://[^ )]+/[^ )]' \
  website/docs/features/fleet.md CHANGELOG.md || echo "clean"
```

Expected: `clean`, apart from any `https://k8sproject.top/...` link, which is the
project's own site and is allowed.

- [ ] **Step 10: Commit — staging by name, never `git add -A`**

`docs/go-concepts.md` is gitignored and must not appear in the commit. Verify
before committing.

```bash
git status --short
git add website/docs/features/fleet.md website/mkdocs.yml \
  website/docs/features/json-schema.md website/docs/roadmap.md CLAUDE.md CHANGELOG.md
git status --short --cached
git commit -s -m "docs: kubeagent fleet, and the eighth versioned document

Two promises stated as two throughout: fleet is read-only toward every
cluster it touches, and separately it makes no model call. The feature page
also writes down what the report may name — contexts and issue kinds — and
why the exclusion of object names is structural rather than a filter
someone has to remember to apply."
```

---

## Definition of done

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
go test -race -p 2 ./internal/fleet/ ./internal/cli/
bash scripts/dco-check.sh main HEAD
git diff --stat main -- go.mod go.sum   # must be empty
git diff --stat main -- internal/report/testdata/golden-scan.txt   # must be empty
```

All green, both `git diff` checks empty, and `kubeagent schema fleet` prints the
eighth document.

## Self-review notes

Checked against the spec, and recorded here so a reviewer does not re-derive them:

- **Spec coverage.** Architecture → Tasks 2, 3, 6. Cluster selection → Task 6.
  The report and "What the report may name" → Tasks 2, 3, 4, and the marker
  tests in Tasks 2 and 3. Verdict and exit codes → Task 2 (`decide`) and Task 6.
  Flags → Task 6, including the refusal table for the flags fleet does not
  offer. The JSON document → Tasks 2 and 5. Testing → the tests inside each
  task. Documentation → Task 7.
- **The `internal/glob` extraction cost** the spec names — the fuzz matrix entry
  — is Task 1 Step 10.
- **Two spec points are resolved rather than followed literally**, both recorded
  under "Two spec ambiguities, resolved here": the sample's `[…12 more passing]`
  line is documentation elision, and selection lives in `internal/cli` (so
  `internal/cli`, not `internal/fleet`, imports `internal/glob`).
- **Naming is consistent across tasks:** `summarize`, `topIssues`, `rank`,
  `sortSummaries`, `decide`, `Sweep`, `RenderText`, `RenderJSON`,
  `selectContexts`, `parseFleetFlags`, `validateFleetOptions`,
  `buildFleetTargets`, `runFleetOpts`, `newFleetCommand`, `glob.Match`.
- **Task 6 Step 4 is a deliberate verification step**, not a placeholder:
  `findings.Parse`'s second return value and the exact name of the duration
  helper are the two signatures most likely to differ from the shape assumed
  here, and the tree governs.
