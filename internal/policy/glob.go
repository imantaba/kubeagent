package policy

// globMatch reports whether s matches pattern. Two metacharacters, and only
// two: `*` matches any run of bytes including the empty run and including `/`,
// and `?` matches exactly one byte. Every other byte — `.`, `[`, `\` — is a
// literal.
//
// The standard library's path.Match will not let `*` cross a `/`, which breaks
// the most obvious rule an operator will write:
//
//	registry.example.com/*  against  registry.example.com/team/app:1.0
//
// Hence this. It is iterative, allocates nothing, and backtracks to the last
// star rather than recursing — so it cannot grow the stack and cannot go
// exponential the way a naive recursive translation can. But it is not
// linear: worst case is O(len(pattern) * len(s)), realized by a single star
// followed by a long, almost-matching literal run, where each mismatch
// re-scans nearly the whole literal. A caller must not hand this an unbounded
// value — checkOp's matches/notMatches case caps the compared value at
// maxMatchLen before it reaches here; do not remove that cap on the mistaken
// belief that this function is linear.
func globMatch(pattern, s string) bool {
	var (
		p, i  int // cursors into pattern and s
		starP = -1
		starI int
	)
	for i < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			// Remember where the star was, try matching zero bytes first.
			// This must be checked before the literal-equality case below:
			// s[i] can itself be the byte '*' (an input that happens to
			// contain a literal asterisk), and if the equality check ran
			// first it would consume the pattern's wildcard as a one-byte
			// literal match instead of opening it as a star.
			starP = p
			starI = i
			p++
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case starP >= 0:
			// Mismatch after a star: let the star swallow one more byte.
			p = starP + 1
			starI++
			i = starI
		default:
			return false
		}
	}
	// Trailing stars may match the empty run.
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
