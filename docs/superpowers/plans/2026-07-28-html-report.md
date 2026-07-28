# Shareable HTML Report Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `kubeagent scan --output html` writes one self-contained HTML document to stdout — the artifact you attach to an incident ticket or mail to a colleague who has no cluster access.

**Architecture:** A new leaf package `internal/htmlreport` renders a `report.Input` plus a `[]findings.Finding` plus `scan.Result.PartialReads` into a single HTML file using stdlib `html/template` and `embed`. It is a new package rather than a third `case` in `report.PrintInventory` because `internal/report` deliberately does not import `internal/scan`, and a findings-first layout needs `internal/findings`, which does. `main.go` picks the renderer at the existing `PrintInventory` call site.

**Tech Stack:** Go 1.26, standard library only (`html/template`, `embed`, `io`, `time`). No new `go.mod` entries. Existing packages: `internal/report`, `internal/findings`, `internal/scan`, `internal/clusterhealth`, `internal/inventory`.

**Spec:** [docs/superpowers/specs/2026-07-28-html-report-design.md](../specs/2026-07-28-html-report-design.md) (commits `d6d75a4`, `a20d1a6`).

**Branch:** `html-report`, cut off `main` at `eb70b0d` (v0.65.0).

## Global Constraints

- **READ-ONLY.** Rendering performs zero cluster calls — it is a pure function over data `scan` already collected. `internal/htmlreport` must never import `k8s.io/client-go`.
- **No LLM calls on the render path.** `internal/htmlreport` must never import `internal/explain`, `internal/investigate`, or `internal/remediate`.
- **`html/template`, never `text/template`.** The contextual auto-escaping is a security property: container termination messages, event reasons, and image-pull errors are free-form cluster-controlled strings that land verbatim in this document.
- **No `<script>` in the rendered document**, and no external stylesheet, font, or image. The page must open offline and survive a strict Content-Security-Policy.
- **No cluster identity in the document**: no context name, no API server URL, no kubeconfig path. A namespace name is scope, not identity, and is allowed.
- **No new entries in `go.mod` or `go.sum`.** Everything used is stdlib.
- **Standard-library `flag` only.** No Cobra.
- **Sequential.** No goroutines anywhere in this slice.
- **`internal/report/testdata/golden-scan.txt` must stay byte-identical.** If a change moves it, the change is wrong — revert and rethink.
- `scan`'s exit code is unchanged in both directions: still `0` on an unhealthy cluster, still non-zero on a real error.
- `watch`, `mcp`, and `gate` behavior unchanged. `gate` does **not** get `--output html`.
- **No secrets, credentials, private IPs, or internal hostnames** — in the document, the fixtures, the tests, or the docs. Documentation and test IPs must be RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`). The existing golden fixtures use `10.244.x.x` pod IPs, which are Kubernetes-internal pod-network addresses in published example output — reuse of those exact literals is fine; do not introduce new private ranges.
- **No `Co-Authored-By: Claude` trailer** and no AI attribution of any kind in commits, code, comments, or docs.
- **TDD:** write the failing test first, run it, watch it fail, then implement.
- Go lives at `/usr/local/go/bin` — every task starts with `export PATH=$PATH:/usr/local/go/bin`.
- **Run the full suite with `go test -p 2 ./...`.** At full parallelism the Go *linker* can panic in `cmd/link/internal/ld.(*pclntab).generatePctab` on `internal/advisory`. That is an environment issue, not a code failure; `-p 2` passes clean.

---

## File Structure

| File | Responsibility |
|------|----------------|
| `internal/htmlreport/htmlreport.go` | `Input`, `Render`, and the `view` model that flattens `Input` into what the template ranges over. All logic lives here so the template stays dumb. |
| `internal/htmlreport/report.html.tmpl` | The document: doctype, inline `<style>`, header, findings table, blind spots, detail sections. Embedded with `//go:embed`. |
| `internal/htmlreport/htmlreport_test.go` | Behavior tests: escaping, ordering, counts, empty state, identity leak, write errors, blind spots, filter, detail sections. |
| `internal/htmlreport/golden_test.go` | Golden snapshot of the whole document against a fixed clock. |
| `internal/htmlreport/testdata/golden-report.html` | The snapshot. |
| `main.go` | `--output html` accepted and routed; usage line updated; `renderScan` helper. |
| `main_test.go` | Format-validation and plumbing tests for the new path. |
| `website/docs/features/html-report.md` | Feature page. |
| `website/mkdocs.yml`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md` | Nav, feature bullet, changelog entry, roadmap status. |

---

### Task 1: `internal/htmlreport` — package, `Render`, header and findings table

**Files:**
- Create: `internal/htmlreport/htmlreport.go`
- Create: `internal/htmlreport/report.html.tmpl`
- Create: `internal/htmlreport/htmlreport_test.go`

**Interfaces:**
- Consumes: `report.Input` (`internal/report/report.go:113`), `findings.Finding` and `findings.Level` (`internal/findings/findings.go:28,71`), `scan.ReadFailure` (`internal/scan/scan.go:71`), `clusterhealth.ClusterHealth` (`internal/clusterhealth/clusterhealth.go:20`), `inventory.Workload`.
- Produces: `htmlreport.Input` (fields `Report report.Input`, `Findings []findings.Finding`, `Blind []scan.ReadFailure`, `Namespace string`, `Version string`) and `func Render(w io.Writer, in Input) error`. Task 4 calls exactly this.

**Reference facts you need and must not re-derive:**
- `findings.Level.String()` returns exactly `"critical"`, `"warning"`, `"info"` — lowercase. These double as CSS class names.
- `findings.Flatten` already ends in `findings.Sort` (`internal/findings/findings.go:176`). **`Render` must not sort.**
- `report.Input.Now` is documented `clock for relative ages; main sets time.Now(); zero → wall-clock`. `Render` follows the same contract.
- `findings.Finding` fields: `Level`, `Kind`, `Namespace`, `Name`, `Issue`, `Reason`, `Owner`.

- [ ] **Step 1: Write the failing tests**

Create `internal/htmlreport/htmlreport_test.go`:

```go
package htmlreport

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/report"
)

// fixedNow is the clock every test that cares about the timestamp injects.
var fixedNow = time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)

// render is the test helper: it renders and fails the test on any error, so the
// assertions below read as assertions about the document, not about plumbing.
func render(t *testing.T, in Input) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, in); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return buf.String()
}

// TestRenderEscapesClusterControlledText is the load-bearing security test. A
// container termination message or image-pull error is a free-form string the
// cluster controls, and it lands verbatim in a document someone mails to a
// colleague. This test fails if anyone swaps html/template for text/template.
func TestRenderEscapesClusterControlledText(t *testing.T) {
	got := render(t, Input{
		Report:  report.Input{Now: fixedNow},
		Version: "v0.66.0",
		Findings: []findings.Finding{{
			Level: findings.Critical, Kind: "Deployment", Namespace: "shop", Name: "web",
			Issue:  "CrashLoopBackOff",
			Reason: `<script>alert(1)</script> exited with "status 2" & no retry`,
		}},
	})
	if strings.Contains(got, "<script>alert(1)</script>") {
		t.Error("the raw <script> tag reached the document — cluster-controlled text is not being escaped")
	}
	for _, want := range []string{"&lt;script&gt;", "&#34;status 2&#34;", "&amp;"} {
		if !strings.Contains(got, want) {
			t.Errorf("escaped form %q missing from the document", want)
		}
	}
}

// TestRenderCarriesNoScript guards the CSP property: the document must be inert.
func TestRenderCarriesNoScript(t *testing.T) {
	got := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if strings.Contains(strings.ToLower(got), "<script") {
		t.Error("the document contains a <script> tag; it must be inert to survive a strict CSP")
	}
}

// TestRenderPreservesFindingOrder pins that Render never re-sorts. findings.Flatten
// already calls findings.Sort; a second ordering here would be free to drift from it.
func TestRenderPreservesFindingOrder(t *testing.T) {
	got := render(t, Input{
		Report:  report.Input{Now: fixedNow},
		Version: "v0.66.0",
		Findings: []findings.Finding{
			{Level: findings.Critical, Kind: "Deployment", Namespace: "shop", Name: "zeta-first", Issue: "CrashLoopBackOff"},
			{Level: findings.Warning, Kind: "Service", Namespace: "shop", Name: "alpha-second", Issue: "NoEndpoints"},
			{Level: findings.Info, Kind: "Pod", Namespace: "shop", Name: "mid-third", Issue: "Noted"},
		},
	})
	first, second, third :=
		strings.Index(got, "zeta-first"), strings.Index(got, "alpha-second"), strings.Index(got, "mid-third")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("a finding is missing from the document: %d %d %d", first, second, third)
	}
	if !(first < second && second < third) {
		t.Errorf("Render reordered the findings: positions %d, %d, %d — it must render the slice as handed", first, second, third)
	}
}

// TestRenderCountsBySeverity checks the header tally, which is what a reader
// skims before anything else.
func TestRenderCountsBySeverity(t *testing.T) {
	got := render(t, Input{
		Report:  report.Input{Now: fixedNow},
		Version: "v0.66.0",
		Findings: []findings.Finding{
			{Level: findings.Critical, Kind: "Deployment", Name: "a", Issue: "CrashLoopBackOff"},
			{Level: findings.Critical, Kind: "Deployment", Name: "b", Issue: "ImagePullBackOff"},
			{Level: findings.Warning, Kind: "Service", Name: "c", Issue: "NoEndpoints"},
		},
	})
	for _, want := range []string{"2 critical", "1 warning", "0 info"} {
		if !strings.Contains(got, want) {
			t.Errorf("header tally missing %q", want)
		}
	}
}

// TestRenderHeaderCarriesVersionAndScope covers both the all-namespaces and the
// scoped case, since the scope line is the only thing telling a reader how much
// of the cluster this document actually covers.
func TestRenderHeaderCarriesVersionAndScope(t *testing.T) {
	all := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if !strings.Contains(all, "v0.66.0") {
		t.Error("header is missing the kubeagent version")
	}
	if !strings.Contains(all, "all namespaces") {
		t.Error(`an unscoped scan must say "all namespaces"`)
	}
	if !strings.Contains(all, "2026-07-28 09:30:00 UTC") {
		t.Error("header is missing the injected generation timestamp")
	}
	scoped := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0", Namespace: "shop"})
	if !strings.Contains(scoped, "namespace shop") {
		t.Error(`a scan with -n shop must say "namespace shop"`)
	}
	if strings.Contains(scoped, "all namespaces") {
		t.Error("a scoped scan must not claim to cover all namespaces")
	}
}

// TestRenderEmptyClusterStatesItExplicitly: a headless table reads as a broken
// report. A clean cluster must say so.
func TestRenderEmptyClusterStatesItExplicitly(t *testing.T) {
	got := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if !strings.Contains(got, "No findings") {
		t.Error("a scan with no findings must say so explicitly")
	}
	if strings.Contains(got, "<tbody>") {
		t.Error("an empty findings set must not render a headless table")
	}
}

// TestRenderLeaksNoIdentity: the document is shared, so it must disclose nothing
// about how kubeagent reached the cluster. The fixture below is deliberately free
// of paths and URLs, so any hit came from the renderer, not from the data.
func TestRenderLeaksNoIdentity(t *testing.T) {
	got := render(t, Input{
		Report:    report.Input{Now: fixedNow},
		Version:   "v0.66.0",
		Namespace: "shop",
		Findings: []findings.Finding{{
			Level: findings.Critical, Kind: "Deployment", Namespace: "shop", Name: "web",
			Issue: "CrashLoopBackOff", Reason: "container web exited with status 2",
		}},
	})
	for _, banned := range []string{"://", "/home", ".kube", "kubeconfig", "--context"} {
		if strings.Contains(got, banned) {
			t.Errorf("the document contains %q; it must carry no cluster identity and no external reference", banned)
		}
	}
}

// TestRenderZeroClockFallsBackToWallClock mirrors report.Input.Now's documented
// contract, so a caller that forgets the clock gets today, not year 1.
func TestRenderZeroClockFallsBackToWallClock(t *testing.T) {
	got := render(t, Input{Version: "v0.66.0"})
	if strings.Contains(got, "0001-01-01") {
		t.Error("a zero Now rendered a year-1 timestamp; it must fall back to wall-clock")
	}
}

// errWriter fails every write, standing in for `kubeagent scan --output html | head`.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestRenderReturnsWriteErrors: a closed downstream pipe must surface, not be swallowed.
func TestRenderReturnsWriteErrors(t *testing.T) {
	err := Render(errWriter{}, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if err == nil {
		t.Fatal("Render swallowed a write error")
	}
	if !strings.Contains(err.Error(), "broken pipe") {
		t.Errorf("Render lost the underlying write error: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/htmlreport/
```

Expected: FAIL — the package does not compile. Errors name `undefined: Render` and `undefined: Input`.

- [ ] **Step 3: Write the template**

Create `internal/htmlreport/report.html.tmpl`. This step's template covers the header, the findings table, and the empty state; Task 2 adds the blind-spots block, the severity filter, and the detail sections.

```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>kubeagent scan report</title>
<style>
  :root {
    --fg: #1a1a1a; --muted: #5c5c5c; --bg: #ffffff; --panel: #f6f6f6;
    --line: #d8d8d8; --critical: #b3261e; --warning: #8a6100; --info: #35618f;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; padding: 2rem 1.25rem; color: var(--fg); background: var(--bg);
    font: 15px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
  }
  main { max-width: 68rem; margin: 0 auto; }
  h1 { font-size: 1.5rem; margin: 0 0 .25rem; }
  h2 { font-size: 1.05rem; margin: 0 0 .5rem; }
  h3 { font-size: .95rem; margin: .75rem 0 .25rem; }
  .meta, .tally { margin: .25rem 0; color: var(--muted); font-size: .9rem; }
  .pill {
    display: inline-block; padding: .1rem .5rem; margin-right: .4rem;
    border: 1px solid var(--line); border-radius: 999px; font-size: .8rem;
  }
  .pill.critical { color: var(--critical); }
  .pill.warning { color: var(--warning); }
  .pill.info { color: var(--info); }
  section { margin-top: 1.75rem; }
  table { width: 100%; border-collapse: collapse; font-size: .88rem; }
  th, td { text-align: left; padding: .4rem .5rem; border-bottom: 1px solid var(--line); vertical-align: top; }
  th { font-weight: 600; color: var(--muted); text-transform: uppercase; font-size: .72rem; letter-spacing: .04em; }
  td.sev { font-weight: 600; white-space: nowrap; }
  tr.critical td.sev { color: var(--critical); }
  tr.warning td.sev { color: var(--warning); }
  tr.info td.sev { color: var(--info); }
  .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .85em; }
  .empty { color: var(--muted); }
</style>
</head>
<body>
<main>

<header>
  <h1>kubeagent scan report</h1>
  <p class="meta">kubeagent {{.Version}} &middot; {{.Scope}} &middot; generated {{.Generated}}</p>
  <p class="tally">
    <span class="pill critical">{{.Counts.Critical}} critical</span>
    <span class="pill warning">{{.Counts.Warning}} warning</span>
    <span class="pill info">{{.Counts.Info}} info</span>
  </p>
</header>

<section class="findings">
  <h2>Findings</h2>
{{- if .Findings}}
  <table>
    <thead><tr><th>Severity</th><th>Kind</th><th>Namespace</th><th>Name</th><th>Issue</th><th>Reason</th><th>Owner</th></tr></thead>
    <tbody>
{{- range .Findings}}
      <tr class="{{.Level}}"><td class="sev">{{.Level}}</td><td>{{.Kind}}</td><td>{{.Namespace}}</td><td class="mono">{{.Name}}</td><td>{{.Issue}}</td><td>{{.Reason}}</td><td>{{.Owner}}</td></tr>
{{- end}}
    </tbody>
  </table>
{{- else}}
  <p class="empty">No findings. Every workload kubeagent could see is healthy.</p>
{{- end}}
</section>

</main>
</body>
</html>
```

- [ ] **Step 4: Write the Go implementation**

Create `internal/htmlreport/htmlreport.go`:

```go
// Package htmlreport renders a scan result as one self-contained HTML document:
// the artifact you attach to an incident ticket, paste into a pull request, or
// mail to a colleague who has no cluster access.
//
// The document is deliberately inert. It carries no <script>, no external
// stylesheet, font, or image, so it opens offline and survives a strict
// Content-Security-Policy — the environments that show these files (artifact
// previews, sandboxed mail clients, corporate proxies) block inline script and
// remote fetches, and none of them block inline CSS.
//
// It also carries no cluster identity: no context name, no API server URL, no
// kubeconfig path. Context names in the wild embed account IDs and internal
// hostnames, and this file is meant to be forwarded. Whoever shares it names the
// cluster in the ticket.
package htmlreport

import (
	_ "embed"
	"html/template"
	"io"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/scan"
)

// Input is everything the document renders. It is a struct rather than a
// parameter list because the fields arrive from different layers: Report is the
// presentation view main already builds for text and JSON, Findings comes from
// internal/findings, and Blind comes straight off the scan.Result.
type Input struct {
	// Report is the same value --output text and --output json render from.
	Report report.Input
	// Findings is severity-ranked by findings.Flatten, which already ends in
	// findings.Sort. Render preserves that order and never re-sorts: a second
	// definition of the same order would be free to drift from the first.
	Findings []findings.Finding
	// Blind is scan.Result.PartialReads — what kubeagent could not read. It is
	// its own field because report.Input has none, and a shared document that
	// silently omits its blind spots is the same green-when-blind failure the
	// CI gate exists to prevent.
	Blind []scan.ReadFailure
	// Namespace is the -n value; "" means all namespaces. report.Input carries
	// no namespace, and ClusterHealth.ScopeNote is not a substitute: it names no
	// namespace and is empty for -n kube-system.
	Namespace string
	// Version is the kubeagent version stamped into the header.
	Version string
}

//go:embed report.html.tmpl
var templateSource string

// tmpl is parsed once at package init so a malformed template fails in CI, not
// at an operator's terminal in the middle of an incident.
//
// html/template, never text/template: the contextual auto-escaping is a security
// property here, not formatting. Container termination messages, event reasons,
// and image-pull errors are free-form strings the cluster controls, and they
// land verbatim in a document that gets forwarded.
var tmpl = template.Must(template.New("report").Parse(templateSource))

// view is the flattened shape the template ranges over. Every decision lives in
// newView so the template stays free of logic beyond ranging and conditionals.
type view struct {
	Version     string
	Scope       string
	Generated   string
	Counts      counts
	Findings    []findingRow
	Blind       []scan.ReadFailure
	Cluster     clusterhealth.ClusterHealth
	Workloads   []inventory.Workload
	Explanation string
}

// counts is the header tally, and also labels the severity filter controls.
type counts struct {
	Critical, Warning, Info, Total int
	// AtLeastWarning is Critical+Warning, precomputed because templates have no
	// arithmetic and the "warning and above" filter control needs the number.
	AtLeastWarning int
}

// findingRow is one row of the findings table. Level is the lowercase spelling
// from findings.Level.String(), used as both the visible label and the CSS class
// the severity filter selects on — so the two can never disagree.
type findingRow struct {
	Level     string
	Kind      string
	Namespace string
	Name      string
	Issue     string
	Reason    string
	Owner     string
}

// Render writes the complete HTML document to w. It performs no cluster calls:
// everything it needs was collected by the scan that produced in.
func Render(w io.Writer, in Input) error {
	return tmpl.Execute(w, newView(in))
}

// newView flattens Input into the template's view model.
func newView(in Input) view {
	now := in.Report.Now
	if now.IsZero() {
		// Same contract as report.Input.Now, which documents zero as wall-clock:
		// a caller that forgets the clock gets today, not year 1.
		now = time.Now()
	}
	scope := "all namespaces"
	if in.Namespace != "" {
		scope = "namespace " + in.Namespace
	}
	v := view{
		Version:     in.Version,
		Scope:       scope,
		Generated:   now.UTC().Format("2006-01-02 15:04:05 UTC"),
		Blind:       in.Blind,
		Cluster:     in.Report.Cluster,
		Workloads:   in.Report.Result.Workloads,
		Explanation: in.Report.Explanation,
	}
	for _, f := range in.Findings {
		v.Findings = append(v.Findings, findingRow{
			Level:     f.Level.String(),
			Kind:      f.Kind,
			Namespace: f.Namespace,
			Name:      f.Name,
			Issue:     f.Issue,
			Reason:    f.Reason,
			Owner:     f.Owner,
		})
		switch f.Level {
		case findings.Critical:
			v.Counts.Critical++
		case findings.Warning:
			v.Counts.Warning++
		default:
			v.Counts.Info++
		}
	}
	v.Counts.Total = len(in.Findings)
	v.Counts.AtLeastWarning = v.Counts.Critical + v.Counts.Warning
	return v
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/htmlreport/ -v
```

Expected: PASS — all nine tests.

- [ ] **Step 6: Confirm no new dependency and the rest of the suite is untouched**

```bash
export PATH=$PATH:/usr/local/go/bin
git diff --exit-code go.mod go.sum && echo "go.mod/go.sum unchanged"
go test -p 2 ./...
```

Expected: `go.mod/go.sum unchanged`, then every package `ok` or `no test files`. `internal/report` must still pass — `golden-scan.txt` is untouched by this task.

- [ ] **Step 7: Commit**

```bash
git add internal/htmlreport/
git commit -m "feat(htmlreport): render scan findings as a self-contained HTML document

A new leaf package rather than a third case in report.PrintInventory:
internal/report deliberately does not import internal/scan, and a
findings-first layout needs internal/findings, which does.

html/template, not text/template. Container termination messages and
image-pull errors are free-form cluster-controlled strings that land
verbatim in a document meant to be forwarded, so the contextual
auto-escaping is a security property, not formatting.

The document is inert -- no script, no external stylesheet, font, or
image -- so it opens offline and survives a strict CSP. It carries no
cluster identity either: context names in the wild embed account IDs and
internal hostnames."
```

---

### Task 2: Blind spots, the pure-CSS severity filter, and the detail sections

**Files:**
- Modify: `internal/htmlreport/report.html.tmpl`
- Modify: `internal/htmlreport/htmlreport_test.go` (append tests)

**Interfaces:**
- Consumes: the `view` model from Task 1 — its `Blind []scan.ReadFailure`, `Cluster clusterhealth.ClusterHealth`, `Workloads []inventory.Workload`, `Explanation string`, and `Counts.AtLeastWarning` fields are already populated by `newView` and currently unrendered.
- Produces: no new Go symbols. The template gains the blind-spots block, the filter, and three `<details>` sections.

**Reference facts you need and must not re-derive:**
- `scan.ReadFailure` has exactly two fields: `Resource string`, `Reason string`.
- `clusterhealth.ClusterHealth` fields used here: `Verdict string`, `NodesTotal int`, `NodesReady int`, `NodeIssues []string`, `SystemIssues []string`, `ScopeNote string`.
- `inventory.Workload` fields used here: `Namespace`, `Kind`, `Name`, `Desired int`, `Ready int`, `Status`, `Image`, `RootCause`.
- The general sibling combinator `~` only matches *following* siblings, so the radio inputs must precede the table inside `.findings`. `#f-crit:checked ~ table tr.warning` works across the intervening `<tbody>` because `table tr.warning` is a descendant selector.

- [ ] **Step 1: Write the failing tests**

Append to `internal/htmlreport/htmlreport_test.go`:

```go
// TestRenderBlindSpotsBlock: a document that omits what kubeagent could not read
// is green-when-blind, and a rendered file is easier to over-trust than an exit
// code. The block must appear whenever there are partial reads — and only then.
func TestRenderBlindSpotsBlock(t *testing.T) {
	with := render(t, Input{
		Report:  report.Input{Now: fixedNow},
		Version: "v0.66.0",
		Blind: []scan.ReadFailure{
			{Resource: "pods in namespace restricted", Reason: `forbidden: User cannot list resource "pods"`},
			{Resource: "horizontalpodautoscalers", Reason: "the server could not find the requested resource"},
		},
	})
	if !strings.Contains(with, "Blind spots") {
		t.Error("partial reads did not render a blind-spots block")
	}
	for _, want := range []string{"pods in namespace restricted", "horizontalpodautoscalers", "could not find the requested resource"} {
		if !strings.Contains(with, want) {
			t.Errorf("blind-spots block is missing %q", want)
		}
	}
	without := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if strings.Contains(without, "Blind spots") {
		t.Error("a scan with no partial reads must not render an empty blind-spots block")
	}
}

// TestRenderSeverityFilterIsPureCSS pins the mechanism, not just the appearance:
// the filter must be :checked sibling selectors, because the document has no
// JavaScript and must keep working where script is blocked.
func TestRenderSeverityFilterIsPureCSS(t *testing.T) {
	got := render(t, Input{
		Report:  report.Input{Now: fixedNow},
		Version: "v0.66.0",
		Findings: []findings.Finding{
			{Level: findings.Critical, Kind: "Deployment", Name: "a", Issue: "CrashLoopBackOff"},
			{Level: findings.Warning, Kind: "Service", Name: "b", Issue: "NoEndpoints"},
		},
	})
	for _, want := range []string{
		`id="f-all"`, `id="f-warn"`, `id="f-crit"`,
		"#f-crit:checked ~ table tr.warning",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("severity filter is missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(got), "<script") {
		t.Error("the severity filter must be pure CSS — the document must stay script-free")
	}
	if !strings.Contains(got, "Critical 1") || !strings.Contains(got, "Warning and above 2") {
		t.Error("the filter controls must be labelled with their counts")
	}
}

// TestRenderDetailSections: the detail lives behind collapsed <details> so the
// findings stay above the fold, but it must actually be in the document.
func TestRenderDetailSections(t *testing.T) {
	got := render(t, Input{
		Report: report.Input{
			Now: fixedNow,
			Cluster: clusterhealth.ClusterHealth{
				Verdict: "Degraded", NodesTotal: 3, NodesReady: 2,
				NodeIssues:   []string{"worker-2 NotReady: KubeletNotReady"},
				SystemIssues: []string{"kube-system/coredns Degraded 1/2"},
			},
			Result: inventory.Result{Workloads: []inventory.Workload{{
				Namespace: "shop", Kind: "Deployment", Name: "web", Desired: 3, Ready: 1,
				Status: "Degraded", Image: "busybox:1.36", RootCause: "node worker-2 (NotReady)",
			}}},
		},
		Version: "v0.66.0",
	})
	if strings.Count(got, "<details") < 2 {
		t.Errorf("want at least two <details> sections, got %d", strings.Count(got, "<details"))
	}
	if strings.Contains(got, "<details open") {
		t.Error("detail sections must be collapsed by default so the findings stay above the fold")
	}
	for _, want := range []string{
		"Cluster health", "Degraded", "2/3",
		"worker-2 NotReady: KubeletNotReady", "kube-system/coredns Degraded 1/2",
		"Workload inventory", "busybox:1.36", "node worker-2 (NotReady)", "1/3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail sections are missing %q", want)
		}
	}
}

// TestRenderExplanationOnlyWhenPresent: --explain produces one plain-English
// paragraph worth carrying into a shared document. Without the flag there is no
// narrative, and an empty section would read as a failure.
func TestRenderExplanationOnlyWhenPresent(t *testing.T) {
	with := render(t, Input{
		Report:  report.Input{Now: fixedNow, Explanation: "The shop namespace is down because worker-2 stopped heartbeating."},
		Version: "v0.66.0",
	})
	if !strings.Contains(with, "worker-2 stopped heartbeating") {
		t.Error("the --explain narrative did not reach the document")
	}
	without := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if strings.Contains(without, "Explanation") {
		t.Error("a scan without --explain must not render an empty Explanation section")
	}
}
```

Add these imports to the existing block at the top of the file:

```go
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/scan"
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/htmlreport/ -run 'BlindSpots|SeverityFilter|DetailSections|ExplanationOnly' -v
```

Expected: FAIL — four tests, with messages including `partial reads did not render a blind-spots block`, `severity filter is missing "id=\"f-all\""`, `want at least two <details> sections, got 0`, and `the --explain narrative did not reach the document`.

- [ ] **Step 3: Extend the stylesheet**

In `internal/htmlreport/report.html.tmpl`, replace the line:

```css
  .empty { color: var(--muted); }
```

with:

```css
  .empty { color: var(--muted); }
  .blind { background: var(--panel); border-left: 3px solid var(--warning); padding: .75rem 1rem; }
  .blind p { color: var(--muted); margin: .25rem 0 .5rem; }
  ul { margin: .5rem 0; padding-left: 1.2rem; }
  details { border: 1px solid var(--line); border-radius: 4px; padding: .5rem .75rem; margin-bottom: .6rem; }
  summary { cursor: pointer; font-weight: 600; }
  details > *:not(summary) { margin-top: .75rem; }
  /* Severity filter: radio inputs plus :checked sibling selectors. The document
     carries no JavaScript, so this keeps working where script is blocked. The
     inputs must precede the table — `~` matches following siblings only. */
  .findings > input[type=radio] { position: absolute; opacity: 0; }
  .filter { margin-bottom: .6rem; }
  .filter label {
    display: inline-block; padding: .15rem .6rem; margin-right: .3rem; cursor: pointer;
    border: 1px solid var(--line); border-radius: 999px; font-size: .8rem; color: var(--muted);
  }
  #f-all:checked ~ .filter label[for=f-all],
  #f-warn:checked ~ .filter label[for=f-warn],
  #f-crit:checked ~ .filter label[for=f-crit] { background: var(--fg); color: var(--bg); border-color: var(--fg); }
  #f-warn:checked ~ table tr.info,
  #f-crit:checked ~ table tr.info,
  #f-crit:checked ~ table tr.warning { display: none; }
  .findings > input[type=radio]:focus-visible ~ .filter label { outline: 2px solid var(--info); }
```

- [ ] **Step 4: Add the blind-spots block**

In `internal/htmlreport/report.html.tmpl`, insert between the closing `</header>` and `<section class="findings">`:

```html
{{- if .Blind}}
<section class="blind">
  <h2>Blind spots</h2>
  <p>kubeagent could not read the following, so the findings below are incomplete.</p>
  <ul>
{{- range .Blind}}
    <li><span class="mono">{{.Resource}}</span> &mdash; {{.Reason}}</li>
{{- end}}
  </ul>
</section>
{{- end}}
```

- [ ] **Step 5: Add the severity filter**

In `internal/htmlreport/report.html.tmpl`, inside `<section class="findings">`, replace:

```html
{{- if .Findings}}
  <table>
```

with:

```html
{{- if .Findings}}
  <input type="radio" name="sev" id="f-all" checked>
  <input type="radio" name="sev" id="f-warn">
  <input type="radio" name="sev" id="f-crit">
  <div class="filter">
    <label for="f-all">All {{.Counts.Total}}</label>
    <label for="f-warn">Warning and above {{.Counts.AtLeastWarning}}</label>
    <label for="f-crit">Critical {{.Counts.Critical}}</label>
  </div>
  <table>
```

- [ ] **Step 6: Add the detail sections**

In `internal/htmlreport/report.html.tmpl`, insert between the closing `</section>` of the findings block and `</main>`:

```html
<section class="detail">
  <h2>Detail</h2>

  <details>
    <summary>Cluster health &mdash; {{.Cluster.Verdict}}, {{.Cluster.NodesReady}}/{{.Cluster.NodesTotal}} nodes ready</summary>
{{- if .Cluster.ScopeNote}}
    <p class="empty">{{.Cluster.ScopeNote}}</p>
{{- end}}
{{- if .Cluster.NodeIssues}}
    <h3>Nodes</h3>
    <ul>
{{- range .Cluster.NodeIssues}}
      <li>{{.}}</li>
{{- end}}
    </ul>
{{- end}}
{{- if .Cluster.SystemIssues}}
    <h3>System workloads</h3>
    <ul>
{{- range .Cluster.SystemIssues}}
      <li>{{.}}</li>
{{- end}}
    </ul>
{{- end}}
  </details>

  <details>
    <summary>Workload inventory &mdash; {{len .Workloads}}</summary>
{{- if .Workloads}}
    <table>
      <thead><tr><th>Namespace</th><th>Kind</th><th>Name</th><th>Ready</th><th>Status</th><th>Image</th><th>Root cause</th></tr></thead>
      <tbody>
{{- range .Workloads}}
        <tr><td>{{.Namespace}}</td><td>{{.Kind}}</td><td class="mono">{{.Name}}</td><td>{{.Ready}}/{{.Desired}}</td><td>{{.Status}}</td><td class="mono">{{.Image}}</td><td>{{.RootCause}}</td></tr>
{{- end}}
      </tbody>
    </table>
{{- else}}
    <p class="empty">No workloads in scope.</p>
{{- end}}
  </details>

{{- if .Explanation}}
  <details>
    <summary>Explanation</summary>
    <p>{{.Explanation}}</p>
  </details>
{{- end}}
</section>
```

- [ ] **Step 7: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/htmlreport/ -v
```

Expected: PASS — all thirteen tests. `TestRenderEmptyClusterStatesItExplicitly` from Task 1 must still pass: the empty-findings path renders no `<tbody>`, and the workload-inventory `<details>` renders `No workloads in scope.` rather than a table when there are none.

- [ ] **Step 8: Run the full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./...
```

Expected: every package `ok` or `no test files`.

- [ ] **Step 9: Commit**

```bash
git add internal/htmlreport/
git commit -m "feat(htmlreport): blind spots, a pure-CSS severity filter, and detail sections

The blind-spots block renders scan.Result.PartialReads whenever it is
non-empty. A shared document that silently omits what kubeagent could not
read is the same green-when-blind failure the CI gate exists to prevent,
and a rendered file is easier to over-trust than an exit code.

The severity filter is radio inputs plus :checked sibling selectors, so
the document stays script-free and keeps working under a strict CSP.

Cluster health, the workload inventory, and the --explain narrative sit
in collapsed <details> so the findings stay above the fold."
```

---

### Task 3: Golden snapshot of the whole document

**Files:**
- Create: `internal/htmlreport/golden_test.go`
- Create: `internal/htmlreport/testdata/golden-report.html` (generated, not hand-written)

**Interfaces:**
- Consumes: `Render` and `Input` from Task 1; the template as completed in Task 2.
- Produces: nothing other packages use.

**Why:** the behavior tests assert on substrings, so a layout regression that keeps every substring present would slip past them. The golden is the whole-document check, mirroring `internal/report/golden_test.go`.

- [ ] **Step 1: Write the golden test**

Create `internal/htmlreport/golden_test.go`:

```go
package htmlreport

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/scan"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenNow is the fixed clock for the snapshot, so the fixture holds no
// time-varying bytes and the comparison is a plain byte comparison.
var goldenNow = time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)

const goldenPath = "testdata/golden-report.html"

// goldenInput exercises every rendered part of the document: all three severity
// levels, partial reads, cluster health with both issue lists, a workload table,
// and an --explain narrative.
func goldenInput() Input {
	return Input{
		Version:   "v0.66.0",
		Namespace: "shop",
		Report: report.Input{
			Now: goldenNow,
			Cluster: clusterhealth.ClusterHealth{
				Verdict: "Degraded", NodesTotal: 4, NodesReady: 2,
				NodeIssues: []string{
					"worker-2 NotReady: KubeletNotReady — container runtime is down",
					"worker-1 kubelet not heartbeating (lease 95s stale)",
				},
				SystemIssues: []string{"kube-system/coredns Degraded 1/2"},
				ScopeNote:    "node health only — re-run without -n (or with -n kube-system) for the system workload check",
			},
			Result: inventory.Result{Workloads: []inventory.Workload{
				{Namespace: "shop", Kind: "Deployment", Name: "web", Desired: 3, Ready: 0,
					Status: "Degraded", Image: "busybox:1.36", RootCause: "node worker-1 (kubelet not heartbeating)"},
				{Namespace: "shop", Kind: "Deployment", Name: "api", Desired: 2, Ready: 0,
					Status: "Degraded", Image: "nginx:9.9.9-nope"},
				{Namespace: "shop", Kind: "StatefulSet", Name: "data", Desired: 1, Ready: 1,
					Status: "Running", Image: "postgres:16"},
			}},
			Explanation: "The shop namespace is degraded because worker-1 stopped heartbeating, " +
				"which stalled web, and api references an image tag that does not exist.",
		},
		Findings: []findings.Finding{
			{Level: findings.Critical, Kind: "Deployment", Namespace: "shop", Name: "web",
				Issue: "CrashLoopBackOff", Reason: `container "web" repeatedly crashes after starting`,
				Owner: "Deployment/web"},
			{Level: findings.Critical, Kind: "Deployment", Namespace: "shop", Name: "api",
				Issue: "ImagePullBackOff", Reason: `Back-off pulling image "nginx:9.9.9-nope": not found`,
				Owner: "Deployment/api"},
			{Level: findings.Warning, Kind: "Service", Namespace: "shop", Name: "payments",
				Issue: "NoEndpoints", Reason: "no ready endpoints"},
			{Level: findings.Info, Kind: "ResourceQuota", Namespace: "shop", Name: "compute",
				Issue: "nearing", Reason: "requests.cpu 3/4 used"},
		},
		Blind: []scan.ReadFailure{
			{Resource: "horizontalpodautoscalers", Reason: `forbidden: User cannot list resource "horizontalpodautoscalers"`},
		},
	}
}

func TestGoldenHTMLReport(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, goldenInput()); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("HTML report output changed:\n%s\n\n"+
			"If this change is intended, run:\n"+
			"  go test ./internal/htmlreport -run TestGoldenHTMLReport -update\n"+
			"then re-read the diff: this document is shared outside the cluster, so a "+
			"new line in it is a disclosure decision.",
			firstDiff(string(want), string(got)))
	}
}

// TestGoldenInputCoversEverySection guards against the fixture silently losing a
// part, which would leave the golden a partial snapshot that still passes.
func TestGoldenInputCoversEverySection(t *testing.T) {
	in := goldenInput()
	if len(in.Blind) == 0 || len(in.Report.Cluster.NodeIssues) == 0 ||
		len(in.Report.Cluster.SystemIssues) == 0 || in.Report.Cluster.ScopeNote == "" ||
		len(in.Report.Result.Workloads) == 0 || in.Report.Explanation == "" {
		t.Fatal("goldenInput must populate every section so the golden stays comprehensive")
	}
	levels := map[findings.Level]bool{}
	for _, f := range in.Findings {
		levels[f.Level] = true
	}
	if !levels[findings.Critical] || !levels[findings.Warning] || !levels[findings.Info] {
		t.Fatalf("goldenInput must exercise all three severity levels, got %v", levels)
	}
}

// firstDiff returns the first differing line, for a readable failure message.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("first difference at line %d:\n  want: %q\n  got:  %q", i+1, wl, gl)
		}
	}
	return "(files differ only in trailing content)"
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/htmlreport/ -run TestGoldenHTMLReport -v
```

Expected: FAIL with `read golden (run with -update to create it): open testdata/golden-report.html: no such file or directory`.

- [ ] **Step 3: Generate the golden**

```bash
export PATH=$PATH:/usr/local/go/bin
mkdir -p internal/htmlreport/testdata
go test ./internal/htmlreport -run TestGoldenHTMLReport -update
```

- [ ] **Step 4: Read the generated golden before trusting it**

```bash
export PATH=$PATH:/usr/local/go/bin
grep -c . internal/htmlreport/testdata/golden-report.html
grep -nE '://|/home|\.kube|<script' internal/htmlreport/testdata/golden-report.html || echo "no identity leak, no script"
```

Expected: a non-zero line count, and `no identity leak, no script`. Then **read the file top to bottom** and confirm every section is present: header with version/scope/timestamp/tally, blind-spots block, filter controls, findings table with all four rows, and three `<details>` sections. A golden generated from a broken template passes forever — this read is the only thing standing between a bug and a permanent fixture.

- [ ] **Step 5: Verify the test now passes and is not self-satisfying**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/htmlreport/ -v
```

Expected: PASS. Then mutation-test the golden to prove it actually compares:

```bash
export PATH=$PATH:/usr/local/go/bin
printf '<!-- tamper -->\n' >> internal/htmlreport/testdata/golden-report.html
go test ./internal/htmlreport/ -run TestGoldenHTMLReport 2>&1 | head -5
git checkout internal/htmlreport/testdata/golden-report.html 2>/dev/null || \
  go test ./internal/htmlreport -run TestGoldenHTMLReport -update
```

Expected: the tampered run FAILS with `HTML report output changed`, then the file is restored. If the tampered run passes, the test is not comparing — stop and fix it.

- [ ] **Step 6: Run the full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./...
```

Expected: every package `ok` or `no test files`.

- [ ] **Step 7: Commit**

```bash
git add internal/htmlreport/
git commit -m "test(htmlreport): golden snapshot of the whole document

The behavior tests assert on substrings, so a layout regression that
keeps every substring present would slip past them. The golden is the
whole-document check.

The fixture injects a fixed clock, so the snapshot holds no time-varying
bytes and the comparison is a plain byte comparison."
```

---

### Task 4: `main.go` — accept and route `--output html`

**Files:**
- Modify: `main.go` — usage string at `:139`, `--output` flag help at `:145`, format validation at `:183-185`, the `PrintInventory` call at `:369-371`, and a new `renderScan` helper next to `resultInput` (`:399`)
- Modify: `main_test.go` — append tests

**Interfaces:**
- Consumes: `htmlreport.Input` and `htmlreport.Render` from Task 1, `findings.Flatten(res scan.Result) []findings.Finding`, `report.PrintInventory(in report.Input, format string, w io.Writer) error`.
- Produces: `func renderScan(w io.Writer, format string, in report.Input, res scan.Result, namespace string) error`, which `main_test.go` drives directly.

**Why a helper and not an inline `if`:** the same reason `gateScanOptions` exists — a test must be able to drive the exact values `runScan` uses without a live cluster. Inline, the only way to reach the HTML path is to connect to a cluster, so a field that silently never reaches `htmlreport.Input` would go unnoticed.

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
func TestScanAcceptsTheHTMLOutputFormat(t *testing.T) {
	// An unknown format is rejected before any cluster connection, so the error
	// text is reachable without a cluster. "html" must not be rejected there.
	err := run([]string{"scan", "--output", "html", "--kubeconfig", filepath.Join(t.TempDir(), "nope")})
	if err != nil && strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("--output html was rejected as an unknown format: %v", err)
	}
}

func TestScanRejectsAnUnknownOutputFormatAndNamesHTML(t *testing.T) {
	err := run([]string{"scan", "--output", "bogus"})
	if err == nil {
		t.Fatal("want an error for an unknown --output format, got nil")
	}
	if !strings.Contains(err.Error(), "want text, json or html") {
		t.Errorf("the rejection must name every accepted format, got: %v", err)
	}
}

func TestUsageMentionsTheHTMLOutputFormat(t *testing.T) {
	err := run([]string{"bogus-subcommand"})
	if err == nil {
		t.Fatal("want a usage error, got nil")
	}
	if !strings.Contains(err.Error(), "--output text|json|html") {
		t.Errorf("usage must advertise the html format on scan, got: %v", err)
	}
}

// TestRenderScanRoutesHTMLWithEveryFieldPlumbed is the regression guard the
// helper exists for: it drives the exact call runScan makes, so a field that
// silently never reaches htmlreport.Input fails here rather than shipping.
func TestRenderScanRoutesHTMLWithEveryFieldPlumbed(t *testing.T) {
	res := scan.Result{
		PartialReads: []scan.ReadFailure{{Resource: "horizontalpodautoscalers", Reason: "forbidden"}},
		Inventory: inventory.Result{Workloads: []inventory.Workload{{
			Namespace: "shop", Kind: "Deployment", Name: "web", Desired: 1, Ready: 0,
			Status: "Degraded", Image: "busybox:1.36",
			Findings: []diagnose.Finding{{
				Pod: "shop/web", Issue: "CrashLoopBackOff", Reason: "container repeatedly crashes",
			}},
		}}},
	}
	in := resultInput(res)
	in.Now = time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)

	var buf bytes.Buffer
	if err := renderScan(&buf, "html", in, res, "shop"); err != nil {
		t.Fatalf("renderScan html: %v", err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "<!doctype html>") {
		t.Errorf("renderScan did not produce an HTML document, got: %.40q", got)
	}
	// Namespace reached htmlreport.Input.
	if !strings.Contains(got, "namespace shop") {
		t.Error("the -n value did not reach the document header")
	}
	// findings.Flatten reached htmlreport.Input.
	if !strings.Contains(got, "CrashLoopBackOff") {
		t.Error("the flattened findings did not reach the document")
	}
	// scan.Result.PartialReads reached htmlreport.Input.
	if !strings.Contains(got, "horizontalpodautoscalers") {
		t.Error("the partial reads did not reach the blind-spots block")
	}
	// report.Input reached htmlreport.Input.
	if !strings.Contains(got, "busybox:1.36") {
		t.Error("the report.Input workloads did not reach the inventory section")
	}
}

// TestRenderScanLeavesTextAndJSONOnTheOldPath: the new branch must not change
// what the two shipped formats emit.
func TestRenderScanLeavesTextAndJSONOnTheOldPath(t *testing.T) {
	res := scan.Result{}
	in := resultInput(res)
	in.Now = time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)

	for _, format := range []string{"text", "json"} {
		var viaHelper, viaReport bytes.Buffer
		if err := renderScan(&viaHelper, format, in, res, ""); err != nil {
			t.Fatalf("renderScan %s: %v", format, err)
		}
		if err := report.PrintInventory(in, format, &viaReport); err != nil {
			t.Fatalf("PrintInventory %s: %v", format, err)
		}
		if viaHelper.String() != viaReport.String() {
			t.Errorf("renderScan changed the %s output", format)
		}
	}
}
```

`main_test.go` already imports everything these tests need except one: add

```go
	"github.com/imantaba/kubeagent/internal/report"
```

to its import block, in the `github.com/imantaba/kubeagent/internal/...` group, alphabetically between `remediate` and `rolloutwait`. `bytes`, `time`, `strings`, `path/filepath`, `diagnose`, `inventory`, and `scan` are already imported.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run 'HTMLOutputFormat|RenderScan|UnknownOutputFormatAndNamesHTML' -v
```

Expected: FAIL — the package does not compile, naming `undefined: renderScan`.

- [ ] **Step 3: Add the `renderScan` helper**

In `main.go`, immediately above `func resultInput(res scan.Result) report.Input` (currently `:399`), add:

```go
// renderScan writes the scan output in the requested format. It is its own
// function — rather than an inline branch at the call site — for the same reason
// gateScanOptions is: so a test can drive the exact values runScan uses without a
// live cluster. Inline, the only way to reach the HTML path would be to connect
// to a cluster, and a field that silently never reached htmlreport.Input would
// ship unnoticed.
func renderScan(w io.Writer, format string, in report.Input, res scan.Result, namespace string) error {
	if format == "html" {
		return htmlreport.Render(w, htmlreport.Input{
			Report:    in,
			Findings:  findings.Flatten(res),
			Blind:     res.PartialReads,
			Namespace: namespace,
			Version:   version,
		})
	}
	return report.PrintInventory(in, format, w)
}
```

Add `"github.com/imantaba/kubeagent/internal/htmlreport"` to `main.go`'s import block. `io`, `internal/findings`, `internal/report`, and `internal/scan` are already imported.

- [ ] **Step 4: Route the call site**

In `main.go`, replace (currently `:369-371`):

```go
	if err := report.PrintInventory(in, *output, os.Stdout); err != nil {
		return err
	}
```

with:

```go
	if err := renderScan(os.Stdout, *output, in, res, namespace); err != nil {
		return err
	}
```

Confirm the surrounding function has `res` and `namespace` in scope at that point — `resultInput(res)` is called earlier in the same function to build `in`, and `namespace` is the variable bound by `fs.StringVar(&namespace, "n", …)` at `:177`.

- [ ] **Step 5: Widen the format validation and the flag help**

In `main.go`, replace (currently `:183-185`):

```go
	if *output != "text" && *output != "json" {
		return fmt.Errorf("unknown output format %q (want text or json)", *output)
	}
```

with:

```go
	if *output != "text" && *output != "json" && *output != "html" {
		return fmt.Errorf("unknown output format %q (want text, json or html)", *output)
	}
```

And replace the flag declaration at `:145`:

```go
	output := fs.String("output", "text", "output format: text | json")
```

with:

```go
	output := fs.String("output", "text", "output format: text | json | html")
```

- [ ] **Step 6: Update the usage string**

In `main.go`, in the usage string at `:139`, replace the **first** occurrence of:

```text
[--output text|json]
```

with:

```text
[--output text|json|html]
```

That first occurrence is the `scan` clause. Leave the `gate` clause's `[--output text|json|sarif]` exactly as it is — `gate` does not gain an HTML format.

- [ ] **Step 7: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run 'HTMLOutputFormat|RenderScan|UnknownOutputFormat|UsageMentions' -v
```

Expected: PASS.

- [ ] **Step 8: Verify end to end against the real binary**

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o /tmp/kubeagent-htmlcheck .
/tmp/kubeagent-htmlcheck scan --output bogus 2>&1 | head -1
/tmp/kubeagent-htmlcheck 2>&1 | head -1 | grep -o -- '--output text|json|html'
rm -f /tmp/kubeagent-htmlcheck
```

Expected: the first command prints `unknown output format "bogus" (want text, json or html)`; the second prints `--output text|json|html`.

- [ ] **Step 9: Run the full suite and confirm the text golden is untouched**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./...
git diff --exit-code internal/report/testdata/golden-scan.txt && echo "golden-scan.txt byte-identical"
git diff --exit-code go.mod go.sum && echo "go.mod/go.sum unchanged"
```

Expected: every package passes, then both confirmation lines print.

- [ ] **Step 10: Commit**

```bash
git add main.go main_test.go
git commit -m "feat(scan): accept --output html

--output grows a third value; text and json are byte-for-byte unchanged,
and scan's exit code is unchanged in both directions.

The branch lives in a renderScan helper rather than inline at the call
site, for the same reason gateScanOptions does: a test can then drive the
exact values runScan uses without a live cluster. Inline, the only way to
reach the HTML path would be to connect to a cluster, and a field that
silently never reached htmlreport.Input would ship unnoticed.

gate keeps text|json|sarif -- its job is a verdict, not a document."
```

---

### Task 5: Documentation

**Files:**
- Create: `website/docs/features/html-report.md`
- Modify: `website/mkdocs.yml:70` (nav, after the CI/CD gate entry)
- Modify: `README.md:25` area (feature bullet) and the features section around `:409-421`
- Modify: `CHANGELOG.md` (`## [Unreleased]`)
- Modify: `website/docs/roadmap.md` (Theme G status at `:451` and `:468`)

**Interfaces:** none — documentation only.

- [ ] **Step 1: Capture real output to quote**

Every example in the page must be actual binary output, not written by hand.

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o /tmp/kubeagent-doc .
/tmp/kubeagent-doc scan --output bogus 2>&1 | head -1
```

Expected: `unknown output format "bogus" (want text, json or html)`. Copy that line verbatim into the page below; do not retype it.

- [ ] **Step 2: Write the feature page**

Create `website/docs/features/html-report.md`:

````markdown
# HTML report

`kubeagent scan --output html` writes one self-contained HTML document to
stdout: the artifact you attach to an incident ticket, paste into a pull
request, or mail to a colleague who has no cluster access.

```bash
kubeagent scan --output html > incident-4821.html
kubeagent scan -n prod --output html > prod.html
```

`--output` accepts `text`, `json`, or `html`. Anything else is rejected before
kubeagent touches the network:

```console
$ kubeagent scan --output bogus
unknown output format "bogus" (want text, json or html)
```

## What the document contains

- **A header** — kubeagent version, the namespace scope (or "all namespaces"),
  the generation timestamp, and the finding counts by severity.
- **Blind spots** — whatever kubeagent could not read, whenever there is any.
- **Findings** — every finding, highest severity first, with a severity filter.
- **Detail** — cluster health, the workload inventory, and the `--explain`
  narrative when you ran with `--explain` or `--investigate`, each in a
  collapsed section so the findings stay at the top.

The opt-in advisory sections (`--capacity`, `--drift`, `--operators`,
`--certs`, and the rest) are not in the document yet. Their *findings* are —
`--output html` renders the same finding set the text report ranks — but their
detailed views only appear in `--output text` and `--output json`.

## What the document deliberately does not contain

**No cluster identity.** No context name, no API server URL, no kubeconfig
path. A context name is not safe by default — in the wild they carry
`arn:aws:eks:eu-west-1:<account>:cluster/prod` or
`admin@prod-db.internal.corp` — and this file is meant to be forwarded.
Whoever shares it names the cluster in the ticket. This is the same rule
[`kubeagent gate`](ci-gate.md) follows for its verdict, so both shareable
artifacts behave identically.

**No JavaScript.** The severity filter is pure CSS. There is no external
stylesheet, font, or image either, so the file opens offline and renders
under a strict Content-Security-Policy — which is what artifact previews,
sandboxed mail clients, and corporate proxies enforce.

## Blind spots are not optional

If kubeagent could not list a resource — RBAC denied it, a CRD is absent, the
API timed out — the document says so in its own block, above the findings:

> kubeagent could not read the following, so the findings below are incomplete.

A shared report that silently omits what it could not see is the same
green-when-blind failure [CI gate mode](ci-gate.md) exists to prevent, and a
rendered document is easier to over-trust than an exit code.

## Escaping

Container termination messages, event reasons, and image-pull errors are
free-form strings the cluster controls, and they land verbatim in this
document. kubeagent renders it with Go's `html/template`, whose contextual
auto-escaping neutralizes them. A pod whose crash message contains markup
produces a report that displays that message as text.

## Notes

- `scan`'s exit code is unchanged: still `0` on an unhealthy cluster. Gating on
  health is [`kubeagent gate`](ci-gate.md).
- `gate` has no `--output html`. Its job is a verdict, not a document; its
  machine-readable surface is JSON and SARIF.
- Two runs against an unchanged cluster differ only in the header timestamp, so
  reports from the same incident diff cleanly.
- `--output html --fix` interleaves the remediation transcript after the
  document, exactly as `--output json --fix` does. Redirect the document to a
  file first, or run `--fix` separately.
````

- [ ] **Step 3: Add the nav entry**

In `website/mkdocs.yml`, after line 70 (`      - CI/CD gate: features/ci-gate.md`), add:

```yaml
      - HTML report: features/html-report.md
```

- [ ] **Step 4: Add the README feature bullet**

In `README.md`, immediately after the CI/CD gate bullet at `:25`, add:

```markdown
- 📄 **Shareable HTML report** — `kubeagent scan --output html` writes one self-contained, script-free HTML file carrying the findings, the blind spots, and the detail — with no cluster identity in it, so it is safe to forward.
```

Then, after the `### CI/CD gate` subsection that ends around `:421`, add:

````markdown
### HTML report

```bash
kubeagent scan --output html > incident-4821.html
```

One self-contained file: no JavaScript, no external stylesheet or font, and no
context name, server URL, or kubeconfig path — so it opens offline, renders
under a strict CSP, and is safe to attach to a ticket. Carries the findings,
whatever kubeagent could not read, and collapsed detail sections. See
[HTML report](website/docs/features/html-report.md).
````

- [ ] **Step 5: Add the CHANGELOG entry**

`CHANGELOG.md:8` is `## [Unreleased]` and is currently empty. Replace it with:

```markdown
## [Unreleased]

### Added

- **Shareable HTML report** — `kubeagent scan --output html` renders one self-contained HTML document: header with version, namespace scope, timestamp and severity tally; a blind-spots block whenever a read failed; the full findings table with a pure-CSS severity filter; and collapsed detail sections for cluster health, the workload inventory, and the `--explain` narrative. The document carries no JavaScript and no external stylesheet, font, or image, so it opens offline and renders under a strict Content-Security-Policy — and it carries no cluster identity (no context name, no API server URL, no kubeconfig path), the same rule `kubeagent gate`'s verdict follows. `--output text` and `--output json` are byte-for-byte unchanged, and `scan`'s exit code is unchanged in both directions. New leaf package `internal/htmlreport`.
```

- [ ] **Step 6: Update the roadmap**

In `website/docs/roadmap.md`, replace the two lines at `:451-452`:

```markdown
  verify, SARIF, exit codes); an interactive TUI and a shareable HTML report
  remain ahead.
```

with:

```markdown
  verify, SARIF, exit codes); and a **shareable HTML report** (shipped, `scan
  --output html`). An interactive TUI remains ahead.
```

Then in the `**v0.5x**` milestone row at `:468`, replace:

```text
interactive TUI + HTML report; optional in-cluster dashboard
```

with:

```text
**shareable HTML report** (shipped, `scan --output html`); interactive TUI; optional in-cluster dashboard
```

- [ ] **Step 7: Verify the docs build and every internal link resolves**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
(cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml)
```

Expected: `Documentation built`, exit 0, and **no `WARNING` line naming `html-report.md`**. The red "Material for MkDocs 2.0" banner is cosmetic — judge by exit code and the absence of page warnings.

- [ ] **Step 8: Verify the quoted example is real**

```bash
export PATH=$PATH:/usr/local/go/bin
diff <(/tmp/kubeagent-doc scan --output bogus 2>&1 | head -1) \
     <(grep -A1 '^\$ kubeagent scan --output bogus$' website/docs/features/html-report.md | tail -1) \
  && echo "quoted output matches the binary"
rm -f /tmp/kubeagent-doc
```

Expected: `quoted output matches the binary`.

- [ ] **Step 9: Scan the new docs for leaked identity**

```bash
grep -nE '([0-9]{1,3}\.){3}[0-9]{1,3}|/home/|\.kube/config' \
  website/docs/features/html-report.md README.md CHANGELOG.md \
  || echo "no IPs, home paths, or kubeconfig paths in the new docs"
```

Expected: `no IPs, home paths, or kubeconfig paths in the new docs`. The `arn:aws:eks:eu-west-1:<account>:cluster/prod` example in the feature page uses the literal `<account>` placeholder and is intentional — it must stay a placeholder, never a real account ID.

- [ ] **Step 10: Run the full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./...
```

Expected: every package `ok` or `no test files`.

- [ ] **Step 11: Commit**

```bash
git add website/docs/features/html-report.md website/mkdocs.yml README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "docs: document the shareable HTML report

Feature page, mkdocs nav, README bullet and section, CHANGELOG entry, and
the Theme G roadmap status.

Names what the document deliberately omits as prominently as what it
carries: no cluster identity, because the file is meant to be forwarded,
and no JavaScript, because the environments that display it block script.
Also names the opt-in advisory sections as not-yet-rendered, so nobody
reads a missing --capacity section as a missing problem."
```

---

## Verification Before Handoff

After Task 5, before the whole-branch review:

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
git diff --exit-code eb70b0d -- internal/report/testdata/golden-scan.txt && echo "text golden byte-identical"
git diff --exit-code eb70b0d -- go.mod go.sum && echo "no new dependencies"
grep -rn "text/template" internal/htmlreport/ && echo "FAIL: text/template found" || echo "html/template only"
grep -rn "client-go\|internal/explain\|internal/remediate\|internal/investigate" internal/htmlreport/*.go \
  && echo "FAIL: forbidden import" || echo "no cluster or LLM imports"
```

Every line must confirm. A failure here is a Critical finding, not a Minor.

Then the branch goes to the opus whole-branch review, and after that to the
release gate. This slice touches no `internal/collect`, no `internal/cluster`,
no RBAC, no `nodes/proxy`, no `--fix`, no watch daemon, and no Helm template, so
the gate is the **lightweight Kind smoke**, not the full chaos suite: bring up a
two-node Kind cluster, render a real report from it, confirm it parses as HTML,
grep the output for the gate's own kubeconfig path, context name, and API server
URL, and confirm `--output text` and `--output json` are unchanged against the
same cluster. That grep is why the fixtures are path- and URL-free: on real data
it is the only check that can tell a renderer leak from a legitimate registry URL
in an `ErrImagePull` reason.
