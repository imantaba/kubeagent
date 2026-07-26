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

func TestRedactURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{slackish, "https://hooks.slack.example"},
		{"http://alertmanager.monitoring:9093/api/v2/alerts", "http://alertmanager.monitoring:9093"},
		{"https://user:secret@example.test/hook?token=abc", "https://example.test"},
		{"not a url at all", "(redacted)"},
		{"", "(redacted)"},
	}
	for _, tc := range tests {
		if got := RedactURL(tc.in); got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

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

func TestRedactError_URLErrorKeepsSchemeHostAndUnderlyingCause(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantHas  []string
		wantMiss []string
	}{
		{
			name:     "userinfo in the request URL",
			err:      &url.Error{Op: "Get", URL: "https://admin:s3cret@cluster.invalid:6443/api", Err: errors.New("connection refused")},
			wantHas:  []string{"https://cluster.invalid:6443", "connection refused"},
			wantMiss: []string{"admin", "s3cret"},
		},
		{
			name:     "token in the query string",
			err:      &url.Error{Op: "Get", URL: "https://cluster.invalid:6443/api?access_token=topsecret", Err: errors.New("net/http: TLS handshake timeout")},
			wantHas:  []string{"https://cluster.invalid:6443", "TLS handshake timeout"},
			wantMiss: []string{"access_token", "topsecret"},
		},
		{
			name:    "non-url error passes through unchanged, not scrubbed to uselessness",
			err:     errors.New("etcd is unhealthy: at least one member is unreachable"),
			wantHas: []string{"etcd is unhealthy: at least one member is unreachable"},
		},
		{
			// A redirect chain can leave one *url.Error wrapping another, and
			// stringifying the inner one directly would republish the URL the
			// outer one just had scrubbed.
			name: "a nested url.Error is redacted too, not just the outer one",
			err: &url.Error{Op: "Get", URL: "https://cluster.invalid:6443/api", Err: &url.Error{
				Op:  "Get",
				URL: "https://admin:s3cret@cluster.invalid:6443/redirected?access_token=topsecret",
				Err: errors.New("connection refused"),
			}},
			wantHas:  []string{"https://cluster.invalid:6443", "connection refused"},
			wantMiss: []string{"admin", "s3cret", "access_token", "topsecret"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactError(tc.err)
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("RedactError(%v) = %q, want it to contain %q", tc.err, got, want)
				}
			}
			for _, unwanted := range tc.wantMiss {
				if strings.Contains(got, unwanted) {
					t.Errorf("RedactError(%v) = %q, leaked credential material %q", tc.err, got, unwanted)
				}
			}
		})
	}
	if got := RedactError(nil); got != "" {
		t.Errorf("RedactError(nil) = %q, want empty", got)
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
