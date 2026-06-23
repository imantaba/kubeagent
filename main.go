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

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kubeagent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "scan" {
		return fmt.Errorf("usage: kubeagent scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json] [--explain] [--model name]")
	}

	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current-context)")
	output := fs.String("output", "text", "output format: text | json")
	explainFlag := fs.Bool("explain", false, "summarize findings via one Claude API call (needs ANTHROPIC_API_KEY)")
	model := fs.String("model", "", "Claude model for --explain (default: $KUBEAGENT_MODEL or claude-opus-4-8)")
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

	var explanation string
	if *explainFlag {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).ExplainInventory(ctx, workloads)
		if err != nil {
			return err
		}
	}

	return report.PrintInventory(clusterhealth.ClusterHealth{}, workloads, explanation, *output, os.Stdout)
}
