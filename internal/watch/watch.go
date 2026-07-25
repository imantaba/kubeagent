// Package watch runs kubeagent as an in-cluster, read-only daemon: it watches the
// cluster via informers, re-runs the deterministic evaluation on change (debounced)
// and on a heartbeat, and surfaces the result as structured logs and Prometheus
// metrics. No writes, no LLM.
package watch

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

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
}

// Run starts the metrics server and the informer-driven control loop, blocking
// until ctx is cancelled.
func Run(ctx context.Context, client kubernetes.Interface, cfg Config) error {
	m := newMetrics()

	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: m.handler()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("kubeagent: metrics server error: %v", err)
		}
	}()

	var factory informers.SharedInformerFactory
	if cfg.Namespace != "" {
		factory = informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(cfg.Namespace))
	} else {
		factory = informers.NewSharedInformerFactory(client, 0)
	}
	trigger := make(chan struct{}, 1)
	enqueue := func() {
		select {
		case trigger <- struct{}{}:
		default: // already pending
		}
	}
	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { enqueue() },
		UpdateFunc: func(interface{}, interface{}) { enqueue() },
		DeleteFunc: func(interface{}) { enqueue() },
	}
	for _, inf := range []cache.SharedIndexInformer{
		factory.Core().V1().Pods().Informer(),
		factory.Apps().V1().Deployments().Informer(),
		factory.Apps().V1().ReplicaSets().Informer(),
		factory.Core().V1().Nodes().Informer(),
		factory.Core().V1().Services().Informer(),
		factory.Discovery().V1().EndpointSlices().Informer(),
	} {
		if _, err := inf.AddEventHandler(h); err != nil {
			return fmt.Errorf("adding informer handler: %w", err)
		}
	}
	factory.Start(ctx.Done())
	if synced := factory.WaitForCacheSync(ctx.Done()); func() bool {
		for _, ok := range synced {
			if !ok {
				return true
			}
		}
		return false
	}() {
		log.Printf("kubeagent: warning: informer caches did not fully sync (context cancelled?)")
	}
	log.Printf("kubeagent: watching cluster (namespace=%q, heartbeat=%s); metrics on %s", scopeLabel(cfg.Namespace), cfg.Heartbeat, cfg.MetricsAddr)

	opts := scan.Options{Namespace: cfg.Namespace, IncludeCron: cfg.IncludeCron, IncludeRestarts: cfg.IncludeRestarts, DiskUsage: cfg.DiskUsage, DiskThreshold: cfg.DiskThreshold, QuotaThreshold: cfg.QuotaThreshold, NodeHeartbeatThreshold: cfg.NodeHeartbeatThreshold, ExpectedNodes: cfg.ExpectedNodes, KubeletHealth: cfg.KubeletHealth, ControlPlaneHealth: cfg.ControlPlaneHealth, DNSHealth: cfg.DNSHealth, DNSServfailRatio: cfg.DNSServfailRatio, Certs: cfg.Certs, CertWarnDays: cfg.CertWarnDays, WebhookTimeoutThreshold: cfg.WebhookTimeoutThreshold}
	tr := watchstate.New(watchstate.Options{})
	reconcile := func() {
		start := time.Now()
		res, err := scan.Evaluate(ctx, client, opts)
		applyResult(m, tr, &res, time.Since(start), time.Now(), err)
	}
	reconcile() // initial snapshot
	m.markReady()

	heartbeat := time.NewTicker(cfg.Heartbeat)
	defer heartbeat.Stop()
	debounce := time.NewTimer(cfg.Debounce)
	debounce.Stop()
	defer debounce.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = srv.Shutdown(shutCtx)
			cancel()
			log.Printf("kubeagent: shutting down")
			return nil
		case <-trigger:
			if !pending {
				pending = true
				debounce.Reset(cfg.Debounce)
			}
		case <-debounce.C:
			pending = false
			reconcile()
		case <-heartbeat.C:
			reconcile()
		}
	}
}

func scopeLabel(ns string) string {
	if ns == "" {
		return "all"
	}
	return ns
}

// applyResult folds one evaluation into the metrics and the issue tracker, and
// logs whatever changed. A failed evaluation never reaches the tracker: an
// evaluation error is not "all clear", and treating it as one would resolve every
// tracked issue, then re-fire them all on the next success.
func applyResult(m *metrics, tr *watchstate.Tracker, res *scan.Result, dur time.Duration, now time.Time, err error) {
	m.update(res, dur, now, err)
	if err != nil {
		log.Printf("kubeagent: evaluation error: %v", err)
		return
	}
	d := tr.Observe(issueKeys(res), now)
	m.updateIssues(tr, now)
	logDelta(res, d, len(tr.Active()), tr.FlapWindow())
}

// logDelta prints one line per transition plus a summary. A reconcile that
// changed nothing prints nothing, so steady state stays quiet.
func logDelta(res *scan.Result, d watchstate.Delta, active int, flapWindow time.Duration) {
	if len(d.New) == 0 && len(d.Resolved) == 0 && len(d.NewlyFlapping) == 0 {
		return
	}
	for _, r := range d.New {
		log.Printf("kubeagent: NEW %s", r.Key)
	}
	for _, r := range d.Resolved {
		log.Printf("kubeagent: RESOLVED %s (fired for %s)", r.Key, r.ResolvedAt.Sub(r.FiringSince).Round(time.Second))
	}
	for _, r := range d.NewlyFlapping {
		log.Printf("kubeagent: FLAPPING %s (%d firings in %s)", r.Key, r.RecentFirings, flapWindow)
	}
	log.Printf("kubeagent: cluster %s (%d/%d nodes ready) — %d issue(s) active, %d new, %d resolved",
		res.Health.Verdict, res.Health.NodesReady, res.Health.NodesTotal, active, len(d.New), len(d.Resolved))
}
