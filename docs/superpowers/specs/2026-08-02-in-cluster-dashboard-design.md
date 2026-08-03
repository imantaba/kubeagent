# In-cluster dashboard — design

**Status:** approved, ready for an implementation plan
**Target release:** v1.2.0 (MINOR — new user-facing feature)
**Roadmap:** Theme G, final unshipped item. Shipping this completes Theme G.

## Problem

The watch daemon knows, continuously, which objects in a cluster are broken, how
long each has been broken, which have recovered and how long that took. Today
that knowledge leaves the process in three machine-shaped forms: Prometheus text
at `/metrics`, JSON at `/issues` and `/explanations`, and outbound alerts. An
operator who wants to *look* at it needs Grafana, a `curl | jq`, or a terminal
with a kubeconfig.

`kubeagent tui` covers the last case, but it runs on a laptop against a
kubeconfig and performs its own reads. It is not the thing you open when someone
asks "what is broken in prod right now" and you want to hand them a URL.

The roadmap has carried this as "an optional in-cluster dashboard" since Theme G
was written. This is that.

## Scope

The dashboard is **a face on daemon state**. It renders what the daemon already
tracks and nothing else:

- tracked incidents, active and resolved, with firing duration and
  time-to-resolution
- per-cluster reachability, so an empty list is distinguishable from a blind one
- the aggregate counters the daemon already keeps
- SLO burn, when `--slo-target` is set
- on-incident explanations, when `--explain` is set

It performs **no cluster read of its own**. Every value on the page comes from a
snapshot the daemon already computed for `/metrics` and `/issues`. This is the
property that keeps `internal/rbacprofile`'s Feature table — and therefore every
generated RBAC manifest and the chart ClusterRole — completely untouched.

### Explicitly out of scope

- **Browsing the cluster.** No namespace list, no workload drill-down, no pod
  detail, no events. That is `kubeagent tui`'s job and it would require
  request-time cluster reads in a process that today reads only on informer
  events and the heartbeat.
- **Blind spots.** `scan.Result.PartialReads` is a field the daemon never
  carries into `clusterSnapshot`, so the dashboard cannot show blind spots
  without new daemon work. Surfacing them is a separate change with its own
  design.
- **Any write.** No buttons, no forms, no actions. See "Invariants" below.
- **Triggering explanations.** The page renders explanations the incident
  pipeline already computed. A dashboard request never causes a model call.
- **Authentication.** kubeagent ships none. See "Exposure and authentication".

## Decisions

Five design questions were settled before this document was written. Recording
them with their reasoning, because each closes off alternatives an implementer
might otherwise reopen.

| Question | Decision | Why |
|---|---|---|
| Scope | Face on daemon state | Zero new cluster reads, so RBAC is untouched. The daemon is the one process already holding this state live. |
| Listener | A path on the existing `:8080` mux | The page renders a strict subset of what unauthenticated `/issues` already serves on that same port. A second listener would guard nothing new while adding a Service port, a readiness story and chart surface. |
| Refresh | Server-rendered per request, `<meta http-equiv="refresh">`, zero JavaScript | Keeps `html/template`'s contextual escaping as the single escape boundary — there is no DOM-building code to get wrong. Survives a strict CSP. Daemon state moves on a 2s debounce and a 60s heartbeat, so a periodic reload loses nothing. |
| Explanations | Rendered read-only from the existing store | Costs nothing, makes no model call, and an operator paying for explanations should see them where they look at incidents. |
| Authentication | None in kubeagent, documented | The daemon's whole security story is that it holds no credential beyond its own read-only ServiceAccount. Adding a password store contradicts that, for a page whose data `/issues` already serves unauthenticated on the same port. |

## Architecture

### `internal/dashboard` (new package)

A pure renderer, modelled directly on `internal/htmlreport`.

```go
Render(w io.Writer, in Input) error
```

- No client, no context, no lock, no I/O beyond the writer it is handed.
- The template is embedded with `//go:embed` and parsed once at package init with
  `template.Must`, so a malformed template fails in CI rather than in front of an
  operator mid-incident. This is the same choice `internal/htmlreport` makes.
- **The package imports nothing from kubeagent.** Only `embed`, `html/template`,
  `io`, `time`, `sort` and `fmt`. This puts it in the same class as
  `internal/jsonschema` and makes reaching `internal/remediate` or
  `internal/explain` structurally impossible rather than a rule someone has to
  remember. A source-level test enforces it.
- It therefore defines its own view types. Reusing `watch.IssuesReport` would
  create an import cycle, since `internal/watch` is the caller.

Files:

- `internal/dashboard/dashboard.go` — `Input`, `Render`, and the view-building
  helpers that turn `Input` into the flat shape the template ranges over. Every
  decision lives in Go; the template ranges and branches and does nothing else.
- `internal/dashboard/dashboard.html.tmpl` — the embedded template.
- `internal/dashboard/testdata/golden-dashboard.html` — the golden snapshot.

### `internal/watch` (modified)

Two additions in `internal/watch/metrics.go`:

1. `dashboardInput()` — builds a `dashboard.Input` under the same `m.mu.RLock()`
   that `issuesJSON` already takes.
2. One `mux.HandleFunc("/dashboard", …)` in `handler()`, **registered only when
   the dashboard is enabled**. A disabled daemon 404s the path through the mux's
   own not-found handling rather than serving a switched-off page.

The `metrics` struct gains one field recording whether the dashboard is enabled,
set from config the same way `m.explain.Enabled` already is.

### Data flow

```text
informers / heartbeat → evaluation → clusterSnapshot          (all existing)

GET /dashboard
  → RLock
  → copy snapshot into dashboard.Input
  → RUnlock
  → dashboard.Render into a bytes.Buffer
  → write headers, copy buffer to the ResponseWriter
```

Three properties of that sequence are load-bearing:

- **The `Input` is built by copying.** Fresh slices, never aliasing the
  snapshot's backing arrays — the same discipline `issueViews` already follows.
- **`Render` runs after `RUnlock`.** Template execution never holds the daemon's
  lock while a worker waits to swap a snapshot. It is also what makes
  `go test -race ./...` honest: the renderer only ever sees a value no other
  goroutine can reach.
- **Rendering targets a buffer, not the `ResponseWriter`.** A template failure
  mid-execution would otherwise land after the 200 header and produce a
  truncated page. Buffering turns it into a clean `500`. The cost is one page in
  memory, which for this data is negligible. The buffer belongs to the handler
  in `internal/watch`, not to `Render`, which keeps taking a plain `io.Writer`.

## The page

Layout, top to bottom:

1. **Header** — kubeagent version, generation time in UTC, refresh interval.
2. **Cluster strip** — one row per watched cluster: name, up/down, last scan,
   error. This band is load-bearing, not decoration: an empty incident list from
   an unreachable cluster looks identical to a healthy one, which is exactly why
   `ClusterView` exists. A down cluster reads loud.
3. **Active incidents** — kind, namespace/name, issue, firing-for, firings,
   flapping badge. The cluster column appears only when more than one cluster is
   watched.
4. **Resolved incidents** — the same columns plus resolved-at and
   time-to-resolution.
5. **Summary tiles** — new / resolved / flapping / dropped totals, and mean time
   to resolution computed as `ResolutionSecondsSum ÷ ResolutionSecondsCount`,
   rendered as `—` when the count is zero rather than `NaN`.
6. **SLO** — present only when `sloSnapshot.Enabled`: per cluster target,
   fast-window and slow-window availability, burn rate, error budget remaining,
   and coverage. Coverage below 0.6 is annotated as suppressing the burn alert,
   matching what the metric help text already documents.
7. **Explanations** — present only when `--explain` is on: object, issues,
   model, explained-at, and text.

### Ordering

Ordering is a **total order**, not a preference. Active incidents sort by
longest-firing first, then cluster, kind, namespace, name, issue as tiebreakers.
Resolved incidents sort by most-recently-resolved first with the same
tiebreakers. Any partial order lets equal rows swap places between renders,
which on a 30-second reload is genuinely unusable — and it is what the
determinism test actually checks.

### Empty and starting states

Before the first evaluation completes the page still renders; the cluster strip
says the cluster has not been scanned yet. The handler never returns 503. A
daemon that is starting up should say so, not go dark.

"No active incidents" and "no cluster reporting" are distinguishable, because
the cluster strip is always present regardless of the incident lists.

## Security

### Escaping

`html/template`'s contextual auto-escaping is the single escape boundary.
`template.HTML`, `template.JS` and `template.URL` appear **nowhere** in the
package — those conversions are the only way to defeat it, so a source-level
test asserts their absence rather than leaving it to review.

Explanation text is model output and gets exactly the same treatment: escaped,
laid out with `white-space: pre-wrap`, and never parsed as markdown, since
parsing means unescaping.

Untrusted API text arrives already sanitized. `internal/safetext.Line` runs at
ingress in the detectors, per the project invariant, and `ClusterView.Error` is
already `redact.Error(err)` at the point it enters `clusterSnapshot`. The
dashboard adds HTML escaping on top; it does not re-sanitize, and it must not
become a second sanitization site.

### Inert document

No `<script>`, no external stylesheet, font, or image. Inline CSS only. The page
passes a strict Content-Security-Policy and performs no third-party fetch. The
`<meta http-equiv="refresh">` carries an interval and no URL, so the document
emits no URL at all — which is what keeps it inside the project's rule that
nothing kubeagent emits carries more than `scheme://host`.

### Cluster identity

One deliberate divergence from `internal/htmlreport`. That renderer strips all
cluster identity because its output is meant to be forwarded to people outside
the cluster. The dashboard keeps cluster **names**: they are operator-chosen
labels already present in `/issues` and in every metric series, and a
multicluster dashboard that will not name its clusters is useless.

API server URLs, kubeconfig paths and kubeconfig context names still never
appear. `redact.Error` at ingress covers the one field that could otherwise
carry them.

### Exposure and authentication

kubeagent implements no authentication for the dashboard. This is a decision,
not an omission, and the docs state it plainly.

The posture is identical to what `/metrics` and `/issues` already have on that
port: unauthenticated, `ClusterIP` by default. The documentation points at the
layers that do this properly — an Ingress with basic-auth or an oauth2-proxy in
front, or a NetworkPolicy keeping the port cluster-internal — and states that
the daemon terminates no TLS.

The rationale belongs in the docs too: the daemon's entire security story is
that it holds no credential beyond its own read-only ServiceAccount. A password
or token store inside kubeagent would contradict that, in exchange for guarding
a page whose data the same port already serves unauthenticated.

## Invariants preserved

- **Read-only toward the cluster.** The dashboard issues no cluster call at all,
  let alone a write. There is no code path from a dashboard request into
  `internal/remediate`.
- **No model call on a request path.** `/dashboard` reads the explanation store
  the incident pipeline fills. It never calls a model. Note that this is a
  separate promise from read-only, and the daemon's `--explain` feature does make
  model calls — from the incident pipeline, never from an HTTP handler.
- **RBAC unchanged.** No new resource kind is read, so `internal/rbacprofile`'s
  Feature table, every generated manifest and the chart ClusterRole are
  untouched.
- **The six JSON documents are untouched.** The dashboard emits HTML, not a JSON
  document. No `schemaVersion` bump, no schema regeneration, no
  `internal/jsonschema` change.
- **`internal/report/testdata/golden-scan.txt` is byte-identical.** Nothing in
  the scan path moves. The demo GIF and `website/docs/quickstart.md` are not
  regenerated.
- **No new dependency.** `html/template` is in the standard library. `go.mod`
  and `go.sum` do not change.

## Configuration

One flag on `watch`:

```text
--dashboard    serve a read-only HTML dashboard at /dashboard (default false)
```

Environment variable `KUBEAGENT_DASHBOARD`, following the `envBool` pattern the
other watch flags already use.

Helm gains one values key:

```yaml
dashboard:
  # Serve a read-only HTML dashboard at /dashboard on the metrics port.
  # Unauthenticated, like /metrics and /issues on the same port: keep the
  # Service cluster-internal, or put an authenticating proxy in front.
  enabled: false
```

No new Service port — the dashboard shares the existing metrics port. No new
RBAC. No new container argument beyond `--dashboard`.

The refresh interval is **hardcoded at 30 seconds with no flag**. A flag would
be a stable surface forever, and the value buys nothing tunable: informers
detect in roughly 2 seconds and the heartbeat is 60, so 30 already sits between
them. If a real need appears, adding a flag later is an additive MINOR.

## Compatibility classification

The v1.0 production contract requires every new surface to be classified.

| Surface | Class |
|---|---|
| `--dashboard` flag and `KUBEAGENT_DASHBOARD` | **Stable** within 1.x |
| `dashboard.enabled` chart value | **Stable** within 1.x |
| `/dashboard` existing and returning HTML when enabled | **Stable** within 1.x |
| The page's HTML markup and layout | **Unstable — will change** |

The last row extends the bullet `website/docs/compatibility.md` already carries
for the HTML report: "a shareable artifact for a human, and its structure will
change". Anyone who wants a contract parses `/issues`, which is versioned.

The golden test that snapshots the markup is a regression guard so a change is
always deliberate — it is not a promise to consumers. That is the same framing
`golden-scan.txt` already carries in the same document.

## Testing

### `internal/dashboard`

- **Golden test** with a `-update` flag, mirroring
  `internal/htmlreport/golden_test.go`. The fixture covers a multicluster input
  with active and resolved incidents, one unreachable cluster, SLO enabled, and
  explanations present, so the snapshot exercises every section.
- **Escaping table** — `<script>alert(1)</script>`, `"><img src=x onerror=…>`,
  a bare `&`, control bytes, and combining marks fed through every string field.
  Asserts each appears escaped and that no executable markup survives.
- **Source test: no unsafe conversions.** The package never references
  `template.HTML`, `template.JS`, or `template.URL`. Walked with `go/parser`.
- **Source test: no kubeagent imports.** Same technique as
  `TestNoProductionImport`.
- **Determinism** — the same `Input` rendered twice is byte-identical, and an
  `Input` whose slices are shuffled renders identically to the unshuffled one.
  The second half is what actually proves the total order.
- **Empty states** — no clusters, no incidents, a cluster that has never been
  scanned, SLO disabled, explanations disabled. Each renders without panicking
  and without a `NaN` or an empty table header with no rows.
- **`FuzzDashboardRender`** — no unescaped `<` originating in the input reaches
  the output. Joins the seven fuzz targets Theme H slice 3 shipped; this is the
  same risk class those targets exist for.

### `internal/watch`

- `/dashboard` returns 404 when the flag is off.
- `/dashboard` returns 200 with `Content-Type: text/html; charset=utf-8` when on.
- The input builder copies rather than aliases: mutating the snapshot after
  building an `Input` does not change what renders.
- A `-race` test issuing concurrent GETs against concurrent snapshot swaps.
- A template failure yields 500 with no partial body.

All test fixtures use RFC 5737 addresses and RFC 2606 domains. No fixture is
named like a credential even when its value is fake.

## Gate

Full chaos gate — Helm templates move, so the lightweight smoke does not apply.

Coverage comes from **extending scenario 12** rather than adding a 23rd
scenario. Scenario 12 already stands up the daemon, breaks a workload, and curls
`/issues` while the incident fires, so adding `--dashboard` to its daemon
invocation costs a handful of `expect_*` calls instead of several minutes of
duplicate daemon setup.

Four new assertions in scenario 12:

- `/dashboard` returns 200 while an incident is firing.
- The response content type is HTML.
- The broken workload's name appears in the page.
- The page body contains no credential material and no filesystem path — the
  same check scenario 20 already applies to recorded output.

The **disabled** path is deliberately not asserted here. Scenario 12 runs one
daemon with one argument set, so proving the 404 would mean either a second
daemon or a restart, for a behaviour a `internal/watch` unit test already covers
directly. Buying minutes of cluster time for a weaker version of an existing
test is a bad trade.

Every new assertion must be **seen to fail** before it counts, per the harness
rule. Four new assertions takes the suite from 124 to 128; that number appears
in `CLAUDE.md`, `chaos/README.md`, `website/docs/compatibility.md` and
`website/docs/roadmap.md`, and all four must move together. If the conversion
lands on a different count, the count that the harness actually prints wins and
all four documents follow it.

## Documentation

- **New:** `website/docs/features/dashboard.md` — what it shows, how to enable
  it, and an unambiguous statement of the exposure posture and that kubeagent
  implements no authentication.
- `website/docs/compatibility.md` — extend the unstable-markup bullet to name
  the dashboard.
- `website/docs/features/watch-mode.md` — cross-reference.
- `website/mkdocs.yml` — nav entry.
- `website/docs/roadmap.md` — "An optional in-cluster dashboard remains ahead"
  becomes Theme G complete.
- `deploy/README.md` and the chart's values documentation.
- `CHANGELOG.md` under `[Unreleased]`.

Every example uses RFC 2606 domains and RFC 5737 addresses. No example carries a
URL longer than `scheme://host`, a kubeconfig path, or a real hostname.

## Global constraints

These bind every task in the implementation plan.

- Every commit carries a `Signed-off-by` trailer matching its author
  (`git commit -s`); `main` enforces DCO. Verify with
  `bash scripts/dco-check.sh main HEAD`.
- No AI attribution anywhere — commits, code, comments, docs, changelog.
- No new dependency. `go.mod` and `go.sum` must not change.
- TDD: the failing test first, watched failing, then the implementation.
- `go test` runs with `-p 2`, never `-short`. CI's `go test -race ./...` stays
  green.
- `internal/report/testdata/golden-scan.txt` stays byte-identical. Do not
  regenerate the demo GIF or `website/docs/quickstart.md`.
- No secrets, credentials, private IPs, or internal hostnames anywhere,
  including test fixtures, golden files, chart values examples and doc examples.
  RFC 5737 for addresses, RFC 2606 for domains.
- Work happens on a feature branch cut from `main`, never on `main` directly.

## Sequencing

The work decomposes into roughly this order, each step independently testable:

1. `internal/dashboard` — `Input`, `Render`, the template, and the full test
   suite including the golden fixture and the two source-level tests.
2. `internal/watch` — the `dashboardInput()` builder and the conditional handler
   registration, with the enabled/disabled, content-type, copy-not-alias and
   race tests.
3. `internal/cli/watch.go` — the `--dashboard` flag and `KUBEAGENT_DASHBOARD`.
4. Helm chart — `dashboard.enabled`, the rendered argument, and a chart MINOR
   bump.
5. Chaos scenario 12 extension, each assertion demonstrated failing.
6. Documentation and the roadmap and compatibility updates.

The exact task boundaries are the implementation plan's job.
