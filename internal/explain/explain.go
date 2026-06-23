// Package explain turns deterministic findings into a plain-English summary
// via a single Claude API call. It is opt-in: nothing here runs unless the
// caller asks for an explanation, so the core tool stays usable offline.
package explain

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
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

// notableRestartThreshold: a healthy workload with at least this many total
// restarts is still worth explaining.
const notableRestartThreshold = 5

// Notable selects the workloads worth sending to the model: those flagged
// (finding or not fully ready) or with a high restart count.
func Notable(workloads []inventory.Workload) []inventory.Workload {
	var out []inventory.Workload
	for _, w := range workloads {
		if w.Flagged() || w.Restarts >= notableRestartThreshold {
			out = append(out, w)
		}
	}
	return out
}

// ExplainInventory summarizes the cluster verdict (when degraded) and the
// notable workloads. It skips the API call and returns "" when the cluster is
// healthy and nothing is notable.
func (c *Client) ExplainInventory(ctx context.Context, cluster clusterhealth.ClusterHealth, workloads []inventory.Workload) (string, error) {
	notable := Notable(workloads)
	if cluster.Verdict != "Degraded" && len(notable) == 0 {
		return "", nil
	}
	out, err := c.s.summarize(ctx, buildInventoryPrompt(cluster, notable))
	if err != nil {
		return "", fmt.Errorf("explaining workloads: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("explaining workloads: model returned no text")
	}
	return out, nil
}

// buildInventoryPrompt renders the cluster verdict (when degraded) and the
// notable workloads. Only structured fields are sent — never raw pod specs or
// secrets (node names in the cluster section are infrastructure identifiers).
func buildInventoryPrompt(cluster clusterhealth.ClusterHealth, workloads []inventory.Workload) string {
	var b strings.Builder
	if cluster.Verdict == "Degraded" {
		fmt.Fprintf(&b, "Cluster health: DEGRADED — %d/%d nodes Ready.\n", cluster.NodesReady, cluster.NodesTotal)
		for _, iss := range cluster.NodeIssues {
			fmt.Fprintf(&b, "  node %s\n", iss)
		}
		for _, iss := range cluster.SystemIssues {
			fmt.Fprintf(&b, "  system %s\n", iss)
		}
		b.WriteString("\n")
	}
	if len(workloads) > 0 {
		b.WriteString("These Kubernetes workloads need attention:\n\n")
		for _, w := range workloads {
			fmt.Fprintf(&b, "- %s/%s (%s): %d/%d ready, status %s, %d restarts\n",
				w.Namespace, w.Name, w.Kind, w.Ready, w.Desired, w.Status, w.Restarts)
			for _, f := range w.Findings {
				fmt.Fprintf(&b, "    issue: %s — %s (%s)\n", f.Issue, f.Reason, f.Evidence)
			}
		}
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
