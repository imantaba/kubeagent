package cli

import (
	"strings"
	"testing"
)

// TestGateHelpExitsZero pins the last of the --help change. gate is the one
// command whose --help used to exit 4 rather than 1, because gate maps a usage
// problem to 4 for the benefit of CI pipelines. Under Cobra --help is not a
// usage problem at all: it prints to stdout and returns nil.
func TestGateHelpExitsZero(t *testing.T) {
	err := Run([]string{"gate", "--help"})
	if err != nil {
		t.Fatalf("gate --help = %v, want nil", err)
	}
	if got := exitCodeFor(err); got != 0 {
		t.Errorf("gate --help exit code = %d, want 0", got)
	}
}

// TestUnknownFlagIsAUsageErrorNotAFindingsExit pins the parse-error contract on
// gate, the machine-consumed surface. The wording is pflag's — the standard
// library said "flag provided but not defined: -nonesuch" — and no usage dump
// follows it, because every command sets SilenceUsage. What must not change is
// the exit code: a CI pipeline reads gate's 4 as "you invoked me wrong" and its
// 1 as "I found problems", and a mistyped flag reported as 1 would be read as
// findings.
func TestUnknownFlagIsAUsageErrorNotAFindingsExit(t *testing.T) {
	err := Run([]string{"gate", "--nonesuch"})
	if err == nil {
		t.Fatal("gate --nonesuch: want an error, got nil")
	}
	if got := exitCodeFor(err); got != 4 {
		t.Errorf("gate --nonesuch: exit code %d, want 4", got)
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("gate --nonesuch: error %q, want it to name the unknown flag", err)
	}
	if strings.Contains(err.Error(), "Usage:") || strings.Contains(err.Error(), "--namespace") {
		t.Errorf("gate --nonesuch: error carries a usage dump, want the message alone:\n%s", err)
	}
}

// TestNamespaceShorthandSpellings pins the one accepted spelling the migration
// drops. The standard library had to declare the shorthand as a second full
// flag name, which made --n work as well as -n; pflag's shorthand is not a
// name, so --n is now rejected. -n and --namespace are unaffected. This matches
// kubectl, which never accepted --n.
func TestNamespaceShorthandSpellings(t *testing.T) {
	for _, spelling := range []string{"-n", "--namespace"} {
		o, err := parseGateFlags([]string{spelling, "example-ns"})
		if err != nil {
			t.Fatalf("gate %s example-ns: %v", spelling, err)
		}
		if o.namespace != "example-ns" {
			t.Errorf("gate %s example-ns: namespace = %q, want %q", spelling, o.namespace, "example-ns")
		}
	}
	if _, err := parseGateFlags([]string{"--n", "example-ns"}); err == nil {
		t.Error("gate --n example-ns: want an error — --n is no longer an accepted spelling")
	}
}

// TestStrayPositionalArgumentIsRejected pins Task 6's Args: cobra.NoArgs. The
// standard-library dispatch ignored a trailing word; Cobra reports it. mcp is
// the case that matters, because a client that passes junk should be told
// rather than served.
func TestStrayPositionalArgumentIsRejected(t *testing.T) {
	if err := Run([]string{"mcp", "stray"}); err == nil {
		t.Error("mcp stray: want an error, got nil")
	}
}
