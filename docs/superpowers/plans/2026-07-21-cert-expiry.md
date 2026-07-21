# Certificate-Expiry Check (`--certs`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An opt-in `--certs` check that flags expired and soon-expiring TLS certificates (from `kubernetes.io/tls` Secrets, public `tls.crt` leaf only) in an advisory `CERTIFICATES` section, cross-referenced with the Ingress routes they front — with daemon parity (env + gauges) and a separate RBAC add-on.

**Architecture:** One new collector (`collect.TLSSecrets`, type-field-selected) feeds a pure `internal/certhealth.Assess(secrets, ingresses, warnDays, now)` producing a `Report`. `scan.Evaluate` runs it only when `Options.Certs` is set (forbidden → graceful flag); `report` renders the advisory section; `main.go`/`watch` add the flag/env/gauges; Helm/RBAC gain an opt-in `certs` add-on. Mirrors the `--disk-usage` opt-in pattern end to end.

**Tech Stack:** Go 1.26 standard library (`crypto/x509`, `encoding/pem`) + `k8s.io/api`. No new dependencies.

## Global Constraints

- **OPT-IN ONLY.** Without `--certs`/`KUBEAGENT_CERTS`, kubeagent makes **NO Secrets API call** and output is byte-identical to today (a test asserts no `list secrets` action by default).
- **Privacy:** only `type: kubernetes.io/tls` Secrets (field selector + in-code re-filter); only the **public** `tls.crt` **leaf** (first PEM block) parsed; **`tls.key` is never read** (the code never touches that map key; a test constructs Secrets WITHOUT `tls.key`); output/JSON carry metadata only (namespace/name, CN/SAN, NotAfter, days, fronting Ingresses).
- **Advisory** — never affects the cluster verdict. Default warn window **30 days** (`--cert-warn-days` / `KUBEAGENT_CERT_WARN_DAYS`).
- **Pure & deterministic** — `Assess` takes injected `now`; results sorted by (Days asc, namespace, name); Invalid by (namespace, name).
- **RBAC:** the `secrets` grant lives ONLY in the new add-on (`deploy/rbac-certs.yaml` / Helm `certs.enabled`) — **never** in the base ClusterRole. Missing grant → `Report.Forbidden` + a hint line, never a crash.
- **No `Co-Authored-By: Claude` trailer** (or any AI attribution) on any commit. **TDD.**
- Set PATH first every task: `export PATH=$PATH:/usr/local/go/bin`.
- Spec: [docs/superpowers/specs/2026-07-21-cert-expiry-design.md](../specs/2026-07-21-cert-expiry-design.md).
- Release gate (post-merge, controller-owned): touches `internal/collect` + RBAC/Helm → **FULL CHAOS GATE**; **minor** bump v0.31.0 → **v0.32.0**.

---

## File Structure

- **Create** `internal/certhealth/certhealth.go` (+ `certhealth_test.go`) — types + pure `Assess`.
- **Modify** `internal/collect/collect.go` (+ test) — `TLSSecrets`.
- **Modify** `internal/scan/scan.go` (+ test) — `Options.Certs`/`CertWarnDays`, gated wiring, `Result.Certificates`.
- **Modify** `internal/report/report.go` (+ test) — `Input.Certificates`, JSON field, `printCertificates`.
- **Modify** `main.go` — scan flags, usage line, report Input plumb, watch env (`envInt` helper), Config fields.
- **Modify** `internal/watch/watch.go` + `internal/watch/metrics.go` (+ `metrics_test.go`) — Config fields, opts plumb, two gauges.
- **Create** `deploy/rbac-certs.yaml`; **Modify** Helm `values.yaml`/`templates/clusterrole.yaml`/`templates/deployment.yaml`, `deploy/README.md`.
- **Modify** `internal/report/golden_test.go` + `testdata/golden-scan.txt`; docs (`diagnostics.md`, `watch-mode.md`, `README.md`, `CHANGELOG.md`, `roadmap.md`).

---

### Task 1: `internal/certhealth` — types + pure `Assess`

**Files:**
- Create: `internal/certhealth/certhealth.go`
- Test: `internal/certhealth/certhealth_test.go`

**Interfaces:**
- Produces (later tasks depend on these exactly):

```go
type Cert struct {
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	CommonName string   `json:"commonName"`          // CN, or the first DNS SAN when CN is empty
	NotAfter   string   `json:"notAfter"`            // RFC3339 (UTC)
	Days       int      `json:"days"`                // days until expiry; negative = days since expired
	Ingresses  []string `json:"ingresses,omitempty"` // "ns/name (host)" routes fronted by this cert
}

type Invalid struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Detail    string `json:"detail"` // "missing tls.crt" | "invalid certificate data"
}

type Report struct {
	Checked   int       `json:"checked"`
	WarnDays  int       `json:"warnDays"`
	Expired   []Cert    `json:"expired,omitempty"`
	Expiring  []Cert    `json:"expiring,omitempty"`
	Invalid   []Invalid `json:"invalid,omitempty"`
	Forbidden bool      `json:"forbidden,omitempty"`
}

func Assess(secrets []corev1.Secret, ingresses []networkingv1.Ingress, warnDays int, now time.Time) Report
```

- [ ] **Step 1: Write the failing tests**

Create `internal/certhealth/certhealth_test.go`:

```go
package certhealth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var now = time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)

// certPEM builds a self-signed certificate with the given CN/SANs and NotAfter.
func certPEM(t *testing.T, cn string, sans []string, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     sans,
		NotBefore:    notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// tlsSecret builds a kubernetes.io/tls Secret. Deliberately NO tls.key entry —
// Assess must never depend on the private key.
func tlsSecret(ns, name string, crt []byte) corev1.Secret {
	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": crt},
	}
}

func ingTLS(ns, name, host, secretName string) networkingv1.Ingress {
	return networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{
			TLS:   []networkingv1.IngressTLS{{SecretName: secretName}},
			Rules: []networkingv1.IngressRule{{Host: host}},
		},
	}
}

func TestAssess_ExpiredAndExpiringAndHealthy(t *testing.T) {
	secrets := []corev1.Secret{
		tlsSecret("shop", "shop-tls", certPEM(t, "shop.example.com", nil, now.Add(-3*24*time.Hour))),   // expired 3d
		tlsSecret("infra", "api-tls", certPEM(t, "api.example.com", nil, now.Add(12*24*time.Hour))),    // expires 12d
		tlsSecret("infra", "ok-tls", certPEM(t, "ok.example.com", nil, now.Add(200*24*time.Hour))),     // healthy
	}
	rep := Assess(secrets, nil, 30, now)
	if rep.Checked != 3 {
		t.Errorf("Checked = %d, want 3", rep.Checked)
	}
	if len(rep.Expired) != 1 || rep.Expired[0].Name != "shop-tls" || rep.Expired[0].Days != -3 {
		t.Errorf("Expired = %+v, want shop-tls Days=-3", rep.Expired)
	}
	if len(rep.Expiring) != 1 || rep.Expiring[0].Name != "api-tls" || rep.Expiring[0].Days != 12 {
		t.Errorf("Expiring = %+v, want api-tls Days=12", rep.Expiring)
	}
	if rep.Expired[0].CommonName != "shop.example.com" {
		t.Errorf("CommonName = %q", rep.Expired[0].CommonName)
	}
}

func TestAssess_SANUsedWhenCNEmpty(t *testing.T) {
	secrets := []corev1.Secret{tlsSecret("shop", "san-tls", certPEM(t, "", []string{"san.example.com"}, now.Add(5*24*time.Hour)))}
	rep := Assess(secrets, nil, 30, now)
	if len(rep.Expiring) != 1 || rep.Expiring[0].CommonName != "san.example.com" {
		t.Errorf("want first SAN as CommonName, got %+v", rep.Expiring)
	}
}

func TestAssess_InvalidAndMissingCrt(t *testing.T) {
	garbage := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "bad-tls"},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("not a certificate")}}
	missing := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "empty-tls"},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{}}
	rep := Assess([]corev1.Secret{garbage, missing}, nil, 30, now)
	if rep.Checked != 2 || len(rep.Invalid) != 2 {
		t.Fatalf("want 2 checked / 2 invalid, got %+v", rep)
	}
	// sorted by ns/name: bad-tls before empty-tls
	if rep.Invalid[0].Detail != "invalid certificate data" || rep.Invalid[1].Detail != "missing tls.crt" {
		t.Errorf("Invalid = %+v", rep.Invalid)
	}
}

func TestAssess_NonTLSTypeSkipped(t *testing.T) {
	opaque := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "opaque"},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"tls.crt": certPEM(t, "x", nil, now.Add(24*time.Hour))}}
	rep := Assess([]corev1.Secret{opaque}, nil, 30, now)
	if rep.Checked != 0 || len(rep.Expiring) != 0 {
		t.Errorf("an Opaque secret must be skipped even if it holds a cert, got %+v", rep)
	}
}

func TestAssess_IngressCrossReference(t *testing.T) {
	secrets := []corev1.Secret{tlsSecret("shop", "shop-tls", certPEM(t, "shop.example.com", nil, now.Add(-1*24*time.Hour)))}
	ings := []networkingv1.Ingress{
		ingTLS("shop", "storefront", "shop.example.com", "shop-tls"),
		ingTLS("other", "elsewhere", "x.example.com", "shop-tls"), // different namespace — must NOT match
	}
	rep := Assess(secrets, ings, 30, now)
	if len(rep.Expired) != 1 {
		t.Fatalf("want 1 expired, got %+v", rep)
	}
	want := []string{"shop/storefront (shop.example.com)"}
	got := rep.Expired[0].Ingresses
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("Ingresses = %v, want %v (same-namespace only)", got, want)
	}
}

func TestAssess_SortedWorstFirst(t *testing.T) {
	secrets := []corev1.Secret{
		tlsSecret("b", "later", certPEM(t, "b", nil, now.Add(20*24*time.Hour))),
		tlsSecret("a", "sooner", certPEM(t, "a", nil, now.Add(5*24*time.Hour))),
	}
	rep := Assess(secrets, nil, 30, now)
	if len(rep.Expiring) != 2 || rep.Expiring[0].Name != "sooner" {
		t.Errorf("expiring must sort soonest-first, got %+v", rep.Expiring)
	}
}

func TestAssess_HealthyOnlyNothingListed(t *testing.T) {
	rep := Assess([]corev1.Secret{tlsSecret("a", "ok", certPEM(t, "ok", nil, now.Add(300*24*time.Hour)))}, nil, 30, now)
	if rep.Checked != 1 || len(rep.Expired)+len(rep.Expiring)+len(rep.Invalid) != 0 {
		t.Errorf("healthy cert: counted only, got %+v", rep)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/certhealth`
Expected: FAIL — build error (package/types undefined).

- [ ] **Step 3: Implement**

Create `internal/certhealth/certhealth.go`:

```go
// Package certhealth flags expired and soon-expiring TLS certificates from
// kubernetes.io/tls Secrets. It parses ONLY the public certificate (the leaf =
// first PEM block of tls.crt) — the private key (tls.key) is never read — and
// reports metadata only: names, expiry dates, and the Ingress routes each cert
// fronts. Pure: the caller supplies the secrets, ingresses, warn window, and
// clock. Opt-in (--certs); advisory (never affects the cluster verdict).
package certhealth

import (
	"crypto/x509"
	"encoding/pem"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

// (the Cert, Invalid, and Report types exactly as in the Interfaces block above)

// Assess parses the leaf certificate of each kubernetes.io/tls Secret and
// classifies it against now + warnDays. Deterministic: injected clock; Expired
// and Expiring sorted by (Days asc, namespace, name); Invalid by (namespace, name).
func Assess(secrets []corev1.Secret, ingresses []networkingv1.Ingress, warnDays int, now time.Time) Report {
	rep := Report{WarnDays: warnDays}
	fronts := ingressFronts(ingresses)
	for _, s := range secrets {
		if s.Type != corev1.SecretTypeTLS {
			continue // in-code re-filter: the fake clientset ignores field selectors
		}
		rep.Checked++
		crt := s.Data["tls.crt"]
		if len(crt) == 0 {
			rep.Invalid = append(rep.Invalid, Invalid{Namespace: s.Namespace, Name: s.Name, Detail: "missing tls.crt"})
			continue
		}
		block, _ := pem.Decode(crt)
		if block == nil {
			rep.Invalid = append(rep.Invalid, Invalid{Namespace: s.Namespace, Name: s.Name, Detail: "invalid certificate data"})
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			rep.Invalid = append(rep.Invalid, Invalid{Namespace: s.Namespace, Name: s.Name, Detail: "invalid certificate data"})
			continue
		}
		name := cert.Subject.CommonName
		if name == "" && len(cert.DNSNames) > 0 {
			name = cert.DNSNames[0]
		}
		c := Cert{
			Namespace: s.Namespace, Name: s.Name, CommonName: name,
			NotAfter: cert.NotAfter.UTC().Format(time.RFC3339),
			Days:     int(cert.NotAfter.Sub(now).Hours() / 24),
			Ingresses: fronts[s.Namespace+"/"+s.Name],
		}
		switch {
		case c.Days < 0:
			rep.Expired = append(rep.Expired, c)
		case c.Days <= warnDays:
			rep.Expiring = append(rep.Expiring, c)
		}
	}
	sortCerts(rep.Expired)
	sortCerts(rep.Expiring)
	sort.Slice(rep.Invalid, func(i, j int) bool {
		if rep.Invalid[i].Namespace != rep.Invalid[j].Namespace {
			return rep.Invalid[i].Namespace < rep.Invalid[j].Namespace
		}
		return rep.Invalid[i].Name < rep.Invalid[j].Name
	})
	return rep
}

// sortCerts orders worst-first: fewest days left, then namespace/name.
func sortCerts(cs []Cert) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Days != cs[j].Days {
			return cs[i].Days < cs[j].Days
		}
		if cs[i].Namespace != cs[j].Namespace {
			return cs[i].Namespace < cs[j].Namespace
		}
		return cs[i].Name < cs[j].Name
	})
}

// ingressFronts maps "ns/secretName" to the sorted "ns/ingName (host)" routes
// referencing it via spec.tls (same-namespace by definition of Ingress TLS).
func ingressFronts(ings []networkingv1.Ingress) map[string][]string {
	out := map[string][]string{}
	for _, ing := range ings {
		label := ing.Namespace + "/" + ing.Name
		if len(ing.Spec.Rules) > 0 && ing.Spec.Rules[0].Host != "" {
			label += " (" + ing.Spec.Rules[0].Host + ")"
		}
		for _, t := range ing.Spec.TLS {
			if t.SecretName == "" {
				continue
			}
			key := ing.Namespace + "/" + t.SecretName
			out[key] = append(out[key], label)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}
```
(Write the three type declarations in full from the Interfaces block — no elision.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/certhealth -v && go vet ./internal/certhealth`
Expected: PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/certhealth/certhealth.go internal/certhealth/certhealth_test.go
git commit -m "feat(certhealth): assess TLS-secret certificate expiry (public leaf only)"
```

---

### Task 2: `collect.TLSSecrets`

**Files:**
- Modify: `internal/collect/collect.go` (place near the other event/list collectors)
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Produces: `func TLSSecrets(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Secret, error)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go`:

```go
func TestTLSSecrets(t *testing.T) {
	tls := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-tls"},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("PEM")}}
	client := fake.NewSimpleClientset(tls)
	got, err := TLSSecrets(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "shop-tls" {
		t.Fatalf("want the seeded TLS secret, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect -run TestTLSSecrets`
Expected: FAIL — `undefined: TLSSecrets`.

- [ ] **Step 3: Implement**

Add to `internal/collect/collect.go`:

```go
// TLSSecrets lists the kubernetes.io/tls Secrets in the namespace ("" = all) —
// public certificate material for the opt-in --certs check. The type field
// selector narrows server-side; certhealth re-filters by type in-code (the fake
// clientset ignores field selectors). Requires the secrets add-on grant
// (deploy/rbac-certs.yaml); never called unless --certs is set.
func TLSSecrets(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Secret, error) {
	secrets, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=kubernetes.io/tls"})
	if err != nil {
		return nil, fmt.Errorf("listing TLS secrets: %w", err)
	}
	return secrets.Items, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go
git commit -m "feat(collect): list kubernetes.io/tls secrets for the opt-in certs check"
```

---

### Task 3: `scan` — gated wiring + `Result.Certificates`

**Files:**
- Modify: `internal/scan/scan.go`
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `certhealth.Assess` (Task 1), `collect.TLSSecrets` (Task 2), the existing `ings` local in `Evaluate`.
- Produces: `Options.Certs bool`, `Options.CertWarnDays int`; `Result.Certificates *certhealth.Report` (`json` not needed — Result is internal; the report layer owns JSON).

- [ ] **Step 1: Write the failing tests**

Add to `internal/scan/scan_test.go` (imports gained: `k8s.io/apimachinery/pkg/runtime`, `k8s.io/apimachinery/pkg/runtime/schema`, `apierrors "k8s.io/apimachinery/pkg/api/errors"`, `k8stesting "k8s.io/client-go/testing"` — add only those not already present):

```go
func TestEvaluate_CertsOffMakesNoSecretsCall(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	cli := fake.NewSimpleClientset(node)
	if _, err := Evaluate(context.Background(), cli, Options{}); err != nil {
		t.Fatal(err)
	}
	for _, a := range cli.Actions() {
		if a.GetResource().Resource == "secrets" {
			t.Fatalf("default scan must not touch secrets, saw action %+v", a)
		}
	}
}

func TestEvaluate_CertsOnAssessesTLSSecrets(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "bad-tls"},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("not a certificate")}}
	cli := fake.NewSimpleClientset(node, bad)
	res, err := Evaluate(context.Background(), cli, Options{Certs: true, CertWarnDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if res.Certificates == nil || res.Certificates.Checked != 1 || len(res.Certificates.Invalid) != 1 {
		t.Errorf("want Certificates with 1 checked / 1 invalid, got %+v", res.Certificates)
	}
}

func TestEvaluate_CertsForbiddenGraceful(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	cli := fake.NewSimpleClientset(node)
	cli.Fake.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", nil)
	})
	res, err := Evaluate(context.Background(), cli, Options{Certs: true, CertWarnDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if res.Certificates == nil || !res.Certificates.Forbidden {
		t.Errorf("forbidden secrets list must set Certificates.Forbidden, got %+v", res.Certificates)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan -run TestEvaluate_Certs`
Expected: FAIL — build error (`Options.Certs` undefined).

- [ ] **Step 3: Implement**

In `internal/scan/scan.go`:
- Add to `Options`: `Certs bool` and `CertWarnDays int` (near `DiskUsage`/`DiskThreshold`).
- Add to `Result`: `Certificates *certhealth.Report`.
- Add imports: `"github.com/imantaba/kubeagent/internal/certhealth"` and `apierrors "k8s.io/apimachinery/pkg/api/errors"`.
- In `Evaluate`, after the ingress assessment (where `ings` is in scope), add:

```go
	var certReport *certhealth.Report
	if opts.Certs {
		warn := opts.CertWarnDays
		if warn <= 0 {
			warn = 30
		}
		tlsSecrets, tlsErr := collect.TLSSecrets(ctx, client, opts.Namespace)
		rep := certhealth.Assess(tlsSecrets, ings, warn, time.Now())
		if apierrors.IsForbidden(tlsErr) {
			rep.Forbidden = true
		}
		certReport = &rep
	}
```
- Add `Certificates: certReport` to the `Result{...}` literal in the return.

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/scan`
Expected: PASS (three new tests + all existing).

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): opt-in certificate-expiry assessment behind --certs"
```

---

### Task 4: `report` — the CERTIFICATES section + JSON

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `certhealth.Report`/`Cert`/`Invalid` (Task 1).
- Produces: `Input.Certificates *certhealth.Report`; JSON `certificates,omitempty` on the report object; `printCertificates` + `certificatesRender`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go` (import `certhealth`):

```go
func TestPrintInventory_CertificatesSection(t *testing.T) {
	rep := &certhealth.Report{Checked: 3, WarnDays: 30,
		Expired: []certhealth.Cert{{Namespace: "shop", Name: "shop-tls", CommonName: "shop.example.com",
			NotAfter: "2026-07-18T00:00:00Z", Days: -3, Ingresses: []string{"shop/storefront (shop.example.com)"}}},
		Expiring: []certhealth.Cert{{Namespace: "infra", Name: "api-tls", CommonName: "api.example.com",
			NotAfter: "2026-08-02T00:00:00Z", Days: 12}},
		Invalid: []certhealth.Invalid{{Namespace: "shop", Name: "bad-tls", Detail: "invalid certificate data"}},
	}
	var buf bytes.Buffer
	if err := PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		Certificates: rep}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"CERTIFICATES  (advisory — public certificate metadata only)",
		"✗ shop/shop-tls  EXPIRED 3d ago  (CN shop.example.com)",
		"— fronts ingress shop/storefront (shop.example.com)",
		"⚠ infra/api-tls  expires in 12d  (CN api.example.com)",
		"⚠ shop/bad-tls  invalid certificate data",
		"· 3 certificates checked (warn window 30d)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPrintInventory_CertificatesForbiddenHint(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		Certificates: &certhealth.Report{WarnDays: 30, Forbidden: true}}, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "secrets access denied — apply deploy/rbac-certs.yaml (or Helm certs.enabled=true)") {
		t.Errorf("missing forbidden hint:\n%s", buf.String())
	}
}

func TestPrintInventory_CertificatesAbsentWhenNilOrClean(t *testing.T) {
	var buf bytes.Buffer
	_ = PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}, "text", &buf)
	if strings.Contains(buf.String(), "CERTIFICATES") {
		t.Error("section must be absent when the check did not run")
	}
	buf.Reset()
	_ = PrintInventory(Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		Certificates: &certhealth.Report{Checked: 5, WarnDays: 30}}, "text", &buf)
	if strings.Contains(buf.String(), "CERTIFICATES") {
		t.Error("section must be absent when everything is healthy")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory_Certificates`
Expected: FAIL — build error (`Input.Certificates` undefined).

- [ ] **Step 3: Implement**

In `internal/report/report.go`:
- Add `Certificates *certhealth.Report` to `Input` (after `KubeletHealth`) and to the JSON `inventoryReport` struct as `Certificates *certhealth.Report \`json:"certificates,omitempty"\`` (populated from `in.Certificates` in the json branch).
- In `printInventoryText`, call `printCertificates(in.Certificates, w)` immediately after `printKubeletHealth(...)`, mirroring its call/spacing pattern (introduce `hasCerts := certificatesRender(in.Certificates)` alongside `hasKubeletHealth` and mirror wherever `hasKubeletHealth` participates in blank-line decisions).
- Add:

```go
// certificatesRender reports whether the CERTIFICATES section would print
// anything: expired/expiring/invalid certs, or the missing-grant hint.
func certificatesRender(rep *certhealth.Report) bool {
	if rep == nil {
		return false
	}
	return rep.Forbidden || len(rep.Expired) > 0 || len(rep.Expiring) > 0 || len(rep.Invalid) > 0
}

// printCertificates renders the advisory CERTIFICATES section (opt-in --certs).
func printCertificates(rep *certhealth.Report, w io.Writer) error {
	if !certificatesRender(rep) {
		return nil
	}
	if _, err := fmt.Fprintln(w, "CERTIFICATES  (advisory — public certificate metadata only)"); err != nil {
		return err
	}
	if rep.Forbidden {
		_, err := fmt.Fprintln(w, "  certificates: secrets access denied — apply deploy/rbac-certs.yaml (or Helm certs.enabled=true)")
		return err
	}
	for _, c := range rep.Expired {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  EXPIRED %dd ago  (CN %s)\n", c.Namespace, c.Name, -c.Days, c.CommonName); err != nil {
			return err
		}
		for _, ing := range c.Ingresses {
			if _, err := fmt.Fprintf(w, "      — fronts ingress %s\n", ing); err != nil {
				return err
			}
		}
	}
	for _, c := range rep.Expiring {
		if _, err := fmt.Fprintf(w, "  ⚠ %s/%s  expires in %dd  (CN %s)\n", c.Namespace, c.Name, c.Days, c.CommonName); err != nil {
			return err
		}
		for _, ing := range c.Ingresses {
			if _, err := fmt.Fprintf(w, "      — fronts ingress %s\n", ing); err != nil {
				return err
			}
		}
	}
	for _, iv := range rep.Invalid {
		if _, err := fmt.Fprintf(w, "  ⚠ %s/%s  %s\n", iv.Namespace, iv.Name, iv.Detail); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "  · %d certificates checked (warn window %dd)\n", rep.Checked, rep.WarnDays)
	return err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory`
Expected: PASS (new + existing; the golden is unaffected — its fixture has no Certificates yet).

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render the advisory CERTIFICATES section"
```

---

### Task 5: `main.go` + `watch` — flags, env, gauges

**Files:**
- Modify: `main.go`
- Modify: `internal/watch/watch.go`, `internal/watch/metrics.go`
- Test: `internal/watch/metrics_test.go`

**Interfaces:**
- Consumes: `Options.Certs`/`CertWarnDays`, `Result.Certificates`, `Input.Certificates` (Tasks 3–4).
- Produces: scan flags `--certs`/`--cert-warn-days`; watch `Config.Certs bool`/`CertWarnDays int` fed by `KUBEAGENT_CERTS`/`KUBEAGENT_CERT_WARN_DAYS`; gauges `kubeagent_certificates_expired`/`kubeagent_certificates_expiring`; `envInt` helper.

- [ ] **Step 1: Write the failing watch-metrics test**

In `internal/watch/metrics_test.go`: add `certhealth` to the imports; in `sampleResult()` add
```go
		Certificates: &certhealth.Report{WarnDays: 30, Checked: 4,
			Expired:  []certhealth.Cert{{Namespace: "shop", Name: "shop-tls", Days: -3}},
			Expiring: []certhealth.Cert{{Namespace: "infra", Name: "api-tls", Days: 12}}},
```
and add to the `TestMetrics_RenderReflectsResult` want-list:
```go
		"kubeagent_certificates_expired 1",
		"kubeagent_certificates_expiring 1",
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch -run TestMetrics_RenderReflectsResult`
Expected: FAIL — metrics missing the two gauge lines (after the build error for the field is resolved by Task 3's Result field, only the gauge lines fail).

- [ ] **Step 3: Implement watch**

- `internal/watch/watch.go`: add `Certs bool` and `CertWarnDays int` to `Config`; add `Certs: cfg.Certs, CertWarnDays: cfg.CertWarnDays,` to the `scan.Options{...}` literal in the reconcile call.
- `internal/watch/metrics.go`: add fields `certsRan bool`, `certsExpired int`, `certsExpiring int` to `metrics`; in `update()` (success path):
```go
	if res.Certificates != nil {
		m.certsRan = true
		m.certsExpired = len(res.Certificates.Expired)
		m.certsExpiring = len(res.Certificates.Expiring)
	}
```
and in `render()`, after the disk-usage block:
```go
	if m.certsRan {
		gauge("kubeagent_certificates_expired", "TLS certificates already expired (opt-in --certs)", float64(m.certsExpired))
		gauge("kubeagent_certificates_expiring", "TLS certificates expiring within the warn window (opt-in --certs)", float64(m.certsExpiring))
	}
```

- [ ] **Step 4: Implement main.go**

- Scan flags (with the other opt-ins):
```go
	certs := fs.Bool("certs", false, "check TLS-secret certificate expiry (public certs only; needs the secrets add-on grant)")
	certWarnDays := fs.Int("cert-warn-days", 30, "with --certs: warn when a certificate expires within this many days")
```
- Pass `Certs: *certs, CertWarnDays: *certWarnDays,` in the `scan.Options{...}` literal; add `Certificates: res.Certificates,` to the `report.Input{...}` literal.
- Usage line: insert `[--certs [--cert-warn-days n]]` after `[--kubelet-health]`.
- Watch config: add `Certs: envBool("KUBEAGENT_CERTS", false), CertWarnDays: envInt("KUBEAGENT_CERT_WARN_DAYS", 30),` to the `watch.Config{...}` literal, and add the helper next to `envFloat`:
```go
// envInt returns the env var parsed as an int, else def.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
```
(Add `"strconv"` to main.go imports if not already present.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/watch ./internal/scan ./internal/report`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add main.go internal/watch/watch.go internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(cli,watch): --certs flag, KUBEAGENT_CERTS env, and certificate gauges"
```

---

### Task 6: RBAC add-on + Helm

**Files:**
- Create: `deploy/rbac-certs.yaml`
- Modify: `deploy/helm/kubeagent/values.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml`, `deploy/helm/kubeagent/templates/deployment.yaml`, `deploy/README.md`

**Interfaces:** none new (deploy manifests consume the Task 5 env vars).

- [ ] **Step 1: Create `deploy/rbac-certs.yaml`** (mirrors `rbac-diskusage.yaml`):

```yaml
# Opt-in add-on: grants the kubeagent ServiceAccount LIST access to Secrets so
# the --certs / KUBEAGENT_CERTS certificate-expiry check can read the PUBLIC
# certificate (tls.crt) of kubernetes.io/tls Secrets. kubeagent never reads
# tls.key and never prints secret values. Apply alongside deploy/ to enable the
# check. Without it, kubeagent makes no Secrets API calls at all.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeagent-certs
rules:
  - apiGroups: [""]
    resources: [secrets]
    verbs: [list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeagent-certs
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeagent-certs
subjects:
  - kind: ServiceAccount
    name: kubeagent
    namespace: kubeagent
```

- [ ] **Step 2: Helm values** — in `values.yaml`, after the `kubeletHealth` block:

```yaml
certs:
  enabled: false
  warnDays: "30"
```

- [ ] **Step 3: Helm clusterrole** — in `templates/clusterrole.yaml`, after the existing `nodes/proxy` conditional block:

```yaml
  {{- if .Values.certs.enabled }}
  - apiGroups: [""]
    resources: [secrets]
    verbs: [list]
  {{- end }}
```

- [ ] **Step 4: Helm deployment env** — in `templates/deployment.yaml`, change the env guard `{{- if or .Values.diskUsage.enabled .Values.kubeletHealth.enabled }}` to `{{- if or .Values.diskUsage.enabled .Values.kubeletHealth.enabled .Values.certs.enabled }}`, and inside add:

```yaml
            {{- if .Values.certs.enabled }}
            - name: KUBEAGENT_CERTS
              value: "true"
            - name: KUBEAGENT_CERT_WARN_DAYS
              value: {{ .Values.certs.warnDays | quote }}
            {{- end }}
```

- [ ] **Step 5: `deploy/README.md`** — add a short "Certificate expiry (opt-in)" note next to the disk-usage add-on note: apply `rbac-certs.yaml` (or `--set certs.enabled=true`), env `KUBEAGENT_CERTS`/`KUBEAGENT_CERT_WARN_DAYS`, public-cert-only privacy note.

- [ ] **Step 6: Verify**

Run: `export PATH=$PATH:$HOME/.local/bin:/usr/local/bin && helm lint deploy/helm/kubeagent && helm template x deploy/helm/kubeagent | grep -c "resources: \[secrets\]" ; helm template x deploy/helm/kubeagent --set certs.enabled=true | grep -E "secrets|KUBEAGENT_CERTS|CERT_WARN"`
Expected: lint clean; default template has **0** secrets rules; with `certs.enabled=true` the secrets rule and both env vars appear.

- [ ] **Step 7: Commit**

```bash
git add deploy/rbac-certs.yaml deploy/helm/kubeagent/values.yaml deploy/helm/kubeagent/templates/clusterrole.yaml deploy/helm/kubeagent/templates/deployment.yaml deploy/README.md
git commit -m "feat(deploy): opt-in secrets RBAC add-on and Helm toggle for --certs"
```

---

### Task 7: Golden + documentation

**Files:**
- Modify: `internal/report/golden_test.go` + `internal/report/testdata/golden-scan.txt`
- Modify: `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

- [ ] **Step 1: Golden fixture** — in `goldenInput`, after `KubeletHealth`, add:

```go
		Certificates: &certhealth.Report{Checked: 3, WarnDays: 30,
			Expired: []certhealth.Cert{{Namespace: "shop", Name: "shop-tls", CommonName: "shop.example.com",
				NotAfter: "2026-07-18T00:00:00Z", Days: -3, Ingresses: []string{"shop/storefront (shop.example.com)"}}},
			Expiring: []certhealth.Cert{{Namespace: "infra", Name: "api-tls", CommonName: "api.example.com",
				NotAfter: "2026-08-02T00:00:00Z", Days: 12}}},
```
(import `certhealth`), and extend `TestGoldenInputCoversAllSections`'s guard with `|| in.Certificates == nil`.

- [ ] **Step 2: Regenerate + inspect** — run the golden without `-update` (FAIL, stale), then with `-update` (PASS), then `grep -n "CERTIFICATES\|shop-tls\|api-tls" internal/report/testdata/golden-scan.txt` — expect the section header, the `✗ shop/shop-tls  EXPIRED 3d ago` + fronts line, `⚠ infra/api-tls  expires in 12d`, and the checked-count line. Then `go test ./internal/report` (full PASS).

- [ ] **Step 3: diagnostics.md** — add after the `### Kubelet health probe (opt-in)` subsection:

```markdown
### Certificate expiry (opt-in)

`scan --certs` reads the cluster's `kubernetes.io/tls` Secrets and flags
certificates that are **expired** or expiring within the warn window
(`--cert-warn-days`, default 30) in an advisory `CERTIFICATES` section, with the
Ingress routes each certificate fronts. Privacy by construction: only the
**public** certificate (`tls.crt`) is parsed — the private key is never read —
and only metadata (names and dates) is reported. Off by default: without the
flag kubeagent makes no Secrets API calls at all. The in-cluster daemon needs
the secrets add-on grant (`deploy/rbac-certs.yaml` or Helm
`certs.enabled=true`) and enables the check with `KUBEAGENT_CERTS=true`.
```

- [ ] **Step 4: watch-mode.md** — add gauge rows `kubeagent_certificates_expired` / `kubeagent_certificates_expiring` ("opt-in; requires `--certs` / `KUBEAGENT_CERTS` and the secrets add-on") to the metrics table, and mention `KUBEAGENT_CERTS`/`KUBEAGENT_CERT_WARN_DAYS` in the flags/env paragraph.

- [ ] **Step 5: README + CHANGELOG + roadmap** —
README (opt-in features area): `- **Certificate expiry (--certs)** — flags expired and soon-expiring TLS certificates (public cert metadata only; never reads keys), with the Ingress routes they front.`
CHANGELOG `### Added`:
```markdown
- **Certificate-expiry check (opt-in `--certs`).** Flags expired and soon-expiring
  TLS certificates from `kubernetes.io/tls` Secrets — parsing only the public
  certificate, never the key — in an advisory CERTIFICATES section with the
  Ingress routes each cert fronts (`--cert-warn-days`, default 30). Daemon parity
  via `KUBEAGENT_CERTS` + `kubeagent_certificates_expired`/`_expiring` gauges and
  a separate secrets RBAC add-on (`deploy/rbac-certs.yaml` / Helm
  `certs.enabled`); without the flag kubeagent makes no Secrets API calls.
```
roadmap Shipped list: `- **Certificate expiry (opt-in)** — \`scan --certs\` flags expired and soon-expiring TLS certificates (public cert metadata only) with the Ingress routes they front; daemon gauges + a separate secrets RBAC add-on. See [Failure diagnostics](features/diagnostics.md).`

- [ ] **Step 6: Verify + commit**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...` (all PASS; mkdocs strict if available, else note).

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt website/docs/features/diagnostics.md website/docs/features/watch-mode.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "test+docs: golden coverage and documentation for the --certs check"
```

---

## Notes for the executor

- **Opt-in is the contract:** the Task 3 no-secrets-call-by-default test and the Task 6 default-template-has-no-secrets-rule check are the two guards — never weaken them.
- **Release gate (post-merge, controller-owned):** FULL CHAOS GATE (`./chaos/run.sh --recreate`) + a targeted live smoke (Kind cluster with an expired and a soon-expiring TLS secret + an Ingress referencing one; verify the section, the fronts line, and that a scan WITHOUT `--certs` shows nothing). Minor bump → **v0.32.0**.
- **Spacing around the new report section** is pinned by the golden — if the regenerated snapshot shows a missing/extra blank line vs the sibling sections, fix `printCertificates`' call site, not the snapshot.
