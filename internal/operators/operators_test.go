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
