// Package investigate runs a bounded, read-only, model-driven tool-use loop over
// the scan findings to gather evidence before explaining. It is opt-in and never
// writes: get/list only.
package investigate

import (
	"strings"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// key identifies a cluster object. Kind is the lowercased singular ("pod",
// "deployment", "node", "pvc"); cluster-scoped kinds (node) use namespace "".
type key struct {
	kind, namespace, name string
}

// Scope is the findings-closure guard: the set of objects the investigation may
// read. It is seeded from the scan findings and grows one hop at a time as the
// reader resolves relations (owner/PVC/node), modelling traversal of a finding's
// resource graph. Nothing outside this set is ever read.
type Scope struct {
	allowed map[key]bool
}

// NewScope seeds the reachable set from the flagged workloads: each workload, its
// pods, and each pod's node.
func NewScope(workloads []inventory.Workload) *Scope {
	s := &Scope{allowed: map[key]bool{}}
	for _, w := range workloads {
		s.Add(w.Kind, w.Namespace, w.Name)
		for _, p := range w.Pods {
			s.Add("pod", w.Namespace, p.Name)
			if p.Node != "" {
				s.Add("node", "", p.Node)
			}
		}
	}
	return s
}

// Allowed reports whether the given object is in the reachable set.
func (s *Scope) Allowed(kind, namespace, name string) bool {
	return s.allowed[key{normKind(kind), namespace, name}]
}

// HasName reports whether any in-scope object matches namespace+name, ignoring
// kind. Used to authorize events for an in-scope object without knowing its kind.
func (s *Scope) HasName(namespace, name string) bool {
	for k := range s.allowed {
		if k.namespace == namespace && k.name == name {
			return true
		}
	}
	return false
}

// Add extends the reachable set by one object (called when a relation resolves).
func (s *Scope) Add(kind, namespace, name string) {
	s.allowed[key{normKind(kind), namespace, name}] = true
}

// normKind lowercases a Kubernetes kind and maps the long PVC name to "pvc" so
// callers can use either form.
func normKind(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	if k == "persistentvolumeclaim" {
		return "pvc"
	}
	return k
}
