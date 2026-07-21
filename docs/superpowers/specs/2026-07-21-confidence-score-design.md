# Per-finding confidence score — design

**Status:** approved · **Date:** 2026-07-21 · **Type:** cross-cutting feature (v0.29–v0.32 detector block)

## Problem

kubeagent's findings mix two kinds of certainty: **direct readouts** of a
Kubernetes state (CrashLoopBackOff, OOMKilled, Unschedulable — k8s itself asserts
them) and **kubeagent heuristics/inferences** (RestartLoop, ProbeFailure, and the
`↳ likely caused by …` correlation hints). Today they render identically, so an
operator triaging — or the `--explain` LLM weighting — can't tell a certainty from
a judgment. A per-finding confidence score makes that distinction explicit,
deterministically and without manufacturing false precision.

## Model (approved)

Three levels — no numeric scores. Confidence reflects **how directly the observed
signal implies the diagnosis**:

- **high** — Kubernetes asserts the state (or a controller/scheduler condition or
  a direct event does): `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`,
  `OOMKilled`, `Unschedulable`, `VolumeAttachError`, `Init:CrashLoopBackOff`,
  `Init:ImagePullBackOff`, `Init:OOMKilled`, `FailedCreate`, `JobFailed`.
- **medium** — kubeagent heuristics: `RestartLoop` (a Running container that keeps
  erroring — a judgment CrashLoopBackOff misses) and `ProbeFailure` (event-based; a
  failing probe can be transient or mis-tuned, not always app-broken).
- **high is the unmarked default:** any issue string not in the medium set →
  high (a new direct-read detector is high without a code change here).

Root-cause attributions carry confidence too (derived from the cause type):
**node → high**, **PVC → high** (evidence-backed join), **registry → medium**
(statistical inference over co-occurring pull failures).

## Design

### 1. `diagnose.Finding` — one field

Add `Confidence string \`json:"confidence,omitempty"\`` (values `"high"` |
`"medium"`; `""` only for a finding that never went through the stamp pass, which
in practice does not happen for scanned output). No other diagnose change; the
classifier lives in the new `confidence` package (below), so `diagnose` gains no
dependency.

### 2. `internal/confidence` — classifier + stamp pass (pure)

```go
// ForIssue returns the confidence level of a finding by its Issue string. Direct
// Kubernetes-asserted states are "high"; kubeagent heuristics are "medium". Any
// unlisted issue defaults to "high" (a new direct-read detector needs no change here).
func ForIssue(issue string) string {
	switch issue {
	case "RestartLoop", "ProbeFailure":
		return "medium"
	default:
		return "high"
	}
}

// ForRootCause returns the confidence of a root-cause attribution from its cause
// type. node/PVC are evidence-backed (high); registry is a statistical inference
// (medium). Empty or unrecognized → "" (rendered without a tag).
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
// point covering all producers (diagnose, createhealth, batchhealth). Pure; mutates
// in place. Idempotent.
func Annotate(workloads []inventory.Workload) {
	for i := range workloads {
		for j := range workloads[i].Findings {
			workloads[i].Findings[j].Confidence = ForIssue(workloads[i].Findings[j].Issue)
		}
	}
}
```

Imports `inventory` + `diagnose` (no cycle: neither imports `confidence`; `strings`
for the prefix check).

### 3. `scan.Evaluate` — one line

After all findings are attached to `result.Workloads` (i.e. after `Prioritize` and
the finding-producing annotators `createhealth`/`batchhealth`; ordering relative to
`rootcause`/`netpolicy`/`rollout` is irrelevant — confidence reads only
`Finding.Issue`):

```go
	confidence.Annotate(result.Workloads)
```

### 4. `report` — two render tweaks

- **Finding line** (`printWorkload`): the existing `⚠ %s: %s` (Issue, Reason)
  becomes — when `f.Confidence != "" && f.Confidence != "high"` — `⚠ %s [%s]: %s`
  (Issue, Confidence, Reason). High/empty → unchanged (unmarked).
- **Attribution line**: the existing `↳ likely caused by %s` (RootCause) gets a
  `[level]` suffix when `confidence.ForRootCause(w.RootCause)` is a non-empty
  non-`high` value: `↳ likely caused by %s [%s]`.
- **JSON:** unchanged code — the new `Finding.Confidence` field serializes
  automatically (omitempty). Attribution confidence is NOT a JSON field: the
  cause type in the `rootCause` string is the machine-readable signal a consumer
  derives it from (documented).

### 5. What does not change

`Prioritize`, the cluster verdict, `inventory.Workload` (no new field), `collect`,
`watch`, RBAC, Helm, `explain.go` (finding confidence flows to `--explain` for free
on the `Finding`; no code change). NetworkPolicy hints keep their "may be blocking"
wording (already hedged — out of scope).

## Global constraints

- **Read-only; NO new RBAC / collector / flag.** Always-on (runs in `scan` and the
  `watch` daemon via `scan.Evaluate`). Not `collect`/`cluster`/`watch` →
  **lightweight real-cluster smoke** gate; **minor** bump v0.32.0 → **v0.33.0**.
- **Pure & deterministic** — a fixed classifier; no clock, no ordering dependence.
- **Informational only** — confidence never affects priority, the verdict, or which
  findings are shown.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.**

## Testing

- **`confidence` (pure):** `ForIssue` — `RestartLoop`/`ProbeFailure` → medium; each
  direct issue (`CrashLoopBackOff`, `OOMKilled`, `Unschedulable`, `FailedCreate`,
  `JobFailed`, `Init:CrashLoopBackOff`, …) → high; an unknown string → high.
  `ForRootCause` — `"node worker-2 (NotReady)"` → high, `"PVC data (…)"` → high,
  `"registry ghcr.io (…)"` → medium, `""`/other → "". `Annotate` — a workload set
  with a RestartLoop + a CrashLoop finding → the RestartLoop finding gets "medium",
  the CrashLoop "high"; idempotent (running twice is stable).
- **`report`:** a medium finding renders `⚠ RestartLoop [medium]: …`; a high finding
  renders `⚠ CrashLoopBackOff: …` (NO tag); a registry attribution renders
  `↳ likely caused by registry … [medium]`; a node attribution renders
  `↳ likely caused by node … (NotReady)` (no tag). JSON output includes
  `"confidence":"high"` on a high finding.
- **`scan` integration:** a RestartLoop-producing pod through `Evaluate` yields a
  workload whose finding has `Confidence == "medium"`.
- **Golden:** the fixture already has a `RestartLoop` finding (`shop/cache`) and a
  `registry ghcr.io` attribution — after stamping, the RestartLoop line gains
  `[medium]` and the two `frontend`/`search` registry attribution lines gain
  `[medium]`; set `Confidence` on the fixture findings directly (the golden renders
  a pre-built Input) and regenerate. No count/verdict lines change.

## Files touched

- **Modify:** `internal/diagnose/diagnose.go` — the `Confidence` field.
- **Create:** `internal/confidence/confidence.go` (+ test) — `ForIssue`, `ForRootCause`, `Annotate`.
- **Modify:** `internal/scan/scan.go` (+ test) — the `confidence.Annotate` line.
- **Modify:** `internal/report/report.go` (+ test) — finding-line + attribution-line tags.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — stamp fixture findings + regenerate.
- **Docs:** `website/docs/features/diagnostics.md` (a short "Confidence" note), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.

## Non-goals (YAGNI)

Numeric/percentage scores; confidence affecting priority or the verdict; a
`Workload.RootCauseConfidence` JSON field (cause type already conveys it);
confidence on NetworkPolicy hints; per-evidence confidence within a single finding.
