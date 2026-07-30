package certhealth

import (
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// seedCertPEM is a throwaway self-signed certificate for the RFC 2606 example
// domain fuzz.example, valid for a century, generated once for this seed. Its
// private key was deleted immediately and never existed in the repository — a
// certificate is public by definition, and this one fronts nothing.
const seedCertPEM = `-----BEGIN CERTIFICATE-----
MIIDKjCCAhKgAwIBAgIUXLenN2guMRspaOeIZ6HsoZrO3MIwDQYJKoZIhvcNAQEL
BQAwFzEVMBMGA1UEAwwMZnV6ei5leGFtcGxlMCAXDTI2MDcyOTIxMDczMFoYDzIx
MjYwNzA1MjEwNzMwWjAXMRUwEwYDVQQDDAxmdXp6LmV4YW1wbGUwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQDL85gB5AIwi7n6PXnqy3i9A4FlGxxmcuMY
x126LLyM0KD9uMMIadykNJtnaUP4cVhtALlGPyQnMm7s1eHaim8rQKES1sXLuBJ2
WFnod5PQ8CVeDId/1xYtnZUENVa1jTwuAHqXusvXFKfa81snHJrLDUAhQSbzeBZi
fFCjNk6I/SLmhFQWK8k9Ir8CaxD24WojR7pAHFZiKyke/h3JVWvbROwGEw8gXwCJ
hzwnRgx1qwH5vdQ0DoJyiT653oKoF5Ea8Ns94gBBMLSgjH3QKsaxiKcFfQwKhFEV
ib9f8iMH0v2P8lZ+zjGE/7ivFLSLmSfe9Tz/tIldaxMuz0IOEsUzAgMBAAGjbDBq
MB0GA1UdDgQWBBSBMmkpEszQNAWTK2Va7IOsJsOd6TAfBgNVHSMEGDAWgBSBMmkp
EszQNAWTK2Va7IOsJsOd6TAPBgNVHRMBAf8EBTADAQH/MBcGA1UdEQQQMA6CDGZ1
enouZXhhbXBsZTANBgkqhkiG9w0BAQsFAAOCAQEAGZMUlmYr4vCHqmgFBrd/Th1C
5emfJXgdKBDlbaVXx0Kqx+j/dt6scmfeEBbqCn8xYwNXoJcKYtqyq+Y3f5Oc8P9n
8hhQp2YPQATOtWG0t4T3R3QzfCLJh86FxeQALxIcDThM1r793oh8c5WtGr6QM8iW
NDwenlFTkIgv2aGgjeYij5rwfIolmh7eGED5muMKSGECkeYYTVzdYLdhMEtsiPxD
YCG0bsXna/zjXI6zGgEjMs8RNIgxNR7Z3nRj7zAlx3pxEBCK9zkXACYDLzXIAT8D
iuYGaGgubV018cZJPVVBH6/MX+q5TMQ9L9PCGHRF8fTtR65PQhFMDIPMFad1BA==
-----END CERTIFICATE-----
`

var fuzzNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// FuzzCertAssess feeds arbitrary bytes to the X.509 path. The bytes stand in for
// tls.crt, which any identity that can create a kubernetes.io/tls Secret
// controls, and the certificate subject it yields is printed in every report.
func FuzzCertAssess(f *testing.F) {
	f.Add([]byte(seedCertPEM), []byte("seed"))
	f.Add([]byte{}, []byte{})
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nnot base64 at all\n-----END CERTIFICATE-----\n"), []byte{1})
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), []byte{2})
	f.Add([]byte("\x1b[2J\x1b[H"), []byte{3})
	f.Add([]byte("\xff\xfe\xfd"), []byte{4})

	f.Fuzz(func(t *testing.T, crt, params []byte) {
		c := fuzzgen.New(params)
		secret := c.TLSSecret(crt)
		warnDays := c.IntN(400)

		rep := Assess([]corev1.Secret{secret}, nil, warnDays, fuzzNow)

		if rep.Checked != 1 {
			t.Errorf("Checked = %d, want 1 — one kubernetes.io/tls secret went in", rep.Checked)
		}
		for _, inv := range rep.Invalid {
			switch inv.Detail {
			case "missing tls.crt", "invalid certificate data":
			default:
				t.Errorf("Invalid.Detail = %q, outside the documented set", inv.Detail)
			}
			fuzzgen.AssertSafe(t, "invalid.namespace", inv.Namespace)
			fuzzgen.AssertSafe(t, "invalid.name", inv.Name)
		}
		for _, cert := range append(append([]Cert{}, rep.Expired...), rep.Expiring...) {
			fuzzgen.AssertSafe(t, "cert.commonName", cert.CommonName)
			fuzzgen.AssertBounded(t, "cert.commonName", cert.CommonName, safetext.MaxLine)
			fuzzgen.AssertSafe(t, "cert.notAfter", cert.NotAfter)
			if _, err := time.Parse(time.RFC3339, cert.NotAfter); err != nil {
				t.Errorf("NotAfter = %q is not RFC3339: %v", cert.NotAfter, err)
			}
			for i, ing := range cert.Ingresses {
				fuzzgen.AssertSafe(t, "cert.ingresses", ing)
				_ = i
			}
		}

		again := Assess([]corev1.Secret{secret}, nil, warnDays, fuzzNow)
		if !reflect.DeepEqual(rep, again) {
			t.Errorf("Assess is not deterministic")
		}
	})
}

func TestAssessSanitizesTheCertificateSubject(t *testing.T) {
	// A cert whose CN carries an ANSI escape. Reproduced from the leaf of a
	// self-signed certificate generated with CN "a\x1b[2Jb" — any identity that
	// can create a kubernetes.io/tls Secret can supply one.
	crt := certPEM(t, "a\x1b[2Jb", nil, fuzzNow.Add(24*time.Hour))
	rep := Assess([]corev1.Secret{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "tls"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": crt},
	}}, nil, 30, fuzzNow)
	if len(rep.Expiring) != 1 {
		t.Fatalf("Expiring = %d certs, want 1", len(rep.Expiring))
	}
	if got := rep.Expiring[0].CommonName; strings.ContainsRune(got, 0x1b) {
		t.Errorf("CommonName = %q still carries an ESC byte", got)
	}
}
