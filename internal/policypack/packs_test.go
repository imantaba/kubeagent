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

// hostish matches anything that looks like a URL or a bare IPv4 address.
var hostish = regexp.MustCompile(`://|\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// TestPackCarriesNoHostOrAddress is the credential wall. A violation's message
// reaches a terminal, a JSON document, a SARIF upload and an HTML report — all
// of them forwarded artifacts — so no rule may carry a host, a URL or an
// address. The rules assert about shapes, not about anyone's infrastructure.
func TestPackCarriesNoHostOrAddress(t *testing.T) {
	for _, p := range policypack.All() {
		t.Run(p.Name, func(t *testing.T) {
			data, _ := policypack.Bytes(p.Name)
			if loc := hostish.FindString(string(data)); loc != "" {
				t.Errorf("the pack carries %q — a rule may not name a host or an address", loc)
			}
			for _, r := range loadPack(t, p.Name) {
				if loc := hostish.FindString(r.Message); loc != "" {
					t.Errorf("rule %q message carries %q", r.ID, loc)
				}
			}
		})
	}
}
