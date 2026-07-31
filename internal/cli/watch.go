package cli

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
		// This line hardcodes "kubeagent" where every other warning uses invokedAs.
		// Preserved verbatim through the Cobra migration, which freezes stderr; it
		// is worth fixing separately, where the change is visible as its own diff.
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
