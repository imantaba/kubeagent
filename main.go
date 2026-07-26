package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/alert"
	"github.com/imantaba/kubeagent/internal/audit"
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
		return fmt.Errorf("usage: kubeagent scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json] [--explain] [--investigate] [--model name] [--include-cron] [--include-restarts] [--pvc-reclaim] [--lint-secrets] [--security] [--security-verbose] [--disk-usage [--disk-threshold r]] [--kubelet-health] [--control-plane-health] [--dns-health] [--certs [--cert-warn-days n]] [--logs] [--node-heartbeat-threshold dur] [--expected-nodes a,b,…] [--fix [--dry-run|--yes] [--audit-log path]] [--rollback --audit-log path] | kubeagent watch [--kubeconfig path] [--context name (repeatable)] [--cluster-name name] [--include-local] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur] [--alert-format json|slack|alertmanager] [--alert-repeat dur] [--slo-target pct] [--explain [--explain-cooldown dur] [--explain-budget n] [--model name]] | kubeagent version")
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
	auditLog := fs.String("audit-log", "", "with --fix: append a JSON-lines audit record per action to this file")
	rollback := fs.Bool("rollback", false, "undo the most recent applied fix recorded in --audit-log (requires --audit-log)")
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
	if *rollback && *fix {
		return fmt.Errorf("--rollback and --fix are mutually exclusive")
	}
	if *rollback && *auditLog == "" {
		return fmt.Errorf("--rollback requires --audit-log (the file to read the last applied fix from)")
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
	if *fix || *rollback {
		var auditw *audit.Writer
		if *auditLog != "" {
			f, err := os.OpenFile(*auditLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("opening audit log %q: %w", *auditLog, err)
			}
			defer f.Close()
			auditw = audit.NewWriter(f)
		}
		if *rollback {
			if err := runRollback(context.Background(), client, *auditLog, *dryRun, *assumeYes, os.Stdout, os.Stdin, auditw); err != nil {
				return err
			}
		} else {
			runFixes(context.Background(), client, fixPlan, *dryRun, *assumeYes, os.Stdout, os.Stdin, auditw)
		}
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

// watchRun is the daemon entry point, indirected so tests can capture the
// watch.Config runWatch builds without starting a daemon. runWatch's config is
// otherwise unobservable: watch.Run blocks on informers until its context is
// cancelled, and that context comes from signal.NotifyContext, which no test
// can cancel.
var watchRun = watch.Run

// contextList collects a repeatable --context flag: one occurrence per cluster
// the daemon should watch.
type contextList []string

func (c *contextList) String() string { return strings.Join(*c, ",") }

func (c *contextList) Set(v string) error {
	if v == "" {
		return fmt.Errorf("--context cannot be empty")
	}
	*c = append(*c, v)
	return nil
}

// buildTargets resolves the flags to the clusters the daemon will watch.
// Building a client contacts no API server, so a failure here is a
// configuration error — a misspelled context — and it is fatal: an operator who
// asked for three clusters and silently got two is worse off than one whose
// daemon refused to start.
func buildTargets(kubeconfig, clusterName string, contexts []string, includeLocal bool) ([]watch.Target, error) {
	var targets []watch.Target
	if len(contexts) == 0 || includeLocal {
		client, err := cluster.NewInClusterOrKubeconfig(kubeconfig, "")
		if err != nil {
			return nil, err
		}
		targets = append(targets, watch.Target{Name: clusterName, Client: client})
	}
	for _, name := range contexts {
		client, err := cluster.NewClient(kubeconfig, name)
		if err != nil {
			return nil, err
		}
		targets = append(targets, watch.Target{Name: name, Client: client})
	}
	return targets, nil
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig for local dev (ignored in-cluster)")
	var contexts contextList
	fs.Var(&contexts, "context", "kubeconfig context to watch; repeat the flag to watch several clusters from one daemon")
	clusterName := fs.String("cluster-name", envOr("KUBEAGENT_CLUSTER_NAME", "local"), "name for the default cluster — the one watched when no --context is given; becomes its `cluster` metric label")
	includeLocal := fs.Bool("include-local", envBool("KUBEAGENT_INCLUDE_LOCAL", false), "also watch the default cluster alongside every --context (no-op when no --context is given)")
	metricsAddr := fs.String("metrics-addr", envOr("KUBEAGENT_METRICS_ADDR", ":8080"), "address for /metrics, /healthz, /readyz")
	heartbeat := fs.Duration("heartbeat", envDur("KUBEAGENT_HEARTBEAT", 60*time.Second), "safety-net full re-evaluation interval")
	debounce := fs.Duration("debounce", envDur("KUBEAGENT_DEBOUNCE", 2*time.Second), "coalescing window for change events")
	includeCron := fs.Bool("include-cron", false, "include CronJobs in the evaluation")
	includeRestarts := fs.Bool("include-restarts", false, "include workloads that are healthy now but have restarted")
	alertFormat := fs.String("alert-format", envOr("KUBEAGENT_ALERT_FORMAT", "json"), "alert payload format: json, slack, or alertmanager")
	alertRepeat := fs.Duration("alert-repeat", envDur("KUBEAGENT_ALERT_REPEAT", 0), "re-send interval for still-firing alerts (0 = the format default: 4h, or 60s for alertmanager)")
	sloTarget := fs.Float64("slo-target", envFloat("KUBEAGENT_SLO_TARGET", 0), "availability SLO as a percentage, e.g. 99.9 (0 = SLO tracking off)")
	explainFlag := fs.Bool("explain", envBool("KUBEAGENT_EXPLAIN", false), "explain new incidents via one LLM call each (needs ANTHROPIC_API_KEY, or KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model)")
	explainCooldown := fs.Duration("explain-cooldown", envDur("KUBEAGENT_EXPLAIN_COOLDOWN", time.Hour), "minimum gap between explanations for the same object (0 = no per-object gap)")
	explainBudget := fs.Int("explain-budget", envInt("KUBEAGENT_EXPLAIN_BUDGET", 20), "model calls per hour, and the burst capacity")
	model := fs.String("model", "", "model for --explain (default: $KUBEAGENT_MODEL or claude-opus-4-8; the local model name when KUBEAGENT_EXPLAIN_ENDPOINT is set)")
	var namespace string
	fs.StringVar(&namespace, "namespace", envOr("KUBEAGENT_NAMESPACE", ""), "namespace to watch (default: all)")
	fs.StringVar(&namespace, "n", envOr("KUBEAGENT_NAMESPACE", ""), "namespace to watch (shorthand)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The webhook URL is a credential (a Slack incoming-webhook URL is a bearer
	// token in URL form), so it comes from the environment only — never a flag,
	// which would put it in the pod spec's args and in `ps` output.
	alertURL := os.Getenv("KUBEAGENT_ALERT_WEBHOOK")
	repeat := *alertRepeat
	if repeat == 0 {
		repeat = alert.DefaultRepeat(alert.Format(*alertFormat))
	}
	if alertURL == "" && (*alertFormat != "json" || *alertRepeat != 0) {
		fmt.Fprintln(os.Stderr, "kubeagent: --alert-* flags ignored: KUBEAGENT_ALERT_WEBHOOK is not set, so alerting is off")
	}

	// --explain needs Anthropic, or a local OpenAI-compatible endpoint. Check
	// before connecting: a credential error must not surface as a daemon that
	// looks up and then silently never explains anything.
	explainEndpoint := os.Getenv("KUBEAGENT_EXPLAIN_ENDPOINT")
	var explainModel string
	if *explainFlag {
		if explainEndpoint == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
			return fmt.Errorf("--explain needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model")
		}
		if explainEndpoint != "" {
			explainModel = firstNonEmpty(*model, os.Getenv("KUBEAGENT_MODEL")) // no Anthropic default for a local model
			if explainModel == "" {
				return fmt.Errorf("--explain with KUBEAGENT_EXPLAIN_ENDPOINT needs --model (or KUBEAGENT_MODEL) set to the local model name")
			}
		} else {
			explainModel = explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))
		}
	}

	// The flag is a percentage because that is how an SRE writes an SLO; the
	// tracker works in ratios. Plain division isn't enough: 99.9/100 in
	// float64 lands on 0.9990000000000001, not 0.999, and metrics.go renders
	// gauges with %g, which prints the shortest string that round-trips the
	// double — so that exact artifact is what would go out on the wire as
	// kubeagent_slo_target_ratio. The numeric error is around 1e-16 and
	// changes no decision the burn-rate arithmetic makes, but the displayed
	// value is one no operator typed, so round to 8 decimal places, which
	// sits past six-nines (99.9999 -> 0.999999).
	//
	// Rounding is a display concern only, and must never change which side of
	// validateSLOTarget's two boundaries a target falls on: 0 is its explicit
	// "SLO tracking off" sentinel, and target >= 1 is rejected outright. A
	// target typed close enough to either boundary rounds past it at 8
	// places — 99.9999999 divides to 0.999999999, which rounds up to exactly
	// 1.0, and a tiny nonzero percentage divides to something that rounds
	// down to exactly 0 — so the rounded value is used only when it agrees
	// with the exact (unrounded) quotient about which side of both
	// boundaries it falls on. When it disagrees, the exact quotient is passed
	// through instead, unrounded, so validateSLOTarget always classifies the
	// value the operator actually typed rather than an artifact of display
	// rounding. (-0.0 == 0.0 in Go, so a negative target that rounds to -0
	// still compares equal to the exact-quotient's 0-ness here; it only takes
	// the fallback, and reaches validateSLOTarget unrounded, when the target
	// is nonzero — which is exactly when validateSLOTarget needs to see it to
	// reject it.)
	exact := *sloTarget / 100
	sloRatio := math.Round(exact*1e8) / 1e8
	if (sloRatio == 0) != (exact == 0) || (sloRatio >= 1) != (exact >= 1) {
		sloRatio = exact
	}

	targets, err := buildTargets(*kubeconfig, *clusterName, contexts, *includeLocal)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return watchRun(ctx, targets, watch.Config{
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
		AlertURL:                alertURL,
		AlertFormat:             *alertFormat,
		AlertRepeat:             repeat,
		SLOTarget:               sloRatio,
		Explain:                 *explainFlag,
		ExplainModel:            explainModel,
		ExplainEndpoint:         explainEndpoint,
		ExplainAPIKey:           os.Getenv("KUBEAGENT_EXPLAIN_API_KEY"),
		ExplainCooldown:         *explainCooldown,
		ExplainBudget:           *explainBudget,
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

// runRollback undoes the most recent applied remediation recorded in the audit log. The
// inverse action is derived deterministically (never LLM-decided) and applied through
// the same guard rails as any fix: preview, confirmation, drift bond, RBAC preflight.
func runRollback(ctx context.Context, client kubernetes.Interface, auditPath string, dryRun, assumeYes bool, w io.Writer, in io.Reader, auditw *audit.Writer) error {
	rec, found, err := audit.ReadLast(auditPath, func(r audit.Record) bool { return r.Disposition == "applied" })
	if err != nil {
		return fmt.Errorf("reading audit log %q: %w", auditPath, err)
	}
	if !found {
		fmt.Fprintf(w, "\nNo applied remediation found in %s; nothing to roll back.\n", auditPath)
		return nil
	}
	a, err := remediate.Inverse(rec.Kind, rec.Namespace, rec.Name, rec.FromRevision, rec.ToRevision)
	if err != nil {
		fmt.Fprintf(w, "\nCannot roll back the last applied fix (%s %s): %v\n", rec.Kind, rec.Target, err)
		return nil
	}
	logAudit := func(disposition, detail string) {
		if auditw == nil {
			return
		}
		if err := auditw.Log(audit.RecordFor(a, disposition, detail, time.Now())); err != nil {
			fmt.Fprintf(os.Stderr, "kubeagent: audit log write failed: %v\n", err)
		}
	}
	fmt.Fprintf(w, "\nRolling back the fix applied at %s\nProposed rollback: %s — %s\n  reason: %s\n",
		rec.Time, a.Target, a.Summary, a.Reason)
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
		logAudit("dry-run", "")
		return nil
	}
	if !assumeYes {
		fmt.Fprint(w, "  Roll back? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			fmt.Fprintln(w, "  skipped.")
			logAudit("declined", "")
			return nil
		}
	}
	res := remediate.Apply(ctx, client, a)
	switch {
	case res.Err != nil:
		fmt.Fprintf(w, "  ERROR: %v\n", res.Err)
		logAudit("error", res.Err.Error())
	case res.Applied:
		fmt.Fprintf(w, "  rolled back: %s\n", res.Detail)
		logAudit("rollback", res.Detail)
	case res.PreflightDenied:
		fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
		logAudit("preflight", res.Detail)
	default:
		fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
		logAudit("refused", res.Detail)
	}
	return nil
}

// runFixes proposes the planned remediations and, unless --dry-run, applies each
// after a [y/N] confirmation (or unconditionally with --yes). The actions were
// planned once in runScan; Apply is bound to what each preview promised. Writes
// are guarded inside remediate.Apply. auditw may be nil (no audit logging).
func runFixes(ctx context.Context, client kubernetes.Interface, actions []remediate.Action, dryRun, assumeYes bool, w io.Writer, in io.Reader, auditw *audit.Writer) {
	if len(actions) == 0 {
		fmt.Fprintln(w, "\nNo automatic remediations available.")
		return
	}
	logAudit := func(a remediate.Action, disposition, detail string) {
		if auditw == nil {
			return
		}
		if err := auditw.Log(audit.RecordFor(a, disposition, detail, time.Now())); err != nil {
			fmt.Fprintf(os.Stderr, "kubeagent: audit log write failed: %v\n", err)
		}
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
			allowed, reason, err := remediate.Preflight(ctx, client, a)
			switch {
			case err != nil:
				fmt.Fprintf(w, "  (dry-run: not applied; permission check errored: %v)\n", err)
				logAudit(a, "dry-run", "permission check errored: "+err.Error())
			case allowed:
				fmt.Fprintln(w, "  (dry-run: not applied; you have permission to apply this)")
				logAudit(a, "dry-run", "permission: allowed")
			default:
				fmt.Fprintf(w, "  (dry-run: not applied; would be blocked — %s)\n", reason)
				logAudit(a, "dry-run", reason)
			}
			continue
		}
		if !assumeYes {
			fmt.Fprint(w, "  Apply? [y/N] ")
			line, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) != "y" {
				fmt.Fprintln(w, "  skipped.")
				logAudit(a, "declined", "")
				continue
			}
		}
		res := remediate.Apply(ctx, client, a)
		switch {
		case res.Err != nil:
			fmt.Fprintf(w, "  ERROR: %v\n", res.Err)
			logAudit(a, "error", res.Err.Error())
		case res.Applied:
			fmt.Fprintf(w, "  applied: %s\n", res.Detail)
			logAudit(a, "applied", res.Detail)
		case res.PreflightDenied:
			fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
			logAudit(a, "preflight", res.Detail)
		default:
			fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
			logAudit(a, "refused", res.Detail)
		}
	}
}
