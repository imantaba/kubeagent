package safetext

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzLine asserts Line's whole postcondition on arbitrary bytes: valid UTF-8,
// nothing a terminal executes, a bounded length, and a bounded number of
// combining marks on any one character.
//
// The mark bound is why this target exists. Line carries state across runes now
// — how many marks the current base character has taken — and a stateful
// sanitizer is where an input that resets the counter without emitting a base
// character hides. Idempotence is the property that catches it: if some rune
// leaves the state inconsistent, a second pass produces a different string.
func FuzzLine(f *testing.F) {
	f.Add("Back-off restarting failed container")
	f.Add("")
	f.Add("\x1b[2J\x1b[Hcleared")
	f.Add("a\x00b\x07c")
	f.Add("bad\xffbyte")
	f.Add("before‮after")
	f.Add("tiếng Việt")
	f.Add("a" + strings.Repeat("́", 300))
	f.Add(strings.Repeat("́", 300))
	f.Add(strings.Repeat("का", 400))
	f.Add(strings.Repeat("x", MaxLine+50))
	f.Add(strings.Repeat("á", MaxLine))

	f.Fuzz(func(t *testing.T, raw string) {
		got := Line(raw)

		if !utf8.ValidString(got) {
			t.Fatalf("Line(%q) = %q, which is not valid UTF-8", raw, got)
		}
		if n := utf8.RuneCountInString(got); n > MaxLine {
			t.Errorf("Line(%q) is %d runes, want at most %d", raw, n, MaxLine)
		}

		marks, base := 0, false
		for i, r := range got {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				t.Fatalf("Line(%q) = %q: %U survived at byte %d", raw, got, r, i)
			}
			switch {
			case r == ' ':
				base, marks = false, 0
			case unicode.Is(unicode.M, r):
				if !base {
					t.Fatalf("Line(%q) = %q: combining %U at byte %d has no base character", raw, got, r, i)
				}
				if marks++; marks > MaxCombining {
					t.Fatalf("Line(%q) = %q: more than %d combining marks at byte %d", raw, got, MaxCombining, i)
				}
			default:
				base, marks = true, 0
			}
		}

		if again := Line(got); again != got {
			t.Errorf("Line is not idempotent: Line(%q) = %q, then %q", raw, got, again)
		}
	})
}
