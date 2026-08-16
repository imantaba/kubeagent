package cli

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/alert"
	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/watch"
)

// watchRun is the daemon entry point, indirected so tests can capture the
// watch.Config runWatch builds without starting a daemon. runWatch's config is
// otherwise unobservable: watch.Run blocks on informers until its context is
// cancelled, and that context comes from signal.NotifyContext, which no test
// can cancel.
var watchRun = watch.Run

// checkContexts rejects an empty --context value. It lives beside the parser
// rather than in runWatchOpts: Task 3's TestParseWatchFlagsRejectsEmptyContext
// asserts the error from parseWatchFlags's own return value, so the check runs
// there. newWatchCommand's RunE calls it too — bindWatchFlags binds
// o.contexts directly onto the real command, so Cobra's own flag parsing has
// already filled it by the time RunE runs, on the same command line
// parseWatchFlags would have rejected.
func checkContexts(contexts []string) error {
	for _, c := range contexts {
		if c == "" {
			return fmt.Errorf("--context cannot be empty")
		}
	}
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

// watchOptions is `kubeagent watch`'s parsed command line. One field per flag,
// in declaration order. It exists so flag wiring is testable without a
// cluster: parseWatchFlags is pure, and runWatchOpts does the I/O.
type watchOptions struct {
	kubeconfig      string
	contexts        []string
	clusterName     string
	includeLocal    bool
	metricsAddr     string
	heartbeat       time.Duration
	debounce        time.Duration
	includeCron     bool
	includeRestarts bool
	alertFormat     string
	alertRepeat     time.Duration
	sloTarget       float64
	explain         bool
	explainCooldown time.Duration
	explainBudget   int
	model           string
	dashboard       bool
	namespace       string
}

// bindWatchFlags declares watch's flags on cmd, writing into o. Flag names,
// defaults and usage strings are unchanged from the standard-library FlagSet
// this replaces, except --namespace/-n: the standard library needed two
// separate declarations to accept both spellings, pflag expresses that as one
// flag with a shorthand, and only the long form's usage text survives.
// --context uses StringArrayVar, not StringSliceVar: the slice form splits its
// input on commas, which would turn --context a,b into two contexts where
// today it is one context literally named a,b.
func bindWatchFlags(cmd *cobra.Command, o *watchOptions) {
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig for local dev (ignored in-cluster)")
	f.StringArrayVar(&o.contexts, "context", nil,
		"kubeconfig context to watch; repeat the flag to watch several clusters from one daemon")
	f.StringVar(&o.clusterName, "cluster-name", envOr("KUBEAGENT_CLUSTER_NAME", "local"), "name for the default cluster — the one watched when no --context is given; becomes its `cluster` metric label")
	f.BoolVar(&o.includeLocal, "include-local", envBool("KUBEAGENT_INCLUDE_LOCAL", false), "also watch the default cluster alongside every --context (no-op when no --context is given)")
	f.StringVar(&o.metricsAddr, "metrics-addr", envOr("KUBEAGENT_METRICS_ADDR", ":8080"), "address for /metrics, /healthz, /readyz")
	f.DurationVar(&o.heartbeat, "heartbeat", envDur("KUBEAGENT_HEARTBEAT", 60*time.Second), "safety-net full re-evaluation interval")
	f.DurationVar(&o.debounce, "debounce", envDur("KUBEAGENT_DEBOUNCE", 2*time.Second), "coalescing window for change events")
	f.BoolVar(&o.includeCron, "include-cron", false, "include CronJobs in the evaluation")
	f.BoolVar(&o.includeRestarts, "include-restarts", false, "include workloads that are healthy now but have restarted")
	f.StringVar(&o.alertFormat, "alert-format", envOr("KUBEAGENT_ALERT_FORMAT", "json"), "alert payload format: json, slack, alertmanager, or pagerduty")
	f.DurationVar(&o.alertRepeat, "alert-repeat", envDur("KUBEAGENT_ALERT_REPEAT", 0), "re-send interval for still-firing alerts (0 = the format default: 4h, or 60s for alertmanager)")
	f.Float64Var(&o.sloTarget, "slo-target", envFloat("KUBEAGENT_SLO_TARGET", 0), "availability SLO as a percentage, e.g. 99.9 (0 = SLO tracking off)")
	f.BoolVar(&o.explain, "explain", envBool("KUBEAGENT_EXPLAIN", false), "explain new incidents via one LLM call each (needs ANTHROPIC_API_KEY, or KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model)")
	f.DurationVar(&o.explainCooldown, "explain-cooldown", envDur("KUBEAGENT_EXPLAIN_COOLDOWN", time.Hour), "minimum gap between explanations for the same object (0 = no per-object gap)")
	f.IntVar(&o.explainBudget, "explain-budget", envInt("KUBEAGENT_EXPLAIN_BUDGET", 20), "model calls per hour, and the burst capacity")
	f.StringVar(&o.model, "model", "", "model for --explain (default: $KUBEAGENT_MODEL or claude-opus-4-8; the local model name when KUBEAGENT_EXPLAIN_ENDPOINT is set)")
	f.BoolVar(&o.dashboard, "dashboard", envBool("KUBEAGENT_DASHBOARD", false), "serve a read-only HTML dashboard at /dashboard on --metrics-addr (unauthenticated, like /metrics and /issues on the same port)")
	f.StringVarP(&o.namespace, "namespace", "n", envOr("KUBEAGENT_NAMESPACE", ""), "namespace to watch (default: all)")
}

// parseWatchFlags parses `kubeagent watch`'s command line. Pure: it reads the
// environment for the env-defaulted flags and nothing else, contacts no
// cluster, and writes nothing. It builds a throwaway command so the flag
// declarations have exactly one home, in bindWatchFlags.
func parseWatchFlags(args []string) (watchOptions, error) {
	var o watchOptions
	cmd := &cobra.Command{Use: "watch", SilenceErrors: true, SilenceUsage: true}
	bindWatchFlags(cmd, &o)
	if err := cmd.Flags().Parse(Normalize(args, longFlagLookup(cmd))); err != nil {
		return watchOptions{}, err
	}
	if err := checkContexts(o.contexts); err != nil {
		return watchOptions{}, err
	}
	return o, nil
}

// runWatchOpts serves `kubeagent watch`. o is the already-parsed command
// line, as produced by parseWatchFlags.
func runWatchOpts(o watchOptions) error {
	// The webhook URL and the PagerDuty routing key are both credentials — a
	// Slack incoming-webhook URL is a bearer token in URL form, and a routing
	// key is one outright — so they come from the environment only, never a
	// flag, which would put them in the pod spec's args and in `ps` output.
	alertURL := os.Getenv("KUBEAGENT_ALERT_WEBHOOK")
	routingKey := os.Getenv("KUBEAGENT_ALERT_ROUTING_KEY")
	repeat := o.alertRepeat
	if repeat == 0 {
		repeat = alert.DefaultRepeat(alert.Format(o.alertFormat))
	}
	// Alerting is on when the format's own credential is present: the URL for
	// every format, or the routing key for pagerduty, which defaults its
	// endpoint.
	alerting := alertURL != "" || (o.alertFormat == string(alert.FormatPagerDuty) && routingKey != "")
	if !alerting && (o.alertFormat != "json" || o.alertRepeat != 0) {
		warnf(os.Stderr, "--alert-* flags ignored: neither KUBEAGENT_ALERT_WEBHOOK nor KUBEAGENT_ALERT_ROUTING_KEY (with --alert-format pagerduty) is set, so alerting is off")
	}
	if routingKey != "" && o.alertFormat != string(alert.FormatPagerDuty) {
		warnf(os.Stderr, "KUBEAGENT_ALERT_ROUTING_KEY is set but --alert-format is %s: the routing key is used by the pagerduty format alone", o.alertFormat)
	}

	// --explain needs Anthropic, or a local OpenAI-compatible endpoint. Check
	// before connecting: a credential error must not surface as a daemon that
	// looks up and then silently never explains anything.
	explainEndpoint := os.Getenv("KUBEAGENT_EXPLAIN_ENDPOINT")
	var explainModel string
	if o.explain {
		if explainEndpoint == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
			return fmt.Errorf("--explain needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model")
		}
		if explainEndpoint != "" {
			explainModel = firstNonEmpty(o.model, os.Getenv("KUBEAGENT_MODEL")) // no Anthropic default for a local model
			if explainModel == "" {
				return fmt.Errorf("--explain with KUBEAGENT_EXPLAIN_ENDPOINT needs --model (or KUBEAGENT_MODEL) set to the local model name")
			}
		} else {
			explainModel = explain.ResolveModel(o.model, os.Getenv("KUBEAGENT_MODEL"))
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
	exact := o.sloTarget / 100
	sloRatio := math.Round(exact*1e8) / 1e8
	if (sloRatio == 0) != (exact == 0) || (sloRatio >= 1) != (exact >= 1) {
		sloRatio = exact
	}

	// Every declared expected-node name must be a real node-name shape: a
	// name that could never match a Node object describes nothing real, and
	// a daemon should not start having silently narrowed the check it was
	// configured to perform. cleanExpected's trim/dedup/sort runs after
	// this, not instead of it.
	expectedNodes := splitCSV(envOr("KUBEAGENT_EXPECTED_NODES", ""))
	if err := validateExpectedNodes(expectedNodes, "KUBEAGENT_EXPECTED_NODES"); err != nil {
		return err
	}

	// The API server itself refuses a webhook timeoutSeconds above 30, so a
	// threshold above 30 could only ever match nothing; refuse it here
	// rather than start a daemon reporting a clean posture that was never
	// actually checked.
	webhookTimeout, err := envIntRange("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15, 1, 30)
	if err != nil {
		return err
	}

	targets, err := buildTargets(o.kubeconfig, o.clusterName, o.contexts, o.includeLocal)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return watchRun(ctx, targets, watch.Config{
		Namespace:               o.namespace,
		MetricsAddr:             o.metricsAddr,
		Heartbeat:               o.heartbeat,
		Debounce:                o.debounce,
		IncludeCron:             o.includeCron,
		IncludeRestarts:         o.includeRestarts,
		DiskUsage:               envBool("KUBEAGENT_DISK_USAGE", false),
		DiskThreshold:           envFloat("KUBEAGENT_DISK_THRESHOLD", 0.80),
		QuotaThreshold:          quotaThresholdFromEnv(os.Stderr),
		NodeHeartbeatThreshold:  envDur("KUBEAGENT_NODE_HEARTBEAT_THRESHOLD", 40*time.Second),
		ExpectedNodes:           expectedNodes,
		KubeletHealth:           envBool("KUBEAGENT_KUBELET_HEALTH", false),
		ControlPlaneHealth:      envBool("KUBEAGENT_CONTROL_PLANE_HEALTH", false),
		DNSHealth:               envBool("KUBEAGENT_DNS_HEALTH", false),
		DNSServfailRatio:        envFloat("KUBEAGENT_DNS_SERVFAIL_RATIO", 0.05),
		Certs:                   envBool("KUBEAGENT_CERTS", false),
		CertWarnDays:            envInt("KUBEAGENT_CERT_WARN_DAYS", 30),
		WebhookTimeoutThreshold: int32(webhookTimeout),
		AlertURL:                alertURL,
		AlertFormat:             o.alertFormat,
		AlertRoutingKey:         routingKey,
		AlertRepeat:             repeat,
		SLOTarget:               sloRatio,
		Explain:                 o.explain,
		ExplainModel:            explainModel,
		ExplainEndpoint:         explainEndpoint,
		ExplainAPIKey:           os.Getenv("KUBEAGENT_EXPLAIN_API_KEY"),
		ExplainCooldown:         o.explainCooldown,
		ExplainBudget:           o.explainBudget,
		Dashboard:               o.dashboard,
		Version:                 version,
	})
}

func runWatch(args []string) error {
	o, err := parseWatchFlags(args)
	if err != nil {
		return err
	}
	return runWatchOpts(o)
}

// newWatchCommand builds `kubeagent watch`.
func newWatchCommand() *cobra.Command {
	var o watchOptions
	cmd := &cobra.Command{
		Use:           "watch",
		Short:         "Watch a cluster continuously and alert on new issues",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// bindWatchFlags binds o.contexts directly onto this command, so
			// Cobra's own ParseFlags has already filled it by the time RunE
			// runs; checkContexts here is the same check parseWatchFlags runs
			// for a caller that parses without executing.
			if err := checkContexts(o.contexts); err != nil {
				return err
			}
			return runWatchOpts(o)
		},
	}
	bindWatchFlags(cmd, &o)
	return cmd
}
