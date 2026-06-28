package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/report"
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
		return fmt.Errorf("usage: kubeagent scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json] [--explain] [--model name] [--include-cron] [--include-restarts] | kubeagent version")
	}

	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current-context)")
	output := fs.String("output", "text", "output format: text | json")
	explainFlag := fs.Bool("explain", false, "summarize findings via one Claude API call (needs ANTHROPIC_API_KEY)")
	model := fs.String("model", "", "Claude model for --explain (default: $KUBEAGENT_MODEL or claude-opus-4-8)")
	includeCron := fs.Bool("include-cron", false, "include CronJobs in the report")
	includeRestarts := fs.Bool("include-restarts", false, "include workloads that are healthy now but have restarted")
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

	inputs, err := collect.CollectInventory(context.Background(), client, namespace)
	if err != nil {
		return err
	}

	detectors := []diagnose.Detector{
		diagnose.CrashLoopDetector{},
		diagnose.ImagePullDetector{},
		diagnose.OOMKilledDetector{},
		diagnose.PendingDetector{},
	}
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods))
	workloads := inventory.Assemble(inputs, findings)

	nodes, err := collect.Nodes(context.Background(), client)
	if err != nil {
		return err
	}
	health := clusterhealth.Assess(nodes, workloads)
	health.ScopeNote = clusterhealth.NamespaceScopeNote(namespace)

	result := inventory.Prioritize(workloads, inventory.Opts{
		IncludeRestarts: *includeRestarts,
		IncludeCron:     *includeCron,
	})

	var explanation string
	if *explainFlag {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).ExplainInventory(ctx, health, nil, result.Workloads)
		if err != nil {
			return err
		}
	}

	return report.PrintInventory(health, result, nil, explanation, *output, os.Stdout)
}
