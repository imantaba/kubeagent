// Package fleetfile decodes the file `kubeagent fleet --fleet-file` reads: the
// list of clusters to sweep, one entry per cluster.
//
// The file NAMES clusters. It never carries a credential, and that is
// structural rather than a rule anyone has to follow: an Entry has three string
// fields and Load decodes with yaml.UnmarshalStrict, so `server:`, `token:`,
// `certificate-authority-data:` and every other kubeconfig field are load
// errors rather than silently ignored keys. Credentials keep coming from the
// kubeconfigs an entry points at, exactly as they did before this package
// existed.
//
// It is pure: no client, no context, no I/O beyond the bytes it is handed — the
// same shape as internal/policy.
//
// Two walls, enforced by internal/fleetfile/imports_test.go. The first is
// internal/fleet's: never internal/remediate, never internal/explain, which is
// what keeps "read-only toward the cluster" and "makes no LLM call" two
// separate, checkable promises. The second is one internal/fleet cannot carry:
// no k8s.io/client-go and no internal/cluster. This package holds kubeconfig
// paths, so "it holds no client" has to be a structural fact rather than a
// stated one.
package fleetfile

import (
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// Entry is one cluster in a fleet file.
//
// sigs.k8s.io/yaml converts YAML to JSON and then uses encoding/json, so these
// are `json:` tags rather than `yaml:` ones — the same as policy.Rule.
type Entry struct {
	// Name is the row identity: what the report calls this cluster. Optional in
	// the file; Load defaults it to Context.
	Name string `json:"name,omitempty"`

	// Kubeconfig is the path to the kubeconfig this cluster is reached through.
	// Optional; internal/cli falls back to --kubeconfig, then $KUBECONFIG, then
	// the default location. This package never opens it and never names it in
	// an error.
	Kubeconfig string `json:"kubeconfig,omitempty"`

	// Context is the kubeconfig context to use. Required, deliberately: an
	// entry naming no context would take its kubeconfig's current-context,
	// which can change under the operator between runs. A checked-in fleet file
	// has to be reproducible, and its identity has to be knowable without
	// loading a kubeconfig.
	Context string `json:"context"`
}

// Load decodes and validates a fleet file's bytes.
//
// It returns entries in file order, with Name already resolved and already
// sanitized, so the caller never has to re-derive either. Every failure is bad
// input discovered before any cluster was touched — internal/cli reports them
// all at exit 4.
func Load(data []byte) ([]Entry, error) {
	var entries []Entry
	if err := yaml.UnmarshalStrict(data, &entries); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("the fleet file names no clusters: a sweep of nothing reporting \"pass\" would look like good news")
	}

	seen := make(map[string]int, len(entries)) // resolved name -> the 1-based entry that took it
	out := make([]Entry, 0, len(entries))
	for i, e := range entries {
		if strings.TrimSpace(e.Context) == "" {
			return nil, fmt.Errorf("entry %d has no context", i+1)
		}

		name := e.Name
		if name == "" {
			name = e.Context
		}
		// Sanitize at ingress: the name reaches a terminal and a JSON document
		// written to be forwarded. safetext.Line already trims, so an empty
		// result is the whole test. Context and Kubeconfig stay raw — they are
		// lookup keys handed to client-go, where a mangled value would silently
		// select nothing.
		name = safetext.Line(name)
		if name == "" {
			return nil, fmt.Errorf("entry %d has an empty name", i+1)
		}

		if first, dup := seen[name]; dup {
			return nil, fmt.Errorf("entry %d and entry %d are both named %q: two rows with one identity make the report ambiguous", first, i+1, name)
		}
		seen[name] = i + 1

		e.Name = name
		out = append(out, e)
	}
	return out, nil
}
