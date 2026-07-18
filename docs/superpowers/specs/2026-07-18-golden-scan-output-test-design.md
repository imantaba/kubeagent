# Golden-file Scan-Output Test — Design

**Date:** 2026-07-18
**Status:** Approved

## Goal

Catch changes to `kubeagent scan`'s human-readable output automatically. When the
text report format drifts — a new section, a reworded line, a changed layout — the
README demo GIF (`docs/kubeagent-demo.gif`) and the quickstart example
(`website/docs/quickstart.md`) silently go stale (exactly what happened before the
v0.22.0 node-reservation change). A golden-file test fails the moment the output
changes, turning "someone notices the docs are stale weeks later" into "CI says
regenerate the GIF/example."

## Level and determinism

The test runs at the **report package**, not the CLI: `report.PrintInventory` is a
pure function of `report.Input`, so a golden test needs no cluster, is fast, and is
deterministic. The single source of non-determinism today is three `time.Now()` calls
that render relative ages; we inject a clock to remove it.

All output loops iterate ordered slices (the security "restricted" aggregate renders a
fixed `[]string{"RunAsRoot","AllowPrivilegeEscalation","CapabilitiesNotDropped"}`
order; detail blocks are `sort.SliceStable`-sorted), so there is no map-iteration-order
non-determinism to handle.

## Components

### 1. Clock injection (`internal/report`, `main.go`) — production change

`report.Input` gains a clock so relative ages are a function of the input, not
wall-clock:

```go
type Input struct {
	// ... existing fields ...
	Now time.Time // clock for relative ages; main sets time.Now(); zero → wall-clock
}
```

- A helper `nowOr(t time.Time) time.Time` returns `t`, or `time.Now()` when `t` is the
  zero value. `printInventoryText` resolves `now := nowOr(in.Now)` **once** and threads
  it to the three `inventory.HumanSince(..., time.Now())` call sites, which become
  `inventory.HumanSince(..., now)`:
  - `printServiceIssues` (service-issue age) — gains a `now time.Time` parameter;
    callers are `printInventoryText` (the `✗` NEEDS-ATTENTION services) and `printNotes`
    (the `•` expected-empty services), so `printNotes` also gains `now` and forwards it.
  - `printWorkload` (two sites: "…restarts, last N ago" and the per-pod
    "restarts=… (N ago)") — gains a `now time.Time` parameter; caller is
    `printInventoryText`.
- `main.go` sets `Now: time.Now()` in its `report.Input{…}` literal.

**Backward-compatible:** a zero `Now` falls back to `time.Now()` via `nowOr`, so every
existing caller and test that leaves `Now` unset behaves exactly as today. Only the
golden test sets a fixed `Now`.

### 2. Comprehensive fixture (`internal/report/golden_test.go`)

A test helper `goldenInput(now time.Time) Input` builds **one** `Input` that exercises
every rendered section, so the golden is a broad snapshot of the whole report:

- **Cluster verdict `Degraded`** with a `NotReady` node (with root-cause reason), a
  `SchedulingDisabled` node, a stale-heartbeat node, and an expected-but-absent node.
- **Workloads** covering each failure mode the report renders: CrashLoopBackOff,
  ImagePullBackOff, OOMKilled, Pending/Unschedulable, RestartLoop, and
  VolumeAttachError — each with the finding evidence and, where applicable, restart
  counts and `LastRestart`/rollout timestamps set **relative to `now`** (e.g.
  `now.Add(-27*time.Second)`) so ages render as fixed strings.
- **Service issues** (no-endpoints), **ingress issues** (broken backend),
  **credential warnings** (a key stored in the clear).
- **Security findings** — at least one `baseline` (e.g. Privileged/HostPath), the
  aggregated `restricted` hardening gaps, and an exposed Service — to render the full
  `SECURITY` section (non-verbose).
- **PVC reclaim** (a Delete-policy PVC), **node reservations** (all three resources,
  with a no-memory and no-ephemeral warning), **kubelet health** (an unhealthy node),
  **resources** summary, and **platform** facts.

All names, namespaces, pod hashes, and IPs are fixed literals. `now` is a hardcoded
`time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)` literal in the test (no `time.Now()`).

### 3. Golden test + update flag (`internal/report/golden_test.go`)

```go
var update = flag.Bool("update", false, "rewrite golden files")

func TestGoldenScanOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(goldenInput(goldenNow), "text", &buf); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	golden := "testdata/golden-scan.txt"
	if *update {
		os.WriteFile(golden, got, 0o644)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil { t.Fatalf("read golden (run with -update to create): %v", err) }
	if !bytes.Equal(got, want) {
		t.Errorf("scan text output changed.\n<unified diff of want vs got>\n\n"+
			"If this change is intended, run:\n"+
			"  go test ./internal/report -run TestGoldenScanOutput -update\n"+
			"then refresh docs/kubeagent-demo.gif (the update-demo-gif skill) and the "+
			"quickstart example output in website/docs/quickstart.md.")
	}
}
```

- The diff in the failure message is a simple line-by-line unified diff (a small
  in-test helper, or first-difference report) — enough to see what changed.
- Regenerate with `go test ./internal/report -run TestGoldenScanOutput -update`.

### 4. Golden file (`internal/report/testdata/golden-scan.txt`)

The rendered comprehensive text output, committed. Regenerated only via `-update`.

## Scope boundaries

- **Text output only.** That is what goes stale in the GIF/quickstart. The JSON
  contract is already covered by the existing per-field JSON tests in
  `report_test.go`; a JSON golden is out of scope (YAGNI).
- **Report level only** — no CLI, no cluster, no fake clientset. The fixture is a
  hand-built `report.Input`.
- The clock injection is the **only** production change; it is behaviour-preserving
  (zero `Now` → wall-clock).
- Not wired into `--explain` and not a lint of doc freshness itself — it guards the
  rendering, and its failure message points at the docs to regenerate.

## Testing

TDD:

1. Add `Now` to `Input` + `nowOr` + thread `now`; `main.go` sets `Now`. Existing report
   tests stay green (zero `Now` → wall-clock).
2. Write `TestGoldenScanOutput` + `goldenInput`; run it → fails (no golden file).
3. Run with `-update` → creates `testdata/golden-scan.txt`; **read the golden and
   confirm it renders every section** (this manual read is the real acceptance check —
   the golden is only as good as its first captured content).
4. Run without `-update` → passes; `go test ./...` green; `gofmt` clean.

A second small test, `TestGoldenInputCoversAllSections`, asserts `goldenInput` populates
each `Input` sub-report (non-nil / non-empty), so the fixture can't silently lose a
section over time and leave the golden a partial snapshot.

## Exact names to use verbatim

- Field `Input.Now`; helper `nowOr`; fixed clock `goldenNow` =
  `time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)`.
- Test file `internal/report/golden_test.go`; tests `TestGoldenScanOutput` and
  `TestGoldenInputCoversAllSections`; helper `goldenInput`.
- Flag `-update`; golden path `internal/report/testdata/golden-scan.txt`.
