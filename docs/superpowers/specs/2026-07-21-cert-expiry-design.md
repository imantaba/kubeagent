# Certificate-expiry check (opt-in `--certs`) — design

**Status:** approved · **Date:** 2026-07-21 · **Type:** new opt-in check (v0.29–v0.31 detector block)

## Problem

An expired TLS certificate is the most preventable outage class there is: the
expiry date is known months in advance, yet it still takes sites down. kubeagent
has no view of it — certificates live in `kubernetes.io/tls` Secrets, and
kubeagent's RBAC deliberately excludes Secrets. This check adds that view as an
**opt-in**, with a strict privacy contract, mirroring the established
`--disk-usage` opt-in + RBAC add-on pattern.

## Behavior (approved)

A new advisory `CERTIFICATES` section — like `SECURITY`, it never affects the
cluster verdict:

```text
$ kubeagent scan --certs
CERTIFICATES  (advisory — public certificate metadata only)
  ✗ shop/shop-tls  EXPIRED 3d ago  (CN shop.example.com)
      — fronts ingress shop/storefront (shop.example.com)
  ⚠ infra/api-tls  expires in 12d  (CN api.example.com)
```

- **Expired** → `✗` with "EXPIRED Nd ago"; **expiring** within the warn window →
  `⚠` with "expires in Nd". Default window **30 days**, tunable
  `--cert-warn-days` (mirrors `--disk-threshold`).
- **Ingress cross-reference:** Ingresses' `spec.tls[].secretName` (objects
  already collected) mark which certs front live routes — rendered as a
  `— fronts ingress <ns>/<name> (<host>)` child line.
- A Secret of type `kubernetes.io/tls` whose `tls.crt` cannot be parsed is
  flagged `invalid certificate data` (a real misconfiguration).
- Healthy certs are counted, not listed (`N certificates checked` context line
  when the section renders; the section renders only when there is something to
  say or the grant is missing).

## Privacy contract (the heart of it)

- Lists **only** Secrets of `type: kubernetes.io/tls`, using the server-side
  field selector `type=kubernetes.io/tls` AND re-filtering by type in-code (the
  fake clientset ignores field selectors — established pattern).
- Parses **only `tls.crt`** — the **public** certificate; the **leaf** = first
  PEM block. **`tls.key` is never read, decoded, or held** — the code never
  touches that map key.
- Reports metadata only: namespace/name, CN (or first DNS SAN when CN is empty),
  NotAfter, days remaining, fronting Ingresses. This is the same class of data
  any TLS client sees. Only this metadata flows to JSON and `--explain`.
- **Without `--certs`, kubeagent makes NO Secrets API call at all** — the
  default posture is unchanged.

## Design

### 1. `collect.TLSSecrets` — the one new collector

```go
// TLSSecrets lists the kubernetes.io/tls Secrets in the namespace ("" = all) —
// public certificate material for the opt-in --certs check. The type field
// selector narrows server-side; certhealth re-filters by type in-code.
func TLSSecrets(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Secret, error) {
	secrets, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=kubernetes.io/tls"})
	if err != nil {
		return nil, fmt.Errorf("listing TLS secrets: %w", err)
	}
	return secrets.Items, nil
}
```

### 2. `internal/certhealth` — pure assessment

```go
type Cert struct {
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	CommonName string   `json:"commonName"`          // CN, or the first DNS SAN when CN is empty
	NotAfter   string   `json:"notAfter"`            // RFC3339
	Days       int      `json:"days"`                // days until expiry; negative = days since expired
	Ingresses  []string `json:"ingresses,omitempty"` // "ns/name (host)" routes fronted by this cert
}

type Invalid struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Detail    string `json:"detail"` // "missing tls.crt" | "invalid certificate data"
}

type Report struct {
	Checked   int       `json:"checked"`             // TLS secrets examined
	WarnDays  int       `json:"warnDays"`
	Expired   []Cert    `json:"expired,omitempty"`
	Expiring  []Cert    `json:"expiring,omitempty"`  // 0 <= Days <= WarnDays
	Invalid   []Invalid `json:"invalid,omitempty"`
	Forbidden bool      `json:"forbidden,omitempty"` // secrets list was denied (missing the RBAC add-on)
}

// Assess parses the leaf certificate (first PEM block of tls.crt) of each
// kubernetes.io/tls Secret and classifies it against now + warnDays. It
// cross-references Ingress spec.tls[].secretName to name the routes each cert
// fronts. Pure and deterministic: injected now, results sorted by (namespace,
// name). tls.key is never touched.
func Assess(secrets []corev1.Secret, ingresses []networkingv1.Ingress, warnDays int, now time.Time) Report
```

Details: skip Secrets whose `Type != corev1.SecretTypeTLS` (in-code re-filter);
`Days = int(cert.NotAfter.Sub(now).Hours() / 24)` (floor toward zero is fine —
deterministic); Ingress cross-ref map built from
`ing.Spec.TLS[].SecretName` (same namespace as the Ingress), entries
`ing.Namespace + "/" + ing.Name + " (" + firstRuleHost + ")"` (host omitted
when the Ingress has no rule host), sorted.

### 3. `scan` wiring — flag-gated, forbidden-graceful

- `Options.Certs bool`, `Options.CertWarnDays int` (default 30).
- In `Evaluate`, only when `opts.Certs`:
  ```go
  var certReport *certhealth.Report
  if opts.Certs {
      tlsSecrets, err := collect.TLSSecrets(ctx, client, opts.Namespace)
      rep := certhealth.Assess(tlsSecrets, ings, opts.CertWarnDays, time.Now())
      if apierrors.IsForbidden(err) {
          rep.Forbidden = true
      }
      certReport = &rep
  }
  ```
  (`ings` already collected. Non-forbidden list errors: ignored like sibling
  advisory collectors — the report just shows 0 checked.)
- `Result.Certificates *certhealth.Report` (`json:"certificates,omitempty"`,
  nil when off).

### 4. `report` — the CERTIFICATES section

Mirrors `printKubeletHealth`: renders only when the report is non-nil AND has
something to say (expired/expiring/invalid non-empty, or `Forbidden`).
`Forbidden` prints the missing-grant hint:
`certificates: secrets access denied — apply deploy/rbac-certs.yaml (or Helm certs.enabled=true)`.
Order: expired (worst first by Days ascending), then expiring, then invalid,
then a `· N certificates checked (warn window Nd)` context line.

### 5. CLI + daemon parity (the disk-usage pattern, exactly)

- `main.go` scan flags: `--certs` (bool) + `--cert-warn-days` (int, 30); usage
  line updated.
- `main.go` watch env: `KUBEAGENT_CERTS` (bool) + `KUBEAGENT_CERT_WARN_DAYS`
  (int, 30) → `watch.Config.Certs/CertWarnDays` → `scan.Options`.
- `watch` metrics: gauges `kubeagent_certificates_expired` and
  `kubeagent_certificates_expiring` (counts; only updated when the check ran —
  guard on `res.Certificates != nil`, mirroring the disk-usage gauge guard).
- **RBAC add-on** `deploy/rbac-certs.yaml` (mirrors `rbac-diskusage.yaml`):
  ClusterRole `kubeagent-certs` with `resources: [secrets]`, `verbs: [list]` +
  binding to the kubeagent ServiceAccount. **NOT in the base ClusterRole.**
- **Helm:** `values.yaml` `certs: {enabled: false, warnDays: 30}`; clusterrole
  template adds the secrets rule under `{{- if .Values.certs.enabled }}`;
  deployment template sets the two env vars under the same guard.

## Global constraints

- **Opt-in only.** Default `scan`/`watch` behavior is byte-identical to today —
  no Secrets call, no RBAC change, no new output.
- **Pure & deterministic** — `certhealth.Assess` takes injected `now`; sorted
  output.
- Touches `internal/collect` + RBAC + Helm → **FULL CHAOS GATE** at release
  (plus a targeted live smoke: a Kind cluster with an expired and a
  soon-expiring cert). **Minor** bump v0.31.0 → **v0.32.0**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.**
- Existing checks (`credlint`, detectors, root-cause) untouched.

## Out of scope (YAGNI)

Live TLS dialing; cert-manager CRs (operator theme F); full-chain/CA
validation (leaf only); non-`kubernetes.io/tls` Secrets (e.g. Opaque secrets
holding certs); JKS/PKCS12 bundles; auto-renewal advice; root-cause attribution
from cert expiry (a later slice can join an expired cert to 5xx routes).

## Testing

- **`certhealth` (pure, generated test certs):** helper builds a self-signed
  cert with a chosen NotAfter (`crypto/x509` + `ecdsa`, deterministic inputs
  except key generation — generate per-test, assert on classification not
  bytes). Cases: expired (Days negative), inside warn window, healthy (beyond
  window, only counted), CN empty → first SAN used, missing `tls.crt` → Invalid,
  garbage `tls.crt` → Invalid, non-TLS-type secret skipped even if present
  (in-code filter), Ingress cross-ref (secretName match, same-namespace only,
  host rendered), sorted output, `tls.key` never accessed (construct the Secret
  WITHOUT a tls.key entry — Assess must work fine, proving no dependency).
- **`collect`:** `TLSSecrets` via fake clientset returns the seeded TLS secret.
- **`scan`:** flag-gating — `Options{Certs: false}` (default) performs NO
  secrets List (assert via a fake-clientset reaction/action list — the fake
  records actions; assert none is a `list secrets`); `Certs: true` yields a
  non-nil `Result.Certificates`; forbidden → `Forbidden: true` (fake reactor
  returning a Forbidden apierror).
- **`report`:** expired + expiring + forbidden renderings; section absent when
  the report is nil.
- **`watch`:** the two gauges reflect a sample Result (and are absent/stale-safe
  when `Certificates` is nil).
- **Golden:** add a `Certificates` report to the golden fixture (one expired
  cert fronting an ingress + one expiring) and regenerate; extend
  `TestGoldenInputCoversAllSections`.
- **Helm/RBAC:** `helm lint` + `helm template` with `certs.enabled=true` shows
  the secrets rule and env vars; without it, neither appears.

## Files touched

- **Create:** `internal/certhealth/certhealth.go` (+ test), `deploy/rbac-certs.yaml`.
- **Modify:** `internal/collect/collect.go` (+ test) — `TLSSecrets`.
- **Modify:** `internal/scan/scan.go` (+ test) — options + gated wiring + Result field.
- **Modify:** `internal/report/report.go` (+ test) — CERTIFICATES section; `report.Input`/JSON plumbing.
- **Modify:** `main.go` — scan flags + watch env + usage line.
- **Modify:** `internal/watch/watch.go` + `internal/watch/metrics.go` (+ test) — Config fields + gauges.
- **Modify:** `deploy/helm/kubeagent/values.yaml` + `templates/clusterrole.yaml` + `templates/deployment.yaml`; `deploy/README.md`.
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt`.
- **Docs:** `website/docs/features/diagnostics.md` (new opt-in subsection), `website/docs/features/watch-mode.md` (gauges + env), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
