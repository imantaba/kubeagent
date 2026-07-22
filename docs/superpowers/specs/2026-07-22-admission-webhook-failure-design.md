# Admission-webhook-failure check — design

**Status:** approved · **Date:** 2026-07-22 · **Type:** new detector (v1 core)

## Problem

An admission webhook (`ValidatingWebhookConfiguration` /
`MutatingWebhookConfiguration`) with `failurePolicy: Fail` whose backing Service
is gone or has no ready endpoints will **reject every create/update it
intercepts** — cluster-wide, silently. This is a classic "nothing can be
deployed anywhere, and `kubectl get` shows nothing wrong" outage (a crashed
policy controller, cert-manager/OPA/Kyverno down, a renamed backend Service).
kubeagent has no view of it today. This adds a read-only check that flags a
Fail-policy webhook whose backend is down and names the culprit.

kubeagent's existing `createhealth` check catches the *symptom* — a workload
already sitting below its replicas because a webhook denied its pods (from a
`FailedCreate` event). This check names the *cause* proactively, before any
workload fails.

## Behavior (approved)

Flagged in NEEDS ATTENTION (prominent, but — like the PDB and HPA checks —
**advisory** to the cluster verdict, which stays node/system-based):

```text
✗ policy-webhook  ValidatingWebhookConfiguration  webhook validate.policy.io
    ⚠ WebhookDown: backend Service kube-system/policy-svc has no ready endpoints — failurePolicy Fail rejects every intercepted create/update
✗ image-signing  MutatingWebhookConfiguration  webhook sign.example.com
    ⚠ WebhookDown: backend Service secure/signer does not exist — failurePolicy Fail rejects every intercepted create/update
```

For every webhook entry across both config kinds, flag it when **all** hold:

- `failurePolicy == Fail` **or** `failurePolicy` is unset (the
  `admissionregistration.k8s.io/v1` default is `Fail`), **and**
- `clientConfig.service != nil` (URL-based webhooks are skipped — their
  reachability can't be checked read-only), **and**
- the backend is down, per the first matching problem:

| Problem | Condition | Reason |
|---|---|---|
| `missing-service` | the referenced Service is not among the collected Services | `backend Service <ns/name> does not exist — failurePolicy Fail rejects every intercepted create/update` |
| `no-endpoints` | the Service exists but `svchealth.ReadyEndpoints(svc, slices) == 0` | `backend Service <ns/name> has no ready endpoints — failurePolicy Fail rejects every intercepted create/update` |

**Deliberately NOT flagged:** a webhook with a healthy backend (≥1 ready
endpoint); `failurePolicy: Ignore` (a down Ignore-webhook does not block
operations); a URL-based webhook (no Service to resolve). Selectors
(`namespaceSelector` / `objectSelector`) are not evaluated — a down Fail-webhook
blocks every request it is configured to intercept regardless of scope, so the
finding stands on the backend state alone.

## Design

### 1. Collectors — two new (cluster-scoped)

```go
// ValidatingWebhookConfigurations lists all validating admission webhook configs
// (cluster-scoped; read-only). Needs the base admissionregistration.k8s.io grant.
func ValidatingWebhookConfigurations(ctx context.Context, client kubernetes.Interface) ([]admissionv1.ValidatingWebhookConfiguration, error) {
	list, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing validatingwebhookconfigurations: %w", err)
	}
	return list.Items, nil
}

// MutatingWebhookConfigurations lists all mutating admission webhook configs
// (cluster-scoped; read-only).
func MutatingWebhookConfigurations(ctx context.Context, client kubernetes.Interface) ([]admissionv1.MutatingWebhookConfiguration, error) {
	list, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing mutatingwebhookconfigurations: %w", err)
	}
	return list.Items, nil
}
```

Services and EndpointSlices are already collected (`collect.Services`,
`collect.EndpointSlices`).

### 2. `internal/webhookhealth` — pure assessment

```go
type Issue struct {
	Kind     string `json:"kind"`     // "ValidatingWebhookConfiguration" | "MutatingWebhookConfiguration"
	Config   string `json:"config"`   // the configuration object's name
	Webhook  string `json:"webhook"`  // the individual webhook's .name
	Service  string `json:"service"`  // "ns/name" of the backend
	Problem  string `json:"problem"`  // "missing-service" | "no-endpoints"
	Reason   string `json:"reason"`
}

// Assess flags admission webhooks whose failurePolicy is Fail and whose backend
// Service is missing or has no ready endpoints. Pure and deterministic: reads only
// the webhook configs and the collected Services/EndpointSlices. Results are sorted
// by (Kind, Config, Webhook).
func Assess(
	validating []admissionv1.ValidatingWebhookConfiguration,
	mutating []admissionv1.MutatingWebhookConfiguration,
	services []corev1.Service,
	slices []discoveryv1.EndpointSlice,
) []Issue
```

- A small internal `spec` struct normalizes a `ValidatingWebhook` and a
  `MutatingWebhook` (they share `.Name`, `.ClientConfig`, `.FailurePolicy` but are
  distinct Go types) so one `check` routine handles both loops.
- `failsClosed(fp *admissionv1.FailurePolicyType)` → `fp == nil || *fp ==
  admissionv1.Fail`.
- Backend resolution: find the collected Service matching
  `clientConfig.service.Namespace/Name`; if absent → `missing-service`; else if
  `svchealth.ReadyEndpoints(svc, slices) == 0` → `no-endpoints`; else healthy
  (skip).
- Imports `admissionv1 "k8s.io/api/admissionregistration/v1"`, `corev1`,
  `discoveryv1`, the internal `svchealth` (for `ReadyEndpoints` — DRY; `svchealth`
  is itself pure and does not import `webhookhealth`, so no cycle), `fmt`, `sort`.

### 3. `scan.Evaluate` — wiring (cluster-wide only, graceful on forbidden)

```go
	var webhookIssues []webhookhealth.Issue
	if opts.Namespace == "" { // webhook backends can live in any namespace; only sound cluster-wide
		vwc, _ := collect.ValidatingWebhookConfigurations(ctx, client)
		mwc, _ := collect.MutatingWebhookConfigurations(ctx, client)
		webhookIssues = webhookhealth.Assess(vwc, mwc, svcs, slices)
	}
```

- `Result.WebhookIssues []webhookhealth.Issue` (nil/empty when nothing is down),
  added to the return literal.
- **Namespace-scope guard:** a `--namespace`-scoped scan collects only that
  namespace's Services, so a webhook backend elsewhere would look "missing" — a
  false positive. The check therefore runs only when `opts.Namespace == ""`. The
  collect errors are intentionally ignored (graceful degradation on a restricted
  kubeconfig, like the sibling advisory collectors).
- `svcs` and `slices` are the already-collected `collect.Services` /
  `collect.EndpointSlices` results (the service-health check's inputs).

### 4. `report` — a NEEDS ATTENTION rendering

Mirrors `printPDBIssues` / `printHPAIssues` (two lines):

```go
// printWebhookIssues lists admission webhooks that will reject every intercepted request.
func printWebhookIssues(issues []webhookhealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s  %s  webhook %s\n", is.Config, is.Kind, is.Webhook); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ WebhookDown: %s\n", is.Reason); err != nil {
			return err
		}
	}
	return nil
}
```

- `Input.WebhookIssues []webhookhealth.Issue`; JSON `webhookIssues,omitempty`.
- `hasAttention` includes `len(in.WebhookIssues) > 0`; the section prints after
  `printHPAIssues` in NEEDS ATTENTION.
- `attentionLine` gains `N admission webhook(s) failing`:
  `fmt.Sprintf("%d %s failing", n, plural(n, "admission webhook", "admission webhooks"))`
  (appended after the HPA fragment).

### 5. `main.go` — the render seam (already testable)

Add `WebhookIssues: res.WebhookIssues` to `resultInput(res scan.Result)` so the
field reaches the CLI report; the existing seam-test pattern
(`TestResultInput_Carries…`) covers it. **This is the wiring that
stuck-terminating originally missed — it must be in `resultInput`, not only the
`scan.Result` literal.**

### 6. `watch` — one gauge

`kubeagent_admission_webhooks_failing` = `len(res.WebhookIssues)` (mirrors
`kubeagent_hpa_scaling_issues`).

### 7. RBAC + Helm

Webhook configs live in the `admissionregistration.k8s.io` API group, not yet
granted. Add a **new base rule** (always-on; webhook metadata is not sensitive)
after the `autoscaling` rule in both `deploy/rbac.yaml` and
`deploy/helm/kubeagent/templates/clusterrole.yaml`:

```yaml
  - apiGroups: ["admissionregistration.k8s.io"]
    resources: [validatingwebhookconfigurations, mutatingwebhookconfigurations]
    verbs: [get, list, watch]
```

## Global constraints

- **Read-only; always-on; no flag.** One new base RBAC grant
  (`admissionregistration.k8s.io`). Touches `internal/collect` + RBAC + Helm →
  **FULL CHAOS GATE** at release (plus a targeted live smoke: a Fail-policy
  webhook pointing at a Service with no endpoints, and one at a missing Service).
  **Minor** bump v0.36.0 → **v0.37.0**; **minor** chart bump (Helm template
  changed).
- **Pure & deterministic** — `webhookhealth.Assess` reads only the passed objects;
  sorted output; no clock, no cluster calls.
- **Advisory to the verdict** — does not flip Healthy/Degraded (consistent with
  the PVC, PDB, and HPA checks).
- **Cluster-wide only** — the check is skipped under `--namespace` (see §3).
- `inventory`, `clusterhealth`, `explain.go` (WebhookIssues is a separate `Result`
  field, not passed to `--explain` — matches PDBIssues/HPAIssues), and `--fix`
  stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.**

## Out of scope (YAGNI)

Editing webhook configs (a write — read-only); probing a URL-based webhook's
reachability; evaluating `namespaceSelector` / `objectSelector` to predict which
requests are hit (a down Fail-webhook is flagged regardless); checking the
webhook's target `port`/`path` against the Service (the ready-endpoint signal is
enough); `ValidatingAdmissionPolicy` (CEL, no backend Service — a different
feature); `failurePolicy: Ignore` webhooks; an opt-in flag; `--explain`
integration; per-finding confidence tags (sibling Assess lists don't carry one).

## Testing

- **`webhookhealth` (pure, fake objects):**
  - `no-endpoints`: a Fail validating webhook → Service `kube-system/policy-svc`
    with an EndpointSlice of zero ready addresses → one Issue, reason names the
    Service + "has no ready endpoints", `Kind ValidatingWebhookConfiguration`.
  - `missing-service`: a Fail mutating webhook → Service `secure/signer` absent
    from the collected Services → reason "does not exist",
    `Kind MutatingWebhookConfiguration`.
  - `failurePolicy` nil → treated as Fail (flagged when backend down).
  - **Not flagged:** `failurePolicy: Ignore` with a down backend; a URL-based
    webhook (`clientConfig.service == nil`); a Fail webhook whose Service has a
    ready endpoint.
  - Sorted output (Kind, Config, Webhook); a config with multiple webhooks yields
    one Issue per down webhook.
- **`collect`:** `ValidatingWebhookConfigurations` / `MutatingWebhookConfigurations`
  via fake clientset return the seeded configs.
- **`scan` integration:** a fake clientset with a Fail webhook + an endpoint-less
  backend Service yields `Result.WebhookIssues`; a forbidden
  `validatingwebhookconfigurations` reactor returns no error and no issues; a
  `--namespace`-scoped scan (`Options{Namespace: "x"}`) yields no webhook issues
  even with a down webhook present (scope guard).
- **`report`:** the two problems render with the `✗ … webhook <name>` +
  `⚠ WebhookDown:` lines; the section is absent when empty; the attention line
  shows `N admission webhook(s) failing`.
- **`main` (seam):** `resultInput(scan.Result{WebhookIssues: …})` carries
  WebhookIssues into `report.Input` (mirrors `TestResultInput_CarriesHPAIssues`).
- **`watch`:** the gauge reflects a sample Result.
- **Golden:** add one `no-endpoints` webhook to the fixture; regenerate; extend
  the attention line and `TestGoldenInputCoversAllSections`.
- **Helm/RBAC:** `helm template` shows the `admissionregistration.k8s.io` rule;
  `helm lint` clean.

## Files touched

- **Create:** `internal/webhookhealth/webhookhealth.go` (+ test).
- **Modify:** `internal/collect/collect.go` (+ test) — the two collectors.
- **Modify:** `internal/scan/scan.go` (+ test) — wiring + `Result.WebhookIssues` + scope guard.
- **Modify:** `main.go` (+ `main_test.go`) — `resultInput` carries `WebhookIssues`.
- **Modify:** `internal/report/report.go` (+ test) — section + attention line + JSON.
- **Modify:** `internal/watch/metrics.go` (+ test) — the gauge.
- **Modify:** `deploy/rbac.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml` — `admissionregistration.k8s.io` rule.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt`.
- **Docs:** `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
