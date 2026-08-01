package cli

import "testing"

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
