package cli

import (
	"slices"
	"testing"
)

func TestGateRegistersPolicyAsARepeatableFlag(t *testing.T) {
	cmd := newGateCommand()
	f := cmd.Flags().Lookup("policy")
	if f == nil {
		t.Fatal("gate has no --policy flag")
	}
	if f.Value.Type() != "stringArray" {
		t.Errorf("--policy is %s, want stringArray", f.Value.Type())
	}
}

// --policy and --policy-pack are declared per command, never as persistent
// flags. The commands that take no policy must reject both rather than
// accept and ignore them.
func TestPolicyIsNotAPersistentFlag(t *testing.T) {
	for _, name := range []string{"watch", "mcp", "tui", "version", "schema"} {
		for _, flag := range [][]string{{"--policy", "x.yaml"}, {"--policy-pack", "reliability"}} {
			// []string{name} is built fresh inside each append call: its cap
			// equals its len, so append always reallocates rather than
			// reusing a backing array another iteration still holds.
			if err := Run(append([]string{name}, flag...)); err == nil {
				t.Errorf("%s accepted %s", name, flag[0])
			}
		}
	}
}

func TestGatePolicyPackFlagReachesItsField(t *testing.T) {
	o, err := parseGateFlags([]string{"--policy-pack", "reliability"})
	if err != nil {
		t.Fatalf("parseGateFlags: %v", err)
	}
	if !slices.Equal(o.policyPackNames, []string{"reliability"}) {
		t.Errorf("policyPackNames = %v, want [reliability]", o.policyPackNames)
	}
}
