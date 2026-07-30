// Package fuzzgen builds Kubernetes API objects deterministically from a
// fuzzer's []byte, and asserts the two properties every kubeagent output field
// must hold.
//
// TEST-ONLY. This package imports "testing"; nothing outside a _test.go file
// may import it, or every kubeagent binary would carry the testing package's
// flag registrations. TestNoProductionImport in this package enforces that.
//
// Go's native fuzzing feeds []byte, string and primitives — never a struct — so
// a detector fuzz target needs a deterministic bytes-to-object builder. That is
// what Cursor is: a cursor over the fuzzer's bytes with one method per field
// shape, wrapping when it runs out so no input is too short to fund a draw.
package fuzzgen

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Base is the fixed instant every generated timestamp is drawn around. Fuzz
// targets pass it to diagnose.DefaultDetectors so a whole fuzzed run is a pure
// function of the fuzzer's bytes — no wall clock anywhere.
var Base = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// Cursor draws values from a fuzzer's input bytes. Every method is total: no
// input, however short or hostile, can make one panic or return out of range.
//
// The byte stream wraps, so it is effectively infinite. Callers must bound their
// own loops with IntN — `for c.Bool() { … }` would never terminate on an input
// of all-odd bytes.
type Cursor struct {
	b []byte
	i int
}

// New returns a Cursor over b. A nil or empty b yields a Cursor whose every draw
// reads zero, which is a legitimate object, not an error.
func New(b []byte) *Cursor { return &Cursor{b: b} }

// next returns the next input byte, wrapping to the start when exhausted and
// yielding 0 when there is no input at all.
//
// The raw input byte is XORed with all four bytes of the running position
// before it wraps. A short, periodic input (a fuzzer-supplied seed like six
// repeating bytes) has only len(b) distinct values, so a draw that depended
// solely on b[i%len(b)] — or even b[i%len(b)] mixed with only the low byte of
// i — would revisit the same small set of phases within a few thousand calls:
// every later draw would replay an earlier one, and a 20,000-iteration loop
// would degenerate into a handful of distinct objects repeated thousands of
// times over. Mixing in the full position keeps that from happening until i
// wraps its own 32 bits — far beyond what any single test budgets — so the
// sequence stays varied instead of locking into a short cycle.
func (c *Cursor) next() byte {
	if len(c.b) == 0 {
		return 0
	}
	v := c.b[c.i%len(c.b)] ^ byte(c.i) ^ byte(c.i>>8) ^ byte(c.i>>16) ^ byte(c.i>>24)
	c.i++
	return v
}

// Bool draws a boolean from the low bit of the next byte.
func (c *Cursor) Bool() bool { return c.next()&1 == 1 }

// IntN draws an int in [0, n). n <= 0 yields 0. Two bytes are drawn, so the
// modulo bias is negligible for the small n this generator uses; Cursor is a
// generator, not a PRNG.
func (c *Cursor) IntN(n int) int {
	if n <= 0 {
		return 0
	}
	v := int(c.next())<<8 | int(c.next())
	return v % n
}

// Int32 draws a full-range int32, negatives included.
func (c *Cursor) Int32() int32 {
	var v uint32
	for i := 0; i < 4; i++ {
		v = v<<8 | uint32(c.next())
	}
	return int32(v)
}

// Pick draws one of opts, or "" when opts is empty.
func (c *Cursor) Pick(opts []string) string {
	if len(opts) == 0 {
		return ""
	}
	return opts[c.IntN(len(opts))]
}

// Hostile draws 0..maxLen arbitrary bytes as a string. The result may carry
// control characters, ANSI escapes, or invalid UTF-8 — it stands in for the API
// fields the API server does not validate: event and condition messages,
// waiting and terminated reasons, involvedObject field paths, log text.
func (c *Cursor) Hostile(maxLen int) string {
	n := c.IntN(maxLen + 1)
	out := make([]byte, n)
	for i := range out {
		out[i] = c.next()
	}
	return string(out)
}

const nameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789-"

// Name draws a DNS-1123 label of 1..min(maxLen, 63) characters: lowercase
// alphanumerics and dashes, never starting or ending with a dash.
//
// Object names, namespaces and container names are validated by the API server,
// so a real cluster can never present anything else here. Drawing them from
// hostile bytes would make an output-safety property assert something about
// names that cannot exist.
func (c *Cursor) Name(maxLen int) string {
	if maxLen < 1 {
		maxLen = 1
	}
	if maxLen > 63 {
		maxLen = 63
	}
	out := make([]byte, 1+c.IntN(maxLen))
	for i := range out {
		out[i] = nameAlphabet[c.IntN(len(nameAlphabet))]
	}
	if out[0] == '-' {
		out[0] = 'a'
	}
	if out[len(out)-1] == '-' {
		out[len(out)-1] = 'z'
	}
	return string(out)
}

// Time draws an instant within +/-30 days of base, at second resolution —
// metav1.Time serializes seconds, so anything finer would be noise the fuzzer
// spends its budget on.
func (c *Cursor) Time(base time.Time) metav1.Time {
	off := time.Duration(int64(c.Int32())%(30*24*3600)) * time.Second
	return metav1.NewTime(base.Add(off).Truncate(time.Second))
}
