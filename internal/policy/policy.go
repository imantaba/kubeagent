// Package policy evaluates operator-authored rules against Kubernetes objects.
//
// It is pure: no client, no context, no I/O beyond the bytes it is handed. A
// caller reads the cluster and hands the objects in; this package decides what
// violates and returns values. That is what makes a policy rule incapable of
// writing anything — there is nothing here to write with.
//
// It must never import internal/remediate, internal/explain,
// internal/investigate, internal/report, internal/scan or internal/findings.
// internal/findings imports internal/scan and internal/scan imports this
// package, so importing findings would close a cycle — which is why Level is
// declared here rather than reused from internal/findings.
package policy

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Level is how loudly a violation is reported. It mirrors findings.Level in
// spelling but is a distinct type: see the package comment for why.
type Level string

const (
	LevelCritical Level = "critical"
	LevelWarning  Level = "warning"
	LevelInfo     Level = "info"
)

// Op is one of the closed set of comparisons a rule may make. The set is
// closed on purpose: an expression language would be a second thing to fuzz,
// to sandbox and to version.
type Op string

const (
	OpExists     Op = "exists"
	OpNotExists  Op = "notExists"
	OpIn         Op = "in"
	OpNotIn      Op = "notIn"
	OpMatches    Op = "matches"
	OpNotMatches Op = "notMatches"
	OpGt         Op = "gt"
	OpGte        Op = "gte"
	OpLt         Op = "lt"
	OpLte        Op = "lte"
)

// Relation is a claim about another resource in the cluster rather than about
// a field of the matched object.
type Relation string

const (
	RelationHasPDB Relation = "hasPodDisruptionBudget"
	RelationHasHPA Relation = "hasHorizontalPodAutoscaler"
)

// Rule is one policy check, as written in a policy file.
type Rule struct {
	ID      string `json:"id"`
	Match   Match  `json:"match"`
	Assert  Assert `json:"assert"`
	Level   Level  `json:"level"`
	Message string `json:"message"`
}

// Match narrows which resources a rule applies to.
type Match struct {
	Kind            string            `json:"kind"`
	Namespaces      []string          `json:"namespaces,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	NamespaceLabels map[string]string `json:"namespaceLabels,omitempty"`
}

// Assert is one claim. Exactly one of (Path+Op) and Relation is set; the
// loader rejects a rule that sets both or neither.
type Assert struct {
	Path     string   `json:"path,omitempty"`
	Op       Op       `json:"op,omitempty"`
	Values   []string `json:"values,omitempty"`
	Relation Relation `json:"relation,omitempty"`
}

// Violation is one resource failing one rule. A resource produces at most one
// violation per rule: the first failing slot wins.
type Violation struct {
	RuleID    string `json:"ruleId"`
	Level     Level  `json:"level"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Message   string `json:"message"`
	Evidence  string `json:"evidence,omitempty"`
}

// Unevaluated is a rule that could not run because the kind it selects was not
// readable. It carries Level so a caller can decide severity without holding
// the rule set — internal/gate never sees the rules.
//
// A refused read is never a pass.
type Unevaluated struct {
	RuleID string `json:"ruleId"`
	Level  Level  `json:"level"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// Inputs is everything Evaluate needs. The caller does every read; this
// package does none.
type Inputs struct {
	// Objects maps a selectable kind to the objects read for it.
	Objects map[string][]*unstructured.Unstructured
	// Namespaces backs match.namespaceLabels.
	Namespaces []*unstructured.Unstructured
	// PDBs and HPAs back the two relations.
	PDBs []*unstructured.Unstructured
	HPAs []*unstructured.Unstructured
	// Unreadable names kinds whose read was refused or failed. Rules
	// selecting them are reported as not evaluated, never as passing.
	Unreadable map[string]bool
}

// kindInfo is what the evaluator and the loader need to know about a kind.
type kindInfo struct {
	namespaced bool
}

// selectableKinds is exactly the set internal/rbacprofile's core rules already
// grant. Keeping the two in step means a policy file never needs an RBAC grant
// kubeagent does not already ask for, so `rbac print` keeps telling the truth.
//
// Secret is deliberately absent: a violation carries evidence, and evidence
// from a Secret would be secret material rendered into a report, a JSON
// document and a SARIF upload. Event and Lease are absent as carrying no
// policy value.
var selectableKinds = map[string]kindInfo{
	// core/v1
	"ConfigMap":             {namespaced: true},
	"Namespace":             {namespaced: false},
	"Node":                  {namespaced: false},
	"PersistentVolume":      {namespaced: false},
	"PersistentVolumeClaim": {namespaced: true},
	"Pod":                   {namespaced: true},
	"ResourceQuota":         {namespaced: true},
	"Service":               {namespaced: true},
	// apps/v1
	"DaemonSet":   {namespaced: true},
	"Deployment":  {namespaced: true},
	"ReplicaSet":  {namespaced: true},
	"StatefulSet": {namespaced: true},
	// batch/v1
	"CronJob": {namespaced: true},
	"Job":     {namespaced: true},
	// discovery.k8s.io/v1
	"EndpointSlice": {namespaced: true},
	// networking.k8s.io/v1
	"Ingress":       {namespaced: true},
	"IngressClass":  {namespaced: false},
	"NetworkPolicy": {namespaced: true},
	// storage.k8s.io/v1
	"StorageClass": {namespaced: false},
	// policy/v1
	"PodDisruptionBudget": {namespaced: true},
	// autoscaling/v2
	"HorizontalPodAutoscaler": {namespaced: true},
	// admissionregistration.k8s.io/v1
	"MutatingWebhookConfiguration":   {namespaced: false},
	"ValidatingWebhookConfiguration": {namespaced: false},
}

// SelectableKinds returns every kind a rule may select, sorted. Callers read
// the cluster in this order, so sorting keeps the read order deterministic.
func SelectableKinds() []string {
	out := make([]string, 0, len(selectableKinds))
	for k := range selectableKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// KindSelectable reports whether a rule may select this kind.
func KindSelectable(kind string) bool {
	_, ok := selectableKinds[kind]
	return ok
}

// KindNamespaced reports whether a kind is namespaced, and whether it is known
// at all. The loader uses it to refuse namespaceLabels on a cluster-scoped
// kind, where the selector could never match anything.
func KindNamespaced(kind string) (namespaced, known bool) {
	info, ok := selectableKinds[kind]
	return info.namespaced, ok
}

// relationKinds names the workload kinds each relation is meaningful for. A
// DaemonSet runs one pod per node and cannot scale horizontally, so it is
// absent from the HPA list.
var relationKinds = map[Relation]map[string]bool{
	RelationHasPDB: {"Deployment": true, "StatefulSet": true, "ReplicaSet": true, "DaemonSet": true},
	RelationHasHPA: {"Deployment": true, "StatefulSet": true, "ReplicaSet": true},
}

// RelationValidForKind reports whether a relation may be asserted about a kind.
// A mismatch is a load error, not a silent pass.
func RelationValidForKind(r Relation, kind string) bool {
	return relationKinds[r][kind]
}

var validOps = map[Op]bool{
	OpExists: true, OpNotExists: true,
	OpIn: true, OpNotIn: true,
	OpMatches: true, OpNotMatches: true,
	OpGt: true, OpGte: true, OpLt: true, OpLte: true,
}

// ValidOp reports whether an operator is one of the ten.
func ValidOp(o Op) bool { return validOps[o] }

// ValidRelation reports whether a relation is one of the two.
func ValidRelation(r Relation) bool { _, ok := relationKinds[r]; return ok }

// ValidLevel reports whether a level is one of the three.
func ValidLevel(l Level) bool {
	return l == LevelCritical || l == LevelWarning || l == LevelInfo
}

// ReadPlan returns every kind an evaluation of these rules must read: the
// kinds the rules select, plus the supporting lists their matches and
// relations compare against. Sorted and deduplicated, so the read order — and
// with it the report — does not depend on the order rules were written in.
//
// Nothing is read speculatively. A rule set with no relations and no
// namespaceLabels plans exactly the kinds it selects, which is also the RBAC
// it needs.
func ReadPlan(rules []Rule) []string {
	seen := map[string]bool{}
	for _, k := range Kinds(rules) {
		seen[k] = true
	}
	aux := Needs(rules)
	if aux.Namespaces {
		seen["Namespace"] = true
	}
	if aux.PDBs {
		seen["PodDisruptionBudget"] = true
	}
	if aux.HPAs {
		seen["HorizontalPodAutoscaler"] = true
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// InputsFrom assembles Inputs from what the caller read. The supporting lists
// are looked up by kind rather than passed separately, so a caller cannot
// populate Objects and forget PDBs — and cannot drop the unreadable set, which
// is the difference between "no violations" and "not checked".
//
// A supporting kind stays in Objects as well: a rule may legitimately select
// PodDisruptionBudget and assert something about it.
func InputsFrom(objects map[string][]*unstructured.Unstructured, unreadable map[string]bool) Inputs {
	if objects == nil {
		objects = map[string][]*unstructured.Unstructured{}
	}
	if unreadable == nil {
		unreadable = map[string]bool{}
	}
	return Inputs{
		Objects:    objects,
		Namespaces: objects["Namespace"],
		PDBs:       objects["PodDisruptionBudget"],
		HPAs:       objects["HorizontalPodAutoscaler"],
		Unreadable: unreadable,
	}
}
