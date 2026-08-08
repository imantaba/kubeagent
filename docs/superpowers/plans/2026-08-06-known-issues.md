# `kubeagent known-issues` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `internal/knownissues` — a curated, offline knowledge base with one entry per failure kind the deterministic detector set can emit — and `kubeagent known-issues [kind]`, the command that prints it, with machine-checked proof that the registry and the detectors agree.

**Architecture:** The thirteen entries are a Go slice literal in a package that imports nothing from kubeagent and nothing outside the standard library. Three completeness tests live in `internal/diagnose` (which may import the registry freely, so nothing is added to the walled package's own import set): a `go/parser` static walk over the detector sources, a fixture-driven behavioural table that drives all nine detectors to produce all thirteen kinds, and a reverse check that no entry documents a kind no detector emits. The CLI command is modeled on `kubeagent schema [name]` — same argument handling, same error shape, no flags at all.

**Tech Stack:** Go 1.26, standard library only. `github.com/spf13/cobra` for the command (already a dependency). `go/parser` + `go/ast` for the static walk (standard library). No new dependency of any kind.

## Global Constraints

Every task's requirements implicitly include this section.

- **NO NEW DEPENDENCY:** `go.mod` and `go.sum` must not change. The entries are a Go slice literal — no YAML, no JSON, no `embed`, no parser.
- **`internal/knownissues` must import NOTHING from kubeagent and nothing outside the standard library.** It joins `internal/jsonschema`, `internal/dashboard`, `internal/baseline` and `internal/glob`. `internal/knownissues/imports_test.go` enforces both halves. The completeness tests live in `internal/diagnose`, which may import it freely; nothing is added to `internal/knownissues`'s own import set.
- **READ-ONLY toward the cluster** — and this command does not touch a cluster at all. Separately and additionally: **it makes NO LLM CALL.** Never blur the two, and never let help text, docs or a commit message suggest this is related to `--explain`, which is the model path.
- **CREDENTIALS:** no secrets, credentials, private IPs or internal hostnames anywhere — code, tests, fixtures, docs, help text. Every `Checks` line uses `<namespace>`, `<pod>`, `<container>`, `<node>`, `<name>` placeholders. The spec's content rules name the first three; `<node>` and `<name>` are a deliberate extension for the two checks that need them (`kubectl describe node <node>` under `Unschedulable`, and the ConfigMap/ServiceAccount lookups), and the closed set is machine-checked in Task 1. Documentation IPs are RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains RFC 2606 (`example.com`/`.org`/`.net`). **URLs are credentials:** the ONLY permitted host is the project's own, `https://k8sproject.top/`, and it appears only in the `Docs` field — the credential-marker test scans `Summary`, `Detail`, `Causes` and `Checks` and treats any host in them as a failure.
- **`internal/report/testdata/golden-scan.txt` must stay BYTE-IDENTICAL.** `scan`'s rendering does not change. Do NOT regenerate the demo GIF or `website/docs/quickstart.md`.
- **NO JSON document changes:** no new field on `findings.Finding`, no ninth versioned document, and none of the eight `schemaVersion` values moves (`scan` stays 1.2, `gate` stays 1.1). Do NOT run any test with `-update`.
- **Flags are declared per command, never as persistent flags.** This command takes NO flags at all, not even `--kubeconfig`. Every command sets `SilenceErrors` and `SilenceUsage`; validation lives in `RunE`, not in Cobra's `Args`/`MarkFlagsMutuallyExclusive` helpers. Use `Args: cobra.ArbitraryArgs` and `ValidArgsFunction` (completion only) — NOT `ValidArgs`, which would make Cobra validate and reword the error.
- **TDD:** write the failing test first, watch it fail, then implement. Detectors are pure functions unit-tested with fake objects (`internal/diagnose/helpers_test.go` already provides them); no cluster and no fake clientset are needed anywhere in this plan.
- Go lives at `/usr/local/go/bin`. `go test` runs with `-p 2` locally, never `-short`.
- Every commit needs `git commit -s` (DCO enforced on `main`), authored solely by the human — NO `Co-Authored-By` trailer and no AI attribution of any kind, anywhere.

**DANGER — read this before running anything:** NEVER run `./chaos/run.sh` in any form. A run takes ~40 minutes and injects real outages into a live cluster. No task in this plan creates, deletes, or touches any cluster; nothing here needs one. The only commands you run are `go build`, `go test`, `git`, and the mkdocs binary named in Task 4.

---

## File Structure

| File | Responsibility |
|------|----------------|
| `internal/knownissues/knownissues.go` | The `Entry` type, `All`/`Kinds`/`Lookup`. Package doc states both promises. |
| `internal/knownissues/entries.go` | The thirteen entries, as one sorted slice literal. Prose only — no logic. |
| `internal/knownissues/knownissues_test.go` | Unit tests: sorting, exact-match lookup, field population, prose style, the credential-marker test, the placeholder test. |
| `internal/knownissues/imports_test.go` | The stdlib-only wall: no kubeagent import, no non-stdlib import. |
| `internal/diagnose/knownissues_test.go` | The three completeness tests. Lives here so the registry's import set stays empty. |
| `internal/cli/knownissues.go` | `runKnownIssues`, `newKnownIssuesCommand`, the prose wrapper. |
| `internal/cli/knownissues_test.go` | Rendering, argument handling, error text, completion. |
| `internal/cli/root.go` | One-line registration. |
| `website/docs/features/known-issues.md` | The feature page. |
| `website/docs/features/diagnostics.md` | A pointer to the new page. |
| `website/mkdocs.yml` | Nav entry. |
| `CHANGELOG.md`, `CLAUDE.md`, `website/docs/roadmap.md` | Release note, invariants, roadmap. |

Splitting the type/functions from the entries is deliberate: `entries.go` is the file a contributor edits to add prose, and keeping it free of logic is what makes "curated content" reviewable as content.

---

## Task 1: The `internal/knownissues` package

**Files:**
- Create: `internal/knownissues/knownissues.go`
- Create: `internal/knownissues/entries.go`
- Create: `internal/knownissues/knownissues_test.go`
- Create: `internal/knownissues/imports_test.go`

**Interfaces:**
- Consumes: nothing. This package is a leaf with an empty import set beyond the standard library — and in fact `knownissues.go` and `entries.go` import nothing at all.
- Produces, and every later task uses these exact names:
  - `type Entry struct { Kind, Summary, Detail string; Causes, Checks []string; Docs string }`
  - `func All() []Entry` — every entry, sorted by `Kind`
  - `func Kinds() []string` — every `Kind`, sorted
  - `func Lookup(kind string) (Entry, bool)` — exact byte-for-byte match, no normalisation

- [ ] **Step 1: Write the failing tests**

Create `internal/knownissues/knownissues_test.go`:

```go
package knownissues

import (
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// The thirteen kinds DefaultDetectors can emit. This list is duplicated from
// internal/diagnose deliberately: this package imports nothing from kubeagent,
// so the join is proved from the other side, in
// internal/diagnose/knownissues_test.go, where both sets are in scope.
var wantKinds = []string{
	"CrashLoopBackOff",
	"CreateContainerConfigError",
	"ErrImagePull",
	"ImagePullBackOff",
	"Init:CrashLoopBackOff",
	"Init:ErrImagePull",
	"Init:ImagePullBackOff",
	"Init:OOMKilled",
	"OOMKilled",
	"ProbeFailure",
	"RestartLoop",
	"Unschedulable",
	"VolumeAttachError",
}

func TestKindsAreTheThirteen(t *testing.T) {
	got := Kinds()
	if len(got) != len(wantKinds) {
		t.Fatalf("Kinds() has %d entries, want %d: %v", len(got), len(wantKinds), got)
	}
	for i, k := range wantKinds {
		if got[i] != k {
			t.Errorf("Kinds()[%d] = %q, want %q", i, got[i], k)
		}
	}
}

// Two entries for one kind would make Lookup's answer depend on slice order,
// and the second would be unreachable.
func TestNoDuplicateKinds(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range All() {
		if seen[e.Kind] {
			t.Errorf("duplicate entry for kind %q", e.Kind)
		}
		seen[e.Kind] = true
	}
}

// All() is sorted by Kind, and Kinds() is All()'s kinds in the same order. A
// caller that ranges over one and indexes the other must not be surprised.
func TestAllIsSortedAndAgreesWithKinds(t *testing.T) {
	all := All()
	if !sort.SliceIsSorted(all, func(i, j int) bool { return all[i].Kind < all[j].Kind }) {
		t.Error("All() is not sorted by Kind")
	}
	kinds := Kinds()
	if len(kinds) != len(all) {
		t.Fatalf("Kinds() has %d, All() has %d", len(kinds), len(all))
	}
	for i := range all {
		if kinds[i] != all[i].Kind {
			t.Errorf("index %d: Kinds() = %q, All() = %q", i, kinds[i], all[i].Kind)
		}
	}
}

// All() must not hand a caller the package's own backing array: a consumer that
// sorts or truncates the result must not corrupt the registry for the next one.
func TestAllReturnsACopy(t *testing.T) {
	first := All()
	if len(first) == 0 {
		t.Fatal("All() is empty")
	}
	first[0].Kind = "mutated"
	if All()[0].Kind == "mutated" {
		t.Error("All() shares its backing array with the registry")
	}
}

// Lookup is an exact match. No case folding, no Init: stripping, no fuzz.
func TestLookupIsExact(t *testing.T) {
	for _, k := range wantKinds {
		e, ok := Lookup(k)
		if !ok {
			t.Errorf("Lookup(%q) not found", k)
			continue
		}
		if e.Kind != k {
			t.Errorf("Lookup(%q).Kind = %q", k, e.Kind)
		}
	}
	for _, miss := range []string{"oomkilled", "OOMKILLED", " OOMKilled", "OOMKilled ", "Init:Nope", "", "Pending"} {
		if _, ok := Lookup(miss); ok {
			t.Errorf("Lookup(%q) matched, want no match", miss)
		}
	}
}

// An Init: kind is its own failure mode, not an alias for the base kind. If
// Lookup ever fell back by stripping the prefix, these two would return the
// same entry.
func TestInitKindsAreDistinctEntries(t *testing.T) {
	base, ok := Lookup("OOMKilled")
	if !ok {
		t.Fatal("OOMKilled missing")
	}
	init, ok := Lookup("Init:OOMKilled")
	if !ok {
		t.Fatal("Init:OOMKilled missing")
	}
	if base.Detail == init.Detail {
		t.Error("Init:OOMKilled reuses OOMKilled's Detail; it is a different failure mode")
	}
}

func TestEveryEntryIsPopulated(t *testing.T) {
	for _, e := range All() {
		if e.Kind == "" || e.Summary == "" || e.Detail == "" {
			t.Errorf("%q: an empty Kind, Summary or Detail", e.Kind)
		}
		if len(e.Causes) < 2 {
			t.Errorf("%q: %d causes, want at least 2", e.Kind, len(e.Causes))
		}
		if len(e.Checks) < 2 {
			t.Errorf("%q: %d checks, want at least 2", e.Kind, len(e.Checks))
		}
		if e.Docs == "" {
			t.Errorf("%q: no Docs anchor", e.Kind)
		}
	}
}

// The struct's doc comments promise a Summary is lowercase with no trailing
// period and a Detail is capitalised and punctuated. A comment that promises
// what the code does not keep is a defect, so the promise is a test.
func TestProseStyle(t *testing.T) {
	for _, e := range All() {
		if r := []rune(e.Summary)[0]; unicode.IsUpper(r) {
			t.Errorf("%q: Summary starts uppercase: %q", e.Kind, e.Summary)
		}
		if strings.HasSuffix(e.Summary, ".") {
			t.Errorf("%q: Summary ends with a period: %q", e.Kind, e.Summary)
		}
		if r := []rune(e.Detail)[0]; !unicode.IsUpper(r) {
			t.Errorf("%q: Detail starts lowercase: %q", e.Kind, e.Detail)
		}
		if !strings.HasSuffix(e.Detail, ".") {
			t.Errorf("%q: Detail is not punctuated: %q", e.Kind, e.Detail)
		}
		for _, c := range e.Causes {
			if !strings.HasSuffix(c, ".") {
				t.Errorf("%q: cause is not punctuated: %q", e.Kind, c)
			}
		}
	}
}

// hostMarkers are the substrings that would mean a host, an address or a URL
// had reached the prose. URLs are credentials in this repository; the one
// permitted host is the project's own and it belongs in Docs, which this test
// deliberately does not scan.
var hostMarkers = []string{"://", "http", "www.", ".com", ".net", ".org", ".io", "k8sproject"}

var dottedQuad = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

func TestNoHostReachesTheProse(t *testing.T) {
	for _, e := range All() {
		fields := map[string][]string{
			"Summary": {e.Summary},
			"Detail":  {e.Detail},
			"Causes":  e.Causes,
			"Checks":  e.Checks,
		}
		for name, texts := range fields {
			for _, text := range texts {
				lower := strings.ToLower(text)
				for _, m := range hostMarkers {
					if strings.Contains(lower, m) {
						t.Errorf("%q %s contains %q: %q", e.Kind, name, m, text)
					}
				}
				if dottedQuad.MatchString(text) {
					t.Errorf("%q %s contains an address: %q", e.Kind, name, text)
				}
			}
		}
	}
}

// Docs is the one field allowed to carry a host, and only the project's own.
func TestDocsPointAtTheProjectSite(t *testing.T) {
	const prefix = "https://k8sproject.top/"
	for _, e := range All() {
		if !strings.HasPrefix(e.Docs, prefix) {
			t.Errorf("%q: Docs = %q, want a %s anchor", e.Kind, e.Docs, prefix)
		}
	}
}

// allowedPlaceholders is the closed set a Checks line may substitute. A real
// namespace, pod, container, node or object name in shipped help text would be
// someone's cluster leaking into the binary.
var allowedPlaceholders = map[string]bool{
	"<namespace>": true, "<pod>": true, "<container>": true, "<node>": true, "<name>": true,
}

var placeholder = regexp.MustCompile(`<[^>]*>`)

func TestChecksUseOnlyAllowedPlaceholders(t *testing.T) {
	for _, e := range All() {
		for _, c := range e.Checks {
			for _, p := range placeholder.FindAllString(c, -1) {
				if !allowedPlaceholders[p] {
					t.Errorf("%q: check uses %q, which is not an allowed placeholder: %q", e.Kind, p, c)
				}
			}
		}
	}
}
```

Create `internal/knownissues/imports_test.go`, on the pattern
`internal/baseline/imports_test.go` established:

```go
package knownissues

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/imantaba/kubeagent"

// TestNoKubeagentImport pins the first half of this package's wall: it imports
// nothing from kubeagent at all. That puts internal/remediate and
// internal/explain out of reach by construction rather than by rule, which is
// the same guarantee internal/jsonschema, internal/dashboard, internal/baseline
// and internal/glob carry.
func TestNoKubeagentImport(t *testing.T) {
	for _, f := range packageFiles(t) {
		for _, imp := range importsOf(t, f) {
			if strings.HasPrefix(imp, modulePath) {
				t.Errorf("%s imports %q; this package must import nothing from kubeagent", f, imp)
			}
		}
	}
}

// TestStdlibOnly pins the second half: nothing outside the standard library
// either. A standard-library path's first segment never contains a dot; a
// module path's always does.
func TestStdlibOnly(t *testing.T) {
	for _, f := range packageFiles(t) {
		for _, imp := range importsOf(t, f) {
			first, _, _ := strings.Cut(imp, "/")
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %q; this package is standard-library only", f, imp)
			}
		}
	}
}

// packageFiles lists the package's non-test .go files. It is fatal on an empty
// result so the guards above can never pass vacuously.
func packageFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var files []string
	for _, f := range all {
		if !strings.HasSuffix(f, "_test.go") {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("no non-test .go files found; the import guards would pass vacuously")
	}
	return files
}

// importsOf returns the import paths of one file.
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquote %s in %s: %v", spec.Path.Value, path, err)
		}
		out = append(out, p)
	}
	return out
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/knownissues/
```

Expected: FAIL — the package does not compile, `undefined: All`, `undefined: Kinds`, `undefined: Lookup`.

- [ ] **Step 3: Write `internal/knownissues/knownissues.go`**

```go
// Package knownissues is kubeagent's offline reference for the failure kinds
// its deterministic detectors report: what each one means, what usually causes
// it, and what to look at next.
//
// The content is curated prose compiled into the binary, so `kubeagent
// known-issues` answers with no cluster, no kubeconfig and no network. It holds
// no client and no context, issues no cluster call and makes no model call —
// two separate promises, and neither implies the other. In particular this is
// not a smaller --explain: nothing here is generated, and nothing here is sent
// anywhere.
//
// The package imports nothing from kubeagent and nothing outside the standard
// library, which puts it in the same class as internal/jsonschema,
// internal/dashboard, internal/baseline and internal/glob and makes reaching
// internal/remediate or internal/explain impossible by construction rather than
// by rule. internal/knownissues/imports_test.go enforces both halves. The
// consequence is that the registry cannot check itself against the detector
// set; that check lives in internal/diagnose/knownissues_test.go, where both
// sides are in scope.
package knownissues

// Entry is what kubeagent knows about one failure mode, offline.
type Entry struct {
	// Kind is the exact Finding.Issue value a detector emits. It is the join
	// key between a scan's output and this reference, which is why it is a
	// verbatim copy rather than a prettier restatement.
	Kind string

	// Summary is one line, lowercase, no trailing period: it is rendered
	// inline in the list view beside the kind.
	Summary string

	// Detail is the sentence or two printed above the causes when one entry is
	// printed in full. Capitalised, punctuated.
	Detail string

	// Causes are what actually produces this, most common first.
	Causes []string

	// Checks are read-only next steps. Any object name is a placeholder.
	Checks []string

	// Docs is the anchor on the project's own documentation site, or empty.
	Docs string
}

// All returns every entry, sorted by Kind.
//
// The result is a fresh slice each call: a caller that sorts, filters or
// truncates it must not be able to corrupt the registry for the next one. The
// Causes and Checks slices inside each Entry are still shared, which is why
// this package hands out no way to mutate them and every consumer only ranges.
func All() []Entry {
	out := make([]Entry, len(entries))
	copy(out, entries)
	return out
}

// Kinds returns every Kind, sorted — the same order as All.
func Kinds() []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Kind
	}
	return out
}

// Lookup returns the entry for a kind, matched byte for byte.
//
// Deliberately no normalisation: no case folding, no Init: stripping, no fuzzy
// match. Falling back from "Init:OOMKilled" to "OOMKilled" would be the
// tempting convenience and it would be wrong — an init container killed for
// memory blocks the pod from ever starting, which is a different failure with
// different causes and different next steps. The caller passes a Finding.Issue
// value verbatim, and an exact match is the only honest answer.
func Lookup(kind string) (Entry, bool) {
	for _, e := range entries {
		if e.Kind == kind {
			return e, true
		}
	}
	return Entry{}, false
}
```

- [ ] **Step 4: Write `internal/knownissues/entries.go`**

Transcribe this verbatim. It is the curated content of the slice; do not
paraphrase, reorder, or "improve" it.

```go
package knownissues

// entries is the curated content: one entry per failure kind
// diagnose.DefaultDetectors can emit, sorted by Kind so All and Kinds need no
// sort at call time.
//
// This is a Go slice literal rather than an embedded data file on purpose. A
// data file would need a parser, an error path for a malformed entry, and a
// dependency decision the no-new-dependency rule forecloses — and it would buy
// nothing, because a contributor edits prose inside a struct either way. Here,
// `go build` rejects a malformed entry and `go vet` sees the whole thing.
var entries = []Entry{
	{
		Kind:    "CrashLoopBackOff",
		Summary: "a container starts, exits, and is restarted on a widening backoff",
		Detail: "Kubernetes started the container, the process exited, and the kubelet " +
			"restarted it — over and over. After the first few attempts the kubelet waits " +
			"longer between them, which is the BackOff in the name. The container is not " +
			"stuck: it is running briefly and failing every time.",
		Causes: []string{
			"The process exits immediately — a missing environment variable, an unreachable dependency, or a bad command.",
			"The image's entrypoint is wrong, so the container runs and exits straight away.",
			"A liveness probe kills the container before it finishes starting; a startup probe is the fix, not a longer liveness period.",
			"The application panics on a configuration value it cannot parse.",
			"A file the process needs at startup is absent from the mount it expects.",
		},
		Checks: []string{
			"kubectl -n <namespace> logs <pod> -c <container> --previous — the crashed run, not the current one",
			"kubectl -n <namespace> describe pod <pod> — the exit code and the last terminated state",
			"The container's command and args against what the image expects to be told to run",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#crashloopbackoff",
	},
	{
		Kind:    "CreateContainerConfigError",
		Summary: "the kubelet cannot build the container from its spec",
		Detail: "The kubelet accepted the pod, tried to assemble the container's " +
			"configuration, and could not: something the spec references does not exist. " +
			"The container never reaches Running, so there are no logs to read — the " +
			"kubelet's waiting message names the missing object and is the whole diagnosis. " +
			"Unlike an event, that state persists for as long as the container is stuck.",
		Causes: []string{
			"The ConfigMap or Secret has not been created yet — manifests applied out of order.",
			"It exists in another namespace; a pod can only reference objects in its own.",
			"The object exists but the key named in configMapKeyRef or secretKeyRef does not.",
			"A typo in the object name in the pod spec.",
			"A controller that generates the object has not reconciled yet.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the waiting message names the missing object",
			"kubectl -n <namespace> get configmap,secret — whether the referenced object is there at all",
			"kubectl -n <namespace> get configmap <name> -o jsonpath='{.data}' — whether it holds the keys the pod asks for",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#createcontainerconfigerror",
	},
	{
		Kind:    "ErrImagePull",
		Summary: "the kubelet's attempt to pull the image failed",
		Detail: "The kubelet asked the container runtime to pull the image and the pull " +
			"returned an error: the tag does not exist, the registry wants credentials the " +
			"node does not have, or the registry could not be reached. This is the first " +
			"failure; after several the kubelet backs off and the state becomes " +
			"ImagePullBackOff. Same problem, one stage later.",
		Causes: []string{
			"The tag does not exist in the registry — a typo, or a tag that was never pushed.",
			"The registry is private and the pod has no imagePullSecret, or the secret lives in another namespace.",
			"The node cannot reach the registry — egress firewall, proxy, or DNS.",
			"The image reference omits a registry and defaults to the public one, which is not where the image lives.",
			"An anonymous-pull rate limit is being applied to the node.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the Failed event carries the registry's own error",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.containers[*].image}' — the exact reference being pulled",
			"kubectl -n <namespace> get serviceaccount <name> -o yaml — whether an imagePullSecret is attached",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#imagepullbackoff-errimagepull",
	},
	{
		Kind:    "ImagePullBackOff",
		Summary: "repeated pull failures, now backing off between attempts",
		Detail: "The same failure ErrImagePull describes, after the kubelet stopped " +
			"retrying immediately. The pod can sit here for minutes with nothing new in its " +
			"events, which makes it look like a hang rather than a pull error. Correcting " +
			"the image or the credential does not always clear it at once: the kubelet " +
			"retries on its own schedule.",
		Causes: []string{
			"Every ErrImagePull cause — this is that failure once the kubelet began backing off.",
			"The image or the credential was corrected, but the kubelet has not retried yet.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the pull error, unchanged from the first attempt",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.containers[*].image}' — the exact reference being pulled",
			"kubectl -n <namespace> get events --field-selector involvedObject.name=<pod> — how far apart the attempts now are",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#imagepullbackoff-errimagepull",
	},
	{
		Kind:    "Init:CrashLoopBackOff",
		Summary: "an init container is crash-looping, so the pod never starts",
		Detail: "Init containers run one at a time, in order, and each must succeed before " +
			"the next begins. One is crashing and being restarted, which blocks the pod " +
			"indefinitely: the main containers have not run at all, so their logs are empty " +
			"and their status says only PodInitializing. The failure — and the logs — are in " +
			"the init container.",
		Causes: []string{
			"A wait-for-dependency init container cannot reach the service or endpoint it polls.",
			"The init container's own command is wrong, or its script exits non-zero.",
			"A migration or setup step fails against a backend that is not ready.",
			"A volume the init container writes to is read-only, or not mounted where it expects.",
			"The init step has no timeout, so a permanent failure looks like a slow start.",
		},
		Checks: []string{
			"kubectl -n <namespace> logs <pod> -c <container> --previous — the failing init container, not a main one",
			"kubectl -n <namespace> describe pod <pod> — which init container, its position, and its restart count",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.status.initContainerStatuses}' — the whole init phase at once",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#init-container-failures",
	},
	{
		Kind:    "Init:ErrImagePull",
		Summary: "an init container's image could not be pulled",
		Detail: "The pull failure is the one ErrImagePull describes, on an init container " +
			"instead of a main one. The consequence differs: init containers block the pod, " +
			"so nothing later runs and the main containers' images are never even fetched. " +
			"An init step often uses a different image from the workload — a small utility, " +
			"a migration tool — and it is easy for that one to miss a credential the main " +
			"image has.",
		Causes: []string{
			"The init image's tag does not exist, or the reference has a typo.",
			"The init image lives in a registry the pod has no imagePullSecret for, even though the main image does not.",
			"The node cannot reach that registry — egress firewall, proxy, or DNS.",
			"A utility image was pinned to a tag that has since been deleted.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the Failed event names the init container and the pull error",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.initContainers[*].image}' — the exact init references",
			"kubectl -n <namespace> get serviceaccount <name> -o yaml — whether an imagePullSecret is attached",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#init-container-failures",
	},
	{
		Kind:    "Init:ImagePullBackOff",
		Summary: "an init container's image pull is backing off",
		Detail: "Init:ErrImagePull after the kubelet started waiting between attempts. The " +
			"pod is wedged at that init container and will stay there: nothing later in the " +
			"sequence runs, and the main containers are never started. The pod's age climbs " +
			"while the events go quiet, which is what makes this read as a hang.",
		Causes: []string{
			"Every Init:ErrImagePull cause — this is that failure once the kubelet began backing off.",
			"The init image or its credential was corrected, but the kubelet has not retried yet.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the pull error, unchanged from the first attempt",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.initContainers[*].image}' — the exact init references",
			"kubectl -n <namespace> get events --field-selector involvedObject.name=<pod> — how far apart the attempts now are",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#init-container-failures",
	},
	{
		Kind:    "Init:OOMKilled",
		Summary: "an init container was killed for exceeding its memory limit",
		Detail: "The kernel's OOM killer terminated an init container, and the pod is " +
			"blocked in its init phase. Init steps are often given small limits, or none of " +
			"their own, on the assumption that they are cheap — a migration, a restore or an " +
			"archive extraction breaks that assumption. The exit code is 137. Raising the " +
			"limit is not automatically right: a step that grows without bound will exceed " +
			"any limit.",
		Causes: []string{
			"The limit is smaller than the work the step actually does.",
			"It loads a whole file or dataset into memory instead of streaming it.",
			"It inherits a low default from a LimitRange it was never sized against.",
			"The work grew: the same step passed when the dataset was smaller.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the init container's exit code and last terminated state",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.initContainers[*].resources}' — the limits it runs under",
			"kubectl -n <namespace> get limitrange -o yaml — a namespace default that may be capping it",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#init-container-failures",
	},
	{
		Kind:    "OOMKilled",
		Summary: "the kernel killed a container for exceeding its memory limit",
		Detail:  "The kernel killed a container for exceeding its memory limit.",
		Causes: []string{
			"The limit is lower than the workload's real steady-state usage.",
			"A leak: usage climbs until the limit is reached, then repeats on a cycle.",
			"A runtime heap sized above the container limit, so the runtime never reclaims before the kernel intervenes.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — lastState.terminated.exitCode 137",
			"kubectl -n <namespace> top pod <pod> — usage against the configured limit",
			"The container's own memory tuning against resources.limits.memory",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#oomkilled",
	},
	{
		Kind:    "ProbeFailure",
		Summary: "a container is running but a probe keeps failing",
		Detail: "The container is Running and the pod is not Ready, because a kubelet probe " +
			"against it keeps failing. Which probe decides what happens next: a readiness " +
			"failure takes the pod out of its Service's endpoints and leaves it running, a " +
			"liveness failure makes the kubelet restart the container, and a startup failure " +
			"means the container is restarted before it ever finishes starting. kubeagent " +
			"names the probe and a coarse reason, and never carries the raw probe message, " +
			"which can hold a pod address or arbitrary exec output.",
		Causes: []string{
			"initialDelaySeconds is shorter than the application's real startup time; a startup probe is the fix, not a longer liveness period.",
			"The probe targets the wrong port or the wrong path.",
			"The application is genuinely unhealthy and answering with an error status.",
			"A dependency the health endpoint checks is down, so the probe reports the dependency's failure as the pod's.",
			"timeoutSeconds is shorter than the endpoint's response time under load.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the Unhealthy events and the probe definitions",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.containers[*].readinessProbe}' — the port, path and timings",
			"kubectl -n <namespace> logs <pod> -c <container> — whether the application logs the failing requests",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#probefailure",
	},
	{
		Kind:    "RestartLoop",
		Summary: "a container keeps exiting and restarting while still Running",
		Detail: "A flapping container: several restarts, the last one recent, the previous " +
			"run ended non-zero and not by an OOM kill — and the container happens to be " +
			"Running at the moment kubeagent looked. This is the case CrashLoopBackOff " +
			"misses, because that condition is only visible while the kubelet is waiting " +
			"between attempts. A pod that restarts every few minutes can look healthy in " +
			"every point-in-time check and never stay up.",
		Causes: []string{
			"An intermittent failure — a dependency that is up most of the time, or a request that occasionally kills the process.",
			"A liveness probe that fails under load and restarts a container that would have recovered.",
			"The process treats a recurring condition, such as a dropped connection, as fatal.",
			"A limit other than memory — ephemeral storage, for instance — terminating the container.",
			"A controller or operator restarting the workload on a schedule.",
		},
		Checks: []string{
			"kubectl -n <namespace> logs <pod> -c <container> --previous — the run that exited is the one that failed",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.status.containerStatuses[*].restartCount}' — how fast it is climbing",
			"kubectl -n <namespace> describe pod <pod> — the last terminated state's exit code and reason",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#restartloop",
	},
	{
		Kind:    "Unschedulable",
		Summary: "no node can place the pod",
		Detail: "The API server accepted the pod and the scheduler cannot find a node that " +
			"satisfies it, so it stays Pending with nothing running. The scheduler records " +
			"why on the pod's PodScheduled condition, usually with a count of how many nodes " +
			"were rejected and for which reason — insufficient capacity and an unsatisfied " +
			"taint read very differently there.",
		Causes: []string{
			"No node has enough allocatable CPU or memory left for the pod's requests.",
			"Every candidate node carries a taint the pod does not tolerate.",
			"A nodeSelector, node affinity or topology constraint no node matches.",
			"The pod needs a volume that can only attach in a zone with no room left.",
			"The cluster has no nodes in a Ready state at all.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the scheduler's own message on the PodScheduled condition",
			"kubectl get nodes -o wide — whether nodes are Ready, and how many there are",
			"kubectl describe node <node> — its taints and its allocated-resources summary",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#pending-unschedulable",
	},
	{
		Kind:    "VolumeAttachError",
		Summary: "a volume cannot be attached, so the container never starts",
		Detail: "The pod is scheduled and the kubelet cannot start it: a volume it needs is " +
			"not attached to the node it landed on. The common shape is Multi-Attach — a " +
			"ReadWriteOnce volume is still attached to the node the previous pod ran on, and " +
			"it cannot be attached in two places at once. That clears itself once the old " +
			"attachment is released, which can take several minutes after a node failure; " +
			"when it does not clear, the old node is usually gone without ever detaching.",
		Causes: []string{
			"A ReadWriteOnce volume is still attached to the node the previous pod ran on.",
			"The node that held the volume was lost, so the detach never completed.",
			"The CSI driver on the target node is not running, or not healthy.",
			"The volume is in a different zone from the node the pod was scheduled to.",
			"The storage backend refused the attach — a quota, a per-node device limit, or an unavailable volume.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the FailedAttachVolume events and what they name",
			"kubectl get volumeattachment — which node the volume is currently attached to",
			"kubectl -n <namespace> get pvc — the claim's phase and the volume it is bound to",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#volumeattacherror",
	},
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/knownissues/ -v
```

Expected: PASS, every test. If `TestProseStyle` or `TestNoHostReachesTheProse`
fails, the transcription drifted from the text above — fix the transcription,
not the test.

- [ ] **Step 6: Verify the whole tree still builds and passes**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
```

Expected: all packages PASS. `go.mod` and `go.sum` must be unchanged —
confirm with `git status --short`.

- [ ] **Step 7: Commit**

```bash
git add internal/knownissues/
git commit -s -m "feat(knownissues): offline registry of the thirteen detector issue kinds

One curated entry per failure kind diagnose.DefaultDetectors can emit:
what it means, what usually causes it, and what to read next. The
content is a Go slice literal, so go build rejects a malformed entry
and there is no parser, no data file and no new dependency.

The package imports nothing from kubeagent and nothing outside the
standard library, joining internal/jsonschema, internal/dashboard,
internal/baseline and internal/glob. imports_test.go enforces both
halves. It holds no client and no context, issues no cluster call and
makes no model call."
```

---

## Task 2: The three completeness tests

**Files:**
- Create: `internal/diagnose/knownissues_test.go`

**Interfaces:**
- Consumes: `knownissues.Kinds()` and `knownissues.Lookup()` from Task 1; `diagnose.DefaultDetectors(now)`, `diagnose.Run(detectors, facts)`, `diagnose.PodFacts{Pod, Events}`, and the existing fake-pod builders in this package's test files — `podWaiting(ns, name, container, reason, message)`, `podWithInit(ns, name, initStatuses...)`, `podOOMKilled(ns, name, container, exitCode, viaLastTermination)`, `podUnschedulable(ns, name, message)`, `podCreating(ns, name)`, `attachEvent(ns, podName, msg)`, `pfPod(ns, name, container)`, `pfEvent(ns, pod, container, message)`, `flapPod(restarts, ranFor, exit, reason, finishedAgo)` and the package-level `rlNow`. All of these already exist; do not redefine any of them.
- Produces: nothing importable. This is the join between the registry and the detectors, and it lives here because `internal/diagnose` may import `internal/knownissues` freely while the reverse is forbidden.

Why three tests and not one: they fail for different reasons and a maintainer
needs to know which. The static walk catches a new literal added to a detector.
The behavioural table catches a kind that is composed at runtime and never
appears as a literal at all. The reverse check catches an entry documenting a
kind nothing emits.

- [ ] **Step 1: Write the failing tests**

Create `internal/diagnose/knownissues_test.go`:

```go
package diagnose

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/knownissues"
)

// TestEveryIssueLiteralIsDocumented is the static half: it parses this
// package's non-test sources and checks every string literal that reaches a
// Finding's Issue field against the registry.
//
// Two shapes appear in the detectors. A bare literal — Issue: "OOMKilled" — must
// be a documented kind. A literal ending in ':' is a prefix composed with a
// runtime value — Issue: "Init:" + w.Reason — and must have at least one
// documented kind beneath it; the exact set it can produce is not knowable
// statically, which is what the behavioural test below covers.
//
// The honest limit, written down rather than implied: this test enumerates
// literals, not kinds. An Issue field assigned a bare variable (imagepull.go's
// Issue: w.Reason) contributes nothing here. It is a guard against a new
// literal slipping in undocumented, not a proof of completeness — that proof is
// TestDetectorsProduceOnlyDocumentedKinds.
func TestEveryIssueLiteralIsDocumented(t *testing.T) {
	documented := map[string]bool{}
	for _, k := range knownissues.Kinds() {
		documented[k] = true
	}

	literals := issueLiterals(t)
	if len(literals) == 0 {
		t.Fatal("found no Issue: literals; the walk is broken and would pass vacuously")
	}
	for _, lit := range literals {
		if strings.HasSuffix(lit, ":") {
			if !anyKindHasPrefix(documented, lit) {
				t.Errorf("Issue prefix %q composes kinds none of which are documented", lit)
			}
			continue
		}
		if !documented[lit] {
			t.Errorf("Issue %q is emitted by a detector and is not in internal/knownissues", lit)
		}
	}
}

// anyKindHasPrefix reports whether at least one documented kind starts with p.
func anyKindHasPrefix(documented map[string]bool, p string) bool {
	for k := range documented {
		if strings.HasPrefix(k, p) && k != p {
			return true
		}
	}
	return false
}

// issueLiterals collects every string literal assigned to an Issue field in a
// composite literal in this package's non-test files, including the left
// operand of a concatenation.
func issueLiterals(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var out []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Issue" {
				return true
			}
			if lit := leadingStringLit(kv.Value); lit != "" {
				out = append(out, lit)
			}
			return true
		})
	}
	return out
}

// leadingStringLit unwraps a string literal, or the leftmost operand of a
// concatenation when that operand is one; "" when the expression begins with
// something else.
func leadingStringLit(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return ""
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return ""
		}
		return s
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return ""
		}
		return leadingStringLit(v.X)
	}
	return ""
}

// TestDetectorsProduceOnlyDocumentedKinds is the behavioural half: it drives
// the shipped detector set over a fixture per kind and checks that every Issue
// it actually produces is documented. This is the test that covers the kinds
// composed at runtime, which the static walk cannot see.
func TestDetectorsProduceOnlyDocumentedKinds(t *testing.T) {
	for _, issue := range producedKinds(t) {
		if _, ok := knownissues.Lookup(issue); !ok {
			t.Errorf("a detector produced Issue %q, which is not in internal/knownissues", issue)
		}
	}
}

// TestEveryDocumentedKindIsProduced is the reverse: nothing in the registry
// documents a kind no detector can emit. Without this the registry could grow
// entries for kinds that were removed, or invented, and every other test would
// still pass.
func TestEveryDocumentedKindIsProduced(t *testing.T) {
	produced := map[string]bool{}
	for _, k := range producedKinds(t) {
		produced[k] = true
	}
	for _, k := range knownissues.Kinds() {
		if !produced[k] {
			t.Errorf("internal/knownissues documents %q, which no detector in this fixture set produces", k)
		}
	}
}

// producedKinds runs DefaultDetectors over one fixture per kind and returns the
// sorted, deduplicated set of Issue values that came out.
//
// The fixtures deliberately go through DefaultDetectors and Run rather than
// calling each detector directly: what must stay in step with the registry is
// the set kubeagent ships, not the set of types that happen to exist.
func producedKinds(t *testing.T) []string {
	t.Helper()

	facts := []PodFacts{
		// CrashLoopBackOff
		{Pod: podWaiting("example-ns", "web-1", "app", "CrashLoopBackOff", "")},
		// CreateContainerConfigError
		{Pod: podWaiting("example-ns", "web-2", "app", "CreateContainerConfigError",
			`configmap "app-config" not found`)},
		// ErrImagePull
		{Pod: podWaiting("example-ns", "web-3", "app", "ErrImagePull", "manifest unknown")},
		// ImagePullBackOff
		{Pod: podWaiting("example-ns", "web-4", "app", "ImagePullBackOff", "back-off pulling image")},
		// Init:CrashLoopBackOff
		{Pod: podWithInit("example-ns", "web-5", corev1.ContainerStatus{
			Name: "setup", RestartCount: 4,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		})},
		// Init:ErrImagePull
		{Pod: podWithInit("example-ns", "web-6", corev1.ContainerStatus{
			Name:  "setup",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}},
		})},
		// Init:ImagePullBackOff
		{Pod: podWithInit("example-ns", "web-7", corev1.ContainerStatus{
			Name:  "setup",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		})},
		// Init:OOMKilled
		{Pod: podWithInit("example-ns", "web-8", corev1.ContainerStatus{
			Name:  "setup",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
		})},
		// OOMKilled
		{Pod: podOOMKilled("example-ns", "web-9", "app", 137, false)},
		// ProbeFailure
		{Pod: pfPod("example-ns", "web-10", "app"), Events: []corev1.Event{
			pfEvent("example-ns", "web-10", "app", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
		}},
		// RestartLoop
		{Pod: flapPod(3, 20*time.Second, 1, "Error", 25*time.Second)},
		// Unschedulable
		{Pod: podUnschedulable("example-ns", "web-11", "0/3 nodes are available: 3 Insufficient memory.")},
		// VolumeAttachError
		{Pod: podCreating("example-ns", "web-12"), Events: []corev1.Event{
			attachEvent("example-ns", "web-12",
				`Multi-Attach error for volume "pvc-example" Volume is already exclusively attached to one node`),
		}},
	}

	seen := map[string]bool{}
	for _, f := range Run(DefaultDetectors(rlNow), facts) {
		seen[f.Issue] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("the fixture set produced no findings at all")
	}
	return out
}
```

- [ ] **Step 2: Run the tests to verify they pass**

These tests are written against code that already exists — Task 1's registry
and the shipped detectors — so they should pass immediately. That is not a
missing red step: their value is as a standing guard, and the red run that
proves they can fail is the next step.

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/diagnose/ -run 'TestEveryIssueLiteral|TestDetectorsProduceOnly|TestEveryDocumentedKind' -v
```

Expected: PASS, all three.

- [ ] **Step 3: Prove each test can fail**

Do this by hand, one at a time, reverting after each. Do not commit any of
these edits.

1. In `internal/knownissues/entries.go`, change `Kind: "OOMKilled"` to
   `Kind: "OOMKilledX"`. Re-run the three tests.
   Expected: `TestEveryIssueLiteralIsDocumented` fails naming `"OOMKilled"`,
   `TestDetectorsProduceOnlyDocumentedKinds` fails naming `"OOMKilled"`, and
   `TestEveryDocumentedKindIsProduced` fails naming `"OOMKilledX"`. Revert.
2. Delete the `Init:OOMKilled` entry from `entries.go`. Re-run.
   Expected: `TestDetectorsProduceOnlyDocumentedKinds` fails naming
   `"Init:OOMKilled"` and the static walk still passes — which is exactly the
   division of labour the comment claims. Revert.
3. Delete the `Init:CrashLoopBackOff` fixture from `producedKinds`. Re-run.
   Expected: `TestEveryDocumentedKindIsProduced` fails naming
   `"Init:CrashLoopBackOff"`. Revert.

Confirm `git status --short` is clean apart from the new test file before
continuing.

- [ ] **Step 4: Run the full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
```

Expected: all packages PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/diagnose/knownissues_test.go
git commit -s -m "test(diagnose): prove the registry and the detector set agree

Three tests, each failing for a different reason. A go/parser walk
checks every string literal reaching a Finding's Issue field, treating
a ':'-terminated literal as a prefix that must have documented kinds
beneath it. A fixture table drives DefaultDetectors to produce all
thirteen kinds and checks each against the registry, which is what
covers the kinds composed at runtime and never present as a literal.
The reverse check refuses an entry for a kind nothing emits.

The tests live here rather than in internal/knownissues so that
package's import set stays empty."
```

---

## Task 3: The `known-issues` command

**Files:**
- Create: `internal/cli/knownissues.go`
- Create: `internal/cli/knownissues_test.go`
- Modify: `internal/cli/root.go` — the `root.AddCommand(...)` call

**Interfaces:**
- Consumes: `knownissues.All()`, `knownissues.Kinds()`, `knownissues.Lookup()` from Task 1; the package-level `invokedAs` variable in `internal/cli` (holds `argv[0]`'s spelling).
- Produces: `func runKnownIssues(args []string, w io.Writer) error` and `func newKnownIssuesCommand() *cobra.Command`.

Layout rules, fixed:

- The list view is `"  %-28s %s\n"` — the longest kind, `CreateContainerConfigError`, is 26 runes, so 28 leaves two spaces of gutter.
- `Detail` and each cause wrap at **72 columns**, counting the prefix. `Checks` do **not** wrap: a wrapped command line cannot be copy-pasted, which is the whole point of that section.
- The `Docs` line is indented two spaces and preceded by a blank line.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/knownissues_test.go`:

```go
package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/knownissues"
)

// The list view names every documented kind, one per line, with its summary.
func TestRunKnownIssuesListsEveryKind(t *testing.T) {
	var buf bytes.Buffer
	if err := runKnownIssues(nil, &buf); err != nil {
		t.Fatalf("runKnownIssues() error = %v", err)
	}
	out := buf.String()
	for _, e := range knownissues.All() {
		if !strings.Contains(out, e.Kind) {
			t.Errorf("list output is missing kind %q", e.Kind)
		}
		if !strings.Contains(out, e.Summary) {
			t.Errorf("list output is missing the summary for %q", e.Kind)
		}
	}
	if !strings.Contains(out, "known-issues <kind>") {
		t.Error("list output does not tell the reader how to print one")
	}
}

// The list is sorted, so a reader can find a kind by scanning.
func TestRunKnownIssuesListIsSorted(t *testing.T) {
	var buf bytes.Buffer
	if err := runKnownIssues(nil, &buf); err != nil {
		t.Fatalf("runKnownIssues() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var prev string
	for _, ln := range lines {
		fields := strings.Fields(ln)
		if len(fields) == 0 {
			continue
		}
		if _, ok := knownissues.Lookup(fields[0]); !ok {
			continue // the footer, not a kind row
		}
		if prev != "" && fields[0] < prev {
			t.Errorf("kinds out of order: %q after %q", fields[0], prev)
		}
		prev = fields[0]
	}
	if prev == "" {
		t.Fatal("no kind rows found; the assertion would pass vacuously")
	}
}

// One kind prints in full: the detail, every cause, every check, the anchor.
func TestRunKnownIssuesPrintsOneEntry(t *testing.T) {
	var buf bytes.Buffer
	if err := runKnownIssues([]string{"OOMKilled"}, &buf); err != nil {
		t.Fatalf("runKnownIssues() error = %v", err)
	}
	out := buf.String()
	e, ok := knownissues.Lookup("OOMKilled")
	if !ok {
		t.Fatal("OOMKilled missing from the registry")
	}
	for _, want := range []string{e.Kind, "Likely causes", "What to check", e.Docs} {
		if !strings.Contains(out, want) {
			t.Errorf("full output is missing %q", want)
		}
	}
	for _, c := range e.Checks {
		if !strings.Contains(out, c) {
			t.Errorf("full output is missing a check verbatim: %q", c)
		}
	}
	// Another kind's content must not leak into this one.
	if strings.Contains(out, "Unschedulable") {
		t.Error("full output for one kind mentions another")
	}
}

// A check is a command line: it is printed on one line, never wrapped, so it
// can be copied.
func TestChecksAreNeverWrapped(t *testing.T) {
	for _, e := range knownissues.All() {
		var buf bytes.Buffer
		if err := runKnownIssues([]string{e.Kind}, &buf); err != nil {
			t.Fatalf("runKnownIssues(%q) error = %v", e.Kind, err)
		}
		for _, c := range e.Checks {
			found := false
			for _, ln := range strings.Split(buf.String(), "\n") {
				if strings.TrimSpace(ln) == "- "+c {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%q: check is not on a line of its own: %q", e.Kind, c)
			}
		}
	}
}

// Prose wraps at 72 columns. Checks are exempt by the rule above, and so is a
// single word longer than the budget, which is emitted whole.
func TestProseWrapsAtSeventyTwoColumns(t *testing.T) {
	for _, e := range knownissues.All() {
		var buf bytes.Buffer
		if err := runKnownIssues([]string{e.Kind}, &buf); err != nil {
			t.Fatalf("runKnownIssues(%q) error = %v", e.Kind, err)
		}
		inChecks := false
		for _, ln := range strings.Split(buf.String(), "\n") {
			switch {
			case strings.HasPrefix(ln, "What to check"):
				inChecks = true
				continue
			case strings.HasPrefix(ln, "Likely causes"):
				inChecks = false
				continue
			}
			if inChecks || strings.Contains(ln, "://") {
				continue
			}
			if n := len([]rune(ln)); n > 72 && len(strings.Fields(ln)) > 1 {
				t.Errorf("%q: line is %d columns: %q", e.Kind, n, ln)
			}
		}
	}
}

func TestWrapProse(t *testing.T) {
	got := wrapProse("one two three four", "  - ", "    ", 12)
	want := []string{"  - one two", "    three", "    four"}
	if len(got) != len(want) {
		t.Fatalf("wrapProse() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A word that cannot fit the budget is emitted whole rather than broken: a
// truncated identifier is less readable than an over-long line.
func TestWrapProseKeepsAnOverlongWordWhole(t *testing.T) {
	got := wrapProse("aaaaaaaaaaaaaaaaaaaa b", "  ", "  ", 10)
	if len(got) != 2 || got[0] != "  aaaaaaaaaaaaaaaaaaaa" || got[1] != "  b" {
		t.Errorf("wrapProse() = %q", got)
	}
}

// An unknown kind names what was asked for and what is available, and the %q
// verb renders it as a Go string literal — so a control byte spliced into the
// argument prints escaped rather than reaching the terminal. That is why this
// path needs no safetext call.
func TestRunKnownIssuesRejectsAnUnknownKind(t *testing.T) {
	var buf bytes.Buffer
	err := runKnownIssues([]string{"NoSuchKind"}, &buf)
	if err == nil {
		t.Fatal("runKnownIssues() accepted an unknown kind")
	}
	if !strings.Contains(err.Error(), `"NoSuchKind"`) {
		t.Errorf("error = %v, want it to name the kind", err)
	}
	if !strings.Contains(err.Error(), "OOMKilled") {
		t.Errorf("error = %v, want it to list what is documented", err)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %q to the output on an error", buf.String())
	}
}

// The message must be honest about coverage rather than implying the operator
// mistyped. This reference documents the deterministic detector set; a kind
// from elsewhere in kubeagent — NoEndpoints, RolloutStuck, JobFailed — is real
// and simply not in it yet, so the message says where those are explained.
func TestUnknownKindMessageIsHonestAboutCoverage(t *testing.T) {
	err := runKnownIssues([]string{"NoEndpoints"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("runKnownIssues() accepted an unknown kind")
	}
	if !strings.Contains(err.Error(), "detector set") {
		t.Errorf("error = %v, want it to say what the reference covers", err)
	}
	if !strings.Contains(err.Error(), "https://k8sproject.top/features/diagnostics/") {
		t.Errorf("error = %v, want it to point at where the rest are explained", err)
	}
}

func TestRunKnownIssuesEscapesAControlByte(t *testing.T) {
	err := runKnownIssues([]string{"\x1b[31mred"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("runKnownIssues() accepted an unknown kind")
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Errorf("error carries a raw escape byte: %q", err.Error())
	}
}

// Lookup is exact, and the command does not soften it.
func TestRunKnownIssuesDoesNotCaseFold(t *testing.T) {
	if err := runKnownIssues([]string{"oomkilled"}, &bytes.Buffer{}); err == nil {
		t.Error("runKnownIssues() matched a lowercase kind; Lookup is exact")
	}
}

// More than one argument is a usage error in the shape `schema` already uses.
func TestRunKnownIssuesRejectsTwoArguments(t *testing.T) {
	err := runKnownIssues([]string{"OOMKilled", "RestartLoop"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("runKnownIssues() accepted two arguments")
	}
	if !strings.Contains(err.Error(), "usage:") || !strings.Contains(err.Error(), "known-issues [kind]") {
		t.Errorf("error = %v, want the usage line", err)
	}
}

// The command is registered, takes no flags at all — not even --kubeconfig —
// and offers the kinds for completion.
func TestKnownIssuesCommandShape(t *testing.T) {
	cmd := newKnownIssuesCommand()
	if !cmd.SilenceErrors || !cmd.SilenceUsage {
		t.Error("the command must silence Cobra's own error and usage rendering")
	}
	if cmd.Flags().Lookup("kubeconfig") != nil {
		t.Error("known-issues registers --kubeconfig; there is no cluster on this path")
	}
	// Cobra registers its own --help lazily, inside Execute, so a freshly
	// constructed command has genuinely no flags at all.
	if cmd.Flags().HasFlags() {
		t.Error("known-issues declares a flag; it must take none")
	}
	if len(cmd.ValidArgs) != 0 {
		t.Error("ValidArgs is set; it would let Cobra reword the unknown-kind error")
	}
	if cmd.ValidArgsFunction == nil {
		t.Fatal("no ValidArgsFunction; completion would offer filenames")
	}
	got, _ := cmd.ValidArgsFunction(cmd, nil, "")
	if len(got) != len(knownissues.Kinds()) {
		t.Errorf("completion offers %d kinds, want %d", len(got), len(knownissues.Kinds()))
	}
}

func TestKnownIssuesIsRegistered(t *testing.T) {
	var found bool
	for _, c := range newRootCommand().Commands() {
		if c.Name() == "known-issues" {
			found = true
		}
	}
	if !found {
		t.Error("known-issues is not registered on the root command")
	}
}
```

`newRootCommand` is verified to exist at `internal/cli/root.go:98`, and
`internal/cli/cobra_behaviour_test.go` already reaches the command tree through
it — use it as written above.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli/ -run KnownIssues
```

Expected: FAIL to compile — `undefined: runKnownIssues`, `undefined:
newKnownIssuesCommand`, `undefined: wrapProse`.

- [ ] **Step 3: Write `internal/cli/knownissues.go`**

```go
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/knownissues"
)

// proseWidth is the column budget for wrapped prose. Checks are exempt: a
// wrapped command line cannot be pasted, which is the only thing that section
// is for.
const proseWidth = 72

// runKnownIssues prints kubeagent's offline reference for one failure kind, or
// lists every documented kind.
//
// It reads a compiled-in slice and nothing else: no cluster connection, no
// kubeconfig, no network — and, separately, no model call. Nothing here is
// generated and nothing here is sent anywhere; this is not a smaller --explain.
func runKnownIssues(args []string, w io.Writer) error {
	if len(args) == 0 {
		for _, e := range knownissues.All() {
			fmt.Fprintf(w, "  %-28s %s\n", e.Kind, e.Summary)
		}
		fmt.Fprintf(w, "\nPrint one:\n  %s known-issues <kind>\n", invokedAs)
		return nil
	}
	if len(args) > 1 {
		return fmt.Errorf("usage: %s known-issues [kind]", invokedAs)
	}
	entry, ok := knownissues.Lookup(args[0])
	if !ok {
		// %q renders the argument as a Go string literal, so a control byte
		// spliced into it prints as \x1b rather than reaching the terminal.
		// That is why this path needs no safetext call of its own.
		//
		// The message says what this reference covers rather than implying the
		// kind is invalid. NoEndpoints, RolloutStuck, JobFailed and the rest of
		// what kubeagent reports are real kinds from the workload and cluster
		// passes; they are not in the detector set this slice documents, so the
		// message points at where they are explained instead.
		return fmt.Errorf("unknown issue kind %q; kubeagent documents the deterministic detector set (%s). "+
			"Other findings are explained at https://k8sproject.top/features/diagnostics/",
			args[0], strings.Join(knownissues.Kinds(), ", "))
	}
	printEntry(w, entry)
	return nil
}

// printEntry renders one entry in full.
func printEntry(w io.Writer, e knownissues.Entry) {
	fmt.Fprintf(w, "%s\n", e.Kind)
	for _, ln := range wrapProse(e.Detail, "  ", "  ", proseWidth) {
		fmt.Fprintf(w, "%s\n", ln)
	}

	fmt.Fprintf(w, "\nLikely causes\n")
	for _, c := range e.Causes {
		for _, ln := range wrapProse(c, "  - ", "    ", proseWidth) {
			fmt.Fprintf(w, "%s\n", ln)
		}
	}

	fmt.Fprintf(w, "\nWhat to check\n")
	for _, c := range e.Checks {
		fmt.Fprintf(w, "  - %s\n", c)
	}

	if e.Docs != "" {
		fmt.Fprintf(w, "\n  %s\n", e.Docs)
	}
}

// wrapProse breaks s onto lines whose rendered width — the prefix plus the
// text — is at most width runes. The first line carries first, every later one
// carries cont.
//
// Runes, not bytes: the prose uses em dashes, and counting their three bytes
// would wrap short. A single word wider than the budget is emitted whole on its
// own line rather than broken, because a split identifier or command fragment
// is harder to read than a long line.
func wrapProse(s, first, cont string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var (
		out    []string
		line   = first + words[0]
		prefix = cont
	)
	for _, word := range words[1:] {
		if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > width {
			out = append(out, line)
			line = prefix + word
			continue
		}
		line += " " + word
	}
	return append(out, line)
}

// newKnownIssuesCommand builds `kubeagent known-issues`.
//
// It keeps its own argument handling rather than cobra.MaximumNArgs(1), for the
// same reason `schema` does: that would reword the usage error runKnownIssues
// already produces. ValidArgsFunction rather than ValidArgs, likewise — the
// former only feeds completion, while the latter would make Cobra validate the
// argument itself and replace the unknown-kind error with its own.
//
// No flags at all, not even --kubeconfig: there is no cluster in this path.
func newKnownIssuesCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "known-issues [kind]",
		Short:         "Print what kubeagent knows about a failure kind, offline",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return knownissues.Kinds(), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKnownIssues(args, os.Stdout)
		},
	}
}
```

- [ ] **Step 4: Register the command**

In `internal/cli/root.go`, add `newKnownIssuesCommand()` to the existing
`root.AddCommand(...)` call, immediately after `newSchemaCommand()` — the two
are the offline pair, and grouping them makes that legible:

```go
	root.AddCommand(newVersionCommand(), newSchemaCommand(), newKnownIssuesCommand(), newMCPCommand(),
		newTUICommand(), newScanCommand(), newWatchCommand(), newGateCommand(), newFleetCommand(),
		newRBACCommand(), newPolicyCommand(), newBaselineCommand(), newCompletionCommand())
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli/ -run KnownIssues -v
```

Expected: PASS, every test.

- [ ] **Step 6: Eyeball the rendering**

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o /tmp/kubeagent-ki . && /tmp/kubeagent-ki known-issues && /tmp/kubeagent-ki known-issues OOMKilled
```

Expected: the list view with thirteen aligned rows and a `Print one:` footer,
then the OOMKilled entry matching the spec's example. Delete `/tmp/kubeagent-ki`
afterwards; it is a scratch binary and must not be committed.

- [ ] **Step 7: Run the full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
```

Expected: all packages PASS. In particular `internal/report`'s golden test must
still pass untouched — nothing in this task changes `scan`'s rendering. Confirm
with `git status --short` that `internal/report/testdata/golden-scan.txt`,
`go.mod` and `go.sum` are unmodified.

- [ ] **Step 8: Commit**

```bash
git add internal/cli/knownissues.go internal/cli/knownissues_test.go internal/cli/root.go
git commit -s -m "feat(cli): kubeagent known-issues [kind]

Lists every documented failure kind, or prints one in full: what it
means, what usually causes it, and what to read next. Modeled on
kubeagent schema — ArbitraryArgs with the validation in RunE so the
error text is kubeagent's, ValidArgsFunction for completion rather
than ValidArgs, which Cobra would use to reword that error.

No flags at all, not even --kubeconfig: the command reads a compiled-in
slice and touches no cluster. Separately, it makes no model call.
Prose wraps at 72 columns; checks never wrap, because a wrapped command
line cannot be pasted."
```

---

## Task 4: Documentation

**Files:**
- Create: `website/docs/features/known-issues.md`
- Modify: `website/docs/features/diagnostics.md` — a pointer under `## Failure modes detected`
- Modify: `website/mkdocs.yml` — nav, after `features/diagnostics.md` (line 57)
- Modify: `CHANGELOG.md` — under `## [Unreleased]`
- Modify: `CLAUDE.md` — three edits, below
- Modify: `website/docs/roadmap.md` — the post-1.0 row

**Interfaces:**
- Consumes: the command and its output from Task 3. Every example in the page must be the binary's real output — generate it, do not write it from memory.
- Produces: nothing code-facing.

- [ ] **Step 1: Capture the real output**

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o /tmp/kubeagent-ki .
/tmp/kubeagent-ki known-issues
/tmp/kubeagent-ki known-issues OOMKilled
/tmp/kubeagent-ki known-issues NoSuchKind ; echo "exit=$?"
```

Paste these into the page verbatim in Step 2. Delete `/tmp/kubeagent-ki` when
done.

- [ ] **Step 2: Write `website/docs/features/known-issues.md`**

```markdown
# Known issues reference

A `scan` names what is wrong: `CrashLoopBackOff`, `Init:OOMKilled`,
`VolumeAttachError`. `kubeagent known-issues` says what those names mean —
what the failure actually is, what usually causes it, and what to read next —
without a cluster, a kubeconfig, or a network connection.

```bash
kubeagent known-issues            # every documented kind, one line each
kubeagent known-issues OOMKilled  # one kind in full
```

```text
$ kubeagent known-issues
  CrashLoopBackOff             a container starts, exits, and is restarted on a widening backoff
  CreateContainerConfigError   the kubelet cannot build the container from its spec
  ErrImagePull                 the kubelet's attempt to pull the image failed
  ImagePullBackOff             repeated pull failures, now backing off between attempts
  Init:CrashLoopBackOff        an init container is crash-looping, so the pod never starts
  Init:ErrImagePull            an init container's image could not be pulled
  Init:ImagePullBackOff        an init container's image pull is backing off
  Init:OOMKilled               an init container was killed for exceeding its memory limit
  OOMKilled                    the kernel killed a container for exceeding its memory limit
  ProbeFailure                 a container is running but a probe keeps failing
  RestartLoop                  a container keeps exiting and restarting while still Running
  Unschedulable                no node can place the pod
  VolumeAttachError            a volume cannot be attached, so the container never starts

Print one:
  kubeagent known-issues <kind>
```

```text
$ kubeagent known-issues OOMKilled
OOMKilled
  The kernel killed a container for exceeding its memory limit.

Likely causes
  - The limit is lower than the workload's real steady-state usage.
  - A leak: usage climbs until the limit is reached, then repeats on a cycle.
  - A runtime heap sized above the container limit, so the runtime never
    reclaims before the kernel intervenes.

What to check
  - kubectl -n <namespace> describe pod <pod> — lastState.terminated.exitCode 137
  - kubectl -n <namespace> top pod <pod> — usage against the configured limit
  - The container's own memory tuning against resources.limits.memory

  https://k8sproject.top/features/diagnostics/#oomkilled
```

Both blocks above are what the renderer must produce, copied from the spec.
Diff them against the Step 1 capture: if they differ, the renderer is wrong, not
the page. Paste the real capture only once it matches.

## Guarantees

`kubeagent known-issues` **touches no cluster at all.** It is not merely
read-only: there is no client, no context, and no kubeconfig on this path —
the command takes no flags, not even `--kubeconfig`.

Separately and additionally, it **makes no LLM call.** The text is curated
prose compiled into the binary. Nothing is generated at run time and nothing
is sent anywhere. Those are two different promises and neither implies the
other — `--explain` is the model path, and this is not a smaller version of
it.

## The vocabulary is closed

`kubeagent known-issues` documents exactly the thirteen kinds the
deterministic detector set can report, and the repository proves it rather
than asserting it. Three tests in `internal/diagnose` run on every `go test`:

- a `go/parser` walk over the detector sources, checking every string literal
  that reaches a finding's issue field;
- a fixture table that drives all nine detectors to produce all thirteen
  kinds and looks each one up in the registry — this is what covers the kinds
  composed at run time, which the parser cannot see;
- the reverse check, refusing an entry for a kind no detector emits.

Adding a detector that emits a new kind fails the build's tests until the kind
is documented. That is the point of the slice: the reference cannot drift from
the code.

## What a kind is

The `Kind` is the exact issue string a scan prints, copied verbatim rather
than restated more prettily, because it is the join between the two outputs.
Lookup is exact — no case folding, no fuzzy match, and no falling back from
`Init:OOMKilled` to `OOMKilled`:

```text
$ kubeagent known-issues oomkilled
error: unknown issue kind "oomkilled"; kubeagent documents the deterministic
detector set (…). Other findings are explained at
https://k8sproject.top/features/diagnostics/
```

Paste the real capture from Step 1 here, wrapped exactly as the terminal
produced it.

An init container killed for memory blocks the pod from ever starting. That is
a different failure from the same reason on a main container, with different
causes and different next steps, so it is a different entry.

## What the entries may name

Every command line in a **What to check** section uses placeholders —
`<namespace>`, `<pod>`, `<container>`, `<node>`, `<name>` — never a real
object name. The only host that appears anywhere in the reference is the
project's own documentation site, in the per-entry link, and a test asserts
that no address or hostname reaches the prose.

## Not in this slice

Deliberately absent:

- **Kinds outside the detector set.** `NoEndpoints`, `RolloutStuck`,
  `JobFailed`, `FailedCreate` and the rest are real findings from the workload
  and cluster passes. They are not statically enumerable the way the detector
  set is, so documenting them here could not carry the same guarantee. They
  keep their prose in [Failure diagnostics](diagnostics.md).
- **A link from `scan` output to an entry.** `scan`'s rendering is unchanged.
- **JSON output.** This is a reference for a person, not a document to
  forward, so it adds no ninth [versioned document](json-schema.md).
- **Operator-supplied entries.** The registry ships with the binary; it is
  curated, not extensible at run time.
```

- [ ] **Step 3: Add the pointer in `diagnostics.md`**

Immediately below the `## Failure modes detected` heading (line 12), before the
first `### CrashLoopBackOff` section, insert:

```markdown
Every kind in this section is also in the binary's own offline reference —
`kubeagent known-issues <kind>` prints what it means, what usually causes it,
and what to check, with no cluster and no network. See
[Known issues reference](known-issues.md).
```

- [ ] **Step 4: Add the nav entry**

In `website/mkdocs.yml`, directly after the `- Failure diagnostics:
features/diagnostics.md` line (line 57), matching its indentation exactly:

```yaml
      - Known issues reference: features/known-issues.md
```

- [ ] **Step 5: Add the CHANGELOG entry**

Under `## [Unreleased]` in `CHANGELOG.md`, add an `### Added` section (or
append to one if it already exists):

```markdown
### Added

- **`kubeagent known-issues [kind]` — an offline reference for every failure
  kind the detectors report.** With no argument it lists all thirteen kinds
  with a one-line summary; with a kind it prints that failure in full — what it
  means, its likely causes most common first, and read-only next steps whose
  object names are placeholders. No cluster, no kubeconfig, no network, and no
  flags at all. Separately: it makes no LLM call — the text is curated prose
  compiled into the binary, not generated.
- **The detector issue vocabulary is now machine-checked.** Three tests in
  `internal/diagnose` keep the reference and the detectors in step: a
  `go/parser` walk over every string literal reaching a finding's issue field, a
  fixture table driving all nine detectors to produce all thirteen kinds, and a
  reverse check refusing an entry for a kind nothing emits. A new detector that
  emits an undocumented kind fails the suite.
```

- [ ] **Step 6: Update `CLAUDE.md`**

Three edits, all in the **Invariants** section unless noted:

1. The `--kubeconfig` sentence currently reads "`--kubeconfig` appears on eight
   commands, and two of the remaining ones deliberately do not accept it."
   Change **two** to **three** — `known-issues` is the third deliberate
   abstainer, and for the strongest reason: there is no cluster on its path at
   all.
2. After the `internal/glob` paragraph in the imports-nothing list, add:

   ```markdown
     `internal/knownissues` (the `known-issues` reference) joins the same
     stdlib-only list: the curated entry per issue kind the detector set can
     emit, as a Go slice literal — no data file, no parser, no dependency.
     `internal/knownissues/imports_test.go` enforces both halves, on
     `internal/baseline/imports_test.go`'s pattern. It holds no client and no
     context, issues no cluster call and makes no model call — two separate
     promises. The completeness check cannot live inside a package that imports
     nothing, so it lives in `internal/diagnose/knownissues_test.go`, where both
     the registry and the detectors are in scope: a `go/parser` walk over every
     `Issue:` literal, a fixture table driving all nine detectors to all
     thirteen kinds, and a reverse check. The vocabulary is closed at thirteen
     because both apparently-dynamic sites — `imagepull.go` and
     `initcontainer.go` — are guarded to two reasons each
     (see [website/docs/features/known-issues.md](website/docs/features/known-issues.md)).
   ```

3. In the **Roadmap** section, extend the final paragraph's last sentence. It
   currently reads "The remaining post-1.0 work is the rest of fleet-scale —
   cross-cluster correlation, and selection from something other than a
   kubeconfig — and a curated community detector library with a known-issues
   knowledge base." Replace the trailing clause so it records what shipped:

   ```markdown
   - **Post-1.0 — the known-issues knowledge base, slice 1 has shipped:**
     `kubeagent known-issues [kind]` prints kubeagent's own reference for the
     thirteen kinds `diagnose.DefaultDetectors` can emit, from a curated Go
     slice literal in `internal/knownissues` — no cluster, no kubeconfig, no
     network, no flags, and no model call. The vocabulary is closed and proved
     closed: three tests in `internal/diagnose` fail the suite if a detector
     emits a kind the reference does not document, or the reference documents a
     kind no detector emits
     (see [website/docs/features/known-issues.md](website/docs/features/known-issues.md)).
     The remaining post-1.0 work is the rest of fleet-scale — cross-cluster
     correlation, and selection from something other than a kubeconfig — and
     the second half of this item: a curated community detector library.
   ```

   Adjust the surrounding sentence so the "remaining work" list is stated once,
   not twice.

- [ ] **Step 7: Update `website/docs/roadmap.md`**

Find the post-1.0 row for the curated detector library / known-issues knowledge
base and mark slice 1 shipped, in the same style the fleet and baseline rows
already use on that page. Read the surrounding rows first and match their
wording, headings and link style rather than inventing a new shape.

- [ ] **Step 8: Build the site strictly**

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: exit 0, no `WARNING` lines naming `known-issues.md` or
`diagnostics.md`. The red "Material for MkDocs 2.0" banner is cosmetic. A
warning about an unresolved anchor is a real failure — fix the link.

Then `cd` back to the repository root and delete the generated `website/site/`
directory if the build produced one; it is not committed.

- [ ] **Step 9: Run the full suite once more**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
```

Expected: all packages PASS.

- [ ] **Step 10: Commit**

```bash
git add website/docs/features/known-issues.md website/docs/features/diagnostics.md \
        website/mkdocs.yml CHANGELOG.md CLAUDE.md website/docs/roadmap.md
git commit -s -m "docs: known-issues reference, and the closed vocabulary written down

A feature page with the binary's real output, a pointer from the
diagnostics page, the nav entry, and the changelog note. CLAUDE.md
records the third deliberate --kubeconfig abstainer, the new
stdlib-only package and where its completeness check lives, and the
roadmap marks slice 1 of the last post-1.0 item shipped."
```

Stage by name, never `git add -A` or `git add .`: `docs/go-concepts.md`,
`docs/testing/` and `.superpowers/` are gitignored, and a blanket add has
picked up scratch files in this repository before.

---

## Notes for the executor

- **Do not run `./chaos/run.sh`.** Nothing in this plan needs a cluster.
- **Do not run any test with `-update`.** No golden file and no schema changes.
- The one Go-learning obligation: this slice introduces `go/ast` traversal
  (`ast.Inspect`, `*ast.KeyValueExpr`, `*ast.BinaryExpr`) as a new concept. If
  `docs/go-concepts.md` does not already cover walking a Go syntax tree, append
  an entry in the established style — a plain everyday example first, then the
  kubeagent example. That file is gitignored, so it is never staged.
- After the last task, the branch is ready for the whole-branch review. Do not
  merge or tag.
