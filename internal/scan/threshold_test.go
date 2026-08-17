package scan

import (
	"testing"
	"time"
)

func TestTerminatingThreshold(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", 2 * time.Minute},
		{"10m", 10 * time.Minute},
		{"30s", 30 * time.Second},
		{"0", 2 * time.Minute},
		{"-1m", 2 * time.Minute},
		{"banana", 2 * time.Minute},
	}
	for _, tc := range cases {
		t.Setenv("KUBEAGENT_TERMINATING_THRESHOLD", tc.env)
		if got := terminatingThreshold(); got != tc.want {
			t.Errorf("KUBEAGENT_TERMINATING_THRESHOLD=%q gave %v, want %v", tc.env, got, tc.want)
		}
	}
}
