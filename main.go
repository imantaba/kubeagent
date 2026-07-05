package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/connectivity"
	"github.com/imantaba/kubeagent/internal/credlint"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/remediate"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/scan"
)

// version is the build version, overridden at release time via
// -ldflags "-X main.version=<tag>". Local/dev builds report "dev".
var version = "dev"

// versionLine is the one-line string printed by `kubeagent version`.
func versionLine() string {
	return "kubeagent " + version
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kubeagent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "version" {
		fmt.Fprintln(os.Stdout, versionLine())
		return nil
	}
	if len(args) == 0 || args[0] != "scan" {
		return fmt.Errorf("usage: kubeagent scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json] [--explain] [--model name] [--include-cron] [--include-restarts] [--lint-secrets] [--fix [--dry-run|--yes]] | kubeagent version")
	}

	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current-context)")
	output := fs.String("output", "text", "output format: text | json")
	explainFlag := fs.Bool("explain", false, "summarize findings via one Claude API call (needs ANTHROPIC_API_KEY)")
	model := fs.String("model", "", "Claude model for --explain (default: $KUBEAGENT_MODEL or claude-opus-4-8)")
	includeCron := fs.Bool("include-cron", false, "include CronJobs in the report")
	includeRestarts := fs.Bool("include-restarts", false, "include workloads that are healthy now but have restarted")
	lintSecrets := fs.Bool("lint-secrets", false, "scan ConfigMaps and pod env for credentials stored in the clear (never prints values)")
	fix := fs.Bool("fix", false, "propose and (after confirmation) apply safe, reversible remediations (opt-in writes)")
	dryRun := fs.Bool("dry-run", false, "with --fix: print proposed remediations only; never prompt or write")
	assumeYes := fs.Bool("yes", false, "with --fix: apply all proposed remediations without prompting")
	var namespace string
	fs.StringVar(&namespace, "namespace", "", "namespace to scan (default: all namespaces)")
	fs.StringVar(&namespace, "n", "", "namespace to scan (shorthand)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	// Validate format up front so we fail fast, before touching the network.
	if *output != "text" && *output != "json" {
		return fmt.Errorf("unknown output format %q (want text or json)", *output)
	}
	// --explain needs an API key; check before running a full scan.
	if *explainFlag && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("--explain needs the ANTHROPIC_API_KEY environment variable")
	}

	client, err := cluster.NewClient(*kubeconfig, *contextName)
	if err != nil {
		return err
	}

	res, err := scan.Evaluate(context.Background(), client, scan.Options{
		Namespace:       namespace,
		IncludeCron:     *includeCron,
		IncludeRestarts: *includeRestarts,
	})
	if err != nil {
		if diag, ok := connectivity.Diagnose(err); ok {
			return fmt.Errorf("%s\ndetails: %w", diag, err)
		}
		return err
	}
	health := res.Health
	result := res.Inventory
	serviceIssues := res.ServiceIssues
	nodes := res.Nodes

	usage, _, metricsErr := collect.NodeMetrics(context.Background(), client)
	if metricsErr != nil {
		fmt.Fprintf(os.Stderr, "kubeagent: warning: metrics unavailable: %v\n", metricsErr)
	}
	resourcePods := res.Inputs.Pods
	if namespace != "" {
		if all, perr := collect.AllPods(context.Background(), client); perr == nil {
			resourcePods = all
		}
	}
	summary := resources.Summarize(nodes, resourcePods, usage)

	scs, _ := collect.StorageClasses(context.Background(), client)
	ics, _ := collect.IngressClasses(context.Background(), client)
	sysDS, _ := collect.SystemDaemonSets(context.Background(), client)
	facts := platform.Detect(nodes, sysDS, scs, ics)

	var explanation string
	if *explainFlag {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).ExplainInventory(ctx, health, &summary, &facts, serviceIssues, result.Workloads)
		if err != nil {
			return err
		}
	}

	var credWarnings []credlint.Finding
	if *lintSecrets {
		cms, _ := collect.ConfigMaps(context.Background(), client, namespace)
		credWarnings = credlint.Scan(cms, res.Inputs.Pods)
	}

	if err := report.PrintInventory(health, result, &summary, &facts, serviceIssues, credWarnings, explanation, *output, os.Stdout); err != nil {
		return err
	}
	if *fix {
		runFixes(context.Background(), client, result.Workloads, res.Inputs.ReplicaSets, nodes, *dryRun, *assumeYes, os.Stdout, os.Stdin)
	}
	return nil
}

// runFixes proposes the planned remediations and, unless --dry-run, applies each
// after a [y/N] confirmation (or unconditionally with --yes). Writes are guarded
// inside remediate.Apply.
func runFixes(ctx context.Context, client kubernetes.Interface, workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, nodes []corev1.Node, dryRun, assumeYes bool, w io.Writer, in io.Reader) {
	actions := remediate.Plan(workloads, replicaSets, nodes)
	if len(actions) == 0 {
		fmt.Fprintln(w, "\nNo automatic remediations available.")
		return
	}
	reader := bufio.NewReader(in)
	for _, a := range actions {
		fmt.Fprintf(w, "\nProposed fix: %s — %s\n  reason: %s\n  kubectl equivalent: %s\n",
			a.Target, a.Summary, a.Reason, a.KubectlEquivalent)
		if dryRun {
			fmt.Fprintln(w, "  (dry-run: not applied)")
			continue
		}
		if !assumeYes {
			fmt.Fprint(w, "  Apply? [y/N] ")
			line, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) != "y" {
				fmt.Fprintln(w, "  skipped.")
				continue
			}
		}
		res := remediate.Apply(ctx, client, a)
		switch {
		case res.Err != nil:
			fmt.Fprintf(w, "  ERROR: %v\n", res.Err)
		case res.Applied:
			fmt.Fprintf(w, "  applied: %s\n", res.Detail)
		default:
			fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
		}
	}
}
