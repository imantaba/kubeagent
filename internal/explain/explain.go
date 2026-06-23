// Package explain turns deterministic findings into a plain-English summary
// via a single Claude API call. It is opt-in: nothing here runs unless the
// caller asks for an explanation, so the core tool stays usable offline.
package explain

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

const systemPrompt = `You are a Kubernetes SRE. You are given the findings of a
read-only cluster scan. Explain in plain English what is going wrong and suggest
concrete next steps an operator can take. Be concise. Respond with only the
explanation, no preamble.`

// DefaultModel is used when neither --model nor KUBEAGENT_MODEL is set.
const DefaultModel = "claude-opus-4-8"

// ResolveModel picks the model by precedence: flag, then env, then DefaultModel.
func ResolveModel(flagVal, envVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if envVal != "" {
		return envVal
	}
	return DefaultModel
}

// summarizer turns a prompt into a single plain-text completion. The
// Anthropic-backed implementation lives in this package; tests use a fake.
type summarizer interface {
	summarize(ctx context.Context, prompt string) (string, error)
}

// Client explains findings via one Claude API call.
type Client struct {
	s summarizer
}

// New returns a Client backed by the Anthropic API, using the given model
// (empty falls back to DefaultModel). The SDK reads ANTHROPIC_API_KEY.
func New(model string) *Client {
	if model == "" {
		model = DefaultModel
	}
	return &Client{s: anthropicSummarizer{client: anthropic.NewClient(), model: model}}
}

// Explain summarizes findings in plain English. With no findings it returns
// "" and makes no API call.
func (c *Client) Explain(ctx context.Context, findings []diagnose.Finding) (string, error) {
	if len(findings) == 0 {
		return "", nil
	}
	out, err := c.s.summarize(ctx, buildPrompt(findings))
	if err != nil {
		return "", fmt.Errorf("explaining findings: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("explaining findings: model returned no text")
	}
	return out, nil
}

// buildPrompt renders the findings into a compact prompt. Only the structured
// fields are sent — never raw pod specs or secrets.
func buildPrompt(findings []diagnose.Finding) string {
	var b strings.Builder
	b.WriteString("A read-only scan found these Kubernetes pod issues:\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "- pod %s: %s\n    reason: %s\n    evidence: %s\n", f.Pod, f.Issue, f.Reason, f.Evidence)
	}
	b.WriteString("\nExplain what is going wrong and suggest concrete next steps.")
	return b.String()
}

// anthropicSummarizer is the real summarizer, backed by the Anthropic SDK.
type anthropicSummarizer struct {
	client anthropic.Client
	model  string
}

func (a anthropicSummarizer) summarize(ctx context.Context, prompt string) (string, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 1024,
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(tb.Text)
		}
	}
	return out.String(), nil
}
