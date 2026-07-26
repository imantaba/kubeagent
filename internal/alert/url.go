// Package alert delivers watch notifications to an operator-configured HTTP
// endpoint. It is the daemon's only egress besides the Kubernetes API and it
// never writes to the cluster.
//
// The destination URL is treated as a credential throughout: a Slack incoming-
// webhook URL is a bearer token in URL form, so nothing in this package logs or
// returns more than scheme://host.
package alert

import (
	"errors"
	"net/url"
)

// RedactURL reduces a webhook URL to scheme://host, dropping the path, query, and
// userinfo. Every log line that names the destination goes through it.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "(redacted)"
	}
	return u.Scheme + "://" + u.Host
}

// RedactError reduces the *url.Error net/http returns on a failed request down
// to scheme://host plus the underlying cause, so a caller that only has an error
// (not the raw URL string) still gets the RedactURL treatment. It exists for
// callers outside this package — e.g. the watch daemon reporting why a watched
// cluster is unreachable — whose "webhook" is a Kubernetes API server whose URL
// can just as validly carry userinfo or an auth-proxy query string.
//
// A non-*url.Error passes through unchanged: most failures reaching this point
// (RBAC, TLS, timeouts) carry no URL at all, and scrubbing them down to "error"
// would make the message useless to the operator it exists for.
func RedactError(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + " " + RedactURL(ue.URL) + ": " + ue.Err.Error()
	}
	return err.Error()
}

// resolveURL validates the destination and, for the alertmanager format, fills in
// the v2 alerts path when the URL carries none. Its errors never echo the input:
// url.Parse's own error text embeds the URL, so it is deliberately not wrapped.
func resolveURL(raw string, f Format) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("alert webhook URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("alert webhook URL must use http or https")
	}
	if u.Host == "" {
		return "", errors.New("alert webhook URL has no host")
	}
	if f == FormatAlertmanager && (u.Path == "" || u.Path == "/") {
		u.Path = "/api/v2/alerts"
	}
	return u.String(), nil
}

// sanitizeErr strips the URL that net/http embeds in *url.Error, so a webhook
// credential can never reach a log line through a delivery failure.
func sanitizeErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
