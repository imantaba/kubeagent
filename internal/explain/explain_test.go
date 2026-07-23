package explain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

// fakeSummarizer stands in for the Anthropic-backed summarizer so tests never
// touch the network. It records whether it was called.
type fakeSummarizer struct {
	called bool
	reply  string
	err    error
}

func (f *fakeSummarizer) summarize(ctx context.Context, prompt string) (string, error) {
	f.called = true
	return f.reply, f.err
}

func TestExplainInventory_SkipsWhenEmptyAndHealthy(t *testing.T) {
	f := &fakeSummarizer{reply: "should not be used"}
	c := &Client{s: f}
	got, err := c.ExplainInventory(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" || f.called {
		t.Errorf("expected no call and empty result; got %q called=%v", got, f.called)
	}
}

func TestExplainInventory_SummarizesFlaggedWorkload(t *testing.T) {
	f := &fakeSummarizer{reply: "  coredns is degraded.  "}
	c := &Client{s: f}
	ws := []inventory.Workload{{Namespace: "kube-system", Name: "coredns", Kind: "Deployment", Ready: 1, Desired: 2}}
	got, err := c.ExplainInventory(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "coredns is degraded." || !f.called {
		t.Errorf("got %q called=%v", got, f.called)
	}
}

func TestExplainInventory_WrapsError(t *testing.T) {
	f := &fakeSummarizer{err: errors.New("boom")}
	c := &Client{s: f}
	_, err := c.ExplainInventory(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, []inventory.Workload{{Name: "x", Ready: 1, Desired: 2}})
	if err == nil || !strings.Contains(err.Error(), "explaining workloads") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

func TestExplainInventory_ErrorsOnEmptyText(t *testing.T) {
	f := &fakeSummarizer{reply: "  \n"}
	c := &Client{s: f}
	_, err := c.ExplainInventory(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, []inventory.Workload{{Name: "x", Ready: 1, Desired: 2}})
	if err == nil || !strings.Contains(err.Error(), "model returned no text") {
		t.Fatalf("expected empty-text error, got %v", err)
	}
}

func TestBuildInventoryPrompt_OnlyStructuredFields(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "kube-system", Name: "coredns", Kind: "Deployment", Ready: 1, Desired: 2, Status: "Degraded", Restarts: 7,
		Findings: []diagnose.Finding{{Pod: "kube-system/coredns-x", Issue: "CrashLoopBackOff", Reason: "boom", Evidence: "restartCount=7"}},
		Pods:     []inventory.PodRow{{Name: "coredns-x", IP: "10.42.9.9", Node: "secret-node-name"}},
	}}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, nil, nil, ws)
	for _, want := range []string{"kube-system", "coredns", "Deployment", "CrashLoopBackOff", "boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
	// Egress guard: per-pod IPs / node names must NOT be sent to the model.
	for _, leak := range []string{"10.42.9.9", "secret-node-name"} {
		if strings.Contains(got, leak) {
			t.Errorf("prompt leaked %q:\n%s", leak, got)
		}
	}
}

func TestExplainInventory_ExplainsDegradedClusterWithNoWorkloads(t *testing.T) {
	f := &fakeSummarizer{reply: "two nodes are NotReady"}
	c := &Client{s: f}
	ch := clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 3, NodesReady: 1, NodeIssues: []string{"n2 NotReady", "n3 NotReady"}}
	got, err := c.ExplainInventory(context.Background(), ch, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "two nodes are NotReady" || !f.called {
		t.Errorf("expected the degraded cluster to be explained; got %q called=%v", got, f.called)
	}
}

func TestBuildInventoryPrompt_LeadsWithDegradedCluster(t *testing.T) {
	ch := clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 3, NodesReady: 1, NodeIssues: []string{"n2 NotReady"}}
	got := buildInventoryPrompt(ch, nil, nil, nil, nil)
	if !strings.Contains(got, "DEGRADED") || !strings.Contains(got, "n2 NotReady") {
		t.Errorf("prompt should lead with the degraded cluster:\n%s", got)
	}
	if strings.Contains(got, "Workload problems") {
		t.Errorf("should not advertise a workloads section when there are none:\n%s", got)
	}
	if !strings.Contains(got, "1/3 nodes Ready") {
		t.Errorf("prompt should include the exact node-count header:\n%s", got)
	}
}

func TestExplainInventory_SummarizesGivenWorkloads(t *testing.T) {
	// ExplainInventory summarizes exactly what it is given — the caller does the
	// filtering, not explain. A healthy workload passed in is summarized.
	f := &fakeSummarizer{reply: "all noted"}
	c := &Client{s: f}
	ws := []inventory.Workload{{Namespace: "a", Name: "web", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running"}}
	got, err := c.ExplainInventory(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "all noted" || !f.called {
		t.Errorf("expected the given workload to be summarized; got %q called=%v", got, f.called)
	}
}

func TestBuildInventoryPrompt_IncludesClusterResources(t *testing.T) {
	s := &resources.Summary{
		MetricsAvailable: true,
		CPU:              resources.Line{Allocatable: "8.0", Requests: "2.0", Limits: "4.0", Usage: "2.0", RequestsPct: 25, LimitsPct: 50, UsagePct: 25},
		Memory:           resources.Line{Allocatable: "16Gi", Requests: "4Gi", Limits: "8Gi", Usage: "4Gi", RequestsPct: 25, LimitsPct: 50, UsagePct: 25},
	}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, s, nil, nil, nil)
	for _, want := range []string{"Cluster resources:", "allocatable 8.0 cores", "requests 2.0 (25%)", "usage 2.0 (25%)", "allocatable 16Gi"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestBuildInventoryPrompt_OmitsUsageWhenMetricsUnavailable(t *testing.T) {
	s := &resources.Summary{
		MetricsAvailable: false,
		CPU:              resources.Line{Allocatable: "8.0", Requests: "2.0", Limits: "4.0", RequestsPct: 25, LimitsPct: 50},
		Memory:           resources.Line{Allocatable: "16Gi", Requests: "4Gi", Limits: "8Gi", RequestsPct: 25, LimitsPct: 50},
	}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, s, nil, nil, nil)
	if !strings.Contains(got, "Cluster resources:") {
		t.Fatalf("expected the resources section:\n%s", got)
	}
	if strings.Contains(got, "usage") {
		t.Errorf("usage clause must be omitted when metrics unavailable:\n%s", got)
	}
}

func TestBuildInventoryPrompt_IncludesOOMContainerResources(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "cattle-system", Name: "rancher", Kind: "Deployment", Ready: 0, Desired: 1, Status: "Degraded",
		Findings: []diagnose.Finding{{
			Pod: "cattle-system/rancher-x", Issue: "OOMKilled", Reason: "killed", Evidence: "e",
			Resources: &diagnose.ContainerResources{Container: "rancher", CPURequest: "500m", CPULimit: "3", MemRequest: "1Gi", MemLimit: "4Gi"},
		}},
	}}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, nil, nil, ws)
	if !strings.Contains(got, "container resources: memory req=1Gi limit=4Gi, cpu req=500m limit=3") {
		t.Errorf("prompt missing OOM container resources:\n%s", got)
	}
}

func TestBuildInventoryPrompt_IncludesPlatform(t *testing.T) {
	f := &platform.Facts{CNI: "Cilium", Ingress: "Traefik", KubeVersion: "v1.35", Distro: "RKE2", Runtime: "containerd", Cloud: "Hetzner Cloud"}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, f, nil, nil)
	if !strings.Contains(got, "Platform: Cilium CNI · Traefik ingress · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud") {
		t.Errorf("prompt missing platform line:\n%s", got)
	}
}

func TestBuildInventoryPrompt_OmitsPlatformWhenNil(t *testing.T) {
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 1, NodesReady: 0, NodeIssues: []string{"n1 NotReady"}}, nil, nil, nil, nil)
	if strings.Contains(got, "Platform:") {
		t.Errorf("no platform line expected when facts nil:\n%s", got)
	}
}

func TestBuildInventoryPrompt_IncludesServiceIssues(t *testing.T) {
	issues := []svchealth.Issue{
		{Namespace: "default", Name: "web", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"},
	}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, nil, issues, nil)
	if !strings.Contains(got, "Service issues:") || !strings.Contains(got, "default/web (ClusterIP): no ready endpoints") {
		t.Errorf("prompt missing service issues:\n%s", got)
	}
	if strings.Contains(got, "NoEndpoints") {
		t.Errorf("internal Problem code must not appear in the prompt:\n%s", got)
	}
}

func TestExplainInventory_ExplainsWhenOnlyServiceIssues(t *testing.T) {
	f := &fakeSummarizer{reply: "web has no endpoints"}
	c := &Client{s: f}
	issues := []svchealth.Issue{{Namespace: "default", Name: "web", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"}}
	got, err := c.ExplainInventory(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, issues, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "web has no endpoints" || !f.called {
		t.Errorf("expected service-only issues to be explained; got %q called=%v", got, f.called)
	}
}

func TestBuildInventoryPrompt_IncludesNetworkPolicyHint(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "default", Name: "api", Kind: "Deployment", Ready: 0, Desired: 2, Status: "Degraded",
		NetworkPolicies: []string{"deny-all", "web-allow"},
		Pods:            []inventory.PodRow{{Name: "api-1", IP: "10.42.9.9"}},
	}}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, nil, nil, ws)
	if !strings.Contains(got, "network policy: pods selected by deny-all, web-allow (possible cause)") {
		t.Errorf("prompt missing NP hint (comma-joined):\n%s", got)
	}
	// Egress guard: the NP hint path must not leak pod IPs.
	if strings.Contains(got, "10.42.9.9") {
		t.Errorf("pod IP leaked into prompt:\n%s", got)
	}
}

func TestSystemPrompt_HasStructureAndGuardrail(t *testing.T) {
	for _, want := range []string{"Root cause", "Check", "Fix", "P1", "P2", "ONLY the facts", "do not invent"} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("systemPrompt missing %q", want)
		}
	}
}

func TestBuildInventoryPrompt_LabelsPriority(t *testing.T) {
	ch := clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 2, NodesReady: 1, NodeIssues: []string{"n2 NotReady"}}
	ws := []inventory.Workload{{Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 1, Status: "Degraded"}}
	got := buildInventoryPrompt(ch, nil, nil, nil, ws)
	if !strings.Contains(got, "(P1)") {
		t.Errorf("degraded cluster block should be labeled P1:\n%s", got)
	}
	if !strings.Contains(got, "Workload problems (P2):") {
		t.Errorf("workload block should be labeled P2:\n%s", got)
	}
}

func TestBuildInventoryPrompt_IncludesRolloutChange(t *testing.T) {
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	ws := []inventory.Workload{{Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 1, Status: "Degraded",
		Rollout: &inventory.RolloutChange{Revision: "6", Since: "4d ago", OldImage: "nginx:1.27", NewImage: "nginx:bad"}}}
	got := buildInventoryPrompt(ch, nil, nil, nil, ws)
	if !strings.Contains(got, "recent change: rolled out to revision 6 4d ago") {
		t.Errorf("prompt missing rollout change:\n%s", got)
	}
	if !strings.Contains(got, "image nginx:1.27 → nginx:bad") {
		t.Errorf("prompt missing image delta:\n%s", got)
	}
}

func TestBuildInventoryPrompt_IncludesLogCauseNotExcerpt(t *testing.T) {
	workloads := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0,
		Findings: []diagnose.Finding{{
			Pod: "shop/web-1", Issue: "CrashLoopBackOff", Reason: "keeps crashing", Evidence: "restartCount=8",
			LogCause: "application panic (code bug)", LogExcerpt: "panic: SECRET_TOKEN=abc123",
		}},
	}}
	prompt := buildInventoryPrompt(clusterhealth.ClusterHealth{Verdict: "Degraded"}, nil, nil, nil, workloads)
	if !strings.Contains(prompt, "application panic (code bug)") {
		t.Errorf("prompt should include the derived LogCause:\n%s", prompt)
	}
	if strings.Contains(prompt, "SECRET_TOKEN") || strings.Contains(prompt, "panic: SECRET_TOKEN") {
		t.Errorf("prompt must NOT include the raw LogExcerpt:\n%s", prompt)
	}
}

func TestBuildInventoryPrompt_InjectsDeterministicSuggestion(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 2, Status: "Degraded",
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "CrashLoopBackOff", Reason: "crashes", Evidence: "restartCount=8", Container: "web"}},
	}}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, nil, nil, ws)
	if !strings.Contains(got, "suggested fix (deterministic, pre-reviewed — do not substitute):") {
		t.Errorf("prompt missing the deterministic suggestion line:\n%s", got)
	}
	// The exact remediation.For command for a CrashLoopBackOff finding.
	if !strings.Contains(got, "kubectl -n shop logs web-abc -c web --previous") {
		t.Errorf("prompt missing the exact remediation.For command:\n%s", got)
	}
}

func TestSystemPrompt_RanksAndGrounds(t *testing.T) {
	if !strings.Contains(systemPrompt, "Fix first:") {
		t.Error("systemPrompt must instruct a leading Fix first ranked list")
	}
	if !strings.Contains(systemPrompt, "verbatim") || !strings.Contains(systemPrompt, "never substitute or invent") {
		t.Error("systemPrompt must ground the Fix on the deterministic command (verbatim / never substitute or invent)")
	}
}

func TestResolveModel(t *testing.T) {
	cases := []struct {
		name, flag, env, want string
	}{
		{"flag wins over env and default", "claude-opus-4-8", "claude-sonnet-4-6", "claude-opus-4-8"},
		{"env used when flag empty", "", "claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"default when both empty", "", "", DefaultModel},
		{"flag wins when env empty", "claude-haiku-4-5", "", "claude-haiku-4-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveModel(tc.flag, tc.env); got != tc.want {
				t.Errorf("ResolveModel(%q, %q) = %q, want %q", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}
