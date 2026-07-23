# Ranked `--explain` remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ground `--explain`'s per-issue Fix on kubeagent's deterministic `remediation.For` command (never substitute/invent) and add a leading `Fix first:` ranked remediation list.

**Architecture:** Change is confined to `internal/explain/explain.go`: inject each workload finding's `remediation.For(f)` suggestion into `buildInventoryPrompt`, and add two instructions to the `systemPrompt` const (a `Fix first:` ranked list; strict grounding on the provided command). The deterministic offline core is untouched; tests use the existing pure `buildInventoryPrompt` + the `systemPrompt` const (no network).

**Tech Stack:** Go 1.26, `internal/remediation` (the deterministic `--suggest` core), the existing fake-summarizer test seam.

## Global Constraints

- **Opt-in; offline core unchanged.** Only the `--explain` prompt is enriched; `scan`/`--suggest` output is untouched. No API call without `--explain`.
- **LLM ranks, never invents:** the deterministic command is the source of truth for the Fix; the model ranks/sequences/phrases only.
- **No import cycle:** `explain` imports `remediation` (which imports only `diagnose`); acyclic.
- Applies to **workload findings only** (service/Assess issues keep their existing prompt lines).
- **Deterministic tests** — `buildInventoryPrompt` is pure and `systemPrompt` is a const; assert prompt content with no network.
- Gate: touches only `internal/explain` → **LIGHTWEIGHT**. **Minor** bump v0.47.0 → **v0.48.0**; **chart PATCH** (no Helm/deploy/RBAC change).
- `remediation`, `diagnose`, `inventory`, `scan`, `report`, `--fix`, watch, and the golden snapshot stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

---

### Task 1: Inject the deterministic suggestion + rank/ground the prompt

**Files:**
- Modify: `internal/explain/explain.go` (import `remediation`; `buildInventoryPrompt` suggestion line; `systemPrompt` additions)
- Test: `internal/explain/explain_test.go` (two new tests)

**Interfaces:**
- Consumes: `remediation.For(f diagnose.Finding) remediation.Suggestion` (fields `NextStep string`, `Command string`); `inventory.Workload` (`.Findings []diagnose.Finding`, each with `Pod`, `Issue`, `Reason`, `Evidence`, `Container`); the existing unexported `buildInventoryPrompt(cluster, summary, facts, serviceIssues, workloads) string` and `systemPrompt` const.
- Produces: an enriched `--explain` prompt (no signature change).

- [ ] **Step 1: Write the failing tests**

Add to `internal/explain/explain_test.go` (the imports `strings`, `clusterhealth`, `inventory`, `diagnose` are already present from the existing `TestBuildInventoryPrompt_*` tests):

```go
func TestBuildInventoryPrompt_InjectsDeterministicSuggestion(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 2, Status: "Degraded",
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "CrashLoopBackOff", Reason: "crashes", Evidence: "restartCount=8", Container: "web"}},
	}}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, nil, nil, ws)
	if !strings.Contains(got, "suggested fix (deterministic, pre-reviewed — do not substitute):") {
		t.Errorf("prompt missing the deterministic suggestion line:\n%s", got)
	}
	// The exact remediation.For command for a CrashLoopBackOff finding.
	if !strings.Contains(got, "kubectl -n shop logs web-abc -c web --previous") {
		t.Errorf("prompt missing the exact remediation.For command:\n%s", got)
	}
}

func TestSystemPrompt_RanksAndGrounds(t *testing.T) {
	if !strings.Contains(systemPrompt, "Fix first:") {
		t.Error("systemPrompt must instruct a leading Fix first ranked list")
	}
	if !strings.Contains(systemPrompt, "verbatim") || !strings.Contains(systemPrompt, "never substitute or invent") {
		t.Error("systemPrompt must ground the Fix on the deterministic command (verbatim / never substitute or invent)")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ -run 'TestBuildInventoryPrompt_InjectsDeterministicSuggestion|TestSystemPrompt_RanksAndGrounds' 2>&1 | tail`
Expected: FAIL — the suggestion line is absent from the prompt and `systemPrompt` lacks `Fix first:` / the grounding phrases.

- [ ] **Step 3: Inject the suggestion into `buildInventoryPrompt`**

In `internal/explain/explain.go`, add the import (alphabetical, in the internal-import group):

```go
	"github.com/imantaba/kubeagent/internal/remediation"
```

In `buildInventoryPrompt`, inside the `for _, f := range w.Findings {` loop, after the existing `issue:` line and its optional `log cause` / `container resources` sub-lines (i.e. as the last statement in the loop body, before the loop's closing `}`), add:

```go
			s := remediation.For(f)
			fmt.Fprintf(&b, "      suggested fix (deterministic, pre-reviewed — do not substitute): %s | run: %s\n", s.NextStep, s.Command)
```

(The dash in `pre-reviewed — do not substitute` is an em dash, U+2014, matching the test assertion.)

- [ ] **Step 4: Add the `Fix first` ranked list + grounding to `systemPrompt`**

Replace the `systemPrompt` const with (two additions: the `Fix first:` paragraph after the first paragraph, and the reworked `Fix:` bullet):

```go
const systemPrompt = `You are a senior Kubernetes SRE reviewing a read-only cluster scan. Explain what
is wrong and exactly how to fix it, using ONLY the facts provided — do not invent
causes, resources, or values that are not given.

Begin your response with a "Fix first:" section — a numbered list ranking the
issues in the order they should be remediated (most blocking / highest-impact
first; cluster / kube-system P1 issues before workload P2 issues), each line
"N. <namespace/name>: <one-phrase action>". Then give the per-issue detail below.

Address issues in priority order: cluster / kube-system problems (P1) before
workload problems (P2). For EACH issue use this structure:

**<namespace/name> — <the issue>**
- Root cause: one line, from the facts. If the facts are ambiguous, name the most
  likely cause AND what to check — never present a guess as certain.
- Check: 1–3 read-only commands to confirm (kubectl get/describe/logs).
- Fix: use the provided deterministic, pre-reviewed command for this issue
  verbatim — you may add a namespace or flag already shown, sequence multiple
  provided commands, and phrase it for on-call, but never substitute or invent a
  different command. When the provided command is a generic describe, keep it and
  say what to look for in the output.

Be tight — no preamble, no restating the input, no generic advice. If a finding
is expected (e.g. a scaled-to-zero workload), say it needs no action. Prefer
"likely"/"check" over false certainty.`
```

(This preserves every existing instruction; it only adds the `Fix first:` paragraph and rewrites the `Fix:` bullet's guidance. Do not remove the use-only-given-facts, tightness, or expected-state lines.)

- [ ] **Step 5: Run the explain suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ 2>&1 | tail -5`
Expected: PASS — the two new tests and all existing explain tests (the fake-summarizer tests are unaffected: the prompt gains a line but `ExplainInventory`'s behavior — call on degraded/workloads, skip when healthy — is unchanged). `TestBuildInventoryPrompt_OnlyStructuredFields` may now also see the suggestion line; if that test asserts an EXACT full-prompt string it will need updating — but it asserts substrings/absence of raw specs, and the suggestion line contains no pod-spec/secret data, so it should still pass. If it fails, confirm it is only because of the added benign suggestion line and update its expectation accordingly (do NOT weaken its "no raw pod spec / no secrets" assertions).

- [ ] **Step 6: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/explain/
git add internal/explain/explain.go internal/explain/explain_test.go
git commit -m "feat(explain): ground the Fix on the deterministic command and add a Fix-first ranking"
```

---

### Task 2: Documentation

**Files:**
- Modify: the doc that describes `--explain` (find it: `grep -rl -- '--explain' website/docs`), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs).

- [ ] **Step 1: Update the docs**

- The `--explain` feature doc (run `grep -rl -- '--explain' website/docs` — it is the file with the `--explain` feature section; likely `website/docs/features/diagnostics.md` or a dedicated page): add that `--explain` now (a) opens with a `Fix first:` ranked remediation order and (b) grounds each Fix on kubeagent's deterministic, pre-reviewed `--suggest` command — the model ranks and phrases but never invents or substitutes a command. Note it remains opt-in and needs an API key; the offline core is unchanged. Match the page's style.

- `README.md`: in the `--explain` description, note the ranked `Fix first:` list and that fixes are grounded on the deterministic `--suggest` core (the model never invents commands).

- `CHANGELOG.md`: under `## [Unreleased]` → `### Changed` (this changes existing `--explain` behavior; use `### Changed`, not `### Added`, and add the `### Changed` subheading if absent):

  ```
  - **`--explain` now ranks and grounds remediation.** The explanation opens with a
    `Fix first:` ordered remediation list, and each per-issue Fix is anchored to
    kubeagent's deterministic, pre-reviewed `--suggest` command — the model ranks,
    sequences, and phrases, but never invents or substitutes a command. First
    Theme-C (principled intelligence) slice; the deterministic offline core is
    unchanged.
  ```

- `website/docs/roadmap.md`: add a Shipped bullet after the webhook-latency entry, tagged **Theme-C** (first principled-intelligence slice), noting `--explain` now ranks + grounds on the deterministic core; link to the `--explain` doc.

- [ ] **Step 2: Verify the docs build**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (venv fallback: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, no page WARNINGs.

- [ ] **Step 3: Run the whole suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add website/ README.md CHANGELOG.md
git commit -m "docs: document ranked, grounded --explain remediation"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the `release` skill owns this. Touches only `internal/explain` (opt-in LLM path; offline core unchanged) → **LIGHTWEIGHT** gate. The chaos gate does not exercise `--explain` (it runs without an API key), so validation is the deterministic unit tests (fake summarizer + pure `buildInventoryPrompt`); if `ANTHROPIC_API_KEY` is available, a single live `scan --explain` against a Kind cluster with a crashing pod confirms the `Fix first:` list and a grounded Fix render. **Minor** bump **v0.47.0 → v0.48.0**; **chart PATCH** (no Helm/template change — the bump script's default patch is correct; do NOT override to minor). Hold for the user's explicit "run release and push".
```
