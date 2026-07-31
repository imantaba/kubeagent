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

func runRBACPrint(args []string) error {
	fs := flag.NewFlagSet("rbac print", flag.ContinueOnError)
	profile := fs.String("profile", "scan", "permission profile: scan | watch | full")
	features := fs.String("features", "", "comma-separated feature names, instead of a profile")
	roleName := fs.String("role-name", "kubeagent", "metadata.name of the printed ClusterRole")
	output := fs.String("output", "yaml", "output format: yaml | json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rules, err := selectedRules(*profile, *features)
	if err != nil {
		return err
	}
	switch *output {
	case "yaml":
		fmt.Fprint(os.Stdout, rbacprofile.RenderClusterRole(*roleName, rules))
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rbacprofile.RulesDocument{
			SchemaVersion: jsonschema.RBACVersion,
			RoleName:      *roleName,
			Rules:         rules,
		})
	default:
		return fmt.Errorf("unknown --output %q: want yaml or json", *output)
	}
	return nil
}

func runRBACCheck(args []string) error {
	fs := flag.NewFlagSet("rbac check", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current-context)")
	profile := fs.String("profile", "full", "permission profile: scan | watch | full")
	features := fs.String("features", "", "comma-separated feature names, instead of a profile")
	output := fs.String("output", "text", "output format: text | json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Validate up front, matching runRBACPrint: a typo in --output must fail
	// immediately, not after connecting to a cluster.
	if *output != "text" && *output != "json" {
		return fmt.Errorf("unknown --output %q: want text or json", *output)
	}
	selected, err := selectedFeatures(*profile, *features)
	if err != nil {
		return err
	}
	client, err := cluster.NewClient(*kubeconfig, *contextName)
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
	if *output == "json" {
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
			fmt.Fprintf(os.Stdout, "\n%d of %d features are blocked. Print the role they need:\n  %s rbac print --profile %s\n", blocked, len(statuses), invokedAs, *profile)
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
