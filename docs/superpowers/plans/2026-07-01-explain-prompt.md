# `--explain` Prompt Improvement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `--explain` output consistently structured and facts-grounded by replacing the system prompt (senior-SRE persona + per-issue structure + anti-hallucination guardrail), lightly labeling the facts P1/P2, and raising the token budget.

**Architecture:** All changes are in `internal/explain/explain.go` (the `systemPrompt` const, `buildInventoryPrompt` framing, and `MaxTokens`), unit-tested by asserting prompt content (the model output is not unit-testable). Docs get a short note.

**Tech Stack:** Go 1.26, the Anthropic Go SDK (already present). No new dependency.

## Global Constraints

- `--explain` stays **opt-in and read-only**; it sends only **structured facts** — never raw pod specs, pod IPs, env values, or secrets. The existing egress-guard test (no pod IPs / node names in the prompt) must still pass.
- `--explain` is **entirely separate from `--fix`** — no coupling, no action context to the model.
- Only `internal/explain` + a README/CHANGELOG note change. No new Go module dependency.
- Go at `/usr/local/go/bin` (`export PATH=$PATH:/usr/local/go/bin`).

---

### Task 1: system prompt + P1/P2 facts framing + token budget

**Files:**
- Modify: `internal/explain/explain.go`
- Test: `internal/explain/explain_test.go`

**Interfaces:**
- Produces: the rewritten `systemPrompt` const; `buildInventoryPrompt` framing labels (`(P1)` / `Workload problems (P2):`); `MaxTokens = 2048`.

- [ ] **Step 1: Write the failing tests**

In `internal/explain/explain_test.go` (it already imports `strings`, `clusterhealth`, `inventory`, `diagnose`), add:

```go
func TestSystemPrompt_HasStructureAndGuardrail(t *testing.T) {
	for _, want := range []string{"Root cause", "Check", "Fix", "P1", "P2", "ONLY the facts", "do not invent"} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("systemPrompt missing %q", want)
		}
	}
}

func TestBuildInventoryPrompt_LabelsPriority(t *testing.T) {
	ch := clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 2, NodesReady: 1, NodeIssues: []string{"n2 NotReady"}}
	ws := []inventory.Workload{{Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 1, Status: "Degraded"}}
	got := buildInventoryPrompt(ch, nil, nil, nil, ws)
	if !strings.Contains(got, "(P1)") {
		t.Errorf("degraded cluster block should be labeled P1:\n%s", got)
	}
	if !strings.Contains(got, "Workload problems (P2):") {
		t.Errorf("workload block should be labeled P2:\n%s", got)
	}
}
```

Also update the existing `TestBuildInventoryPrompt_LeadsWithDegradedCluster`: its check `if strings.Contains(got, "need attention")` (asserting no workload section when there are none) must reference the new label. Change that line to:

```go
	if strings.Contains(got, "Workload problems") {
		t.Errorf("should not advertise a workloads section when there are none:\n%s", got)
	}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/`
Expected: FAIL — `systemPrompt` lacks the new directives / prompt lacks the `(P1)` / `Workload problems (P2):` labels.

- [ ] **Step 3: Implement**

In `internal/explain/explain.go`, replace the `systemPrompt` const with:

```go
const systemPrompt = `You are a senior Kubernetes SRE reviewing a read-only cluster scan. Explain what
is wrong and exactly how to fix it, using ONLY the facts provided — do not invent
causes, resources, or values that are not given.

Address issues in priority order: cluster / kube-system problems (P1) before
workload problems (P2). For EACH issue use this structure:

**<namespace/name> — <the issue>**
- Root cause: one line, from the facts. If the facts are ambiguous, name the most
  likely cause AND what to check — never present a guess as certain.
- Check: 1–3 read-only commands to confirm (kubectl get/describe/logs).
- Fix: the exact command(s) or concrete change to resolve it.

Be tight — no preamble, no restating the input, no generic advice. If a finding
is expected (e.g. a scaled-to-zero workload), say it needs no action. Prefer
"likely"/"check" over false certainty.`
```

In `buildInventoryPrompt`, make three edits:

- The degraded-cluster header — change:
  ```go
  		fmt.Fprintf(&b, "Cluster health: DEGRADED — %d/%d nodes Ready.\n", cluster.NodesReady, cluster.NodesTotal)
  ```
  to:
  ```go
  		fmt.Fprintf(&b, "Cluster health (P1): DEGRADED — %d/%d nodes Ready.\n", cluster.NodesReady, cluster.NodesTotal)
  ```
- The workloads header — change:
  ```go
  		b.WriteString("These Kubernetes workloads need attention:\n\n")
  ```
  to:
  ```go
  		b.WriteString("Workload problems (P2):\n\n")
  ```
- The closing instruction — change:
  ```go
  	b.WriteString("\nExplain what is going wrong and suggest concrete next steps.")
  ```
  to:
  ```go
  	b.WriteString("\nExplain each problem and its fix using the required structure.")
  ```

In `anthropicSummarizer.summarize`, raise the token budget — change `MaxTokens: 1024,` to `MaxTokens: 2048,`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ -v && go build ./... && go vet ./internal/explain/ && gofmt -l internal/explain/`
Expected: all explain tests PASS (new + existing, including the egress-guard and skip-path tests), build succeeds, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/explain/
git commit -m "feat(explain): structured, facts-grounded prompt (P1/P2, root cause/check/fix, anti-hallucination)"
```

---

### Task 2: docs

**Files:**
- Modify: `README.md`, `CHANGELOG.md`

- [ ] **Step 1: `README.md` — note the structured output**

Read `README.md` and find the sentence introducing `--explain` (the "summarize the findings in plain English" area / the `--explain` privacy note block). Add one sentence describing the new behavior:

```markdown
The explanation is structured — per issue, a root cause, read-only checks, and an
exact fix, with cluster/kube-system problems (P1) before workloads (P2) — and is
grounded strictly in the scan's facts (the model is told not to invent causes).
```

Place it adjacent to the existing `--explain` description (do not alter the privacy note's substance).

- [ ] **Step 2: `CHANGELOG.md` — a `### Changed` entry under `## [Unreleased]`**

Under `## [Unreleased]`, add a `### Changed` section (after the existing `### Added`) with:

```markdown
### Changed

- **Sharper `--explain`.** The `--explain` prompt now instructs a consistent,
  scannable structure (per issue: root cause → checks → fix; cluster/kube-system
  problems before workloads) and is grounded strictly in the scan's facts (told
  not to invent causes), reducing misattributed root causes. Still opt-in,
  read-only, structured-facts-only, and independent of `--fix`.
```

- [ ] **Step 3: Build + commit**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: build succeeds, all packages `ok` (no code changed).

```bash
git add README.md CHANGELOG.md
git commit -m "docs(explain): note the structured, facts-grounded --explain output"
```

---

## Self-Review

**Spec coverage:**
- New system prompt (persona + structure + anti-hallucination) → Task 1. ✓
- `buildInventoryPrompt` P1/P2 labels + aligned closing → Task 1. ✓
- `MaxTokens` 1024 → 2048 → Task 1. ✓
- Tests: systemPrompt directives, P1/P2 labels, existing egress-guard + skip-path unaffected → Task 1 (+ the `LeadsWithDegradedCluster` assertion update). ✓
- Docs (README + CHANGELOG Changed) → Task 2. ✓
- Read-only / structured-facts-only / decoupled-from-`--fix` / no new dep (Global Constraints) → no data-collection or `--fix`/model changes; only prompt text + token count. ✓

**Placeholder scan:** none — complete strings/edits in every step.

**Type/name consistency:** `systemPrompt`, `buildInventoryPrompt`, the exact labels `(P1)` / `Workload problems (P2):`, and `MaxTokens: 2048` are used identically across the task and its tests. The guardrail phrases asserted in `TestSystemPrompt_HasStructureAndGuardrail` (`Root cause`, `Check`, `Fix`, `P1`, `P2`, `ONLY the facts`, `do not invent`) all appear verbatim in the new `systemPrompt`.
