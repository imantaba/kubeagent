// Package operators assesses the health of third-party operator custom
// resources — cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, and the
// Prometheus operator — from the unstructured objects collect fetched. Pure: no
// Kubernetes client, no I/O, unit-tested with fixture objects.
//
// Advisory only. A third-party CRD's opinion of itself, read through field
// paths kubeagent infers, must not drive kubeagent's headline verdict.
//
// This package is also the boundary the raw objects do not cross. Assess reads
// unstructured content and returns only Resource values — namespace, name,
// kind, state, and the operator's own short condition reason. A CR's spec and
// arbitrary status content never reach the report: an Argo CD Application
// embeds a Git URL that can carry a token, a cert-manager Issuer references
// ACME account keys, and a CNPG Cluster names backup credentials.
package operators

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// State is one resource's health as its own operator reports it.
type State string

const (
	StateHealthy     State = "healthy"
	StateProgressing State = "progressing"
	StateUnhealthy   State = "unhealthy"
	StateSuspended   State = "suspended"
	StateUnknown     State = "unknown"
)

// Rule decides one resource's State from its unstructured content. The second
// return is a short, single-line reason, empty for healthy resources (they are
// counted, never enumerated).
type Rule interface {
	Evaluate(obj map[string]any) (State, string)
}

// Adapter describes one operator resource kubeagent knows how to read.
type Adapter struct {
	Operator    string   // "cert-manager"
	Group       string   // "cert-manager.io"
	Version     string   // preferred version to try first: "v1"
	Resource    string   // plural, as discovery reports it: "certificates"
	Kind        string   // "Certificate"
	SuspendPath []string // optional; a truthy bool here ⇒ StateSuspended
	Rule        Rule     // nil ⇒ counted, never judged
}

// Resource is one fetched CR, reduced to what the report may show.
type Resource struct {
	Operator  string `json:"operator"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	State     State  `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

// KindReport is one resource kind's roll-up. Counts are per kind, not per
// operator: 214 unjudged ServiceMonitors must not be averaged into the one
// Prometheus CR's health.
type KindReport struct {
	Kind       string        `json:"kind"`
	APIVersion string        `json:"apiVersion"`          // as served, e.g. "cert-manager.io/v1"
	Judged     bool          `json:"judged"`              // false for adapters with no Rule
	Counts     map[State]int `json:"counts"`              // exact, never truncated
	Unhealthy  []Resource    `json:"unhealthy,omitempty"` // capped at MaxUnhealthyPerKind
	Truncated  int           `json:"truncated,omitempty"` // how many unhealthy were omitted
	Forbidden  bool          `json:"forbidden,omitempty"` // RBAC denied listing this kind
	Error      string        `json:"error,omitempty"`     // any other list failure, redacted
}

// Total is the number of resources counted for this kind.
func (k KindReport) Total() int {
	n := 0
	for _, c := range k.Counts {
		n += c
	}
	return n
}

// OperatorReport is one operator's roll-up across its kinds. An operator with
// no kinds is installed and idle — a different answer from not installed,
// which omits the operator entirely.
type OperatorReport struct {
	Operator    string       `json:"operator"`
	APIVersions []string     `json:"apiVersions,omitempty"`
	Kinds       []KindReport `json:"kinds,omitempty"`
}

// Report is the whole advisory view. Empty when no known operator is installed.
type Report struct {
	Operators []OperatorReport `json:"operators,omitempty"`
}

// Fetched is one adapter's raw result, as collect hands it over. The adapter
// travels with its own result so Assess cannot pair one with the wrong table row.
type Fetched struct {
	Adapter    Adapter
	APIVersion string                      // the group/version actually served
	Items      []unstructured.Unstructured // nil when Forbidden or Err is set
	Forbidden  bool
	Err        error
}
