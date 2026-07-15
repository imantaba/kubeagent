# Node Reservation Visibility — Design

**Date:** 2026-07-15
**Status:** Approved

## Goal

Make kubeagent's node-reservation reporting **clear** and **multi-resource**.
Today the `nodereserve` detector computes `Capacity − Allocatable` for cpu and
memory but only warns on memory and only prints a cryptic one-liner —
`Nodes  0/2 reserve memory OK` — which reads ambiguously (it means "0 of 2 nodes
reserve memory OK", i.e. both reserve none). CPU and ephemeral-storage
reservations are never surfaced.

This change: (1) rewrites the summary into an unambiguous per-resource block, and
(2) covers **memory, CPU, and ephemeral-storage**. Reserving no **memory** or no
**ephemeral-storage** raises an actionable `NOTES` warning (both are node-killers:
OOM and disk pressure); **CPU** is informational only (compressible — throttling,
not death).

## Data source and constraint

The Node API exposes only `Status.Capacity` and `Status.Allocatable` per resource.
Their difference is the **combined** reservation
(`kube-reserved` + `system-reserved` + hard-eviction threshold). The node object
**cannot** split kube-reserved from system-reserved — that split lives only in the
kubelet's own config (`/configz`), which kubeagent does not read. So the output
reports the combined reservation per resource and labels it "combined kube+system";
it never claims a kube-vs-system breakdown.

A resource may be absent from a node's `Capacity` (unusual, old kubelets). Such a
resource is treated as **not reported** for that node: shown as `not reported`,
excluded from the none/ok counts, never flagged.

`reserved = Capacity − Allocatable`, clamped to zero on a negative delta (unchanged
from today). "Reserves none" means the clamped reserved quantity is exactly zero.

## Components

### `internal/nodereserve` (extend, pure)

Add ephemeral-storage alongside cpu/memory. Additive struct changes — existing JSON
field names are unchanged so current consumers keep working.

```go
type NodeReservation struct {
	Name              string `json:"name"`
	Role              string `json:"role,omitempty"`
	CPUReserved       string `json:"cpuReserved"`                  // "200m" | "0" | "—"
	MemReserved       string `json:"memReserved"`                  // "800Mi" | "0" | "—"
	EphemeralReserved string `json:"ephemeralReserved"`            // NEW: "2Gi" | "0" | "—"
	Warning           bool   `json:"warning"`                      // memory reserves none (unchanged)
	NoEphemeral       bool   `json:"noEphemeralReserve,omitempty"` // NEW: ephemeral reserves none
	NoCPU             bool   `json:"noCPUReserve,omitempty"`       // NEW: cpu reserves none (info)
}

type Report struct {
	Nodes         []NodeReservation `json:"nodes"`
	WarnCount     int               `json:"warnCount"`     // nodes reserving no memory (unchanged; drives the gauge)
	EphemeralNone int               `json:"ephemeralNone"` // NEW: nodes reserving no ephemeral-storage
	CPUNone       int               `json:"cpuNone"`       // NEW: nodes reserving no cpu
}
```

- `Warning` / `WarnCount` keep their exact current meaning (memory reserves none),
  so `internal/watch/metrics.go`'s `kubeagent_nodes_without_reservations` gauge is
  untouched.
- The reserved-quantity string for a **not reported** resource is `"—"` and the
  corresponding `No*` flag is `false` (not counted). Reserved strings continue to
  use the existing `fmtCPU`/`fmtMem` helpers; ephemeral-storage reuses `fmtMem`
  (bytes → Gi/Mi).
- A node reserving nothing for a resource it *does* report sets the matching flag
  and increments the matching count.

`role`, `reserved`, `fmtCPU`, `fmtMem` are unchanged.

### `internal/report`

Replace the single CONTEXT line and expand the NOTES block.

**NOTES (actionable — memory and ephemeral-storage only):** for each of memory and
ephemeral-storage that has ≥1 node reserving none, one bullet listing the node names
plus a one-line consequence:

```
  • 2 nodes reserve no memory: node-a, node-b
      — OS/kubelet memory pressure can destabilize the node
  • 2 nodes reserve no ephemeral-storage: node-a, node-b
      — disk pressure can destabilize the node
```

The memory bullet is the reworded current bullet (adds the consequence line). CPU
never appears in NOTES.

**CONTEXT (informational — replaces `Nodes  N/M reserve memory OK`):** a labeled
per-resource block, rendered whenever `NodeReserve` has nodes:

```
Kubelet reservations (combined kube+system)
  memory             2 of 2 nodes reserve none  ⚠
  cpu                2 of 2 nodes reserve none
  ephemeral-storage  2 of 2 nodes reserve none  ⚠
```

Per-resource line wording, where `N` = nodes reserving none of that resource (among
nodes that report it) and `M` = nodes reporting it:

- `N == 0`  → `all M nodes reserve some` (append `  ✓` for memory and
  ephemeral-storage; nothing for cpu).
- `0 < N`   → `N of M nodes reserve none` (append `  ⚠` for memory and
  ephemeral-storage; nothing for cpu).
- `M == 0` (resource not reported by any node) → `not reported`, no glyph.

CPU never gets a `⚠` or `✓` glyph (informational). The `⚠`/`✓` on the memory and
ephemeral lines mirror the NOTES actionability and aid scanning; NOTES remains the
place that names the affected nodes.

The block is fixed-order: memory, cpu, ephemeral-storage. Label column is left-
aligned/padded so the statuses line up.

## Scope boundaries

- **Read-only** (only reads `Capacity`/`Allocatable`) and **advisory**: no impact
  on the cluster verdict, exit code, `kubeagent_cluster_healthy`, or the "Needs
  attention" line. Only `NOTES` + `CONTEXT` (and JSON) change.
- **`watch` daemon, Helm, and the `kubeagent_nodes_without_reservations` gauge are
  untouched.** The gauge stays memory-scoped (matching its name/help); `WarnCount`
  is unchanged. Surfacing ephemeral-storage as a daemon gauge is out of scope.
- No kube-vs-system split (impossible from the node API).
- No new thresholds/config: "reserves none" is a strict `== 0`, as today.
- JSON changes are **additive** (new fields on `NodeReservation`/`Report`);
  existing fields keep their names and meaning.

## Testing

Follow TDD; detectors are pure functions tested with fake nodes.

**`internal/nodereserve/nodereserve_test.go`:**

- Node reserving none of memory/cpu/ephemeral → `Warning`, `NoCPU`, `NoEphemeral`
  all true; `WarnCount`, `CPUNone`, `EphemeralNone` all 1.
- Node reserving memory + cpu but **no ephemeral-storage** → only `NoEphemeral`;
  `EphemeralNone == 1`, `WarnCount == 0`, `CPUNone == 0`.
- Node reserving all three → no flags, all counts 0; reserved strings non-"0".
- Node whose `Capacity` omits `ephemeral-storage` → `EphemeralReserved == "—"`,
  `NoEphemeral == false`, not counted in `EphemeralNone`.
- Reserved formatting: cpu millicores (`"200m"`), memory/ephemeral Gi/Mi.

**`internal/report/report_test.go`:**

- All-none input → both NOTES bullets (memory + ephemeral, with node names + the
  consequence lines) and the CONTEXT block with `⚠` on memory/ephemeral, none on
  cpu.
- All-ok input → no NOTES reservation bullets; CONTEXT block reads
  `all N nodes reserve some  ✓` for memory/ephemeral.
- Mixed (e.g. memory none but ephemeral set) → only the memory NOTES bullet; CONTEXT
  shows `⚠` on memory only.
- Not-reported ephemeral → CONTEXT ephemeral line reads `not reported`; no ephemeral
  NOTES bullet.
- The old exact string `reserve memory OK` no longer appears.

## Docs

- `CHANGELOG.md` (`## [Unreleased]` → `### Changed`): node-reservation reporting now
  covers memory, CPU, and ephemeral-storage with a clearer per-resource summary;
  no-ephemeral joins no-memory as a warning.
- `website/docs/features/diagnostics.md`: update the node-reservation subsection to
  describe the three resources, the memory/ephemeral warnings, and the
  "combined kube+system" framing.

## Exact strings to use verbatim

- CONTEXT header: `Kubelet reservations (combined kube+system, per node)`
- Resource labels: `memory`, `cpu`, `ephemeral-storage`
- Status phrases: `N of M nodes reserve none`, `all M nodes reserve some`,
  `not reported`
- NOTES consequence lines: `— OS/kubelet memory pressure can destabilize the node`,
  `— disk pressure can destabilize the node`
- Glyphs: `⚠` (warn, memory/ephemeral only), `✓` (ok, memory/ephemeral only)
