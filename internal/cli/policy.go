package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/policy"
)

// policyDocuments reads every --policy path into a document internal/policy can
// load. The filesystem stops here: internal/policy takes bytes and a name, which
// is what keeps it importable by gate and mcp.
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
func policyDocuments(paths []string) ([]policy.Document, error) {
	var out []policy.Document
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("--policy %s: %w", p, err)
		}
		if !info.IsDir() {
			doc, err := readPolicyFile(p)
			if err != nil {
				return nil, err
			}
			out = append(out, doc)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, fmt.Errorf("--policy %s: %w", p, err)
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
			return nil, fmt.Errorf("--policy %s: no .yaml or .yml files in this directory", p)
		}
		// Name order, so the rule order — and the report — does not depend on
		// what the filesystem happens to return.
		sort.Strings(names)
		for _, n := range names {
			doc, err := readPolicyFile(filepath.Join(p, n))
			if err != nil {
				return nil, err
			}
			out = append(out, doc)
		}
	}
	return out, nil
}

// readPolicyFile reads one file into a document. Source is the path as the
// operator wrote it, because it reaches only an error on stderr, where naming
// the file is the whole point.
func readPolicyFile(path string) (policy.Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return policy.Document{}, fmt.Errorf("--policy %s: %w", path, err)
	}
	return policy.Document{Source: path, Data: data}, nil
}

// loadPolicy reads and validates the files named by --policy. Both scan and
// gate call it, so neither can load a policy the other would reject.
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
func runPolicyValidate(args []string, w io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s policy validate <file>…", invokedAs)
	}
	rules, err := loadPolicy(args)
	if err != nil {
		return err
	}
	kinds := policy.Kinds(rules)
	fmt.Fprintf(w, "%s, %s\n",
		plural(len(rules), "rule", "rules"), plural(len(kinds), "kind", "kinds"))
	return nil
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
			return fmt.Errorf("usage: %s policy validate <file>…", invokedAs)
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
	return cmd
}

// plural picks the singular or plural spelling for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
