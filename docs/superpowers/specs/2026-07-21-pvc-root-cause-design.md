# Failed-PVC root cause — design

**Status:** approved · **Date:** 2026-07-21 · **Type:** new feature (v0.29 root-cause theme, third slice)

## Problem

When a PersistentVolumeClaim cannot provision or bind, every pod that mounts it
sits in `Pending`/`ContainerCreating` with **no pod-level detector finding** —
the real cause is a hop away in the storage chain. kubeagent already diagnoses
the broken PVC itself (`pvchealth`, v0.26.0: `✗ shop/reports-data
PersistentVolumeClaim Pending — ProvisioningFailed`), but nothing connects the
stranded workload to it. This slice joins the two through the shared `RootCause`
mechanism (nodes v0.29.0, registries v0.30.0).

## Behavior (approved)

A **flagged, not-yet-attributed** workload whose pod mounts a PVC that appears in
`pvchealth`'s issues is attributed to that PVC:

```text
Needs attention: 2 workloads failing (1 ⇐ PVC reports-data) · 1 PVC failing to provision

✗ shop/reports  Deployment  0/1 Pending
    ↳ likely caused by PVC reports-data (ProvisioningFailed)
```

- **Threshold 1 — deliberate and honest.** Unlike the registry slice (statistical
  inference over co-occurring failures → threshold 2), this is a **join against
  independent evidence**: `pvchealth` has already diagnosed the PVC as broken
  from its own events. One workload mounting one broken PVC is correctly
  attributed. The wording stays hedged (`likely`) for consistency with the other
  slices.
- **Precedence: node → PVC → registry.** Node attribution stays first (most
  fundamental); PVC runs before registry because evidence-backed beats
  statistical. Each annotator skips workloads with an existing `RootCause`.
- `RootCause` string (fixed format, feeds the existing generic rendering):
  `PVC <name> (<Reason>)` — `<Reason>` ∈ {`ProvisioningFailed`, `FailedBinding`}
  from `pvchealth.Issue.Reason`. Name only (a pod mounts PVCs in its own
  namespace, and the workload line already shows the namespace).

## Design

### 1. `internal/rootcause` — third annotator

```go
// AnnotatePVC sets w.RootCause = "PVC <name> (<reason>)" on each flagged,
// not-yet-attributed workload that has a pod mounting a PersistentVolumeClaim
// pvchealth diagnosed as broken (Pending with a provisioning/binding failure).
// podPVCs maps "namespace/podName" to the PVC names the pod mounts. Threshold is
// one workload — the PVC is independently diagnosed, so this is a join, not an
// inference. Pure and deterministic (issue PVC names checked in sorted order).
// Call after Annotate (nodes) and before AnnotateRegistry.
func AnnotatePVC(workloads []inventory.Workload, podPVCs map[string][]string, issues []pvchealth.Issue)
```

Logic:
- Build `issueByKey["ns/pvcName"]reason` from `issues`, and a sorted slice of the
  keys (determinism).
- For each workload: skip unless `w.Flagged() && w.RootCause == ""`. Build the
  set of `"w.Namespace/pvc"` keys from its pods' mounts
  (`podPVCs[w.Namespace+"/"+pod.Name]`). Walk the sorted issue keys; the first
  one present in the workload's mount set wins:
  `RootCause = "PVC " + pvcName + " (" + reason + ")"`.
- Namespace isolation is inherent: keys are `ns/name`, so a same-named PVC in
  another namespace never matches.

Imports gained: `internal/pvchealth` (no cycle — pvchealth imports only k8s API
types). `sort` already imported.

### 2. `scan.Evaluate` — build the map + one call

Alongside the existing `podLabels` map (scan.go:184), build the mount map from
the already-collected pods:

```go
	podPVCs := make(map[string][]string, len(inputs.Pods))
	for _, p := range inputs.Pods {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				key := p.Namespace + "/" + p.Name
				podPVCs[key] = append(podPVCs[key], v.PersistentVolumeClaim.ClaimName)
			}
		}
	}
```

Annotator ordering (node → PVC → registry):

```go
	rootcause.Annotate(result.Workloads, health.DownNodes)
	rootcause.AnnotatePVC(result.Workloads, podPVCs, pvcIssues)
	rootcause.AnnotateRegistry(result.Workloads)
```

`pvcIssues` is already computed earlier (scan.go:176) — no reordering needed.

### 3. Zero report changes

The `↳ likely caused by PVC reports-data (ProvisioningFailed)` line, the
single-cause rollup `(M ⇐ PVC reports-data)` (via the generic
`rootCauseNode` prefix extraction on `" ("`), and the mixed
`(M ⇐ K root causes)` wording all come free from the v0.29/v0.30 machinery.
JSON: the existing `rootCause` field gains a new value (PVC name + reason — no
secrets/IPs; flows to `--explain` like the sibling causes).

## Global constraints

- **Read-only; NO new RBAC / collector / flag** — pods, PVCs, and PVC events are
  already collected; `pvcIssues` is already computed. Not
  `internal/collect`/`cluster`/`watch` → **lightweight real-cluster smoke** gate;
  **minor** bump v0.30.0 → **v0.31.0**.
- **Pure & deterministic** — sorted issue keys; fixed strings.
- **Always-on** — runs in `scan` and the `watch` daemon via `scan.Evaluate`.
- `inventory`, `clusterhealth`, `report.go`, `pvchealth`, `internal/collect`,
  `internal/watch`, `explain.go`, RBAC, Helm stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.**

## Out of scope (YAGNI)

Ephemeral volume claims (`pod.Spec.Volumes[].Ephemeral`); PV-level attach causes
(the `VolumeAttachError` detector owns those); WaitForFirstConsumer PVCs
(`pvchealth` already never flags them, so they can never attribute); generic
Secret/ConfigMap mount failures; confidence scores.

## Testing

- **`AnnotatePVC` (pure):** a flagged 0/1 workload whose pod mounts an
  issue-PVC → `RootCause == "PVC reports-data (ProvisioningFailed)"` (threshold
  1); a flagged workload mounting only healthy PVCs → not attributed; a workload
  with `RootCause` already set (node) → preserved; a **not-flagged** workload
  mounting the issue-PVC → skipped; namespace isolation — same PVC name in a
  different namespace → NOT matched; two issue-PVCs mounted by one workload →
  deterministic sorted-first pick; empty `issues`/`podPVCs` → no-op;
  `FailedBinding` reason renders in the string.
- **Precedence:** a flagged workload that both mounts an issue-PVC AND has an
  `ImagePullBackOff` finding sharing a registry with another pull-failer → gets
  the PVC cause (AnnotatePVC ran first) and shrinks the registry group.
- **`scan` integration:** fake clientset with a Pending PVC + its
  `ProvisioningFailed` event + a Deployment whose pod mounts it → the workload
  carries the PVC `RootCause` through `Evaluate`. (The fake clientset ignores
  field selectors; `pvchealth.Assess` correlates by InvolvedObject in-code, as
  established in v0.26.0.)
- **Golden:** the fixture already has the `shop/reports-data` PVC issue. Add one
  new workload — `reports`, Deployment 0/1 `Pending`, **no findings** (flagged
  via Ready<Desired; the realistic stuck-on-storage shape), pod on `worker-3`,
  `RootCause: "PVC reports-data (ProvisioningFailed)"` set directly (the golden
  renders a pre-built Input). Attention line becomes
  `14 workloads failing (7 ⇐ 4 root causes)` (14=13+1; 7=6+1; 4 = worker-1,
  worker-2, ghcr.io, PVC reports-data). Regenerate.

## Files touched

- **Modify:** `internal/rootcause/rootcause.go` (+ test) — `AnnotatePVC`.
- **Modify:** `internal/scan/scan.go` (+ test) — the `podPVCs` map + one call line.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — the `reports` fixture workload + regenerate. (`report.go` itself unchanged.)
- **Docs:** `website/docs/features/diagnostics.md` (extend the `### Root-cause attribution` subsection), `README.md` (extend the bullet), `CHANGELOG.md` (`### Added`), `website/docs/roadmap.md` (extend the Shipped bullet).
