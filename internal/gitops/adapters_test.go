package gitops

import "testing"

func TestAdaptersAreGitOpsKindsOnly(t *testing.T) {
	want := map[string]string{
		"argoproj.io/applications":                   "Application",
		"kustomize.toolkit.fluxcd.io/kustomizations": "Kustomization",
		"helm.toolkit.fluxcd.io/helmreleases":        "HelmRelease",
	}
	got := Adapters()
	if len(got) != len(want) {
		t.Fatalf("Adapters() has %d rows, want %d", len(got), len(want))
	}
	for _, a := range got {
		key := a.Group + "/" + a.Resource
		kind, ok := want[key]
		if !ok {
			t.Errorf("unexpected adapter %s", key)
			continue
		}
		if a.Kind != kind {
			t.Errorf("%s: Kind = %q, want %q", key, a.Kind, kind)
		}
		if a.Rule != nil {
			t.Errorf("%s: carries a health Rule; health belongs to internal/operators", key)
		}
		if len(a.SuspendPath) != 0 {
			t.Errorf("%s: carries a SuspendPath; this package reads suspend itself", key)
		}
		if a.Version == "" || a.Operator == "" {
			t.Errorf("%s: Operator/Version must be set", key)
		}
	}
}
