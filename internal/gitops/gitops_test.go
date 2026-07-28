package gitops

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/operators"
)

var assessNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// adapterFor returns the gitops adapter row for a plural resource name.
func adapterFor(t *testing.T, resource string) operators.Adapter {
	t.Helper()
	for _, a := range Adapters() {
		if a.Resource == resource {
			return a
		}
	}
	t.Fatalf("no adapter for %q", resource)
	return operators.Adapter{}
}

// item wraps a fixture object with a namespace and name.
func item(ns, name string, obj map[string]any) unstructured.Unstructured {
	obj["metadata"] = map[string]any{"namespace": ns, "name": name}
	return unstructured.Unstructured{Object: obj}
}

func TestAssessGroupsByReconciler(t *testing.T) {
	fetched := []operators.Fetched{
		{
			Adapter: adapterFor(t, "applications"), APIVersion: "argoproj.io/v1alpha1",
			Items: []unstructured.Unstructured{
				item("prod", "payments", argoApp("OutOfSync", "Succeeded", "2026-07-21T12:00:00Z", false)),
				item("prod", "web", argoApp("Synced", "Succeeded", "2026-07-27T11:00:00Z", true)),
			},
		},
		{
			Adapter: adapterFor(t, "kustomizations"), APIVersion: "kustomize.toolkit.fluxcd.io/v1",
			Items: []unstructured.Unstructured{
				item("flux-system", "infra", fluxObj(false, []map[string]any{
					cond("Ready", "False", "BuildFailed", "2026-07-24T12:00:00Z"),
				}, revA, revB)),
			},
		},
		{
			Adapter: adapterFor(t, "helmreleases"), APIVersion: "helm.toolkit.fluxcd.io/v2",
			Items: []unstructured.Unstructured{
				item("apps", "redis", fluxObj(false, []map[string]any{
					cond("Ready", "True", "InstallSucceeded", "2026-07-27T11:00:00Z"),
				}, "", "")),
			},
		},
	}
	rep := Assess(fetched, assessNow, time.Hour)

	if rep.Threshold != "1h" {
		t.Errorf("Threshold = %q, want %q", rep.Threshold, "1h")
	}
	if len(rep.Reconcilers) != 2 {
		t.Fatalf("got %d reconcilers, want 2 (Argo CD, Flux)", len(rep.Reconcilers))
	}
	if rep.Reconcilers[0].Reconciler != "Argo CD" || rep.Reconcilers[1].Reconciler != "Flux" {
		t.Errorf("reconcilers = %q, %q; want Argo CD, Flux in adapter-table order",
			rep.Reconcilers[0].Reconciler, rep.Reconcilers[1].Reconciler)
	}
	flux := rep.Reconcilers[1]
	if len(flux.Kinds) != 2 {
		t.Errorf("Flux has %d kinds, want 2", len(flux.Kinds))
	}
	if len(flux.APIVersions) != 2 {
		t.Errorf("Flux APIVersions = %v, want both served group/versions", flux.APIVersions)
	}
	argoKind := rep.Reconcilers[0].Kinds[0]
	if argoKind.Counts[StateSynced] != 1 || argoKind.Counts[StateBlocked] != 1 {
		t.Errorf("Application counts = %v, want 1 synced + 1 blocked", argoKind.Counts)
	}
	if len(argoKind.Drifted) != 1 || argoKind.Drifted[0].Name != "payments" {
		t.Errorf("Drifted = %+v, want only the blocked Application", argoKind.Drifted)
	}
}

func TestAssessIgnoresNonGitOpsKinds(t *testing.T) {
	// main.go hands Assess the operator adapter superset when both flags are set.
	var certManager operators.Adapter
	for _, a := range operators.Adapters() {
		if a.Resource == "certificates" {
			certManager = a
		}
	}
	if certManager.Resource == "" {
		t.Fatal("operators.Adapters() no longer has a certificates row")
	}
	rep := Assess([]operators.Fetched{{
		Adapter: certManager, APIVersion: "cert-manager.io/v1",
		Items: []unstructured.Unstructured{item("shop", "web-tls", map[string]any{})},
	}}, assessNow, time.Hour)
	if len(rep.Reconcilers) != 0 {
		t.Errorf("got %+v, want no reconcilers — cert-manager is not a GitOps kind", rep.Reconcilers)
	}
}

func TestAssessOmitsEmptyKindsButKeepsDenialsAndErrors(t *testing.T) {
	rep := Assess([]operators.Fetched{
		{Adapter: adapterFor(t, "applications"), APIVersion: "argoproj.io/v1alpha1"},
		{Adapter: adapterFor(t, "kustomizations"), APIVersion: "kustomize.toolkit.fluxcd.io/v1", Forbidden: true},
		{Adapter: adapterFor(t, "helmreleases"), APIVersion: "helm.toolkit.fluxcd.io/v2",
			Err: &url.Error{
				Op:  "Get",
				URL: "https://tok3n@10.0.0.1:6443/apis",
				Err: errors.New("connection refused"),
			}},
	}, assessNow, time.Hour)

	if len(rep.Reconcilers) != 2 {
		t.Fatalf("got %d reconcilers, want 2", len(rep.Reconcilers))
	}
	if len(rep.Reconcilers[0].Kinds) != 0 {
		t.Errorf("an installed but empty kind must be omitted, got %+v", rep.Reconcilers[0].Kinds)
	}
	flux := rep.Reconcilers[1]
	if len(flux.Kinds) != 2 {
		t.Fatalf("Flux has %d kinds, want the forbidden one and the failed one", len(flux.Kinds))
	}
	if !flux.Kinds[0].Forbidden {
		t.Error("forbidden kind must be kept")
	}
	if flux.Kinds[1].Error == "" {
		t.Error("failed list must be kept")
	}
	if strings.Contains(flux.Kinds[1].Error, "tok3n") {
		t.Errorf("Error = %q leaks credentials; it must go through redact.Error", flux.Kinds[1].Error)
	}
}

func TestAssessOrdersWorstFirstAndTruncates(t *testing.T) {
	var items []unstructured.Unstructured
	// 15 pending, then 6 blocked and 4 stale, deliberately appended after them so
	// only an ordering by severity can rescue the interesting rows from the cap.
	for i := 0; i < 15; i++ {
		items = append(items, item("aaa", fmt.Sprintf("pending-%02d", i),
			argoApp("OutOfSync", "Succeeded", "2026-07-27T11:59:00Z", true)))
	}
	for i := 0; i < 6; i++ {
		items = append(items, item("zzz", fmt.Sprintf("blocked-%02d", i),
			argoApp("OutOfSync", "Succeeded", "2026-07-21T12:00:00Z", false)))
	}
	for i := 0; i < 4; i++ {
		items = append(items, item("zzz", fmt.Sprintf("stale-%02d", i),
			argoApp("OutOfSync", "Succeeded", "2026-07-21T12:00:00Z", true)))
	}
	rep := Assess([]operators.Fetched{{
		Adapter: adapterFor(t, "applications"), APIVersion: "argoproj.io/v1alpha1", Items: items,
	}}, assessNow, time.Hour)

	k := rep.Reconcilers[0].Kinds[0]
	if k.Total() != 25 {
		t.Errorf("Total() = %d, want 25 — counts are exact even when the list is capped", k.Total())
	}
	if len(k.Drifted) != MaxPerKind {
		t.Fatalf("enumerated %d, want %d", len(k.Drifted), MaxPerKind)
	}
	if k.Truncated != 5 {
		t.Errorf("Truncated = %d, want 5", k.Truncated)
	}
	for i := 0; i < 6; i++ {
		if k.Drifted[i].State != StateBlocked {
			t.Fatalf("Drifted[%d].State = %q, want blocked first", i, k.Drifted[i].State)
		}
	}
	for i := 6; i < 10; i++ {
		if k.Drifted[i].State != StateStale {
			t.Fatalf("Drifted[%d].State = %q, want stale after blocked", i, k.Drifted[i].State)
		}
	}
	if k.Drifted[10].State != StatePending || k.Drifted[10].Name != "pending-00" {
		t.Errorf("Drifted[10] = %+v, want pending sorted by name", k.Drifted[10])
	}
}

func TestAssessClampsNegativeThreshold(t *testing.T) {
	rep := Assess([]operators.Fetched{{
		Adapter: adapterFor(t, "applications"), APIVersion: "argoproj.io/v1alpha1",
		Items: []unstructured.Unstructured{
			item("prod", "web", argoApp("OutOfSync", "Succeeded", "2026-07-27T11:59:00Z", true)),
		},
	}}, assessNow, -time.Hour)
	if rep.Threshold != "0s" {
		t.Errorf("Threshold = %q, want %q", rep.Threshold, "0s")
	}
	if got := rep.Reconcilers[0].Kinds[0].Drifted[0].State; got != StateStale {
		t.Errorf("State = %q, want stale — a zero threshold flags anything that differs", got)
	}
}

// TestAssessLeaksNothing is the boundary test: every fixture carries a repo URL
// with a token, a spec path, a branch-qualified revision, and a prose condition
// message. None may survive into the assessed report, in Go or in JSON.
func TestAssessLeaksNothing(t *testing.T) {
	fetched := []operators.Fetched{
		{Adapter: adapterFor(t, "applications"), APIVersion: "argoproj.io/v1alpha1",
			Items: []unstructured.Unstructured{
				item("prod", "payments", argoApp("OutOfSync", "Failed", "2026-07-21T12:00:00Z", false)),
			}},
		{Adapter: adapterFor(t, "kustomizations"), APIVersion: "kustomize.toolkit.fluxcd.io/v1",
			Items: []unstructured.Unstructured{
				item("flux-system", "infra", fluxObj(false, []map[string]any{
					cond("Ready", "False", "BuildFailed", "2026-07-24T12:00:00Z"),
				}, revA, revB)),
			}},
		{Adapter: adapterFor(t, "helmreleases"), APIVersion: "helm.toolkit.fluxcd.io/v2",
			Items: []unstructured.Unstructured{
				item("apps", "redis", fluxObj(true, []map[string]any{
					cond("Ready", "False", "UpgradeFailed", "2026-07-24T12:00:00Z"),
				}, revA, "")),
			}},
	}
	rep := Assess(fetched, assessNow, time.Hour)
	blob, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	haystack := fmt.Sprintf("%+v %s", rep, blob)
	for _, leak := range []string{
		"tok3n", "git.example", "overlays/prod", "./overlays/prod",
		"failed to clone", "failed to apply", "sha1:", "tok3n-repo",
		"a1b2c3d4e5f6", // the full SHA — only the 7-character prefix may appear
	} {
		if strings.Contains(haystack, leak) {
			t.Errorf("assessed report leaks %q", leak)
		}
	}
}

// TestEveryAdapterHasAFixture stops a new adapter row shipping untested: adding a
// row without teaching assessorFor about it would silently produce an empty
// section instead of a wrong one.
func TestEveryAdapterHasAFixture(t *testing.T) {
	for _, a := range Adapters() {
		if _, ok := assessorFor(a); !ok {
			t.Errorf("adapter %s/%s has no assessor — add one and a fixture test", a.Group, a.Resource)
		}
	}
}
