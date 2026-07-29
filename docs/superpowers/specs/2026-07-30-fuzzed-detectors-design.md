# Fuzzed detectors — design

Theme H, sub-project 2 of 7 on the way to the v1.0 production contract.
Sub-project 1 (per-feature least-privilege RBAC) shipped as v0.69.0.

## The problem

kubeagent's detectors are pure functions over Kubernetes API objects, and they
are well covered by hand-written table tests with fake pods. Every one of those
tests supplies a *plausible* object: a realistic phase, a realistic reason
string, a message a real kubelet would write.

Nothing tests the implausible. And the fields detectors read most are exactly
the fields the API server does **not** validate:

- `ContainerStatus.State.Waiting.Message` and `.Terminated.Reason` — written by
  the kubelet, arbitrary length, arbitrary bytes.
- `PodCondition.Message` — written by the scheduler or any controller with
  `patch` on `pods/status`.
- `Event.Message` and `Event.InvolvedObject.FieldPath` — written by whatever
  emitted the event.
- The tail of a crashed container's log — written by **the workload itself**.
  This is the only input kubeagent reads that an unprivileged attacker controls
  outright: anything a pod prints to stdout.
- The bytes of a `tls.crt` in a `kubernetes.io/tls` Secret, and the body of a
  CoreDNS `/metrics` or apiserver `/readyz` response.

A production contract that promises "a scan never makes things worse" has to
cover the case where one of those values is hostile, not merely unexpected. Go
1.18 shipped native coverage-guided fuzzing for precisely this shape of
problem, and kubeagent has no fuzz target at all today.

Reading the code before writing a single target already surfaced three defects
of this class. They are described below, and closing them is part of this
sub-project — finding a defect and shipping the finder without the fix would be
theatre.

## Goals

1. Every detector and every raw-input parser has a fuzz target asserting more
   than "it did not panic".
2. A hostile string reaching a detector cannot corrupt kubeagent's own output —
   not the text report, not JSON, not SARIF, not the HTML report, not the TUI.
3. A crasher, once found, becomes a permanent regression case that runs on
   every `go test` with no fuzzing budget at all.
4. Fuzzing runs on a schedule without slowing pull requests down.

## Non-goals

- **Fuzzing the I/O layer.** `internal/collect` and `internal/cluster` talk to
  an API server; their inputs are typed client-go responses, and the fake
  clientset already covers them. Only the one hardening fix below touches
  `internal/collect`.
- **Fuzzing the renderers.** `internal/report`, `internal/htmlreport`,
  `internal/sarif` and `internal/tui` consume kubeagent's own structs. This
  design keeps text safe at the point it *enters* those structs, so the
  renderers inherit the guarantee rather than each re-deriving it. Fuzzing them
  directly is a plausible later sub-project, not this one.
- **A second detector implementation to diff against.** An oracle would double
  the code under review and create a second source of truth to keep in sync.

## The three defects found while grounding this design

### Defect 1 — `dnshealth.ParseResponses` accepts `NaN`, `Inf` and overflow

`internal/dnshealth/dnshealth.go:47-51` parses each Prometheus sample value
with `strconv.ParseFloat` and converts to `int64`:

```go
v, err := strconv.ParseFloat(fields[0], 64)
if err != nil {
    continue
}
out[rcode] += int64(v)
```

`ParseFloat` accepts `"NaN"`, `"+Inf"`, `"-Inf"` and `"1e30"` without error,
and converting any of them to `int64` is implementation-defined. Verified on
this platform: all four yield `-9223372036854775808`.

`Assess` then computes

```go
errors := agg["SERVFAIL"] + agg["REFUSED"]
ratio := float64(errors) / float64(total)
```

so a CoreDNS `/metrics` body containing
`coredns_dns_responses_total{rcode="SERVFAIL"} NaN` drives `total` negative,
makes `ratio` negative or greater than 1, and flips the `degraded`/`ok` verdict
on a comparison against `threshold` that no longer means anything. Summing many
large-but-valid counters overflows the same way.

The body arrives over `pods/proxy` from a pod. kubeagent picks that pod by
label, so a workload that answers on `:9153` with a metrics-shaped body chooses
these numbers.

**Fix:** reject non-finite and out-of-range values at the parse site, and
saturate rather than wrap on every accumulation, in `ParseResponses` and in
`Assess`. Properties assert counters are non-negative and `ServfailRatio` lands
in `[0, 1]`.

### Defect 2 — no control-character sanitization anywhere on the report path

`grep` for `unicode.IsControl`, `unicode.IsPrint`, `strings.ToValidUTF8` and
`strings.Map` across `internal/` returns nothing outside `internal/tui`'s own
escape constants. Seven detector sites copy server- or workload-supplied text
into a `Finding` verbatim:

| Site | Field | Source | Escaped today? |
|---|---|---|---|
| `internal/logscan/logscan.go:66-71` | `Clue.Excerpt` | **the container's own log** | no — `truncate` is rune-safe but removes nothing |
| `internal/diagnose/imagepull.go:16` | `Evidence` | `waiting.Message` | no (`%s`) |
| `internal/diagnose/configerror.go:28` | `Evidence` | `waiting.Message` | no (`%s`) |
| `internal/diagnose/initcontainer.go:41` | `Evidence` | `waiting.Message` | no (`%s`) |
| `internal/diagnose/pending.go:18` | `Evidence` | `PodScheduled` condition `Message` | no — assigned directly |
| `internal/diagnose/volumeattach.go:31` | `Evidence` | event `Message` | no — assigned directly |
| `internal/diagnose/restartloop.go:41` | `Evidence` | `terminated.Reason` | no (`%s`) |

Container names are interpolated with `%q` throughout, which Go-quotes and so
already escapes control bytes — those sites are fine. `Finding.Issue` is fine
too, and worth stating explicitly because it becomes a SARIF rule ID: the two
sites that appear to copy `waiting.Reason` into it
(`imagepull.go:14`, `initcontainer.go:39`) are both guarded by
`w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull"`, so `Issue` only
ever holds one of a fixed set of literals.

The remaining sites are not fine. A workload that prints `\x1b[?1049h` to
stdout gets that sequence written verbatim into an operator's terminal by
`scan --logs` — the same escape `internal/tui/tui.go:26` uses to switch to the
alternate screen. `\r` rewrites the current line; `\x1b[2J` clears the display.
U+202E (`RIGHT-TO-LEFT OVERRIDE`) reorders the rest of the line, the
trojan-source trick, and is *not* covered by `unicode.IsControl` — it is
category `Cf`.

**Fix:** a new `internal/safetext` package, applied at each ingress site above.
See "internal/safetext" below.

### Defect 3 — three response bodies are read with no size limit

```
internal/collect/collect.go:472   /api/v1/nodes/<node>/proxy/healthz
internal/collect/collect.go:484   /api/v1/namespaces/<ns>/pods/<pod>:9153/proxy/metrics
internal/collect/collect.go:494   /readyz?verbose=true
```

All three end in client-go's `Result.Raw()`, which returns a body already read
in full with no cap. The CoreDNS one is the exposed case for the same reason as
defect 1: it proxies to a pod, and a pod can answer with an endless body until
the scan dies of memory exhaustion. `internal/explain/local.go:65` already caps
its own response read at `io.LimitReader(resp.Body, 1<<20)`, so the pattern and
the precedent both exist in-repo.

**Fix:** cap all three at 1 MiB. Because `Raw()` gives no access to the
underlying reader, the cap is applied to the returned slice — which bounds what
every parser downstream must handle and what the process holds per call, but
does **not** bound the transfer itself. Say so in the code comment rather than
implying a protection that is not there; bounding the transfer needs a custom
`http.RoundTripper` on the rest config, which is a larger change than this
sub-project should carry.

This is why the sub-project's gate is the **full chaos suite** rather than the
unit-only gate its decomposition originally assumed: `internal/collect` is on
the project's full-gate list.

## What ships

| Component | Kind | Purpose |
|---|---|---|
| `diagnose.DefaultDetectors(now time.Time) []Detector` | new exported function | one source of truth for the production detector set, so a fuzz target cannot silently cover a stale list |
| `internal/safetext` | new package | `Line` — make one line of untrusted text safe to render |
| `internal/fuzzgen` | new package, test-only | fuzzer bytes to Kubernetes objects, plus the shared property assertions |
| 7 fuzz targets | new `_test.go` files | `diagnose`, `logscan`, `dnshealth`, `controlplane`, `certhealth`, `redact` ×2 |
| `dnshealth` numeric hardening | fix | defect 1 |
| ingress sanitization | fix | defect 2, at the seven sites in the table above |
| `collect` 1 MiB read cap | fix | defect 3 |
| `.github/workflows/fuzz.yml` | new workflow | nightly fuzzing, one matrix leg per target |
| docs | edit | `CONTRIBUTING.md`, `docs/go-concepts.md`, `CHANGELOG.md`, `website/docs/roadmap.md`, `CLAUDE.md` |

## `diagnose.DefaultDetectors`

`internal/scan/scan.go:195-205` builds the production detector slice inline.
A fuzz target that re-declares that list would drift the first time a detector
is added, and the drift would be invisible — the target would keep passing
while covering eight of nine detectors.

```go
// DefaultDetectors is the detector set scan runs, in order. now is the
// reference time for the detectors that reason about recency; scan passes
// time.Now(), tests pass a fixed instant.
func DefaultDetectors(now time.Time) []Detector {
	return []Detector{
		CrashLoopDetector{},
		ImagePullDetector{},
		OOMKilledDetector{},
		PendingDetector{},
		VolumeAttachDetector{},
		RestartLoopDetector{Now: now},
		InitContainerDetector{},
		ProbeFailureDetector{},
		ConfigErrorDetector{},
	}
}
```

`scan.go` becomes `detectors := diagnose.DefaultDetectors(time.Now())`. Order
is preserved exactly, because `Run` appends findings in detector order and the
golden output depends on it. `internal/report/testdata/golden-scan.txt` must
stay byte-identical.

`RestartLoopDetector.Now` is the only non-determinism in the set, and it is
already injected — the fuzz target passes a fixed instant, which is what makes
the determinism property meaningful.

## `internal/safetext`

One exported function. Small enough to read in full, which is the point: it is
the only thing standing between a workload's stdout and an operator's terminal.

```go
// Package safetext makes untrusted text safe to render.
//
// kubeagent reports strings it did not write: a kubelet's waiting message, a
// scheduler's condition message, an event, the tail of a crashed container's
// own log. Those reach a terminal, a JSON document, a SARIF file, an HTML
// page and the TUI. A control character in one of them is not a cosmetic
// problem: "\x1b[2J" clears the operator's screen, "\r" rewrites the line
// that was just printed, and U+202E reverses everything after it.
//
// Call Line where untrusted text first enters a kubeagent struct, not where
// it is rendered — one call per source beats one call per renderer.
package safetext

// MaxLine bounds a single reported line. Long enough for any real kubelet or
// scheduler message; short enough that a megabyte of log cannot become a
// megabyte of report.
const MaxLine = 512

// Line returns s with anything that could corrupt rendered output removed:
// invalid UTF-8 replaced with U+FFFD, tabs turned into spaces, control
// characters and Unicode format characters (which include the bidirectional
// overrides) dropped, and the result truncated to MaxLine runes with a
// trailing ellipsis. Ordinary text is returned unchanged, which is what keeps
// the golden report snapshot stable.
func Line(s string) string
```

Rules, in order:

1. `strings.ToValidUTF8(s, "�")` — a lone continuation byte otherwise
   survives into JSON as a silently-substituted replacement character, so the
   substitution happens here where it is visible and tested.
2. `strings.Map` dropping every rune where `unicode.IsControl(r)` or
   `unicode.Is(unicode.Cf, r)` is true, and mapping `'\t'` to `' '`. `Cf`
   covers U+202A–U+202E and U+2066–U+2069, the bidi overrides and isolates.
   U+2028/U+2029 are category `Zl`/`Zp`, not `Cf`, and are mapped to a space
   explicitly because a JSON consumer may treat them as line terminators.
3. Truncate to `MaxLine` runes, appending `…` when truncation happened —
   the same rune-safe shape `logscan.truncate` already uses, so a multi-byte
   rune is never split.

`Line` is deliberately *not* HTML-escaping or shell-quoting: those are the
renderer's job and `internal/htmlreport` already escapes. `Line` removes what
no renderer can make safe.

Where a site already truncates — `logscan.truncate` bounds an excerpt to 200
runes — `Line` runs **first** and the existing truncation second. Sanitizing
after truncation would mean the 200-rune budget was partly spent on characters
about to be dropped, and the tighter of the two bounds is the one that should
survive.

## `internal/fuzzgen`

Go's fuzzer supplies `[]byte`, `string` and the numeric types. It never
supplies a struct. A fuzz target for a detector therefore needs a deterministic
function from one input to one `PodFacts`.

```go
// Package fuzzgen builds Kubernetes API objects from fuzzer-supplied bytes.
//
// Test-only. Go's native fuzzing feeds []byte and the primitive types, never a
// struct, so a fuzz target for a detector needs a deterministic way to turn one
// fuzz input into one Pod. TestNoProductionImport in this package fails if a
// non-test file ever imports it.
package fuzzgen

// Cursor hands out values drawn from a fixed byte slice, wrapping when it runs
// out so every draw succeeds and no target has to special-case a short input.
// A Cursor over an empty slice returns zero values forever.
//
// Every method is total: a Cursor never panics and never returns an error,
// because a panic in the generator is indistinguishable, in a fuzz failure,
// from a panic in the code under test.
type Cursor struct { /* … */ }

func New(b []byte) *Cursor

func (c *Cursor) Bool() bool
func (c *Cursor) IntN(n int) int              // 0 ≤ result < n; n ≤ 0 yields 0
func (c *Cursor) Int32() int32                // full range, negatives included
func (c *Cursor) Pick(opts []string) string   // one of opts; empty opts yields ""
func (c *Cursor) Hostile(maxLen int) string   // arbitrary bytes, possibly invalid UTF-8
func (c *Cursor) Name(maxLen int) string      // DNS-1123-safe: [a-z0-9-]
func (c *Cursor) Time(base time.Time) metav1.Time

func (c *Cursor) Pod() *corev1.Pod
func (c *Cursor) Events(pod *corev1.Pod, max int) []corev1.Event
func (c *Cursor) TLSSecret(crt []byte) corev1.Secret
```

**Which fields get hostile bytes and which get safe ones is the design
decision that makes the properties meaningful.** The API server validates
object names, namespaces and container names as DNS-1123 labels; a real cluster
cannot present anything else, so `Name` draws from `[a-z0-9-]`. Everything the
API server does *not* validate — messages, reasons, field paths, log text,
certificate bytes — gets `Hostile`. Asserting that a detector's output is
control-free would otherwise be asserting something about pod names that no
cluster can violate, which would look like coverage and be noise.

`Pod` fills what the nine detectors actually read:

- `Namespace`, `Name` — `Name(24)`
- `Spec.Containers` and `Spec.InitContainers` — 0–3 each, `Name(16)`, with
  `Resources.Requests`/`Limits` drawn from a fixed list of quantity strings
  parsed with `resource.ParseQuantity` and skipped on error (never
  `MustParse`, which panics)
- `Status.Phase` — `Pick` over the five real phases plus `Hostile(8)`
- `Status.ContainerStatuses`, `Status.InitContainerStatuses` — 0–3 each, with
  `Name` matching a spec container most of the time and diverging sometimes;
  exactly one of `Waiting`/`Running`/`Terminated` set in `State`, plus an
  independently-drawn `LastTerminationState`; `Reason` from `Pick` over the
  real reason set (`CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`,
  `OOMKilled`, `CreateContainerConfigError`, `ContainerCreating`, `Error`,
  `Completed`) plus `Hostile(16)`; `Message` from `Hostile(64)`; `ExitCode` and
  `RestartCount` from `Int32` so negatives occur; `StartedAt`/`FinishedAt` from
  `Time`
- `Status.Conditions` — 0–3, `Type` from `Pick` over `Ready`, `PodScheduled`,
  `Initialized`, `ContainersReady` plus `Hostile(8)`; `Status` from `Pick` over
  `True`/`False`/`Unknown` plus `Hostile(4)`; `Reason` including
  `Unschedulable`; `Message` from `Hostile(64)`

`Events` produces events whose `Reason` is drawn from `Unhealthy`,
`FailedAttachVolume` plus `Hostile`, whose `Message` is `Hostile(96)` — the
seed corpus supplies the literals `Multi-Attach`, `Readiness probe failed`,
`Liveness probe failed`, `Startup probe failed`, `connection refused` and
`statuscode: 503` so the fuzzer can reach `classifyProbe` and
`probeReasonTail` from a seed rather than by discovering an English phrase
byte by byte — and whose `InvolvedObject.FieldPath` is
`"spec.containers{" + name + "}"` most of the time and `Hostile(24)` otherwise,
because `containerFromFieldPath` slices on brace positions.

The package also holds the shared assertions, so seven targets state the same
property once:

```go
// AssertSafe fails t when s is not safe to render: invalid UTF-8, a control
// character, or a Unicode format character. where names the field, so a
// failure says which one.
func AssertSafe(t *testing.T, where, s string)

// AssertBounded fails t when s is longer than max runes.
func AssertBounded(t *testing.T, where string, s string, max int)
```

Length is a separate assertion from safety on purpose. A detector composes its
evidence — `fmt.Sprintf("container %q: %s", name, safetext.Line(msg))` — so the
composed field is legitimately longer than `safetext.MaxLine` even when every
untrusted part of it is bounded. Folding length into `AssertSafe` would make
every composed field fail, and the natural response to that would be to raise
the limit until nothing fails, which tests nothing. `AssertSafe` therefore
holds for every reported string, and `AssertBounded` is applied per field with
the bound that field actually has.

### The import guard

`internal/fuzzgen` is a normal package, not a `_test.go` file, so nothing in
the language stops production code from importing it. A guard test asserts
nothing does:

```go
// TestNoProductionImport fails if any non-test Go file in the module imports
// fuzzgen. The package generates hostile objects on purpose; it has no business
// in a binary a user runs.
func TestNoProductionImport(t *testing.T)
```

It walks the module with `go/build`-style parsing (`parser.ParseFile` with
`parser.ImportsOnly`) over every `.go` file not ending in `_test.go`, and fails
naming the file. `packages.Load` is deliberately not used — no new dependency.

## The seven fuzz targets

Each lives in its own package as `fuzz_test.go`. Seeds are `f.Add` calls in the
target: they live next to the properties they exercise, they are readable in
review, and Go replays them on a plain `go test` with no fuzzing budget.
`testdata/fuzz/<Target>/` holds only crashers found later, which Go writes
there itself.

### 1. `FuzzDetectors` — `internal/diagnose`

Input: `[]byte`. Builds `PodFacts` via `fuzzgen`, runs
`diagnose.Run(diagnose.DefaultDetectors(fixedNow), []PodFacts{facts})`.

Properties: no panic; purity; determinism; output safety on `Reason`,
`Evidence`, `Container`, and `Resources`' four quantity strings. `Pod` is
checked for the `namespace/name` shape rather than for safety, since both
halves come from `Name`.

Seeds: one input per detector, hand-built so the corpus starts with all nine
firing, plus the empty input and an all-`0xff` input.

### 2. `FuzzClassify` — `internal/logscan`

Input: `string` — a container's log tail, the one fully attacker-controlled
input kubeagent reads.

Properties: no panic; determinism; `Excerpt` and `Cause` safe; `Signature` is
either empty or one of the nine known signature names — a fuzzer must not be
able to invent a tenth.

Seeds: a plain crash log, one line per signature, a log of only newlines, a
line with an ANSI escape, a line with U+202E, a line of invalid UTF-8, and a
line longer than `MaxLine`.

### 3. `FuzzParseResponses` — `internal/dnshealth`

Input: `[]byte` — a CoreDNS `/metrics` body, plus a `[]byte` of parameter bytes
driving `podsProbed`, `forbidden`, `unreachable`, `threshold` and `floor`
through a `Cursor`, so `Assess` is fuzzed in the same target as its input.

Properties: no panic; determinism; every count in the returned map is
non-negative; `Report.ErrorResponses` and `.TotalResponses` are non-negative
with `ErrorResponses ≤ TotalResponses`; `ServfailRatio` is finite and in
`[0, 1]`; `Status` is one of the five documented values; `Detail` safe.

Seeds: a real metrics body, the pre-1.7 metric name, and the crashers from
defect 1 — `NaN`, `+Inf`, `-Inf`, `1e30`, and two large-but-valid counters that
overflow when summed.

### 4. `FuzzParseReadyz` — `internal/controlplane`

Input: `int` status code and `[]byte` body.

Properties: no panic; determinism; `Status` one of the four documented values;
every entry of `Failed` safe; `len(Failed)` bounded — a body of a million
`[-]` lines must not become a million-element slice in a report.

Seeds: a real verbose `/readyz` body, an empty body, `[-]` with no name, and a
`[-]` line whose check name is hostile bytes.

### 5. `FuzzCertAssess` — `internal/certhealth`

Input: `[]byte` — used as a Secret's `tls.crt`, so the fuzzer drives
`pem.Decode` and `x509.ParseCertificate`.

Properties: no panic; determinism; `CommonName` safe; `NotAfter` parses as
RFC 3339 when present; every Secret lands in exactly one of `Expired`,
`Expiring`, `Invalid` or nowhere, and `Checked` equals the number of TLS
Secrets passed in.

Seeds: a valid self-signed PEM generated once and committed as a literal in
the test (RFC 2606 `example` domain, no real hostname), a truncated PEM, a PEM
whose body is not DER, and empty.

`now` is a fixed instant, not `time.Now()`, so `Days` is deterministic.

### 6 and 7. `FuzzRedactURL` and `FuzzRedactError` — `internal/redact`

Input: `string`.

`FuzzRedactURL` properties: no panic; determinism; the output is either exactly
`(redacted)` or re-parses with `Scheme` and `Host` non-empty and `Path`,
`RawQuery`, `Fragment`, `Opaque` and `User` all empty. That last one is the
property that matters — it is the package's whole promise, stated as an
assertion instead of as a comment.

`FuzzRedactError` wraps the input in a `*url.Error` and asserts an exact
equality rather than an absence:

```go
err := &url.Error{Op: "Post", URL: raw, Err: errors.New("boom")}
want := "Post " + redact.URL(raw) + ": boom"
if got := redact.Error(err); got != want { … }
```

Exact equality, not "the output does not contain the path". A containment check
on a fuzzed URL produces constant false failures — a one-character path is a
substring of almost any output — and a check that only rejects long paths
silently stops testing the short ones. The equality says the whole thing: the
rendered error is the operation, the host-only URL, and the cause, and nothing
else can appear. The same assertion is made at two levels of nesting, since
`Error` recurses.

Seeds include a Slack-shaped webhook URL built from an RFC 2606 `example` host
with placeholder path segments — never a real webhook.

## CI wiring

Seeds replay in the existing `go test ./...`, so every pull request already
runs every target's corpus at zero added cost. Real fuzzing runs nightly:

```yaml
name: Fuzz

on:
  schedule:
    - cron: '17 3 * * *'
  workflow_dispatch:
    inputs:
      fuzztime:
        description: "Per-target fuzzing budget, e.g. 300s"
        default: "120s"

permissions:
  contents: read

jobs:
  fuzz:
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      matrix:
        include:
          - { pkg: ./internal/diagnose,     target: FuzzDetectors }
          - { pkg: ./internal/logscan,      target: FuzzClassify }
          - { pkg: ./internal/dnshealth,    target: FuzzParseResponses }
          - { pkg: ./internal/controlplane, target: FuzzParseReadyz }
          - { pkg: ./internal/certhealth,   target: FuzzCertAssess }
          - { pkg: ./internal/redact,       target: FuzzRedactURL }
          - { pkg: ./internal/redact,       target: FuzzRedactError }
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Fuzz ${{ matrix.target }}
        run: |
          go test ${{ matrix.pkg }} \
            -run '^$' -fuzz '^${{ matrix.target }}$' \
            -fuzztime '${{ inputs.fuzztime || '120s' }}'
      # Go writes a crasher into the package's testdata/fuzz/<target>/ inside
      # the checkout. Uploading it is the whole point of the job: the input is
      # committed as a permanent regression case, and from then on it runs on
      # every `go test` with no fuzzing budget at all.
      - name: Upload crashers
        if: failure()
        uses: actions/upload-artifact@v4
        with:
          name: crashers-${{ matrix.target }}
          path: internal/**/testdata/fuzz/**
          if-no-files-found: ignore
```

`fail-fast: false` matters: one target crashing must not cancel the other six,
or a single flaky finding hides everything else that night.

`-run '^$'` skips the package's unit tests so the whole budget goes to
fuzzing — the unit tests already ran in CI.

`go test -fuzz` accepts exactly one target and one package per invocation,
which is why the matrix enumerates pairs rather than passing a pattern.

## Testing strategy and gate

- **TDD throughout.** Each fuzz target is written and run *before* its fix.
  For defects 1 and 2 the target's property must be watched failing on the
  current code — that failing run is the evidence the property has teeth. A
  property that has never failed has not been shown to test anything.
- **Mutation-check each fix.** After a fix lands, revert it locally, confirm
  the property fails, restore. Recorded in the task report, per the practice
  established on the previous sub-project.
- **`go test -p 2 ./... -count=1`** — full parallelism trips a known Go linker
  panic on `internal/advisory`. Never `-short`.
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.**
  `safetext.Line` is a no-op on the golden's clean text, and
  `DefaultDetectors` preserves detector order. If the golden moves, the change
  is a bug, not a golden to regenerate.
- **Full chaos gate** — `./chaos/run.sh --recreate`, all 20 scenarios plus the
  baseline, because the read cap touches `internal/collect`.
- **A short nightly-workflow dry run** — trigger `fuzz.yml` once via
  `workflow_dispatch` with a small budget before merging, so the workflow is
  known to work rather than assumed to.

## Documentation

- `CONTRIBUTING.md` — a "Fuzzing" bullet in the Testing section: how to run one
  target locally, and that a crasher gets committed under `testdata/fuzz`.
- `docs/go-concepts.md` — a new entry for native fuzzing, in the established
  style: the plain everyday example first, then the kubeagent example. No
  Python comparisons.
- `CHANGELOG.md` — `[Unreleased]`, with the three defects under `### Fixed`
  described in terms of what a user could have observed.
- `website/docs/roadmap.md` — the Theme H bullet currently ends "Fuzzed
  detectors remain ahead."
- `CLAUDE.md` — two lines: `internal/fuzzgen` is test-only and guarded by a
  test; untrusted text is sanitized at ingress with `safetext.Line`, not at
  render time.

## Out of scope, named

- **The renderer-side sweep.** The ~30 other health packages that copy a
  `.Message` into a report are untouched here. The guarantee this sub-project
  delivers is therefore partial and should be described that way: the detector
  path and the five fuzzed parsers are safe; a message copied by, say,
  `internal/pdbhealth` is not. A later sub-project either sweeps the remaining
  ingress sites or adds a render-time backstop in `internal/report` and
  `internal/tui`.
- **Bounding the HTTP transfer**, as opposed to the returned slice — needs a
  custom `RoundTripper` on the rest config.
- **Fuzzing the renderers themselves.**

## Global constraints

- Every commit carries a `Signed-off-by` trailer matching its author
  (`git commit -s`); `main` enforces DCO.
- No `Co-Authored-By` trailer and no Claude / Claude Code / Anthropic / AI
  attribution anywhere — commits, code, comments, docs, changelog.
- Detectors stay pure functions. The fuzz purity property enforces it.
- Standard-library `flag` only; no Cobra (that is sub-project 5).
- No new third-party dependency: `internal/fuzzgen` replaces
  `go-fuzz-headers`, and the import guard uses `go/parser` from the standard
  library.
- No secrets, credentials, private IPs or internal hostnames anywhere,
  including in seed corpora and test fixtures. Documentation and test IPs are
  RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example
  domains are RFC 2606 (`.example`).
- URLs are credentials: no log line, error, metric label or seed corpus entry
  carries more than `scheme://host` of anything resembling a real endpoint.
- `internal/report/testdata/golden-scan.txt` stays byte-identical.
- `go test` runs with `-p 2`.
- `internal/safetext` and `internal/fuzzgen` must never import
  `internal/remediate` or `internal/explain`.
