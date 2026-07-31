package cli

import (
	"bytes"
	"io"
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

// TestCompletionEmitsAScriptForEveryShell asserts that `kubeagent completion`
// produces a non-trivial, name-bearing script for each of the four shells
// Cobra supports.
//
// Moved here from surface_test.go: that file's entire value is that it can
// be shown byte-identical to the commit that created it with one `git diff`,
// so a reviewer never has to read it line by line to confirm the three
// frozen tables were left alone. A new test — even one that never touches
// those tables — defeats that property the moment it lands there instead of
// here.
func TestCompletionEmitsAScriptForEveryShell(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			root := newRootCommand()
			var out strings.Builder
			root.SetOut(&out)
			root.SetArgs([]string{"completion", shell})
			if err := root.Execute(); err != nil {
				t.Fatalf("completion %s: %v", shell, err)
			}
			if out.Len() == 0 {
				t.Fatalf("completion %s produced no output", shell)
			}
			if !strings.Contains(out.String(), "kubeagent") {
				t.Errorf("completion %s does not name the command", shell)
			}
		})
	}
}

// TestCompletionRejectsAnUnknownShell asserts that a shell name outside the
// four Cobra supports is rejected rather than silently accepted. Moved here
// alongside TestCompletionEmitsAScriptForEveryShell — see its comment.
func TestCompletionRejectsAnUnknownShell(t *testing.T) {
	root := newRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"completion", "nonesuch"})
	if err := root.Execute(); err == nil {
		t.Fatal("completion nonesuch = nil, want an error")
	}
}

// TestCompletionNeedsNoCluster asserts that a completion script is
// generatable with no kubeconfig at all — completion is the one kubeagent
// command that touches nothing. Moved here alongside
// TestCompletionEmitsAScriptForEveryShell — see its comment.
func TestCompletionNeedsNoCluster(t *testing.T) {
	t.Setenv("KUBECONFIG", "/nonexistent/kubeconfig")
	root := newRootCommand()
	root.SetOut(io.Discard)
	root.SetArgs([]string{"completion", "bash"})
	if err := root.Execute(); err != nil {
		t.Errorf("completion bash with no kubeconfig: %v", err)
	}
}

// cobraUpstreamComment is the one line Cobra's own bash-completion-v2
// template carries that names a scheme. Confirmed by generating the actual
// script (`kubeagent completion bash | grep '://'`) and reading it back with
// `xxd`, byte for byte: eight literal spaces, then the comment, no trailing
// whitespace, no tab. It links an upstream GitHub issue explaining a shell
// quirk; it ships in every Cobra program's bash script; it names no
// kubeagent build detail and is not a credential. It is boilerplate present
// in every Cobra CLI's output and is not something kubeagent controls, so it
// is excluded by exact line rather than by pattern — matching on "any
// github.com URL" or "any :// on a comment line" would also silently wave
// through a real leak that happened to reuse the same shape. Anything else
// carrying a scheme is kubeagent's own text and is a leak: a completion
// script is a file users commit to dotfile repos and paste into issues.
const cobraUpstreamComment = "        # https://github.com/spf13/cobra/issues/1508"

// TestCompletionScriptCarriesNoPathOrURL pins a project-wide rule onto the
// one new command the Cobra migration adds: a generated completion script
// must never carry a filesystem path from the machine it was built on, nor
// a URL kubeagent itself put there. Kubeconfig paths and URLs are treated as
// credentials in this project, and a completion script is a file users
// commonly commit to dotfile repos or paste into issues, so it must be as
// safe to share as the binary name it embeds. Cobra's generators only ever
// embed the command tree — names, flags, short descriptions — so this
// should hold for every shell; bash is the representative check.
//
// Three independent axes, each asserted unconditionally (a lookup failure
// fails the test rather than silently skipping the check it guards):
//   - no "/home/" — a common Linux user-directory prefix;
//   - no occurrence of this run's actual working directory or home
//     directory — a direct, environment-derived check for "no absolute path
//     from the build machine", rather than a static substring guess;
//   - no line containing "://" other than the one known-safe Cobra
//     boilerplate line named by cobraUpstreamComment (see its comment) — a
//     future Short, Long or Example string that bakes in a real URL (an
//     internal wiki link, a webhook, a URL with a token) is exactly what
//     this axis exists to catch.
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

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v — this check must not silently skip", err)
	}
	if strings.Contains(script, wd) {
		t.Errorf("completion bash script embeds the build machine's working directory %q", wd)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v — this check must not silently skip", err)
	}
	if strings.Contains(script, home) {
		t.Errorf("completion bash script embeds the build machine's home directory %q", home)
	}

	for _, line := range strings.Split(script, "\n") {
		if line == cobraUpstreamComment {
			continue
		}
		if strings.Contains(line, "://") {
			t.Errorf("completion bash script carries a URL outside Cobra's own boilerplate: %q", line)
		}
	}
}

// TestUsageErrorNamesEveryCommand pins the top-level usage error against the
// command tree itself. It exists because completion shipped without being
// added to the list: the string is hand-maintained, nothing derives it from
// the tree, and no test compared the two. Adding a command to the root now
// fails here until the usage error mentions it.
func TestUsageErrorNamesEveryCommand(t *testing.T) {
	text := usageError().Error()
	for _, cmd := range newRootCommand().Commands() {
		name := cmd.Name()
		if name == "help" || cmd.Hidden {
			continue
		}
		if !strings.Contains(text, name) {
			t.Errorf("usage error does not mention the %q command:\n%s", name, text)
		}
	}
}
