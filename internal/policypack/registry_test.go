package policypack

import (
	"io/fs"
	"regexp"
	"testing"
)

// TestEveryEmbeddedPackIsRegistered closes the one gap no per-pack test can
// see.
//
// `//go:embed packs/*.yaml` compiles every file under packs/ into the binary,
// but every other test over the packs — here and in package policypack_test —
// iterates All(). A file with no registry entry is therefore invisible to all
// of them while still travelling in every release artifact: its rules are
// never loaded, never validated and never run, and nothing reports it.
//
// That is the mistake a contributed pack is most likely to make — adding the
// YAML and forgetting the five-line registry entry — which is why it is
// checked here rather than left to review.
//
// Only this direction needs checking. The reverse, a registry entry naming a
// file that is not embedded, already fails TestEveryPackLoads in
// packs_test.go, where Bytes returns false.
func TestEveryEmbeddedPackIsRegistered(t *testing.T) {
	entries, err := fs.ReadDir(files, "packs")
	if err != nil {
		t.Fatalf("reading the embedded packs directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no embedded packs — every assertion below would pass vacuously")
	}

	// Built from the registry, keyed by embedded path, so the same file named
	// by two entries is caught too: one pack's bytes would then ship under two
	// names, and the second name's rule ids would carry the first pack's
	// prefix.
	registered := make(map[string]string, len(packs))
	for _, p := range packs {
		if other, dup := registered[p.file]; dup {
			t.Errorf("packs %q and %q both name %s — one file cannot be two packs", other, p.Name, p.file)
		}
		registered[p.file] = p.Name
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := "packs/" + e.Name()
		if _, ok := registered[path]; !ok {
			t.Errorf("%s is embedded in the binary but no registry entry names it — it ships unlisted, unloaded and untested", path)
		}
	}
}

// nameWidth is the widest pack name the listing can align. It mirrors the
// column width in internal/cli/policy.go, whose runPolicyPacks renders each
// row with "  %-14s %s — %s\n": a longer name pushes its own row's summary
// out of line with every other row.
//
// The constant is duplicated rather than shared because internal/policypack
// may import nothing from kubeagent. A maintainer who changes one is told
// here where the other lives.
const nameWidth = 14

// packNamePattern is the shape a pack name may take. The name is the value an
// operator types after --policy-pack and the join between the listing and the
// flag, so it stays to lowercase letters, digits and interior hyphens: no
// empty name, no uppercase, no whitespace, no dot, no slash, and no leading,
// trailing or doubled hyphen.
var packNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// validPackName reports whether a name is usable as a --policy-pack value and
// fits the listing column. Both halves matter and neither implies the other:
// the pattern is about what an operator can type, the width about what the
// listing can align.
func validPackName(name string) bool {
	return packNamePattern.MatchString(name) && len(name) <= nameWidth
}

// TestPackNamesAreUniqueAndCLISafe checks the registry itself, which no
// per-pack test can reach.
//
// Uniqueness is not cosmetic. Lookup returns the first match and stops, while
// All lists every entry — so a duplicate name silently shadows a pack that is
// still advertised in the listing. It is also what makes
// TestRuleIDsCarryTheirPackPrefix sufficient to rule out a rule-id collision
// between two packs: with unique names, prefixed ids cannot collide across
// packs; without them, they can.
func TestPackNamesAreUniqueAndCLISafe(t *testing.T) {
	if len(packs) == 0 {
		t.Fatal("no packs — every assertion below would pass vacuously")
	}
	seen := make(map[string]bool, len(packs))
	for _, p := range packs {
		if seen[p.Name] {
			t.Errorf("two registry entries are named %q — Lookup returns the first and stops, so the second is unreachable while still listed", p.Name)
		}
		seen[p.Name] = true
		if !validPackName(p.Name) {
			t.Errorf("pack name %q is not usable as a --policy-pack value: want lowercase letters, digits and interior hyphens, at most %d bytes", p.Name, nameWidth)
		}
	}
}

// TestPackNameRulesRefuseHostileNames proves validPackName actually refuses
// what it claims to. Without it the pattern could be `^.*$` and the width
// bound could be missing entirely, and TestPackNamesAreUniqueAndCLISafe would
// still pass on the three shipped names.
func TestPackNameRulesRefuseHostileNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		why  string
	}{
		{"", "no name at all"},
		{"Security", "a leading uppercase letter"},
		{"COST", "all uppercase"},
		{"two words", "whitespace, which the shell would split"},
		{"cost.pack", "a dot"},
		{"team/cost", "a slash"},
		{"-cost", "a leading hyphen, which reads as a flag"},
		{"cost-", "a trailing hyphen"},
		{"cost--pack", "an empty segment"},
		{"cost_pack", "an underscore"},
		{"compliance-cis1", "fifteen bytes, one over the listing column"},
	} {
		if validPackName(tc.name) {
			t.Errorf("validPackName accepts %q (%s), which is not usable as a pack name", tc.name, tc.why)
		}
	}
	for _, name := range []string{"cost", "reliability", "security", "supply-chain", "cis1"} {
		if !validPackName(name) {
			t.Errorf("validPackName refuses %q, which is a well-formed pack name", name)
		}
	}
}
