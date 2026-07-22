# ResourceQuota near-exhaustion check — design

**Status:** approved · **Date:** 2026-07-23 · **Type:** new advisory check (Theme B, deeper diagnosis)

## Problem

kubeagent catches quota problems only **reactively**: when a namespace's
ResourceQuota already blocks pod creation, the controller emits a `FailedCreate`
event and the `createhealth` detector names it ("blocked by a ResourceQuota").
There is no **proactive** signal — a namespace sitting at 95% of its pod or CPU
quota is invisible until the next deploy wedges. ResourceQuota is never collected
or assessed today (only referenced as a cause string in `createhealth`/
`remediation`), and there is no RBAC grant for it. This adds a read-only,
deterministic early-warning check that flags a ResourceQuota whose usage is at or
near its hard limit, so the operator raises the quota before it blocks work.

## Behavior (approved)

Every ResourceQuota entry whose `used/hard` ratio is **≥ 0.90** is flagged, in its
own `RESOURCE QUOTA` block under NEEDS ATTENTION:

```text
⚠ RESOURCE QUOTA
    shop/compute  requests.cpu  exhausted   4/4 (100%)
    web/compute   pods          near limit  47/50 (94%)
```

- **Threshold:** fixed default **0.90**, overridable by the environment variable
  `KUBEAGENT_QUOTA_THRESHOLD` (a float in `(0,1]`). No new CLI flag — the check is
  always-on, like `pdbhealth`/`hpahealth`/`webhookhealth`.
- **Coverage:** **every** entry in the quota's `status.hard` is evaluated
  generically (pods, requests.cpu/memory, limits.cpu/memory, services, configmaps,
  secrets, persistentvolumeclaims, `count/*`, and extended/GPU resources) — the
  same used/hard ratio math for all. The 0.90 threshold suppresses low-signal
  cases.
- **Severity:** `exhausted` when `used >= hard` (ratio ≥ 1.0 — actively blocking
  new objects **now**); `near` when `threshold ≤ ratio < 1.0` (a warning). Output
  is sorted **exhausted-first**, then by ratio descending, then by
  (Namespace, Quota, Resource) for a deterministic order.
- **Advisory** — the block appears in NEEDS ATTENTION but does **not** change the
  cluster Healthy/Unhealthy verdict (a near-full quota is not itself an outage;
  when it actually blocks creation, that surfaces as a verdict-relevant
  `FailedCreate` finding on the affected workload). This mirrors the other Assess
  checks (PDB/HPA/service/PVC).

**False-positive / edge guards:**

- An entry whose `hard <= 0` is **skipped** — a `hard: 0` quota is a deliberate
  "none of this resource allowed" block, not exhaustion, and skipping it avoids a
  div-by-zero. (`used` cannot exceed a `hard: 0`.)
- An entry absent from `status.hard` is not evaluated (nothing to be near).
- A quota with usage below the threshold contributes no Issue.

## Design

### 1. `collect.ResourceQuotas` — the collector

A namespace-scoped List (respects `--namespace`), mirroring
`collect.PodDisruptionBudgets`:

```go
// ResourceQuotas lists ResourceQuotas in the namespace (all namespaces when
// namespace is ""). Read-only; needs the core-group resourcequotas grant.
func ResourceQuotas(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.ResourceQuota, error)
```

It uses `client.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{})`.
A forbidden/absent result returns the error to the caller, which handles it the
same way sibling optional collectors do (see wiring).

### 2. `internal/quotahealth` — the pure assessment (new package)

Mirrors `pdbhealth`/`hpahealth`: pure, read-only, deterministic.

```go
// Issue is one ResourceQuota entry at or over the usage threshold.
type Issue struct {
    Namespace string  `json:"namespace"`
    Quota     string  `json:"quota"`     // ResourceQuota object name
    Resource  string  `json:"resource"`  // e.g. "pods", "requests.cpu"
    Used      string  `json:"used"`      // Quantity.String(), e.g. "3800m"
    Hard      string  `json:"hard"`      // Quantity.String(), e.g. "4"
    Ratio     float64 `json:"ratio"`
    Severity  string  `json:"severity"`  // "exhausted" | "near"
}

// Assess flags each ResourceQuota status.hard entry whose used/hard ratio is
// >= threshold. Entries with hard <= 0 are skipped. Output is sorted
// exhausted-first, then ratio desc, then (Namespace, Quota, Resource).
func Assess(quotas []corev1.ResourceQuota, threshold float64) []Issue
```

- For each quota, for each `resName, hardQty` in `quota.Status.Hard`: read
  `usedQty := quota.Status.Used[resName]`; `hf := hardQty.AsApproximateFloat64()`;
  skip if `hf <= 0`; `ratio := usedQty.AsApproximateFloat64() / hf`; if
  `ratio >= threshold`, append an `Issue` with `Used: usedQty.String()`,
  `Hard: hardQty.String()`, `Severity` = `exhausted` when `ratio >= 1.0` else
  `near`.
- Sort: `exhausted` before `near`; within a severity, higher `Ratio` first; ties
  broken by `Namespace`, then `Quota`, then `Resource`.
- Imports `corev1 "k8s.io/api/core/v1"` and `sort`. `resource.Quantity` methods
  (`AsApproximateFloat64`, `String`) need no extra import (the type rides on the
  `corev1` maps).

### 3. `scan.Evaluate` — collect + assess + expose

- Collect: `quotas, _ := collect.ResourceQuotas(ctx, client, opts.Namespace)`
  (the same tolerant `_`-error pattern the other always-on optional collectors
  use; an RBAC-forbidden result yields an empty slice → no issues, never a hard
  error).
- Assess: `quotaIssues := quotahealth.Assess(quotas, quotaThreshold)` where
  `quotaThreshold` comes from `opts.QuotaThreshold` (defaulted to 0.90 in `main`).
- Add `QuotaIssues []quotahealth.Issue` to the `Result` struct and set it in the
  returned `Result{…}`.
- Add `QuotaThreshold float64` to `scan.Options`.

### 4. `report` — render the NEEDS ATTENTION block

- Add `QuotaIssues []quotahealth.Issue` to `report.Input`.
- A `printQuotaIssues` helper renders the `RESOURCE QUOTA` block, mirroring
  `printPDBIssues`: a header line, then one aligned row per Issue —
  `<ns>/<quota>  <resource>  <exhausted|near limit>  <used>/<hard> (<pct>%)`.
  `near` renders as the label `near limit`; `exhausted` as `exhausted`. `pct` is
  `round(ratio*100)`.
- **`main.go` `resultInput`**: map `in.QuotaIssues = res.QuotaIssues` — the
  Result-field → Input seam. (This is the step that, if missed, silently drops the
  section, as happened with stuck-terminating.)
- **`main.go`**: read the threshold — `QuotaThreshold: envFloat("KUBEAGENT_QUOTA_THRESHOLD", 0.90)` in the
  Options built in `run` (both the scan and any env-driven path), clamped to
  `(0,1]` (fall back to 0.90 if the env value is out of range).
- JSON: `quotaIssues` serializes for free via the struct tags.

### 5. `watch` — the daemon gauge

Add a gauge `kubeagent_resourcequota_issues` set to `len(result.QuotaIssues)` each
evaluation cycle, registered and updated alongside the existing
`kubeagent_pdb_blocking_issues`/`kubeagent_hpa_scaling_issues` gauges. Read-only;
no new watch behavior beyond the metric.

### 6. RBAC — grant `resourcequotas`

Append `resourcequotas` to the **existing** core-group (`apiGroups: [""]`)
`get/list/watch` rule in both `deploy/rbac.yaml` and
`deploy/helm/kubeagent/templates/clusterrole.yaml`. No new rule block — it joins
`pods, nodes, services, …, namespaces`.

## Global constraints

- **Read-only; always-on; no flag** (threshold via env only). Touches
  `internal/collect` (new collector) + a new `internal/quotahealth` package +
  `internal/scan` + `internal/report` + `main.go` + `internal/watch` + the RBAC
  manifests and Helm `clusterrole.yaml` template.
- **Gate:** collection/RBAC/watch/Helm-template changes → **FULL CHAOS GATE**
  (`./chaos/run.sh --recreate`). **Minor** bump v0.43.0 → **v0.44.0**; **chart
  MINOR** bump (the clusterrole template changed — override the bump script's
  default patch to a minor).
- **Pure & deterministic** — `Assess` reads only the passed quotas and threshold;
  no clock, no cluster calls. Sorted output.
- **Advisory** — no verdict change; the cluster verdict logic is untouched.
- `confidence`, `inventory`, `clusterhealth`, `rootcause`, `explain.go`, `--fix`,
  the existing detectors and Assess checks stay **unchanged** (this is an
  additional, independent Assess list).
- **Privacy** — ResourceQuota `used`/`hard` are counts and resource quantities, no
  secrets or pod contents; safe to display and to send to `--explain` if the
  explain payload includes advisory issues (follow whatever the existing Assess
  issues do — no new sensitive data).
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Per-`ResourceQuota` **scope selectors** (`scopeSelector`/`BestEffort`/`PriorityClass`
scoping — the used/hard ratio is correct regardless of scope; naming the scope is
noise for v1); a **CLI `--quota-threshold` flag** (env override is enough; a flag
can be added if asked); **trend/rate** ("filling up fast" — needs `watch` state,
a later Theme-E slice); correlating a near-full quota to the **specific workloads**
that would be blocked (the `FailedCreate` detector already names the workload once
it actually blocks); flagging **LimitRange** (a separate object with different
semantics — its own later slice); a **remediation `--suggest`** entry (these are
namespace-level Assess issues, not per-workload `Finding`s, so they do not flow
through `remediation.For`, matching PDB/HPA/service).

## Testing

- **`quotahealth.Assess` (pure, fake objects):**
  - `near`: a quota `pods` used 47 / hard 50 (0.94) with threshold 0.90 → one
    Issue, Severity `near`, Used `"47"`, Hard `"50"`, Ratio ≈ 0.94.
  - `exhausted`: `requests.cpu` used `4` / hard `4` → Severity `exhausted`, Ratio
    1.0; and used `5` / hard `4` (over-committed edge, if the API ever reports it)
    → `exhausted`.
  - sub-threshold not flagged: `pods` used 40 / hard 50 (0.80) → no Issue.
  - `hard <= 0` skipped: a `pods: "0"` entry (used 0) → no Issue (no div-by-zero).
  - generic resources: a `requests.cpu` (`"3800m"`/`"4"` = 0.95) and a
    `count/configmaps` (`"9"`/`"10"` = 0.90) both flagged with their exact
    Used/Hard strings — proves the check is resource-agnostic.
  - sort/precedence: a mix (one `exhausted` at 1.0, two `near` at 0.95 and 0.92)
    across two namespaces → `exhausted` first, then 0.95 before 0.92; ties by
    (Namespace, Quota, Resource).
  - empty input → empty (non-nil) slice.
- **`collect.ResourceQuotas` (fake clientset):** a namespace with two
  ResourceQuotas → both returned; namespace filter respected.
- **`scan` integration (fake clientset):** a namespace with a ResourceQuota at
  95% `pods` → `Result.QuotaIssues` contains it with Severity `near` (proves the
  collector is wired, the threshold default applies, and the Result field is set).
- **`report` render:** an `Input{QuotaIssues: …}` with one `exhausted` and one
  `near` renders the `RESOURCE QUOTA` block with the aligned rows and the correct
  labels/percentages; an empty `QuotaIssues` renders no block.
- **`main` seam:** a test that `resultInput` copies `QuotaIssues` from `Result` to
  `Input` (guards the drop-the-section regression).
- **`watch` gauge:** the daemon exposes `kubeagent_resourcequota_issues` equal to
  the number of quota issues (mirrors the existing PDB/HPA gauge tests).
- **Golden:** add a namespace ResourceQuota issue to the golden fixture Input;
  regenerate; the snapshot shows the new `RESOURCE QUOTA` block.

## Files touched

- **Create:** `internal/quotahealth/quotahealth.go` (+ test).
- **Modify:** `internal/collect/collect.go` (+ test) — `ResourceQuotas` collector.
- **Modify:** `internal/scan/scan.go` (+ test) — collect, assess, `Options.QuotaThreshold`, `Result.QuotaIssues`.
- **Modify:** `internal/report/report.go` (+ test) — `Input.QuotaIssues`, `printQuotaIssues`.
- **Modify:** `main.go` (+ `main_test.go`) — `resultInput` maps `QuotaIssues`; `KUBEAGENT_QUOTA_THRESHOLD` env (default 0.90, clamped).
- **Modify:** `internal/watch/*` (+ test) — `kubeagent_resourcequota_issues` gauge.
- **Modify:** `deploy/rbac.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml` — add `resourcequotas` to the core-group grant.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — a fixture quota issue + regenerate.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
