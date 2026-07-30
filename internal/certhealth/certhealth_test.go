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

func TestAssess_JustExpiredIsExpiredNotExpiring(t *testing.T) {
	// Expired 6h ago: int() truncation would give Days=0 (EXPIRING); floor gives -1 (EXPIRED).
	secrets := []corev1.Secret{tlsSecret("shop", "fresh-dead", certPEM(t, "fd.example.com", nil, now.Add(-6*time.Hour)))}
	rep := Assess(secrets, nil, 30, now)
	if len(rep.Expired) != 1 || rep.Expired[0].Days != -1 {
		t.Fatalf("a cert expired <24h ago must be EXPIRED with Days=-1, got expired=%+v expiring=%+v", rep.Expired, rep.Expiring)
	}
	if len(rep.Expiring) != 0 {
		t.Errorf("must not be classified expiring, got %+v", rep.Expiring)
	}
}
