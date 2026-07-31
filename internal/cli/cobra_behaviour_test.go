package cli

import (
	"bytes"
	"os"
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
}

// TestParseErrorPrintsNoUsageDump pins the second half of the parse-error
// contract: the message stands alone. Cobra never puts usage text into the
// error value — it prints it separately in ExecuteC, and only when neither
// the subcommand nor the root has SilenceUsage set — so the assertion has
// to watch the command's writers rather than the returned error. Run does
// not expose them, so the test drives the root it builds. Dropping
// SilenceUsage from gate or from the root puts roughly a kilobyte of flag
// listing on stdout after the message; today both are silent.
func TestParseErrorPrintsNoUsageDump(t *testing.T) {
	var out, errOut bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"gate", "--nonesuch"})
	if err := root.Execute(); err == nil {
		t.Fatal("gate --nonesuch: want an error, got nil")
	}
	if out.Len() != 0 {
		t.Errorf("gate --nonesuch wrote %d bytes to stdout, want none:\n%s", out.Len(), out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("gate --nonesuch wrote %d bytes to stderr, want none — Main owns the error message:\n%s", errOut.Len(), errOut.String())
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

// TestCompletionScriptCarriesNoPathOrURL pins a project-wide rule onto the
// one new command the Cobra migration adds: a generated completion script
// must never carry a filesystem path from the machine it was built on.
// Kubeconfig paths are treated as credentials in this project, and a
// completion script is a file users commonly commit to dotfile repos or
// paste into issues, so it must be as safe to share as the binary name it
// embeds. Cobra's generators only ever embed the command tree — names,
// flags, short descriptions — so this should hold for every shell; bash is
// the representative check.
//
// This does not assert "no URL at all": Cobra's own bash-completion-v2
// template embeds a static comment linking a public upstream GitHub issue
// (https://github.com/spf13/cobra/issues/1508) to explain a shell quirk.
// That line ships in every Cobra program's bash script; it names no
// kubeagent build detail and is not a credential, so it is not the leak this
// test guards against. What the test does assert — no occurrence of this
// build's actual working directory or home directory — is a direct,
// environment-derived check for "no absolute path from the build machine",
// rather than a substring list that would have to special-case Cobra's own
// boilerplate.
func TestCompletionScriptCarriesNoPathOrURL(t *testing.T) {
	root := newRootCommand()
	var out strings.Builder
	root.SetOut(&out)
	root.SetArgs([]string{"completion", "bash"})
	if err := root.Execute(); err != nil {
		t.Fatalf("completion bash: %v", err)
	}
	script := out.String()
	if strings.Contains(script, "/home/") {
		t.Errorf("completion bash script contains %q, want none", "/home/")
	}
	if wd, err := os.Getwd(); err == nil && strings.Contains(script, wd) {
		t.Errorf("completion bash script embeds the build machine's working directory %q", wd)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.Contains(script, home) {
		t.Errorf("completion bash script embeds the build machine's home directory %q", home)
	}
}
