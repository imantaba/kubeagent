// Package watch runs kubeagent as an in-cluster, read-only daemon: it watches the
// cluster via informers, re-runs the deterministic evaluation on change (debounced)
// and on a heartbeat, and surfaces the result as structured logs and Prometheus
// metrics. No writes. The LLM is opt-in, off by default, and sees only findings
// the daemon has already collected — it triggers no additional cluster reads and
// needs no additional RBAC.
package watch

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/imantaba/kubeagent/internal/alert"
	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/oncall"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

// Config configures the daemon.
type Config struct {
	Namespace               string
	MetricsAddr             string
	Heartbeat               time.Duration
	Debounce                time.Duration
	IncludeCron             bool
	IncludeRestarts         bool
	DiskUsage               bool
	DiskThreshold           float64
	QuotaThreshold          float64
	NodeHeartbeatThreshold  time.Duration
	ExpectedNodes           []string
	KubeletHealth           bool
	ControlPlaneHealth      bool
	DNSHealth               bool
	DNSServfailRatio        float64
	Certs                   bool
	CertWarnDays            int
	WebhookTimeoutThreshold int32
	AlertURL                string        // empty disables alerting entirely
	AlertFormat             string        // "json" | "slack" | "alertmanager"
	AlertRepeat             time.Duration // re-send interval for still-firing alerts
	SLOTarget               float64       // availability SLO as a ratio in (0,1); 0 disables SLO tracking
	Explain                 bool          // opt-in on-incident explanations; off by default
	ExplainModel            string        // resolved model name
	ExplainEndpoint         string        // OpenAI-compatible endpoint; empty selects Anthropic
	ExplainAPIKey           string        // bearer token for a local endpoint; ignored by Anthropic
	ExplainCooldown         time.Duration // per-object minimum gap between explanations
	ExplainBudget           int           // model calls per hour, and the burst capacity
}

// Run starts the metrics server and one informer-driven control loop per target,
// blocking until ctx is cancelled.
func Run(ctx context.Context, targets []Target, cfg Config) error {
	// Validate every piece of configuration before anything else starts. A bad
	// --slo-target, --alert-format, --alert-repeat or target list must fail
	// fast: once the metrics server is listening and WaitForCacheSync is
	// underway, a reachable-but-unresponsive API server can block that sync
	// forever, hiding the config error behind what looks like a cluster hang.
	if err := validateTargets(targets); err != nil {
		return err
	}
	if err := validateSLOTarget(cfg.SLOTarget); err != nil {
		return err
	}
	if err := validateExplain(cfg); err != nil {
		return err
	}

	// The sink runs off its own cancellable context, alertCtx, rather than ctx
	// directly: al.sink.Close() blocks on <-s.done, which only closes once the
	// sender goroutine observes its context's Done channel. Every step between
	// here and the main loop below must therefore be able to fail and return
	// without hanging on that Close — including a step that runs before ctx is
	// ever cancelled.
	//
	// The two defers below make that true, and the order is load-bearing: defers
	// run LIFO, so deferring stopAlerts() *after* al.sink.Close() makes stopAlerts
	// the one that runs FIRST on the way out. That cancels alertCtx and lets the
	// sender goroutine exit, so the Close() that runs second never blocks — even
	// on a Run() exit (say, a future fallible step added in this window) that
	// leaves ctx itself still live. Swap the order and Close() would run first,
	// waiting forever on a cancel that hasn't happened yet.
	alertCtx, stopAlerts := context.WithCancel(ctx)
	al, err := newAlerter(alertCtx, cfg)
	if err != nil {
		stopAlerts()
		return err
	}
	if al != nil {
		defer func() { noteTeardown("sinkClose"); al.sink.Close() }() // deferred first, so it runs last
	}
	defer func() { noteTeardown("stopAlerts"); stopAlerts() }()

	// The explainer is a producer for the alert sink, so it must be stopped
	// before the sink is. Defers run LIFO, so deferring these two after the
	// alert pair puts them first on the way out: stopExplain cancels any call
	// in flight, ex.Close waits for the worker, and only then does stopAlerts
	// let the sender drain and sink.Close return. Reversed, an explanation
	// finishing during shutdown would be enqueued into a sink whose sender had
	// already returned — never delivered, and never counted as a drop.
	explainCtx, stopExplain := context.WithCancel(ctx)
	ex := newExplainer(explainCtx, cfg, al)
	if ex != nil {
		defer func() { noteTeardown("explainerClose"); ex.Close() }()
	}
	defer func() { noteTeardown("stopExplain"); stopExplain() }()

	m := newMetrics(targetNames(targets))

	// Every worker is constructed before any of them runs, so a handler that
	// cannot be registered still fails the whole daemon at startup.
	workers := make([]*clusterWorker, 0, len(targets))
	for _, t := range targets {
		w, err := newClusterWorker(t, cfg, m, al, ex)
		if err != nil {
			return err
		}
		workers = append(workers, w)
	}

	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: m.handler()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("kubeagent: metrics server error: %v", err)
		}
	}()
	log.Printf("kubeagent: watching %d cluster(s); metrics on %s", len(workers), cfg.MetricsAddr)

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *clusterWorker) {
			defer wg.Done()
			w.run(ctx)
		}(w)
	}

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(shutCtx)
	cancel()
	wg.Wait()
	log.Printf("kubeagent: shutting down")
	return nil
}

func scopeLabel(ns string) string {
	if ns == "" {
		return "all"
	}
	return ns
}

// validateExplain checks the explanation limits. A zero cooldown is legal and
// disables the per-object gap, leaving the budget as the only limit.
func validateExplain(cfg Config) error {
	if !cfg.Explain {
		return nil
	}
	if cfg.ExplainBudget < 1 {
		return fmt.Errorf("--explain-budget must be at least 1 call per hour, got %d", cfg.ExplainBudget)
	}
	if cfg.ExplainCooldown < 0 {
		return fmt.Errorf("--explain-cooldown cannot be negative, got %s", cfg.ExplainCooldown)
	}
	return nil
}

// newExplainer builds the explainer from the config, returning nil when
// --explain is off. It is handed no Kubernetes client: the explainer sees only
// findings the daemon has already collected.
func newExplainer(ctx context.Context, cfg Config, al *alerter) *oncall.Explainer {
	if !cfg.Explain {
		return nil
	}
	ex := oncall.New(oncall.Config{
		Client:   explain.NewFromConfig(cfg.ExplainModel, cfg.ExplainEndpoint, cfg.ExplainAPIKey),
		Model:    cfg.ExplainModel,
		Cooldown: cfg.ExplainCooldown,
		Budget:   cfg.ExplainBudget,
		Notify:   al.enqueue,
	})
	ex.Start(ctx)
	backend, credential := "anthropic", ""
	if cfg.ExplainEndpoint != "" {
		// The endpoint may carry a token in its URL, so only scheme://host is
		// ever logged — the same rule the alert webhook follows.
		backend = alert.RedactURL(cfg.ExplainEndpoint)
		// A local model may need no key at all, so an absent one is not an
		// error — but a mistyped Secret key looks identical from here, and the
		// chart marks that key optional. Say which case it is, never the value.
		credential = ", api-key=absent"
		if cfg.ExplainAPIKey != "" {
			credential = ", api-key=set"
		}
	}
	log.Printf("kubeagent: on-incident explanations enabled (model=%s, backend=%s%s, cooldown=%s, budget=%d/h)",
		cfg.ExplainModel, backend, credential, cfg.ExplainCooldown, cfg.ExplainBudget)
	return ex
}

// teardownOrder records Run's teardown steps when non-nil. Run's defer order is
// load-bearing — the explainer produces for the alert sink, so it must stop
// first — and the order is otherwise unobservable from outside the function.
var teardownOrder func(step string)

func noteTeardown(step string) {
	if teardownOrder != nil {
		teardownOrder(step)
	}
}

// alerter routes tracker state to the outbound sink. A nil *alerter means no
// webhook is configured, which is the default: every method is a no-op, so the
// reconcile loop needs no conditional. There is one alerter per process — one
// webhook, one bounded queue — while the per-object rollup state lives in each
// cluster's own roller, because two clusters routinely run an object with the
// same namespace and name.
type alerter struct {
	sink *alert.Sink
}

// notify hands one cluster's rolled-up notifications to the sink. Enqueue never
// blocks.
func (a *alerter) notify(roller *alertstate.Roller, tr *watchstate.Tracker, now time.Time) {
	if a == nil {
		return
	}
	for _, n := range roller.Roll(tr.Active(), now) {
		a.sink.Enqueue(n)
	}
}

// stats returns the sink's delivery counters, or the zero value when alerting is off.
func (a *alerter) stats() alert.Stats {
	if a == nil {
		return alert.Stats{}
	}
	return a.sink.Stats()
}

// enqueue hands one already-built notification to the sink. The SLO burn alert
// uses this rather than notify: it is not derived from the issue tracker, but it
// shares the sink so retry, backoff, the bounded queue and URL redaction all
// apply to it unchanged.
func (a *alerter) enqueue(n alertstate.Notification) {
	if a == nil {
		return
	}
	a.sink.Enqueue(n)
}

// newAlerter builds the alerter from the config, returning nil when no webhook is
// configured. The URL is a credential: only scheme://host is ever logged.
func newAlerter(ctx context.Context, cfg Config) (*alerter, error) {
	if cfg.AlertURL == "" {
		return nil, nil
	}
	format := alert.Format(cfg.AlertFormat)
	sink, err := alert.New(alert.Config{URL: cfg.AlertURL, Format: format, Repeat: cfg.AlertRepeat}, nil)
	if err != nil {
		return nil, err
	}
	sink.Start(ctx)
	log.Printf("kubeagent: alerting enabled (format=%s, repeat=%s, endpoint=%s)", format, cfg.AlertRepeat, alert.RedactURL(cfg.AlertURL))
	return &alerter{sink: sink}, nil
}

// logDelta prints one line per transition plus a summary. A reconcile that
// changed nothing prints nothing, so steady state stays quiet.
func logDelta(cluster string, res *scan.Result, d watchstate.Delta, active int, flapWindow time.Duration) {
	if len(d.New) == 0 && len(d.Resolved) == 0 && len(d.NewlyFlapping) == 0 {
		return
	}
	for _, r := range d.New {
		clusterLogf(cluster, "NEW %s", r.Key)
	}
	for _, r := range d.Resolved {
		clusterLogf(cluster, "RESOLVED %s (fired for %s)", r.Key, r.ResolvedAt.Sub(r.FiringSince).Round(time.Second))
	}
	for _, r := range d.NewlyFlapping {
		clusterLogf(cluster, "FLAPPING %s (%d firings in %s)", r.Key, r.RecentFirings, flapWindow)
	}
	clusterLogf(cluster, "cluster %s (%d/%d nodes ready) — %d issue(s) active, %d new, %d resolved",
		res.Health.Verdict, res.Health.NodesReady, res.Health.NodesTotal, active, len(d.New), len(d.Resolved))
}
