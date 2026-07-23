package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
