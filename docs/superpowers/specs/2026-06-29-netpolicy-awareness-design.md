# kubeagent — Design: NetworkPolicy awareness (gap-feature B)

**Status:** approved design (pre-implementation)
**Date:** 2026-06-29

## Goal

Turn the chaos test gap #4 from a bare symptom into a root-cause hint: when a
workload is degraded with no known detector cause, name the NetworkPolicies whose
podSelector matches its pods, so the operator knows where to look.

## Decisions (from brainstorming)

- **Trigger:** attach the hint only to a workload that is `Flagged()` AND has **no
  detector finding** (no CrashLoop/ImagePull/OOM/Pending) AND is selected by ≥1
  NetworkPolicy. This targets the "mysterious degraded / Running-but-not-Ready"
  case and avoids noise on failures whose cause is already known.
- **Depth:** list the **names** of the selecting NetworkPolicies. No default-deny
  special-casing, no rule/traffic analysis — kubeagent cannot know what traffic a
  pod needs, so it points at the policies to check.
- **No new params:** the hint is per-workload, so it rides on the `Workload`
  struct (like `Findings`), needing no signature change to `PrintInventory` /
  `ExplainInventory`.

## Invariants preserved

- **READ-ONLY:** one new List call (NetworkPolicies). Never mutate the cluster.
- **No new Go module dependency.** `k8s.io/api/networking/v1` is already used
  (IngressClass); selector matching uses `k8s.io/apimachinery/pkg/apis/meta/v1`
  `LabelSelectorAsSelector` + `k8s.io/apimachinery/pkg/labels`, already present.
- **Sequential**, stdlib `flag`, exit codes unchanged.
- **Namespace scope:** NetworkPolicies are namespaced, so the check honors the
  scan's `-n` scope (like workloads/services).
- **`--explain` egress:** only NetworkPolicy **names** are sent — never pod/endpoint
  IPs, raw specs, selectors with values, or secrets.
- **Best-effort:** a List failure in `main` is non-fatal (no policies → no hints).

## Architecture

```text
collect (+NetworkPolicies in scope)
inventory.Assemble → inventory.Prioritize
      → netpolicy.Annotate(result.Workloads, podLabels, policies)  ← sets Workload.NetworkPolicies
      → report / explain   (render the per-workload hint; no new params)
```

## Component 1 — `internal/inventory` (one field)

Add to `Workload`:

```go
NetworkPolicies []string `json:"networkPolicies,omitempty"` // names of NPs selecting this workload's pods (hint)
```

`inventory` itself never sets this field; `netpolicy.Annotate` populates it
(mirroring how `clusterhealth.ClusterHealth.ScopeNote` is set outside `Assess`).
It is rendered alongside `Findings`.

## Component 2 — collection (`internal/collect`)

```go
func NetworkPolicies(ctx context.Context, client kubernetes.Interface, namespace string) ([]networkingv1.NetworkPolicy, error)
```

`client.NetworkingV1().NetworkPolicies(namespace).List(...)`, wraps error, takes
the scan's `namespace` (empty = all).

## Component 3 — `internal/netpolicy` (pure)

```go
// Annotate sets w.NetworkPolicies for each workload that is flagged with no
// detector finding and whose pods are selected by one or more NetworkPolicies in
// the same namespace. podLabels maps "namespace/podName" -> pod labels. Mutates
// the slice elements in place.
func Annotate(workloads []inventory.Workload, podLabels map[string]map[string]string, policies []networkingv1.NetworkPolicy)
```

Per workload `w` at index `i`:
- Skip unless `w.Flagged() && len(w.Findings) == 0`.
- For each policy `p` with `p.Namespace == w.Namespace`:
  - `sel, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)`; skip the
    policy on `err`. An empty podSelector yields a selector matching everything
    (so a deny-all selecting all pods matches).
  - The policy selects the workload if, for any `pr` in `w.Pods`,
    `sel.Matches(labels.Set(podLabels[w.Namespace+"/"+pr.Name]))` is true.
  - On a match, add `p.Name` to a set.
- If the set is non-empty, assign the sorted, de-duplicated names to
  `workloads[i].NetworkPolicies`.

Helper `selectingPolicies(w inventory.Workload, podLabels ..., policies ...) []string`
keeps `Annotate` readable. A pod with no entry in `podLabels` is treated as having
empty labels (still matched by an empty/everything selector).

## Component 4 — rendering (no signature changes)

### text (`internal/report`, `printWorkload`)

After the `Findings` loop, when `len(wl.NetworkPolicies) > 0`:

```text
    ⚠ NetworkPolicy: pods selected by deny-all, web-allow — may be blocking traffic
```

(names joined by `, `).

### json (`internal/report`)

No change needed — `Workload.NetworkPolicies` serializes automatically via the
existing workloads array.

### explain (`internal/explain`, workloads loop)

After a workload's findings, when `len(w.NetworkPolicies) > 0`:

```text
    network policy: pods selected by deny-all, web-allow (possible cause)
```

Only the names are emitted (egress-safe). No change to `ExplainInventory` /
`buildInventoryPrompt` signatures.

## Component 5 — wiring (`main.go`)

After `result := inventory.Prioritize(...)` and before the explain/report calls:

```go
nps, _ := collect.NetworkPolicies(context.Background(), client, namespace)
podLabels := make(map[string]map[string]string, len(inputs.Pods))
for _, p := range inputs.Pods {
	podLabels[p.Namespace+"/"+p.Name] = p.Labels
}
netpolicy.Annotate(result.Workloads, podLabels, nps)
```

## Testing (TDD)

- `netpolicy.Annotate` — table tests: flagged + no findings + selected → names set
  (sorted, deduped); flagged + has a finding (e.g. OOM) → not set; healthy → not
  set; empty-podSelector policy matches all pods; label mismatch → not set;
  policy in another namespace → not set; multiple policies → sorted names;
  malformed selector → skipped.
- `collect.NetworkPolicies` — via the fake clientset; namespace scoping.
- `report` — the NP hint sub-line renders when `NetworkPolicies` is set and is
  absent otherwise.
- `explain` — the prompt includes the NP hint when set; egress guard (only names,
  no pod IPs / selectors-with-values).
- `main` — wiring stays green; List failure is non-fatal.

## Out of scope (explicit non-goals)

- Analyzing NetworkPolicy ingress/egress rules or whether they actually block the
  workload's needed traffic (kubeagent cannot know the needed traffic).
- Default-deny special-casing or severity grading of policies.
- Cross-namespace policy effects, `namespaceSelector`, or CNI enforcement checks
  (kindnet, for example, does not enforce NetworkPolicies at all).
- Attaching hints to workloads that already have a detector finding, or to healthy
  workloads.
