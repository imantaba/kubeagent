package gitops

import "github.com/imantaba/kubeagent/internal/operators"

// Adapters lists the three kinds a GitOps drift scan reads. They carry no Rule
// and no SuspendPath: health is internal/operators' question, and this package
// reads suspend through its own field paths.
//
// The rows deliberately duplicate three entries of operators.Adapters(). That
// table is the operator census; this one is the smallest set --drift can fetch on
// its own, so a drift-only user needs no grant on Longhorn volumes or CNPG
// clusters. Assess matches on group and resource, so it is equally happy being
// handed either table's results.
func Adapters() []operators.Adapter {
	return []operators.Adapter{
		{
			Operator: "Argo CD", Group: "argoproj.io", Version: "v1alpha1",
			Resource: "applications", Kind: "Application",
		},
		{
			Operator: "Flux", Group: "kustomize.toolkit.fluxcd.io", Version: "v1",
			Resource: "kustomizations", Kind: "Kustomization",
		},
		{
			Operator: "Flux", Group: "helm.toolkit.fluxcd.io", Version: "v2",
			Resource: "helmreleases", Kind: "HelmRelease",
		},
	}
}
