package operators

import (
	"strings"
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

// ready() and availableCond() build a status carrying one condition of the
// named type. ready is defined in operators_test.go — same package, so it is
// reused here rather than duplicated.
func availableCond(status, reason string) map[string]any {
	return map[string]any{"conditions": []any{
		map[string]any{"type": "Available", "status": status, "reason": reason},
	}}
}

func adapterFixtures() []adapterFixture {
	otherCond := map[string]any{"conditions": []any{
		map[string]any{"type": "Synced", "status": "True"},
	}}
	return []adapterFixture{
		{kind: "Certificate", healthy: ready("True", "Ready"), unhealthy: ready("False", "IssuerNotFound"), missing: otherCond},
		{kind: "Issuer", healthy: ready("True", "IsReady"), unhealthy: ready("False", "ErrInitIssuer"), missing: otherCond},
		{kind: "ClusterIssuer", healthy: ready("True", "IsReady"), unhealthy: ready("False", "ErrGetKeyPair"), missing: otherCond},
		{kind: "Cluster", healthy: ready("True", "ClusterIsReady"), unhealthy: ready("False", "FailedInstance"), missing: otherCond},
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
		{kind: "Kustomization", healthy: ready("True", "ReconciliationSucceeded"), unhealthy: ready("False", "BuildFailed"), missing: otherCond},
		{kind: "HelmRelease", healthy: ready("True", "InstallSucceeded"), unhealthy: ready("False", "UpgradeFailed"), missing: otherCond},
		{kind: "Prometheus", healthy: availableCond("True", ""), unhealthy: availableCond("False", "SomePodsNotReady"), missing: ready("True", "")},
		// ServiceMonitor has no .status at all and no rule: every fixture is unknown.
		{kind: "ServiceMonitor", healthy: nil, unhealthy: nil, missing: nil},
	}
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
		if a.Resource != strings.ToLower(a.Resource) {
			t.Errorf("resource %q must be the lowercase plural discovery reports", a.Resource)
		}
	}
}
