# Stuck-terminating / finalizer-deadlock check — design

**Status:** approved · **Date:** 2026-07-21 · **Type:** new detector (v1 core)

## Problem

A resource whose deletion wedges — a Namespace stuck `Terminating` for hours
(the classic finalizer deadlock, usually a downstream APIService or a leftover
finalizer), a Pod stuck `Terminating` past its grace period (blocking a rollout
or a node drain), a PVC held by `pvc-protection` because something still mounts
it — is a common, high-friction outage that kubeagent has no view of today. This
adds a read-only check that flags stuck-terminating resources and names the
actual blocker.

## Behavior (approved)

Flagged in NEEDS ATTENTION (prominent, but — like the Pending-PVC check —
advisory to the cluster verdict, which stays node/system-based):

```text
✗ legacy-ns  Namespace  Terminating 3h
    ⚠ StuckTerminating: NamespaceFinalizersRemaining — kubernetes finalizer remains (an APIService may be down)
✗ shop/api-7c9d5  Pod  Terminating 8m (past grace)
    ⚠ StuckTerminating: finalizer example.com/cleanup-hook
✗ shop/data  PersistentVolumeClaim  Terminating 20m
    ⚠ StuckTerminating: pvc-protection — still mounted by pod shop/db-0
```

- **Threshold:** a resource is stuck when `now − deletionTimestamp > 2 minutes`
  (fixed constant; normal deletions finish in seconds). Kubernetes sets a pod's
  `deletionTimestamp` to its grace deadline, so for a pod this is uniformly "past
  the deletion deadline by 2 min" — the `(past grace)` note is shown for pods.
- **Reason names the blocker:**
  - **Namespace:** the message of the first present blocking `status.condition`
    among `NamespaceDeletionContentFailure`, `NamespaceContentRemaining`,
    `NamespaceFinalizersRemaining` (rendered `<Type> — <trimmed message>`); if
    none present, `finalizers <names>`.
  - **Pod:** `finalizer <names>` when `metadata.finalizers` is non-empty; else
    (past grace, no finalizers) `deletion not confirmed (node gone or kubelet not
    reporting)`.
  - **PVC:** `pvc-protection — still mounted by pod <ns/name>` when a collected
    pod mounts it (cross-referenced from the pod list); else `pvc-protection`
    (or the raw finalizer names if not the standard one).

## Design

### 1. `collect.Namespaces` — one new collector

```go
// Namespaces lists all namespaces (cluster-scoped; read-only). Used by the
// stuck-terminating check to see a namespace wedged in Terminating and its
// status conditions. Needs the base `namespaces` list grant.
func Namespaces(ctx context.Context, client kubernetes.Interface) ([]corev1.Namespace, error) {
	list, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	return list.Items, nil
}
```

Pods and PVCs are already collected (`inputs.Pods`; `collect.PersistentVolumeClaims`).

### 2. `internal/termhealth` — pure assessment

```go
type Issue struct {
	Kind      string `json:"kind"`                // "Namespace" | "Pod" | "PersistentVolumeClaim"
	Namespace string `json:"namespace,omitempty"` // empty for cluster-scoped Namespace
	Name      string `json:"name"`
	Age       string `json:"age"`      // compact humanized time since deletionTimestamp, e.g. "3h", "8m"
	PastGrace bool   `json:"pastGrace,omitempty"` // pods only — rendered as "(past grace)"
	Reason    string `json:"reason"`   // the blocker (finalizer / condition / pvc-protection detail)
}

// Assess flags resources whose deletion has been pending longer than threshold.
// Pure and deterministic: the caller injects now and the threshold. Results are
// sorted by (Kind, Namespace, Name). A resource with no deletionTimestamp, or
// stuck for <= threshold, is not flagged.
func Assess(namespaces []corev1.Namespace, pods []corev1.Pod, pvcs []corev1.PersistentVolumeClaim, threshold time.Duration, now time.Time) []Issue
```

- A resource qualifies when `dt := obj.DeletionTimestamp` is set and
  `now.Sub(dt.Time) > threshold`.
- `Age` = a compact humanizer of `now.Sub(dt.Time)` (days/hours/minutes; a small
  local helper — no `time.Now`).
- Pod reason: `podReason(pod)` → finalizers joined, else the node/kubelet phrase;
  `PastGrace = true` for pods.
- PVC reason: `pvcReason(pvc, pods)` → `pvc-protection — still mounted by pod
  <ns/name>` (first pod whose `spec.volumes[].persistentVolumeClaim.claimName`
  matches, same namespace), else `pvc-protection`, else the raw finalizer list.
- Namespace reason: `nsReason(ns)` → the first present blocking condition's
  `Type — message` (message trimmed of trailing period/whitespace), else
  `finalizers <ns.Spec.Finalizers…>`.

Imports only `corev1`, `strings`, `time` (no internal packages — a leaf).

### 3. `scan.Evaluate` — wiring (graceful on forbidden namespaces)

```go
	namespaces, nsErr := collect.Namespaces(ctx, client)
	_ = nsErr // a forbidden/absent namespaces list simply omits namespace checks; pods+PVCs still run
	stuckTerminating := termhealth.Assess(namespaces, inputs.Pods, pvcs, 2*time.Minute, time.Now())
```

`Result.StuckTerminating []termhealth.Issue` (nil/empty when nothing is stuck).
The namespace list error is intentionally ignored (like the sibling advisory
collectors) — a restricted kubeconfig without namespace-list still gets the
pod/PVC checks (those need no grant beyond the base pods/pvcs already used).

### 4. `report` — a NEEDS ATTENTION rendering

Mirrors `printPVCIssues`:

```go
// printStuckTerminating lists resources wedged in Terminating past the threshold.
func printStuckTerminating(issues []termhealth.Issue, w io.Writer) error {
	for _, is := range issues {
		id := is.Name
		if is.Namespace != "" {
			id = is.Namespace + "/" + is.Name
		}
		grace := ""
		if is.PastGrace {
			grace = " (past grace)"
		}
		fmt.Fprintf(w, "  ✗ %s  %s  Terminating %s%s\n", id, is.Kind, is.Age, grace)
		fmt.Fprintf(w, "      ⚠ StuckTerminating: %s\n", is.Reason)
	}
	return nil
}
```

- `Input.StuckTerminating []termhealth.Issue`; JSON `stuckTerminating,omitempty`.
- `hasAttention` includes `len(in.StuckTerminating) > 0`; the section prints after
  `printPVCIssues` in NEEDS ATTENTION.
- `attentionLine` gains `N resource(s) stuck terminating`.

### 5. `watch` — one gauge

`kubeagent_resources_stuck_terminating` = `len(res.StuckTerminating)` (mirrors
`kubeagent_pvc_pending_issues`).

### 6. RBAC + Helm

Add `namespaces` to the **base** core read rule (not an opt-in add-on — namespace
metadata is not sensitive and the check is always-on): both `deploy/rbac.yaml`
and `deploy/helm/kubeagent/templates/clusterrole.yaml` line 11 →
`resources: [pods, nodes, services, configmaps, events, persistentvolumeclaims, persistentvolumes, namespaces]`
with verbs unchanged (`[get, list, watch]`).

## Global constraints

- **Read-only; always-on; no flag.** One new base RBAC grant (`namespaces` list).
  Touches `internal/collect` + RBAC + Helm → **FULL CHAOS GATE** at release (plus a
  targeted live smoke: a namespace wedged Terminating and a pod stuck past grace).
  **Minor** bump v0.33.0 → **v0.34.0**.
- **Pure & deterministic** — `termhealth.Assess` takes injected `now` + threshold;
  sorted output.
- **Advisory to the verdict** — does not flip Healthy/Degraded (consistent with the
  PVC-provisioning and service checks).
- `inventory`, `clusterhealth`, `explain.go` (the finding-free `Result` field flows
  to `--explain`? — no; explain reads workloads/findings; StuckTerminating is a
  separate `Result` field not passed to explain — acceptable, matches PVCIssues),
  `--fix` stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.**

## Out of scope (YAGNI)

Auto-removing finalizers (a `--fix` concern — this is read-only); other resource
kinds (arbitrary CRs, PVs, Jobs); identifying *which* APIService is down (the
namespace condition message is surfaced as-is); an opt-in flag or configurable
threshold (fixed 2 min); `--explain` integration.

## Testing

- **`termhealth` (pure, fake objects, injected now):** a namespace with
  `deletionTimestamp` 3h ago + a `NamespaceFinalizersRemaining` condition → one
  Issue, reason names the condition; a pod deleting past-grace with a finalizer →
  `finalizer …`; a pod past-grace with no finalizers → the node/kubelet phrase,
  `PastGrace true`; a PVC with `pvc-protection` mounted by a collected pod →
  `still mounted by pod ns/name`; a resource with `deletionTimestamp` 30s ago
  (< threshold) → NOT flagged; a resource with no `deletionTimestamp` → NOT
  flagged; sorted output (Kind, Namespace, Name); a namespace with no blocking
  condition → `finalizers <spec.finalizers>`.
- **`collect`:** `Namespaces` via fake clientset returns the seeded namespace.
- **`scan` integration:** a fake clientset with a Terminating namespace (old
  `deletionTimestamp` + condition) yields `Result.StuckTerminating` with it; a
  forbidden namespaces reactor still returns pod/PVC stuck issues and no error.
- **`report`:** the three kinds render with the `✗ … Terminating <age>` +
  `⚠ StuckTerminating:` lines; `(past grace)` only on pods; the section is absent
  when empty; the attention line shows `N resource(s) stuck terminating`.
- **`watch`:** the gauge reflects a sample Result.
- **Golden:** add one stuck namespace + one stuck pod to the fixture; regenerate;
  extend the attention line and `TestGoldenInputCoversAllSections`.
- **Helm/RBAC:** `helm template` shows `namespaces` in the (single) core rule;
  `helm lint` clean.

## Files touched

- **Create:** `internal/termhealth/termhealth.go` (+ test).
- **Modify:** `internal/collect/collect.go` (+ test) — `Namespaces`.
- **Modify:** `internal/scan/scan.go` (+ test) — wiring + `Result.StuckTerminating`.
- **Modify:** `internal/report/report.go` (+ test) — section + attention line + JSON.
- **Modify:** `internal/watch/metrics.go` (+ test) — the gauge.
- **Modify:** `deploy/rbac.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml` — `namespaces`.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt`.
- **Docs:** `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
