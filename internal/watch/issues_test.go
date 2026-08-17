package watch

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/certhealth"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/hpahealth"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/pdbhealth"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/termhealth"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

// keySet renders the keys as a lookup of "kind/ns/name:issue" strings.
func keySet(keys []watchstate.Key) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k.String()] = true
	}
	return out
}

// TestIssueKeys_CoversEverySource walks the shared sampleResult fixture and
// asserts the exact key set, so a new detector wired into scan.Result but not into
// issueKeys shows up as a missing key — and a miswired one that emits an extra key
// (a wrong Kind, an unfiltered advisory finding) shows up as a surplus.
func TestIssueKeys_CoversEverySource(t *testing.T) {
	got := keySet(issueKeys(sampleResult()))
	want := []string{
		"Deployment/shop/web:CrashLoopBackOff",
		"Service/shop/api-svc:NoEndpoints",
		"Ingress/shop/web:NoEndpoints",
		"PVC/shop/data-pvc:ProvisioningFailed",
		"Namespace/legacy-ns:StuckTerminating",
		"PodDisruptionBudget/shop/api:PDBBlocked",
		"HorizontalPodAutoscaler/shop/api-hpa:HPACapped",
		"ValidatingWebhookConfiguration/policy-webhook/w:NoEndpoints",
		"ValidatingWebhookConfiguration/slow-webhook/s.io:HighTimeout",
		"ResourceQuota/shop/compute/pods:near",
		"Node/w:KubeletUnhealthy",
		"Cluster/control-plane:Unhealthy",
		"Cluster/coredns:DNSDegraded",
		"Secret/shop/shop-tls:CertExpired",
		"Secret/infra/api-tls:CertExpiring",
		"Secret/infra/broken-tls:CertInvalid",
		"Volume/n1:DiskOverThreshold",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing key %q; got %v", w, got)
		}
	}
	if total := len(got); total != len(want) {
		for _, w := range want {
			delete(got, w)
		}
		t.Errorf("issueKeys returned %d unique keys, want %d; surplus: %v", total, len(want), got)
	}
}

// TestIssueKeys_AgreesWithGateOnStuckTerminatingPDBAndHPAVocabulary drives the
// same scan.Result through issueKeys (the watch daemon's /issues projection)
// and findings.Flatten (gate's projection) and asserts the two name every
// stuck-terminating, PDB and HPA instance with the identical CamelCase Issue
// string — so /issues and gate can never disagree about the same input.
func TestIssueKeys_AgreesWithGateOnStuckTerminatingPDBAndHPAVocabulary(t *testing.T) {
	res := scan.Result{
		StuckTerminating: []termhealth.Issue{
			{Kind: "Namespace", Name: "legacy-ns", Age: "2h", Reason: "stuck in Terminating"},
		},
		PDBIssues: []pdbhealth.Issue{
			{Namespace: "shop", Name: "unsat", Category: "unsatisfiable", Reason: "r1"},
			{Namespace: "shop", Name: "stl", Category: "stale", Reason: "r2"},
			{Namespace: "shop", Name: "blk", Category: "blocking", Reason: "r3"},
			{Namespace: "shop", Name: "sgl", Category: "singleton", Reason: "r4"},
		},
		HPAIssues: []hpahealth.Issue{
			{Namespace: "shop", Name: "unable-hpa", Category: "unable", Reason: "r5"},
			{Namespace: "shop", Name: "metrics-hpa", Category: "metrics", Reason: "r6"},
			{Namespace: "shop", Name: "capped-hpa", Category: "capped", Reason: "r7"},
			{Namespace: "shop", Name: "disabled-hpa", Category: "disabled", Reason: "r8"},
			{Namespace: "shop", Name: "ambiguous-hpa", Category: "ambiguous", Reason: "r9"},
		},
	}

	watchIssues := map[string]string{} // name -> issue, from /issues
	for _, k := range issueKeys(&res) {
		watchIssues[k.Name] = k.Issue
	}
	gateIssues := map[string]string{} // name -> issue, from gate
	for _, f := range findings.Flatten(res) {
		gateIssues[f.Name] = f.Issue
	}

	names := []string{"legacy-ns", "unsat", "stl", "blk", "sgl", "unable-hpa", "metrics-hpa", "capped-hpa", "disabled-hpa", "ambiguous-hpa"}
	for _, name := range names {
		w, ok := watchIssues[name]
		if !ok {
			t.Errorf("%s: missing from /issues", name)
			continue
		}
		g, ok := gateIssues[name]
		if !ok {
			t.Errorf("%s: missing from gate", name)
			continue
		}
		if w != g {
			t.Errorf("%s: /issues says %q, gate says %q; the two surfaces disagree", name, w, g)
		}
	}
}

func TestIssueKeys_SkipsExpectedIssues(t *testing.T) {
	got := keySet(issueKeys(sampleResult()))
	for _, unwanted := range []string{
		"Service/shop/parked-svc:NoEndpoints",
		"Ingress/shop/parked:NoEndpoints",
	} {
		if got[unwanted] {
			t.Errorf("expected/parked issue %q must not be tracked", unwanted)
		}
	}
}

func TestIssueKeys_CollapsesDuplicateRoutes(t *testing.T) {
	res := &scan.Result{IngressIssues: []ingresshealth.RouteIssue{
		{Namespace: "shop", Ingress: "web", Host: "a.example", Path: "/x", Problem: "NoEndpoints"},
		{Namespace: "shop", Ingress: "web", Host: "b.example", Path: "/y", Problem: "NoEndpoints"},
	}}
	if got := issueKeys(res); len(got) != 1 {
		t.Errorf("got %d keys, want 1 (same ingress + problem collapses): %v", len(got), got)
	}
}

func TestIssueKeys_FlaggedWithoutFindingsIsDegraded(t *testing.T) {
	res := &scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{
		{Namespace: "shop", Name: "api", Kind: "StatefulSet", Ready: 1, Desired: 3},
		{Namespace: "shop", Name: "ok", Kind: "Deployment", Ready: 2, Desired: 2},
	}}}
	got := keySet(issueKeys(res))
	if !got["StatefulSet/shop/api:Degraded"] {
		t.Errorf("flagged findingless workload must yield Degraded; got %v", got)
	}
	if len(got) != 1 {
		t.Errorf("healthy workload must not be tracked; got %v", got)
	}
}

func TestIssueKeys_WorkloadWithFindingsSkipsDegraded(t *testing.T) {
	res := &scan.Result{Inventory: inventory.Result{Workloads: []inventory.Workload{
		{Namespace: "shop", Name: "api", Kind: "Deployment", Ready: 0, Desired: 1,
			Findings: []diagnose.Finding{{Issue: "OOMKilled"}, {Issue: "CrashLoopBackOff"}}},
	}}}
	got := keySet(issueKeys(res))
	if got["Deployment/shop/api:Degraded"] {
		t.Errorf("a workload with findings must not also report Degraded; got %v", got)
	}
	if len(got) != 2 {
		t.Errorf("want one key per finding; got %v", got)
	}
}

func TestIssueKeys_DownNodesCarryTheirReason(t *testing.T) {
	res := &scan.Result{Health: clusterhealth.ClusterHealth{DownNodes: []clusterhealth.DownNode{
		{Name: "w1", Reason: "NotReady"},
		{Name: "w2", Reason: "kubelet not heartbeating"},
	}}}
	got := keySet(issueKeys(res))
	if !got["Node/w1:NotReady"] || !got["Node/w2:KubeletNotHeartbeating"] {
		t.Errorf("down nodes = %v, want NotReady and KubeletNotHeartbeating", got)
	}
}

func TestIssueKeys_NilReportsAndHealthyClusterYieldNothing(t *testing.T) {
	res := &scan.Result{} // no Certificates report, no issues anywhere
	if got := issueKeys(res); len(got) != 0 {
		t.Errorf("empty result yielded %v, want no keys", got)
	}
	healthy := &scan.Result{
		Certificates:  &certhealth.Report{Checked: 3},
		ServiceIssues: []svchealth.Issue{{Namespace: "shop", Name: "ok", Problem: "NoEndpoints", Expected: true}},
	}
	if got := issueKeys(healthy); len(got) != 0 {
		t.Errorf("healthy result yielded %v, want no keys", got)
	}
}
