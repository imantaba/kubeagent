# Root-Cause for Pending & Node NotReady Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Node NotReady issues name their cause (the `NodeReady` condition Reason + trimmed Message), and the text scan shows each finding's `Evidence` (e.g. the scheduler's message for a pending pod).

**Architecture:** Two small, independent enrichments — `clusterhealth.nodeHealth` folds the NodeReady Reason/Message into the NotReady issue string; `report.printWorkload` prints the already-present `Finding.Evidence` under each finding. No new collection, detector, RBAC, or dependency.

**Tech Stack:** Go 1.26. Tests use fake `corev1.Node` values and `diagnose.Finding` fixtures.

## Global Constraints

- Read-only; no new API call, no new RBAC, no new dependency, no new detector, no `--fix`.
- Advisory: the cluster **verdict** logic is unchanged (a NotReady node still makes the cluster `Degraded`); only the issue/finding strings get richer.
- JSON schema and `--explain` behavior unchanged (`NodeIssues` is still `[]string`; `Finding.Evidence` was already serialized/explained).
- NotReady issue string: `NotReady: <Reason> — <Message>`; Message trimmed to its first line and ≤120 runes (append `…` when truncated); fall back to plain `NotReady` when Reason and Message are both empty.
- Evidence text line: shown only when `f.Evidence != "" && f.Evidence != f.Reason`.
- Commits carry **no `Co-Authored-By: Claude` trailer**.
- TDD: failing test first, watch it fail, implement, pass, commit.

---

### Task 1: Node NotReady names its cause

**Files:**
- Modify: `internal/clusterhealth/clusterhealth.go` (`nodeHealth`; add `notReadyIssue` + `trimLine`; import `strings`)
- Test: `internal/clusterhealth/clusterhealth_test.go` (add a `notReadyNode` helper + tests)

**Interfaces:**
- Produces (unexported, same package): `notReadyIssue(reason, message string) string`, `trimLine(s string, max int) string`. `nodeHealth` and `Assess` signatures are unchanged.

- [ ] **Step 1: Write the failing tests**

Add to `internal/clusterhealth/clusterhealth_test.go` (the file already imports `corev1` and `metav1`):

```go
// notReadyNode builds a node whose NodeReady condition is False with the given
// reason and message.
func notReadyNode(name, reason, message string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: reason, Message: message},
		}},
	}
}

func TestNodeHealth_NotReadyIncludesReasonAndMessage(t *testing.T) {
	_, issues := nodeHealth(notReadyNode("n1", "KubeletNotReady", "container runtime network not ready: cni config uninitialized"))
	if len(issues) != 1 {
		t.Fatalf("want one issue, got %v", issues)
	}
	want := "NotReady: KubeletNotReady — container runtime network not ready: cni config uninitialized"
	if issues[0] != want {
		t.Errorf("want %q, got %q", want, issues[0])
	}
}

func TestNodeHealth_NotReadyTrimsLongMessage(t *testing.T) {
	long := "KubeletNotReady"
	msg := ""
	for i := 0; i < 200; i++ {
		msg += "x"
	}
	_, issues := nodeHealth(notReadyNode("n1", long, msg))
	if len(issues) != 1 {
		t.Fatalf("want one issue, got %v", issues)
	}
	if []rune(issues[0])[len([]rune(issues[0]))-1] != '…' {
		t.Errorf("expected a trailing ellipsis on a truncated message: %q", issues[0])
	}
	// "NotReady: KubeletNotReady — " prefix + 120 runes + "…"
	if n := len([]rune(issues[0])); n > 160 {
		t.Errorf("issue string too long (%d runes): %q", n, issues[0])
	}
}

func TestNodeHealth_NotReadyFallsBackWhenEmpty(t *testing.T) {
	_, issues := nodeHealth(notReadyNode("n1", "", ""))
	if len(issues) != 1 || issues[0] != "NotReady" {
		t.Errorf("want plain NotReady, got %v", issues)
	}
}

func TestNodeHealth_FirstLineOfMessageOnly(t *testing.T) {
	_, issues := nodeHealth(notReadyNode("n1", "KubeletNotReady", "first line\nsecond line"))
	if len(issues) != 1 || issues[0] != "NotReady: KubeletNotReady — first line" {
		t.Errorf("want only the first line of the message, got %v", issues)
	}
}

func TestAssess_NotReadyIssueCarriesNodeNameAndReason(t *testing.T) {
	ch := Assess([]corev1.Node{notReadyNode("worker-2", "KubeletNotReady", "kubelet stopped posting node status")}, nil)
	if ch.Verdict != "Degraded" {
		t.Errorf("a NotReady node should still make the cluster Degraded, got %q", ch.Verdict)
	}
	if len(ch.NodeIssues) != 1 || ch.NodeIssues[0] != "worker-2 NotReady: KubeletNotReady — kubelet stopped posting node status" {
		t.Errorf("want the node name + enriched NotReady, got %v", ch.NodeIssues)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/clusterhealth/ -run 'NotReady|NotReadyIssue|FirstLine'`
Expected: FAIL — `nodeHealth` still emits the plain `"NotReady"` string; `notReadyNode`/enriched output not present yet.

- [ ] **Step 3: Implement the enrichment**

In `internal/clusterhealth/clusterhealth.go`, add `"strings"` to the imports. Replace `nodeHealth` with:

```go
// nodeHealth returns whether the node's Ready condition is true and a list of
// its problems. The NotReady issue is enriched with the NodeReady condition's
// reason and (trimmed) message so the output names the cause, not just "NotReady".
func nodeHealth(n corev1.Node) (ready bool, issues []string) {
	var readyReason, readyMessage string
	for _, c := range n.Status.Conditions {
		switch c.Type {
		case corev1.NodeReady:
			ready = c.Status == corev1.ConditionTrue
			readyReason, readyMessage = c.Reason, c.Message
		case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
			if c.Status == corev1.ConditionTrue {
				issues = append(issues, string(c.Type))
			}
		}
	}
	if !ready {
		issues = append(issues, notReadyIssue(readyReason, readyMessage))
	}
	if n.Spec.Unschedulable {
		issues = append(issues, "SchedulingDisabled")
	}
	return ready, issues
}

// notReadyIssue builds the NotReady issue string, adding the NodeReady
// condition's reason and trimmed message when present.
func notReadyIssue(reason, message string) string {
	s := "NotReady"
	m := trimLine(message, 120)
	switch {
	case reason != "" && m != "":
		s += ": " + reason + " — " + m
	case reason != "":
		s += ": " + reason
	case m != "":
		s += ": " + m
	}
	return s
}

// trimLine returns the first line of s, trimmed of surrounding space and
// truncated to max runes with a trailing ellipsis when longer.
func trimLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/clusterhealth/`
Expected: PASS (new tests + the existing `TestNodeHealth` and Assess tests unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/clusterhealth/clusterhealth.go internal/clusterhealth/clusterhealth_test.go
git commit -m "feat(clusterhealth): name the cause of a NotReady node (condition reason + message)"
```

---

### Task 2: Show finding Evidence in the text report

**Files:**
- Modify: `internal/report/report.go` (`printWorkload` finding loop, ~456)
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `diagnose.Finding` (`Issue`, `Reason`, `Evidence` string fields), already used by the report.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go` (it already imports `diagnose`, `inventory`, `clusterhealth`, `bytes`, `strings`):

```go
func TestPrintInventory_TextShowsFindingEvidence(t *testing.T) {
	var buf bytes.Buffer
	ws := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Pending",
		Findings: []diagnose.Finding{{
			Issue:    "Unschedulable",
			Reason:   "No node can schedule this pod (resources, taints, or affinity)",
			Evidence: "0/5 nodes are available: 3 Insufficient memory, 2 node(s) had untolerated taint",
		}},
	}}
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "⚠ Unschedulable:") {
		t.Errorf("missing the finding line:\n%s", out)
	}
	if !strings.Contains(out, "↳ 0/5 nodes are available: 3 Insufficient memory") {
		t.Errorf("expected the Evidence sub-line with the scheduler message:\n%s", out)
	}
}

func TestPrintInventory_TextOmitsEvidenceWhenEmptyOrDuplicate(t *testing.T) {
	var buf bytes.Buffer
	ws := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{
			{Issue: "CrashLoopBackOff", Reason: "boom", Evidence: ""},     // empty -> no sub-line
			{Issue: "OOMKilled", Reason: "same", Evidence: "same"},        // equals Reason -> no sub-line
		},
	}}
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "↳") {
		t.Errorf("no Evidence sub-line expected for empty/duplicate evidence:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run 'FindingEvidence|OmitsEvidence'`
Expected: FAIL — the `↳` Evidence line is not printed yet.

- [ ] **Step 3: Print Evidence under each finding**

In `internal/report/report.go`, inside `printWorkload`'s `for _, f := range wl.Findings` loop, immediately after the `⚠ <Issue>: <Reason>` line and before the `if f.Resources != nil` block, add:

```go
		if f.Evidence != "" && f.Evidence != f.Reason {
			if _, err := fmt.Fprintf(w, "      ↳ %s\n", f.Evidence); err != nil {
				return err
			}
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/report/`
Expected: PASS (2 new tests + existing report tests unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): show finding Evidence (e.g. the scheduler message) in text"
```

---

### Task 3: Docs

**Files:**
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`/`### Changed`)
- Modify: `website/docs/features/diagnostics.md`

**Interfaces:** none.

- [ ] **Step 1: CHANGELOG**

Add a new `## [Unreleased]` section above the current top `## [0.15.0]` with:

```markdown
## [Unreleased]

### Changed

- **Root cause for NotReady nodes and findings.** A `NotReady` node now names its
  cause — the `NodeReady` condition's reason and message (e.g.
  `NotReady: KubeletNotReady — container runtime network not ready: cni config
  uninitialized`) — instead of a bare `NotReady`. And the text scan now prints
  each finding's underlying signal (`Finding.Evidence`) beneath it, so a pending
  pod shows the scheduler's message (`0/5 nodes are available: 3 Insufficient
  memory, …`) without needing `--output json` or `--explain`. Read-only; the
  cluster verdict and JSON schema are unchanged.
```

- [ ] **Step 2: diagnostics.md**

Run: `grep -nE '^#|^##|^###' website/docs/features/diagnostics.md | head -40`

Add or extend a short note (matching the surrounding heading level/style) explaining that node NotReady output names the kubelet-reported cause, and that each finding shows the underlying signal (the `↳` line) — for a pending pod, the scheduler's verbatim reason. Keep it to a few sentences.

- [ ] **Step 3: Verify the website builds**

Run:

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: "Documentation built" with no strict WARNING lines about the edited page.

- [ ] **Step 4: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md
git commit -m "docs: document NotReady cause and finding evidence in text output"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Manual smoke (any cluster):

```bash
go build -o kubeagent . && ./kubeagent scan --output text
```

Expected: a NotReady node reads `✗ node <name> NotReady: <reason> — <message>`; a
flagged workload shows a `↳ <evidence>` line under its finding (e.g. the
scheduler message for an unschedulable pod).
