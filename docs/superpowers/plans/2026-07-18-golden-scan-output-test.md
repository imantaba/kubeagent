# Golden-file Scan-Output Test Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A deterministic golden-file test that fails when `kubeagent scan`'s text output changes, so drift is caught (and the README GIF + quickstart example get regenerated) instead of shipping stale docs.

**Architecture:** Test the pure `report.PrintInventory` at the report package (no cluster). Remove the only non-determinism (three `time.Now()` calls that render relative ages) by injecting a clock via a new `Input.Now` field, then snapshot a comprehensive hand-built `report.Input` against a committed golden file.

**Tech Stack:** Go 1.26, standard `testing`, a `-update` flag (`flag.Bool`), `testdata/` golden file.

## Global Constraints

- **No `Co-Authored-By: Claude` trailer** on any commit; author is the human only.
- **Text output only** — JSON is already covered by existing per-field JSON tests; no JSON golden.
- **Clock injection is behaviour-preserving:** a zero `Input.Now` falls back to `time.Now()` (via `nowOr`), so every existing caller/test that leaves `Now` unset behaves exactly as today.
- **Determinism:** the golden test sets a fixed `Input.Now` (`goldenNow`); every fixture timestamp *precedes* it so ages render as fixed strings. `humanAge` renders `Nd`/`Nh`/`Nm`/`Ns` (≥24h → `%dd`) and clamps a negative delta to `0s`.
- **Read-only:** the report is display-only; this change adds no cluster interaction.
- The report renders **every** workload in `in.Result.Workloads` (no `Flagged`/`Priority` filter — the caller pre-selects), so a fixture workload always appears.
- TDD: failing test first, watch it fail, implement, confirm pass, commit.

Run Go with `export PATH=$PATH:/usr/local/go/bin`.

---

### Task 1: Inject a clock into report rendering

Make relative ages a function of `Input.Now` instead of wall-clock, so the report is a pure function of its inputs.

**Files:**
- Modify: `internal/report/report.go` (add `Input.Now`; add `nowOr`; add `now` param to `printServiceIssues` and `printWorkload`; resolve `now` in `printInventoryText` and `printNotes`)
- Modify: `main.go` (set `Now:` in the `report.Input{…}` literal)
- Test: `internal/report/report_test.go` (add `TestPrintInventory_UsesInjectedClock`)

**Interfaces:**
- Consumes: `inventory.HumanSince(rfc3339 string, now time.Time) string` (existing).
- Produces: `report.Input.Now time.Time`; `printServiceIssues(issues []svchealth.Issue, glyph string, now time.Time, w io.Writer) error`; `printWorkload(wl inventory.Workload, now time.Time, w io.Writer) error`; `nowOr(t time.Time) time.Time`.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go` (add `"time"` to its imports if absent):

```go
func TestPrintInventory_UsesInjectedClock(t *testing.T) {
	// A degraded workload that restarted at a fixed instant; Now is 5 days later.
	// The rendered age must be measured from Input.Now, not wall-clock.
	in := Input{
		Now:     time.Date(2020, 1, 6, 0, 0, 0, 0, time.UTC),
		Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 1, NodesTotal: 1},
		Result: inventory.Result{Workloads: []inventory.Workload{{
			Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0,
			Status: "Degraded", Restarts: 8, LastRestart: "2020-01-01T00:00:00Z",
			Findings: []diagnose.Finding{{Pod: "shop/web", Issue: "CrashLoopBackOff", Reason: "keeps crashing", Evidence: "restartCount=8"}},
		}}},
	}
	var buf bytes.Buffer
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "5d ago") {
		t.Errorf("age should be measured from Input.Now (want \"5d ago\"):\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestPrintInventory_UsesInjectedClock`
Expected: FAIL to compile — `unknown field 'Now' in struct literal of type Input`.

- [ ] **Step 3: Add the `Now` field and the `nowOr` helper**

In `internal/report/report.go`, add to the `Input` struct after its last field (`Explanation string`):

```go
	Now time.Time // clock for relative ages; main sets time.Now(); zero → wall-clock
```

Add this helper just above `func printInventoryText`:

```go
// nowOr returns t, or the wall clock when t is the zero value, so callers that
// don't set Input.Now keep rendering ages against time.Now() exactly as before.
func nowOr(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
```

(`report.go` already imports `"time"`.)

- [ ] **Step 4: Add the `now` parameter to the two age-rendering helpers**

In `printServiceIssues` (line ~616) add `now time.Time` before `w`, and pass it to `HumanSince` (line ~620):

```go
func printServiceIssues(issues []svchealth.Issue, glyph string, now time.Time, w io.Writer) error {
```
```go
			line += " · " + inventory.HumanSince(is.Since, now)
```

In `printWorkload` (line ~638) add `now time.Time` before `w`, and pass it to both `HumanSince` calls (lines ~655 and ~700):

```go
func printWorkload(wl inventory.Workload, now time.Time, w io.Writer) error {
```
```go
			header += fmt.Sprintf(", last %s", inventory.HumanSince(wl.LastRestart, now))
```
```go
			restarts += " (" + inventory.HumanSince(p.LastRestart, now) + ")"
```

- [ ] **Step 5: Resolve and pass `now` at the call sites**

In `printInventoryText` (line ~90) add as its first statement, then update its two calls (lines ~104, ~108):

```go
	now := nowOr(in.Now)
```
```go
			if err := printWorkload(wl, now, w); err != nil {
```
```go
		if err := printServiceIssues(real, "  ✗", now, w); err != nil {
```

In `printNotes` (line ~240) add as its first statement, then update its `printServiceIssues` call (line ~267):

```go
	now := nowOr(in.Now)
```
```go
	if err := printServiceIssues(expected, "  •", now, &b); err != nil {
```

- [ ] **Step 6: Set `Now` in `main.go`**

In `main.go`, in the `report.PrintInventory(report.Input{…})` literal (starts ~line 170), add:

```go
		Now:                time.Now(),
```

(`main.go` already imports `"time"`; add it if the build says otherwise.)

- [ ] **Step 7: Run the tests**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && gofmt -l internal/report/report.go main.go && go test ./internal/report/ && go test ./...`
Expected: build OK, gofmt silent, `TestPrintInventory_UsesInjectedClock` passes, all existing report tests and the full suite stay green (they leave `Now` zero → wall-clock, unchanged behaviour).

- [ ] **Step 8: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go main.go
git commit -m "refactor(report): inject a clock (Input.Now) for relative ages"
```

---

### Task 2: Comprehensive fixture + golden snapshot test

Snapshot the whole text report against a committed golden file, regenerable with `-update`.

**Files:**
- Create: `internal/report/golden_test.go`
- Create: `internal/report/testdata/golden-scan.txt` (generated by `-update` in Step 3)

**Interfaces:**
- Consumes: `report.Input` incl. `Input.Now` (Task 1); `PrintInventory`; the sub-report types below; and the existing `report_test.go` builders `sampleSummary()`, `sampleFacts()`, `sampleServiceIssues()`, `sampleCredWarnings()` (same package — call them directly).
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write `golden_test.go`**

Create `internal/report/golden_test.go` with exactly this content:

```go
package report

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
	"github.com/imantaba/kubeagent/internal/secscan"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenNow is the fixed clock for the snapshot; every fixture timestamp precedes it.
var goldenNow = time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

const goldenPath = "testdata/golden-scan.txt"

// goldenInput builds one Input exercising every rendered section, so the golden is a
// broad snapshot of the whole report. All values are fixed literals.
func goldenInput(now time.Time) Input {
	return Input{
		Now: now,
		Cluster: clusterhealth.ClusterHealth{
			Verdict: "Degraded", NodesTotal: 4, NodesReady: 2,
			NodesStaleHeartbeat: 1, NodesExpectedAbsent: 1,
			NodeIssues: []string{
				"node worker-2 NotReady: KubeletNotReady — container runtime is down",
				"node worker-3 SchedulingDisabled",
				"node worker-1 kubelet not heartbeating (lease 95s stale)",
				"node db-01 expected but absent from the cluster",
			},
			SystemIssues: []string{"kube-system/coredns Degraded 1/2"},
		},
		Result:             inventory.Result{Workloads: goldenWorkloads()},
		Resources:          sampleSummary(),
		Platform:           sampleFacts(),
		CredentialWarnings: sampleCredWarnings(),
		ServiceIssues:      sampleServiceIssues(),
		IngressIssues: []ingresshealth.RouteIssue{{
			Namespace: "shop", Ingress: "storefront", Host: "shop.example.com", Path: "/",
			Service: "payments", Port: "80", Problem: "NoEndpoints",
			Detail: "backend Service payments:80 has no ready endpoints (likely 502/503)",
		}},
		SecurityIssues: goldenSecurity(),
		NodeReserve: &nodereserve.Report{
			WarnCount: 2, EphemeralNone: 2, CPUNone: 2, EphemeralReporting: 2,
			Nodes: []nodereserve.NodeReservation{
				{Name: "worker-1", CPUReserved: "0", MemReserved: "0", EphemeralReserved: "0", Warning: true, NoEphemeral: true, NoCPU: true},
				{Name: "worker-2", CPUReserved: "0", MemReserved: "0", EphemeralReserved: "0", Warning: true, NoEphemeral: true, NoCPU: true},
			},
		},
		PVCReclaim: &pvcreclaim.Report{Count: 1, PVCs: []pvcreclaim.PVCReclaim{
			{Namespace: "shop", Name: "cache-data", PV: "pvc-abc123", StorageClass: "standard", Capacity: "128Mi"},
		}},
		KubeletHealth: &nodehealth.Report{Probed: 3, Unhealthy: []nodehealth.Issue{{Node: "worker-2", Detail: "[-]syncloop failed"}}},
	}
}

// goldenWorkloads covers every failure mode the report renders: CrashLoopBackOff,
// ImagePullBackOff, OOMKilled(+CrashLoop), Pending/Unschedulable, RestartLoop, and
// VolumeAttachError. Timestamps precede goldenNow so ages are fixed.
func goldenWorkloads() []inventory.Workload {
	r := "2025-12-25T00:00:00Z" // ~8d before goldenNow
	return []inventory.Workload{
		{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Restarts: 8, LastRestart: r, Image: "busybox:1.36",
			Pods:     []inventory.PodRow{{Name: "web-5b8-2wplt", Phase: "Running", Ready: "0/1", Restarts: 8, LastRestart: r, Node: "worker-1", IP: "10.244.2.2", Age: "20d", Image: "busybox:1.36"}},
			Findings: []diagnose.Finding{{Pod: "shop/web", Issue: "CrashLoopBackOff", Reason: "Container repeatedly crashes after starting", Evidence: `container "web", restartCount=8`}}},
		{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 2, Ready: 0, Status: "Degraded",
			Image:    "nginx:9.9.9-nope",
			Pods:     []inventory.PodRow{{Name: "api-864-dxtdh", Phase: "Pending", Ready: "0/1", Restarts: 0, Node: "worker-1", IP: "10.244.2.4", Age: "6d", Image: "nginx:9.9.9-nope"}},
			Findings: []diagnose.Finding{{Pod: "shop/api", Issue: "ImagePullBackOff", Reason: "Bad image reference or registry authentication", Evidence: `container "api": Back-off pulling image "nginx:9.9.9-nope": not found`}}},
		{Namespace: "shop", Name: "billing-worker", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Restarts: 6, LastRestart: r, Image: "polinux/stress",
			Pods:     []inventory.PodRow{{Name: "billing-7c7-vbgd7", Phase: "Running", Ready: "0/1", Restarts: 6, LastRestart: r, Node: "worker-2", IP: "10.244.1.2", Age: "18d", Image: "polinux/stress"}},
			Findings: []diagnose.Finding{
				{Pod: "shop/billing-worker", Issue: "CrashLoopBackOff", Reason: "Container repeatedly crashes after starting", Evidence: `container "worker", restartCount=6`},
				{Pod: "shop/billing-worker", Issue: "OOMKilled", Reason: "Container exceeded its memory limit and was killed", Evidence: `container "worker", exitCode=137`,
					Resources: &diagnose.ContainerResources{Container: "worker", CPURequest: "", CPULimit: "", MemRequest: "32Mi", MemLimit: "64Mi"}},
			}},
		{Namespace: "shop", Name: "report-cron", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Pending",
			Image:    "busybox:1.36",
			Pods:     []inventory.PodRow{{Name: "report-cron-767-xghsp", Phase: "Pending", Ready: "0/1", Restarts: 0, Node: "", IP: "", Age: "20d", Image: "busybox:1.36"}},
			Findings: []diagnose.Finding{{Pod: "shop/report-cron", Issue: "Unschedulable", Reason: "No node can schedule this pod (resources, taints, or affinity)", Evidence: "0/4 nodes are available: 1 node(s) had untolerated taint, 3 Insufficient cpu"}}},
		{Namespace: "shop", Name: "cache", Kind: "Deployment", Desired: 1, Ready: 1, Status: "Running",
			Restarts: 5, LastRestart: r, Image: "redis:7-alpine",
			Pods:     []inventory.PodRow{{Name: "cache-6d9-abcde", Phase: "Running", Ready: "1/1", Restarts: 5, LastRestart: r, Node: "worker-3", IP: "10.244.3.7", Age: "12d", Image: "redis:7-alpine"}},
			Findings: []diagnose.Finding{{Pod: "shop/cache", Issue: "RestartLoop", Reason: "Container keeps exiting with a non-OOM error and restarting", Evidence: `container "cache", restartCount=5 (still flapping)`}}},
		{Namespace: "shop", Name: "data", Kind: "StatefulSet", Desired: 1, Ready: 0, Status: "Degraded",
			Image:    "postgres:16",
			Pods:     []inventory.PodRow{{Name: "data-0", Phase: "Pending", Ready: "0/1", Restarts: 0, Node: "worker-2", IP: "", Age: "9d", Image: "postgres:16"}},
			Findings: []diagnose.Finding{{Pod: "shop/data-0", Issue: "VolumeAttachError", Reason: "A ReadWriteOnce volume is still attached to another node (Multi-Attach)", Evidence: "Multi-Attach error for volume \"pvc-data-0\": volume is already used by pod(s) on node worker-1"}}},
	}
}

// goldenSecurity renders the full SECURITY section (non-verbose): baseline (Privileged,
// HostPath), an exposed Service, and enough restricted gaps across workloads for the
// "restricted (hardening gaps, near-universal)" aggregate.
func goldenSecurity() []secscan.Finding {
	restricted := func(ns, wl, container, check string) secscan.Finding {
		return secscan.Finding{Namespace: ns, Workload: wl, Kind: "Deployment", Container: container, Profile: "restricted", Check: check, Detail: check + " gap"}
	}
	fs := []secscan.Finding{
		{Namespace: "shop", Workload: "legacy-agent", Kind: "Deployment", Container: "agent", Profile: "baseline", Check: "Privileged", Detail: `container "agent" runs privileged (full host access)`},
		{Namespace: "shop", Workload: "legacy-agent", Kind: "Deployment", Profile: "baseline", Check: "HostPath", Detail: "mounts hostPath /var/run (writable host filesystem)"},
		{Namespace: "shop", Workload: "payments", Kind: "Service", Profile: "kubeagent", Check: "ExposedService", Detail: "type NodePort exposes port(s) 80 externally"},
	}
	for _, wl := range []string{"web", "api", "billing-worker", "cache", "data", "legacy-agent"} {
		fs = append(fs,
			restricted("shop", wl, wl, "RunAsRoot"),
			restricted("shop", wl, wl, "AllowPrivilegeEscalation"),
			restricted("shop", wl, wl, "CapabilitiesNotDropped"),
		)
	}
	return fs
}

func TestGoldenScanOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(goldenInput(goldenNow), "text", &buf); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("scan text output changed:\n%s\n\n"+
			"If this change is intended, run:\n"+
			"  go test ./internal/report -run TestGoldenScanOutput -update\n"+
			"then refresh docs/kubeagent-demo.gif (the update-demo-gif skill) and the "+
			"quickstart example output in website/docs/quickstart.md.",
			firstDiff(string(want), string(got)))
	}
}

// firstDiff returns the first differing line, for a readable failure message.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("first difference at line %d:\n  want: %q\n  got:  %q", i+1, wl, gl)
		}
	}
	return "(files differ only in trailing content)"
}

// TestGoldenInputCoversAllSections guards against the fixture silently losing a section,
// which would leave the golden a partial snapshot.
func TestGoldenInputCoversAllSections(t *testing.T) {
	in := goldenInput(goldenNow)
	if in.Cluster.Verdict == "" || len(in.Result.Workloads) < 6 || in.Resources == nil ||
		in.Platform == nil || len(in.ServiceIssues) == 0 || len(in.CredentialWarnings) == 0 ||
		len(in.IngressIssues) == 0 || len(in.SecurityIssues) == 0 || in.NodeReserve == nil ||
		in.PVCReclaim == nil || in.KubeletHealth == nil {
		t.Fatal("goldenInput must populate every section so the golden stays comprehensive")
	}
}
```

- [ ] **Step 2: Run to verify it fails (no golden yet)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestGolden`
Expected: build OK; `TestGoldenInputCoversAllSections` PASSES; `TestGoldenScanOutput` FAILS with `read golden (run with -update to create it)`.

If instead it fails to **compile** (a struct field name mismatch), fix the fixture against the real struct definitions (`internal/inventory`, `internal/diagnose`, `internal/secscan`, `internal/pvcreclaim`, `internal/nodereserve`, `internal/nodehealth`) and re-run.

- [ ] **Step 3: Generate the golden and read it**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestGoldenScanOutput -update`

Then **read `internal/report/testdata/golden-scan.txt`** and confirm it contains every section:
the `Cluster: Degraded` verdict line with the four node issues; a `NEEDS ATTENTION` block
with CrashLoopBackOff, ImagePullBackOff, OOMKilled, Unschedulable, RestartLoop, and
VolumeAttachError workloads plus the service / credential / ingress lines; a `SECURITY`
section with the baseline findings, the exposed Service, and the restricted aggregate; a
`NOTES` block with the no-memory + no-ephemeral reservation warnings and the PVC-reclaim
line; a `KUBELET HEALTH` section; and a `CONTEXT` block with the reservations table,
resources, and platform. If a section is missing, fix `goldenInput` and re-run `-update`.

- [ ] **Step 4: Run to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestGolden && go test ./... && gofmt -l internal/report/golden_test.go`
Expected: both golden tests pass, full suite green, gofmt silent.

- [ ] **Step 5: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): golden-file snapshot of the full scan text output"
```

---

### Task 3: Document the golden test

Point future contributors at the regenerate-and-refresh-docs workflow.

**Files:**
- Modify: `CLAUDE.md` (the `## Testing style` section)

- [ ] **Step 1: Add a bullet to CLAUDE.md**

In `CLAUDE.md` under `## Testing style`, add:

```markdown
- **Golden output test:** `internal/report/golden_test.go` snapshots the full `scan`
  text output against `testdata/golden-scan.txt`. When a report-format change is
  intentional, regenerate it with
  `go test ./internal/report -run TestGoldenScanOutput -update`, then refresh the README
  demo GIF (the `update-demo-gif` skill) and the quickstart example output.
```

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: note the golden scan-output test + regenerate workflow"
```
