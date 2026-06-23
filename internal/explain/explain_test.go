package explain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
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

func TestBuildPrompt_IncludesEveryFindingField(t *testing.T) {
	findings := []diagnose.Finding{
		{Pod: "default/web", Issue: "CrashLoopBackOff", Reason: "exits 1 on boot", Evidence: "restartCount=14"},
	}
	got := buildPrompt(findings)
	for _, want := range []string{"default/web", "CrashLoopBackOff", "exits 1 on boot", "restartCount=14", "next steps"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestExplain_SkipsCallWhenNoFindings(t *testing.T) {
	f := &fakeSummarizer{reply: "should not be used"}
	c := &Client{s: f}
	got, err := c.Explain(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty explanation, got %q", got)
	}
	if f.called {
		t.Error("summarizer should not be called when there are no findings")
	}
}

func TestExplain_ReturnsTrimmedSummary(t *testing.T) {
	f := &fakeSummarizer{reply: "  Two pods are failing.  \n"}
	c := &Client{s: f}
	got, err := c.Explain(context.Background(), []diagnose.Finding{{Pod: "default/web", Issue: "X"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Two pods are failing." {
		t.Errorf("got %q, want the trimmed summary", got)
	}
	if !f.called {
		t.Error("expected the summarizer to be called")
	}
}

func TestExplain_WrapsSummarizerError(t *testing.T) {
	f := &fakeSummarizer{err: errors.New("boom")}
	c := &Client{s: f}
	_, err := c.Explain(context.Background(), []diagnose.Finding{{Pod: "default/web"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "explaining findings") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error not wrapped as expected: %v", err)
	}
}

func TestExplain_ErrorsOnEmptySummary(t *testing.T) {
	f := &fakeSummarizer{reply: "   \n"}
	c := &Client{s: f}
	_, err := c.Explain(context.Background(), []diagnose.Finding{{Pod: "default/web"}})
	if err == nil {
		t.Fatal("expected an error when the model returns no text")
	}
	if !strings.Contains(err.Error(), "explaining findings") {
		t.Errorf("error not wrapped as expected: %v", err)
	}
	if !f.called {
		t.Error("expected the summarizer to be called when findings are present")
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
