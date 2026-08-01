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

// MaxCombining is how many combining marks may follow one base character.
//
// Combining marks are not dropped, because they are ordinary text: decomposed
// Vietnamese stacks two on a vowel, and Arabic, Devanagari, Hebrew and Thai
// reach three or four in normal words. Any of those can reach kubeagent in a
// container log. What is not ordinary is an unbounded stack — the "Zalgo"
// trick, where hundreds of marks on one character paint over the terminal rows
// above and below the line. That is the same theft of the operator's screen
// MaxLine already denies a long message. Four is above every real script and
// far below a stack that can reach another row.
const MaxCombining = 4

// Line returns s fit to print: valid UTF-8, on one line, with no control or
// Unicode formatting characters, at most MaxLine runes, and no character
// carrying more than MaxCombining combining marks.
//
// Four rules, in this order:
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
//  3. A combining mark (category M) is kept only while it has a base character
//     to sit on and that base carries fewer than MaxCombining of them; the rest
//     are dropped. A space is not a base, so a mark cannot decorate the gap
//     between two words, and a mark that opens the line has nothing to attach
//     to. A rune dropped by rule 2 does not break the attachment: a mark that
//     follows a zero-width joiner still belongs to the character before it.
//  4. The result is trimmed and truncated to MaxLine runes — runes, not bytes,
//     so a multi-byte character is never cut in half. Marks left dangling at
//     the cut go with it, so the ellipsis is never what they decorate.
//
// Idempotent: Line(Line(s)) == Line(s).
func Line(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	b.Grow(len(s))
	var (
		base  bool // a base character is available for a mark to attach to
		marks int  // marks already attached to it
	)
	for _, r := range s {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', '\u2028', '\u2029':
			r = ' '
		}
		switch {
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue // dropped, and the base it interrupts outlives it
		case r == ' ':
			base, marks = false, 0
		case unicode.Is(unicode.M, r):
			if !base || marks == MaxCombining {
				continue
			}
			marks++
		default:
			base, marks = true, 0
		}
		b.WriteRune(r)
	}

	s = strings.TrimSpace(b.String())
	if r := []rune(s); len(r) > MaxLine {
		cut := r[:MaxLine-1]
		for len(cut) > 0 && unicode.Is(unicode.M, cut[len(cut)-1]) {
			cut = cut[:len(cut)-1]
		}
		return string(cut) + "…"
	}
	return s
}
