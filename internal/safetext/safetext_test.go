package safetext

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLine(t *testing.T) {
	// Named, because a combining mark written inline renders on top of whatever
	// precedes it — in a test table that is usually a quote or a comma.
	const (
		acute    = "́" // Mn — combining acute accent
		cedilla  = "̧" // Mn — combining cedilla
		vowelAA  = "ा" // Mc — Devanagari vowel sign AA (spacing)
		enclose  = "⃝" // Me — combining enclosing circle
		devaKa   = "क" // Devanagari KA, the base for vowelAA
		zwj      = "‍" // Cf — dropped before the mark rules run
		circumfl = "̂" // Mn — combining circumflex
		dotBelow = "̣" // Mn — combining dot below
	)
	// "tiếng Việt" fully decomposed: two marks on each accented vowel.
	viet := "tie" + circumfl + acute + "ng Vie" + dotBelow + circumfl + "t"

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean text is unchanged", `Error: ImagePullBackOff pulling "registry.example/app:1.2"`, `Error: ImagePullBackOff pulling "registry.example/app:1.2"`},
		{"empty stays empty", "", ""},
		{"ansi escape loses its ESC", "\x1b[2J\x1b[Hgotcha", "[2J[Hgotcha"},
		{"osc title escape loses ESC and BEL", "\x1b]0;pwned\x07rest", "]0;pwnedrest"},
		{"nul is dropped", "a\x00b", "ab"},
		{"carriage return cannot overwrite the line", "real\rfake", "real fake"},
		{"newline folds to a space", "line1\nline2", "line1 line2"},
		{"tab folds to a space", "a\tb", "a b"},
		{"rtl override is dropped", "before‮after", "beforeafter"},
		{"zero-width joiner is dropped", "a‍b", "ab"},
		{"unicode line separator folds to a space", "a b c", "a b c"},
		{"invalid utf-8 becomes the replacement rune", "bad\xffbyte", "bad�byte"},
		{"surrounding whitespace is trimmed", "  padded\n", "padded"},
		{"non-ascii text survives", "café — naïve ✓", "café — naïve ✓"},
		{"decomposed diacritics survive", viet, viet},
		{"a combining mark with no base character is dropped", acute + "abc", "abc"},
		{"a stack of combining marks is capped", "a" + strings.Repeat(acute, 20) + "b", "a" + strings.Repeat(acute, MaxCombining) + "b"},
		{"the cap is per base character, not per line", "a" + strings.Repeat(acute, 9) + "b" + strings.Repeat(cedilla, 9), "a" + strings.Repeat(acute, MaxCombining) + "b" + strings.Repeat(cedilla, MaxCombining)},
		{"a spacing combining mark counts against the same cap", devaKa + strings.Repeat(vowelAA, 9), devaKa + strings.Repeat(vowelAA, MaxCombining)},
		{"an enclosing mark counts against the same cap", "a" + strings.Repeat(enclose, 9), "a" + strings.Repeat(enclose, MaxCombining)},
		{"a mark keeps the base a dropped format character sat between", "a" + zwj + strings.Repeat(acute, 9), "a" + strings.Repeat(acute, MaxCombining)},
		{"a folded newline resets the base, so the next mark has none", "a\n" + acute + "b", "a b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Line(tc.in); got != tc.want {
				t.Errorf("Line(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLineTruncatesToMaxLineRunes(t *testing.T) {
	got := Line(strings.Repeat("x", MaxLine+200))
	if n := utf8.RuneCountInString(got); n != MaxLine {
		t.Errorf("rune count = %d, want exactly %d", n, MaxLine)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated result %q does not end in an ellipsis", got[len(got)-8:])
	}
}

func TestLineTruncatesOnRuneBoundaries(t *testing.T) {
	// A multi-byte rune must never be cut in half: a byte-indexed truncation
	// would produce invalid UTF-8, which is exactly what Line exists to remove.
	got := Line(strings.Repeat("é", MaxLine+200))
	if !utf8.ValidString(got) {
		t.Error("truncation produced invalid UTF-8")
	}
	if n := utf8.RuneCountInString(got); n != MaxLine {
		t.Errorf("rune count = %d, want exactly %d", n, MaxLine)
	}
}

func TestLineTruncationDoesNotDecorateTheEllipsis(t *testing.T) {
	// The cut lands on a combining mark here: one leading rune, then base/mark
	// pairs, puts a mark at the last kept position. The mark has to go with the
	// text it was attached to, or it renders on the ellipsis instead.
	got := Line("x" + strings.Repeat("á", MaxLine))
	if !strings.HasSuffix(got, "a…") {
		t.Errorf("truncated result ends %q, want a base character before the ellipsis", string([]rune(got)[len([]rune(got))-2:]))
	}
	if n := utf8.RuneCountInString(got); n > MaxLine {
		t.Errorf("rune count = %d, want at most %d", n, MaxLine)
	}
}

func TestLineIsIdempotent(t *testing.T) {
	// Sanitizing twice must not change the result: detectors compose fields, and
	// a value may pass through Line at more than one layer.
	inputs := []string{
		"clean",
		"a\x1b[1mb",
		strings.Repeat("x", MaxLine+10),
		"bad\xffbyte",
		"a" + strings.Repeat("́", 60) + "b", // capped stack
		strings.Repeat("́", 5) + "no base",  // dropped leading marks
		strings.Repeat("a"+strings.Repeat("́", 9), 80), // capping crosses the truncation point
	}
	for _, in := range inputs {
		once := Line(in)
		if twice := Line(once); twice != once {
			t.Errorf("Line(Line(%q)) = %q, want %q", in, twice, once)
		}
	}
}
