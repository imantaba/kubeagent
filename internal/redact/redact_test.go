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

func TestAddresses_RedactsWhatALogCanCarry(t *testing.T) {
	// These are address shapes a diagnostic string can carry — Go's dialer
	// formats a dial error as "dial tcp <host:port>", and a cluster-internal
	// DNS name reads the same way. internal/logscan's conn-refused signature
	// no longer captures container text into a cause (it returns a fixed
	// constant), so these inputs no longer arrive from that source; Addresses
	// keeps redacting them for its other callers.
	cases := []struct{ name, in, want string }{
		{"ipv4 with port", "cannot reach a dependency (10.96.14.203:80) — connection refused",
			"cannot reach a dependency (<redacted>) — connection refused"},
		{"bare ipv4", "cannot reach a dependency (192.0.2.7) — connection refused",
			"cannot reach a dependency (<redacted>) — connection refused"},
		{"ipv6 in brackets with port", "cannot reach a dependency ([fd00::1]:5432) — connection refused",
			"cannot reach a dependency (<redacted>) — connection refused"},
		{"cluster dns name with port", "cannot reach a dependency (db.chaos.svc.cluster.local:5432) — connection refused",
			"cannot reach a dependency (<redacted>) — connection refused"},
		{"two addresses in one line", "tried 10.0.0.1:80 then 10.0.0.2:80",
			"tried <redacted> then <redacted>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Addresses(c.in); got != c.want {
				t.Errorf("Addresses(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestAddresses_LeavesOrdinaryDiagnosticProseAlone(t *testing.T) {
	// Over-redaction costs the model the signal it needs, so the negative cases
	// matter as much as the positive ones. None of these carries an address.
	for _, s := range []string{
		"application panic (code bug)",
		"DNS resolution failed (name lookup)",
		"ran out of memory in-process",
		"cannot reach a dependency — connection refused",
		"0/2 ready, status CrashLoopBackOff, 5 restarts",
		"permission denied — check securityContext / file permissions",
		"back-off 5m0s restarting failed container",
	} {
		if got := Addresses(s); got != s {
			t.Errorf("Addresses(%q) = %q, want it unchanged", s, got)
		}
	}
}
