package redact

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// FuzzRedactURL asserts the whole contract by exact equality: whatever URL
// returns for a parseable input is scheme://host and nothing else — no path, no
// query, no fragment, no userinfo.
//
// Exact equality, not containment. A containment check ("the output must not
// contain the path") fails constantly on fuzzed input, because a one-character
// path is a substring of almost any output, and a check that only rejects long
// paths silently stops testing short ones.
func FuzzRedactURL(f *testing.F) {
	f.Add("https://api.example/v1/messages?key=REDACTED-LOOKING-TOKEN")
	f.Add("https://hooks.example/services/T000/B000/xxxxxxxx")
	f.Add("https://user:pw@host.example/path#frag")
	f.Add("")
	f.Add("not a url")
	f.Add("://")
	f.Add("http://[::1]:8080/x")
	f.Add("\x1b[2Jhttps://host.example/")

	f.Fuzz(func(t *testing.T, raw string) {
		got := URL(raw)
		if got == "(redacted)" {
			return // the safe fallback is always acceptable
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("URL(%q) = %q, but the input does not parse: %v", raw, got, err)
		}
		if want := u.Scheme + "://" + u.Host; got != want {
			t.Errorf("URL(%q) = %q, want %q", raw, got, want)
		}
		if strings.Contains(got, "@") {
			t.Errorf("URL(%q) = %q — userinfo survived redaction", raw, got)
		}
	})
}

// FuzzRedactError asserts that walking a *url.Error chain redacts the URL at
// every level while keeping the operation and the cause, again by exact
// equality against what URL itself returns.
func FuzzRedactError(f *testing.F) {
	f.Add("https://api.example/v1/messages?key=REDACTED-LOOKING-TOKEN")
	f.Add("")
	f.Add("not a url")
	f.Add("https://user:pw@host.example/path")

	f.Fuzz(func(t *testing.T, raw string) {
		err := &url.Error{Op: "Post", URL: raw, Err: errors.New("boom")}
		want := "Post " + URL(raw) + ": boom"
		if got := Error(err); got != want {
			t.Errorf("Error(%q) = %q, want %q", raw, got, want)
		}

		// A nested *url.Error must be redacted at both levels.
		nested := &url.Error{Op: "Get", URL: raw, Err: err}
		wantNested := "Get " + URL(raw) + ": " + want
		if got := Error(nested); got != wantNested {
			t.Errorf("Error(nested %q) = %q, want %q", raw, got, wantNested)
		}
	})
}
