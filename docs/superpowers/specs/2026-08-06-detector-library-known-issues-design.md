# Known issues — an offline reference for the failure modes kubeagent reports

**Status:** design approved, not yet implemented
**Roadmap item:** post-1.0 item 3 — "a curated community detector library with a
known-issues knowledge base" — slice 1 of 2
**Date:** 2026-08-06

## The problem

`kubeagent scan` names a failure mode and stops there:

```text
✗ CrashLoopBackOff  example-ns/api-7d9f  Container repeatedly crashes after starting (container "api": back-off 5m0s restarting failed container)
```

That is the correct amount of output for a scan — a report that explained every
finding at length would bury the findings. But it leaves the operator holding a
kind name and nothing else. Today the only place that explains what
`CrashLoopBackOff` means, what usually causes it, and what to look at next is
[`website/docs/features/diagnostics.md`](../../../website/docs/features/diagnostics.md),
which is a web page: it needs a browser, a network, and the operator to guess
which heading corresponds to the string they are looking at.

Two things are wrong with that, and the second is worse than the first.

**It is not available where the failure is.** An operator triaging a cluster at
02:00 over a bastion has the binary and no browser.

**Nothing keeps it true.** There is no link of any kind between the `Issue`
string a detector emits and the section that documents it. A detector can ship a
new kind and the page will not notice; a section can document a kind no detector
emits any more and nothing will notice that either. The docs and the code drift
freely, and the drift is silent.

There is a third problem underneath both, which this slice's investigation
surfaced: **the repository cannot currently answer "what issue kinds can
kubeagent emit"**. A sweep of `internal/` finds 24 distinct string literals
assigned to an `Issue:` field, five of which are fragments produced by
concatenation or a format string (`Init:`, `nearing`, `X`, `Noted`,
`Informational`). No single place holds the vocabulary. Forcing that list to
exist — for at least one well-bounded part of the codebase — is most of this
slice's value.

## What ships

A new package `internal/knownissues` holding one entry per failure mode the
deterministic detector set can report, and a new command:

```bash
kubeagent known-issues            # list every documented kind, one line each
kubeagent known-issues OOMKilled  # print one entry in full
```

No cluster, no kubeconfig, no network, no model call. The command is modeled on
`kubeagent schema [name]`, which already prints a versioned artifact straight
from the running binary with none of those things.

### The rendered output

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

## The vocabulary is closed at thirteen

This is the finding the slice rests on, and it was not obvious before reading
the detectors. Two of the nine detectors appear to emit whatever reason the
kubelet supplies — `Issue: w.Reason` in
[`imagepull.go`](../../../internal/diagnose/imagepull.go) and `Issue: "Init:" +
w.Reason` in [`initcontainer.go`](../../../internal/diagnose/initcontainer.go)
— which would make the set open-ended and the completeness check impossible.
They do not. Both are guarded:

```go
if w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull") {
```

So `DefaultDetectors` emits exactly thirteen kinds, and no more:

| Kind | Detector |
|------|----------|
| `CrashLoopBackOff` | `CrashLoopDetector` |
| `CreateContainerConfigError` | `ConfigErrorDetector` |
| `ErrImagePull` | `ImagePullDetector` |
| `ImagePullBackOff` | `ImagePullDetector` |
| `Init:CrashLoopBackOff` | `InitContainerDetector` |
| `Init:ErrImagePull` | `InitContainerDetector` |
| `Init:ImagePullBackOff` | `InitContainerDetector` |
| `Init:OOMKilled` | `InitContainerDetector` |
| `OOMKilled` | `OOMKilledDetector` |
| `ProbeFailure` | `ProbeFailureDetector` |
| `RestartLoop` | `RestartLoopDetector` |
| `Unschedulable` | `PendingDetector` |
| `VolumeAttachError` | `VolumeAttachDetector` |

Thirteen entries is the whole of slice 1's content.

## The package

`internal/knownissues` joins the strictest layering class in the repository —
alongside `internal/jsonschema`, `internal/dashboard`, `internal/baseline` and
`internal/glob`. It **imports nothing from kubeagent at all, and nothing outside
the standard library.** Reaching `internal/remediate` or `internal/explain` is
impossible by construction rather than forbidden by rule.
`internal/knownissues/imports_test.go` enforces both halves, on the pattern
`internal/baseline/imports_test.go` established. It holds no client and no
context, issues no cluster call and makes no model call — two separate promises,
and neither implies the other.

```go
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

func All() []Entry                        // every entry, sorted by Kind
func Kinds() []string                     // every Kind, sorted
func Lookup(kind string) (Entry, bool)    // exact match, no normalisation
```

`Lookup` does no normalisation — no case folding, no `Init:` stripping, no
fuzzy match. The argument is compared to `Kind` byte for byte. Stripping the
`Init:` prefix and falling back to the base kind would be the tempting
convenience and it would be wrong: an init container failure is a different
failure mode from the same reason on a main container, because it blocks the pod
from ever starting. Each of the four `Init:` kinds carries its own entry saying
so.

### Content is Go, not a data file

The entries are a `[]Entry` slice literal in a Go source file. No YAML, no
JSON, no `embed`, no parser, and therefore no error path, no malformed-input
test, and no new dependency — `go.mod` and `go.sum` do not change. `go build`
rejects a malformed entry, and `gofmt` keeps the file tidy without a style
argument.

The alternative — a data file parsed at startup — buys a lower contribution
barrier, and that barrier is not actually lower: a contributor to this file
edits prose inside a struct, and writes no logic at all. It would also cost a
parse step, an error path on a file that ships inside the binary and therefore
cannot be malformed at runtime unless the build was already broken, and a
dependency decision the no-new-dependency rule forecloses.

## The completeness check

Three tests, in both directions. They are the reason this slice exists; without
them it is a second copy of `diagnostics.md` that drifts the same way the first
one does.

All three live in `internal/diagnose`, which imports `internal/knownissues`
freely in test files. Nothing is added to `internal/knownissues`'s own import
set.

**Static — a new kind cannot ship undocumented.** A `go/parser` walk of
`internal/diagnose`'s non-test `.go` files collects every string literal that
reaches an `Issue:` composite-literal field, including the left operand of a
`+` concatenation. A bare literal must be a `knownissues` kind. A literal ending
in `:` is treated as a prefix and must have at least one kind beneath it. The
repository already walks itself this way in `TestNoProductionImport`.

**Behavioural — the composed kinds are covered too.** A fixture table drives
each of the nine detectors to produce each of the thirteen kinds and asserts
`Lookup` finds every one. This is what covers `"Init:" + w.Reason`, which the
parser can see the halves of but cannot compose. It uses the fake pods
`helpers_test.go` already provides; no cluster, no fake clientset needed —
detectors are pure functions.

**Reverse — the reference cannot outlive the code.** Every `knownissues` entry's
`Kind` must appear in the behavioural table. An entry documenting a kind no
detector emits any more is a test failure, so deleting a detector forces
deleting its entry.

## The command

`internal/cli/knownissues.go`, registered in the command tree beside
`newSchemaCommand`. It takes **no flags at all** — not even `--kubeconfig`.

CLAUDE.md's CLI invariant currently reads "`--kubeconfig` appears on eight
commands, and two of the remaining ones deliberately do not accept it". That is
accurate today — verified: `watch`, `tui`, `scan`, `rbac`, `mcp`, `gate`,
`fleet` and `baseline` register it. This command makes the count of deliberate
abstainers **three**, and the invariant text moves with it.

Argument handling copies `runSchema` exactly, and for the reason stated there:
`cobra.MaximumNArgs(1)` would reword an error the command already produces well.
`Args: cobra.ArbitraryArgs`, `SilenceErrors`, `SilenceUsage`, validation in
`RunE`.

- no argument → the list, then `Print one:\n  <invokedAs> known-issues <kind>`
- one argument → the entry, or an error
- more than one → `usage: <invokedAs> known-issues [kind]`

`ValidArgsFunction` completes kind names, so `kubeagent known-issues <TAB>`
works in every shell `kubeagent completion` supports. Deliberately **not**
`ValidArgs`, which would make Cobra validate the argument itself and produce its
own wording — the same rule that keeps validation in `RunE`.

### The unknown-kind path

```go
return fmt.Errorf("unknown issue kind %q (kubeagent documents %s)", name, strings.Join(...))
```

`%q` renders a Go string literal, so a control byte spliced into the argument
prints as `\x1b` rather than reaching the terminal. That is the same escape
`schemadoc.Generate` already relies on for an unknown schema name, and it is why
this command needs no `internal/safetext` import — which in turn is what lets
`internal/knownissues` keep its stdlib-only wall.

The message is honest about coverage rather than implying the kind is invalid:
this reference documents the deterministic detector set, and a kind from
elsewhere in kubeagent is simply not in it yet. It points at `diagnostics.md`
for the rest.

## Content rules

**Placeholders only.** Every `Checks` line uses `<namespace>`, `<pod>`,
`<container>`. No real object names, no addresses, no hostnames. A documentation
address, if one were ever needed, is RFC 5737; an example domain is RFC 2606.

**One host.** The only URLs are `https://k8sproject.top/features/diagnostics/#…`
— the project's own site, which is the standing exception to the rule that a
URL is a credential. A test pins the host: any `Docs` value that is non-empty
must begin with `https://k8sproject.top/`.

**Deliberately not tested: that each anchor resolves.** mkdocs slugifies `###
ImagePullBackOff / ErrImagePull` into a single anchor covering two kinds, so the
mapping is not one-to-one, and a Go test that reimplemented mkdocs'
slugification would be a second source of truth about a third one. The host
check is the guarantee; a broken anchor lands the reader on the right page.

**Checks are read-only.** Every suggested command is a `get`, `describe`,
`logs`, `top` or an inspection of the spec the operator already has. Nothing in
this reference suggests a write. This is a property of the prose, enforced by
review rather than by a test — a test that pattern-matched kubectl verbs would
give false confidence about English sentences.

## What this slice does not do

- **`scan`'s output does not change.** `internal/report/testdata/golden-scan.txt`
  stays byte-identical, so the demo GIF and `website/docs/quickstart.md` do not
  move.
- **No JSON document changes.** No new field on `findings.Finding`, no ninth
  versioned document, and none of the eight `schemaVersion` values moves. `scan`
  stays 1.2, `gate` stays 1.1. `known-issues` prints text only; a machine-readable
  form is not needed to answer "what does this mean" and would be a ninth
  contract to keep.
- **It gates nothing.** There is nothing here for `gate` to fail on. The gating
  question — answered as "the rule's own level decides, exactly as
  `internal/policy` does today" — belongs to slice 2, which is where rules exist.
- **It covers thirteen kinds, not thirty.** `NoEndpoints`, `RolloutStuck`,
  `JobFailed`, `FailedCreate`, `RestartRateDeviation` and the rest of what the
  wider repository emits are a later slice. The unknown-kind message says so
  rather than implying the operator mistyped.
- **No LLM call, ever.** This is a static lookup. It must never be confused with
  `--explain`, which is the model path — and the two must not be blurred in help
  text, docs or commit messages. "Offline" here means offline.

## Slice 2, for context only

The library half of the roadmap item is a **curated pack of `internal/policy`
rules**, chosen because the trust boundary already exists and is already fuzzed:
the evaluator is pure, is handed bytes rather than a client, has a closed
ten-operator set, and selects only the 23 kinds
`TestSelectableKindsMatchesRBACProfileCore` pins to `rbacprofile.coreRules`. A
contributed rule can therefore not reach the cluster, cannot write, cannot make
a model call, and cannot require an RBAC grant beyond `core` — none of which
needs new machinery to promise.

Its honest limit, worth writing down now so slice 2 does not have to rediscover
it: policy sees configuration, not runtime. It cannot read events, container
logs, or anything time-windowed, so a pack rule can say "this Deployment has no
resource limits" and can never say "this container is crash-looping". The pack
is a best-practices library, not a second detector set, and calling it one would
be the blurring this project keeps refusing to do elsewhere.

Slice 2 is out of scope here and gets its own spec.

## Testing

- `internal/knownissues/imports_test.go` — no kubeagent import; no import
  outside the standard library. Both halves, on `internal/baseline`'s pattern.
- `internal/knownissues/knownissues_test.go` — `All()` is sorted by `Kind` and
  has no duplicate `Kind`; `Kinds()` agrees with `All()`; `Lookup` finds every
  kind and misses an unknown one; every entry has a non-empty `Summary`,
  `Detail`, at least one `Cause` and at least one `Check`; every non-empty
  `Docs` begins with `https://k8sproject.top/`; and the credential-marker test —
  no entry's prose carries an address or a hostname. `k8sproject.top` is the one
  permitted host and appears only in `Docs`, so the marker test scans `Summary`,
  `Detail`, `Causes` and `Checks` and treats a host anywhere in them as a
  failure. Splitting the fields this way is what keeps the test from having to
  special-case its own allowed value.
- `internal/diagnose/knownissues_test.go` — the three completeness tests above.
- `internal/cli/knownissues_test.go` — the list output names every kind; a known
  kind prints its detail; an unknown kind errors, names what was passed in `%q`
  form, and a control byte in the argument does not survive into the message;
  two arguments produce the usage error; the command registers no flags.

## Documentation

- `website/docs/features/known-issues.md` — a new page: what the command is, the
  thirteen kinds, why the reference is machine-checked against the detectors, and
  the explicit statement that it makes no model call and needs no cluster.
- `website/docs/features/diagnostics.md` — a pointer to the new command from the
  "Failure modes detected" section, so a reader on the site learns the offline
  form exists.
- `mkdocs.yml` — the new page in the nav.
- `CHANGELOG.md` — an `### Added` entry under `[Unreleased]`.
- `CLAUDE.md` — the `--kubeconfig` count moves from two abstainers to three; a
  roadmap bullet records the slice; `internal/knownissues` joins the list of
  packages that import nothing from kubeagent.
- `website/docs/roadmap.md` — post-1.0 item 3 marked as slice 1 shipped.
