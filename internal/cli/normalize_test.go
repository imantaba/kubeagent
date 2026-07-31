package cli

import (
	"slices"
	"testing"
)

// known is the flag set the table pretends the target command declares.
func known(name string) bool {
	switch name {
	case "kubeconfig", "context", "output", "namespace", "disk-threshold":
		return true
	}
	return false
}

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"registered long name", []string{"-kubeconfig", "/nonexistent/kubeconfig"}, []string{"--kubeconfig", "/nonexistent/kubeconfig"}},
		{"equals form", []string{"-kubeconfig=/nonexistent/kubeconfig"}, []string{"--kubeconfig=/nonexistent/kubeconfig"}},
		{"equals form with an equals in the value", []string{"-context=a=b"}, []string{"--context=a=b"}},
		{"several flags", []string{"-context", "example-context", "-output", "json"}, []string{"--context", "example-context", "--output", "json"}},
		{"registered shorthand is untouched", []string{"-n", "example-ns"}, []string{"-n", "example-ns"}},
		{"help shorthand is untouched", []string{"-h"}, []string{"-h"}},
		{"already double dash", []string{"--kubeconfig", "/nonexistent/kubeconfig"}, []string{"--kubeconfig", "/nonexistent/kubeconfig"}},
		{"unregistered name is left for pflag", []string{"-xyz"}, []string{"-xyz"}},
		{"unregistered equals form is left for pflag", []string{"-xyz=1"}, []string{"-xyz=1"}},
		{"bare dash is a value", []string{"-"}, []string{"-"}},
		{"terminator stops rewriting", []string{"--", "-kubeconfig"}, []string{"--", "-kubeconfig"}},
		{"terminator mid-line", []string{"-output", "json", "--", "-output"}, []string{"--output", "json", "--", "-output"}},
		{"non-flag argument", []string{"print", "-profile"}, []string{"print", "-profile"}},
		{"empty", nil, nil},
		{"value that looks like a flag", []string{"--context", "-kubeconfig"}, []string{"--context", "-kubeconfig"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.in, known)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeDoesNotMutateItsInput(t *testing.T) {
	in := []string{"-output", "json"}
	Normalize(in, known)
	if in[0] != "-output" {
		t.Errorf("input mutated: in[0] = %q, want -output", in[0])
	}
}
