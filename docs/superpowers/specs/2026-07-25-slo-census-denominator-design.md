# SLO census denominator — design

## Problem

The watch daemon's SLI does not measure availability. It reads a list that was
built to decide what to print to a human, and that list omits exactly the
workloads an availability figure is made of.

`workloadCensus` (`internal/watch/slo.go:106-114`) iterates
`res.Inventory.Workloads`. That field is the output of `inventory.Prioritize`
(`internal/inventory/inventory.go:415-442`), whose final branch reads:

```go
default:
    // healthy-quiet — always hidden
```

`scan.Evaluate` assembles the full workload set at `internal/scan/scan.go:182`
and discards the healthy ones at `:255`. The census never sees them.

Two distinct defects follow.

**The denominator is wrong.** Healthy workloads are absent, so `total` counts
only workloads already in trouble. On a fully healthy cluster the list is empty,
`total` is 0, and `Tracker.Observe` returns before `add` — nothing is recorded at
all, so window coverage stays at zero indefinitely.

**The numerator is wrong.** `good` tests `len(w.Findings) == 0`, but membership
in the list is decided by `Flagged()`:

```go
func (w Workload) Flagged() bool {
    return len(w.Findings) > 0 || w.Ready < w.Desired || w.Status == "Failed"
}
```

A workload that is under-replicated, or Failed, with no detector Finding is in
the list *and* is counted as good. An actively degraded workload raises
availability.

### Observed

Two independent observations, both against a live Kind cluster.

Chaos scenario 13 reported `kubeagent_slo_availability_ratio` of
`0.1612054025156958` where its own expectation string predicted approximately
`0.4`, and a burn rate of `838.79` where the prediction was near `600`. The
expectation allows for timing shift; a 2.5x deviation is not timing shift.

Running the daemon directly against a healthy namespace with `--heartbeat 10s`,
scraping every ten seconds for a minute, gave six consecutive scrapes of:

```
kubeagent_slo_window_coverage_ratio{window="fast"} 0
kubeagent_slo_window_coverage_ratio{window="slow"} 0
```

### Consequences

Coverage accrues only while at least one workload is flagged. The slow window's
60% gate therefore needs roughly 3.6 hours of continuous breakage before a page
can fire at all, so the alert is effectively dead in normal operation — which is
the opposite of the feature's purpose.

When coverage does accrue, availability collapses toward zero regardless of blast
radius. A single failing workload in a large cluster reports what a total outage
reports. The worked example in the shipped documentation — 3 of 200 workloads
failing for an hour giving 15x burn — cannot be produced by the code.

### Why the tests passed

Every unit test builds a `scan.Result` by hand with healthy entries in
`Inventory.Workloads`. `scan.Evaluate` never emits that shape. The fixtures and
the code agreed with each other and both disagreed with reality. This is the same
vacuous-fixture failure mode as the twelve defects already found in this feature
area, and the largest of them: the mutation testing in the whole-branch review
confirmed the assertions were load-bearing, but every one of them was anchored to
a fixture rather than to the pipeline's real output.

## Design

### The census moves to where the full set is

`inventory.Prioritize` already iterates every assembled workload — it is the
precise point at which healthy ones are dropped. Compute the census there, in the
same pass, and return it alongside the filtered list:

```go
// Census counts long-running workloads before display filtering: Total is every
// one of them, Good those that are not Flagged(). The watch daemon's SLI reads
// this rather than Workloads, which is filtered for display and omits exactly
// the healthy majority an availability figure needs.
type Census struct {
    Good  int
    Total int
}
```

`inventory.Result` gains one field:

```go
type Result struct {
    Workloads      []Workload
    HiddenRestarts int
    HiddenCron     int
    Census         Census `json:"-"`
}
```

`workloadCensus` in `internal/watch/slo.go` is deleted. `applyResult` reads
`res.Inventory.Census.Good` and `.Total` directly. No second traversal, no extra
retained memory, and the SLI no longer depends on a list whose contract is "what
to show a human".

### What counts as long-running

Everything except `Job` and `CronJob`.

Stated as an exclusion rather than an inclusion list on purpose. `Assemble`'s pod
rollup falls through to `default: kind, name = o.Kind, o.Name`
(`internal/inventory/inventory.go:313-315`), so a pod owned by an arbitrary
controller appears under that controller's kind — an Argo `Rollout`, or any
CRD-owned workload. An inclusion list of Deployment/StatefulSet/DaemonSet/Pod
would silently drop those from the SLI, which cuts against the operator-coverage
track on the roadmap. Excluding the two kinds whose normal lifecycle is to finish
or sit idle puts every unknown kind on the correct side.

Jobs and CronJobs are excluded because neither is expected to be continuously up.
A CronJob idle between runs is not unavailable, and a Job that failed once carries
its findings forever, permanently denting an availability figure it has no
business influencing.

### What counts as good

`!w.Flagged()`.

One predicate now serves both the report and the SLI. The defect found here
existed because two predicates disagreed about the same question; using
`Flagged()` removes that class of bug rather than relocating it.

### The late annotators do not affect this

`scan.Evaluate` runs six annotators at `internal/scan/scan.go:275-282`, *after*
`Prioritize`, on the filtered slice. They can only reach workloads that were
already `Flagged()` — an unflagged workload is not in that slice. So `Flagged()`
evaluated during `Prioritize` gives the same answer the annotated set would, and
computing the census before annotation is exact, not an approximation.

### JSON

`Census` is tagged `json:"-"`. `inventory.Result` is serialized by
`scan --output json`, that output is a documented contract, and the golden test
snapshots it. The census exists to feed the watch SLI, not the scan report, so it
stays out of the payload: no schema change, no golden regeneration.

## Testing

The failure here was fixture-shaped, so the primary test must not be.

**Binding test, written first.** Drive `scan.Evaluate` against client-go's fake
clientset with one healthy Deployment and assert
`res.Inventory.Census == Census{Good: 1, Total: 1}`. Then break it and assert
`{Good: 0, Total: 1}`. This binds the census to what the real pipeline produces
instead of to a fixture's opinion, and it is the test whose absence allowed the
defect.

**Unit tests on `Prioritize`.** A healthy workload is absent from `Workloads` but
present in `Census.Total`. A `Job` and a `CronJob` are counted in neither
`Census.Good` nor `Census.Total`. A workload with `Ready < Desired` and no
Findings is `Total` but not `Good` — the numerator defect, pinned directly.

**Watch-level test.** `applyResult` with a result whose census is `{Good: 1,
Total: 1}` records a sample and raises coverage above zero; the pre-fix code
recorded nothing. This is the regression test for the observed symptom.

**Mutation checks the implementer must run and report.** Change the exclusion set
to also drop `Pod`, and confirm a test fails. Change `!w.Flagged()` to
`len(w.Findings) == 0`, and confirm the under-replicated test fails. A mutation
that leaves the suite green is a finding, not a curiosity.

## Out of scope

- The chaos scenario 12 double-print (`chaos/run.sh:326-327`), pre-existing and
  cosmetic.
- Rewriting scenario 13's expectation figures. The `~0.4` and `600x` predictions
  are re-derived from measured post-fix behavior at the re-gate, not guessed now.
- Any change to `--fix`, RBAC, or the read-only guarantee. The daemon remains
  get/list/watch only with no LLM in the watch path.

## Global constraints

- The watch daemon stays strictly read-only toward the cluster: get/list/watch
  only, no writes, no LLM, no new RBAC.
- Standard library only; no new dependency.
- The percentage-to-ratio conversion stays in exactly one place, `main.go`.
- `validateSLOTarget` stays the first statement of `watch.Run`.
- TDD: the failing test is written and watched to fail before the implementation.
- No Claude, Claude Code, or Anthropic attribution in any commit, comment, or doc.
