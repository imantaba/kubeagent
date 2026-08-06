package glob

import (
	"strings"
	"testing"
)

// FuzzGlob asserts that no pattern can panic or hang the matcher, and pins two
// identities: `*` matches everything, and a metacharacter-free pattern matches
// itself.
func FuzzGlob(f *testing.F) {
	f.Add("registry.example.com/*", "registry.example.com/team/app:1.0")
	f.Add("*", "")
	f.Add("*a*a*a*a*a*a*a*b", strings.Repeat("a", 64))
	f.Add("?", "\x00")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, pattern, s string) {
		Match(pattern, s)
		if !Match("*", s) {
			t.Errorf("* must match %q", s)
		}
		if !strings.ContainsAny(s, "*?") && !Match(s, s) {
			t.Errorf("a metacharacter-free pattern must match itself: %q", s)
		}
	})
}
