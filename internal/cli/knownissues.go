package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/knownissues"
)

// proseWidth is the column budget for wrapped prose. Checks are exempt: a
// wrapped command line cannot be pasted, which is the only thing that section
// is for.
const proseWidth = 72

// runKnownIssues prints kubeagent's offline reference for one failure kind, or
// lists every documented kind.
//
// It reads a compiled-in slice and nothing else: no cluster connection, no
// kubeconfig, no network — and, separately, no model call. Nothing here is
// generated and nothing here is sent anywhere; this is not a smaller --explain.
func runKnownIssues(args []string, w io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintf(w, "Failure kinds kubeagent's pod and workload detectors report:\n")
		for _, e := range knownissues.All() {
			fmt.Fprintf(w, "  %-28s %s\n", e.Kind, e.Summary)
		}
		fmt.Fprintf(w, "\nPrint one:\n  %s known-issues <kind>\n", invokedAs)
		fmt.Fprintf(w, "\nThe %s watch daemon additionally reports cluster-level and certificate\nissue kinds that this reference does not document.\n", invokedAs)
		return nil
	}
	if len(args) > 1 {
		return fmt.Errorf("usage: %s known-issues [kind]", invokedAs)
	}
	entry, ok := knownissues.Lookup(args[0])
	if !ok {
		// Both branches below use %q, which renders the argument as a Go string
		// literal — so a control byte spliced into it prints as \x1b rather than
		// reaching the terminal. That is why this path needs no safetext call of
		// its own.
		//
		// A kind kubeagent emits from a workload pass is answered separately. An
		// operator who reads RolloutStuck off a scan and asks about it must not
		// be told the kind is unknown: kubeagent printed it a moment earlier.
		// The exit code is the same either way — naming a workload kind is still
		// not a documented-kind lookup — so only the wording differs.
		if knownissues.IsWorkloadKind(args[0]) {
			return fmt.Errorf("%q is a workload-level finding, not one of the pod detectors this reference covers; "+
				"it is explained at https://k8sproject.top/features/diagnostics/", args[0])
		}
		// The unknown-kind message says what this reference covers rather than
		// implying the kind is invalid — but it does keep the word "unknown",
		// which is the correct word for a typo. NoEndpoints and the rest of what
		// kubeagent reports from the cluster pass are real kinds that are not in
		// the detector set this slice documents, so the message points at where
		// they are explained instead.
		return fmt.Errorf("unknown issue kind %q; kubeagent documents the deterministic detector set (%s). "+
			"Other findings are explained at https://k8sproject.top/features/diagnostics/",
			args[0], strings.Join(knownissues.Kinds(), ", "))
	}
	printEntry(w, entry)
	return nil
}

// printEntry renders one entry in full.
func printEntry(w io.Writer, e knownissues.Entry) {
	fmt.Fprintf(w, "%s\n", e.Kind)
	for _, ln := range wrapProse(e.Detail, "  ", "  ", proseWidth) {
		fmt.Fprintf(w, "%s\n", ln)
	}

	fmt.Fprintf(w, "\nLikely causes\n")
	for _, c := range e.Causes {
		for _, ln := range wrapProse(c, "  - ", "    ", proseWidth) {
			fmt.Fprintf(w, "%s\n", ln)
		}
	}

	fmt.Fprintf(w, "\nWhat to check\n")
	for _, c := range e.Checks {
		fmt.Fprintf(w, "  - %s\n", c)
	}

	if e.Docs != "" {
		fmt.Fprintf(w, "\n  %s\n", e.Docs)
	}
}

// wrapProse breaks s onto lines whose rendered width — the prefix plus the
// text — is at most width runes. The first line carries first, every later one
// carries cont.
//
// Runes, not bytes: the prose uses em dashes, and counting their three bytes
// would wrap short. A single word wider than the budget is emitted whole on its
// own line rather than broken, because a split identifier or command fragment
// is harder to read than a long line.
func wrapProse(s, first, cont string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var (
		out    []string
		line   = first + words[0]
		prefix = cont
	)
	for _, word := range words[1:] {
		if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > width {
			out = append(out, line)
			line = prefix + word
			continue
		}
		line += " " + word
	}
	return append(out, line)
}

// newKnownIssuesCommand builds `kubeagent known-issues`.
//
// It keeps its own argument handling rather than cobra.MaximumNArgs(1), for the
// same reason `schema` does: that would reword the usage error runKnownIssues
// already produces. ValidArgsFunction rather than ValidArgs, likewise — the
// former only feeds completion, while the latter would make Cobra validate the
// argument itself and replace the unknown-kind error with its own.
//
// No flags at all, not even --kubeconfig: there is no cluster in this path.
func newKnownIssuesCommand() *cobra.Command {
	return &cobra.Command{
		Use:           "known-issues [kind]",
		Short:         "Print what kubeagent's pod and workload detectors know about a failure kind, offline",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return knownissues.Kinds(), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKnownIssues(args, os.Stdout)
		},
	}
}
