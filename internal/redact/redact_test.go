package redact

import (
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"strips path query and fragment", "https://hooks.example.com/services/T000/B000/XXXX?tok=abc#frag", "https://hooks.example.com"},
		{"keeps port", "http://alerts.example.com:8080/ingest", "http://alerts.example.com:8080"},
		{"userinfo is dropped with the rest", "https://user:pass@alerts.example.com/hook", "https://alerts.example.com"},
		{"unparseable", "://nonsense", "(redacted)"},
		{"no host", "file:///etc/kubeagent/token", "(redacted)"},
		{"empty", "", "(redacted)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := URL(tt.raw); got != tt.want {
				t.Errorf("URL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestError_URLErrorKeepsSchemeHostAndUnderlyingCause(t *testing.T) {
	inner := errors.New("connection refused")
	err := &url.Error{Op: "Post", URL: "https://hooks.example.com/services/T000/SECRET", Err: inner}

	got := Error(err)
	want := "Post https://hooks.example.com: connection refused"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestError_NestedURLErrorsAreRedactedAtEveryLevel(t *testing.T) {
	inner := &url.Error{Op: "Get", URL: "https://inner.example.com/a/SECRET", Err: errors.New("timeout")}
	outer := &url.Error{Op: "Post", URL: "https://outer.example.com/b/SECRET", Err: fmt.Errorf("wrapped: %w", inner)}

	got := Error(outer)
	want := "Post https://outer.example.com: Get https://inner.example.com: timeout"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestError_NilIsEmpty(t *testing.T) {
	if got := Error(nil); got != "" {
		t.Errorf("Error(nil) = %q, want empty", got)
	}
}

func TestError_PlainErrorPassesThrough(t *testing.T) {
	if got := Error(errors.New("boom")); got != "boom" {
		t.Errorf("Error() = %q, want %q", got, "boom")
	}
}
