package fuzzgen

import (
	"testing"
	"unicode"
	"unicode/utf8"
)

// AssertSafe fails the test when s carries anything that must never reach an
// operator's terminal: invalid UTF-8, a control character, a Unicode formatting
// character, or a Unicode line separator.
//
// The rejection set is stated here independently of internal/safetext, on
// purpose: a property written in terms of the sanitizer's own definition of
// "safe" would be circular and would pass no matter what the sanitizer did.
//
// Length is deliberately NOT checked here. A detector composes
// fmt.Sprintf("container %q: %s", name, safetext.Line(msg)), so the composed
// field legitimately exceeds one line's budget while every untrusted part is
// bounded. Folding length into this assertion would fail on every composed
// field, and the natural response — raising the limit until nothing fails —
// tests nothing. Use AssertBounded for the parts.
func AssertSafe(t *testing.T, where, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("%s: invalid UTF-8 in %q", where, s)
		return
	}
	for i, r := range s {
		switch {
		case r == '\u2028' || r == '\u2029':
			t.Errorf("%s: Unicode line separator %U at byte %d in %q", where, r, i, s)
		case unicode.IsControl(r):
			t.Errorf("%s: control character %U at byte %d in %q", where, r, i, s)
		case unicode.Is(unicode.Cf, r):
			// U+202E RIGHT-TO-LEFT OVERRIDE and friends are category Cf, not Cc,
			// so unicode.IsControl does not catch them. They reorder everything
			// printed after them.
			t.Errorf("%s: Unicode formatting character %U at byte %d in %q", where, r, i, s)
		}
	}
}

// AssertBounded fails the test when s is longer than max runes. Runes, not
// bytes: a 512-rune line of CJK is over 1500 bytes and perfectly reasonable.
func AssertBounded(t *testing.T, where, s string, max int) {
	t.Helper()
	if n := utf8.RuneCountInString(s); n > max {
		t.Errorf("%s: %d runes exceeds the %d-rune budget", where, n, max)
	}
}
