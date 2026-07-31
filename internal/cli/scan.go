package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/imantaba/kubeagent/internal/advisory"
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
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/remediate"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/scan"
)

// scanOptions is `kubeagent scan`'s parsed command line. One field per flag,
// in declaration order. It exists so flag wiring is testable without a
// cluster: parseScanFlags is pure, and runScan does the I/O.
type scanOptions struct {
	kubeconfig             string
	contextName            string
	output                 string
	explain                bool
	investigate            bool
	model                  string
	includeCron            bool
	includeRestarts        bool
	lintSecrets            bool
	pvcReclaimFull         bool
	diskUsage              bool
	diskThreshold          float64
	kubeletHealth          bool
	controlPlaneHealth     bool
	dnsHealth              bool
	certs                  bool
	certWarnDays           int
	operators              bool
	drift                  bool
	driftAge               time.Duration
	capacity               bool
	logs                   bool
	nodeHeartbeatThreshold time.Duration
	expectedNodes          string
	security               bool
	securityVerbose        bool
	suggest                bool
	fix                    bool
	dryRun                 bool
	assumeYes              bool
	auditLog               string
	rollback               bool
	namespace              string
}

// parseScanFlags parses `kubeagent scan`'s command line. Pure: it reads the
// environment for the four env-defaulted flags and nothing else, contacts no
// cluster, and writes nothing.
func parseScanFlags(args []string) (scanOptions, error) {
	var o scanOptions
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&o.contextName, "context", "", "kubeconfig context to use (default: current-context)")
	fs.StringVar(&o.output, "output", "text", "output format: text | json | html")
	fs.BoolVar(&o.explain, "explain", false, "summarize findings via one LLM call (needs ANTHROPIC_API_KEY, or KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model)")
	fs.BoolVar(&o.investigate, "investigate", false, "agentic read-only investigation of findings via a bounded tool-use loop (needs ANTHROPIC_API_KEY; supersedes --explain)")
	fs.StringVar(&o.model, "model", "", "model for --explain / --investigate (default: $KUBEAGENT_MODEL or claude-opus-4-8; the local model name when KUBEAGENT_EXPLAIN_ENDPOINT is set)")
	fs.BoolVar(&o.includeCron, "include-cron", false, "include CronJobs in the report")
	fs.BoolVar(&o.includeRestarts, "include-restarts", false, "include workloads that are healthy now but have restarted")
	fs.BoolVar(&o.lintSecrets, "lint-secrets", false, "scan ConfigMaps and pod env for credentials stored in the clear (never prints values)")
	fs.BoolVar(&o.pvcReclaimFull, "pvc-reclaim", false, "list every PVC on a Delete reclaim policy (default: a grouped summary)")
	fs.BoolVar(&o.diskUsage, "disk-usage", false, "check node filesystem and PVC usage via the kubelet (needs the nodes/proxy grant)")
	fs.Float64Var(&o.diskThreshold, "disk-threshold", 0.80, "with --disk-usage: warn at this used ratio (0-1)")
	fs.BoolVar(&o.kubeletHealth, "kubelet-health", false, "probe each kubelet's /healthz via nodes/proxy and flag unhealthy nodes (needs the nodes/proxy add-on)")
	fs.BoolVar(&o.controlPlaneHealth, "control-plane-health", false, "probe the apiserver /readyz endpoint and flag an unhealthy control plane / etcd (needs the /readyz grant)")
	fs.BoolVar(&o.dnsHealth, "dns-health", false, "probe CoreDNS /metrics and flag an elevated SERVFAIL+REFUSED response ratio (needs the pods/proxy grant)")
	fs.BoolVar(&o.certs, "certs", false, "check TLS-secret certificate expiry (public certs only; needs the secrets add-on grant)")
	fs.IntVar(&o.certWarnDays, "cert-warn-days", 30, "with --certs: warn when a certificate expires within this many days")
	fs.BoolVar(&o.operators, "operators", envBool("KUBEAGENT_OPERATORS", false), "report operator custom-resource health (cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, Prometheus operator; advisory, needs deploy/rbac-operators.yaml on a restricted context)")
	fs.BoolVar(&o.drift, "drift", envBool("KUBEAGENT_DRIFT", false), "report GitOps convergence for Argo CD and Flux (advisory, needs deploy/rbac-gitops.yaml on a restricted context)")
	fs.DurationVar(&o.driftAge, "drift-age", envDuration("KUBEAGENT_DRIFT_AGE", time.Hour), "how long an object may differ from Git before --drift calls it stale (e.g. 30m, 2h)")
	fs.BoolVar(&o.capacity, "capacity", envBool("KUBEAGENT_CAPACITY", false), "report scheduling headroom and structurally wrong workload shapes (advisory; uses metrics-server for context when present)")
	fs.BoolVar(&o.logs, "logs", false, "read each crashing container's previous logs and classify the failure (needs the pods/log grant)")
	fs.DurationVar(&o.nodeHeartbeatThreshold, "node-heartbeat-threshold", 40*time.Second, "flag a Ready node whose kubelet lease is stale beyond this (0 disables)")
	fs.StringVar(&o.expectedNodes, "expected-nodes", "", "names of nodes expected in the cluster; a declared name with no Node object is flagged Degraded (comma-separated)")
	fs.BoolVar(&o.security, "security", false, "flag insecure workloads and exposed Services (read-only, advisory)")
	fs.BoolVar(&o.securityVerbose, "security-verbose", false, "with --security: list every finding per workload (default: dangerous findings in full, restricted gaps aggregated)")
	fs.BoolVar(&o.suggest, "suggest", false, "print a deterministic next-step suggestion (and a read-only kubectl command) under each finding")
	fs.BoolVar(&o.fix, "fix", false, "propose and (after confirmation) apply safe, reversible remediations (opt-in writes)")
	fs.BoolVar(&o.dryRun, "dry-run", false, "with --fix: print proposed remediations only; never prompt or write")
	fs.BoolVar(&o.assumeYes, "yes", false, "with --fix: apply all proposed remediations without prompting")
	fs.StringVar(&o.auditLog, "audit-log", "", "with --fix: append a JSON-lines audit record per action to this file")
	fs.BoolVar(&o.rollback, "rollback", false, "undo the most recent applied fix recorded in --audit-log (requires --audit-log)")
	fs.StringVar(&o.namespace, "namespace", "", "namespace to scan (default: all namespaces)")
	fs.StringVar(&o.namespace, "n", "", "namespace to scan (shorthand)")
	if err := fs.Parse(args); err != nil {
		return scanOptions{}, err
	}
	return o, nil
}

// runScan serves `kubeagent scan`. o is the already-parsed command line, as
// produced by parseScanFlags.
func runScan(o scanOptions) error {
	// Validate format up front so we fail fast, before touching the network.
	if o.output != "text" && o.output != "json" && o.output != "html" {
		return fmt.Errorf("unknown output format %q (want text, json or html)", o.output)
	}
	// --explain needs Anthropic, or a local OpenAI-compatible endpoint; check before scanning.
	explainEndpoint := os.Getenv("KUBEAGENT_EXPLAIN_ENDPOINT")
	if o.explain && explainEndpoint == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("--explain needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model")
	}
	// --investigate requires the Anthropic API key directly; local endpoints do not
	// support the tool-use loop in v1.
	if o.investigate && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("--investigate needs ANTHROPIC_API_KEY (local endpoints do not support the tool-use loop yet)")
	}
	if o.rollback && o.fix {
		return fmt.Errorf("--rollback and --fix are mutually exclusive")
	}
	if o.rollback && o.auditLog == "" {
		return fmt.Errorf("--rollback requires --audit-log (the file to read the last applied fix from)")
	}
	var explainModel string
	if explainEndpoint != "" {
		explainModel = firstNonEmpty(o.model, os.Getenv("KUBEAGENT_MODEL")) // no Anthropic default for a local model
		if o.explain && explainModel == "" {
			return fmt.Errorf("--explain with KUBEAGENT_EXPLAIN_ENDPOINT needs --model (or KUBEAGENT_MODEL) set to the local model name")
		}
	} else {
		explainModel = explain.ResolveModel(o.model, os.Getenv("KUBEAGENT_MODEL"))
	}

	client, err := cluster.NewClient(o.kubeconfig, o.contextName)
	if err != nil {
		return err
	}

	res, err := scan.Evaluate(context.Background(), client, scan.Options{
		Namespace:               o.namespace,
		IncludeCron:             o.includeCron,
		IncludeRestarts:         o.includeRestarts,
		DiskUsage:               o.diskUsage,
		DiskThreshold:           o.diskThreshold,
		Security:                o.security,
		NodeHeartbeatThreshold:  o.nodeHeartbeatThreshold,
		ExpectedNodes:           splitCSV(o.expectedNodes),
		KubeletHealth:           o.kubeletHealth,
		ControlPlaneHealth:      o.controlPlaneHealth,
		DNSHealth:               o.dnsHealth,
		DNSServfailRatio:        envFloat("KUBEAGENT_DNS_SERVFAIL_RATIO", 0.05),
		Certs:                   o.certs,
		CertWarnDays:            o.certWarnDays,
		Logs:                    o.logs,
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
		warnf(os.Stderr, "metrics unavailable: %v", metricsErr)
	}
	resourcePods, podsErr := advisory.ClusterPods(context.Background(), client, o.namespace, res.Inputs.Pods)
	if podsErr != nil {
		warnf(os.Stderr,
			"cluster-wide pod list unavailable: %s; "+
				"capacity headroom and the resources summary will be computed from "+
				"namespace %q only, overstating free capacity across the whole cluster",
			redact.Error(podsErr), o.namespace)
	}
	summary := resources.Summarize(nodes, resourcePods, usage)

	scs, _ := collect.StorageClasses(context.Background(), client)
	ics, _ := collect.IngressClasses(context.Background(), client)
	sysDS, _ := collect.SystemDaemonSets(context.Background(), client)
	facts := platform.Detect(nodes, sysDS, scs, ics)

	advRes := advisory.Assess(context.Background(), client,
		func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
			return cluster.NewDynamicClients(o.kubeconfig, o.contextName)
		},
		advisory.Inputs{
			Deployments:  res.Inputs.Deployments,
			StatefulSets: res.Inputs.StatefulSets,
			DaemonSets:   res.Inputs.DaemonSets,
			Jobs:         res.Inputs.Jobs,
			CronJobs:     res.Inputs.CronJobs,
			ReplicaSets:  res.Inputs.ReplicaSets,
			Nodes:        nodes,
			Pods:         resourcePods,
		},
		advisory.Options{
			Operators: o.operators,
			Drift:     o.drift,
			DriftAge:  o.driftAge,
			Capacity:  o.capacity,
			Namespace: o.namespace,
		}, time.Now())

	for _, d := range advRes.Degradations {
		warnf(os.Stderr, "%s unavailable: %s", d.Subject, d.Reason)
	}
	res.PartialReads = append(res.PartialReads, advisoryBlindSpots(advRes.Degradations)...)

	operatorRep := advRes.Operators
	gitopsRep := advRes.GitOps
	capacityRep := advRes.Capacity

	var explanation string
	var investigationReport investigate.Report
	switch {
	case o.investigate:
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		investigationReport, err = investigate.New(explain.ResolveModel(o.model, os.Getenv("KUBEAGENT_MODEL"))).
			Investigate(ctx, health, &summary, &facts, serviceIssues, result.Workloads, client)
		if err != nil {
			return err
		}
	case o.explain:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		explanation, err = explain.NewFromConfig(explainModel, explainEndpoint, os.Getenv("KUBEAGENT_EXPLAIN_API_KEY")).
			ExplainInventory(ctx, health, &summary, &facts, serviceIssues, result.Workloads)
		if err != nil {
			return err
		}
	}

	var credWarnings []credlint.Finding
	if o.lintSecrets {
		cms, _ := collect.ConfigMaps(context.Background(), client, o.namespace)
		credWarnings = credlint.Scan(cms, res.Inputs.Pods)
	}

	var diskRep *diskusage.Report
	if o.diskUsage {
		diskRep = &res.DiskUsage
	}

	var kubeletRep *nodehealth.Report
	if o.kubeletHealth {
		kubeletRep = &res.KubeletHealth
	}

	var cpRep *controlplane.Probe
	if o.controlPlaneHealth {
		cpRep = &res.ControlPlane
	}

	var dnsRep *dnshealth.Report
	if o.dnsHealth {
		dnsRep = &res.DNS
	}

	var fixPlan []remediate.Action
	if o.fix {
		fixPlan = remediate.Plan(result.Workloads, res.Inputs.ReplicaSets, nodes)
	}

	in := resultInput(res)
	// Presentation-layer extras that live only in runScan (clock, summaries,
	// flag-gated reports, credential/explain output).
	in.Now = time.Now()
	in.Resources = &summary
	in.Platform = &facts
	in.CredentialWarnings = credWarnings
	in.PVCReclaimFull = o.pvcReclaimFull
	in.DiskUsage = diskRep
	in.KubeletHealth = kubeletRep
	in.ControlPlane = cpRep
	in.DNS = dnsRep
	in.Operators = operatorRep
	in.GitOps = gitopsRep
	in.Capacity = capacityRep
	in.SecurityVerbose = o.securityVerbose
	in.Suggest = o.suggest
	in.Explanation = explanation
	in.Investigation = investigationReport.Narrative
	in.InvestigationConsulted = investigationReport.Consulted
	in.RemediationPlan = fixPlan
	if err := renderScan(os.Stdout, o.output, in, res, o.namespace); err != nil {
		return err
	}
	if o.fix || o.rollback {
		var auditw *audit.Writer
		if o.auditLog != "" {
			f, err := os.OpenFile(o.auditLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("opening audit log %q: %w", o.auditLog, err)
			}
			defer f.Close()
			auditw = audit.NewWriter(f)
		}
		if o.rollback {
			if err := runRollback(context.Background(), client, o.auditLog, o.dryRun, o.assumeYes, os.Stdout, os.Stdin, auditw); err != nil {
				return err
			}
		} else {
			runFixes(context.Background(), client, fixPlan, o.dryRun, o.assumeYes, os.Stdout, os.Stdin, auditw)
		}
	}
	return nil
}
