# Node & PVC Disk-Usage Check Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An opt-in `--disk-usage` check that reads each node's kubelet `/stats/summary` (via `nodes/proxy`) and warns when a node's root filesystem or a PVC is at/over a threshold (default 0.80).

**Architecture:** A new pure package `internal/diskusage` computes over-threshold volumes from parsed kubelet summaries; `collect.NodeStats` fetches+parses one node's summary read-only; `scan.Evaluate` gathers them per-node only when enabled; `internal/report` renders over-threshold volumes in NEEDS ATTENTION; the watch daemon exposes gauges. RBAC for `nodes/proxy` is a documented opt-in add-on.

**Tech Stack:** Go 1.26, `k8s.io/client-go` RESTClient raw GET (`AbsPath().DoRaw()`), `encoding/json` hand-parsing (no new dependency). Tests use fake summaries/JSON.

## Global Constraints

- **Opt-in only.** Off by default: no `nodes/proxy` call and no RBAC unless `--disk-usage` (CLI) / `KUBEAGENT_DISK_USAGE=true` (daemon). Base `deploy/rbac.yaml` + Helm ClusterRole stay `get`/`list`/`watch`.
- **No new dependency** — parse the kubelet summary JSON with a local struct (do not import `k8s.io/kubelet`).
- **Best-effort collection** — a forbidden/unreachable node's stats are skipped; the scan never fails on it.
- **Warn rule:** report a volume when `usedBytes/capacityBytes >= threshold`; skip `capacityBytes == 0`.
- **Advisory** — must NOT change the `clusterhealth` verdict, `kubeagent_cluster_healthy`, or the scan exit code. Not sent to `--explain`.
- **No `--fix`** for disk-full.
- `--output json` includes `diskUsage` only when the check ran.
- Threshold default is `0.80`; tunable via `--disk-threshold` / `KUBEAGENT_DISK_THRESHOLD`.
- Commits carry **no `Co-Authored-By: Claude` trailer**.
- TDD: failing test first, watch it fail, implement, pass, commit.

---

### Task 1: `internal/diskusage` package

**Files:**
- Create: `internal/diskusage/diskusage.go`
- Test: `internal/diskusage/diskusage_test.go`

**Interfaces:**
- Produces:
  - `type NodeSummary struct { Node string; FSUsed, FSCap int64; Volumes []PVCVolume }`
  - `type PVCVolume struct { Namespace, Name string; Used, Cap int64 }`
  - `type VolumeUsage struct { Kind, Node, Namespace, Name string; UsedBytes, CapacityBytes int64; Ratio float64 }` (JSON tags below)
  - `type Report struct { Over []VolumeUsage; Nodes []VolumeUsage; Threshold float64 }` (JSON tags below)
  - `func Assess(stats []NodeSummary, threshold float64) Report`

- [ ] **Step 1: Write the failing test**

Create `internal/diskusage/diskusage_test.go`:

```go
package diskusage

import "testing"

func gib(n int64) int64 { return n << 30 }

func TestAssess_NodeOverAndUnder(t *testing.T) {
	stats := []NodeSummary{
		{Node: "n1", FSUsed: gib(170), FSCap: gib(200)}, // 85% -> over
		{Node: "n2", FSUsed: gib(50), FSCap: gib(200)},  // 25% -> under
	}
	r := Assess(stats, 0.80)
	if len(r.Over) != 1 || r.Over[0].Kind != "node" || r.Over[0].Name != "n1" {
		t.Fatalf("want only n1 over, got %+v", r.Over)
	}
	if len(r.Nodes) != 2 {
		t.Errorf("want all-node ratios for the metric, got %d", len(r.Nodes))
	}
	if r.Threshold != 0.80 {
		t.Errorf("want threshold echoed, got %v", r.Threshold)
	}
}

func TestAssess_PVCOverAndSkipZeroCap(t *testing.T) {
	stats := []NodeSummary{{
		Node: "n1", FSUsed: gib(1), FSCap: gib(100), // 1% under
		Volumes: []PVCVolume{
			{Namespace: "shop", Name: "data", Used: gib(46), Cap: gib(50)}, // 92% over
			{Namespace: "shop", Name: "cache", Used: gib(1), Cap: gib(50)}, // 2% under
			{Namespace: "shop", Name: "nostat", Used: 0, Cap: 0},           // no capacity -> skipped
		},
	}}
	r := Assess(stats, 0.80)
	if len(r.Over) != 1 || r.Over[0].Kind != "pvc" || r.Over[0].Name != "data" {
		t.Fatalf("want only shop/data over, got %+v", r.Over)
	}
	if r.Over[0].Namespace != "shop" || r.Over[0].CapacityBytes != gib(50) {
		t.Errorf("wrong pvc row: %+v", r.Over[0])
	}
}

func TestAssess_SortsByRatioDesc(t *testing.T) {
	stats := []NodeSummary{{
		Node: "n1", FSUsed: gib(90), FSCap: gib(100), // node 90%
		Volumes: []PVCVolume{{Namespace: "a", Name: "p", Used: gib(95), Cap: gib(100)}}, // pvc 95%
	}}
	r := Assess(stats, 0.80)
	if len(r.Over) != 2 || r.Over[0].Ratio < r.Over[1].Ratio {
		t.Fatalf("want highest ratio first, got %+v", r.Over)
	}
	if r.Over[0].Name != "p" {
		t.Errorf("want pvc p (95%%) first, got %q", r.Over[0].Name)
	}
}

func TestAssess_EmptyWhenNoneOver(t *testing.T) {
	r := Assess([]NodeSummary{{Node: "n1", FSUsed: gib(1), FSCap: gib(100)}}, 0.80)
	if len(r.Over) != 0 {
		t.Errorf("want no over-threshold entries, got %+v", r.Over)
	}
	if len(r.Nodes) != 1 {
		t.Errorf("all-node ratios still populated, got %d", len(r.Nodes))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/diskusage/`
Expected: FAIL — `undefined: Assess` / undefined types.

- [ ] **Step 3: Write the implementation**

Create `internal/diskusage/diskusage.go`:

```go
// Package diskusage flags node root filesystems and PVCs at or over a usage
// threshold, from parsed kubelet /stats/summary data. Pure: the caller supplies
// the per-node summaries. Read-only, opt-in (the collection needs nodes/proxy).
package diskusage

import "sort"

// NodeSummary is the slice of a node's kubelet /stats/summary that we consume.
type NodeSummary struct {
	Node    string
	FSUsed  int64
	FSCap   int64
	Volumes []PVCVolume
}

// PVCVolume is one pod volume backed by a PVC.
type PVCVolume struct {
	Namespace string
	Name      string
	Used      int64
	Cap       int64
}

// VolumeUsage is one node fs or PVC's usage.
type VolumeUsage struct {
	Kind          string  `json:"kind"` // "node" | "pvc"
	Node          string  `json:"node,omitempty"`
	Namespace     string  `json:"namespace,omitempty"`
	Name          string  `json:"name"`
	UsedBytes     int64   `json:"usedBytes"`
	CapacityBytes int64   `json:"capacityBytes"`
	Ratio         float64 `json:"ratio"`
}

// Report is the disk-usage picture. Over holds node+PVC volumes at/over the
// threshold (for display), highest ratio first. Nodes holds every node's fs
// ratio (for the daemon gauge), regardless of threshold.
type Report struct {
	Over      []VolumeUsage `json:"over"`
	Nodes     []VolumeUsage `json:"nodes,omitempty"`
	Threshold float64       `json:"threshold"`
}

// Assess flags volumes whose used/capacity ratio is >= threshold. Volumes with
// zero capacity are skipped.
func Assess(stats []NodeSummary, threshold float64) Report {
	rep := Report{Over: []VolumeUsage{}, Threshold: threshold}
	for _, s := range stats {
		if s.FSCap > 0 {
			r := ratio(s.FSUsed, s.FSCap)
			node := VolumeUsage{Kind: "node", Node: s.Node, Name: s.Node, UsedBytes: s.FSUsed, CapacityBytes: s.FSCap, Ratio: r}
			rep.Nodes = append(rep.Nodes, node)
			if r >= threshold {
				rep.Over = append(rep.Over, node)
			}
		}
		for _, v := range s.Volumes {
			if v.Cap <= 0 {
				continue
			}
			r := ratio(v.Used, v.Cap)
			if r >= threshold {
				rep.Over = append(rep.Over, VolumeUsage{
					Kind: "pvc", Namespace: v.Namespace, Name: v.Name,
					UsedBytes: v.Used, CapacityBytes: v.Cap, Ratio: r,
				})
			}
		}
	}
	sort.SliceStable(rep.Over, func(i, j int) bool {
		if rep.Over[i].Ratio != rep.Over[j].Ratio {
			return rep.Over[i].Ratio > rep.Over[j].Ratio
		}
		return rep.Over[i].Name < rep.Over[j].Name
	})
	return rep
}

func ratio(used, capacity int64) float64 {
	return float64(used) / float64(capacity)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/diskusage/`
Expected: PASS (all 4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/diskusage/
git commit -m "feat(diskusage): flag node fs and PVC volumes over a usage threshold"
```

---

### Task 2: `collect.NodeStats` (kubelet summary via nodes/proxy)

**Files:**
- Modify: `internal/collect/collect.go` (new `NodeStats` + `parseNodeSummary`, import `diskusage`)
- Test: `internal/collect/collect_test.go` (test `parseNodeSummary` with canned JSON)

**Interfaces:**
- Consumes: `diskusage.NodeSummary` / `diskusage.PVCVolume` from Task 1.
- Produces: `func NodeStats(ctx context.Context, client kubernetes.Interface, node string) (diskusage.NodeSummary, bool, error)`.

`collect.go` already imports `encoding/json` (used by `parseNodeMetrics`), `fmt`, `context`, `kubernetes`. Add the `diskusage` import.

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go`:

```go
func TestParseNodeSummary_NodeFSAndPVCVolumes(t *testing.T) {
	data := []byte(`{
	  "node": {"fs": {"usedBytes": 170000000000, "capacityBytes": 200000000000}},
	  "pods": [
	    {"volume": [
	      {"usedBytes": 46000000000, "capacityBytes": 50000000000, "pvcRef": {"name": "data", "namespace": "shop"}},
	      {"usedBytes": 10, "capacityBytes": 20}
	    ]},
	    {"volume": [
	      {"usedBytes": 5, "capacityBytes": 10, "pvcRef": {"name": "cache", "namespace": "shop"}}
	    ]}
	  ]
	}`)
	s, ok, err := parseNodeSummary("n1", data)
	if err != nil || !ok {
		t.Fatalf("parse failed: ok=%v err=%v", ok, err)
	}
	if s.Node != "n1" || s.FSUsed != 170000000000 || s.FSCap != 200000000000 {
		t.Errorf("wrong node fs: %+v", s)
	}
	// Only volumes with a pvcRef are kept.
	if len(s.Volumes) != 2 {
		t.Fatalf("want 2 pvc volumes, got %d (%+v)", len(s.Volumes), s.Volumes)
	}
	if s.Volumes[0].Namespace != "shop" || s.Volumes[0].Name != "data" || s.Volumes[0].Cap != 50000000000 {
		t.Errorf("wrong first volume: %+v", s.Volumes[0])
	}
}

func TestParseNodeSummary_BadJSON(t *testing.T) {
	if _, ok, err := parseNodeSummary("n1", []byte("not json")); ok || err == nil {
		t.Errorf("want (false, err) on bad json, got ok=%v err=%v", ok, err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -run TestParseNodeSummary`
Expected: FAIL to compile — `undefined: parseNodeSummary`.

- [ ] **Step 3: Implement `NodeStats` + `parseNodeSummary`**

Add the import `"github.com/imantaba/kubeagent/internal/diskusage"` to `collect.go`, then add (after `NodeMetrics`):

```go
// NodeStats fetches one node's kubelet /stats/summary through the nodes/proxy
// subresource (read-only). A forbidden or unreachable node yields
// (zero, false, nil) so a scan still succeeds without it. Requires the
// nodes/proxy grant (opt-in; see deploy/rbac-diskusage.yaml).
func NodeStats(ctx context.Context, client kubernetes.Interface, node string) (diskusage.NodeSummary, bool, error) {
	data, err := client.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/nodes/%s/proxy/stats/summary", node)).DoRaw(ctx)
	if err != nil {
		return diskusage.NodeSummary{}, false, nil // forbidden/unreachable — non-fatal
	}
	return parseNodeSummary(node, data)
}

// parseNodeSummary decodes the kubelet Summary JSON we consume: the node root
// filesystem and each pod volume that carries a pvcRef.
func parseNodeSummary(node string, data []byte) (diskusage.NodeSummary, bool, error) {
	var raw struct {
		Node struct {
			Fs struct {
				UsedBytes     int64 `json:"usedBytes"`
				CapacityBytes int64 `json:"capacityBytes"`
			} `json:"fs"`
		} `json:"node"`
		Pods []struct {
			Volume []struct {
				UsedBytes     int64 `json:"usedBytes"`
				CapacityBytes int64 `json:"capacityBytes"`
				PVCRef        *struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"pvcRef"`
			} `json:"volume"`
		} `json:"pods"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return diskusage.NodeSummary{}, false, err
	}
	out := diskusage.NodeSummary{Node: node, FSUsed: raw.Node.Fs.UsedBytes, FSCap: raw.Node.Fs.CapacityBytes}
	for _, p := range raw.Pods {
		for _, v := range p.Volume {
			if v.PVCRef == nil {
				continue
			}
			out.Volumes = append(out.Volumes, diskusage.PVCVolume{
				Namespace: v.PVCRef.Namespace, Name: v.PVCRef.Name,
				Used: v.UsedBytes, Cap: v.CapacityBytes,
			})
		}
	}
	return out, true, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go build ./... && go test ./internal/collect/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go
git commit -m "feat(collect): NodeStats reads kubelet /stats/summary via nodes/proxy"
```

---

### Task 3: Wire disk usage into `scan`

**Files:**
- Modify: `internal/scan/scan.go` (`Options`, `Result`, `Evaluate`, import `diskusage`)
- Test: `internal/scan/scan_test.go` (add a test that disk usage is off by default)

**Interfaces:**
- Consumes: `collect.NodeStats`, `diskusage.Assess` from Tasks 1–2.
- Produces: `scan.Options.DiskUsage bool` + `scan.Options.DiskThreshold float64`; `scan.Result.DiskUsage diskusage.Report`.

- [ ] **Step 1: Add fields to `Options` and `Result`**

In `internal/scan/scan.go`, add the import `"github.com/imantaba/kubeagent/internal/diskusage"`, then:

```go
type Options struct {
	Namespace       string
	IncludeCron     bool
	IncludeRestarts bool
	DiskUsage       bool
	DiskThreshold   float64
}
```

Add a field to `Result` (after `PVCReclaim`):

```go
	DiskUsage     diskusage.Report
```

- [ ] **Step 2: Collect per-node stats in `Evaluate` when enabled**

In `Evaluate`, after the `nodes` are collected and before the final `return`, add:

```go
	var diskReport diskusage.Report
	if opts.DiskUsage {
		var summaries []diskusage.NodeSummary
		for _, n := range nodes {
			if s, ok, _ := collect.NodeStats(ctx, client, n.Name); ok {
				summaries = append(summaries, s)
			}
		}
		diskReport = diskusage.Assess(summaries, opts.DiskThreshold)
	}
```

Add `DiskUsage: diskReport` to the returned `Result` literal.

- [ ] **Step 3: Write the failing test**

Add to `internal/scan/scan_test.go` (it uses the fake clientset; check its existing imports and mirror them):

```go
func TestEvaluate_DiskUsageOffByDefault(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
	)
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.DiskUsage.Over) != 0 || len(res.DiskUsage.Nodes) != 0 {
		t.Errorf("disk usage must be empty when not enabled, got %+v", res.DiskUsage)
	}
}
```

If `scan_test.go` does not already import `corev1`/`metav1`/`fake`, add them (mirror `internal/collect/collect_test.go`).

- [ ] **Step 4: Run tests**

Run: `go build ./... && go test ./internal/scan/`
Expected: PASS (disk usage stays zero when `Options{}` has `DiskUsage=false`).

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): gather per-node disk usage when opts.DiskUsage is set"
```

---

### Task 4: Report — NEEDS ATTENTION disk lines + JSON

**Files:**
- Modify: `internal/report/report.go` (`Input`, `inventoryReport`, `printInventoryText`, add `printDiskUsage` + `fmtBytes`, import `diskusage`)
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `diskusage.Report` / `diskusage.VolumeUsage` from Task 1; `scan.Result.DiskUsage` from Task 3.
- Produces: `report.Input.DiskUsage *diskusage.Report`.

`report.Input` is a struct, so adding a field needs no callsite changes.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go` (add the `"github.com/imantaba/kubeagent/internal/diskusage"` import):

```go
func TestPrintInventory_TextShowsDiskUsageInNeedsAttention(t *testing.T) {
	var buf bytes.Buffer
	rep := &diskusage.Report{Threshold: 0.80, Over: []diskusage.VolumeUsage{
		{Kind: "pvc", Namespace: "clickhouse", Name: "data", UsedBytes: 46 << 30, CapacityBytes: 50 << 30, Ratio: 0.92},
		{Kind: "node", Node: "n1", Name: "n1", UsedBytes: 168 << 30, CapacityBytes: 200 << 30, Ratio: 0.84},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, DiskUsage: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "NEEDS ATTENTION") {
		t.Errorf("disk usage should trip the NEEDS ATTENTION zone:\n%s", out)
	}
	if !strings.Contains(out, "✗ pvc clickhouse/data") || !strings.Contains(out, "92% full") {
		t.Errorf("missing pvc disk line:\n%s", out)
	}
	if !strings.Contains(out, "✗ node n1") || !strings.Contains(out, "84% full") {
		t.Errorf("missing node disk line:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must be suppressed when a disk is over threshold:\n%s", out)
	}
}

func TestPrintInventory_DiskUsageAbsentWhenNilOrEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, DiskUsage: &diskusage.Report{Threshold: 0.80}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "disk") {
		t.Errorf("no disk lines expected for an empty report:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("empty disk report must not suppress all-clear:\n%s", buf.String())
	}
}

func TestPrintInventory_JSONIncludesDiskUsage(t *testing.T) {
	var buf bytes.Buffer
	rep := &diskusage.Report{Threshold: 0.80, Over: []diskusage.VolumeUsage{
		{Kind: "node", Node: "n1", Name: "n1", UsedBytes: 168 << 30, CapacityBytes: 200 << 30, Ratio: 0.84},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, DiskUsage: rep}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["diskUsage"].(map[string]any); !ok {
		t.Fatalf("diskUsage missing in JSON: %s", buf.String())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/report/ -run DiskUsage`
Expected: FAIL to compile — `Input` has no `DiskUsage` field.

- [ ] **Step 3: Add the field, JSON, and rendering**

In `internal/report/report.go`: add the import `"github.com/imantaba/kubeagent/internal/diskusage"`. Add to `Input` (after `PVCReclaim`/`PVCReclaimFull`):

```go
	DiskUsage          *diskusage.Report
```

Add to `inventoryReport` (after `PVCReclaim`):

```go
	DiskUsage          *diskusage.Report           `json:"diskUsage,omitempty"`
```

In the JSON encode call in `PrintInventory`, add `DiskUsage: in.DiskUsage,` to the `inventoryReport{...}` literal.

In `printInventoryText`, extend the `hasAttention` condition and render the disk lines inside the NEEDS ATTENTION block, after the credential warnings:

```go
	hasDisk := in.DiskUsage != nil && len(in.DiskUsage.Over) > 0
	hasAttention := len(in.Result.Workloads) > 0 || len(real) > 0 || len(in.CredentialWarnings) > 0 || hasDisk
	if hasAttention {
		if _, err := fmt.Fprintln(w, "NEEDS ATTENTION"); err != nil {
			return err
		}
		for _, wl := range in.Result.Workloads {
			if err := printWorkload(wl, w); err != nil {
				return err
			}
		}
		if err := printServiceIssues(real, "  ✗", w); err != nil {
			return err
		}
		if err := printCredentialWarnings(in.CredentialWarnings, w); err != nil {
			return err
		}
		if err := printDiskUsage(in.DiskUsage, w); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
```

Add the renderer and a byte formatter (place near `printPVCReclaim`):

```go
// printDiskUsage lists node filesystems and PVCs at or over the threshold.
func printDiskUsage(rep *diskusage.Report, w io.Writer) error {
	if rep == nil {
		return nil
	}
	for _, v := range rep.Over {
		pct := int(v.Ratio*100 + 0.5)
		var line string
		if v.Kind == "node" {
			line = fmt.Sprintf("  ✗ node %s  disk %d%% full (%s/%s)", v.Name, pct, fmtBytes(v.UsedBytes), fmtBytes(v.CapacityBytes))
		} else {
			line = fmt.Sprintf("  ✗ pvc %s/%s  %d%% full (%s/%s)", v.Namespace, v.Name, pct, fmtBytes(v.UsedBytes), fmtBytes(v.CapacityBytes))
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// fmtBytes renders a byte count in Gi/Mi (or B below 1Mi).
func fmtBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.0fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go build ./... && go test ./internal/report/`
Expected: PASS (3 new tests + existing unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): show over-threshold disks in NEEDS ATTENTION + diskUsage JSON"
```

---

### Task 5: CLI flags `--disk-usage` / `--disk-threshold`

**Files:**
- Modify: `main.go` (scan flag set, `scan.Options`, `report.Input`, usage string)

**Interfaces:**
- Consumes: `scan.Options.DiskUsage/DiskThreshold` (Task 3), `report.Input.DiskUsage` (Task 4).

- [ ] **Step 1: Add the flags**

In the `scan` flag set in `main.go` (near `lint-secrets` / `pvc-reclaim`), add:

```go
	diskUsage := fs.Bool("disk-usage", false, "check node filesystem and PVC usage via the kubelet (needs the nodes/proxy grant)")
	diskThreshold := fs.Float64("disk-threshold", 0.80, "with --disk-usage: warn at this used ratio (0-1)")
```

- [ ] **Step 2: Thread into scan options**

In the `scan.Evaluate(..., scan.Options{...})` call, add:

```go
		DiskUsage:       *diskUsage,
		DiskThreshold:   *diskThreshold,
```

- [ ] **Step 3: Pass into the report (only when enabled)**

Before the `report.PrintInventory` call, add:

```go
	var diskRep *diskusage.Report
	if *diskUsage {
		diskRep = &res.DiskUsage
	}
```

Add `DiskUsage: diskRep,` to the `report.Input{...}` literal, and add the import `"github.com/imantaba/kubeagent/internal/diskusage"` to `main.go`.

- [ ] **Step 4: Update the usage string**

In the `scan` usage error string, add `[--disk-usage [--disk-threshold r]]` after `[--pvc-reclaim]`.

- [ ] **Step 5: Verify build + flag**

Run:

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o kubeagent . && ./kubeagent scan -h 2>&1 | grep -E 'disk-usage|disk-threshold'
go test ./...
```

Expected: both flags appear; all tests pass.

- [ ] **Step 6: Commit**

```bash
git add main.go
git commit -m "feat(scan): add --disk-usage and --disk-threshold flags"
```

---

### Task 6: Daemon — env toggle + metrics

**Files:**
- Modify: `internal/watch/watch.go` (`Config` fields + `scan.Options` mapping)
- Modify: `internal/watch/metrics.go` (`metrics` fields, `update`, `render`)
- Modify: `internal/watch/metrics_test.go` (assert the gauges)
- Modify: `main.go` (`runWatch` reads `KUBEAGENT_DISK_USAGE`/`KUBEAGENT_DISK_THRESHOLD`; add `envBool`/`envFloat` helpers)

**Interfaces:**
- Consumes: `scan.Result.DiskUsage` (Task 3).

- [ ] **Step 1: Add Config fields and map into scan options**

In `internal/watch/watch.go`, add to `Config`:

```go
	DiskUsage       bool
	DiskThreshold   float64
```

In the `scan.Options{...}` built in `Run` (currently `opts := scan.Options{Namespace: cfg.Namespace, IncludeCron: cfg.IncludeCron, IncludeRestarts: cfg.IncludeRestarts}`), add `DiskUsage: cfg.DiskUsage, DiskThreshold: cfg.DiskThreshold`.

- [ ] **Step 2: Write the failing metrics test**

In `internal/watch/metrics_test.go`, extend `sampleResult()` to include a disk report and assert the gauges. Add `"github.com/imantaba/kubeagent/internal/diskusage"` to the imports, set on the sample:

```go
		DiskUsage: diskusage.Report{
			Threshold: 0.80,
			Over:      []diskusage.VolumeUsage{{Kind: "node", Node: "n1", Name: "n1", Ratio: 0.84}},
			Nodes:     []diskusage.VolumeUsage{{Kind: "node", Node: "n1", Name: "n1", Ratio: 0.84}},
		},
```

Add to the `want` list in `TestMetrics_RenderReflectsResult`:

```go
		`kubeagent_node_fs_usage_ratio{node="n1"} 0.84`,
		"kubeagent_volumes_over_disk_threshold 1",
```

- [ ] **Step 3: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult`
Expected: FAIL — gauges missing (and compile error until fields exist).

- [ ] **Step 4: Add the metrics fields, update, and render**

In `internal/watch/metrics.go`, add to the `metrics` struct:

```go
	nodeFSRatio     map[string]float64
	volumesOverDisk int
```

In `update`, after the existing gauge assignments in the success path, add:

```go
	if len(res.DiskUsage.Nodes) > 0 {
		ratios := make(map[string]float64, len(res.DiskUsage.Nodes))
		for _, n := range res.DiskUsage.Nodes {
			ratios[n.Node] = n.Ratio
		}
		m.nodeFSRatio = ratios
		m.volumesOverDisk = len(res.DiskUsage.Over)
	}
```

In `render`, after the node gauges, add (deterministic key order):

```go
	if len(m.nodeFSRatio) > 0 {
		names := make([]string, 0, len(m.nodeFSRatio))
		for n := range m.nodeFSRatio {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "# HELP kubeagent_node_fs_usage_ratio Node root filesystem used/capacity (0-1)\n# TYPE kubeagent_node_fs_usage_ratio gauge\n")
		for _, n := range names {
			fmt.Fprintf(&b, "kubeagent_node_fs_usage_ratio{node=%q} %g\n", n, m.nodeFSRatio[n])
		}
		gauge("kubeagent_volumes_over_disk_threshold", "Node+PVC volumes at or over the disk-usage threshold", float64(m.volumesOverDisk))
	}
```

Add `"sort"` to `metrics.go` imports if not present.

- [ ] **Step 5: Read the env in `runWatch` and add helpers**

In `main.go` `runWatch`, add to the `watch.Config{...}`:

```go
		DiskUsage:       envBool("KUBEAGENT_DISK_USAGE", false),
		DiskThreshold:   envFloat("KUBEAGENT_DISK_THRESHOLD", 0.80),
```

Add the helpers near `envDur`:

```go
// envBool parses a boolean env var, falling back to def on empty/invalid.
func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// envFloat parses a float env var, falling back to def on empty/invalid.
func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
```

Add `"strconv"` to `main.go` imports.

- [ ] **Step 6: Run the tests**

Run: `go build ./... && go test ./internal/watch/ ./...`
Expected: PASS (metrics test sees the new gauges; error-path test keeps last-good).

- [ ] **Step 7: Commit**

```bash
git add internal/watch/watch.go internal/watch/metrics.go internal/watch/metrics_test.go main.go
git commit -m "feat(watch): opt-in disk-usage via env + node fs ratio / over-threshold gauges"
```

---

### Task 7: RBAC add-on for `nodes/proxy`

**Files:**
- Create: `deploy/rbac-diskusage.yaml`
- Modify: `deploy/helm/kubeagent/values.yaml` (a `diskUsage` block)
- Modify: `deploy/helm/kubeagent/templates/clusterrole.yaml` (conditional `nodes/proxy` rule)
- Modify: `deploy/helm/kubeagent/templates/deployment.yaml` (conditional `KUBEAGENT_DISK_USAGE` env)

**Interfaces:** none (manifests). Consumed operationally by the daemon when the operator opts in.

- [ ] **Step 1: Create the raw add-on manifest**

Create `deploy/rbac-diskusage.yaml`:

```yaml
# Opt-in add-on: grants the kubeagent ServiceAccount read access to the kubelet
# summary API via the nodes/proxy subresource, needed only when the daemon runs
# with KUBEAGENT_DISK_USAGE=true. Apply alongside deploy/ to enable the
# disk-usage check. Without it, kubeagent stays strictly get/list/watch.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeagent-nodes-proxy
rules:
  - apiGroups: [""]
    resources: [nodes/proxy]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeagent-nodes-proxy
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeagent-nodes-proxy
subjects:
  - kind: ServiceAccount
    name: kubeagent
    namespace: kubeagent
```

- [ ] **Step 2: Add the Helm value**

In `deploy/helm/kubeagent/values.yaml`, add:

```yaml
diskUsage:
  enabled: false
  threshold: "0.80"
```

- [ ] **Step 3: Conditional ClusterRole rule**

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, inside the `rules:` list, add:

```yaml
  {{- if .Values.diskUsage.enabled }}
  - apiGroups: [""]
    resources: [nodes/proxy]
    verbs: [get]
  {{- end }}
```

- [ ] **Step 4: Conditional Deployment env**

In `deploy/helm/kubeagent/templates/deployment.yaml`, add an `env:` block on the container (or extend the existing one) gated on the value:

```yaml
          {{- if .Values.diskUsage.enabled }}
          env:
            - name: KUBEAGENT_DISK_USAGE
              value: "true"
            - name: KUBEAGENT_DISK_THRESHOLD
              value: {{ .Values.diskUsage.threshold | quote }}
          {{- end }}
```

(If the container already has an `env:` block, add these two entries under it instead of a second `env:` key.)

- [ ] **Step 5: Verify the chart renders both states**

Run:

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
echo "--- default: no nodes/proxy, no env ---"
helm template x deploy/helm/kubeagent | grep -E 'nodes/proxy|KUBEAGENT_DISK_USAGE' && echo UNEXPECTED || echo "clean (opt-out)"
echo "--- enabled: nodes/proxy + env present ---"
helm template x deploy/helm/kubeagent --set diskUsage.enabled=true | grep -E 'nodes/proxy|KUBEAGENT_DISK_USAGE'
```

Expected: default render has neither; `--set diskUsage.enabled=true` shows the `nodes/proxy` rule and the `KUBEAGENT_DISK_USAGE` env. Also confirm no write verbs anywhere:

```bash
helm template x deploy/helm/kubeagent --set diskUsage.enabled=true | grep -iE 'create|update|patch|delete' | grep -i verb && echo BAD || echo "read-only OK"
```

- [ ] **Step 6: Commit**

```bash
git add deploy/rbac-diskusage.yaml deploy/helm/kubeagent/values.yaml deploy/helm/kubeagent/templates/clusterrole.yaml deploy/helm/kubeagent/templates/deployment.yaml
git commit -m "feat(deploy): opt-in nodes/proxy RBAC + Helm diskUsage toggle"
```

---

### Task 8: Docs

**Files:**
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `website/docs/features/diagnostics.md`
- Modify: `website/docs/features/watch-mode.md`
- Modify: `deploy/README.md`
- Modify: `README.md`

**Interfaces:** none. Use exact names: flag `--disk-usage` / `--disk-threshold`; env `KUBEAGENT_DISK_USAGE` / `KUBEAGENT_DISK_THRESHOLD`; gauges `kubeagent_node_fs_usage_ratio` / `kubeagent_volumes_over_disk_threshold`; Helm value `diskUsage.enabled`.

- [ ] **Step 1: CHANGELOG**

Add a new `## [Unreleased]` section above the current top entry with:

```markdown
## [Unreleased]

### Added

- **Disk-usage check (opt-in).** `scan --disk-usage` reads each node's kubelet
  `/stats/summary` (via the `nodes/proxy` subresource) and flags node
  filesystems and PVCs at or over a threshold (`--disk-threshold`, default
  `0.80`) in the NEEDS ATTENTION section and JSON `diskUsage` — an early warning
  before the kubelet's `DiskPressure` eviction signal. Off by default (adds no
  RBAC); enable the daemon with `KUBEAGENT_DISK_USAGE=true` and the
  `nodes/proxy` add-on (`deploy/rbac-diskusage.yaml` or Helm
  `diskUsage.enabled=true`), which also exposes `kubeagent_node_fs_usage_ratio`
  and `kubeagent_volumes_over_disk_threshold`. Read-only; advisory (does not
  change the cluster verdict).
```

- [ ] **Step 2: diagnostics.md**

Run: `grep -nE '^#|^##|^###' website/docs/features/diagnostics.md | head -40`

Add a subsection matching the surrounding heading level:

```markdown
### Disk usage (opt-in)

`scan --disk-usage` reads each node's kubelet `/stats/summary` and warns when a
node's root filesystem or a PersistentVolumeClaim is at or over
`--disk-threshold` (default `0.80`) — an early warning that fires before the
kubelet's `DiskPressure` eviction signal. Over-threshold volumes appear in
**NEEDS ATTENTION**; the full detail is in JSON `diskUsage`.

It is **off by default**: it needs the `nodes/proxy` subresource (a broader grant
than kubeagent's usual `get`/`list`/`watch`), so you opt in explicitly with the
flag and, in-cluster, with the `nodes/proxy` RBAC add-on. It never changes the
cluster verdict.
```

- [ ] **Step 3: watch-mode.md**

Run: `grep -nE 'kubeagent_|metric' website/docs/features/watch-mode.md | head -30`

Document enabling the daemon check (`KUBEAGENT_DISK_USAGE=true` / Helm
`diskUsage.enabled=true`, plus the `nodes/proxy` add-on) and the two gauges
`kubeagent_node_fs_usage_ratio{node}` and `kubeagent_volumes_over_disk_threshold`,
matching the format of the neighbouring metric entries.

- [ ] **Step 4: deploy/README.md**

Run: `grep -nE 'Security|RBAC|get.*list.*watch|Helm' deploy/README.md | head`

Add a short "Disk usage (opt-in)" note: applying `deploy/rbac-diskusage.yaml`
(or Helm `diskUsage.enabled=true`) grants `nodes/proxy` `get` and sets
`KUBEAGENT_DISK_USAGE=true`; without it kubeagent stays strictly
`get`/`list`/`watch`.

- [ ] **Step 5: README**

Run: `grep -nE 'node reservation|PVC reclaim|detect' README.md | head`

Add a one-line mention of the opt-in disk-usage check alongside the existing
feature list.

- [ ] **Step 6: Verify the website builds**

Run:

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: "Documentation built" with no strict WARNING lines about the edited pages.

- [ ] **Step 7: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md website/docs/features/watch-mode.md deploy/README.md README.md
git commit -m "docs: document the opt-in disk-usage check and nodes/proxy add-on"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Then a manual smoke against a cluster where you have the `nodes/proxy` grant:

```bash
go build -o kubeagent .
./kubeagent scan --output text                 # no disk section, no proxy calls
./kubeagent scan --disk-usage --output text    # NEEDS ATTENTION disk lines for >=80% volumes
./kubeagent scan --disk-usage --disk-threshold 0.5 --output json | grep -o '"diskUsage"'
```

Expected: default scan makes no `nodes/proxy` call; `--disk-usage` lists over-threshold node/PVC volumes; JSON carries `diskUsage` only when enabled.
