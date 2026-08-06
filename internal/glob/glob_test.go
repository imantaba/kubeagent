package glob

import (
	"strings"
	"testing"
	"time"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		want    bool
		why     string
	}{
		{"", "", true, "empty pattern matches empty string"},
		{"", "x", false, "empty pattern matches nothing else"},
		{"*", "", true, "star matches the empty string"},
		{"*", "anything/at/all:1.0", true, "star matches everything"},
		{"nginx", "nginx", true, "literal"},
		{"nginx", "nginxx", false, "literal is anchored at both ends"},
		{"registry.example.com/*", "registry.example.com/team/app:1.0", true, "star crosses a slash — path.Match would not"},
		{"registry.example.com/*", "quay.example.org/team/app:1.0", false, "different registry"},
		{"*/app:*", "registry.example.com/team/app:1.0", true, "two stars"},
		{"?", "a", true, "question mark matches one byte"},
		{"?", "", false, "question mark needs a byte"},
		{"?", "ab", false, "question mark matches exactly one"},
		{"a?c", "abc", true, "question mark mid-pattern"},
		{"a.c", "abc", false, "dot is a literal, not a regexp metacharacter"},
		{"a.c", "a.c", true, "dot matches itself"},
		{"a[bc]d", "abd", false, "brackets are literals"},
		{"a[bc]d", "a[bc]d", true, "brackets match themselves"},
		{"**", "abc", true, "adjacent stars collapse"},
		{"*a*b*c*", "xxaxxbxxcxx", true, "many stars backtrack correctly"},
		{"*a*b*c*", "xxaxxcxxbxx", false, "order still matters"},
		{"prod-*", "prod-", true, "trailing star may match nothing"},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.input); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v (%s)", c.pattern, c.input, got, c.want, c.why)
		}
	}
}

// TestMatchHasNoCatastrophicBlowup guards the matcher against the
// exponential backtracking a naive recursive implementation shows, and
// against the (non-exponential, but still worst-case quadratic) blowup this
// iterative implementation does have. Match is not linear: a single star
// followed by a long, almost-matching literal run is O(len(pattern) *
// len(s)), because each mismatch re-scans nearly the whole literal — see the
// doc comment on Match. This test does not claim linearity; it only
// asserts that two shapes finish well inside a generous deadline, so a
// regression to true exponential blowup (which would take far longer than
// this deadline even for these small sizes) is caught without making the
// test flaky on a loaded machine.
func TestMatchHasNoCatastrophicBlowup(t *testing.T) {
	const deadline = 2 * time.Second

	t.Run("many adjacent stars", func(t *testing.T) {
		pattern := "*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*b"
		input := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		start := time.Now()
		got := Match(pattern, input)
		elapsed := time.Since(start)
		if got {
			t.Error("no trailing b, so no match")
		}
		if elapsed > deadline {
			t.Errorf("Match took %v, want under %v (possible exponential regression)", elapsed, deadline)
		}
	})

	t.Run("single star plus long almost-matching literal", func(t *testing.T) {
		// The matcher's actual worst-case shape: O(len(pattern) * len(s)).
		// n=2000 measured well under a millisecond on reference hardware;
		// the deadline below leaves generous headroom above that for a
		// loaded machine while still catching a regression to exponential
		// behavior, which would blow past it by orders of magnitude.
		const n = 2000
		pattern := "*" + strings.Repeat("a", n/2) + "b"
		input := strings.Repeat("a", n) // no 'b' anywhere
		start := time.Now()
		got := Match(pattern, input)
		elapsed := time.Since(start)
		if got {
			t.Error("no trailing b, so no match")
		}
		if elapsed > deadline {
			t.Errorf("Match took %v, want under %v (possible exponential regression)", elapsed, deadline)
		}
	})
}
