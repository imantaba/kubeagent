# Local `--investigate`: One Model Call per Workload — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Local verdict mode asks the model once per flagged workload the rules did not decide, each prompt holding one workload and only its own evidence, instead of one call for all workloads.

**Architecture:** `gatherEvidence` keeps its reads, budget and trail, but returns one evidence string per scoped workload (a shared describe is read once and its text copied to every workload that uses it). `LocalClient.Investigate` loops over the scoped workloads in report order and calls the model only for undecided ones; each reply's first row is pinned to the workload asked about, the summary is ignored, and partial failure becomes a new text-only `Report.Notice` that the CLI prints on stderr.

**Tech Stack:** Go 1.26, `net/http/httptest`, client-go fake clientset. No new dependency.

**Spec:** `docs/superpowers/specs/2026-10-12-per-workload-verdicts-design.md`

## Global Constraints

- Go lives at `/usr/local/go/bin`: every command starts with `export PATH=$PATH:/usr/local/go/bin` and runs with `GOFLAGS=-mod=mod`.
- TDD: write the failing test first, run it, watch it fail, then implement.
- Never run any test with `-update`. Golden files and schema files must not change.
- Commit with exactly `git -c user.name=imantaba -c user.email=itn.taba@gmail.com -c core.fileMode=false commit -s -m "<msg>"`. No `Co-Authored-By` trailer, no mention of Claude or any AI tool anywhere.
- Stage files by name. Never `git add -A` or `git add .`. Never stage `.gitignore` (it has an unrelated local change).
- Never run `git config`. Never push.
- `internal/hypothesis` does not change at all.
- `verdictSystemPrompt`, `verdictSchema`, `verdictDoc`, `verdictRow`, `post`, `parseVerdicts`, `bodySnippet` and `capRunes` do not change.
- `scan` stays at schema 1.8: `Report.Notice` is never marshalled to JSON.
- Bounds stay: 8 reads (`maxToolCalls`), 10 scoped workloads (`maxGatherWorkloads`), 4 KiB per read, 10 service issues per prompt, 64 KiB per prompt, 1 MiB per response, 512 runes per model line, 10 rows (`maxVerdictRows`).
- Notice text, exactly: `model gave no verdict for <failed> of <undecided> workloads: <first failure reason>`, where the reason is `safetext.Line(redact.Error(firstErr))`.
- No-row error text, exactly: `model returned no verdict`.
- Untrusted text: every model string still passes `capRunes` then `safetext.Line` before it reaches the narrative.

## Review Focus

1. **Endpoint URL in the notice.** A transport failure (connection dropped) wraps a `*url.Error` carrying the full endpoint URL; the notice must go through `redact.Error` so the `/chat/completions` path never reaches stderr. Test: `TestLocalInvestigateNoticeCarriesNoEndpointPath` (Task 2).
2. **Deadline in the middle of the run.** The first call answers, the second hangs until the 300 s context ends; the first row must still render, the third workload must get no request, and the notice must say `2 of 3`. Test: `TestLocalInvestigateDeadlineMidRunKeepsEarlierRows` (Task 2).
3. **A reply whose cause is blank after sanitizing** (spaces, or only control characters) must count as no verdict, not render an empty row. Test: `TestLocalInvestigateBlankCauseIsNoVerdict` (Task 2).
4. **Service issues from other namespaces** must not reach a workload's prompt, and the 10-issue cap applies after the namespace filter. Test: `TestBuildVerdictPromptOnlyItsNamespaceServiceIssues` (Task 2).
5. **Workloads past the read budget.** With 10 undecided workloads, the 9th and 10th have no reads; they still get a call, and their evidence section is `(none)`. Test: `TestLocalInvestigateTenWorkloadsPastTheReadBudget` (Task 2).

---

### Task 1: Per-workload evidence from the gather

**Files:**
- Modify: `internal/investigate/gather.go` (functions `gatherEvidence` and `appendRead`, lines 45–169)
- Modify: `internal/investigate/local.go:215` (one interim line in `Investigate`)
- Test: `internal/investigate/gather_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `func gatherEvidence(ctx context.Context, client kubernetes.Interface, scoped []inventory.Workload) ([]string, []string, hypothesis.Reads)` — returns the trail, then `evidence` with `len(evidence) == len(scoped)` and `evidence[i]` belonging to `scoped[i]`, then the reads.
  - `func appendRead(b *strings.Builder, trail *[]string, spent *int, label, content string) string` — returns the section it wrote.

- [ ] **Step 1: Update the existing gather tests to the new return type**

In `internal/investigate/gather_test.go`, make these edits.

Replace the whole of `TestGatherEvidenceDeterministicTrailAndSections` with:

```go
func TestGatherEvidenceDeterministicTrailAndSections(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev-1", Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Name: "web-abc"},
			Reason:         "BackOff", Message: "Back-off restarting failed container", Count: 4},
	)
	w := gatherWL("shop", "web", diagnose.Finding{Pod: "shop/web-abc", Issue: "CrashLoopBackOff", Container: "app"})
	w.RootCauseTrace = []inventory.Hypothesis{
		{Cause: "node worker-1 (NotReady)", Kind: "node", Object: "worker-1",
			Verdict: inventory.VerdictAttributed, Reason: "pod web-abc is scheduled on it"},
		{Cause: "registry ghcr.io", Kind: "registry", Object: "ghcr.io",
			Verdict: inventory.VerdictOutranked, Reason: "node worker-1 (NotReady) is the stronger cause"},
		{Cause: "PVC web-data (ProvisioningFailed)", Kind: "pvc", Object: "web-data",
			Verdict: inventory.VerdictRuledOut, Reason: "not mounted by this workload's pods"},
	}
	scoped := []inventory.Workload{w}
	trail1, ev1, _ := gatherEvidence(context.Background(), client, scoped)
	trail2, ev2, _ := gatherEvidence(context.Background(), client, scoped)
	if strings.Join(trail1, "|") != strings.Join(trail2, "|") || strings.Join(ev1, "\x00") != strings.Join(ev2, "\x00") {
		t.Fatalf("gather must be deterministic")
	}
	if len(ev1) != 1 {
		t.Fatalf("want one evidence entry per scoped workload, got %d", len(ev1))
	}
	// Registry candidates get no read; ruled-out candidates get no read.
	wantTrail := []string{
		"events shop/web-abc",
		"describe node /worker-1",
		"log causes shop/web-abc container app",
	}
	if strings.Join(trail1, "|") != strings.Join(wantTrail, "|") {
		t.Errorf("trail = %v, want %v", trail1, wantTrail)
	}
	for _, label := range wantTrail {
		if !strings.Contains(ev1[0], "== "+label+" ==\n") {
			t.Errorf("evidence missing section %q:\n%s", label, ev1[0])
		}
	}
	if !strings.Contains(ev1[0], "BackOff: Back-off restarting failed container (x4)") {
		t.Errorf("event content missing from evidence:\n%s", ev1[0])
	}
}
```

Replace the whole of `TestGatherEvidenceDedupesDescribesAcrossWorkloads` with:

```go
func TestGatherEvidenceDedupesDescribesAcrossWorkloads(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})
	shared := inventory.Hypothesis{Cause: "node worker-1 (NotReady)", Kind: "node", Object: "worker-1",
		Verdict: inventory.VerdictAttributed, Reason: "pod a is scheduled on it"}
	w1 := gatherWL("shop", "web")
	w1.RootCauseTrace = []inventory.Hypothesis{shared}
	w2 := gatherWL("shop", "api")
	w2.RootCauseTrace = []inventory.Hypothesis{shared}
	trail, ev, _ := gatherEvidence(context.Background(), client, []inventory.Workload{w1, w2})
	describes := 0
	for _, l := range trail {
		if l == "describe node /worker-1" {
			describes++
		}
	}
	if describes != 1 {
		t.Errorf("node described %d times, want 1 (read once); trail: %v", describes, trail)
	}
	// Read once, but each prompt holds one workload, so each needs the text.
	for i, e := range ev {
		if !strings.Contains(e, "== describe node /worker-1 ==\n") {
			t.Errorf("workload %d's evidence lacks the shared describe:\n%s", i, e)
		}
	}
}
```

In `TestGatherEvidenceFailedReadCountsAndIsReduced`, replace the line

```go
	trail, bundle, _ := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
```

with

```go
	trail, ev, _ := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
	bundle := ev[0]
```

In `TestGatherEvidenceCapsOneReadAtFourKiB`, replace

```go
	_, bundle, _ := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
```

with

```go
	_, ev, _ := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
	bundle := ev[0]
```

In `TestGatherEvidenceMissingNodeIsAFailedRead`, replace

```go
	_, bundle, reads := gatherEvidence(context.Background(), client, []inventory.Workload{w})
```

with

```go
	_, ev, reads := gatherEvidence(context.Background(), client, []inventory.Workload{w})
	bundle := ev[0]
```

In `TestGatherEvidenceFailedReadIsSanitizedForTheRules`, replace

```go
	_, bundle, reads := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
```

with

```go
	_, ev, reads := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
	bundle := ev[0]
```

Replace the comment above `TestSharedCapsMatchTheSummaryCaps` with:

```go
// The rules' shared-cause lines are capped at maxSummaryLines and cut with
// the same marker; the two packages must agree on both.
```

The other gather tests (`TestFlaggedScopeCapsAtTen`, `TestGatherEvidenceGlobalBudgetIsEight`, `TestGatherEvidenceEventsFallBackToWorkloadName`, `TestGatherEvidenceSkipsLogReadWithoutContainerAndDedupes`, `TestCapContentCutsAtLineBoundaryWithMarker`, `TestGatherEvidenceReadsReachTheRules`, `TestGatherEvidenceBudgetLeavesReadsUnmade`) already use `_` for the middle value and do not change.

- [ ] **Step 2: Add the new gather tests**

Append to `internal/investigate/gather_test.go`:

```go
func TestGatherEvidenceIsPerWorkload(t *testing.T) {
	trail, ev, _ := gatherEvidence(context.Background(), fake.NewSimpleClientset(),
		[]inventory.Workload{gatherWL("shop", "web"), gatherWL("shop", "api")})
	if len(trail) != 2 {
		t.Fatalf("want two events reads, trail: %v", trail)
	}
	if len(ev) != 2 {
		t.Fatalf("want one evidence entry per scoped workload, got %d", len(ev))
	}
	if !strings.Contains(ev[0], "== events shop/web ==\n") || strings.Contains(ev[0], "events shop/api") {
		t.Errorf("web's evidence must hold its own read only:\n%s", ev[0])
	}
	if !strings.Contains(ev[1], "== events shop/api ==\n") || strings.Contains(ev[1], "events shop/web") {
		t.Errorf("api's evidence must hold its own read only:\n%s", ev[1])
	}
}

// A describe already read for an earlier workload costs no budget, so it
// still reaches a later workload after the budget is spent.
func TestGatherEvidenceCachedDescribeReachesWorkloadPastBudget(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})
	node := inventory.Hypothesis{Cause: "node worker-1 (NotReady)", Kind: "node", Object: "worker-1",
		Verdict: inventory.VerdictAttributed, Reason: "pods are scheduled on it"}
	var findings []diagnose.Finding
	for i := 1; i <= 6; i++ {
		findings = append(findings, diagnose.Finding{Pod: "shop/web-abc", Issue: "CrashLoopBackOff", Container: fmt.Sprintf("c%d", i)})
	}
	// web: events + describe + six log reads = the whole budget of 8.
	w1 := gatherWL("shop", "web", findings...)
	w1.RootCauseTrace = []inventory.Hypothesis{node}
	w2 := gatherWL("shop", "api")
	w2.RootCauseTrace = []inventory.Hypothesis{node}
	trail, ev, _ := gatherEvidence(context.Background(), client, []inventory.Workload{w1, w2})
	if len(trail) != maxToolCalls {
		t.Fatalf("want the whole budget spent on web, trail: %v", trail)
	}
	for _, l := range trail {
		if l == "events shop/api" {
			t.Errorf("api's events read is past the budget and must not be made")
		}
	}
	if !strings.Contains(ev[1], "== describe node /worker-1 ==\n") {
		t.Errorf("api must still get the describe web already paid for:\n%s", ev[1])
	}
	if strings.Contains(ev[1], "== events ") {
		t.Errorf("api's evidence must not carry an events read that was never made:\n%s", ev[1])
	}
}

func TestGatherEvidenceOneEntryPerScopedWorkload(t *testing.T) {
	var ws []inventory.Workload
	for i := 0; i < maxGatherWorkloads; i++ {
		ws = append(ws, gatherWL("shop", fmt.Sprintf("web-%02d", i)))
	}
	_, ev, _ := gatherEvidence(context.Background(), fake.NewSimpleClientset(), ws)
	if len(ev) != maxGatherWorkloads {
		t.Fatalf("want %d evidence entries, got %d", maxGatherWorkloads, len(ev))
	}
	if ev[7] == "" {
		t.Errorf("the 8th workload's events read is within budget")
	}
	if ev[8] != "" || ev[9] != "" {
		t.Errorf("workloads past the budget have empty evidence, got %q and %q", ev[8], ev[9])
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin; GOFLAGS=-mod=mod go test ./internal/investigate -run 'TestGatherEvidence' -v`
Expected: FAIL — a build error such as `cannot use ... (variable of type string) as []string value` / `invalid operation: cannot index ev`.

- [ ] **Step 4: Implement per-workload evidence**

In `internal/investigate/gather.go`, replace `gatherEvidence` and `appendRead` (the doc comment of `gatherEvidence` through the end of `appendRead`) with:

```go
// gatherEvidence is local verdict mode's deterministic evidence pre-fetch:
// kubeagent chooses the reads, in report order, under the tool loop's global
// budget. Per workload, in order: the events of its first finding's pod (the
// workload name when there is no finding), a describe per surviving node or
// PVC candidate (registry candidates have nothing to read), and a classified
// previous-log cause per crash-family finding (deduped per container). A
// describe is read once: a later workload with the same node or PVC gets a
// copy of the same section text, at no budget cost, even after the budget
// is spent — each prompt holds one workload, so each needs it. It returns
// the evidence trail — byte-for-byte the tool loop's label() formats, one
// entry per read actually made —, one evidence string per scoped workload
// (evidence[i] belongs to scoped[i]; "" when no read reached it), and the
// objects it read, keyed for the rules. A failed read still consumes budget
// (refusal is evidence) and renders as a reduced error, never a raw
// client-go message; the same reduced error, passed through safetext.Line,
// is what the rules see under Reads.Failed.
func gatherEvidence(ctx context.Context, client kubernetes.Interface, scoped []inventory.Workload) ([]string, []string, hypothesis.Reads) {
	var (
		trail []string
		spent int
	)
	evidence := make([]string, len(scoped))
	fresh := hypothesis.Reads{
		Nodes:  map[string]*corev1.Node{},
		PVCs:   map[string]*corev1.PersistentVolumeClaim{},
		Events: map[string][]corev1.Event{},
		Failed: map[string]string{},
	}
	described := map[string]string{} // describe key → the section it rendered
	seenLog := map[string]bool{}
	for i, w := range scoped {
		var b strings.Builder
		if spent < maxToolCalls {
			name := w.Name
			if len(w.Findings) > 0 {
				if p := podPart(w.Findings[0].Pod); p != "" {
					name = p
				}
			}
			var content string
			items, err := listEvents(ctx, client, w.Namespace, name)
			if err != nil {
				fresh.Failed["events/"+w.Namespace+"/"+name] = safetext.Line(redact.Error(err))
				content = "read failed: " + redact.Error(err)
			} else {
				fresh.Events[w.Namespace+"/"+name] = items
				content = formatEvents(w.Namespace, name, items)
			}
			appendRead(&b, &trail, &spent, fmt.Sprintf("events %s/%s", w.Namespace, name), content)
		}

		own := map[string]bool{} // describes already in this workload's evidence
		for _, h := range w.RootCauseTrace {
			if h.Verdict == inventory.VerdictRuledOut || h.Object == "" {
				continue
			}
			if h.Kind != "node" && h.Kind != "pvc" {
				continue // registry: no object to read
			}
			ns := ""
			if h.Kind == "pvc" {
				ns = w.Namespace
			}
			key := h.Kind + "/" + ns + "/" + h.Object
			if own[key] {
				continue
			}
			if sec, ok := described[key]; ok {
				own[key] = true
				b.WriteString(sec)
				continue
			}
			if spent >= maxToolCalls {
				continue // a later candidate may still be a cached describe
			}
			own[key] = true
			var content string
			switch h.Kind {
			case "node":
				n, err := client.CoreV1().Nodes().Get(ctx, h.Object, metav1.GetOptions{})
				if err != nil {
					fresh.Failed["node/"+h.Object] = safetext.Line(redact.Error(err))
					content = "read failed: " + redact.Error(err)
				} else {
					fresh.Nodes[h.Object] = n
					content = describeNode(n)
				}
			case "pvc":
				pvc, err := client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, h.Object, metav1.GetOptions{})
				if err != nil {
					fresh.Failed["pvc/"+ns+"/"+h.Object] = safetext.Line(redact.Error(err))
					content = "read failed: " + redact.Error(err)
				} else {
					fresh.PVCs[ns+"/"+h.Object] = pvc
					content = describePVC(pvc)
				}
			}
			described[key] = appendRead(&b, &trail, &spent, fmt.Sprintf("describe %s %s/%s", h.Kind, ns, h.Object), content)
		}

		for _, f := range w.Findings {
			if spent >= maxToolCalls {
				break
			}
			if !crashFamily(f.Issue) || f.Container == "" {
				continue
			}
			pod := podPart(f.Pod)
			if pod == "" {
				continue
			}
			key := w.Namespace + "/" + pod + "/" + f.Container
			if seenLog[key] {
				continue
			}
			seenLog[key] = true
			log, ok, err := collect.PreviousLogs(ctx, client, w.Namespace, pod, f.Container)
			res := logCauseResult("", w.Namespace, pod, f.Container, log, ok, err)
			appendRead(&b, &trail, &spent, fmt.Sprintf("log causes %s/%s container %s", w.Namespace, pod, f.Container), res.Content)
		}
		evidence[i] = b.String()
	}
	return trail, evidence, fresh
}

// appendRead records one completed read: one trail entry, one budget unit,
// one evidence section, which it also returns so a describe can be copied
// into a later workload's evidence. Content arrives already reduced (never a
// raw error) and is capped at maxReadBytes here; trailing newlines are
// normalized so a section is always exactly "== label ==\n<content>\n\n".
func appendRead(b *strings.Builder, trail *[]string, spent *int, label, content string) string {
	*trail = append(*trail, label)
	*spent++
	sec := "== " + label + " ==\n" + strings.TrimRight(capContent(content), "\n") + "\n\n"
	b.WriteString(sec)
	return sec
}
```

In `internal/investigate/local.go`, inside `Investigate`, replace

```go
	trail, bundle, reads := gatherEvidence(ctx, client, scoped)
```

with

```go
	trail, evidence, reads := gatherEvidence(ctx, client, scoped)
	bundle := strings.Join(evidence, "") // interim: Task 2 gives each workload its own prompt
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin; GOFLAGS=-mod=mod go test ./internal/investigate -v -run 'TestGatherEvidence|TestFlaggedScope|TestCapContent|TestSharedCaps'`
Expected: PASS, every test listed.

Run: `export PATH=$PATH:/usr/local/go/bin; GOFLAGS=-mod=mod go test ./internal/investigate`
Expected: `ok` — the existing local verdict tests still pass with the interim join.

- [ ] **Step 6: Commit**

```bash
git add internal/investigate/gather.go internal/investigate/gather_test.go internal/investigate/local.go
git -c user.name=imantaba -c user.email=itn.taba@gmail.com -c core.fileMode=false commit -s -m "feat(investigate): gather evidence per workload in local verdict mode"
```

---

### Task 2: One model call per undecided workload

**Files:**
- Modify: `internal/investigate/local.go` (imports; `buildVerdictPrompt`; the output-bounds const block; `LocalClient` doc; `Investigate`; `call`; `renderVerdicts`; `modelRow`; `ruleRow`; delete `firstModelRows` and `capSummary`)
- Modify: `internal/investigate/investigate.go:130-138` (`Report` gains `Notice`)
- Modify: `internal/investigate/local_test.go`
- Create: `internal/investigate/local_calls_test.go`

**Interfaces:**
- Consumes (Task 1): `gatherEvidence(ctx, client, scoped) (trail []string, evidence []string, reads hypothesis.Reads)`, with `evidence[i]` for `scoped[i]`.
- Produces:
  - `Report.Notice string` in `internal/investigate` — set only by local verdict mode on partial failure; Task 3 reads it.
  - `func buildVerdictPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, w inventory.Workload, r hypothesis.Result, evidence string) string`
  - `var errNoVerdict = errors.New("model returned no verdict")`
  - `func (c *LocalClient) call(ctx context.Context, prompt string, withFormat *bool) (verdictRow, bool, error)`
  - `func renderVerdicts(model map[string]verdictRow, results []hypothesis.Result, shared []string) string` — `model` keyed by `"namespace/name"`.
  - `func modelRow(name string, v verdictRow) string`
  - `func ruleRow(r hypothesis.Result) string`

- [ ] **Step 1: Rework the existing tests in `local_test.go`**

Add `"errors"` to the import block of `internal/investigate/local_test.go`.

Add this helper right after `verdictTestWorkloads`:

```go
// untracedWorkloads is verdictTestWorkloads with no candidate: nothing for
// the rules to decide, so the workload gets a model call.
func untracedWorkloads() []inventory.Workload {
	ws := verdictTestWorkloads()
	ws[0].RootCauseTrace = nil
	return ws
}
```

Edit the prompt tests:

- `TestBuildVerdictPromptSectionsInOrder`: replace the call with
  ```go
  	prompt := buildVerdictPrompt(clusterhealth.ClusterHealth{Verdict: "Degraded"}, nil, nil, nil,
  		w, hypothesis.Result{}, "== events shop/web-abc ==\nBackOff: restarting (x4)\n\n")
  ```
- `TestBuildVerdictPromptEmptyEvidenceRendersNone`: replace the call with
  ```go
  	prompt := buildVerdictPrompt(clusterhealth.ClusterHealth{Verdict: "Degraded"}, nil, nil, nil, inventory.Workload{}, hypothesis.Result{}, "")
  ```
- `TestBuildVerdictPromptDefensiveCap` and `TestBuildVerdictPromptDefensiveCapRealisticBundleShape`: in each, replace the call with
  ```go
  	prompt := buildVerdictPrompt(clusterhealth.ClusterHealth{Verdict: "Degraded"}, nil, nil, nil, inventory.Workload{}, hypothesis.Result{}, huge)
  ```
- `TestBuildVerdictPromptCarriesRuleLines`: replace the call with
  ```go
  	prompt := buildVerdictPrompt(clusterhealth.ClusterHealth{Verdict: "Degraded"}, nil, nil, nil, w, r, "")
  ```
- Delete `TestBuildVerdictPromptScopesToTenWorkloads` and put this in its place:
  ```go
  func TestBuildVerdictPromptOnlyItsNamespaceServiceIssues(t *testing.T) {
  	var issues []svchealth.Issue
  	for i := 0; i < 5; i++ {
  		issues = append(issues, svchealth.Issue{Namespace: "other", Name: fmt.Sprintf("svc-%02d", i),
  			Type: "ClusterIP", Detail: "no ready endpoints"})
  	}
  	for i := 0; i < 12; i++ {
  		issues = append(issues, svchealth.Issue{Namespace: "shop", Name: fmt.Sprintf("svc-%02d", i),
  			Type: "ClusterIP", Detail: "no ready endpoints"})
  	}
  	prompt := buildVerdictPrompt(degraded(), nil, nil, issues, gatherWL("shop", "web"), hypothesis.Result{}, "")
  	if n := strings.Count(prompt, "  - shop/svc-"); n != maxServiceIssuesInPrompt {
  		t.Errorf("want %d shop service issues (the cap applies after the filter), got %d:\n%s", maxServiceIssuesInPrompt, n, prompt)
  	}
  	if strings.Contains(prompt, "other/") {
  		t.Errorf("another namespace's service issues must not reach this workload's prompt:\n%s", prompt)
  	}
  	if strings.Contains(prompt, "shop/svc-10") || strings.Contains(prompt, "shop/svc-11") {
  		t.Errorf("issues past the cap must be dropped:\n%s", prompt)
  	}
  }
  ```

Replace `TestLocalInvestigateHappyPath` with:

```go
func TestLocalInvestigateHappyPath(t *testing.T) {
	verdict := `{"verdicts":[{"workload":"shop/web","cause":"none_of_these","confidence":"high","rationale":"events show nothing conclusive"}],"summary":"One workload."}`
	var gotPath, gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Write(chatReply(t, verdict, "stop"))
	}))
	defer srv.Close()
	rep, err := NewLocal(srv.URL, "tiny-model", "").Investigate(context.Background(),
		degraded(), nil, nil, nil, untracedWorkloads(), fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("no apiKey must mean no Authorization header, got %q", gotAuth)
	}
	var req struct {
		Model          string `json:"model"`
		ResponseFormat any    `json:"response_format"`
		Messages       []struct {
			Role, Content string
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "tiny-model" {
		t.Errorf("model = %q", req.Model)
	}
	if req.ResponseFormat == nil {
		t.Errorf("first attempt must carry response_format")
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
		t.Fatalf("want [system,user] messages, got %+v", req.Messages)
	}
	if !strings.Contains(req.Messages[1].Content, "== BEGIN evidence ==") {
		t.Errorf("user message must be the delimited prompt")
	}
	want := "Root-cause verdicts:\n- shop/web: none_of_these [model, confidence: high] — events show nothing conclusive"
	if rep.Narrative != want {
		t.Errorf("got:\n%s\nwant:\n%s", rep.Narrative, want)
	}
	if strings.Contains(rep.Narrative, "One workload.") {
		t.Errorf("the model's summary is not rendered:\n%s", rep.Narrative)
	}
	if len(rep.Consulted) == 0 {
		t.Errorf("the evidence trail must reach Report.Consulted")
	}
	if rep.Truncated || rep.Notice != "" {
		t.Errorf("a clean call sets neither Truncated nor Notice: %+v", rep)
	}
}
```

Delete `TestLocalInvestigateRetriesWithoutResponseFormatOn400` (Step 2's `TestLocalInvestigate400DropsResponseFormatForTheRun` replaces it).

Replace `TestLocalInvestigateParsesFencedJSON` with:

```go
func TestLocalInvestigateParsesFencedJSON(t *testing.T) {
	content := "Here is my answer:\n```json\n{\"verdicts\":[{\"workload\":\"shop/web\",\"cause\":\"node worker-1 (NotReady)\",\"confidence\":\"medium\",\"rationale\":\"node is NotReady\"}],\"summary\":\"One down node.\"}\n```\nDone."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(chatReply(t, content, "stop"))
	}))
	defer srv.Close()
	rep, err := NewLocal(srv.URL, "tiny-model", "").Investigate(context.Background(),
		degraded(), nil, nil, nil, untracedWorkloads(), fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Narrative, "- shop/web: node worker-1 (NotReady) [model, confidence: medium] — node is NotReady") {
		t.Errorf("fence-wrapped JSON must still parse:\n%s", rep.Narrative)
	}
}
```

Replace `TestRenderVerdictsCapsRowsAndDropsUnknownWorkloads` with:

```go
func TestRenderVerdictsCapsRows(t *testing.T) {
	var results []hypothesis.Result
	model := map[string]verdictRow{}
	for i := 0; i < 11; i++ {
		wl := fmt.Sprintf("shop/web-%02d", i)
		results = append(results, hypothesis.Result{Workload: wl})
		model[wl] = verdictRow{Cause: fmt.Sprintf("cause-%02d", i), Confidence: "low", Rationale: "r"}
	}
	got := renderVerdicts(model, results, nil)
	if n := strings.Count(got, "cause-"); n != maxVerdictRows {
		t.Errorf("rendered %d rows, want the cap %d:\n%s", n, maxVerdictRows, got)
	}
	if strings.Contains(got, "cause-10") {
		t.Errorf("row 11 must be dropped by the cap:\n%s", got)
	}
}
```

Delete `TestRenderVerdictsKeepsFlaggedWorkloadBeyondGatherCap` (a workload beyond the scope no longer gets a row).

Replace `TestRenderVerdictsSanitizesAndBoundsModelText` with:

```go
func TestRenderVerdictsSanitizesAndBoundsModelText(t *testing.T) {
	model := map[string]verdictRow{"shop/web": {
		Workload:   "shop/web",
		Cause:      "bad\x1b[31mcause\nwith newline",
		Confidence: "certain!!",
		Rationale:  strings.Repeat("я", 600),
	}}
	got := renderVerdicts(model, []hypothesis.Result{{Workload: "shop/web"}}, nil)
	if strings.Contains(got, "\x1b") {
		t.Errorf("control bytes must not survive:\n%q", got)
	}
	// One row, no shared lines: exactly one newline (header/row boundary). A
	// newline surviving inside the cause would add a second.
	if strings.Count(got, "\n") != 1 {
		t.Errorf("a newline inside model text must not survive into the narrative:\n%q", got)
	}
	if !strings.Contains(got, "[model, confidence: unstated]") {
		t.Errorf("an out-of-vocabulary confidence must render unstated:\n%s", got)
	}
	if !strings.Contains(got, truncationMarker) {
		t.Errorf("a 600-rune rationale must be cut and marked:\n%s", got)
	}
}
```

Delete `TestCapSummaryFourLines` (`capSummary` is deleted).

Replace `TestLocalInvestigateBearerHeader` with:

```go
func TestLocalInvestigateBearerHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write(chatReply(t, `{"verdicts":[{"workload":"shop/web","cause":"none_of_these","confidence":"low","rationale":"r"}],"summary":""}`, "stop"))
	}))
	defer srv.Close()
	_, err := NewLocal(srv.URL, "tiny-model", "test-key").Investigate(context.Background(),
		degraded(), nil, nil, nil, untracedWorkloads(), fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}
```

In each of `TestLocalInvestigateErrorCarriesStatusAndSnippet`, `TestLocalInvestigateErrorSanitizesHostileBodySnippet`, `TestLocalInvestigateFinishReasonLengthSetsTruncated` and `TestLocalInvestigateRejectsOversizedResponse`, change the workloads argument from `verdictTestWorkloads()` to `untracedWorkloads()`. (With the attributed node candidate and an empty clientset the node read fails, the rules decide the workload as unverified, and no call would be made.) Nothing else in those four tests changes.

Replace `TestLocalInvestigateEmptyVerdictsIsAnError` with:

```go
func TestLocalInvestigateEmptyVerdictsIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(chatReply(t, `{"verdicts":[],"summary":"a summary alone is not a verdict"}`, "stop"))
	}))
	defer srv.Close()
	rep, err := NewLocal(srv.URL, "tiny-model", "").Investigate(context.Background(),
		degraded(), nil, nil, nil, untracedWorkloads(), fake.NewSimpleClientset())
	if err == nil || err.Error() != "investigating: model returned no verdict" {
		t.Fatalf("no rows must be the no-verdict error, got %v", err)
	}
	if !errors.Is(err, errNoVerdict) {
		t.Errorf("the error must wrap errNoVerdict: %v", err)
	}
	if rep.Narrative != "" || len(rep.Consulted) != 0 || rep.Notice != "" {
		t.Errorf("with no rule decision the failed report is empty: %+v", rep)
	}
}
```

`TestLocalInvestigateSkipsHealthyCluster` does not change.

Replace `TestRenderVerdictsRowShapes` with:

```go
func TestRenderVerdictsRowShapes(t *testing.T) {
	results := []hypothesis.Result{
		nodeDown("shop/web"),
		decidedResult("shop/api", "PVC api-data (ProvisioningFailed)", hypothesis.Unverified, "not re-read: the read budget was spent first"),
		{Workload: "shop/cart"},
	}
	// The model wrote a short name; the row is placed by the map key.
	model := map[string]verdictRow{"shop/cart": {Workload: "cart", Cause: "none_of_these", Confidence: "low", Rationale: "evidence is thin"}}
	want := "Root-cause verdicts:\n" +
		"- shop/web: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now\n" +
		"- shop/api: PVC api-data (ProvisioningFailed) [rule, unverified] — not re-read: the read budget was spent first\n" +
		"- shop/cart: none_of_these [model, confidence: low] — evidence is thin"
	if got := renderVerdicts(model, results, nil); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// The order is report order, whichever kind of row each gets.
	results = []hypothesis.Result{results[2], results[0], results[1]}
	want = "Root-cause verdicts:\n" +
		"- shop/cart: none_of_these [model, confidence: low] — evidence is thin\n" +
		"- shop/web: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now\n" +
		"- shop/api: PVC api-data (ProvisioningFailed) [rule, unverified] — not re-read: the read budget was spent first"
	if got := renderVerdicts(model, results, nil); got != want {
		t.Errorf("reordered: got:\n%s\nwant:\n%s", got, want)
	}
}
```

Replace `TestRenderVerdictsRuleRowRationale` with:

```go
func TestRenderVerdictsRuleRowRationale(t *testing.T) {
	results := []hypothesis.Result{nodeDown("shop/web")}
	want := "Root-cause verdicts:\n- shop/web: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now"
	cases := []struct {
		name  string
		model map[string]verdictRow
	}{
		{"no model row", nil},
		{"model row with the same cause", map[string]verdictRow{"shop/web": {Cause: "node worker-1 (NotReady)", Confidence: "high", Rationale: "events show the pod stuck on the down node"}}},
		{"model row with another cause", map[string]verdictRow{"shop/web": {Cause: "none_of_these", Confidence: "high", Rationale: "the node looks fine"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderVerdicts(tc.model, results, nil); got != want {
				t.Errorf("a rule row always carries the rule's evidence sentence:\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}
```

Replace `TestRenderVerdictsRuleRowRendersWhenModelDropsIt` with:

```go
func TestRenderVerdictsRuleRowRendersWhenModelDropsIt(t *testing.T) {
	results := []hypothesis.Result{nodeDown("shop/web"), {Workload: "shop/api"}}
	model := map[string]verdictRow{"shop/api": {Cause: "none_of_these", Confidence: "low", Rationale: "r"}}
	want := "Root-cause verdicts:\n" +
		"- shop/web: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now\n" +
		"- shop/api: none_of_these [model, confidence: low] — r"
	if got := renderVerdicts(model, results, nil); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// The undecided workload's call failed: the rule row still renders.
	if got := renderVerdicts(nil, results, nil); got != "Root-cause verdicts:\n- shop/web: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now" {
		t.Errorf("a decided workload renders without any model row:\n%s", got)
	}
}
```

Delete `TestRenderVerdictsFirstModelRowPerWorkloadWins` and `TestRenderVerdictsScopedOrderBeforeModelOrder` (Step 2's `TestLocalInvestigateExtraRowsAreIgnored` and `TestLocalInvestigateRowIsPinnedToTheAskedWorkload` replace them).

Replace `TestRenderVerdictsCapCountsBothSources` with:

```go
func TestRenderVerdictsCapCountsBothSources(t *testing.T) {
	var results []hypothesis.Result
	model := map[string]verdictRow{}
	for i := 0; i < 6; i++ {
		r := fmt.Sprintf("shop/r-%02d", i)
		m := fmt.Sprintf("shop/m-%02d", i)
		results = append(results, nodeDown(r), hypothesis.Result{Workload: m})
		model[m] = verdictRow{Cause: "none_of_these", Confidence: "low", Rationale: "r"}
	}
	got := renderVerdicts(model, results, nil)
	if strings.Count(got, "[rule, ") != 5 || strings.Count(got, "[model, ") != 5 {
		t.Errorf("the cap counts rule rows and model rows together, in report order:\n%s", got)
	}
	if strings.Contains(got, "shop/r-05") || strings.Contains(got, "shop/m-05") {
		t.Errorf("rows past the cap must be dropped:\n%s", got)
	}
}
```

Replace `TestRenderVerdictsSharedLinesBeforeSummary` with:

```go
func TestRenderVerdictsSharedLinesFollowRows(t *testing.T) {
	results := []hypothesis.Result{nodeDown("shop/web"), nodeDown("shop/api")}
	shared := []string{"2 workloads share one upstream cause: node worker-1 (NotReady)"}
	want := "Root-cause verdicts:\n" +
		"- shop/web: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now\n" +
		"- shop/api: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now\n\n" +
		"2 workloads share one upstream cause: node worker-1 (NotReady)"
	if got := renderVerdicts(nil, results, shared); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Shared lines with no rows: no leading blank line.
	if got := renderVerdicts(nil, nil, shared); got != shared[0] {
		t.Errorf("got %q, want %q", got, shared[0])
	}
}
```

Replace `TestRenderVerdictsAllRefutedRowFallsToModel` with:

```go
func TestRenderVerdictsAllRefutedRowFallsToModel(t *testing.T) {
	ws := verdictTestWorkloads()
	refuted := hypothesis.Result{Workload: "shop/web", Decisions: []hypothesis.Decision{{
		Candidate: ws[0].RootCauseTrace[0], Outcome: hypothesis.Refuted, Evidence: "Ready condition is True now"}}}
	model := map[string]verdictRow{"shop/web": {Cause: "none_of_these", Confidence: "medium", Rationale: "the node is healthy now"}}
	want := "Root-cause verdicts:\n- shop/web: none_of_these [model, confidence: medium] — the node is healthy now"
	if got := renderVerdicts(model, []hypothesis.Result{refuted}, nil); got != want {
		t.Errorf("an undecided workload is the model's to name:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if got := renderVerdicts(nil, []hypothesis.Result{refuted}, nil); got != "" {
		t.Errorf("an undecided workload with no model row renders nothing, got:\n%s", got)
	}
}
```

Delete `TestLocalInvestigateFailedCallReturnsRulesOnlyReport` (its one workload is now rule-decided, so no call is made; Step 2's `TestLocalInvestigateEveryCallFailedReturnsRulesOnly` replaces it).

In `TestLocalInvestigateFailedCallWithNoRuleDecisionIsEmpty`, replace the two lines

```go
	ws := verdictTestWorkloads()
	ws[0].RootCauseTrace = nil // nothing for the rules to decide
```

with

```go
	ws := untracedWorkloads() // nothing for the rules to decide
```

Delete `TestLocalInvestigateSharedLineFromRules` (both its workloads are rule-decided, so no call is made; Step 2's `TestLocalInvestigateAllDecidedMakesNoCall` replaces it with an exact narrative).

- [ ] **Step 2: Write the new per-call tests**

Create `internal/investigate/local_calls_test.go`:

```go
package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// verdictCall is one request the test endpoint received.
type verdictCall struct {
	User       string // the user message: the delimited prompt
	WithFormat bool   // the request carried response_format
}

// verdictReply is how the test endpoint answers one request.
type verdictReply struct {
	Status  int    // 0 means 200
	Content string // assistant message content, or the error body
	Finish  string // "" means "stop"
	Hang    bool   // block until the client gives up
	Drop    bool   // close the connection without answering
}

// verdictEndpoint is a /chat/completions stand-in that records every request
// (retries included, in arrival order) and answers each with reply(i, user),
// where i counts requests from 0.
func verdictEndpoint(t *testing.T, reply func(i int, user string) verdictReply) (string, func() []verdictCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []verdictCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ResponseFormat json.RawMessage `json:"response_format"`
			Messages       []struct {
				Role, Content string
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 2 {
			t.Errorf("unexpected request body: %v", err)
			http.Error(w, "unexpected request body", http.StatusTeapot)
			return
		}
		mu.Lock()
		i := len(calls)
		calls = append(calls, verdictCall{User: req.Messages[1].Content, WithFormat: len(req.ResponseFormat) > 0})
		mu.Unlock()
		rep := reply(i, req.Messages[1].Content)
		switch {
		case rep.Hang:
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		case rep.Drop:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
			return
		case rep.Status != 0 && rep.Status != http.StatusOK:
			http.Error(w, rep.Content, rep.Status)
			return
		}
		finish := rep.Finish
		if finish == "" {
			finish = "stop"
		}
		w.Write(chatReply(t, rep.Content, finish))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []verdictCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]verdictCall(nil), calls...)
	}
}

// subject names the one test workload a prompt is about, from its
// inventory line; "" when none of them appears.
func subject(user string) string {
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		if strings.Contains(user, "- shop/"+name+" (Deployment):") {
			return name
		}
	}
	return ""
}

// oneRow is a verdict contract v1 reply with one row and a summary that
// must never render.
func oneRow(workload, cause, rationale string) string {
	b, err := json.Marshal(verdictDoc{
		Verdicts: []verdictRow{{Workload: workload, Cause: cause, Confidence: "medium", Rationale: rationale}},
		Summary:  "model summary text",
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ownRow answers each prompt with a row for the workload it is about.
func ownRow(_ int, user string) verdictReply {
	n := subject(user)
	return verdictReply{Content: oneRow("shop/"+n, n+"-cause", n+"-why")}
}

// pvcTrace is one attributed PVC candidate on claim.
func pvcTrace(claim string) []inventory.Hypothesis {
	return []inventory.Hypothesis{{Cause: "PVC " + claim + " (ProvisioningFailed)", Kind: "pvc", Object: claim,
		Verdict: inventory.VerdictAttributed, Reason: "its pods mount it"}}
}

// refutedWL is a flagged workload whose only candidate is its PVC
// <name>-data. With boundPVC(<name>-data) in the clientset the rules refute
// it, so the workload is undecided and gets a model call.
func refutedWL(name string) inventory.Workload {
	w := gatherWL("shop", name)
	w.RootCauseTrace = pvcTrace(name + "-data")
	return w
}

// decidedWL is a flagged workload on worker-1. With notReadyNode("worker-1")
// in the clientset the rules confirm it, so it gets no model call.
func decidedWL(name string) inventory.Workload {
	w := gatherWL("shop", name)
	w.RootCauseTrace = []inventory.Hypothesis{{Cause: "node worker-1 (NotReady)", Kind: "node", Object: "worker-1",
		Verdict: inventory.VerdictAttributed, Reason: "its pod is scheduled on it"}}
	return w
}

// boundPVC is a Bound claim in shop.
func boundPVC(claim string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: claim},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
}

func modelRowFor(n string) string {
	return "- shop/" + n + ": " + n + "-cause [model, confidence: medium] — " + n + "-why"
}

const alphaRule = "- shop/alpha: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now"

func TestLocalInvestigateOneCallPerUndecidedWorkload(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	names := []string{"alpha", "bravo", "charlie"}
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo"), refutedWL("charlie")}
	client := fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data"), boundPVC("charlie-data"))
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws, client)
	if err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 3 {
		t.Fatalf("want one request per undecided workload, got %d", len(got))
	}
	for i, name := range names {
		u := got[i].User
		if subject(u) != name {
			t.Errorf("request %d is about %q, want %q (report order)", i, subject(u), name)
		}
		head := u[strings.Index(u, "== BEGIN inventory =="):strings.Index(u, "== END candidates ==")]
		for _, other := range names {
			if other != name && strings.Contains(head, "shop/"+other) {
				t.Errorf("%s's inventory and candidates name %s:\n%s", name, other, head)
			}
		}
		if !strings.Contains(u, "== events shop/"+name+" ==\n") {
			t.Errorf("%s's prompt lacks its own events read:\n%s", name, u)
		}
		if !strings.Contains(rep.Narrative, modelRowFor(name)) {
			t.Errorf("narrative missing %s's row:\n%s", name, rep.Narrative)
		}
	}
	if strings.Contains(got[0].User, "events shop/bravo") || strings.Contains(got[0].User, "events shop/charlie") {
		t.Errorf("alpha's prompt carries another workload's evidence:\n%s", got[0].User)
	}
	if rep.Notice != "" {
		t.Errorf("no call failed, Notice = %q", rep.Notice)
	}
}

func TestLocalInvestigateNoCallForRuleDecidedWorkload(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	ws := []inventory.Workload{decidedWL("alpha"), refutedWL("bravo"), refutedWL("charlie")}
	client := fake.NewSimpleClientset(notReadyNode("worker-1"), boundPVC("bravo-data"), boundPVC("charlie-data"))
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws, client)
	if err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || subject(got[0].User) != "bravo" || subject(got[1].User) != "charlie" {
		t.Fatalf("want calls for bravo and charlie only, got %d calls", len(got))
	}
	if !strings.Contains(rep.Narrative, alphaRule) {
		t.Errorf("alpha's rule row must carry the rule's evidence sentence:\n%s", rep.Narrative)
	}
}

func TestLocalInvestigateAllDecidedMakesNoCall(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	ws := []inventory.Workload{decidedWL("alpha"), decidedWL("bravo")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(notReadyNode("worker-1")))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("every workload is rule-decided: want 0 requests, got %d", n)
	}
	want := "Root-cause verdicts:\n" + alphaRule + "\n" +
		"- shop/bravo: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now\n\n" +
		"2 workloads share one upstream cause: node worker-1 (NotReady)"
	if rep.Narrative != want {
		t.Errorf("got:\n%s\nwant:\n%s", rep.Narrative, want)
	}
	if rep.Notice != "" || rep.Truncated {
		t.Errorf("no call: no Notice, no Truncated: %+v", rep)
	}
}

func TestLocalInvestigateDegradedWithNoWorkloadMakesNoCall(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, nil, fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(calls()); n != 0 {
		t.Errorf("no flagged workload: want 0 requests, got %d", n)
	}
	if rep.Narrative != "" || len(rep.Consulted) != 0 || rep.Notice != "" || rep.Truncated {
		t.Errorf("want an empty report, got %+v", rep)
	}
}

func TestLocalInvestigateRowIsPinnedToTheAskedWorkload(t *testing.T) {
	for _, written := range []string{"alpha", "shop/somebody-else", ""} {
		t.Run("model wrote "+written, func(t *testing.T) {
			url, _ := verdictEndpoint(t, func(int, string) verdictReply {
				return verdictReply{Content: oneRow(written, "c", "r")}
			})
			rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil,
				[]inventory.Workload{refutedWL("alpha")}, fake.NewSimpleClientset(boundPVC("alpha-data")))
			if err != nil {
				t.Fatal(err)
			}
			if want := "Root-cause verdicts:\n- shop/alpha: c [model, confidence: medium] — r"; rep.Narrative != want {
				t.Errorf("got:\n%s\nwant:\n%s", rep.Narrative, want)
			}
		})
	}
}

func TestLocalInvestigateExtraRowsAreIgnored(t *testing.T) {
	url, _ := verdictEndpoint(t, func(i int, user string) verdictReply {
		if subject(user) == "alpha" {
			return verdictReply{Content: `{"verdicts":[` +
				`{"workload":"shop/alpha","cause":"alpha-cause","confidence":"medium","rationale":"alpha-why"},` +
				`{"workload":"shop/bravo","cause":"hijack","confidence":"high","rationale":"planted"}],"summary":""}`}
		}
		return ownRow(i, user)
	})
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data")))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rep.Narrative, "hijack") || strings.Contains(rep.Narrative, "planted") {
		t.Errorf("a second row in alpha's reply must be ignored:\n%s", rep.Narrative)
	}
	if strings.Count(rep.Narrative, "- shop/bravo:") != 1 || !strings.Contains(rep.Narrative, modelRowFor("bravo")) {
		t.Errorf("bravo's row comes from bravo's own call:\n%s", rep.Narrative)
	}
}

func TestLocalInvestigateNarrativeHasNoModelSummary(t *testing.T) {
	url, _ := verdictEndpoint(t, ownRow)
	ws := []inventory.Workload{decidedWL("alpha"), decidedWL("bravo"), refutedWL("charlie")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(notReadyNode("worker-1"), boundPVC("charlie-data")))
	if err != nil {
		t.Fatal(err)
	}
	want := "Root-cause verdicts:\n" + alphaRule + "\n" +
		"- shop/bravo: node worker-1 (NotReady) [rule, confirmed] — Ready condition is False now\n" +
		modelRowFor("charlie") + "\n\n" +
		"2 workloads share one upstream cause: node worker-1 (NotReady)"
	if rep.Narrative != want {
		t.Errorf("got:\n%s\nwant:\n%s", rep.Narrative, want)
	}
}

func TestLocalInvestigatePartialFailureSetsNotice(t *testing.T) {
	url, calls := verdictEndpoint(t, func(i int, user string) verdictReply {
		if subject(user) == "bravo" {
			return verdictReply{Status: http.StatusInternalServerError, Content: "upstream is down"}
		}
		return ownRow(i, user)
	})
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo"), refutedWL("charlie")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data"), boundPVC("charlie-data")))
	if err != nil {
		t.Fatalf("a partial failure is not an error, got %v", err)
	}
	if n := len(calls()); n != 3 {
		t.Errorf("a failed call costs only its own workload: want 3 requests, got %d", n)
	}
	if !strings.Contains(rep.Narrative, modelRowFor("alpha")) || !strings.Contains(rep.Narrative, modelRowFor("charlie")) {
		t.Errorf("alpha's and charlie's rows must render:\n%s", rep.Narrative)
	}
	if strings.Contains(rep.Narrative, "shop/bravo") {
		t.Errorf("bravo got no verdict and has no row:\n%s", rep.Narrative)
	}
	want := "model gave no verdict for 1 of 3 workloads: local investigate endpoint returned 500: upstream is down"
	if rep.Notice != want {
		t.Errorf("Notice = %q, want %q", rep.Notice, want)
	}
}

func TestLocalInvestigateEveryCallFailedReturnsRulesOnly(t *testing.T) {
	url, calls := verdictEndpoint(t, func(int, string) verdictReply {
		return verdictReply{Status: http.StatusInternalServerError, Content: "upstream is down"}
	})
	ws := []inventory.Workload{decidedWL("alpha"), refutedWL("bravo"), refutedWL("charlie")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(notReadyNode("worker-1"), boundPVC("bravo-data"), boundPVC("charlie-data")))
	if err == nil || !strings.HasPrefix(err.Error(), "investigating: ") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("every call failed: want the first failure as the error, got %v", err)
	}
	if n := len(calls()); n != 2 {
		t.Errorf("want 2 requests, got %d", n)
	}
	if want := "Root-cause verdicts:\n" + alphaRule; rep.Narrative != want {
		t.Errorf("the rules-only report must ride with the error:\ngot:\n%s\nwant:\n%s", rep.Narrative, want)
	}
	if len(rep.Consulted) == 0 {
		t.Errorf("the rules-only report must keep the evidence trail")
	}
	if rep.Notice != "" || rep.Truncated {
		t.Errorf("the error path sets neither Notice nor Truncated: %+v", rep)
	}
}

func TestLocalInvestigate400DropsResponseFormatForTheRun(t *testing.T) {
	url, calls := verdictEndpoint(t, func(i int, user string) verdictReply {
		if i == 0 {
			return verdictReply{Status: http.StatusBadRequest, Content: `{"error":"response_format is not supported"}`}
		}
		return ownRow(i, user)
	})
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data")))
	if err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 3 {
		t.Fatalf("want 3 requests (alpha, alpha retried, bravo), got %d", len(got))
	}
	for i, want := range []struct {
		subject    string
		withFormat bool
	}{{"alpha", true}, {"alpha", false}, {"bravo", false}} {
		if subject(got[i].User) != want.subject || got[i].WithFormat != want.withFormat {
			t.Errorf("request %d: subject %q withFormat %v, want %q %v",
				i, subject(got[i].User), got[i].WithFormat, want.subject, want.withFormat)
		}
	}
	if !strings.Contains(rep.Narrative, modelRowFor("alpha")) || !strings.Contains(rep.Narrative, modelRowFor("bravo")) {
		t.Errorf("both rows must render:\n%s", rep.Narrative)
	}
}

func TestLocalInvestigateDeadlineStopsFurtherCalls(t *testing.T) {
	url, calls := verdictEndpoint(t, func(int, string) verdictReply { return verdictReply{Hang: true} })
	ws := []inventory.Workload{gatherWL("shop", "alpha"), gatherWL("shop", "bravo"), gatherWL("shop", "charlie")}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rep, err := NewLocal(url, "tiny-model", "").Investigate(ctx, degraded(), nil, nil, nil, ws, fake.NewSimpleClientset())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the deadline as the error, got %v", err)
	}
	if n := len(calls()); n != 1 {
		t.Errorf("after the deadline no further request is sent: want 1, got %d", n)
	}
	if rep.Narrative != "" || rep.Notice != "" {
		t.Errorf("no rule decided anything and every call failed: want an empty report, got %+v", rep)
	}
}

func TestLocalInvestigateSharedPVCDescribedOnceSeenByBoth(t *testing.T) {
	url, calls := verdictEndpoint(t, ownRow)
	alpha, bravo := gatherWL("shop", "alpha"), gatherWL("shop", "bravo")
	alpha.RootCauseTrace, bravo.RootCauseTrace = pvcTrace("shared-data"), pvcTrace("shared-data")
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil,
		[]inventory.Workload{alpha, bravo}, fake.NewSimpleClientset(boundPVC("shared-data")))
	if err != nil {
		t.Fatal(err)
	}
	describes := 0
	for _, l := range rep.Consulted {
		if l == "describe pvc shop/shared-data" {
			describes++
		}
	}
	if describes != 1 {
		t.Errorf("the shared claim is read once, trail: %v", rep.Consulted)
	}
	got := calls()
	if len(got) != 2 {
		t.Fatalf("want 2 requests, got %d", len(got))
	}
	for _, c := range got {
		if !strings.Contains(c.User, "== describe pvc shop/shared-data ==\n") {
			t.Errorf("%s's prompt lacks the shared describe:\n%s", subject(c.User), c.User)
		}
	}
}

func TestLocalInvestigateAnyTruncatedReplySetsTruncated(t *testing.T) {
	url, _ := verdictEndpoint(t, func(i int, user string) verdictReply {
		rep := ownRow(i, user)
		if subject(user) == "bravo" {
			rep.Finish = "length"
		}
		return rep
	})
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data")))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Truncated {
		t.Errorf("one reply cut at the length ceiling must set Truncated")
	}
}

// Review Focus 1: a dropped connection is a *url.Error carrying the whole
// endpoint URL; the notice must not print its path.
func TestLocalInvestigateNoticeCarriesNoEndpointPath(t *testing.T) {
	url, _ := verdictEndpoint(t, func(i int, user string) verdictReply {
		if subject(user) == "bravo" {
			return verdictReply{Drop: true}
		}
		return ownRow(i, user)
	})
	ws := []inventory.Workload{refutedWL("alpha"), refutedWL("bravo")}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws,
		fake.NewSimpleClientset(boundPVC("alpha-data"), boundPVC("bravo-data")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rep.Notice, "model gave no verdict for 1 of 2 workloads: ") {
		t.Errorf("Notice = %q", rep.Notice)
	}
	if strings.Contains(rep.Notice, "/chat/completions") {
		t.Errorf("the notice must not carry the endpoint path: %q", rep.Notice)
	}
	if !strings.Contains(rep.Narrative, modelRowFor("alpha")) {
		t.Errorf("alpha's row must still render:\n%s", rep.Narrative)
	}
}

// Review Focus 2: the deadline lands during the second of three calls.
func TestLocalInvestigateDeadlineMidRunKeepsEarlierRows(t *testing.T) {
	url, calls := verdictEndpoint(t, func(i int, user string) verdictReply {
		if subject(user) == "bravo" {
			return verdictReply{Hang: true}
		}
		return ownRow(i, user)
	})
	ws := []inventory.Workload{gatherWL("shop", "alpha"), gatherWL("shop", "bravo"), gatherWL("shop", "charlie")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rep, err := NewLocal(url, "tiny-model", "").Investigate(ctx, degraded(), nil, nil, nil, ws, fake.NewSimpleClientset())
	if err != nil {
		t.Fatalf("one row came back, so this is a partial failure, got %v", err)
	}
	if n := len(calls()); n != 2 {
		t.Errorf("charlie is skipped after the deadline: want 2 requests, got %d", n)
	}
	if !strings.Contains(rep.Narrative, modelRowFor("alpha")) {
		t.Errorf("alpha's row must render:\n%s", rep.Narrative)
	}
	if !strings.Contains(rep.Notice, "2 of 3") || !strings.Contains(rep.Notice, "context deadline exceeded") {
		t.Errorf("Notice = %q", rep.Notice)
	}
}

// Review Focus 3: a cause that sanitizes to nothing is no verdict.
func TestLocalInvestigateBlankCauseIsNoVerdict(t *testing.T) {
	for _, cause := range []string{"   ", "\a\u200b"} {
		t.Run("cause "+strings.ToValidUTF8(cause, "?"), func(t *testing.T) {
			url, _ := verdictEndpoint(t, func(i int, user string) verdictReply {
				if subject(user) == "alpha" {
					return verdictReply{Content: oneRow("shop/alpha", cause, "r")}
				}
				return ownRow(i, user)
			})
			ws := []inventory.Workload{gatherWL("shop", "alpha"), gatherWL("shop", "bravo")}
			rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws, fake.NewSimpleClientset())
			if err != nil {
				t.Fatal(err)
			}
			if want := "model gave no verdict for 1 of 2 workloads: model returned no verdict"; rep.Notice != want {
				t.Errorf("Notice = %q, want %q", rep.Notice, want)
			}
			if strings.Contains(rep.Narrative, "shop/alpha") {
				t.Errorf("a blank cause must not render a row:\n%s", rep.Narrative)
			}
		})
	}
}

// Review Focus 5: workloads 9 and 10 are past the read budget but still
// get a call, with an empty evidence section.
func TestLocalInvestigateTenWorkloadsPastTheReadBudget(t *testing.T) {
	url, calls := verdictEndpoint(t, func(int, string) verdictReply {
		return verdictReply{Content: oneRow("x", "c", "r")}
	})
	var ws []inventory.Workload
	for i := 0; i < maxGatherWorkloads; i++ {
		ws = append(ws, gatherWL("shop", "web-0"+string(rune('0'+i))))
	}
	rep, err := NewLocal(url, "tiny-model", "").Investigate(context.Background(), degraded(), nil, nil, nil, ws, fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != maxGatherWorkloads {
		t.Fatalf("want %d requests, got %d", maxGatherWorkloads, len(got))
	}
	none := "== BEGIN evidence ==\n(none)\n== END evidence =="
	if strings.Contains(got[7].User, none) {
		t.Errorf("the 8th workload's events read is within budget")
	}
	if !strings.Contains(got[8].User, none) || !strings.Contains(got[9].User, none) {
		t.Errorf("workloads past the budget must show (none) as evidence")
	}
	if n := strings.Count(rep.Narrative, "[model, "); n != maxGatherWorkloads {
		t.Errorf("want %d model rows, got %d:\n%s", maxGatherWorkloads, n, rep.Narrative)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin; GOFLAGS=-mod=mod go test ./internal/investigate`
Expected: FAIL — build errors naming `buildVerdictPrompt` (wrong argument count/types), `renderVerdicts` (wrong argument types), `rep.Notice` (no field) and `errNoVerdict` (undefined).

- [ ] **Step 4: Add `Report.Notice`**

In `internal/investigate/investigate.go`, replace the `Report` struct with:

```go
type Report struct {
	Consulted []string // evidence trail: the reads that were made
	Narrative string   // the Fix-first explanation, grounded in the evidence
	// Truncated is true when Narrative was cut short at the model's own
	// output-length ceiling rather than because the model chose to stop.
	// Text-only: it renders, it is never marshalled to JSON (report.Input
	// carries it the same way).
	Truncated bool
	// Notice is set only by local verdict mode, when some of its per-workload
	// model calls failed and at least one succeeded: it says how many
	// workloads got no verdict and why the first one failed. The CLI prints
	// it on stderr. Text-only like Truncated: never marshalled to JSON. The
	// tool loop never sets it.
	Notice string
}
```

Keep the doc comment above the struct unchanged.

- [ ] **Step 5: Rework `local.go`**

In `internal/investigate/local.go`:

Replace the import block with:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/hypothesis"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/safetext"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"k8s.io/client-go/kubernetes"
)
```

Replace `buildVerdictPrompt` and its doc comment with:

```go
// buildVerdictPrompt assembles the user message for one workload: the
// inventory with that workload alone and only the service issues in its
// namespace (its --explain closing instruction stripped — the contract here
// is JSON verdicts, not prose), the workload's capped candidate trace with
// each fresh read's outcome and the rule decision beside it, and the
// workload's own evidence, each delimited. If the whole prompt still
// exceeds maxPromptBytes, the evidence — the only unbounded-in-principle
// section — is cut to fit, marked, and the sections reassembled so the
// delimiters stay closed.
func buildVerdictPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, w inventory.Workload, r hypothesis.Result, evidence string) string {
	var own []svchealth.Issue
	for _, is := range serviceIssues {
		if is.Namespace == w.Namespace {
			own = append(own, is)
		}
	}
	one := []inventory.Workload{w}
	inventorySection := strings.TrimSuffix(
		explain.BuildInventoryPrompt(cluster, summary, facts, capServiceIssues(own), one),
		"\nExplain each problem and its fix using the required structure.")
	candidates := renderCandidates(one, []hypothesis.Result{r})
	assemble := func(ev string) string {
		return section("inventory", inventorySection) +
			section("candidates", candidates) +
			section("evidence", ev) +
			"Judge each listed workload now and answer with the JSON object only."
	}
	prompt := assemble(evidence)
	if len(prompt) > maxPromptBytes {
		// section() trims the body's trailing newlines before wrapping it,
		// so the budget must be measured against the trimmed evidence — not
		// the raw one, which gatherEvidence's real output always ends with
		// two newlines, not one. The cut evidence below is "cut + \n +
		// truncationMarker + \n"; section() trims its own single trailing
		// "\n" back off, so it contributes len(cut) + 1 + len(truncationMarker)
		// bytes, which is what "- 1" accounts for.
		over := len(prompt) - maxPromptBytes
		trimmed := strings.TrimRight(evidence, "\n")
		keep := len(trimmed) - over - len(truncationMarker) - 1
		if keep < 0 {
			keep = 0
		}
		cut := trimmed[:keep]
		if i := strings.LastIndexByte(cut, '\n'); i > 0 {
			cut = cut[:i]
		}
		prompt = assemble(cut + "\n" + truncationMarker + "\n")
	}
	return prompt
}
```

Replace the output-bounds const block (`maxVerdictRows`, `maxSummaryLines`, `maxModelLineRunes`) with:

```go
// Output bounds for what model text may enter the report. Model output is
// untrusted: every string passes safetext.Line and a rune cap before it
// reaches a kubeagent value. maxSummaryLines caps the rules' shared-cause
// lines; hypothesis.MaxSharedLines must equal it
// (TestSharedCapsMatchTheSummaryCaps).
const (
	maxVerdictRows    = 10
	maxSummaryLines   = 4
	maxModelLineRunes = 512
)
```

Replace the `LocalClient` doc comment with:

```go
// LocalClient runs --investigate's local verdict mode: kubeagent gathers
// the evidence deterministically and a local OpenAI-compatible model
// adjudicates it, one call per workload the rules left undecided. No tool
// loop, no Anthropic dependency.
```

Replace `Investigate` (with its doc comment) and `call` (with its doc comment) with:

```go
// Investigate matches Client.Investigate's signature and skip rule. It
// gathers evidence under the tool loop's budget, lets the rules decide
// each candidate from the fresh reads, and asks the model once per scoped
// workload the rules left undecided — one workload per prompt, one call at
// a time, in report order, all under ctx. A rule-decided workload gets no
// call, and once ctx is done no further request is sent. Each model row is
// pinned to the workload its call was about. When some calls fail and at
// least one succeeds, the report renders the rows it has and Notice says
// how many workloads got no verdict. When every call fails, the first
// failure comes back as the error with the rules-only report: the trail,
// the rule rows and the shared lines. That report is empty when no rule
// decided anything.
func (c *LocalClient) Investigate(ctx context.Context, cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload, client kubernetes.Interface) (Report, error) {
	if cluster.Verdict != "Degraded" && len(workloads) == 0 && len(serviceIssues) == 0 {
		return Report{}, nil
	}
	scoped := flaggedScope(workloads)
	trail, evidence, reads := gatherEvidence(ctx, client, scoped)
	results := make([]hypothesis.Result, len(scoped))
	for i, w := range scoped {
		results[i] = hypothesis.Decide(w, reads)
	}
	shared := hypothesis.Shared(results)
	rulesOnly := Report{Consulted: trail, Narrative: renderVerdicts(nil, results, shared)}
	if rulesOnly.Narrative == "" {
		rulesOnly = Report{}
	}

	model := map[string]verdictRow{}
	withFormat := true
	var (
		undecided, failed int
		firstErr          error
		truncated         bool
	)
	for i, r := range results {
		if r.Decided {
			continue
		}
		undecided++
		err := ctx.Err()
		if err == nil {
			prompt := buildVerdictPrompt(cluster, summary, facts, serviceIssues, scoped[i], r, evidence[i])
			var (
				row verdictRow
				cut bool
			)
			row, cut, err = c.call(ctx, prompt, &withFormat)
			if err == nil {
				truncated = truncated || cut
				model[r.Workload] = row
				continue
			}
		}
		failed++
		if firstErr == nil {
			firstErr = err
		}
	}
	switch {
	case undecided == 0:
		return rulesOnly, nil
	case failed == undecided:
		return rulesOnly, fmt.Errorf("investigating: %w", firstErr)
	}
	rep := Report{Consulted: trail, Narrative: renderVerdicts(model, results, shared), Truncated: truncated}
	if failed > 0 {
		rep.Notice = fmt.Sprintf("model gave no verdict for %d of %d workloads: %s",
			failed, undecided, safetext.Line(redact.Error(firstErr)))
	}
	return rep, nil
}

// errNoVerdict is a reply that parsed but carried no usable row: no
// verdicts at all, or a first row whose cause is blank once sanitized.
var errNoVerdict = errors.New("model returned no verdict")

// call posts one workload's prompt and returns the first verdict row and
// whether the reply was cut at the length ceiling. The row's workload field
// is not read — the caller pins the row to the workload it asked about —
// and any further rows and the summary are ignored. When the endpoint 400s
// a request that carried response_format, the request is retried once
// without it and *withFormat is cleared, so every later call in the same
// run goes out without it from the start: some local servers reject the
// json_schema shape they do not implement.
func (c *LocalClient) call(ctx context.Context, prompt string, withFormat *bool) (verdictRow, bool, error) {
	doc, truncated, retry, err := c.post(ctx, prompt, *withFormat)
	if retry {
		*withFormat = false
		doc, truncated, _, err = c.post(ctx, prompt, false)
	}
	if err != nil {
		return verdictRow{}, false, err
	}
	if len(doc.Verdicts) == 0 || strings.TrimSpace(safetext.Line(capRunes(doc.Verdicts[0].Cause, maxModelLineRunes))) == "" {
		return verdictRow{}, false, errNoVerdict
	}
	return doc.Verdicts[0], truncated, nil
}
```

Replace `renderVerdicts`, `firstModelRows`, `modelRow` and `ruleRow` (each with its doc comment) with:

```go
// renderVerdicts builds the narrative: one row per scoped workload that has
// one, in report order — a rule row for a rule-decided workload, a model
// row for an undecided workload whose call returned one (model is keyed by
// "namespace/name") — at most maxVerdictRows, then the rules' shared
// lines. No model summary is rendered.
func renderVerdicts(model map[string]verdictRow, results []hypothesis.Result, shared []string) string {
	var rows []string
	for _, r := range results {
		if r.Decided {
			rows = append(rows, ruleRow(r))
			continue
		}
		if m, ok := model[r.Workload]; ok {
			rows = append(rows, modelRow(r.Workload, m))
		}
	}
	if len(rows) > maxVerdictRows {
		rows = rows[:maxVerdictRows]
	}
	var b strings.Builder
	if len(rows) > 0 {
		b.WriteString("Root-cause verdicts:\n")
		b.WriteString(strings.Join(rows, "\n"))
	}
	if len(shared) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.Join(shared, "\n"))
	}
	return b.String()
}

// modelRow renders one model-written row under name, kubeagent's own
// "namespace/name" for the workload the call was about — never the
// model's workload string. Cause and rationale are sanitized and capped;
// confidence is normalized to the closed vocabulary.
func modelRow(name string, v verdictRow) string {
	conf := v.Confidence
	switch conf {
	case "low", "medium", "high":
	default:
		conf = "unstated"
	}
	return fmt.Sprintf("- %s: %s [model, confidence: %s] — %s",
		name, safetext.Line(capRunes(v.Cause, maxModelLineRunes)), conf,
		safetext.Line(capRunes(v.Rationale, maxModelLineRunes)))
}

// ruleRow renders one rule-decided row: the trace's own cause text and the
// rule's evidence sentence. A rule-decided workload gets no model call, so
// nothing on this row comes from the model.
func ruleRow(r hypothesis.Result) string {
	return fmt.Sprintf("- %s: %s [rule, %s] — %s", r.Workload, r.Cause, r.Outcome, r.Evidence)
}
```

Delete `capSummary` and its doc comment (the last function in the file). In the doc comment of `capRunes`, nothing refers to `capSummary`; leave `capRunes` as it is.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin; GOFLAGS=-mod=mod go test ./internal/investigate -v -run 'TestLocalInvestigate|TestRenderVerdicts|TestBuildVerdictPrompt|TestVerdictSystemPrompt|TestCapServiceIssues'`
Expected: PASS, every test listed.

Run: `export PATH=$PATH:/usr/local/go/bin; GOFLAGS=-mod=mod go vet ./internal/investigate && GOFLAGS=-mod=mod go test -race -count=3 ./internal/investigate`
Expected: no vet output; `ok` three times over with the race detector (the endpoint handler and the deadline tests run goroutines).

Run: `grep -n 'firstModelRows\|capSummary\|bundle' internal/investigate/*.go | grep -v '_test.go'`
Expected: no output (test names such as `TestBuildVerdictPromptDefensiveCapRealisticBundleShape` keep their names).

- [ ] **Step 7: Commit**

```bash
git add internal/investigate/local.go internal/investigate/investigate.go internal/investigate/local_test.go internal/investigate/local_calls_test.go
git -c user.name=imantaba -c user.email=itn.taba@gmail.com -c core.fileMode=false commit -s -m "feat(investigate): one local verdict call per undecided workload"
```

---

### Task 3: The CLI prints the partial-failure notice

**Files:**
- Modify: `internal/cli/enrichment.go:57-89` (`runModelPath` and its doc comment)
- Test: `internal/cli/enrichment_test.go` (`TestRunModelPath`)

**Interfaces:**
- Consumes (Task 2): `investigate.Report.Notice string` — non-empty only on a nil-error partial failure.
- Produces: `modelPathResult.notice == "--investigate: " + rep.Notice` with `investigation` kept. `scan.go` already prints `modelRes.notice` with `warnf` and needs no change.

- [ ] **Step 1: Write the failing test**

In `internal/cli/enrichment_test.go`, inside `TestRunModelPath`, add this subtest right after the subtest named `"investigate failure with rule-decided rows keeps the report and extends the notice"`:

```go
	t.Run("investigate partial failure keeps the report and prints the notice", func(t *testing.T) {
		rep := investigate.Report{
			Narrative: "Root-cause verdicts:\n- shop/web: none_of_these [model, confidence: low] — r",
			Consulted: []string{"events shop/web"},
			Notice:    "model gave no verdict for 1 of 3 workloads: local investigate endpoint returned 500: upstream is down",
		}
		res := runModelPath(scanOptions{investigate: true},
			func() (investigate.Report, error) { return rep, nil },
			func() (explain.Explanation, error) { return explain.Explanation{}, nil },
		)
		want := "--investigate: model gave no verdict for 1 of 3 workloads: local investigate endpoint returned 500: upstream is down"
		if res.notice != want {
			t.Errorf("notice = %q, want %q", res.notice, want)
		}
		if res.investigation.Narrative != rep.Narrative {
			t.Errorf("investigation.Narrative = %q, want the rows kept", res.investigation.Narrative)
		}
		if strings.Contains(res.notice, "; rule-decided") {
			t.Errorf("notice = %q, a partial failure is not the model-absent case", res.notice)
		}
	})
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin; GOFLAGS=-mod=mod go test ./internal/cli -run 'TestRunModelPath' -v`
Expected: FAIL — `notice = "", want "--investigate: model gave no verdict for 1 of 3 workloads: ..."`.

- [ ] **Step 3: Implement**

In `internal/cli/enrichment.go`, replace the doc comment of `runModelPath` and its `case o.investigate:` arm with:

```go
// runModelPath runs whichever model-enrichment arm is selected —
// --investigate supersedes --explain, matching the flag's own description —
// and reduces a failure to one notice line instead of aborting the run. When
// the failed call still returns a report with a narrative (local verdict
// mode's rule-decided rows), the report is kept and the notice says the
// model was absent. When local verdict mode succeeds but some of its
// per-workload calls failed, the report is kept and its Notice becomes the
// notice line.
// investigateFn and explainFn are the actual calls, injected so this is
// testable with no network and no cluster.
func runModelPath(o scanOptions, investigateFn func() (investigate.Report, error), explainFn func() (explain.Explanation, error)) modelPathResult {
	switch {
	case o.investigate:
		rep, err := investigateFn()
		if err != nil {
			notice := fmt.Sprintf("--investigate: %s", enrichmentFailure(err))
			if rep.Narrative != "" {
				// Local verdict mode returns the rule-decided rows beside its
				// error; they render, and the notice says the model was absent.
				notice += "; rule-decided verdicts rendered without the model"
				return modelPathResult{investigation: rep, notice: notice}
			}
			return modelPathResult{notice: notice}
		}
		if rep.Notice != "" {
			// Local verdict mode: some per-workload calls failed, the rows
			// that came back render, and the notice says how many did not.
			return modelPathResult{investigation: rep, notice: "--investigate: " + rep.Notice}
		}
		return modelPathResult{investigation: rep}
```

The `case o.explain:` arm and the rest of the function do not change.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin; GOFLAGS=-mod=mod go test ./internal/cli -run 'TestRunModelPath' -v`
Expected: PASS, every subtest.

Run: `export PATH=$PATH:/usr/local/go/bin; GOFLAGS=-mod=mod go test ./...`
Expected: every package `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/enrichment.go internal/cli/enrichment_test.go
git -c user.name=imantaba -c user.email=itn.taba@gmail.com -c core.fileMode=false commit -s -m "feat(cli): print local verdict mode's partial-failure notice"
```

---

### Task 4: Docs

**Files:**
- Modify: `website/docs/features/diagnostics.md` (section "Local verdict mode", lines ~1372–1525)
- Modify: `CLAUDE.md:613-614`
- Modify: `website/docs/roadmap.md:572` (the long "post-1.0" table row)
- Modify: `CHANGELOG.md` (`## [Unreleased]`)

**Interfaces:**
- Consumes: the behavior from Tasks 1–3. No code.
- Produces: nothing a later task uses.

- [ ] **Step 1: Find every sentence that still describes the old shape**

Run: `grep -rn "exactly one call\|asks once\|exactly one\s*$\|4-line summary\|model's summary\|outside the\s*$\|outside the scope\|model returned no text\|one local OpenAI-compatible model call" website/docs CLAUDE.md skills commands README.md`
Expected: hits in `website/docs/features/diagnostics.md`, `CLAUDE.md:613` and `website/docs/roadmap.md:572`. If `skills/`, `commands/` or `README.md` hit too, fix those sentences the same way as below.

- [ ] **Step 2: Edit `website/docs/features/diagnostics.md`, "Local verdict mode"**

Replace the intro paragraph (from "In the Anthropic tool-use loop above" to "then asks once.") with:

```markdown
In the Anthropic tool-use loop above, the model chooses which reads to make.
Local verdict mode inverts that: kubeagent chooses the reads, and the model
does nothing but adjudicate them. It calls `/chat/completions` on the
OpenAI-compatible endpoint `KUBEAGENT_EXPLAIN_ENDPOINT` names — the same
endpoint and `KUBEAGENT_EXPLAIN_API_KEY` bearer key `--explain`'s local path
already uses — once per flagged workload the rules did not decide: at most
10 calls, one at a time, in report order. Each prompt holds one workload,
built from three delimited sections (inventory, candidates, evidence), and
each reply is read back as JSON. There is no tool loop and no back-and-forth:
kubeagent gathers, decides what the rules can, then asks about each
workload that is left.
```

In the "The deterministic gather." paragraph, replace

```markdown
names a node or a PVC, deduplicated across workloads (a registry candidate
has nothing to read); and, for each crash-family finding, a classified
```

with

```markdown
names a node or a PVC (a registry candidate has nothing to read); and, for
each crash-family finding, a classified
```

and add these sentences at the end of that paragraph, after "the same label format the tool loop's trail uses.":

```markdown
Each workload gets its own evidence: its own reads, and nothing read for
another workload. A node or PVC shared by two workloads is read once, and
its text goes into both workloads' evidence at no extra budget cost.
```

At the end of the "Rules decide the candidates." paragraph, add: `A workload the rules decided gets no model call.`

Replace the paragraph right after the verdict contract's JSON block (from "`cause` is a candidate's text verbatim" to "the eight `schemaVersion` surfaces.") with:

```markdown
`cause` is a candidate's text verbatim, `none_of_these`, or a cause the model
grounds in its own reading of the evidence; `confidence` is `low`, `medium` or
`high` — anything else renders as `unstated`. Only the first row of a reply is
used, and it is placed on the workload the call was about: the `workload`
field is part of the shape but is not used to place the row, and any further
rows are ignored. `summary` is part of the shape too and is not rendered. A
reply with no row, or whose first row has a blank cause, counts as no
verdict for that workload. A rule row's cause never comes from the model.
This contract is **prose, versioned in this document** — it crosses the model
boundary, not kubeagent's own JSON output, so it is deliberately not one of
the eight `schemaVersion` surfaces.
```

Replace the paragraph right after the three-row example block (from "A `[rule, …]` row's cause is the candidate text" to "in the model's order.") with:

```markdown
A `[rule, …]` row's cause is the candidate text from the scan, never the
model's, and its rationale is the rule's own evidence sentence. A `[model, …]`
row is the model's answer for a workload the rules could not decide, with the
cause, confidence and rationale sanitized and capped. Rows come in report
order: a rule row when a rule decided, a model row when it did not and the
call returned one. A flagged workload beyond the gather's 10 gets no row: it
had no evidence, and the model could only guess from its inventory line.
```

In "The shared line." paragraph, replace `render, and they come before the model's summary.` with `render. No model summary follows them.`

In the size-bounds table, replace the row `| Shared-cause summary lines | 4 |` with `| Shared-cause lines | 4 |`, and replace `| Whole prompt | 64 KiB |` with `| Whole prompt (one per workload) | 64 KiB |`.

Replace the "Injection posture." paragraph with:

```markdown
**Injection posture.** Evidence is untrusted cluster data, carried inside
the prompt's section delimiters; the system prompt pins that an instruction
found inside evidence is never followed. Model output is itself untrusted:
every string is sanitized and length-capped before it can enter the report,
and a reply can only ever place a row on the workload its call was about —
a row naming another workload is ignored.
```

Replace the "Structured output." paragraph's first sentence ("The request asks for … gets exactly one retry without it.") with:

```markdown
The request asks for `response_format: json_schema` matching verdict contract
v1. The first call an endpoint rejects with a 400 is retried once without it,
and every later call in the same run is sent without it from the start.
```

Keep the rest of that paragraph ("Parsing is lenient either way: …").

In the "Model guidance." paragraph, replace `window of 32k tokens and treat 16k as the floor: the prompt alone may approach the 64 KiB bound above.` with:

```markdown
window of 32k tokens and treat 16k as the floor: one prompt may approach the
64 KiB bound above. A prompt holds one workload: in a live chaos run, a small
model tuned to this contract found the right cause for 1 of 45 workloads when
several shared one prompt, and for 6 of 11 when it was asked about one at a
time.
```

Replace the "When the model fails." paragraph with:

```markdown
**When the model fails.** A call fails when the request errors, the endpoint
returns a non-2xx status, the reply does not parse, or the reply has no
usable row; once the 300-second budget runs out, the workloads not yet asked
count as failed too and get no request. When some calls fail and at least
one returns a row, the section renders with the rows it has and stderr reads
`--investigate: model gave no verdict for <n> of <m> workloads: <reason>`,
where `<m>` counts the workloads the rules left undecided and `<reason>` is
the first failure. When every call fails, the rule rows and the shared lines
still render, and the notice reads
`--investigate: <reason>; rule-decided verdicts rendered without the model`.
When no rule decided anything either, there is no section, and the notice
carries the reason alone.
```

- [ ] **Step 3: Edit `CLAUDE.md`**

In the roadmap bullet about v1.23.0, replace

```markdown
  cluster connection. The mode inverts the tool loop: kubeagent chooses
  every read and the model only adjudicates, in exactly one
  `/chat/completions` call to the same endpoint and bearer key `--explain`'s
  local path already uses.
```

with

```markdown
  cluster connection. The mode inverts the tool loop: kubeagent chooses
  every read and the model only adjudicates, in one `/chat/completions`
  call per workload the rules left undecided (at most 10, one at a time,
  one workload per prompt, the model's summary not rendered) to the same
  endpoint and bearer key `--explain`'s local path already uses.
```

- [ ] **Step 4: Edit `website/docs/roadmap.md`**

In the post-1.0 table row, replace

```markdown
and one local OpenAI-compatible model call adjudicates the root-cause candidates, versioned as verdict contract v1
```

with

```markdown
and a local OpenAI-compatible model adjudicates the root-cause candidates of each workload the rules left undecided, one call per workload, versioned as verdict contract v1
```

- [ ] **Step 5: Edit `CHANGELOG.md`**

Under `## [Unreleased]`, insert before `### Fixed`:

```markdown
### Changed

- `--investigate`'s local verdict mode now asks the model once per flagged
  workload the rules did not decide — at most 10 calls, one at a time —
  instead of once for all of them. Each prompt holds one workload and only
  its own evidence. In a live chaos run, a small model tuned for this mode
  found the right cause for 1 of 45 workloads when several shared one
  prompt, and for 6 of 11 when asked about one at a time. The model's
  summary is no longer rendered; the shared-cause lines still come from the
  rules. When some calls fail, the rows that came back still render and
  stderr says `--investigate: model gave no verdict for <n> of <m>
  workloads: <reason>`.

```

- [ ] **Step 6: Verify**

Run: `grep -rn "exactly one call\|asks once\|4-line summary\|before the model's summary\|outside the scope follow\|one local OpenAI-compatible model call" website/docs CLAUDE.md skills commands README.md`
Expected: no output.

Run: `export PATH=$PATH:/usr/local/go/bin; GOFLAGS=-mod=mod go test ./...`
Expected: every package `ok` (`plugin_manifest_test.go` and `internal/cli/plugin_flags_test.go` read shipped skill text, so a docs edit there must not break them).

- [ ] **Step 7: Commit**

```bash
git add website/docs/features/diagnostics.md CLAUDE.md website/docs/roadmap.md CHANGELOG.md
git -c user.name=imantaba -c user.email=itn.taba@gmail.com -c core.fileMode=false commit -s -m "docs: local verdict mode asks once per undecided workload"
```

If Step 1 found and fixed sentences in `skills/`, `commands/` or `README.md`, add those files to the same `git add`.
