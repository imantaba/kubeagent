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
// star rather than recursing, so a pattern full of stars stays linear in the
// length of s rather than exponential.
func globMatch(pattern, s string) bool {
	var (
		p, i  int // cursors into pattern and s
		starP = -1
		starI int
	)
	for i < len(s) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case p < len(pattern) && pattern[p] == '*':
			// Remember where the star was, try matching zero bytes first.
			starP = p
			starI = i
			p++
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
