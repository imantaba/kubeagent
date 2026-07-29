package tui

import "testing"

func TestDecodeKey(t *testing.T) {
	tests := []struct {
		name  string
		in    []byte
		final bool
		want  Key
		n     int
	}{
		{"plain rune", []byte("j"), false, Key{Kind: KeyRune, Rune: 'j'}, 1},
		{"capital rune", []byte("G"), false, Key{Kind: KeyRune, Rune: 'G'}, 1},
		{"digit", []byte("1"), false, Key{Kind: KeyRune, Rune: '1'}, 1},
		{"ctrl-c", []byte{0x03}, false, Key{Kind: KeyCtrlC}, 1},
		{"ctrl-z is not special", []byte{0x1a}, false, Key{Kind: KeyRune, Rune: 0x1a}, 1},
		{"carriage return", []byte{'\r'}, false, Key{Kind: KeyEnter}, 1},
		{"line feed", []byte{'\n'}, false, Key{Kind: KeyEnter}, 1},
		{"arrow up", []byte{0x1b, '[', 'A'}, false, Key{Kind: KeyUp}, 3},
		{"arrow down", []byte{0x1b, '[', 'B'}, false, Key{Kind: KeyDown}, 3},
		{"arrow right", []byte{0x1b, '[', 'C'}, false, Key{Kind: KeyRight}, 3},
		{"arrow left", []byte{0x1b, '[', 'D'}, false, Key{Kind: KeyLeft}, 3},
		{"unknown csi is consumed whole", []byte{0x1b, '[', 'Z'}, false, Key{Kind: KeyUnknown}, 3},
		// A lone ESC is indistinguishable from the start of an arrow key until
		// either more bytes arrive or the caller's timer says none will.
		{"lone esc mid-stream waits", []byte{0x1b}, false, Key{}, 0},
		{"split csi waits", []byte{0x1b, '['}, false, Key{}, 0},
		{"lone esc resolves when final", []byte{0x1b}, true, Key{Kind: KeyEsc}, 1},
		{"partial csi resolves when final", []byte{0x1b, '['}, true, Key{Kind: KeyUnknown}, 2},
		// ESC followed by a non-CSI byte is an alt-key chord. Report Esc and
		// consume both, so a stray byte can never wedge the decoder.
		{"esc then non-csi", []byte{0x1b, 'x'}, false, Key{Kind: KeyEsc}, 2},
		{"multi-byte rune", []byte("é"), false, Key{Kind: KeyRune, Rune: 'é'}, 2},
		{"empty", nil, false, Key{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n := decodeKey(tt.in, tt.final)
			if got != tt.want || n != tt.n {
				t.Errorf("decodeKey(%q, %v) = %+v, %d; want %+v, %d", tt.in, tt.final, got, n, tt.want, tt.n)
			}
		})
	}
}

// A run holding several keys decodes one at a time, so a paste or a fast
// typist cannot drop input.
func TestDecodeKey_ConsumesOneKeyAtATime(t *testing.T) {
	b := append([]byte("jk"), 0x1b, '[', 'B')
	var got []KeyKind
	for len(b) > 0 {
		k, n := decodeKey(b, false)
		if n == 0 {
			t.Fatalf("stalled with %q remaining", b)
		}
		got = append(got, k.Kind)
		b = b[n:]
	}
	want := []KeyKind{KeyRune, KeyRune, KeyDown}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
