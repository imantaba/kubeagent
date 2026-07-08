# Scan Output Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reorganize `--output text` scan output into severity zones (NEEDS ATTENTION / NOTES / CONTEXT) with a workload-attention header line, collapse all-OK node reservations, and summarize Delete-policy PVCs behind a new `--pvc-reclaim` flag.

**Architecture:** All changes live in `internal/report` (text rendering) plus one CLI flag in `main.go`. First bundle `PrintInventory`'s positional params into a `report.Input` struct with no behavior change, then rewrite the text renderer into zone functions, then add collapse/summary refinements, then wire the flag.

**Tech Stack:** Go 1.26. Tests use `bytes.Buffer` + string assertions (existing style).

## Global Constraints

- `--output json` stays **full and unchanged** — the `inventoryReport` JSON shape and its contents do not change. The redesign and `--pvc-reclaim` affect text mode only.
- The `clusterhealth` **verdict line is unchanged** (`Cluster: <Verdict> — R/T nodes Ready` + node/system issues + scope note).
- **`--fix` / `runFixes` in `main.go` is untouched** — the report never renders remediation info; no read-only fix hint.
- No detector changes, no exit-code change, no new API call, no new RBAC, no new dependency.
- Text glyphs: NEEDS ATTENTION items use `✗`; NOTES items use `•`. Zone labels are exactly `NEEDS ATTENTION`, `NOTES`, `CONTEXT` (uppercase).
- Commits carry **no `Co-Authored-By: Claude` trailer**.
- TDD: failing test first, watch it fail, implement, pass, commit.

---

### Task 1: Bundle `PrintInventory` params into `report.Input` (no behavior change)

**Files:**
- Modify: `internal/report/report.go` (`PrintInventory`, `printInventoryText` signatures + the JSON encode call)
- Modify: `main.go:143` (the one production caller)
- Modify: `internal/report/report_test.go` (all 42 `PrintInventory(...)` callsites → struct literal)

**Interfaces:**
- Produces: `type Input struct { ... }` and `func PrintInventory(in Input, format string, w io.Writer) error`.

This task changes only the call shape. The rendered output stays byte-for-byte identical, so every existing test keeps the same assertions — only the call syntax changes.

- [ ] **Step 1: Define the `Input` struct and new signatures**

In `internal/report/report.go`, add the struct above `PrintInventory` and rewrite both function signatures to consume it. Keep the JSON branch identical by mapping fields:

```go
// Input carries everything the report renders. Bundled into a struct because the
// positional parameter list had grown unwieldy.
type Input struct {
	Cluster            clusterhealth.ClusterHealth
	Result             inventory.Result
	Resources          *resources.Summary
	Platform           *platform.Facts
	ServiceIssues      []svchealth.Issue
	CredentialWarnings []credlint.Finding
	NodeReserve        *nodereserve.Report
	PVCReclaim         *pvcreclaim.Report
	PVCReclaimFull     bool // --pvc-reclaim: expand the PVC list (text only)
	Explanation        string
}

// PrintInventory writes the cluster verdict and the prioritized workload set to w.
func PrintInventory(in Input, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{
			Cluster:            in.Cluster,
			Workloads:          in.Result.Workloads,
			Resources:          in.Resources,
			Platform:           in.Platform,
			ServiceIssues:      in.ServiceIssues,
			CredentialWarnings: in.CredentialWarnings,
			NodeReserve:        in.NodeReserve,
			PVCReclaim:         in.PVCReclaim,
			Explanation:        in.Explanation,
		})
	case "text":
		return printInventoryText(in, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}
```

Change `printInventoryText` to `func printInventoryText(in Input, w io.Writer) error` and, for THIS task only, adapt its body mechanically to read from `in.` (e.g. `in.Cluster`, `in.Result`, `in.Resources`, `in.Platform`, `in.ServiceIssues`, `in.CredentialWarnings`, `in.NodeReserve`, `in.PVCReclaim`, `in.Explanation`). Do not change what it prints.

- [ ] **Step 2: Update the `main.go` caller**

Replace the call at `main.go:143`:

```go
	if err := report.PrintInventory(report.Input{
		Cluster:            health,
		Result:             result,
		Resources:          &summary,
		Platform:           &facts,
		ServiceIssues:      serviceIssues,
		CredentialWarnings: credWarnings,
		NodeReserve:        &res.NodeReserve,
		PVCReclaim:         &res.PVCReclaim,
		Explanation:        explanation,
	}, *output, os.Stdout); err != nil {
		return err
	}
```

- [ ] **Step 3: Convert every test callsite**

`go build ./...` now lists all 42 `PrintInventory(...)` calls in `internal/report/report_test.go`. Convert each to the struct form. The old positional order was
`(cluster, result, summary, facts, serviceIssues, credentialWarnings, nodeReserve, pvcReclaim, explanation, format, w)`,
so map positionally into the struct. Examples:

```go
// before
PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, nil, nil, nil, "", "text", &buf)
// after
PrintInventory(Input{Result: inventory.Result{Workloads: sampleWorkloads()}}, "text", &buf)

// before
PrintInventory(ch, inventory.Result{}, sampleSummary(), nil, nil, nil, nil, nil, "", "json", &buf)
// after
PrintInventory(Input{Cluster: ch, Resources: sampleSummary()}, "json", &buf)

// before (non-nil pvcReclaim rep in slot 8)
PrintInventory(ch, inventory.Result{}, nil, nil, nil, nil, nil, rep, "", "text", &buf)
// after
PrintInventory(Input{Cluster: ch, PVCReclaim: rep}, "text", &buf)
```

Omit zero-value fields (`nil`/`""`/`false`) from the literal. Keep each test's assertions exactly as they are. Re-run `go build ./...` until clean.

- [ ] **Step 4: Run the report suite — output unchanged**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/report/`
Expected: PASS — same assertions, new call shape.

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go main.go
git commit -m "refactor(report): bundle PrintInventory params into report.Input"
```

---

### Task 2: Zoned text renderer (header line + NEEDS ATTENTION / NOTES / CONTEXT)

**Files:**
- Modify: `internal/report/report.go` (`printInventoryText` + new zone helpers; adapt `printWorkload`, `printServiceIssues`, `printCredentialWarnings` glyphs; `footerHint`)
- Modify: `internal/report/report_test.go` (update assertions changed by the new order/glyphs; add zone/attention tests)

**Interfaces:**
- Consumes: `report.Input` from Task 1.
- Produces: text output organized as HEADER → `NEEDS ATTENTION` → `NOTES` → `CONTEXT` → Explanation.

For this task, node reservations render in CONTEXT with the existing per-node format and PVCs render in NOTES with the existing full-row format (collapse/summary come in Task 3). Service issues split by `Expected`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go`:

```go
func TestPrintInventory_HeaderAttentionLine(t *testing.T) {
	var buf bytes.Buffer
	ws := sampleWorkloads()
	ws[0].Findings = []diagnose.Finding{{Issue: "ImagePullBackOff", Reason: "bad ref"}}
	svc := []svchealth.Issue{
		{Namespace: "a", Name: "svc1", Type: "ClusterIP", Detail: "no ready endpoints"}, // real
		{Namespace: "b", Name: "svc2", Type: "ClusterIP", Detail: "scaled to 0", Expected: true},
	}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 3, NodesTotal: 3}, Result: inventory.Result{Workloads: ws}, ServiceIssues: svc}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "Needs attention:") {
		t.Errorf("missing attention line in:\n%s", out)
	}
	if !strings.Contains(out, "1 workload failing") {
		t.Errorf("missing workload count in:\n%s", out)
	}
	if !strings.Contains(out, "1 service without endpoints") {
		t.Errorf("missing real-service count in:\n%s", out)
	}
}

func TestPrintInventory_ZoneOrderAndGlyphs(t *testing.T) {
	var buf bytes.Buffer
	ws := sampleWorkloads()
	ws[0].Findings = []diagnose.Finding{{Issue: "ImagePullBackOff", Reason: "bad ref"}}
	svc := []svchealth.Issue{
		{Namespace: "a", Name: "real", Type: "ClusterIP", Detail: "no ready endpoints"},
		{Namespace: "b", Name: "expected", Type: "ClusterIP", Detail: "scaled to 0", Expected: true},
	}
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 3, NodesTotal: 3},
		Result:        inventory.Result{Workloads: ws},
		Resources:     sampleSummary(),
		Platform:      sampleFacts(),
		ServiceIssues: svc,
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	na := strings.Index(out, "NEEDS ATTENTION")
	notes := strings.Index(out, "NOTES")
	ctx := strings.Index(out, "CONTEXT")
	if !(na >= 0 && notes > na && ctx > notes) {
		t.Fatalf("zones out of order: NEEDS ATTENTION=%d NOTES=%d CONTEXT=%d\n%s", na, notes, ctx, out)
	}
	// real service under NEEDS ATTENTION (before NOTES), expected under NOTES.
	if i := strings.Index(out, "a/real"); !(i > na && i < notes) {
		t.Errorf("real service not in NEEDS ATTENTION zone:\n%s", out)
	}
	if i := strings.Index(out, "b/expected"); !(i > notes && i < ctx) {
		t.Errorf("expected service not in NOTES zone:\n%s", out)
	}
	// resources + platform live in CONTEXT.
	if i := strings.Index(out, "Resources (cluster):"); i < ctx {
		t.Errorf("resources not in CONTEXT zone:\n%s", out)
	}
	if !strings.Contains(out, "✗ ") {
		t.Errorf("expected ✗ glyph for a problem in:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/report/ -run 'HeaderAttentionLine|ZoneOrderAndGlyphs'`
Expected: FAIL (no attention line / zones not present yet).

- [ ] **Step 3: Rewrite `printInventoryText` into zones**

Replace `printInventoryText` with:

```go
func printInventoryText(in Input, w io.Writer) error {
	real, expected := splitServiceIssues(in.ServiceIssues)

	if err := printHeader(in, real, w); err != nil {
		return err
	}

	hasAttention := len(in.Result.Workloads) > 0 || len(real) > 0 || len(in.CredentialWarnings) > 0
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
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	if err := printNotes(in, expected, w); err != nil {
		return err
	}

	if err := printContext(in, w); err != nil {
		return err
	}

	if !hasAttention && in.Cluster.Verdict == "Healthy" {
		if _, err := fmt.Fprintln(w, "No issues found. ✅"); err != nil {
			return err
		}
	}

	if in.Explanation != "" {
		if _, err := fmt.Fprintf(w, "\n── Explanation ──\n%s\n", in.Explanation); err != nil {
			return err
		}
	}
	return nil
}

// splitServiceIssues separates real problems from expected-empty (annotated) ones.
func splitServiceIssues(issues []svchealth.Issue) (real, expected []svchealth.Issue) {
	for _, is := range issues {
		if is.Expected {
			expected = append(expected, is)
		} else {
			real = append(real, is)
		}
	}
	return real, expected
}

// printHeader prints the cluster verdict line and, when anything is flagged, a
// workload-scoped attention line.
func printHeader(in Input, real []svchealth.Issue, w io.Writer) error {
	c := in.Cluster
	if c.Verdict == "" {
		return nil
	}
	if _, err := fmt.Fprintf(w, "Cluster: %s — %d/%d nodes Ready\n", c.Verdict, c.NodesReady, c.NodesTotal); err != nil {
		return err
	}
	for _, iss := range c.NodeIssues {
		if _, err := fmt.Fprintf(w, "  ✗ node %s\n", iss); err != nil {
			return err
		}
	}
	for _, iss := range c.SystemIssues {
		if _, err := fmt.Fprintf(w, "  ✗ system %s\n", iss); err != nil {
			return err
		}
	}
	if c.ScopeNote != "" {
		if _, err := fmt.Fprintf(w, "  · %s\n", c.ScopeNote); err != nil {
			return err
		}
	}
	if line := attentionLine(in, real); line != "" {
		if _, err := fmt.Fprintf(w, "  Needs attention: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}

// attentionLine summarizes flagged workloads and real service issues.
func attentionLine(in Input, real []svchealth.Issue) string {
	failing := 0
	for _, wl := range in.Result.Workloads {
		if wl.Flagged() {
			failing++
		}
	}
	var parts []string
	if failing > 0 {
		parts = append(parts, fmt.Sprintf("%d %s failing", failing, plural(failing, "workload", "workloads")))
	}
	if len(real) > 0 {
		parts = append(parts, fmt.Sprintf("%d %s without endpoints", len(real), plural(len(real), "service", "services")))
	}
	return strings.Join(parts, " · ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// printNotes renders advisory content: expected-empty services, PVC reclaim, and
// the hidden-counts footer.
func printNotes(in Input, expected []svchealth.Issue, w io.Writer) error {
	var b strings.Builder
	if err := printPVCReclaim(in.PVCReclaim, &b); err != nil {
		return err
	}
	if err := printServiceIssues(expected, "  •", &b); err != nil {
		return err
	}
	if hint := footerHint(in.Result); hint != "" {
		fmt.Fprintf(&b, "  • %s\n", hint)
	}
	if b.Len() == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "NOTES"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

// printContext renders reference material: nodes/reservations, resources, platform.
func printContext(in Input, w io.Writer) error {
	var b strings.Builder
	if err := printNodeReservations(in.NodeReserve, &b); err != nil {
		return err
	}
	if err := printResources(in.Resources, &b); err != nil {
		return err
	}
	if in.Platform != nil {
		if line := in.Platform.Line(); line != "" {
			fmt.Fprintf(&b, "Platform: %s\n", line)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "CONTEXT"); err != nil {
		return err
	}
	_, err := io.WriteString(w, b.String())
	return err
}
```

- [ ] **Step 4: Adapt the reused helpers to take a glyph / stay glyph-consistent**

Change `printServiceIssues` to accept a glyph prefix and drop its own header (the zone owns the header now):

```go
func printServiceIssues(issues []svchealth.Issue, glyph string, w io.Writer) error {
	for _, is := range issues {
		line := fmt.Sprintf("%s %s/%s  %s  %s", glyph, is.Namespace, is.Name, is.Type, is.Detail)
		if is.Since != "" {
			line += " · " + inventory.HumanSince(is.Since, time.Now())
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
```

Change `printWorkload`'s flag glyph from `⚠ ` to `✗ `:

```go
	flag := "  "
	if wl.Flagged() {
		flag = "✗ "
	}
```

Change `printCredentialWarnings` to drop its header and use `✗` (it renders inside NEEDS ATTENTION):

```go
func printCredentialWarnings(findings []credlint.Finding, w io.Writer) error {
	for _, f := range findings {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  %s[%s]  %s\n", f.Namespace, f.Name, f.Kind, f.Location, f.Pattern); err != nil {
			return err
		}
	}
	return nil
}
```

Leave `printResources`, `printResLine`, `printNodeReservations`, `printPVCReclaim`, and `footerHint` as they are for now (Task 3 refines the last two). `footerHint` still returns the `+N restarted … · +N CronJobs …` string.

- [ ] **Step 5: Update the existing tests changed by the new layout**

Run `go test ./internal/report/` and update the assertions that the new zones/glyphs changed. Concretely:

- Tests asserting a workload/service/cred line begins with `⚠ ` now expect `✗ ` (workloads, real services, credential warnings). Update those substrings.
- `TestPrintInventory_TextShowsServiceIssues` / `_ServiceSectionFollowsWorkloads`: the header `Service issues:` no longer exists; assert the service line under `NEEDS ATTENTION` (real) instead. Give the real service a non-`Expected` issue.
- `TestPrintInventory_TextNoServiceSectionWhenEmpty`: assert neither `NEEDS ATTENTION` nor a service line appears when there are no issues (keep the intent).
- `_ServiceIssuesSuppressAllClear` / `_CredentialWarningsSuppressAllClear`: still valid — a real service issue or a cred warning makes `hasAttention` true, so `No issues found. ✅` must be absent. Keep the assertion.
- All-clear tests (`_AllClearWhenHealthyAndEmpty`, `_NoAllClearWhenDegraded`): the condition is now `!hasAttention && Verdict=="Healthy"`. Verify: empty input + Healthy → all-clear present; a flagged workload or Degraded verdict → absent.
- `_FooterHintListsHidden` / `_FooterShownAndNoAllClearWhenDegraded` / `_NoFooterWhenNothingHidden`: the footer now renders as a `  • +N …` line inside `NOTES`. Update the expected substring to include the hidden-count text (the counts are unchanged); for "no footer" assert the hidden text is absent.
- Resource/platform/node-reservation tests: assert their content still appears (now under `CONTEXT`); ordering-specific assertions (e.g. `_ResourceBlockPrecedesWorkloads`) invert — resources now come AFTER workloads, so update or remove that ordering assertion to reflect CONTEXT-after-NEEDS-ATTENTION.

Do not weaken assertions to pass — update them to the new, correct expected output.

- [ ] **Step 6: Run the suite**

Run: `go build ./... && go test ./internal/report/`
Expected: PASS (updated existing tests + two new zone tests).

- [ ] **Step 7: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): zone the text output into NEEDS ATTENTION / NOTES / CONTEXT"
```

---

### Task 3: Collapse node reservations + summarize PVC reclaim

**Files:**
- Modify: `internal/report/report.go` (`printNodeReservations`, `printPVCReclaim`, and pass `PVCReclaimFull` through)
- Modify: `internal/report/report_test.go` (collapse/summary tests)

**Interfaces:**
- Consumes: `Input.NodeReserve`, `Input.PVCReclaim`, `Input.PVCReclaimFull`.

The renderer calls in Task 2 are `printNodeReservations(in.NodeReserve, &b)` (CONTEXT) and `printPVCReclaim(in.PVCReclaim, &b)` (NOTES). Update the PVC call to pass the flag: `printPVCReclaim(in.PVCReclaim, in.PVCReclaimFull, &b)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestPrintInventory_NodeReservationsCollapseWhenAllOK(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{WarnCount: 0, Nodes: []nodereserve.NodeReservation{
		{Name: "n1", CPUReserved: "300m", MemReserved: "1Gi"},
		{Name: "n2", CPUReserved: "300m", MemReserved: "1Gi"},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 2, NodesTotal: 2}, NodeReserve: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "n1") || strings.Contains(out, "n2") {
		t.Errorf("all-OK reservations must collapse (no per-node lines):\n%s", out)
	}
	if !strings.Contains(out, "reservations OK") {
		t.Errorf("missing collapsed reservations line:\n%s", out)
	}
}

func TestPrintInventory_NodeReservationsWarningIsNote(t *testing.T) {
	var buf bytes.Buffer
	rep := &nodereserve.Report{WarnCount: 1, Nodes: []nodereserve.NodeReservation{
		{Name: "bad", CPUReserved: "0", MemReserved: "0", Warning: true},
		{Name: "ok", CPUReserved: "300m", MemReserved: "1Gi"},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 2, NodesTotal: 2}, NodeReserve: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	notes := strings.Index(out, "NOTES")
	if notes < 0 || !strings.Contains(out, "reserve no memory") || !strings.Contains(out, "bad") {
		t.Errorf("expected a NOTES warning naming the bad node:\n%s", out)
	}
}

func TestPrintInventory_PVCReclaimSummaryByDefault(t *testing.T) {
	var buf bytes.Buffer
	rep := &pvcreclaim.Report{Count: 3, PVCs: []pvcreclaim.PVCReclaim{
		{Namespace: "a", Name: "p1", PV: "pv1", StorageClass: "fast", Capacity: "1Gi"},
		{Namespace: "a", Name: "p2", PV: "pv2", StorageClass: "fast", Capacity: "1Gi"},
		{Namespace: "b", Name: "p3", PV: "pv3", StorageClass: "slow", Capacity: "1Gi"},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, PVCReclaim: rep}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "3 PVCs on Delete reclaim") || !strings.Contains(out, "fast ×2") || !strings.Contains(out, "slow ×1") {
		t.Errorf("missing grouped PVC summary:\n%s", out)
	}
	if !strings.Contains(out, "[--pvc-reclaim]") {
		t.Errorf("missing --pvc-reclaim hint:\n%s", out)
	}
	if strings.Contains(out, "pv1") {
		t.Errorf("summary must not list individual PV rows:\n%s", out)
	}
}

func TestPrintInventory_PVCReclaimFullWhenFlagged(t *testing.T) {
	var buf bytes.Buffer
	rep := &pvcreclaim.Report{Count: 1, PVCs: []pvcreclaim.PVCReclaim{
		{Namespace: "a", Name: "p1", PV: "pv1", StorageClass: "fast", Capacity: "1Gi"},
	}}
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, PVCReclaim: rep, PVCReclaimFull: true}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "a/p1") || !strings.Contains(out, "pv pv1") {
		t.Errorf("full list expected under --pvc-reclaim:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/report/ -run 'NodeReservationsCollapse|NodeReservationsWarning|PVCReclaimSummary|PVCReclaimFull'`
Expected: FAIL.

- [ ] **Step 3: Rewrite `printNodeReservations` (collapse) and `printPVCReclaim` (summary)**

Replace `printNodeReservations` with a collapsing version. When there are no warnings it prints a single line; when there are warnings it prints a `•` NOTE-style line and the collapsed CONTEXT line drops the OK suffix. Because CONTEXT and NOTES are rendered separately, split the responsibilities: the warning NOTE is emitted from `printNotes`, and the CONTEXT one-liner from `printContext`. Implement two small helpers and wire them:

In `printNotes`, before the PVC block, add:

```go
	if n := in.NodeReserve; n != nil && n.WarnCount > 0 {
		var names []string
		for _, r := range n.Nodes {
			if r.Warning {
				names = append(names, r.Name)
			}
		}
		fmt.Fprintf(&b, "  • %d %s reserve no memory: %s\n", n.WarnCount, plural(n.WarnCount, "node", "nodes"), strings.Join(names, ", "))
	}
```

In `printContext`, replace the `printNodeReservations(in.NodeReserve, &b)` call with a collapsed line:

```go
	if n := in.NodeReserve; n != nil && len(n.Nodes) > 0 {
		total := len(n.Nodes)
		ok := total - n.WarnCount
		line := fmt.Sprintf("Nodes  %d/%d reserve memory OK", ok, total)
		if n.WarnCount == 0 {
			line = fmt.Sprintf("Nodes  %d nodes · kubelet reservations OK", total)
		}
		fmt.Fprintln(&b, line)
	}
```

Delete the now-unused `printNodeReservations` function (it is replaced by the two inline blocks above). If any test referenced it directly, update that test to call `PrintInventory`.

Replace `printPVCReclaim` with a flag-aware version:

```go
// printPVCReclaim renders the Delete-reclaim PVCs: a grouped one-line summary by
// default, or the full per-PVC rows when full is true. Nothing prints when empty.
func printPVCReclaim(rep *pvcreclaim.Report, full bool, w io.Writer) error {
	if rep == nil || len(rep.PVCs) == 0 {
		return nil
	}
	if full {
		for _, p := range rep.PVCs {
			line := fmt.Sprintf("  • %s/%s  pv %s", p.Namespace, p.Name, p.PV)
			if p.StorageClass != "" {
				line += "  class " + p.StorageClass
			}
			if p.Capacity != "" {
				line += "  " + p.Capacity
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := fmt.Fprintf(w, "  • %d %s on Delete reclaim policy — %s   [--pvc-reclaim]\n",
		len(rep.PVCs), plural(len(rep.PVCs), "PVC", "PVCs"), groupByClass(rep.PVCs))
	return err
}

// groupByClass builds "classA ×N, classB ×M" ordered by count desc, then name.
func groupByClass(pvcs []pvcreclaim.PVCReclaim) string {
	counts := map[string]int{}
	var order []string
	for _, p := range pvcs {
		c := p.StorageClass
		if c == "" {
			c = "(no class)"
		}
		if _, seen := counts[c]; !seen {
			order = append(order, c)
		}
		counts[c]++
	}
	sort.SliceStable(order, func(i, j int) bool {
		if counts[order[i]] != counts[order[j]] {
			return counts[order[i]] > counts[order[j]]
		}
		return order[i] < order[j]
	})
	parts := make([]string, 0, len(order))
	for _, c := range order {
		parts = append(parts, fmt.Sprintf("%s ×%d", c, counts[c]))
	}
	return strings.Join(parts, ", ")
}
```

Update the `printNotes` PVC call to pass the flag: `printPVCReclaim(in.PVCReclaim, in.PVCReclaimFull, &b)`. Add `"sort"` to the imports.

- [ ] **Step 4: Run the tests**

Run: `go build ./... && go test ./internal/report/`
Expected: PASS. Update `TestPrintInventory_TextShowsNodeReservations` and the earlier PVC tests (`_TextShowsPVCReclaim`, `_TextNoPVCReclaimSectionWhenEmpty`) to the new shapes: the node-reservation test should now expect the collapsed/NOTE output; the PVC "shows" test should use `PVCReclaimFull: true` to assert full rows (or assert the summary). Keep `_JSONIncludesPVCReclaim` and `_JSONIncludesNodeReserve` unchanged — JSON is untouched.

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): collapse all-OK reservations and summarize PVC reclaim"
```

---

### Task 4: `--pvc-reclaim` CLI flag

**Files:**
- Modify: `main.go` (the `scan` flag set + `report.Input` construction)

**Interfaces:**
- Consumes: `report.Input.PVCReclaimFull` from Task 1.

- [ ] **Step 1: Add the flag**

In the `scan` subcommand's flag set in `main.go` (near the other `fs.Bool` flags like `include-restarts`), add:

```go
	pvcReclaimFull := fs.Bool("pvc-reclaim", false, "list every PVC on a Delete reclaim policy (default: a grouped summary)")
```

- [ ] **Step 2: Wire it into the report input**

In the `report.PrintInventory(report.Input{...}, ...)` call, add the field:

```go
		PVCReclaimFull:     *pvcReclaimFull,
```

- [ ] **Step 3: Verify build + manual check**

Run:

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o kubeagent . && ./kubeagent scan -h 2>&1 | grep -A1 pvc-reclaim
```

Expected: the `-pvc-reclaim` flag and its help text appear. Then `go test ./...` stays green.

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "feat(scan): add --pvc-reclaim to expand the Delete-policy PVC list"
```

---

### Task 5: Docs

**Files:**
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Changed`)
- Modify: `website/docs/features/diagnostics.md`
- Modify: `README.md` (only if it shows sample scan output)

**Interfaces:** none.

- [ ] **Step 1: CHANGELOG**

Add a new `## [Unreleased]` section above the current top `## [0.13.0] - 2026-07-08`, with a `### Changed` entry:

```markdown
## [Unreleased]

### Changed

- **Redesigned `scan` text output.** The human-readable output is now organized
  by severity into **NEEDS ATTENTION** (failing workloads, dead Services,
  credential warnings), **NOTES** (advisories — Delete-policy PVCs, expected-empty
  Services, hidden-workload counts), and **CONTEXT** (nodes/reservations,
  resources, platform), with a workload-scoped "Needs attention" line under the
  cluster verdict. All-OK node reservations collapse to one line, and
  Delete-policy PVCs show as a grouped summary — pass `--pvc-reclaim` for the full
  per-PVC list. `--output json` is unchanged, and `--fix` behavior is unchanged.
```

- [ ] **Step 2: diagnostics.md**

Run: `grep -nE '^#|^##|^###' website/docs/features/diagnostics.md | head -40`

Add a subsection describing the zoned output and the `--pvc-reclaim` flag, matching the surrounding heading level and style:

```markdown
### Output layout

`scan --output text` groups findings by how urgently they need action:

- **NEEDS ATTENTION** — failing workloads, Services with no ready endpoints, and
  credential warnings.
- **NOTES** — advisories that rarely need immediate action: PersistentVolumeClaims
  on a `Delete` reclaim policy (a grouped summary; pass `--pvc-reclaim` for the
  full list), Services that are intentionally empty (scaled to zero or a CronJob
  between runs), and counts of workloads hidden behind `--include-restarts` /
  `--include-cron`.
- **CONTEXT** — reference data: node readiness and kubelet reservations (collapsed
  to one line when all nodes are fine), the cluster resource summary, and platform
  facts.

A "Needs attention" line under the cluster verdict summarizes how many workloads
are failing and how many Services have no endpoints. `--output json` is
unaffected and always contains the full detail.
```

- [ ] **Step 3: README (only if it shows sample output)**

Run: `grep -nE 'Cluster:|No issues found|Service issues|scan' README.md | head`

If the README contains a sample scan block reflecting the old layout, update it to the zoned layout (or leave a one-line note that `scan` groups output into NEEDS ATTENTION / NOTES / CONTEXT). If there is no sample block, make no change and note that in the report.

- [ ] **Step 4: Verify the website builds**

Run:

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: "Documentation built" with no strict WARNING lines about the edited pages.

- [ ] **Step 5: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md README.md
git commit -m "docs: document the zoned scan output and --pvc-reclaim"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Then a manual smoke:

```bash
go build -o kubeagent . && ./kubeagent scan --output text        # zoned output, PVC summary
./kubeagent scan --output text --pvc-reclaim | sed -n '/NOTES/,/CONTEXT/p'   # full PVC rows
./kubeagent scan --output json | head -c 200                     # JSON unchanged
```

Expected: text output shows the three zones with the attention line and a collapsed PVC summary; `--pvc-reclaim` expands the rows; JSON is the same shape as before.
