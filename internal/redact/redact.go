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
