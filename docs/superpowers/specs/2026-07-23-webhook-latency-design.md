# Admission-webhook latency risk — design

**Status:** approved · **Date:** 2026-07-23 · **Type:** detector extension (Theme B, deeper diagnosis — closer #3 of 3, closes Theme B)

## Problem

kubeagent flags an admission webhook whose backend is down under `failurePolicy:
Fail` (v0.37.0, `internal/webhookhealth`), but never looks at **latency**. A
Fail-policy webhook with a high `timeoutSeconds` is a latency landmine: every
create/update it intercepts can block for up to `timeoutSeconds`, and if the
webhook is slow it stalls — then, because the policy is Fail, the operation is
**rejected**. Nothing reads `timeoutSeconds` today. This extends the existing
webhook check to flag a Fail-policy webhook whose `timeoutSeconds` is at or above a
threshold, closing the Theme-B admission-webhook line.

## Behavior (approved)

A Fail-policy webhook (validating or mutating) whose `timeoutSeconds` is **≥ 15**
(the default; env `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS` to tune) produces one Issue
in the existing WEBHOOK section, labelled `WebhookSlow`:

```text
WEBHOOK
  ✗ slow-validator  ValidatingWebhookConfiguration  webhook policy.example.com
      ⚠ WebhookSlow: timeoutSeconds 30 ≥ 15s under failurePolicy Fail — a slow webhook blocks every intercepted create/update for up to 30s, then rejects it
```

- **Scope:** only `failurePolicy: Fail` (or nil → Fail). Ignore-policy webhooks are
  a deliberate "don't block" choice and are not flagged.
- **Threshold:** `timeoutSeconds ≥ 15` by default; `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS`
  overrides (a value ≤ 0 falls back to 15). `timeoutSeconds` in Kubernetes ranges
  1–30; a threshold above 30 simply never matches.
- **No double-report:** a webhook whose backend is already flagged (missing-service
  / no-endpoints) is **not** additionally flagged for latency — the backend outage
  is the headline. A healthy-but-slow webhook is flagged for latency alone.
- **URL webhooks:** a Fail-policy webhook that uses a `url:` client config (no
  Service) is not backend-checkable but **is** latency-checkable — the existing
  code skips URL webhooks for the backend check; the latency check covers them.
- **Advisory** — the WEBHOOK section does not change the cluster verdict (unchanged
  from the existing webhook check).
- **Nil `timeoutSeconds`:** the API server defaults an unset `timeoutSeconds` to 10
  at admission time, but the stored object may have it nil. A nil value is treated
  as "not high" (not flagged) — we only flag an explicitly-set high value.

## Design

### 1. `internal/webhookhealth` — extend `Assess`

- The `hook` struct gains `timeout *int32`:
  ```go
  type hook struct {
      kind    string
      config  string
      name    string
      fp      *admissionv1.FailurePolicyType
      service *admissionv1.ServiceReference
      timeout *int32
  }
  ```
- Both hook-building literals pass `w.TimeoutSeconds` as the final field.
- `Assess` gains a `timeoutThreshold int32` parameter (last param):
  ```go
  func Assess(
      validating []admissionv1.ValidatingWebhookConfiguration,
      mutating []admissionv1.MutatingWebhookConfiguration,
      services []corev1.Service,
      slices []discoveryv1.EndpointSlice,
      timeoutThreshold int32,
  ) []Issue
  ```
- The loop is restructured (the current early `continue` on `h.service == nil`
  becomes conditional so URL webhooks reach the latency check):
  ```go
  for _, h := range hooks {
      if !failsClosed(h.fp) {
          continue // Ignore policy
      }
      backendFlagged := false
      if h.service != nil {
          id := h.service.Namespace + "/" + h.service.Name
          svc, found := findService(services, h.service.Namespace, h.service.Name)
          switch {
          case !found:
              out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "missing-service", Reason: ...})
              backendFlagged = true
          case svchealth.ReadyEndpoints(svc, slices) == 0:
              out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "no-endpoints", Reason: ...})
              backendFlagged = true
          }
      }
      if !backendFlagged && h.timeout != nil && *h.timeout >= timeoutThreshold {
          id := ""
          if h.service != nil {
              id = h.service.Namespace + "/" + h.service.Name
          }
          out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "high-timeout",
              Reason: fmt.Sprintf("timeoutSeconds %d ≥ %ds under failurePolicy Fail — a slow webhook blocks every intercepted create/update for up to %ds, then rejects it", *h.timeout, timeoutThreshold, *h.timeout)})
      }
  }
  ```
  (The existing `missing-service`/`no-endpoints` Reason strings are unchanged. The
  sort by (Kind, Config, Webhook) is unchanged.)
- The package doc comment is updated to mention the latency risk.

### 2. `scan.Evaluate` — pass the threshold

- Add `WebhookTimeoutThreshold int32` to `scan.Options`.
- At the `webhookhealth.Assess(...)` call, pass a clamped threshold:
  ```go
  webhookThreshold := opts.WebhookTimeoutThreshold
  if webhookThreshold <= 0 {
      webhookThreshold = 15
  }
  webhookIssues = webhookhealth.Assess(vwc, mwc, svcs, slices, webhookThreshold)
  ```
  (The webhook check is cluster-wide only — this is inside the existing
  `if opts.Namespace == ""` guard, unchanged.)

### 3. `report` — the `WebhookSlow` label

`printWebhookIssues` branches the label by `Problem`:
```go
func printWebhookIssues(issues []webhookhealth.Issue, w io.Writer) error {
    for _, is := range issues {
        if _, err := fmt.Fprintf(w, "  ✗ %s  %s  webhook %s\n", is.Config, is.Kind, is.Webhook); err != nil {
            return err
        }
        label := "WebhookDown"
        if is.Problem == "high-timeout" {
            label = "WebhookSlow"
        }
        if _, err := fmt.Fprintf(w, "      ⚠ %s: %s\n", label, is.Reason); err != nil {
            return err
        }
    }
    return nil
}
```
(No struct/field change in `report`; the `WebhookIssues` list already carries the
new Problem. JSON is unchanged in shape.)

### 4. `main.go` — the threshold env

- CLI `scan.Options`: `WebhookTimeoutThreshold: int32(envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15))`.
- Daemon: `watch.Config` gains `WebhookTimeoutThreshold int32`; `main` sets it from
  the same env; `internal/watch`'s `scan.Options` sets
  `WebhookTimeoutThreshold: cfg.WebhookTimeoutThreshold`.
  (If `envInt` returns an `int`, cast to `int32`.)

### 5. `watch` — partition the gauge

The existing `kubeagent_admission_webhooks_failing` gauge counts `len(WebhookIssues)`.
Partition so its meaning (backend failures) is preserved and latency risks get
their own gauge:
```go
failing, latency := 0, 0
for _, i := range res.WebhookIssues {
    if i.Problem == "high-timeout" {
        latency++
    } else {
        failing++
    }
}
m.webhooksFailing = failing
m.webhookLatencyRisks = latency
```
Add the field `webhookLatencyRisks int` and the gauge:
```go
gauge("kubeagent_admission_webhook_latency_risks", "Fail-policy admission webhooks with a high timeoutSeconds (a latency landmine)", float64(m.webhookLatencyRisks))
```

### 6. Helm — the threshold value

- `deploy/helm/kubeagent/values.yaml`: add `webhookLatency: { timeoutThreshold: 15 }`.
- `deploy/helm/kubeagent/templates/deployment.yaml`: render the env
  **unconditionally** (the check is always-on), e.g. in the container `env:` list
  outside the opt-in `{{- if or … }}` block — a `- name: KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS
  value: {{ .Values.webhookLatency.timeoutThreshold | quote }}` entry. (If the
  container currently has no unconditional `env:` entries, add the entry so the
  daemon honours the value; ensure the `env:` key renders even when no opt-in flags
  are set — i.e. the entry lives outside the `{{- if or … }}` guard.)

## Global constraints

- **Read-only; always-on; no new flag/collector/RBAC/package.** The webhook configs
  and Services/EndpointSlices are already collected. Env-only tuning knob.
- **Gate:** touches `internal/watch` (gauge) + a Helm template → **FULL CHAOS GATE**.
  **Minor** bump v0.46.0 → **v0.47.0**; **chart MINOR** bump (deployment/values
  templates changed).
- **Pure & deterministic** — `Assess` reads only its arguments; no clock, no cluster
  calls. Sorted output unchanged.
- **Advisory** — no cluster-verdict change.
- **No double-report** — backend-flagged webhooks are not also latency-flagged.
- `confidence`, `inventory`, `clusterhealth`, `rootcause`, `explain.go`, `--fix`,
  and the other detectors/Assess checks stay **unchanged**. The existing
  `kubeagent_admission_webhooks_failing` gauge keeps its backend-only meaning.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Measured latency via apiserver admission metrics (`apiserver_admission_webhook_
admission_duration_seconds` p99, `..._fail_open_count_total`) — the real p99/fail-open
signal; a later opt-in `/metrics`-probe slice; Ignore-policy webhooks (a deliberate
non-blocking choice); per-operation / per-resource timeout analysis; a `--fix` to
lower the timeout (read-only diagnosis); wiring webhook latency into the cluster
verdict (advisory).

## Testing

- **`webhookhealth.Assess` (pure, fake objects):**
  - a Fail-policy validating webhook with `timeoutSeconds 30`, threshold 15, backend
    healthy → one Issue `Problem "high-timeout"`, Reason contains `timeoutSeconds 30`
    and `≥ 15s`.
  - `timeoutSeconds 14`, threshold 15 → not flagged.
  - `timeoutSeconds 15` (exactly at threshold) → flagged (inclusive).
  - Ignore-policy webhook with `timeoutSeconds 30` → not flagged.
  - nil `timeoutSeconds` → not flagged.
  - backend down (missing-service) AND `timeoutSeconds 30` → exactly one Issue,
    `Problem "missing-service"` (no double-report; no high-timeout Issue).
  - URL-based Fail webhook (`service == nil`) with `timeoutSeconds 30` → flagged
    `high-timeout` (Service field empty).
  - a mutating webhook variant → same behavior (both kinds covered).
  - threshold parameter respected: same webhook with threshold 31 → not flagged.
- **`scan` integration:** the clamp — `Evaluate` with `Options{WebhookTimeoutThreshold: 0}`
  uses 15 (a `timeoutSeconds 20` Fail webhook is flagged); confirmed via a fake
  clientset with one high-timeout webhook config → `Result.WebhookIssues` contains a
  `high-timeout` Issue.
- **`report`:** a `high-timeout` Issue renders `⚠ WebhookSlow: …`; a `no-endpoints`
  Issue still renders `⚠ WebhookDown: …` (label branch, existing behavior intact).
- **`watch` gauge:** a `Result.WebhookIssues` with one `high-timeout` and one
  `no-endpoints` renders `kubeagent_admission_webhooks_failing 1` **and**
  `kubeagent_admission_webhook_latency_risks 1` (partition correct).
- **`main`:** `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS` is read into the Options (a focused
  test that `int32(envInt(...))` wiring compiles/executes, or coverage via the scan
  integration default).
- **Golden:** add a `high-timeout` webhook Issue to the golden fixture's
  `WebhookIssues`; regenerate; the snapshot shows the `WebhookSlow` line. The
  existing backend webhook fixture (if present) still renders `WebhookDown`.

## Files touched

- **Modify:** `internal/webhookhealth/webhookhealth.go` (+ test) — `hook.timeout`, `Assess` threshold param + latency branch.
- **Modify:** `internal/scan/scan.go` (+ test) — `Options.WebhookTimeoutThreshold`, clamped Assess call.
- **Modify:** `internal/report/report.go` (+ test) — `WebhookSlow` label branch.
- **Modify:** `main.go` (+ `main_test.go`) — the threshold env (CLI + daemon-config).
- **Modify:** `internal/watch/watch.go`, `internal/watch/metrics.go` (+ test) — threshold plumbing + the partitioned gauge.
- **Modify:** `deploy/helm/kubeagent/values.yaml`, `deploy/helm/kubeagent/templates/deployment.yaml` — the threshold value + env.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — a fixture high-timeout Issue + regenerate.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
