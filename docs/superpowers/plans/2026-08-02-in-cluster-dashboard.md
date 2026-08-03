# In-cluster dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve a read-only HTML dashboard at `/dashboard` on the watch daemon's existing metrics port, rendering only state the daemon already tracks.

**Architecture:** A new pure renderer package `internal/dashboard` exposes `Render(io.Writer, Input) error` over an embedded `html/template`, importing nothing from kubeagent. `internal/watch` copies its snapshot into a `dashboard.Input` under the read lock it already takes for `/issues`, renders into a buffer after unlocking, and writes the buffer to the response. A `--dashboard` flag gates handler registration, so a disabled daemon 404s the path through the mux rather than serving a switched-off page.

**Tech Stack:** Go 1.26 standard library only — `embed`, `html/template`, `io`, `time`, `sort`, `fmt`. Cobra for the flag (already a dependency). Helm for the chart value. Bash for the chaos assertions.

**Source of truth:** [docs/superpowers/specs/2026-08-02-in-cluster-dashboard-design.md](../specs/2026-08-02-in-cluster-dashboard-design.md), committed as `a8a4401` on `main`. The spec records five settled design questions with the alternatives each closes off. Reopening one is a defect, not an improvement.

**Branch:** `in-cluster-dashboard`, already cut from `main` at `a8a4401` and checked out.

**Ships as:** v1.2.0 (MINOR). Completes roadmap Theme G.

## Global Constraints

Every task's requirements implicitly include this section.

- **Every commit carries a `Signed-off-by` trailer matching its author** — use `git commit -s`. `main` enforces DCO. Verify the branch with `bash scripts/dco-check.sh main HEAD`.
- **No AI attribution anywhere** — no `Co-Authored-By: Claude` trailer, no "Generated with Claude Code" line, no mention of Claude/Claude Code/Anthropic in commits, code, comments, docs, or the changelog.
- **NO NEW DEPENDENCY.** `go.mod` and `go.sum` must not change. `html/template` is in the standard library.
- **`internal/dashboard` imports NOTHING from kubeagent** — only `embed`, `html/template`, `io`, `time`, `sort` and `fmt`. Enforced by a test written in Task 1, not by review.
- **`template.HTML`, `template.JS` and `template.URL` appear nowhere in `internal/dashboard`.** Those conversions are the only way to defeat contextual escaping. Enforced by a test written in Task 1.
- **Read-only toward the cluster.** The dashboard issues no cluster call at all. There is no code path from a dashboard request into `internal/remediate`. `internal/watch` must still import neither `internal/report` nor `internal/remediate` after this change.
- **No model call on a request path.** `/dashboard` reads the store the incident pipeline fills. *Read-only-toward-the-cluster* and *makes-no-model-call* are SEPARATE promises — never blur them into one sentence in a comment or a doc.
- **The six JSON documents do not move.** No `schemaVersion` bump, no `internal/jsonschema` change, no schema regeneration. The dashboard emits HTML, not a JSON document.
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.** Do NOT regenerate the demo GIF or `website/docs/quickstart.md`.
- **`internal/rbacprofile`'s Feature table and every generated RBAC manifest stay untouched** — the dashboard reads no new resource kind.
- **Untrusted API text is sanitized at ingress** via `internal/safetext`, never at a renderer. `clusterSnapshot.lastError` is already `redact.Error(err)` at `internal/watch/metrics.go:183`. The dashboard adds HTML escaping on top and must not become a second sanitization site.
- **TDD.** Write the failing test first, watch it fail, then implement.
- **`go test` runs with `-p 2`, never `-short`.** CI's `go test -race ./...` must stay green.
- **No secrets, credentials, private IPs, or internal hostnames anywhere** — including test fixtures, the golden HTML file, the fuzz seed corpus, chart values examples and every doc example. Use RFC 5737 addresses (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`) and RFC 2606 domains (`example.com`, `example.org`, `example.net`). **A fixture *named* like a credential is a defect even when its value is fake.**
- **URLs are credentials.** Nothing the dashboard emits may carry more than `scheme://host`, and no kubeconfig path, filesystem path or kubeconfig context name may appear in the page.
- **Never expose API keys to the shell.** The chaos harness runs with `ANTHROPIC_API_KEY` unset.
- **Work stays on branch `in-cluster-dashboard`.** Never commit to `main` directly.

### Environment

```bash
export PATH=$PATH:/usr/local/go/bin          # Go
export PATH=$PATH:$HOME/.local/bin           # helm
```

mkdocs exists only in the scratchpad venv:
`/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs`

### Per-task verification gate

```bash
go build ./... && go test ./... -p 2
```

Runs in well under a minute. Additional gates are named per task.

---

## File Structure

**Created:**

| File | Responsibility |
|---|---|
| `internal/dashboard/dashboard.go` | `Input` and its sub-types, `Render`, the unexported `view` types and `newView`, and the formatting helpers. Every decision lives here so the template only ranges and branches. |
| `internal/dashboard/dashboard.html.tmpl` | The embedded template. No `<script>`, no external asset, inline CSS only. |
| `internal/dashboard/imports_test.go` | The two source-level guard tests: no kubeagent imports, no unsafe `template.*` conversions. |
| `internal/dashboard/dashboard_test.go` | Escaping table, empty/starting states, determinism, total order. |
| `internal/dashboard/golden_test.go` | Golden snapshot with `-update`, plus the every-section coverage assertion. |
| `internal/dashboard/fuzz_test.go` | `FuzzDashboardRender` and its seed corpus. |
| `internal/dashboard/testdata/golden-dashboard.html` | The golden fixture. |
| `website/docs/features/dashboard.md` | Feature documentation, including the exposure posture. |

**Modified:**

| File | Change |
|---|---|
| `internal/watch/metrics.go` | `metrics.dashboard`/`metrics.version` fields, `dashboardInput()`, the conditional `/dashboard` handler, and the `renderDashboard` indirection. |
| `internal/watch/watch.go` | `Config.Dashboard`, `Config.Version`, and setting the two `metrics` fields before the server starts. |
| `internal/watch/metrics_test.go` | Handler tests: 404 disabled, 200 + content type enabled, copy-not-alias, 500 on render failure, concurrent GETs vs snapshot swaps. |
| `internal/cli/watch.go` | `--dashboard` flag with `KUBEAGENT_DASHBOARD`, wired into `watch.Config`. |
| `internal/cli/root.go` | `[--dashboard]` in the usage line. |
| `internal/cli/surface_test.go` | A row in `TestCommandSurfaceWatch`, plus the env-key list and its comment. |
| `internal/cli/cli_test.go` | `TestParseWatchFlagsCarriesEveryValue`: the flag, the env-key list and its comment. |
| `deploy/helm/kubeagent/values.yaml` | The `dashboard:` block. |
| `deploy/helm/kubeagent/templates/deployment.yaml` | The rendered `--dashboard` argument. |
| `deploy/helm/kubeagent/Chart.yaml` | Chart `version` MINOR bump (templates and values move). |
| `chaos/run.sh` | `--dashboard` on scenario 12's daemon, four new assertions. |
| `CLAUDE.md` | The `internal/dashboard` invariant, and 124 → 128. |
| `chaos/README.md`, `website/docs/compatibility.md`, `website/docs/roadmap.md` | 124 → 128; compatibility's unstable-markup bullet; Theme G complete. |
| `website/docs/features/watch-mode.md`, `website/mkdocs.yml`, `deploy/README.md`, `CHANGELOG.md` | Cross-reference, nav entry, chart docs, changelog entry. |

---

## Task 1: `internal/dashboard` skeleton and the two structural guard tests

These two tests are the invariants everything else is built inside. Writing them first means a later task cannot quietly violate one.

**Files:**

- Create: `internal/dashboard/dashboard.go`
- Create: `internal/dashboard/dashboard.html.tmpl`
- Test: `internal/dashboard/imports_test.go`

**Interfaces:**

- Consumes: nothing.
- Produces: package `dashboard` with `const RefreshSeconds = 30`, `var templateSource string` (embedded), `var tmpl *template.Template`. Later tasks add `Input` and `Render` to the same file.

- [ ] **Step 1: Write the two failing guard tests**

Create `internal/dashboard/imports_test.go`:

```go
package dashboard

import (
	"go/ast"
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

// TestNoKubeagentImport is the structural half of this package's security
// contract. internal/dashboard renders a page for a browser out of daemon
// state; the design makes reaching internal/remediate or internal/explain
// impossible by construction rather than by a rule someone has to remember,
// by forbidding every kubeagent import. That is strictly stronger than the
// two-entry rule internal/fuzzgen's `constrained` map applies to the other
// surface packages, which is why this package is absent from that map: the
// weaker rule there would add nothing to the stronger one here.
//
// Only non-test files are walked. A test file importing internal/watch would
// not compile anyway — watch imports this package, so the edge would close a
// cycle and the compiler refuses it.
func TestNoKubeagentImport(t *testing.T) {
	for _, path := range packageFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if strings.HasPrefix(p, modulePath) {
				t.Errorf("%s imports %s — internal/dashboard must import nothing from kubeagent", path, p)
			}
		}
	}
}

// unsafeConversions are the html/template types that carry a string into the
// document without escaping it. The design names HTML, JS and URL; the other
// four defeat the same boundary in the same way, so all seven are refused.
var unsafeConversions = map[string]bool{
	"HTML": true, "JS": true, "URL": true,
	"HTMLAttr": true, "CSS": true, "JSStr": true, "Srcset": true,
}

// TestNoUnsafeTemplateConversion asserts that contextual auto-escaping is this
// package's single escape boundary. Converting a string to one of the types
// above is the only way to defeat it, so their absence is what makes the
// escaping guarantee a property of the package rather than of its reviewers.
//
// Test files are walked too: a test that constructed template.HTML would be
// asserting on a value the renderer can never produce.
func TestNoUnsafeTemplateConversion(t *testing.T) {
	for _, path := range packageFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "template" {
				return true
			}
			if unsafeConversions[sel.Sel.Name] {
				t.Errorf("%s: references template.%s at %s — it defeats contextual escaping",
					path, sel.Sel.Name, fset.Position(sel.Pos()))
			}
			return true
		})
	}
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

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/dashboard -run 'TestNo' -v
```

Expected: FAIL — the package has no non-test Go file yet, so `packageFiles` finds only `imports_test.go`... which is enough for the glob, but `dashboard.go` does not exist and the package will not build once Step 3's template embed is referenced. At this point the honest failure is `go test` reporting the package builds but `TestNoKubeagentImport` walks zero non-test files. **That is a vacuous pass, not a fail.** Do Step 3 first, then Step 4 proves both guards genuinely fail.

- [ ] **Step 3: Write the minimal package**

Create `internal/dashboard/dashboard.go`:

```go
// Package dashboard renders the watch daemon's tracked state as one
// self-contained HTML page: the URL you hand someone who asks what is broken
// right now.
//
// The document is deliberately inert. It carries no <script>, no external
// stylesheet, font, or image, so it survives a strict Content-Security-Policy
// and performs no third-party fetch. The only dynamic behaviour is a
// <meta http-equiv="refresh"> carrying an interval and no URL — so the page
// emits no URL at all.
//
// The package imports nothing from kubeagent. That is a security property, not
// a style choice: it makes reaching internal/remediate or internal/explain
// structurally impossible rather than a rule someone has to remember, and
// imports_test.go enforces it. It is also why the view types below are defined
// here rather than reused from internal/watch, which is the caller.
//
// Render performs no cluster call and makes no model call. Those are two
// separate promises: the daemon's --explain feature does call a model, from
// the incident pipeline, never from an HTTP handler.
package dashboard

import (
	_ "embed"
	"html/template"
)

// RefreshSeconds is how often the page reloads itself. It is a constant with no
// flag behind it: a flag would be a stable surface forever, and the value buys
// nothing tunable — informers detect in roughly two seconds and the heartbeat
// is sixty, so thirty already sits between them.
const RefreshSeconds = 30

//go:embed dashboard.html.tmpl
var templateSource string

// tmpl is parsed once at package init so a malformed template fails in CI, not
// in front of an operator mid-incident. This is the same choice
// internal/htmlreport makes.
//
// html/template, never text/template: the contextual auto-escaping is this
// package's single escape boundary. Issue text, cluster names and model output
// are all free-form strings that land verbatim in a page a browser renders.
var tmpl = template.Must(template.New("dashboard").Parse(templateSource))
```

Create `internal/dashboard/dashboard.html.tmpl` with a placeholder body that Task 2 replaces:

```html
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>kubeagent</title>
</head>
<body>
</body>
</html>
```

- [ ] **Step 4: Prove each guard actually fails**

A guard test that has never been seen failing is a test that asserts nothing. Break each one in turn.

Guard 1 — temporarily add a kubeagent import to `internal/dashboard/dashboard.go`:

```go
import (
	_ "embed"
	"html/template"

	_ "github.com/imantaba/kubeagent/internal/safetext"
)
```

```bash
go test ./internal/dashboard -run TestNoKubeagentImport
```

Expected: FAIL with `dashboard.go imports github.com/imantaba/kubeagent/internal/safetext`. **Remove the import**, re-run, expect PASS.

Guard 2 — temporarily add to `internal/dashboard/dashboard.go`:

```go
var _ = template.HTML("")
```

```bash
go test ./internal/dashboard -run TestNoUnsafeTemplateConversion
```

Expected: FAIL naming `template.HTML` and its position. **Remove the line**, re-run, expect PASS.

- [ ] **Step 5: Run the full gate**

```bash
go build ./... && go test ./... -p 2
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dashboard/dashboard.go internal/dashboard/dashboard.html.tmpl internal/dashboard/imports_test.go
git commit -s -m "dashboard: package skeleton with its two structural guards

internal/dashboard renders daemon state for a browser. Two source-level
tests fix what it may never do before any rendering code exists: import
anything from kubeagent, and reference an html/template type that carries
a string into the document unescaped.

The import rule is stronger than the two-entry rule internal/fuzzgen
applies to the other surface packages — forbidding every kubeagent import
makes reaching remediate or explain impossible by construction."
```

---

## Task 2: `Input`, `Render`, and the page — header, clusters, incidents, totals

The renderer's core. SLO and explanations are Task 3.

**Files:**

- Modify: `internal/dashboard/dashboard.go`
- Modify: `internal/dashboard/dashboard.html.tmpl`
- Test: `internal/dashboard/dashboard_test.go` (create)

**Interfaces:**

- Consumes: `RefreshSeconds`, `tmpl` from Task 1.
- Produces, for Task 3 and Task 6:
  - `type Input struct { Version string; Now time.Time; Clusters []Cluster; Active, Resolved []Incident; Stats Stats; SLO []SLO; Explanations []Explanation; ExplainEnabled bool }`
  - `type Cluster struct { Name string; Up bool; LastScan, Error string }`
  - `type Incident struct { Cluster, Kind, Namespace, Name, Issue, FiringSince string; Firings int; Flapping bool; AgeSeconds int64; ResolvedAt string; ResolutionSeconds int64 }`
  - `type Stats struct { NewTotal, ResolvedTotal, FlapTotal, DroppedTotal int64; ResolutionSecondsSum float64; ResolutionSecondsCount int64 }`
  - `func Render(w io.Writer, in Input) error`
  - test helper `func payloadInput(payload string) Input` in `dashboard_test.go`, extended by Task 3.

  `SLO` and `Explanation` are declared in Task 3; declare the two `Input` fields in this task with those types only once Task 3 lands — see Step 3's note.

- [ ] **Step 1: Write the failing tests**

Create `internal/dashboard/dashboard_test.go`:

```go
package dashboard

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// fixedNow is the clock every test that compares bytes uses, so no fixture
// holds a time-varying value.
var fixedNow = time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC)

// render is the tests' entry point: it fails the test on a render error, which
// no test in this file expects, and returns the page as a string.
func render(t *testing.T, in Input) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, in); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return buf.String()
}

// payloadInput puts the same string into every field of the page that carries
// caller-supplied text. Task 3 extends it with the SLO and explanation fields.
func payloadInput(payload string) Input {
	return Input{
		Version: payload,
		Now:     fixedNow,
		Clusters: []Cluster{{
			Name:     payload,
			Up:       false,
			LastScan: "2026-08-02T09:29:00Z",
			Error:    payload,
		}},
		Active: []Incident{{
			Cluster: payload, Kind: payload, Namespace: payload, Name: payload,
			Issue: payload, FiringSince: "2026-08-02T09:00:00Z",
			Firings: 3, Flapping: true, AgeSeconds: 1800,
		}},
		Resolved: []Incident{{
			Cluster: payload, Kind: payload, Namespace: payload, Name: payload,
			Issue: payload, FiringSince: "2026-08-02T08:00:00Z",
			Firings: 1, ResolvedAt: "2026-08-02T08:30:00Z", ResolutionSeconds: 1800,
		}},
		Stats: Stats{
			NewTotal: 4, ResolvedTotal: 2, FlapTotal: 1, DroppedTotal: 0,
			ResolutionSecondsSum: 3600, ResolutionSecondsCount: 2,
		},
	}
}

// escapePayloads are the strings a hostile or merely unlucky cluster can put
// into a field the API server does not validate. Each must reach the page
// escaped and inert.
var escapePayloads = []struct{ name, payload string }{
	{"script tag", "<script>alert(1)</script>"},
	{"attribute break-out", `"><img src=x onerror=alert(1)>`},
	{"bare ampersand", "a & b"},
	{"single quote", "it's broken"},
	{"combining marks", "é́́"},
}

// TestRenderEscapesEveryStringField is the escaping table. It asserts the whole
// postcondition rather than a spelling: no executable markup survives anywhere
// in the page, and the payload's angle brackets arrive as entities.
func TestRenderEscapesEveryStringField(t *testing.T) {
	for _, tc := range escapePayloads {
		t.Run(tc.name, func(t *testing.T) {
			out := render(t, payloadInput(tc.payload))
			lower := strings.ToLower(out)
			if strings.Contains(lower, "<script") {
				t.Error("a <script tag reached the page")
			}
			// There is deliberately no assertion on the substring "onerror=".
			// Contextual escaping rewrites < > & " ' and nothing else, so in a
			// text node that substring survives verbatim inside
			// &#34;&gt;&lt;img src=x onerror=alert(1)&gt; — inert, because an
			// event handler runs only inside a tag, and the two assertions
			// around this comment are what prove no tag boundary was created.
			// Asserting its absence would fail correct code and could only be
			// satisfied by a second transformation on top of escaping, which
			// this package must not become.
			if strings.Contains(out, "<img") {
				t.Error("an <img element reached the page")
			}
			if strings.Contains(out, tc.payload) && strings.ContainsAny(tc.payload, "<>&") {
				t.Errorf("payload %q reached the page unescaped", tc.payload)
			}
		})
	}
}

// TestRenderEscapesAngleBracketsAsEntities pins the positive half: the payload
// is not merely absent, it is present in escaped form. A renderer that dropped
// the field entirely would pass the negative assertions above.
func TestRenderEscapesAngleBracketsAsEntities(t *testing.T) {
	out := render(t, payloadInput("<script>alert(1)</script>"))
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Error("the payload is neither present escaped nor rendered at all")
	}
}

// TestRenderEmptyInput is the starting state: a daemon that has just come up,
// with no cluster reporting and nothing tracked. It must render a page, not
// panic and not go dark.
func TestRenderEmptyInput(t *testing.T) {
	out := render(t, Input{Now: fixedNow})
	if !strings.Contains(out, "No active incidents") {
		t.Error("an empty page does not say there are no active incidents")
	}
	if strings.Contains(out, "NaN") {
		t.Error("an empty page renders NaN")
	}
}

// TestRenderUnscannedCluster covers the state before the first evaluation
// completes. An empty incident list from a cluster that has never been scanned
// must not read like a healthy one — that distinction is the whole reason the
// cluster strip exists.
func TestRenderUnscannedCluster(t *testing.T) {
	out := render(t, Input{
		Now:      fixedNow,
		Clusters: []Cluster{{Name: "example-cluster"}},
	})
	if !strings.Contains(out, "not scanned yet") {
		t.Error("a cluster with no completed evaluation does not say so")
	}
}

// TestRenderUnreachableCluster is the other half: reachable-and-quiet must not
// look like unreachable.
func TestRenderUnreachableCluster(t *testing.T) {
	out := render(t, Input{
		Now: fixedNow,
		Clusters: []Cluster{{
			Name:     "example-cluster",
			LastScan: "2026-08-02T09:29:00Z",
			Error:    "connection refused",
		}},
	})
	if !strings.Contains(out, "unreachable") {
		t.Error("a down cluster is not reported as unreachable")
	}
	if !strings.Contains(out, "connection refused") {
		t.Error("the cluster's error is not shown")
	}
}

// TestMeanTimeToResolutionWithNoResolutions asserts the tile shows an em dash
// rather than dividing by zero.
func TestMeanTimeToResolutionWithNoResolutions(t *testing.T) {
	out := render(t, Input{Now: fixedNow, Stats: Stats{ResolutionSecondsSum: 0, ResolutionSecondsCount: 0}})
	if strings.Contains(out, "NaN") || strings.Contains(out, "+Inf") {
		t.Error("mean time to resolution divided by zero")
	}
	if !strings.Contains(out, "—") {
		t.Error("mean time to resolution does not render an em dash when nothing has resolved")
	}
}

// TestClusterColumnOnlyWhenMulticluster keeps a single-cluster page from
// carrying a column that says the same thing on every row.
func TestClusterColumnOnlyWhenMulticluster(t *testing.T) {
	one := render(t, Input{
		Now:      fixedNow,
		Clusters: []Cluster{{Name: "example-cluster", Up: true, LastScan: "2026-08-02T09:29:00Z"}},
		Active:   []Incident{{Cluster: "example-cluster", Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff", AgeSeconds: 90}},
	})
	if strings.Count(one, "<th>Cluster</th>") != 0 {
		t.Error("a single-cluster page carries a Cluster column in an incident table")
	}
	two := render(t, Input{
		Now: fixedNow,
		Clusters: []Cluster{
			{Name: "example-a", Up: true, LastScan: "2026-08-02T09:29:00Z"},
			{Name: "example-b", Up: true, LastScan: "2026-08-02T09:29:00Z"},
		},
		Active: []Incident{{Cluster: "example-a", Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff", AgeSeconds: 90}},
	})
	if !strings.Contains(two, "<th>Cluster</th>") {
		t.Error("a multicluster page omits the Cluster column")
	}
}

// TestHumanDuration pins the duration spelling the incident tables use.
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{0, "0s"},
		{45, "45s"},
		{90, "1m 30s"},
		{3600, "1h 0m"},
		{7845, "2h 10m"},
		{86400, "1d 0h"},
		{-5, "0s"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.sec); got != tc.want {
			t.Errorf("humanDuration(%d) = %q, want %q", tc.sec, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/dashboard -run 'TestRender|TestMean|TestCluster|TestHuman' -v
```

Expected: FAIL to build — `undefined: Input`, `undefined: Render`, `undefined: humanDuration`.

- [ ] **Step 3: Implement the types, the view and `Render`**

Append to `internal/dashboard/dashboard.go`. Declare `SLO`/`Explanation`/`ExplainEnabled` on `Input` in **Task 3**; in this task `Input` carries only the fields below, and Task 3 adds the rest. That keeps this task compiling on its own.

```go
// Input is everything the page renders. It is a plain value: the caller copies
// its state in, and the renderer holds no reference into anything a goroutine
// can still mutate.
type Input struct {
	// Version is the kubeagent version stamped into the header.
	Version string
	// Now is the generation time. Zero means wall-clock, the same contract
	// report.Input.Now carries: a caller that forgets the clock gets today,
	// not year 1.
	Now time.Time
	// Clusters is the cluster strip. It is always rendered, even when both
	// incident lists are empty — an empty list from an unreachable cluster and
	// an empty list from a healthy one are not the same thing, and this band is
	// what tells them apart.
	Clusters []Cluster
	// Active and Resolved are the tracked incidents. Render sorts them into a
	// total order; the caller's order is not preserved and does not matter.
	Active   []Incident
	Resolved []Incident
	// Stats is the aggregate the daemon already keeps.
	Stats Stats
}

// Cluster is one watched cluster's reachability.
type Cluster struct {
	Name string
	Up   bool
	// LastScan is RFC 3339, or empty when no evaluation has completed. Empty is
	// the starting state, and it renders differently from "down".
	LastScan string
	// Error is the last read failure. It arrives already redacted — the caller
	// passes redact.Error's output — so this package escapes it and nothing
	// more. It must not become a second sanitization site.
	Error string
}

// Incident is one tracked issue instance. Active records carry AgeSeconds and
// leave the resolution fields zero; resolved records the reverse. Two fields
// rather than one pointer each because the two lists are already separate, and
// a nil pointer in a template is a defect waiting to happen.
type Incident struct {
	Cluster           string
	Kind              string
	Namespace         string
	Name              string
	Issue             string
	FiringSince       string
	Firings           int
	Flapping          bool
	AgeSeconds        int64
	ResolvedAt        string
	ResolutionSeconds int64
}

// Stats is the aggregate counter set behind the summary tiles.
type Stats struct {
	NewTotal               int64
	ResolvedTotal          int64
	FlapTotal              int64
	DroppedTotal           int64
	ResolutionSecondsSum   float64
	ResolutionSecondsCount int64
}

// Render writes the complete HTML page to w. It performs no cluster call and
// makes no model call.
func Render(w io.Writer, in Input) error {
	return tmpl.Execute(w, newView(in))
}

// view is the flat shape the template ranges over. Every decision lives in
// newView so the template stays free of logic beyond ranging and conditionals.
type view struct {
	Version        string
	Generated      string
	RefreshSeconds int
	// MultiCluster drops the Cluster column from the incident tables when only
	// one cluster is watched, where it would repeat the same value on every row.
	MultiCluster bool
	Clusters     []clusterRow
	Active       []incidentRow
	Resolved     []incidentRow
	Tiles        tiles
}

// clusterRow is one line of the cluster strip. State is a fixed keyword used as
// a CSS class, so the class and the visible label can never disagree.
type clusterRow struct {
	Name  string
	State string // "up" | "down" | "pending"
	Label string
	LastScan string
	Error    string
}

// incidentRow is one row of either incident table. Target is namespace/name, or
// name alone for a cluster-scoped object.
type incidentRow struct {
	Cluster    string
	Kind       string
	Target     string
	Issue      string
	Duration   string
	Firings    int
	Flapping   bool
	ResolvedAt string
	Resolution string
}

// tiles is the summary band. MTTR is a string because "nothing has resolved
// yet" is a legitimate state that no number expresses.
type tiles struct {
	New      int64
	Resolved int64
	Flapping int64
	Dropped  int64
	MTTR     string
}

// none is what every field prints when it has no value. One constant so the
// page never mixes spellings.
const none = "—"

// newView flattens Input into the template's model.
func newView(in Input) view {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	v := view{
		Version:        in.Version,
		Generated:      now.UTC().Format("2006-01-02 15:04:05 UTC"),
		RefreshSeconds: RefreshSeconds,
		MultiCluster:   len(in.Clusters) > 1,
		Tiles: tiles{
			New:      in.Stats.NewTotal,
			Resolved: in.Stats.ResolvedTotal,
			Flapping: in.Stats.FlapTotal,
			Dropped:  in.Stats.DroppedTotal,
			MTTR:     meanResolution(in.Stats.ResolutionSecondsSum, in.Stats.ResolutionSecondsCount),
		},
	}
	for _, c := range in.Clusters {
		state, label := clusterState(c)
		lastScan := c.LastScan
		if lastScan == "" {
			lastScan = none
		}
		errText := c.Error
		if errText == "" {
			errText = none
		}
		v.Clusters = append(v.Clusters, clusterRow{
			Name: c.Name, State: state, Label: label, LastScan: lastScan, Error: errText,
		})
	}

	active := append([]Incident(nil), in.Active...)
	sort.Slice(active, func(i, j int) bool {
		if active[i].AgeSeconds != active[j].AgeSeconds {
			return active[i].AgeSeconds > active[j].AgeSeconds // longest-firing first
		}
		return lessKey(active[i], active[j])
	})
	for _, r := range active {
		v.Active = append(v.Active, incidentRow{
			Cluster: r.Cluster, Kind: r.Kind, Target: target(r), Issue: r.Issue,
			Duration: humanDuration(r.AgeSeconds), Firings: r.Firings, Flapping: r.Flapping,
		})
	}

	resolved := append([]Incident(nil), in.Resolved...)
	sort.Slice(resolved, func(i, j int) bool {
		// ResolvedAt is RFC 3339 in UTC, fixed width and zero-padded, so a
		// lexicographic comparison is a chronological one.
		if resolved[i].ResolvedAt != resolved[j].ResolvedAt {
			return resolved[i].ResolvedAt > resolved[j].ResolvedAt // most recent first
		}
		return lessKey(resolved[i], resolved[j])
	})
	for _, r := range resolved {
		at := r.ResolvedAt
		if at == "" {
			at = none
		}
		v.Resolved = append(v.Resolved, incidentRow{
			Cluster: r.Cluster, Kind: r.Kind, Target: target(r), Issue: r.Issue,
			ResolvedAt: at, Resolution: humanDuration(r.ResolutionSeconds),
			Firings: r.Firings, Flapping: r.Flapping,
		})
	}
	return v
}

// lessKey is the tiebreaker chain both tables share. Cluster, kind, namespace,
// name and issue are a tracked issue's identity — the daemon's key is
// kind/namespace/name/issue and a cluster name is unique within a daemon — so
// the chain is a total order, and two distinct rows can never compare equal.
// A partial order would let equal rows swap places between renders, which on a
// thirty-second reload is genuinely unusable.
func lessKey(a, b Incident) bool {
	if a.Cluster != b.Cluster {
		return a.Cluster < b.Cluster
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Issue < b.Issue
}

// target is how an object is named in a row: namespace/name, or name alone for
// a cluster-scoped object.
func target(r Incident) string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}

// clusterState classifies a cluster for the strip. The never-scanned case is
// first because a starting daemon reports up=false, and "starting up" is not
// the same statement as "unreachable".
func clusterState(c Cluster) (state, label string) {
	switch {
	case c.LastScan == "":
		return "pending", "starting up — not scanned yet"
	case c.Up:
		return "up", "up"
	default:
		return "down", "unreachable"
	}
}

// humanDuration spells a whole-second span the way an operator reads it. A
// negative span is impossible from the daemon (it floors at zero already) and
// is floored again here so a hostile Input cannot produce "-3m -20s".
func humanDuration(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	case sec < 86400:
		return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
	default:
		return fmt.Sprintf("%dd %dh", sec/86400, (sec%86400)/3600)
	}
}

// maxRenderableSeconds bounds what meanResolution will convert to an integer.
// A float64 outside int64's range converts with implementation-defined results
// in Go, which is exactly the class of defect the fuzz campaign behind Theme H
// slice 3 found in the DNS health parser. Anything past this is not a duration
// anyone will read, so it prints as no value at all.
const maxRenderableSeconds = 1e15

// meanResolution is the mean-time-to-resolution tile. Zero resolutions is a
// legitimate state, not a division to be papered over.
func meanResolution(sum float64, count int64) string {
	if count <= 0 || !finite(sum) {
		return none
	}
	avg := sum / float64(count)
	if avg < 0 || avg > maxRenderableSeconds {
		return none
	}
	return humanDuration(int64(avg))
}

// finite reports whether f is a real number: NaN fails f == f, and ±Inf fails
// f-f == 0, since ∞-∞ is NaN. math.IsNaN and math.IsInf say the same thing;
// these comparisons keep the package's import list to the standard-library
// packages the design named, which is the list imports_test.go asserts against.
func finite(f float64) bool { return f == f && f-f == 0 }
```

Update the import block at the top of the file to:

```go
import (
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"sort"
	"time"
)
```

- [ ] **Step 4: Write the template**

Replace `internal/dashboard/dashboard.html.tmpl` entirely:

```html
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="{{ .RefreshSeconds }}">
<title>kubeagent — cluster incidents</title>
<style>
:root { color-scheme: light dark; }
body { font: 14px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; margin: 0; padding: 1.5rem; }
h1 { font-size: 1.4rem; margin: 0; }
h2 { font-size: 1.05rem; margin: 2rem 0 .6rem; border-bottom: 1px solid currentColor; padding-bottom: .3rem; opacity: .95; }
.meta { opacity: .7; margin: .25rem 0 0; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: .35rem .6rem; border-bottom: 1px solid rgba(128,128,128,.35); vertical-align: top; }
th { font-weight: 600; opacity: .8; white-space: nowrap; }
tr.down td { font-weight: 600; }
tr.down td:first-child::before { content: "\25CF\00a0"; }
tr.pending td { opacity: .75; }
.empty { opacity: .7; }
.badge { border: 1px solid currentColor; border-radius: .6rem; padding: 0 .4rem; font-size: .8rem; }
.tiles { display: flex; flex-wrap: wrap; gap: 1rem; }
.tile { border: 1px solid rgba(128,128,128,.45); border-radius: .4rem; padding: .6rem 1rem; min-width: 7rem; }
.tile .n { display: block; font-size: 1.4rem; font-weight: 600; }
.tile .l { display: block; opacity: .7; font-size: .8rem; }
</style>
</head>
<body>
<header>
<h1>kubeagent</h1>
<p class="meta">version {{ .Version }} · generated {{ .Generated }} · reloads every {{ .RefreshSeconds }}s</p>
</header>

<h2>Clusters</h2>
{{- if .Clusters }}
<table>
<thead><tr><th>Name</th><th>State</th><th>Last evaluation</th><th>Error</th></tr></thead>
<tbody>
{{- range .Clusters }}
<tr class="{{ .State }}"><td>{{ .Name }}</td><td>{{ .Label }}</td><td>{{ .LastScan }}</td><td>{{ .Error }}</td></tr>
{{- end }}
</tbody>
</table>
{{- else }}
<p class="empty">No cluster configured.</p>
{{- end }}

<h2>Active incidents ({{ len .Active }})</h2>
{{- if .Active }}
<table>
<thead><tr>{{ if .MultiCluster }}<th>Cluster</th>{{ end }}<th>Kind</th><th>Object</th><th>Issue</th><th>Firing for</th><th>Firings</th><th>Flapping</th></tr></thead>
<tbody>
{{- range .Active }}
<tr>{{ if $.MultiCluster }}<td>{{ .Cluster }}</td>{{ end }}<td>{{ .Kind }}</td><td>{{ .Target }}</td><td>{{ .Issue }}</td><td>{{ .Duration }}</td><td>{{ .Firings }}</td><td>{{ if .Flapping }}<span class="badge">flapping</span>{{ else }}—{{ end }}</td></tr>
{{- end }}
</tbody>
</table>
{{- else }}
<p class="empty">No active incidents.</p>
{{- end }}

<h2>Resolved incidents ({{ len .Resolved }})</h2>
{{- if .Resolved }}
<table>
<thead><tr>{{ if .MultiCluster }}<th>Cluster</th>{{ end }}<th>Kind</th><th>Object</th><th>Issue</th><th>Resolved at</th><th>Time to resolution</th><th>Firings</th></tr></thead>
<tbody>
{{- range .Resolved }}
<tr>{{ if $.MultiCluster }}<td>{{ .Cluster }}</td>{{ end }}<td>{{ .Kind }}</td><td>{{ .Target }}</td><td>{{ .Issue }}</td><td>{{ .ResolvedAt }}</td><td>{{ .Resolution }}</td><td>{{ .Firings }}</td></tr>
{{- end }}
</tbody>
</table>
{{- else }}
<p class="empty">No incident has resolved yet.</p>
{{- end }}

<h2>Totals</h2>
<div class="tiles">
<div class="tile"><span class="n">{{ .Tiles.New }}</span><span class="l">new</span></div>
<div class="tile"><span class="n">{{ .Tiles.Resolved }}</span><span class="l">resolved</span></div>
<div class="tile"><span class="n">{{ .Tiles.Flapping }}</span><span class="l">flapping</span></div>
<div class="tile"><span class="n">{{ .Tiles.Dropped }}</span><span class="l">dropped</span></div>
<div class="tile"><span class="n">{{ .Tiles.MTTR }}</span><span class="l">mean time to resolution</span></div>
</div>
</body>
</html>
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/dashboard -v
```

Expected: PASS, including the two guards from Task 1.

- [ ] **Step 6: Run the full gate**

```bash
go build ./... && go test ./... -p 2
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/dashboard/dashboard.go internal/dashboard/dashboard.html.tmpl internal/dashboard/dashboard_test.go
git commit -s -m "dashboard: render clusters, incidents and totals

Input carries a copy of the daemon's tracked state; newView flattens it so
the template only ranges and branches. Active incidents sort longest-firing
first and resolved most-recently-first, both falling through the same
five-field tiebreaker chain — a tracked issue's identity — so the order is
total and two renders of the same state are byte-identical.

The cluster strip is always present: an empty incident list from an
unreachable cluster and one from a healthy cluster are not the same thing,
and a cluster that has not completed an evaluation says so rather than
reading as down."
```

---

## Task 3: The SLO and explanations sections

Both are conditional sections with the same shape, which is why they land together.

**Files:**

- Modify: `internal/dashboard/dashboard.go`
- Modify: `internal/dashboard/dashboard.html.tmpl`
- Modify: `internal/dashboard/dashboard_test.go`

**Interfaces:**

- Consumes: `Input`, `view`, `newView`, `none`, `finite`, `payloadInput` from Task 2.
- Produces, for Task 6:
  - `type SLO struct { Cluster string; Target float64; Windows []SLOWindow }`
  - `type SLOWindow struct { Name string; Availability, BurnRate, Coverage float64 }`
  - `type Explanation struct { Cluster, Kind, Namespace, Name string; Issues []string; ExplainedAt, Model, Text string }`
  - `Input.SLO []SLO`, `Input.Explanations []Explanation`, `Input.ExplainEnabled bool`

- [ ] **Step 1: Write the failing tests**

Append to `internal/dashboard/dashboard_test.go`:

```go
// sloInput is an SLO section with both windows populated and coverage below the
// suppression floor on the fast window.
func sloInput() Input {
	return Input{
		Now: fixedNow,
		SLO: []SLO{{
			Cluster: "example-cluster",
			Target:  0.999,
			Windows: []SLOWindow{
				{Name: "fast (1h)", Availability: 0.9, BurnRate: 100, Coverage: 0.4},
				{Name: "slow (6h)", Availability: 0.9995, BurnRate: 0.5, Coverage: 0.95},
			},
		}},
	}
}

// TestSLOSectionAbsentWhenNoSLO keeps the section out of a page for a daemon
// running without --slo-target, rather than rendering an empty table.
func TestSLOSectionAbsentWhenNoSLO(t *testing.T) {
	if out := render(t, Input{Now: fixedNow}); strings.Contains(out, "<h2>SLO") {
		t.Error("the SLO section renders with no SLO configured")
	}
}

// TestSLOSectionRenders covers the numbers and the coverage annotation. The
// suppression note matches what the kubeagent_slo_window_coverage_ratio help
// text already documents: below 0.6 the burn alert is suppressed.
func TestSLOSectionRenders(t *testing.T) {
	out := render(t, sloInput())
	for _, want := range []string{"<h2>SLO", "example-cluster", "99.90%", "fast (1h)", "slow (6h)", "burn alert suppressed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the SLO section does not contain %q", want)
		}
	}
	if strings.Contains(out, "NaN") {
		t.Error("the SLO section renders NaN")
	}
}

// TestSLOSectionSurvivesNonFiniteNumbers is the arithmetic boundary. A burn
// rate is a quotient, and a quotient by a target of exactly 1 is infinite.
func TestSLOSectionSurvivesNonFiniteNumbers(t *testing.T) {
	out := render(t, Input{
		Now: fixedNow,
		SLO: []SLO{{
			Cluster: "example-cluster",
			Target:  1,
			Windows: []SLOWindow{{Name: "fast (1h)", Availability: 0.5, BurnRate: math.Inf(1), Coverage: math.NaN()}},
		}},
	})
	if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
		t.Error("a non-finite SLO number reached the page")
	}
}

// TestExplanationsSectionAbsentWhenDisabled keeps the section off a page for a
// daemon running without --explain.
func TestExplanationsSectionAbsentWhenDisabled(t *testing.T) {
	if out := render(t, Input{Now: fixedNow}); strings.Contains(out, "<h2>Explanations") {
		t.Error("the explanations section renders with --explain off")
	}
}

// TestExplanationsSectionEmptyWhenEnabled asserts the section is present but
// empty when --explain is on and nothing has been explained yet. That is a
// distinguishable state an operator paying for the feature needs to see; a
// section that vanished would look identical to the feature being off.
func TestExplanationsSectionEmptyWhenEnabled(t *testing.T) {
	out := render(t, Input{Now: fixedNow, ExplainEnabled: true})
	if !strings.Contains(out, "<h2>Explanations") {
		t.Error("the explanations section is absent with --explain on")
	}
	if !strings.Contains(out, "No incident has been explained yet") {
		t.Error("an enabled but empty explanations section does not say so")
	}
}

// TestExplanationsRender covers the populated case.
func TestExplanationsRender(t *testing.T) {
	out := render(t, Input{
		Now:            fixedNow,
		ExplainEnabled: true,
		Explanations: []Explanation{{
			Cluster: "example-cluster", Kind: "Deployment", Namespace: "example-ns", Name: "web",
			Issues: []string{"ImagePullBackOff", "Degraded"},
			ExplainedAt: "2026-08-02T09:20:00Z", Model: "example-model",
			Text: "The image tag does not exist in the registry.",
		}},
	})
	for _, want := range []string{"example-ns/web", "ImagePullBackOff, Degraded", "example-model", "does not exist in the registry"} {
		if !strings.Contains(out, want) {
			t.Errorf("the explanations section does not contain %q", want)
		}
	}
}

// TestExplanationNamesItsClusterOnlyWhenMulticluster mirrors the rule the
// incident tables follow. Explanations are a flat list, not a block per
// cluster, so on a one-cluster page the name is on the header already and
// repeating it on every article is noise — but on a multi-cluster page an
// explanation that does not say which cluster it came from is unreadable.
func TestExplanationNamesItsClusterOnlyWhenMulticluster(t *testing.T) {
	explanations := []Explanation{{
		Cluster: "example-cluster", Kind: "Deployment", Namespace: "example-ns", Name: "web",
		ExplainedAt: "2026-08-02T09:20:00Z", Model: "example-model",
		Text: "The image tag does not exist in the registry.",
	}}
	one := render(t, Input{
		Now: fixedNow, ExplainEnabled: true, Explanations: explanations,
		Clusters: []Cluster{{Name: "example-cluster", Up: true, LastScan: "2026-08-02T09:00:00Z"}},
	})
	if strings.Contains(one, "example-cluster · Deployment") {
		t.Error("a single-cluster page repeats the cluster name on an explanation")
	}
	two := render(t, Input{
		Now: fixedNow, ExplainEnabled: true, Explanations: explanations,
		Clusters: []Cluster{
			{Name: "example-cluster", Up: true, LastScan: "2026-08-02T09:00:00Z"},
			{Name: "example-other", Up: true, LastScan: "2026-08-02T09:00:00Z"},
		},
	})
	if !strings.Contains(two, "example-cluster · Deployment") {
		t.Error("a multi-cluster page does not say which cluster an explanation came from")
	}
}
```

Extend `payloadInput` so the escaping table covers the two new sections. Replace its `return` statement's struct literal tail — after the `Stats:` field — with:

```go
		Stats: Stats{
			NewTotal: 4, ResolvedTotal: 2, FlapTotal: 1, DroppedTotal: 0,
			ResolutionSecondsSum: 3600, ResolutionSecondsCount: 2,
		},
		SLO: []SLO{{
			Cluster: payload,
			Target:  0.999,
			Windows: []SLOWindow{{Name: payload, Availability: 0.9, BurnRate: 2, Coverage: 0.8}},
		}},
		ExplainEnabled: true,
		Explanations: []Explanation{{
			Cluster: payload, Kind: payload, Namespace: payload, Name: payload,
			Issues:      []string{payload},
			ExplainedAt: "2026-08-02T09:20:00Z",
			Model:       payload,
			Text:        payload,
		}},
	}
}
```

Add `"math"` to `dashboard_test.go`'s import block. (`math` is a test-only import; `imports_test.go` forbids kubeagent imports, not standard-library ones, and only walks non-test files for that rule anyway.)

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/dashboard -run 'TestSLO|TestExplanations' -v
```

Expected: FAIL to build — `undefined: SLO`, `undefined: SLOWindow`, `undefined: Explanation`, and `Input` has no field `ExplainEnabled`.

- [ ] **Step 3: Implement the types and the view extension**

Add the three fields to `Input`, immediately after `Stats`:

```go
	// SLO is one entry per cluster with SLO tracking on. Empty means the
	// section does not render — a daemon running without --slo-target should
	// not carry an empty table.
	SLO []SLO
	// ExplainEnabled reports whether --explain is on. It gates the section
	// independently of Explanations, because "on and nothing explained yet" is
	// a state an operator paying for the feature needs to be able to see, and a
	// vanishing section would look identical to the feature being off.
	ExplainEnabled bool
	// Explanations is the latest explanation per object, as the incident
	// pipeline computed it. Rendering one makes no model call.
	Explanations []Explanation
```

Add the new types beside the others:

```go
// SLO is one cluster's error-budget state.
type SLO struct {
	Cluster string
	// Target is the availability target as a ratio in (0,1).
	Target  float64
	Windows []SLOWindow
}

// SLOWindow is one measurement window. Name is the caller's label — the
// renderer never interprets it — and matches the `window` label on the
// kubeagent_slo_* series so the page and a Grafana panel name the same thing.
type SLOWindow struct {
	Name         string
	Availability float64
	BurnRate     float64
	Coverage     float64
}

// Explanation is one object's latest explanation. Text is model output and gets
// exactly the same escaping as everything else: it is laid out with
// white-space: pre-wrap and never parsed as markdown, since parsing means
// unescaping.
type Explanation struct {
	Cluster     string
	Kind        string
	Namespace   string
	Name        string
	Issues      []string
	ExplainedAt string
	Model       string
	Text        string
}
```

Add the view types:

```go
// sloView is one cluster's SLO block.
type sloView struct {
	Cluster string
	Target  string
	Windows []sloWindowRow
}

// sloWindowRow is one window's line. Suppressed carries the same threshold the
// kubeagent_slo_window_coverage_ratio help text documents: below 0.6 the burn
// alert is suppressed, and a reader looking at a high burn rate needs to know
// that before acting on it.
type sloWindowRow struct {
	Name            string
	Availability    string
	BurnRate        string
	BudgetRemaining string
	Coverage        string
	Suppressed      bool
}

// explanationRow is one explanation as the page shows it.
type explanationRow struct {
	Cluster     string
	Kind        string
	Target      string
	Issues      string
	Model       string
	ExplainedAt string
	Text        string
}

// coverageFloor is the coverage below which the burn alert is suppressed. It
// matches internal/watch's metric help text; the two must not drift.
const coverageFloor = 0.6
```

Add to `view`:

```go
	SLO              []sloView
	ShowExplanations bool
	Explanations     []explanationRow
```

Add the formatting helpers:

```go
// percent renders a ratio as a percentage. A non-finite value prints as no
// value: a burn rate is a quotient, and a target of exactly 1 makes it
// infinite.
func percent(f float64) string {
	if !finite(f) {
		return none
	}
	return fmt.Sprintf("%.2f%%", f*100)
}

// ratio renders a plain multiple, such as a burn rate.
func ratio(f float64) string {
	if !finite(f) {
		return none
	}
	return fmt.Sprintf("%.2f", f)
}

// budgetRemaining is the fraction of the error budget left over the window,
// clamped to [0,1] — the same definition the
// kubeagent_slo_error_budget_remaining_ratio series carries. A burn above 1x means
// the budget is already spent.
func budgetRemaining(burn float64) string {
	if !finite(burn) {
		return none
	}
	left := 1 - burn
	if left < 0 {
		left = 0
	}
	if left > 1 {
		left = 1
	}
	return percent(left)
}
```

Extend `newView`, after the resolved loop and before `return v`:

```go
	for _, s := range in.SLO {
		sv := sloView{Cluster: s.Cluster, Target: percent(s.Target)}
		for _, w := range s.Windows {
			sv.Windows = append(sv.Windows, sloWindowRow{
				Name:            w.Name,
				Availability:    percent(w.Availability),
				BurnRate:        ratio(w.BurnRate),
				BudgetRemaining: budgetRemaining(w.BurnRate),
				Coverage:        percent(w.Coverage),
				Suppressed:      finite(w.Coverage) && w.Coverage < coverageFloor,
			})
		}
		v.SLO = append(v.SLO, sv)
	}

	v.ShowExplanations = in.ExplainEnabled
	for _, x := range in.Explanations {
		v.Explanations = append(v.Explanations, explanationRow{
			Cluster:     x.Cluster,
			Kind:        x.Kind,
			Target:      target(Incident{Namespace: x.Namespace, Name: x.Name}),
			Issues:      strings.Join(x.Issues, ", "),
			Model:       x.Model,
			ExplainedAt: x.ExplainedAt,
			Text:        x.Text,
		})
	}
```

Add `"strings"` to `dashboard.go`'s import block. It is a standard-library package, so the import guard is unaffected.

- [ ] **Step 4: Extend the template**

Insert before the closing `</body>`, after the Totals block:

```html
{{- if .SLO }}
<h2>SLO</h2>
{{- range .SLO }}
<h3>{{ .Cluster }} — target {{ .Target }}</h3>
<table>
<thead><tr><th>Window</th><th>Availability</th><th>Burn rate</th><th>Error budget left</th><th>Coverage</th></tr></thead>
<tbody>
{{- range .Windows }}
<tr><td>{{ .Name }}</td><td>{{ .Availability }}</td><td>{{ .BurnRate }}</td><td>{{ .BudgetRemaining }}</td><td>{{ .Coverage }}{{ if .Suppressed }} <span class="badge">burn alert suppressed</span>{{ end }}</td></tr>
{{- end }}
</tbody>
</table>
{{- end }}
{{- end }}

{{- if .ShowExplanations }}
<h2>Explanations ({{ len .Explanations }})</h2>
{{- if .Explanations }}
{{- range .Explanations }}
<article>
<h3>{{ if $.MultiCluster }}{{ .Cluster }} · {{ end }}{{ .Kind }} {{ .Target }}</h3>
<p class="meta">{{ .Issues }} · {{ .Model }} · {{ .ExplainedAt }}</p>
<pre class="explanation">{{ .Text }}</pre>
</article>
{{- end }}
{{- else }}
<p class="empty">No incident has been explained yet.</p>
{{- end }}
{{- end }}
```

Add to the `<style>` block, after the `.tile .l` rule:

```css
h3 { font-size: .95rem; margin: 1rem 0 .4rem; opacity: .9; }
pre.explanation { white-space: pre-wrap; word-break: break-word; margin: 0; font: inherit; border-left: 3px solid rgba(128,128,128,.45); padding-left: .8rem; }
article { margin-bottom: 1.2rem; }
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/dashboard -v
```

Expected: PASS — including the escaping table, which now covers the SLO window name and every explanation field.

- [ ] **Step 6: Run the full gate**

```bash
go build ./... && go test ./... -p 2
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/dashboard/dashboard.go internal/dashboard/dashboard.html.tmpl internal/dashboard/dashboard_test.go
git commit -s -m "dashboard: SLO and explanations sections

Both sections are conditional and both refuse to print a number that is not
one: a burn rate is a quotient, a target of exactly 1 makes it infinite, and
a page that renders NaN is worse than one that renders an em dash.

The explanations section is gated on --explain being on rather than on the
list being non-empty. 'Explanations are on and nothing has been explained
yet' is a state an operator paying for the feature needs to see, and a
section that vanished would look identical to the feature being off.

Explanation text is escaped like every other field and laid out with
white-space: pre-wrap. It is never parsed as markdown, because parsing
means unescaping."
```

---

## Task 4: Determinism, total order, and the golden snapshot

**Files:**

- Test: `internal/dashboard/golden_test.go` (create)
- Test: `internal/dashboard/dashboard_test.go` (extend)
- Create: `internal/dashboard/testdata/golden-dashboard.html`

**Interfaces:**

- Consumes: everything from Tasks 2 and 3.
- Produces: `func goldenInput() Input` in `golden_test.go`, and the fixture file.

- [ ] **Step 1: Write the failing determinism and total-order tests**

Append to `internal/dashboard/dashboard_test.go`:

```go
// TestRenderIsDeterministic asserts that the same Input rendered twice produces
// the same bytes. Map iteration order and an unstable sort are the two ways
// this fails, and both would show up as a page that reshuffles itself every
// thirty seconds.
func TestRenderIsDeterministic(t *testing.T) {
	in := goldenInput()
	first := render(t, in)
	for i := 0; i < 20; i++ {
		if got := render(t, in); got != first {
			t.Fatalf("render %d differs from the first render", i+2)
		}
	}
}

// TestRenderIgnoresInputOrder is what actually proves the order is total. If
// any pair of rows compared equal, some permutation would place them in the
// other order and the bytes would differ.
func TestRenderIgnoresInputOrder(t *testing.T) {
	in := goldenInput()
	want := render(t, in)
	for i := 0; i < len(in.Active); i++ {
		shuffled := goldenInput()
		// A rotation by i is a deterministic permutation — no random source, so
		// a failure reproduces exactly.
		shuffled.Active = append(append([]Incident(nil), shuffled.Active[i:]...), shuffled.Active[:i]...)
		shuffled.Resolved = append(append([]Incident(nil), shuffled.Resolved[i%len(shuffled.Resolved):]...), shuffled.Resolved[:i%len(shuffled.Resolved)]...)
		if got := render(t, shuffled); got != want {
			t.Errorf("rotating the input by %d changed the page — the sort order is not total", i)
		}
	}
}

// TestEqualDurationsStillOrderTotally is the case the age-first sort makes
// likely: two incidents firing for exactly the same number of seconds. They
// must fall through to the tiebreaker chain, not to whatever order they
// arrived in.
func TestEqualDurationsStillOrderTotally(t *testing.T) {
	a := Incident{Cluster: "example-a", Kind: "Deployment", Namespace: "example-ns", Name: "alpha", Issue: "Degraded", AgeSeconds: 600}
	b := Incident{Cluster: "example-a", Kind: "Deployment", Namespace: "example-ns", Name: "beta", Issue: "Degraded", AgeSeconds: 600}
	one := render(t, Input{Now: fixedNow, Active: []Incident{a, b}})
	two := render(t, Input{Now: fixedNow, Active: []Incident{b, a}})
	if one != two {
		t.Error("two incidents with equal firing durations render in input order")
	}
	if strings.Index(one, "alpha") > strings.Index(one, "beta") {
		t.Error("the tiebreaker chain did not order equal-duration rows by name")
	}
}
```

- [ ] **Step 2: Write the failing golden test**

Create `internal/dashboard/golden_test.go`:

```go
package dashboard

import (
	"bytes"
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const goldenPath = "testdata/golden-dashboard.html"

// goldenInput is the fixture: two clusters, one of them unreachable, active and
// resolved incidents across both, SLO on, and explanations present — so the
// snapshot exercises every section of the page in one comparison.
//
// Every value is an RFC 2606 name or an obviously synthetic label. Nothing here
// is named like a credential, and no value is a path, a URL or an address.
func goldenInput() Input {
	return Input{
		Version: "v1.2.0",
		Now:     fixedNow,
		Clusters: []Cluster{
			{Name: "example-a", Up: true, LastScan: "2026-08-02T09:29:30Z"},
			{Name: "example-b", Up: false, LastScan: "2026-08-02T09:14:00Z", Error: "connection refused"},
			{Name: "example-c"},
		},
		Active: []Incident{
			{Cluster: "example-a", Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff",
				FiringSince: "2026-08-02T09:00:00Z", Firings: 2, AgeSeconds: 1800},
			{Cluster: "example-a", Kind: "Pod", Namespace: "example-ns", Name: "worker-0", Issue: "CrashLoopBackOff",
				FiringSince: "2026-08-02T08:30:00Z", Firings: 7, Flapping: true, AgeSeconds: 3600},
			{Cluster: "example-b", Kind: "Node", Name: "node-1", Issue: "NotReady",
				FiringSince: "2026-08-02T09:25:00Z", Firings: 1, AgeSeconds: 300},
		},
		Resolved: []Incident{
			{Cluster: "example-a", Kind: "StatefulSet", Namespace: "example-ns", Name: "cache", Issue: "Degraded",
				FiringSince: "2026-08-02T07:00:00Z", Firings: 1, ResolvedAt: "2026-08-02T07:45:00Z", ResolutionSeconds: 2700},
			{Cluster: "example-b", Kind: "Service", Namespace: "example-ns", Name: "api", Issue: "NoEndpoints",
				FiringSince: "2026-08-02T08:10:00Z", Firings: 3, ResolvedAt: "2026-08-02T08:20:00Z", ResolutionSeconds: 600},
		},
		Stats: Stats{
			NewTotal: 9, ResolvedTotal: 6, FlapTotal: 2, DroppedTotal: 1,
			ResolutionSecondsSum: 9900, ResolutionSecondsCount: 6,
		},
		SLO: []SLO{{
			Cluster: "example-a",
			Target:  0.999,
			Windows: []SLOWindow{
				{Name: "fast (1h)", Availability: 0.982, BurnRate: 18, Coverage: 0.45},
				{Name: "slow (6h)", Availability: 0.9993, BurnRate: 0.7, Coverage: 0.98},
			},
		}},
		ExplainEnabled: true,
		Explanations: []Explanation{{
			Cluster: "example-a", Kind: "Deployment", Namespace: "example-ns", Name: "web",
			Issues:      []string{"ImagePullBackOff"},
			ExplainedAt: "2026-08-02T09:05:00Z",
			Model:       "example-model",
			Text:        "The image tag referenced by the Deployment does not exist in the registry.\nRoll back to the previous tag, or push the missing tag.",
		}},
	}
}

// TestGoldenDashboard snapshots the whole page. The markup is an unstable
// surface — website/docs/compatibility.md says so — and this fixture is a
// regression guard that keeps a change to it deliberate, not a promise to a
// consumer. Anyone who wants a contract parses /issues, which is versioned.
func TestGoldenDashboard(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, goldenInput()); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.Bytes()
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", goldenPath, err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("dashboard output changed:\n%s\n\n"+
			"If this change is intended, run:\n"+
			"  go test ./internal/dashboard -run TestGoldenDashboard -update\n"+
			"and review the diff.", firstDiff(string(want), string(got)))
	}
}

// TestGoldenInputCoversEverySection stops the fixture from quietly narrowing.
// A golden test over an input that exercises three of seven sections is a
// regression guard over three of seven sections.
func TestGoldenInputCoversEverySection(t *testing.T) {
	out := render(t, goldenInput())
	for _, section := range []string{
		"<h2>Clusters",
		"<h2>Active incidents",
		"<h2>Resolved incidents",
		"<h2>Totals",
		"<h2>SLO",
		"<h2>Explanations",
	} {
		if !strings.Contains(out, section) {
			t.Errorf("the golden input does not exercise the %q section", section)
		}
	}
	if !strings.Contains(out, "unreachable") {
		t.Error("the golden input has no unreachable cluster")
	}
	if !strings.Contains(out, "not scanned yet") {
		t.Error("the golden input has no cluster in the starting state")
	}
	if !strings.Contains(out, "<th>Cluster</th>") {
		t.Error("the golden input is not multicluster, so the Cluster column is never exercised")
	}
	if !strings.Contains(out, "burn alert suppressed") {
		t.Error("the golden input never exercises the coverage suppression note")
	}
}

// firstDiff names the first line where two renders diverge, so a failure points
// at a line instead of printing two pages.
func firstDiff(want, got string) string {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + a + "\n  got:  " + b
		}
	}
	return "(no line differs — check trailing bytes)"
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
go test ./internal/dashboard -run 'TestGolden|TestRenderIsDeterministic|TestRenderIgnoresInputOrder|TestEqualDurations' -v
```

Expected: `TestGoldenDashboard` FAILs with `reading testdata/golden-dashboard.html: ... no such file or directory`. The determinism tests should PASS — the sort was written to be total in Task 2 — but they must be seen running against the golden fixture, so run them again after Step 4.

If `TestRenderIgnoresInputOrder` or `TestEqualDurationsStillOrderTotally` fails, the tiebreaker chain in `lessKey` is wrong. Fix it before generating the fixture: a golden file generated from a non-deterministic renderer is worse than no golden file.

- [ ] **Step 4: Generate the fixture**

```bash
mkdir -p internal/dashboard/testdata
go test ./internal/dashboard -run TestGoldenDashboard -update
```

Then **read the generated file** and check it by eye:

```bash
cat internal/dashboard/testdata/golden-dashboard.html
```

It must contain no `<script`, no `http://` or `https://` beyond nothing at all, no filesystem path, no address, and nothing named like a credential. If any appears, the renderer is wrong — fix the renderer, not the fixture.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/dashboard -v
```

Expected: PASS.

- [ ] **Step 6: Run the full gate**

```bash
go build ./... && go test ./... -p 2
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/dashboard/golden_test.go internal/dashboard/dashboard_test.go internal/dashboard/testdata/golden-dashboard.html
git commit -s -m "dashboard: determinism, total order and a golden snapshot

The order test rotates the input rather than shuffling it randomly, so a
failure reproduces exactly. If any two rows compared equal under the
tiebreaker chain, some rotation would place them the other way round and the
bytes would differ — which is what makes this a proof that the order is
total rather than a check that it is usually stable.

The fixture covers all six sections, an unreachable cluster, a cluster that
has never been evaluated, the multicluster column and the coverage
suppression note, and TestGoldenInputCoversEverySection keeps it from
quietly narrowing to a subset."
```

---

## Task 5: `FuzzDashboardRender`

Joins the `go test -fuzz` suite Theme H slice 3 opened. Same risk class: no value a cluster can produce may panic a render or reach a browser as markup.

**Files:**

- Test: `internal/dashboard/fuzz_test.go` (create)
- Modify: `.github/workflows/fuzz.yml` — the nightly matrix names every `(package, target)` pair explicitly, so a target it does not list never runs a campaign.

**Interfaces:**

- Consumes: `Input`, `Render` from Tasks 2 and 3.
- Produces: nothing other tasks consume.

- [ ] **Step 1: Write the failing fuzz target**

Create `internal/dashboard/fuzz_test.go`:

```go
package dashboard

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// FuzzDashboardRender asserts the renderer's whole postcondition on arbitrary
// input: it never errors, it never panics, it is deterministic, and no angle
// bracket originating in the input reaches the output.
//
// The bracket count is the load-bearing assertion. Comparing the page against
// the same page rendered from input with every '<' and '>' stripped isolates
// the brackets the template itself emits from the ones the input contributed:
// html/template escapes an input bracket to an entity, so the two counts must
// agree exactly. A spelling-based check ("does the output contain <script")
// only catches the payloads someone thought of.
func FuzzDashboardRender(f *testing.F) {
	f.Add("v1.2.0", "example-cluster", "connection refused", "Deployment", "example-ns", "web", "ImagePullBackOff", "the image tag does not exist", int64(1800), int64(600), 9900.0, int64(6), 0.999, 1.5, 0.8)
	f.Add("", "", "", "", "", "", "", "", int64(0), int64(0), 0.0, int64(0), 0.0, 0.0, 0.0)
	f.Add("<script>alert(1)</script>", "<script>", "\"><img src=x onerror=alert(1)>", "&", "'", "</style>", "</pre>", "<!--", int64(-1), int64(-1), -1.0, int64(-1), 1.0, math.Inf(1), math.NaN())
	f.Add("a\x00b", "\x1b[2J", "bad\xffbyte", "before‮after", "tiếng Việt", "é́́", "  ", "\n\n", int64(1)<<62, int64(1)<<62, math.MaxFloat64, int64(1), 1e308, math.Inf(-1), 1e308)
	f.Add("{{ . }}", "{{ template \"x\" }}", "{{/*", "*/}}", "%s", "%!v(PANIC=", "\\", "\"", int64(86400), int64(86400), 1e15, int64(1), 0.5, 0.0, 0.6)

	f.Fuzz(func(t *testing.T,
		version, cluster, clusterErr, kind, namespace, name, issue, text string,
		age, resolution int64,
		sum float64, count int64,
		target, burn, coverage float64,
	) {
		in := fuzzInput(version, cluster, clusterErr, kind, namespace, name, issue, text,
			age, resolution, sum, count, target, burn, coverage)

		var buf bytes.Buffer
		if err := Render(&buf, in); err != nil {
			t.Fatalf("Render returned an error: %v", err)
		}
		got := buf.String()

		// Determinism: a second render of the same value must produce the same
		// bytes. Map iteration is the classic way this fails.
		var again bytes.Buffer
		if err := Render(&again, in); err != nil {
			t.Fatalf("second Render returned an error: %v", err)
		}
		if again.String() != got {
			t.Fatal("Render is not deterministic for this input")
		}

		// No input bracket survives as a bracket.
		clean := fuzzInput(
			stripBrackets(version), stripBrackets(cluster), stripBrackets(clusterErr),
			stripBrackets(kind), stripBrackets(namespace), stripBrackets(name),
			stripBrackets(issue), stripBrackets(text),
			age, resolution, sum, count, target, burn, coverage)
		var base bytes.Buffer
		if err := Render(&base, clean); err != nil {
			t.Fatalf("Render of the bracket-free input returned an error: %v", err)
		}
		for _, b := range []string{"<", ">"} {
			if a, c := strings.Count(got, b), strings.Count(base.String(), b); a != c {
				t.Fatalf("%d %q in the page, want %d — an input bracket reached the output unescaped", a, b, c)
			}
		}

		// Belt and braces on the two spellings that matter most.
		lower := strings.ToLower(got)
		if strings.Contains(lower, "<script") {
			t.Fatal("a <script tag reached the page")
		}
		if strings.Contains(lower, "javascript:") {
			t.Fatal("a javascript: URL reached the page")
		}

		// No arithmetic artifact reaches a reader.
		for _, bad := range []string{"NaN", "+Inf", "-Inf"} {
			if strings.Contains(got, bad) {
				t.Fatalf("%s reached the page", bad)
			}
		}
	})
}

// fuzzInput threads the fuzzed values through every field the page renders.
func fuzzInput(version, cluster, clusterErr, kind, namespace, name, issue, text string,
	age, resolution int64, sum float64, count int64, target, burn, coverage float64) Input {
	return Input{
		Version:  version,
		Now:      fixedNow,
		Clusters: []Cluster{{Name: cluster, Up: false, LastScan: "2026-08-02T09:29:00Z", Error: clusterErr}},
		Active: []Incident{{
			Cluster: cluster, Kind: kind, Namespace: namespace, Name: name, Issue: issue,
			FiringSince: "2026-08-02T09:00:00Z", Firings: 2, Flapping: true, AgeSeconds: age,
		}},
		Resolved: []Incident{{
			Cluster: cluster, Kind: kind, Namespace: namespace, Name: name, Issue: issue,
			FiringSince: "2026-08-02T08:00:00Z", Firings: 1,
			ResolvedAt: "2026-08-02T08:30:00Z", ResolutionSeconds: resolution,
		}},
		Stats: Stats{ResolutionSecondsSum: sum, ResolutionSecondsCount: count},
		SLO: []SLO{{
			Cluster: cluster, Target: target,
			Windows: []SLOWindow{{Name: issue, Availability: coverage, BurnRate: burn, Coverage: coverage}},
		}},
		ExplainEnabled: true,
		Explanations: []Explanation{{
			Cluster: cluster, Kind: kind, Namespace: namespace, Name: name,
			Issues: []string{issue}, ExplainedAt: "2026-08-02T09:05:00Z", Model: version, Text: text,
		}},
	}
}

// stripBrackets removes the two characters html/template must never let
// through, so the comparison render carries only the template's own brackets.
func stripBrackets(s string) string {
	return strings.NewReplacer("<", "", ">", "").Replace(s)
}
```

- [ ] **Step 2: Run the seed corpus to verify it fails or passes honestly**

```bash
go test ./internal/dashboard -run FuzzDashboardRender -v
```

A plain `go test` replays the `f.Add` seeds. Expected on a correct Task 2/3: PASS. If it FAILs, the seed found a real defect — fix the renderer, not the target. The most likely finds are a non-finite number reaching a tile and an out-of-range float-to-int conversion in `meanResolution`; both are guarded, and the seeds exist to prove the guards work.

To see the target genuinely catch something, temporarily change `percent` to drop its `finite` check, re-run, and expect FAIL naming `NaN`. Restore it.

- [ ] **Step 3: Run a short campaign**

```bash
go test ./internal/dashboard -run FuzzDashboardRender -fuzz FuzzDashboardRender -fuzztime 60s
```

Expected: no new failing input. If the fuzzer writes a crasher into `testdata/fuzz/`, **do not commit the crasher and move on** — fix the renderer so the crasher passes, then commit both the fix and the crasher file, which becomes a permanent seed.

- [ ] **Step 4: Enrol the target in the nightly campaign**

A target the nightly workflow does not name never runs a campaign — a plain
`go test` only replays its seeds, which is a regression check, not a search.
`.github/workflows/fuzz.yml` enumerates `(package, target)` pairs explicitly,
because `go test -fuzz` accepts exactly one of each per invocation. Add the
thirteenth pair to the end of the matrix list, matching the existing
indentation:

```yaml
          - package: ./internal/dashboard
            target: FuzzDashboardRender
```

- [ ] **Step 5: Run the full gate**

```bash
go build ./... && go test ./... -p 2
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dashboard/fuzz_test.go .github/workflows/fuzz.yml
git commit -s -m "dashboard: fuzz the renderer

Thirteenth target in the suite Theme H slice 3 opened, same risk class: nothing
a cluster can put in a field the API server does not validate may panic a
render or reach a browser as markup.

The load-bearing assertion is a bracket count, not a payload spelling.
Rendering the same input twice — once as given, once with every angle
bracket stripped — isolates the brackets the template emits from the ones
the input contributed, and html/template escaping an input bracket to an
entity is exactly what makes the two counts agree. A contains-check only
catches the payloads someone thought of."
```

---

## Task 6: `internal/watch` — the input builder and the conditional handler

**Files:**

- Modify: `internal/watch/metrics.go`
- Modify: `internal/watch/watch.go:27-56` (the `Config` struct) and `internal/watch/watch.go:115` (where `newMetrics` is called)
- Test: `internal/watch/metrics_test.go`

**Interfaces:**

- Consumes: `dashboard.Input`, `dashboard.Cluster`, `dashboard.Incident`, `dashboard.Stats`, `dashboard.SLO`, `dashboard.SLOWindow`, `dashboard.Explanation`, `dashboard.Render` from Tasks 2 and 3.
- Produces, for Task 7: `watch.Config.Dashboard bool` and `watch.Config.Version string`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/watch/metrics_test.go`:

```go
// TestDashboardDisabledReturns404 asserts that a daemon without --dashboard
// does not register the path at all. The mux's own not-found handling answers,
// so there is no switched-off page to serve and no handler to get wrong.
func TestDashboardDisabledReturns404(t *testing.T) {
	srv := httptest.NewServer(newMetrics([]string{"local"}).handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatalf("GET /dashboard: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestDashboardEnabledServesHTML covers the enabled path end to end: the
// status, the content type, and that the page names the tracked incident.
func TestDashboardEnabledServesHTML(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.dashboard = true
	m.version = "v1.2.0"
	c := m.clusters["local"]
	c.up = true
	c.lastScanUnix = time.Date(2026, 8, 2, 9, 29, 0, 0, time.UTC).Unix()
	c.issues = issueSnapshot{
		At: time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC),
		Active: []watchstate.Record{{
			Key:         watchstate.Key{Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff"},
			FirstSeen:   time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
			FiringSince: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
			LastSeen:    time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC),
			Firings:     1,
		}},
	}

	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatalf("GET /dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}
	page := string(body)
	for _, want := range []string{"v1.2.0", "example-ns/web", "ImagePullBackOff"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
}

// TestDashboardInputCopies asserts the builder copies rather than aliases.
// The renderer runs after the read lock is released, so an Input holding a
// reference into a snapshot a worker can swap would be a data race that
// happens to produce plausible output most of the time.
func TestDashboardInputCopies(t *testing.T) {
	m := newMetrics([]string{"local"})
	c := m.clusters["local"]
	c.up = true
	c.lastScanUnix = time.Date(2026, 8, 2, 9, 29, 0, 0, time.UTC).Unix()
	c.issues = issueSnapshot{
		At: time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC),
		Active: []watchstate.Record{{
			Key:         watchstate.Key{Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff"},
			FiringSince: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
			Firings:     1,
		}},
	}
	m.explain = explainSnapshot{Enabled: true, Latest: []oncall.Explanation{{
		Cluster: "local", Kind: "Deployment", Namespace: "example-ns", Name: "web",
		Issues: []string{"ImagePullBackOff"}, ExplainedAt: time.Date(2026, 8, 2, 9, 5, 0, 0, time.UTC),
		Model: "example-model", Text: "example text",
	}}}

	in := m.dashboardInput("v1.2.0", time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC))

	// Mutate everything the snapshot owns, including the one slice field an
	// Input reaches into.
	c.issues.Active[0].Key.Name = "mutated"
	c.lastError = "mutated"
	m.explain.Latest[0].Issues[0] = "mutated"

	if in.Active[0].Name != "web" {
		t.Errorf("Active[0].Name = %q — the Input aliases the snapshot's records", in.Active[0].Name)
	}
	if in.Explanations[0].Issues[0] != "ImagePullBackOff" {
		t.Errorf("Explanations[0].Issues[0] = %q — the Input aliases the explanation's issue slice", in.Explanations[0].Issues[0])
	}
}

// TestDashboardRenderFailureIs500 covers the path buffering exists for. A
// template failure mid-execution would otherwise land after the 200 header and
// produce a truncated page; buffering turns it into a clean 500 with no body
// from the renderer.
func TestDashboardRenderFailureIs500(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.dashboard = true

	orig := renderDashboard
	renderDashboard = func(w io.Writer, in dashboard.Input) error {
		io.WriteString(w, "<html>partial")
		return errors.New("synthetic template failure")
	}
	defer func() { renderDashboard = orig }()

	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatalf("GET /dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if strings.Contains(string(body), "partial") {
		t.Error("a partially rendered page reached the client")
	}
}

// TestDashboardConcurrentReadsAndSwaps is the race test. Run under -race it
// asserts that no dashboard request observes a snapshot a worker is writing.
func TestDashboardConcurrentReadsAndSwaps(t *testing.T) {
	m := newMetrics([]string{"local"})
	m.dashboard = true
	m.version = "v1.2.0"

	srv := httptest.NewServer(m.handler())
	defer srv.Close()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			m.mu.Lock()
			c := m.clusters["local"]
			c.up = i%2 == 0
			c.lastScanUnix = int64(1754126000 + i)
			c.lastError = "connection refused"
			c.issues = issueSnapshot{
				At: time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC),
				Active: []watchstate.Record{{
					Key:         watchstate.Key{Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff"},
					FiringSince: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
					Firings:     i,
				}},
			}
			m.mu.Unlock()
		}
	}()

	for i := 0; i < 50; i++ {
		resp, err := http.Get(srv.URL + "/dashboard")
		if err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("GET /dashboard: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			close(done)
			wg.Wait()
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}
	close(done)
	wg.Wait()
}
```

Add whatever of `errors`, `io`, `net/http`, `net/http/httptest`, `strings`, `sync`, `time`, `github.com/imantaba/kubeagent/internal/dashboard`, `github.com/imantaba/kubeagent/internal/oncall` and `github.com/imantaba/kubeagent/internal/watchstate` the file does not already import.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/watch -run TestDashboard -v
```

Expected: FAIL to build — `m.dashboard undefined`, `m.version undefined`, `m.dashboardInput undefined`, `renderDashboard undefined`.

- [ ] **Step 3: Implement the metrics fields, the builder and the handler**

In `internal/watch/metrics.go`, extend the `metrics` struct (line 120) with two fields:

```go
type metrics struct {
	mu       sync.RWMutex
	names    []string // configured target names, sorted; fixed for the process lifetime
	clusters map[string]*clusterSnapshot
	pending  map[string]bool // clusters yet to finish a first reconcile attempt
	alerts   alert.Stats     // process-wide: one sink
	explain  explainSnapshot // process-wide: one budget
	// dashboard and version are set once, in Run, before the HTTP server starts
	// and before any worker exists. They are never written again, so they need
	// no lock — unlike everything above them, which workers update live.
	dashboard bool
	version   string
}
```

Add the render indirection beside the other package-level vars, above `handler`:

```go
// renderDashboard is dashboard.Render, indirected so a test can make it fail.
// A parsed template only errors on a write failure, and the handler writes into
// a bytes.Buffer, which cannot fail — so the 500 path is otherwise unreachable
// and would go untested. This is the same indirection internal/cli uses for
// watch.Run.
var renderDashboard = dashboard.Render
```

Extend `handler()` — insert before the `/healthz` registration:

```go
	// Registered only when --dashboard is set. A disabled daemon 404s the path
	// through the mux's own not-found handling; there is no switched-off page
	// to serve.
	if m.dashboard {
		mux.HandleFunc("/dashboard", func(w http.ResponseWriter, _ *http.Request) {
			// Render into a buffer, not into the ResponseWriter: a failure
			// mid-execution would otherwise land after the 200 header and
			// produce a truncated page. One page in memory is a negligible
			// cost for turning that into a clean 500.
			var buf bytes.Buffer
			if err := renderDashboard(&buf, m.dashboardInput(m.version, time.Now())); err != nil {
				http.Error(w, "rendering dashboard", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(buf.Bytes())
		})
	}
```

Update `handler`'s doc comment:

```go
// handler serves /metrics, /issues, /explanations, /healthz, /readyz, and —
// only when --dashboard is set — /dashboard.
```

Add `dashboardInput` after `explanationsJSON` at the end of the file:

```go
// dashboardInput copies the daemon's state into the renderer's input, under the
// same read lock issuesJSON takes.
//
// Every slice is freshly allocated. The renderer runs after this function
// returns and therefore after the lock is released, so an Input holding a
// reference into a snapshot a worker can swap would be a data race that happens
// to produce plausible output most of the time.
//
// It performs no cluster call. It also makes no model call: m.explain.Latest is
// what the incident pipeline already computed, and reading it is free. Those
// are two separate promises.
func (m *metrics) dashboardInput(version string, now time.Time) dashboard.Input {
	m.mu.RLock()
	defer m.mu.RUnlock()
	in := dashboard.Input{
		Version:        version,
		Now:            now,
		ExplainEnabled: m.explain.Enabled,
		Clusters:       make([]dashboard.Cluster, 0, len(m.names)),
	}
	for _, n := range m.names {
		c := m.clusters[n]
		cv := dashboard.Cluster{Name: n, Up: c.up, Error: c.lastError}
		if c.lastScanUnix != 0 {
			cv.LastScan = rfc3339(time.Unix(c.lastScanUnix, 0))
		}
		in.Clusters = append(in.Clusters, cv)

		for _, r := range c.issues.Active {
			in.Active = append(in.Active, dashboard.Incident{
				Cluster: n, Kind: r.Key.Kind, Namespace: r.Key.Namespace,
				Name: r.Key.Name, Issue: r.Key.Issue,
				FiringSince: rfc3339(r.FiringSince),
				Firings:     r.Firings,
				Flapping:    r.Flapping,
				AgeSeconds:  ageSeconds(r.FiringSince, c.issues.At),
			})
		}
		for _, r := range c.issues.Resolved {
			in.Resolved = append(in.Resolved, dashboard.Incident{
				Cluster: n, Kind: r.Key.Kind, Namespace: r.Key.Namespace,
				Name: r.Key.Name, Issue: r.Key.Issue,
				FiringSince:       rfc3339(r.FiringSince),
				Firings:           r.Firings,
				Flapping:          r.Flapping,
				ResolvedAt:        rfc3339(r.ResolvedAt),
				ResolutionSeconds: ageSeconds(r.FiringSince, r.ResolvedAt),
			})
		}

		in.Stats.NewTotal += c.issues.Stats.NewTotal
		in.Stats.ResolvedTotal += c.issues.Stats.ResolvedTotal
		in.Stats.FlapTotal += c.issues.Stats.FlapTotal
		in.Stats.DroppedTotal += c.issues.Stats.DroppedTotal
		in.Stats.ResolutionSecondsSum += c.issues.Stats.ResolutionSecondsSum
		in.Stats.ResolutionSecondsCount += c.issues.Stats.ResolutionSecondsCount

		if c.slo.Enabled {
			// The window labels carry their spans for a reader; the bare
			// "fast"/"slow" halves match the `window` label on the
			// kubeagent_slo_* series, so the page and a Grafana panel name the
			// same thing.
			in.SLO = append(in.SLO, dashboard.SLO{
				Cluster: n,
				Target:  c.slo.Target,
				Windows: []dashboard.SLOWindow{
					{Name: "fast (1h)", Availability: c.slo.Fast.Availability, BurnRate: c.slo.Fast.BurnRate, Coverage: c.slo.Fast.Coverage},
					{Name: "slow (6h)", Availability: c.slo.Slow.Availability, BurnRate: c.slo.Slow.BurnRate, Coverage: c.slo.Slow.Coverage},
				},
			})
		}
	}
	for _, x := range m.explain.Latest {
		in.Explanations = append(in.Explanations, dashboard.Explanation{
			Cluster:     x.Cluster,
			Kind:        x.Kind,
			Namespace:   x.Namespace,
			Name:        x.Name,
			Issues:      append([]string(nil), x.Issues...), // copied: the snapshot still owns the original
			ExplainedAt: x.ExplainedAt.UTC().Format(time.RFC3339),
			Model:       x.Model,
			Text:        x.Text,
		})
	}
	return in
}
```

Add `"bytes"` and `"github.com/imantaba/kubeagent/internal/dashboard"` to `metrics.go`'s import block.

In `internal/watch/watch.go`, add two fields to `Config` after `ExplainBudget`:

```go
	Dashboard               bool          // serve the read-only HTML dashboard at /dashboard
	Version                 string        // kubeagent version, stamped into the dashboard header
```

And at line 115, immediately after `m := newMetrics(targetNames(targets))`:

```go
	// Set before the HTTP server starts and before any worker exists, and never
	// written again — which is why these two need no lock.
	m.dashboard = cfg.Dashboard
	m.version = cfg.Version
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/watch -run TestDashboard -v
go test -race ./internal/dashboard ./internal/watch
```

Expected: PASS both.

- [ ] **Step 5: Verify the layering invariant still holds**

```bash
go list -deps ./internal/watch | grep -E 'kubeagent/internal/(report|remediate)$' && echo "INVARIANT BROKEN" || echo "ok: watch reaches neither report nor remediate"
go list -deps ./internal/dashboard | grep -v '^github.com/imantaba/kubeagent/internal/dashboard$' | grep kubeagent && echo "INVARIANT BROKEN" || echo "ok: dashboard imports nothing from kubeagent"
```

Expected: both print `ok:`.

- [ ] **Step 6: Run the full gate**

```bash
go build ./... && go test ./... -p 2
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/watch/metrics.go internal/watch/watch.go internal/watch/metrics_test.go
git commit -s -m "watch: serve the dashboard at /dashboard when enabled

dashboardInput copies the snapshot under the same read lock issuesJSON
takes, and the handler renders after that lock is released — so the
renderer only ever sees a value no other goroutine can reach. Every slice
is freshly allocated, including the one slice an explanation owns, because
an Input that aliased a snapshot would be a race that produces plausible
output most of the time.

Rendering targets a buffer rather than the ResponseWriter: a failure
mid-execution would otherwise land after the 200 header and truncate the
page. A parsed template only fails on a write error, and a bytes.Buffer
cannot fail, so renderDashboard is indirected to let a test reach that path.

With --dashboard off the route is never registered, so the mux's own
not-found handling answers and there is no switched-off page to serve."
```

---

## Task 7: `internal/cli` — the `--dashboard` flag

**Files:**

- Modify: `internal/cli/watch.go:70-88` (`watchOptions`), `:98-118` (`bindWatchFlags`), `:210-239` (the `watch.Config` literal)
- Modify: `internal/cli/root.go:91` (the usage line)
- Test: `internal/cli/surface_test.go:107-160` (`TestCommandSurfaceWatch`)
- Test: `internal/cli/cli_test.go:2492` (`TestParseWatchFlagsCarriesEveryValue`)

**Interfaces:**

- Consumes: `watch.Config.Dashboard` and `watch.Config.Version` from Task 6.
- Produces: `--dashboard` on `kubeagent watch`, defaulting from `KUBEAGENT_DASHBOARD`.

- [ ] **Step 1: Write the failing tests**

In `internal/cli/surface_test.go`, add `"KUBEAGENT_DASHBOARD"` to the env-key list at lines 112-117 and update the comment above it — it currently reads "Thirteen of watch's flags default from the environment, reading the twelve keys below"; it becomes fourteen flags and thirteen keys:

```go
	// Fourteen of watch's flags default from the environment, reading the
	// thirteen keys below — --namespace and -n share KUBEAGENT_NAMESPACE. This
	// is the same set TestParseWatchFlagsCarriesEveryValue clears: a
	// developer's shell must not decide whether an explicit flag value lands.
	for _, k := range []string{
		"KUBEAGENT_CLUSTER_NAME", "KUBEAGENT_INCLUDE_LOCAL", "KUBEAGENT_METRICS_ADDR",
		"KUBEAGENT_HEARTBEAT", "KUBEAGENT_DEBOUNCE", "KUBEAGENT_ALERT_FORMAT",
		"KUBEAGENT_ALERT_REPEAT", "KUBEAGENT_SLO_TARGET", "KUBEAGENT_NAMESPACE",
		"KUBEAGENT_EXPLAIN", "KUBEAGENT_EXPLAIN_COOLDOWN", "KUBEAGENT_EXPLAIN_BUDGET",
		"KUBEAGENT_DASHBOARD",
	} {
```

Add a row to the `cases` table in `TestCommandSurfaceWatch`, beside the `explain` row:

```go
		{"dashboard", []string{"--dashboard"}, func(o watchOptions) bool { return o.dashboard }},
```

Add a new test to `internal/cli/cli_test.go`:

```go
// TestWatchDashboardDefaultsFromEnvironment pins KUBEAGENT_DASHBOARD to the
// same envBool contract every other watch toggle uses.
func TestWatchDashboardDefaultsFromEnvironment(t *testing.T) {
	t.Setenv("KUBEAGENT_DASHBOARD", "true")
	o, err := parseWatchFlags(nil)
	if err != nil {
		t.Fatalf("parseWatchFlags: %v", err)
	}
	if !o.dashboard {
		t.Error("dashboard = false with KUBEAGENT_DASHBOARD=true")
	}

	t.Setenv("KUBEAGENT_DASHBOARD", "")
	o, err = parseWatchFlags(nil)
	if err != nil {
		t.Fatalf("parseWatchFlags: %v", err)
	}
	if o.dashboard {
		t.Error("dashboard = true with KUBEAGENT_DASHBOARD unset")
	}
}

// TestNormalizeAcceptsSingleDashDashboard keeps the single-dash long-flag shim
// covering the new flag. Command lines written against v0.72 and earlier use
// this spelling, and Normalize is why they keep working.
func TestNormalizeAcceptsSingleDashDashboard(t *testing.T) {
	t.Setenv("KUBEAGENT_DASHBOARD", "")
	o, err := parseWatchFlags([]string{"-dashboard"})
	if err != nil {
		t.Fatalf("parseWatchFlags: %v", err)
	}
	if !o.dashboard {
		t.Error("-dashboard did not set the flag")
	}
}
```

In `TestParseWatchFlagsCarriesEveryValue` (`internal/cli/cli_test.go:2492`), add `"KUBEAGENT_DASHBOARD"` to the env-key list, update the "Thirteen … twelve keys" comment to "Fourteen … thirteen keys", add `"--dashboard",` to the `parseWatchFlags` argument slice, and add the assertion beside the others:

```go
	if !o.dashboard {
		t.Error("dashboard = false, want true")
	}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cli -run 'TestCommandSurfaceWatch|TestParseWatchFlagsCarriesEveryValue|TestWatchDashboard|TestNormalizeAcceptsSingleDashDashboard' -v
```

Expected: FAIL to build — `o.dashboard undefined`.

- [ ] **Step 3: Implement the flag**

In `internal/cli/watch.go`, add the field to `watchOptions` after `explainBudget`:

```go
	model           string
	dashboard       bool
	namespace       string
```

Add the declaration in `bindWatchFlags`, after the `--model` line:

```go
	f.BoolVar(&o.dashboard, "dashboard", envBool("KUBEAGENT_DASHBOARD", false), "serve a read-only HTML dashboard at /dashboard on --metrics-addr (unauthenticated, like /metrics and /issues on the same port)")
```

Add both fields to the `watch.Config` literal in `runWatchOpts`, after `ExplainBudget`:

```go
		ExplainBudget:           o.explainBudget,
		Dashboard:               o.dashboard,
		Version:                 version,
```

In `internal/cli/root.go:91`, add `[--dashboard]` to the `watch` clause of the usage string, immediately after `[--slo-target pct]`:

```text
[--slo-target pct] [--dashboard] [--explain [--explain-cooldown dur] [--explain-budget n] [--model name]]
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cli -v
```

Expected: PASS.

- [ ] **Step 5: Verify by hand**

```bash
go build -o /tmp/kubeagent-dash . && /tmp/kubeagent-dash watch --help | grep -A1 dashboard
```

Expected: the flag and its usage text appear. Then check the shell completion still generates:

```bash
/tmp/kubeagent-dash completion bash | head -1
rm -f /tmp/kubeagent-dash
```

Expected: a script is emitted. Do NOT grep it for `dashboard`: Cobra's
bash-completion-v2 script names no flag at all — it calls the binary's hidden
`__complete` at completion time, so `grep -c dashboard` returns 0 for this flag
exactly as it does for the long-standing `--explain`. The flag reaching
completion is covered by `TestCommandSurfaceWatch`, which asserts the command
registers it.

- [ ] **Step 6: Run the full gate**

```bash
go build ./... && go test ./... -p 2
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/watch.go internal/cli/root.go internal/cli/surface_test.go internal/cli/cli_test.go
git commit -s -m "cli: add watch --dashboard

Off by default, defaulting from KUBEAGENT_DASHBOARD through the same
envBool the other watch toggles use. The flag is per-command, never
persistent, and the single-dash spelling Normalize rewrites is covered by
its own test so a command line written against v0.72 keeps working.

The version reaches the daemon for the first time here: the dashboard
header names it, and main stamps it into cli.version at release."
```

---

## Task 8: Helm chart

**Files:**

- Modify: `deploy/helm/kubeagent/values.yaml`
- Modify: `deploy/helm/kubeagent/templates/deployment.yaml`
- Modify: `deploy/helm/kubeagent/Chart.yaml`

**Interfaces:**

- Consumes: `--dashboard` from Task 7.
- Produces: `dashboard.enabled` chart value.

- [ ] **Step 1: Write the failing render check**

There is no Go test for the chart; the render itself is the check. Write the assertion as a command that must fail now and pass after Step 2:

```bash
export PATH=$PATH:$HOME/.local/bin
helm template x deploy/helm/kubeagent --set dashboard.enabled=true | grep -- '--dashboard'
```

Expected now: no output, exit 1 — the value does not exist and nothing renders.

- [ ] **Step 2: Add the value**

In `deploy/helm/kubeagent/values.yaml`, insert after the `slo:` block (which ends at the `target: 99.9` line) and before the `# On-incident explanations` comment:

```yaml
# A read-only HTML dashboard at /dashboard on the metrics port: the URL you
# hand someone who asks what is broken right now. It renders only state the
# daemon already tracks, so it performs no extra cluster read and needs no
# extra RBAC, and it makes no model call.
#
# It is UNAUTHENTICATED, exactly like /metrics and /issues on the same port,
# and kubeagent terminates no TLS. The Service is ClusterIP by default; keep it
# cluster-internal, or put an authenticating proxy in front of it.
dashboard:
  enabled: false
```

- [ ] **Step 3: Render the argument**

In `deploy/helm/kubeagent/templates/deployment.yaml`, insert between the `slo` block's `{{- end }}` and the `{{- if .Values.explain.enabled }}` line:

```yaml
            {{- if .Values.dashboard.enabled }}
            - "--dashboard"
            {{- end }}
```

- [ ] **Step 4: Bump the chart version**

In `deploy/helm/kubeagent/Chart.yaml`, change `version: 0.24.1` to:

```yaml
version: 0.25.0
```

A MINOR bump because templates **and** values move. `appVersion` stays `"v1.1.0"` — the release skill's `bump-version.sh` sets it at release time, and it will patch-bump `version` to `0.25.1`, which is correct and should be left alone.

- [ ] **Step 5: Verify the render**

```bash
export PATH=$PATH:$HOME/.local/bin
helm lint deploy/helm/kubeagent

# enabled: the argument renders
helm template x deploy/helm/kubeagent --set dashboard.enabled=true | grep -- '--dashboard'

# disabled: it does not
helm template x deploy/helm/kubeagent | grep -- '--dashboard' && echo "UNEXPECTED" || echo "ok: absent by default"

# no new Service port
helm template x deploy/helm/kubeagent --set dashboard.enabled=true | grep -c 'port: 8080'
```

Expected: lint passes with no error; the argument appears exactly once when enabled and not at all by default; the Service still declares one port.

Then confirm the default render is unchanged from before this task, which is what "off by default changes nothing" means:

```bash
helm template x deploy/helm/kubeagent > /tmp/dash-after.yaml
git stash && helm template x deploy/helm/kubeagent > /tmp/dash-before.yaml && git stash pop
diff /tmp/dash-before.yaml /tmp/dash-after.yaml && echo "ok: default render byte-identical"
rm -f /tmp/dash-before.yaml /tmp/dash-after.yaml
```

Expected: `ok: default render byte-identical`.

- [ ] **Step 6: Run the full gate**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add deploy/helm/kubeagent/values.yaml deploy/helm/kubeagent/templates/deployment.yaml deploy/helm/kubeagent/Chart.yaml
git commit -s -m "chart: add dashboard.enabled

Off by default, so the default render is byte-identical to the previous
chart. No new Service port — the dashboard shares the metrics port — and no
new RBAC, because it reads nothing the daemon was not already reading.

Chart version is a MINOR bump: both templates and values moved.

The values comment states the exposure posture plainly. The page is
unauthenticated, exactly like /metrics and /issues on the same port, and
kubeagent terminates no TLS."
```

---

## Task 9: Chaos scenario 12

**Files:**

- Modify: `chaos/run.sh` (`scenario_12_watch`, starting at line 638)
- Modify: `CLAUDE.md`, `chaos/README.md`, `website/docs/compatibility.md`, `website/docs/roadmap.md` (124 → 128)

**Interfaces:**

- Consumes: `--dashboard` from Task 7.
- Produces: four assertions; suite total 124 → 128.

**Harness rules that bind this task:**

- No helper ever returns non-zero. The suite runs under `set -e`; a failing assertion must let the remaining scenarios run and surface at the end, in the exit code.
- Assertions are written at **kubeagent's** contract level, never Kubernetes' wording.
- `$ASSERTLOG` is a file, not a variable, because `record()` is fed by a pipeline and a pipeline runs in a subshell.
- `scenario_01_etcd` stays LAST in `run_scenarios()` — this task does not reorder anything.

- [ ] **Step 1: Add `--dashboard` to the scenario's daemon and capture the page**

In `scenario_12_watch`, extend the local declaration line:

```bash
  local ns=chaos-watch port=18080 aport=18081 wlog wpid i firing after alerts apid
  local dash dash_code dash_type dash_webhook
```

Add `--dashboard` to the daemon invocation:

```bash
  KUBEAGENT_ALERT_WEBHOOK="http://127.0.0.1:$aport" \
  ./kubeagent watch --context "$CTX" -n "$ns" --metrics-addr "127.0.0.1:$port" \
    --heartbeat 10s --debounce 2s --alert-format json --alert-repeat 1h --dashboard >"$wlog" 2>&1 &
```

Immediately after the existing `firing="$(curl ...)"` line, capture the page while the incident is still firing:

```bash
  # The dashboard is a face on the same state /issues just served, captured at
  # the same moment so the two cannot disagree about what was firing.
  dash_code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/dashboard" 2>/dev/null || echo 000)"
  dash_type="$(curl -s -o /dev/null -w '%{content_type}' "http://127.0.0.1:$port/dashboard" 2>/dev/null || true)"
  dash="$(curl -s "http://127.0.0.1:$port/dashboard" 2>/dev/null || echo '<unreachable>')"
  # Count, never quote: assert.sh embeds a needle in its own PASS/FAIL line, so
  # an expect_absent here would write the endpoint into the report on every
  # passing run — the leak the assertion exists to rule out. The webhook
  # redaction check twenty lines above and scenario 20 both count for the same
  # reason.
  dash_webhook="$(printf '%s\n' "$dash" | grep -cF -- "127.0.0.1:$aport" || true)"
```

- [ ] **Step 2: Add the four assertions**

In the `{ ... } | record` block, add a reporting section before the `--- assertions ---` line:

```bash
    echo '--- dashboard served while the outage was firing ---'
    printf 'status: %s\ncontent type: %s\npage bytes: %s\n' \
      "$dash_code" "$dash_type" "$(printf '%s' "$dash" | wc -c | tr -d ' ')"
    printf 'page lines naming the webhook endpoint host: %s\n' "$dash_webhook"
    echo
```

and the four `expect_*` calls after the existing six:

```bash
    expect_eq       "dashboard returns 200 while an incident is firing" "$dash_code" 200
    expect_contains "dashboard content type is HTML"                    "$dash_type" "text/html"
    expect_contains "dashboard names the broken workload"               "$dash"      "$ns/web"
    # A webhook URL is a credential, and this scenario is the only place in the
    # suite where the daemon actually holds one. Asserting it never reaches the
    # page is a stronger test of the "URLs are credentials" rule than a generic
    # path grep would be, because the value is really there to leak. The
    # assertion carries a count rather than the endpoint itself, so a passing
    # run does not print what it just proved absent.
    expect_eq       "dashboard body carries no alert webhook URL"       "$dash_webhook" 0
```

Extend the `record` header's `expect:` sentence — append to the existing text:

```text
 The dashboard is served from the same snapshot at the same moment: 200, HTML, naming the broken workload, and carrying none of the webhook URL the daemon holds in memory.
```

- [ ] **Step 3: Check the script parses**

```bash
bash -n chaos/run.sh
```

Expected: no output, exit 0.

- [ ] **Step 4: See each new assertion fail**

An assertion never seen failing is an assertion that asserts nothing. Break each in turn against the already-running `kubeagent-chaos` cluster.

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o kubeagent .
```

For each of the four, make a temporary edit, run the scenario, confirm the FAIL line, then revert:

1. **200** — remove `--dashboard` from the daemon invocation. Expect `FAIL: dashboard returns 200 while an incident is firing (got '404', want '200')`.
2. **HTML** — change the assertion's needle from `text/html` to `application/json`. Expect a FAIL naming the missing needle.
3. **workload name** — change the needle from `$ns/web` to `$ns/does-not-exist`. Expect a FAIL.
4. **webhook URL** — point the counted needle at a string the page really does carry, e.g. `grep -cF -- "$ns/web"`. Expect `FAIL: dashboard body carries no alert webhook URL (got '1', want '0')`, proving the counter counts rather than always returning zero.

Each run:

```bash
./chaos/run.sh --only 12 --out /tmp/chaos-12.md
grep -E '^(PASS|FAIL): dashboard' /tmp/chaos-12.md
```

After all four have been seen failing, revert every temporary edit and run once more:

```bash
./chaos/run.sh --only 12 --out /tmp/chaos-12.md
grep -E '^(PASS|FAIL): dashboard' /tmp/chaos-12.md
```

Expected: four `PASS:` lines and no `FAIL:`.

- [ ] **Step 5: Update the assertion count in all four documents**

The harness prints the real total. Read it from the run above:

```bash
grep -A3 '## Assertion summary' /tmp/chaos-12.md
```

That run covers one scenario, so use the full-suite arithmetic: **124 + 4 = 128**. If the final full run in the slice gate prints something else, the printed number wins and all four documents follow it.

Change `124` to `128` in:

- `CLAUDE.md` — the Theme H slice 8/9 paragraph: "124 machine-checked assertions"
- `chaos/README.md`
- `website/docs/compatibility.md:114` — "with 124 machine-checked assertions per cell"
- `website/docs/roadmap.md`

```bash
grep -rn '124 machine-checked\|124 assertions\|124\b' CLAUDE.md chaos/README.md website/docs/compatibility.md website/docs/roadmap.md
```

Check every hit by hand — only the assertion-count occurrences change.

- [ ] **Step 6: Commit**

```bash
git add chaos/run.sh CLAUDE.md chaos/README.md website/docs/compatibility.md website/docs/roadmap.md
git commit -s -m "chaos: assert the dashboard in scenario 12 (124 -> 128)

Scenario 12 already stands up the daemon, breaks a workload and curls
/issues while the incident fires, so the dashboard costs four expect_*
calls here instead of several minutes of duplicate daemon setup in a
twenty-third scenario. The page is captured at the same moment as /issues,
so the two cannot disagree about what was firing.

The webhook-URL assertion is the interesting one: this scenario is the only
place in the suite where the daemon actually holds a credential-class URL,
so asserting its absence from the page tests the rule against a value that
is really there to leak.

The disabled path is deliberately not asserted. One daemon runs here with
one argument set, so proving the 404 would need a second daemon or a
restart, for a behaviour internal/watch already covers directly."
```

---

## Task 10: Documentation

**Files:**

- Create: `website/docs/features/dashboard.md`
- Modify: `website/mkdocs.yml:77` (nav), `website/docs/features/watch-mode.md`, `website/docs/compatibility.md`, `website/docs/roadmap.md:456` and `:544`, `deploy/README.md`, `CHANGELOG.md`, `CLAUDE.md`

**Interfaces:**

- Consumes: everything.
- Produces: nothing.

- [ ] **Step 1: Write the feature page**

Create `website/docs/features/dashboard.md`:

````markdown
# In-cluster dashboard

`kubeagent watch --dashboard` serves a read-only HTML page at `/dashboard` on
the daemon's metrics port. It is the URL you hand someone who asks what is
broken right now.

```bash
kubeagent watch --dashboard
# then, from inside the cluster or through a port-forward:
#   http://localhost:8080/dashboard
```

## What it shows

Everything on the page comes from state the daemon already tracks for
`/metrics` and `/issues`. The dashboard performs **no cluster read of its own**,
which is why enabling it changes no RBAC.

| Section | What it answers |
|---|---|
| Clusters | Is each watched cluster reachable, and when did it last evaluate? |
| Active incidents | What is broken, for how long, and is it flapping? |
| Resolved incidents | What recovered, when, and how long did it take? |
| Totals | New, resolved, flapping and dropped counts, and mean time to resolution. |
| SLO | Availability, burn rate, error budget left and coverage per window — only with `--slo-target`. |
| Explanations | The latest explanation per object — only with `--explain`. |

The cluster strip is always present, including when both incident lists are
empty. An empty list from an unreachable cluster and an empty list from a
healthy one are not the same thing, and this band is what tells them apart. A
cluster that has not completed its first evaluation says so rather than reading
as down.

The page reloads itself every 30 seconds through a `<meta http-equiv="refresh">`.
The interval is fixed and has no flag: informers detect in roughly two seconds
and the heartbeat is sixty, so thirty already sits between them.

## What it does not do

- **It does not browse the cluster.** No namespace list, no workload
  drill-down, no pod detail, no events. That is [`kubeagent tui`](tui.md), which
  runs on a laptop against a kubeconfig and performs its own reads.
- **It changes nothing.** No buttons, no forms, no actions. `/dashboard` is a
  `GET` that renders a snapshot.
- **It never triggers an explanation.** The page renders explanations the
  incident pipeline already computed. A dashboard request makes no model call.
- **It shows no blind spots.** What kubeagent could not read is a `scan`
  concept the daemon does not carry; use `kubeagent scan` or
  [`kubeagent gate`](ci-gate.md) for that.

## Exposure and authentication

**kubeagent implements no authentication for the dashboard.** This is a
decision, not an omission.

The posture is identical to what `/metrics` and `/issues` already have on the
same port: unauthenticated, and `ClusterIP` by default. **kubeagent terminates
no TLS.**

The rationale: the daemon's entire security story is that it holds no credential
beyond its own read-only ServiceAccount. A password or token store inside
kubeagent would contradict that, in exchange for guarding a page whose data the
same port already serves unauthenticated.

Put authentication where it belongs — in front:

- an Ingress with basic-auth, or an `oauth2-proxy` sidecar, terminating TLS and
  authenticating before the request reaches the daemon;
- a `NetworkPolicy` keeping the metrics port reachable only from your monitoring
  namespace;
- or nothing at all, if the Service stays `ClusterIP` and you reach it with
  `kubectl port-forward`.

The page itself carries no cluster identity beyond the operator-chosen cluster
**names** that `/issues` and every metric series already carry. No API server
URL, no kubeconfig path, no kubeconfig context name, and no URL of any kind —
the meta refresh carries an interval, not a target.

## Enabling it

=== "CLI"

    ```bash
    kubeagent watch --dashboard
    ```

=== "Environment"

    ```bash
    KUBEAGENT_DASHBOARD=true kubeagent watch
    ```

=== "Helm"

    ```bash
    helm upgrade --install kubeagent deploy/helm/kubeagent \
      --namespace kubeagent --create-namespace \
      --set dashboard.enabled=true
    ```

It shares the existing metrics port, so there is no new Service port and no new
RBAC rule to grant.

## Stability

`--dashboard`, `KUBEAGENT_DASHBOARD`, `dashboard.enabled`, and the existence of
`/dashboard` returning HTML are **stable within 1.x**.

**The page's markup and layout are not.** It is an artifact for a human to look
at, and its structure will change. Anyone who wants a contract parses
[`/issues`](watch-mode.md#issues), which is versioned. See
[Compatibility](../compatibility.md).
````

- [ ] **Step 2: Add the nav entry**

In `website/mkdocs.yml`, add after the `Policy as code` line (line 77):

```yaml
      - In-cluster dashboard: features/dashboard.md
```

- [ ] **Step 3: Cross-reference from watch-mode**

In `website/docs/features/watch-mode.md`, add a short section immediately after the `### /explanations` section ends and before `## Watching several clusters`:

```markdown
## Dashboard (`--dashboard`)

`--dashboard` serves the same tracked state as an HTML page at `/dashboard` on
the metrics port — the URL you hand someone instead of a `curl | jq`. It
performs no extra cluster read, needs no extra RBAC, and makes no model call.
It is unauthenticated, exactly like `/metrics` and `/issues` on the same port.

See [In-cluster dashboard](dashboard.md).
```

- [ ] **Step 4: Extend the compatibility statement**

In `website/docs/compatibility.md`, extend the HTML-report bullet under
"Unstable surfaces" so it names both artifacts:

```markdown
- **The HTML report's and the dashboard's markup.** Both are artifacts for a
  human to look at — a shareable file in one case, a page in a browser in the
  other — and their structure will change. Parse `--output json` or `/issues`.
```

And add the dashboard's stable surfaces to the stable-surfaces list, beside the
existing flag and chart-value entries — one line each:

```markdown
- `watch --dashboard`, `KUBEAGENT_DASHBOARD`, the `dashboard.enabled` chart
  value, and `/dashboard` existing and returning HTML when enabled.
```

- [ ] **Step 5: Mark Theme G complete in the roadmap**

In `website/docs/roadmap.md:456`, the sentence currently ends "…optional
in-cluster dashboard remains ahead." Replace the clause so it reads that the
dashboard has shipped and Theme G is complete, naming the doc:

```markdown
  optional in-cluster dashboard has shipped (`kubeagent watch --dashboard`,
  documented in [features/dashboard.md](features/dashboard.md)), and **Theme G
  is complete**.
```

At line 544, change `optional in-cluster dashboard` in the milestone table cell
to `**in-cluster dashboard** (shipped, `watch --dashboard`)`.

- [ ] **Step 6: Document the chart value**

In `deploy/README.md`, add after the `slo.enabled` / `slo.target` `--set` block
(around line 210):

```markdown
Serve the read-only dashboard on the metrics port:

```bash
helm upgrade --install kubeagent deploy/helm/kubeagent \
  --namespace kubeagent --create-namespace \
  --set dashboard.enabled=true
```

It is unauthenticated, exactly like `/metrics` and `/issues` on the same port,
and kubeagent terminates no TLS. Keep the Service `ClusterIP`, or put an
authenticating proxy in front. It adds no Service port and no RBAC rule.
```

- [ ] **Step 7: Record the invariant in CLAUDE.md**

In the "Invariants (do not break)" section, after the `internal/policy`
paragraph, add:

```markdown
  `internal/dashboard` (the `watch --dashboard` renderer) is a seventh case and
  the strictest: it **imports nothing from kubeagent at all** — only `embed`,
  `html/template`, `io`, `time`, `sort`, `strings` and `fmt` — which puts it in
  the same class as `internal/jsonschema` and makes reaching
  `internal/remediate` or `internal/explain` impossible by construction rather
  than by rule. `internal/dashboard/imports_test.go` enforces that, and a second
  test in the same file asserts that `template.HTML`, `template.JS`,
  `template.URL` and their four siblings appear nowhere, so contextual
  auto-escaping is the package's single escape boundary. It holds no client and
  no context, issues no cluster call and makes no model call — two separate
  promises. The page it renders is HTML, not one of the six versioned JSON
  documents, so no `schemaVersion` moves
  (see [website/docs/features/dashboard.md](website/docs/features/dashboard.md)).
```

- [ ] **Step 8: Add the changelog entry**

Under `## [Unreleased]` in `CHANGELOG.md`, in an `### Added` block:

```markdown
- **In-cluster dashboard (`kubeagent watch --dashboard`).** A read-only HTML
  page at `/dashboard` on the daemon's metrics port: tracked incidents active
  and resolved with firing duration and time-to-resolution, per-cluster
  reachability, the aggregate counters, SLO burn when `--slo-target` is set, and
  on-incident explanations when `--explain` is set. It renders only state the
  daemon already tracks, so it performs no extra cluster read and needs no extra
  RBAC, and it makes no model call. Server-rendered with zero JavaScript, so
  `html/template`'s contextual escaping is the single escape boundary; the new
  `internal/dashboard` package imports nothing from kubeagent, enforced by a
  source-level test. The page is **unauthenticated**, exactly like `/metrics`
  and `/issues` on the same port — see
  [the docs](https://k8sproject.top/features/dashboard/) for the exposure
  posture. Enable with `--dashboard`, `KUBEAGENT_DASHBOARD=true`, or
  `--set dashboard.enabled=true`. Completes Theme G.
```

- [ ] **Step 9: Verify the docs build**

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml; cd ..
```

Expected: "Documentation built", exit 0, and **no `WARNING` line naming any page**. The red "Material for MkDocs 2.0" banner is cosmetic — judge by the exit code and the absence of page warnings.

- [ ] **Step 10: Scan the new docs for leaked values**

```bash
grep -rnE '([0-9]{1,3}\.){3}[0-9]{1,3}|https?://[^ )`]+/[^ )`]+' \
  website/docs/features/dashboard.md deploy/README.md CHANGELOG.md | grep -v k8sproject.top
```

Expected: no output, or only RFC 5737 addresses. Any private address, internal
hostname, or URL carrying more than `scheme://host` (other than the project's
own `k8sproject.top` links) is a defect.

- [ ] **Step 11: Run the full gate**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
```

Expected: PASS.

- [ ] **Step 12: Commit**

```bash
git add website/docs/features/dashboard.md website/mkdocs.yml \
  website/docs/features/watch-mode.md website/docs/compatibility.md \
  website/docs/roadmap.md deploy/README.md CHANGELOG.md CLAUDE.md
git commit -s -m "docs: in-cluster dashboard, and Theme G complete

The feature page states the exposure posture in its own section rather than
in a footnote: kubeagent implements no authentication for the dashboard and
terminates no TLS, the posture is the one /metrics and /issues already have
on that port, and the layers that do this properly go in front.

Compatibility gains the four stable surfaces and folds the dashboard into
the unstable-markup bullet the HTML report already carries. The roadmap's
last Theme G item moves from ahead to shipped."
```

---

## Slice gate (run once, after Task 10)

Not a task — the gate for the whole branch, run once at the end.

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY          # keep keys out of the shell
go build -o kubeagent .
./chaos/run.sh --recreate        # ~35-40 minutes; run in the background and watch the log
```

The exit code is the gate. Zero means every assertion passed. Non-zero names the
failures on the console and in the report's `## Assertion summary`.

Then confirm the total the harness actually printed:

```bash
grep -A3 '## Assertion summary' docs/testing/chaos-results.md
```

Expected: `assertions run: 128`, `failed: 0`. If the number differs from 128,
the printed number wins — correct all four documents from Task 9 to match it in
a follow-up commit.

Finally, verify DCO across the branch:

```bash
bash scripts/dco-check.sh main HEAD
```

Expected: `DCO: all N commit(s) signed off.`

---

## Self-review

**Spec coverage** — every section of the spec maps to a task:

| Spec section | Task |
|---|---|
| `internal/dashboard` pure renderer, imports nothing, `template.Must` at init | 1, 2 |
| Page layout items 1-5 (header, cluster strip, active, resolved, tiles) | 2 |
| Page layout items 6-7 (SLO, explanations) | 3 |
| Total order, empty and starting states | 2, 3, 4 |
| Escaping, inert document, cluster identity | 2, 3 (tests), 4 (fixture inspection) |
| Golden test with `-update` | 4 |
| `FuzzDashboardRender` | 5 |
| Source test: no unsafe conversions; source test: no kubeagent imports | 1 |
| `dashboardInput()`, conditional registration, buffered render, 500 | 6 |
| `internal/watch` tests: 404, 200 + content type, copy-not-alias, race | 6 |
| `--dashboard` + `KUBEAGENT_DASHBOARD` | 7 |
| `dashboard.enabled`, no new Service port, chart MINOR bump | 8 |
| Four chaos assertions, 124 → 128 across four documents | 9 |
| `website/docs/features/dashboard.md`, compatibility, watch-mode, nav, roadmap, `deploy/README.md`, `CHANGELOG.md` | 10 |
| Compatibility classification table (four rows) | 10 |
| Refresh hardcoded at 30s, no flag | 2 (`RefreshSeconds`) |
| RBAC untouched, six JSON documents untouched, golden-scan.txt untouched | Global Constraints; verified by the per-task `go test ./... -p 2` gate |

Two spec sentences deliberately land differently from their literal wording, and
both are recorded where an implementer will see them:

1. The spec's fourth chaos assertion reads "no credential material and no
   filesystem path". Task 9 implements it as a count of the alert-webhook-URL
   host in the page, asserted equal to zero — the only credential-class value
   the daemon actually holds during that scenario, and therefore a stronger test
   than a generic path grep. It counts rather than naming a needle because
   `assert.sh` embeds the needle in its own PASS line, which would put the
   endpoint into the report on every passing run; scenario 20 and the webhook
   redaction check in this same scenario count for that reason. The reasoning is
   in the task and in the commit message.
2. The spec lists `internal/dashboard`'s imports as six packages;
   `strings.Join` for the explanation issue list makes it seven, all standard
   library. The invariant is "nothing from kubeagent", which is what the test
   enforces and what CLAUDE.md records in Task 10.

**Type consistency** — `Input`, `Cluster`, `Incident`, `Stats`, `SLO`,
`SLOWindow`, `Explanation`, `Render`, `RefreshSeconds`, `newView`, `view`,
`clusterRow`, `incidentRow`, `tiles`, `sloView`, `sloWindowRow`,
`explanationRow`, `lessKey`, `target`, `clusterState`, `humanDuration`,
`meanResolution`, `percent`, `ratio`, `budgetRemaining`, `finite`, `none`,
`coverageFloor`, `maxRenderableSeconds`, `payloadInput`, `render`, `fixedNow`,
`goldenInput`, `fuzzInput`, `stripBrackets`, `packageFiles`, `dashboardInput`,
`renderDashboard`, `Config.Dashboard`, `Config.Version`, `watchOptions.dashboard`
are each defined in exactly one task and referenced with the same spelling and
signature everywhere after.

**Placeholder scan** — every code step carries the code. No "TBD", no "add
appropriate error handling", no "similar to Task N".
