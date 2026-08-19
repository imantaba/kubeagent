package explain

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// TestToExplanation_SetsTruncatedFromStopReason mirrors
// internal/investigate's TestToReply_SetsFieldsFromStopReason: toExplanation
// is a pure func(*anthropic.Message) Explanation with a single production
// caller (anthropicSummarizer.summarize), so a bare composite literal
// carrying only StopReason is enough to exercise it — no fake, no SDK round
// trip.
func TestToExplanation_SetsTruncatedFromStopReason(t *testing.T) {
	cases := []struct {
		name          string
		stopReason    anthropic.StopReason
		wantTruncated bool
	}{
		{"max_tokens is truncated", anthropic.StopReasonMaxTokens, true},
		{"end_turn is not truncated", anthropic.StopReasonEndTurn, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := toExplanation(&anthropic.Message{StopReason: tc.stopReason})
			if e.Truncated != tc.wantTruncated {
				t.Errorf("Truncated = %v, want %v", e.Truncated, tc.wantTruncated)
			}
		})
	}
}
