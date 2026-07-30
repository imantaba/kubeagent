package fuzzgen

import (
	"bytes"
	"regexp"
	"testing"
	"unicode/utf8"
)

// inputs covers the shapes a fuzzer actually hands a target: nothing, one byte,
// all zeroes, all ones, and something arbitrary.
var inputs = [][]byte{
	nil,
	{},
	{0},
	{0xff},
	bytes.Repeat([]byte{0}, 64),
	bytes.Repeat([]byte{0xff}, 64),
	[]byte("kubeagent"),
	[]byte{0x1b, 0x5b, 0x32, 0x4a, 0x00, 0xff, 0xfe},
}

func TestCursorIsTotal(t *testing.T) {
	// No input may make any draw panic. A fuzz target that panicked inside its
	// own generator would report a defect in the generator, not in kubeagent.
	for _, in := range inputs {
		c := New(in)
		_ = c.Bool()
		_ = c.IntN(0)
		_ = c.IntN(-1)
		_ = c.IntN(7)
		_ = c.Int32()
		_ = c.Pick(nil)
		_ = c.Pick([]string{"a", "b"})
		_ = c.Hostile(0)
		_ = c.Hostile(-1)
		_ = c.Hostile(32)
		_ = c.Name(0)
		_ = c.Name(-1)
		_ = c.Name(30)
		_ = c.Time(Base)
	}
}

func TestIntNStaysInRange(t *testing.T) {
	for _, in := range inputs {
		c := New(in)
		for i := 0; i < 500; i++ {
			for _, n := range []int{1, 2, 3, 7, 256, 1000} {
				if got := c.IntN(n); got < 0 || got >= n {
					t.Fatalf("IntN(%d) = %d, out of range", n, got)
				}
			}
		}
		if got := c.IntN(0); got != 0 {
			t.Errorf("IntN(0) = %d, want 0", got)
		}
		if got := c.IntN(-5); got != 0 {
			t.Errorf("IntN(-5) = %d, want 0", got)
		}
	}
}

func TestHostileRespectsItsLengthCap(t *testing.T) {
	for _, in := range inputs {
		c := New(in)
		for i := 0; i < 200; i++ {
			if got := c.Hostile(16); len(got) > 16 {
				t.Fatalf("Hostile(16) returned %d bytes", len(got))
			}
		}
		if got := c.Hostile(0); got != "" {
			t.Errorf("Hostile(0) = %q, want empty", got)
		}
		if got := c.Hostile(-1); got != "" {
			t.Errorf("Hostile(-1) = %q, want empty", got)
		}
	}
}

var dns1123 = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func TestNameIsAlwaysADNS1123Label(t *testing.T) {
	// The API server validates these, so a real cluster cannot present anything
	// else. If Name could emit a hostile byte, every output-safety property
	// downstream would be testing an object no cluster can produce.
	for _, in := range inputs {
		c := New(in)
		for i := 0; i < 500; i++ {
			got := c.Name(30)
			if got == "" {
				t.Fatal("Name returned an empty string")
			}
			if len(got) > 30 {
				t.Fatalf("Name(30) = %q, %d chars", got, len(got))
			}
			if !dns1123.MatchString(got) {
				t.Fatalf("Name = %q, not a DNS-1123 label", got)
			}
		}
		if got := c.Name(0); !dns1123.MatchString(got) {
			t.Errorf("Name(0) = %q, not a DNS-1123 label", got)
		}
		if got := c.Name(500); len(got) > 63 {
			t.Errorf("Name(500) = %d chars, want at most 63 (a DNS-1123 label's limit)", len(got))
		}
	}
}

func TestHostileCanProduceInvalidUTF8(t *testing.T) {
	// The point of Hostile is that it is hostile. If 1000 draws over a byte range
	// that includes 0xff never yield invalid UTF-8, the generator is sanitizing
	// its own output and the properties downstream are asserting nothing.
	c := New(bytes.Repeat([]byte{0xff, 0xfe, 0x1b, 0x00}, 64))
	var sawInvalid, sawControl bool
	for i := 0; i < 1000; i++ {
		s := c.Hostile(8)
		if !utf8.ValidString(s) {
			sawInvalid = true
		}
		for _, b := range []byte(s) {
			if b < 0x20 {
				sawControl = true
			}
		}
	}
	if !sawInvalid {
		t.Error("Hostile never produced invalid UTF-8")
	}
	if !sawControl {
		t.Error("Hostile never produced a control byte")
	}
}

func TestCursorIsDeterministic(t *testing.T) {
	// Native fuzzing replays a crasher from its bytes. If the same bytes built a
	// different object each time, a reported crash would not reproduce.
	for _, in := range inputs {
		a, b := New(in), New(in)
		for i := 0; i < 200; i++ {
			if x, y := a.IntN(97), b.IntN(97); x != y {
				t.Fatalf("IntN diverged at draw %d: %d vs %d", i, x, y)
			}
			if x, y := a.Hostile(12), b.Hostile(12); x != y {
				t.Fatalf("Hostile diverged at draw %d: %q vs %q", i, x, y)
			}
		}
	}
}

func TestCursorWrapsShortInput(t *testing.T) {
	// One byte must still fund an arbitrarily long draw sequence: the fuzzer
	// starts small, and a generator that ran dry would silently stop varying.
	c := New([]byte{0x5a})
	for i := 0; i < 10_000; i++ {
		_ = c.IntN(31)
	}
	if got := c.Name(20); !dns1123.MatchString(got) {
		t.Errorf("after 10k draws from one byte, Name = %q", got)
	}
}
