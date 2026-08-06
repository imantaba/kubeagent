package policypack_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/policy"
	"github.com/imantaba/kubeagent/internal/policypack"
)

// loadPack loads one pack through the real loader. Source is "pack:<name>",
// which is what the CLI uses too: a pack has no filesystem path, so there is
// none to leak into an error.
func loadPack(t *testing.T, name string) []policy.Rule {
	t.Helper()
	data, ok := policypack.Bytes(name)
	if !ok {
		t.Fatalf("Bytes(%q) = false, want the shipped pack", name)
	}
	rules, err := policy.Load([]policy.Document{{Source: "pack:" + name, Data: data}})
	if err != nil {
		t.Fatalf("loading pack %q: %v", name, err)
	}
	return rules
}

// TestEveryPackLoads is the one assertion that covers most of the contract.
// policy.Load already refuses an empty or malformed rule id, a kind that is
// not selectable, a cluster-scoped kind carrying namespace selectors, an
// assert that sets both or neither of path and relation, an unknown level and
// an empty message — and sigs.k8s.io/yaml's UnmarshalStrict refuses an unknown
// key, so a typo in the pack fails here rather than being ignored. Re-asserting
// any of that separately would be testing Load.
//
// The selectable-kind half is also the RBAC promise: the kinds a rule may
// select are exactly the kinds rbacprofile's core rules already grant, so a
// pack can never require a grant kubeagent does not already ask for.
func TestEveryPackLoads(t *testing.T) {
	for _, p := range policypack.All() {
		t.Run(p.Name, func(t *testing.T) {
			if rules := loadPack(t, p.Name); len(rules) == 0 {
				t.Error("the pack loaded but holds no rules")
			}
		})
	}
}

// TestRuleIDsCarryTheirPackPrefix keeps a pack's ids in its own namespace, so
// giving --policy-pack and --policy together cannot collide by accident. Load
// detects a collision across documents; this is what makes one unlikely.
func TestRuleIDsCarryTheirPackPrefix(t *testing.T) {
	for _, p := range policypack.All() {
		t.Run(p.Name, func(t *testing.T) {
			prefix := p.Name + "."
			for _, r := range loadPack(t, p.Name) {
				if !strings.HasPrefix(r.ID, prefix) {
					t.Errorf("rule id %q does not start with %q", r.ID, prefix)
				}
			}
		})
	}
}

// TestNoPackRuleIsCritical is the promise that adding a pack to a pipeline
// cannot fail a build that passed yesterday: gate's default is
// --fail-on critical, and no curated rule reaches that level. An operator who
// wants these to block raises --fail-on warning, which is an explicit act.
func TestNoPackRuleIsCritical(t *testing.T) {
	for _, p := range policypack.All() {
		t.Run(p.Name, func(t *testing.T) {
			for _, r := range loadPack(t, p.Name) {
				if r.Level == policy.LevelCritical {
					t.Errorf("rule %q is critical — adding this pack to a gate would fail it at default settings", r.ID)
				}
			}
		})
	}
}

// hostMarkers are the substrings that would mean a host or a URL had reached
// a rule's message — the same list internal/knownissues checks its prose
// fields against, plus "k8sproject": internal/policypack has no field
// equivalent to knownissues' Docs, so unlike that package there is no field
// here allowed to carry a host, not even the project's own.
var hostMarkers = []string{"://", "http", "www.", ".com", ".net", ".org", ".io", "k8sproject"}

// dottedQuad matches a bare IPv4 address.
var dottedQuad = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// TestPackCarriesNoHostOrAddress runs two checks of deliberately different
// strength, not one blanket scan.
//
// A rule's Message is prose: it is what reaches a terminal, a JSON document,
// a SARIF upload and an HTML report, so it is checked against the full
// hostMarkers list plus dottedQuad — nothing that looks like a URL, a
// domain-shaped word or an address may reach it.
//
// The whole pack YAML is checked only for "://" and dottedQuad, never the
// full marker list. The YAML's structural fields legitimately contain dotted
// text that is not a host: a match.labels selector such as
// app.kubernetes.io/name contains ".io", and a path such as
// spec.template.spec.containers[*].readinessProbe is made of dotted
// segments. "://" and a dotted quad cannot appear in a legitimate kind, path,
// label key or level, so they stay meaningful checks over the whole file —
// the wider marker list would false-positive on the file's own shape.
func TestPackCarriesNoHostOrAddress(t *testing.T) {
	for _, p := range policypack.All() {
		t.Run(p.Name, func(t *testing.T) {
			data, _ := policypack.Bytes(p.Name)
			text := string(data)
			if strings.Contains(text, "://") {
				t.Error(`the pack carries "://" — a rule may not name a URL`)
			}
			if loc := dottedQuad.FindString(text); loc != "" {
				t.Errorf("the pack carries %q — a rule may not name an address", loc)
			}
			for _, r := range loadPack(t, p.Name) {
				lower := strings.ToLower(r.Message)
				for _, m := range hostMarkers {
					if strings.Contains(lower, m) {
						t.Errorf("rule %q message contains %q: %q", r.ID, m, r.Message)
					}
				}
				if loc := dottedQuad.FindString(r.Message); loc != "" {
					t.Errorf("rule %q message carries an address: %q", r.ID, loc)
				}
			}
		})
	}
}
