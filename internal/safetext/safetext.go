// Package safetext bounds and sanitizes text that reaches kubeagent from fields
// the Kubernetes API server does not validate — container logs, event and
// condition messages, container-status reasons, certificate subjects — before it
// is put in front of an operator.
//
// Object names, namespaces and container names are NOT this package's problem:
// the API server validates them as DNS-1123 labels, so a real cluster cannot
// present anything hostile there. The unvalidated fields are the ones that
// matter, and the tail of a crashed container's log is the one an unprivileged
// attacker controls outright.
//
// Pure: no I/O, no state, no clock. Safe to call from a detector.
package safetext

import (
	"strings"
	"unicode"
)

// MaxLine is the rune budget for one sanitized line: long enough for any real
// kubelet or scheduler message, short enough that a hostile multi-megabyte one
// cannot own the terminal. The ellipsis a truncated line ends with is inside
// this budget, so Line's result is never longer than MaxLine runes.
const MaxLine = 512

// Line returns s fit to print: valid UTF-8, on one line, with no control or
// Unicode formatting characters, at most MaxLine runes.
//
// Three rules, in this order:
//
//  1. Invalid UTF-8 bytes become U+FFFD. A terminal's handling of a stray
//     continuation byte is its own business, not kubeagent's.
//  2. Whitespace controls (tab, newline, carriage return, vertical tab, form
//     feed) and the Unicode line separators U+2028/U+2029 fold to a space, so a
//     multi-line message reads as words rather than running together. Every
//     other control character is dropped — that covers ESC, which is what makes
//     an ANSI escape sequence an escape sequence, and NUL and BEL. So are the
//     Unicode formatting characters (category Cf): U+202E RIGHT-TO-LEFT OVERRIDE
//     reorders everything after it, and unicode.IsControl does not catch it
//     because it is Cf, not Cc.
//  3. The result is trimmed and truncated to MaxLine runes — runes, not bytes,
//     so a multi-byte character is never cut in half.
//
// Idempotent: Line(Line(s)) == Line(s).
func Line(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', '\u2028', '\u2029':
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > MaxLine {
		return string(r[:MaxLine-1]) + "…"
	}
	return s
}
