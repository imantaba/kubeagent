# Node Reservation Visibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make kubeagent's node-reservation reporting clear and multi-resource — cover memory, CPU, and ephemeral-storage, warn on no-memory *and* no-ephemeral-storage, and replace the cryptic `Nodes 0/2 reserve memory OK` line with a per-resource CONTEXT block.

**Architecture:** Extend the pure `internal/nodereserve` detector to also compute ephemeral-storage reservation and per-resource "reserves none" flags/counts (additive struct changes; `WarnCount` stays memory-scoped). Then rework the `internal/report` text rendering: two NOTES warnings (memory, ephemeral-storage) and a per-resource CONTEXT block. Docs last.

**Tech Stack:** Go 1.26, `client-go` types (`corev1`, `resource.Quantity`), standard-library only. Tests use fake `corev1.Node` values (no cluster).

## Global Constraints

- **Read-only + advisory.** No impact on the cluster verdict, exit code, `kubeagent_cluster_healthy`, or the "Needs attention" line. Only `NOTES` + `CONTEXT` (and JSON) change.
- **`watch` daemon, the `kubeagent_nodes_without_reservations` gauge, and Helm are untouched.** `WarnCount` keeps its exact current meaning (nodes reserving no memory).
- **JSON changes are additive** — existing `NodeReservation`/`Report` field names and meanings are unchanged.
- **"Reserves none" is a strict `== 0`.** No thresholds/config.
- **No kube-vs-system split** (impossible from the Node API); wording says "combined kube+system".
- Go: `export PATH=$PATH:/usr/local/go/bin`. Build `go build ./...`, test `go test ./...`.
- **No `Co-Authored-By: Claude` trailer** on any commit. TDD: failing test first.
- Exact strings (verbatim): CONTEXT header `Kubelet reservations (combined kube+system)`; labels `memory`, `cpu`, `ephemeral-storage`; status phrases `%d of %d nodes reserve none`, `all %d %s reserve some`, `not reported`; consequence lines `— OS/kubelet memory pressure can destabilize the node`, `— disk pressure can destabilize the node`; glyphs `⚠` (warn) and `✓` (ok) on memory/ephemeral only.

---

### Task 1: Extend `nodereserve` detector (ephemeral-storage + cpu/ephemeral flags & counts)

**Files:**
- Modify: `internal/nodereserve/nodereserve.go` (struct fields + `Assess`)
- Test: `internal/nodereserve/nodereserve_test.go` (new fake-node helper + tests)

**Interfaces:**
- Consumes: `corev1.Node`, `resource.Quantity` (unchanged).
- Produces: `NodeReservation{..., EphemeralReserved string, NoEphemeral bool, NoCPU bool}` and `Report{..., EphemeralNone int, CPUNone int, EphemeralReporting int}`. `Assess(nodes []corev1.Node) Report` signature unchanged. `WarnCount` unchanged = nodes reserving no memory.

- [ ] **Step 1: Write the failing tests**

Add to `internal/nodereserve/nodereserve_test.go` (a new helper that also sets ephemeral-storage, and four tests):

```go
// nodeEph builds a fake node that also reports ephemeral-storage capacity/allocatable.
func nodeEph(name, capCPU, capMem, capEph, allocCPU, allocMem, allocEph string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse(capCPU),
				corev1.ResourceMemory:           resource.MustParse(capMem),
				corev1.ResourceEphemeralStorage: resource.MustParse(capEph),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse(allocCPU),
				corev1.ResourceMemory:           resource.MustParse(allocMem),
				corev1.ResourceEphemeralStorage: resource.MustParse(allocEph),
			},
		},
	}
}

func TestAssess_FlagsAllThreeWhenNoneReserved(t *testing.T) {
	// capacity == allocatable for cpu/mem/ephemeral -> nothing reserved anywhere.
	r := Assess([]corev1.Node{nodeEph("n", "4", "16Gi", "100Gi", "4", "16Gi", "100Gi")})
	n := find(r, "n")
	if !n.Warning || !n.NoCPU || !n.NoEphemeral {
		t.Fatalf("want all three no-reserve flags set, got %+v", n)
	}
	if r.WarnCount != 1 || r.CPUNone != 1 || r.EphemeralNone != 1 || r.EphemeralReporting != 1 {
		t.Errorf("want counts 1/1/1 reporting 1, got warn=%d cpu=%d eph=%d reporting=%d",
			r.WarnCount, r.CPUNone, r.EphemeralNone, r.EphemeralReporting)
	}
}

func TestAssess_EphemeralOnlyUnreserved(t *testing.T) {
	// cpu + mem reserved, ephemeral not reserved.
	r := Assess([]corev1.Node{nodeEph("n", "4", "16Gi", "100Gi", "3800m", "15Gi", "100Gi")})
	n := find(r, "n")
	if n.Warning || n.NoCPU {
		t.Errorf("want mem/cpu reserved (no flags), got %+v", n)
	}
	if !n.NoEphemeral {
		t.Errorf("want NoEphemeral=true, got %+v", n)
	}
	if r.EphemeralNone != 1 || r.WarnCount != 0 || r.CPUNone != 0 {
		t.Errorf("want eph-none 1, warn 0, cpu-none 0; got %d/%d/%d", r.EphemeralNone, r.WarnCount, r.CPUNone)
	}
	if n.EphemeralReserved != "0" {
		t.Errorf("want EphemeralReserved %q, got %q", "0", n.EphemeralReserved)
	}
}

func TestAssess_AllThreeReserved(t *testing.T) {
	// 200m cpu, 1Gi mem, 2Gi ephemeral reserved -> no flags.
	r := Assess([]corev1.Node{nodeEph("n", "4", "16Gi", "100Gi", "3800m", "15Gi", "98Gi")})
	n := find(r, "n")
	if n.Warning || n.NoCPU || n.NoEphemeral {
		t.Errorf("want no flags when all reserved, got %+v", n)
	}
	if n.EphemeralReserved != "2Gi" {
		t.Errorf("want EphemeralReserved %q, got %q", "2Gi", n.EphemeralReserved)
	}
	if r.EphemeralReporting != 1 || r.EphemeralNone != 0 {
		t.Errorf("want reporting 1, eph-none 0; got %d/%d", r.EphemeralReporting, r.EphemeralNone)
	}
}

func TestAssess_EphemeralNotReported(t *testing.T) {
	// node() reports only cpu/mem -> ephemeral is "not reported".
	r := Assess([]corev1.Node{node("n", "4", "16Gi", "3800m", "15Gi", nil)})
	n := find(r, "n")
	if n.EphemeralReserved != "—" {
		t.Errorf("want EphemeralReserved %q when not reported, got %q", "—", n.EphemeralReserved)
	}
	if n.NoEphemeral {
		t.Errorf("want NoEphemeral=false when not reported, got true")
	}
	if r.EphemeralReporting != 0 || r.EphemeralNone != 0 {
		t.Errorf("want reporting 0, eph-none 0; got %d/%d", r.EphemeralReporting, r.EphemeralNone)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/nodereserve/ -run 'Ephemeral|AllThree'`
Expected: FAIL to compile — `NodeReservation` has no field `NoEphemeral`/`NoCPU`/`EphemeralReserved`, `Report` has no `EphemeralNone`/`CPUNone`/`EphemeralReporting`.

- [ ] **Step 3: Implement**

In `internal/nodereserve/nodereserve.go`, replace the `NodeReservation` and `Report` struct definitions and the `Assess` function with:

```go
// NodeReservation is one node's observed reservation. Reserved amounts are
// human-readable strings ("200m", "800Mi", "0", or "—" when the resource is not
// reported). Warning is set when the node reserves no memory; NoEphemeral/NoCPU
// when it reserves no ephemeral-storage / cpu.
type NodeReservation struct {
	Name              string `json:"name"`
	Role              string `json:"role,omitempty"`
	CPUReserved       string `json:"cpuReserved"`
	MemReserved       string `json:"memReserved"`
	EphemeralReserved string `json:"ephemeralReserved"`
	Warning           bool   `json:"warning"`
	NoEphemeral       bool   `json:"noEphemeralReserve,omitempty"`
	NoCPU             bool   `json:"noCPUReserve,omitempty"`
}

// Report is the per-node reservation picture. WarnCount is the number of nodes
// reserving no memory (unchanged; drives the daemon gauge). EphemeralNone/CPUNone
// count nodes reserving none of that resource; EphemeralReporting is the number of
// nodes that report ephemeral-storage capacity at all.
type Report struct {
	Nodes              []NodeReservation `json:"nodes"`
	WarnCount          int               `json:"warnCount"`
	EphemeralNone      int               `json:"ephemeralNone"`
	CPUNone            int               `json:"cpuNone"`
	EphemeralReporting int               `json:"ephemeralReporting"`
}

// Assess computes reserved cpu/memory/ephemeral-storage for each node as
// Capacity - Allocatable (clamped at 0) and flags nodes that reserve none of
// memory (Warning), ephemeral-storage (NoEphemeral), or cpu (NoCPU).
func Assess(nodes []corev1.Node) Report {
	rep := Report{Nodes: make([]NodeReservation, 0, len(nodes))}
	for _, n := range nodes {
		cpuRes := reserved(n.Status.Capacity[corev1.ResourceCPU], n.Status.Allocatable[corev1.ResourceCPU])
		memRes := reserved(n.Status.Capacity[corev1.ResourceMemory], n.Status.Allocatable[corev1.ResourceMemory])

		nr := NodeReservation{
			Name:        n.Name,
			Role:        role(n),
			CPUReserved: fmtCPU(cpuRes),
			MemReserved: fmtMem(memRes),
			Warning:     memRes.Value() == 0,
			NoCPU:       cpuRes.MilliValue() == 0,
		}
		if nr.Warning {
			rep.WarnCount++
		}
		if nr.NoCPU {
			rep.CPUNone++
		}

		if capEph, ok := n.Status.Capacity[corev1.ResourceEphemeralStorage]; ok {
			ephRes := reserved(capEph, n.Status.Allocatable[corev1.ResourceEphemeralStorage])
			nr.EphemeralReserved = fmtMem(ephRes)
			nr.NoEphemeral = ephRes.Value() == 0
			rep.EphemeralReporting++
			if nr.NoEphemeral {
				rep.EphemeralNone++
			}
		} else {
			nr.EphemeralReserved = "—"
		}

		rep.Nodes = append(rep.Nodes, nr)
	}
	return rep
}
```

Also update the package doc comment (top of file) — change the first sentence to: `// Package nodereserve reports each node's aggregate kubelet resource reservation` `// for cpu, memory, and ephemeral-storage, observed as Capacity - Allocatable` `// (kube-reserved + system-reserved + eviction-hard combined). It warns when a node` `// reserves no memory or no ephemeral-storage, kubelet configurations that let` `// OS/kubelet pressure destabilise the node. Pure: the caller supplies the nodes. Read-only.`

- [ ] **Step 4: Run to verify they pass**

Run: `go build ./... && go test ./internal/nodereserve/`
Expected: PASS (4 new tests + all existing tests still green).

- [ ] **Step 5: Commit**

```bash
git add internal/nodereserve/nodereserve.go internal/nodereserve/nodereserve_test.go
git commit -m "feat(nodereserve): cover ephemeral-storage + cpu/ephemeral reserve-none flags"
```

---

### Task 2: Report rendering — NOTES (memory + ephemeral) and per-resource CONTEXT block

**Files:**
- Modify: `internal/report/report.go` (`printNotes` node-reserve block ~lines 242-250; `printContext` node-reserve block ~lines 276-284; add `reserveLine` helper)
- Test: `internal/report/report_test.go` (update 2 existing tests, add 2 new)

**Interfaces:**
- Consumes: `nodereserve.Report` with `WarnCount`, `EphemeralNone`, `CPUNone`, `EphemeralReporting` and per-node `Warning`, `NoEphemeral` (Task 1). Existing `plural(n int, one, many string) string` helper in `report.go`.
- Produces: no new exported API.

- [ ] **Step 1: Write / update the failing tests**

In `internal/report/report_test.go`:

(a) Update `TestPrintInventory_TextShowsNodeReservations` — replace the CONTEXT assertion block (the `if !strings.Contains(out, "1/2 reserve memory OK")` check) with:

```go
	// CONTEXT shows the per-resource reservation block.
	if !strings.Contains(out, "Kubelet reservations (combined kube+system)") {
		t.Errorf("missing reservations block header in:\n%s", out)
	}
	if !strings.Contains(out, "1 of 2 nodes reserve none") {
		t.Errorf("missing per-resource memory status in:\n%s", out)
	}
```

(b) Update `TestPrintInventory_NodeReservationsCollapseWhenAllOK` — replace the `if !strings.Contains(out, "reservations OK")` check with:

```go
	if !strings.Contains(out, "all 2 nodes reserve some") {
		t.Errorf("missing all-OK reservation status in:\n%s", out)
	}
```

(c) Add two new tests:

```go
func TestPrintInventory_NoEphemeralWarnsAndContext(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{
		WarnCount: 0, EphemeralNone: 1, EphemeralReporting: 2,
		Nodes: []nodereserve.NodeReservation{
			{Name: "diskless", CPUReserved: "200m", MemReserved: "1Gi", EphemeralReserved: "0", NoEphemeral: true},
			{Name: "ok", CPUReserved: "200m", MemReserved: "1Gi", EphemeralReserved: "5Gi"},
		},
	}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 2, NodesTotal: 2}, NodeReserve: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "reserve no ephemeral-storage: diskless") || !strings.Contains(out, "disk pressure can destabilize the node") {
		t.Errorf("expected ephemeral NOTES warning naming diskless in:\n%s", out)
	}
	// WarnCount==0 here, so memory reads "all ... reserve some"; the
	// "reserve none ⚠" status uniquely identifies the ephemeral-storage line.
	if !strings.Contains(out, "1 of 2 nodes reserve none  ⚠") {
		t.Errorf("expected ephemeral CONTEXT line with warn glyph in:\n%s", out)
	}
}

func TestPrintInventory_ReservationsNotReportedEphemeral(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{
		WarnCount: 0, EphemeralReporting: 0,
		Nodes: []nodereserve.NodeReservation{
			{Name: "n1", CPUReserved: "200m", MemReserved: "1Gi", EphemeralReserved: "—"},
		},
	}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, NodeReserve: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "ephemeral-storage not reported") {
		t.Errorf("expected 'ephemeral-storage not reported' in:\n%s", out)
	}
	if strings.Contains(out, "reserve no ephemeral-storage") {
		t.Errorf("must not warn on ephemeral when not reported:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run 'NodeReserv|Ephemeral|Reservation'`
Expected: FAIL — the old CONTEXT strings are gone / the new `reserveLine`-formatted strings and the ephemeral NOTES bullet don't exist yet.

- [ ] **Step 3: Implement `printNotes` (memory + ephemeral)**

In `internal/report/report.go`, replace the node-reserve block at the top of `printNotes` (the `if n := in.NodeReserve; n != nil && n.WarnCount > 0 { ... }` block) with:

```go
	if n := in.NodeReserve; n != nil {
		if n.WarnCount > 0 {
			var names []string
			for _, r := range n.Nodes {
				if r.Warning {
					names = append(names, r.Name)
				}
			}
			fmt.Fprintf(&b, "  • %d %s reserve no memory: %s\n", n.WarnCount, plural(n.WarnCount, "node", "nodes"), strings.Join(names, ", "))
			fmt.Fprintln(&b, "      — OS/kubelet memory pressure can destabilize the node")
		}
		if n.EphemeralNone > 0 {
			var names []string
			for _, r := range n.Nodes {
				if r.NoEphemeral {
					names = append(names, r.Name)
				}
			}
			fmt.Fprintf(&b, "  • %d %s reserve no ephemeral-storage: %s\n", n.EphemeralNone, plural(n.EphemeralNone, "node", "nodes"), strings.Join(names, ", "))
			fmt.Fprintln(&b, "      — disk pressure can destabilize the node")
		}
	}
```

- [ ] **Step 4: Implement `printContext` block + `reserveLine` helper**

In `internal/report/report.go`, replace the node-reserve block in `printContext` (the `if n := in.NodeReserve; n != nil && len(n.Nodes) > 0 { ... }` block that builds the `Nodes  %d/%d reserve memory OK` line) with:

```go
	if n := in.NodeReserve; n != nil && len(n.Nodes) > 0 {
		total := len(n.Nodes)
		fmt.Fprintln(&b, "Kubelet reservations (combined kube+system)")
		fmt.Fprintln(&b, reserveLine("memory", n.WarnCount, total, true))
		fmt.Fprintln(&b, reserveLine("cpu", n.CPUNone, total, false))
		if n.EphemeralReporting == 0 {
			fmt.Fprintf(&b, "  %-17s %s\n", "ephemeral-storage", "not reported")
		} else {
			fmt.Fprintln(&b, reserveLine("ephemeral-storage", n.EphemeralNone, n.EphemeralReporting, true))
		}
	}
```

Add the helper near `printContext` (before or after it, package level):

```go
// reserveLine formats one CONTEXT reservation line, padded so statuses align.
// warn=true appends ⚠ (some node reserves none) or ✓ (all reserve some) — used
// for memory and ephemeral-storage; cpu (warn=false) gets no glyph (informational).
func reserveLine(label string, none, reporting int, warn bool) string {
	var status string
	if none == 0 {
		status = fmt.Sprintf("all %d %s reserve some", reporting, plural(reporting, "node", "nodes"))
		if warn {
			status += "  ✓"
		}
	} else {
		status = fmt.Sprintf("%d of %d nodes reserve none", none, reporting)
		if warn {
			status += "  ⚠"
		}
	}
	return fmt.Sprintf("  %-17s %s", label, status)
}
```

- [ ] **Step 5: Run to verify pass + gofmt + full suite**

Run: `go build ./... && gofmt -l internal/report/report.go && go test ./internal/report/ && go test ./...`
Expected: build OK, gofmt silent, all report tests pass (the 2 updated + 2 new + the unchanged `TestPrintInventory_NodeReservationsWarningIsNote` and JSON test), full suite green.

- [ ] **Step 6: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): per-resource kubelet-reservation block + no-ephemeral warning"
```

---

### Task 3: Docs

**Files:**
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Changed`)
- Modify: `website/docs/features/diagnostics.md` (the `### Node reservations` section, ~lines 55-65)

**Interfaces:** none.

- [ ] **Step 1: CHANGELOG**

Under the existing `## [Unreleased]` heading in `CHANGELOG.md`, add a `### Changed` subsection (create it if absent, below any existing `### Added`):

```markdown
### Changed

- **Node-reservation reporting is clearer and multi-resource.** `scan` now reports
  the combined kube+system reservation for **memory, CPU, and ephemeral-storage**
  in a labeled per-resource `CONTEXT` block (replacing the cryptic
  `Nodes 0/2 reserve memory OK` line). Reserving no **ephemeral-storage** now
  raises a `NOTES` warning alongside the existing no-memory warning (both are
  node-destabilizers); CPU is informational. Still read-only and advisory; the
  `watch` daemon and `kubeagent_nodes_without_reservations` gauge are unchanged.
```

- [ ] **Step 2: diagnostics.md**

Replace the body of the `### Node reservations` section in `website/docs/features/diagnostics.md` (the paragraph currently starting "`scan` shows each node's aggregate kubelet resource reservation…") with:

```markdown
`scan` reports each node's aggregate kubelet resource reservation for **memory,
CPU, and ephemeral-storage**, computed as `Capacity − Allocatable` (the combined
effect of `system-reserved`, `kube-reserved`, and `eviction-hard` — the Node API
cannot split kube- from system-reserved). A per-resource summary appears under
`CONTEXT` (e.g. `memory  2 of 2 nodes reserve none ⚠`). A node that reserves no
**memory** or no **ephemeral-storage** is flagged with a **WARNING** in `NOTES` —
both let OS/kubelet memory or disk pressure destabilise the node. CPU reservation
is shown but not warned, since it is compressible and many clusters intentionally
leave it unset; a resource a node does not report is shown as `not reported`. The
check reads only the Node objects already listed during a scan, so it needs no
extra permissions, and it is advisory: it never changes the cluster verdict.
```

- [ ] **Step 3: Verify docs build**

Run: `cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml`
Expected: exit 0, "Documentation built", no page WARNING lines (ignore the cosmetic Material team banner). If the venv is missing, recreate: `python3 -m venv <path> && <path>/bin/pip install -r requirements.txt`.

- [ ] **Step 4: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md
git commit -m "docs: document multi-resource node-reservation reporting"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && gofmt -l . && go test ./...
```

Expected: build/vet clean, gofmt lists only pre-existing files (not the ones touched here), all tests pass. Manual smoke against a cluster whose nodes reserve nothing:

```bash
go build -o kubeagent . && ./kubeagent scan | sed -n '/Kubelet reservations/,/Platform/p'
```

Expected: the `Kubelet reservations (combined kube+system)` block with per-resource lines; `NOTES` bullets for no-memory and (if applicable) no-ephemeral-storage.
