// Package policypack holds kubeagent's curated policy packs: rule sets
// compiled into the binary and run by name, so an operator with only a
// container image or a krew install has them.
//
// A pack is data, not code. The bytes here are handed to internal/policy,
// which is the only thing that parses or evaluates them — which is what makes
// a curated rule incapable of writing anything, panicking a scan, or widening
// RBAC. There is no --fix path from a rule.
//
// It holds no client and no context, issues no cluster call and makes no model
// call — two separate promises, and neither implies the other. In particular a
// pack has nothing to do with --explain, which is the model path.
//
// The package imports nothing from kubeagent and nothing outside the standard
// library, which puts it in the same class as internal/jsonschema,
// internal/dashboard, internal/baseline, internal/glob and internal/knownissues
// and makes reaching internal/remediate or internal/explain impossible by
// construction rather than by rule. internal/policypack/imports_test.go
// enforces both halves. The consequence is that this package cannot parse its
// own YAML, so it does not claim to know how many rules a pack holds: the
// caller counts by loading (see internal/cli/policy.go). A number that cannot
// be wrong beats a number that has to be checked.
package policypack

import (
	"embed"
	"sort"
)

//go:embed packs/*.yaml
var files embed.FS

// Pack is one curated rule set, compiled into the binary.
type Pack struct {
	// Name is how an operator selects the pack: `--policy-pack <name>`.
	// Lookup is exact — it is the join between the listing and the flag.
	Name string

	// Summary is one line for the listing: lowercase, no trailing period.
	Summary string

	// file is the embedded path. Unexported: a caller gets bytes, never a
	// path, so nothing downstream can learn where the pack lives.
	file string
}

// packs is the registry, in name order.
var packs = []Pack{
	{
		Name:    "cost",
		Summary: "resource requests and limits, retention and history limits, autoscaler ceilings and claim sizes",
		file:    "packs/cost.yaml",
	},
	{
		Name:    "reliability",
		Summary: "probes, resource requests and limits, replica counts, disruption budgets and image tags",
		file:    "packs/reliability.yaml",
	},
	{
		Name:    "security",
		Summary: "privileged containers, host namespaces and paths, root filesystems, capabilities and service account tokens",
		file:    "packs/security.yaml",
	},
}

// All returns every pack, sorted by name. The slice is fresh, so a caller may
// sort, filter or truncate what it is handed without any of it reaching the
// next caller.
func All() []Pack {
	out := make([]Pack, len(packs))
	copy(out, packs)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns every pack name, sorted.
func Names() []string {
	out := make([]string, 0, len(packs))
	for _, p := range All() {
		out = append(out, p.Name)
	}
	return out
}

// Lookup finds a pack by name. The match is exact: no case folding and no
// fuzzy match, for the same reason `known-issues` refuses one — a near miss
// that silently resolved would run rules the operator did not ask for.
func Lookup(name string) (Pack, bool) {
	for _, p := range packs {
		if p.Name == name {
			return p, true
		}
	}
	return Pack{}, false
}

// Bytes returns a pack's YAML. The result is a fresh copy: a caller that
// mutates what it was handed must not change what the next caller sees.
func Bytes(name string) ([]byte, bool) {
	p, ok := Lookup(name)
	if !ok {
		return nil, false
	}
	data, err := files.ReadFile(p.file)
	if err != nil {
		// The file is embedded at build time, so this cannot happen without a
		// registry entry naming a file that is not there — which the package's
		// own tests catch before a build ships.
		return nil, false
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, true
}
