# Fleet cross-cluster correlation ("shared signals") Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `kubeagent fleet` gains two sections naming which issue kinds and which refused reads appear in two or more of the judged clusters, most widespread first — computed from data the sweep already reads.

**Architecture:** `summarize` starts returning a per-cluster *evidence set* alongside the row it already returns; `Sweep` collects those sets and hands them to a new pure `correlate` in `internal/fleet/correlate.go`; `RenderText` grows two sections and `Report` grows one `omitempty` property. No new package, no new cluster read, no new flag, no verdict change.

**Tech Stack:** Go 1.26, standard library only (`sort`, `strings`, `fmt`, `io`, `encoding/json` — all already imported by the package). No new dependency.

**Spec:** [docs/superpowers/specs/2026-08-07-fleet-correlation-shared-signals-design.md](../specs/2026-08-07-fleet-correlation-shared-signals-design.md), committed `de135c0` on `main`.

## Global Constraints

Every task's requirements implicitly include this section.

- **READ-ONLY toward every cluster swept:** get/list only, no write of any kind, no `--fix` path. **SEPARATELY AND ADDITIONALLY: no LLM call.** These are two promises, not one — never blur them, and never let a comment, doc line, help string or commit message suggest a correlation is related to `--explain`, which is the model path.
- `internal/fleet` must never import `internal/remediate` or `internal/explain`. **No new package in this slice, so no new wall to declare:** `correlate.go` inherits fleet's, and `internal/fleet/imports_test.go` already globs `*.go` in the directory including test files. **Do NOT add a second imports test.**
- **NO NEW DEPENDENCY:** `go.mod` and `go.sum` must not change. `sort`, `strings`, `fmt` are standard library and already imported by the package.
- **CREDENTIALS:** no secrets, credentials, private IPs or internal hostnames anywhere — code, tests, fixtures, docs, help text, schema descriptions. Documentation IPs are RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains RFC 2606 (`example.com`/`.org`/`.net`). URLs are credentials — nothing beyond `scheme://host`, and the project's own `https://k8sproject.top` links are the only permitted host. Kubeconfig paths and Kubernetes node names are credentials. Test context names are invented and generic (`example-a`, `prod-eu`, `staging`, …). **The correlation reads `Blindspot.Resource` and NEVER `Blindspot.Reason`.**
- **ONLY `fleet`'s `schemaVersion` moves, 1.0 → 1.1.** `scan` stays 1.2, `gate` stays 1.1, the other five do not move. Regenerate EXACTLY ONCE with `go test ./internal/schemadoc -run TestSchemaDrift -update` in Task 4; never run any other test with `-update`, and never run `-update` in a review.
- `internal/report/testdata/golden-scan.txt` must stay **BYTE-IDENTICAL** — fleet has no `scan` render path, so it cannot move. Do NOT regenerate the demo GIF or `website/docs/quickstart.md`.
- **The verdict is NOT touched:** `decide()` is unchanged, and a correlation adds no severity. A test pins verdict and exit code identical to slice 1 with a correlation present.
- **TDD:** write the failing test first, watch it fail, then implement. Everything in this slice is a pure function over values built in the test — no cluster, no fake clientset for the correlation logic, no network anywhere in this plan.
- Go lives at `/usr/local/go/bin` — `export PATH=$PATH:/usr/local/go/bin`. `go test` runs with `-p 2` locally, never `-short`.
- Every commit needs `git commit -s` (DCO enforced on `main`), authored solely by the human — **NO `Co-Authored-By` trailer and no AI attribution of any kind, anywhere**, including the commit body.

### DANGER

**NEVER run `./chaos/run.sh` in any form.** A run takes ~40 minutes and injects real outages into a real cluster. No task in this plan creates, deletes, or touches any cluster; nothing here needs one.

### Three semantics that are easy to get wrong

1. **Evidence is a SET, not a count.** A kind hitting four hundred pods in one cluster is **one** cluster. A count-weighted fold would let a single noisy cluster manufacture a fleet-wide signal that does not exist.
2. **The denominator is the count of JUDGED clusters** (`len(rep.Clusters)`), **not selected ones.** An unreachable cluster produced no verdict and could not have contributed a signal; counting it would make a 2-of-2 correlation read as 2-of-5 and understate it. The header word **"judged" is CONSTANT**, never conditional on whether any cluster was unreachable.
3. **An empty section is OMITTED ENTIRELY, header included.** A heading over nothing reads as a failed render. Same for the JSON: `omitempty` means a sweep with no correlation encodes no `shared` key at all, and a v1.7.0 consumer is unaffected.

### One note on the spec's ASCII sample

The spec's rendered-output block uses **illustrative** column spacing. **This plan's expected strings are the authoritative bytes** — they were computed from the format string in Task 3 and are what the tests assert. Where the two differ in whitespace, the plan wins; do not "correct" the plan to match the spec.

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/fleet/correlate.go` (new) | `Shared`, `SourceIssue`/`SourceBlindspot`, `minShared`, `clusterEvidence`, the pure `correlate` fold and its sort helpers | 1 |
| `internal/fleet/correlate_test.go` (new) | Every `correlate` behaviour: threshold, ordering, source grouping, determinism | 1 |
| `internal/fleet/summarize.go` | `summarize` gains a second return value, the evidence set | 2 |
| `internal/fleet/fleet.go` | `Report.Shared`; `Sweep` collects evidence and calls `correlate`; package comment (Task 5) | 2, 5 |
| `internal/fleet/summarize_test.go` | Five existing call sites updated; three new evidence tests; one stale comment corrected | 2 |
| `internal/fleet/fleet_test.go` | Gating test, one-cluster test, the whole-report credential walk extended over `Shared` | 2 |
| `internal/fleet/render.go` | `maxNamedClusters`, `renderShared`, `countCell`, `namedClusters`; one call added to `RenderText` | 3 |
| `internal/fleet/render_test.go` | `sharedReport()` fixture, exact-bytes test, section omission, `+N more`, denominator, JSON tests, write-failure table | 3, 4 |
| `internal/jsonschema/jsonschema.go` | `FleetVersion` 1.0 → 1.1 | 4 |
| `website/docs/schemas/fleet-v1.json` | Regenerated, never hand-edited | 4 |
| `website/docs/features/fleet.md`, `CHANGELOG.md`, `CLAUDE.md`, `website/docs/roadmap.md` | Documentation | 5 |

**`internal/cli/fleet.go` needs NO change.** It calls `fleet.Sweep` and hands the `Report` to `RenderText`/`RenderJSON`; both pick the new field up for free. **There is no new flag in this slice** — do not go looking for one to add.

### The schema drift test fails between Task 2 and Task 4 — this is expected

Adding `Report.Shared` in Task 2 changes the fleet document's shape, so from the moment Task 2 lands until Task 4 runs, `go test ./internal/schemadoc` **FAILS** with a drift report naming the change additive. **Nothing is broken.** Task 2's and Task 3's "run the suite" steps scope themselves to `./internal/fleet/...` for exactly this reason, and Task 4 is the only place `-update` is permitted — exactly once, on `TestSchemaDrift`.

---

## Task 1: The `Shared` type and the pure `correlate` fold

**Files:**

- Create: `internal/fleet/correlate.go`
- Create: `internal/fleet/correlate_test.go`

**Interfaces:**

- Consumes: nothing from earlier tasks. This task touches no existing file.
- Produces, all used verbatim by Tasks 2 and 3:
  - `type Shared struct { Signal string; Source string; Clusters []string }` with JSON tags `signal`, `source`, `clusters`
  - `const SourceIssue = "issue"`, `const SourceBlindspot = "blindspot"`
  - `const minShared = 2`
  - `type clusterEvidence struct { context string; issues map[string]bool; blindspots map[string]bool }` (unexported)
  - `func correlate(evidence []clusterEvidence) []Shared`

- [ ] **Step 1: Cut the branch**

```bash
export PATH=$PATH:/usr/local/go/bin
cd /home/ubuntu/git/kubeagent
git checkout main
git status --short           # must be empty
git rev-parse HEAD           # must be de135c0...
git checkout -b fleet-correlation
```

- [ ] **Step 2: Write the failing tests**

Create `internal/fleet/correlate_test.go` with exactly this content:

```go
package fleet

import (
	"reflect"
	"testing"
)

// ev builds one cluster's evidence from two literal lists. Sets, not counts —
// see TestCorrelateCountsAClusterOnceHoweverLoudItIs in summarize_test.go for
// why that distinction is load-bearing.
func ev(context string, issues, blindspots []string) clusterEvidence {
	e := clusterEvidence{context: context, issues: map[string]bool{}, blindspots: map[string]bool{}}
	for _, i := range issues {
		e.issues[i] = true
	}
	for _, b := range blindspots {
		e.blindspots[b] = true
	}
	return e
}

// A signal in one cluster is not a correlation, and neither is a sweep of one
// cluster however much that cluster reports.
func TestCorrelateNeedsTwoClusters(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []clusterEvidence
	}{
		{"no clusters at all", nil},
		{"one cluster, however much it reports", []clusterEvidence{
			ev("prod-eu", []string{"ImagePullBackOff", "OOMKilled"}, []string{"nodes/proxy"}),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := correlate(tc.in); got != nil {
				t.Errorf("correlate() = %+v, want nil", got)
			}
		})
	}
}

// Three clusters, one signal in two of them and two signals in one each. Only
// the shared one survives.
func TestCorrelateKeepsOnlySignalsInTwoOrMoreClusters(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("prod-eu", []string{"ImagePullBackOff", "OOMKilled"}, nil),
		ev("prod-us", []string{"ImagePullBackOff"}, nil),
		ev("staging", []string{"Unschedulable"}, nil),
	})

	want := []Shared{
		{Signal: "ImagePullBackOff", Source: SourceIssue, Clusters: []string{"prod-eu", "prod-us"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("correlate() = %+v, want %+v", got, want)
	}
}

// Nothing shared at all is nil, not an empty slice, so Report's omitempty drops
// the key rather than encoding [].
func TestCorrelateWithNothingInCommonIsNil(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("prod-eu", []string{"ImagePullBackOff"}, []string{"nodes/proxy"}),
		ev("prod-us", []string{"OOMKilled"}, []string{"pods/log"}),
	})
	if got != nil {
		t.Errorf("correlate() = %+v, want nil so omitempty drops the key", got)
	}
}

// Most widespread first; equal counts break by signal name ascending. Go
// randomizes map iteration, so the tiebreak is not a nicety.
func TestCorrelateOrdersByClusterCountThenName(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("example-a", []string{"bbb", "zzz", "aaa"}, nil),
		ev("example-b", []string{"bbb", "zzz", "aaa"}, nil),
		ev("example-c", []string{"bbb"}, nil),
	})

	want := []Shared{
		{Signal: "bbb", Source: SourceIssue, Clusters: []string{"example-a", "example-b", "example-c"}},
		{Signal: "aaa", Source: SourceIssue, Clusters: []string{"example-a", "example-b"}},
		{Signal: "zzz", Source: SourceIssue, Clusters: []string{"example-a", "example-b"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("correlate() = %+v, want %+v", got, want)
	}
}

// Source rank beats cluster count: the two-cluster issue below precedes the
// three-cluster blind spot. A plain string compare would invert this, because
// "blindspot" sorts before "issue".
func TestCorrelatePutsEveryIssueBeforeEveryBlindSpot(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("example-a", []string{"zzz-issue"}, []string{"aaa-resource", "bbb-resource"}),
		ev("example-b", []string{"zzz-issue"}, []string{"aaa-resource", "bbb-resource"}),
		ev("example-c", nil, []string{"aaa-resource"}),
	})

	want := []Shared{
		{Signal: "zzz-issue", Source: SourceIssue, Clusters: []string{"example-a", "example-b"}},
		{Signal: "aaa-resource", Source: SourceBlindspot,
			Clusters: []string{"example-a", "example-b", "example-c"}},
		{Signal: "bbb-resource", Source: SourceBlindspot, Clusters: []string{"example-a", "example-b"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("correlate() = %+v, want %+v", got, want)
	}
}

// An issue kind and an API resource name are different vocabularies that happen
// to share a namespace of strings. Folding them together would report one
// signal where there are two.
func TestCorrelateKeepsAnIssueAndABlindSpotOfTheSameNameApart(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("example-a", []string{"events"}, []string{"events"}),
		ev("example-b", []string{"events"}, []string{"events"}),
	})

	want := []Shared{
		{Signal: "events", Source: SourceIssue, Clusters: []string{"example-a", "example-b"}},
		{Signal: "events", Source: SourceBlindspot, Clusters: []string{"example-a", "example-b"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("correlate() = %+v, want %+v", got, want)
	}
}

// Context names within an entry are ascending, so two runs over the same fleet
// render identical bytes.
func TestCorrelateNamesClustersAscending(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("zulu", []string{"OOMKilled"}, nil),
		ev("alpha", []string{"OOMKilled"}, nil),
		ev("mike", []string{"OOMKilled"}, nil),
	})

	want := []string{"alpha", "mike", "zulu"}
	if len(got) != 1 {
		t.Fatalf("correlate() = %+v, want one entry", got)
	}
	if !reflect.DeepEqual(got[0].Clusters, want) {
		t.Errorf("Clusters = %v, want %v", got[0].Clusters, want)
	}
}

// The fold walks two maps, and Go randomizes map iteration order per run. Every
// tiebreak in the sort exists to make this loop pass.
func TestCorrelateIsDeterministicAcrossRuns(t *testing.T) {
	in := []clusterEvidence{
		ev("example-a", []string{"ppp", "qqq", "rrr", "sss"}, []string{"ttt", "uuu"}),
		ev("example-b", []string{"ppp", "qqq", "rrr", "sss"}, []string{"ttt", "uuu"}),
	}

	first := correlate(in)
	if len(first) != 6 {
		t.Fatalf("correlate() = %+v, want six shared signals", first)
	}
	for i := 0; i < 50; i++ {
		if got := correlate(in); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs:\n got %+v\nwant %+v", i, got, first)
		}
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
go test ./internal/fleet/ -run TestCorrelate 2>&1 | head -20
```

Expected: FAIL to **compile**, with errors naming `correlate`, `clusterEvidence`, `Shared`, `SourceIssue` and `SourceBlindspot` as undefined. A compile failure is the correct red state here — there is nothing to call yet.

- [ ] **Step 4: Write the implementation**

Create `internal/fleet/correlate.go` with exactly this content:

```go
package fleet

import "sort"

// minShared is how many clusters a signal must appear in before it is a
// correlation. Two, and not configurable: one cluster is not a pattern, and
// every number above two is an arbitrary line an operator would have to learn.
const minShared = 2

// Shared is one signal that appeared in minShared or more judged clusters.
//
// There is no count field on purpose: len(Clusters) is the count, and a stored
// duplicate is a defect waiting to disagree with the array beside it.
//
// Source is not decoration. The two vocabularies are different kinds of fact —
// an issue kind is something wrong inside the cluster, an API resource name is
// something kubeagent was not allowed to look at — and a consumer reading the
// JSON has no other way to tell them apart. The text renderer carries the same
// distinction as two labelled sections.
type Shared struct {
	Signal   string   `json:"signal"`
	Source   string   `json:"source"`
	Clusters []string `json:"clusters"`
}

// The fixed Shared.Source vocabulary.
const (
	SourceIssue     = "issue"
	SourceBlindspot = "blindspot"
)

// clusterEvidence is what one judged cluster contributes to a correlation:
// which signals it showed, not how many times it showed them.
//
// Sets, not counts, and that is load-bearing. A kind hitting four hundred pods
// in one cluster is still one cluster. A count-weighted fold would let a single
// noisy cluster manufacture a fleet-wide signal that does not exist.
type clusterEvidence struct {
	context    string
	issues     map[string]bool
	blindspots map[string]bool
}

// correlate folds per-cluster evidence into the signals that appeared in
// minShared or more clusters, most widespread first.
//
// It is pure, and what it carries is bounded by construction rather than by a
// filter: a Shared holds a context name, an issue kind or an API resource name,
// and nothing else. In particular nothing on a gate.Blindspot reaches here
// except Resource — never Reason, which is a redacted error string rather than
// a bounded vocabulary, and this document is written to be forwarded.
func correlate(evidence []clusterEvidence) []Shared {
	if len(evidence) < minShared {
		return nil
	}

	issues, blindspots := map[string][]string{}, map[string][]string{}
	for _, e := range evidence {
		for signal := range e.issues {
			issues[signal] = append(issues[signal], e.context)
		}
		for signal := range e.blindspots {
			blindspots[signal] = append(blindspots[signal], e.context)
		}
	}

	shared := append(gather(issues, SourceIssue), gather(blindspots, SourceBlindspot)...)
	if len(shared) == 0 {
		return nil // nil rather than an empty slice, so Report's omitempty drops the key
	}

	sort.Slice(shared, func(i, j int) bool {
		a, b := shared[i], shared[j]
		if ra, rb := sourceRank(a.Source), sourceRank(b.Source); ra != rb {
			return ra < rb
		}
		if len(a.Clusters) != len(b.Clusters) {
			return len(a.Clusters) > len(b.Clusters)
		}
		return a.Signal < b.Signal
	})
	return shared
}

// gather turns one signal-to-clusters map into the entries that clear
// minShared. The context names are sorted here rather than at the call site
// because Go randomizes map iteration: without it the same fleet would render
// two different orders on two runs.
func gather(m map[string][]string, source string) []Shared {
	var out []Shared
	for signal, contexts := range m {
		if len(contexts) < minShared {
			continue
		}
		sort.Strings(contexts)
		out = append(out, Shared{Signal: signal, Source: source, Clusters: contexts})
	}
	return out
}

// sourceRank puts every issue before every blind spot. Comparing the strings
// would not: "blindspot" sorts before "issue".
func sourceRank(source string) int {
	if source == SourceIssue {
		return 0
	}
	return 1
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/fleet/ -run TestCorrelate -v 2>&1 | tail -30
gofmt -l internal/fleet/
go vet ./internal/fleet/
```

Expected: every `TestCorrelate*` PASSes, `gofmt -l` prints nothing, `go vet` is silent.

- [ ] **Step 6: Confirm the whole package and the whole suite are still green**

```bash
go test -p 2 ./... 2>&1 | grep -v '^ok' | head -20
```

Expected: no output at all. Task 1 adds an unused exported type and an unused unexported function reachable only from the test, which Go permits — nothing else in the tree has moved yet, so `./internal/schemadoc` is still green at this point.

- [ ] **Step 7: Commit**

```bash
git add internal/fleet/correlate.go internal/fleet/correlate_test.go
git commit -s -m "feat(fleet): the Shared type and the pure correlate fold

correlate folds per-cluster evidence into the signals that appeared in two
or more judged clusters, most widespread first. Evidence is a set rather
than a count, so a kind hitting four hundred pods in one cluster is still
one cluster -- a count-weighted fold would let a single noisy cluster
manufacture a fleet-wide signal that does not exist.

Issues sort before blind spots by an explicit rank, not by comparing the
Source strings, which would invert the order.

Nothing is wired up yet: Report does not carry the result and no renderer
prints it."
```

---

## Task 2: `summarize` produces evidence, `Sweep` correlates

**Files:**

- Modify: `internal/fleet/summarize.go:15-39` (the `summarize` function)
- Modify: `internal/fleet/fleet.go:73-98` (`Report`), `internal/fleet/fleet.go:181-190` (`Sweep`'s result loop)
- Modify: `internal/fleet/summarize_test.go` (five call sites, one stale comment, three new tests)
- Modify: `internal/fleet/fleet_test.go` (two new tests, one existing test extended)

**Interfaces:**

- Consumes from Task 1: `Shared`, `SourceIssue`, `SourceBlindspot`, `clusterEvidence`, `correlate(evidence []clusterEvidence) []Shared`.
- Produces, used by Tasks 3 and 4:
  - `Report.Shared []Shared` with JSON tag `shared,omitempty`
  - `func summarize(context string, v gate.Verdict) (ClusterSummary, clusterEvidence)` — **two** return values now

### Two comments in the existing tests become false in this task and must be corrected here

This is not optional tidying. A comment that promises something the code does not keep is a defect, and both of these are promises about the credential wall:

1. `internal/fleet/summarize_test.go:99-102`, above `TestSummarizeCarriesNoObjectName`, says "The report names clusters and issue kinds." After this task the report also names API resource names, through `Report.Shared`. The **test** stays correct — a `ClusterSummary` row still carries neither — but the comment must be narrowed to be about a row rather than the report.
2. `internal/fleet/fleet_test.go:91-92`, above `TestSweepCarriesNoObjectName`, says "The whole report — every string it carries — must be free of node, namespace, pod, workload and container names." That test builds its haystack from `SchemaVersion`, `Verdict`, `Clusters` and `Unreachable`. After this task the report has a fifth field the walk does not visit, so the comment overclaims. The fix is to make the comment true by walking `Shared` too — narrowing the claim here would be the wrong remedy, because a credential walk that skips a field is exactly what this test exists to prevent.

- [ ] **Step 1: Write the failing tests — evidence, in `summarize_test.go`**

Append these three tests to `internal/fleet/summarize_test.go`. `reflect` and `strings` are already imported by that file.

```go
// Evidence is a set, not a count: a kind hitting four hundred pods in one
// cluster is still one cluster. A count-weighted fold would let a single noisy
// cluster manufacture a fleet-wide signal that does not exist.
func TestCorrelateCountsAClusterOnceHoweverLoudItIs(t *testing.T) {
	var fs []findings.Finding
	for i := 0; i < 400; i++ {
		fs = append(fs, finding(findings.Critical, "CrashLoopBackOff"))
	}

	_, ev := summarize("example-context", gate.Verdict{Verdict: "fail", Failing: fs})

	if len(ev.issues) != 1 || !ev.issues["CrashLoopBackOff"] {
		t.Errorf("issues = %v, want the one kind exactly once", ev.issues)
	}
	if ev.context != "example-context" {
		t.Errorf("context = %q, want the cluster the evidence came from", ev.context)
	}
}

// Evidence spans both halves of the verdict, exactly as the level counts do. A
// finding below --fail-on is still evidence of what this cluster is showing.
func TestSummarizeEvidenceSpansFailingAndReported(t *testing.T) {
	_, ev := summarize("example-context", gate.Verdict{
		Verdict:  "fail",
		Failing:  []findings.Finding{finding(findings.Critical, "CrashLoopBackOff")},
		Reported: []findings.Finding{finding(findings.Info, "Unschedulable")},
	})

	want := map[string]bool{"CrashLoopBackOff": true, "Unschedulable": true}
	if !reflect.DeepEqual(ev.issues, want) {
		t.Errorf("issues = %v, want %v", ev.issues, want)
	}
}

// Blindspot.Resource is a closed set of API resource type names, every one a
// string literal in internal/scan. Blindspot.Reason is a redacted error string,
// bounded by nothing, and must never reach a document written to be forwarded.
// The evidence reads Resource and nothing else off a blind spot.
func TestSummarizeEvidenceCarriesResourceNeverReason(t *testing.T) {
	const sentinel = "SENTINELREASON"
	_, ev := summarize("example-context", gate.Verdict{
		Verdict: "inconclusive",
		Inconclusive: []gate.Blindspot{
			{Resource: "nodes/proxy", Reason: sentinel},
			{Resource: "pods/log", Reason: sentinel, Waived: true},
		},
	})

	want := map[string]bool{"nodes/proxy": true, "pods/log": true}
	if !reflect.DeepEqual(ev.blindspots, want) {
		t.Errorf("blindspots = %v, want %v — a waived read is still a blind spot", ev.blindspots, want)
	}
	for signal := range ev.blindspots {
		if strings.Contains(signal, sentinel) {
			t.Errorf("evidence carries a blind-spot reason: %q", signal)
		}
	}
}
```

- [ ] **Step 2: Write the failing tests — correlation at sweep level, in `fleet_test.go`**

Append these two tests to `internal/fleet/fleet_test.go`. Every import they need is already there.

```go
// A correlation adds no severity. Every finding it counts was already counted
// in the cluster that produced it, and that cluster already got its verdict
// from gate.Decide — so folding the same evidence again would double-count it,
// and would let a sweep disagree with a single-cluster `kubeagent gate` about
// the same cluster, which this package's doc comment says can never happen.
func TestSweepCorrelationChangesNoVerdict(t *testing.T) {
	targets := []Target{
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("alpha"))},
		{Name: "example-b", Client: fake.NewSimpleClientset(crashingPod("beta"))},
	}
	opts := Options{FailOn: findings.Critical, Workers: 2, ClusterTimeout: 30 * time.Second}

	rep := Sweep(context.Background(), targets, opts)

	if len(rep.Shared) == 0 {
		t.Fatal("no correlation; the fixture must share a signal or this test proves nothing")
	}
	if rep.Verdict != "fail" || rep.Code != 1 {
		t.Errorf("verdict = %q/%d, want fail/1 — the same answer slice 1 gave", rep.Verdict, rep.Code)
	}
	for _, c := range rep.Clusters {
		if c.Verdict != "fail" {
			t.Errorf("cluster %s = %q, want fail — a correlation changes no cluster verdict",
				c.Context, c.Verdict)
		}
	}
}

// One cluster cannot correlate with itself, however much it reports.
func TestSweepOfOneClusterCarriesNoCorrelation(t *testing.T) {
	rep := Sweep(context.Background(), []Target{
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("alpha"))},
	}, Options{FailOn: findings.Critical, Workers: 1, ClusterTimeout: 30 * time.Second})

	if rep.Shared != nil {
		t.Errorf("Shared = %+v, want nil so omitempty drops the key", rep.Shared)
	}
}
```

- [ ] **Step 3: Extend the whole-report credential walk**

In `internal/fleet/fleet_test.go`, inside `TestSweepCarriesNoObjectName`, add a third loop after the `rep.Unreachable` loop and before the `strings.Contains` check:

```go
	for _, s := range rep.Shared {
		sb.WriteString(" " + s.Signal + " " + s.Source + " " + strings.Join(s.Clusters, " "))
	}
```

The comment above that test now describes what it does again. Leave the comment as it is.

- [ ] **Step 4: Run the tests to verify they fail**

```bash
go test ./internal/fleet/ -run 'TestCorrelateCountsACluster|TestSummarizeEvidence|TestSweepCorrelation|TestSweepOfOneCluster' 2>&1 | head -20
```

Expected: FAIL to **compile** — `summarize` returns one value, not two, and `Report` has no field `Shared`.

- [ ] **Step 5: Implement — `summarize` returns evidence**

In `internal/fleet/summarize.go`, replace the whole `summarize` function (currently lines 15-39) with:

```go
// summarize reduces one cluster's gate verdict to its fleet row, and to the
// evidence that row cannot carry.
//
// It is pure, and it reads only Level and Issue off each finding and only
// Resource off each blind spot — which is what keeps a namespace, pod, workload
// or node name out of the report by construction, and keeps a blind spot's
// redacted Reason out of it too.
//
// The row's Blindspots count and the evidence's blindspots set answer different
// questions and are allowed to disagree: the count is how many reads failed,
// the set is which resources they were, and two failed reads of one resource
// are two of the former and one of the latter.
func summarize(context string, v gate.Verdict) (ClusterSummary, clusterEvidence) {
	s := ClusterSummary{
		Context:    context,
		Verdict:    v.Verdict,
		Blindspots: len(v.Inconclusive),
	}
	ev := clusterEvidence{
		context:    context,
		issues:     map[string]bool{},
		blindspots: map[string]bool{},
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
		ev.issues[f.Issue] = true
	}
	for _, b := range v.Inconclusive {
		ev.blindspots[b.Resource] = true
	}

	s.TopIssues = topIssues(counts)
	return s, ev
}
```

- [ ] **Step 6: Implement — `Report.Shared`**

In `internal/fleet/fleet.go`, add the field to `Report` after `Unreachable`:

```go
	// Shared is the cross-cluster correlation: which signals appeared in two or
	// more judged clusters. Absent from the document when there is none, so a
	// consumer written against the version before it is unaffected.
	Shared []Shared `json:"shared,omitempty"`
```

- [ ] **Step 7: Implement — `Sweep` collects evidence**

In `internal/fleet/fleet.go`, replace the result loop (currently lines 181-190) with:

```go
	evidence := make([]clusterEvidence, 0, len(targets))
	for i, r := range results {
		if r.err != nil {
			rep.Unreachable = append(rep.Unreachable, Unreachable{
				Context: targets[i].Name,
				Reason:  reasonFor(r.err),
			})
			continue
		}
		summary, ev := summarize(targets[i].Name, r.verdict)
		rep.Clusters = append(rep.Clusters, summary)
		evidence = append(evidence, ev)
	}

	// Only judged clusters contribute. An unreachable cluster produced no
	// verdict and so no evidence — it is absent from this slice rather than
	// present and empty, which is also what makes the rendered denominator the
	// count of clusters kubeagent actually judged.
	rep.Shared = correlate(evidence)
```

Leave `sortSummaries`, the `Unreachable` sort and `decide` exactly as they are.

- [ ] **Step 8: Update the five existing `summarize` call sites**

In `internal/fleet/summarize_test.go`, each of these becomes a two-value assignment discarding the evidence — the tests below are about the row:

- line 30: `got := summarize("example-context", v)` → `got, _ := summarize("example-context", v)`
- line 49: `got := summarize("example-context", gate.Verdict{Verdict: "pass"})` → `got, _ := …`
- line 74: `got := summarize("example-context", gate.Verdict{Verdict: "fail", Failing: fs})` → `got, _ := …`
- line 91: `got := summarize("example-context", gate.Verdict{Verdict: "fail", Failing: fs})` → `got, _ := …`
- line 119: `got := summarize("example-context", v)` → `got, _ := summarize("example-context", v)`

- [ ] **Step 9: Correct the stale comment on `TestSummarizeCarriesNoObjectName`**

In `internal/fleet/summarize_test.go`, replace the comment block currently at lines 99-102 (immediately above `func TestSummarizeCarriesNoObjectName`) with:

```go
// A cluster's row names its context and its issue kinds. A namespace, pod,
// workload or node name reaching it would be a credential leak, and the defence
// is structural: summarize reads Level and Issue and nothing else off a
// finding. This test proves the structure holds by marking every other field
// and looking for the markers. The blind-spot resource names the same verdict
// carries reach the report through Report.Shared, never through a row — which
// is why the marked Resource below must not appear here either.
```

- [ ] **Step 10: Run the tests to verify they pass**

```bash
go test ./internal/fleet/... -v 2>&1 | tail -40
gofmt -l internal/fleet/
go vet ./internal/fleet/
```

Expected: every test in `internal/fleet` PASSes, including the six pre-existing `TestSummarize*`, `TestSweep*` and `TestRender*` tests. `gofmt -l` prints nothing.

If `TestSweepCorrelationChangesNoVerdict` fails on its `t.Fatal("no correlation…")` guard, the two fake clusters did not produce a common issue kind. Print `rep.Shared` and `rep.Clusters[i].TopIssues` to see what they did produce, and adjust the fixture so both clusters genuinely share a kind — **do not** delete the guard, which is what keeps this test from passing vacuously.

- [ ] **Step 11: Confirm the rest of the tree, and confirm the expected schema failure**

```bash
go test -p 2 ./... 2>&1 | grep -v '^ok' | head -20
```

Expected: exactly one failing package, `github.com/imantaba/kubeagent/internal/schemadoc`, reporting fleet drift as **additive**. **This is the expected state until Task 4** — see "The schema drift test fails between Task 2 and Task 4" above. Do not run `-update` here.

Also confirm nothing else moved:

```bash
git diff --stat
git status --short internal/report/testdata/golden-scan.txt go.mod go.sum
```

Expected: only the four `internal/fleet` files in the diff; the second command prints nothing.

- [ ] **Step 12: Commit**

```bash
git add internal/fleet/summarize.go internal/fleet/fleet.go internal/fleet/summarize_test.go internal/fleet/fleet_test.go
git commit -s -m "feat(fleet): collect per-cluster evidence and correlate the sweep

summarize returns the evidence its row cannot carry -- the set of issue
kinds and the set of blind-spot resources the cluster showed -- and Sweep
folds those across clusters into Report.Shared.

Only judged clusters contribute: an unreachable cluster produced no verdict
and so no evidence, which is also what makes the denominator the count of
clusters kubeagent actually judged.

Evidence reads Blindspot.Resource and never Blindspot.Reason, which is a
redacted error string rather than a bounded vocabulary. Two tests pin that:
one on the evidence directly, and one walking every string the whole report
carries, now including the new field.

The verdict is untouched. A correlation counts findings already counted in
the clusters that produced them, so folding them again would double-count
and would let a sweep disagree with a single-cluster gate.

internal/schemadoc's drift test fails from here until the version bump --
Report grew a property and that is exactly what the test is for."
```

---

## Task 3: The two rendered sections

**Files:**

- Modify: `internal/fleet/render.go` (add `maxNamedClusters`, `renderShared`, `countCell`, `namedClusters`; one call inside `RenderText`)
- Modify: `internal/fleet/render_test.go` (a `sharedReport()` fixture and six tests; one existing test becomes table-driven)

**Interfaces:**

- Consumes from Tasks 1 and 2: `Shared`, `SourceIssue`, `SourceBlindspot`, `minShared`, `Report.Shared`.
- Produces: nothing later tasks call. `RenderText`'s and `RenderJSON`'s signatures do not change.

### The exact bytes

`renderShared` writes, per non-empty section: a blank line, the header, a blank line, then one line per entry. The verdict line's leading `\n` supplies the trailing blank, so nothing extra is emitted at the end.

Row format string, applied after computing **one** count-cell width and **one** signal width across **both** sections:

```
"  %*s  %-*s  %s"
```

— two leading spaces, the right-aligned `N/M` cell, two spaces, the left-aligned signal padded to the shared width, two spaces, the cluster list. The line is `strings.TrimRight`'d exactly as `writeRow` does.

### Two existing tests must stay green **unchanged**

`TestRenderTextIsExactBytes` and `TestRenderTextSingularBlindSpot` use fixtures with a nil `Shared`, so `renderShared` writes nothing and their expected bytes do not move. **Do not edit them.** If either fails, the section-omission logic is wrong.

- [ ] **Step 1: Write the failing tests**

Append to `internal/fleet/render_test.go`. Every import it needs (`bytes`, `encoding/json`, `errors`, `strings`, `testing`) is already there.

```go
// sharedReport is sampleReport with a correlation. Four judged clusters, so the
// denominator is 4 — the fifth is unreachable and contributed no evidence.
func sharedReport() Report {
	rep := sampleReport()
	rep.Shared = []Shared{
		{Signal: "ImagePullBackOff", Source: SourceIssue, Clusters: []string{
			"example-eu-1", "example-eu-2", "example-staging-2", "example-us-3"}},
		{Signal: "OOMKilled", Source: SourceIssue, Clusters: []string{
			"example-eu-1", "example-us-3"}},
		{Signal: "nodes/proxy", Source: SourceBlindspot, Clusters: []string{
			"example-eu-1", "example-staging-2"}},
	}
	return rep
}

func TestRenderTextWithCorrelationIsExactBytes(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderText(&buf, sharedReport()); err != nil {
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
		"SHARED ISSUES  in 2 or more of 4 judged clusters",
		"",
		"  4/4  ImagePullBackOff  example-eu-1, example-eu-2, example-staging-2, +1 more",
		"  2/4  OOMKilled         example-eu-1, example-us-3",
		"",
		"SHARED BLIND SPOTS  in 2 or more of 4 judged clusters",
		"",
		"  2/4  nodes/proxy       example-eu-1, example-staging-2",
		"",
		"verdict: inconclusive (exit 2)",
		"",
	}, "\n")

	if got := buf.String(); got != want {
		t.Errorf("RenderText() =\n%s\nwant\n%s", got, want)
	}
}

// A heading over nothing reads as a failed render, so a section with no entries
// is omitted entirely rather than printed empty.
func TestRenderTextOmitsASharedSectionWithNoEntries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		shared      []Shared
		wantPresent string
		wantAbsent  string
	}{
		{
			name: "blind spots only",
			shared: []Shared{{Signal: "nodes/proxy", Source: SourceBlindspot,
				Clusters: []string{"example-a", "example-b"}}},
			wantPresent: "SHARED BLIND SPOTS",
			wantAbsent:  "SHARED ISSUES",
		},
		{
			name: "issues only",
			shared: []Shared{{Signal: "OOMKilled", Source: SourceIssue,
				Clusters: []string{"example-a", "example-b"}}},
			wantPresent: "SHARED ISSUES",
			wantAbsent:  "SHARED BLIND SPOTS",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := Report{
				Verdict: "fail", Code: 1,
				Clusters: []ClusterSummary{
					{Context: "example-a", Verdict: "fail", Critical: 1},
					{Context: "example-b", Verdict: "fail", Critical: 1},
				},
				Shared: tc.shared,
			}
			var buf bytes.Buffer
			if err := RenderText(&buf, rep); err != nil {
				t.Fatalf("RenderText() error = %v", err)
			}
			if !strings.Contains(buf.String(), tc.wantPresent) {
				t.Errorf("output = %q, want a %s section", buf.String(), tc.wantPresent)
			}
			if strings.Contains(buf.String(), tc.wantAbsent) {
				t.Errorf("output = %q, want no %s heading over an empty section",
					buf.String(), tc.wantAbsent)
			}
		})
	}
}

// A signpost, not an inventory — the same reasoning that caps TopIssues at
// three. The JSON document carries every name.
func TestRenderTextNamesAtMostThreeClustersThenCountsTheRest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		clusters []string
		wantLine string
	}{
		{"three are all named", []string{"example-a", "example-b", "example-c"},
			"  3/3  OOMKilled  example-a, example-b, example-c"},
		{"four name three and count one",
			[]string{"example-a", "example-b", "example-c", "example-d"},
			"  4/4  OOMKilled  example-a, example-b, example-c, +1 more"},
		{"six name three and count three",
			[]string{"example-a", "example-b", "example-c", "example-d", "example-e", "example-f"},
			"  6/6  OOMKilled  example-a, example-b, example-c, +3 more"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := Report{Verdict: "pass", Shared: []Shared{
				{Signal: "OOMKilled", Source: SourceIssue, Clusters: tc.clusters},
			}}
			for _, c := range tc.clusters {
				rep.Clusters = append(rep.Clusters, ClusterSummary{Context: c, Verdict: "pass"})
			}

			var buf bytes.Buffer
			if err := RenderText(&buf, rep); err != nil {
				t.Fatalf("RenderText() error = %v", err)
			}
			if !strings.Contains(buf.String(), "\n"+tc.wantLine+"\n") {
				t.Errorf("output =\n%s\nwant a line %q", buf.String(), tc.wantLine)
			}
		})
	}
}

// The denominator is judged clusters, not selected ones. An unreachable cluster
// produced no verdict and could not have contributed a signal, so counting it
// would make a 2-of-2 correlation read as 2-of-5 and understate it.
func TestRenderTextSharedDenominatorCountsJudgedClustersOnly(t *testing.T) {
	rep := Report{
		Verdict: "inconclusive", Code: 2,
		Clusters: []ClusterSummary{
			{Context: "example-a", Verdict: "fail", Critical: 1},
			{Context: "example-b", Verdict: "fail", Critical: 1},
		},
		Unreachable: []Unreachable{
			{Context: "example-c", Reason: ReasonUnreachable},
			{Context: "example-d", Reason: ReasonTimedOut},
			{Context: "example-e", Reason: ReasonTimedOut},
		},
		Shared: []Shared{
			{Signal: "OOMKilled", Source: SourceIssue, Clusters: []string{"example-a", "example-b"}},
		},
	}

	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	if !strings.Contains(buf.String(), "in 2 or more of 2 judged clusters") {
		t.Errorf("output =\n%s\nwant a denominator of 2 — three unreachable clusters "+
			"produced no verdict and could not have contributed a signal", buf.String())
	}
	if !strings.Contains(buf.String(), "  2/2  OOMKilled") {
		t.Errorf("output =\n%s\nwant the count cell measured against judged clusters too", buf.String())
	}
}

func TestRenderJSONOmitsSharedWhenThereIsNoCorrelation(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sampleReport()); err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}
	if strings.Contains(buf.String(), `"shared"`) {
		t.Errorf("document carries a shared key with no correlation; omitempty must drop it:\n%s",
			buf.String())
	}
}

// The text renderer names at most three clusters. The document names every one:
// a jq filter asking which clusters share a signal must get the answer, not a
// signpost.
func TestRenderJSONCarriesEverySharedClusterName(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sharedReport()); err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	shared, ok := doc["shared"].([]any)
	if !ok || len(shared) != 3 {
		t.Fatalf("shared = %#v, want three entries", doc["shared"])
	}
	first, ok := shared[0].(map[string]any)
	if !ok {
		t.Fatalf("shared[0] = %#v, want an object", shared[0])
	}
	if first["signal"] != "ImagePullBackOff" || first["source"] != "issue" {
		t.Errorf("shared[0] = %#v, want the most widespread issue first", first)
	}
	if clusters, ok := first["clusters"].([]any); !ok || len(clusters) != 4 {
		t.Errorf("clusters = %#v, want all four names — the +N more elision belongs to "+
			"the text renderer, not to the document", first["clusters"])
	}
	if strings.Contains(buf.String(), "more") {
		t.Errorf("the document carries the text renderer's elision:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Make the write-failure test cover the new writes**

In `internal/fleet/render_test.go`, replace the whole body of `TestRenderTextReportsAFailedWrite` (currently lines 188-206) with a table over both fixtures, so an unchecked `Fprintf` inside `renderShared` cannot slip through:

```go
func TestRenderTextReportsAFailedWrite(t *testing.T) {
	boom := errors.New("disk full")

	for _, tc := range []struct {
		name string
		rep  Report
	}{
		{"without a correlation", sampleReport()},
		{"with both correlation sections", sharedReport()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// How many writes a full render makes, so the loop below covers every one.
			counter := &flakyWriter{failAt: -1}
			if err := RenderText(counter, tc.rep); err != nil {
				t.Fatalf("counting writes: %v", err)
			}
			if counter.n < 3 {
				t.Fatalf("counted %d writes; the fixture should render a header and several rows",
					counter.n)
			}

			for i := 1; i <= counter.n; i++ {
				w := &flakyWriter{failAt: i, err: boom}
				if err := RenderText(w, tc.rep); !errors.Is(err, boom) {
					t.Errorf("write %d of %d failed: err = %v, want %v", i, counter.n, err, boom)
				}
			}
		})
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
go test ./internal/fleet/ -run 'TestRender' 2>&1 | head -30
```

Expected: FAIL. `TestRenderTextWithCorrelationIsExactBytes` fails with the two sections missing from the output; `TestRenderJSONOmitsSharedWhenThereIsNoCorrelation` may already pass (nothing writes `Shared` for `sampleReport`) — that is fine, it is the guard against a later regression.

- [ ] **Step 4: Implement the renderer**

In `internal/fleet/render.go`, add this constant next to `verdictWidth`:

```go
// maxNamedClusters caps the context names a shared-signal line spells out
// before it counts the rest. Three, on the same reasoning that caps TopIssues:
// the line has to stay readable when a signal spans three hundred clusters, and
// the document carries every name for whoever needs them all.
const maxNamedClusters = 3
```

Then add the call inside `RenderText`, between the row loop and the verdict line — replace:

```go
	_, err := fmt.Fprintf(w, "\nverdict: %s (exit %d)\n", rep.Verdict, rep.Code)
	return err
```

with:

```go
	if err := renderShared(w, rep); err != nil {
		return err
	}

	_, err := fmt.Fprintf(w, "\nverdict: %s (exit %d)\n", rep.Verdict, rep.Code)
	return err
```

Then append these three functions to the end of the file:

```go
// renderShared writes the two correlation sections, between the cluster table
// and the verdict line.
//
// A section with no entries is omitted entirely, heading included: a heading
// over nothing reads as a failed render. The signal column is padded to one
// width computed across both sections rather than per section, because two
// sections that nearly line up read as a bug.
//
// The verdict line's own leading newline supplies the blank line after the last
// row here, so this function emits no trailing blank of its own.
func renderShared(w io.Writer, rep Report) error {
	judged := len(rep.Clusters)

	var issues, blindspots []Shared
	countWidth, signalWidth := 0, 0
	for _, s := range rep.Shared {
		if s.Source == SourceIssue {
			issues = append(issues, s)
		} else {
			blindspots = append(blindspots, s)
		}
		if n := len(countCell(s, judged)); n > countWidth {
			countWidth = n
		}
		if n := len(s.Signal); n > signalWidth {
			signalWidth = n
		}
	}

	for _, section := range []struct {
		title   string
		entries []Shared
	}{
		{"SHARED ISSUES", issues},
		{"SHARED BLIND SPOTS", blindspots},
	} {
		if len(section.entries) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "\n%s  in %d or more of %d judged clusters\n\n",
			section.title, minShared, judged); err != nil {
			return err
		}
		for _, s := range section.entries {
			line := fmt.Sprintf("  %*s  %-*s  %s",
				countWidth, countCell(s, judged),
				signalWidth, s.Signal,
				namedClusters(s.Clusters))
			if _, err := fmt.Fprintln(w, strings.TrimRight(line, " ")); err != nil {
				return err
			}
		}
	}
	return nil
}

// countCell is the N/M cell: how many judged clusters showed this signal, out
// of how many were judged.
//
// The denominator is judged clusters and not selected ones. An unreachable
// cluster produced no verdict and so contributed no evidence, and counting it
// would make a 2-of-2 correlation read as 2-of-5 — understating exactly the
// thing the section exists to surface.
func countCell(s Shared, judged int) string {
	return fmt.Sprintf("%d/%d", len(s.Clusters), judged)
}

// namedClusters spells out at most maxNamedClusters context names and then says
// how many it left out.
func namedClusters(contexts []string) string {
	if len(contexts) <= maxNamedClusters {
		return strings.Join(contexts, ", ")
	}
	return fmt.Sprintf("%s, +%d more",
		strings.Join(contexts[:maxNamedClusters], ", "), len(contexts)-maxNamedClusters)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/fleet/... -v 2>&1 | tail -50
gofmt -l internal/fleet/
go vet ./internal/fleet/
```

Expected: every test PASSes, including `TestRenderTextIsExactBytes` and `TestRenderTextSingularBlindSpot` **unchanged** — a nil `Shared` renders nothing. `gofmt -l` prints nothing.

If `TestRenderTextWithCorrelationIsExactBytes` fails on whitespace, compare the diff character by character; the expected block above is authoritative and was computed from the format string, so a mismatch means the implementation differs, not the test.

- [ ] **Step 6: Confirm the rest of the tree**

```bash
go test -p 2 ./... 2>&1 | grep -v '^ok' | head -20
git status --short internal/report/testdata/golden-scan.txt go.mod go.sum
```

Expected: still exactly one failing package, `internal/schemadoc`, for the same reason as Task 2. The second command prints nothing.

- [ ] **Step 7: Commit**

```bash
git add internal/fleet/render.go internal/fleet/render_test.go
git commit -s -m "feat(fleet): render the shared-issue and shared-blind-spot sections

Two sections between the cluster table and the verdict line, each omitted
entirely -- heading included -- when it has no entries, because a heading
over nothing reads as a failed render.

The denominator is judged clusters, not selected ones: an unreachable
cluster produced no verdict and could not have contributed a signal.

The text names at most three clusters and then counts the rest, on the same
reasoning that caps TopIssues at three. The JSON document names every one,
which a test pins so the elision cannot leak into the document.

A sweep with no correlation renders byte-identically to before, which is
why the existing exact-bytes test is untouched and still green."
```

---

## Task 4: The schema version bump

**Files:**

- Modify: `internal/jsonschema/jsonschema.go:34-37`
- Modify: `internal/fleet/render_test.go:15` (the fixture's stale `SchemaVersion` literal)
- Regenerate: `website/docs/schemas/fleet-v1.json`

**Interfaces:**

- Consumes: `Report.Shared` from Task 2 — the shape change that makes the version move.
- Produces: `jsonschema.FleetVersion == "1.1"`, which `Sweep` already stamps into every report.

This is the **only** task permitted to run a test with `-update`, exactly once, on `TestSchemaDrift`.

- [ ] **Step 1: Confirm the drift test fails, and read what it says**

```bash
go test ./internal/schemadoc -run TestSchemaDrift 2>&1 | head -40
```

Expected: FAIL, naming `fleet` and classifying the change as **additive (MINOR)** — one property added, none removed, none changed type, none newly required. If it says **breaking**, stop and report: something other than an added `omitempty` property moved, and this plan's premise is wrong.

- [ ] **Step 2: Bump the version**

In `internal/jsonschema/jsonschema.go`, replace lines 34-36:

```go
	// FleetVersion is `kubeagent fleet --output json`. It enters the contract at
	// 1.0 as the eighth document; no existing surface moves for it.
	FleetVersion = "1.0"
```

with:

```go
	// FleetVersion is `kubeagent fleet --output json`. It entered the contract
	// at 1.0 as the eighth document. 1.1 adds the optional `shared` property —
	// the cross-cluster correlation — which is absent from a sweep that found
	// none, so every 1.0 consumer is unaffected.
	FleetVersion = "1.1"
```

- [ ] **Step 3: Refresh the stale fixture literal**

`internal/fleet/render_test.go:15` hardcodes `SchemaVersion: "1.0"` in `sampleReport()`. No assertion reads it, but a stale literal in a fixture is a small lie about what the package produces. Change it to `"1.1"`.

- [ ] **Step 4: Regenerate the published schema — the one permitted `-update`**

```bash
go test ./internal/schemadoc -run TestSchemaDrift -update
```

- [ ] **Step 5: Verify exactly one schema file moved**

```bash
git status --short website/docs/schemas/
git diff website/docs/schemas/ | head -40
```

Expected: only `website/docs/schemas/fleet-v1.json` is modified, and the diff shows the `schemaVersion` default/const moving to `1.1` plus a new `shared` property. **No other file under `website/docs/schemas/` may appear.** If `scan`, `gate`, `rbac`, `watch` or `baseline` moved, revert everything and stop — that is a defect, not a rename.

- [ ] **Step 6: Re-run without `-update` and confirm green**

```bash
go test ./internal/schemadoc 2>&1 | tail -10
go test -p 2 ./... 2>&1 | grep -v '^ok' | head -20
```

Expected: `internal/schemadoc` PASSes and the second command prints **nothing** — the whole suite is green again for the first time since Task 2.

- [ ] **Step 7: Verify the binary prints the new schema**

```bash
go build -o /tmp/kubeagent-fleetcorr . && /tmp/kubeagent-fleetcorr schema fleet | head -8
```

Expected: the schema prints, with `1.1` in it. No cluster and no kubeconfig are involved. Remove the binary afterwards: `rm -f /tmp/kubeagent-fleetcorr`.

- [ ] **Step 8: Commit**

```bash
git add internal/jsonschema/jsonschema.go internal/fleet/render_test.go website/docs/schemas/fleet-v1.json
git commit -s -m "feat(fleet): fleet schema 1.0 -> 1.1 for the shared property

Additive: one optional property added, none removed, none changed type,
none newly required -- which is what the drift test says. A sweep that
found no correlation encodes no shared key, so every 1.0 consumer reads
the same document it read before.

scan stays 1.2 and gate stays 1.1; no other surface moves."
```

---

## Task 5: Documentation

**Files:**

- Modify: `internal/fleet/fleet.go:15-20` (the package comment's credential sentence)
- Modify: `website/docs/features/fleet.md` — the `--output json` example, "What the report may name", "The schema", "Not in this slice", and a new section
- Modify: `CHANGELOG.md` (`[Unreleased]` → `### Added`)
- Modify: `CLAUDE.md:190-191` (the fleet bullet's credential sentence)
- Modify: `website/docs/roadmap.md:562` (the post-1.0 row)

**Interfaces:**

- Consumes: everything from Tasks 1-4. Produces nothing code depends on.

### The widened promise

Fleet's credential promise is written down in two places and both **overclaim** after Task 2, because the report now also names API resource names. Widening the claim honestly is the remedy; narrowing the feature is not.

- [ ] **Step 1: Widen the package comment**

In `internal/fleet/fleet.go`, replace the paragraph currently at lines 15-20:

```go
// The report names kubeconfig context names and issue kinds. It never names a
// node, namespace, pod or workload, and that is structural rather than
// filtered: a summary is counts plus issue kinds, a shape an object name cannot
// fit into. Nor does it ever carry a kubeconfig path — the one accepted place a
// path may appear is stderr, from internal/cli, and this package writes no
// errors of its own.
```

with:

```go
// The report names kubeconfig context names, issue kinds, and the API resource
// names of refused reads. It never names a node, namespace, pod or workload,
// and that is structural rather than filtered: a summary is counts plus issue
// kinds, and a correlation is context names plus a signal drawn from one of two
// closed vocabularies — shapes an object name cannot fit into. In particular a
// correlation reads gate.Blindspot.Resource and never gate.Blindspot.Reason,
// which is a redacted error string rather than a bounded vocabulary. Nor does
// the report ever carry a kubeconfig path — the one accepted place a path may
// appear is stderr, from internal/cli, and this package writes no errors of its
// own.
```

- [ ] **Step 2: Update `CLAUDE.md`'s fleet bullet**

Replace lines 190-191:

```
  Its report names kubeconfig context names and issue kinds — never a node,
  namespace, pod or workload name
```

with:

```
  Its report names kubeconfig context names, issue kinds, and the API resource
  names of refused reads — never a node, namespace, pod or workload name, and
  never a blind spot's `Reason`, which is a redacted error string rather than a
  bounded vocabulary
```

- [ ] **Step 3: Update the fleet feature page**

Four edits to `website/docs/features/fleet.md`.

**A consistency rule that governs all of them:** the page's flagship example (the text table at lines 16-27) and the `--output json` example are **the same sweep**, and that sweep shares nothing — its two failing clusters report disjoint issue kinds. So the flagship table gets **no** shared section and the JSON example gets **no** `shared` key. Do not add one to either: it would contradict the table above it, and its absence is a working demonstration of the `omitempty` rule. The new section below carries its own, different, self-consistent example.

**(a)** In the `--output json` example, change `"schemaVersion": "1.0"` to `"schemaVersion": "1.1"` — **and change nothing else in that block.** Then extend the paragraph after it: replace

```markdown
Same data, shaped for what each consumer needs. A passing cluster carries no
`topIssues` key at all: `omitempty` drops it rather than writing an empty
array.
```

with

```markdown
Same data, shaped for what each consumer needs. A passing cluster carries no
`topIssues` key at all: `omitempty` drops it rather than writing an empty
array, and this sweep's two failing clusters report disjoint issue kinds, so
there is no `shared` key either — see [Shared signals](#shared-signals).
```

**(b)** Add a new `## Shared signals` section immediately after the `## \`--output json\`` section and immediately before `## What the report may name`. Both output shapes have been introduced by that point, which is what this section needs.

Its text example is a **different, smaller sweep** than the flagship, and it is internally consistent: three judged clusters, one `fail`, one `inconclusive` (a blind spot always costs a cluster `inconclusive`), one `pass`; each row's counts match its issue kinds; only one cluster has a blind spot, so the shared-blind-spot section is absent — demonstrating the omission rule in the same breath. The block below is byte-exact, computed from the renderer's format strings:

```markdown
## Shared signals

One row per cluster answers "which one do I open first". It cannot answer
"is this one problem or five".

Under the table, `fleet` names the issue kinds and the refused reads that
appear in **two or more** of the judged clusters, most widespread first:

​```text
FLEET  3 clusters, 1 failing, 0 unreachable

CLUSTER       VERDICT       CRIT  WARN  INFO  TOP ISSUES
example-eu-1  inconclusive     2     1     0  ImagePullBackOff, OOMKilled (1 blind spot)
example-us-3  fail             1     0     0  ImagePullBackOff
example-eu-2  pass             0     1     0  OOMKilled

SHARED ISSUES  in 2 or more of 3 judged clusters

  2/3  ImagePullBackOff  example-eu-1, example-us-3
  2/3  OOMKilled         example-eu-1, example-eu-2

verdict: inconclusive (exit 2)
​```

There is no `SHARED BLIND SPOTS` section above because only one cluster has a
blind spot, and one cluster is not a correlation. A section with no entries is
omitted entirely, heading included — a heading over nothing reads as a failed
render.

Both sections appear in the JSON document as one `shared` array, each entry
tagged with which vocabulary its signal came from:

​```json
  "shared": [
    {
      "signal": "ImagePullBackOff",
      "source": "issue",
      "clusters": [
        "example-eu-1",
        "example-us-3"
      ]
    },
    {
      "signal": "OOMKilled",
      "source": "issue",
      "clusters": [
        "example-eu-1",
        "example-eu-2"
      ]
    }
  ]
​```

The text names at most three clusters per line and then counts the rest
(`+2 more`). The document names every one: a `jq` filter asking which clusters
share a signal must get the answer, not a signpost.

A repeated blind spot — `source` `blindspot`, rendered under `SHARED BLIND
SPOTS` — is often the more actionable of the two: it usually means one RBAC
binding is missing everywhere, and it is the class of problem a per-cluster
view is worst at surfacing, because each cluster reports it as a single quiet
line.

**Some things this deliberately does not do.**

A cluster counts **once** per signal, however loud it is. A kind hitting four
hundred pods in one cluster is one cluster — otherwise a single noisy cluster
could manufacture a fleet-wide signal that does not exist.

The denominator is the count of clusters kubeagent **judged**, never the count
it selected. An unreachable cluster produced no verdict and could not have
contributed a signal.

**It changes no verdict.** Every finding a correlation counts was already
counted in the cluster that produced it, and that cluster already got its
verdict from the same `gate` evaluation a single-cluster run would use.
Counting it twice would let a sweep disagree with `kubeagent gate` about the
same cluster. The threshold is two and is not configurable: one cluster is
not a pattern, and every number above two is an arbitrary line you would
have to learn.

Matching is exact. `Init:CrashLoopBackOff` and `CrashLoopBackOff` are
different kinds and stay different — they have different causes and different
fixes, and folding them together would report a coincidence as a correlation.

The text names at most three clusters per line and then counts the rest. The
JSON document names every one.
```

Remove the two zero-width space characters before the two ` ``` ` fences — they are here only so this plan's own fenced block does not terminate early. The fences in the page must be plain ` ```text `.

**(c)** In `## What the report may name`, extend the "may name" paragraph. Replace:

```markdown
**It may name:** kubeconfig context names, and issue kinds
(`CrashLoopBackOff`, `ImagePullBackOff`, `Unschedulable`, and so on). A
```

with:

```markdown
**It may name:** kubeconfig context names; issue kinds (`CrashLoopBackOff`,
`ImagePullBackOff`, `Unschedulable`, and so on); and, in the shared-signals
section, the API resource names of reads kubeagent was refused (`nodes/proxy`,
`pods/log`, `secrets`, `events`, and so on). Both of those are closed,
kubeagent-authored vocabularies, and a resource name names a *kind* of read,
never an object. A
```

and add this paragraph immediately after the paragraph beginning "That last exclusion is not a filter":

```markdown
A shared signal is the same shape of promise: it carries a context name, a
signal from one of those two closed vocabularies, and nothing else. In
particular it reads a blind spot's `Resource` and never its `Reason`, which
is a redacted error string rather than a bounded vocabulary — redacted is not
the same as bounded, and a fleet report is written to be forwarded.
```

**(d)** In `## Not in this slice`, replace the first bullet — verbatim, all five lines of it:

```markdown
- **Cross-cluster correlation** — "the same image is failing in all three".
  This slice is the first thing in the repo that holds many clusters'
  findings at once, which makes that question possible to ask, but it does
  not attempt to answer it: each row is one cluster's own verdict, computed
  independently of every other row.
```

with:

```markdown
- **Correlation on an image.** The shared-signals section correlates issue
  kinds and refused reads, not images: no finding in kubeagent carries an
  image reference at any point in `scan` → `findings` → `gate`, and a private
  registry host in one would be an internal hostname in a document written to
  be forwarded.
```

- [ ] **Step 4: Update `website/docs/roadmap.md`**

In the **post-1.0** row (line 562), replace the clause:

```
**fleet-scale slice 1 shipped** (`kubeagent fleet` sweeps every selected kubeconfig context in bounded parallel and reports one verdict per cluster, worst first) — cross-cluster correlation ("the same image is failing in all three"), which this slice makes possible and deliberately does not attempt;
```

with:

```
**fleet-scale slices 1 and 2 shipped** (`kubeagent fleet` sweeps every selected kubeconfig context in bounded parallel and reports one verdict per cluster worst first, then names the issue kinds and refused reads shared by two or more judged clusters — correlation on an issue kind and a blind spot rather than on an image, which no kubeagent finding carries);
```

and, in the same row's trailing "still ahead" list, make sure cross-cluster correlation is no longer named as future work while **selection from something other than a kubeconfig** still is.

- [ ] **Step 5: Add the CHANGELOG entry**

Under `## [Unreleased]`, in an `### Added` block (create it if the section has none):

```markdown
- **`kubeagent fleet` cross-cluster correlation.** Under the per-cluster table,
  a sweep now names the issue kinds and the refused reads that appear in two or
  more of the judged clusters, most widespread first — the answer to "is this
  one problem or five" that a one-row-per-cluster view cannot give. It costs no
  new cluster read: both axes were already computed inside the sweep. A cluster
  counts once per signal however loud it is, the denominator is judged clusters
  rather than selected ones, and the correlation changes no verdict — every
  finding it counts was already counted in the cluster that produced it. The
  fleet JSON document gains an optional `shared` property and moves to schema
  version `1.1`; a sweep that found no correlation encodes no key, so every
  existing consumer is unaffected.
```

- [ ] **Step 6: Build the docs**

```bash
cd /home/ubuntu/git/kubeagent/website
/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml 2>&1 | tail -20
cd /home/ubuntu/git/kubeagent
```

Expected: exit 0, "Documentation built", and **no `WARNING` lines naming `fleet.md` or `roadmap.md`**. The red "Material for MkDocs 2.0" banner is cosmetic — judge by the exit code and the absence of page warnings.

**The `cd` back to the repo root is not optional.** The bash working directory persists between calls in this environment and has drifted into `website/` before.

- [ ] **Step 7: Verify the whole tree**

```bash
export PATH=$PATH:/usr/local/go/bin
cd /home/ubuntu/git/kubeagent
go build ./... && go test -p 2 ./... 2>&1 | grep -v '^ok' | head -20
gofmt -l internal/ && go vet ./...
git status --short internal/report/testdata/golden-scan.txt go.mod go.sum
```

Expected: the test command prints nothing, `gofmt -l` prints nothing, `go vet` is silent, and the last command prints nothing.

- [ ] **Step 8: Confirm the credential and attribution rules held across the branch**

```bash
git diff main...HEAD | grep -inE 'co-authored|claude|anthropic' | grep -viE 'CLAUDE\.md' | head
git diff main...HEAD | grep -nE 'https?://' | grep -v 'k8sproject.top' | grep -v 'github.com/imantaba' | head
bash scripts/dco-check.sh main HEAD
```

Expected: the first two commands print nothing; `dco-check.sh` reports every commit signed off.

- [ ] **Step 9: Commit**

```bash
git add internal/fleet/fleet.go website/docs/features/fleet.md website/docs/roadmap.md CHANGELOG.md CLAUDE.md
git commit -s -m "docs: cross-cluster correlation, and fleet's widened credential promise

The report now names API resource names as well as context names and issue
kinds, so the two places that promise otherwise -- the package comment and
CLAUDE.md's fleet bullet -- are widened to say so. A promise that no longer
describes the code is a defect, and the honest remedy is to widen the claim
rather than to narrow the feature.

The feature page gains a shared-signals section and drops the 'not in this
slice' bullet that said correlation was deliberately unattempted. What
replaces it is narrower and still true: correlation on an *image* is out of
scope, because no kubeagent finding carries an image reference and a private
registry host in one would be an internal hostname in a document written to
be forwarded."
```

---

## Self-Review

**1. Spec coverage.** Every section of the spec maps to a task:

| Spec section | Task |
|---|---|
| Architecture / data flow / types | 1, 2 |
| Sets not counts | 1 (`clusterEvidence` doc), 2 (`TestCorrelateCountsAClusterOnceHoweverLoudItIs`) |
| Threshold and ordering | 1 (four ordering tests) |
| What "same" means — exact match, no normalization | 1 (`TestCorrelateKeepsAnIssueAndABlindSpotOfTheSameNameApart`), 5 (doc paragraph) |
| Credential wall — `Resource` never `Reason` | 2 (`TestSummarizeEvidenceCarriesResourceNeverReason`, extended `TestSweepCarriesNoObjectName`) |
| The documented promise widens | 5 (steps 1 and 2) |
| Text output, both sections, exact bytes | 3 |
| Section omitted when empty | 3 (`TestRenderTextOmitsASharedSectionWithNoEntries`) |
| Denominator is judged clusters | 3 (`TestRenderTextSharedDenominatorCountsJudgedClustersOnly`) |
| `maxNamedClusters` / `+N more` | 3 (`TestRenderTextNamesAtMostThreeClustersThenCountsTheRest`) |
| JSON `omitempty`, every cluster named | 3 (two `TestRenderJSON*` tests) |
| Schema 1.0 → 1.1 | 4 |
| Gating unchanged | 2 (`TestSweepCorrelationChangesNoVerdict`) |
| Files list | matches the File Structure table above |

No gaps.

**2. Placeholder scan.** No "TBD", no "TODO", no "handle edge cases", no "similar to Task N". Every code step carries the literal code. Every test step carries the literal test.

**3. Type consistency.** `Shared`, `SourceIssue`, `SourceBlindspot`, `minShared`, `maxNamedClusters`, `clusterEvidence`, `correlate`, `gather`, `sourceRank`, `renderShared`, `countCell`, `namedClusters`, `summarize`'s two-value signature, and `Report.Shared` are spelled identically in every task that names them. `minShared` is defined in Task 1 and read by Task 3's header format string — Task 3's Interfaces block names it as consumed.

**One asymmetry worth noting for the reviewer:** `minShared` lives in `correlate.go` (a correlation rule) while `maxNamedClusters` lives in `render.go` (a rendering rule). That is deliberate — the threshold is part of what a correlation *is*, the elision is part of how one *looks* — and `internal/fleet` is one package, so both are in scope everywhere.
