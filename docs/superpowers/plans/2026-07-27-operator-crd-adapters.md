# Operator/CRD Adapters Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Teach `kubeagent scan --operators` to report the health of cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, and the Prometheus operator without compiling against any of their Go APIs.

**Architecture:** A new pure package `internal/operators` holds a declarative adapter table and two evaluation rules over `unstructured` objects. `internal/collect` gates each adapter on API discovery — an absent group means zero API calls — then lists the served ones through the dynamic client. `main.go` composes the view onto `report.Input`, exactly as `credlint` and `platform` already do; `internal/scan` and `internal/watch` are untouched.

**Tech Stack:** Go 1.26, `k8s.io/client-go` v0.36.2 (`dynamic`, `dynamic/fake`, `discovery`, `discovery/fake`), `k8s.io/apimachinery` `unstructured`. No new module dependency.

**Spec:** [docs/superpowers/specs/2026-07-26-operator-crd-adapters-design.md](../specs/2026-07-26-operator-crd-adapters-design.md)

## Global Constraints

Every task's requirements implicitly include this section.

- **Read-only.** `list` only, on the operator groups. No `get` on individual objects, no watch, no writes, no new verb anywhere. Without `--operators`, kubeagent makes no discovery call and no dynamic call.
- **Advisory only.** The operator report never affects the `Healthy`/`Degraded` verdict, exactly like `--certs` and `--disk-usage`.
- **The report shows metadata and state only** — namespace, name, kind, state, and the operator's own short condition *reason*. It must **never** carry a CR's `spec` or arbitrary `status` content: an Argo CD `Application` embeds a Git URL that can carry a token, a cert-manager `Issuer` references ACME account keys, and a CNPG `Cluster` names backup credentials.
- **Never read a condition's `message`, only its `reason`.** A `reason` is a CamelCase token by API convention; a `message` is arbitrary operator prose that routinely embeds URLs (cert-manager puts ACME order URLs there), and a URL can carry a token.
- **`StateUnknown` is never a problem state.** A field path that misses — renamed field, unseen CRD version — must degrade to "I cannot tell", never to "your database is down". An unrecognized field value maps to `unknown`, never `unhealthy`.
- **No new module dependency.** Only `k8s.io/client-go` and `k8s.io/apimachinery` sub-packages already in `go.mod`. Do not add any operator's own Go API module.
- **`internal/scan` and `internal/watch` are untouched.** No change to `scan.Options`, `scan.Result`, `watch.Config`, or any daemon RBAC.
- **stdlib `flag` only** — no Cobra.
- **20 unhealthy resources enumerated per kind**, sorted by namespace then name, remainder reported as `Truncated`. Counts are always exact.
- **Reasons are single-line and capped at 120 runes.**
- **TDD:** write the failing test first, run it, watch it fail, then implement.
- **No `Co-Authored-By: Claude` trailer** and no Claude/AI attribution in any commit message, comment, doc, or changelog line.
- Go lives at `/usr/local/go/bin` — `export PATH=$PATH:/usr/local/go/bin` before any `go` command.

### Plan decisions that refine the spec

The spec left four points ambiguous or self-contradictory. These are the resolutions; they are binding, and they are what the reviewer should check against.

1. **Where raw `unstructured` stops.** The spec's security section says the reduction to `Resource` happens "in `collect`", but its type list has `collect` hand `Fetched{Items []unstructured.Unstructured}` to `Assess`. The binding rule is the one that matters: **the raw object never leaves `internal/operators`.** `Assess` reads it and returns only `Report`, whose leaves are `Resource` values. `report.Input.Operators` is `*operators.Report`, which structurally cannot carry spec content. Task 2 pins this with a test.
2. **`FieldRule` needs a `Suspended` value set.** The spec's rule details require Argo CD's `Suspended` → `StateSuspended`, but the struct as written has no set for it. `FieldRule` gains `Suspended []string`.
3. **`OperatorReport` needs `APIVersions`.** The spec's `collect` behaviour mentions recording served versions "in `APIVersions`", and the text sample prints them per operator, but the type is never defined. `OperatorReport` gains `APIVersions []string`. An operator that is installed with every kind empty is still listed with its versions — "installed and idle" is a different answer from "not installed".
4. **Version tolerance is a fallback, not a rejection.** The spec both says an unrecognized served version falls back to the served one *and* that it is "reported installed, resources not assessed". These cannot both hold. The resolution: if the adapter's preferred version is served, use it; otherwise use the group's preferred version and record what was actually read. A field path that does not exist in that version yields `unknown`, which is the degradation the design already relies on. The "version unrecognized" state and its type field are therefore **not implemented** — they would be dead code.
5. **`KUBEAGENT_OPERATORS`** is implemented as the `--operators` flag's *default*, the idiom already used by `main.go:355` (`--include-local`). No other scan flag reads an env var, but the spec calls for the override and this is the in-repo way to do it.

## File structure

| File | Responsibility |
| --- | --- |
| `internal/operators/operators.go` | Types (`State`, `Rule`, `Adapter`, `Resource`, `KindReport`, `OperatorReport`, `Report`, `Fetched`) and `Assess`. |
| `internal/operators/rules.go` | `ConditionRule`, `FieldRule`, and the reason-shortening helpers. |
| `internal/operators/adapters.go` | `Adapters()` — the ten-row table. One row per operator resource. |
| `internal/operators/rules_test.go` | Rule semantics, including the never-read-the-message guard. |
| `internal/operators/operators_test.go` | `Assess`: grouping, counts, cap, sort, `Forbidden`, error redaction, the no-spec-content guard. |
| `internal/operators/adapters_test.go` | One literal fixture set per table row, plus a completeness check that fails when a row has no fixture. |
| `internal/collect/operators.go` | `OperatorResources` — discovery gate, version choice, namespace scoping, failure isolation. New file: `collect.go` is already 527 lines and this brings a distinct import set. |
| `internal/collect/operators_test.go` | `dynamic/fake` + `discovery/fake` coverage of the gate. |
| `internal/cluster/client.go` | `restConfig` extraction plus `NewDynamicClients`. |
| `internal/report/report.go` | `Input.Operators`, the JSON field, `operatorsRender`, `printOperators`. |
| `main.go` | `--operators` flag, usage line, wiring. |
| `deploy/rbac-operators.yaml` | Scan-only ClusterRole + binding. No Helm values, no chart bump. |
| `website/docs/features/operators.md` | Feature page. |
| `chaos/run.sh` | Scenario 16. |

---

### Task 1: `internal/operators` types and rules

**Files:**
- Create: `internal/operators/operators.go`
- Create: `internal/operators/rules.go`
- Test: `internal/operators/rules_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `State` (`StateHealthy`, `StateProgressing`, `StateUnhealthy`, `StateSuspended`, `StateUnknown`), `Rule` interface with `Evaluate(obj map[string]any) (State, string)`, structs `Adapter`, `Resource`, `KindReport`, `OperatorReport`, `Report`, `Fetched`, and the two rule types `ConditionRule{Type string}` and `FieldRule{Path, Healthy, Progressing, Unhealthy, Suspended []string}`. Constants `MaxUnhealthyPerKind = 20` and unexported `maxReasonRunes = 120`. Unexported helpers `shortReason`, `truncateRunes`, `contains`, `fieldLabel`.

`Assess` is Task 2. This task compiles the types and makes the rules green.

- [ ] **Step 1: Write the failing test**

Create `internal/operators/rules_test.go`:

```go
package operators

import (
	"strings"
	"testing"
)

// condObj builds a CR carrying one status condition.
func condObj(condType, status, reason, message string) map[string]any {
	c := map[string]any{"type": condType, "status": status}
	if reason != "" {
		c["reason"] = reason
	}
	if message != "" {
		c["message"] = message
	}
	return map[string]any{"status": map[string]any{"conditions": []any{c}}}
}

func TestConditionRule_TrueIsHealthyWithNoReason(t *testing.T) {
	state, reason := ConditionRule{Type: "Ready"}.Evaluate(condObj("Ready", "True", "Issued", ""))
	if state != StateHealthy {
		t.Errorf("state = %q, want %q", state, StateHealthy)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty (healthy resources are never enumerated)", reason)
	}
}

func TestConditionRule_FalseIsUnhealthyAndNamesTheReason(t *testing.T) {
	state, reason := ConditionRule{Type: "Ready"}.Evaluate(condObj("Ready", "False", "IssuerNotFound", ""))
	if state != StateUnhealthy {
		t.Errorf("state = %q, want %q", state, StateUnhealthy)
	}
	if reason != "Ready=False: IssuerNotFound" {
		t.Errorf("reason = %q, want %q", reason, "Ready=False: IssuerNotFound")
	}
}

func TestConditionRule_UnknownIsProgressing(t *testing.T) {
	state, _ := ConditionRule{Type: "Ready"}.Evaluate(condObj("Ready", "Unknown", "Pending", ""))
	if state != StateProgressing {
		t.Errorf("state = %q, want %q", state, StateProgressing)
	}
}

func TestConditionRule_MissingConditionIsUnknown(t *testing.T) {
	// The condition list exists but carries a different type: kubeagent cannot
	// tell, and "cannot tell" must never read as an outage.
	state, _ := ConditionRule{Type: "Ready"}.Evaluate(condObj("Synced", "False", "Whatever", ""))
	if state != StateUnknown {
		t.Errorf("state = %q, want %q", state, StateUnknown)
	}
}

func TestConditionRule_NoStatusAtAllIsUnknown(t *testing.T) {
	state, _ := ConditionRule{Type: "Ready"}.Evaluate(map[string]any{"spec": map[string]any{}})
	if state != StateUnknown {
		t.Errorf("state = %q, want %q", state, StateUnknown)
	}
}

func TestConditionRule_NeverReportsTheConditionMessage(t *testing.T) {
	// A condition message is arbitrary operator prose. cert-manager puts ACME
	// order URLs in it, and a URL can carry a token — only the reason is read.
	obj := condObj("Ready", "False", "Pending",
		"waiting on order https://acme.example.invalid/order/abc?token=LEAKED")
	_, reason := ConditionRule{Type: "Ready"}.Evaluate(obj)
	for _, bad := range []string{"acme.example.invalid", "LEAKED", "https://"} {
		if strings.Contains(reason, bad) {
			t.Errorf("reason %q leaked the condition message (found %q)", reason, bad)
		}
	}
}

func TestConditionRule_ReasonIsSingleLineAndCapped(t *testing.T) {
	obj := condObj("Ready", "False", strings.Repeat("x", 400)+"\nsecond line", "")
	_, reason := ConditionRule{Type: "Ready"}.Evaluate(obj)
	if strings.ContainsAny(reason, "\r\n") {
		t.Errorf("reason %q spans more than one line", reason)
	}
	if n := len([]rune(reason)); n > maxReasonRunes+1 { // +1 for the ellipsis
		t.Errorf("reason is %d runes, want at most %d", n, maxReasonRunes+1)
	}
}

func TestFieldRule_MapsEachValueSet(t *testing.T) {
	rule := FieldRule{
		Path:        []string{"status", "health", "status"},
		Healthy:     []string{"Healthy"},
		Progressing: []string{"Progressing"},
		Unhealthy:   []string{"Degraded", "Missing"},
		Suspended:   []string{"Suspended"},
	}
	cases := map[string]State{
		"Healthy":     StateHealthy,
		"Progressing": StateProgressing,
		"Degraded":    StateUnhealthy,
		"Missing":     StateUnhealthy,
		"Suspended":   StateSuspended,
	}
	for value, want := range cases {
		obj := map[string]any{"status": map[string]any{"health": map[string]any{"status": value}}}
		if got, _ := rule.Evaluate(obj); got != want {
			t.Errorf("value %q: state = %q, want %q", value, got, want)
		}
	}
}

func TestFieldRule_UnrecognizedValueIsUnknown(t *testing.T) {
	// A value from a CRD version kubeagent has not seen must not be an outage.
	rule := FieldRule{Path: []string{"status", "robustness"}, Healthy: []string{"healthy"}, Unhealthy: []string{"degraded"}}
	obj := map[string]any{"status": map[string]any{"robustness": "rebuilding-v3"}}
	if got, _ := rule.Evaluate(obj); got != StateUnknown {
		t.Errorf("state = %q, want %q", got, StateUnknown)
	}
}

func TestFieldRule_MissingPathIsUnknown(t *testing.T) {
	rule := FieldRule{Path: []string{"status", "robustness"}, Healthy: []string{"healthy"}}
	if got, _ := rule.Evaluate(map[string]any{"status": map[string]any{}}); got != StateUnknown {
		t.Errorf("state = %q, want %q", got, StateUnknown)
	}
}

func TestFieldRule_ReasonNamesTheFieldBelowStatus(t *testing.T) {
	rule := FieldRule{Path: []string{"status", "robustness"}, Unhealthy: []string{"degraded"}}
	obj := map[string]any{"status": map[string]any{"robustness": "degraded"}}
	_, reason := rule.Evaluate(obj)
	if reason != "robustness=degraded" {
		t.Errorf("reason = %q, want %q", reason, "robustness=degraded")
	}

	nested := FieldRule{Path: []string{"status", "health", "status"}, Unhealthy: []string{"Degraded"}}
	obj = map[string]any{"status": map[string]any{"health": map[string]any{"status": "Degraded"}}}
	_, reason = nested.Evaluate(obj)
	if reason != "health.status=Degraded" {
		t.Errorf("reason = %q, want %q", reason, "health.status=Degraded")
	}
}

func TestFieldRule_HealthyReportsNoReason(t *testing.T) {
	rule := FieldRule{Path: []string{"status", "robustness"}, Healthy: []string{"healthy"}}
	obj := map[string]any{"status": map[string]any{"robustness": "healthy"}}
	if _, reason := rule.Evaluate(obj); reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/operators/ -run 'TestConditionRule|TestFieldRule' -v
```

Expected: FAIL to build — `undefined: ConditionRule`, `undefined: StateHealthy`, and so on.

- [ ] **Step 3: Write the types**

Create `internal/operators/operators.go`:

```go
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
```

- [ ] **Step 4: Write the rules**

Create `internal/operators/rules.go`:

```go
package operators

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// maxReasonRunes caps a reported reason. An operator can put an arbitrarily
// long string in a condition's reason field; one CR must not flood the report.
const maxReasonRunes = 120

// ConditionRule reads .status.conditions[type=Type].status, the Kubernetes API
// convention: "True" ⇒ healthy, "False" ⇒ unhealthy, "Unknown" ⇒ progressing,
// the condition absent ⇒ unknown.
//
// Only the condition's reason is reported, never its message. A reason is a
// CamelCase token by API convention; a message is arbitrary operator prose that
// routinely embeds URLs — cert-manager puts ACME order URLs in it — and a URL
// can carry a token.
type ConditionRule struct{ Type string }

// Evaluate implements Rule.
func (r ConditionRule) Evaluate(obj map[string]any) (State, string) {
	conds, found, err := unstructured.NestedSlice(obj, "status", "conditions")
	if err != nil || !found {
		return StateUnknown, ""
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t != r.Type {
			continue
		}
		status, _, _ := unstructured.NestedString(m, "status")
		reason, _, _ := unstructured.NestedString(m, "reason")
		switch status {
		case "True":
			return StateHealthy, ""
		case "False":
			return StateUnhealthy, conditionReason(r.Type, status, reason)
		case "Unknown":
			return StateProgressing, conditionReason(r.Type, status, reason)
		}
		return StateUnknown, ""
	}
	return StateUnknown, ""
}

// conditionReason renders "Type=Status", plus the operator's reason when set.
func conditionReason(condType, status, reason string) string {
	out := condType + "=" + status
	if reason != "" {
		out += ": " + reason
	}
	return shortReason(out)
}

// FieldRule reads a string at Path and maps it through explicit value sets.
// Matching is exact and case-sensitive: Longhorn writes "healthy", Argo CD
// writes "Healthy". A value in no set ⇒ unknown — an unrecognized value from a
// CRD version kubeagent has not seen must never be reported as an outage.
type FieldRule struct {
	Path        []string
	Healthy     []string
	Progressing []string
	Unhealthy   []string
	Suspended   []string
}

// Evaluate implements Rule.
func (r FieldRule) Evaluate(obj map[string]any) (State, string) {
	v, found, err := unstructured.NestedString(obj, r.Path...)
	if err != nil || !found || v == "" {
		return StateUnknown, ""
	}
	label := shortReason(fieldLabel(r.Path) + "=" + v)
	switch {
	case contains(r.Healthy, v):
		return StateHealthy, ""
	case contains(r.Progressing, v):
		return StateProgressing, label
	case contains(r.Suspended, v):
		return StateSuspended, label
	case contains(r.Unhealthy, v):
		return StateUnhealthy, label
	}
	return StateUnknown, ""
}

// fieldLabel names the field for a report line: the path with a leading
// "status" element dropped, joined with dots — "robustness", "health.status".
func fieldLabel(path []string) string {
	if len(path) > 1 && path[0] == "status" {
		path = path[1:]
	}
	return strings.Join(path, ".")
}

// shortReason reduces a reason to one line of at most maxReasonRunes.
func shortReason(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return truncateRunes(strings.TrimSpace(s), maxReasonRunes)
}

// truncateRunes shortens s to max runes, marking that it cut. Local to this
// package on purpose: collect has an unexported twin, and collect imports
// operators — sharing it would invert the dependency.
func truncateRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func contains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/operators/ -v && go vet ./internal/operators/
```

Expected: PASS for every `TestConditionRule_*` and `TestFieldRule_*`; `go vet` silent.

- [ ] **Step 6: Commit**

```bash
git add internal/operators/operators.go internal/operators/rules.go internal/operators/rules_test.go
git commit -m "feat(operators): types and the two CR evaluation rules

ConditionRule reads the Kubernetes conditions convention; FieldRule maps a
string field through explicit value sets. Both degrade to unknown when the
path misses or the value is unrecognized: a drifted heuristic must report a
missing signal, not a false outage.

Only a condition's reason is read, never its message. A reason is a CamelCase
token by convention; a message is arbitrary operator prose that routinely
embeds URLs, and a URL can carry a token."
```

---

### Task 2: `Assess` — the pure roll-up

**Files:**
- Modify: `internal/operators/operators.go` (append `Assess` and its helpers)
- Test: `internal/operators/operators_test.go`

**Interfaces:**
- Consumes: every type from Task 1.
- Produces: `func Assess(fetched []Fetched) Report`, `const MaxUnhealthyPerKind = 20`.

- [ ] **Step 1: Write the failing test**

Create `internal/operators/operators_test.go`:

```go
package operators

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// cr builds an unstructured CR with the given namespace/name and status content.
func cr(namespace, name string, status map[string]any) unstructured.Unstructured {
	obj := map[string]any{
		"metadata": map[string]any{"name": name},
	}
	if namespace != "" {
		obj["metadata"].(map[string]any)["namespace"] = namespace
	}
	if status != nil {
		obj["status"] = status
	}
	return unstructured.Unstructured{Object: obj}
}

// ready builds a status carrying one Ready condition.
func ready(status, reason string) map[string]any {
	return map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": status, "reason": reason},
	}}
}

var certAdapter = Adapter{
	Operator: "cert-manager", Group: "cert-manager.io", Version: "v1",
	Resource: "certificates", Kind: "Certificate", Rule: ConditionRule{Type: "Ready"},
}

func TestAssess_GroupsKindsUnderTheirOperatorInFetchOrder(t *testing.T) {
	issuers := Adapter{Operator: "cert-manager", Group: "cert-manager.io", Version: "v1",
		Resource: "issuers", Kind: "Issuer", Rule: ConditionRule{Type: "Ready"}}
	argo := Adapter{Operator: "Argo CD", Group: "argoproj.io", Version: "v1alpha1",
		Resource: "applications", Kind: "Application", Rule: FieldRule{
			Path: []string{"status", "health", "status"}, Healthy: []string{"Healthy"}}}

	rep := Assess([]Fetched{
		{Adapter: certAdapter, APIVersion: "cert-manager.io/v1", Items: []unstructured.Unstructured{cr("shop", "web", ready("True", ""))}},
		{Adapter: issuers, APIVersion: "cert-manager.io/v1", Items: []unstructured.Unstructured{cr("shop", "ca", ready("True", ""))}},
		{Adapter: argo, APIVersion: "argoproj.io/v1alpha1", Items: []unstructured.Unstructured{
			cr("argocd", "app", map[string]any{"health": map[string]any{"status": "Healthy"}})}},
	})

	if len(rep.Operators) != 2 {
		t.Fatalf("got %d operators, want 2", len(rep.Operators))
	}
	if rep.Operators[0].Operator != "cert-manager" || rep.Operators[1].Operator != "Argo CD" {
		t.Errorf("operator order = %q, %q; want cert-manager then Argo CD", rep.Operators[0].Operator, rep.Operators[1].Operator)
	}
	if got := len(rep.Operators[0].Kinds); got != 2 {
		t.Errorf("cert-manager kinds = %d, want 2", got)
	}
	if got := rep.Operators[0].APIVersions; len(got) != 1 || got[0] != "cert-manager.io/v1" {
		t.Errorf("apiVersions = %v, want [cert-manager.io/v1] deduped", got)
	}
}

func TestAssess_EmptyKindIsOmittedButTheOperatorSurvives(t *testing.T) {
	// "Installed and idle" is a different answer from "not installed".
	rep := Assess([]Fetched{{Adapter: certAdapter, APIVersion: "cert-manager.io/v1"}})
	if len(rep.Operators) != 1 {
		t.Fatalf("got %d operators, want 1 (installed, idle)", len(rep.Operators))
	}
	if len(rep.Operators[0].Kinds) != 0 {
		t.Errorf("kinds = %d, want 0 (an empty kind prints nothing)", len(rep.Operators[0].Kinds))
	}
	if got := rep.Operators[0].APIVersions; len(got) != 1 {
		t.Errorf("apiVersions = %v, want the served version recorded", got)
	}
}

func TestAssess_CountsAreExactAndUnhealthyIsCapped(t *testing.T) {
	var items []unstructured.Unstructured
	for i := 0; i < 25; i++ {
		items = append(items, cr("shop", fmt.Sprintf("bad-%02d", i), ready("False", "IssuerNotFound")))
	}
	items = append(items, cr("shop", "good", ready("True", "")))

	rep := Assess([]Fetched{{Adapter: certAdapter, APIVersion: "cert-manager.io/v1", Items: items}})
	k := rep.Operators[0].Kinds[0]
	if k.Counts[StateUnhealthy] != 25 || k.Counts[StateHealthy] != 1 {
		t.Errorf("counts = %v, want 25 unhealthy and 1 healthy (exact, never truncated)", k.Counts)
	}
	if len(k.Unhealthy) != MaxUnhealthyPerKind {
		t.Errorf("enumerated %d, want %d", len(k.Unhealthy), MaxUnhealthyPerKind)
	}
	if k.Truncated != 5 {
		t.Errorf("truncated = %d, want 5", k.Truncated)
	}
	if k.Total() != 26 {
		t.Errorf("total = %d, want 26", k.Total())
	}
}

func TestAssess_UnhealthyIsSortedByNamespaceThenName(t *testing.T) {
	rep := Assess([]Fetched{{Adapter: certAdapter, APIVersion: "cert-manager.io/v1", Items: []unstructured.Unstructured{
		cr("shop", "zeta", ready("False", "X")),
		cr("infra", "beta", ready("False", "X")),
		cr("shop", "alpha", ready("False", "X")),
	}}})
	var got []string
	for _, r := range rep.Operators[0].Kinds[0].Unhealthy {
		got = append(got, r.Namespace+"/"+r.Name)
	}
	want := []string{"infra/beta", "shop/alpha", "shop/zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestAssess_ForbiddenKindIsKeptWithNoCounts(t *testing.T) {
	rep := Assess([]Fetched{{Adapter: certAdapter, APIVersion: "cert-manager.io/v1", Forbidden: true}})
	k := rep.Operators[0].Kinds[0]
	if !k.Forbidden {
		t.Error("Forbidden = false, want true")
	}
	if k.Total() != 0 {
		t.Errorf("total = %d, want 0 (nothing was listed)", k.Total())
	}
}

func TestAssess_ListErrorIsRecordedAndRedacted(t *testing.T) {
	// A kubeconfig server URL can carry basic-auth userinfo or an auth-proxy
	// token, and client-go returns it inside a *url.Error.
	err := &url.Error{
		Op:  "Get",
		URL: "https://user:hunter2@api.internal.invalid/apis/cert-manager.io/v1/certificates?token=LEAKED",
		Err: errors.New("connection refused"),
	}
	rep := Assess([]Fetched{{Adapter: certAdapter, APIVersion: "cert-manager.io/v1", Err: err}})
	got := rep.Operators[0].Kinds[0].Error
	if got == "" {
		t.Fatal("Error is empty, want the failure recorded")
	}
	for _, bad := range []string{"hunter2", "LEAKED", "/apis/"} {
		if strings.Contains(got, bad) {
			t.Errorf("error %q leaked %q", got, bad)
		}
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("error %q dropped the underlying cause", got)
	}
}

func TestAssess_NilRuleCountsWithoutJudging(t *testing.T) {
	// ServiceMonitor has no .status at all: counted so the report can say the
	// Prometheus operator is installed and how much it scrapes, never judged.
	sm := Adapter{Operator: "Prometheus operator", Group: "monitoring.coreos.com", Version: "v1",
		Resource: "servicemonitors", Kind: "ServiceMonitor"}
	rep := Assess([]Fetched{{Adapter: sm, APIVersion: "monitoring.coreos.com/v1", Items: []unstructured.Unstructured{
		cr("monitoring", "a", nil), cr("monitoring", "b", nil),
	}}})
	k := rep.Operators[0].Kinds[0]
	if k.Judged {
		t.Error("Judged = true, want false for an adapter with no rule")
	}
	if k.Total() != 2 {
		t.Errorf("total = %d, want 2", k.Total())
	}
	if len(k.Unhealthy) != 0 {
		t.Errorf("enumerated %d unhealthy, want 0 (nothing was judged)", len(k.Unhealthy))
	}
}

func TestAssess_SuspendPathBeatsTheRule(t *testing.T) {
	// A suspended Flux reconciler leaves a stale Ready condition behind. The
	// parked state is a deliberate operator choice, not an incident.
	flux := Adapter{Operator: "Flux", Group: "kustomize.toolkit.fluxcd.io", Version: "v1",
		Resource: "kustomizations", Kind: "Kustomization",
		SuspendPath: []string{"spec", "suspend"}, Rule: ConditionRule{Type: "Ready"}}
	obj := cr("flux-system", "apps", ready("False", "BuildFailed"))
	obj.Object["spec"] = map[string]any{"suspend": true}

	rep := Assess([]Fetched{{Adapter: flux, APIVersion: "kustomize.toolkit.fluxcd.io/v1",
		Items: []unstructured.Unstructured{obj}}})
	k := rep.Operators[0].Kinds[0]
	if k.Counts[StateSuspended] != 1 {
		t.Errorf("counts = %v, want 1 suspended", k.Counts)
	}
	if len(k.Unhealthy) != 0 {
		t.Errorf("enumerated %d unhealthy, want 0", len(k.Unhealthy))
	}
}

func TestAssess_ReportCarriesNoSpecContent(t *testing.T) {
	// The structural guard: whatever a CR holds, only metadata and state cross
	// into the Report. An Argo Application's repoURL can embed a token.
	obj := cr("argocd", "app", map[string]any{"health": map[string]any{"status": "Degraded"}})
	obj.Object["spec"] = map[string]any{
		"source": map[string]any{"repoURL": "https://x-token:PLANTEDSECRET@git.invalid/o/r.git"},
	}
	obj.Object["status"].(map[string]any)["summary"] = map[string]any{"images": []any{"registry.invalid/PLANTEDSECRET:1"}}

	argo := Adapter{Operator: "Argo CD", Group: "argoproj.io", Version: "v1alpha1",
		Resource: "applications", Kind: "Application", Rule: FieldRule{
			Path: []string{"status", "health", "status"}, Unhealthy: []string{"Degraded"}}}
	rep := Assess([]Fetched{{Adapter: argo, APIVersion: "argoproj.io/v1alpha1",
		Items: []unstructured.Unstructured{obj}}})

	blob, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshalling the report: %v", err)
	}
	if strings.Contains(string(blob), "PLANTEDSECRET") {
		t.Fatalf("the report carried CR content: %s", blob)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/operators/ -run TestAssess -v
```

Expected: FAIL to build — `undefined: Assess`, `undefined: MaxUnhealthyPerKind`.

- [ ] **Step 3: Implement `Assess`**

Append to `internal/operators/operators.go` (and add `"sort"` plus `"github.com/imantaba/kubeagent/internal/alert"` to its imports):

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/operators/ -v && go vet ./internal/operators/
```

Expected: PASS, every `TestAssess_*` included.

- [ ] **Step 5: Commit**

```bash
git add internal/operators/operators.go internal/operators/operators_test.go
git commit -m "feat(operators): Assess rolls fetched CRs up per kind

Counts are per kind, not per operator: 214 unjudged ServiceMonitors must not
be averaged into the one Prometheus CR's health. Counts stay exact while the
enumerated unhealthy list is sorted and capped at 20, with the remainder
reported rather than silently dropped.

Assess is where the raw unstructured objects stop. It returns only Resource
values — namespace, name, kind, state, reason — so no CR spec content can
reach the report; a test marshals the whole report and asserts a planted
secret is absent."
```

---

### Task 3: The adapter table and its fixtures

**Files:**
- Create: `internal/operators/adapters.go`
- Test: `internal/operators/adapters_test.go`

**Interfaces:**
- Consumes: `Adapter`, `ConditionRule`, `FieldRule`, `Assess`, `Fetched` from Tasks 1-2.
- Produces: `func Adapters() []Adapter` — ten rows, in report order.

**Note on the fixture test's shape:** the fixtures below are hand-written literals per row, taken from each project's documented CR shape. They are *not* generated from the adapter's own path, which is the failure mode the spec rules out — a generated fixture makes a wrong path and an absent field indistinguishable. `TestAdapters_EveryRowHasAFixture` is the enforcement: adding a table row without a fixture fails the suite.

- [ ] **Step 1: Write the failing test**

Create `internal/operators/adapters_test.go`:

```go
package operators

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// adapterFixture pins one table row against CR shapes its project documents.
// healthy and unhealthy are literal status blocks — never derived from the
// adapter's own path, which is what makes a wrong path detectable here.
type adapterFixture struct {
	kind      string
	healthy   map[string]any
	unhealthy map[string]any
	// missing is a status block with the rule's field absent. Always unknown.
	missing map[string]any
}

func readyCond(status, reason string) map[string]any {
	return map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": status, "reason": reason},
	}}
}

func adapterFixtures() []adapterFixture {
	otherCond := map[string]any{"conditions": []any{
		map[string]any{"type": "Synced", "status": "True"},
	}}
	return []adapterFixture{
		{kind: "Certificate", healthy: readyCond("True", "Ready"), unhealthy: readyCond("False", "IssuerNotFound"), missing: otherCond},
		{kind: "Issuer", healthy: readyCond("True", "IsReady"), unhealthy: readyCond("False", "ErrInitIssuer"), missing: otherCond},
		{kind: "ClusterIssuer", healthy: readyCond("True", "IsReady"), unhealthy: readyCond("False", "ErrGetKeyPair"), missing: otherCond},
		{kind: "Cluster", healthy: readyCond("True", "ClusterIsReady"), unhealthy: readyCond("False", "FailedInstance"), missing: otherCond},
		{
			kind:      "Volume",
			healthy:   map[string]any{"robustness": "healthy", "state": "attached"},
			unhealthy: map[string]any{"robustness": "faulted", "state": "detached"},
			missing:   map[string]any{"state": "attached"},
		},
		{
			kind:      "Application",
			healthy:   map[string]any{"health": map[string]any{"status": "Healthy"}, "sync": map[string]any{"status": "Synced"}},
			unhealthy: map[string]any{"health": map[string]any{"status": "Degraded"}, "sync": map[string]any{"status": "OutOfSync"}},
			missing:   map[string]any{"sync": map[string]any{"status": "Synced"}},
		},
		{kind: "Kustomization", healthy: readyCond("True", "ReconciliationSucceeded"), unhealthy: readyCond("False", "BuildFailed"), missing: otherCond},
		{kind: "HelmRelease", healthy: readyCond("True", "InstallSucceeded"), unhealthy: readyCond("False", "UpgradeFailed"), missing: otherCond},
		{kind: "Prometheus", healthy: availableCond("True", ""), unhealthy: availableCond("False", "SomePodsNotReady"), missing: readyCond("True", "")},
		// ServiceMonitor has no .status at all and no rule: every fixture is unknown.
		{kind: "ServiceMonitor", healthy: nil, unhealthy: nil, missing: nil},
	}
}

func availableCond(status, reason string) map[string]any {
	return map[string]any{"conditions": []any{
		map[string]any{"type": "Available", "status": status, "reason": reason},
	}}
}

// stateFor runs one CR through the whole adapter path and returns its state.
func stateFor(t *testing.T, a Adapter, status map[string]any) State {
	t.Helper()
	obj := map[string]any{"metadata": map[string]any{"namespace": "ns", "name": "x"}}
	if status != nil {
		obj["status"] = status
	}
	rep := Assess([]Fetched{{
		Adapter:    a,
		APIVersion: a.Group + "/" + a.Version,
		Items:      []unstructured.Unstructured{{Object: obj}},
	}})
	k := rep.Operators[0].Kinds[0]
	for state, n := range k.Counts {
		if n > 0 {
			return state
		}
	}
	t.Fatalf("adapter %s produced no counted state", a.Kind)
	return ""
}

func TestAdapters_EveryRowHasAFixture(t *testing.T) {
	// An adapter table row without a fixture test is incomplete work.
	have := map[string]bool{}
	for _, f := range adapterFixtures() {
		have[f.kind] = true
	}
	for _, a := range Adapters() {
		if !have[a.Kind] {
			t.Errorf("adapter %s/%s has no fixture in adapterFixtures()", a.Operator, a.Kind)
		}
	}
	if got, want := len(Adapters()), 10; got != want {
		t.Errorf("Adapters() has %d rows, want %d", got, want)
	}
}

func TestAdapters_FixturesPinEveryRow(t *testing.T) {
	byKind := map[string]Adapter{}
	for _, a := range Adapters() {
		byKind[a.Kind] = a
	}
	for _, f := range adapterFixtures() {
		a, ok := byKind[f.kind]
		if !ok {
			t.Fatalf("fixture for unknown kind %q", f.kind)
		}
		t.Run(f.kind, func(t *testing.T) {
			want := StateHealthy
			if a.Rule == nil {
				want = StateUnknown // counted, never judged
			}
			if got := stateFor(t, a, f.healthy); got != want {
				t.Errorf("healthy fixture: state = %q, want %q", got, want)
			}
			want = StateUnhealthy
			if a.Rule == nil {
				want = StateUnknown
			}
			if got := stateFor(t, a, f.unhealthy); got != want {
				t.Errorf("unhealthy fixture: state = %q, want %q", got, want)
			}
			if got := stateFor(t, a, f.missing); got != StateUnknown {
				t.Errorf("missing-field fixture: state = %q, want %q", got, StateUnknown)
			}
		})
	}
}

func TestAdapters_LonghornDetachedVolumeIsUnknownNotUnhealthy(t *testing.T) {
	// A detached volume reports robustness "unknown". An idle PVC is not an
	// incident, which is exactly why unknown must be a non-problem state.
	var vol Adapter
	for _, a := range Adapters() {
		if a.Kind == "Volume" {
			vol = a
		}
	}
	got := stateFor(t, vol, map[string]any{"robustness": "unknown", "state": "detached"})
	if got != StateUnknown {
		t.Errorf("state = %q, want %q", got, StateUnknown)
	}
}

func TestAdapters_ArgoDegradedIsUnhealthyButOutOfSyncIsNot(t *testing.T) {
	// Sync status is deliberately not read: OutOfSync is drift, the next slice,
	// and flagging it would make every pending deploy look like a failure.
	var app Adapter
	for _, a := range Adapters() {
		if a.Kind == "Application" {
			app = a
		}
	}
	healthyButDrifted := map[string]any{
		"health": map[string]any{"status": "Healthy"},
		"sync":   map[string]any{"status": "OutOfSync"},
	}
	if got := stateFor(t, app, healthyButDrifted); got != StateHealthy {
		t.Errorf("OutOfSync but Healthy: state = %q, want %q", got, StateHealthy)
	}
	suspended := map[string]any{"health": map[string]any{"status": "Suspended"}}
	if got := stateFor(t, app, suspended); got != StateSuspended {
		t.Errorf("Suspended: state = %q, want %q", got, StateSuspended)
	}
}

func TestAdapters_EveryRowIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range Adapters() {
		if a.Operator == "" || a.Group == "" || a.Version == "" || a.Resource == "" || a.Kind == "" {
			t.Errorf("incomplete adapter row: %+v", a)
		}
		key := a.Group + "/" + a.Version + "/" + a.Resource
		if seen[key] {
			t.Errorf("duplicate adapter row for %s", key)
		}
		seen[key] = true
		if a.Resource != lower(a.Resource) {
			t.Errorf("resource %q must be the lowercase plural discovery reports", a.Resource)
		}
	}
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/operators/ -run TestAdapters -v
```

Expected: FAIL to build — `undefined: Adapters`.

- [ ] **Step 3: Write the adapter table**

Create `internal/operators/adapters.go`:

```go
package operators

// Adapters returns the operator resources kubeagent knows how to read, in the
// order the report prints them. Adding an operator is one row here plus its
// fixture in adapters_test.go — a row without a fixture is incomplete work,
// because a wrong field path and an absent field are indistinguishable to any
// test that derives its fixture from the path.
//
// Field paths and values are the ones each project documents. Anything not
// listed maps to unknown, which is counted and never flagged.
func Adapters() []Adapter {
	return []Adapter{
		{
			Operator: "cert-manager", Group: "cert-manager.io", Version: "v1",
			Resource: "certificates", Kind: "Certificate",
			Rule: ConditionRule{Type: "Ready"},
		},
		{
			// The namespaced Issuer, not just ClusterIssuer: a broken Issuer
			// breaks every Certificate in its namespace, and it is the more
			// common shape in application namespaces.
			Operator: "cert-manager", Group: "cert-manager.io", Version: "v1",
			Resource: "issuers", Kind: "Issuer",
			Rule: ConditionRule{Type: "Ready"},
		},
		{
			Operator: "cert-manager", Group: "cert-manager.io", Version: "v1",
			Resource: "clusterissuers", Kind: "ClusterIssuer",
			Rule: ConditionRule{Type: "Ready"},
		},
		{
			Operator: "CloudNativePG", Group: "postgresql.cnpg.io", Version: "v1",
			Resource: "clusters", Kind: "Cluster",
			Rule: ConditionRule{Type: "Ready"},
		},
		{
			// A detached volume reports robustness "unknown" — left out of every
			// set on purpose, so an idle PVC is not an incident.
			Operator: "Longhorn", Group: "longhorn.io", Version: "v1beta2",
			Resource: "volumes", Kind: "Volume",
			Rule: FieldRule{
				Path:      []string{"status", "robustness"},
				Healthy:   []string{"healthy"},
				Unhealthy: []string{"degraded", "faulted"},
			},
		},
		{
			// status.sync.status is deliberately not read: OutOfSync is drift,
			// the next Theme F slice, and flagging it here would make every
			// pending deploy look like a failure.
			Operator: "Argo CD", Group: "argoproj.io", Version: "v1alpha1",
			Resource: "applications", Kind: "Application",
			Rule: FieldRule{
				Path:        []string{"status", "health", "status"},
				Healthy:     []string{"Healthy"},
				Progressing: []string{"Progressing"},
				Unhealthy:   []string{"Degraded", "Missing"},
				Suspended:   []string{"Suspended"},
			},
		},
		{
			Operator: "Flux", Group: "kustomize.toolkit.fluxcd.io", Version: "v1",
			Resource: "kustomizations", Kind: "Kustomization",
			SuspendPath: []string{"spec", "suspend"},
			Rule:        ConditionRule{Type: "Ready"},
		},
		{
			Operator: "Flux", Group: "helm.toolkit.fluxcd.io", Version: "v2",
			Resource: "helmreleases", Kind: "HelmRelease",
			SuspendPath: []string{"spec", "suspend"},
			Rule:        ConditionRule{Type: "Ready"},
		},
		{
			// The Available condition exists in prometheus-operator >= 0.68. On
			// older versions it is absent, so the rule yields unknown and the
			// resource is counted, not flagged — the correct degradation.
			Operator: "Prometheus operator", Group: "monitoring.coreos.com", Version: "v1",
			Resource: "prometheuses", Kind: "Prometheus",
			Rule: ConditionRule{Type: "Available"},
		},
		{
			// ServiceMonitor has no .status at all. It is counted so the report
			// can say the operator is installed and how much it is scraping;
			// judging it would mean inventing a health signal that does not exist.
			Operator: "Prometheus operator", Group: "monitoring.coreos.com", Version: "v1",
			Resource: "servicemonitors", Kind: "ServiceMonitor",
		},
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/operators/ -v && go vet ./internal/operators/
```

Expected: PASS, including every `TestAdapters_FixturesPinEveryRow/<Kind>` subtest.

- [ ] **Step 5: Commit**

```bash
git add internal/operators/adapters.go internal/operators/adapters_test.go
git commit -m "feat(operators): the ten-row adapter table with per-row fixtures

Six operators, ten resources: cert-manager (Certificate, Issuer,
ClusterIssuer), CloudNativePG, Longhorn, Argo CD, Flux (Kustomization,
HelmRelease), and the Prometheus operator (Prometheus, ServiceMonitor).

Each row is pinned by hand-written fixtures taken from the project's own
documented CR shape, never derived from the adapter's field path — a derived
fixture cannot tell a wrong path from an absent field. A completeness test
fails the suite when a row is added without one."
```

---

### Task 4: `cluster.NewDynamicClients`

**Files:**
- Modify: `internal/cluster/client.go`
- Test: `internal/cluster/client_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func NewDynamicClients(kubeconfigPath, contextName string) (dynamic.Interface, discovery.DiscoveryInterface, error)`. `NewClient` and `NewInClusterOrKubeconfig` keep their exact current signatures.

- [ ] **Step 1: Write the failing test**

Append to `internal/cluster/client_test.go`:

```go
func TestNewDynamicClients_BuildsBothFromAKubeconfig(t *testing.T) {
	// Client construction contacts no API server: this passes with nothing running.
	path := twoContextKubeconfig(t)
	dyn, disco, err := NewDynamicClients(path, "beta")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dyn == nil {
		t.Error("dynamic client is nil")
	}
	if disco == nil {
		t.Error("discovery client is nil")
	}
}

func TestNewDynamicClients_UnknownContextIsAnError(t *testing.T) {
	path := twoContextKubeconfig(t)
	if _, _, err := NewDynamicClients(path, "does-not-exist"); err == nil {
		t.Fatal("expected an error for an unknown context, got nil")
	}
}

func TestNewDynamicClients_BadPathReturnsError(t *testing.T) {
	if _, _, err := NewDynamicClients("/nonexistent/kubeconfig", ""); err == nil {
		t.Fatal("expected an error for a missing kubeconfig, got nil")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cluster/ -run TestNewDynamicClients -v
```

Expected: FAIL to build — `undefined: NewDynamicClients`.

- [ ] **Step 3: Extract `restConfig` and add `NewDynamicClients`**

Replace the body of `NewClient` in `internal/cluster/client.go` and add the two new functions. The final file's relevant parts:

```go
package cluster

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient builds a Kubernetes clientset from a kubeconfig file.
// If kubeconfigPath is empty, it falls back to $KUBECONFIG, then ~/.kube/config.
// If contextName is empty, the kubeconfig's current-context is used.
func NewClient(kubeconfigPath, contextName string) (*kubernetes.Clientset, error) {
	config, err := restConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return clientset, nil
}

// NewDynamicClients builds the dynamic and discovery clients for the same
// kubeconfig/context NewClient would use — the pair `scan --operators` needs to
// read custom resources it was not compiled against. Contacts no API server.
func NewDynamicClients(kubeconfigPath, contextName string) (dynamic.Interface, discovery.DiscoveryInterface, error) {
	config, err := restConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("creating dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("creating discovery client: %w", err)
	}
	return dyn, disco, nil
}

// restConfig resolves the kubeconfig path and context into a REST config. It is
// the single place that resolution lives, so every client kubeagent builds
// honours --kubeconfig and --context identically.
func restConfig(kubeconfigPath, contextName string) (*rest.Config, error) {
	path, err := resolveKubeconfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = path
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		if contextName == "" {
			return nil, fmt.Errorf("loading kubeconfig %q: %w", path, err)
		}
		return nil, fmt.Errorf("loading kubeconfig %q (context %q): %w", path, contextName, err)
	}
	return config, nil
}
```

`NewInClusterOrKubeconfig` is unchanged.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cluster/ -v && go build ./...
```

Expected: PASS, including the pre-existing `TestNewClient_*` and `TestResolveKubeconfig_*` — the extraction must not change their behaviour.

- [ ] **Step 5: Commit**

```bash
git add internal/cluster/client.go internal/cluster/client_test.go
git commit -m "feat(cluster): NewDynamicClients for CRD access

Extracts the kubeconfig/context resolution NewClient had inlined into an
unexported restConfig, so every client kubeagent builds honours --kubeconfig
and --context identically, and adds the dynamic + discovery pair that
scan --operators needs. NewClient and NewInClusterOrKubeconfig keep their
exact signatures; no existing caller changes."
```

---

### Task 5: `collect.OperatorResources` — the discovery gate

**Files:**
- Create: `internal/collect/operators.go`
- Test: `internal/collect/operators_test.go`

**Interfaces:**
- Consumes: `operators.Adapter`, `operators.Fetched` (Task 1); `operators.Adapters()` (Task 3).
- Produces: `func OperatorResources(ctx context.Context, disco discovery.DiscoveryInterface, dyn dynamic.Interface, adapters []operators.Adapter, namespace string) []operators.Fetched`.

- [ ] **Step 1: Write the failing test**

Create `internal/collect/operators_test.go`:

```go
package collect

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/operators"
)

var certGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
var clusterIssuerGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}

var certAdapter = operators.Adapter{
	Operator: "cert-manager", Group: "cert-manager.io", Version: "v1",
	Resource: "certificates", Kind: "Certificate", Rule: operators.ConditionRule{Type: "Ready"},
}
var clusterIssuerAdapter = operators.Adapter{
	Operator: "cert-manager", Group: "cert-manager.io", Version: "v1",
	Resource: "clusterissuers", Kind: "ClusterIssuer", Rule: operators.ConditionRule{Type: "Ready"},
}

// discoveryFor builds a fake discovery serving the given resource lists.
func discoveryFor(lists ...*metav1.APIResourceList) *discoveryfake.FakeDiscovery {
	return &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: lists}}
}

// certManagerV1 is the resource list a cluster with cert-manager installed serves.
func certManagerV1() *metav1.APIResourceList {
	return &metav1.APIResourceList{
		GroupVersion: "cert-manager.io/v1",
		APIResources: []metav1.APIResource{
			{Name: "certificates", Kind: "Certificate", Namespaced: true},
			{Name: "clusterissuers", Kind: "ClusterIssuer", Namespaced: false},
		},
	}
}

// dynamicFor builds a fake dynamic client that knows the cert-manager list kinds.
func dynamicFor(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			certGVR:           "CertificateList",
			clusterIssuerGVR:  "ClusterIssuerList",
			{Group: "longhorn.io", Version: "v1beta1", Resource: "volumes"}: "VolumeList",
		},
		objs...,
	)
}

// certCR builds a cert-manager Certificate object.
func certCR(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"namespace": namespace, "name": name},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True"},
		}},
	}}
}

func TestOperatorResources_AbsentGroupCostsZeroDynamicCalls(t *testing.T) {
	// Discovery is the installation signal. An operator whose group the API
	// server does not serve is skipped entirely: no call, no error, no entry.
	disco := discoveryFor() // nothing installed
	dyn := dynamicFor()

	got := OperatorResources(context.Background(), disco, dyn, operators.Adapters(), "")
	if len(got) != 0 {
		t.Errorf("got %d fetched results, want 0 for a cluster with no operators", len(got))
	}
	if n := len(dyn.Actions()); n != 0 {
		t.Errorf("dynamic client made %d calls, want 0: %v", n, dyn.Actions())
	}
}

func TestOperatorResources_ListsAServedResource(t *testing.T) {
	disco := discoveryFor(certManagerV1())
	dyn := dynamicFor(certCR("shop", "web-tls"), certCR("infra", "api-tls"))

	got := OperatorResources(context.Background(), disco, dyn, []operators.Adapter{certAdapter}, "")
	if len(got) != 1 {
		t.Fatalf("got %d fetched results, want 1", len(got))
	}
	if got[0].APIVersion != "cert-manager.io/v1" {
		t.Errorf("apiVersion = %q, want cert-manager.io/v1", got[0].APIVersion)
	}
	if len(got[0].Items) != 2 {
		t.Errorf("listed %d items, want 2", len(got[0].Items))
	}
	if got[0].Err != nil || got[0].Forbidden {
		t.Errorf("unexpected failure: err=%v forbidden=%v", got[0].Err, got[0].Forbidden)
	}
}

func TestOperatorResources_UninstalledCRDInAServedGroupIsSkipped(t *testing.T) {
	// The group is served but this CRD is not installed — a real shape with
	// Flux, where source-controller can be present without kustomize-controller.
	partial := &metav1.APIResourceList{
		GroupVersion: "cert-manager.io/v1",
		APIResources: []metav1.APIResource{{Name: "clusterissuers", Kind: "ClusterIssuer", Namespaced: false}},
	}
	disco := discoveryFor(partial)
	dyn := dynamicFor()

	got := OperatorResources(context.Background(), disco, dyn, []operators.Adapter{certAdapter}, "")
	if len(got) != 0 {
		t.Errorf("got %d fetched results, want 0 (certificates is not served)", len(got))
	}
}

func TestOperatorResources_NamespaceScopingHonoursTheDiscoveredScope(t *testing.T) {
	// Namespaced resources honour -n; cluster-scoped ones are always listed
	// cluster-wide, the way Nodes already ignores the namespace filter.
	disco := discoveryFor(certManagerV1())
	dyn := dynamicFor(certCR("shop", "web-tls"), certCR("infra", "api-tls"))

	got := OperatorResources(context.Background(), disco, dyn,
		[]operators.Adapter{certAdapter, clusterIssuerAdapter}, "shop")
	if len(got) != 2 {
		t.Fatalf("got %d fetched results, want 2", len(got))
	}
	if len(got[0].Items) != 1 {
		t.Errorf("namespaced list returned %d items, want 1 (scoped to shop)", len(got[0].Items))
	}

	var sawNamespacedList, sawClusterList bool
	for _, a := range dyn.Actions() {
		la, ok := a.(clienttesting.ListAction)
		if !ok {
			continue
		}
		switch la.GetResource().Resource {
		case "certificates":
			sawNamespacedList = true
			if la.GetNamespace() != "shop" {
				t.Errorf("certificates listed in namespace %q, want shop", la.GetNamespace())
			}
		case "clusterissuers":
			sawClusterList = true
			if la.GetNamespace() != "" {
				t.Errorf("clusterissuers listed in namespace %q, want cluster-wide", la.GetNamespace())
			}
		}
	}
	if !sawNamespacedList || !sawClusterList {
		t.Errorf("missing list calls: namespaced=%v cluster=%v", sawNamespacedList, sawClusterList)
	}
}

func TestOperatorResources_ForbiddenIsIsolatedToItsOwnAdapter(t *testing.T) {
	disco := discoveryFor(certManagerV1())
	dyn := dynamicFor(certCR("shop", "web-tls"))
	dyn.PrependReactor("list", "certificates", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "cert-manager.io", Resource: "certificates"}, "", errors.New("denied"))
	})

	got := OperatorResources(context.Background(), disco, dyn,
		[]operators.Adapter{certAdapter, clusterIssuerAdapter}, "")
	if len(got) != 2 {
		t.Fatalf("got %d fetched results, want 2 — one denial must not stop the rest", len(got))
	}
	if !got[0].Forbidden {
		t.Error("certificates: Forbidden = false, want true")
	}
	if got[0].Err != nil {
		t.Errorf("certificates: Err = %v, want nil (a denial is not an error to report)", got[0].Err)
	}
	if got[1].Forbidden || got[1].Err != nil {
		t.Errorf("clusterissuers was affected by the other adapter's denial: %+v", got[1])
	}
}

func TestOperatorResources_OtherListErrorIsRecordedAgainstThatAdapterOnly(t *testing.T) {
	disco := discoveryFor(certManagerV1())
	dyn := dynamicFor()
	dyn.PrependReactor("list", "certificates", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcdserver: request timed out")
	})

	got := OperatorResources(context.Background(), disco, dyn,
		[]operators.Adapter{certAdapter, clusterIssuerAdapter}, "")
	if len(got) != 2 {
		t.Fatalf("got %d fetched results, want 2", len(got))
	}
	if got[0].Err == nil {
		t.Error("certificates: Err = nil, want the list failure recorded")
	}
	if got[1].Err != nil {
		t.Errorf("clusterissuers: Err = %v, want nil", got[1].Err)
	}
}

func TestOperatorResources_FallsBackToTheServedVersion(t *testing.T) {
	// The adapter prefers longhorn.io/v1beta2; this cluster serves v1beta1. The
	// served version is used and recorded — a field path that does not exist
	// there yields unknown, never unhealthy.
	longhorn := operators.Adapter{
		Operator: "Longhorn", Group: "longhorn.io", Version: "v1beta2",
		Resource: "volumes", Kind: "Volume",
		Rule: operators.FieldRule{Path: []string{"status", "robustness"}, Healthy: []string{"healthy"}},
	}
	disco := discoveryFor(&metav1.APIResourceList{
		GroupVersion: "longhorn.io/v1beta1",
		APIResources: []metav1.APIResource{{Name: "volumes", Kind: "Volume", Namespaced: true}},
	})
	dyn := dynamicFor()

	got := OperatorResources(context.Background(), disco, dyn, []operators.Adapter{longhorn}, "")
	if len(got) != 1 {
		t.Fatalf("got %d fetched results, want 1", len(got))
	}
	if got[0].APIVersion != "longhorn.io/v1beta1" {
		t.Errorf("apiVersion = %q, want the version actually served", got[0].APIVersion)
	}
}

func TestOperatorResources_DiscoveryFailureYieldsNothing(t *testing.T) {
	// Discovery is available to every authenticated user, so a failure here
	// means the API server is unreachable — which the base scan already reports.
	disco := discoveryFor()
	disco.Fake.PrependReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	dyn := dynamicFor()
	// ServerGroups on the fake derives from Fake.Resources; with none set it
	// returns an empty group list, which is the same outcome: nothing fetched.
	if got := OperatorResources(context.Background(), disco, dyn, operators.Adapters(), ""); len(got) != 0 {
		t.Errorf("got %d fetched results, want 0", len(got))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -run TestOperatorResources -v
```

Expected: FAIL to build — `undefined: OperatorResources`.

- [ ] **Step 3: Implement the gate**

Create `internal/collect/operators.go`:

```go
package collect

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/imantaba/kubeagent/internal/operators"
)

// OperatorResources gates each adapter on API discovery, then lists the ones the
// cluster actually serves. Read-only: List calls only — never get, watch, or write.
//
// Discovery is the installation signal. An operator counts as installed when the
// API server serves its group, not because a Deployment is named after it, so an
// adapter whose group is absent is skipped with zero API calls, no error, and no
// report entry. A cluster running none of the six costs one discovery round trip.
//
// Discovery itself needs no RBAC grant: the default system:discovery ClusterRole
// is bound to system:authenticated on every conformant cluster. Listing the
// custom resources does — see deploy/rbac-operators.yaml. Without that grant the
// kind is marked Forbidden and the scan continues, which still answers "which
// operators are installed".
func OperatorResources(ctx context.Context, disco discovery.DiscoveryInterface,
	dyn dynamic.Interface, adapters []operators.Adapter, namespace string) []operators.Fetched {

	groups, err := disco.ServerGroups()
	if err != nil {
		// Discovery is open to every authenticated user, so a failure here means
		// the API server is unreachable — already the base scan's headline.
		return nil
	}
	served := map[string][]string{} // group → versions, the preferred one first
	for _, g := range groups.Groups {
		var vs []string
		if g.PreferredVersion.Version != "" {
			vs = append(vs, g.PreferredVersion.Version)
		}
		for _, v := range g.Versions {
			if v.Version != "" && !containsString(vs, v.Version) {
				vs = append(vs, v.Version)
			}
		}
		if len(vs) > 0 {
			served[g.Name] = vs
		}
	}

	// resourceScope caches one ServerResourcesForGroupVersion call per
	// group/version, so cert-manager's three adapters cost one round trip.
	resourceScope := map[string]map[string]bool{} // "group/version" → plural → namespaced
	var out []operators.Fetched

	for _, a := range adapters {
		versions, ok := served[a.Group]
		if !ok {
			continue // the group is not served: this operator is not installed
		}
		// Version tolerance: prefer the version the adapter names, else take the
		// group's preferred one. A field path missing from an unfamiliar version
		// yields unknown, which is the designed degradation.
		version := versions[0]
		if containsString(versions, a.Version) {
			version = a.Version
		}
		gv := a.Group + "/" + version
		if _, cached := resourceScope[gv]; !cached {
			scope := map[string]bool{}
			if list, err := disco.ServerResourcesForGroupVersion(gv); err == nil {
				for _, r := range list.APIResources {
					scope[r.Name] = r.Namespaced
				}
			}
			resourceScope[gv] = scope
		}
		namespaced, serves := resourceScope[gv][a.Resource]
		if !serves {
			continue // the group is served but this CRD is not installed
		}

		gvr := schema.GroupVersionResource{Group: a.Group, Version: version, Resource: a.Resource}
		var ri dynamic.ResourceInterface = dyn.Resource(gvr)
		if namespaced && namespace != "" {
			ri = dyn.Resource(gvr).Namespace(namespace)
		}

		f := operators.Fetched{Adapter: a, APIVersion: gv}
		list, err := ri.List(ctx, metav1.ListOptions{})
		switch {
		case apierrors.IsForbidden(err):
			// One denial marks its own kind and nothing else: a missing grant on
			// one CRD must never fail the scan or another operator.
			f.Forbidden = true
		case err != nil:
			f.Err = err
		default:
			f.Items = list.Items
		}
		out = append(out, f)
	}
	return out
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -v && go vet ./internal/collect/
```

Expected: PASS, all pre-existing `collect` tests included.

If `discoveryfake.FakeDiscovery.ServerGroups()` does not populate `PreferredVersion` the way a real API server does, adapt the *test's* expectations, not the production code — `OperatorResources` already tolerates an empty `PreferredVersion` by falling through to `g.Versions`.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/operators.go internal/collect/operators_test.go
git commit -m "feat(collect): OperatorResources gates CRD listing on discovery

Discovery is the installation signal: an operator counts as installed when
the API server serves its group, so an absent group is skipped with zero API
calls, no error, and no report entry — a test asserts the dynamic client made
none. Listing stays read-only.

Failures are isolated per adapter: a forbidden list marks its own kind and
nothing else, so a missing grant on one CRD never fails the scan or another
operator. When the adapter's preferred version is not served, the group's
preferred version is used and recorded."
```

---

### Task 6: The report section

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`, `internal/report/golden_test.go`, `internal/report/testdata/golden-scan.txt`

**Interfaces:**
- Consumes: `operators.Report`, `operators.OperatorReport`, `operators.KindReport`, `operators.Resource`, `operators.State*`, `KindReport.Total()` (Tasks 1-2).
- Produces: `report.Input.Operators *operators.Report`, rendered as the `OPERATORS` text section and the top-level `operators` JSON object.

**Format contract:**

```text
OPERATORS  (advisory — operator-reported state; no CR spec content)
  cert-manager (cert-manager.io/v1)
    Certificate     12 healthy, 1 unhealthy
      ✗ shop/web-tls  Ready=False: IssuerNotFound
    Issuer          3 healthy
  Prometheus operator (monitoring.coreos.com/v1)
    ServiceMonitor  214 (not assessed)
```

- Operator line: `  %s (%s)` — name, `APIVersions` joined with `", "`. Omit the parenthesis when `APIVersions` is empty.
- Kind line: `    %-16s%s`. The summary lists non-zero counts in the fixed order healthy, progressing, unhealthy, suspended, unknown as `"<n> <state>"`, joined with `", "`. An unjudged kind (`Judged == false`) renders `"<total> (not assessed)"` instead.
- Unhealthy line: `      ✗ %s  %s` — `namespace/name` (bare `name` when the namespace is empty), then the reason. Omit the trailing spaces and reason when the reason is empty.
- Truncation line: `      … +%d more unhealthy`.
- `Forbidden`: `    %-16slist forbidden — apply deploy/rbac-operators.yaml`.
- `Error`: `    %-16slist failed: %s`.
- The section prints when `Operators` is non-nil and has at least one operator.
- **`OPERATORS` is deliberately NOT added to the "No issues found. ✅" suppression condition.** Unlike `CERTIFICATES`, this section renders on a perfectly healthy cluster, so counting it as "attention" would silence the all-clear line for every `--operators` run.

- [ ] **Step 1: Write the failing test**

Append to `internal/report/report_test.go`:

```go
func operatorsFixture() *operators.Report {
	return &operators.Report{Operators: []operators.OperatorReport{
		{
			Operator: "cert-manager", APIVersions: []string{"cert-manager.io/v1"},
			Kinds: []operators.KindReport{
				{
					Kind: "Certificate", APIVersion: "cert-manager.io/v1", Judged: true,
					Counts: map[operators.State]int{operators.StateHealthy: 12, operators.StateUnhealthy: 1},
					Unhealthy: []operators.Resource{{
						Operator: "cert-manager", Kind: "Certificate", Namespace: "shop",
						Name: "web-tls", State: operators.StateUnhealthy, Reason: "Ready=False: IssuerNotFound",
					}},
				},
				{
					Kind: "ClusterIssuer", APIVersion: "cert-manager.io/v1", Judged: true,
					Counts: map[operators.State]int{operators.StateHealthy: 1},
				},
			},
		},
		{
			Operator: "Prometheus operator", APIVersions: []string{"monitoring.coreos.com/v1"},
			Kinds: []operators.KindReport{{
				Kind: "ServiceMonitor", APIVersion: "monitoring.coreos.com/v1", Judged: false,
				Counts: map[operators.State]int{operators.StateUnknown: 214},
			}},
		},
	}}
}

func TestPrintInventory_OperatorsSection(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:   clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		Operators: operatorsFixture(),
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"OPERATORS",
		"cert-manager (cert-manager.io/v1)",
		"Certificate     12 healthy, 1 unhealthy",
		"✗ shop/web-tls  Ready=False: IssuerNotFound",
		"ClusterIssuer   1 healthy",
		"ServiceMonitor  214 (not assessed)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPrintInventory_OperatorsSectionOmittedWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "OPERATORS") {
		t.Errorf("printed the operators section with no report:\n%s", buf.String())
	}

	buf.Reset()
	in.Operators = &operators.Report{}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "OPERATORS") {
		t.Errorf("printed the operators section with no operators installed:\n%s", buf.String())
	}
}

func TestPrintInventory_HealthyOperatorsStillPrintTheAllClear(t *testing.T) {
	// The section renders on a healthy cluster, so it must not suppress the
	// all-clear line the way a problem-only section like CERTIFICATES does.
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		Operators: &operators.Report{Operators: []operators.OperatorReport{{
			Operator: "Flux", APIVersions: []string{"kustomize.toolkit.fluxcd.io/v1"},
			Kinds: []operators.KindReport{{
				Kind: "Kustomization", APIVersion: "kustomize.toolkit.fluxcd.io/v1", Judged: true,
				Counts: map[operators.State]int{operators.StateHealthy: 9, operators.StateSuspended: 1},
			}},
		}}},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Kustomization   9 healthy, 1 suspended") {
		t.Errorf("output missing the kind summary:\n%s", out)
	}
	if !strings.Contains(out, "No issues found") {
		t.Errorf("healthy operators suppressed the all-clear line:\n%s", out)
	}
}

func TestPrintInventory_OperatorsForbiddenAndTruncation(t *testing.T) {
	var unhealthy []operators.Resource
	for i := 0; i < operators.MaxUnhealthyPerKind; i++ {
		unhealthy = append(unhealthy, operators.Resource{
			Kind: "Application", Namespace: "argocd", Name: fmt.Sprintf("app-%02d", i),
			State: operators.StateUnhealthy, Reason: "health.status=Degraded",
		})
	}
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		Operators: &operators.Report{Operators: []operators.OperatorReport{
			{
				Operator: "Argo CD", APIVersions: []string{"argoproj.io/v1alpha1"},
				Kinds: []operators.KindReport{{
					Kind: "Application", APIVersion: "argoproj.io/v1alpha1", Judged: true,
					Counts:    map[operators.State]int{operators.StateUnhealthy: 27},
					Unhealthy: unhealthy, Truncated: 7,
				}},
			},
			{
				Operator: "Longhorn", APIVersions: []string{"longhorn.io/v1beta2"},
				Kinds: []operators.KindReport{{
					Kind: "Volume", APIVersion: "longhorn.io/v1beta2", Judged: true,
					Counts: map[operators.State]int{}, Forbidden: true,
				}},
			},
		}},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "… +7 more unhealthy") {
		t.Errorf("truncation not reported:\n%s", out)
	}
	if !strings.Contains(out, "list forbidden — apply deploy/rbac-operators.yaml") {
		t.Errorf("forbidden kind not reported:\n%s", out)
	}
}

func TestPrintInventory_OperatorsJSON(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:   clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		Operators: operatorsFixture(),
	}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		Operators *operators.Report `json:"operators"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Operators == nil || len(got.Operators.Operators) != 2 {
		t.Fatalf("operators = %+v, want the two-operator report", got.Operators)
	}
	if got.Operators.Operators[0].Kinds[0].Counts[operators.StateHealthy] != 12 {
		t.Errorf("counts did not round-trip: %+v", got.Operators.Operators[0].Kinds[0].Counts)
	}
}
```

Add `"encoding/json"`, `"fmt"`, and `"github.com/imantaba/kubeagent/internal/operators"` to `report_test.go`'s imports if they are not already there.

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ -run TestPrintInventory_Operators -v
```

Expected: FAIL to build — `unknown field Operators in struct literal of type Input`.

- [ ] **Step 3: Wire the field and write the renderer**

In `internal/report/report.go`:

1. Add `"github.com/imantaba/kubeagent/internal/operators"` to the imports.
2. Add to `inventoryReport`, after `Certificates`:

```go
	Operators          *operators.Report           `json:"operators,omitempty"`
```

3. Add to `Input`, after `Certificates`:

```go
	Operators              *operators.Report
```

4. Add to the `inventoryReport` literal in `PrintInventory`, after `Certificates: in.Certificates,`:

```go
			Operators:          in.Operators,
```

5. In `printInventoryText`, after the `printCertificates` block and before `printNotes`:

```go
	if err := printOperators(in.Operators, w); err != nil {
		return err
	}
```

Do **not** add an `hasOperators` term to the `No issues found. ✅` condition: this section renders on a healthy cluster, and suppressing the all-clear for every `--operators` run would be wrong.

6. Add the renderer next to `printCertificates`:

```go
// operatorsRender reports whether the OPERATORS section would print anything.
func operatorsRender(rep *operators.Report) bool {
	return rep != nil && len(rep.Operators) > 0
}

// printOperators renders the advisory OPERATORS section (opt-in --operators):
// one line per operator, one per resource kind, and the unhealthy resources a
// kind enumerates. Metadata and state only — no CR spec content ever reaches it.
func printOperators(rep *operators.Report, w io.Writer) error {
	if !operatorsRender(rep) {
		return nil
	}
	if _, err := fmt.Fprintln(w, "OPERATORS  (advisory — operator-reported state; no CR spec content)"); err != nil {
		return err
	}
	for _, op := range rep.Operators {
		line := "  " + op.Operator
		if len(op.APIVersions) > 0 {
			line += " (" + strings.Join(op.APIVersions, ", ") + ")"
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		for _, k := range op.Kinds {
			switch {
			case k.Forbidden:
				if _, err := fmt.Fprintf(w, "    %-16slist forbidden — apply deploy/rbac-operators.yaml\n", k.Kind); err != nil {
					return err
				}
				continue
			case k.Error != "":
				if _, err := fmt.Fprintf(w, "    %-16slist failed: %s\n", k.Kind, k.Error); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(w, "    %-16s%s\n", k.Kind, kindSummary(k)); err != nil {
				return err
			}
			for _, r := range k.Unhealthy {
				name := r.Name
				if r.Namespace != "" {
					name = r.Namespace + "/" + r.Name
				}
				line := "      ✗ " + name
				if r.Reason != "" {
					line += "  " + r.Reason
				}
				if _, err := fmt.Fprintln(w, line); err != nil {
					return err
				}
			}
			if k.Truncated > 0 {
				if _, err := fmt.Fprintf(w, "      … +%d more unhealthy\n", k.Truncated); err != nil {
					return err
				}
			}
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// kindSummary renders one kind's counts in a fixed state order, omitting zeros.
// An adapter with no rule counts its resources without judging them, so its
// summary says so rather than implying an assessment happened.
func kindSummary(k operators.KindReport) string {
	if !k.Judged {
		return fmt.Sprintf("%d (not assessed)", k.Total())
	}
	order := []operators.State{
		operators.StateHealthy, operators.StateProgressing,
		operators.StateUnhealthy, operators.StateSuspended, operators.StateUnknown,
	}
	var parts []string
	for _, s := range order {
		if n := k.Counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, ", ")
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ -run TestPrintInventory_Operators -v
```

Expected: PASS.

- [ ] **Step 5: Add the section to the golden snapshot and regenerate**

In `internal/report/golden_test.go`, add `"github.com/imantaba/kubeagent/internal/operators"` to the imports and add this field to the `Input` literal in `goldenInput`, after `Certificates`:

```go
		Operators: &operators.Report{Operators: []operators.OperatorReport{
			{
				Operator: "cert-manager", APIVersions: []string{"cert-manager.io/v1"},
				Kinds: []operators.KindReport{
					{Kind: "Certificate", APIVersion: "cert-manager.io/v1", Judged: true,
						Counts: map[operators.State]int{operators.StateHealthy: 12, operators.StateUnhealthy: 1},
						Unhealthy: []operators.Resource{{Operator: "cert-manager", Kind: "Certificate",
							Namespace: "shop", Name: "web-tls", State: operators.StateUnhealthy,
							Reason: "Ready=False: IssuerNotFound"}}},
					{Kind: "ClusterIssuer", APIVersion: "cert-manager.io/v1", Judged: true,
						Counts: map[operators.State]int{operators.StateHealthy: 1}},
				},
			},
			{
				Operator: "Flux", APIVersions: []string{"kustomize.toolkit.fluxcd.io/v1", "helm.toolkit.fluxcd.io/v2"},
				Kinds: []operators.KindReport{
					{Kind: "Kustomization", APIVersion: "kustomize.toolkit.fluxcd.io/v1", Judged: true,
						Counts: map[operators.State]int{operators.StateHealthy: 9, operators.StateSuspended: 1}},
					{Kind: "HelmRelease", APIVersion: "helm.toolkit.fluxcd.io/v2", Judged: true,
						Counts: map[operators.State]int{operators.StateHealthy: 4}},
				},
			},
			{
				Operator: "Prometheus operator", APIVersions: []string{"monitoring.coreos.com/v1"},
				Kinds: []operators.KindReport{
					{Kind: "Prometheus", APIVersion: "monitoring.coreos.com/v1", Judged: true,
						Counts: map[operators.State]int{operators.StateHealthy: 1}},
					{Kind: "ServiceMonitor", APIVersion: "monitoring.coreos.com/v1", Judged: false,
						Counts: map[operators.State]int{operators.StateUnknown: 214}},
				},
			},
		}},
```

Then regenerate and inspect:

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report -run TestGoldenScanOutput -update
git diff internal/report/testdata/golden-scan.txt
go test ./internal/report/ -v
```

Expected: the diff adds only the `OPERATORS` block; every other line is unchanged. Then PASS.

**Do not refresh the README demo GIF or `website/docs/quickstart.md`.** Both show a *default* scan, and `--operators` is off by default — the rendered default output does not change. The golden moves only because `goldenInput` deliberately exercises every section.

- [ ] **Step 6: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "feat(report): advisory OPERATORS section, text and JSON

One line per operator, one per resource kind, and only the unhealthy
resources a kind enumerates — progressing, suspended, and unknown are states,
not incidents. An adapter with no rule renders '(not assessed)' rather than
implying a judgement happened.

The section is deliberately excluded from the all-clear suppression: unlike
CERTIFICATES it renders on a healthy cluster, so counting it as attention
would silence 'No issues found' for every --operators run."
```

---

### Task 7: The `--operators` flag

**Files:**
- Modify: `main.go`
- Test: manual, plus `go build ./...` and the full suite (there is no test harness for `run()`'s flag wiring; `main_test.go` covers helpers only).

**Interfaces:**
- Consumes: `cluster.NewDynamicClients` (Task 4), `collect.OperatorResources` (Task 5), `operators.Adapters` / `operators.Assess` / `operators.Report` (Tasks 1-3), `report.Input.Operators` (Task 6).
- Produces: the user-facing `--operators` flag and `KUBEAGENT_OPERATORS` override.

- [ ] **Step 1: Add the flag and the usage entry**

In `main.go`, add the import `"github.com/imantaba/kubeagent/internal/operators"`.

After the `certWarnDays` flag declaration (currently `main.go:84`), add:

```go
	operatorsFlag := fs.Bool("operators", envBool("KUBEAGENT_OPERATORS", false), "report operator custom-resource health (cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, Prometheus operator; advisory, needs deploy/rbac-operators.yaml on a restricted context)")
```

In the usage string at `main.go:64`, insert `[--operators]` immediately after `[--certs [--cert-warn-days n]]`:

```
… [--certs [--cert-warn-days n]] [--operators] [--logs] …
```

- [ ] **Step 2: Wire the collection**

In `main.go`, after the `facts := platform.Detect(nodes, sysDS, scs, ics)` line (currently `main.go:183`), add:

```go
	// Operator custom resources: opt-in, advisory, and built lazily — a default
	// scan constructs no dynamic client and issues no discovery call.
	var operatorRep *operators.Report
	if *operatorsFlag {
		dyn, disco, derr := cluster.NewDynamicClients(*kubeconfig, *contextName)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "kubeagent: warning: --operators unavailable: %v\n", derr)
		} else {
			rep := operators.Assess(collect.OperatorResources(
				context.Background(), disco, dyn, operators.Adapters(), namespace))
			operatorRep = &rep
		}
	}
```

Then, in the presentation-layer block, after `in.DNS = dnsRep` (currently `main.go:248`), add:

```go
	in.Operators = operatorRep
```

- [ ] **Step 3: Build and verify the flag exists**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && go test ./...
go build -o kubeagent . && ./kubeagent scan --help 2>&1 | grep -A2 -- '-operators'
```

Expected: the suite passes and the help text shows the `-operators` flag with its description.

- [ ] **Step 4: Verify a default scan makes no discovery call**

Against any reachable cluster (or with none — the point is the code path):

```bash
export PATH=$PATH:/usr/local/go/bin
./kubeagent scan --output json 2>/dev/null | grep -c '"operators"'
```

Expected: `0` — a default scan emits no `operators` key at all.

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "feat(scan): --operators reports operator custom-resource health

Off by default, overridable with KUBEAGENT_OPERATORS. The dynamic and
discovery clients are constructed only when the flag is set, so a default
scan issues no discovery call and no dynamic call. A client-construction
failure warns on stderr and leaves the rest of the scan intact — an advisory
view must never be able to fail a scan."
```

---

### Task 8: RBAC add-on and documentation

**Files:**
- Create: `deploy/rbac-operators.yaml`
- Create: `website/docs/features/operators.md`
- Modify: `deploy/README.md`, `website/mkdocs.yml`, `website/docs/roadmap.md`, `CHANGELOG.md`

**Interfaces:**
- Consumes: the `--operators` flag (Task 7) and the ten adapter rows (Task 3).
- Produces: no code.

**No Helm values and no chart bump.** The chart deploys the `watch` daemon, and the daemon does not read operator CRDs in this slice; chart values for a flag the chart cannot set would be dead configuration. Do not touch `deploy/helm/`.

- [ ] **Step 1: Write the RBAC add-on**

Create `deploy/rbac-operators.yaml`:

```yaml
# Opt-in add-on: grants list access to the operator custom resources `scan
# --operators` reads. Apply alongside deploy/ for a restricted context; most
# human kubeconfigs already allow these. Without it, --operators still names
# which operators are installed (API discovery is open to every authenticated
# user) and marks each kind as forbidden — a useful answer, not an error.
#
# Scan-only: the watch daemon does not read operator CRDs, so this is not wired
# into the Helm chart. list only — kubeagent never writes to a CRD.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeagent-operators
rules:
  - apiGroups: [cert-manager.io]
    resources: [certificates, issuers, clusterissuers]
    verbs: [list]
  - apiGroups: [postgresql.cnpg.io]
    resources: [clusters]
    verbs: [list]
  - apiGroups: [longhorn.io]
    resources: [volumes]
    verbs: [list]
  - apiGroups: [argoproj.io]
    resources: [applications]
    verbs: [list]
  - apiGroups: [kustomize.toolkit.fluxcd.io]
    resources: [kustomizations]
    verbs: [list]
  - apiGroups: [helm.toolkit.fluxcd.io]
    resources: [helmreleases]
    verbs: [list]
  - apiGroups: [monitoring.coreos.com]
    resources: [prometheuses, servicemonitors]
    verbs: [list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeagent-operators
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeagent-operators
subjects:
  - kind: ServiceAccount
    name: kubeagent
    namespace: kubeagent
```

- [ ] **Step 2: Verify the manifest parses**

```bash
kubectl apply --dry-run=client -f deploy/rbac-operators.yaml
```

Expected: two `... created (dry run)` lines, no error. (If no cluster is reachable, `kubectl apply --dry-run=client` still validates locally.)

- [ ] **Step 3: Add the `deploy/README.md` section**

Insert after the "Crash log root-cause (opt-in)" section (currently ends around `deploy/README.md:118`), before "## Alerting (opt-in)":

```markdown
## Operator health (opt-in)

Applying `deploy/rbac-operators.yaml` grants `list` on the custom resources
`scan --operators` reads: cert-manager, CloudNativePG, Longhorn, Argo CD, Flux,
and the Prometheus operator. This is a scan-only add-on (not used by the watch
daemon, and not wired into the Helm chart); most human kubeconfigs already allow
these. Without it, `--operators` still names which operators are installed — API
discovery is open to every authenticated user — and marks each kind forbidden.

kubeagent only ever `list`s these resources, and the report carries metadata and
state alone: namespace, name, kind, state, and the operator's own condition
reason. No CR `spec` content is read into the report.
```

- [ ] **Step 4: Write the feature page**

Create `website/docs/features/operators.md`:

```markdown
# Operator health (`--operators`)

`kubeagent scan --operators` reports what the operators you actually run say
about themselves — cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, and the
Prometheus operator — without kubeagent being compiled against any of their Go
APIs, and without a rebuild when you install one.

```bash
kubeagent scan --operators
```

```text
OPERATORS  (advisory — operator-reported state; no CR spec content)
  cert-manager (cert-manager.io/v1)
    Certificate     12 healthy, 1 unhealthy
      ✗ shop/web-tls  Ready=False: IssuerNotFound
    Issuer          3 healthy
    ClusterIssuer   1 healthy
  Argo CD (argoproj.io/v1alpha1)
    Application     48 healthy, 2 progressing
  Flux (kustomize.toolkit.fluxcd.io/v1, helm.toolkit.fluxcd.io/v2)
    Kustomization   9 healthy, 1 suspended
    HelmRelease     4 healthy
  Prometheus operator (monitoring.coreos.com/v1)
    Prometheus      1 healthy
    ServiceMonitor  214 (not assessed)
```

## Discovery is the installation signal

An operator counts as installed when the API server *serves its API group* — not
because a Deployment happens to be named after it. If the group is absent,
kubeagent skips that adapter entirely: zero API calls, no error, and no line in
the report. A cluster running none of the six costs one discovery round trip.

That is also why nothing needs configuring. Install cert-manager tomorrow and
the next `--operators` scan picks it up.

## What is covered

| Operator | API group | Kinds | Signal |
| --- | --- | --- | --- |
| cert-manager | `cert-manager.io/v1` | `Certificate`, `Issuer`, `ClusterIssuer` | `Ready` condition |
| CloudNativePG | `postgresql.cnpg.io/v1` | `Cluster` | `Ready` condition |
| Longhorn | `longhorn.io/v1beta2` | `Volume` | `status.robustness` |
| Argo CD | `argoproj.io/v1alpha1` | `Application` | `status.health.status` |
| Flux | `kustomize.toolkit.fluxcd.io/v1`, `helm.toolkit.fluxcd.io/v2` | `Kustomization`, `HelmRelease` | `Ready` condition, `spec.suspend` |
| Prometheus operator | `monitoring.coreos.com/v1` | `Prometheus`, `ServiceMonitor` | `Available` condition; `ServiceMonitor` is counted, not judged |

A `ServiceMonitor` has no `.status` at all. It is counted so the report can say
the operator is installed and how much it is scraping — judging it would mean
inventing a health signal that does not exist, so its line says
`(not assessed)`.

## What it deliberately does not do

- **It never drives the cluster verdict.** The section is advisory, like
  `--certs` and `--disk-usage`. A third-party CRD's opinion of itself, read
  through field paths kubeagent infers, must not decide whether your cluster is
  Healthy or Degraded.
- **It does not compute GitOps drift.** Argo CD's `OutOfSync` and Flux's
  equivalent are deliberately not read: drift is a separate concern, and
  flagging it here would make every pending deploy look like a failure.
- **It never writes.** No CRD is in the `--fix` allowlist, and none will be.
- **A suspended reconciler is not an incident.** A Flux `Kustomization` with
  `spec.suspend: true` reports `suspended`, not `unhealthy` — its `Ready`
  condition went stale the moment somebody parked it on purpose.

## When kubeagent cannot tell

Every rule degrades to `unknown` rather than to `unhealthy`. If an operator
renames a field, or serves a CRD version kubeagent has not seen, the resource is
counted as `unknown` and never flagged. A drifted heuristic should report a
missing signal, not a false outage. A detached Longhorn volume is the everyday
case: it reports `robustness: unknown`, and an idle PVC is not an incident.

## What the report can contain

Metadata and state only: namespace, name, kind, state, and the operator's own
condition **reason**. Never a CR's `spec`, never arbitrary `status` content, and
never a condition's free-text `message`.

That boundary is deliberate. An Argo CD `Application` carries a Git repository
URL that can embed a token; a cert-manager `Issuer` references ACME account
keys; a CNPG `Cluster` names backup credentials; and cert-manager writes ACME
order URLs into condition messages. Only the CamelCase `reason` — a token by API
convention — is read, and it is trimmed to one line.

Large estates are bounded too: counts are always exact, but at most 20 unhealthy
resources are listed per kind, with the remainder reported as `… +N more
unhealthy` rather than silently dropped.

## RBAC

Most human kubeconfigs already allow these `list` calls. On a restricted context
or for the in-cluster ServiceAccount, apply the scan-only add-on:

```bash
kubectl apply -f deploy/rbac-operators.yaml
```

Without it, `--operators` still runs: API discovery is available to every
authenticated user, so the report names which operators are installed and marks
each kind as forbidden — a genuinely useful answer rather than an error.
```

- [ ] **Step 5: Add the nav entry**

In `website/mkdocs.yml`, add the page to the `Features` list, after `Platform facts`:

```yaml
      - Operator health: features/operators.md
```

- [ ] **Step 6: Update the roadmap Theme F line**

In `website/docs/roadmap.md`, replace the Theme F bullet (currently lines 418-421) with:

```markdown
- **F · Ecosystem & operators** — first-class awareness of the operators people
  actually run: operator/CRD adapters for cert-manager, CloudNativePG, Longhorn,
  Argo CD, Flux, and the Prometheus operator (shipped), with GitOps drift,
  cost/right-sizing, and scheduling-headroom hints still to come.
```

- [ ] **Step 7: Add the CHANGELOG entry**

Under `## [Unreleased]` → `### Added` in `CHANGELOG.md` (create the `### Added` heading if `[Unreleased]` has none):

```markdown
- **Operator health (`scan --operators`, opt-in, advisory)** — reports what
  cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, and the Prometheus
  operator say about themselves, read through the dynamic client so kubeagent
  compiles against none of their Go APIs and needs no rebuild when you install
  one. API discovery is the installation signal: an operator whose group the
  API server does not serve is skipped with zero API calls, no error, and no
  report entry, so a default scan and a scan on a cluster running none of them
  both cost nothing.

  Ten resource kinds are assessed by two declarative rules, and every rule
  degrades to `unknown` rather than `unhealthy`: a renamed field or an unseen
  CRD version yields a missing signal, never a false outage. A suspended Flux
  reconciler reports `suspended`, not a stale `Ready=False`. Argo CD's sync
  status is deliberately not read — drift is a separate concern, and flagging
  it would make every pending deploy look like a failure.

  The report carries metadata and state only: namespace, name, kind, state, and
  the operator's own CamelCase condition reason. A CR's `spec`, its arbitrary
  `status` content, and a condition's free-text `message` never reach it — an
  Argo CD `Application` embeds a Git URL that can carry a token, a cert-manager
  `Issuer` references ACME account keys, a CNPG `Cluster` names backup
  credentials, and cert-manager writes ACME order URLs into condition messages.
  Counts stay exact while at most 20 unhealthy resources are listed per kind,
  the remainder reported rather than dropped.

  Read-only: `list` only, never `get`, `watch`, or any write. Advisory: the
  section never affects the cluster verdict. `deploy/rbac-operators.yaml` is
  the scan-only grant; without it the report still names which operators are
  installed and marks each kind forbidden.
```

- [ ] **Step 8: Verify the docs build**

```bash
export PATH=$PATH:/usr/local/go/bin:/tmp/mkdocs-venv/bin
(cd website && mkdocs build --strict -f mkdocs.yml)
```

Expected: "Documentation built", exit 0, and no `WARNING` line naming `features/operators.md`. The red "Material for MkDocs 2.0" banner is cosmetic.

- [ ] **Step 9: Commit**

```bash
git add deploy/rbac-operators.yaml deploy/README.md website/docs/features/operators.md website/mkdocs.yml website/docs/roadmap.md CHANGELOG.md
git commit -m "docs(operators): RBAC add-on, feature page, changelog, roadmap

deploy/rbac-operators.yaml is a standalone scan-only ClusterRole granting list
on the six groups, following rbac-logs.yaml's shape — not an aggregation into
the base role, so applying deploy/ without it changes nothing. It gets no Helm
values: the chart deploys the watch daemon, which does not read operator CRDs
in this slice, and values for a flag the chart cannot set would be dead
configuration."
```

---

### Task 9: Chaos scenario 16

**Files:**
- Modify: `chaos/run.sh`, `chaos/README.md`

**Interfaces:**
- Consumes: the built `kubeagent` binary with `--operators` (Task 7).
- Produces: `scenario_16_operators`, registered in `run_scenarios`.

The scenario proves the gate in **both** directions on a real cluster: a real operator's CRDs are read and reported, and a CRD kubeagent has no adapter for stays absent. It also asserts no CR spec content reaches the output.

- [ ] **Step 1: Add the scenario**

In `chaos/run.sh`, insert after `scenario_15_multicluster`'s closing brace and before `scenario_02_certs`:

```bash
scenario_16_operators() {   # real cert-manager CRDs -> --operators; an unadapted CRD stays absent
  log "scenario 16: operator/CRD adapters (--operators)"
  local ns=chaos-operators
  local cmurl="https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml"
  kubectl --context "$CTX" apply -f "$cmurl" >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n cert-manager rollout status deploy/cert-manager --timeout=180s >/dev/null 2>&1 || true
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null

  # A Certificate pointing at an Issuer that does not exist: cert-manager sets
  # Ready=False (reason IssuerNotFound) within seconds, with no ACME round trip
  # and no outbound network. The distinctive secretName and commonName are the
  # spec-leak probe below — neither may appear anywhere in the report.
  kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null <<'CERT'
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: doomed
spec:
  secretName: doomed-tls-chaosonlytoken
  commonName: doomed.chaos.invalid
  dnsNames: [doomed.chaos.invalid]
  issuerRef:
    name: no-such-issuer
    kind: Issuer
CERT

  # A CRD kubeagent has no adapter for: the discovery gate must leave it out.
  kubectl --context "$CTX" apply -f - >/dev/null <<'CRD'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.chaos.example.com
spec:
  group: chaos.example.com
  scope: Namespaced
  names: { plural: widgets, singular: widget, kind: Widget }
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema: { type: object, x-kubernetes-preserve-unknown-fields: true }
CRD
  kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null 2>&1 || true
  sleep 5
  kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null 2>&1 <<'WIDGET' || true
apiVersion: chaos.example.com/v1
kind: Widget
metadata: { name: unadapted }
WIDGET

  sleep 20
  local out body
  out="$(scan --operators 2>&1 || true)"
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- gate checks ---\n'
    printf 'unadapted Widget kind in report: %s\n' "$(printf '%s\n' "$body" | grep -c 'Widget' || true)"
    printf 'CR spec content in report:       %s\n' "$(printf '%s\n' "$body" | grep -cE 'chaosonlytoken|doomed\.chaos\.invalid' || true)"
    printf 'Certificate line:                %s\n' "$(printf '%s\n' "$body" | grep -m1 'Certificate' || true)"
    printf 'cluster verdict:                 %s\n' "$(printf '%s\n' "$body" | grep -m1 '^Cluster:' || true)"
  } | record "16. Operator/CRD adapters (--operators)" "detected: cert-manager Certificate Ready=False; unadapted CRD absent (0); no CR spec content (0)"

  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete crd widgets.chaos.example.com --wait=false >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete -f "$cmurl" --wait=false >/dev/null 2>&1 || true
}
```

- [ ] **Step 2: Register it and widen the `--only` comment**

In `run_scenarios`, add `16_operators` to the `all` array, before `01_etcd`:

```bash
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 14 15_multicluster 16_operators 01_etcd)
```

And update the zero-padding comment near the top of the file:

```bash
# Normalize a numeric --only to the zero-padded form used in scenario keys (01..16).
```

- [ ] **Step 3: Add the `chaos/README.md` row**

Append after the row for scenario 15:

```markdown
| 16 | Operator/CRD adapters | install real cert-manager, create a `Certificate` referencing an `Issuer` that does not exist, and apply an unrelated CRD kubeagent has no adapter for | `--operators` names the cert-manager `Certificate` with `Ready=False`, the unadapted CRD is **absent** from the report (the discovery gate proven in both directions), no CR `spec` content appears in any line, and the cluster verdict stays driven by core workloads |
```

- [ ] **Step 4: Syntax-check the harness and run the scenario alone**

```bash
bash -n chaos/run.sh
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
./chaos/run.sh --recreate --only 16 --out /tmp/chaos-16.md
```

Expected: `bash -n` silent; the run reports scenario 16 and `/tmp/chaos-16.md` shows `unadapted Widget kind in report: 0` and `CR spec content in report: 0`, with a `Certificate` line naming `Ready=False`.

- [ ] **Step 5: Commit**

```bash
git add chaos/run.sh chaos/README.md
git commit -m "test(chaos): scenario 16 proves the operator discovery gate both ways

Installs real cert-manager and creates a Certificate whose issuerRef cannot
resolve — Ready=False within seconds, no ACME round trip — then applies a CRD
kubeagent has no adapter for and asserts it never appears. A synthetic CRD
alone would exercise the dynamic path without proving a single real adapter
is correct.

The Certificate carries a distinctive secretName and commonName that the
scenario greps for: the report must contain neither, pinning that no CR spec
content crosses into the output on a live cluster."
```

---

## Post-plan gate (controller, not a task)

After Task 9 and the whole-branch review:

1. **Full chaos gate.** This branch touches `internal/collect` and `internal/cluster`, so the release rule requires the whole suite, not just scenario 16:
   ```bash
   export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
   unset ANTHROPIC_API_KEY
   ./chaos/run.sh --recreate
   ```
2. **No chart bump.** `deploy/helm/` is untouched by this branch; the release's `scripts/bump-version.sh` patch bump of the chart version is correct and no hand MINOR bump is needed.
3. **No demo-GIF refresh.** `--operators` is off by default, so the default scan output the GIF and `website/docs/quickstart.md` show is unchanged.

## Self-review

**Spec coverage.** Every spec section maps to a task: the `internal/operators` types and rules → Tasks 1-2; the adapter table and its non-optional per-row fixtures → Task 3; `internal/cluster` extraction plus `NewDynamicClients` → Task 4; the `collect` discovery gate, version tolerance, namespace scoping, and failure isolation → Task 5; the report field, text section, JSON, and golden regeneration → Task 6; the flag, env override, usage line, and lazy client construction → Task 7; RBAC, the feature page, `deploy/README.md`, CHANGELOG, and the roadmap line → Task 8; chaos scenario 16 and its README row → Task 9. Bounding, the advisory rule, and the metadata-only boundary are Global Constraints, each pinned by a named test (`TestAssess_CountsAreExactAndUnhealthyIsCapped`, `TestPrintInventory_HealthyOperatorsStillPrintTheAllClear`, `TestAssess_ReportCarriesNoSpecContent`).

**Deliberate deviations from the spec** are listed under "Plan decisions that refine the spec" and are binding: the raw-object boundary is `internal/operators`, `FieldRule` gains `Suspended`, `OperatorReport` gains `APIVersions`, the "version unrecognized" state is dropped as dead code in favour of a served-version fallback, and `KUBEAGENT_OPERATORS` is the flag's default. The spec's illustrative reason string `Ready=False: order is pending` is rendered as `Ready=False: IssuerNotFound` throughout, because reading the condition's free-text `message` is exactly what the security boundary forbids.

**One deliberate coupling.** `internal/operators` imports `internal/alert` for `RedactError`, which transitively pulls in `alertstate` and `watchstate` — weight a pure assessment package would otherwise not carry. It is the right trade: `RedactError`'s own doc comment blesses exactly this use ("callers outside this package … whose webhook is a Kubernetes API server whose URL can just as validly carry userinfo or an auth-proxy query string"), and duplicating security-critical redaction logic risks the two copies diverging, with the divergent one leaking. Redaction happens where the string is produced for the report, not upstream in `collect`, so a caller that hand-builds a `Fetched` cannot bypass it.

**Type consistency.** `Adapter`, `Fetched`, `Resource`, `KindReport`, `OperatorReport`, `Report`, `State`, `Rule`, `ConditionRule`, `FieldRule`, `Assess`, `Adapters`, `MaxUnhealthyPerKind`, `KindReport.Total`, `OperatorResources`, `NewDynamicClients`, and `Input.Operators` are spelled identically in every task that names them. `contains` lives in `internal/operators`; `containsString` in `internal/collect` — different packages, no collision with anything existing (verified: neither name is currently defined in either package).
