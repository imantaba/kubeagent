# PodDisruptionBudget-blocked drains — design

**Status:** approved · **Date:** 2026-07-21 · **Type:** new detector (v1 core)

## Problem

A PodDisruptionBudget (PDB) caps how many of a workload's pods may be
*voluntarily* evicted at once. When a PDB permits **zero** evictions, any
`kubectl drain` (node maintenance, autoscaler scale-down, upgrade) that lands on
one of its pods **hangs forever** — silently, with no cluster-level "error"
state. kubeagent has no view of this today. This adds a read-only check that
flags PDBs that will block a drain and names why.

The trap is noise: a healthy single-replica app with `minAvailable: 1` sits at
`disruptionsAllowed: 0` **permanently**, and that is correct and intentional.
Flagging every zero-disruption PDB would fire on most real clusters. So the
check flags only the sharp cases.

## Behavior (approved)

Flagged in NEEDS ATTENTION (prominent, but — like the Pending-PVC and
stuck-terminating checks — **advisory** to the cluster verdict, which stays
node/system-based):

```text
✗ shop/api-pdb  PodDisruptionBudget  minAvailable: 3
    ⚠ PDBBlocked: covers all 3 pods — no voluntary eviction can ever proceed; every node drain will hang
✗ shop/cache-pdb  PodDisruptionBudget  maxUnavailable: 0
    ⚠ PDBBlocked: blocking evictions with only 1/2 guarded pods healthy
✗ ops/legacy-pdb  PodDisruptionBudget  minAvailable: 50%
    ⚠ PDBBlocked: selector matches no pods (stale?)
```

Each PDB is classified into the **first** matching category (flagged once), from
the PDB's own `spec` + `status` (Kubernetes populates the status counts):

| Category | Predicate | Reason string |
|---|---|---|
| `stale` | `status.expectedPods == 0` | `selector matches no pods (stale?)` |
| `unsatisfiable` | `expectedPods > 1 && desiredHealthy >= expectedPods` | `covers all <expectedPods> pods — no voluntary eviction can ever proceed; every node drain will hang` |
| `blocking` | `disruptionsAllowed == 0 && currentHealthy < desiredHealthy` | `blocking evictions with only <currentHealthy>/<desiredHealthy> guarded pods healthy` |

`unsatisfiable` covers `minAvailable ≥ replicas`, `minAvailable: 100%`, and
`maxUnavailable: 0` on a multi-replica workload — all cases where
`disruptionsAllowed` can mathematically never exceed 0. The `expectedPods > 1`
guard is what keeps benign single-replica singletons quiet.

**Deliberately NOT flagged** (intentional/benign — the noise we exclude):

- `expectedPods == 1` (a single-replica singleton with any rule) — evicting the
  one pod always means downtime; the PDB at `disruptionsAllowed: 0` is correct.
- A healthy PDB at the floor: `currentHealthy >= desiredHealthy` with
  `disruptionsAllowed == 0` and `expectedPods > 1` where `desiredHealthy <
  expectedPods` (e.g. `minAvailable: 2` of 3 healthy → `disruptionsAllowed: 1`,
  not flagged at all; at 2/2 healthy during a rollout it is momentarily 0 but
  `currentHealthy (2) >= desiredHealthy (2)` and it is **not** `unsatisfiable`,
  so it stays silent).
- `disruptionsAllowed >= 1` — not blocking.

## Design

### 1. `collect.PodDisruptionBudgets` — one new collector

```go
// PodDisruptionBudgets lists PDBs in the namespace (empty = all), read-only.
// Used by the PDB-blocked-drains check. Needs the base policy/poddisruptionbudgets
// list grant; a forbidden/absent result simply omits the check.
func PodDisruptionBudgets(ctx context.Context, client kubernetes.Interface, namespace string) ([]policyv1.PodDisruptionBudget, error) {
	list, err := client.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing poddisruptionbudgets: %w", err)
	}
	return list.Items, nil
}
```

### 2. `internal/pdbhealth` — pure assessment

```go
type Issue struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Rule      string `json:"rule"`     // "minAvailable: 3" | "maxUnavailable: 0" | "minAvailable: 50%"
	Category  string `json:"category"` // "stale" | "unsatisfiable" | "blocking"
	Reason    string `json:"reason"`   // the blocker explanation, with counts
}

// Assess flags PDBs that will block a node drain. Pure and deterministic: reads
// only each PDB's spec (for the rule string) and status (for the counts, which
// Kubernetes populates). Results are sorted by (Namespace, Name). A PDB that is
// benign (a healthy at-floor budget, a single-replica singleton, or one that
// currently allows a disruption) is not flagged.
func Assess(pdbs []policyv1.PodDisruptionBudget) []Issue
```

- `Rule` from `spec.minAvailable`/`spec.maxUnavailable` (whichever is set) via
  `intstr.IntOrString.String()`, prefixed with the field name. Exactly one of the
  two is set on a valid PDB; if neither is set (unusual), `Rule = ""` and the PDB
  is skipped.
- Classification uses `status.ExpectedPods`, `status.DesiredHealthy`,
  `status.CurrentHealthy`, `status.DisruptionsAllowed` per the table above, first
  match wins (stale → unsatisfiable → blocking); a PDB matching none is not
  flagged.
- Imports only `policyv1`, `intstr`, `fmt`/`sort` (a leaf — no internal packages).

### 3. `scan.Evaluate` — wiring (graceful on forbidden)

```go
	pdbs, _ := collect.PodDisruptionBudgets(ctx, client, opts.Namespace) // forbidden/absent → nil, check skipped
	pdbIssues := pdbhealth.Assess(pdbs)
```

`Result.PDBIssues []pdbhealth.Issue` (nil/empty when nothing is flagged), added
to the return literal. The list error is intentionally ignored, like the sibling
advisory collectors — a restricted kubeconfig without the grant still gets every
other check.

### 4. `report` — a NEEDS ATTENTION rendering

Mirrors `printStuckTerminating` (two lines):

```go
// printPDBIssues lists PodDisruptionBudgets that will block a node drain.
func printPDBIssues(issues []pdbhealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  PodDisruptionBudget  %s\n", is.Namespace, is.Name, is.Rule); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ PDBBlocked: %s\n", is.Reason); err != nil {
			return err
		}
	}
	return nil
}
```

- `Input.PDBIssues []pdbhealth.Issue`; JSON `pdbIssues,omitempty`.
- `hasAttention` includes `len(in.PDBIssues) > 0`; the section prints after
  `printStuckTerminating` in NEEDS ATTENTION.
- `attentionLine` gains `N PodDisruptionBudget(s) blocking drains` (appended after
  the stuck-terminating fragment):
  `fmt.Sprintf("%d %s blocking drains", n, plural(n, "PodDisruptionBudget", "PodDisruptionBudgets"))`.

### 5. `main.go` — the render seam (already testable)

`report.Input` is built in `runScan` via `resultInput(res scan.Result)` (the seam
added in the v0.34.0 fix). Add `PDBIssues: res.PDBIssues` to that function so the
field reaches the CLI report; the existing seam test pattern covers it. **This is
the wiring that stuck-terminating originally missed — it must be in `resultInput`,
not only in the `scan.Result` literal.**

### 6. `watch` — one gauge

`kubeagent_pdb_blocking_issues` = `len(res.PDBIssues)` (mirrors
`kubeagent_resources_stuck_terminating`).

### 7. RBAC + Helm

PDBs live in the `policy` API group, which the clusterrole does not yet grant.
Add a **new base rule** (always-on; PDB metadata is not sensitive) after the
`coordination.k8s.io` rule in both `deploy/rbac.yaml` and
`deploy/helm/kubeagent/templates/clusterrole.yaml`:

```yaml
  - apiGroups: ["policy"]
    resources: [poddisruptionbudgets]
    verbs: [get, list, watch]
```

## Global constraints

- **Read-only; always-on; no flag.** One new base RBAC grant
  (`policy/poddisruptionbudgets`). Touches `internal/collect` + RBAC + Helm →
  **FULL CHAOS GATE** at release (plus a targeted live smoke: a PDB with
  `minAvailable` ≥ replicas that hangs a drain). **Minor** bump v0.34.0 →
  **v0.35.0**; **minor** chart bump (Helm template changed).
- **Pure & deterministic** — `pdbhealth.Assess` reads only the PDB objects;
  sorted output; no clock, no cluster calls.
- **Advisory to the verdict** — does not flip Healthy/Degraded (consistent with
  the PVC-provisioning, service, and stuck-terminating checks).
- `inventory`, `clusterhealth`, `explain.go` (PDBIssues is a separate `Result`
  field, not passed to `--explain` — matches PVCIssues/StuckTerminating), and
  `--fix` stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.**

## Out of scope (YAGNI)

Editing/deleting PDBs (a write — this is read-only); naming the guarded
Deployment/StatefulSet by resolving pod owners (the PDB's own status counts are
enough — chosen in design); correlating a PDB to a specific node drain in
progress; an opt-in flag or configurable behavior (always-on); `--explain`
integration; per-finding confidence tags (the sibling Assess lists don't carry
one).

## Testing

- **`pdbhealth` (pure, fake `policyv1.PodDisruptionBudget`s):**
  - `unsatisfiable`: `minAvailable: 3` with `expectedPods 3, desiredHealthy 3` →
    one Issue, reason "covers all 3 pods…", `Rule "minAvailable: 3"`.
  - `unsatisfiable` via `maxUnavailable: 0` on `expectedPods 2, desiredHealthy 2`
    → flagged, `Rule "maxUnavailable: 0"`.
  - `blocking`: `expectedPods 2, desiredHealthy 2, currentHealthy 1,
    disruptionsAllowed 0` → reason "blocking evictions with only 1/2…".
  - `stale`: `expectedPods 0` → reason "selector matches no pods (stale?)".
  - **Not flagged:** single-replica `minAvailable: 1` (`expectedPods 1,
    desiredHealthy 1, disruptionsAllowed 0` — excluded by the `expectedPods > 1`
    guard); a healthy `minAvailable: 2` of 3 (`expectedPods 3, desiredHealthy 2,
    disruptionsAllowed 1` — not unsatisfiable, not blocking); percentage
    `minAvailable: 50%` of 4 healthy (`expectedPods 4, desiredHealthy 2,
    disruptionsAllowed 2`). Note `minAvailable: N` of exactly N pods **is**
    flagged (`unsatisfiable`) — see the design note below.
  - Category precedence: a PDB that is both `expectedPods 0` and would be
    `blocking` is reported as `stale`.
  - Sorted output (Namespace, Name).
- **`collect`:** `PodDisruptionBudgets` via fake clientset returns the seeded PDB;
  a namespace filter scopes it.
- **`scan` integration:** a fake clientset with an unsatisfiable PDB yields
  `Result.PDBIssues` with it; a forbidden `poddisruptionbudgets` reactor returns
  no error and an empty list (other checks still run).
- **`report`:** the three categories render with the `✗ … PodDisruptionBudget
  <rule>` + `⚠ PDBBlocked:` lines; the section is absent when empty; the
  attention line shows `N PodDisruptionBudget(s) blocking drains`.
- **`main` (seam):** `resultInput(scan.Result{PDBIssues: …})` carries PDBIssues
  into `report.Input` (mirrors `TestResultInput_CarriesStuckTerminating`).
- **`watch`:** the gauge reflects a sample Result.
- **Golden:** add one `unsatisfiable` PDB to the fixture; regenerate; extend the
  attention line and `TestGoldenInputCoversAllSections`.
- **Helm/RBAC:** `helm template` shows the `policy`/`poddisruptionbudgets` rule;
  `helm lint` clean.

> **Design note on the `minAvailable: N` of N healthy case.** A PDB with
> `minAvailable` equal to the replica count is `unsatisfiable` by our predicate
> (`desiredHealthy >= expectedPods`, `expectedPods > 1`) **even when currently
> healthy** — because it can never allow a voluntary eviction, which is exactly
> the drain-blocking misconfiguration we want to surface. This is intended: a
> multi-replica workload whose PDB never permits a disruption is a real trap,
> flagged regardless of momentary health.

## Files touched

- **Create:** `internal/pdbhealth/pdbhealth.go` (+ test).
- **Modify:** `internal/collect/collect.go` (+ test) — `PodDisruptionBudgets`.
- **Modify:** `internal/scan/scan.go` (+ test) — wiring + `Result.PDBIssues`.
- **Modify:** `main.go` (+ `main_test.go`) — `resultInput` carries `PDBIssues`.
- **Modify:** `internal/report/report.go` (+ test) — section + attention line + JSON.
- **Modify:** `internal/watch/metrics.go` (+ test) — the gauge.
- **Modify:** `deploy/rbac.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml` — `policy` rule.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt`.
- **Docs:** `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
