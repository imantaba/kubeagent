// Package certhealth flags expired and soon-expiring TLS certificates from
// kubernetes.io/tls Secrets. It parses ONLY the public certificate — every
// CERTIFICATE PEM block in tls.crt, reporting whichever one expires soonest —
// the private key (tls.key) is never read — and reports metadata only: names,
// expiry dates, and the Ingress routes each cert fronts. Pure: the caller
// supplies the secrets, ingresses, warn window, and clock. Opt-in (--certs);
// advisory (never affects the cluster verdict).
package certhealth

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// Cert holds metadata about a single TLS certificate from a kubernetes.io/tls Secret.
type Cert struct {
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	CommonName string   `json:"commonName"`          // CN, or the first DNS SAN when CN is empty
	NotAfter   string   `json:"notAfter"`            // RFC3339 (UTC)
	Days       int      `json:"days"`                // days until expiry; negative = days since expired
	Ingresses  []string `json:"ingresses,omitempty"` // "ns/name (host)" routes fronted by this cert
}

// Invalid records a kubernetes.io/tls Secret whose certificate could not be parsed.
type Invalid struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Detail    string `json:"detail"` // "empty tls.crt" | "invalid certificate data"
}

// Report is the result of Assess — a summary of all TLS-Secret certificate states.
type Report struct {
	Checked   int       `json:"checked"`
	WarnDays  int       `json:"warnDays"`
	Expired   []Cert    `json:"expired,omitempty"`
	Expiring  []Cert    `json:"expiring,omitempty"`
	Invalid   []Invalid `json:"invalid,omitempty"`
	Forbidden bool      `json:"forbidden,omitempty"`
}

// Assess parses the earliest-expiring certificate of each kubernetes.io/tls
// Secret and classifies it against now + warnDays. Deterministic: injected
// clock; Expired and Expiring sorted by (Days asc, namespace, name); Invalid
// by (namespace, name).
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
			rep.Invalid = append(rep.Invalid, Invalid{Namespace: s.Namespace, Name: s.Name, Detail: "empty tls.crt"})
			continue
		}
		cert := earliestCertificate(crt)
		if cert == nil {
			rep.Invalid = append(rep.Invalid, Invalid{Namespace: s.Namespace, Name: s.Name, Detail: "invalid certificate data"})
			continue
		}
		// The subject and SANs are attacker-controlled: any identity that can
		// create a kubernetes.io/tls Secret chooses them, X.509 string types do
		// not exclude control characters, and CommonName is printed in the text
		// report, the JSON, the HTML report and the TUI.
		name := safetext.Line(cert.Subject.CommonName)
		if name == "" && len(cert.DNSNames) > 0 {
			name = safetext.Line(cert.DNSNames[0])
		}
		elapsedDays := cert.NotAfter.Sub(now).Hours() / 24
		days := math.Floor(elapsedDays)
		if elapsedDays < 0 {
			// Elapsed time rounds toward zero, not toward negative infinity: a
			// certificate that expired 78 minutes ago is one day old, not two.
			days = math.Ceil(elapsedDays)
		}
		c := Cert{
			Namespace:  s.Namespace,
			Name:       s.Name,
			CommonName: name,
			NotAfter:   cert.NotAfter.UTC().Format(time.RFC3339),
			Days:       int(days),
			Ingresses:  fronts[s.Namespace+"/"+s.Name],
		}
		switch {
		case !cert.NotAfter.After(now):
			// Compare the certificate against the clock directly rather than
			// keying on Days: rounding toward zero means Days==0 no longer
			// distinguishes "expires today" (not yet expired) from "expired
			// today" (already past NotAfter), and NotAfter.After is false both
			// when NotAfter is in the past and when it equals now exactly.
			rep.Expired = append(rep.Expired, c)
		case warnDays > 0 && c.Days <= warnDays:
			// warnDays == 0 is a real window, "expired only" — not "the check
			// is disabled" and not "fall back to some default". Without the
			// warnDays > 0 guard, a certificate expiring within hours (Days
			// == 0, not yet past NotAfter) would satisfy 0 <= 0 and land in
			// Expiring even though the operator asked for a zero-day window.
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

// maxCertBlocks bounds how many PEM blocks earliestCertificate parses from a
// single tls.crt. A real bundle is a handful of certificates — a leaf plus a
// few intermediates — so the bound is generous headroom, and it exists so a
// pathological tls.crt with thousands of blocks reports from the blocks read
// rather than spending unbounded CPU decoding and parsing X.509.
const maxCertBlocks = 32

// earliestCertificate decodes up to maxCertBlocks PEM blocks from crt, parses
// every block whose Type is CERTIFICATE, and returns the one with the
// earliest NotAfter — the certificate soonest to expire, whichever block it
// came from and whether or not it is a CA. A block that fails to decode or
// parse is skipped rather than treated as fatal, as long as some other block
// in the bound yields a certificate. Returns nil when none does.
func earliestCertificate(crt []byte) *x509.Certificate {
	var earliest *x509.Certificate
	rest := crt
	for i := 0; i < maxCertBlocks; i++ {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if earliest == nil || cert.NotAfter.Before(earliest.NotAfter) {
			earliest = cert
		}
	}
	return earliest
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

// maxIngressHosts bounds how many hosts ingressFronts joins into one label.
// A real Ingress fronts a handful of hostnames; the bound exists so a
// spec.tls[].hosts list of many entries cannot produce an unbounded report
// line.
const maxIngressHosts = 5

// boundedHosts sanitizes and joins up to maxIngressHosts of hosts, appending
// an overflow marker for the remainder. Returns "" for an empty list.
// spec.tls[].hosts is a field the API server does not deeply validate, so
// each host is sanitized here, at the point it enters a kubeagent value —
// not later, at the renderer.
func boundedHosts(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	n := len(hosts)
	if n > maxIngressHosts {
		n = maxIngressHosts
	}
	parts := make([]string, 0, n+1)
	for _, h := range hosts[:n] {
		parts = append(parts, safetext.Line(h))
	}
	if len(hosts) > maxIngressHosts {
		parts = append(parts, fmt.Sprintf("+%d more", len(hosts)-maxIngressHosts))
	}
	return strings.Join(parts, ", ")
}

// ingressFronts maps "ns/secretName" to the sorted "ns/ingName (host1, host2,
// …)" routes referencing it via spec.tls (same-namespace by definition of
// Ingress TLS). The host list comes from the IngressTLS entry itself
// (spec.tls[].hosts) rather than spec.rules[0] -- the entry may cover hosts
// no rule names, or none at all, and spec.rules[0].host may name a host the
// certificate does not cover.
func ingressFronts(ings []networkingv1.Ingress) map[string][]string {
	out := map[string][]string{}
	for _, ing := range ings {
		for _, t := range ing.Spec.TLS {
			if t.SecretName == "" {
				continue
			}
			label := ing.Namespace + "/" + ing.Name
			if hosts := boundedHosts(t.Hosts); hosts != "" {
				label += " (" + hosts + ")"
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
