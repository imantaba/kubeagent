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

	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/connectivity"
	"github.com/imantaba/kubeagent/internal/controlplane"
	"github.com/imantaba/kubeagent/internal/credlint"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/dnshealth"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/investigate"
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
		return fmt.Errorf("usage: kubeagent scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json] [--explain] [--investigate] [--model name] [--include-cron] [--include-restarts] [--pvc-reclaim] [--lint-secrets] [--security] [--security-verbose] [--disk-usage [--disk-threshold r]] [--kubelet-health] [--control-plane-health] [--dns-health] [--certs [--cert-warn-days n]] [--logs] [--node-heartbeat-threshold dur] [--expected-nodes a,b,…] [--fix [--dry-run|--yes]] | kubeagent watch [--kubeconfig path] [--context name] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur] | kubeagent version")
	}

	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current-context)")
	output := fs.String("output", "text", "output format: text | json")
	explainFlag := fs.Bool("explain", false, "summarize findings via one LLM call (needs ANTHROPIC_API_KEY, or KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model)")
	investigateFlag := fs.Bool("investigate", false, "agentic read-only investigation of findings via a bounded tool-use loop (needs ANTHROPIC_API_KEY; supersedes --explain)")
	model := fs.String("model", "", "model for --explain / --investigate (default: $KUBEAGENT_MODEL or claude-opus-4-8; the local model name when KUBEAGENT_EXPLAIN_ENDPOINT is set)")
	includeCron := fs.Bool("include-cron", false, "include CronJobs in the report")
	includeRestarts := fs.Bool("include-restarts", false, "include workloads that are healthy now but have restarted")
	lintSecrets := fs.Bool("lint-secrets", false, "scan ConfigMaps and pod env for credentials stored in the clear (never prints values)")
	pvcReclaimFull := fs.Bool("pvc-reclaim", false, "list every PVC on a Delete reclaim policy (default: a grouped summary)")
	diskUsage := fs.Bool("disk-usage", false, "check node filesystem and PVC usage via the kubelet (needs the nodes/proxy grant)")
	diskThreshold := fs.Float64("disk-threshold", 0.80, "with --disk-usage: warn at this used ratio (0-1)")
	kubeletHealth := fs.Bool("kubelet-health", false, "probe each kubelet's /healthz via nodes/proxy and flag unhealthy nodes (needs the nodes/proxy add-on)")
	controlPlaneHealth := fs.Bool("control-plane-health", false, "probe the apiserver /readyz endpoint and flag an unhealthy control plane / etcd (needs the /readyz grant)")
	dnsHealth := fs.Bool("dns-health", false, "probe CoreDNS /metrics and flag an elevated SERVFAIL+REFUSED response ratio (needs the pods/proxy grant)")
	certs := fs.Bool("certs", false, "check TLS-secret certificate expiry (public certs only; needs the secrets add-on grant)")
	certWarnDays := fs.Int("cert-warn-days", 30, "with --certs: warn when a certificate expires within this many days")
	logs := fs.Bool("logs", false, "read each crashing container's previous logs and classify the failure (needs the pods/log grant)")
	nodeHeartbeatThreshold := fs.Duration("node-heartbeat-threshold", 40*time.Second, "flag a Ready node whose kubelet lease is stale beyond this (0 disables)")
	expectedNodes := fs.String("expected-nodes", "", "names of nodes expected in the cluster; a declared name with no Node object is flagged Degraded (comma-separated)")
	security := fs.Bool("security", false, "flag insecure workloads and exposed Services (read-only, advisory)")
	securityVerbose := fs.Bool("security-verbose", false, "with --security: list every finding per workload (default: dangerous findings in full, restricted gaps aggregated)")
	suggest := fs.Bool("suggest", false, "print a deterministic next-step suggestion (and a read-only kubectl command) under each finding")
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
	// --explain needs Anthropic, or a local OpenAI-compatible endpoint; check before scanning.
	explainEndpoint := os.Getenv("KUBEAGENT_EXPLAIN_ENDPOINT")
	if *explainFlag && explainEndpoint == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("--explain needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model")
	}
	// --investigate requires the Anthropic API key directly; local endpoints do not
	// support the tool-use loop in v1.
	if *investigateFlag && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("--investigate needs ANTHROPIC_API_KEY (local endpoints do not support the tool-use loop yet)")
	}
	var explainModel string
	if explainEndpoint != "" {
		explainModel = firstNonEmpty(*model, os.Getenv("KUBEAGENT_MODEL")) // no Anthropic default for a local model
		if *explainFlag && explainModel == "" {
			return fmt.Errorf("--explain with KUBEAGENT_EXPLAIN_ENDPOINT needs --model (or KUBEAGENT_MODEL) set to the local model name")
		}
	} else {
		explainModel = explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))
	}

	client, err := cluster.NewClient(*kubeconfig, *contextName)
	if err != nil {
		return err
	}

	res, err := scan.Evaluate(context.Background(), client, scan.Options{
		Namespace:               namespace,
		IncludeCron:             *includeCron,
		IncludeRestarts:         *includeRestarts,
		DiskUsage:               *diskUsage,
		DiskThreshold:           *diskThreshold,
		Security:                *security,
		NodeHeartbeatThreshold:  *nodeHeartbeatThreshold,
		ExpectedNodes:           splitCSV(*expectedNodes),
		KubeletHealth:           *kubeletHealth,
		ControlPlaneHealth:      *controlPlaneHealth,
		DNSHealth:               *dnsHealth,
		DNSServfailRatio:        envFloat("KUBEAGENT_DNS_SERVFAIL_RATIO", 0.05),
		Certs:                   *certs,
		CertWarnDays:            *certWarnDays,
		Logs:                    *logs,
		QuotaThreshold:          envFloat("KUBEAGENT_QUOTA_THRESHOLD", 0.90),
		WebhookTimeoutThreshold: int32(envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15)),
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
	var investigationReport investigate.Report
	switch {
	case *investigateFlag:
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		investigationReport, err = investigate.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).
			Investigate(ctx, health, &summary, &facts, serviceIssues, result.Workloads, client)
		if err != nil {
			return err
		}
	case *explainFlag:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		explanation, err = explain.NewFromConfig(explainModel, explainEndpoint, os.Getenv("KUBEAGENT_EXPLAIN_API_KEY")).
			ExplainInventory(ctx, health, &summary, &facts, serviceIssues, result.Workloads)
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

	var cpRep *controlplane.Probe
	if *controlPlaneHealth {
		cpRep = &res.ControlPlane
	}

	var dnsRep *dnshealth.Report
	if *dnsHealth {
		dnsRep = &res.DNS
	}

	var fixPlan []remediate.Action
	if *fix {
		fixPlan = remediate.Plan(result.Workloads, res.Inputs.ReplicaSets, nodes)
	}

	in := resultInput(res)
	// Presentation-layer extras that live only in runScan (clock, summaries,
	// flag-gated reports, credential/explain output).
	in.Now = time.Now()
	in.Resources = &summary
	in.Platform = &facts
	in.CredentialWarnings = credWarnings
	in.PVCReclaimFull = *pvcReclaimFull
	in.DiskUsage = diskRep
	in.KubeletHealth = kubeletRep
	in.ControlPlane = cpRep
	in.DNS = dnsRep
	in.SecurityVerbose = *securityVerbose
	in.Suggest = *suggest
	in.Explanation = explanation
	in.Investigation = investigationReport.Narrative
	in.InvestigationConsulted = investigationReport.Consulted
	in.RemediationPlan = fixPlan
	if err := report.PrintInventory(in, *output, os.Stdout); err != nil {
		return err
	}
	if *fix {
		runFixes(context.Background(), client, fixPlan, *dryRun, *assumeYes, os.Stdout, os.Stdin)
	}
	return nil
}

// resultInput maps every scan.Result-derived field onto a report.Input. Keeping
// this mapping in one testable place guards against a Result field silently never
// reaching the report — as StuckTerminating once did when only the inline literal
// carried the wiring. The presentation-layer extras (clock, resource summary,
// platform facts, flag-gated reports, credential/explain output) are filled in by
// the caller after this returns.
func resultInput(res scan.Result) report.Input {
	return report.Input{
		Cluster:          res.Health,
		Result:           res.Inventory,
		ServiceIssues:    res.ServiceIssues,
		NodeReserve:      &res.NodeReserve,
		PVCReclaim:       &res.PVCReclaim,
		IngressIssues:    res.IngressIssues,
		PVCIssues:        res.PVCIssues,
		SecurityIssues:   res.SecurityIssues,
		Certificates:     res.Certificates,
		StuckTerminating: res.StuckTerminating,
		PDBIssues:        res.PDBIssues,
		HPAIssues:        res.HPAIssues,
		WebhookIssues:    res.WebhookIssues,
		QuotaIssues:      res.QuotaIssues,
	}
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
		Namespace:               namespace,
		MetricsAddr:             *metricsAddr,
		Heartbeat:               *heartbeat,
		Debounce:                *debounce,
		IncludeCron:             *includeCron,
		IncludeRestarts:         *includeRestarts,
		DiskUsage:               envBool("KUBEAGENT_DISK_USAGE", false),
		DiskThreshold:           envFloat("KUBEAGENT_DISK_THRESHOLD", 0.80),
		QuotaThreshold:          envFloat("KUBEAGENT_QUOTA_THRESHOLD", 0.90),
		NodeHeartbeatThreshold:  envDur("KUBEAGENT_NODE_HEARTBEAT_THRESHOLD", 40*time.Second),
		ExpectedNodes:           splitCSV(envOr("KUBEAGENT_EXPECTED_NODES", "")),
		KubeletHealth:           envBool("KUBEAGENT_KUBELET_HEALTH", false),
		ControlPlaneHealth:      envBool("KUBEAGENT_CONTROL_PLANE_HEALTH", false),
		DNSHealth:               envBool("KUBEAGENT_DNS_HEALTH", false),
		DNSServfailRatio:        envFloat("KUBEAGENT_DNS_SERVFAIL_RATIO", 0.05),
		Certs:                   envBool("KUBEAGENT_CERTS", false),
		CertWarnDays:            envInt("KUBEAGENT_CERT_WARN_DAYS", 30),
		WebhookTimeoutThreshold: int32(envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15)),
	})
}

// firstNonEmpty returns a if non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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

// envInt returns the env var parsed as an int, else def.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// runFixes proposes the planned remediations and, unless --dry-run, applies each
// after a [y/N] confirmation (or unconditionally with --yes). The actions were
// planned once in runScan; Apply is bound to what each preview promised. Writes
// are guarded inside remediate.Apply.
func runFixes(ctx context.Context, client kubernetes.Interface, actions []remediate.Action, dryRun, assumeYes bool, w io.Writer, in io.Reader) {
	if len(actions) == 0 {
		fmt.Fprintln(w, "\nNo automatic remediations available.")
		return
	}
	reader := bufio.NewReader(in)
	for _, a := range actions {
		fmt.Fprintf(w, "\nProposed fix: %s — %s\n  reason: %s\n", a.Target, a.Summary, a.Reason)
		if len(a.Changes) > 0 {
			fmt.Fprintln(w, "  will change:")
			for _, c := range a.Changes {
				if c.From == "" && c.To == "" {
					fmt.Fprintf(w, "    %s\n", c.Field)
				} else {
					fmt.Fprintf(w, "    %s: %s → %s\n", c.Field, c.From, c.To)
				}
			}
		}
		fmt.Fprintf(w, "  kubectl equivalent: %s\n", a.KubectlEquivalent)
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
