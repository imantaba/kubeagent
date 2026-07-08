# Scan Output Redesign — Design

**Date:** 2026-07-08
**Status:** Approved

## Goal

Reorganize the human-readable (`--output text`) scan output so the operator sees
**what needs action first**, advisories are demoted and summarized, and all-OK
context collapses to one-liners. The current output is ordered by collector, so a
real failure (an `ImagePullBackOff`) can sit below 17 lines of informational
PVC-reclaim rows.

## Non-goals / invariants (do not change)

- `--output json` stays **full and unchanged** — machines get every row. The
  redesign and the new flag affect text mode only.
- The `clusterhealth` **verdict is unchanged**. It is already correctly
  cluster-scoped: `Degraded` only on node issues or kube-system workload issues,
  so an app-workload failure keeps the cluster `Healthy`. We keep that and add a
  separate workload line.
- **`--fix` is untouched.** `runFixes` in `main.go` runs after the report and
  prints its own lines; the report never renders remediation info. No read-only
  fix hint (explicitly out of scope).
- No detector changes, no exit-code change, no new API call, no new RBAC.
- Commits carry **no `Co-Authored-By: Claude` trailer**.

## Header (two lines)

- **Line 1** — the existing cluster verdict, unchanged:
  `Cluster: Healthy — 5/5 nodes Ready` plus node issues, system issues, and the
  namespace scope note when present.
- **Line 2 (new)** — a workload-scoped attention summary, printed only when at
  least one app workload is flagged or a real service issue exists:
  `  Needs attention: 1 workload failing · 3 services without endpoints`
  - "N workload(s) failing" = flagged workloads that appear in NEEDS ATTENTION
    (the same set the workload loop renders; kube-system ones are already in the
    verdict but still counted here if flagged and shown).
  - "M service(s) without endpoints" = service issues with `Expected == false`.
  - Singular/plural handled. A term is omitted when its count is 0; the whole
    line is omitted when both are 0.

## Zones (text mode), in order

1. **NEEDS ATTENTION** — glyph `✗`. Flagged app workloads (full existing
   per-workload detail: findings, resources, rollout, pods), **real** service
   issues (`Expected == false`), and credential warnings (`--lint-secrets`).
   Never collapsed.
2. **NOTES** — glyph `•`. Advisories:
   - PVC-reclaim **summary** (see below),
   - **expected-empty** services (`Expected == true`: CronJob-idle, scaled-to-0),
   - the hidden-counts footer (`+N restarted workloads [--include-restarts] ·
     +N CronJobs [--include-cron]`).
3. **CONTEXT** — dim, no glyph:
   - `Nodes  5/5 Ready · kubelet reservations OK` (one line when all nodes
     reserve OK),
   - the Resources (cluster) block (unchanged content),
   - the Platform line.
4. **Explanation** — `--explain` output, unchanged, at the very end.

`--fix` output is appended by `main.go` after everything, unchanged.

### All-clear

`No issues found. ✅` prints when NEEDS ATTENTION is empty **and** there are no
real NOTES-worthy issues, matching the current all-clear condition (extended to
the new zones). Purely informational NOTES/CONTEXT do not suppress it.

## Collapse / summarize rules

### PVC reclaim (new `--pvc-reclaim` flag)

- **Default:** one NOTE line, grouped by StorageClass, counts descending:
  `• 18 PVCs on Delete reclaim policy — longhorn-single-replica ×11, nfs-client ×7   [--pvc-reclaim]`
  - PVCs with an empty StorageClass group under `(no class)`.
  - Group order: by count descending, then class name ascending for ties.
- **With `--pvc-reclaim`:** the full per-PVC rows (current format:
  `namespace/name  pv <pv>  class <class>  <capacity>`), under the NOTES zone.
- **JSON:** always the full `pvcReclaim.pvcs` list regardless of the flag.

### Node reservations

- All nodes OK → single CONTEXT line `Nodes  N/N Ready · kubelet reservations OK`.
- One or more warnings → a NOTE `• K node(s) reserve no memory: <name>, <name>`
  (advisory; never NEEDS ATTENTION), and the CONTEXT line drops the
  "reservations OK" suffix.

### Services

- `Expected == false` → NEEDS ATTENTION.
- `Expected == true` → NOTES (demoted). Detail text (`backs CronJob — expected
  between runs`, `scaled to 0`) is preserved.

## Components

### `report.Input` struct (param bundling)

`PrintInventory` currently takes 11 positional parameters; the new flag would
make 12. Replace them with a single struct:

```go
type Input struct {
    Cluster            clusterhealth.ClusterHealth
    Result             inventory.Result
    Resources          *resources.Summary
    Platform           *platform.Facts
    ServiceIssues      []svchealth.Issue
    CredentialWarnings []credlint.Finding
    NodeReserve        *nodereserve.Report
    PVCReclaim         *pvcreclaim.Report
    PVCReclaimFull     bool   // --pvc-reclaim: expand the PVC list
    Explanation        string
}

func PrintInventory(in Input, format string, w io.Writer) error
```

JSON serialization uses the same `inventoryReport` shape as today (the struct is
just how callers pass data in; the JSON output is unchanged).

### Rendering helpers (in `internal/report`)

`printInventoryText` becomes a small orchestrator calling zone functions:

- `printHeader(in, w)` — verdict line (existing logic) + the new attention line.
- `printNeedsAttention(in, w)` — flagged workloads (reuse `printWorkload`), real
  service issues (reuse `printServiceIssues` filtered to `!Expected`), credential
  warnings (reuse `printCredentialWarnings`).
- `printNotes(in, w)` — PVC-reclaim summary or full list, expected-empty services,
  hidden-counts footer.
- `printContext(in, w)` — nodes/reservations one-liner, resources block, platform.

Existing helpers (`printWorkload`, `printResources`, `printResLine`,
`printServiceIssues`, `printCredentialWarnings`, `printNodeReservations`,
`printPVCReclaim`, `footerHint`) are reused or lightly adapted (e.g.
`printServiceIssues` gains a filter; `printPVCReclaim` gains a summary mode). If
`report.go` grows unwieldy, split the text renderer into `report_text.go` — same
package, no behavior change.

### `main.go`

- Add `pvcReclaimFull := fs.Bool("pvc-reclaim", false, "list every PVC on a Delete reclaim policy (default: a grouped summary)")` to the `scan` flag set.
- Build a `report.Input` and call `report.PrintInventory(in, *output, os.Stdout)`.
- `runFixes` call site unchanged.

## Data flow

`scan.Evaluate` result → `main.go` assembles `report.Input` (adding
`PVCReclaimFull` from the flag) → `PrintInventory` dispatches to JSON (full,
unchanged) or the zoned text renderer. No pipeline/collector change.

## Testing

- **Header:** attention line present with correct counts when a workload/service
  is flagged; omitted when nothing is flagged; verdict line unchanged.
- **Zones/order:** NEEDS ATTENTION before NOTES before CONTEXT; a failing
  workload renders in NEEDS ATTENTION; an `Expected==true` service renders in
  NOTES, an `Expected==false` service in NEEDS ATTENTION.
- **PVC summary:** default groups by class with counts and the `[--pvc-reclaim]`
  hint; `PVCReclaimFull=true` emits full rows; JSON path still emits full list.
- **Node reservations:** all-OK collapses to the CONTEXT one-liner; a warning
  produces a NOTE and drops the OK suffix.
- **All-clear:** printed when only NOTES/CONTEXT content exists and no real issues.
- **JSON unchanged:** existing JSON tests still pass against `report.Input`.
- Existing `--fix`/`runFixes` tests untouched and passing.

## Docs

- `CHANGELOG.md` `[Unreleased]`.
- Website `features/diagnostics.md` (describe the zoned output + `--pvc-reclaim`).
- `README.md` if it shows sample output.
