package investigate

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

func TestInvestigate_RunsLoopAndReturnsReport(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	client := fake.NewSimpleClientset(pod)

	// Inject a fake conversation: one describe call, then a conclusion.
	c := &Client{newConversation: func(system, firstUser string, specs []toolSpec) conversation {
		if !strings.Contains(system, "read-only tools") {
			t.Error("system prompt should carry the investigation instruction")
		}
		if len(specs) != 3 {
			t.Errorf("expected 3 tool specs, got %d", len(specs))
		}
		return &fakeConv{t: t, replies: []reply{
			{Calls: []toolCall{mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "web-abc"})}},
			{Text: "root cause: image pull", Done: true},
		}}
	}}

	wl := []inventory.Workload{{
		Kind: "Deployment", Namespace: "shop", Name: "web",
		Pods:     []inventory.PodRow{{Name: "web-abc"}},
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "ImagePullBackOff", Reason: "bad tag", Evidence: "ErrImagePull"}},
	}}
	rep, err := c.Investigate(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, wl, client)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Narrative != "root cause: image pull" {
		t.Errorf("narrative = %q", rep.Narrative)
	}
	if len(rep.Consulted) != 1 || !strings.Contains(rep.Consulted[0], "describe pod shop/web-abc") {
		t.Errorf("consulted = %v", rep.Consulted)
	}
}

func TestInvestigate_SkipsWhenNothingToDo(t *testing.T) {
	called := false
	c := &Client{newConversation: func(string, string, []toolSpec) conversation {
		called = true
		return &fakeConv{t: t}
	}}
	rep, err := c.Investigate(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, nil, fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("must not open a conversation when there is nothing to investigate")
	}
	if rep.Narrative != "" || len(rep.Consulted) != 0 {
		t.Errorf("expected an empty report, got %+v", rep)
	}
}

// TestInvestigate_RunsWhenOnlyServiceIssues proves the serviceIssues arm of the
// skip condition: a healthy cluster with no workload findings but a non-empty
// serviceIssues slice must still open a conversation (not skip).
func TestInvestigate_RunsWhenOnlyServiceIssues(t *testing.T) {
	opened := false
	c := &Client{newConversation: func(string, string, []toolSpec) conversation {
		opened = true
		return &fakeConv{t: t, replies: []reply{
			{Text: "svc root cause", Done: true},
		}}
	}}
	issues := []svchealth.Issue{{
		Namespace: "shop",
		Name:      "frontend",
		Type:      "ClusterIP",
		Problem:   "NoEndpoints",
		Detail:    "no ready endpoints",
	}}
	rep, err := c.Investigate(
		context.Background(),
		clusterhealth.ClusterHealth{Verdict: "Healthy"},
		nil, nil,
		issues,
		nil, // no workloads
		fake.NewSimpleClientset(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !opened {
		t.Error("Investigate must open a conversation when serviceIssues is non-empty")
	}
	if rep.Narrative != "svc root cause" {
		t.Errorf("narrative = %q, want %q", rep.Narrative, "svc root cause")
	}
}
