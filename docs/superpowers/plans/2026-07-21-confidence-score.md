# Per-Finding Confidence Score Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stamp every finding with a deterministic confidence level (high for direct Kubernetes-asserted states, medium for kubeagent heuristics), and tag correlation attributions the same way — shown in text only when not high, always in JSON — so operators and `--explain` can tell a certainty from a judgment.

**Architecture:** `diagnose.Finding` gains a `Confidence` field. A pure `internal/confidence` package classifies by issue string (`ForIssue`) and by root-cause type (`ForRootCause`), and `Annotate(workloads)` stamps every finding — one choke point run once in `scan.Evaluate`. The report appends a `[level]` tag only for non-high confidence. Informational only: priority and verdict are unchanged.

**Tech Stack:** Go 1.26 standard library. No new dependencies, no new API calls.

## Global Constraints

- **Read-only; NO new RBAC / collector / flag.** Always-on via `scan.Evaluate`. Not `internal/collect`/`cluster`/`watch` → **lightweight real-cluster smoke** gate; **minor** bump v0.32.0 → **v0.33.0**.
- **Pure & deterministic** — a fixed classifier; no clock, no ordering dependence; `Annotate` idempotent.
- **Informational only** — confidence NEVER affects `Prioritize`, the cluster verdict, or which findings show.
- `inventory` (no new field), `collect`, `watch`, RBAC, Helm, `explain.go`, and the NetworkPolicy hint wording stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** (or any AI attribution) on any commit. **TDD.**
- Glyphs `⚠` / `↳` already in report.go — reuse; do not substitute ASCII.
- Set PATH first every task: `export PATH=$PATH:/usr/local/go/bin`.
- Spec: [docs/superpowers/specs/2026-07-21-confidence-score-design.md](../specs/2026-07-21-confidence-score-design.md).

---

## File Structure

- **Modify** `internal/diagnose/diagnose.go` — the `Confidence` field on `Finding`.
- **Create** `internal/confidence/confidence.go` (+ test) — `ForIssue`, `ForRootCause`, `Annotate`.
- **Modify** `internal/scan/scan.go` (+ test) — one `confidence.Annotate` line.
- **Modify** `internal/report/report.go` (+ test) — finding-line + attribution-line tags.
- **Modify** `internal/report/golden_test.go` + `internal/report/testdata/golden-scan.txt` — stamp the fixture RestartLoop finding + regenerate.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.

---

### Task 1: `diagnose.Finding.Confidence` + `internal/confidence`

**Files:**
- Modify: `internal/diagnose/diagnose.go`
- Create: `internal/confidence/confidence.go`
- Test: `internal/confidence/confidence_test.go`

**Interfaces:**
- Consumes: `inventory.Workload` (`Findings []diagnose.Finding`), `diagnose.Finding.Issue`.
- Produces: `diagnose.Finding.Confidence string` (`json:"confidence,omitempty"`); `func ForIssue(issue string) string`; `func ForRootCause(rootCause string) string`; `func Annotate(workloads []inventory.Workload)`.

- [ ] **Step 1: Add the field**

In `internal/diagnose/diagnose.go`, add to the `Finding` struct (after `Container`, keeping the annotator-set/enrichment fields grouped):

```go
	Confidence string `json:"confidence,omitempty"` // "high" (direct k8s state) | "medium" (heuristic); set by confidence.Annotate
```

Verify it compiles: `export PATH=$PATH:/usr/local/go/bin && go build ./internal/diagnose`.

- [ ] **Step 2: Write the failing tests**

Create `internal/confidence/confidence_test.go`:

```go
package confidence

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func TestForIssue(t *testing.T) {
	medium := []string{"RestartLoop", "ProbeFailure"}
	high := []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "OOMKilled",
		"Unschedulable", "VolumeAttachError", "Init:CrashLoopBackOff", "Init:ImagePullBackOff",
		"Init:OOMKilled", "FailedCreate", "JobFailed", "SomeFutureDirectDetector"}
	for _, iss := range medium {
		if got := ForIssue(iss); got != "medium" {
			t.Errorf("ForIssue(%q) = %q, want medium", iss, got)
		}
	}
	for _, iss := range high {
		if got := ForIssue(iss); got != "high" {
			t.Errorf("ForIssue(%q) = %q, want high (default)", iss, got)
		}
	}
}

func TestForRootCause(t *testing.T) {
	cases := map[string]string{
		"node worker-2 (NotReady)":                      "high",
		"PVC reports-data (ProvisioningFailed)":         "high",
		"registry ghcr.io (2 workloads failing to pull)": "medium",
		"":                                              "",
		"something else":                                "",
	}
	for rc, want := range cases {
		if got := ForRootCause(rc); got != want {
			t.Errorf("ForRootCause(%q) = %q, want %q", rc, got, want)
		}
	}
}

func TestAnnotate_StampsEveryFinding(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "cache", Findings: []diagnose.Finding{
		{Issue: "RestartLoop"}, {Issue: "CrashLoopBackOff"},
	}}}
	Annotate(ws)
	if ws[0].Findings[0].Confidence != "medium" {
		t.Errorf("RestartLoop confidence = %q, want medium", ws[0].Findings[0].Confidence)
	}
	if ws[0].Findings[1].Confidence != "high" {
		t.Errorf("CrashLoopBackOff confidence = %q, want high", ws[0].Findings[1].Confidence)
	}
	// idempotent
	Annotate(ws)
	if ws[0].Findings[0].Confidence != "medium" || ws[0].Findings[1].Confidence != "high" {
		t.Error("Annotate must be idempotent")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/confidence`
Expected: FAIL — build error (package undefined).

- [ ] **Step 4: Implement**

Create `internal/confidence/confidence.go`:

```go
// Package confidence classifies findings and root-cause attributions by how
// directly the observed signal implies the diagnosis: "high" for a state
// Kubernetes itself asserts, "medium" for a kubeagent heuristic or inference.
// Pure and deterministic; informational only (never affects priority or the
// cluster verdict).
package confidence

import (
	"strings"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// ForIssue returns the confidence level of a finding by its Issue string.
// kubeagent heuristics are "medium"; every other (direct-read) issue is "high",
// so a new direct-state detector needs no change here.
func ForIssue(issue string) string {
	switch issue {
	case "RestartLoop", "ProbeFailure":
		return "medium"
	default:
		return "high"
	}
}

// ForRootCause returns the confidence of a root-cause attribution from its cause
// type: node and PVC are evidence-backed ("high"); a shared registry is a
// statistical inference ("medium"). Empty or unrecognized input returns "".
func ForRootCause(rootCause string) string {
	switch {
	case strings.HasPrefix(rootCause, "node "):
		return "high"
	case strings.HasPrefix(rootCause, "PVC "):
		return "high"
	case strings.HasPrefix(rootCause, "registry "):
		return "medium"
	default:
		return ""
	}
}

// Annotate stamps Confidence on every finding of every workload — a single choke
// point covering all finding producers. Mutates in place; idempotent.
func Annotate(workloads []inventory.Workload) {
	for i := range workloads {
		for j := range workloads[i].Findings {
			workloads[i].Findings[j].Confidence = ForIssue(workloads[i].Findings[j].Issue)
		}
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/confidence -v && go vet ./internal/confidence`
Expected: PASS, vet clean.

- [ ] **Step 6: Commit**

```bash
git add internal/diagnose/diagnose.go internal/confidence/confidence.go internal/confidence/confidence_test.go
git commit -m "feat(confidence): classify findings and attributions by confidence"
```

---

### Task 2: Wire `confidence.Annotate` into `scan.Evaluate`

**Files:**
- Modify: `internal/scan/scan.go`
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `confidence.Annotate` (Task 1); `result.Workloads`.
- Produces: no signature change; `Finding.Confidence` now populated on `Evaluate` output.

- [ ] **Step 1: Write the failing integration test**

Add to `internal/scan/scan_test.go` (mirrors the existing `TestEvaluate_FlagsRestartLoop` shape — a Running container that keeps erroring; reuse its pod construction if present, else build inline):

```go
func TestEvaluate_StampsFindingConfidence(t *testing.T) {
	now := time.Now()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "cache-1", Labels: map[string]string{"app": "cache"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "cache", Ready: true, RestartCount: 5,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-20 * time.Second))}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(now.Add(-25 * time.Second))}}}}}}
	cli := fake.NewSimpleClientset(node, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "RestartLoop" {
				found = true
				if f.Confidence != "medium" {
					t.Errorf("RestartLoop confidence = %q, want medium", f.Confidence)
				}
			}
		}
	}
	if !found {
		t.Errorf("expected a RestartLoop finding, got %+v", res.Inventory.Workloads)
	}
}
```

(If `TestEvaluate_FlagsRestartLoop` already builds an equivalent pod, copy its exact literal to guarantee the RestartLoop detector fires.)

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan -run TestEvaluate_StampsFindingConfidence`
Expected: FAIL — `Confidence` empty (annotate not wired).

- [ ] **Step 3: Add the import + wiring**

In `internal/scan/scan.go`, add the import (alphabetical in the `internal/...` group):

```go
	"github.com/imantaba/kubeagent/internal/confidence"
```

After the existing annotator block (the `rootcause.AnnotateRegistry(result.Workloads)` line, scan.go:222), add:

```go
	confidence.Annotate(result.Workloads)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/scan`
Expected: PASS (new test + all existing `Evaluate` tests).

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): stamp finding confidence after annotation"
```

---

### Task 3: `report` — the `[level]` tags

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `diagnose.Finding.Confidence` (Task 1), `confidence.ForRootCause` (Task 1).
- Produces: `[level]` tag on non-high finding lines and non-high attribution lines.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go`:

```go
func TestPrintInventory_ConfidenceTags(t *testing.T) {
	ws := []inventory.Workload{
		{Namespace: "shop", Name: "cache", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "registry ghcr.io (2 workloads failing to pull)",
			Findings: []diagnose.Finding{
				{Issue: "RestartLoop", Reason: "keeps erroring", Confidence: "medium"},
				{Issue: "CrashLoopBackOff", Reason: "repeatedly crashes", Confidence: "high"},
			}},
		{Namespace: "shop", Name: "db", Kind: "StatefulSet", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "node worker-2 (NotReady)",
			Findings:  []diagnose.Finding{{Issue: "VolumeAttachError", Reason: "Multi-Attach", Confidence: "high"}}},
	}
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		Result: inventory.Result{Workloads: ws}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "⚠ RestartLoop [medium]: keeps erroring") {
		t.Errorf("medium finding should be tagged:\n%s", out)
	}
	if !strings.Contains(out, "⚠ CrashLoopBackOff: repeatedly crashes") || strings.Contains(out, "CrashLoopBackOff [high]") {
		t.Errorf("high finding must be unmarked:\n%s", out)
	}
	if !strings.Contains(out, "↳ likely caused by registry ghcr.io (2 workloads failing to pull) [medium]") {
		t.Errorf("registry attribution should be tagged medium:\n%s", out)
	}
	if strings.Contains(out, "node worker-2 (NotReady) [") {
		t.Errorf("node attribution (high) must be unmarked:\n%s", out)
	}
}

func TestPrintInventory_JSONFindingCarriesConfidence(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff", Reason: "x", Confidence: "high"}}}}
	var buf bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{Workloads: ws}}, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"confidence": "high"`) {
		t.Errorf("JSON must carry finding confidence:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run 'TestPrintInventory_Confidence|TestPrintInventory_JSONFindingCarriesConfidence'`
Expected: FAIL — tags not rendered (JSON test may already pass, since the field serializes automatically once set; the text test fails).

- [ ] **Step 3: Implement the finding-line tag**

In `internal/report/report.go`, `printWorkload`, replace the finding-line print:

```go
	for _, f := range wl.Findings {
		if _, err := fmt.Fprintf(w, "    ⚠ %s: %s\n", f.Issue, f.Reason); err != nil {
			return err
		}
```

with:

```go
	for _, f := range wl.Findings {
		tag := ""
		if f.Confidence != "" && f.Confidence != "high" {
			tag = " [" + f.Confidence + "]"
		}
		if _, err := fmt.Fprintf(w, "    ⚠ %s%s: %s\n", f.Issue, tag, f.Reason); err != nil {
			return err
		}
```

- [ ] **Step 4: Implement the attribution-line tag**

In `internal/report/report.go`, add the import `"github.com/imantaba/kubeagent/internal/confidence"`, and replace the root-cause line (report.go:745):

```go
		if _, err := fmt.Fprintf(w, "    ↳ likely caused by %s\n", wl.RootCause); err != nil {
			return err
		}
```

with:

```go
		rcTag := ""
		if c := confidence.ForRootCause(wl.RootCause); c != "" && c != "high" {
			rcTag = " [" + c + "]"
		}
		if _, err := fmt.Fprintf(w, "    ↳ likely caused by %s%s\n", wl.RootCause, rcTag); err != nil {
			return err
		}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory`
Expected: PASS (new tests + existing `TestPrintInventory_*`). `TestGoldenScanOutput` will now fail (the golden's RestartLoop/registry lines changed) — that is Task 4.

- [ ] **Step 6: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): tag non-high-confidence findings and attributions"
```

---

### Task 4: Golden fixture + snapshot + documentation

**Files:**
- Modify: `internal/report/golden_test.go` + `internal/report/testdata/golden-scan.txt`
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

- [ ] **Step 1: Stamp the fixture's medium findings**

In `internal/report/golden_test.go`, `goldenWorkloads()`, add `Confidence: "medium"` to the two `diagnose.Finding` literals that are kubeagent heuristics — the `shop/cache` **RestartLoop** finding AND the `shop/checkout` **ProbeFailure** finding (these are the only two medium issues in the fixture; everything else is high and renders unmarked whether `Confidence` is unset or `"high"`). Keep every other field on each. The two `registry ghcr.io` attributions (frontend/search) gain their `[medium]` tag automatically via `confidence.ForRootCause` at render time (no fixture change).

Example (the `cache` finding):

```go
			Findings: []diagnose.Finding{{Pod: "shop/cache", Issue: "RestartLoop", Reason: "Container keeps exiting with a non-OOM error and restarting", Evidence: `container "cache", restartCount=5 (still flapping)`, Confidence: "medium"}}},
```

Apply the analogous single-field addition to the `shop/checkout` `ProbeFailure` finding. Any OTHER medium finding you find in the fixture must be stamped too — grep `goldenWorkloads` for `Issue: "RestartLoop"` and `Issue: "ProbeFailure"` and confirm both (and only those) get `Confidence: "medium"`.

- [ ] **Step 2: Regenerate + inspect**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput` (expect FAIL, stale), then `go test ./internal/report -run TestGoldenScanOutput -update` (PASS).
Then `grep -n "RestartLoop\|likely caused by registry" internal/report/testdata/golden-scan.txt`.
Expected: the `cache` RestartLoop line now reads `⚠ RestartLoop [medium]: …`; the two `↳ likely caused by registry ghcr.io (2 workloads failing to pull) [medium]` lines gained `[medium]`. Confirm NO other finding line gained a tag (all others are high) and NO count/verdict/attention line changed.

- [ ] **Step 3: Full report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report`
Expected: PASS (`TestGoldenScanOutput` + `TestGoldenInputCoversAllSections`).

- [ ] **Step 4: Docs**

- `website/docs/features/diagnostics.md` — add a short subsection (after the root-cause attribution subsection is a good home):

```markdown
### Finding confidence

Every finding carries a **confidence** level reflecting how directly the observed
signal implies the diagnosis: **high** when Kubernetes itself asserts the state
(CrashLoopBackOff, OOMKilled, Unschedulable, a controller event, …) and
**medium** for a kubeagent heuristic (`RestartLoop`, `ProbeFailure`) or a
statistical correlation (a shared-registry attribution). High is the unmarked
default; the text report tags only the less-certain findings and hints
(`⚠ RestartLoop [medium]: …`, `↳ likely caused by registry … [medium]`) so the
tag draws the eye to exactly what to second-guess. JSON always carries
`"confidence"` on every finding. Confidence is informational — it never changes a
finding's priority or the cluster verdict.
```

- `README.md` — add one bullet near the diagnostics description:

```markdown
- **Finding confidence** — every finding is labelled high (a direct Kubernetes
  state) or medium (a kubeagent heuristic or correlation); the report tags only
  the less-certain ones, and JSON always carries it.
```

- `CHANGELOG.md` — under `## [Unreleased]` → `### Added`:

```markdown
- **Per-finding confidence score.** Every finding now carries a confidence level —
  high for a directly Kubernetes-asserted state, medium for a kubeagent heuristic
  (`RestartLoop`, `ProbeFailure`) or a statistical correlation (a shared-registry
  attribution). The text report tags only the non-high findings and hints
  (`⚠ RestartLoop [medium]`, `↳ likely caused by registry … [medium]`); JSON
  always carries `"confidence"`. Informational — it never changes priority or the
  cluster verdict. Read-only, always-on, no new RBAC.
```

- `website/docs/roadmap.md` — Shipped bullet:

```markdown
- **Finding confidence** — every finding and correlation hint is labelled high
  (direct Kubernetes state) or medium (kubeagent heuristic / statistical
  correlation); tagged in the report only when not high, always in JSON. See
  [Failure diagnostics](features/diagnostics.md).
```

- [ ] **Step 5: Verify + commit**

Run: `cd website && mkdocs build --strict -f mkdocs.yml 2>&1 | tail -3; cd ..` (skip with a note if mkdocs absent).
Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...` (all PASS).

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt website/docs/features/diagnostics.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "test+docs: golden coverage and documentation for finding confidence"
```

---

## Notes for the executor

- **Release gate (post-merge, controller-owned):** touches `diagnose`/`confidence`/`scan`/`report` only — **not** `internal/collect`/`cluster`/`watch`/RBAC/Helm — so a **lightweight real-cluster smoke** suffices; no full chaos gate. Version bump: **minor**, v0.32.0 → **v0.33.0**.
- **Informational-only invariant:** nothing in this feature may touch `Prioritize`, the verdict, or the set of findings shown. If a reviewer sees confidence used in a comparison that affects ordering/severity, that's a defect.
- **High stays unmarked** — the text tag is emitted only for a non-empty, non-`high` confidence. Do not print `[high]`.
