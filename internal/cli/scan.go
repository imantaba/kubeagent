package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/imantaba/kubeagent/internal/advisory"
	"github.com/imantaba/kubeagent/internal/audit"
	"github.com/imantaba/kubeagent/internal/baseline"
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
	policyPaths            []string
	policyPackNames        []string
	baselinePath           string
	baselineFactor         float64
	baselineFloor          float64
	logs                   bool
	nodeHeartbeatThreshold time.Duration
	expectedNodes          []string
	security               bool
	securityVerbose        bool
	suggest                bool
	why                    bool
	fix                    bool
	dryRun                 bool
	assumeYes              bool
	auditLog               string
	rollback               bool
	namespace              string
}

// bindScanFlags declares scan's flags on cmd, writing into o. Flag names,
// defaults and usage strings are unchanged from the standard-library FlagSet
// this replaces, except --namespace/-n: the standard library needed two
// separate declarations to accept both spellings, pflag expresses that as one
// flag with a shorthand, and only the long form's usage text survives.
//
// The four env-defaulted flags (--operators, --drift, --drift-age,
// --capacity) keep their envBool/envDuration default expressions verbatim;
// those are evaluated when the command is built, same as before.
func bindScanFlags(cmd *cobra.Command, o *scanOptions) {
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	f.StringVar(&o.contextName, "context", "", "kubeconfig context to use (default: current-context)")
	f.StringVar(&o.output, "output", "text", "output format: text | json | html")
	f.BoolVar(&o.explain, "explain", false, "summarize findings via one LLM call (needs ANTHROPIC_API_KEY, or KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model)")
	f.BoolVar(&o.investigate, "investigate", false, "agentic read-only investigation of findings (ANTHROPIC_API_KEY: bounded tool-use loop; else KUBEAGENT_EXPLAIN_ENDPOINT: local-model verdicts over pre-fetched evidence; supersedes --explain)")
	f.StringVar(&o.model, "model", "", "model for --explain / --investigate (default: $KUBEAGENT_MODEL, else claude-opus-4-8). With KUBEAGENT_EXPLAIN_ENDPOINT set, --explain takes the local model name here instead; --investigate does too when ANTHROPIC_API_KEY is not set (required, no default) and otherwise still sends this value to the Anthropic API.")
	f.BoolVar(&o.includeCron, "include-cron", false, "include CronJobs in the report")
	f.BoolVar(&o.includeRestarts, "include-restarts", false, "include workloads that are healthy now but have restarted")
	f.BoolVar(&o.lintSecrets, "lint-secrets", false, "scan ConfigMaps and pod env for credentials stored in the clear (never prints values)")
	f.BoolVar(&o.pvcReclaimFull, "pvc-reclaim", false, "list every PVC on a Delete reclaim policy (default: a grouped summary)")
	f.BoolVar(&o.diskUsage, "disk-usage", false, "check node filesystem and PVC usage via the kubelet (needs the nodes/proxy grant)")
	f.Float64Var(&o.diskThreshold, "disk-threshold", 0.80, "with --disk-usage: warn at this used ratio (0-1)")
	f.BoolVar(&o.kubeletHealth, "kubelet-health", false, "probe each kubelet's /healthz via nodes/proxy and flag unhealthy nodes (needs the nodes/proxy add-on)")
	f.BoolVar(&o.controlPlaneHealth, "control-plane-health", false, "probe the apiserver /readyz endpoint and flag an unhealthy control plane / etcd (needs the /readyz grant)")
	f.BoolVar(&o.dnsHealth, "dns-health", false, "probe CoreDNS /metrics and flag an elevated SERVFAIL+REFUSED response ratio (needs the pods/proxy grant)")
	f.BoolVar(&o.certs, "certs", false, "check TLS-secret certificate expiry (public certs only; needs the secrets add-on grant)")
	f.IntVar(&o.certWarnDays, "cert-warn-days", 30, "with --certs: warn when a certificate expires within this many days")
	f.BoolVar(&o.operators, "operators", envBool("KUBEAGENT_OPERATORS", false), "report operator custom-resource health (cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, Prometheus operator; advisory, needs deploy/rbac-operators.yaml on a restricted context)")
	f.BoolVar(&o.drift, "drift", envBool("KUBEAGENT_DRIFT", false), "report GitOps convergence for Argo CD and Flux (advisory, needs deploy/rbac-gitops.yaml on a restricted context)")
	f.DurationVar(&o.driftAge, "drift-age", envDuration("KUBEAGENT_DRIFT_AGE", time.Hour), "how long an object may differ from Git before --drift calls it stale (e.g. 30m, 2h)")
	f.BoolVar(&o.capacity, "capacity", envBool("KUBEAGENT_CAPACITY", false), "report scheduling headroom and structurally wrong workload shapes (advisory; uses metrics-server for context when present)")
	f.StringArrayVar(&o.policyPaths, "policy", nil, "evaluate organization-specific checks from this policy file or directory (repeatable)")
	f.StringArrayVar(&o.policyPackNames, "policy-pack", nil, "evaluate a curated rule pack compiled into kubeagent (repeatable; see `kubeagent policy packs`)")
	f.StringVar(&o.baselinePath, "baseline", "", "compare restart rates against this captured baseline (see "+invokedAs+" baseline capture)")
	f.Float64Var(&o.baselineFactor, "baseline-factor", envFloat("KUBEAGENT_BASELINE_FACTOR", baseline.DefaultFactor),
		"with --baseline: flag a workload at this multiple of its baseline rate (KUBEAGENT_BASELINE_FACTOR)")
	f.Float64Var(&o.baselineFloor, "baseline-floor", envFloat("KUBEAGENT_BASELINE_FLOOR", baseline.DefaultFloor),
		"with --baseline: also require this absolute rise in restarts/hour (KUBEAGENT_BASELINE_FLOOR)")
	f.BoolVar(&o.logs, "logs", false, "read each crashing container's previous logs and classify the failure (needs the pods/log grant)")
	f.DurationVar(&o.nodeHeartbeatThreshold, "node-heartbeat-threshold", 40*time.Second, "flag a Ready node whose kubelet lease is stale beyond this (0 disables)")
	f.StringSliceVar(&o.expectedNodes, "expected-nodes", nil, "names of nodes expected in the cluster; a declared name with no Node object is flagged Degraded (comma-separated)")
	f.BoolVar(&o.security, "security", false, "flag insecure workloads and exposed Services (read-only, advisory)")
	f.BoolVar(&o.securityVerbose, "security-verbose", false, "with --security: list every finding per workload (default: dangerous findings in full, restricted gaps aggregated)")
	f.BoolVar(&o.suggest, "suggest", false, "print a deterministic next-step suggestion (and a read-only kubectl command) under each finding")
	f.BoolVar(&o.why, "why", false, "print the root-cause hypothesis trace under each workload: every candidate cause considered, with its verdict and evidence")
	f.BoolVar(&o.fix, "fix", false, "propose and (after confirmation) apply safe, reversible remediations (opt-in writes)")
	f.BoolVar(&o.dryRun, "dry-run", false, "with --fix: print proposed remediations only; never prompt or write")
	f.BoolVar(&o.assumeYes, "yes", false, "with --fix: apply all proposed remediations without prompting")
	f.StringVar(&o.auditLog, "audit-log", "", "with --fix: append a JSON-lines audit record per action to this file")
	f.BoolVar(&o.rollback, "rollback", false, "undo the most recent applied fix recorded in --audit-log (requires --audit-log)")
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace to scan (default: all namespaces)")
}

// parseScanFlags parses `kubeagent scan`'s command line. Pure: it reads the
// environment for the four env-defaulted flags and nothing else, contacts no
// cluster, and writes nothing. It builds a throwaway command so the flag
// declarations have exactly one home, in bindScanFlags.
func parseScanFlags(args []string) (scanOptions, error) {
	var o scanOptions
	cmd := &cobra.Command{Use: "scan", SilenceErrors: true, SilenceUsage: true}
	bindScanFlags(cmd, &o)
	if err := cmd.Flags().Parse(Normalize(args, longFlagLookup(cmd))); err != nil {
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
	// diskThreshold gates diskusage.Assess's r >= threshold test, a ratio in
	// (0, 1]. 1.0 is valid ("warn only at full"); 0 is not — "warn on
	// everything" is not a threshold — and this is also the check that
	// catches "80" typed as a percent instead of a fraction.
	if o.diskThreshold <= 0 || o.diskThreshold > 1 {
		return fmt.Errorf("--disk-threshold must be a fraction in (0, 1], got %v", o.diskThreshold)
	}
	// 0 keeps meaning "disabled" — internal/clusterhealth's own Threshold <= 0
	// guard is what stays correct at the package boundary. A negative value is
	// never intentional, and left unchecked it silently disables the check the
	// same way 0 does, only without saying so.
	if o.nodeHeartbeatThreshold < 0 {
		return fmt.Errorf("--node-heartbeat-threshold cannot be negative (got %s; use 0 to disable the check)", o.nodeHeartbeatThreshold)
	}
	// 0 keeps meaning "expired only" — a real, deliberately narrow warn
	// window, not "disabled" and not "use the 30-day default" — so only a
	// negative value is refused here.
	if o.certWarnDays < 0 {
		return fmt.Errorf("--cert-warn-days must not be negative (got %d)", o.certWarnDays)
	}
	// Every declared expected-node name must be a real node-name shape: a
	// name that could never match a Node object describes nothing real.
	// cleanExpected's trim/dedup/sort runs after this, not instead of it.
	// pflag's StringSliceVar has already comma-split and accumulated every
	// --expected-nodes occurrence into o.expectedNodes.
	if err := validateExpectedNodes(o.expectedNodes, "--expected-nodes"); err != nil {
		return err
	}
	// --security-verbose only changes how a security finding is rendered; with
	// no --security there is no security section for it to change, so the flag
	// on its own is a mistake worth naming rather than a silent no-op.
	if o.securityVerbose && !o.security {
		return fmt.Errorf("--security-verbose requires --security")
	}
	// --explain needs Anthropic, or a local OpenAI-compatible endpoint; check
	// before scanning. --investigate supersedes --explain (checked below,
	// separately), so this guard is --explain's alone: with both flags set,
	// the --investigate guard is what must fire, not this one.
	explainEndpoint := os.Getenv("KUBEAGENT_EXPLAIN_ENDPOINT")
	if !o.investigate && o.explain && explainEndpoint == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("--explain needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model")
	}
	// --investigate needs a model: the Anthropic key selects the tool-use
	// loop; without it, a local OpenAI-compatible endpoint selects the
	// evidence-first verdict mode, which needs the local model's name.
	if o.investigate && os.Getenv("ANTHROPIC_API_KEY") == "" {
		if explainEndpoint == "" {
			return fmt.Errorf("--investigate needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model")
		}
		if firstNonEmpty(o.model, os.Getenv("KUBEAGENT_MODEL")) == "" {
			return fmt.Errorf("--investigate with KUBEAGENT_EXPLAIN_ENDPOINT needs --model (or KUBEAGENT_MODEL) set to the local model name")
		}
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
		// The --explain error below must not fire when --investigate selected
		// the model path: local verdict mode has its own guard above, with its
		// own message naming --investigate.
		if !o.investigate && o.explain && explainModel == "" {
			return fmt.Errorf("--explain with KUBEAGENT_EXPLAIN_ENDPOINT needs --model (or KUBEAGENT_MODEL) set to the local model name")
		}
	} else {
		explainModel = explain.ResolveModel(o.model, os.Getenv("KUBEAGENT_MODEL"))
	}

	// Load before connecting: an unreadable or wrong-version baseline is bad
	// input, and nothing about the cluster should have been attempted when the
	// run fails on it.
	baselineDoc, err := loadBaseline(o.baselinePath)
	if err != nil {
		return err
	}

	// The API server itself refuses a webhook timeoutSeconds above 30, so a
	// threshold above 30 could only ever match nothing; refuse it here
	// rather than report a clean posture that was never actually checked.
	webhookTimeout, err := envIntRange("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15, 1, 30)
	if err != nil {
		return err
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
		ExpectedNodes:           o.expectedNodes,
		KubeletHealth:           o.kubeletHealth,
		ControlPlaneHealth:      o.controlPlaneHealth,
		DNSHealth:               o.dnsHealth,
		DNSServfailRatio:        dnsRatioFromEnv(os.Stderr),
		Certs:                   o.certs,
		CertWarnDays:            o.certWarnDays,
		Logs:                    o.logs,
		QuotaThreshold:          quotaThresholdFromEnv(os.Stderr),
		WebhookTimeoutThreshold: int32(webhookTimeout),
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

	policyView, err := evaluatePolicy(context.Background(), o.policyPaths, o.policyPackNames, o.kubeconfig, o.contextName, o.namespace)
	if err != nil {
		return err
	}

	operatorRep := advRes.Operators
	gitopsRep := advRes.GitOps
	capacityRep := advRes.Capacity

	// An --investigate or --explain failure is never fatal to the scan (R223):
	// the deterministic report below still renders on stdout with exit 0, and
	// runModelPath reduces the failure to one stderr notice naming which flag
	// failed and why (via enrichmentFailure, R227) instead of returning an
	// error. A narrative-less investigation is covered by the same rule
	// (R220): it reaches runModelPath as just another error and gets the
	// same notice-not-failure treatment, so the report renders with no
	// Investigation section rather than nothing at all.
	modelRes := runModelPath(o,
		func() (investigate.Report, error) {
			if os.Getenv("ANTHROPIC_API_KEY") == "" && explainEndpoint != "" {
				// Local verdict mode: a small model adjudicating pre-fetched
				// evidence needs more wall clock than one Anthropic tool loop.
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
				defer cancel()
				return investigate.NewLocal(explainEndpoint, explainModel, os.Getenv("KUBEAGENT_EXPLAIN_API_KEY")).
					Investigate(ctx, health, &summary, &facts, serviceIssues, result.Workloads, client)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			return investigate.New(explain.ResolveModel(o.model, os.Getenv("KUBEAGENT_MODEL"))).
				Investigate(ctx, health, &summary, &facts, serviceIssues, result.Workloads, client)
		},
		func() (explain.Explanation, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return explain.NewFromConfig(explainModel, explainEndpoint, os.Getenv("KUBEAGENT_EXPLAIN_API_KEY")).
				ExplainInventory(ctx, health, &summary, &facts, serviceIssues, result.Workloads)
		},
	)
	if modelRes.notice != "" {
		warnf(os.Stderr, "%s", modelRes.notice)
	}
	explanation := modelRes.explanation
	investigationReport := modelRes.investigation

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

	baselineRep := baselineReport(baselineDoc, o.baselineFactor, o.baselineFloor, res.Inputs, time.Now())

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
	in.Policy = policyView
	in.Baseline = baselineRep
	in.SecurityVerbose = o.securityVerbose
	in.SecurityRequested = o.security
	in.Suggest = o.suggest
	in.Why = o.why
	in.Explanation = explanation
	in.ExplanationTruncated = modelRes.explanationTruncated
	in.Investigation = investigationReport.Narrative
	in.InvestigationConsulted = investigationReport.Consulted
	in.InvestigationTruncated = investigationReport.Truncated
	// InvestigationSkipped names the one case that is otherwise
	// indistinguishable from --investigate never having been passed: with
	// o.investigate set, runModelPath always enters the investigate arm
	// above; a failure sets modelRes.notice (handled above, and leaves
	// investigationReport zero); and a success with an empty narrative is
	// impossible because Investigate itself returns an error rather than an
	// empty report for that case. So this conjunction is exactly the skip
	// and nothing else.
	in.InvestigationSkipped = o.investigate && modelRes.notice == "" && investigationReport.Narrative == ""
	// ExplanationSkipped follows the same argument one flag over: with
	// o.explain set and o.investigate clear, runModelPath enters the explain
	// arm; a failure sets modelRes.notice (handled above); and a success with
	// an empty explanation happens only through ExplainInventory's clean-scan
	// guard, because a real run that produced no text returns an error rather
	// than an empty explanation. --investigate supersedes --explain in
	// runModelPath, so o.investigate is excluded — that path never entered
	// the explain arm at all. So this conjunction is exactly the skip and
	// nothing else.
	in.ExplanationSkipped = o.explain && !o.investigate && modelRes.notice == "" && explanation == ""
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

// newScanCommand builds `kubeagent scan`.
func newScanCommand() *cobra.Command {
	var o scanOptions
	cmd := &cobra.Command{
		Use:           "scan",
		Short:         "Diagnose a cluster and report what is wrong",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScan(o)
		},
	}
	bindScanFlags(cmd, &o)
	return cmd
}
