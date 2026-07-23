package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeConv scripts a fixed sequence of replies; the i-th call to start/next/
// concludes returns replies[i]. It records the tool results it was given.
type fakeConv struct {
	replies   []reply
	i         int
	gotResult int
	concluded bool
	startErr  error
}

func (f *fakeConv) pop() reply {
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
	conv := &fakeConv{replies: []reply{
		{Calls: []toolCall{mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "web-abc"})}},
		{Calls: []toolCall{mkCall("get_events", map[string]string{"namespace": "shop", "name": "web-abc"})}},
		{Text: "root cause: bad image", Done: true},
	}}
	exec := &countingExec{}
	narrative, trail, err := runLoop(context.Background(), conv, exec, NewScope(nil))
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
	// With 1 call per reply, the turn cap (maxTurns) fires first; the final
	// reply is placed at the position conclude() will pop.
	var reps []reply
	for i := 0; i < maxTurns; i++ {
		reps = append(reps, reply{Calls: []toolCall{mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "p"})}})
	}
	reps = append(reps, reply{Text: "final under cap", Done: true})
	conv := &fakeConv{replies: reps}
	exec := &countingExec{}
	s := NewScope(nil)
	s.Add("pod", "shop", "p")
	narrative, _, err := runLoop(context.Background(), conv, exec, s)
	if err != nil {
		t.Fatal(err)
	}
	if exec.n > maxToolCalls {
		t.Errorf("executed %d calls, must not exceed maxToolCalls=%d", exec.n, maxToolCalls)
	}
	if !conv.concluded {
		t.Error("loop must call conclude when a cap is hit")
	}
	if narrative != "final under cap" {
		t.Errorf("narrative = %q", narrative)
	}
}

func TestRunLoop_StartErrorPropagates(t *testing.T) {
	conv := &fakeConv{startErr: errors.New("api down")}
	_, _, err := runLoop(context.Background(), conv, &countingExec{}, NewScope(nil))
	if err == nil {
		t.Error("expected the start error to propagate")
	}
}
