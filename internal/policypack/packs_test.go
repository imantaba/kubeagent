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

// dottedQuad matches a bare IPv4 address. Used only for the whole-file scan
// below — a rule Message is held to a stricter, list-free rule instead (see
// TestPackCarriesNoHostOrAddress).
var dottedQuad = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// TestPackCarriesNoHostOrAddress runs two checks of deliberately different
// strength, not one blanket scan.
//
// A rule's Message is prose: it is what reaches a terminal, a JSON document,
// a SARIF upload and an HTML report. No closed list of markers can do this
// job — the project's concern is chiefly an *internal* hostname, and an
// internal hostname is exactly what a public-suffix list cannot enumerate;
// every list is only as good as its last update, and the next missing
// suffix passes silently. So a Message is held to an open-ended shape rule
// instead of a list: it may contain no dot, and no "://", at all. A URL
// need not contain a dot (a bare host and port has none), which is why
// "://" is still checked separately. Together the two catch every dotted or
// schemed form a host or address can take — every FQDN, every IPv4 literal
// and every URL — with nothing to maintain.
//
// What they do not catch: a bare single-label word, such as "backend," with
// no dot and no scheme. That limit is real and deliberate, not an
// oversight — a single label is indistinguishable from an ordinary English
// noun, so no textual check can separate the two, and a single label
// carries no domain and identifies no infrastructure on its own. The
// fourteen shipped messages already rely on ordinary single words like
// "Service," "container" and "node"; that is the concrete reason a
// single-label check is impossible here, not merely unattempted.
//
// This is deliberately STRICTER than the rule it enforces: it also refuses
// an innocent dotted token that names no host at all, such as a field path
// like spec.replicas. That is the intended trade, not an oversight — an
// allowlist of "permitted dots" is the same treadmill in reverse, and a
// curated one-clause message never needs one; an author who wants to name a
// field writes "the replicas field" rather than the path itself. A
// maintainer who hits this failure on a message that names no host has
// found a false positive by design, and the fix is to reword the message
// to drop the dot, not to weaken this check.
//
// The whole pack YAML, by contrast, is checked only for "://" and
// dottedQuad — never a blanket dot ban. Unlike a Message, the YAML's
// structural fields legitimately contain dotted text that is not a host: a
// match.labels selector such as app.kubernetes.io/name contains ".io", and
// a path such as spec.template.spec.containers[*].readinessProbe is made of
// dotted segments. "://" and a dotted quad cannot appear in a legitimate
// kind, path, label key or level, so they stay meaningful checks over the
// whole file without banning dots outright.
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
				if strings.Contains(r.Message, ".") {
					t.Errorf("rule %q message contains a dot — it may be a host, so the message must be reworded without one: %q", r.ID, r.Message)
				}
				if strings.Contains(r.Message, "://") {
					t.Errorf("rule %q message contains a URL scheme: %q", r.ID, r.Message)
				}
			}
		})
	}
}
