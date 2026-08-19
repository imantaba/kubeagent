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
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/remediation"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

const SystemPrompt = `You are a senior Kubernetes SRE reviewing a read-only cluster scan. Explain what
is wrong and exactly how to fix it, using ONLY the facts provided — do not invent
causes, resources, or values that are not given.

Respond in plain text only: no markdown emphasis, no headings, no horizontal
rules, no fenced code blocks.

Begin your response with a "Fix first:" section — a numbered list ranking the
issues in the order they should be remediated (most blocking / highest-impact
first; cluster / kube-system P1 issues before workload P2 issues), each line
"N. <namespace/name>: <one-phrase action>". Then give the per-issue detail below.

Address issues in priority order: cluster / kube-system problems (P1) before
workload problems (P2). For EACH issue use this structure — a bare header line,
then two-space indented lines beneath it:

<namespace/name> — <the issue>
  Root cause: one line, from the facts. If the facts are ambiguous, name the most
  likely cause AND what to check — never present a guess as certain.
  Check: 1–3 read-only commands to confirm (kubectl get/describe/logs).
  Fix: use the provided deterministic, pre-reviewed command for this issue
  verbatim — you may add a namespace or flag already shown, sequence multiple
  provided commands, and phrase it for on-call, but never substitute or invent a
  different command. A Fix line may contain only a read-only command, or the
  exact command supplied with the finding. When the provided command is a
  generic describe, keep it and say what to look for in the output.

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

// Explanation is a summarizer call's result: the narrative text plus whether
// it was cut short at the model's own output-length ceiling rather than
// because the model chose to stop. It is the same shape
// investigate.Report already carries (Narrative/Truncated there, Text/
// Truncated here).
type Explanation struct {
	Text      string
	Truncated bool
}

// summarizer turns a system prompt plus a user prompt into a single plain-text
// completion. The Anthropic-backed implementation lives in this package; tests
// use a fake. The system prompt is a parameter rather than a constant because
// a one-object incident follow-up and a whole-cluster scan summary want
// different instructions.
type summarizer interface {
	summarize(ctx context.Context, system, prompt string) (string, error)
}

// toExplanation turns an *anthropic.Message into an Explanation: the
// concatenated text blocks, plus whether the reply was cut short at the
// model's own output-length ceiling (StopReasonMaxTokens) rather than
// because the model chose to stop. Pure, no network — the seam
// anthropicSummarizer.summarize calls, and TestToExplanation_* exercises
// directly with a bare composite literal.
func toExplanation(resp *anthropic.Message) Explanation {
	var e Explanation
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			e.Text += tb.Text
		}
	}
	e.Truncated = resp.StopReason == anthropic.StopReasonMaxTokens
	return e
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
	out, err := c.s.summarize(ctx, SystemPrompt, BuildInventoryPrompt(cluster, summary, facts, serviceIssues, workloads))
	if err != nil {
		return "", fmt.Errorf("explaining workloads: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("explaining workloads: model returned no text")
	}
	return out, nil
}

// suggestionFor renders a finding's deterministic next step in the form a
// prompt may carry. remediation.For targets the pod the finding was diagnosed
// on, and a controller's pod name is generated per replica: it identifies one
// instance of a workload to anyone who reads it and explains nothing, which is
// the same reason pod rows are not rendered at all. The prompt therefore gets
// the command with the pod's name replaced by a placeholder — the namespace,
// the verb and the container survive, so the model can still reproduce a Fix
// line the operator can complete.
//
// A finding diagnosed on the object itself keeps its name: RolloutStuck sets
// Pod to the Deployment's own identity, which the prompt has already named as
// the object that broke. Both callers render inside a workload loop, so the
// comparison is against the workload the finding hangs off.
func suggestionFor(f diagnose.Finding, w inventory.Workload) remediation.Suggestion {
	if f.Pod != "" && f.Pod != w.Namespace+"/"+w.Name {
		if i := strings.IndexByte(f.Pod, '/'); i >= 0 {
			f.Pod = f.Pod[:i+1] + "<pod>"
		} else {
			f.Pod = "<pod>"
		}
	}
	return remediation.For(f)
}

// BuildInventoryPrompt renders the cluster verdict (when degraded) and the
// given (pre-filtered) workloads. Only structured fields are sent — never raw pod specs or
// secrets (node names in the cluster section are infrastructure identifiers).
func BuildInventoryPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload) string {
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
			writeFindingBlocks(&b, w)
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

// maxFindingBlocksPerWorkload caps how many finding blocks BuildInventoryPrompt
// writes per workload, after collapsing. It bounds what the request costs; the
// summarizer's own output cap bounds what the response may cost. The two act
// on opposite sides of the same API call and neither makes the other
// unnecessary — a prompt small enough to send can still ask for an answer
// longer than the model is allowed to write.
const maxFindingBlocksPerWorkload = 3

// writeFindingBlocks renders w.Findings as the prompt's per-finding blocks. It
// first collapses consecutive blocks that are byte-identical — after
// redaction and the <pod> substitution have already run — into one block with
// a "(×N)" count appended to its issue: line, then caps the number of blocks
// emitted to maxFindingBlocksPerWorkload, adding one "… and N more of the
// same kind" line for the rest.
//
// This converges with internal/report's own text-output collapse
// (groupFindings) without being the same rule: groupFindings keys on
// head+tail and deliberately excludes evidence, so it collapses on a
// narrower key than the byte-identical comparison here. A workload with
// exactly one finding takes neither path and renders unchanged.
func writeFindingBlocks(b *strings.Builder, w inventory.Workload) {
	type findingGroup struct {
		block string
		count int
	}
	var groups []findingGroup
	for _, f := range w.Findings {
		block := findingBlock(f, w)
		if n := len(groups); n > 0 && groups[n-1].block == block {
			groups[n-1].count++
			continue
		}
		groups = append(groups, findingGroup{block: block, count: 1})
	}
	shown := groups
	if len(groups) > maxFindingBlocksPerWorkload {
		shown = groups[:maxFindingBlocksPerWorkload]
	}
	for _, g := range shown {
		if g.count == 1 {
			b.WriteString(g.block)
			continue
		}
		// The count belongs on the block's first line (the issue: line).
		nl := strings.IndexByte(g.block, '\n')
		fmt.Fprintf(b, "%s (×%d)%s", g.block[:nl], g.count, g.block[nl:])
	}
	if more := len(groups) - len(shown); more > 0 {
		fmt.Fprintf(b, "    … and %d more of the same kind\n", more)
	}
}

// findingBlock renders one finding's prompt block — the issue: line, the
// optional log cause and container resources lines, and the suggested fix
// line. It is the unit writeFindingBlocks compares, collapses and caps.
func findingBlock(f diagnose.Finding, w inventory.Workload) string {
	var blk strings.Builder
	fmt.Fprintf(&blk, "    issue: %s — %s (%s)\n", f.Issue, f.Reason, f.Evidence)
	// LogCause is one of logscan's fixed classifier strings — every signature
	// discards its submatches — so it cannot carry an in-cluster address
	// today. This call is defence in depth at the boundary where a prompt
	// leaves the process: it holds even if a future signature interpolates
	// the line it matched.
	if f.LogCause != "" {
		fmt.Fprintf(&blk, "      log cause: %s\n", redact.Addresses(f.LogCause))
	}
	if f.Resources != nil {
		r := f.Resources
		fmt.Fprintf(&blk, "      container resources: memory req=%s limit=%s, cpu req=%s limit=%s\n",
			r.MemRequest, r.MemLimit, r.CPURequest, r.CPULimit)
	}
	s := suggestionFor(f, w)
	fmt.Fprintf(&blk, "      suggested fix (deterministic, pre-reviewed — do not substitute): %s | run: %s\n", s.NextStep, s.Command)
	return blk.String()
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

func (a anthropicSummarizer) summarize(ctx context.Context, system, prompt string) (string, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model: anthropic.Model(a.model),
		// The hard ceiling on the narrative, not a target. 2048 cut a
		// many-workload summary off mid-sentence; 8192 matches what
		// internal/investigate asks for on the same model.
		MaxTokens: 8192,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", err
	}
	return toExplanation(resp).Text, nil
}
