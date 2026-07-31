package cli

import "testing"

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

// --policy is declared per command, never as a persistent flag. The commands
// that take no policy must reject it rather than accept and ignore it.
func TestPolicyIsNotAPersistentFlag(t *testing.T) {
	for _, name := range []string{"watch", "mcp", "tui", "version", "schema"} {
		if err := Run([]string{name, "--policy", "x.yaml"}); err == nil {
			t.Errorf("%s accepted --policy", name)
		}
	}
}
