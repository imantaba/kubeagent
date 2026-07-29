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
// A count of 0 means b holds a valid prefix of an escape sequence that has not
// fully arrived: the caller must read more bytes and try again. That is exactly
// the ambiguity a bare esc creates — 0x1b is also the first byte of every arrow
// key — and it is what final resolves. The caller arms a short timer whenever
// bytes are pending; when it fires without new input, it re-decodes with final
// set and a lone 0x1b becomes KeyEsc.
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
		// An invalid byte. Consume it so a corrupt run cannot stall the loop.
		return Key{Kind: KeyUnknown}, 1
	}
	return Key{Kind: KeyRune, Rune: r}, n
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
