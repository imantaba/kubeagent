package cli

import (
	"slices"
	"strings"
	"testing"
	"time"
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

// TestRunScanRejectsAnInvalidExpectedNodeName proves runScan's --expected-nodes
// validation runs before any cluster work, at the same point as R49 and R65's
// validations.
func TestRunScanRejectsAnInvalidExpectedNodeName(t *testing.T) {
	o := scanOptions{output: "text", diskThreshold: 0.80, expectedNodes: []string{"NODE-UPPER"}, kubeconfig: "/nonexistent-for-this-test"}
	err := runScan(o)
	if err == nil {
		t.Fatal("want an error for an invalid --expected-nodes value, got nil")
	}
	if !strings.Contains(err.Error(), "--expected-nodes") {
		t.Errorf("error %q does not name --expected-nodes", err)
	}
}

// TestRunScanRejectsAnOutOfRangeWebhookTimeout proves the
// KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS bound (R163/R164/R165) is wired into
// runScan's RunE, not merely into the envIntRange helper itself. Asserting
// only "err != nil" would pass today for the wrong reason: the dead
// kubeconfig in cluster.NewClient already errors before the webhook value is
// ever read. Naming the variable in the error is what proves the bound
// actually fired.
func TestRunScanRejectsAnOutOfRangeWebhookTimeout(t *testing.T) {
	t.Setenv("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", "60")
	o := scanOptions{output: "text", diskThreshold: 0.80, kubeconfig: "/nonexistent-for-this-test"}
	err := runScan(o)
	if err == nil {
		t.Fatal("want an error for KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS=60, got nil")
	}
	if !strings.Contains(err.Error(), "KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS") {
		t.Errorf("error %q does not name KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS — did the check actually run before cluster.NewClient?", err)
	}
}

// TestExpectedNodesAccumulatesAcrossOccurrences proves --expected-nodes
// resolves to the same set of names whether given as one comma-separated
// value, split across repeated occurrences, or split with an overlapping
// duplicate across occurrences — the duplicate case is what proves a later
// dedup still runs across occurrences, not just within one.
func TestExpectedNodesAccumulatesAcrossOccurrences(t *testing.T) {
	cases := [][]string{
		{"--expected-nodes", "node-a,node-b"},
		{"--expected-nodes", "node-a", "--expected-nodes", "node-b"},
		{"--expected-nodes", "node-a,node-b", "--expected-nodes", "node-b"},
	}
	var sets [][]string
	for _, args := range cases {
		o, err := parseScanFlags(args)
		if err != nil {
			t.Fatalf("parseScanFlags(%v): %v", args, err)
		}
		sets = append(sets, dedupSorted(o.expectedNodes))
	}
	for i := 1; i < len(sets); i++ {
		if !slices.Equal(sets[0], sets[i]) {
			t.Errorf("case %v = %v, want %v (same set as case 0)", cases[i], sets[i], sets[0])
		}
	}
}

func dedupSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// TestRunScanSecurityVerboseRequiresSecurity proves --security-verbose
// without --security is refused before any cluster work, at the same point
// as the other flag-shape validations. The three other combinations must
// clear this check and instead surface cluster.NewClient's dead-kubeconfig
// error, proving the check does not fire when the flags are used as
// documented.
func TestRunScanSecurityVerboseRequiresSecurity(t *testing.T) {
	tests := []struct {
		name            string
		security        bool
		securityVerbose bool
		wantErr         bool
	}{
		{"verbose alone", false, true, true},
		{"security alone", true, false, false},
		{"both", true, true, false},
		{"neither", false, false, false},
	}
	for _, tt := range tests {
		o := scanOptions{output: "text", diskThreshold: 0.80, security: tt.security, securityVerbose: tt.securityVerbose, kubeconfig: "/nonexistent-for-this-test"}
		err := runScan(o)
		rejected := err != nil && err.Error() == "--security-verbose requires --security"
		if rejected != tt.wantErr {
			t.Errorf("%s: err=%v, want rejected=%v", tt.name, err, tt.wantErr)
		}
	}
}

// TestRunScanRejectsNegativeNodeHeartbeatThreshold proves the boundary sits at
// zero, not merely that some negative value is refused: 0 keeps meaning
// "disabled" and every positive value keeps behaving as documented.
func TestRunScanRejectsNegativeNodeHeartbeatThreshold(t *testing.T) {
	tests := []struct {
		duration string
		wantErr  bool
	}{
		{"-1s", true},
		{"-5s", true},
		{"-1h", true},
		{"0", false},
		{"40s", false},
		{"1h", false},
	}
	for _, tt := range tests {
		d, err := time.ParseDuration(tt.duration)
		if err != nil {
			t.Fatalf("time.ParseDuration(%q): %v", tt.duration, err)
		}
		o := scanOptions{output: "text", diskThreshold: 0.80, nodeHeartbeatThreshold: d, kubeconfig: "/nonexistent-for-this-test"}
		err = runScan(o)
		rejected := err != nil && strings.Contains(err.Error(), "--node-heartbeat-threshold")
		if rejected != tt.wantErr {
			t.Errorf("nodeHeartbeatThreshold=%s: err=%v, want rejected=%v", tt.duration, err, tt.wantErr)
		}
	}
}
