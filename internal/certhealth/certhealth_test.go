package certhealth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
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

// ingTLS builds an Ingress where the rule's host and the TLS entry's host
// match, the common real-world shape (cert-manager and most operators set
// both to the same value).
func ingTLS(ns, name, host, secretName string) networkingv1.Ingress {
	return networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{
			TLS:   []networkingv1.IngressTLS{{SecretName: secretName, Hosts: []string{host}}},
			Rules: []networkingv1.IngressRule{{Host: host}},
		},
	}
}

// ingressTLS builds an Ingress with independent control over the TLS entry's
// hosts and the rule's host, for tests that need them to disagree.
func ingressTLS(ns, name, secretName string, tlsHosts []string, ruleHost string) networkingv1.Ingress {
	ing := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{SecretName: secretName, Hosts: tlsHosts}},
		},
	}
	if ruleHost != "" {
		ing.Spec.Rules = []networkingv1.IngressRule{{Host: ruleHost}}
	}
	return ing
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

// pemBundle concatenates already-PEM-encoded blocks into a single tls.crt
// value, the way a real CA bundle interleaves a leaf certificate with one or
// more intermediates.
func pemBundle(blocks ...[]byte) []byte {
	var out []byte
	for _, b := range blocks {
		out = append(out, b...)
	}
	return out
}

// TestAssess_MultiBlockSelectsEarliestExpiring pins R98: Assess parses every
// CERTIFICATE block in tls.crt, not just the first, and reports whichever one
// expires soonest -- regardless of block order or which block is a CA.
func TestAssess_MultiBlockSelectsEarliestExpiring(t *testing.T) {
	leaf := certPEM(t, "leaf.example.com", nil, now.Add(5*24*time.Hour))
	ca := certPEM(t, "ca.example.com", nil, now.Add(400*24*time.Hour))

	t.Run("leaf-first: answer unchanged", func(t *testing.T) {
		secrets := []corev1.Secret{tlsSecret("shop", "bundle", pemBundle(leaf, ca))}
		rep := Assess(secrets, nil, 30, now)
		if len(rep.Expiring) != 1 || rep.Expiring[0].CommonName != "leaf.example.com" || rep.Expiring[0].Days != 5 {
			t.Errorf("want leaf selected, got expiring=%+v expired=%+v", rep.Expiring, rep.Expired)
		}
	})

	t.Run("CA-first: still reports the leaf's CN and date", func(t *testing.T) {
		secrets := []corev1.Secret{tlsSecret("shop", "bundle", pemBundle(ca, leaf))}
		rep := Assess(secrets, nil, 30, now)
		if len(rep.Expiring) != 1 || rep.Expiring[0].CommonName != "leaf.example.com" || rep.Expiring[0].Days != 5 {
			t.Errorf("want leaf selected regardless of block order, got expiring=%+v expired=%+v", rep.Expiring, rep.Expired)
		}
	})

	t.Run("single block: unchanged", func(t *testing.T) {
		secrets := []corev1.Secret{tlsSecret("shop", "single", leaf)}
		rep := Assess(secrets, nil, 30, now)
		if len(rep.Expiring) != 1 || rep.Expiring[0].CommonName != "leaf.example.com" {
			t.Errorf("single-block secret must behave as before, got expiring=%+v", rep.Expiring)
		}
	})

	t.Run("CA expires before its leaf: reports the CA", func(t *testing.T) {
		earlyCA := certPEM(t, "early-ca.example.com", nil, now.Add(2*24*time.Hour))
		laterLeaf := certPEM(t, "later-leaf.example.com", nil, now.Add(20*24*time.Hour))
		secrets := []corev1.Secret{tlsSecret("shop", "bundle", pemBundle(laterLeaf, earlyCA))}
		rep := Assess(secrets, nil, 30, now)
		if len(rep.Expiring) != 1 || rep.Expiring[0].CommonName != "early-ca.example.com" || rep.Expiring[0].Days != 2 {
			t.Errorf("want the earlier-expiring block reported even though it is not first, got expiring=%+v", rep.Expiring)
		}
	})

	t.Run("bundle of only CA blocks: earliest of the CAs wins", func(t *testing.T) {
		ca1 := certPEM(t, "ca1.example.com", nil, now.Add(10*24*time.Hour))
		ca2 := certPEM(t, "ca2.example.com", nil, now.Add(3*24*time.Hour))
		secrets := []corev1.Secret{tlsSecret("shop", "cas", pemBundle(ca1, ca2))}
		rep := Assess(secrets, nil, 30, now)
		if len(rep.Expiring) != 1 || rep.Expiring[0].CommonName != "ca2.example.com" || rep.Expiring[0].Days != 3 {
			t.Errorf("want the earlier CA reported, got expiring=%+v", rep.Expiring)
		}
	})

	t.Run("garbage second block: reports the first, not Invalid", func(t *testing.T) {
		garbageBlock := []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
		secrets := []corev1.Secret{tlsSecret("shop", "mixed", pemBundle(leaf, garbageBlock))}
		rep := Assess(secrets, nil, 30, now)
		if len(rep.Invalid) != 0 {
			t.Fatalf("a valid first block must not become Invalid because a later block is garbage, got %+v", rep.Invalid)
		}
		if len(rep.Expiring) != 1 || rep.Expiring[0].CommonName != "leaf.example.com" {
			t.Errorf("want the first (valid) block reported, got expiring=%+v", rep.Expiring)
		}
	})
}

// TestAssess_MultiBlockBounded pins the bound on how many PEM blocks Assess
// parses from one tls.crt: the loop must stop at the bound (32) rather than
// spin over a pathological bundle. A within-bound block with a near-term date
// must be reported even though a beyond-bound block has a sooner date still.
func TestAssess_MultiBlockBounded(t *testing.T) {
	const bound = 32 // must match certhealth.go's maxCertBlocks
	filler := certPEM(t, "filler.example.com", nil, now.Add(200*24*time.Hour))
	blocks := make([][]byte, bound+5)
	for i := range blocks {
		blocks[i] = filler
	}
	blocks[bound-1] = certPEM(t, "within-bound.example.com", nil, now.Add(10*24*time.Hour))
	blocks[bound+3] = certPEM(t, "beyond-bound.example.com", nil, now.Add(1*24*time.Hour))

	secrets := []corev1.Secret{tlsSecret("shop", "huge-bundle", pemBundle(blocks...))}
	rep := Assess(secrets, nil, 30, now)
	if len(rep.Expiring) != 1 || rep.Expiring[0].CommonName != "within-bound.example.com" {
		t.Errorf("want the within-bound block reported -- the loop must stop at the bound and never reach the beyond-bound block, got expiring=%+v", rep.Expiring)
	}
}

// TestIngressFronts_TwoHostEntry pins R99: the label is built from the
// IngressTLS entry's own Hosts, not spec.rules[0].host.
func TestIngressFronts_TwoHostEntry(t *testing.T) {
	out := ingressFronts([]networkingv1.Ingress{
		ingressTLS("shop", "storefront", "shop-tls", []string{"a.example.com", "b.example.com"}, ""),
	})
	got := out["shop/shop-tls"]
	want := "shop/storefront (a.example.com, b.example.com)"
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %v, want %q", got, want)
	}
}

func TestIngressFronts_ZeroHostEntryRendersBare(t *testing.T) {
	out := ingressFronts([]networkingv1.Ingress{
		ingressTLS("shop", "storefront", "shop-tls", nil, ""),
	})
	got := out["shop/shop-tls"]
	want := "shop/storefront"
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %v, want %q (bare, no parens)", got, want)
	}
}

func TestIngressFronts_TwoIngressesFrontOneSecret(t *testing.T) {
	out := ingressFronts([]networkingv1.Ingress{
		ingressTLS("shop", "storefront", "shop-tls", []string{"a.example.com"}, ""),
		ingressTLS("shop", "admin", "shop-tls", []string{"admin.example.com"}, ""),
	})
	got := out["shop/shop-tls"]
	if len(got) != 2 {
		t.Fatalf("want both Ingresses listed, got %v", got)
	}
}

func TestIngressFronts_SameNamedSecretInAnotherNamespaceAbsent(t *testing.T) {
	out := ingressFronts([]networkingv1.Ingress{
		ingressTLS("other", "elsewhere", "shop-tls", []string{"x.example.com"}, ""),
	})
	if got := out["shop/shop-tls"]; len(got) != 0 {
		t.Errorf("a same-named secret in another namespace must not match, got %v", got)
	}
}

// TestIngressFronts_HostControlCharacterSanitized pins that spec.tls[].hosts
// -- a field the API server does not deeply validate -- passes through
// safetext.Line at this ingress point, not at the renderer.
func TestIngressFronts_HostControlCharacterSanitized(t *testing.T) {
	out := ingressFronts([]networkingv1.Ingress{
		ingressTLS("shop", "storefront", "shop-tls", []string{"a\x1b[2Jexample.com"}, ""),
	})
	got := out["shop/shop-tls"]
	if len(got) != 1 || strings.ContainsRune(got[0], 0x1b) {
		t.Errorf("host must be sanitized, got %q", got)
	}
}

// TestIngressFronts_HostSourceIsTheTLSEntryNotTheRule pins the two
// direction changes R99 accepts: an Ingress can now gain hosts it never had
// before (no spec.rules, but spec.tls[].hosts set), and can now render bare
// where it used to borrow a rule's host that the certificate may not cover
// (spec.rules present, spec.tls[].hosts empty).
func TestIngressFronts_HostSourceIsTheTLSEntryNotTheRule(t *testing.T) {
	t.Run("no rules but tls hosts set: gains its hosts", func(t *testing.T) {
		out := ingressFronts([]networkingv1.Ingress{
			ingressTLS("shop", "storefront", "shop-tls", []string{"a.example.com"}, ""),
		})
		got := out["shop/shop-tls"]
		want := "shop/storefront (a.example.com)"
		if len(got) != 1 || got[0] != want {
			t.Errorf("got %v, want %q", got, want)
		}
	})

	t.Run("rules present but tls hosts empty: renders bare", func(t *testing.T) {
		out := ingressFronts([]networkingv1.Ingress{
			ingressTLS("shop", "storefront", "shop-tls", nil, "rule-only.example.com"),
		})
		got := out["shop/shop-tls"]
		want := "shop/storefront"
		if len(got) != 1 || got[0] != want {
			t.Errorf("got %v, want %q (bare, ignoring the rule's host)", got, want)
		}
	})
}

// TestIngressFronts_HostListBounded pins the length bound on the joined host
// list: a many-host IngressTLS entry cannot produce an unbounded line.
func TestIngressFronts_HostListBounded(t *testing.T) {
	hosts := make([]string, 8) // exceeds the bound (5)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%d.example.com", i)
	}
	out := ingressFronts([]networkingv1.Ingress{
		ingressTLS("shop", "storefront", "shop-tls", hosts, ""),
	})
	got := out["shop/shop-tls"]
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(got[0], "+3 more") {
		t.Errorf("want overflow marked '+3 more' for 8 hosts over a bound of 5, got %q", got[0])
	}
	for i := 5; i < 8; i++ {
		if strings.Contains(got[0], hosts[i]) {
			t.Errorf("host %d must not appear individually beyond the bound, got %q", i, got[0])
		}
	}
}
