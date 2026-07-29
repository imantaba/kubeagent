# `kubeagent tui` — interactive findings browser (Theme G slice 4b)

**Status:** approved, ready for planning
**Slice:** Theme G, slice 4b — the last slice before Theme G's optional in-cluster dashboard
**Target release:** v0.67.0
**Branch:** `tui`, cut off `main` at `d00adb1` (v0.66.0)

## Goal

Add `kubeagent tui`: a full-screen, keyboard-driven browser over one scan
result. It shows the same findings `kubeagent scan` shows, ranked the same way,
and lets an operator filter by severity, move through the list, and read one
finding's full text without re-running the command with different flags.

It is a snapshot browser, not a dashboard. It scans once, on demand, and stays
on screen until you quit.

## Why this shape

Three facts about the existing code decided most of the design.

**`golang.org/x/term` is already in the module graph.** `go mod why` traces it
through `internal/cluster` → `k8s.io/client-go/tools/clientcmd` →
`golang.org/x/term`, and `go.sum` carries the full `h1:` hash, not only the
`/go.mod` one. Importing it directly adds **zero new modules** — it moves one
line from the indirect block of `go.mod` to the direct block. Raw mode and
window size are therefore available at no dependency cost, which is why this
slice hand-rolls its rendering with ANSI escapes instead of adding bubbletea
(~10-14 new modules) or tview (~6-8). The project's small-dependency posture
holds until v1.0, and Theme H is about to add signing, SBOM and provenance —
the wrong moment to grow a new supply-chain surface for a convenience.

**`kubeagent gate` already set the subcommand precedent.** `runGate` declares
its own small `flag.FlagSet` and builds its `scan.Options` through a dedicated
`gateScanOptions(namespace)` helper rather than inheriting `scan`'s ~30 flags.
`tui` follows that precedent exactly, so this slice needs no shared-flag-builder
refactor of `main.go`; it lands additively.

**`findings.Flatten` already covers the workload inventory.** A workload that
`Flagged()` but produced no detector match becomes a Warning through
`fromWorkload`. One flattened list is therefore complete, and the TUI needs no
second inventory screen to be honest about what it shows.

## Contract

`kubeagent tui` shows **exactly what bare `kubeagent scan` shows** — the same
default detector set, the same findings, the same order. Not a subset, not a
superset.

This is the whole coverage claim, and it is deliberately one sentence rather
than a table of which optional checks are wired. The opt-in advisories
(`--security`, `--certs`, `--capacity`, `--operators`, `--drift`, `--logs`,
`--disk-usage`, and the rest) stay where they are: run `kubeagent scan` for
those. The help screen (`?`) says so, in one line. The footer is the key map
and has no room for it; burying the coverage claim in a row of key hints would
also make it easy to miss, which is the opposite of the point.

### Flags

```text
kubeagent tui [--kubeconfig path] [--context name] [-n namespace]
```

Three flags, matching their `scan` spellings and defaults exactly:

| Flag | Type | Default | Meaning |
|------|------|---------|---------|
| `--kubeconfig` | string | `""` | path to kubeconfig (default: `$KUBECONFIG` or `~/.kube/config`) |
| `--context` | string | `""` | kubeconfig context to use (default: current-context) |
| `-n` / `--namespace` | string | `""` | namespace to browse (default: all namespaces) |

No other flags. `--output` does not exist here: a TUI seizes the terminal and
is not redirectable, so it is not an output format.

### Invariants

- **Read-only toward the cluster.** `get`/`list` only — not even `watch`. The
  TUI reaches the cluster through exactly the same `cluster.NewClient` +
  `scan.Evaluate` path the CLI already uses.
- **No LLM call on any TUI path.** `--explain` and `--investigate` are not
  accepted. This is a separate, stronger claim than read-only, and it is worth
  stating on its own: a re-scan key that silently re-billed an API call every
  time it was pressed would be a trap.
- **`internal/tui` must never import `internal/remediate` or
  `internal/explain`**, joining `internal/mcp` and `internal/gate` in that
  family. Unlike `internal/htmlreport`, `internal/tui` does not consume
  `report.Input`, so it carries no transitive edge to `internal/remediate`
  either.
- **No cluster identity in the chrome.** No context name and no kubeconfig path
  in any frame, ever. Those are values kubeagent was handed and would be
  printing on purpose; the HTML report and the gate verdict refuse them for the
  same reason.

  Error text is the one place a host can still appear, and it follows the rule
  the rest of the CLI already follows rather than a stricter one invented here:
  a failed re-scan's message goes through `internal/redact` — which strips the
  path, the query and any userinfo and keeps `scheme://host` — and lands in the
  footer. That is byte-for-byte what `kubeagent scan` prints to stderr today for
  the same failure. The distinction that matters is not "which package rendered
  it" but "where do the bytes go": the HTML report is written to be forwarded,
  a TUI frame is on the operator's own screen and is never captured. This is the
  same reasoning that keeps blind-spot reasons verbatim here.
- `internal/report/testdata/golden-scan.txt` is untouched. This slice adds no
  code to `internal/report`.

## Architecture

One new leaf package, four files, with all the logic in pure functions:

```text
internal/tui/
  tui.go     Run() — terminal setup and teardown, the input loop, the scan calls.
             The only file that touches a terminal, a signal, or a cluster.
  model.go   Model + Update(Model, Event) Model — pure state transition.
  render.go  Render(Model) string — the exact bytes to write. No terminal, no clock.
  key.go     decodeKey([]byte) (Key, int) — pure byte-run to key event.
```

The split exists so that everything worth testing is a pure function tested
without a terminal, and `tui.go` stays small enough to review by eye.

### Data in

`Run` calls `scan.Evaluate(ctx, client, tuiScanOptions(namespace))` and projects
the `scan.Result` into a `Model`. It consumes four things and nothing else:

| From | Used for |
|------|----------|
| `findings.Flatten(res)` | the ranked finding list — the body of the screen |
| `res.PartialReads` (`[]scan.ReadFailure`) | the blind-spots view |
| `res.Health` (`clusterhealth.ClusterHealth`) | the header's cluster line |
| `len(res.Inventory.Workloads)` | the header's denominator |

`tuiScanOptions(namespace)` mirrors `gateScanOptions(namespace)`: the default
detector set with the same two environment-tunable thresholds.

```go
func tuiScanOptions(namespace string) scan.Options {
	return scan.Options{
		Namespace:               namespace,
		QuotaThreshold:          envFloat("KUBEAGENT_QUOTA_THRESHOLD", 0.90),
		WebhookTimeoutThreshold: int32(envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15)),
	}
}
```

### Model

```go
// Mode is which view the screen is showing.
type Mode int

const (
	ModeList Mode = iota // the findings table
	ModeDetail           // one finding, full text
	ModeBlind            // what kubeagent could not read
	ModeHelp             // the key map
)

// Filter is the severity floor the list shows, matching the HTML report's
// three-way control so the two surfaces filter identically.
type Filter int

const (
	FilterAll      Filter = iota // everything
	FilterWarning                // warning and above
	FilterCritical               // critical only
)

type Model struct {
	Version   string
	Scope     string // "all namespaces" or "namespace shop"
	Generated string // "2026-07-29 11:04:12 UTC" — formatted by the caller, never time.Now here
	Health    clusterhealth.ClusterHealth
	Workloads int

	All   []findings.Finding // every finding, in findings.Sort order, never re-sorted
	Blind []scan.ReadFailure

	Mode     Mode
	Filter   Filter
	Cursor   int // index into the filtered list
	Top      int // first visible row, for scrolling
	Width    int
	Height   int
	Colour   bool   // false under NO_COLOR; the golden frame renders with this false
	Scanning bool   // true while a re-scan is in flight; renders the busy frame
	Err      string // a failed re-scan's message, shown in the footer; cleared by the next success
	Quit     bool
}
```

`Model` holds `All` and derives the filtered view on demand
(`func (m Model) visible() []findings.Finding`) rather than storing a second
slice. One list, one order, no chance of the two drifting.

`Generated` is a preformatted string and `Model` holds no clock, so `Render` is
deterministic and the golden frame test needs no time injection.

### Update

```go
func Update(m Model, e Event) Model
```

Pure: same model and event in, same model out. `Event` is a small sum type
covering a key press, a resize, and a completed re-scan:

```go
type EventKind int

const (
	EventKey     EventKind = iota // a decoded key press
	EventResize                   // SIGWINCH: Width and Height are set
	EventScanned                  // a re-scan finished: Result is set, or Err is
)

// ScanSnapshot is the part of a scan.Result the TUI keeps. Run projects one of
// these and hands it to Update; Update never sees a scan.Result or a client.
type ScanSnapshot struct {
	Findings  []findings.Finding
	Blind     []scan.ReadFailure
	Health    clusterhealth.ClusterHealth
	Workloads int
	Generated string
}

type Event struct {
	Kind   EventKind
	Key    Key           // Kind == EventKey
	Width  int           // Kind == EventResize
	Height int           // Kind == EventResize
	Result *ScanSnapshot // Kind == EventScanned, on success
	Err    string        // Kind == EventScanned, on failure
}
```

`Update` handling `EventScanned` is what keeps the failed-re-scan rule honest:
on `Err` it sets `Model.Err` and leaves `All`, `Blind`, `Health` and
`Workloads` untouched, so the findings already on screen survive.

Every rule the screen obeys — cursor clamping at both ends, the cursor moving
when a filter change shrinks the list under it, `Top` following the cursor,
mode transitions, quitting — lives here and is table-tested.

### Render

```go
func Render(m Model) string
```

Returns the complete frame: cursor home, the drawn screen, and the trailing
erase. It never writes, never queries the terminal, and never branches on
anything outside `m`.

Colour comes from `m` too: `Run` sets `Model.Colour` from
`os.Getenv("NO_COLOR") == ""` — no terminal check is needed, because `Run`
already refused to start unless both fds are TTYs. The golden test renders the
uncoloured frame; one focused test covers the coloured one.

## Screen

```text
kubeagent v0.67.0 · all namespaces · 62 workloads · 2026-07-29 11:04:12 UTC
Cluster: Degraded · 3/3 nodes Ready · 2 blind spots (b)
──────────────────────────────────────────────────────────────────────────
  LEVEL     KIND         NAMESPACE   NAME               ISSUE
▸ critical  Pod          shop        crasher-7d9f-x2    CrashLoopBackOff
  critical  Pod          shop        badimage-5b8-qq    ImagePullBackOff
  warning   Deployment   shop        api                1/3 ready
──────────────────────────────────────────────────────────────────────────
[1] critical [2] warning+ [0] all (14)  ↑↓ move  ⏎ detail  b blind  r rescan  ? help  q quit
```

Columns are width-budgeted: `LEVEL`, `KIND`, `NAMESPACE` and `NAME` take fixed
shares of the terminal width and `ISSUE` takes the remainder. A value too long
for its column is truncated with `…`. Truncation is why the detail view exists.

Detail:

```text
critical · Pod · shop/crasher-7d9f-x2
owner Deployment/crasher

Issue
  CrashLoopBackOff

Reason
  back-off 5m0s restarting failed container=app pod=crasher-7d9f-x2
  (last state: Error, exit code 1)

──────────────────────────────────────────────────────────────────────────
esc back   ↑↓ prev/next finding   q quit
```

The detail view's job is the full, wrapped `Reason`. An `ErrImagePull` reason
with a nested DNS failure is far longer than any 80-column table cell, and
truncating it in the only view that shows it would hide the diagnosis.

### Keys

| Key | Action |
|-----|--------|
| `↑` `k` | move up |
| `↓` `j` | move down |
| `g` / `G` | first / last finding |
| `⏎` `→` `l` | open detail for the selected finding |
| `esc` `←` `h` | back to the list |
| `1` | filter: critical only |
| `2` | filter: warning and above |
| `0` `a` | filter: all |
| `b` | blind spots |
| `r` | re-scan |
| `?` | help |
| `q` `Ctrl-C` | quit |

An unrecognised key is ignored — never an error, never a beep.

`Key` is one decoded key press. Printable keys carry their rune; the rest are
named constants, so `Update` never re-parses bytes:

```go
type Key struct {
	Rune rune    // set when Kind == KeyRune
	Kind KeyKind
}

type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyEnter
	KeyEsc
	KeyCtrlC
	KeyUnknown // a byte run decodeKey does not recognise; Update ignores it
)
```

```go
func decodeKey(b []byte, final bool) (Key, int)
```

It returns the key and how many bytes it consumed. A `0` count means the run is
a valid prefix of a longer escape sequence that has not fully arrived, so `Run`
keeps the bytes and reads more — the split-escape-sequence case the tests cover.

`final` is what makes a bare `esc` usable. A lone `0x1b` is indistinguishable
from the start of an arrow-key sequence, so streaming decoding must return `0`
and wait. Nothing follows a real `esc` press, so waiting forever would make the
key appear dead until the user pressed something else. `Run` therefore arms a
50 ms timer whenever bytes are pending and re-decodes with `final: true` when it
fires; under `final` a lone `0x1b` resolves to `KeyEsc` and an incomplete CSI
run resolves to `KeyUnknown`. The resolution stays a pure function — only the
timer lives in `Run`.

### Re-scan

`r` renders the busy frame **first**, then runs `scan.Evaluate` synchronously on
the main loop, then renders the result. Keys are ignored while it runs, which is
why the busy frame must be on screen before the call starts rather than after.

Cluster access stays strictly sequential: one scan at a time, from one place,
exactly as the CLI does it today.

## Terminal safety

This section is explicit because getting it wrong leaves an operator with a
terminal that echoes nothing and needs `reset` to recover.

`Run` puts the terminal in raw mode, switches to the alternate screen buffer
(`\x1b[?1049h`) so the user's scrollback survives, and hides the cursor
(`\x1b[?25l`). All three must be undone on **every** exit path:

- a `defer` for the normal return and for any error return;
- a `recover` that restores the terminal, then re-panics — a panic in the render
  path that skipped restoration would leave the shell unusable and bury the
  stack trace in the alternate screen;
- a handler for `SIGTERM` and `SIGHUP` that restores and exits.

Raw mode disables the terminal's own signal generation, so `Ctrl-C` arrives as
byte `0x03` and `Ctrl-Z` as `0x1a`:

- `Ctrl-C` is handled as a clean quit through the normal teardown.
- `Ctrl-Z` is **ignored** rather than suspending. Suspending would drop the user
  back to a cooked shell and resume into a terminal whose mode no longer matches
  what the TUI believes, and handling `SIGCONT` correctly is more machinery than
  a first slice needs.

`SIGWINCH` triggers a size re-query and a redraw. Handling it needs exactly
**one goroutine**: stdin is read into a channel so the main loop can `select`
over key bytes and resize notifications. That goroutine reads stdin and nothing
else — the cluster is only ever touched from the main loop.

This makes `kubeagent tui` the **third long-lived process** in the codebase,
after `internal/watch` and `internal/mcp`, and the CLAUDE.md invariant that
names the exceptions must be updated to say so.

### Not a terminal

Both stdin and stdout must be TTYs — stdin to read keys, stdout to draw. If
either is not, `Run` refuses **before touching the network**:

```text
kubeagent: tui needs an interactive terminal; use 'kubeagent scan' for pipes and files
```

No kubeconfig path and no context name in that message. The check is extracted
as a pure function so the refusal path is tested without a terminal:

```go
func checkTTY(inFD, outFD int, isTerm func(int) bool) error
```

## Blind spots — deliberately unlike the HTML report

The blind-spots view (`b`) shows the cluster's error **verbatim**, the way
`--output text` does:

```text
kubeagent could not read the following, so the findings are incomplete.

  horizontalpodautoscalers
    horizontalpodautoscalers.autoscaling is forbidden: User
    "system:serviceaccount:kubeagent:reader" cannot list resource
    "horizontalpodautoscalers" in API group "autoscaling"
```

`internal/htmlreport` classifies these reasons into three kubeagent-authored
phrases and never quotes the cluster, because `apierrors.NewForbidden`
interpolates the authorizer's own error — which embeds the username, and under
webhook authorization a third-party backend's free text — and that document is
written to be forwarded.

A TUI frame is on the operator's own screen and is never forwarded. Classifying
it there would destroy diagnostic value for no gain: an operator debugging their
own RBAC needs to see which principal was denied.

**This difference is intentional. Do not copy `htmlreport.safeReason` into
`internal/tui`.** The rule is about where the bytes go, not about which package
produced them.

The blind-spot count also appears in the header whenever it is non-zero, so a
partial scan can never look complete — the same green-when-blind failure
`kubeagent gate` exists to prevent.

## Errors

| Situation | Behaviour |
|-----------|-----------|
| stdin or stdout not a TTY | refuse before connecting, message above, exit 1 |
| `cluster.NewClient` fails | plain stderr error before raw mode, exit 1 — identical to `scan` |
| first `scan.Evaluate` fails | plain stderr error before raw mode, exit 1 |
| re-scan (`r`) fails | stay in the TUI, show `redact.Error(err)` in the footer, keep the previous findings on screen |
| terminal too small (< 40 cols or < 10 rows) | render a single "terminal too small" line rather than a corrupted frame |

A failed re-scan must not discard the findings already on screen: an operator
whose VPN dropped mid-incident should not lose the list they were reading.

## Testing

| Unit | Test |
|------|------|
| `Render` | golden frame at a fixed 80×24 against `internal/tui/testdata/golden-frame.txt`, with `\x1b` written as `␛` so the golden file diffs readably. Regenerated with `-update`, like the scan golden. |
| `Render` | focused tests: truncation at a narrow width, the too-small frame, the coloured frame, the empty-findings frame |
| `Update` | table tests: cursor clamped at both ends; cursor pulled back when a filter change shrinks the list under it; `Top` following the cursor past the viewport; every mode transition; quit; unknown key is a no-op |
| `decodeKey` | table tests: `j`; arrow `\x1b[B`; `Ctrl-C` `\x03`; bare `esc`; an escape sequence split across two reads |
| `checkTTY` | both refusal cases and the success case, with an injected `isTerm` |
| `main.go` | `kubeagent tui` reaches `runTUI`; unknown flags rejected; usage line names `tui` |

`Run` itself is not unit-tested. It is kept under roughly 80 lines and every
decision it would otherwise make is pushed into `Update`, `Render`, `decodeKey`
or `checkTTY`.

### Release gate

Kind smoke, not the chaos suite: this slice touches no `internal/collect`, no
`internal/cluster`, no RBAC, no `--fix`, no watch daemon and no Helm templates.
The smoke run drives the real binary against a live Kind cluster with a crashing
pod and a bad image, exercises every key, and confirms the terminal is restored
to cooked mode on quit, on `Ctrl-C`, and on `SIGTERM`.

## Out of scope

Deliberately left out of this slice:

- **Live / auto-refresh.** That is `kubeagent watch`, which already exists and
  already has informers.
- **`--fix` from inside the TUI.** Every remediation guard-rail — allowlist,
  protected namespaces, per-action confirmation, re-verify, audit log — would
  have to be re-proven through a screen-based confirm flow. That is its own
  slice, if it is ever worth one.
- **Remediation suggestions in the detail pane.** `remediation.For` takes a
  `diagnose.Finding`, not a `findings.Finding`, so wiring it in would mean a
  second definition of the suggestion mapping, free to drift from the first.
- **Browsing healthy workloads.** `findings.Flatten` includes only
  attention-worthy items by design; the full inventory stays in `scan` and
  `--output html`.
- **Opt-in advisory checks** (`--security`, `--certs`, `--capacity`, …). See
  the contract above.
- **Mouse support, colour themes, search, config files.**

Colour itself is in scope: severity-coloured rows, with `NO_COLOR` honoured.

## Documentation

- `website/docs/features/tui.md` — new page: what it shows, the key map, the
  coverage contract, the not-a-terminal behaviour, and the blind-spots
  difference from the HTML report.
- `website/mkdocs.yml` — nav entry.
- `website/docs/roadmap.md` — Theme G's remaining item becomes the optional
  in-cluster dashboard; the `v0.5x` milestone row marks the TUI shipped.
- `CLAUDE.md` — the long-lived-process invariant gains `internal/tui` as the
  third exception, with its never-import rule.
- `README.md` — one feature bullet.
- `CHANGELOG.md` — `[Unreleased] → Added`.
