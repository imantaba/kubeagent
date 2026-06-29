// Package connectivity turns a failed Kubernetes API call into an actionable,
// human-readable diagnosis. It is pure — it classifies an existing error and
// issues no requests of its own.
package connectivity

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Diagnose inspects an error from a Kubernetes API call and returns an
// actionable, multi-line diagnosis plus true when it recognizes a
// connectivity / transport / authentication failure. It returns ("", false)
// for anything it does not recognize, so the caller falls back to the raw error.
func Diagnose(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	at := "the Kubernetes API server"
	if host := serverHost(err); host != "" {
		at += " " + host
	}
	msg := err.Error()

	// TLS / certificate (checked first so an expired cert isn't read as a bare
	// connection error).
	var unknownAuth x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostnameErr x509.HostnameError
	if errors.As(err, &unknownAuth) || errors.As(err, &certInvalid) || errors.As(err, &hostnameErr) ||
		strings.Contains(msg, "x509:") || strings.Contains(msg, "certificate") {
		return fmt.Sprintf("TLS/certificate problem reaching %s.\n"+
			"The cluster certificates may be expired, or the CA/credentials in your kubeconfig are wrong.\n"+
			"Check: control-plane certificate expiry and that your kubeconfig matches this cluster.", at), true
	}

	// Authentication / authorization.
	if apierrors.IsUnauthorized(err) {
		return fmt.Sprintf("Authentication failed (401) at %s.\n"+
			"Check: the credentials/token in your kubeconfig are valid and not expired.", at), true
	}
	if apierrors.IsForbidden(err) {
		return fmt.Sprintf("Authorization failed (403) at %s.\n"+
			"Check: your user has RBAC permission to read this (kubeagent needs only list/get).", at), true
	}

	// Connection refused.
	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(msg, "connection refused") {
		return fmt.Sprintf("%s refused the connection.\n"+
			"The control plane may be down (the API server or etcd is not running).\n"+
			"Check: control-plane pods/processes and the server URL in your kubeconfig.", capitalize(at)), true
	}

	// Connection reset.
	if strings.Contains(msg, "connection reset by peer") {
		return fmt.Sprintf("%s reset the connection.\n"+
			"The control plane may be unhealthy or restarting (e.g. the API server or etcd).\n"+
			"Check: control-plane health and recent restarts.", capitalize(at)), true
	}

	// Timeout.
	var netErr net.Error
	if (errors.As(err, &netErr) && netErr.Timeout()) ||
		strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "Client.Timeout") {
		return fmt.Sprintf("Timed out reaching %s.\n"+
			"Check: network/VPN/firewall reachability and the server URL in your kubeconfig.", at), true
	}

	// DNS.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) || strings.Contains(msg, "no such host") || strings.Contains(msg, "server misbehaving") {
		return fmt.Sprintf("Cannot resolve %s.\n"+
			"Check: the server URL/hostname in your kubeconfig and your DNS.", at), true
	}

	return "", false
}

// serverHost returns "scheme://host" from a *url.Error in the chain, or "".
func serverHost(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	return ""
}

// capitalize upper-cases the first byte (ASCII) of s.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
