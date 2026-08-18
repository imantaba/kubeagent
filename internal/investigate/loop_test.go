package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeConv scripts a fixed sequence of replies; the i-th call to start/next/
// concludes returns replies[i]. It records the tool results it was given.
type fakeConv struct {
	t         *testing.T
	replies   []reply
	i         int
	gotResult int
	concluded bool
	startErr  error
}

func (f *fakeConv) pop() reply {
	if f.i >= len(f.replies) {
		f.t.Fatalf("fakeConv: ran out of scripted replies at index %d", f.i)
	}
	r := f.replies[f.i]
	f.i++
	return r
}
func (f *fakeConv) start(ctx context.Context) (reply, error) {
	if f.startErr != nil {
		return reply{}, f.startErr
	}
	return f.pop(), nil
}
func (f *fakeConv) next(ctx context.Context, res []toolResult) (reply, error) {
	f.gotResult += len(res)
	return f.pop(), nil
}
func (f *fakeConv) conclude(ctx context.Context, res []toolResult) (reply, error) {
	f.concluded = true
	f.gotResult += len(res)
	return f.pop(), nil
}

// countingExec records how many calls it executed.
type countingExec struct{ n int }

func (e *countingExec) execute(ctx context.Context, c toolCall, s *Scope) toolResult {
	e.n++
	return okResult(c.ID, "observed")
}

func mkCall(name string, in map[string]string) toolCall {
	b, _ := json.Marshal(in)
	return toolCall{ID: name, Name: name, Input: b}
}

func TestRunLoop_GathersThenConcludes(t *testing.T) {
	conv := &fakeConv{t: t, replies: []reply{
		{Calls: []toolCall{mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "web-abc"})}},
		{Calls: []toolCall{mkCall("get_events", map[string]string{"namespace": "shop", "name": "web-abc"})}},
		{Text: "root cause: bad image", Done: true},
	}}
	exec := &countingExec{}
	narrative, trail, _, err := runLoop(context.Background(), conv, exec, NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if narrative != "root cause: bad image" {
		t.Errorf("narrative = %q", narrative)
	}
	if exec.n != 2 || len(trail) != 2 {
		t.Errorf("expected 2 executed calls and 2 trail entries, got %d/%d", exec.n, len(trail))
	}
	if !strings.Contains(trail[0], "describe") {
		t.Errorf("trail[0] = %q", trail[0])
	}
}

func TestRunLoop_CapsToolCallsAndConcludes(t *testing.T) {
	// Script more tool-call replies than either cap allows, so whichever bound
	// (maxToolCalls or maxTurns) fires first, the loop is forced to conclude.
	// With 1 call per reply, the turn cap (maxTurns=6) fires first; conclude()
	// is invoked after turn 6 so exec.n must equal maxTurns exactly.
	var reps []reply
	for i := 0; i < maxTurns; i++ {
		reps = append(reps, reply{Calls: []toolCall{mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "p"})}})
	}
	reps = append(reps, reply{Text: "final under cap", Done: true})
	conv := &fakeConv{t: t, replies: reps}
	exec := &countingExec{}
	s := NewScope(nil)
	s.Add("pod", "shop", "p")
	narrative, _, _, err := runLoop(context.Background(), conv, exec, s)
	if err != nil {
		t.Fatal(err)
	}
	if exec.n != maxTurns {
		t.Errorf("executed %d calls, want exactly maxTurns=%d (1 call/turn, turn cap fires first)", exec.n, maxTurns)
	}
	if !conv.concluded {
		t.Error("loop must call conclude when a cap is hit")
	}
	if narrative != "final under cap" {
		t.Errorf("narrative = %q", narrative)
	}
}

func TestRunLoop_StartErrorPropagates(t *testing.T) {
	conv := &fakeConv{t: t, startErr: errors.New("api down")}
	_, _, _, err := runLoop(context.Background(), conv, &countingExec{}, NewScope(nil))
	if err == nil {
		t.Error("expected the start error to propagate")
	}
}

// TestRunLoop_BudgetSlicesWithinATurn verifies that the inner-loop break at the
// per-call budget check is load-bearing: when a single reply returns more tool
// calls than the remaining budget, only the remaining calls execute (not all of
// them), and conclude is invoked immediately.
//
// Sequence:
//   - reply #1 (start): maxToolCalls-1 calls → executes all 7; budget = 1 remaining, turn < maxTurns → next()
//   - reply #2 (next):  3 calls → only 1 may execute (budget hits maxToolCalls=8); break mid-slice → conclude()
//   - reply #3 (conclude): {Text:"done", Done:true}
func TestRunLoop_BudgetSlicesWithinATurn(t *testing.T) {
	// Build maxToolCalls-1 (7) calls for the first reply.
	firstCalls := make([]toolCall, maxToolCalls-1)
	for i := range firstCalls {
		firstCalls[i] = mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": fmt.Sprintf("p%d", i)})
	}
	// Second reply returns 3 calls but only 1 slot remains in the budget.
	secondCalls := []toolCall{
		mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "extra0"}),
		mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "extra1"}),
		mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "extra2"}),
	}
	conv := &fakeConv{t: t, replies: []reply{
		{Calls: firstCalls},
		{Calls: secondCalls},
		{Text: "done", Done: true},
	}}
	exec := &countingExec{}
	narrative, _, _, err := runLoop(context.Background(), conv, exec, NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if exec.n != maxToolCalls {
		t.Errorf("executed %d calls, want exactly maxToolCalls=%d; inner-loop break may be missing", exec.n, maxToolCalls)
	}
	if !conv.concluded {
		t.Error("loop must call conclude when tool-call budget is exhausted mid-turn")
	}
	if narrative != "done" {
		t.Errorf("narrative = %q, want %q", narrative, "done")
	}
}

// TestRunLoop_AnswersEveryToolUseEvenWhenOverBudget proves R219: when a
// single reply requests more tool_use blocks than the remaining budget
// allows, the loop must still produce one tool_result per tool_use -- the
// over-budget ones refused rather than dropped. The previous behaviour
// (break, dropping the remainder) sends the next request fewer tool_result
// blocks than the model's own prior message had tool_use blocks, which the
// Anthropic API rejects as malformed; the live evidence was a real
// investigation landing on exactly maxToolCalls with more calls pending.
//
// One reply requests 20 tool_use blocks -- 8 (maxToolCalls) fit the budget,
// 12 do not. Only 8 reads may execute (the budget is still enforced), but
// all 20 must reach the model as answered.
func TestRunLoop_AnswersEveryToolUseEvenWhenOverBudget(t *testing.T) {
	const n = 20
	calls := make([]toolCall, n)
	for i := range calls {
		calls[i] = mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": fmt.Sprintf("p%d", i)})
	}
	conv := &fakeConv{t: t, replies: []reply{
		{Calls: calls},
		{Text: "done", Done: true},
	}}
	exec := &countingExec{}
	narrative, _, _, err := runLoop(context.Background(), conv, exec, NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if exec.n != maxToolCalls {
		t.Errorf("executed %d reads, want exactly maxToolCalls=%d -- the budget must still be enforced", exec.n, maxToolCalls)
	}
	if conv.gotResult != n {
		t.Errorf("model was sent %d tool_result blocks for %d tool_use blocks in its own prior message -- every tool_use must be answered", conv.gotResult, n)
	}
	if narrative != "done" {
		t.Errorf("narrative = %q, want %q", narrative, "done")
	}
}

// TestRunLoop_PropagatesTruncated proves R225(A): the Truncated bit on the
// reply that produced the final narrative reaches runLoop's return value, so
// a caller can tell whether the model's own turn hit its output-length
// ceiling rather than choosing to stop.
func TestRunLoop_PropagatesTruncated(t *testing.T) {
	conv := &fakeConv{t: t, replies: []reply{
		{Text: "cut off mid", Done: true, Truncated: true},
	}}
	_, _, truncated, err := runLoop(context.Background(), conv, &countingExec{}, NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("expected runLoop to report the final reply's Truncated bit as true")
	}
}

// TestRunLoop_ReportsNotTruncatedOnCleanFinish is the negative case: a normal
// conclusion (Done, not Truncated) must not be reported as truncated.
func TestRunLoop_ReportsNotTruncatedOnCleanFinish(t *testing.T) {
	conv := &fakeConv{t: t, replies: []reply{
		{Text: "root cause: bad image", Done: true},
	}}
	_, _, truncated, err := runLoop(context.Background(), conv, &countingExec{}, NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("a clean finish must not be reported as truncated")
	}
}
