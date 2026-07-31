package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// Run parses the top-level command line and dispatches to the named
// subcommand. It is the seam every test in this package drives directly: it
// touches no process-global state itself (Main owns stderr and the exit
// code), so a test can assert on the returned error alone.
func Run(args []string) error {
	if len(args) > 0 && args[0] == "version" {
		fmt.Fprintln(os.Stdout, versionLine())
		return nil
	}
	if len(args) > 0 && args[0] == "watch" {
		return runWatch(args[1:])
	}
	if len(args) > 0 && args[0] == "mcp" {
		return runMCP(args[1:])
	}
	if len(args) > 0 && args[0] == "gate" {
		return runGate(args[1:])
	}
	if len(args) > 0 && args[0] == "tui" {
		return runTUI(args[1:])
	}
	if len(args) > 0 && args[0] == "rbac" {
		return runRBAC(args[1:])
	}
	if len(args) > 0 && args[0] == "schema" {
		return runSchema(args[1:], os.Stdout)
	}
	if len(args) == 0 || args[0] != "scan" {
		return fmt.Errorf("usage: %[1]s scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json|html] [--explain] [--investigate] [--model name] [--include-cron] [--include-restarts] [--pvc-reclaim] [--lint-secrets] [--security] [--security-verbose] [--disk-usage [--disk-threshold r]] [--kubelet-health] [--control-plane-health] [--dns-health] [--certs [--cert-warn-days n]] [--operators] [--drift] [--drift-age dur] [--capacity] [--logs] [--node-heartbeat-threshold dur] [--expected-nodes a,b,…] [--fix [--dry-run|--yes] [--audit-log path]] [--rollback --audit-log path] | %[1]s watch [--kubeconfig path] [--context name (repeatable)] [--cluster-name name] [--include-local] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur] [--alert-format json|slack|alertmanager] [--alert-repeat dur] [--slo-target pct] [--explain [--explain-cooldown dur] [--explain-budget n] [--model name]] | %[1]s mcp [--kubeconfig path] [--context name] [--allow-context-switch] [--logs] | %[1]s gate [--kubeconfig path] [--context name] [-n namespace] [--wait-for kind/name] [--timeout dur] [--fail-on critical|warning|info] [--allow-partial-read resource (repeatable)] [--output text|json|sarif] | %[1]s tui [--kubeconfig path] [--context name] [-n namespace] | %[1]s rbac print [--profile scan|watch|full] [--features a,b,…] [--role-name name] [--output yaml|json] | %[1]s rbac check [--kubeconfig path] [--context name] [--profile scan|watch|full] [--features a,b,…] [--output text|json] | %[1]s schema [name] | %[1]s version", invokedAs)
	}
	return runScan(args[1:])
}
