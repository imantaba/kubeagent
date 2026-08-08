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

// pagerDutyEventsURL is PagerDuty's published Events API v2 endpoint. It is a
// default rather than a hardcoded destination: an operator on a non-default
// service region, behind an egress proxy, or pointing at a test double sets
// KUBEAGENT_ALERT_WEBHOOK and this is not consulted.
const pagerDutyEventsURL = "https://events.pagerduty.com/v2/enqueue"

// DefaultURL is the endpoint a format uses when the operator configured none.
// Only pagerduty has one, because it publishes a single fixed events endpoint
// while every other format's URL is the operator's own receiver. Exported
// because internal/watch logs the resolved endpoint at startup and must not
// print an empty string.
func DefaultURL(f Format) string {
	if f == FormatPagerDuty {
		return pagerDutyEventsURL
	}
	return ""
}

// resolveURL validates the destination and fills in the path the format expects
// when the URL carries none: /api/v2/alerts for alertmanager, /v2/enqueue for
// pagerduty. An empty URL takes the format's default, which only pagerduty has.
// Its errors never echo the input: url.Parse's own error text embeds the URL, so
// it is deliberately not wrapped.
func resolveURL(raw string, f Format) (string, error) {
	if raw == "" {
		raw = DefaultURL(f)
	}
	if raw == "" {
		return "", errors.New("alerting needs KUBEAGENT_ALERT_WEBHOOK set to the receiver URL")
	}
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
	if f == FormatPagerDuty && (u.Path == "" || u.Path == "/") {
		u.Path = "/v2/enqueue"
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
