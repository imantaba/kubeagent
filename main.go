package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/connectivity"
	"github.com/imantaba/kubeagent/internal/credlint"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/remediate"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/watch"
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
	if len(args) > 0 && args[0] == "watch" {
		return runWatch(args[1:])
	}
	if len(args) == 0 || args[0] != "scan" {
		return fmt.Errorf("usage: kubeagent scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json] [--explain] [--model name] [--include-cron] [--include-restarts] [--pvc-reclaim] [--lint-secrets] [--security] [--security-verbose] [--disk-usage [--disk-threshold r]] [--kubelet-health] [--node-heartbeat-threshold dur] [--expected-nodes a,b,…] [--fix [--dry-run|--yes]] | kubeagent watch [--kubeconfig path] [--context name] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur] | kubeagent version")
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
	pvcReclaimFull := fs.Bool("pvc-reclaim", false, "list every PVC on a Delete reclaim policy (default: a grouped summary)")
	diskUsage := fs.Bool("disk-usage", false, "check node filesystem and PVC usage via the kubelet (needs the nodes/proxy grant)")
	diskThreshold := fs.Float64("disk-threshold", 0.80, "with --disk-usage: warn at this used ratio (0-1)")
	kubeletHealth := fs.Bool("kubelet-health", false, "probe each kubelet's /healthz via nodes/proxy and flag unhealthy nodes (needs the nodes/proxy add-on)")
	nodeHeartbeatThreshold := fs.Duration("node-heartbeat-threshold", 40*time.Second, "flag a Ready node whose kubelet lease is stale beyond this (0 disables)")
	expectedNodes := fs.String("expected-nodes", "", "names of nodes expected in the cluster; a declared name with no Node object is flagged Degraded (comma-separated)")
	security := fs.Bool("security", false, "flag insecure workloads and exposed Services (read-only, advisory)")
	securityVerbose := fs.Bool("security-verbose", false, "with --security: list every finding per workload (default: dangerous findings in full, restricted gaps aggregated)")
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
		Namespace:              namespace,
		IncludeCron:            *includeCron,
		IncludeRestarts:        *includeRestarts,
		DiskUsage:              *diskUsage,
		DiskThreshold:          *diskThreshold,
		Security:               *security,
		NodeHeartbeatThreshold: *nodeHeartbeatThreshold,
		ExpectedNodes:          splitCSV(*expectedNodes),
		KubeletHealth:          *kubeletHealth,
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

	var diskRep *diskusage.Report
	if *diskUsage {
		diskRep = &res.DiskUsage
	}

	var kubeletRep *nodehealth.Report
	if *kubeletHealth {
		kubeletRep = &res.KubeletHealth
	}

	if err := report.PrintInventory(report.Input{
		Cluster:            health,
		Result:             result,
		Resources:          &summary,
		Platform:           &facts,
		ServiceIssues:      serviceIssues,
		CredentialWarnings: credWarnings,
		NodeReserve:        &res.NodeReserve,
		PVCReclaim:         &res.PVCReclaim,
		PVCReclaimFull:     *pvcReclaimFull,
		DiskUsage:          diskRep,
		KubeletHealth:      kubeletRep,
		IngressIssues:      res.IngressIssues,
		SecurityIssues:     res.SecurityIssues,
		SecurityVerbose:    *securityVerbose,
		Explanation:        explanation,
	}, *output, os.Stdout); err != nil {
		return err
	}
	if *fix {
		runFixes(context.Background(), client, result.Workloads, res.Inputs.ReplicaSets, nodes, *dryRun, *assumeYes, os.Stdout, os.Stdin)
	}
	return nil
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig for local dev (ignored in-cluster)")
	contextName := fs.String("context", "", "kubeconfig context for local dev")
	metricsAddr := fs.String("metrics-addr", envOr("KUBEAGENT_METRICS_ADDR", ":8080"), "address for /metrics, /healthz, /readyz")
	heartbeat := fs.Duration("heartbeat", envDur("KUBEAGENT_HEARTBEAT", 60*time.Second), "safety-net full re-evaluation interval")
	debounce := fs.Duration("debounce", envDur("KUBEAGENT_DEBOUNCE", 2*time.Second), "coalescing window for change events")
	includeCron := fs.Bool("include-cron", false, "include CronJobs in the evaluation")
	includeRestarts := fs.Bool("include-restarts", false, "include workloads that are healthy now but have restarted")
	var namespace string
	fs.StringVar(&namespace, "namespace", envOr("KUBEAGENT_NAMESPACE", ""), "namespace to watch (default: all)")
	fs.StringVar(&namespace, "n", envOr("KUBEAGENT_NAMESPACE", ""), "namespace to watch (shorthand)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := cluster.NewInClusterOrKubeconfig(*kubeconfig, *contextName)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return watch.Run(ctx, client, watch.Config{
		Namespace:              namespace,
		MetricsAddr:            *metricsAddr,
		Heartbeat:              *heartbeat,
		Debounce:               *debounce,
		IncludeCron:            *includeCron,
		IncludeRestarts:        *includeRestarts,
		DiskUsage:              envBool("KUBEAGENT_DISK_USAGE", false),
		DiskThreshold:          envFloat("KUBEAGENT_DISK_THRESHOLD", 0.80),
		NodeHeartbeatThreshold: envDur("KUBEAGENT_NODE_HEARTBEAT_THRESHOLD", 40*time.Second),
		ExpectedNodes:          splitCSV(envOr("KUBEAGENT_EXPECTED_NODES", "")),
	})
}

// splitCSV splits a comma-separated list into a slice, returning nil for empty.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// envOr returns the env var value if set, else def.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDur parses a duration env var, falling back to def on empty/invalid.
func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// envBool parses a boolean env var, falling back to def on empty/invalid.
func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// envFloat parses a float env var, falling back to def on empty/invalid.
func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
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
