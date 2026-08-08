# Fleet selection from a file — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `kubeagent fleet` a second selection source — a YAML file that names the clusters to sweep, one entry per cluster, each pointing at a kubeconfig and a context — so a fleet can span several kubeconfigs and each row can carry an operator-chosen name.

**Architecture:** A new pure package `internal/fleetfile` decodes and validates the file's bytes and returns entries with the row identity already resolved and sanitized. `internal/cli` owns the file read, the flag, the `--match` filter and the client construction. `internal/fleet` learns nothing about files: `Target` gains a `Context` field beside its existing `Name`, `ClusterSummary` and `Unreachable` each gain an optional `Name`, and one unexported `identity()` helper becomes the single definition of what the report calls a cluster.

**Tech Stack:** Go 1.26, `sigs.k8s.io/yaml` (already a direct dependency), Cobra/pflag, `go/parser` for the import walls, `internal/safetext` for ingress sanitizing, `internal/glob` for `--match`.

**Source spec:** `docs/superpowers/specs/2026-08-07-fleet-selection-file-design.md` (committed `95b81fc` on `main`). The spec is the requirements; this plan is how to build it. The branch is cut off `main` at this plan's own commit, which sits directly on top of the spec.

**DANGER — never run `./chaos/run.sh` in any form, with any flags.** It takes ~40 minutes and injects real outages into a real cluster. Nothing in this plan needs a cluster, a kubeconfig or a network. Do not create, delete or touch any cluster.

## Global Constraints

Every task's requirements implicitly include this section.

- **READ-ONLY toward every cluster swept:** `get`/`list` only, no write of any kind, and no `--fix` path. **Separately and additionally: `fleet` makes no LLM call.** These are two promises, not one restatement — never blur them, and never let a comment, doc line, help string or commit message suggest a selection source is related to `--explain`, which is the model path.
- **NEW PACKAGE WALL, stated explicitly:** `internal/fleetfile` inherits `internal/fleet`'s wall — it must never import `internal/remediate` or `internal/explain` — **plus one `internal/fleet` cannot carry**: no `k8s.io/client-go` and no `internal/cluster`, which makes "holds no client" structural rather than a rule. It is **not** in the stdlib-only class (`internal/glob`, `internal/baseline`, `internal/knownissues`, `internal/policypack`): it imports `sigs.k8s.io/yaml` and `internal/safetext`. `internal/fleetfile/imports_test.go` enforces **both** halves and must fatal on an empty file list so the guard cannot pass vacuously.
- **NO NEW DEPENDENCY:** `go.mod` and `go.sum` must not change. `sigs.k8s.io/yaml v1.6.0` is **already** a direct dependency (go.mod's first `require` block, used by `internal/policy/load.go`), so importing it costs nothing. Verify at the end of every task: `git diff --stat main -- go.mod go.sum` must print nothing.
- **CREDENTIALS:** no secrets, credentials, private IPs or internal hostnames anywhere — code, tests, fixtures, docs, help text, schema descriptions, commit messages. Documentation IPs are RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains are RFC 2606 (`example.com`, `example.org`, `example.net`). **URLs are credentials:** nothing beyond `scheme://host` in any forwarded artifact; the project's own `https://k8sproject.top/...` links are fine, and `127.0.0.1` in a test kubeconfig is fine. **Kubeconfig paths are credentials:** the `--fleet-file` path and any per-entry kubeconfig path may appear on **stderr** and nowhere else — never in the report, never in a rendered row, never in an `internal/fleetfile` error. **Kubernetes node names are credentials.** Test context and cluster names are invented and generic (`prod-eu`, `prod-us`, `edge-a`, `edge-b`, `staging`, `dev`, `sandbox`). Test kubeconfig paths come from `t.TempDir()` — never a real home directory.
- **SANITIZE AT INGRESS:** `Entry.Name` passes through `safetext.Line` inside `Load`, because it reaches a terminal and a JSON document written to be forwarded. `Entry.Context` and `Entry.Kubeconfig` are **not** sanitized — they are lookup keys handed to `cluster.NewClient`, and the standing project rule is that matching and lookup run on the **raw** value.
- **ONLY fleet's schemaVersion moves, 1.1 → 1.2**, and only because two **optional** properties were added (both `omitempty`, both absent from `required`) — additive/MINOR. `scan` stays 1.2, `gate` stays 1.1, the other five do not move. Regenerate **exactly once**, in Task 5, with `go test ./internal/schemadoc -run TestSchemaDrift -update`. Never run any other test with `-update`, and never run `-update` during a review.
- **NO FIELD RENAME IN THE JSON CONTRACT:** `ClusterSummary.Context` and `Unreachable.Context` keep their `context` JSON names and their meaning — a consumer piping one into `kubectl --context` keeps working. A rename would be BREAKING/MAJOR and is off the table within 1.x. The new `name` property is additive and is written **only when it differs from the context**.
- **`internal/report/testdata/golden-scan.txt` must stay BYTE-IDENTICAL** — fleet has no scan render path, so it cannot move. Do **not** regenerate the demo GIF and do **not** touch `website/docs/quickstart.md`.
- **THE PER-CLUSTER PIPELINE IS UNTOUCHED:** `scan.Evaluate` then the pure `gate.Decide`, exactly as `kubeagent gate` runs it. `decide()` is unchanged. A sweep and a single-cluster `gate` can never disagree about the same cluster, and a new selection source must not change that.
- **`Unreachable.Reason` stays a two-entry vocabulary** (`ReasonUnreachable = "connecting to the cluster"`, `ReasonTimedOut = "timed out"`). A malformed fleet file is a new exit-4 failure, never a new `unreachable` reason. `cluster.NewClient` does no network I/O, so a client that cannot be built stays **fatal at exit 4**, never a reachability event.
- **TDD:** write the failing test first, run it, watch it fail for the right reason, then implement. Detectors and pure functions are unit-tested with values built in the test; I/O packages use client-go's fake clientset.
- **Tests run as `go test -p 2 ./...`**, never `-short`. Go lives at `/usr/local/go/bin` — `export PATH=$PATH:/usr/local/go/bin` first. Run `gofmt -l internal/` and `go vet ./...` before every commit; both must be clean.
- **Every commit uses `git commit -s`** (DCO is enforced on `main`; identity `imantaba <itn.taba@gmail.com>`). **No `Co-Authored-By: Claude` trailer and no AI attribution of any kind** anywhere — commit messages, code, comments, docs. Stage files **by name**; never `git add -A` or `git add .`.
- **A comment, doc line or error string that promises something the code does not keep is a defect to fix, not a deferrable Minor.** Two remedies: close the gap, or narrow the claim.

## ORDERING NOTE — read this before Task 2

Adding `Name` to `ClusterSummary` and `Unreachable` in **Task 2** changes the shape of the fleet JSON document, so **`go test ./internal/schemadoc` FAILS from Task 2 until Task 5 runs**. `TestSchemaDrift` will report an additive change with no version bump. **That failing test is the EXPECTED state, not a break.** No implementer in Tasks 2, 3 or 4 should try to "fix" it, and no reviewer should flag it. **Task 5 is the only place `-update` is permitted, exactly once, on `TestSchemaDrift`.**

In Tasks 2, 3 and 4, run the package under test plus its neighbours — `go test -p 2 ./internal/fleet/... ./internal/fleetfile/... ./internal/cli/...` — and run the full `go test -p 2 ./...` from Task 5 onward.

**Second interim state, also expected:** after Task 2 the renderer still reads `c.Context` for the row, so for a fleet-file sweep the table and the SHARED section could disagree about what a cluster is called. Task 3 fixes it. No fleet-file path exists until Task 4, so nothing user-visible ever ships in that state.

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `internal/fleetfile/fleetfile.go` | create | `Entry`, `Load` — decode, validate, resolve and sanitize the row identity. Pure. |
| `internal/fleetfile/fleetfile_test.go` | create | The whole validation table over bytes. |
| `internal/fleetfile/imports_test.go` | create | Both halves of the wall. |
| `internal/fleet/fleet.go` | modify | `Target.Context`; `ClusterSummary.Name` and `Unreachable.Name` (both types are declared here, not in `summarize.go`); `Sweep` resolves the identity once; the `Unreachable` sort; the package comment (Task 6). |
| `internal/fleet/summarize.go` | modify | `identity()`; `summarize`'s signature; `sortSummaries`'s tiebreak and its comment. |
| `internal/fleet/correlate.go` | modify | `clusterEvidence.context` → `.id`. |
| `internal/fleet/render.go` | modify | `row.context` → `row.id`; `namedClusters(ids []string)`; the two row-build sites use `identity(...)`. |
| `internal/cli/fleet.go` | modify | `--fleet-file`; `readFleetFile`; `selectEntries`; `buildFleetFileTargets`; the widened `--match` refusal; the branch in `runFleetOpts`; the command's help text. |
| `internal/jsonschema/jsonschema.go` | modify | `FleetVersion` 1.1 → 1.2. |
| `website/docs/schemas/fleet-v1.json` | regenerate | Once, in Task 5. |
| docs | modify | `website/docs/features/fleet.md`, `website/docs/features/json-schema.md`, `CHANGELOG.md`, `CLAUDE.md`, `website/docs/roadmap.md`. |

---

### Task 1: `internal/fleetfile` — the new package

**Files:**
- Create: `internal/fleetfile/fleetfile.go`
- Create: `internal/fleetfile/fleetfile_test.go`
- Create: `internal/fleetfile/imports_test.go`

**Interfaces:**
- Consumes: `sigs.k8s.io/yaml` (already in `go.mod`), `github.com/imantaba/kubeagent/internal/safetext`.
- Produces, for Task 4:
  - `type Entry struct { Name string \`json:"name,omitempty"\`; Kubeconfig string \`json:"kubeconfig,omitempty"\`; Context string \`json:"context"\` }`
  - `func Load(data []byte) ([]Entry, error)` — returns entries **in file order**, with `Name` already defaulted to `Context` and already sanitized. The caller never re-derives either.

**Spec correction the implementer must know:** spec §4.1 says the package imports exactly three things (`fmt`, `sigs.k8s.io/yaml`, `internal/safetext`). It needs **four**: `strings` as well, because §6.2 requires refusing a `context` that is empty *after trimming*, and that check runs before `safetext.Line` is involved. Add `strings`. Do not add anything else.

- [ ] **Step 1: Cut the branch**

```bash
export PATH=$PATH:/usr/local/go/bin
cd /home/ubuntu/git/kubeagent
git checkout main
git checkout -b fleet-selection-file
git log --oneline -2     # expect this plan's commit on top of the spec at 95b81fc
```

- [ ] **Step 2: Write the failing tests**

Create `internal/fleetfile/fleetfile_test.go`:

```go
package fleetfile

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    []Entry
		wantErr string // a substring the error must contain; "" means no error
	}{
		{
			name: "a multi-entry file loads in file order",
			yaml: "- context: prod-eu\n" +
				"- context: prod-us\n" +
				"- name: edge-a\n" +
				"  kubeconfig: /fleet/edge-a.kubeconfig\n" +
				"  context: default\n",
			want: []Entry{
				{Name: "prod-eu", Context: "prod-eu"},
				{Name: "prod-us", Context: "prod-us"},
				{Name: "edge-a", Kubeconfig: "/fleet/edge-a.kubeconfig", Context: "default"},
			},
		},
		{
			name: "name defaults to context",
			yaml: "- context: staging\n",
			want: []Entry{{Name: "staging", Context: "staging"}},
		},
		{
			name:    "a missing context is refused",
			yaml:    "- name: sandbox\n",
			wantErr: "entry 1 has no context",
		},
		{
			name:    "a whitespace-only context is refused",
			yaml:    "- context: \"   \"\n",
			wantErr: "entry 1 has no context",
		},
		{
			name:    "an empty list is refused",
			yaml:    "[]\n",
			wantErr: "names no clusters",
		},
		{
			name:    "an empty document is refused",
			yaml:    "",
			wantErr: "names no clusters",
		},
		{
			name:    "two entries resolving to one identity are refused",
			yaml:    "- context: default\n- context: default\n",
			wantErr: `entry 1 and entry 2 are both named "default"`,
		},
		{
			name:    "an unknown field is refused",
			yaml:    "- context: prod-eu\n  cluster: prod-eu\n",
			wantErr: "cluster",
		},
		{
			name:    "a server URL is refused: the format has no field for one",
			yaml:    "- context: prod-eu\n  server: https://api.example.com\n",
			wantErr: "server",
		},
		{
			name:    "a token is refused: the format has no field for one",
			yaml:    "- context: prod-eu\n  token: <PLACEHOLDER>\n",
			wantErr: "token",
		},
		{
			name:    "a typo'd kubeconfig key fails loudly rather than falling back",
			yaml:    "- context: prod-eu\n  kubconfig: /fleet/prod-eu.kubeconfig\n",
			wantErr: "kubconfig",
		},
		{
			name:    "a mapping instead of a list is refused",
			yaml:    "context: prod-eu\n",
			wantErr: "invalid YAML",
		},
		{
			name:    "malformed YAML is refused",
			yaml:    "- context: [unclosed\n",
			wantErr: "invalid YAML",
		},
		{
			name: "a name carrying a control character is sanitized at ingress",
			yaml: "- name: \"edge\\ta\"\n  context: prod-eu\n",
			want: []Entry{{Name: "edge a", Context: "prod-eu"}},
		},
		{
			name:    "a name that sanitizes to empty is refused",
			yaml:    "- name: \"\\u202E\"\n  context: prod-eu\n",
			wantErr: "entry 1 has an empty name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Load([]byte(tt.yaml))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Load() error = nil, want one containing %q (got %+v)", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v, want none", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// No error this package produces may name an entry's kubeconfig path. No
// validation failure is about that field, so the question never arises — and a
// path in an error is a credential in whatever collects that error.
func TestLoadErrorsNameNoKubeconfigPath(t *testing.T) {
	const marker = "/fleet/MARKERVALUE.kubeconfig"
	for _, body := range []string{
		"- kubeconfig: " + marker + "\n",
		"- kubeconfig: " + marker + "\n  context: prod-eu\n  cluster: prod-eu\n",
		"- kubeconfig: " + marker + "\n  name: \"\\u202E\"\n  context: prod-eu\n",
	} {
		_, err := Load([]byte(body))
		if err == nil {
			t.Fatalf("Load(%q) error = nil, want one", body)
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("Load(%q) error = %q, want it to name no kubeconfig path", body, err)
		}
	}
}
```

Create `internal/fleetfile/imports_test.go`:

```go
package fleetfile

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// banned is the wall internal/fleetfile inherits from internal/fleet, whose
// selection it feeds. internal/remediate is the only package that writes to a
// cluster and internal/explain is the only one that calls a model — so keeping
// both out is what makes "read-only toward the cluster" and "makes no LLM call"
// two separate, checkable promises rather than one slogan.
var banned = []string{
	"github.com/imantaba/kubeagent/internal/remediate",
	"github.com/imantaba/kubeagent/internal/explain",
}

// clientPackages is the second half of the wall, and it is one internal/fleet
// cannot carry: internal/fleet holds built clients by design. This package
// holds kubeconfig paths, so "it holds no client" has to be structural — a
// package that cannot import client-go cannot connect to anything, whatever a
// future edit intends.
var clientPackages = []string{
	"k8s.io/client-go",
	"github.com/imantaba/kubeagent/internal/cluster",
}

func TestNoRemediateOrExplainImport(t *testing.T) {
	for _, file := range packageFiles(t) {
		for _, imp := range importsOf(t, file) {
			for _, b := range banned {
				// The prefix arm covers a subpackage neither banned package has
				// today. Both are flat, so the arm is dead — and it is here so
				// that adding internal/remediate/foo tomorrow does not silently
				// open the wall.
				if imp == b || strings.HasPrefix(imp, b+"/") {
					t.Errorf("%s imports %s; internal/fleetfile must never import it", file, b)
				}
			}
		}
	}
}

func TestHoldsNoClient(t *testing.T) {
	for _, file := range packageFiles(t) {
		for _, imp := range importsOf(t, file) {
			for _, c := range clientPackages {
				if imp == c || strings.HasPrefix(imp, c+"/") {
					t.Errorf("%s imports %s; internal/fleetfile parses a file and holds no client", file, c)
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

- [ ] **Step 3: Run the tests to verify they fail**

```bash
go test ./internal/fleetfile/
```

Expected: FAIL — `undefined: Load`, `undefined: Entry`.

- [ ] **Step 4: Write the implementation**

Create `internal/fleetfile/fleetfile.go`:

```go
// Package fleetfile decodes the file `kubeagent fleet --fleet-file` reads: the
// list of clusters to sweep, one entry per cluster.
//
// The file NAMES clusters. It never carries a credential, and that is
// structural rather than a rule anyone has to follow: an Entry has three string
// fields and Load decodes with yaml.UnmarshalStrict, so `server:`, `token:`,
// `certificate-authority-data:` and every other kubeconfig field are load
// errors rather than silently ignored keys. Credentials keep coming from the
// kubeconfigs an entry points at, exactly as they did before this package
// existed.
//
// It is pure: no client, no context, no I/O beyond the bytes it is handed — the
// same shape as internal/policy.
//
// Two walls, enforced by internal/fleetfile/imports_test.go. The first is
// internal/fleet's: never internal/remediate, never internal/explain, which is
// what keeps "read-only toward the cluster" and "makes no LLM call" two
// separate, checkable promises. The second is one internal/fleet cannot carry:
// no k8s.io/client-go and no internal/cluster. This package holds kubeconfig
// paths, so "it holds no client" has to be a structural fact rather than a
// stated one.
package fleetfile

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// Entry is one cluster in a fleet file.
//
// sigs.k8s.io/yaml converts YAML to JSON and then uses encoding/json, so these
// are `json:` tags rather than `yaml:` ones — the same as policy.Rule.
type Entry struct {
	// Name is the row identity: what the report calls this cluster. Optional in
	// the file; Load defaults it to Context.
	Name string `json:"name,omitempty"`

	// Kubeconfig is the path to the kubeconfig this cluster is reached through.
	// Optional; internal/cli falls back to --kubeconfig, then $KUBECONFIG, then
	// the default location. This package never opens it and never names it in
	// an error.
	Kubeconfig string `json:"kubeconfig,omitempty"`

	// Context is the kubeconfig context to use. Required, deliberately: an
	// entry naming no context would take its kubeconfig's current-context,
	// which can change under the operator between runs. A checked-in fleet file
	// has to be reproducible, and its identity has to be knowable without
	// loading a kubeconfig.
	Context string `json:"context"`
}

// Load decodes and validates a fleet file's bytes.
//
// It returns entries in file order, with Name already resolved and already
// sanitized, so the caller never has to re-derive either. Every failure is bad
// input discovered before any cluster was touched — internal/cli reports them
// all at exit 4.
func Load(data []byte) ([]Entry, error) {
	var entries []Entry
	if err := yaml.UnmarshalStrict(data, &entries); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("the fleet file names no clusters: a sweep of nothing reporting \"pass\" would look like good news")
	}

	seen := make(map[string]int, len(entries)) // resolved name -> the 1-based entry that took it
	out := make([]Entry, 0, len(entries))
	for i, e := range entries {
		if strings.TrimSpace(e.Context) == "" {
			return nil, fmt.Errorf("entry %d has no context", i+1)
		}

		name := e.Name
		if name == "" {
			name = e.Context
		}
		// Sanitize at ingress: the name reaches a terminal and a JSON document
		// written to be forwarded. safetext.Line already trims, so an empty
		// result is the whole test. Context and Kubeconfig stay raw — they are
		// lookup keys handed to client-go, where a mangled value would silently
		// select nothing.
		name = safetext.Line(name)
		if name == "" {
			return nil, fmt.Errorf("entry %d has an empty name", i+1)
		}

		if first, dup := seen[name]; dup {
			return nil, fmt.Errorf("entry %d and entry %d are both named %q: two rows with one identity make the report ambiguous", first, i+1, name)
		}
		seen[name] = i + 1

		e.Name = name
		out = append(out, e)
	}
	return out, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/fleetfile/
gofmt -l internal/fleetfile/
go vet ./internal/fleetfile/
git diff --stat main -- go.mod go.sum     # must print nothing
```

Expected: `ok`, no gofmt output, no vet output, no go.mod/go.sum diff.

- [ ] **Step 6: Commit**

```bash
git add internal/fleetfile/fleetfile.go internal/fleetfile/fleetfile_test.go internal/fleetfile/imports_test.go
git commit -s -m "fleetfile: parse the file that names a fleet

A bare YAML list, one entry per cluster: a required context, an optional
kubeconfig path, an optional display name defaulting to the context. Load
returns entries in file order with the name resolved and sanitized.

The format cannot express a credential, and structurally rather than by
rule: three string fields decoded with UnmarshalStrict make server, token
and certificate-authority-data load errors. Strictness pays twice — a
typo'd kubconfig key fails loudly instead of silently sweeping whatever
the default kubeconfig points at.

Two walls, both enforced: never remediate or explain, and never
client-go or internal/cluster. The second is one internal/fleet cannot
carry, and it is what makes 'holds no client' structural in a package
that holds kubeconfig paths."
```

---

### Task 2: `internal/fleet` — row identity

**Files:**
- Modify: `internal/fleet/fleet.go` (`Target`, `Unreachable`, `Sweep`'s loop, the `Unreachable` sort)
- Modify: `internal/fleet/summarize.go` (`ClusterSummary`, `identity`, `summarize`, `sortSummaries`)
- Modify: `internal/fleet/correlate.go` (`clusterEvidence.context` → `.id`)
- Test: `internal/fleet/fleet_test.go`, `internal/fleet/summarize_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 — `internal/fleet` learns nothing about files.
- Produces, for Tasks 3 and 4:
  - `type Target struct { Name string; Context string; Client kubernetes.Interface }`
  - `ClusterSummary` and `Unreachable` each gain `Name string \`json:"name,omitempty"\``
  - `func identity(name, context string) string` (unexported, `internal/fleet`)
  - `func summarize(name, context string, v gate.Verdict) (ClusterSummary, clusterEvidence)`
  - `clusterEvidence` field `context` is now `id`

**Reminder:** `go test ./internal/schemadoc` starts failing at this task and stays failing until Task 5. That is expected. Do not run `-update` here.

- [ ] **Step 1: Write the failing tests**

Append to `internal/fleet/summarize_test.go`:

```go
func TestIdentity(t *testing.T) {
	tests := []struct {
		name, context, want string
	}{
		{"", "prod-eu", "prod-eu"},
		{"edge-a", "default", "edge-a"},
		{"edge-a", "", "edge-a"},
		{"", "", ""},
	}
	for _, tt := range tests {
		if got := identity(tt.name, tt.context); got != tt.want {
			t.Errorf("identity(%q, %q) = %q, want %q", tt.name, tt.context, got, tt.want)
		}
	}
}

// summarize copies the pair it is handed. Resolving the identity is Sweep's
// job, and doing it in two places would be two definitions of one rule.
func TestSummarizeCopiesTheResolvedNameAndContext(t *testing.T) {
	s, ev := summarize("edge-a", "default", gate.Verdict{Verdict: "pass"})
	if s.Name != "edge-a" || s.Context != "default" {
		t.Errorf("summary = {Name:%q Context:%q}, want {edge-a default}", s.Name, s.Context)
	}
	if ev.id != "edge-a" {
		t.Errorf("evidence id = %q, want edge-a — the evidence carries the row identity", ev.id)
	}

	s, ev = summarize("", "prod-eu", gate.Verdict{Verdict: "pass"})
	if s.Name != "" {
		t.Errorf("summary Name = %q, want empty so omitempty drops the key", s.Name)
	}
	if s.Context != "prod-eu" || ev.id != "prod-eu" {
		t.Errorf("summary = {Context:%q}, evidence id = %q, want both prod-eu", s.Context, ev.id)
	}
}

// Four clusters whose context is all "default" is exactly the per-cluster
// kubeconfig case, and it is what made the old tiebreak non-total: sort.Slice
// is not stable, so a non-total comparator renders different bytes on different
// runs. The comparator must break on the row identity.
func TestSortSummariesIsTotalWhenEveryContextIsTheSame(t *testing.T) {
	build := func(names ...string) []ClusterSummary {
		out := make([]ClusterSummary, 0, len(names))
		for _, n := range names {
			out = append(out, ClusterSummary{Name: n, Context: "default", Verdict: "pass"})
		}
		return out
	}

	a := build("edge-a", "edge-b", "edge-c", "edge-d")
	b := build("edge-d", "edge-c", "edge-b", "edge-a")
	sortSummaries(a)
	sortSummaries(b)

	if !reflect.DeepEqual(a, b) {
		t.Fatalf("two input orders sorted differently:\n %+v\n %+v", a, b)
	}
	for i, want := range []string{"edge-a", "edge-b", "edge-c", "edge-d"} {
		if a[i].Name != want {
			t.Errorf("sorted[%d] = %q, want %q", i, a[i].Name, want)
		}
	}
}
```

Append to `internal/fleet/fleet_test.go`:

```go
// Target must never gain a kubeconfig path field. The caller builds the client
// precisely so that a credential never enters this package, and a field set is
// the only thing a test can pin that a comment cannot.
func TestTargetCarriesNoKubeconfigPath(t *testing.T) {
	var got []string
	typ := reflect.TypeOf(Target{})
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	want := []string{"Name", "Context", "Client"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Target fields = %v, want exactly %v — a kubeconfig path is a credential this package must never hold", got, want)
	}
}

// A name is written only when it says something the context does not. A
// kubeconfig sweep hands in Name with Context unset, and its rows must carry no
// name at all so the JSON stays byte-identical to v1.10.0's.
func TestSweepNamesAClusterOnlyWhenItDiffersFromItsContext(t *testing.T) {
	rep := Sweep(context.Background(), []Target{
		{Name: "edge-a", Context: "default", Client: healthyClient()},
		{Name: "prod-eu", Client: healthyClient()},
		{Name: "prod-us", Context: "prod-us", Client: healthyClient()},
	}, Options{FailOn: findings.Critical, Workers: 3, ClusterTimeout: 30 * time.Second})

	want := map[string]string{ // context -> expected Name
		"default": "edge-a",
		"prod-eu": "",
		"prod-us": "",
	}
	if len(rep.Clusters) != 3 {
		t.Fatalf("Clusters = %d, want 3", len(rep.Clusters))
	}
	for _, c := range rep.Clusters {
		expected, known := want[c.Context]
		if !known {
			t.Fatalf("unexpected context %q", c.Context)
		}
		if c.Name != expected {
			t.Errorf("cluster %q Name = %q, want %q", c.Context, c.Name, expected)
		}
	}
}

// The Unreachable sort had the same non-total comparator the summary sort had,
// and the same fix: order on the row identity, not the context.
func TestSweepSortsUnreachableByIdentityNotContext(t *testing.T) {
	targets := []Target{
		{Name: "edge-d", Context: "default"},
		{Name: "edge-b", Context: "default"},
		{Name: "edge-c", Context: "default"},
		{Name: "edge-a", Context: "default"},
	}
	rep := Sweep(context.Background(), targets,
		Options{FailOn: findings.Critical, Workers: 4, ClusterTimeout: 30 * time.Second})

	var got []string
	for _, u := range rep.Unreachable {
		got = append(got, u.Name)
	}
	want := []string{"edge-a", "edge-b", "edge-c", "edge-d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Unreachable order = %v, want %v", got, want)
	}
	for _, u := range rep.Unreachable {
		if u.Context != "default" || u.Reason != ReasonUnreachable {
			t.Errorf("unreachable row = %+v, want context default and the fixed reason", u)
		}
	}
}

// What a cluster is CALLED must not change what it is JUDGED to be. The same
// fake clients wrapped the way a kubeconfig sweep wraps them and the way a
// fleet file wraps them must produce the same verdict, the same exit code and
// the same per-row counts. This is the test that pins "a sweep and a
// single-cluster gate can never disagree about the same cluster" across a new
// selection source.
func TestSelectionSourceChangesNoVerdict(t *testing.T) {
	opts := Options{FailOn: findings.Critical, Workers: 2, ClusterTimeout: 30 * time.Second}

	crashing := fake.NewSimpleClientset(crashingPod("alpha"))
	healthy := healthyClient()

	fromKubeconfig := Sweep(context.Background(), []Target{
		{Name: "edge-a", Client: crashing},
		{Name: "edge-b", Client: healthy},
	}, opts)

	fromFile := Sweep(context.Background(), []Target{
		{Name: "edge-a", Context: "default", Client: crashing},
		{Name: "edge-b", Context: "default", Client: healthy},
	}, opts)

	if fromKubeconfig.Verdict != fromFile.Verdict || fromKubeconfig.Code != fromFile.Code {
		t.Fatalf("verdict = %q/%d from a kubeconfig and %q/%d from a file, want identical",
			fromKubeconfig.Verdict, fromKubeconfig.Code, fromFile.Verdict, fromFile.Code)
	}
	if len(fromKubeconfig.Clusters) != len(fromFile.Clusters) {
		t.Fatalf("row counts differ: %d and %d", len(fromKubeconfig.Clusters), len(fromFile.Clusters))
	}
	for i := range fromKubeconfig.Clusters {
		a, b := fromKubeconfig.Clusters[i], fromFile.Clusters[i]
		if a.Verdict != b.Verdict || a.Critical != b.Critical || a.Warning != b.Warning ||
			a.Info != b.Info || a.Blindspots != b.Blindspots {
			t.Errorf("row %d differs beyond its name:\n %+v\n %+v", i, a, b)
		}
	}
}
```

Both files already import everything these tests need: `summarize_test.go` has `reflect`, `strings`, `testing`, `findings` and `gate`; `fleet_test.go` has `context`, `reflect`, `strings`, `testing`, `time`, `fake` and `findings`. Add no imports.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/fleet/
```

Expected: FAIL to compile — `undefined: identity`, `ev.id undefined`, `too many arguments in call to summarize`, `unknown field Context in struct literal of type Target`.

- [ ] **Step 3: Add `Context` to `Target`**

In `internal/fleet/fleet.go`, replace the `Target` declaration:

```go
// Target is one cluster to sweep. The caller builds the client, because
// building it needs a kubeconfig path and a kubeconfig path is a credential
// this package must never hold.
type Target struct {
	// Name is the row identity: what the report calls this cluster. For a
	// kubeconfig sweep it is the context name.
	Name string

	// Context is the kubeconfig context this cluster was reached through.
	// Empty means it is the same as Name — a caller that did not distinguish
	// the two has only one identity to report.
	Context string

	Client kubernetes.Interface
}
```

- [ ] **Step 4: Add `Name` to `Unreachable` and `ClusterSummary`**

In `internal/fleet/fleet.go`, `Unreachable` becomes:

```go
type Unreachable struct {
	// Name is the row identity when the selection source gave one that differs
	// from the context. Absent otherwise, so a sweep selected from a kubeconfig
	// encodes no name key and its document stays byte-identical to v1.10.0's.
	//
	// An unreachable per-cluster-kubeconfig entry would otherwise render as
	// "default", which names nothing.
	Name string `json:"name,omitempty"`

	Context string `json:"context"`
	...
}
```

Keep the existing `Reason` field and its comment exactly as they are.

`ClusterSummary` is declared in `internal/fleet/fleet.go` too, immediately above `Unreachable`. It becomes:

```go
type ClusterSummary struct {
	// Name is the row identity when the selection source gave one that differs
	// from the context. Absent otherwise. Context always holds a real
	// kubeconfig context name, as it has since v1.7.0, so a consumer piping it
	// into `kubectl --context` keeps working.
	Name string `json:"name,omitempty"`

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
```

Both types stay in `fleet.go`. Do not move either one into `summarize.go`, and keep every field below `Name` — including `TopIssues` and its comment — exactly as it is today. The only change is the added field.

- [ ] **Step 5: Add `identity`, and take the resolved pair in `summarize`**

In `internal/fleet/summarize.go`, add above `summarize`:

```go
// identity is what the report calls a cluster: the operator's name when the
// selection source gave one, the kubeconfig context otherwise. It is the single
// definition — the sorts, the renderer and the evidence all go through it.
func identity(name, context string) string {
	if name != "" {
		return name
	}
	return context
}
```

Change `summarize`'s signature and its first two statements. Everything below `counts := map[string]int{}` is unchanged:

```go
func summarize(name, context string, v gate.Verdict) (ClusterSummary, clusterEvidence) {
	s := ClusterSummary{
		Name:       name,
		Context:    context,
		Verdict:    v.Verdict,
		Blindspots: len(v.Inconclusive),
	}
	ev := clusterEvidence{
		id:         identity(name, context),
		issues:     map[string]bool{},
		blindspots: map[string]bool{},
	}
```

Add one paragraph to `summarize`'s doc comment:

```go
// It copies the name and context it is handed rather than resolving them. Sweep
// resolves the pair once, before the branch that chooses between a summary and
// an unreachable row, so the rule lives in one place instead of two.
```

- [ ] **Step 6: Rename `clusterEvidence.context` to `.id`**

In `internal/fleet/correlate.go`, the struct field and both reads:

```go
type clusterEvidence struct {
	// id is the row identity — the operator's name when the selection source
	// gave one, the kubeconfig context otherwise. The shared-signal section
	// must name what the table names, or an operator cannot cross-reference
	// the two.
	id         string
	issues     map[string]bool
	blindspots map[string]bool
}
```

and inside `correlate`:

```go
	for _, e := range evidence {
		for signal := range e.issues {
			issues[signal] = append(issues[signal], e.id)
		}
		for signal := range e.blindspots {
			blindspots[signal] = append(blindspots[signal], e.id)
		}
	}
```

`gather`'s local variable is called `contexts`; rename it to `ids` and update its doc comment's "context names are sorted here" to "identities are sorted here". `correlate`'s own doc comment says "a Shared holds a context name" — change that phrase to "a Shared holds a row identity". No other logic in this file changes.

- [ ] **Step 7: Resolve the identity once in `Sweep`**

In `internal/fleet/fleet.go`, replace the loop over `results`:

```go
	evidence := make([]clusterEvidence, 0, len(targets))
	for i, r := range results {
		// Resolve the pair once, here, rather than in each branch below: a
		// caller that gave only a Name has one identity to report, and a name
		// equal to its context says nothing the context does not — so it is
		// blanked and omitempty drops the key.
		name, ctx := targets[i].Name, targets[i].Context
		if ctx == "" {
			ctx = name
		}
		if name == ctx {
			name = ""
		}

		if r.err != nil {
			rep.Unreachable = append(rep.Unreachable, Unreachable{
				Name:    name,
				Context: ctx,
				Reason:  reasonFor(r.err),
			})
			continue
		}
		summary, ev := summarize(name, ctx, r.verdict)
		rep.Clusters = append(rep.Clusters, summary)
		evidence = append(evidence, ev)
	}
```

- [ ] **Step 8: Fix both sorts**

In `internal/fleet/fleet.go`, the `Unreachable` sort:

```go
	sort.Slice(rep.Unreachable, func(i, j int) bool {
		a, b := rep.Unreachable[i], rep.Unreachable[j]
		return identity(a.Name, a.Context) < identity(b.Name, b.Context)
	})
```

In `internal/fleet/summarize.go`, `sortSummaries`'s comment and last tiebreak:

```go
// sortSummaries puts the worst cluster first, in place. The last tiebreak is
// the row identity, which is unique because fleetfile.Load refuses two entries
// that resolve to the same name — so the order is total and two runs over the
// same fleet render identical bytes.
//
// It was the context name until the fleet file arrived, justified by the
// context being unique within a kubeconfig. That premise dies the moment a
// sweep spans several kubeconfigs: four per-cluster k3s kubeconfigs are four
// clusters whose context is "default", which makes the comparator non-total,
// and sort.Slice is not stable.
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
		return identity(a.Name, a.Context) < identity(b.Name, b.Context)
	})
}
```

- [ ] **Step 9: Run the tests to verify they pass**

```bash
go test -p 2 ./internal/fleet/... ./internal/fleetfile/... ./internal/cli/...
gofmt -l internal/
go vet ./internal/fleet/...
git diff --stat main -- go.mod go.sum     # must print nothing
git diff --stat main -- internal/report/testdata/golden-scan.txt   # must print nothing
```

Expected: `ok` for all three packages. **`./internal/schemadoc` is expected to fail from here until Task 5 — do not run it, and do not run `-update`.**

- [ ] **Step 10: Commit**

```bash
git add internal/fleet/fleet.go internal/fleet/summarize.go internal/fleet/correlate.go \
        internal/fleet/fleet_test.go internal/fleet/summarize_test.go
git commit -s -m "fleet: give a row an identity distinct from its context

Target gains Context beside Name; ClusterSummary and Unreachable each
gain an optional Name, written only when it says something the context
does not. A sweep selected from a kubeconfig encodes no name key, so its
document is byte-identical to v1.10.0's.

Sweep resolves the pair once, before the branch, so the rule has one
definition rather than one per branch. An unexported identity() is the
single answer to what the report calls a cluster.

This fixes a determinism defect it also exposes. Both sorts broke ties on
the context name under a comment justifying it as unique within a
kubeconfig. Four per-cluster kubeconfigs are four clusters whose context
is 'default', which makes the comparator non-total — and sort.Slice is
not stable, so two runs over the same fleet could render different bytes.
Both now break on the row identity, unique because a duplicate is a load
error.

The fleet schema now differs from the published one; the bump is its own
commit."
```

---

### Task 3: `internal/fleet/render.go` — the renderer names the identity

**Files:**
- Modify: `internal/fleet/render.go`
- Test: `internal/fleet/render_test.go`

**Interfaces:**
- Consumes from Task 2: `identity(name, context string) string`, `ClusterSummary.Name`, `Unreachable.Name`.
- Produces: no new exported surface. `row.context` becomes `row.id`; `namedClusters(contexts []string)` becomes `namedClusters(ids []string)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/fleet/render_test.go`:

```go
// The table names what the operator called each cluster, and the SHARED section
// names the same thing — an operator who cannot cross-reference the two has two
// reports rather than one. Compared byte for byte, because column padding is
// the whole point of this renderer.
func TestRenderTextNamesTheRowIdentity(t *testing.T) {
	rep := Report{
		SchemaVersion: "1.2",
		Verdict:       "fail",
		Code:          1,
		Clusters: []ClusterSummary{
			{Name: "edge-a", Context: "default", Verdict: "fail", Critical: 2,
				TopIssues: []string{"ImagePullBackOff", "OOMKilled"}},
			{Name: "edge-b", Context: "default", Verdict: "fail", Critical: 1,
				TopIssues: []string{"OOMKilled"}},
			{Context: "prod-eu", Verdict: "pass"},
			{Context: "prod-us", Verdict: "pass"},
		},
		Unreachable: []Unreachable{},
		Shared: []Shared{
			{Signal: "OOMKilled", Source: SourceIssue, Clusters: []string{"edge-a", "edge-b"}},
		},
	}

	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}

	want := strings.Join([]string{
		"FLEET  4 clusters, 2 failing, 0 unreachable",
		"",
		"CLUSTER  VERDICT       CRIT  WARN  INFO  TOP ISSUES",
		"edge-a   fail             2     0     0  ImagePullBackOff, OOMKilled",
		"edge-b   fail             1     0     0  OOMKilled",
		"prod-eu  pass             0     0     0",
		"prod-us  pass             0     0     0",
		"",
		"SHARED ISSUES  in 2 or more of 4 judged clusters",
		"",
		"  2/4  OOMKilled  edge-a, edge-b",
		"",
		"verdict: fail (exit 1)",
		"",
	}, "\n")

	if got := buf.String(); got != want {
		t.Errorf("RenderText() =\n%q\nwant\n%q", got, want)
	}
}

// An unreachable row is named by its identity too. Rendering "default" for a
// per-cluster kubeconfig would name nothing at all.
func TestRenderTextNamesAnUnreachableClusterByItsIdentity(t *testing.T) {
	var buf bytes.Buffer
	err := RenderText(&buf, Report{
		SchemaVersion: "1.2",
		Verdict:       "inconclusive",
		Code:          2,
		Clusters:      []ClusterSummary{{Context: "prod-eu", Verdict: "pass"}},
		Unreachable:   []Unreachable{{Name: "edge-a", Context: "default", Reason: ReasonUnreachable}},
	})
	if err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	if !strings.Contains(buf.String(), "edge-a   unreachable") {
		t.Errorf("rendered =\n%s\nwant an unreachable row named edge-a", buf.String())
	}
	if strings.Contains(buf.String(), "default") {
		t.Errorf("rendered =\n%s\nwant no bare context where an identity was given", buf.String())
	}
}

// A sweep selected from a kubeconfig must encode no name key anywhere, so a
// consumer written against fleet 1.0 or 1.1 sees the document it expects.
func TestRenderJSONOmitsNameWhenNoNameDiffers(t *testing.T) {
	var buf bytes.Buffer
	err := RenderJSON(&buf, Report{
		SchemaVersion: "1.2",
		Verdict:       "inconclusive",
		Code:          2,
		Clusters:      []ClusterSummary{{Context: "prod-eu", Verdict: "pass"}},
		Unreachable:   []Unreachable{{Context: "prod-us", Reason: ReasonUnreachable}},
	})
	if err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}
	if strings.Contains(buf.String(), `"name"`) {
		t.Errorf("document =\n%s\nwant no name key when no name differs from its context", buf.String())
	}
}
```

`render_test.go` already imports `bytes`, `strings` and `testing`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/fleet/ -run 'TestRenderText|TestRenderJSON'
```

Expected: FAIL — the table renders `default` twice instead of `edge-a`/`edge-b`, so the byte comparison fails and the unreachable test finds `default`.

- [ ] **Step 3: Rename `row.context` and build rows from the identity**

In `internal/fleet/render.go`:

```go
// row is one rendered line. The count cells are strings rather than ints so an
// unreachable cluster can leave them blank — it has no counts, and printing 0
// would claim kubeagent looked and found nothing.
type row struct {
	id, verdict, crit, warn, info, detail string
}
```

`RenderText`'s two row-build loops and the width computation:

```go
	rows := make([]row, 0, len(rep.Clusters)+len(rep.Unreachable))
	for _, u := range rep.Unreachable {
		rows = append(rows, row{id: identity(u.Name, u.Context), verdict: "unreachable", detail: u.Reason})
	}
	for _, c := range rep.Clusters {
		rows = append(rows, row{
			id:      identity(c.Name, c.Context),
			verdict: c.Verdict,
			crit:    fmt.Sprint(c.Critical),
			warn:    fmt.Sprint(c.Warning),
			info:    fmt.Sprint(c.Info),
			detail:  detailOf(c),
		})
	}

	width := len("CLUSTER")
	for _, r := range rows {
		if len(r.id) > width {
			width = len(r.id)
		}
	}
```

The header row and `writeRow`:

```go
	if err := writeRow(w, width, row{id: "CLUSTER", verdict: "VERDICT",
		crit: "CRIT", warn: "WARN", info: "INFO", detail: "TOP ISSUES"}); err != nil {
		return err
	}
```

```go
func writeRow(w io.Writer, width int, r row) error {
	line := fmt.Sprintf("%-*s  %-*s  %4s  %4s  %4s  %s",
		width, r.id, verdictWidth, r.verdict, r.crit, r.warn, r.info, r.detail)
	_, err := fmt.Fprintln(w, strings.TrimRight(line, " "))
	return err
}
```

- [ ] **Step 4: Rename `namedClusters`'s parameter**

```go
// namedClusters spells out at most maxNamedClusters row identities and then
// says how many it left out.
func namedClusters(ids []string) string {
	if len(ids) <= maxNamedClusters {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s, +%d more",
		strings.Join(ids[:maxNamedClusters], ", "), len(ids)-maxNamedClusters)
}
```

`maxNamedClusters`'s own comment says "the context names a shared-signal line spells out"; change that phrase to "the row identities a shared-signal line spells out". Nothing else in `render.go` changes — the format strings, the widths and the section logic are all untouched.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test -p 2 ./internal/fleet/... ./internal/fleetfile/... ./internal/cli/...
gofmt -l internal/
go vet ./internal/fleet/...
git diff --stat main -- go.mod go.sum     # must print nothing
```

Expected: `ok`. `./internal/schemadoc` still fails; that is still expected.

- [ ] **Step 6: Commit**

```bash
git add internal/fleet/render.go internal/fleet/render_test.go
git commit -s -m "fleet: render the row identity, not the context

The table and the SHARED sections now name what the operator called each
cluster. Rendering the context in one and the identity in the other would
give an operator two reports they cannot cross-reference.

Three unexported names said 'context' while carrying something that may
not be one, so each is renamed: row.context, and namedClusters's
parameter. No format string, width or section rule moves — the byte-exact
render test is the proof."
```

---

### Task 4: `internal/cli` — the flag, the file read, the selection

**Files:**
- Modify: `internal/cli/fleet.go`
- Test: `internal/cli/fleet_test.go`

**Interfaces:**
- Consumes from Task 1: `fleetfile.Entry{Name, Kubeconfig, Context}`, `fleetfile.Load(data []byte) ([]Entry, error)`.
- Consumes from Task 2: `fleet.Target{Name, Context, Client}`.
- Consumes, already in the tree: `namePath(label, path string) string` (`internal/cli/policy.go`), `cluster.NewClient(kubeconfigPath, contextName string) (*kubernetes.Clientset, error)`, `glob.Match(pattern, s string) bool`, `exitError{code, msg}`, `gate.CodeUsage` (= 4).
- Produces:
  - `fleetOptions` gains `fleetFile string`
  - `func readFleetFile(path string) ([]fleetfile.Entry, error)`
  - `func selectEntries(entries []fleetfile.Entry, match string) ([]fleetfile.Entry, error)`
  - `func buildFleetFileTargets(fallbackKubeconfig string, entries []fleetfile.Entry) ([]fleet.Target, error)`

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/fleet_test.go`:

```go
// selectEntries preserves FILE ORDER — unlike selectContexts, which sorts,
// because a kubeconfig's context list has no order an operator chose. A fleet
// file does: the order the operator wrote. Sweep sorts the report anyway, so
// this only decides which clusters go to which worker.
func TestSelectEntries(t *testing.T) {
	all := []fleetfile.Entry{
		{Name: "prod-eu", Context: "prod-eu"},
		{Name: "edge-a", Context: "default"},
		{Name: "prod-us", Context: "prod-us"},
		{Name: "edge-b", Context: "default"},
	}

	tests := []struct {
		name    string
		match   string
		want    []string // resolved names, in the order selectEntries returns them
		wantErr string
	}{
		{
			name: "no match takes every entry in file order",
			want: []string{"prod-eu", "edge-a", "prod-us", "edge-b"},
		},
		{
			name:  "a match takes the subset in file order",
			match: "prod-*",
			want:  []string{"prod-eu", "prod-us"},
		},
		{
			name:  "the match runs against the row identity, not the context",
			match: "edge-*",
			want:  []string{"edge-a", "edge-b"},
		},
		{
			name:    "a match selecting nothing is refused",
			match:   "staging-*",
			wantErr: "no cluster matches --match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectEntries(all, tt.match)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("selectEntries() error = nil, want one containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("selectEntries() error = %q, want it to contain %q", err, tt.wantErr)
				}
				if code := exitCodeFor(err); code != 4 {
					t.Errorf("exit code = %d, want 4 — bad input, found before any cluster was touched", code)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectEntries() error = %v, want none", err)
			}
			var names []string
			for _, e := range got {
				names = append(names, e.Name)
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Errorf("selectEntries() = %v, want %v", names, tt.want)
			}
		})
	}
}

// The whole flag-conflict matrix from the spec. --fleet-file names the
// clusters, so a flag that also names them is refused; a flag that says how to
// reach them or which subset to take is not.
func TestValidateFleetOptionsFleetFileConflicts(t *testing.T) {
	base := func() fleetOptions {
		return fleetOptions{
			fleetFile:      "/fleet/clusters.yaml",
			failOn:         "critical",
			output:         "text",
			clusterTimeout: 60 * time.Second,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*fleetOptions)
		wantErr string
	}{
		{
			name:    "--fleet-file and --context are refused",
			mutate:  func(o *fleetOptions) { o.contexts = []string{"prod-eu"} },
			wantErr: "--fleet-file and --context cannot be combined",
		},
		{
			name:    "--fleet-file and --all-contexts are refused",
			mutate:  func(o *fleetOptions) { o.allContexts = true },
			wantErr: "--fleet-file and --all-contexts cannot be combined",
		},
		{
			name:   "--fleet-file and --kubeconfig are allowed",
			mutate: func(o *fleetOptions) { o.kubeconfig = "/fleet/fallback.kubeconfig" },
		},
		{
			name:   "--fleet-file and --match are allowed",
			mutate: func(o *fleetOptions) { o.match = "prod-*" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := base()
			tt.mutate(&o)
			err := validateFleetOptions(o)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateFleetOptions() error = %v, want none", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateFleetOptions() error = nil, want one containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateFleetOptions() error = %q, want it to contain %q", err, tt.wantErr)
			}
			if code := exitCodeFor(err); code != 4 {
				t.Errorf("exit code = %d, want 4", code)
			}
		})
	}
}

// --match filters something a sweep would otherwise take all of. There are now
// two such sources, and the refusal has to name both or an operator reading it
// learns only half the answer.
func TestSelectContextsMatchNeedsAllContextsOrAFleetFile(t *testing.T) {
	_, err := selectContexts([]cluster.ContextInfo{{Name: "prod-eu"}}, nil, false, "prod-*")
	if err == nil {
		t.Fatal("selectContexts() error = nil, want one")
	}
	for _, want := range []string{"--all-contexts", "--fleet-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err, want)
		}
	}
}

// --fleet-file accepts the single-dash long-flag spelling, like every other
// flag: Normalize is what keeps command lines written against v0.72 working.
func TestParseFleetFlagsAcceptsTheFleetFileFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--fleet-file", "/fleet/clusters.yaml"},
		{"-fleet-file", "/fleet/clusters.yaml"},
	} {
		o, err := parseFleetFlags(args)
		if err != nil {
			t.Fatalf("parseFleetFlags(%v) error = %v", args, err)
		}
		if o.fleetFile != "/fleet/clusters.yaml" {
			t.Errorf("parseFleetFlags(%v) fleetFile = %q, want /fleet/clusters.yaml", args, o.fleetFile)
		}
	}
}

// An unreadable file is bad input, found before any cluster was touched, and
// the path may be named because this reaches stderr and nowhere else.
func TestReadFleetFileNamesTheFlagAndThePathOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	_, err := readFleetFile(path)
	if err == nil {
		t.Fatal("readFleetFile() error = nil, want one")
	}
	if !strings.Contains(err.Error(), "--fleet-file") || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name both --fleet-file and the path", err)
	}
	if code := exitCodeFor(err); code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
}

// A file that reads but does not load is the same class of failure.
func TestReadFleetFileReportsALoadFailureAtExitFour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.yaml")
	if err := os.WriteFile(path, []byte("- name: edge-a\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	_, err := readFleetFile(path)
	if err == nil {
		t.Fatal("readFleetFile() error = nil, want one")
	}
	if !strings.Contains(err.Error(), "entry 1 has no context") {
		t.Errorf("error = %q, want the load failure", err)
	}
	if code := exitCodeFor(err); code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
}

// buildFleetFileTargets uses the entry's own kubeconfig when it names one and
// the fallback otherwise, and it carries the identity pair through to the
// target. cluster.NewClient does no network I/O, so this needs no cluster.
func TestBuildFleetFileTargets(t *testing.T) {
	fallback := fleetFileKubeconfigPath(t, "prod-eu")
	perCluster := fleetFileKubeconfigPath(t, "default")

	targets, err := buildFleetFileTargets(fallback, []fleetfile.Entry{
		{Name: "prod-eu", Context: "prod-eu"},
		{Name: "edge-a", Kubeconfig: perCluster, Context: "default"},
	})
	if err != nil {
		t.Fatalf("buildFleetFileTargets() error = %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	for i, want := range []fleet.Target{
		{Name: "prod-eu", Context: "prod-eu"},
		{Name: "edge-a", Context: "default"},
	} {
		if targets[i].Name != want.Name || targets[i].Context != want.Context {
			t.Errorf("target %d = {Name:%q Context:%q}, want {%q %q}",
				i, targets[i].Name, targets[i].Context, want.Name, want.Context)
		}
		if targets[i].Client == nil {
			t.Errorf("target %d has no client", i)
		}
	}
}

// A context the kubeconfig does not define is a configuration defect, not a
// reachability event: cluster.NewClient does no network I/O. Fatal at exit 4,
// the same ruling buildFleetTargets makes.
func TestBuildFleetFileTargetsRejectsAnUnknownContext(t *testing.T) {
	path := fleetFileKubeconfigPath(t, "prod-eu")
	_, err := buildFleetFileTargets(path, []fleetfile.Entry{
		{Name: "edge-a", Context: "nonexistent"},
	})
	if err == nil {
		t.Fatal("buildFleetFileTargets() error = nil, want one")
	}
	if code := exitCodeFor(err); code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
}

// fleetFileKubeconfigPath writes a one-context kubeconfig into a temp dir. The
// server is a loopback address on a port nothing listens on: cluster.NewClient
// does no network I/O, so nothing ever connects to it.
func fleetFileKubeconfigPath(t *testing.T, contextName string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := "apiVersion: v1\n" +
		"kind: Config\n" +
		"current-context: " + contextName + "\n" +
		"clusters:\n" +
		"  - name: " + contextName + "\n" +
		"    cluster:\n" +
		"      server: https://127.0.0.1:1\n" +
		"contexts:\n" +
		"  - name: " + contextName + "\n" +
		"    context: {cluster: " + contextName + ", user: " + contextName + "}\n" +
		"users:\n" +
		"  - name: " + contextName + "\n" +
		"    user: {token: <PLACEHOLDER>}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}
```

`fleet_test.go` must import `os`, `path/filepath`, `reflect`, `strings`, `time`, `github.com/imantaba/kubeagent/internal/cluster`, `github.com/imantaba/kubeagent/internal/fleet` and `github.com/imantaba/kubeagent/internal/fleetfile`. Add whichever it does not already have.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cli/ -run 'Fleet'
```

Expected: FAIL to compile — `undefined: selectEntries`, `undefined: readFleetFile`, `undefined: buildFleetFileTargets`, `unknown field fleetFile`.

- [ ] **Step 3: Add the flag**

In `internal/cli/fleet.go`, add to `fleetOptions` after `match`:

```go
	fleetFile      string
```

and register it in `bindFleetFlags`, after the `--match` line:

```go
	cmd.Flags().StringVar(&o.fleetFile, "fleet-file", "", "read the clusters to sweep from a file")
```

Add `"github.com/imantaba/kubeagent/internal/fleetfile"` to the import block.

- [ ] **Step 4: Add the two conflict refusals**

In `validateFleetOptions`, before the existing output-format check:

```go
	// --fleet-file names the clusters. A flag that also names them is refused
	// rather than silently losing to one of them; a flag that says how to reach
	// them (--kubeconfig, the fallback) or which subset to take (--match) is
	// not in conflict at all.
	if o.fleetFile != "" {
		switch {
		case len(o.contexts) > 0:
			return &exitError{code: gate.CodeUsage, msg: "--fleet-file and --context cannot be combined: the file names the clusters to sweep"}
		case o.allContexts:
			return &exitError{code: gate.CodeUsage, msg: "--fleet-file and --all-contexts cannot be combined: the file names the clusters to sweep"}
		}
	}
```

- [ ] **Step 5: Widen the `--match` refusal**

In `selectContexts`, the first switch arm:

```go
	case match != "" && !allContexts:
		return nil, usage("--match needs --all-contexts or --fleet-file: it filters the clusters a sweep would otherwise take all of")
```

`selectContexts` is only reached on the kubeconfig path — `runFleetOpts` branches before it when a fleet file is set — so this arm cannot fire for a fleet-file sweep. It names both sources because an operator reading it needs to know both.

- [ ] **Step 6: Add `readFleetFile`, `selectEntries` and `buildFleetFileTargets`**

Add to `internal/cli/fleet.go`, after `selectContexts`:

```go
// readFleetFile reads and loads the fleet file.
//
// internal/cli owns the read and owns naming the path, on the precedent
// readPolicyFile set for --policy. Every failure is exit 4: bad input,
// discovered before any cluster was touched. The path reaches stderr and
// nowhere else — it never crosses into internal/fleetfile's errors and never
// into internal/fleet at all.
func readFleetFile(path string) ([]fleetfile.Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("%s: %v", namePath("--fleet-file", path), err)}
	}
	entries, err := fleetfile.Load(data)
	if err != nil {
		return nil, &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("%s: %v", namePath("--fleet-file", path), err)}
	}
	return entries, nil
}

// selectEntries filters a fleet file's entries by --match and refuses an empty
// result, the same ruling selectContexts makes and for the same reason: an
// empty sweep reporting "pass" is the worst possible answer, because it looks
// like good news.
//
// It matches the row identity rather than the context, because the identity is
// what the operator wrote and what the report will show. It keeps file order
// rather than sorting: a kubeconfig's context list has no order anyone chose,
// but a fleet file does.
func selectEntries(entries []fleetfile.Entry, match string) ([]fleetfile.Entry, error) {
	if match == "" {
		return entries, nil
	}
	var selected []fleetfile.Entry
	for _, e := range entries {
		if glob.Match(match, e.Name) {
			selected = append(selected, e)
		}
	}
	if len(selected) == 0 {
		return nil, &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("no cluster matches --match %q", match)}
	}
	return selected, nil
}

// buildFleetFileTargets connects to each entry's cluster.
//
// An entry's own kubeconfig wins; --kubeconfig is the fallback for entries that
// name none, and an empty fallback lets client-go take $KUBECONFIG and then the
// default location, exactly as every other command does.
//
// A client that cannot be built is fatal, the same ruling buildFleetTargets
// makes: cluster.NewClient does no network I/O, so a failure here is a
// configuration defect and never a reachability event — it must not become a
// third Unreachable.Reason. This is the one place a kubeconfig path may be
// named, on stderr, and it is why no path ever reaches internal/fleet.
func buildFleetFileTargets(fallbackKubeconfig string, entries []fleetfile.Entry) ([]fleet.Target, error) {
	targets := make([]fleet.Target, 0, len(entries))
	for _, e := range entries {
		kubeconfig := e.Kubeconfig
		if kubeconfig == "" {
			kubeconfig = fallbackKubeconfig
		}
		client, err := cluster.NewClient(kubeconfig, e.Context)
		if err != nil {
			return nil, &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("connecting to cluster %q: %v", e.Name, err)}
		}
		targets = append(targets, fleet.Target{Name: e.Name, Context: e.Context, Client: client})
	}
	return targets, nil
}
```

- [ ] **Step 7: Branch in `runFleetOpts`**

Replace the block between `level, _ := findings.Parse(o.failOn)` and the `fleet.Sweep` call:

```go
	targets, err := fleetTargets(o)
	if err != nil {
		return err
	}
```

and add, above `runFleetOpts`:

```go
// fleetTargets resolves the selection to connected clusters, from whichever
// source the operator chose. The two sources meet here and nowhere deeper:
// fleet.Sweep takes []Target and never learns where they came from, which is
// what keeps a fleet-file sweep and a kubeconfig sweep the same evaluation.
func fleetTargets(o fleetOptions) ([]fleet.Target, error) {
	if o.fleetFile != "" {
		entries, err := readFleetFile(o.fleetFile)
		if err != nil {
			return nil, err
		}
		selected, err := selectEntries(entries, o.match)
		if err != nil {
			return nil, err
		}
		return buildFleetFileTargets(o.kubeconfig, selected)
	}

	all, err := cluster.Contexts(o.kubeconfig)
	if err != nil {
		return nil, &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
	names, err := selectContexts(all, o.contexts, o.allContexts, o.match)
	if err != nil {
		return nil, err
	}
	return buildFleetTargets(o.kubeconfig, names)
}
```

- [ ] **Step 8: Widen the command's help text**

In `newFleetCommand`, the `Long` string's first sentence currently says "Sweep every selected kubeconfig context in bounded parallel". Replace the whole `Long` with:

```go
		Long: "Sweep every selected cluster in bounded parallel, running the same evaluation\n" +
			"`kubeagent gate` runs against each, and print one row per cluster worst first.\n" +
			"Clusters come from the kubeconfig's contexts, or from a file named by\n" +
			"--fleet-file when a fleet spans several kubeconfigs. Read-only toward every\n" +
			"cluster (get and list only, no write of any kind), and it makes no model call —\n" +
			"two separate promises. The report names clusters and issue kinds — never a node,\n" +
			"namespace, pod or workload.",
```

- [ ] **Step 9: Run the tests to verify they pass**

```bash
go test -p 2 ./internal/fleet/... ./internal/fleetfile/... ./internal/cli/...
gofmt -l internal/
go vet ./internal/cli/...
git diff --stat main -- go.mod go.sum     # must print nothing
```

Expected: `ok`. `./internal/schemadoc` still fails; still expected.

- [ ] **Step 10: Commit**

```bash
git add internal/cli/fleet.go internal/cli/fleet_test.go
git commit -s -m "cli: select a fleet from a file

--fleet-file reads the clusters to sweep from a YAML list instead of a
kubeconfig's contexts, so a fleet can span several kubeconfigs and each
row can carry a name the operator chose. Selection comes from the file;
credentials keep coming from the kubeconfigs it points at.

--context and --all-contexts are refused beside it, because both also
name the clusters. --kubeconfig is not: it becomes the fallback for
entries that name none. --match is not either: it filters the row
identity. Its own refusal now names both sources it can filter.

A file that will not read or will not load is exit 4, bad input found
before any cluster was touched, and it is the one place the path may be
named — on stderr, never in the report."
```

---

### Task 5: the schema bump and its one regeneration

**Files:**
- Modify: `internal/jsonschema/jsonschema.go:38`
- Modify: `internal/fleet/render_test.go:15` (a stale fixture version)
- Regenerate: `website/docs/schemas/fleet-v1.json`

**Interfaces:**
- Consumes from Task 2: `ClusterSummary.Name`, `Unreachable.Name`, both `json:"name,omitempty"`.
- Produces: `jsonschema.FleetVersion == "1.2"`.

**This is the only task that may run a test with `-update`, exactly once, on `TestSchemaDrift`.**

- [ ] **Step 1: Confirm the drift the previous tasks created**

```bash
go test ./internal/schemadoc -run TestSchemaDrift
```

Expected: FAIL, naming the change **additive** (two added optional properties) with no version bump. Read the failure and confirm it says additive, not breaking. If it says breaking, stop and report — something in Tasks 2-4 changed the contract in a way this plan did not intend.

- [ ] **Step 2: Bump the version**

In `internal/jsonschema/jsonschema.go`, line 38's constant and its comment:

```go
	// FleetVersion is `kubeagent fleet --output json`. 1.1 added the optional
	// `shared` array; 1.2 added the optional `name` on a cluster summary and on
	// an unreachable cluster, written only when the row identity differs from
	// the kubeconfig context. Both bumps are additive: every property is
	// omitempty and absent from `required`, so a document produced without
	// them still validates against the older schema.
	FleetVersion = "1.2"
```

Match the surrounding comment style — read the four constants around it first and keep the file's own voice. Do not touch `ScanVersion`, `GateVersion`, `RBACVersion`, `WatchVersion` or `BaselineVersion`.

One stale literal goes with it: `internal/fleet/render_test.go:15`'s `sampleReport()` sets `SchemaVersion: "1.1"`. Nothing asserts that value — it is a fixture that flows through `RenderJSON` — but a fixture claiming a version the package no longer emits is a comment that lies in test clothing. Change it to `"1.2"`.

- [ ] **Step 3: Regenerate, exactly once**

```bash
go test ./internal/schemadoc -run TestSchemaDrift -update
```

- [ ] **Step 4: Verify the regeneration is what was intended**

```bash
go test -p 2 ./...
git diff website/docs/schemas/fleet-v1.json
git status --short
```

Expected: every package `ok`, including `./internal/schemadoc`. The schema diff must show **only**: the `schemaVersion` value moving to 1.2, and a `name` property added under `fleet.ClusterSummary` and `fleet.Unreachable`. Neither may appear in either `required` list. **No other schema file may have changed** — if `git status` shows a second file under `website/docs/schemas/`, stop and report.

- [ ] **Step 5: Commit**

```bash
git add internal/jsonschema/jsonschema.go website/docs/schemas/fleet-v1.json internal/fleet/render_test.go
git commit -s -m "schema: fleet 1.1 -> 1.2, the optional row name

Two added optional properties, clusters[].name and unreachable[].name,
both omitempty and both absent from required. Additive, so MINOR: a sweep
selected from a kubeconfig encodes neither key, and a consumer written
against fleet 1.0 or 1.1 is unaffected.

scan stays 1.2, gate stays 1.1, and the other five do not move."
```

---

### Task 6: documentation

**Files:**
- Modify: `website/docs/features/fleet.md`
- Modify: `website/docs/features/json-schema.md`
- Modify: `CHANGELOG.md`
- Modify: `CLAUDE.md`
- Modify: `website/docs/roadmap.md`
- Modify: `internal/fleet/fleet.go` (package comment only)

**Interfaces:** consumes everything above; produces nothing code depends on.

- [ ] **Step 1: `website/docs/features/fleet.md`**

Read the whole page first and match its voice and heading depth. Add:

1. **A "Selecting from a file" section**, after whatever section documents `--context`/`--all-contexts`. It carries the example file and the rendered output **verbatim from the spec §2**:

```yaml
# clusters.yaml
- context: prod-eu
- context: prod-us
- name: edge-a
  kubeconfig: /path/to/edge-a.kubeconfig
  context: default
- name: edge-b
  kubeconfig: /path/to/edge-b.kubeconfig
  context: default
```

```text
$ kubeagent fleet --fleet-file clusters.yaml

FLEET  4 clusters, 2 failing, 0 unreachable

CLUSTER  VERDICT       CRIT  WARN  INFO  TOP ISSUES
edge-a   fail             2     0     0  ImagePullBackOff, OOMKilled
edge-b   fail             1     0     0  OOMKilled
prod-eu  pass             0     0     0
prod-us  pass             0     0     0

SHARED ISSUES  in 2 or more of 4 judged clusters

  2/4  OOMKilled  edge-a, edge-b

verdict: fail (exit 1)
```

2. **The format table**:

| Field | Required | Meaning |
|---|---|---|
| `context` | yes | the kubeconfig context to reach this cluster through |
| `kubeconfig` | no | path to the kubeconfig; falls back to `--kubeconfig`, then `$KUBECONFIG`, then the default location |
| `name` | no | the row identity; defaults to `context` |

with a sentence on why `context` is required: an entry naming no context would take its kubeconfig's current-context, which can change under the operator between runs, and a checked-in fleet file has to be reproducible.

3. **The flag matrix**, verbatim from spec §7:

| Combination | Outcome |
|---|---|
| `--fleet-file` + `--context` | refused, exit 4 — the file names the clusters |
| `--fleet-file` + `--all-contexts` | refused, exit 4 — same |
| `--fleet-file` + `--kubeconfig` | allowed; `--kubeconfig` becomes the fallback for entries that set none |
| `--fleet-file` + `--match` | allowed; matches the row identity |

4. **The credential statement**, in prose, from spec §10: selection comes from the file and **credentials still come from the kubeconfigs it points at**. No server URL, no bearer token and no CA data can enter a kubeagent value, and structurally rather than by rule — an entry has three string fields decoded strictly, so `server:`, `token:` and `certificate-authority-data:` are load errors. The `--fleet-file` path and an entry's `kubeconfig` path reach stderr only.

5. **Update the page's existing "names contexts" language** where it describes what the report carries: it now names an operator-chosen name as well as a context name. Do not weaken the surrounding promise — it still names no node, namespace, pod or workload.

Keep the page's existing read-only / no-LLM-call phrasing as two separate promises. Do not merge them, and do not add any sentence connecting a selection source to `--explain`.

- [ ] **Step 2: `website/docs/features/json-schema.md`**

Find the table or list of surfaces and their versions and change fleet's `1.1` to `1.2`, with the same one-line "what moved" note the other entries carry: two optional `name` properties, additive.

- [ ] **Step 3: `CHANGELOG.md`**

Under the (currently empty) `## [Unreleased]`, add an `### Added` section in Keep-a-Changelog style, matching the voice of the 1.10.0 entry above it:

```markdown
### Added

- `kubeagent fleet --fleet-file <path>` reads the clusters to sweep from a YAML
  file instead of a kubeconfig's contexts, so a fleet can span several
  kubeconfigs and each row can carry a name the operator chose. An entry names a
  `context`, optionally a `kubeconfig` path and optionally a `name`; selection
  comes from the file, and credentials still come from the kubeconfigs it points
  at. The format cannot express a credential — an entry has three string fields
  decoded strictly, so `server:`, `token:` and `certificate-authority-data:` are
  load errors. `--context` and `--all-contexts` are refused beside it;
  `--kubeconfig` becomes the fallback and `--match` filters the row identity.
  `fleet` moves to schema version 1.2 (added the optional `name` on a cluster
  summary and on an unreachable cluster, both `omitempty`), so a sweep selected
  from a kubeconfig encodes neither key and its document is unchanged.

### Fixed

- `kubeagent fleet` could render two runs over the same fleet in different
  orders when several clusters shared a kubeconfig context name. Both sorts
  broke ties on the context name, which is not unique across kubeconfigs, and
  `sort.Slice` is not stable. Both now break on the row identity, which a
  duplicate-name load error keeps unique.
```

- [ ] **Step 4: `CLAUDE.md`**

Two edits, both surgical.

**(a)** In the Invariants section, the paragraph that begins "`internal/fleet` (the `kubeagent fleet` sweep) is a ninth case" — append to it:

```
`internal/fleetfile` (the `--fleet-file` decoder) is a tenth case and takes
`internal/fleet`'s wall plus one `internal/fleet` cannot carry: it must never
import `internal/remediate` or `internal/explain`, and it must never import
`k8s.io/client-go` or `internal/cluster` either, which makes "holds no client"
structural rather than stated in a package that holds kubeconfig paths.
`internal/fleetfile/imports_test.go` enforces both halves. It is not in the
stdlib-only class — it imports `sigs.k8s.io/yaml`, already a direct dependency,
and `internal/safetext`. It is pure: no client, no context, no I/O beyond the
bytes it is handed. The file it decodes names clusters and cannot carry a
credential: an `Entry` has three string fields decoded with
`yaml.UnmarshalStrict`, so `server:`, `token:` and `certificate-authority-data:`
are load errors rather than ignored keys.
```

**(b)** In the Roadmap section, extend the post-1.0 fleet-scale bullet. Record slice 3 **without a `(vX.Y.Z)` parenthetical** — that parenthetical is added exclusively by the `release: vX.Y.Z` commit, never a docs commit. State that fleet-scale is now complete, and update the closing "remaining post-1.0 work" sentence so it no longer lists "selection from something other than a kubeconfig": what remains is the rest of the curated-packs item's second half — security and cost packs, and a pack contributed by someone other than kubeagent itself — plus other baseline dimensions.

- [ ] **Step 5: `website/docs/roadmap.md`**

Find the fleet-scale item and mark it complete, in the same form the other completed items use on that page. Say what slice 3 shipped in one sentence.

- [ ] **Step 6: `internal/fleet/fleet.go`'s package comment**

The comment currently says "The report names kubeconfig context names, issue kinds, and the API resource names of refused reads." Widen exactly that sentence and leave every other sentence in the comment alone:

```go
// The report names a row identity — the operator's own name for a cluster when
// the selection source gave one, the kubeconfig context otherwise — plus issue
// kinds and the API resource names of refused reads. It never names a node,
// namespace, pod or workload, and that is structural rather than filtered: ...
```

The "Two separate promises" paragraph, the `Blindspot.Reason` sentence and the closing "Nor does the report ever carry a kubeconfig path" sentence all stay exactly as they are — all three are still true.

- [ ] **Step 7: Verify**

```bash
go build ./... && go test -p 2 ./...
gofmt -l internal/
go vet ./...
git diff --stat main -- go.mod go.sum internal/report/testdata/golden-scan.txt   # must print nothing
(cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml)
cd /home/ubuntu/git/kubeagent
```

Expected: all packages `ok`; `mkdocs` exits 0 with no `WARNING` line naming a page you edited. The red "Material for MkDocs 2.0" banner is cosmetic. **The bash working directory persists between calls and has drifted into `website/` before — `cd` back to the repo root, as the last line does.**

Then grep the whole diff for the things that must not be in it:

```bash
git diff main --  | grep -nE 'ANTHROPIC_API_KEY|BEGIN [A-Z ]*PRIVATE KEY|10\.[0-9]+\.[0-9]+\.[0-9]+|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.' || echo "clean"
```

Expected: `clean`.

- [ ] **Step 8: Commit**

```bash
git add website/docs/features/fleet.md website/docs/features/json-schema.md \
        website/docs/roadmap.md CHANGELOG.md CLAUDE.md internal/fleet/fleet.go
git commit -s -m "docs: fleet selection from a file, and fleet-scale complete

The feature page gains the file format, the flag matrix and the
credential statement: selection comes from the file, credentials keep
coming from the kubeconfigs it points at, and the format cannot express
one. The schema page records fleet 1.2.

CLAUDE.md gains internal/fleetfile and both halves of its wall, and the
post-1.0 fleet-scale bullet records the last slice in that item.

internal/fleet's package comment says what the report now names: a row
identity, which is a context name only when the selection source gave no
other. The promise below it is unchanged — still no node, namespace, pod
or workload."
```

---

## Self-review

**1. Spec coverage.**

| Spec section | Task |
|---|---|
| §2 the flag, the example file, the rendered output | 4 (flag), 3 (byte-exact render), 6 (docs) |
| §3 alternatives rejected | no code — recorded in the spec only |
| §4.1 `internal/fleetfile`, pure, both wall halves | 1 |
| §4.2 `selectEntries`, `buildFleetFileTargets`, the path on stderr | 4 |
| §4.3 `internal/fleet` learns nothing about files | 2 (nothing file-shaped enters the package) |
| §5.1 `Target.Context`, the reflect test | 2 |
| §5.2 `ClusterSummary.Name`, `Unreachable.Name`, written only when it differs | 2 |
| §5.3 the determinism defect, both sorts, the corrected comment | 2 |
| §5.4 the three renames | 2 (`clusterEvidence.id`), 3 (`row.id`, `namedClusters(ids)`) |
| §6 the format table, `context` required | 1 (behaviour), 6 (docs) |
| §6.1 cannot express a credential | 1 (`UnmarshalStrict` + its tests) |
| §6.2 the five load-time refusals | 1 |
| §6.3 sanitizing `name`, not `context`/`kubeconfig` | 1 |
| §7 the CLI surface and the five-row flag matrix | 4 |
| §8 what does not change | 2 (`TestSelectionSourceChangesNoVerdict`), plus the Global Constraints |
| §9 schema 1.1 → 1.2, one regeneration | 5 |
| §10 credentials | Global Constraints, 1, 4, 6 |
| §11 testing | 1, 2, 3, 4, 5 |
| §12 out of scope | nothing built |
| §13 docs to update | 6 |

No gap.

**2. Placeholder scan.** Every code step carries the code to write. Every test step carries the test. The one prose-only step is Task 6, which is documentation: it names each file, the exact content to add, and quotes the two sentences that must be edited rather than rewritten.

**3. Type consistency.** `Entry{Name, Kubeconfig, Context}` is declared in Task 1 and used in Task 4 with those field names. `Load(data []byte) ([]Entry, error)` is called only from `readFleetFile`. `identity(name, context string) string` is declared in Task 2 and called in Tasks 2 (both sorts, `summarize`) and 3 (both row-build sites) with the same argument order. `summarize(name, context string, v gate.Verdict)` has one caller, `Sweep`, updated in the same task. `clusterEvidence.id` is written in `summarize` (Task 2) and read in `correlate` (Task 2) — one task, so the build never breaks between them. `row.id` is written and read only inside `render.go` (Task 3). `fleet.Target{Name, Context, Client}` is built in Task 4 with the field set Task 2 declares and Task 2's reflect test pins.

**Deliberate spec correction, stated in Task 1:** spec §4.1 names three imports for `internal/fleetfile`; four are required, because §6.2's "empty after trimming" check on `context` needs `strings`.
