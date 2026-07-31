package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyValidateIsReachableWithNoKubeconfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rules.yaml")
	if err := os.WriteFile(p, []byte(validPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	// No kubeconfig anywhere: policy validate must not read one. This is gate
	// evidence item 3, asserted in a unit test as well as on a real machine.
	t.Setenv("KUBECONFIG", filepath.Join(dir, "does-not-exist"))
	t.Setenv("HOME", dir)

	if err := Run([]string{"policy", "validate", p}); err != nil {
		t.Fatalf("policy validate: %v", err)
	}
}

func TestUsageNamesPolicyValidate(t *testing.T) {
	if !strings.Contains(usageError().Error(), "policy validate") {
		t.Error("the usage line does not name policy validate")
	}
}
