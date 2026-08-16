package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestScanRegistersPolicyAsARepeatableFlag(t *testing.T) {
	cmd := newScanCommand()
	f := cmd.Flags().Lookup("policy")
	if f == nil {
		t.Fatal("scan has no --policy flag")
	}
	// stringArray, not stringSlice: a path may contain a comma.
	if f.Value.Type() != "stringArray" {
		t.Errorf("--policy is %s, want stringArray so a comma in a path is not a separator", f.Value.Type())
	}
}

func TestScanPolicyPackFlagReachesItsField(t *testing.T) {
	o, err := parseScanFlags([]string{"--policy-pack", "reliability", "--policy-pack", "other"})
	if err != nil {
		t.Fatalf("parseScanFlags: %v", err)
	}
	want := []string{"reliability", "other"}
	if !slices.Equal(o.policyPackNames, want) {
		t.Errorf("policyPackNames = %v, want %v — the flag is repeatable", o.policyPackNames, want)
	}
}

// TestRunScanRejectsDiskThresholdOutsideZeroToOne drives runScan's validation
// directly. 1.0 is valid ("warn only at full"); 0 is not ("warn on everything"
// is not a threshold). 80 is the percent typo this observation exists for —
// a plausible "80%" that parses as a float and silently warns on everything.
func TestRunScanRejectsDiskThresholdOutsideZeroToOne(t *testing.T) {
	tests := []struct {
		threshold float64
		wantErr   bool
	}{
		{0.80, false},
		{0.01, false},
		{1.0, false},
		{0, true},
		{-0.5, true},
		{1.0000001, true},
		{5.0, true},
		{80, true},
	}
	for _, tt := range tests {
		o := scanOptions{output: "text", diskThreshold: tt.threshold, kubeconfig: "/nonexistent-for-this-test"}
		err := runScan(o)
		rejected := err != nil && strings.Contains(err.Error(), "--disk-threshold")
		if rejected != tt.wantErr {
			t.Errorf("diskThreshold=%v: err=%v, want rejected=%v", tt.threshold, err, tt.wantErr)
		}
	}
}
