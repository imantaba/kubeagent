package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/jsonschema"
	"github.com/imantaba/kubeagent/internal/rbacprofile"
)

// runRBAC dispatches the two rbac verbs. Standard-library flag only — v1 has no
// Cobra, so each verb owns its own FlagSet, the same shape runGate uses.
func runRBAC(args []string) error {
	if len(args) > 0 && args[0] == "print" {
		return runRBACPrint(args[1:])
	}
	if len(args) > 0 && args[0] == "check" {
		return runRBACCheck(args[1:])
	}
	return fmt.Errorf("usage: %[1]s rbac print [--profile scan|watch|full] [--features a,b,…] [--role-name name] [--output yaml|json] | %[1]s rbac check [--kubeconfig path] [--context name] [--profile scan|watch|full] [--features a,b,…] [--output text|json]", invokedAs)
}

// splitFeatureList turns "core, certs" into ["core", "certs"], tolerating the
// spaces a human types.
func splitFeatureList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// selectedFeatures resolves the --profile / --features pair to feature rows.
// --features wins when both are given: naming features is the more specific
// request.
func selectedFeatures(profile, features string) ([]rbacprofile.Feature, error) {
	var names []string
	if strings.TrimSpace(features) != "" {
		names = splitFeatureList(features)
	} else {
		p, ok := rbacprofile.ProfileByName(profile)
		if !ok {
			return nil, fmt.Errorf("unknown --profile %q: want scan, watch or full", profile)
		}
		names = p.Features
	}
	out := make([]rbacprofile.Feature, 0, len(names))
	for _, name := range names {
		f, ok := rbacprofile.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("unknown feature %q", name)
		}
		out = append(out, f)
	}
	return out, nil
}

// selectedRules resolves the same pair to the rules a ClusterRole should carry.
func selectedRules(profile, features string) ([]rbacprofile.Rule, error) {
	if strings.TrimSpace(features) != "" {
		return rbacprofile.Resolve(rbacprofile.Profile{Name: "custom", Features: splitFeatureList(features)})
	}
	p, ok := rbacprofile.ProfileByName(profile)
	if !ok {
		return nil, fmt.Errorf("unknown --profile %q: want scan, watch or full", profile)
	}
	return rbacprofile.Resolve(p)
}

// rbacPrintOptions is `kubeagent rbac print`'s parsed command line. One field
// per flag, in declaration order. It exists so flag wiring is testable
// without a cluster: parseRBACPrintFlags is pure, and runRBACPrintOpts does
// the I/O. rbac print never touches a cluster at all — the ClusterRole it
// renders comes entirely from internal/rbacprofile.
type rbacPrintOptions struct {
	profile  string
	features string
	roleName string
	output   string
}

// parseRBACPrintFlags parses `kubeagent rbac print`'s command line. Pure: it
// contacts no cluster and writes nothing.
func parseRBACPrintFlags(args []string) (rbacPrintOptions, error) {
	var o rbacPrintOptions
	fs := flag.NewFlagSet("rbac print", flag.ContinueOnError)
	fs.StringVar(&o.profile, "profile", "scan", "permission profile: scan | watch | full")
	fs.StringVar(&o.features, "features", "", "comma-separated feature names, instead of a profile")
	fs.StringVar(&o.roleName, "role-name", "kubeagent", "metadata.name of the printed ClusterRole")
	fs.StringVar(&o.output, "output", "yaml", "output format: yaml | json")
	if err := fs.Parse(args); err != nil {
		return rbacPrintOptions{}, err
	}
	return o, nil
}

// runRBACPrintOpts serves `kubeagent rbac print`. o is the already-parsed
// command line, as produced by parseRBACPrintFlags.
func runRBACPrintOpts(o rbacPrintOptions) error {
	rules, err := selectedRules(o.profile, o.features)
	if err != nil {
		return err
	}
	switch o.output {
	case "yaml":
		fmt.Fprint(os.Stdout, rbacprofile.RenderClusterRole(o.roleName, rules))
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rbacprofile.RulesDocument{
			SchemaVersion: jsonschema.RBACVersion,
			RoleName:      o.roleName,
			Rules:         rules,
		})
	default:
		return fmt.Errorf("unknown --output %q: want yaml or json", o.output)
	}
	return nil
}

func runRBACPrint(args []string) error {
	o, err := parseRBACPrintFlags(args)
	if err != nil {
		return err
	}
	return runRBACPrintOpts(o)
}

// rbacCheckOptions is `kubeagent rbac check`'s parsed command line. One field
// per flag, in declaration order. It exists so flag wiring is testable
// without a cluster: parseRBACCheckFlags is pure, and runRBACCheckOpts does
// the I/O.
type rbacCheckOptions struct {
	kubeconfig  string
	contextName string
	profile     string
	features    string
	output      string
}

// parseRBACCheckFlags parses `kubeagent rbac check`'s command line. Pure: it
// contacts no cluster and writes nothing.
func parseRBACCheckFlags(args []string) (rbacCheckOptions, error) {
	var o rbacCheckOptions
	fs := flag.NewFlagSet("rbac check", flag.ContinueOnError)
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&o.contextName, "context", "", "kubeconfig context to use (default: current-context)")
	fs.StringVar(&o.profile, "profile", "full", "permission profile: scan | watch | full")
	fs.StringVar(&o.features, "features", "", "comma-separated feature names, instead of a profile")
	fs.StringVar(&o.output, "output", "text", "output format: text | json")
	if err := fs.Parse(args); err != nil {
		return rbacCheckOptions{}, err
	}
	return o, nil
}

// runRBACCheckOpts serves `kubeagent rbac check`. o is the already-parsed
// command line, as produced by parseRBACCheckFlags.
func runRBACCheckOpts(o rbacCheckOptions) error {
	// Validate up front, matching runRBACPrint: a typo in --output must fail
	// immediately, not after connecting to a cluster.
	if o.output != "text" && o.output != "json" {
		return fmt.Errorf("unknown --output %q: want text or json", o.output)
	}
	selected, err := selectedFeatures(o.profile, o.features)
	if err != nil {
		return err
	}
	client, err := cluster.NewClient(o.kubeconfig, o.contextName)
	if err != nil {
		return err
	}
	statuses, err := rbacprofile.Check(context.Background(), client, selected)
	if err != nil {
		return err
	}
	blocked := 0
	for _, s := range statuses {
		if !s.Allowed {
			blocked++
		}
	}
	if o.output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rbacprofile.CheckDocument{
			SchemaVersion: jsonschema.RBACVersion,
			Features:      statuses,
		}); err != nil {
			return err
		}
	} else {
		for _, s := range statuses {
			label := s.Name
			if s.Flag != "" {
				label += " (" + s.Flag + ")"
			}
			if s.Allowed {
				fmt.Fprintf(os.Stdout, "  ok       %s\n", label)
				continue
			}
			fmt.Fprintf(os.Stdout, "  blocked  %s — needs %s\n", label, strings.Join(s.Missing, ", "))
		}
		if blocked == 0 {
			fmt.Fprintf(os.Stdout, "\nAll %d checked features are permitted.\n", len(statuses))
		} else {
			fmt.Fprintf(os.Stdout, "\n%d of %d features are blocked. Print the role they need:\n  %s rbac print --profile %s\n", blocked, len(statuses), invokedAs, o.profile)
		}
	}
	if blocked > 0 {
		// Exit 1 so a CI step can gate on it, the same contract `kubeagent gate`
		// offers. Nothing failed to run — the answer is simply "no". The
		// blocked count is already on stdout above (the text summary, or the
		// JSON results an operator can grep) — msg stays empty, the same
		// reason runGate's non-pass return does, so main() does not print a
		// second, differently-worded line to stderr on top of it.
		return &exitError{code: 1}
	}
	return nil
}

func runRBACCheck(args []string) error {
	o, err := parseRBACCheckFlags(args)
	if err != nil {
		return err
	}
	return runRBACCheckOpts(o)
}
