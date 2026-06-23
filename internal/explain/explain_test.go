package explain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
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

func TestNotable_SelectsFlaggedAndHighRestarts(t *testing.T) {
	ws := []inventory.Workload{
		{Name: "healthy", Ready: 3, Desired: 3, Restarts: 0},
		{Name: "degraded", Ready: 1, Desired: 2},
		{Name: "restarted", Ready: 3, Desired: 3, Restarts: 64},
		{Name: "withfinding", Ready: 1, Desired: 1, Findings: []diagnose.Finding{{Pod: "a/b", Issue: "OOMKilled"}}},
		{Name: "quiet", Ready: 1, Desired: 1, Restarts: 2},
	}
	got := Notable(ws)
	names := map[string]bool{}
	for _, w := range got {
		names[w.Name] = true
	}
	if names["healthy"] || names["quiet"] {
		t.Errorf("healthy/quiet should not be notable: %v", names)
	}
	if !names["degraded"] || !names["restarted"] || !names["withfinding"] {
		t.Errorf("expected degraded, restarted, withfinding; got %v", names)
	}
}

func TestExplainInventory_SkipsWhenNothingNotable(t *testing.T) {
	f := &fakeSummarizer{reply: "should not be used"}
	c := &Client{s: f}
	got, err := c.ExplainInventory(context.Background(), []inventory.Workload{{Name: "ok", Ready: 1, Desired: 1}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" || f.called {
		t.Errorf("expected no call and empty result; got %q called=%v", got, f.called)
	}
}

func TestExplainInventory_SummarizesNotable(t *testing.T) {
	f := &fakeSummarizer{reply: "  coredns is degraded.  "}
	c := &Client{s: f}
	ws := []inventory.Workload{{Namespace: "kube-system", Name: "coredns", Kind: "Deployment", Ready: 1, Desired: 2}}
	got, err := c.ExplainInventory(context.Background(), ws)
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
	_, err := c.ExplainInventory(context.Background(), []inventory.Workload{{Name: "x", Ready: 1, Desired: 2}})
	if err == nil || !strings.Contains(err.Error(), "explaining workloads") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

func TestExplainInventory_ErrorsOnEmptyText(t *testing.T) {
	f := &fakeSummarizer{reply: "  \n"}
	c := &Client{s: f}
	_, err := c.ExplainInventory(context.Background(), []inventory.Workload{{Name: "x", Ready: 1, Desired: 2}})
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
	got := buildInventoryPrompt(ws)
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
