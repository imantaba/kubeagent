// Package redact turns credential-bearing values into safe-to-log strings.
//
// Endpoint URLs are credentials: a Slack or Teams webhook URL is a bearer
// token in path form, and a Git remote or API server URL can carry userinfo
// and query secrets. Nothing outside this package should format a URL or a
// URL-carrying error for a log line, a metric label, a CLI warning, or a
// tool result — call URL or Error instead.
package redact

import (
	"errors"
	"net/url"
	"regexp"
)

// URL reduces a URL to scheme://host, dropping the path, query, fragment and
// any userinfo. Anything that does not parse into both a scheme and a host is
// reported as "(redacted)" rather than echoed.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "(redacted)"
	}
	return u.Scheme + "://" + u.Host
}

// addr matches a network address embedded in otherwise free-form text. The
// three alternatives are, in order, a bracketed IPv6 literal with its port, a
// dotted-quad IPv4 with an optional port, and a dotted DNS name with a port.
//
// The DNS alternative requires the port on purpose. Without it the pattern
// would swallow any dotted word — "securityContext / file permissions" reads
// as prose, but "kube-system.svc" and "v1.32.8" do not differ structurally
// from a bare hostname, and redacting a version string would cost the reader
// signal while protecting nothing. Every address this package exists to catch
// arrives from Go's dialer, which always formats host:port.
//
// Not every address is caught. The dotted-hostname alternative requires a
// dot, so a single-label service host with a port ("redis:6379") passes
// through (R248). Widening the pattern to catch it would change redaction for
// every caller — the two prompt paths included — and wants its own fixture
// set and review, so the gap stays open behind this sentence rather than
// behind a claim that it does not exist.
var addr = regexp.MustCompile(
	`\[[0-9a-fA-F:]+\]:\d+` +
		`|\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?` +
		`|\b[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)+:\d+`)

// Addresses rewrites every network address embedded in free-form text to
// "<redacted>", leaving the surrounding prose intact.
//
// It exists for text kubeagent read out of a container's own log. A log line
// is attacker-influenced and unvalidated, and internal/logscan's conn-refused
// signature deliberately captures the dial target so an operator reading the
// report learns which dependency was unreachable. That address is appropriate
// in the operator's own terminal and inappropriate in anything kubeagent
// forwards — the same split URL draws. Call this before writing a log-derived
// string into a model prompt or any other outbound payload.
//
// Redacting is not sanitizing: run the value through safetext.Line as well if
// it has not already been sanitized at ingress.
func Addresses(s string) string { return addr.ReplaceAllString(s, "<redacted>") }

// Error renders an error safely. net/http embeds the full request URL in the
// *url.Error it returns, so a bare err.Error() would leak the whole webhook
// path; Error walks the chain and redacts the URL at every level while keeping
// the operation and the underlying cause, which are what a reader needs.
func Error(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + " " + URL(ue.URL) + ": " + Error(ue.Err)
	}
	return err.Error()
}
