package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"k8s.io/client-go/kubernetes"
)

// Loop bounds (Global Constraints): no user-facing knobs in v1.
const (
	maxToolCalls = 8
	maxTurns     = 6
)

// reply is one model turn: text and/or tool-use requests. Done is true when the
// model finished its turn without requesting tools.
type reply struct {
	Text  string
	Calls []toolCall
	Done  bool
}

// conversation is a live tool-use session. Only the Anthropic backend implements
// it; tests use a fake. start opens the session; next feeds tool results and
// continues; conclude feeds the final results plus a "stop and answer" instruction
// with no tools offered, forcing a text answer.
type conversation interface {
	start(ctx context.Context) (reply, error)
	next(ctx context.Context, results []toolResult) (reply, error)
	conclude(ctx context.Context, results []toolResult) (reply, error)
}

// executor runs one tool call against the scope (satisfied by Reader).
type executor interface {
	execute(ctx context.Context, c toolCall, scope *Scope) toolResult
}

// runLoop drives the conversation until the model concludes or a bound is hit,
// returning the final narrative and the evidence trail of executed calls.
func runLoop(ctx context.Context, conv conversation, exec executor, scope *Scope) (string, []string, error) {
	rep, err := conv.start(ctx)
	if err != nil {
		return "", nil, err
	}
	var trail []string
	calls := 0
	for turn := 1; ; turn++ {
		if rep.Done || len(rep.Calls) == 0 {
			return strings.TrimSpace(rep.Text), trail, nil
		}
		var results []toolResult
		for _, c := range rep.Calls {
			if calls >= maxToolCalls {
				break
			}
			calls++
			trail = append(trail, label(c))
			results = append(results, exec.execute(ctx, c, scope))
		}
		if calls >= maxToolCalls || turn >= maxTurns {
			rep, err = conv.conclude(ctx, results)
			if err != nil {
				return "", nil, err
			}
			return strings.TrimSpace(rep.Text), trail, nil
		}
		rep, err = conv.next(ctx, results)
		if err != nil {
			return "", nil, err
		}
	}
}

// label renders a tool call for the evidence trail, e.g. "describe pod shop/web-abc".
func label(c toolCall) string {
	var m map[string]string
	_ = json.Unmarshal(c.Input, &m)
	switch c.Name {
	case "describe":
		return fmt.Sprintf("describe %s %s/%s", m["kind"], m["namespace"], m["name"])
	case "get_events":
		return fmt.Sprintf("events %s/%s", m["namespace"], m["name"])
	case "get_related":
		return fmt.Sprintf("related %s/%s→%s", m["namespace"], m["name"], m["relation"])
	default:
		return c.Name
	}
}

// investigateSuffix extends the shared explain system prompt with the tool-use
// instruction. The Fix-first structure lives in explain.SystemPrompt (one source).
const investigateSuffix = `

You may call the provided read-only tools to gather more evidence about a finding
before you conclude — describe an object, list its events, or resolve a related
object (owner, node, PVC). Investigate only what the findings point to. Use only
the facts you observe. When you have enough, stop calling tools and give the
explanation in the required structure.`

// Report is the investigation result for the report layer.
type Report struct {
	Consulted []string // evidence trail: the reads that were made
	Narrative string   // the Fix-first explanation, grounded in the evidence
}

// Client runs a bounded, read-only investigation via a tool-use loop.
type Client struct {
	// newConversation builds the model session; a field so tests inject a fake.
	newConversation func(system, firstUser string, specs []toolSpec) conversation
}

// New returns a Client backed by the Anthropic API (the SDK reads ANTHROPIC_API_KEY).
func New(model string) *Client {
	return &Client{
		newConversation: func(system, firstUser string, specs []toolSpec) conversation {
			return &anthropicConversation{
				client: anthropic.NewClient(),
				model:  model,
				system: system,
				tools:  toAnthropicTools(specs),
				msgs:   []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(firstUser))},
			}
		},
	}
}

// Investigate runs the scan-grounded tool-use loop and returns its report. It
// skips (empty report) when the cluster is healthy with no workload or service
// findings — there is nothing to investigate.
func (c *Client) Investigate(ctx context.Context, cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload, client kubernetes.Interface) (Report, error) {
	if cluster.Verdict != "Degraded" && len(workloads) == 0 && len(serviceIssues) == 0 {
		return Report{}, nil
	}
	system := explain.SystemPrompt + investigateSuffix
	firstUser := explain.BuildInventoryPrompt(cluster, summary, facts, serviceIssues, workloads) +
		"\n\nInvestigate the findings with the read-only tools, then explain."
	conv := c.newConversation(system, firstUser, toolSpecs())
	narrative, trail, err := runLoop(ctx, conv, Reader{client: client}, NewScope(workloads))
	if err != nil {
		return Report{}, fmt.Errorf("investigating: %w", err)
	}
	if narrative == "" {
		return Report{}, fmt.Errorf("investigating: model returned no text")
	}
	return Report{Consulted: trail, Narrative: narrative}, nil
}

// anthropicConversation is the real tool-use session, backed by the Anthropic SDK.
type anthropicConversation struct {
	client anthropic.Client
	model  string
	system string
	tools  []anthropic.ToolUnionParam
	msgs   []anthropic.MessageParam
}

func (a *anthropicConversation) start(ctx context.Context) (reply, error) {
	return a.roundtrip(ctx, a.tools)
}

func (a *anthropicConversation) next(ctx context.Context, results []toolResult) (reply, error) {
	a.msgs = append(a.msgs, anthropic.NewUserMessage(toolResultBlocks(results)...))
	return a.roundtrip(ctx, a.tools)
}

func (a *anthropicConversation) conclude(ctx context.Context, results []toolResult) (reply, error) {
	blocks := toolResultBlocks(results)
	blocks = append(blocks, anthropic.NewTextBlock(
		"Stop investigating now and give your final explanation using only what you have observed."))
	a.msgs = append(a.msgs, anthropic.NewUserMessage(blocks...))
	return a.roundtrip(ctx, nil) // no tools offered → the model must answer
}

func (a *anthropicConversation) roundtrip(ctx context.Context, tools []anthropic.ToolUnionParam) (reply, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 2048,
		System:    []anthropic.TextBlockParam{{Text: a.system}},
		Messages:  a.msgs,
		Tools:     tools,
	})
	if err != nil {
		return reply{}, err
	}
	a.msgs = append(a.msgs, resp.ToParam())
	return toReply(resp), nil
}

func toReply(resp *anthropic.Message) reply {
	var r reply
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			r.Text += b.Text
		case anthropic.ToolUseBlock:
			r.Calls = append(r.Calls, toolCall{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	r.Done = resp.StopReason != anthropic.StopReasonToolUse
	return r
}

func toolResultBlocks(results []toolResult) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, len(results))
	for i, res := range results {
		blocks[i] = anthropic.NewToolResultBlock(res.ID, res.Content, res.IsError)
	}
	return blocks
}

func toAnthropicTools(specs []toolSpec) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, len(specs))
	for i, s := range specs {
		t := anthropic.ToolUnionParamOfTool(
			anthropic.ToolInputSchemaParam{Properties: s.Properties, Required: s.Required}, s.Name)
		t.OfTool.Description = anthropic.String(s.Description)
		out[i] = t
	}
	return out
}
