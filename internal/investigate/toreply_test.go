package investigate

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// TestToReply_SetsFieldsFromStopReason proves R225(A) at its source. Every
// other Truncated-propagation test in this package (loop_test.go,
// investigate_test.go) injects a fakeConv reply literal with Truncated
// already set, which pins the plumbing downstream of toReply — reply ->
// runLoop -> Report -> Input -> the renderer — but never toReply itself,
// the one place the model's raw StopReason becomes that bit. This is the
// one test that calls the real function. toReply is a pure
// func(*anthropic.Message) reply with a single production caller
// (anthropicConversation.roundtrip), so a bare composite literal carrying
// only StopReason is enough to exercise it — no fake, no SDK round trip.
func TestToReply_SetsFieldsFromStopReason(t *testing.T) {
	cases := []struct {
		name          string
		stopReason    anthropic.StopReason
		wantDone      bool
		wantTruncated bool
	}{
		{"max_tokens is done and truncated", anthropic.StopReasonMaxTokens, true, true},
		{"end_turn is done and not truncated", anthropic.StopReasonEndTurn, true, false},
		{"tool_use is neither done nor truncated", anthropic.StopReasonToolUse, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := toReply(&anthropic.Message{StopReason: tc.stopReason})
			if r.Done != tc.wantDone {
				t.Errorf("Done = %v, want %v", r.Done, tc.wantDone)
			}
			if r.Truncated != tc.wantTruncated {
				t.Errorf("Truncated = %v, want %v", r.Truncated, tc.wantTruncated)
			}
		})
	}
}
