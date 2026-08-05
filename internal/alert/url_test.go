package alert

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// The webhook URL is a credential: a Slack incoming-webhook URL is a bearer token
// in URL form. Nothing but scheme://host may ever reach a log line.
const slackish = "https://hooks.slack.example/services/T00000000/B00000000/abcdefghijklmnopqrstuvwx"

func TestResolveURL(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		format Format
		want   string
	}{
		{"json passes through", "https://example.test/hook", FormatJSON, "https://example.test/hook"},
		{"alertmanager bare host gains the v2 path", "http://alertmanager:9093", FormatAlertmanager, "http://alertmanager:9093/api/v2/alerts"},
		{"alertmanager root path gains the v2 path", "http://alertmanager:9093/", FormatAlertmanager, "http://alertmanager:9093/api/v2/alerts"},
		{"alertmanager explicit path is respected", "http://alertmanager:9093/custom/alerts", FormatAlertmanager, "http://alertmanager:9093/custom/alerts"},
		{"pagerduty empty URL takes the published endpoint", "", FormatPagerDuty, "https://events.pagerduty.com/v2/enqueue"},
		{"pagerduty bare host gains the enqueue path", "https://events.eu.example.com", FormatPagerDuty, "https://events.eu.example.com/v2/enqueue"},
		{"pagerduty root path gains the enqueue path", "https://events.eu.example.com/", FormatPagerDuty, "https://events.eu.example.com/v2/enqueue"},
		{"pagerduty explicit path is respected", "http://192.0.2.10:8080/capture", FormatPagerDuty, "http://192.0.2.10:8080/capture"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveURL(tc.in, tc.format)
			if err != nil {
				t.Fatalf("resolveURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveURL_ErrorsNeverEchoTheURL(t *testing.T) {
	bad := []string{"", "://nope", "ftp://example.test/hook", "/no-scheme"}
	for _, in := range bad {
		_, err := resolveURL(in, FormatJSON)
		if err == nil {
			t.Errorf("resolveURL(%q) must error", in)
			continue
		}
		if in != "" && strings.Contains(err.Error(), in) {
			t.Errorf("resolveURL(%q) error echoes the URL: %v", in, err)
		}
	}
	_, err := resolveURL(slackish+"\x7f", FormatJSON)
	if err != nil && strings.Contains(err.Error(), "abcdefghijklmnopqrstuvwx") {
		t.Errorf("error leaked the webhook token: %v", err)
	}
}

func TestSanitizeErr_StripsTheURLNetHTTPEmbeds(t *testing.T) {
	inner := errors.New("connection refused")
	err := sanitizeErr(&url.Error{Op: "Post", URL: slackish, Err: inner})
	if strings.Contains(err.Error(), "slack") {
		t.Errorf("sanitizeErr left the URL in place: %v", err)
	}
	if !errors.Is(err, inner) {
		t.Errorf("sanitizeErr dropped the underlying error: %v", err)
	}
	other := errors.New("plain")
	if got := sanitizeErr(other); got != other {
		t.Errorf("sanitizeErr changed a non-url.Error: %v", got)
	}
}

// Every other format's URL is the operator's own receiver and cannot be
// guessed, so only pagerduty has a default. A json install with no
// KUBEAGENT_ALERT_WEBHOOK must still be an error, not a silent post to nowhere.
func TestDefaultURL(t *testing.T) {
	if got := DefaultURL(FormatPagerDuty); got != "https://events.pagerduty.com/v2/enqueue" {
		t.Errorf("DefaultURL(pagerduty) = %q", got)
	}
	for _, f := range []Format{FormatJSON, FormatSlack, FormatAlertmanager, Format("teletype")} {
		if got := DefaultURL(f); got != "" {
			t.Errorf("DefaultURL(%s) = %q, want empty", f, got)
		}
	}
	if _, err := resolveURL("", FormatJSON); err == nil {
		t.Error("resolveURL(\"\", json) must error: there is no default receiver to fall back to")
	}
}

// The routing key is a credential. A validation error names the variable the
// operator must fix and never echoes what they set it to.
func TestValidateRoutingKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"a plain token is accepted", "not-a-real-routing-key", false},
		{"empty is rejected", "", true},
		{"a trailing newline is rejected", "not-a-real-routing-key\n", true},
		{"an embedded space is rejected", "not a real routing key", true},
		{"a pasted multi-line blob is rejected", "not-a-real\nrouting-key", true},
		{"a control byte is rejected", "not-a-real-routing-key\x07", true},
		{"a tab is rejected", "\tnot-a-real-routing-key", true},
		{"a non-ASCII byte is rejected", "not-a-real-routing-k\xffy", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRoutingKey(tc.key)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateRoutingKey(%q) error = %v, wantErr %v", tc.key, err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), "KUBEAGENT_ALERT_ROUTING_KEY") {
				t.Errorf("error does not name the variable to fix: %v", err)
			}
			if tc.key != "" && strings.Contains(err.Error(), tc.key) {
				t.Errorf("error echoes the routing key: %v", err)
			}
		})
	}
}
