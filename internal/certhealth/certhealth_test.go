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
		tlsSecret("shop", "shop-tls", certPEM(t, "shop.example.com", nil, now.Add(-3*24*time.Hour))), // expired 3d
		tlsSecret("infra", "api-tls", certPEM(t, "api.example.com", nil, now.Add(12*24*time.Hour))),  // expires 12d
		tlsSecret("infra", "ok-tls", certPEM(t, "ok.example.com", nil, now.Add(200*24*time.Hour))),   // healthy
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

func TestAssess_InvalidAndEmptyCrt(t *testing.T) {
	garbage := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "bad-tls"},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("not a certificate")}}
	// The tls.crt key is present but empty — the only state a kubernetes.io/tls
	// Secret can actually reach through the API server; it always requires both
	// data keys to exist, so a genuinely absent key cannot occur in practice.
	empty := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "empty-tls"},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("")}}
	rep := Assess([]corev1.Secret{garbage, empty}, nil, 30, now)
	if rep.Checked != 2 || len(rep.Invalid) != 2 {
		t.Fatalf("want 2 checked / 2 invalid, got %+v", rep)
	}
	// sorted by ns/name: bad-tls before empty-tls
	if rep.Invalid[0].Detail != "invalid certificate data" || rep.Invalid[1].Detail != "empty tls.crt" {
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

func TestAssess_JustExpiredIsExpiredNotExpiring(t *testing.T) {
	// Expired 6h ago: elapsed time rounds toward zero (R97), so Days=0 — the
	// classifier no longer keys on Days to decide Expired vs Expiring, it
	// compares the certificate against the clock directly, so a Days=0 cert
	// that has actually expired still lands in Expired, not Expiring.
	secrets := []corev1.Secret{tlsSecret("shop", "fresh-dead", certPEM(t, "fd.example.com", nil, now.Add(-6*time.Hour)))}
	rep := Assess(secrets, nil, 30, now)
	if len(rep.Expired) != 1 || rep.Expired[0].Days != 0 {
		t.Fatalf("a cert expired <24h ago must be EXPIRED with Days=0, got expired=%+v expiring=%+v", rep.Expired, rep.Expiring)
	}
	if len(rep.Expiring) != 0 {
		t.Errorf("must not be classified expiring, got %+v", rep.Expiring)
	}
}

// TestAssess_DaysRoundTowardZero pins R97: elapsed time rounds toward zero —
// math.Floor on the positive side (already correct, unchanged), math.Ceil on
// the negative side, so an expired certificate's Days magnitude is one smaller
// than plain truncation-toward-negative-infinity would give. Because Days=0 no
// longer distinguishes "expires today" from "expired today", classification is
// asserted directly alongside Days: the classifier must compare the
// certificate's NotAfter against the clock, not merely read Days' sign.
func TestAssess_DaysRoundTowardZero(t *testing.T) {
	tests := []struct {
		name         string
		notAfter     time.Time
		wantDays     int
		wantExpired  bool
		wantExpiring bool
	}{
		{
			name:         "expiring in 5.49 days rounds down",
			notAfter:     now.Add(time.Duration(5.49 * float64(24*time.Hour))),
			wantDays:     5,
			wantExpiring: true,
		},
		{
			name:        "expired 78 minutes ago rounds toward zero to 0, still Expired",
			notAfter:    now.Add(-78 * time.Minute),
			wantDays:    0,
			wantExpired: true,
		},
		{
			name:        "expired 10.003 days ago rounds toward zero to -10",
			notAfter:    now.Add(-time.Duration(10.003 * float64(24*time.Hour))),
			wantDays:    -10,
			wantExpired: true,
		},
		{
			name:        "NotAfter equal to now exactly is Expired",
			notAfter:    now,
			wantDays:    0,
			wantExpired: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secrets := []corev1.Secret{tlsSecret("shop", "cert", certPEM(t, "cert.example.com", nil, tt.notAfter))}
			rep := Assess(secrets, nil, 30, now)
			var got *Cert
			var gotExpired, gotExpiring bool
			if len(rep.Expired) == 1 {
				got = &rep.Expired[0]
				gotExpired = true
			}
			if len(rep.Expiring) == 1 {
				got = &rep.Expiring[0]
				gotExpiring = true
			}
			if gotExpired != tt.wantExpired || gotExpiring != tt.wantExpiring {
				t.Fatalf("expired=%v expiring=%v, want expired=%v expiring=%v (rep=%+v)",
					gotExpired, gotExpiring, tt.wantExpired, tt.wantExpiring, rep)
			}
			if got == nil || got.Days != tt.wantDays {
				t.Errorf("Days = %v, want %d", got, tt.wantDays)
			}
		})
	}
}

// TestAssess_WarnDaysBoundaryUnchanged pins the 30/31 boundary the R97
// classifier must preserve exactly: a certificate expiring in precisely
// warnDays days is Expiring (inclusive), one day later it is neither
// (exclusive) — unaffected by the change in rounding direction or the
// classifier now comparing the clock directly.
func TestAssess_WarnDaysBoundaryUnchanged(t *testing.T) {
	secrets := []corev1.Secret{
		tlsSecret("shop", "at-boundary", certPEM(t, "at.example.com", nil, now.Add(30*24*time.Hour))),
		tlsSecret("shop", "past-boundary", certPEM(t, "past.example.com", nil, now.Add(31*24*time.Hour))),
	}
	rep := Assess(secrets, nil, 30, now)
	if len(rep.Expiring) != 1 || rep.Expiring[0].Name != "at-boundary" || rep.Expiring[0].Days != 30 {
		t.Errorf("Days==warnDays must be Expiring (inclusive), got expiring=%+v", rep.Expiring)
	}
	if len(rep.Expired)+len(rep.Expiring) != 1 {
		t.Errorf("Days==warnDays+1 must be neither expired nor expiring, got expired=%+v expiring=%+v", rep.Expired, rep.Expiring)
	}
}
