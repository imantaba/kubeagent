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
	"github.com/imantaba/kubeagent/internal/explain"
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
		if len(specs) != 4 {
			t.Errorf("expected 4 tool specs, got %d", len(specs))
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

// TestInvestigate_ReportsTruncated proves R225(A) end to end through
// Investigate: when the model's final reply was cut short at its own
// output-length ceiling, Report.Truncated carries that fact out.
func TestInvestigate_ReportsTruncated(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	client := fake.NewSimpleClientset(pod)

	c := &Client{newConversation: func(system, firstUser string, specs []toolSpec) conversation {
		return &fakeConv{t: t, replies: []reply{
			{Text: "root cause: cut off", Done: true, Truncated: true},
		}}
	}}

	wl := []inventory.Workload{{
		Kind: "Deployment", Namespace: "shop", Name: "web",
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "ImagePullBackOff", Reason: "bad tag", Evidence: "ErrImagePull"}},
	}}
	rep, err := c.Investigate(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, wl, client)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Truncated {
		t.Error("expected Report.Truncated to be true when the final reply was cut short")
	}
}

// TestInvestigate_SkipsWhenNothingToDo's zero Report already covers the
// negative case for Truncated (it is the Go zero value, false); see also
// TestInvestigate_RunsLoopAndReturnsReport, whose scripted final reply does
// not set Truncated and so must report false.
func TestInvestigate_DoesNotReportTruncatedOnCleanFinish(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	client := fake.NewSimpleClientset(pod)

	c := &Client{newConversation: func(system, firstUser string, specs []toolSpec) conversation {
		return &fakeConv{t: t, replies: []reply{
			{Text: "root cause: image pull", Done: true},
		}}
	}}

	wl := []inventory.Workload{{
		Kind: "Deployment", Namespace: "shop", Name: "web",
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "ImagePullBackOff", Reason: "bad tag", Evidence: "ErrImagePull"}},
	}}
	rep, err := c.Investigate(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, wl, client)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Truncated {
		t.Error("a clean finish must not report Truncated")
	}
}

// TestInvestigateSuffix_HasFixedBudgetInstruction proves R225(C): the
// --investigate-only suffix constrains the narrative to a fixed budget,
// ranked by severity, with an honest count of what was not examined. It must
// not land in explain.SystemPrompt, which --explain also uses.
func TestInvestigateSuffix_HasFixedBudgetInstruction(t *testing.T) {
	for _, want := range []string{"fixed budget", "rank", "severity", "how many findings you did not examine"} {
		if !strings.Contains(investigateSuffix, want) {
			t.Errorf("investigateSuffix missing %q", want)
		}
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

// TestInvestigate_PrimesFirstUserWithTrace proves the spec's trace-primed
// opening: the first user message carries the deterministic hypothesis trace
// after the inventory prompt and before the closing instruction.
func TestInvestigate_PrimesFirstUserWithTrace(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	client := fake.NewSimpleClientset(pod)

	var gotFirstUser string
	c := &Client{newConversation: func(system, firstUser string, specs []toolSpec) conversation {
		gotFirstUser = firstUser
		return &fakeConv{t: t, replies: []reply{{Text: "done", Done: true}}}
	}}

	wl := []inventory.Workload{{
		Kind: "Deployment", Namespace: "shop", Name: "web",
		Pods:     []inventory.PodRow{{Name: "web-abc"}},
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "Pending", Reason: "node down", Evidence: "NotReady"}},
		RootCauseTrace: []inventory.Hypothesis{
			{Cause: "node worker-3 (NotReady)", Kind: "node", Verdict: inventory.VerdictAttributed, Reason: "pod web-abc is scheduled on it"},
		},
		RootCauseConfidence: "high",
	}}
	if _, err := c.Investigate(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, wl, client); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotFirstUser, "considered node worker-3 (NotReady): attributed — pod web-abc is scheduled on it") {
		t.Errorf("first user message missing the hypothesis trace:\n%s", gotFirstUser)
	}
	if !strings.HasSuffix(gotFirstUser, "Investigate the findings with the read-only tools, then explain.") {
		t.Errorf("first user message must still end with the investigate instruction:\n%s", gotFirstUser)
	}
}

// TestInvestigate_FirstUserUnchangedWithoutTrace pins the no-trace case to the
// pre-slice bytes: priming must cost nothing when there is nothing to prime.
func TestInvestigate_FirstUserUnchangedWithoutTrace(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	client := fake.NewSimpleClientset(pod)

	var gotFirstUser string
	c := &Client{newConversation: func(system, firstUser string, specs []toolSpec) conversation {
		gotFirstUser = firstUser
		return &fakeConv{t: t, replies: []reply{{Text: "done", Done: true}}}
	}}

	wl := []inventory.Workload{{
		Kind: "Deployment", Namespace: "shop", Name: "web",
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "ImagePullBackOff", Reason: "bad tag", Evidence: "ErrImagePull"}},
	}}
	if _, err := c.Investigate(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, wl, client); err != nil {
		t.Fatal(err)
	}
	want := explain.BuildInventoryPrompt(clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, wl) +
		"\n\nInvestigate the findings with the read-only tools, then explain."
	if gotFirstUser != want {
		t.Errorf("a traceless run's first message must be byte-identical to the pre-slice shape:\n%s", gotFirstUser)
	}
}
