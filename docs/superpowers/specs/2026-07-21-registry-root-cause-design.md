# Shared-registry root cause — design

**Status:** approved · **Date:** 2026-07-21 · **Type:** new feature (v0.29 root-cause theme, second slice)

## Problem

When a container registry has an incident — outage, expired pull credentials,
Docker Hub rate-limiting — every workload pulling from it starts failing at once,
and kubeagent shows N independent `ImagePullBackOff` findings with nothing tying
them together. The just-shipped `RootCause` mechanism (node slice, v0.29.0) is
built for exactly this shape: name the one shared cause on each affected workload.

## Behavior (approved)

When **two or more distinct flagged workloads** carry an image-pull failure
(`ImagePullBackOff` or `ErrImagePull`) whose images resolve to the **same
registry host**, each is attributed to that registry:

```text
Needs attention: 4 workloads failing (3 ⇐ registry ghcr.io)

✗ shop/api  Deployment  0/2 Degraded
    ↳ likely caused by registry ghcr.io (3 workloads failing to pull)
    ⚠ ImagePullBackOff: Bad image reference or registry authentication
```

- **Threshold 2** — one workload failing a pull is far more likely a typo'd
  tag/name; two-plus simultaneously against one host is a shared-cause signal.
  The hedged **"likely caused by"** (same contract as the node slice) covers the
  rare N-simultaneous-typos case.
- **Precedence: node wins.** A workload already attributed to a hard-down node
  keeps that attribution (a pull failure on a kubelet-dead node is the node's
  fault); it is excluded from registry grouping entirely (both from being
  annotated AND from the group count N).
- `RootCause` string (fixed format, feeds the existing generic rendering):
  `registry <host> (<N> workloads failing to pull)` where N = group size.

## Design

### 1. `internal/rootcause` — a second annotator + the host parser

```go
// AnnotateRegistry sets w.RootCause = "registry <host> (<N> workloads failing to
// pull)" on each flagged, not-yet-attributed workload whose image-pull failure
// shares a registry host with at least one other such workload. Pure and
// deterministic (hosts processed in sorted order). Runs after Annotate (node
// attribution wins).
func AnnotateRegistry(workloads []inventory.Workload)
```

Logic (two passes, pure):
1. **Group:** for each workload — skip unless `w.Flagged()`, `w.RootCause == ""`,
   `w.Image != ""`, and it has a finding with `Issue` ∈ {`ImagePullBackOff`,
   `ErrImagePull`} — append its index to `groups[registryHost(w.Image)]`.
2. **Attribute:** for each host (sorted — determinism, though groups are disjoint
   by construction since a workload has one Image), if `len(group) >= 2`, set on
   every member: `RootCause = fmt.Sprintf("registry %s (%d workloads failing to
   pull)", host, len(group))`.

```go
// registryHost extracts the registry host from a container image reference using
// the standard rules: the first path segment is a registry iff it contains "." or
// ":" or is "localhost"; otherwise the image lives on Docker Hub ("docker.io").
func registryHost(image string) string {
	seg, _, found := strings.Cut(image, "/")
	if !found || (!strings.ContainsAny(seg, ".:") && seg != "localhost") {
		return "docker.io"
	}
	return seg
}
```

Examples: `ghcr.io/org/app:v1` → `ghcr.io`; `nginx:1.27` → `docker.io`;
`library/nginx` → `docker.io`; `registry.local:5000/app` → `registry.local:5000`;
`localhost/app` → `localhost`.

### 2. `scan.Evaluate` — one line

Immediately after the existing node attribution:

```go
	rootcause.Annotate(result.Workloads, health.DownNodes)
	rootcause.AnnotateRegistry(result.Workloads)
```

Ordering is load-bearing (node precedence).

### 3. `report` — one wording generalization

The `↳ likely caused by …` workload line and the single-cause attention rollup
(`(M ⇐ <cause>)` via `rootCauseNode` = prefix before `" ("`) already render the
new cause with **zero changes** (`registry ghcr.io (…)` → `registry ghcr.io`).
The only edit: the multi-cause attention form changes from the node-specific
`(M ⇐ K unhealthy nodes)` to the generic **`(M ⇐ K root causes)`** so nodes and
registries mix honestly. Comments on `rootCauseNode`/`attentionLine` updated to
say "cause" rather than "node".

### 4. What does not change

`inventory` (the `RootCause` field already exists), `clusterhealth`,
`printWorkload`, JSON shape (the existing `rootCause` field just gains a new
value, which flows to `--explain` — an image registry host, no secrets/IPs),
`internal/collect`, `internal/watch`, `explain.go`, RBAC, Helm.

## Global constraints

- **Read-only; NO new RBAC / no new collector / no flag** — everything derives
  from `Workload.Image` + `Findings`, already assembled. Not
  `internal/collect`/`cluster`/`watch` → **lightweight real-cluster smoke** gate;
  **minor** bump v0.29.0 → **v0.30.0**.
- **Pure & deterministic** — sorted host processing; fixed strings.
- **Always-on** — runs in `scan` and the `watch` daemon via `scan.Evaluate`.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.**

## Out of scope (YAGNI)

Init-container image refs (`Workload.Image` is the main image; an
`Init:ImagePullBackOff` workload is not grouped); per-registry error
classification (auth vs timeout vs rate-limit); mirror/alias awareness
(`docker.io` vs `registry-1.docker.io` treated as written); cordon/pressure node
causes; confidence scores.

## Testing

- **`registryHost` table test:** `ghcr.io/org/app:v1`→`ghcr.io`;
  `nginx:1.27`→`docker.io`; `library/nginx`→`docker.io`;
  `registry.local:5000/app`→`registry.local:5000`; `localhost/app`→`localhost`;
  bare `nginx`→`docker.io`.
- **`AnnotateRegistry`:** two flagged ghcr.io pull-failers → both attributed with
  N=2; a single pull-failer → NOT attributed (threshold); a workload already
  node-attributed → skipped AND excluded from N (group of {node-attributed, one
  other} → the other is alone → no attribution); a flagged workload with a
  non-pull finding → not grouped; not-flagged → skipped; two groups (ghcr.io ×2,
  quay.io ×2) → each attributed to its own host deterministically.
- **`report`:** mixed node+registry causes → attention line `(M ⇐ K root causes)`;
  single registry cause → `(M ⇐ registry ghcr.io)`.
- **`scan` integration:** fake clientset with two Deployments whose pods are
  `ImagePullBackOff` on `ghcr.io/...` images → both workloads carry the registry
  `RootCause` through `Evaluate`.
- **Golden:** add two compact ghcr.io pull-failing Deployments carrying
  `RootCause: "registry ghcr.io (2 workloads failing to pull)"`; the attention
  line becomes `13 workloads failing (6 ⇐ 3 root causes)` (the wording
  generalization also updates the existing multi-node text). Regenerate.

## Files touched

- **Modify:** `internal/rootcause/rootcause.go` (+ test) — `AnnotateRegistry`, `registryHost`.
- **Modify:** `internal/scan/scan.go` (+ test) — one wiring line.
- **Modify:** `internal/report/report.go` (+ test) — the multi-cause wording.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt`.
- **Docs:** `website/docs/features/diagnostics.md` (retitle `### Root-cause
  attribution (node)` → `### Root-cause attribution`, add the registry
  paragraph), `README.md` (extend the root-cause bullet), `CHANGELOG.md`
  (`### Added`), `website/docs/roadmap.md` (extend the Shipped bullet).
