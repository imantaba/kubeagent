package tui

import "unicode/utf8"

// KeyKind classifies one decoded key press. Only the keys the TUI binds get a
// kind of their own; everything else arrives as KeyRune or KeyUnknown and
// Update ignores it.
type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyEnter
	KeyEsc
	KeyCtrlC
	// KeyUnknown is a byte run decodeKey recognised as an escape sequence but
	// does not bind. It is reported rather than dropped so the caller always
	// makes progress through its buffer.
	KeyUnknown
)

// Key is one decoded key press. Rune is meaningful only when Kind is KeyRune.
type Key struct {
	Rune rune
	Kind KeyKind
}

// decodeKey decodes the first key in b and reports how many bytes it consumed.
//
// A count of 0 means b holds a valid prefix of something bigger that has not
// fully arrived, and the caller must read more bytes and try again. Two things
// are ambiguous mid-stream: a lone 0x1b, which is also the first byte of every
// arrow key, and a lead byte whose multi-byte rune has not fully landed yet.
// final resolves both. The caller arms a short timer whenever bytes are
// pending; when it fires without new input, it re-decodes with final set, and
// a lone 0x1b becomes KeyEsc while a stranded lead byte becomes KeyUnknown.
//
// Keeping the resolution here rather than in the input loop keeps every decoding
// rule in one pure, table-tested function.
func decodeKey(b []byte, final bool) (Key, int) {
	if len(b) == 0 {
		return Key{}, 0
	}
	switch b[0] {
	case 0x03:
		return Key{Kind: KeyCtrlC}, 1
	case '\r', '\n':
		return Key{Kind: KeyEnter}, 1
	case 0x1b:
		return decodeEsc(b, final)
	}
	r, n := utf8.DecodeRune(b)
	if r == utf8.RuneError && n <= 1 {
		if !final && runePrefix(b) {
			return Key{}, 0
		}
		// An invalid byte, or a prefix that will never be completed. Consume it
		// so a corrupt run cannot stall the loop.
		return Key{Kind: KeyUnknown}, 1
	}
	return Key{Kind: KeyRune, Rune: r}, n
}

// runePrefix reports whether b is a valid but incomplete prefix of a multi-byte
// rune: a lead byte followed only by continuation bytes, with more still to
// come. utf8.DecodeRune cannot answer this — it returns (RuneError, 1) both for
// a truncated prefix and for a byte that can never start a rune — so the lead
// byte's own length has to be read directly.
func runePrefix(b []byte) bool {
	if len(b) == 0 || len(b) >= utf8.UTFMax {
		return false
	}
	var want int
	switch {
	case b[0] >= 0xC2 && b[0] <= 0xDF:
		want = 2
	case b[0] >= 0xE0 && b[0] <= 0xEF:
		want = 3
	case b[0] >= 0xF0 && b[0] <= 0xF4:
		want = 4
	default:
		return false
	}
	if len(b) >= want {
		return false
	}
	for _, c := range b[1:] {
		if c&0xC0 != 0x80 {
			return false
		}
	}
	return true
}

// decodeEsc handles the runs that start with 0x1b. Only the four CSI arrow keys
// are bound; kubeagent's key map is otherwise plain runes, so there is no reason
// to grow a general CSI parser here.
func decodeEsc(b []byte, final bool) (Key, int) {
	if len(b) == 1 {
		if final {
			return Key{Kind: KeyEsc}, 1
		}
		return Key{}, 0
	}
	if b[1] != '[' {
		// An alt-key chord (esc + rune). Treat it as esc and consume both bytes
		// rather than leaving the rune to be read as a separate key press.
		return Key{Kind: KeyEsc}, 2
	}
	if len(b) == 2 {
		if final {
			return Key{Kind: KeyUnknown}, 2
		}
		return Key{}, 0
	}
	switch b[2] {
	case 'A':
		return Key{Kind: KeyUp}, 3
	case 'B':
		return Key{Kind: KeyDown}, 3
	case 'C':
		return Key{Kind: KeyRight}, 3
	case 'D':
		return Key{Kind: KeyLeft}, 3
	}
	return Key{Kind: KeyUnknown}, 3
}
