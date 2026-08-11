package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/knownissues"
)

// The list view names every documented kind, one per line, with its summary.
func TestRunKnownIssuesListsEveryKind(t *testing.T) {
	var buf bytes.Buffer
	if err := runKnownIssues(nil, &buf); err != nil {
		t.Fatalf("runKnownIssues() error = %v", err)
	}
	out := buf.String()
	for _, e := range knownissues.All() {
		if !strings.Contains(out, e.Kind) {
			t.Errorf("list output is missing kind %q", e.Kind)
		}
		if !strings.Contains(out, e.Summary) {
			t.Errorf("list output is missing the summary for %q", e.Kind)
		}
	}
	if !strings.Contains(out, "known-issues <kind>") {
		t.Error("list output does not tell the reader how to print one")
	}
}

// The list is sorted, so a reader can find a kind by scanning.
func TestRunKnownIssuesListIsSorted(t *testing.T) {
	var buf bytes.Buffer
	if err := runKnownIssues(nil, &buf); err != nil {
		t.Fatalf("runKnownIssues() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var prev string
	for _, ln := range lines {
		fields := strings.Fields(ln)
		if len(fields) == 0 {
			continue
		}
		if _, ok := knownissues.Lookup(fields[0]); !ok {
			continue // the footer, not a kind row
		}
		if prev != "" && fields[0] < prev {
			t.Errorf("kinds out of order: %q after %q", fields[0], prev)
		}
		prev = fields[0]
	}
	if prev == "" {
		t.Fatal("no kind rows found; the assertion would pass vacuously")
	}
}

// One kind prints in full: the detail, every cause, every check, the anchor.
func TestRunKnownIssuesPrintsOneEntry(t *testing.T) {
	var buf bytes.Buffer
	if err := runKnownIssues([]string{"OOMKilled"}, &buf); err != nil {
		t.Fatalf("runKnownIssues() error = %v", err)
	}
	out := buf.String()
	e, ok := knownissues.Lookup("OOMKilled")
	if !ok {
		t.Fatal("OOMKilled missing from the registry")
	}
	for _, want := range []string{e.Kind, "Likely causes", "What to check", e.Docs} {
		if !strings.Contains(out, want) {
			t.Errorf("full output is missing %q", want)
		}
	}
	for _, c := range e.Checks {
		if !strings.Contains(out, c) {
			t.Errorf("full output is missing a check verbatim: %q", c)
		}
	}
	// Another kind's content must not leak into this one.
	if strings.Contains(out, "Unschedulable") {
		t.Error("full output for one kind mentions another")
	}
}

// A check is a command line: it is printed on one line, never wrapped, so it
// can be copied.
func TestChecksAreNeverWrapped(t *testing.T) {
	for _, e := range knownissues.All() {
		var buf bytes.Buffer
		if err := runKnownIssues([]string{e.Kind}, &buf); err != nil {
			t.Fatalf("runKnownIssues(%q) error = %v", e.Kind, err)
		}
		for _, c := range e.Checks {
			found := false
			for _, ln := range strings.Split(buf.String(), "\n") {
				if strings.TrimSpace(ln) == "- "+c {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%q: check is not on a line of its own: %q", e.Kind, c)
			}
		}
	}
}

// Prose wraps at 72 columns. Checks are exempt by the rule above, and so is a
// single word longer than the budget, which is emitted whole.
func TestProseWrapsAtSeventyTwoColumns(t *testing.T) {
	for _, e := range knownissues.All() {
		var buf bytes.Buffer
		if err := runKnownIssues([]string{e.Kind}, &buf); err != nil {
			t.Fatalf("runKnownIssues(%q) error = %v", e.Kind, err)
		}
		inChecks := false
		for _, ln := range strings.Split(buf.String(), "\n") {
			switch {
			case strings.HasPrefix(ln, "What to check"):
				inChecks = true
				continue
			case strings.HasPrefix(ln, "Likely causes"):
				inChecks = false
				continue
			}
			if inChecks || strings.Contains(ln, "://") {
				continue
			}
			if n := len([]rune(ln)); n > 72 && len(strings.Fields(ln)) > 1 {
				t.Errorf("%q: line is %d columns: %q", e.Kind, n, ln)
			}
		}
	}
}

func TestWrapProse(t *testing.T) {
	got := wrapProse("one two three four", "  - ", "    ", 12)
	want := []string{"  - one two", "    three", "    four"}
	if len(got) != len(want) {
		t.Fatalf("wrapProse() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A word that cannot fit the budget is emitted whole rather than broken: a
// truncated identifier is less readable than an over-long line.
func TestWrapProseKeepsAnOverlongWordWhole(t *testing.T) {
	got := wrapProse("aaaaaaaaaaaaaaaaaaaa b", "  ", "  ", 10)
	if len(got) != 2 || got[0] != "  aaaaaaaaaaaaaaaaaaaa" || got[1] != "  b" {
		t.Errorf("wrapProse() = %q", got)
	}
}

// An unknown kind names what was asked for and what is available, and the %q
// verb renders it as a Go string literal — so a control byte spliced into the
// argument prints escaped rather than reaching the terminal. That is why this
// path needs no safetext call.
func TestRunKnownIssuesRejectsAnUnknownKind(t *testing.T) {
	var buf bytes.Buffer
	err := runKnownIssues([]string{"NoSuchKind"}, &buf)
	if err == nil {
		t.Fatal("runKnownIssues() accepted an unknown kind")
	}
	if !strings.Contains(err.Error(), `"NoSuchKind"`) {
		t.Errorf("error = %v, want it to name the kind", err)
	}
	if !strings.Contains(err.Error(), "OOMKilled") {
		t.Errorf("error = %v, want it to list what is documented", err)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %q to the output on an error", buf.String())
	}
}

// The message must be honest about coverage rather than implying the operator
// mistyped. This reference documents the deterministic detector set; a kind
// from elsewhere in kubeagent — NoEndpoints is one, from the cluster pass — is
// real and simply not in it, so the message says where those are explained.
func TestUnknownKindMessageIsHonestAboutCoverage(t *testing.T) {
	err := runKnownIssues([]string{"NoEndpoints"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("runKnownIssues() accepted an unknown kind")
	}
	if !strings.Contains(err.Error(), "detector set") {
		t.Errorf("error = %v, want it to say what the reference covers", err)
	}
	if !strings.Contains(err.Error(), "https://k8sproject.top/features/diagnostics/") {
		t.Errorf("error = %v, want it to point at where the rest are explained", err)
	}
}

// A workload-level kind gets its own message. kubeagent emitted RolloutStuck,
// FailedCreate or JobFailed a moment earlier, so telling the operator who asks
// about one that the kind is unknown would be false. The message says the kind
// is real and not part of the detector set this reference covers.
func TestWorkloadKindMessageDoesNotSayUnknown(t *testing.T) {
	for _, kind := range knownissues.WorkloadKinds() {
		err := runKnownIssues([]string{kind}, &bytes.Buffer{})
		if err == nil {
			t.Fatalf("runKnownIssues(%q) returned no error; the exit code must not change", kind)
		}
		if strings.Contains(err.Error(), "unknown") {
			t.Errorf("%q: error = %v, want it not to call an emitted kind unknown", kind, err)
		}
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("%q: error = %v, want it to name the kind", kind, err)
		}
		if !strings.Contains(err.Error(), "workload-level") {
			t.Errorf("%q: error = %v, want it to say what the kind is", kind, err)
		}
		if !strings.Contains(err.Error(), "https://k8sproject.top/features/diagnostics/") {
			t.Errorf("%q: error = %v, want it to point at where it is explained", kind, err)
		}
	}
}

// The two cases must not blur into one softer message. A typo is unknown —
// that is the correct word for it — and its message still lists what the
// reference documents, which the workload-kind message does not.
func TestWorkloadKindMessageIsDistinctFromTheUnknownOne(t *testing.T) {
	unknown := runKnownIssues([]string{"RolloutStucc"}, &bytes.Buffer{})
	if unknown == nil {
		t.Fatal("runKnownIssues() accepted a typo")
	}
	if !strings.Contains(unknown.Error(), "unknown issue kind") {
		t.Errorf("error = %v, want the unknown-kind wording verbatim", unknown)
	}
	if !strings.Contains(unknown.Error(), "OOMKilled") {
		t.Errorf("error = %v, want it to list what is documented", unknown)
	}

	workload := runKnownIssues([]string{"RolloutStuck"}, &bytes.Buffer{})
	if workload == nil {
		t.Fatal("runKnownIssues() accepted a workload kind")
	}
	if workload.Error() == unknown.Error() {
		t.Error("the two messages are identical; the cases must stay distinct")
	}
}

// A workload kind writes nothing to the output, exactly as an unknown one does:
// this is an error path, not a second rendering of the reference.
func TestWorkloadKindWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := runKnownIssues([]string{"JobFailed"}, &buf); err == nil {
		t.Fatal("runKnownIssues() accepted a workload kind")
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %q to the output on an error", buf.String())
	}
}

func TestRunKnownIssuesEscapesAControlByte(t *testing.T) {
	err := runKnownIssues([]string{"\x1b[31mred"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("runKnownIssues() accepted an unknown kind")
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Errorf("error carries a raw escape byte: %q", err.Error())
	}
}

// Lookup is exact, and the command does not soften it.
func TestRunKnownIssuesDoesNotCaseFold(t *testing.T) {
	if err := runKnownIssues([]string{"oomkilled"}, &bytes.Buffer{}); err == nil {
		t.Error("runKnownIssues() matched a lowercase kind; Lookup is exact")
	}
}

// More than one argument is a usage error in the shape `schema` already uses.
func TestRunKnownIssuesRejectsTwoArguments(t *testing.T) {
	err := runKnownIssues([]string{"OOMKilled", "RestartLoop"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("runKnownIssues() accepted two arguments")
	}
	if !strings.Contains(err.Error(), "usage:") || !strings.Contains(err.Error(), "known-issues [kind]") {
		t.Errorf("error = %v, want the usage line", err)
	}
}

// The command is registered, takes no flags at all — not even --kubeconfig —
// and offers the kinds for completion.
func TestKnownIssuesCommandShape(t *testing.T) {
	cmd := newKnownIssuesCommand()
	if !cmd.SilenceErrors || !cmd.SilenceUsage {
		t.Error("the command must silence Cobra's own error and usage rendering")
	}
	if cmd.Flags().Lookup("kubeconfig") != nil {
		t.Error("known-issues registers --kubeconfig; there is no cluster on this path")
	}
	// Cobra registers its own --help lazily, inside Execute, so a freshly
	// constructed command has genuinely no flags at all.
	if cmd.Flags().HasFlags() {
		t.Error("known-issues declares a flag; it must take none")
	}
	if len(cmd.ValidArgs) != 0 {
		t.Error("ValidArgs is set; it would let Cobra reword the unknown-kind error")
	}
	if cmd.ValidArgsFunction == nil {
		t.Fatal("no ValidArgsFunction; completion would offer filenames")
	}
	got, _ := cmd.ValidArgsFunction(cmd, nil, "")
	if len(got) != len(knownissues.Kinds()) {
		t.Errorf("completion offers %d kinds, want %d", len(got), len(knownissues.Kinds()))
	}
}

func TestKnownIssuesIsRegistered(t *testing.T) {
	var found bool
	for _, c := range newRootCommand().Commands() {
		if c.Name() == "known-issues" {
			found = true
		}
	}
	if !found {
		t.Error("known-issues is not registered on the root command")
	}
}
