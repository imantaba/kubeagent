package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// version is the build version. main passes it in from its own package-level
// var so the release workflow's -ldflags target stays main.version; every
// command that stamps a version (mcp, tui, gate's SARIF, version) reads it
// from here. Tests that call Run directly leave it at "dev", exactly as
// tests against main() did.
var version = "dev"

// invocationName returns how the user invoked this process, for use in usage
// and error text. krew installs the binary as ~/.krew/bin/kubectl-kubeagent
// and kubectl execs it under that name, so argv[0]'s basename tells us which
// command the user actually typed. Anything else — a plain ./kubeagent, a
// kubectl-kubeagent directory in the path, a kubectl-kubeagent-extra sibling
// plugin — is the ordinary binary.
func invocationName(argv0 string) string {
	if filepath.Base(argv0) == "kubectl-kubeagent" {
		return "kubectl kubeagent"
	}
	return "kubeagent"
}

// invokedAs is the command name used in usage and error text, resolved once at
// startup. Tests override it to exercise the kubectl-plugin spelling.
var invokedAs = invocationName(os.Args[0])

// warnf writes one non-fatal warning line, prefixed with the name the user
// actually typed. Warnings go through here rather than a bare Fprintf so a
// kubectl-plugin user is never told about a "kubeagent" that is not on their
// PATH. The trailing newline is supplied.
func warnf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s: warning: %s\n", invokedAs, fmt.Sprintf(format, args...))
}

// exitError lets a subcommand choose its process exit status. `gate` publishes
// a five-code contract a pipeline branches on (see
// website/docs/features/ci-gate.md); every other subcommand still exits 0 or 1
// exactly as before, because a plain error is unaffected by this type.
//
// An empty msg means the command already reported on stdout and main() should
// exit quietly rather than print a second, redundant line.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// exitCodeFor maps a run() result to a process exit status.
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}

// Main runs one command line and returns the process exit status. It owns the
// two process-level concerns Run deliberately does not: rendering the error to
// stderr, and choosing the exit code.
func Main(v string) int {
	version = v
	err := Run(os.Args[1:])
	if err != nil {
		var ee *exitError
		if !errors.As(err, &ee) || ee.msg != "" {
			fmt.Fprintf(os.Stderr, "%s: %v\n", invokedAs, err)
		}
	}
	return exitCodeFor(err)
}

// usageError is kubeagent's top-level usage error: the exhaustive list of
// every subcommand and its flags. Both the root command's RunE and Run's
// unknown-subcommand fallback return it, so an invalid invocation always
// gets this text rather than Cobra's own "unknown command" text.
func usageError() error {
	return fmt.Errorf("usage: %[1]s scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json|html] [--explain] [--investigate] [--model name] [--include-cron] [--include-restarts] [--pvc-reclaim] [--lint-secrets] [--security] [--security-verbose] [--disk-usage [--disk-threshold r]] [--kubelet-health] [--control-plane-health] [--dns-health] [--certs [--cert-warn-days n]] [--operators] [--drift] [--drift-age dur] [--capacity] [--logs] [--node-heartbeat-threshold dur] [--expected-nodes a,b,…] [--fix [--dry-run|--yes] [--audit-log path]] [--rollback --audit-log path] | %[1]s watch [--kubeconfig path] [--context name (repeatable)] [--cluster-name name] [--include-local] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur] [--alert-format json|slack|alertmanager] [--alert-repeat dur] [--slo-target pct] [--explain [--explain-cooldown dur] [--explain-budget n] [--model name]] | %[1]s mcp [--kubeconfig path] [--context name] [--allow-context-switch] [--logs] | %[1]s gate [--kubeconfig path] [--context name] [-n namespace] [--wait-for kind/name] [--timeout dur] [--fail-on critical|warning|info] [--allow-partial-read resource (repeatable)] [--output text|json|sarif] | %[1]s tui [--kubeconfig path] [--context name] [-n namespace] | %[1]s rbac print [--profile scan|watch|full] [--features a,b,…] [--role-name name] [--output yaml|json] | %[1]s rbac check [--kubeconfig path] [--context name] [--profile scan|watch|full] [--features a,b,…] [--output text|json] | %[1]s schema [name] | %[1]s version | %[1]s completion bash|zsh|fish|powershell", invokedAs)
}

// newRootCommand builds the command tree. It is a function rather than a
// package-level var so each test gets a clean tree, and so invokedAs is read
// at construction time — the tests that exercise the kubectl-plugin spelling
// set it and rebuild.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "kubeagent",
		Short:         "Read-only Kubernetes troubleshooting",
		SilenceErrors: true,
		SilenceUsage:  true,
		// CommandDisplayNameAnnotation makes CommandPath render the spelling
		// the user actually typed. krew installs the binary as
		// kubectl-kubeagent, and Use cannot carry a space because Name takes
		// everything before the first one — "kubectl kubeagent" would produce
		// a root named "kubectl" and a subcommand path of "kubectl scan".
		Annotations: map[string]string{
			cobra.CommandDisplayNameAnnotation: invokedAs,
		},
		// A bare `kubeagent` returns the usage error the standard-library
		// implementation returned, so the exit code and the text are unchanged.
		RunE: func(cmd *cobra.Command, args []string) error {
			return usageError()
		},
	}
	root.AddCommand(newVersionCommand(), newSchemaCommand(), newMCPCommand(), newTUICommand(), newScanCommand(),
		newWatchCommand(), newGateCommand(), newRBACCommand(), newCompletionCommand())
	return root
}

// longFlagLookup reports whether a name is a long flag on cmd and, if so,
// whether it takes a value, for Normalize. pflag sets NoOptDefVal on a flag
// exactly when it can stand alone with no following argument — a bool or a
// count — so an empty NoOptDefVal is pflag's own signal that the flag needs
// one.
func longFlagLookup(cmd *cobra.Command) func(name string) (registered, takesValue bool) {
	return func(name string) (bool, bool) {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			return false, false
		}
		return true, f.NoOptDefVal == ""
	}
}

// Run parses the top-level command line and dispatches to the named
// subcommand. It is the seam every test in this package drives directly: it
// touches no process-global state itself (Main owns stderr and the exit
// code), so a test can assert on the returned error alone.
//
// Every subcommand is built and dispatched through Cobra.
func Run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	root := newRootCommand()
	sub, rest, err := root.Find(args)
	if err != nil || sub == root {
		return usageError()
	}
	root.SetArgs(append(commandPathArgs(sub, root), Normalize(rest, longFlagLookup(sub))...))
	return root.Execute()
}

// commandPathArgs rebuilds the verb path from root down to sub — one element
// for `scan`, two for `rbac print` — so SetArgs re-resolves the same command
// Find already located.
func commandPathArgs(sub, root *cobra.Command) []string {
	var path []string
	for c := sub; c != nil && c != root; c = c.Parent() {
		path = append([]string{c.Name()}, path...)
	}
	return path
}
