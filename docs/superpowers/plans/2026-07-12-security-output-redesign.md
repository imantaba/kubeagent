# Security Output Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the `scan --security` text section signal-first — a one-line tier summary, the `baseline`/`kubeagent` findings in full (worst-first), the near-universal `restricted` gaps folded into a compact aggregate — with `--security-verbose` restoring the full per-finding listing.

**Architecture:** A text-only rendering change. `report.printSecurityIssues` is rewritten to take a `verbose bool` and compute tiers/aggregate from the existing `[]secscan.Finding`. `report.Input` gains `SecurityVerbose bool`; `main.go` adds the `--security-verbose` flag and passes it. `internal/secscan`, `internal/scan`, and the JSON output are untouched (findings are identical).

**Tech Stack:** Go 1.26, standard library only. Tests use `bytes.Buffer` via the existing `report_test.go` harness.

## Global Constraints

- **Text-only.** JSON `securityIssues` is unchanged — always all findings, regardless of `--security-verbose`. Only `printSecurityIssues` and its callers change.
- Advisory unchanged: security output never affects the cluster verdict, `kubeagent_cluster_healthy`, the exit code, or the "Needs attention" line. The all-clear stays suppressed whenever there are any security findings (existing `!hasSecurity` guard — do not change it).
- Tiers: `baseline` + `kubeagent` = "act-on-these" (shown in full); `restricted` = aggregated by default, shown in full only with `--security-verbose`.
- Exact names/wording: flag `--security-verbose`; header line `SECURITY  (advisory — does not affect the cluster verdict)`; summary tiers `N baseline` / `N exposed service(s)` / `N restricted hardening gaps` joined by ` · ` then `M workloads`; workload line `  ✗ <ns>/<workload>  <kind>`; finding line `      [<profile>] <check> — <detail>`; aggregate `  restricted (hardening gaps, near-universal): N across M workloads`, per-check `<Check> ×N` joined by ` · ` in the order RunAsRoot, AllowPrivilegeEscalation, CapabilitiesNotDropped, and the hint `    → run with --security-verbose to list every finding per workload`.
- No separate trailing `Security: …` line (the current one is removed).
- Commits carry no `Co-Authored-By: Claude` trailer. TDD. `export PATH=$PATH:/usr/local/go/bin`.

---

### Task 1: Rewrite the SECURITY renderer + `--security-verbose` flag

**Files:**
- Modify: `internal/report/report.go` (`Input` struct: add `SecurityVerbose bool`; `printInventoryText` call site ~line 121; rewrite `printSecurityIssues` ~line 378)
- Modify: `main.go` (declare `--security-verbose`; add to usage string ~line 60; pass `SecurityVerbose` into `report.Input` ~line 169)
- Test: `internal/report/report_test.go` (replace `TestPrintInventory_TextShowsSecurity` ~line 1015; add three tests)

**Interfaces:**
- Consumes: `secscan.Finding{Namespace, Workload, Kind, Container, Profile, Check, Detail}` (unchanged); `plural(n int, one, many string) string` (existing helper).
- Produces: `report.Input.SecurityVerbose bool`; `printSecurityIssues(issues []secscan.Finding, verbose bool, w io.Writer) error`.

- [ ] **Step 1: Update the failing tests**

In `internal/report/report_test.go`, **replace** the existing `TestPrintInventory_TextShowsSecurity` function (currently ~line 1015) with the following, and **add** the three tests after it. Leave `TestPrintInventory_JSONIncludesSecurity` and `TestPrintInventory_NoSecurityWhenEmpty` unchanged (JSON and empty behavior are unchanged).

```go
func TestPrintInventory_SecurityDefaultView(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityIssues: []secscan.Finding{
			{Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Profile: "baseline", Check: "Privileged", Detail: `container "app" runs privileged (full host access)`},
			{Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Profile: "restricted", Check: "RunAsRoot", Detail: `container "app" is not guaranteed to run as non-root`},
			{Namespace: "shop", Workload: "web", Kind: "Deployment", Container: "web", Profile: "restricted", Check: "AllowPrivilegeEscalation", Detail: `container "web" allows privilege escalation (allowPrivilegeEscalation not false)`},
			{Namespace: "shop", Workload: "web", Kind: "Deployment", Container: "web", Profile: "restricted", Check: "CapabilitiesNotDropped", Detail: `container "web" does not drop all capabilities`},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "SECURITY") {
		t.Errorf("expected a SECURITY section:\n%s", out)
	}
	if !strings.Contains(out, "1 baseline · 3 restricted hardening gaps · 2 workloads") {
		t.Errorf("missing tier summary header:\n%s", out)
	}
	if !strings.Contains(out, "✗ shop/api  Deployment") || !strings.Contains(out, "[baseline] Privileged") {
		t.Errorf("missing the act-on-these detail block:\n%s", out)
	}
	if strings.Contains(out, "[restricted] RunAsRoot") {
		t.Errorf("restricted findings must be folded into the aggregate, not listed:\n%s", out)
	}
	if strings.Contains(out, "✗ shop/web") {
		t.Errorf("a restricted-only workload must not get a detail block:\n%s", out)
	}
	if !strings.Contains(out, "restricted (hardening gaps, near-universal): 3 across 2 workloads") {
		t.Errorf("missing restricted aggregate:\n%s", out)
	}
	if !strings.Contains(out, "RunAsRoot ×1 · AllowPrivilegeEscalation ×1 · CapabilitiesNotDropped ×1") {
		t.Errorf("missing per-check counts:\n%s", out)
	}
	if !strings.Contains(out, "--security-verbose") {
		t.Errorf("missing the --security-verbose hint:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must be suppressed when there are findings:\n%s", out)
	}
}

func TestPrintInventory_SecurityVerbose(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:         clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityVerbose: true,
		SecurityIssues: []secscan.Finding{
			{Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Profile: "baseline", Check: "Privileged", Detail: "p"},
			{Namespace: "shop", Workload: "api", Kind: "Deployment", Container: "app", Profile: "restricted", Check: "RunAsRoot", Detail: "r"},
			{Namespace: "shop", Workload: "web", Kind: "Deployment", Container: "web", Profile: "restricted", Check: "CapabilitiesNotDropped", Detail: "c"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "[restricted] RunAsRoot") {
		t.Errorf("verbose must list restricted findings:\n%s", out)
	}
	if !strings.Contains(out, "✗ shop/web  Deployment") {
		t.Errorf("verbose must show restricted-only workloads:\n%s", out)
	}
	if strings.Contains(out, "restricted (hardening gaps, near-universal)") {
		t.Errorf("verbose must omit the aggregate block:\n%s", out)
	}
}

func TestPrintInventory_SecurityOnlyRestricted(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityIssues: []secscan.Finding{
			{Namespace: "shop", Workload: "web", Kind: "Deployment", Container: "web", Profile: "restricted", Check: "RunAsRoot", Detail: "r"},
			{Namespace: "shop", Workload: "web", Kind: "Deployment", Container: "web", Profile: "restricted", Check: "CapabilitiesNotDropped", Detail: "c"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "✗ ") {
		t.Errorf("restricted-only findings must produce no detail blocks:\n%s", out)
	}
	if !strings.Contains(out, "restricted (hardening gaps, near-universal): 2 across 1 workload") {
		t.Errorf("missing restricted aggregate for restricted-only input:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must stay suppressed:\n%s", out)
	}
}

func TestPrintInventory_SecurityWorstFirst(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityIssues: []secscan.Finding{
			{Namespace: "ns", Workload: "bbb", Kind: "Deployment", Profile: "baseline", Check: "HostPath", Detail: "mounts hostPath /a"},
			{Namespace: "ns", Workload: "aaa", Kind: "Deployment", Profile: "baseline", Check: "HostPath", Detail: "mounts hostPath /b"},
			{Namespace: "ns", Workload: "aaa", Kind: "Deployment", Profile: "baseline", Check: "HostPath", Detail: "mounts hostPath /c"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Index(out, "ns/aaa") > strings.Index(out, "ns/bbb") {
		t.Errorf("workload with more findings (aaa: 2) must sort before bbb (1):\n%s", out)
	}
	if strings.Contains(out, "restricted (hardening") {
		t.Errorf("no restricted aggregate expected when there are no restricted findings:\n%s", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run 'Security'`
Expected: FAIL — the new default view/aggregate/verbose behavior and the `SecurityVerbose` field don't exist yet (compile error on `SecurityVerbose`, then assertion failures).

- [ ] **Step 3: Add the `Input.SecurityVerbose` field**

In `internal/report/report.go`, in the `Input` struct, add the field immediately after `SecurityIssues []secscan.Finding`:

```go
	SecurityVerbose    bool
```

- [ ] **Step 4: Update the call site**

In `printInventoryText`, change the security render call (currently `printSecurityIssues(in.SecurityIssues, w)`) to pass the verbose flag:

```go
	if err := printSecurityIssues(in.SecurityIssues, in.SecurityVerbose, w); err != nil {
		return err
	}
```

(`hasSecurity := len(in.SecurityIssues) > 0` and the `!hasSecurity` all-clear guard stay exactly as they are.)

- [ ] **Step 5: Rewrite `printSecurityIssues`**

Replace the entire existing `printSecurityIssues` function with:

```go
// printSecurityIssues renders the advisory SECURITY section. By default it is
// signal-first: a one-line tier summary, the baseline/kubeagent ("act-on-these")
// findings in full per workload (worst-first), and the near-universal restricted
// hardening gaps folded into a compact aggregate. verbose lists every finding
// per workload and omits the aggregate.
func printSecurityIssues(issues []secscan.Finding, verbose bool, w io.Writer) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "SECURITY  (advisory — does not affect the cluster verdict)"); err != nil {
		return err
	}

	// Tallies for the summary header and the restricted aggregate.
	var nBaseline, nExposed, nRestricted int
	allWorkloads := map[string]bool{}
	restrictedWorkloads := map[string]bool{}
	restrictedByCheck := map[string]int{}
	for _, f := range issues {
		wl := f.Namespace + "/" + f.Workload
		allWorkloads[wl] = true
		switch f.Profile {
		case "restricted":
			nRestricted++
			restrictedWorkloads[wl] = true
			restrictedByCheck[f.Check]++
		case "kubeagent":
			nExposed++
		default: // baseline
			nBaseline++
		}
	}

	// Summary header: non-zero tiers joined by " · ", then the workload count.
	var parts []string
	if nBaseline > 0 {
		parts = append(parts, fmt.Sprintf("%d baseline", nBaseline))
	}
	if nExposed > 0 {
		parts = append(parts, fmt.Sprintf("%d exposed %s", nExposed, plural(nExposed, "service", "services")))
	}
	if nRestricted > 0 {
		parts = append(parts, fmt.Sprintf("%d restricted hardening %s", nRestricted, plural(nRestricted, "gap", "gaps")))
	}
	parts = append(parts, fmt.Sprintf("%d %s", len(allWorkloads), plural(len(allWorkloads), "workload", "workloads")))
	if _, err := fmt.Fprintf(w, "  %s\n\n", strings.Join(parts, " · ")); err != nil {
		return err
	}

	// Group findings by workload, preserving Assess's per-workload finding order.
	type grp struct{ ns, name, kind string }
	var order []grp
	byGrp := map[grp][]secscan.Finding{}
	for _, f := range issues {
		g := grp{f.Namespace, f.Workload, f.Kind}
		if _, ok := byGrp[g]; !ok {
			order = append(order, g)
		}
		byGrp[g] = append(byGrp[g], f)
	}

	// Detail blocks. Default: only workloads with act-on-these (non-restricted)
	// findings, showing just those. Verbose: every workload, every finding.
	type block struct {
		g     grp
		shown []secscan.Finding
	}
	var blocks []block
	for _, g := range order {
		shown := byGrp[g]
		if !verbose {
			var act []secscan.Finding
			for _, f := range shown {
				if f.Profile != "restricted" {
					act = append(act, f)
				}
			}
			if len(act) == 0 {
				continue // restricted-only workload -> aggregate only
			}
			shown = act
		}
		blocks = append(blocks, block{g, shown})
	}
	// Worst-first: most shown findings, then namespace, then workload.
	sort.SliceStable(blocks, func(i, j int) bool {
		a, b := blocks[i], blocks[j]
		if len(a.shown) != len(b.shown) {
			return len(a.shown) > len(b.shown)
		}
		if a.g.ns != b.g.ns {
			return a.g.ns < b.g.ns
		}
		return a.g.name < b.g.name
	})
	for _, b := range blocks {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  %s\n", b.g.ns, b.g.name, b.g.kind); err != nil {
			return err
		}
		for _, f := range b.shown {
			if _, err := fmt.Fprintf(w, "      [%s] %s — %s\n", f.Profile, f.Check, f.Detail); err != nil {
				return err
			}
		}
	}

	// Restricted aggregate (default only, when there are restricted findings).
	if !verbose && nRestricted > 0 {
		if _, err := fmt.Fprintf(w, "\n  restricted (hardening gaps, near-universal): %d across %d %s\n",
			nRestricted, len(restrictedWorkloads), plural(len(restrictedWorkloads), "workload", "workloads")); err != nil {
			return err
		}
		var checks []string
		for _, c := range []string{"RunAsRoot", "AllowPrivilegeEscalation", "CapabilitiesNotDropped"} {
			if restrictedByCheck[c] > 0 {
				checks = append(checks, fmt.Sprintf("%s ×%d", c, restrictedByCheck[c]))
			}
		}
		if _, err := fmt.Fprintf(w, "    %s\n", strings.Join(checks, " · ")); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "    → run with --security-verbose to list every finding per workload"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 6: Wire the `--security-verbose` flag in `main.go`**

In `main.go`, declare the flag immediately after the `security` flag (~line 75):

```go
	securityVerbose := fs.Bool("security-verbose", false, "with --security: list every finding per workload (default: dangerous findings in full, restricted gaps aggregated)")
```

Add `[--security-verbose]` to the scan usage string (~line 60), immediately after `[--security]`, so it reads `... [--security] [--security-verbose] [--disk-usage ...`.

Add to the `report.Input{...}` literal (~line 169), immediately after `SecurityIssues: res.SecurityIssues,`:

```go
		SecurityVerbose:    *securityVerbose,
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go build ./... && go test ./internal/report/`
Expected: PASS (the four Security tests above, plus the unchanged JSON/empty tests).

- [ ] **Step 8: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go main.go
git commit -m "feat(report): signal-first SECURITY section + --security-verbose"
```

---

### Task 2: Docs

**Files:**
- Modify: `CHANGELOG.md` (the existing `## [Unreleased]` security bullet)
- Modify: `website/docs/features/diagnostics.md` (the Security posture subsection)

**Interfaces:** none. Use exact names: flag `--security-verbose`; JSON field `securityIssues` (unchanged); the redesign is text-only.

- [ ] **Step 1: Update the CHANGELOG bullet**

The workload-security-posture entry is still under `## [Unreleased]` (unreleased), so fold the redesign into that same bullet rather than adding a second one. Run `grep -n 'Workload security posture' CHANGELOG.md` to find it, and revise its rendering sentence so it reads that the `SECURITY` section is **signal-first** — a tier summary, the dangerous `baseline`/exposed findings in full, the near-universal `restricted` gaps aggregated, with `--security-verbose` to list every finding. Keep it read-only/advisory and keep the JSON `securityIssues` mention.

- [ ] **Step 2: Update diagnostics.md**

Run: `grep -n 'Security posture' website/docs/features/diagnostics.md`

In the Security posture subsection, add a sentence describing the output shape: the `SECURITY` section leads with a tier summary, shows the dangerous `baseline`/exposed-service findings in full per workload, and folds the near-universal `restricted` hardening gaps into a per-check aggregate; `--security-verbose` lists every finding per workload. Note JSON `securityIssues` always contains all findings. Match the surrounding prose style.

- [ ] **Step 3: Verify the docs build**

Run:

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: exit 0, "Documentation built", no page `WARNING` lines (the Material for MkDocs team banner is cosmetic — ignore it). If the venv path is missing, recreate it: `python3 -m venv <path> && <path>/bin/pip install -r requirements.txt`.

- [ ] **Step 4: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md
git commit -m "docs: signal-first security output + --security-verbose"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Manual smoke against a cluster with security findings (system namespaces excluded by default):

```bash
go build -o kubeagent . && ./kubeagent scan --security | sed -n '/^SECURITY/,/^$/p'
./kubeagent scan --security --security-verbose | grep -c '\[restricted\]'   # > 0 in verbose
./kubeagent scan --security --output json | grep -o '"securityIssues"'      # JSON unchanged
```

Expected: default view shows the tier summary + act-on-these blocks + restricted aggregate; verbose lists restricted findings; JSON still carries `securityIssues`.
