# DNS / CoreDNS resolution health (`--dns-health`) — design

**Status:** approved · **Date:** 2026-07-23 · **Type:** new opt-in check (Theme B, deeper diagnosis — closer #2 of 3)

## Problem

kubeagent diagnoses cluster DNS only as "is the CoreDNS pod running": `clusterhealth`
flags a degraded `coredns` Deployment via the generic kube-system workload rollup,
and `svchealth` would flag a `kube-dns` Service with no ready endpoints like any
other Service. But **DNS that is up but failing to resolve** — a SERVFAIL storm, a
broken upstream `forward`, a Corefile misconfiguration — is invisible today.
Nothing reads DNS resolution health. This adds an opt-in probe of CoreDNS's own
Prometheus metrics that flags an elevated SERVFAIL/REFUSED response ratio, mirroring
the `--control-plane-health` / `--kubelet-health` opt-in-probe pattern.

## Behavior (approved)

With `--dns-health`, kubeagent finds the CoreDNS pods, reads each pod's
`:9153/metrics`, aggregates the DNS response counts by rcode, and — when the
error ratio is elevated over a volume floor — prints a `DNS` section:

```text
DNS  (opt-in)
  ✗ cluster DNS is failing to resolve
      ⚠ CoreDNS SERVFAIL+REFUSED ratio 12.3% (1234/10000 responses across 2 pods)
```

- **Probed only when `--dns-health` is set** (opt-in). Off by default → default
  output unchanged.
- **Error set:** `SERVFAIL + REFUSED` (server-side / policy failures). `NXDOMAIN`
  and `NOERROR` are legitimate outcomes and are NOT counted as errors.
- **Threshold:** error ratio ≥ **0.05** (env-tunable `KUBEAGENT_DNS_SERVFAIL_RATIO`,
  clamped to `(0,1]`, default 0.05), evaluated only when total responses ≥ a
  **fixed floor of 100** (so a handful of early bad lookups can't trip it).
- **Statuses:** `ok` (silent), `degraded` (ratio ≥ threshold over the floor — the
  finding above), `forbidden` (all probes 401/403 — the `pods/proxy` grant is
  missing; a one-line hint), `unreachable` (all probes transport-failed), `""` (not
  probed, or no CoreDNS pods found — silent).
- **Advisory** — the `DNS` section appears but does **not** change the cluster
  verdict (consistent with the other opt-in probes; the metric drives alerting).

## Design

### 1. `internal/dnshealth` — pure parse + assess (new package)

```go
// Report is the advisory CoreDNS resolution-health result.
type Report struct {
    Status         string  `json:"status"`         // "ok" | "degraded" | "forbidden" | "unreachable" | ""
    ServfailRatio  float64 `json:"servfailRatio"`  // (SERVFAIL+REFUSED)/total, 0 when not degraded/unknown
    ErrorResponses int64   `json:"errorResponses"` // SERVFAIL + REFUSED
    TotalResponses int64   `json:"totalResponses"`
    PodsProbed     int     `json:"podsProbed"`
    Detail         string  `json:"detail,omitempty"`
}

// ParseResponses sums CoreDNS DNS response counts by rcode from one pod's
// /metrics body. It reads BOTH the current metric name (coredns_dns_responses_total)
// and the pre-1.7 name (coredns_dns_response_rcode_count_total) for version
// robustness. Returns rcode → summed count.
func ParseResponses(body []byte) map[string]int64

// Assess collapses the aggregated rcode counts (summed across all probed pods)
// plus the per-pod probe outcome counts into a Report. threshold is the error
// ratio that trips "degraded"; floor is the minimum total responses required to
// judge. Pure and deterministic.
func Assess(agg map[string]int64, podsProbed, forbidden, unreachable int, threshold float64, floor int64) Report
```

- **`ParseResponses`**: for each non-comment line beginning with either
  `coredns_dns_responses_total{` or `coredns_dns_response_rcode_count_total{`,
  extract the `rcode="X"` label value and the trailing numeric sample; add it to
  `map[rcode] += value`. Lines without an `rcode` label or without a parseable
  value are skipped. A `#`-comment or blank line is skipped. Pure; imports
  `strings`/`strconv` only.
- **`Assess` classification** (in order):
  - `podsProbed == 0` → `Report{Status: ""}` (no CoreDNS — not applicable).
  - `forbidden > 0 && forbidden == podsProbed` → `Report{Status: "forbidden"}`.
  - `unreachable == podsProbed` (and none forbidden) → `Report{Status:
    "unreachable"}`.
  - else compute `total = sum(agg)`, `errors = agg["SERVFAIL"] + agg["REFUSED"]`,
    `ratio = errors/total` (guard `total == 0` → ratio 0):
    - `total < floor` → `Report{Status: "ok", TotalResponses: total, PodsProbed}`
      (insufficient volume).
    - `ratio >= threshold` → `Report{Status: "degraded", ServfailRatio: ratio,
      ErrorResponses: errors, TotalResponses: total, PodsProbed}`.
    - else `Report{Status: "ok", ServfailRatio: ratio, ErrorResponses: errors,
      TotalResponses: total, PodsProbed}`.
  - (If some pods were forbidden/unreachable but at least one returned metrics,
    the aggregated total drives the verdict; the partial-probe counts inform
    only the all-forbidden / all-unreachable short-circuits above.)

### 2. `collect.CoreDNSMetrics` — the probe

Mirrors `KubeletHealthz` / `ControlPlaneReadyz` (never returns an error):

```go
// CoreDNSMetrics fetches a CoreDNS pod's :9153/metrics via the pods/proxy
// subresource, returning the raw body and HTTP status code. Never returns an
// error (non-fatal). Needs the pods/proxy get grant; 401/403 → code surfaced to
// the caller as forbidden.
func CoreDNSMetrics(ctx context.Context, client kubernetes.Interface, namespace, pod string) (body []byte, code int) {
    var c int
    b, _ := client.CoreV1().RESTClient().Get().
        AbsPath(fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:9153/proxy/metrics", namespace, pod)).
        Do(ctx).StatusCode(&c).Raw()
    return b, c
}
```

### 3. `scan.Evaluate` — find CoreDNS pods, probe, assess

- Add `DNSHealth bool` and `DNSServfailRatio float64` to `scan.Options`.
- Add `DNS dnshealth.Report` to `Result`.
- When `opts.DNSHealth`:
  1. Select CoreDNS pods from the already-collected `inputs.Pods`: namespace
     `kube-system`, label `k8s-app == "kube-dns"` (the standard CoreDNS pod
     label), pod phase Running. (A helper `coreDNSPods([]corev1.Pod) []corev1.Pod`.)
  2. For each, `body, code := collect.CoreDNSMetrics(ctx, client, ns, name)`;
     classify each probe: `code == 401||403` → forbidden++, `code == 200` →
     `mergeCounts(agg, dnshealth.ParseResponses(body))`, else unreachable++.
  3. `result.DNS = dnshealth.Assess(agg, len(pods), forbidden, unreachable,
     ratio, 100)` where `ratio` is the clamped `opts.DNSServfailRatio` (≤0 or >1 →
     0.05).
- When off, `Result.DNS` is the zero `Report{}` (`Status == ""`).

### 4. `report` — the `DNS` section

- Add `DNS *dnshealth.Report` (pointer, mirroring `KubeletHealth`) to `Input` and
  the JSON `inventoryReport` (`json:"dns,omitempty"`); copy in the JSON-build
  literal.
- `dnsRenders(p)` = `p != nil && (Status=="degraded" || Status=="forbidden")`.
- `printDNSHealth(p, w)`:
  - `degraded` → `DNS  (opt-in)` header, `  ✗ cluster DNS is failing to resolve`,
    `      ⚠ CoreDNS SERVFAIL+REFUSED ratio <pct>% (<errors>/<total> responses
    across <pods> pods)` (pct = `round(ratio*100*10)/10`, one decimal).
  - `forbidden` → `DNS  (opt-in)` header + `  ⚠ CoreDNS /metrics forbidden — grant
    pods/proxy to enable this check`.
  - `ok`/`unreachable`/`""`/nil → nothing.
- Call `printDNSHealth` next to the CONTROL PLANE section; add `dnsRenders(in.DNS)`
  to the "No issues found" empty-cluster guard.
- **`main.go` extras block** maps `dnsRep` (pointer, nil unless the flag is on),
  mirroring `cpRep`.

### 5. `main.go` — flag, env, daemon plumbing

- `--dns-health` bool flag (usage string updated); `KUBEAGENT_DNS_HEALTH` (envBool)
  and `KUBEAGENT_DNS_SERVFAIL_RATIO` (envFloat, default 0.05) for the daemon path.
- CLI `scan.Options`: `DNSHealth: *dnsHealth`, `DNSServfailRatio: envFloat(
  "KUBEAGENT_DNS_SERVFAIL_RATIO", 0.05)`.
- `watch.Config` gains `DNSHealth bool` and `DNSServfailRatio float64`; the
  daemon's `scan.Options` sets both from `cfg`.

### 6. `watch` — the daemon gauge

`gauge("kubeagent_dns_servfail_ratio", "CoreDNS SERVFAIL+REFUSED response ratio
(0 when healthy or not probed)", res.DNS.ServfailRatio)`. (A float gauge — alert on
`> 0.05`.)

### 7. RBAC — a conditional `pods/proxy` grant

- `deploy/helm/kubeagent/templates/clusterrole.yaml`: a conditional rule gated by
  `.Values.dnsHealth.enabled`:
  ```yaml
  {{- if .Values.dnsHealth.enabled }}
  - apiGroups: [""]
    resources: [pods/proxy]
    verbs: [get]
  {{- end }}
  ```
  (Note: `pods/proxy`, distinct from the existing `nodes/proxy` block. If
  `dnsHealth` and disk-usage/kubelet-health are both enabled, both `nodes/proxy`
  and `pods/proxy` blocks render — that is correct, they are different
  subresources.)
- `deploy/helm/kubeagent/values.yaml`: `dnsHealth: { enabled: false }`.
- `deploy/helm/kubeagent/templates/deployment.yaml`: add `.Values.dnsHealth.enabled`
  to the env-block `{{- if or … }}` guard; add a `KUBEAGENT_DNS_HEALTH: "true"` env
  block gated by the value.
- `deploy/rbac-dnshealth.yaml`: a raw add-on granting `pods/proxy get`, mirroring
  `deploy/rbac-diskusage.yaml`.

## Global constraints

- **Read-only; opt-in; offline.** No writes, no LLM. Off by default → default output
  and golden snapshot unchanged.
- **Gate:** collect/RBAC/watch/Helm changes → **FULL CHAOS GATE**. **Minor** bump
  v0.45.0 → **v0.46.0**; **chart MINOR** bump.
- **Pure & deterministic** — `ParseResponses`/`Assess` read only their arguments;
  the collector is the only I/O and never errors (a failed probe → forbidden/
  unreachable, never a scan failure).
- **Advisory** — no cluster-verdict change.
- **Privacy** — CoreDNS `/metrics` are aggregate rcode counters; no query names,
  no client data. Safe to display.
- `confidence`, `inventory`, `clusterhealth`, `rootcause`, `explain.go`, `--fix`,
  and the existing detectors/Assess checks stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Instantaneous SERVFAIL *rate* (the cumulative ratio is the one-shot signal; a
rate needs `watch` state — a later Theme-E slice); actively resolving a name (would
require running a pod — a write); Corefile/ConfigMap validation (fragile, separate
slice); per-zone / per-upstream breakdown (aggregate ratio is the high-signal
summary); NXDOMAIN alerting (legitimate); kube-dns Service-endpoint availability
(already surfaced by `svchealth`); wiring DNS into the cluster verdict (advisory
for now).

## Testing

- **`dnshealth.ParseResponses` (pure):**
  - current metric name: two `coredns_dns_responses_total{...rcode="NOERROR"...} 900`
    / `...rcode="SERVFAIL"...} 100` lines → `{"NOERROR":900,"SERVFAIL":100}`.
  - legacy metric name `coredns_dns_response_rcode_count_total{...rcode="REFUSED"...} 5`
    → `{"REFUSED":5}` (proves version robustness).
  - multiple series for the same rcode (different `server`/`zone`) are summed.
  - `#`-comment / `# HELP` / blank / unrelated-metric lines are ignored; a line
    with no `rcode` label or an unparseable value is skipped without panic.
- **`dnshealth.Assess` (pure):**
  - degraded: `{"NOERROR":9000,"SERVFAIL":800,"REFUSED":200}` (10% errors),
    threshold 0.05, floor 100, pods 2 → `Status "degraded"`, `ServfailRatio` ≈ 0.10,
    `ErrorResponses` 1000, `TotalResponses` 10000, `PodsProbed` 2.
  - ok (under threshold): 2% errors → `Status "ok"`.
  - below floor: total 50 with 40% errors, floor 100 → `Status "ok"` (insufficient
    volume; not flagged).
  - no pods: `Assess(nil, 0, 0, 0, 0.05, 100)` → `Status ""`.
  - all forbidden: `Assess(nil, 2, 2, 0, …)` → `Status "forbidden"`.
  - all unreachable: `Assess(nil, 2, 0, 2, …)` → `Status "unreachable"`.
- **`collect.CoreDNSMetrics`:** returns a `(body, code)` pair without panicking
  (I/O covered by the chaos gate / live smoke; classification unit-covered).
- **`scan` integration:** `Evaluate` with `Options{}` (DNS off) → `Result.DNS.Status
  == ""` (off-by-default; no probe).
- **`report` render:** a `degraded` Report renders the `DNS` section with the ratio
  line; a `forbidden` Report renders the grant hint; `ok`/`""`/nil render nothing.
- **`main` seam:** the extras block sets `in.DNS` only when `--dns-health` is on;
  flag-parse test (`--dns-health --output bogus` → output-format error).
- **`watch` gauge:** a `Result` with `DNS.ServfailRatio == 0.12` renders
  `kubeagent_dns_servfail_ratio 0.12`.
- **Golden:** unchanged (opt-in; the default fixture sets no DNS).

## Files touched

- **Create:** `internal/dnshealth/dnshealth.go` (+ test).
- **Modify:** `internal/collect/collect.go` (+ test) — `CoreDNSMetrics`.
- **Modify:** `internal/scan/scan.go` (+ test) — Options, Result, the probe/aggregate/assess block, `coreDNSPods` helper.
- **Modify:** `internal/report/report.go` (+ test) — `Input.DNS`, `printDNSHealth`, `dnsRenders`.
- **Modify:** `main.go` (+ `main_test.go`) — flag, env, extras-block mapping.
- **Modify:** `internal/watch/watch.go`, `internal/watch/metrics.go` (+ test) — config plumbing + gauge.
- **Modify:** `deploy/helm/kubeagent/templates/clusterrole.yaml`, `values.yaml`, `templates/deployment.yaml`; **create** `deploy/rbac-dnshealth.yaml`.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
