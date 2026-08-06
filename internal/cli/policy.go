package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/policy"
	"github.com/imantaba/kubeagent/internal/policypack"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/scan"
)

// namedPolicyDocuments reads every path into a document internal/policy can
// load. The filesystem stops here: internal/policy takes bytes and a name,
// which is what keeps it importable by gate and mcp.
//
// A path may be a file or a directory. A named file is read whatever it is
// called — the operator typed the name. A directory contributes its .yaml and
// .yml entries in name order, and only those: a directory is a place other
// things live too, and reading a README as a policy would be an error message
// about YAML instead of about the mistake.
//
// The walk is not recursive. A nested directory is a structure kubeagent would
// have to invent a meaning for, and "the files I can see in this directory" is
// the meaning an operator already has.
//
// label names how the path reached this call, for the error: "--policy" for
// the flag Task 15 adds to scan and gate, or "" for a positional argument
// that already names itself. One walk, two callers, two wordings — inventing
// a second implementation to get the wording right would risk the two
// drifting apart on everything else.
func namedPolicyDocuments(paths []string, label string) ([]policy.Document, error) {
	var out []policy.Document
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", namePath(label, p), err)
		}
		if !info.IsDir() {
			doc, err := readPolicyFile(p, label)
			if err != nil {
				return nil, err
			}
			out = append(out, doc)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", namePath(label, p), err)
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			switch strings.ToLower(filepath.Ext(e.Name())) {
			case ".yaml", ".yml":
				names = append(names, e.Name())
			}
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("%s: no .yaml or .yml files in this directory", namePath(label, p))
		}
		// Name order, so the rule order — and the report — does not depend on
		// what the filesystem happens to return.
		sort.Strings(names)
		for _, n := range names {
			doc, err := readPolicyFile(filepath.Join(p, n), label)
			if err != nil {
				return nil, err
			}
			out = append(out, doc)
		}
	}
	return out, nil
}

// namePath formats a path for an error message: "--policy <path>" when label
// names the flag the path arrived through, or the bare path when it does not
// — a positional argument already names itself, so prefixing it with a flag
// the operator never typed would point at the wrong thing.
func namePath(label, path string) string {
	if label == "" {
		return path
	}
	return label + " " + path
}

// readPolicyFile reads one file into a document. Source is the path as the
// operator wrote it, because it reaches only an error on stderr, where naming
// the file is the whole point. label is passed straight through to namePath.
func readPolicyFile(path, label string) (policy.Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return policy.Document{}, fmt.Errorf("%s: %w", namePath(label, path), err)
	}
	return policy.Document{Source: path, Data: data}, nil
}

// policyDocuments reads every --policy path into a document internal/policy
// can load. It is a thin wrapper over namedPolicyDocuments naming the flag
// this signature is reached through — the one Task 15 adds to scan and gate.
func policyDocuments(paths []string) ([]policy.Document, error) {
	return namedPolicyDocuments(paths, "--policy")
}

// loadPolicy reads and validates the files the --policy flag Task 15 adds to
// scan and gate will name. It goes through policyDocuments, so a rejected
// path is reported the same way the flag itself reports it.
func loadPolicy(paths []string) ([]policy.Rule, error) {
	docs, err := policyDocuments(paths)
	if err != nil {
		return nil, err
	}
	return policy.Load(docs)
}

// runPolicyValidate checks policy files and prints a count. It contacts
// nothing: no cluster, no kubeconfig, no LLM. The count is all stdout gets —
// the paths stay in the error, which Main writes to stderr.
//
// Its argument is a positional file, not the --policy flag — that flag does
// not exist until Task 15 — so it goes through namedPolicyDocuments directly
// with an empty label rather than through policyDocuments/loadPolicy, whose
// wording names a flag this command does not have.
func runPolicyValidate(args []string, w io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s policy validate <file>…", invokedAs)
	}
	docs, err := namedPolicyDocuments(args, "")
	if err != nil {
		return err
	}
	rules, err := policy.Load(docs)
	if err != nil {
		return err
	}
	kinds := policy.Kinds(rules)
	fmt.Fprintf(w, "%s, %s\n",
		plural(len(rules), "rule", "rules"), plural(len(kinds), "kind", "kinds"))
	return nil
}

// unknownPackErr reports a pack name that policypack does not have, naming
// the packs that do exist so the operator can pick a real one. packDocuments
// and runPolicyPacks's --print both reach an unknown name through
// policypack.Bytes's ok return; sharing the wording keeps the two from
// quietly drifting apart on how they describe the same miss.
func unknownPackErr(name string) error {
	return fmt.Errorf("unknown policy pack %q (want %s)", name, strings.Join(policypack.Names(), ", "))
}

// requirePackBytes turns a pack's raw bytes into either the bytes unchanged
// or an error naming the pack. policy.Load treats empty or nil YAML as a
// valid, empty document, not an error — so an empty result passed through
// unchecked would silently run, list, or print zero rules under the pack's
// own name instead of failing loudly. No pack that ships is ever empty; this
// only fires if a registry entry and its embedded file drift apart.
func requirePackBytes(name string, data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("pack %q: embedded pack is empty (broken build)", name)
	}
	return data, nil
}

// packDocuments turns pack names into documents internal/policy can load.
// Source is "pack:<name>", not a path: a pack has no filesystem location, so
// there is none to reach an error message, a JSON document or a report.
//
// An unknown name is refused rather than skipped. Silently ignoring it would
// run fewer rules than the operator asked for and say nothing. So is a name
// that resolves but whose embedded bytes are empty — see requirePackBytes.
func packDocuments(names []string) ([]policy.Document, error) {
	var out []policy.Document
	for _, name := range names {
		data, ok := policypack.Bytes(name)
		if !ok {
			return nil, unknownPackErr(name)
		}
		data, err := requirePackBytes(name, data)
		if err != nil {
			return nil, err
		}
		out = append(out, policy.Document{Source: "pack:" + name, Data: data})
	}
	return out, nil
}

// runPolicyPacks lists the curated packs, or prints one when printName names
// it. It contacts nothing: no cluster, no kubeconfig, no network, and no model
// — the packs are compiled into the binary.
//
// The rule count is computed by loading rather than stored beside the name, so
// it cannot disagree with the file it describes.
func runPolicyPacks(args []string, printName string, w io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: %s policy packs [--print name]", invokedAs)
	}
	if printName != "" {
		data, ok := policypack.Bytes(printName)
		if !ok {
			return unknownPackErr(printName)
		}
		data, err := requirePackBytes(printName, data)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	for _, p := range policypack.All() {
		data, err := requirePackBytes(p.Name, mustPackBytes(p.Name))
		if err != nil {
			return err
		}
		rules, err := policy.Load([]policy.Document{{Source: "pack:" + p.Name, Data: data}})
		if err != nil {
			// The packs ship with the binary and their tests load every one,
			// so this is unreachable outside a broken build.
			return fmt.Errorf("pack %q: %w", p.Name, err)
		}
		fmt.Fprintf(w, "  %-14s %s — %s\n", p.Name, plural(len(rules), "rule", "rules"), p.Summary)
	}
	fmt.Fprintf(w, "\nPrint one to fork it:\n  %s policy packs --print <name>\n", invokedAs)
	return nil
}

// mustPackBytes reads a pack that policypack.All just named, so the lookup
// itself cannot miss. Returning nil rather than panicking on the impossible
// case means the caller — requirePackBytes — can turn a broken build into a
// load error that names the pack, rather than a bare panic or (since
// policy.Load treats empty or nil YAML as a valid, empty document) a
// healthy-looking zero.
func mustPackBytes(name string) []byte {
	data, _ := policypack.Bytes(name)
	return data
}

// newPolicyCommand builds `kubeagent policy validate`. Like `schema`, it keeps
// its own argument handling rather than cobra.MinimumNArgs(1), which would
// reword the usage error runPolicyValidate produces.
func newPolicyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "policy",
		Short:         "Work with policy files",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("usage: %s policy validate <file>… | %s policy packs [--print name]", invokedAs, invokedAs)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:           "validate <file>…",
		Short:         "Validate policy files without contacting a cluster",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPolicyValidate(args, os.Stdout)
		},
	})
	packs := &cobra.Command{
		Use:           "packs",
		Short:         "List the curated policy packs compiled into this binary",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	var printName string
	packs.Flags().StringVar(&printName, "print", "", "print this pack's rules as YAML instead of listing")
	packs.RunE = func(cmd *cobra.Command, args []string) error {
		return runPolicyPacks(args, printName, os.Stdout)
	}
	cmd.AddCommand(packs)
	return cmd
}

// plural picks the singular or plural spelling for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// evaluatePolicy is the whole --policy path, and the only one. scan and gate
// both call it, so neither can load a policy the other would reject, and
// neither can drop the unreadable set — which is the difference between "the
// rule passed" and "the rule never ran".
//
// Returns nil when no --policy was given, so a run without the flag renders
// exactly the bytes it rendered before the flag existed.
//
// Read-only toward the cluster: ReadPlan names the kinds, collect.PolicyObjects
// lists them, and nothing here writes. There is no --fix path from a policy.
func evaluatePolicy(ctx context.Context, paths []string, kubeconfig, contextName, namespace string) (*report.PolicyView, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	rules, err := loadPolicy(paths)
	if err != nil {
		return nil, err
	}
	// A dynamic client, because a policy selects kinds the typed collectors do
	// not cover. Same construction scan already uses for the advisory reads.
	dyn, _, err := cluster.NewDynamicClients(kubeconfig, contextName)
	if err != nil {
		return nil, err
	}
	objects, unreadable := collect.PolicyObjects(ctx, dyn, policy.ReadPlan(rules), namespace, scan.Workers())
	violations, notEvaluated := policy.Evaluate(rules, policy.InputsFrom(objects, unreadable))
	return &report.PolicyView{
		Rules: len(rules), Violations: violations, NotEvaluated: notEvaluated,
	}, nil
}
