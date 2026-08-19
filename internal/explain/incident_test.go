package explain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

func incidentWorkloads() []inventory.Workload {
	return []inventory.Workload{
		{
			Namespace: "shop", Name: "web", Kind: "Deployment",
			Desired: 3, Ready: 0, Status: "Degraded", Restarts: 4,
			// A real finding always names the pod it was diagnosed on — that is
			// what the report's kubectl command targets. The fixture carries it
			// so the egress guard below tests the builder as it runs in
			// production rather than a pod-less shape it never sees.
			Findings: []diagnose.Finding{{
				Pod:   "shop/web-7d9f-abcde",
				Issue: "ImagePullBackOff", Reason: "tag not found", Evidence: "manifest unknown",
			}},
			RootCause: "registry ghcr.example (12 workloads failing to pull)",
			Pods: []inventory.PodRow{{
				Name: "web-7d9f-abcde", Phase: "Pending", Ready: "0/1",
				Node: "worker-2", IP: "10.244.3.17", Image: "ghcr.example/web:missing",
			}},
		},
		{
			Namespace: "shop", Name: "cart", Kind: "Deployment",
			Desired: 2, Ready: 1, Status: "Degraded",
			Findings: []diagnose.Finding{{
				Pod: "shop/cart-6b8d94f7c5-q2xzt", Container: "cart",
				Issue: "CrashLoopBackOff", Reason: "exit 1", Evidence: "restarts 9",
			}},
		},
	}
}

func TestBuildIncidentPromptNamesTheTargetObject(t *testing.T) {
	p := BuildIncidentPrompt("Deployment/shop/web", []string{"ImagePullBackOff"},
		clusterhealth.ClusterHealth{Verdict: "Healthy"}, incidentWorkloads(), nil)
	if !strings.Contains(p, "Deployment/shop/web") {
		t.Errorf("prompt must name the object that broke:\n%s", p)
	}
	if !strings.Contains(p, "ImagePullBackOff") {
		t.Errorf("prompt must carry the triggering issues:\n%s", p)
	}
}

func TestBuildIncidentPromptCarriesClusterContext(t *testing.T) {
	p := BuildIncidentPrompt("Deployment/shop/web", []string{"ImagePullBackOff"},
		clusterhealth.ClusterHealth{Verdict: "Healthy"}, incidentWorkloads(), nil)
	if !strings.Contains(p, "shop/cart") {
		t.Errorf("prompt must include the other flagged workloads so the model can correlate:\n%s", p)
	}
	if !strings.Contains(p, "registry ghcr.example") {
		t.Errorf("prompt must include the root-cause attribution kubeagent already computed:\n%s", p)
	}
}

func TestBuildIncidentPromptIncludesDegradedClusterAndServiceIssues(t *testing.T) {
	cluster := clusterhealth.ClusterHealth{
		Verdict: "Degraded", NodesReady: 2, NodesTotal: 3,
		NodeIssues: []string{"worker-2 NotReady"},
	}
	svc := []svchealth.Issue{{
		Namespace: "shop", Name: "web", Type: "ClusterIP",
		Problem: "NoEndpoints", Detail: "0 ready endpoints",
	}}
	p := BuildIncidentPrompt("Deployment/shop/web", []string{"ImagePullBackOff"}, cluster, incidentWorkloads(), svc)
	if !strings.Contains(p, "DEGRADED") || !strings.Contains(p, "worker-2 NotReady") {
		t.Errorf("prompt must include the degraded cluster verdict:\n%s", p)
	}
	if !strings.Contains(p, "NoEndpoints") || !strings.Contains(p, "0 ready endpoints") {
		t.Errorf("prompt must include the service issue's problem and detail:\n%s", p)
	}
}

// The egress guard. Pod rows carry pod names, node names and pod IPs. None of
// that is needed to explain a failure, so none of it may leave the cluster. A
// positive-only test would pass just as happily if the builder started
// serializing whole pod specs.
//
// The pod name has a second way out that pod rows do not cover: every finding
// names the pod it was diagnosed on, and the deterministic kubectl command
// rendered from it targets that pod by name. Both fixtures' findings carry a
// pod, so this test fails if either route reopens.
func TestBuildIncidentPromptDoesNotLeakPodDetail(t *testing.T) {
	p := BuildIncidentPrompt("Deployment/shop/web", []string{"ImagePullBackOff"},
		clusterhealth.ClusterHealth{Verdict: "Healthy"}, incidentWorkloads(), nil)
	for _, forbidden := range []string{"10.244.3.17", "web-7d9f-abcde", "worker-2", "cart-6b8d94f7c5-q2xzt"} {
		if strings.Contains(p, forbidden) {
			t.Errorf("prompt leaked %q, which no explanation needs:\n%s", forbidden, p)
		}
	}
}

// The command still has to be worth pasting: the namespace, the verb and the
// container survive, and only the pod's generated name is replaced. A guard
// that let the whole command drop would pass the leak test above and quietly
// remove the Fix line's only concrete instruction.
func TestBuildIncidentPromptKeepsTheCommandUsable(t *testing.T) {
	p := BuildIncidentPrompt("Deployment/shop/web", []string{"ImagePullBackOff"},
		clusterhealth.ClusterHealth{Verdict: "Healthy"}, incidentWorkloads(), nil)
	for _, want := range []string{
		"kubectl -n shop describe pod <pod>",
		"kubectl -n shop logs <pod> -c cart --previous",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt must carry %q:\n%s", want, p)
		}
	}
}

// Not every finding is diagnosed on a pod: RolloutStuck names the workload
// itself, and that name is the object the alert fired for — already in the
// prompt's first line. Replacing it would cost the model the one command it is
// told to reproduce verbatim, for no privacy gain.
func TestBuildIncidentPromptKeepsTheObjectsOwnName(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment",
		Desired: 3, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{
			Pod:   "shop/web",
			Issue: "RolloutStuck", Reason: "rollout cannot complete", Evidence: "ProgressDeadlineExceeded",
		}},
	}}
	p := BuildIncidentPrompt("Deployment/shop/web", []string{"RolloutStuck"},
		clusterhealth.ClusterHealth{Verdict: "Healthy"}, ws, nil)
	if !strings.Contains(p, "--field-selector involvedObject.name=web") {
		t.Errorf("prompt must keep the command targeting the object itself:\n%s", p)
	}
}

type fakeIncidentSummarizer struct {
	system string
	prompt string
	out    string
	err    error
}

func (f *fakeIncidentSummarizer) summarize(_ context.Context, system, prompt string) (Explanation, error) {
	f.system, f.prompt = system, prompt
	return Explanation{Text: f.out}, f.err
}

func TestExplainIncidentUsesTheIncidentSystemPrompt(t *testing.T) {
	f := &fakeIncidentSummarizer{out: "  the registry tag is missing  "}
	c := &Client{s: f}
	got, err := c.ExplainIncident(context.Background(), "PROMPT")
	if err != nil {
		t.Fatalf("ExplainIncident: %v", err)
	}
	if got != "the registry tag is missing" {
		t.Errorf("output = %q, want it trimmed", got)
	}
	if f.system != IncidentSystemPrompt {
		t.Errorf("system prompt = %q, want IncidentSystemPrompt", f.system)
	}
	if f.prompt != "PROMPT" {
		t.Errorf("user prompt = %q, want %q", f.prompt, "PROMPT")
	}
}

func TestExplainIncidentRejectsEmptyOutput(t *testing.T) {
	c := &Client{s: &fakeIncidentSummarizer{out: "   \n  "}}
	if _, err := c.ExplainIncident(context.Background(), "PROMPT"); err == nil {
		t.Error("an empty explanation must be an error, not a delivered blank message")
	}
}

func TestExplainIncidentPropagatesModelErrors(t *testing.T) {
	c := &Client{s: &fakeIncidentSummarizer{err: errors.New("boom")}}
	if _, err := c.ExplainIncident(context.Background(), "PROMPT"); err == nil {
		t.Error("a model error must surface")
	}
}
