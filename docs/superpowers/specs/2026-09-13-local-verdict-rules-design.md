# Local `--investigate`: rules decide the candidates — design

Date: 2026-09-13. Status: approved direction, spec for planning.

## Summary

Local verdict mode today hands the model three things: the scan's root-cause
candidates, the fresh evidence kubeagent read, and the job of weighing one
against the other. That job is the one small models fail. Four fine-tunes of
a 0.6B model and a zero-shot 8B model all fail the same test: they do not let
a fresh read beat the candidate text. Best result so far is 4 of 10 paired
rows; the 8B scores 1 of 10.

This slice moves that job into rules. A new pure package,
`internal/hypothesis`, re-checks every surviving candidate against the objects
the gather already fetched and returns one of three outcomes per candidate:
confirmed, refuted or unverified. The first confirmed candidate in trace order
is the workload's cause. Rules also write the "N workloads share one cause"
line. The model keeps two jobs: the rationale on every row, and the cause on
rows where no candidate survived.

Every verdict row now says where its cause came from: `rule` or `model`.

Nothing else moves. Scan output stays byte-identical, scan stays at schema
1.8, verdict contract v1 is unchanged, the read budget is unchanged, the tool
loop mode is untouched, and no dependency is added.

## Decisions (locked with the operator)

1. **Scope: the three candidate kinds that exist.** `node`, `pvc` and
   `registry` are the kinds `internal/rootcause` emits and the gather already
   reads. Node pressure, DNS, and deny-all NetworkPolicy candidates do not
   exist yet; they are slice 2 with their own spec. The package is shaped so
   a new kind costs one rule and one read.
2. **Mixed naming, labelled per row.** Rules name the cause on every row that
   has a surviving candidate. The model names the cause only on rows with no
   candidate left. Each row is labelled `rule` or `model`.
3. **Rules write the shared line.** The summary opens with a fixed line from
   the rules — "N workloads share one upstream cause: …" or "no shared cause
   among …". The model's summary lines follow.
4. **Verdict contract v1 stays.** The model still answers a cause on every
   row. kubeagent ignores that cause on rule rows. The prompt gains one
   `fresh read` line per candidate and one `decided by rules` line per rule
   row. The doc says plainly that a rule row's cause never comes from the
   model.
5. **A pure verifier package fed by a structured gather.** `internal/hypothesis`
   holds no client, no context and no I/O. The gather keeps the objects it
   already fetches and hands them over. Verdict mode calls the verifier after
   the gather and before the prompt.
6. **No new cluster read.** The verifier only sees what the gather read
   under the existing budget of 8. A candidate the gather could not read
   stays unverified.

## Data flow

Today: scope, gather, prompt, one model call, render.

New: scope, gather, **check**, prompt, one model call, render.

```
flaggedScope(workloads)               unchanged, at most 10 workloads
gatherEvidence(ctx, client, scoped)   same reads, now also returns hypothesis.Reads
hypothesis.Decide(w, reads)           one Result per scoped workload, pure
hypothesis.Shared(results)            the fixed summary lines, pure
buildVerdictPrompt(..., results)      candidates gain the fresh-read lines
c.call(ctx, prompt)                   one /chat/completions call, unchanged
renderVerdicts(doc, results, ...)     rule rows override, every row labelled
```

## The package: `internal/hypothesis`

### Purpose

Given one workload's candidate list and the objects the gather read, say per
candidate whether a fresh read confirms it, refutes it, or could not check
it. Then name the workload's cause when a rule can.

### Walls

- Imports only the standard library, `k8s.io/api`, and `internal/inventory`.
- Never imports `k8s.io/client-go` or `internal/cluster`, so "holds no
  client" is structural, the way `internal/fleetfile` is.
- Never imports `internal/investigate`, `internal/remediate`,
  `internal/explain`, `internal/report` or `internal/scan`.
- `internal/hypothesis/imports_test.go` pins all of that, on the pattern of
  `internal/baseline/imports_test.go`.
- No `context.Context`, no `io`, no `time.Now`. Every function is a pure
  function of its arguments.
- It never returns an error. A rule that cannot decide says `unverified` and
  says why.
- No API text reaches an evidence sentence. Sentences are built from fixed
  words, the candidate's own cause text (already sanitized by `rootcause`),
  and kubeagent's own literals. The one outside string — a failed read's
  reduced error — is sanitized by the gather before it enters `Reads`.

### Input: `Reads`

```go
// Reads is what the gather fetched, keyed the way the rules look things up.
// A missing key means the read was not made (the budget ran out first).
type Reads struct {
    Nodes  map[string]*corev1.Node                  // by node name
    PVCs   map[string]*corev1.PersistentVolumeClaim // by "namespace/name"
    Events map[string][]corev1.Event                // by "namespace/pod"
    // Failed holds one reduced, sanitized error per read that was attempted
    // and failed, keyed "node/<name>", "pvc/<namespace>/<name>" or
    // "events/<namespace>/<pod>".
    Failed map[string]string
}
```

### Output: `Decision` and `Result`

```go
type Outcome string

const (
    Confirmed  Outcome = "confirmed"
    Refuted    Outcome = "refuted"
    Unverified Outcome = "unverified"
)

// Decision is one candidate re-checked against a fresh read.
type Decision struct {
    Candidate inventory.Hypothesis
    Outcome   Outcome
    Evidence  string // one fixed-shape sentence
}

// Result is one workload after the rules ran.
type Result struct {
    Workload  string     // "namespace/name"
    Decided   bool       // a rule named the cause
    Cause     string     // the deciding candidate's Cause, verbatim; "" when !Decided
    Outcome   Outcome    // Confirmed or Unverified when Decided; "" otherwise
    Evidence  string     // the deciding candidate's sentence
    GroupKey  string     // what the cause points at; set only when Decided && Confirmed
    GroupText string     // the cause text the shared line prints for GroupKey
    Decisions []Decision // one per surviving candidate, trace order
}

func Decide(w inventory.Workload, reads Reads) Result
func Shared(results []Result) []string
```

`Decide` is called once per scoped workload. `Shared` is called once over all
of them.

### Which candidates are re-checked

Every candidate in `w.RootCauseTrace` whose verdict is not `ruled_out`, in
trace order. That is the attributed one and every outranked one. A ruled-out
candidate is never re-checked; the scan already showed it does not apply to
this workload (no pod on the node, PVC not mounted, host below threshold).

Trace order is already node, then PVC, then registry, because that is the
order `rootcause` runs. So an outranked PVC can win when the attributed node
reads healthy. That is the point.

### The rules

**Node, reason `NotReady`.** Look up `reads.Nodes[name]`. Read the `Ready`
condition.

| Fresh read | Outcome | Evidence sentence |
|---|---|---|
| `Ready` is `True` | refuted | `Ready condition is True now` |
| `Ready` is `False` | confirmed | `Ready condition is False now` |
| `Ready` is `Unknown` | confirmed | `Ready condition is Unknown now` |
| no `Ready` condition | confirmed | `the node has no Ready condition` |

**Node, reason `kubelet not heartbeating` or `no kubelet lease`.** Same
lookup. The lease is not re-read, so a healthy-looking node cannot refute
this candidate; only the scan's lease check can.

| Fresh read | Outcome | Evidence sentence |
|---|---|---|
| `Ready` is `False` | confirmed | `Ready condition is False now` |
| `Ready` is `Unknown` | confirmed | `Ready condition is Unknown now` |
| no `Ready` condition | confirmed | `the node has no Ready condition` |
| `Ready` is `True` | unverified | `Ready condition is True, but the kubelet lease was not re-read` |

The node's reason is read from the candidate's `Cause` text, which
`rootcause.Annotate` writes as `node <name> (<reason>)`. A reason outside the
three `clusterhealth` writes is treated like `NotReady`.

**PVC.** Look up `reads.PVCs[namespace/name]`, where the namespace is the
workload's — a PVC candidate is always in the workload's own namespace.
Read `status.phase`.

| Fresh read | Outcome | Evidence sentence |
|---|---|---|
| `Bound` | refuted | `phase is Bound now` |
| `Pending` | confirmed | `phase is still Pending` |
| `Lost` | confirmed | `phase is Lost` |
| anything else or empty | unverified | `phase is not one kubeagent expects` |

The candidate's diagnosed reason (`ProvisioningFailed`, `MissingStorageClass`,
…) is kept from the trace, not re-derived. The PVC's own state says whether
the claim is still broken; why it is broken stays the scan's answer.

**Registry.** No object was read for a registry candidate, and none is now.
The rule reads the pull events of the pod that carries the workload's first
`ImagePullBackOff` or `ErrImagePull` finding, in `Findings` order — the same
pod `rootcause.AnnotateRegistry` took the image from. Look up
`reads.Events[namespace/pod]`.

A pull event is one whose `Reason` is `Failed` or `BackOff` and whose
`Message` contains `pull` (case-insensitive). Matching runs on the **raw**
message, case-insensitive, against three closed lists of kubeagent literals.
Any pull event is enough; order does not matter. Precedence runs across all
of the pod's pull events, not inside one: a connection literal in any event
wins, then an auth literal in any event, then an image literal in any event.

| Any pull event matches | Class | Outcome | Evidence sentence |
|---|---|---|---|
| `dial tcp`, `i/o timeout`, `connection refused`, `connection reset`, `no such host`, `network is unreachable`, `tls handshake`, `x509:`, `502 bad gateway`, `503 service unavailable`, `504 gateway timeout`, `toomanyrequests`, `429 too many requests` | connection | confirmed | `a pull event shows a connection error: <literal>` |
| else `unauthorized`, `denied`, `no basic auth credentials`, `pull access denied` | auth | unverified | `a pull event shows an auth error: <literal>; that can be one image or the whole host` |
| else `manifest unknown`, `not found`, `name unknown`, `repository does not exist`, `invalid reference format` | image | refuted | `a pull event shows an image error: <literal>; this pull fails for this image, not the host` |
| no pull event, or none matches | — | unverified | `no pull event names the failure; events may have aged out` |

Precedence is connection, then auth, then image. A host that is down explains
an image error but not the other way round. An auth error sits in the middle
on purpose: a rotated pull secret breaks every workload on a host, a wrong
secret breaks one, and the reads kubeagent has cannot tell them apart. So the
rule does not claim it can. Docker Hub's "pull access denied … repository
does not exist" matches auth before image, which lands it in unverified, the
honest answer for that message.

Only the matched literal enters the sentence. It is kubeagent's own string,
never a slice of the message.

**Every kind — a read that was attempted and failed.** `reads.Failed[key]`
present: unverified, `fresh read failed: <reduced error>`.

**Every kind — a read that was never made.** No object and no `Failed`
entry: unverified, `not re-read: the read budget was spent first`.

**Registry — the pull pod's events were not read.** The gather reads the
events of the first finding's pod. When the pull finding sits on another pod,
that pod's events are missing: unverified,
`events of the pulling pod were not read`. This is a known limit of this
slice; it costs no wrong answer, only a decided row.

### The row decision

Walk the decisions in trace order.

1. The first `confirmed` decision names the cause. `Decided = true`,
   `Outcome = Confirmed`.
2. Otherwise the first `unverified` decision names the cause. `Decided =
   true`, `Outcome = Unverified`. The scan's claim stands; the row says it was
   not re-checked.
3. Otherwise — every candidate refuted, or no candidate at all — `Decided =
   false`. The row is the model's.

### Grouping

`GroupKey` and `GroupText` are set only on a result that is decided **and**
confirmed. An unverified row keeps its cause but does not join a group: a
line that says "share one upstream cause" should count only rows a fresh read
backed.

| Kind | `GroupKey` | `GroupText` |
|---|---|---|
| node | `node/<name>` | the candidate's `Cause` |
| registry | `registry/<host>` | the candidate's `Cause` |
| pvc, reason `ProvisionerNotResponding` or `MissingStorageClass`, and the fresh PVC has a `storageClassName` | `storageclass/<class>/<reason>` | `storage class <class> (<reason>)` |
| pvc, any other case | `pvc/<namespace>/<name>` | the candidate's `Cause` |

The PVC reason is read from the candidate's `Cause` text, which
`rootcause.AnnotatePVC` writes as `PVC <name> (<reason>)`: the text inside the
last pair of parentheses. A test in `internal/investigate`, where both
packages are in scope, pins that `rootcause`'s format still parses.

`Shared` returns the fixed lines:

- For every group of two or more confirmed rows, one line:
  `N workloads share one upstream cause: <GroupText>`. Groups sort by size,
  largest first, then by key.
- When there are two or more confirmed rows and no group of two: one line,
  `no shared cause among the N workloads confirmed by rules`.
- Fewer than two confirmed rows: no line.
- At most `maxSummaryLines` (4) lines; more than that is cut and marked with
  the truncation marker, the same way `capSummary` marks a cut.

## The gather: `internal/investigate/gather.go`

`gatherEvidence` makes the same reads in the same order under the same budget
and writes the same bundle bytes. Two things change:

- It returns `(trail []string, bundle string, reads hypothesis.Reads)`.
- Every object it reads is kept in `reads`, and every failed read is recorded
  in `reads.Failed` as `safetext.Line(redact.Error(err))`. The bundle keeps
  its current `read failed: <reduced error>` text.

`eventsFor` today returns a formatted string. It is split: one function lists
and returns the items; the formatting stays byte-for-byte what it is now, so
the tool loop's `get_events` and the bundle are unchanged. The gather stores
the items under `reads.Events["<namespace>/<pod>"]` and formats them for the
bundle.

The describe reads are deduped globally today; the object is stored once and
every workload that names that node or PVC sees it.

## Verdict mode: `internal/investigate/local.go`

### Order of work

```go
scoped := flaggedScope(workloads)
trail, bundle, reads := gatherEvidence(ctx, client, scoped)
results := make([]hypothesis.Result, len(scoped))
for i, w := range scoped { results[i] = hypothesis.Decide(w, reads) }
shared := hypothesis.Shared(results)
prompt := buildVerdictPrompt(cluster, summary, facts, serviceIssues, scoped, results, bundle)
doc, truncated, err := c.call(ctx, prompt)
narrative := renderVerdicts(doc, results, shared, workloads)
```

### The prompt

The `candidates` section keeps its `considered …` lines and adds:

- After every candidate line that has a decision (every non-ruled-out
  candidate, within the existing cap of 8 per workload), one line:
  `      fresh read: <outcome> — <evidence sentence>`.
- After the candidate lines of a decided workload, one line:
  `    decided by rules: <cause> — <confirmed|unverified>`.

The tool loop's primer (`renderTrace`) is untouched: it has no fresh reads to
show. `writeWorkloadTrace` gains the result as an argument or a sibling
writer; the plan chooses, the primer's bytes do not change either way.

The system prompt gains one sentence, placed after "name your own cause only
when …":

> A workload marked "decided by rules" has its cause fixed by kubeagent's own
> fresh read: return that cause verbatim and use the rationale to explain it.
> A candidate marked refuted is not supported; a workload whose every
> candidate is refuted is yours to name.

The injection-posture sentences the test pins stay verbatim.

`maxPromptBytes` (64 KiB) is unchanged. The added lines are at most 10
workloads × (8 + 1) lines; the prompt's overflow rule still cuts evidence
only.

### The renderer

Header: `Root-cause verdicts:` (was `Root-cause verdicts (local model):`;
rows now come from two sources and each row says which).

Row shapes:

```
- <namespace>/<name>: <cause> [rule, confirmed] — <rationale>
- <namespace>/<name>: <cause> [rule, unverified] — <rationale>
- <namespace>/<name>: <cause> [model, confidence: <low|medium|high|unstated>] — <rationale>
```

Row order and inclusion:

1. Every scoped workload, in report order. A decided workload always renders
   as a rule row, whether or not the model answered it. An undecided workload
   renders as a model row only when the model gave a row for it.
2. Then every further model row whose workload is flagged but outside the
   scope, in the model's order — the same rule as today for a flagged
   workload beyond the gather cap.
3. At most `maxVerdictRows` (10) rows in total. The first model row per
   workload wins; later duplicates are ignored.

A rule row's cause is the trace's candidate text verbatim. The model's
`cause` on that row is ignored. Its `rationale` is used — sanitized and
capped as today — only when the model's `cause` equals the rule's cause
verbatim; otherwise the row prints the rule's evidence sentence. This keeps a
row from arguing against itself, and it is the same posture the rest of the
mode takes: model output is untrusted until it matches something kubeagent
knows. A rule row with no model row also prints the evidence sentence.

A model row is sanitized, capped, confidence-normalized and flagged-set
checked exactly as today.

Summary: the `Shared` lines first, then the model's summary through
`capSummary` (still at most 4 lines), separated by one newline. The two
blocks are separated from the rows by a blank line, as today.

### When the model fails

`LocalClient.Investigate` still returns an error when the call fails or the
model gives nothing usable (no valid row and an empty summary). It now returns
the rules-only report alongside that error: `Consulted` is the trail and
`Narrative` is the rule rows plus the shared lines. When no rule decided
anything, the report is empty as today.

`runModelPath` in `internal/cli/enrichment.go` keeps a report whose
`Narrative` is not empty even when the arm returned an error, and appends to
the notice: `--investigate: <reduced error>; rule-decided verdicts rendered
without the model`. The notice is composed in the CLI, not by wrapping the
error, because `redact.Error` drops outer wrapping when a `*url.Error` sits
in the chain — the common case when the endpoint is down.

The Anthropic tool loop returns an empty report on error, so this change is
invisible on that path. Never-fatal is unchanged: exit 0, the deterministic
report on stdout, one notice on stderr.

## What does not change

- Scan output, text and JSON, is byte-identical without `--investigate`.
  `scan` stays at schema 1.8. `Investigation.narrative` is a string whose
  content changes; its shape does not.
- `internal/rootcause` is untouched. The trace, its verdicts and its reasons
  are what they were.
- The tool-loop mode (`ANTHROPIC_API_KEY` set), its primer and its tools are
  untouched.
- `--explain` is untouched in both modes.
- The read budget (8), the per-read cap (4 KiB), the workload cap (10), the
  candidate cap (8), the prompt cap (64 KiB), the response cap (1 MiB) and
  the model-line cap (512 runes) are unchanged.
- Verdict contract v1 is unchanged on the wire: same fields, same schema,
  same `response_format`.
- No RBAC manifest changes: the reads are the same reads.
- No new dependency. `go.mod` and `go.sum` do not change.
- Read-only toward the cluster. No LLM call from `internal/hypothesis`.

## Not in this slice

- Re-reading the kubelet lease for heartbeat candidates. One extra read per
  such node; slice 2 if the unverified rows turn out to matter.
- Candidates for node pressure, DNS and deny-all NetworkPolicy. They need
  `rootcause` rules first; own spec.
- Reading the pull pod's events when it is not the first finding's pod.
- The training repository. Its dataset builder carries a copy of the system
  prompt and the candidate line format; both change here. Re-syncing that
  copy, and deciding what the exam should ask of a model that no longer
  names the cause on rule rows, is a follow-up in that repository. The
  shipped model is not retrained by this slice; the rules do not depend on
  the model reading the new lines.

## Testing

`internal/hypothesis`, table tests with plain fake objects, no cluster:

- Node: each row of both tables, plus a reason outside the three known ones.
- PVC: each phase row.
- Registry: one message per literal in each list, the precedence cases
  (connection + image, auth + image), a non-pull `Failed` event, an empty
  event list, and a missing pull pod.
- Failed read and never-read for each kind.
- Row decision: first confirmed wins over an earlier refuted attributed
  candidate (node refuted, outranked PVC confirmed); no confirmed but one
  unverified; all refuted; no candidates; ruled-out candidates skipped.
- Grouping: node group, registry group, storage-class group, per-PVC
  fallback, unverified rows excluded, the "no shared cause" line, sort order,
  the cap.
- Every evidence sentence contains no character outside the fixed vocabulary
  plus the literal it names; a hostile event message never reaches a
  sentence.
- `imports_test.go`: no forbidden import, no `client-go`.

`internal/investigate`:

- `gather_test.go`: the fake clientset's objects come back in `Reads`; a
  refused read lands in `Failed` sanitized; the bundle and trail bytes are
  what they were.
- `local_test.go` (httptest): the two new prompt lines; the pinned system
  prompt sentences still present; a rule row overrides the model's cause; a
  rule row renders when the model drops it; the model's rationale is used
  only when its cause matches; the label on each row shape; the shared line
  comes before the model's summary; a failed call returns the rules-only
  report and an error; an all-refuted row falls to the model.
- `prime_test.go`: `renderTrace` output is unchanged.
- The `rootcause` format check for the PVC reason parse.

`internal/cli`:

- `runModelPath` keeps a non-empty report with an error and composes the
  extended notice; an empty report with an error behaves as today.

The golden scan test proves the scan is unchanged.

## Docs

- `website/docs/features/diagnostics.md`, the local verdict mode section:
  a "rules decide the candidates" paragraph before the contract; the
  contract prose says a rule row's cause never comes from the model; the
  row shapes and the header; the shared line; the size-bounds table gains
  the rule lines; the "when the model fails" behavior.
- `CHANGELOG.md` under `[Unreleased]`: `### Changed` for the row labels,
  header and summary; `### Added` for the rules.
- `CLAUDE.md`, the hypothesis-engine bullet: one sentence for this slice,
  and `internal/hypothesis` added to the list of pure packages with its
  walls. No version number; the release commit adds that.
- `docs/go-concepts.md`: nothing new. Table tests, maps and closed string
  sets are already there.

## File map

- Create `internal/hypothesis/hypothesis.go`, `decide.go`, `shared.go`,
  `imports_test.go`, `hypothesis_test.go`.
- Modify `internal/investigate/gather.go` (return `Reads`),
  `internal/investigate/reader.go` (split `eventsFor`),
  `internal/investigate/prime.go` (candidate lines with fresh reads),
  `internal/investigate/local.go` (order of work, system prompt sentence,
  renderer, failure path).
- Modify `internal/cli/enrichment.go` (`runModelPath`).
- Modify the three docs above.
