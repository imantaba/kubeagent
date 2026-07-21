# HPA-can't-scale check — design

**Status:** approved · **Date:** 2026-07-21 · **Type:** new detector (v1 core)

## Problem

A HorizontalPodAutoscaler that is silently stuck is a common, invisible outage:
the autoscaler can't fetch metrics (no metrics-server, a bad custom metric), so
it never scales; or its scale target was renamed/deleted so it can't act at all;
or it is pinned at `maxReplicas` while demand exceeds the cap, leaving the app
under-provisioned. Nothing in a normal `kubectl get` surfaces this. This adds a
read-only check that flags a stuck HPA and names why.

## Behavior (approved)

Flagged in NEEDS ATTENTION (prominent, but — like the PDB and stuck-terminating
checks — **advisory** to the cluster verdict, which stays node/system-based):

```text
✗ shop/api-hpa  HorizontalPodAutoscaler  targets Deployment/api
    ⚠ HPAStuck: can't fetch metrics — unable to get resource metric cpu: no metrics returned
✗ shop/worker-hpa  HorizontalPodAutoscaler  targets Deployment/worker
    ⚠ HPAStuck: can't scale — the scale target Deployment/worker was not found
✗ ops/ingest-hpa  HorizontalPodAutoscaler  targets Deployment/ingest
    ⚠ HPAStuck: pinned at maxReplicas 10 — desired exceeds the cap
```

Each HPA is classified into the **first** matching category (flagged once), read
from `status.conditions` (the HPA controller populates them):

| Category | Predicate | Reason string |
|---|---|---|
| `unable` | `AbleToScale` condition `Status == False` | `can't scale — <trimmed condition message>` |
| `metrics` | `ScalingActive` condition `Status == False` | `can't fetch metrics — <trimmed condition message>` |
| `capped` | `ScalingLimited` condition `Status == True && Reason == "TooManyReplicas"` | `pinned at maxReplicas <spec.maxReplicas> — desired exceeds the cap` |

- Precedence `unable → metrics → capped` (most fundamental first): a target it
  can't even reach (`AbleToScale=False`, e.g. the workload was deleted) outranks a
  metrics gap, which outranks a ceiling clamp.
- A condition is only considered when its `Status` matches the predicate above —
  a `ScalingActive=True` HPA is healthy on that axis and falls through.
- Condition messages are trimmed of trailing period/whitespace (like
  `termhealth.trimMsg`).
- `TooManyReplicas` / `TooFewReplicas` are the literal reason strings the
  upstream HPA controller sets on the `ScalingLimited` condition (they are not
  exported constants in `k8s.io/api`); the detector matches the string.

**Deliberately NOT flagged** (benign / working-as-intended):

- A healthy HPA (`AbleToScale=True`, `ScalingActive=True`, `ScalingLimited=False`).
- `ScalingLimited=True` at the **floor** (`Reason == "TooFewReplicas"`) — a normal
  idle app sitting at `minReplicas`.
- Any HPA whose three conditions are absent (a brand-new HPA the controller has
  not yet reconciled) — no `False`/capped condition present → not flagged.

## Design

### 1. `collect.HorizontalPodAutoscalers` — one new collector

```go
// HorizontalPodAutoscalers lists HPAs in the namespace (empty = all), read-only.
// Used by the HPA-can't-scale check. Needs the base autoscaling/horizontalpodautoscalers
// list grant; a forbidden/absent result simply omits the check.
func HorizontalPodAutoscalers(ctx context.Context, client kubernetes.Interface, namespace string) ([]autoscalingv2.HorizontalPodAutoscaler, error) {
	list, err := client.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing horizontalpodautoscalers: %w", err)
	}
	return list.Items, nil
}
```

### 2. `internal/hpahealth` — pure assessment

```go
type Issue struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Target    string `json:"target"`   // "Deployment/api" from spec.scaleTargetRef (Kind/Name)
	Category  string `json:"category"` // "unable" | "metrics" | "capped"
	Reason    string `json:"reason"`
}

// Assess flags HPAs that cannot scale as intended. Pure and deterministic: reads
// only each HPA's spec.scaleTargetRef, spec.maxReplicas, and status.conditions.
// Results are sorted by (Namespace, Name). A healthy HPA, or one limited only at
// its floor, is not flagged.
func Assess(hpas []autoscalingv2.HorizontalPodAutoscaler) []Issue
```

- `Target` = `ref.Kind + "/" + ref.Name` from `spec.scaleTargetRef`.
- A small `condition(hpa, type)` helper returns the `*HorizontalPodAutoscalerCondition`
  of a given type (or nil); `classify` applies the precedence table.
- Imports only `autoscalingv2 "k8s.io/api/autoscaling/v2"`, `fmt`, `sort`,
  `strings` (a leaf — no internal packages).

### 3. `scan.Evaluate` — wiring (graceful on forbidden)

```go
	hpas, _ := collect.HorizontalPodAutoscalers(ctx, client, opts.Namespace) // forbidden/absent → nil, check skipped
	hpaIssues := hpahealth.Assess(hpas)
```

`Result.HPAIssues []hpahealth.Issue` (nil/empty when nothing is stuck), added to
the return literal. The list error is intentionally ignored, like the sibling
advisory collectors.

### 4. `report` — a NEEDS ATTENTION rendering

Mirrors `printPDBIssues` (two lines):

```go
// printHPAIssues lists HorizontalPodAutoscalers that cannot scale as intended.
func printHPAIssues(issues []hpahealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  HorizontalPodAutoscaler  targets %s\n", is.Namespace, is.Name, is.Target); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ HPAStuck: %s\n", is.Reason); err != nil {
			return err
		}
	}
	return nil
}
```

- `Input.HPAIssues []hpahealth.Issue`; JSON `hpaIssues,omitempty`.
- `hasAttention` includes `len(in.HPAIssues) > 0`; the section prints after
  `printPDBIssues` in NEEDS ATTENTION.
- `attentionLine` gains `N HPA(s) can't scale`:
  `fmt.Sprintf("%d %s can't scale", n, plural(n, "HPA", "HPAs"))` (appended after
  the PDB fragment).

### 5. `main.go` — the render seam (already testable)

`report.Input` is built in `runScan` via `resultInput(res scan.Result)`. Add
`HPAIssues: res.HPAIssues` to that function so the field reaches the CLI report;
the existing seam-test pattern (`TestResultInput_Carries…`) covers it. **This is
the wiring that stuck-terminating originally missed — it must be in `resultInput`,
not only the `scan.Result` literal.**

### 6. `watch` — one gauge

`kubeagent_hpa_scaling_issues` = `len(res.HPAIssues)` (mirrors
`kubeagent_pdb_blocking_issues`).

### 7. RBAC + Helm

HPAs live in the `autoscaling` API group, not yet granted. Add a **new base rule**
(always-on; HPA metadata is not sensitive) after the `policy` rule in both
`deploy/rbac.yaml` and `deploy/helm/kubeagent/templates/clusterrole.yaml`:

```yaml
  - apiGroups: ["autoscaling"]
    resources: [horizontalpodautoscalers]
    verbs: [get, list, watch]
```

## Global constraints

- **Read-only; always-on; no flag.** One new base RBAC grant
  (`autoscaling/horizontalpodautoscalers`). Touches `internal/collect` + RBAC +
  Helm → **FULL CHAOS GATE** at release (plus a targeted live smoke: an HPA whose
  scale target is missing, and one pinned at maxReplicas). **Minor** bump
  v0.35.0 → **v0.36.0**; **minor** chart bump (Helm template changed).
- **Pure & deterministic** — `hpahealth.Assess` reads only the HPA objects;
  sorted output; no clock, no cluster calls.
- **Advisory to the verdict** — does not flip Healthy/Degraded (consistent with
  the PVC, PDB, and stuck-terminating checks).
- `inventory`, `clusterhealth`, `explain.go` (HPAIssues is a separate `Result`
  field, not passed to `--explain` — matches PDBIssues/StuckTerminating), and
  `--fix` stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.**

## Out of scope (YAGNI)

Editing HPAs (a write — this is read-only); recommending target replica counts or
metric fixes; reading the metrics API to second-guess the controller (we trust
the HPA's own conditions); the autoscaling/v1 API (v2 conditions are richer and
standard on supported clusters); static misconfig like `minReplicas > maxReplicas`
(the API server rejects most such cases; not worth the edge-case surface — the
user chose "hard failures + capped"); an opt-in flag; `--explain` integration;
per-finding confidence tags (sibling Assess lists don't carry one).

## Testing

- **`hpahealth` (pure, fake `autoscalingv2.HorizontalPodAutoscaler`s):**
  - `unable`: an HPA with `AbleToScale` `Status=False`, message "the scale target
    Deployment/worker was not found" → one Issue, reason `can't scale — the scale
    target Deployment/worker was not found`, `Target "Deployment/worker"`.
  - `metrics`: `ScalingActive=False`, message "unable to get resource metric cpu:
    no metrics returned." → reason `can't fetch metrics — unable to get resource
    metric cpu: no metrics returned` (trailing period trimmed).
  - `capped`: `ScalingLimited=True` `Reason="TooManyReplicas"`, `spec.maxReplicas
    10` → reason `pinned at maxReplicas 10 — desired exceeds the cap`.
  - Precedence: an HPA with BOTH `AbleToScale=False` and `ScalingActive=False` →
    reported as `unable`.
  - **Not flagged:** a healthy HPA (all three conditions healthy);
    `ScalingLimited=True` with `Reason="TooFewReplicas"` (at floor); an HPA with no
    conditions at all (freshly created).
  - Sorted output (Namespace, Name).
- **`collect`:** `HorizontalPodAutoscalers` via fake clientset returns the seeded
  HPA; a namespace filter scopes it.
- **`scan` integration:** a fake clientset with a `ScalingActive=False` HPA yields
  `Result.HPAIssues` with it; a forbidden `horizontalpodautoscalers` reactor
  returns no error and an empty list (other checks still run).
- **`report`:** the three categories render with the `✗ … HorizontalPodAutoscaler
  targets <t>` + `⚠ HPAStuck:` lines; the section is absent when empty; the
  attention line shows `N HPA(s) can't scale`.
- **`main` (seam):** `resultInput(scan.Result{HPAIssues: …})` carries HPAIssues
  into `report.Input` (mirrors `TestResultInput_CarriesPDBIssues`).
- **`watch`:** the gauge reflects a sample Result.
- **Golden:** add one `metrics` HPA to the fixture; regenerate; extend the
  attention line and `TestGoldenInputCoversAllSections`.
- **Helm/RBAC:** `helm template` shows the `autoscaling`/`horizontalpodautoscalers`
  rule; `helm lint` clean.

## Files touched

- **Create:** `internal/hpahealth/hpahealth.go` (+ test).
- **Modify:** `internal/collect/collect.go` (+ test) — `HorizontalPodAutoscalers`.
- **Modify:** `internal/scan/scan.go` (+ test) — wiring + `Result.HPAIssues`.
- **Modify:** `main.go` (+ `main_test.go`) — `resultInput` carries `HPAIssues`.
- **Modify:** `internal/report/report.go` (+ test) — section + attention line + JSON.
- **Modify:** `internal/watch/metrics.go` (+ test) — the gauge.
- **Modify:** `deploy/rbac.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml` — `autoscaling` rule.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt`.
- **Docs:** `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
