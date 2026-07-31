package policy

import "testing"

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		want    bool
		why     string
	}{
		{"", "", true, "empty pattern matches empty string"},
		{"", "x", false, "empty pattern matches nothing else"},
		{"*", "", true, "star matches the empty string"},
		{"*", "anything/at/all:1.0", true, "star matches everything"},
		{"nginx", "nginx", true, "literal"},
		{"nginx", "nginxx", false, "literal is anchored at both ends"},
		{"registry.example.com/*", "registry.example.com/team/app:1.0", true, "star crosses a slash — path.Match would not"},
		{"registry.example.com/*", "quay.example.org/team/app:1.0", false, "different registry"},
		{"*/app:*", "registry.example.com/team/app:1.0", true, "two stars"},
		{"?", "a", true, "question mark matches one byte"},
		{"?", "", false, "question mark needs a byte"},
		{"?", "ab", false, "question mark matches exactly one"},
		{"a?c", "abc", true, "question mark mid-pattern"},
		{"a.c", "abc", false, "dot is a literal, not a regexp metacharacter"},
		{"a.c", "a.c", true, "dot matches itself"},
		{"a[bc]d", "abd", false, "brackets are literals"},
		{"a[bc]d", "a[bc]d", true, "brackets match themselves"},
		{"**", "abc", true, "adjacent stars collapse"},
		{"*a*b*c*", "xxaxxbxxcxx", true, "many stars backtrack correctly"},
		{"*a*b*c*", "xxaxxcxxbxx", false, "order still matters"},
		{"prod-*", "prod-", true, "trailing star may match nothing"},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.input); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v (%s)", c.pattern, c.input, got, c.want, c.why)
		}
	}
}

// TestGlobMatchIsLinearOnPathologicalInput guards the matcher against the
// exponential backtracking a naive recursive implementation shows. This must
// finish instantly, not in geologic time.
func TestGlobMatchIsLinearOnPathologicalInput(t *testing.T) {
	pattern := "*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*a*b"
	input := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if globMatch(pattern, input) {
		t.Error("no trailing b, so no match")
	}
}
