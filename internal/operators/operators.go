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
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/alert"
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

// MaxUnhealthyPerKind bounds how many unhealthy resources one kind enumerates.
// An Argo CD estate can hold thousands of Applications: counts stay exact, the
// printed list does not, and the remainder is reported rather than dropped.
const MaxUnhealthyPerKind = 20

// Assess reduces each fetched adapter result to states and counts.
// Deterministic: operators and kinds keep the order collect handed them (the
// adapter-table order), and each kind's unhealthy list is sorted by namespace
// then name before it is capped.
func Assess(fetched []Fetched) Report {
	var rep Report
	index := map[string]int{} // operator name → position in rep.Operators
	for _, f := range fetched {
		i, ok := index[f.Adapter.Operator]
		if !ok {
			rep.Operators = append(rep.Operators, OperatorReport{Operator: f.Adapter.Operator})
			i = len(rep.Operators) - 1
			index[f.Adapter.Operator] = i
		}
		op := &rep.Operators[i]
		if f.APIVersion != "" && !contains(op.APIVersions, f.APIVersion) {
			op.APIVersions = append(op.APIVersions, f.APIVersion)
		}
		if k, keep := kindReport(f); keep {
			op.Kinds = append(op.Kinds, k)
		}
	}
	return rep
}

// kindReport builds one kind's roll-up and reports whether it has anything to
// say. A kind with no resources, no denial, and no error is omitted: "installed
// and idle" is carried by the operator's own entry, not by an empty kind line.
func kindReport(f Fetched) (KindReport, bool) {
	k := KindReport{
		Kind:       f.Adapter.Kind,
		APIVersion: f.APIVersion,
		Judged:     f.Adapter.Rule != nil,
		Counts:     map[State]int{},
		Forbidden:  f.Forbidden,
	}
	if f.Err != nil {
		// A cluster's API URL can carry userinfo or an auth-proxy token, and
		// client-go wraps it in a *url.Error. Reduce it to scheme://host.
		k.Error = alert.RedactError(f.Err)
	}
	if k.Forbidden || k.Error != "" {
		return k, true
	}
	if len(f.Items) == 0 {
		return k, false
	}
	var unhealthy []Resource
	for _, item := range f.Items {
		state, reason := evaluate(f.Adapter, item.Object)
		k.Counts[state]++
		if state != StateUnhealthy {
			continue
		}
		unhealthy = append(unhealthy, Resource{
			Operator:  f.Adapter.Operator,
			Kind:      f.Adapter.Kind,
			Namespace: item.GetNamespace(),
			Name:      item.GetName(),
			State:     state,
			Reason:    reason,
		})
	}
	sort.Slice(unhealthy, func(i, j int) bool {
		if unhealthy[i].Namespace != unhealthy[j].Namespace {
			return unhealthy[i].Namespace < unhealthy[j].Namespace
		}
		return unhealthy[i].Name < unhealthy[j].Name
	})
	if len(unhealthy) > MaxUnhealthyPerKind {
		k.Truncated = len(unhealthy) - MaxUnhealthyPerKind
		unhealthy = unhealthy[:MaxUnhealthyPerKind]
	}
	k.Unhealthy = unhealthy
	return k, true
}

// evaluate applies the adapter's suspend path first — a suspended reconciler is
// a deliberate operator choice, and its Ready condition goes stale the moment it
// is parked — then its rule. An adapter with no rule counts, never judges.
func evaluate(a Adapter, obj map[string]any) (State, string) {
	if len(a.SuspendPath) > 0 {
		if v, found, err := unstructured.NestedBool(obj, a.SuspendPath...); err == nil && found && v {
			return StateSuspended, "suspended"
		}
	}
	if a.Rule == nil {
		return StateUnknown, ""
	}
	return a.Rule.Evaluate(obj)
}
