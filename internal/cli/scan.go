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

// runScan serves `kubeagent scan`. args holds the flags following the "scan"
// token itself, matching every other subcommand's runX convention.
func runScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current-context)")
	output := fs.String("output", "text", "output format: text | json | html")
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
	operatorsFlag := fs.Bool("operators", envBool("KUBEAGENT_OPERATORS", false), "report operator custom-resource health (cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, Prometheus operator; advisory, needs deploy/rbac-operators.yaml on a restricted context)")
	driftFlag := fs.Bool("drift", envBool("KUBEAGENT_DRIFT", false), "report GitOps convergence for Argo CD and Flux (advisory, needs deploy/rbac-gitops.yaml on a restricted context)")
	driftAge := fs.Duration("drift-age", envDuration("KUBEAGENT_DRIFT_AGE", time.Hour), "how long an object may differ from Git before --drift calls it stale (e.g. 30m, 2h)")
	capacityFlag := fs.Bool("capacity", envBool("KUBEAGENT_CAPACITY", false), "report scheduling headroom and structurally wrong workload shapes (advisory; uses metrics-server for context when present)")
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Validate format up front so we fail fast, before touching the network.
	if *output != "text" && *output != "json" && *output != "html" {
		return fmt.Errorf("unknown output format %q (want text, json or html)", *output)
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
		warnf(os.Stderr, "metrics unavailable: %v", metricsErr)
	}
	resourcePods, podsErr := advisory.ClusterPods(context.Background(), client, namespace, res.Inputs.Pods)
	if podsErr != nil {
		warnf(os.Stderr,
			"cluster-wide pod list unavailable: %s; "+
				"capacity headroom and the resources summary will be computed from "+
				"namespace %q only, overstating free capacity across the whole cluster",
			redact.Error(podsErr), namespace)
	}
	summary := resources.Summarize(nodes, resourcePods, usage)

	scs, _ := collect.StorageClasses(context.Background(), client)
	ics, _ := collect.IngressClasses(context.Background(), client)
	sysDS, _ := collect.SystemDaemonSets(context.Background(), client)
	facts := platform.Detect(nodes, sysDS, scs, ics)

	advRes := advisory.Assess(context.Background(), client,
		func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
			return cluster.NewDynamicClients(*kubeconfig, *contextName)
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
			Operators: *operatorsFlag,
			Drift:     *driftFlag,
			DriftAge:  *driftAge,
			Capacity:  *capacityFlag,
			Namespace: namespace,
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
	in.Operators = operatorRep
	in.GitOps = gitopsRep
	in.Capacity = capacityRep
	in.SecurityVerbose = *securityVerbose
	in.Suggest = *suggest
	in.Explanation = explanation
	in.Investigation = investigationReport.Narrative
	in.InvestigationConsulted = investigationReport.Consulted
	in.RemediationPlan = fixPlan
	if err := renderScan(os.Stdout, *output, in, res, namespace); err != nil {
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
