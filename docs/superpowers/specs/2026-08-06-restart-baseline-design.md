# Learned restart-rate baseline — design

**Status:** approved, ready to plan
**Date:** 2026-08-06
**Roadmap:** post-1.0 item 1 — anomaly/baseline learning, "what's normal for *this* cluster"
([website/docs/roadmap.md:559](../../../website/docs/roadmap.md))

This is slice 1 of that item. It ships one dimension end to end and names the
rest as deferred. Themes A–H are complete; this is the first post-1.0 feature.

---

## 1. The problem

Every detector kubeagent ships fires on a *concrete, named failure mode*:
CrashLoopBackOff, OOMKilled, ImagePullBackOff. That is deliberate — it is why
`internal/findings` maps every `diagnose.Finding` to `Critical` without asking
([internal/findings/findings.go:91-101](../../../internal/findings/findings.go)).

But an operator's real question is often comparative, not absolute: *this*
workload restarts more than it used to. No detector can answer that, because a
detector sees one pod at one instant and has no notion of what this cluster
usually looks like.

kubeagent has no durable memory of any kind. `internal/watchstate`,
`internal/slo`, `internal/alertstate` and `internal/oncall` all keep state in
process memory and reset on restart — `internal/slo/slo.go:243-244` says so in a
comment. There is exactly one file write in the repository,
`internal/cli/scan.go:336`, the explicitly-flagged `--audit-log` for
`--fix`/`--rollback`. The Helm chart declares one volume and it is a read-only
Secret mount for a multicluster kubeconfig; there is no PersistentVolume and no
emptyDir anywhere under `deploy/`.

So "what's normal" cannot come from anything kubeagent remembers today.

## 2. The shape: an operator-captured artifact, not daemon memory

**Decision.** A new one-shot command reads the cluster once and prints a small
JSON document describing what it saw. The operator reviews that document and
commits it. `scan --baseline <file>` and `gate --baseline <file>` compare the
live cluster against it. A pure package does the comparison.

**Alternatives closed off:**

- *Daemon-memory rolling window.* Extend the `watchstate`/`slo` pattern so the
  watch daemon learns rates over a rolling window and flags outliers. Rejected:
  it resets on every daemon restart or rollout, so a multi-day "normal" is
  unreachable, and nothing outside the daemon — `scan`, `gate`, the MCP server —
  can use what it learned.
- *Daemon writes a ConfigMap.* A genuinely learned-over-days baseline that
  survives restarts. Rejected: it makes the watch daemon **write to the
  cluster**, which is the strongest invariant in the project and one the docs
  state plainly. It would need its own guard-rail regime the way `--fix` has one.
  Not a slice-1 mechanism, and possibly never.
- *A file the daemon writes behind a flag.* Rejected: in-cluster it needs an
  emptyDir (which dies with the pod, so no better than memory) or a PVC (a new
  chart surface and a new class of failure).

**Why the artifact wins here.** It keeps kubeagent read-only toward the cluster
with no new mechanism at all. The learned data names namespaces and workloads,
so making it a file the operator *reads before it goes anywhere* is the right
default for something that will end up in a git repository and a CI log. And it
is the same shape the project already uses for `--policy`: an operator-owned
document in, a pure evaluator, findings out.

## 3. Naming

**Decision.** The command is `kubeagent baseline capture`; the flag is
`--baseline <path>` on `scan` and `gate`; the package is `internal/baseline`.

Two collisions were checked:

- `--profile` is **taken**: `rbac print --profile scan|watch|full` and
  `rbac check --profile` ([internal/cli/rbac.go:83](../../../internal/cli/rbac.go),
  `:154`). "Profile" is therefore not available.
- "Baseline" is taken **in prose only**. `README.md:141`,
  `website/docs/features/diagnostics.md:316`, `website/docs/roadmap.md:74` and
  `docs/superpowers/specs/2026-07-13-expected-node-baseline-design.md` call
  `--expected-nodes` the "Expected-node baseline". There is no `baseline`
  command and no `--baseline` flag today.

**Decision.** Take the word. Reword the shipped prose from "Expected-node
baseline" to "expected-node list", which is more accurate anyway — that feature
is an admin-declared static list of node names, nothing learned. **The
`--expected-nodes` flag and `KUBEAGENT_EXPECTED_NODES` are not renamed**, so
nothing breaks and no deprecation is owed. The spec file under
`docs/superpowers/specs/` is historical and is left alone.

This also matches what the docs already call the missing feature: the roadmap
says "anomaly/baseline learning", and
`website/docs/features/ci-gate.md:341` records that `gate` has "No baseline/diff
mode (comparing this run against a previous one)" — a line this slice makes
false and must therefore update.

## 4. `internal/baseline` — a stdlib-only pure package

**Decision.** `internal/baseline` **imports nothing from kubeagent at all** —
only the standard library. That puts it in the same class as
`internal/dashboard` and `internal/jsonschema`, where reaching
`internal/remediate` or `internal/explain` is impossible by construction rather
than by rule.

It is stronger than `internal/policy`, which is pure but imports
`k8s.io/apimachinery`'s `unstructured`. `baseline` does not need a Kubernetes
type: the caller reduces pods to a flat sample struct.

Enforced by `internal/baseline/imports_test.go`, modelled on
`internal/dashboard/imports_test.go:29-48`, which walks the package with
`go/parser` and fails on any `github.com/imantaba/kubeagent/` import.

### Types

```go
// PodSample is one pod's contribution, already resolved to its workload.
type PodSample struct {
    Kind       string  // "Deployment" | "StatefulSet" | "DaemonSet" | "Job" | "CronJob" | "Pod"
    Namespace  string
    Name       string  // the WORKLOAD's name, not the pod's
    Restarts   int     // sum of ContainerStatus.RestartCount across the pod's containers
    AgeSeconds float64 // now - pod start
}

// Entry is one workload's learned normal.
type Entry struct {
    Kind            string  `json:"kind"`
    Namespace       string  `json:"namespace"`
    Name            string  `json:"name"`
    RestartsPerHour float64 `json:"restartsPerHour"`
    Pods            int     `json:"pods"`            // pods that counted
    ObservedSeconds float64 `json:"observedSeconds"` // total pod-seconds behind the rate
}

// Document is the artifact. A published, versioned JSON document — see §7.
type Document struct {
    SchemaVersion    string  `json:"schemaVersion"`
    CapturedAt       string  `json:"capturedAt"`       // RFC3339 UTC
    MinPodAgeSeconds float64 `json:"minPodAgeSeconds"`
    Workloads        []Entry `json:"workloads"`
}

// Deviation is one workload whose current rate is abnormal for this cluster.
// Tagged because Report is embedded in report.ScanReport and therefore lands in
// `scan --output json` and in the published scan schema.
type Deviation struct {
    Kind         string  `json:"kind"`
    Namespace    string  `json:"namespace"`
    Name         string  `json:"name"`
    BaselineRate float64 `json:"baselineRestartsPerHour"`
    CurrentRate  float64 `json:"currentRestartsPerHour"`
    Pods         int     `json:"pods"` // pods behind CurrentRate
}

// Report is what Compare returns.
type Report struct {
    // Deviations is always non-nil: a run that found nothing encodes
    // "deviations": [], which says the comparison happened, where an absent key
    // would not.
    Deviations      []Deviation `json:"deviations"`
    Compared        int         `json:"compared"`        // present in both the document and the cluster
    NotInBaseline   int         `json:"notInBaseline"`   // in the cluster, absent from the document
    GoneFromCluster int         `json:"goneFromCluster"` // in the document, absent from the cluster
}

// CompareOptions tunes the deviation rule. A zero field takes its default.
type CompareOptions struct {
    Factor float64 // default 3.0
    Floor  float64 // default 0.5 restarts/hour
}
```

`CompareOptions`'s zero-takes-default convention copies
`watchstate.Options` ([internal/watchstate/watchstate.go:55-68](../../../internal/watchstate/watchstate.go)).

### Functions

```go
func Capture(pods []PodSample, minPodAge time.Duration, now time.Time) Document
func Compare(doc Document, pods []PodSample, opts CompareOptions) Report
func Load(b []byte) (Document, error)
func (d Document) Marshal() ([]byte, error)
```

**Decision: the minimum-pod-age filter lives inside the package, not in the
caller.** `Capture` applies `minPodAge` and records it in the document;
`Compare` applies `doc.MinPodAgeSeconds`. Both sides therefore filter
identically by construction. The alternative — a caller that filters before
building samples — makes symmetry a matter of discipline, and a capture and a
compare run with different floors would silently produce garbage. That is the
kind of coupling this project pins in code rather than in a comment.

`now` is passed in rather than read, matching `RestartLoopDetector`'s injected
instant ([internal/diagnose/defaults.go:8-12](../../../internal/diagnose/defaults.go))
and `watchstate`'s "the caller passes now".

## 5. The rate, and what it honestly measures

`ContainerStatus.RestartCount` is cumulative over a pod's lifetime. The rate is
pod-hours normalised across a workload's pods:

```text
RestartsPerHour = sum(restarts of counted pods) / (sum(age of counted pods) / 3600)
```

A pod is **counted** only if `AgeSeconds >= MinPodAgeSeconds`. Its restarts are
excluded from the numerator along with its seconds from the denominator — a
30-second-old pod with 2 restarts implies 240 restarts/hour and would swamp
everything. Default floor **1 hour**.

A workload with zero counted pods produces no entry: it is unknown, not zero.

**Honesty clause — load-bearing, and it goes in the docs, not only here.** This
measures restarts over the lifetimes of the pods *present when the sample was
taken*. It is **not** long-term history. A workload whose pods were all
recreated an hour before capture shows only what those pods have done since.
The project already writes this kind of limit down where it exists —
`internal/capacity/capacity.go:6-10` states that metrics-server "never claims a
peak: … a single sample of roughly a 30-second average and retains no history".
The baseline docs carry the equivalent sentence, prominently, and the
`baseline capture` help text carries a short form of it.

## 6. The deviation rule

A workload deviates when **both** hold:

```text
current >= baseline * Factor        (default 3.0)
current - baseline >= Floor         (default 0.5 restarts/hour)
```

Two thresholds, no magic, no statistics. The multiplicative test catches "this
got much worse relative to itself"; the absolute floor is what stops
`0.001 → 0.01` from being a 10× alarm, and it is also what carries the
baseline-is-zero case, where the multiplicative test is trivially true.

Only increases are deviations. A workload that restarts *less* than its baseline
is not reported: nobody is paged for a thing improving, and reporting it would
double the section for no decision it supports.

- Workloads **in the cluster but not in the document** are counted in
  `NotInBaseline` and not flagged. They are new since capture; the baseline
  simply has nothing to say.
- Workloads **in the document but gone from the cluster** are counted in
  `GoneFromCluster` and not flagged. That is inventory drift, deliberately
  deferred (§10) — and `--drift`/`KUBEAGENT_DRIFT` already means GitOps drift,
  so the word is spoken for.

Deviations sort by `Kind/Namespace/Name` so output is deterministic.

## 7. The document becomes the seventh versioned JSON document

**Decision.** `baseline` joins the published schema set at version **1.0**.

A file an operator writes today, commits, and feeds back to a different
kubeagent build in six months is exactly the thing the other six versions exist
to protect. The machinery is already generic: `internal/jsonschema` reflects
over a Go type and `internal/schemadoc` publishes it.

Concretely:

- `internal/jsonschema`: add `BaselineVersion = "1.0"` beside `ScanVersion`,
  `GateVersion`, `RBACVersion`, `WatchVersion`
  ([internal/jsonschema/jsonschema.go:27-32](../../../internal/jsonschema/jsonschema.go)).
- `internal/schemadoc`: add a seventh entry to `Documents`
  ([internal/schemadoc/schemadoc.go:41-74](../../../internal/schemadoc/schemadoc.go))
  with `Name: "baseline"`, `Surface: "baseline"`,
  `Root: reflect.TypeOf(baseline.Document{})`.
- Published as `website/docs/schemas/baseline-v1.json`, matching the existing
  file naming (`scan-v1.json`, `gate-v1.json`, …).
- `kubeagent schema baseline` prints it with no cluster and no kubeconfig —
  automatic, since the command reads `schemadoc.Documents`.
- `CLAUDE.md`'s "The six JSON documents are a versioned contract" invariant
  becomes **seven**, naming `baseline.Document`.

`internal/schemadoc` may import `internal/baseline` freely: the invariants
constrain what the walled packages import, not who imports them, and `schemadoc`
already reaches `remediate` and `explain` transitively by design.

`Load` accepts any document whose `schemaVersion` has the **same MAJOR** and
rejects a different MAJOR with a named error. That matches the schemas' own
stated contract: `additionalProperties` is unset on purpose, and "a document
must still validate against an older schema of the same MAJOR".

### Schema moves in this slice

- **scan 1.1 → 1.2 (additive).** `report.ScanReport` gains
  `Baseline *baseline.Report \`json:"baseline,omitempty"\``. An added optional
  property plus a new `$defs` subtree is additive under `classify()`
  ([internal/schemadoc/schemadoc_test.go:230-283](../../../internal/schemadoc/schemadoc_test.go)),
  exactly like `policy` in scan 1.0 → 1.1. Regenerate with
  `go test ./internal/schemadoc -run TestSchemaDrift -update`.
- **gate stays 1.1.** `findings.Finding` already carries
  `Level/Kind/Namespace/Name/Issue/Reason`
  ([internal/findings/findings.go:72-80](../../../internal/findings/findings.go)),
  which is everything a deviation needs. Deviations land in the existing
  `Failing`/`Reported` arrays, so `gate.Verdict` gains no key and its schema does
  not move. `policy` needed `PolicyNotEvaluated` because "a rule that never ran"
  had no representation as a finding; a baseline has no analogous case — an
  unreadable or malformed `--baseline` file is a load error that exits non-zero
  with a message on stderr, before any verdict exists.
- **watch and rbac do not move.** The daemon is untouched in this slice.

## 8. Wiring

### `kubeagent baseline capture`

New file `internal/cli/baseline.go`, following `internal/cli/policy.go` and
`internal/cli/rbac.go`.

```text
kubeagent baseline capture [--kubeconfig PATH] [--context NAME]
                           [--namespace NS] [--min-pod-age DURATION]
```

Prints the JSON document to **stdout**. No `--output` flag: the document is JSON
by definition, the way `kubeagent schema` emits one thing. Printing to stdout
rather than writing a file keeps "the only file write in the repository is
`--audit-log`" true, and matches `rbac print`. The operator redirects:

```bash
kubeagent baseline capture > cluster-baseline.json
```

Reads: it reuses `internal/collect` and `internal/inventory.Build` to resolve
pods to their controlling workloads — the same lists `scan` already makes. No
new RBAC grant is needed and `internal/rbacprofile`'s `Feature` table is
untouched, because every read is already in the `scan` profile's core rules.

Sample construction lives in the CLI layer, not in `internal/baseline`: walk
`inventory.Result.Workloads` for `Kind/Namespace/Name` and the pod names under
each, look each pod up in `inventory.Inputs.Pods` for
`Status.StartTime` and its containers' `RestartCount`, and emit one `PodSample`
per pod. `inventory.PodRow.Age` is a humanised string ("3d") and is **not**
usable here — the age must come from the raw pod. **No change to
`inventory.PodRow` or `inventory.Workload`.**

`--min-pod-age` defaults to `1h`, with `KUBEAGENT_BASELINE_MIN_POD_AGE`
following the `envDur` pattern in `internal/cli/helpers.go:24-77`.

### `scan --baseline`

```text
--baseline PATH            compare restart rates against this captured baseline
--baseline-factor FLOAT    deviation multiplier         (default 3.0,  KUBEAGENT_BASELINE_FACTOR)
--baseline-floor FLOAT     minimum absolute increase    (default 0.5,  KUBEAGENT_BASELINE_FLOOR)
```

`report.Input` gains `Baseline *baseline.Report`; `report.ScanReport` gains the
matching `omitempty` key. The text renderer adds a section:

```text
Baseline deviations (confidence: medium — a learned rate, not a detector)

  Deployment prod/api        0.12 → 2.40 restarts/hour   (20x baseline, 3 pods)
  StatefulSet prod/cache     0.00 → 0.80 restarts/hour   (2 pods)

  42 workloads compared, 3 not in the baseline, 1 no longer present.
```

**The section renders only when `--baseline` was passed and a report exists.**
`internal/report/testdata/golden-scan.txt` is generated without the flag and
therefore stays **byte-identical** — no golden regeneration, no demo GIF
refresh, no `website/docs/quickstart.md` change.

### `gate --baseline`

Same three flags. `internal/findings` gains
`FromBaseline(*baseline.Report) []Finding`, mapping each deviation to:

- `Level: findings.Info`
- `Kind`/`Namespace`/`Name` from the deviation
- `Issue: "RestartRateDeviation"`
- `Reason:` the rates and the multiple, e.g.
  `"0.12 -> 2.40 restarts/hour (20x baseline, 3 pods)"`

`findings.Info` is the right level and needs no invention:
`internal/findings/findings.go:32-35` says Info is *"reserved: no detector emits
it yet, but `--fail-on info` must have a meaning"*. This is that meaning.

Because `gate --fail-on` defaults to `critical`
([internal/cli/gate.go:76](../../../internal/cli/gate.go)), a deviation **never
fails a gate by default**, and `--fail-on info` is the explicit opt-in. **No new
gate flag is required for the opt-in.**

`internal/findings` importing `internal/baseline` closes no cycle: `baseline`
imports nothing.

## 9. Confidence, and why a deviation is not a Finding

A learned rate is an inference. `internal/confidence` exists precisely to mark
that class, and its package doc says such a signal is "informational only (never
affects priority or the cluster verdict)"
([internal/confidence/confidence.go:1-5](../../../internal/confidence/confidence.go)).

So:

- A deviation is **not** a `diagnose.Finding`. The `Detector` interface, the nine
  detectors and `PodFacts` are **entirely untouched** by this slice. Detectors
  stay pure functions of one pod and its events.
- The scan text section states its confidence in its own heading, in the same
  vocabulary (`medium`).
- The cluster verdict is unchanged by a deviation. `clusterhealth` is untouched.
- Only `gate --fail-on info` lets a deviation change an exit code, and that is
  the operator saying so.

## 10. What slice 1 does not do

Named, not silently omitted:

- **No HTML, TUI, MCP or dashboard surface.** `scan --output html`, `kubeagent
  tui`, the MCP tools and the watch dashboard do not show deviations. Adding
  them is later work.
- **No watch-daemon integration.** The daemon does not load a baseline and
  `/issues` gains no field. `watch` and `internal/watchstate` are untouched.
- **No inventory drift.** Workloads that appeared or disappeared are counted,
  never flagged.
- **No second dimension.** Replica counts, per-namespace pod counts and
  resource usage against metrics-server all wait. Resource usage is
  specifically weak today: `internal/capacity` already documents that
  metrics-server returns one ~30-second average with no history.
- **No automatic capture.** Nothing writes a baseline on a schedule, and nothing
  updates one in place. Re-capturing is an operator action.
- **No multi-baseline merge or per-namespace file set.** One file, one cluster.

## 11. Testing

TDD throughout: failing test first, watch it fail, then implement.

- **`internal/baseline` unit tests** — pure, no cluster, no fixtures beyond
  literals: the pod-hours maths; the min-pod-age filter excluding a young pod
  from *both* numerator and denominator; a workload with zero counted pods
  producing no entry; the two-threshold rule including the baseline-is-zero case
  and the small-numbers case the floor exists to suppress; decreases not
  reported; `NotInBaseline`/`GoneFromCluster` counted and not flagged;
  deterministic ordering; `Load` round-tripping `Marshal`; `Load` rejecting a
  different MAJOR by name and accepting a higher MINOR.
- **`internal/baseline/imports_test.go`** — walks the package with `go/parser`
  and fails on any kubeagent import, modelled on
  `internal/dashboard/imports_test.go:29-48`.
- **A fuzz target** on `Load`, joining the seven that already exist from Theme H
  slice 3: no byte sequence may panic it. The document is operator-supplied and
  therefore semi-trusted, which is exactly the class already fuzzed. Seed corpus
  carries a valid document, a truncated one, a wrong-MAJOR one and a
  non-finite-float one — the DNS health parser's integer overflow came from a
  non-finite float, so it is a known shape of bug in this codebase.
- **CLI surface tests** — `internal/cli/surface_test.go` already table-tests
  flag parsing per command; the three new flags on `scan` and `gate` and the new
  `baseline capture` command get rows there, and `Normalize`'s single-dash shim
  is exercised for `-baseline`.
- **Schema drift** — `go test ./internal/schemadoc -run TestSchemaDrift` must
  classify the scan change as **additive** and the baseline document as new.
- **Golden output** — `internal/report/golden_test.go` must pass **unchanged**,
  proving the new section is genuinely conditional.
- **No new fixture may name a real cluster.** Namespaces and workload names in
  tests and docs are generic (`prod`, `api`, `cache`); no node names, no context
  names, no paths.

The chaos harness is **not** extended in this slice. Nothing here touches
cluster writes, `nodes/proxy`, RBAC or the daemon, so the pre-release gate is the
lightweight real-cluster smoke, not the full outage suite.

## 12. Security and forwarding

- The baseline document contains namespaces, workload names and numbers. Those
  are DNS-1123-validated fields the API server itself constrains, so they do not
  pass through `internal/safetext` — that seam is for fields the API server does
  **not** validate. No free-form message, event text, log line, node name,
  kubeconfig path or context name enters the document.
- The document is nonetheless cluster-shaped data an operator will commit and CI
  will print. Making capture an explicit operator action whose output goes to
  stdout — reviewable before it goes anywhere — is the mitigation, and the docs
  say so.
- No URL is emitted anywhere in this feature.
- `kubeagent baseline capture` is **read-only toward the cluster** (`list` only)
  and makes **no LLM call**. Those are two separate promises and the docs state
  them separately.

## 13. Global constraints

Inherited, non-negotiable, and carried verbatim into the implementation plan:

- `go.mod` and `go.sum` **must not change**. `internal/baseline` is stdlib-only.
- `internal/report/testdata/golden-scan.txt` stays **byte-identical**. Do **not**
  regenerate the demo GIF or `website/docs/quickstart.md`.
- The Helm chart, `deploy/`, `internal/rbacprofile`'s `Feature` table and every
  generated RBAC manifest are untouched.
- Detectors stay pure functions; `internal/diagnose` is not modified.
- `internal/watch`, `internal/watchstate`, `internal/slo`, `internal/tui`,
  `internal/mcp`, `internal/dashboard` and `internal/htmlreport` are not
  modified.
- `internal/baseline` imports nothing from kubeagent. `internal/findings` and
  `internal/report` may import it; it may import neither of them, nor
  `internal/scan`, `internal/remediate`, `internal/explain` or
  `internal/investigate`.
- Every commit needs a `Signed-off-by` trailer matching its author
  (`git commit -s`) — `main` enforces DCO. Verify with
  `bash scripts/dco-check.sh main HEAD`. **No `Co-Authored-By` trailer and no AI
  attribution anywhere** — commits, code, comments, docs, changelog.
- No secrets, credentials, private IPs or internal hostnames anywhere — code,
  tests, fixtures, help text, schemas, docs. RFC 5737 addresses
  (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`), RFC 2606 domains
  (`example.com`, `example.org`, `example.net`). Node names, kubeconfig context
  names and kubeconfig paths are credentials.
- Never expose API keys to the shell.
- `go test` runs locally with `-p 2`, never `-short`; CI runs
  `go test -race ./...`.
- Work never lands on `main` directly. This slice is on branch
  `restart-baseline`, cut off `main` at `70fcd8c`.

## 14. Documentation

- **New:** `website/docs/features/baseline.md` — what it measures, the honesty
  clause from §5 stated prominently, the capture/compare workflow, the two
  thresholds, the deferred surfaces from §10.
- **New:** `website/docs/schemas/baseline-v1.json`, generated.
- `website/docs/features/ci-gate.md:341` — the "No baseline/diff mode" line is
  now false for restart rates; reword it to name what is and is not covered.
- `README.md:141`, `website/docs/features/diagnostics.md:316`,
  `website/docs/roadmap.md:74` — "Expected-node baseline" becomes
  "expected-node list".
- `website/docs/roadmap.md` — the post-1.0 row records that anomaly/baseline
  learning has begun with the restart-rate dimension, and names what is still
  ahead. It must **not** claim more than this slice ships.
- `CLAUDE.md` — "six JSON documents" becomes seven; add `internal/baseline` to
  the layering invariants as an imports-nothing package alongside
  `internal/dashboard` and `internal/jsonschema`.
- `CHANGELOG.md` under `## [Unreleased]`.
- `docs/go-concepts.md` — this slice introduces no new Go concept the
  cheat-sheet lacks; add an entry only if the implementation reaches for one.

## 15. Acceptance

The slice is done when all of the following hold on the branch:

1. `go build ./...` and `go test -p 2 ./...` pass.
2. `internal/report/testdata/golden-scan.txt` is unchanged from `main`.
3. `git diff main -- go.mod go.sum` is empty.
4. `go test ./internal/schemadoc -run TestSchemaDrift` passes, having classified
   scan's change as additive, with `scan` at 1.2 and `baseline` at 1.0.
5. `kubeagent schema baseline` prints the new schema with no cluster reachable.
6. Against a live disposable cluster: `kubeagent baseline capture` prints a
   document; `kubeagent scan --baseline <that file>` reports zero deviations
   immediately afterwards; and after a workload is made to restart repeatedly,
   the same command reports it.
7. `kubeagent gate --baseline <file>` exits 0 with a deviation present, and
   exits non-zero with `--fail-on info`.
8. `(cd website && mkdocs build --strict)` exits 0 with no page warnings.
9. `bash scripts/dco-check.sh main HEAD` passes.
