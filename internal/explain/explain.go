// Package explain turns deterministic findings into a plain-English summary
// via a single Claude API call. It is opt-in: nothing here runs unless the
// caller asks for an explanation, so the core tool stays usable offline.
package explain

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/remediation"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

const systemPrompt = `You are a senior Kubernetes SRE reviewing a read-only cluster scan. Explain what
is wrong and exactly how to fix it, using ONLY the facts provided — do not invent
causes, resources, or values that are not given.

Begin your response with a "Fix first:" section — a numbered list ranking the
issues in the order they should be remediated (most blocking / highest-impact
first; cluster / kube-system P1 issues before workload P2 issues), each line
"N. <namespace/name>: <one-phrase action>". Then give the per-issue detail below.

Address issues in priority order: cluster / kube-system problems (P1) before
workload problems (P2). For EACH issue use this structure:

**<namespace/name> — <the issue>**
- Root cause: one line, from the facts. If the facts are ambiguous, name the most
  likely cause AND what to check — never present a guess as certain.
- Check: 1–3 read-only commands to confirm (kubectl get/describe/logs).
- Fix: use the provided deterministic, pre-reviewed command for this issue
  verbatim — you may add a namespace or flag already shown, sequence multiple
  provided commands, and phrase it for on-call, but never substitute or invent a
  different command. When the provided command is a generic describe, keep it and
  say what to look for in the output.

Be tight — no preamble, no restating the input, no generic advice. If a finding
is expected (e.g. a scaled-to-zero workload), say it needs no action. Prefer
"likely"/"check" over false certainty.`

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

// New returns a Client backed by the Anthropic API (empty model falls back to
// DefaultModel). The SDK reads ANTHROPIC_API_KEY.
func New(model string) *Client {
	return NewFromConfig(model, "", "")
}

// NewFromConfig returns a Client using the local OpenAI-compatible endpoint when
// endpoint is non-empty, otherwise the Anthropic backend. apiKey is the optional
// bearer token for the local endpoint (ignored by the Anthropic path).
func NewFromConfig(model, endpoint, apiKey string) *Client {
	if endpoint != "" {
		return &Client{s: openaiSummarizer{endpoint: endpoint, model: model, apiKey: apiKey, http: http.DefaultClient}}
	}
	if model == "" {
		model = DefaultModel
	}
	return &Client{s: anthropicSummarizer{client: anthropic.NewClient(), model: model}}
}

// ExplainInventory summarizes the cluster verdict (when degraded) and the given
// (already-prioritized) workloads. It skips the API call and returns "" when the
// cluster is healthy and there are no workloads or service issues to explain.
func (c *Client) ExplainInventory(ctx context.Context, cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload) (string, error) {
	if cluster.Verdict != "Degraded" && len(workloads) == 0 && len(serviceIssues) == 0 {
		return "", nil
	}
	out, err := c.s.summarize(ctx, buildInventoryPrompt(cluster, summary, facts, serviceIssues, workloads))
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
// given (pre-filtered) workloads. Only structured fields are sent — never raw pod specs or
// secrets (node names in the cluster section are infrastructure identifiers).
func buildInventoryPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload) string {
	var b strings.Builder
	if cluster.Verdict == "Degraded" {
		fmt.Fprintf(&b, "Cluster health (P1): DEGRADED — %d/%d nodes Ready.\n", cluster.NodesReady, cluster.NodesTotal)
		for _, iss := range cluster.NodeIssues {
			fmt.Fprintf(&b, "  node %s\n", iss)
		}
		for _, iss := range cluster.SystemIssues {
			fmt.Fprintf(&b, "  system %s\n", iss)
		}
		b.WriteString("\n")
	}

	if facts != nil {
		if line := facts.Line(); line != "" {
			fmt.Fprintf(&b, "Platform: %s\n\n", line)
		}
	}

	if summary != nil {
		b.WriteString("Cluster resources:\n")
		writeResLine(&b, "CPU", summary.CPU, "cores", summary.MetricsAvailable)
		writeResLine(&b, "Memory", summary.Memory, "", summary.MetricsAvailable)
		b.WriteString("\n")
	}

	if len(workloads) > 0 {
		b.WriteString("Workload problems (P2):\n\n")
		for _, w := range workloads {
			fmt.Fprintf(&b, "- %s/%s (%s): %d/%d ready, status %s, %d restarts\n",
				w.Namespace, w.Name, w.Kind, w.Ready, w.Desired, w.Status, w.Restarts)
			for _, f := range w.Findings {
				fmt.Fprintf(&b, "    issue: %s — %s (%s)\n", f.Issue, f.Reason, f.Evidence)
				if f.LogCause != "" {
					fmt.Fprintf(&b, "      log cause: %s\n", f.LogCause)
				}
				if f.Resources != nil {
					r := f.Resources
					fmt.Fprintf(&b, "      container resources: memory req=%s limit=%s, cpu req=%s limit=%s\n",
						r.MemRequest, r.MemLimit, r.CPURequest, r.CPULimit)
				}
				s := remediation.For(f)
				fmt.Fprintf(&b, "      suggested fix (deterministic, pre-reviewed — do not substitute): %s | run: %s\n", s.NextStep, s.Command)
			}
			if len(w.NetworkPolicies) > 0 {
				fmt.Fprintf(&b, "    network policy: pods selected by %s (possible cause)\n", strings.Join(w.NetworkPolicies, ", "))
			}
			if w.Rollout != nil {
				fmt.Fprintf(&b, "    recent change: rolled out to revision %s %s", w.Rollout.Revision, w.Rollout.Since)
				if w.Rollout.NewImage != "" {
					fmt.Fprintf(&b, ", image %s → %s", w.Rollout.OldImage, w.Rollout.NewImage)
				}
				b.WriteString("\n")
			}
		}
	}
	if len(serviceIssues) > 0 {
		b.WriteString("Service issues:\n")
		for _, is := range serviceIssues {
			fmt.Fprintf(&b, "  - %s/%s (%s): %s\n", is.Namespace, is.Name, is.Type, is.Detail)
		}
		b.WriteString("\n")
	}

	b.WriteString("\nExplain each problem and its fix using the required structure.")
	return b.String()
}

func writeResLine(b *strings.Builder, label string, l resources.Line, unit string, metrics bool) {
	alloc := l.Allocatable
	if unit != "" {
		alloc += " " + unit
	}
	fmt.Fprintf(b, "  %s: allocatable %s, requests %s (%d%%), limits %s (%d%%)",
		label, alloc, l.Requests, l.RequestsPct, l.Limits, l.LimitsPct)
	if metrics {
		fmt.Fprintf(b, ", usage %s (%d%%)", l.Usage, l.UsagePct)
	}
	b.WriteString("\n")
}

// anthropicSummarizer is the real summarizer, backed by the Anthropic SDK.
type anthropicSummarizer struct {
	client anthropic.Client
	model  string
}

func (a anthropicSummarizer) summarize(ctx context.Context, prompt string) (string, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 2048,
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
