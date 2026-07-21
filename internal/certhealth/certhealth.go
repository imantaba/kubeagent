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
	Detail    string `json:"detail"` // "missing tls.crt" | "invalid certificate data"
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
			Namespace:  s.Namespace,
			Name:       s.Name,
			CommonName: name,
			NotAfter:   cert.NotAfter.UTC().Format(time.RFC3339),
			Days:       int(cert.NotAfter.Sub(now).Hours() / 24),
			Ingresses:  fronts[s.Namespace+"/"+s.Name],
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
