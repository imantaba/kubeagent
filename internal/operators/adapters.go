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
