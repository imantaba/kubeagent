package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/gitops"
)

func driftFixture() *gitops.Report {
	return &gitops.Report{
		Threshold: "1h",
		Reconcilers: []gitops.ReconcilerReport{
			{
				Reconciler:  "Argo CD",
				APIVersions: []string{"argoproj.io/v1alpha1"},
				Kinds: []gitops.KindReport{{
					Kind:       "Application",
					APIVersion: "argoproj.io/v1alpha1",
					Counts: map[gitops.State]int{
						gitops.StateSynced: 14, gitops.StatePending: 1, gitops.StateBlocked: 1,
					},
					Drifted: []gitops.Workload{
						{Reconciler: "Argo CD", Kind: "Application", Namespace: "prod", Name: "payments",
							State: gitops.StateBlocked, Detail: "OutOfSync a1b2c3d, last synced 6d ago (auto-sync off)"},
						{Reconciler: "Argo CD", Kind: "Application", Namespace: "staging", Name: "web",
							State: gitops.StatePending, Detail: "OutOfSync 9f8e7d6, last synced 4m ago"},
					},
				}},
			},
			{
				Reconciler:  "Flux",
				APIVersions: []string{"kustomize.toolkit.fluxcd.io/v1", "helm.toolkit.fluxcd.io/v2"},
				Kinds: []gitops.KindReport{
					{
						Kind:       "Kustomization",
						APIVersion: "kustomize.toolkit.fluxcd.io/v1",
						Counts:     map[gitops.State]int{gitops.StateSynced: 9, gitops.StateStale: 1},
						Drifted: []gitops.Workload{
							{Reconciler: "Flux", Kind: "Kustomization", Namespace: "flux-system", Name: "infra",
								State: gitops.StateStale, Detail: "attempted a1b2c3d, applied 9f8e7d6, not ready 3d: BuildFailed"},
						},
						Truncated: 2,
					},
					{Kind: "HelmRelease", APIVersion: "helm.toolkit.fluxcd.io/v2",
						Counts: map[gitops.State]int{gitops.StateSynced: 4}},
				},
			},
		},
	}
}

func TestPrintGitOps(t *testing.T) {
	var buf bytes.Buffer
	if err := printGitOps(driftFixture(), &buf); err != nil {
		t.Fatalf("printGitOps: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"GITOPS DRIFT  (advisory — reconciler-reported; threshold 1h; no repo URLs)",
		"  Argo CD (argoproj.io/v1alpha1)",
		"    Application     14 synced, 1 pending, 1 blocked",
		"      ✗ prod/payments  OutOfSync a1b2c3d, last synced 6d ago (auto-sync off)",
		"      · staging/web  OutOfSync 9f8e7d6, last synced 4m ago",
		"  Flux (kustomize.toolkit.fluxcd.io/v1, helm.toolkit.fluxcd.io/v2)",
		"    Kustomization   9 synced, 1 stale",
		"      ✗ flux-system/infra  attempted a1b2c3d, applied 9f8e7d6, not ready 3d: BuildFailed",
		"      … +2 more",
		"    HelmRelease     4 synced",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestPrintGitOpsSkipsEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := printGitOps(nil, &buf); err != nil {
		t.Fatalf("printGitOps(nil): %v", err)
	}
	if err := printGitOps(&gitops.Report{Threshold: "1h"}, &buf); err != nil {
		t.Fatalf("printGitOps(empty): %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %q, want nothing when there is no reconciler installed", buf.String())
	}
}

func TestPrintGitOpsForbiddenAndError(t *testing.T) {
	var buf bytes.Buffer
	rep := &gitops.Report{
		Threshold: "1h",
		Reconcilers: []gitops.ReconcilerReport{{
			Reconciler: "Flux",
			Kinds: []gitops.KindReport{
				{Kind: "Kustomization", Forbidden: true},
				{Kind: "HelmRelease", Error: "Get \"https://api.example\": connection refused"},
			},
		}},
	}
	if err := printGitOps(rep, &buf); err != nil {
		t.Fatalf("printGitOps: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "list forbidden — apply deploy/rbac-gitops.yaml") {
		t.Errorf("missing the RBAC hint\n%s", out)
	}
	if !strings.Contains(out, "list failed: Get \"https://api.example\": connection refused") {
		t.Errorf("missing the list failure\n%s", out)
	}
}

func TestDriftSummaryOrderIsFixed(t *testing.T) {
	k := gitops.KindReport{Counts: map[gitops.State]int{
		gitops.StateUnknown: 1, gitops.StateBlocked: 2, gitops.StateStale: 3,
		gitops.StatePending: 4, gitops.StateSynced: 5,
	}}
	const want = "5 synced, 4 pending, 3 stale, 2 blocked, 1 unknown"
	// Repeated because ranging a map would pass once and fail later.
	for i := 0; i < 20; i++ {
		if got := driftSummary(k); got != want {
			t.Fatalf("driftSummary() = %q, want %q", got, want)
		}
	}
	if got := driftSummary(gitops.KindReport{Counts: map[gitops.State]int{}}); got != "0" {
		t.Errorf("empty counts = %q, want %q", got, "0")
	}
}

func TestPrintInventoryJSONIncludesGitOps(t *testing.T) {
	var buf bytes.Buffer
	in := Input{GitOps: driftFixture()}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatalf("PrintInventory: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	gitopsAny, ok := decoded["gitops"]
	if !ok {
		t.Fatalf("decoded JSON has no %q key\n--- got ---\n%s", "gitops", buf.String())
	}
	gitopsMap, ok := gitopsAny.(map[string]any)
	if !ok {
		t.Fatalf("gitops value is %T, want an object", gitopsAny)
	}
	reconcilers, ok := gitopsMap["reconcilers"].([]any)
	if !ok || len(reconcilers) == 0 {
		t.Fatalf("gitops.reconcilers missing or empty: %v", gitopsMap["reconcilers"])
	}
	argocd, ok := reconcilers[0].(map[string]any)
	if !ok || argocd["reconciler"] != "Argo CD" {
		t.Fatalf("gitops.reconcilers[0].reconciler = %v, want %q", argocd["reconciler"], "Argo CD")
	}
	kinds, ok := argocd["kinds"].([]any)
	if !ok || len(kinds) == 0 {
		t.Fatalf("gitops.reconcilers[0].kinds missing or empty: %v", argocd["kinds"])
	}
	application, ok := kinds[0].(map[string]any)
	if !ok {
		t.Fatalf("gitops.reconcilers[0].kinds[0] is %T, want an object", kinds[0])
	}
	drifted, ok := application["drifted"].([]any)
	if !ok || len(drifted) == 0 {
		t.Fatalf("gitops.reconcilers[0].kinds[0].drifted missing or empty: %v", application["drifted"])
	}
	payments, ok := drifted[0].(map[string]any)
	if !ok || payments["name"] != "payments" || payments["state"] != "blocked" {
		t.Fatalf("gitops.reconcilers[0].kinds[0].drifted[0] = %v, want name=payments state=blocked", drifted[0])
	}
}

func TestPrintInventoryJSONOmitsGitOpsWhenNil(t *testing.T) {
	var buf bytes.Buffer
	in := Input{}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatalf("PrintInventory: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := decoded["gitops"]; ok {
		t.Errorf("decoded JSON has a %q key with GitOps nil, want it absent\n--- got ---\n%s", "gitops", buf.String())
	}
}
