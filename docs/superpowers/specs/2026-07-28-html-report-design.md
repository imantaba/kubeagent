# Shareable HTML report — design

**Status:** approved
**Theme:** G · Interfaces & adoption (slice 4a)
**Ships as:** v0.66.0 (MINOR)
**Branch:** `html-report`

## Goal

`kubeagent scan --output html` writes one self-contained HTML document to
stdout: the artifact you attach to an incident ticket, paste into a PR, or mail
to a colleague who does not have cluster access.

## Scope

The roadmap pairs "an interactive TUI and a shareable HTML report". This spec
covers **only the HTML report**. The TUI is deferred to its own slice with its
own spec: it needs either a new UI dependency in a deliberately small `go.mod`
or hand-rolled raw-mode ANSI, an input event loop, and it would be a third
documented long-lived-process exception beside `internal/watch` and
`internal/mcp`. Those are different problems with a different test strategy, and
bundling them would delay a low-risk deliverable behind a high-risk one.

## Surface

`--output` grows a third value: `text | json | html`. Nothing else changes.

```bash
kubeagent scan --output html > incident-4821.html
kubeagent scan -n prod --output html > prod.html
```

Rejected alternatives:

- **`--html-out FILE`** — gives `scan` a second output channel where it has
  exactly one today, and puts a filesystem path in a flag (unwritable directory,
  existing file, symlink) for something `>` already does.
- **A `kubeagent report` subcommand** — would duplicate every one of `scan`'s
  flags (`--namespace` and the whole opt-in advisory set) to perform the
  collection `scan` already performs.

**`gate` does not get `--output html`.** Its job is a verdict, not a document,
and SARIF already covers its machine-readable surface.

**`scan`'s exit code is unchanged in both directions:** still `0` on an unhealthy
cluster, still non-zero on a real error. Gating on health is `kubeagent gate`.

## Architecture

A new leaf package, `internal/htmlreport`.

```go
package htmlreport

// Input is everything the document renders. It is a struct rather than a
// parameter list because the fields come from different layers.
type Input struct {
	Report    report.Input       // the presentation view scan already builds
	Findings  []findings.Finding // severity-ranked, from findings.Flatten
	Blind     []scan.ReadFailure // scan.Result.PartialReads
	Namespace string             // -n value; "" means all namespaces
	Version   string             // main.version, for the header
}

func Render(w io.Writer, in Input) error
```

### Why a new package rather than `case "html"` in `PrintInventory`

`internal/report` does **not** import `internal/scan`. The conversion lives in
`main.go` (`resultInput`, `main.go:399`), which keeps presentation decoupled from
collection. The findings-first layout needs `[]findings.Finding`, and
`internal/findings` imports `internal/scan` — so routing HTML through
`report.PrintInventory` would drag `scan` into a package that today knows nothing
about it, to serve one output format.

`internal/htmlreport` imports `report`, `findings`, and `scan`, and nothing
imports it but `main`. No cycle, `report`'s boundary survives, and
`internal/report/report.go` does not grow past its current 1611 lines.

This makes `internal/findings` its second consumer, which is the direction the
roadmap wants for the severity model. It is **not** a migration: `internal/mcp`,
`internal/watch`, and `internal/report` keep their existing per-surface severity
handling, so the MCP tool payloads shipped in v0.63.0 are untouched.

### Call site

`main.go` already holds both values where `PrintInventory` is called today
(`main.go:369`): `res` is the `scan.Result`, `in` is the `report.Input`. The
existing statement is `if err := report.PrintInventory(in, *output, os.Stdout);
err != nil { return err }`; it becomes:

```go
render := func() error {
	if *output == "html" {
		return htmlreport.Render(os.Stdout, htmlreport.Input{
			Report:    in,
			Findings:  findings.Flatten(res),
			Blind:     res.PartialReads,
			Namespace: namespace,
			Version:   version,
		})
	}
	return report.PrintInventory(in, *output, os.Stdout)
}
if err := render(); err != nil {
	return err
}
```

The `--output` validation (`main.go:183`) and the `scan` usage line
(`main.go:139`) both grow the third value.

`--fix` and `--rollback` write their own progress to `os.Stdout` after this
statement, so `--output html --fix` interleaves plain text after the document —
exactly as `--output json --fix` does today. That is pre-existing behavior for
the machine-readable formats and this slice does not change it in either
direction: no new rejection, no new suppression.

## The document

One file. `<!doctype html>`, one inline `<style>`, **no `<script>`**, no external
font, image, or stylesheet. It opens offline and survives any
Content-Security-Policy — GitHub artifact previews, sandboxed mail clients, and
corporate proxies block inline script; none block inline CSS.

### Header

kubeagent version · namespace scope (or "all namespaces") · generation timestamp
· finding counts by severity.

The scope comes from `Input.Namespace` — its own field because `report.Input`
carries no namespace, and `ClusterHealth.ScopeNote` is not a substitute: it
names no namespace and is empty for `-n kube-system`
(`clusterhealth.go:139`). A namespace name is scope, not cluster identity: it
says which slice was examined, not which cluster or how to reach it.

**No cluster identity.** No context name, no API server URL, no kubeconfig path.
This is the same rule the v0.65.0 gate verdict follows, so both shareable
artifacts behave identically. A context name is not safe by default: in the wild
they carry `arn:aws:eks:eu-west-1:<account>:cluster/prod` or
`admin@prod-db.internal.corp`. Whoever shares the file names the cluster in the
ticket.

### Findings table

Columns: severity, kind, namespace, name, issue, reason, owner (blank when the
finding has none).

**`Render` does not sort.** `findings.Flatten` already ends in `Sort`
(`findings.go:176`), whose total order is highest severity first, then
namespace, kind, name, issue — chosen so an unchanged cluster renders
byte-identical output and two runs diff cleanly. The template renders the slice
in the order it is handed, and the golden test locks that order. Re-sorting in
`htmlreport` would be a second definition of the same order, free to drift.

The severity filter is **pure CSS** — radio inputs plus `:checked` sibling
selectors:

```css
#f-crit:checked ~ table .warning,
#f-crit:checked ~ table .info { display: none }
```

No JavaScript, and no persisted state: it is a stylesheet, not an application.

### Blind spots

`scan.Result.PartialReads` renders in its own block whenever non-empty, naming
each resource and reason.

This block is why `Input` carries `Blind` separately: `report.Input` has no
`PartialReads` field, and `--output text` does not surface partial reads today.
Adding them to the text report would change
`internal/report/testdata/golden-scan.txt` and is out of scope here — but a
*shared document* must carry them. A report that silently omits what kubeagent
could not read is the same green-when-blind failure `kubeagent gate` exists to
prevent, and a document is easier to over-trust than an exit code.

### Detail sections

Three, each inside a native `<details>`/`<summary>`, collapsed by default so the
findings stay above the fold:

1. **Cluster health** — verdict, nodes ready/total, node issues, system issues,
   scope note, from `in.Report.Cluster`.
2. **Workload inventory** — `in.Report.Result.Workloads`: namespace, kind, name,
   ready/desired, status, image, root cause.
3. **Explanation** — `in.Report.Explanation`, rendered only when non-empty (so
   only under `--explain`; `--investigate` fills the separate `Investigation`
   field, which stays out of scope below). One string, high value in a shared
   document, near-zero cost.

**The opt-in advisory sections are deliberately out of scope for this slice.**
`report.Input` carries seventeen more (`Resources`, `Platform`, `NodeReserve`,
`PVCReclaim`, `DiskUsage`, `SecurityIssues`, `KubeletHealth`, `ControlPlane`,
`DNS`, `Certificates`, `Operators`, `GitOps`, `Capacity`,
`CredentialWarnings`, `Investigation`, `InvestigationConsulted`,
`RemediationPlan`), each with its own nested structure. Rendering all of them
is most of `report.go`'s 1611 lines re-expressed in HTML plus a golden fixture
to match, which would hold a small, high-value deliverable behind a large one.

Nothing is silently dropped by this: every *finding* those detectors produce
still reaches the document, because `findings.Flatten` already folds
`ServiceIssues`, `IngressIssues`, `PVCIssues`, `StuckTerminating`, `PDBIssues`,
`HPAIssues`, `WebhookIssues`, and `QuotaIssues` into the findings table. What
is deferred is the *advisory detail* behind the opt-in flags. Adding a section
later is additive and breaks no contract.

### Empty cluster

No findings renders an explicit "nothing found" state, not a headless table.

## Escaping is a security property, not formatting

`html/template`, never `text/template`, for contextual auto-escaping.

Kubernetes object names are DNS-1123 and harmless. Container termination
messages, event reasons, and image-pull errors are not: they are free-form
strings that land verbatim in this document via `findings.Finding.Reason`. The
`ErrImagePull` text captured during the v0.65.0 release gate already carried
quotes, colons, and nested quoting. A document mailed to a colleague is the
wrong place to render unescaped cluster-controlled text.

The escaping test is load-bearing: it must fail if someone swaps the import.

## Determinism

The timestamp reads `in.Report.Now`, already documented as an injectable clock
("main sets `time.Now()`; zero → wall-clock", `report.go:150`). `Render` follows
that same contract: a zero `Now` falls back to wall-clock, so the header never
renders a year-1 timestamp. The golden test sets a fixed instant, so the fixture
has no time-varying bytes and the comparison is plain bytes.

Nothing else in the document varies between two runs against an unchanged
cluster. That is a property worth keeping: two reports from the same incident,
taken minutes apart, should diff to exactly what changed.

## Testing

- **Golden test** — `internal/htmlreport/testdata/golden-report.html`, fixed
  `Now`, regenerated with `-update`, the same idiom as
  `internal/report/golden_test.go`.
- **Escaping** — a fixture finding whose `Reason` contains
  `<script>alert(1)</script>` and a raw `"`. Asserts the rendered bytes contain
  the escaped entities and do **not** contain the raw tag.
- **Severity ordering** — Critical before Warning before Info.
- **Blind spots** — the block is present when `PartialReads` is non-empty and
  absent when empty.
- **Empty cluster** — renders the "nothing found" state.
- **No leaked identity** — renders with a fixture and asserts the document
  contains none of: a kubeconfig path (`/home`, `/tmp`, `.kube/config`), a
  context name, or an API server URL.

  The test works by controlling its input: the fixture is deliberately free of
  paths and URLs, so **any** `://`, `/home`, or `.kube` in the output came from
  the renderer, not from the data. That makes the assertion non-vacuous — it
  fails the moment someone adds a server URL or kubeconfig path to the header.

  A blanket "no URL anywhere" rule would be wrong on real data: an
  `ErrImagePull` reason legitimately carries `https://index.docker.io/v2/...`.
  That is cluster-supplied content in a finding, not kubeagent disclosing how it
  connected. Hence the release gate's version of this check (below) greps for
  its own kubeconfig path, context name, and server URL specifically, rather
  than for `://`.
- `internal/report/testdata/golden-scan.txt` stays byte-identical. The text and
  JSON paths are not touched.

## Failure handling

The template is parsed once at package scope with `template.Must`, so a
malformed template fails at package init and in CI — never at an operator's
terminal.

`Render` returns the write error on a closed pipe (`kubeagent scan --output html
| head`), and `runScan` returns it through the path it already uses.

## Non-goals

No dark mode. No print stylesheet. No charts. No embedded logo. No multi-cluster
aggregation. No `gate --output html`. No migration of `mcp`/`watch`/`report` onto
`internal/findings`. No partial reads in the text report. No opt-in advisory
sections in the document (see Detail sections).

## Invariants this slice must not break

- **READ-ONLY.** Rendering performs no cluster calls at all; it is a pure
  function over data `scan` already collected.
- No LLM calls on the render path. `internal/htmlreport` must never import
  `internal/explain` or `internal/investigate`.
- Standard-library `flag` only. No Cobra. **No new entries in `go.mod` or
  `go.sum`** — `html/template` is stdlib.
- Sequential. No goroutines.
- `internal/report/testdata/golden-scan.txt` byte-identical.
- `scan`, `watch`, `mcp`, and `gate` behavior unchanged.
- URLs and kubeconfig paths are credentials, and never appear in the document.
- No secrets, credentials, private IPs, or internal hostnames — in the document,
  the fixtures, or the docs. Documentation IPs are RFC 5737 only.
- No `Co-Authored-By` trailer and no AI attribution anywhere.

## Release gate

**Not the chaos suite.** This slice touches no `internal/collect`,
`internal/cluster`, RBAC, `--fix`, watch daemon, or Helm template.

A Kind smoke instead:

1. Render a real report from a live cluster carrying genuine findings.
2. Confirm it parses as HTML.
3. Grep the actual rendered bytes for the gate's own kubeconfig path, its
   context name, and its API server URL — zero hits each. Not a blanket
   `https://` grep, for the reason given under Testing.
4. Confirm `--output text` and `--output json` are unchanged on the same cluster.

## Documentation

A feature page under `website/docs/features/`, mkdocs nav, README, CHANGELOG
`[Unreleased]`, and the roadmap (Theme G: the HTML report ships, the TUI and the
optional in-cluster dashboard remain).
