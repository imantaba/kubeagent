package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/fleet"
	"github.com/imantaba/kubeagent/internal/gate"
	"github.com/imantaba/kubeagent/internal/glob"
)

// fleetOptions is one field per flag, so parsing stays pure and testable and
// the flag surface can be asserted in a table.
type fleetOptions struct {
	kubeconfig     string
	contexts       []string
	allContexts    bool
	match          string
	failOn         string
	workers        int
	clusterTimeout time.Duration
	output         string
	namespace      string
}

func bindFleetFlags(cmd *cobra.Command, o *fleetOptions) {
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	cmd.Flags().StringArrayVar(&o.contexts, "context", nil, "kubeconfig context to sweep (repeatable)")
	cmd.Flags().BoolVar(&o.allContexts, "all-contexts", false, "sweep every context the kubeconfig defines")
	cmd.Flags().StringVar(&o.match, "match", "", "with --all-contexts: only contexts whose name matches this glob")
	cmd.Flags().StringVar(&o.failOn, "fail-on", "critical", "severity that fails the sweep: critical, warning or info")
	cmd.Flags().IntVar(&o.workers, "workers", envInt("KUBEAGENT_FLEET_WORKERS", 8), "clusters read concurrently")
	cmd.Flags().DurationVar(&o.clusterTimeout, "cluster-timeout", envDuration("KUBEAGENT_FLEET_CLUSTER_TIMEOUT", 60*time.Second), "per-cluster budget")
	cmd.Flags().StringVar(&o.output, "output", "text", "output format: text or json")
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "namespace to judge (default: all namespaces)")
}

// parseFleetFlags is pure: it builds a throwaway command, normalizes the
// single-dash long-flag spelling pflag would reject, and parses. It touches no
// kubeconfig and no cluster, which is what makes the flag surface testable
// without one.
func parseFleetFlags(args []string) (fleetOptions, error) {
	var o fleetOptions
	cmd := &cobra.Command{Use: "fleet"}
	bindFleetFlags(cmd, &o)
	if err := cmd.Flags().Parse(Normalize(args, longFlagLookup(cmd))); err != nil {
		return o, &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
	return o, nil
}

// validateFleetOptions checks the values that do not need a kubeconfig.
func validateFleetOptions(o fleetOptions) error {
	if o.output != "text" && o.output != "json" {
		return &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("unsupported output format %q: use text or json", o.output)}
	}
	if _, err := findings.Parse(o.failOn); err != nil {
		return &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("unsupported --fail-on %q: use critical, warning or info", o.failOn)}
	}
	// A non-positive budget is refused rather than read as "no deadline".
	// fleet.Sweep attaches a deadline only when the budget is positive, and the
	// worker pool returns only once every worker has returned — so one cluster
	// whose API server accepts the connection and then never answers would block
	// the sweep forever and render nothing at all, for any cluster. A hang with
	// no output is a worse answer than an error.
	if o.clusterTimeout <= 0 {
		return &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("--cluster-timeout must be positive, got %s: a sweep with no per-cluster budget can hang forever on one unresponsive API server", o.clusterTimeout)}
	}
	return nil
}

// selectContexts resolves the flag combination to the ordered list of context
// names to sweep. It is pure over the kubeconfig's context list, so every
// selection rule is unit-testable without a kubeconfig.
//
// Every failure here is exit 4: bad input, discovered before any cluster was
// touched. A selection that resolves to nothing is one of them — an empty sweep
// reporting "pass" would be the worst possible answer, because it looks like
// good news.
func selectContexts(all []cluster.ContextInfo, wanted []string, allContexts bool, match string) ([]string, error) {
	usage := func(format string, a ...any) error {
		return &exitError{code: gate.CodeUsage, msg: fmt.Sprintf(format, a...)}
	}

	switch {
	case match != "" && !allContexts:
		return nil, usage("--match needs --all-contexts: it filters the contexts a sweep would otherwise take all of")
	case len(wanted) > 0 && allContexts:
		return nil, usage("--context and --all-contexts cannot be combined: pick the contexts or take them all")
	}

	known := make(map[string]bool, len(all))
	for _, c := range all {
		known[c.Name] = true
	}

	if len(wanted) > 0 {
		for _, name := range wanted {
			if !known[name] {
				return nil, usage("unknown context %q: the kubeconfig does not define it", name)
			}
		}
		return wanted, nil
	}

	if !allContexts {
		for _, c := range all {
			if c.Current {
				return []string{c.Name}, nil
			}
		}
		return nil, usage("no context selected and the kubeconfig names no current context: pass --context or --all-contexts")
	}

	var selected []string
	for _, c := range all {
		if match == "" || glob.Match(match, c.Name) {
			selected = append(selected, c.Name)
		}
	}
	if len(selected) == 0 {
		return nil, usage("no context matches --match %q", match)
	}
	sort.Strings(selected)
	return selected, nil
}

// buildFleetTargets connects to each selected context.
//
// A client that cannot be built is fatal, the same ruling buildTargets makes for
// the watch daemon: an operator who asked for three hundred clusters and
// silently got two hundred and ninety is worse off than one whose sweep refused
// to start. This is the one place a kubeconfig path may be named — on stderr,
// the operator's own channel — and it is why the path never reaches
// internal/fleet at all.
func buildFleetTargets(kubeconfig string, names []string) ([]fleet.Target, error) {
	targets := make([]fleet.Target, 0, len(names))
	for _, name := range names {
		client, err := cluster.NewClient(kubeconfig, name)
		if err != nil {
			return nil, &exitError{code: gate.CodeUsage, msg: fmt.Sprintf("connecting to context %q: %v", name, err)}
		}
		targets = append(targets, fleet.Target{Name: name, Client: client})
	}
	return targets, nil
}

func runFleetOpts(o fleetOptions) error {
	if err := validateFleetOptions(o); err != nil {
		return err
	}
	level, _ := findings.Parse(o.failOn) // validated just above

	all, err := cluster.Contexts(o.kubeconfig)
	if err != nil {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
	names, err := selectContexts(all, o.contexts, o.allContexts, o.match)
	if err != nil {
		return err
	}
	targets, err := buildFleetTargets(o.kubeconfig, names)
	if err != nil {
		return err
	}

	rep := fleet.Sweep(context.Background(), targets, fleet.Options{
		FailOn:         level,
		Scan:           gateScanOptions(o.namespace),
		Workers:        o.workers,
		ClusterTimeout: o.clusterTimeout,
	})

	render := fleet.RenderText
	if o.output == "json" {
		render = fleet.RenderJSON
	}
	if err := render(os.Stdout, rep); err != nil {
		return err
	}
	if rep.Code != gate.CodePass {
		return &exitError{code: rep.Code}
	}
	return nil
}

func newFleetCommand() *cobra.Command {
	var o fleetOptions
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Sweep many clusters and report one verdict per cluster",
		Long: "Sweep every selected kubeconfig context in bounded parallel, running the same\n" +
			"evaluation `kubeagent gate` runs against each, and print one row per cluster\n" +
			"worst first. Read-only toward every cluster (get and list only, no write of any\n" +
			"kind), and it makes no model call — two separate promises. The report names\n" +
			"contexts and issue kinds — never a node, namespace, pod or workload.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetOpts(o)
		},
	}
	bindFleetFlags(cmd, &o)
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	})
	return cmd
}
