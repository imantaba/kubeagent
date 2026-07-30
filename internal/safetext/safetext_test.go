package safetext

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLine(t *testing.T) {
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
		{"rtl override is dropped", "before\u202eafter", "beforeafter"},
		{"zero-width joiner is dropped", "a\u200db", "ab"},
		{"unicode line separator folds to a space", "a\u2028b\u2029c", "a b c"},
		{"invalid utf-8 becomes the replacement rune", "bad\xffbyte", "bad�byte"},
		{"surrounding whitespace is trimmed", "  padded\n", "padded"},
		{"non-ascii text survives", "café — naïve ✓", "café — naïve ✓"},
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

func TestLineIsIdempotent(t *testing.T) {
	// Sanitizing twice must not change the result: detectors compose fields, and
	// a value may pass through Line at more than one layer.
	for _, in := range []string{"clean", "a\x1b[1mb", strings.Repeat("x", MaxLine+10), "bad\xffbyte"} {
		once := Line(in)
		if twice := Line(once); twice != once {
			t.Errorf("Line(Line(%q)) = %q, want %q", in, twice, once)
		}
	}
}
