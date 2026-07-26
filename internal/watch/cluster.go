package watch

import (
	"context"
	"fmt"
	"log"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/oncall"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/slo"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

// cacheSyncTimeout bounds how long one cluster's informers may take to fill
// before the worker proceeds anyway. An unreachable cluster would otherwise
// block in WaitForCacheSync forever, and with the readiness rule being "every
// cluster has finished a first attempt", that one cluster would hold the whole
// daemon out of its Service endpoints. The informers keep retrying in the
// background on the daemon's own context, so a cluster that comes back later
// simply starts producing successful evaluations.
//
// A var, not a const: a broken-cluster test needs to observe this bound being
// hit without spending the real 30s on it, and shrinking it for the duration of
// one test is simpler and more honest than mocking WaitForCacheSync itself.
var cacheSyncTimeout = 30 * time.Second

// clusterLogf prefixes a daemon log line with the cluster it concerns. Without
// it, N interleaved reconcile loops produce an unreadable log.
func clusterLogf(cluster, format string, args ...any) {
	log.Printf("kubeagent: ["+cluster+"] "+format, args...)
}

// clusterWorker owns everything that is per cluster: the informers, the issue
// tracker, the alert roller and the SLO tracker. The alert sink, the explainer
// and the metrics snapshot are shared and are handed in.
type clusterWorker struct {
	name    string
	client  kubernetes.Interface
	cfg     Config
	opts    scan.Options
	factory informers.SharedInformerFactory
	trigger chan struct{}

	m      *metrics
	al     *alerter
	roller *alertstate.Roller
	ex     *oncall.Explainer
	tr     *watchstate.Tracker
	sloTr  *slo.Tracker
	sloN   *sloNotifier
}

// newClusterWorker builds one cluster's worker and registers its informer
// handlers. It is deliberately fallible and deliberately called synchronously
// from Run, before any goroutine starts: a handler that cannot be registered is
// a startup failure, not something to discover in a background goroutine that
// has no way to report it.
func newClusterWorker(t Target, cfg Config, m *metrics, al *alerter, ex *oncall.Explainer) (*clusterWorker, error) {
	var factory informers.SharedInformerFactory
	if cfg.Namespace != "" {
		factory = informers.NewSharedInformerFactoryWithOptions(t.Client, 0, informers.WithNamespace(cfg.Namespace))
	} else {
		factory = informers.NewSharedInformerFactory(t.Client, 0)
	}
	w := &clusterWorker{
		name:    t.Name,
		client:  t.Client,
		cfg:     cfg,
		factory: factory,
		trigger: make(chan struct{}, 1),
		m:       m,
		al:      al,
		roller:  alertstate.New(alertstate.Options{Repeat: cfg.AlertRepeat, Cluster: t.Name}),
		ex:      ex,
		tr:      watchstate.New(watchstate.Options{}),
		opts: scan.Options{
			Namespace: cfg.Namespace, IncludeCron: cfg.IncludeCron, IncludeRestarts: cfg.IncludeRestarts,
			DiskUsage: cfg.DiskUsage, DiskThreshold: cfg.DiskThreshold, QuotaThreshold: cfg.QuotaThreshold,
			NodeHeartbeatThreshold: cfg.NodeHeartbeatThreshold, ExpectedNodes: cfg.ExpectedNodes,
			KubeletHealth: cfg.KubeletHealth, ControlPlaneHealth: cfg.ControlPlaneHealth,
			DNSHealth: cfg.DNSHealth, DNSServfailRatio: cfg.DNSServfailRatio,
			Certs: cfg.Certs, CertWarnDays: cfg.CertWarnDays, WebhookTimeoutThreshold: cfg.WebhookTimeoutThreshold,
		},
	}
	w.sloTr, w.sloN = newSLOTracker(t.Name, cfg)

	enqueue := func() {
		select {
		case w.trigger <- struct{}{}:
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
			return nil, fmt.Errorf("cluster %q: adding informer handler: %w", t.Name, err)
		}
	}
	return w, nil
}

// run drives one cluster until ctx is cancelled.
func (w *clusterWorker) run(ctx context.Context) {
	w.factory.Start(ctx.Done())

	// The sync wait is bounded but the informers are not: factory.Start above
	// runs them on ctx, so they keep retrying after this returns.
	syncCtx, cancelSync := context.WithTimeout(ctx, cacheSyncTimeout)
	synced := w.factory.WaitForCacheSync(syncCtx.Done())
	cancelSync()
	for _, ok := range synced {
		if !ok {
			clusterLogf(w.name, "warning: informer caches did not fully sync within %s; evaluating anyway (the informers keep retrying)", cacheSyncTimeout)
			break
		}
	}
	clusterLogf(w.name, "watching cluster (namespace=%q, heartbeat=%s)", scopeLabel(w.cfg.Namespace), w.cfg.Heartbeat)
	if w.sloTr != nil {
		clusterLogf(w.name, "SLO burn-rate tracking enabled (target=%g%%, windows 1h/6h, alert suppressed below %d%% window coverage)",
			w.cfg.SLOTarget*100, 60)
	}

	w.reconcile(ctx)
	// Ready as soon as the FIRST ATTEMPT is done, success or failure. Readiness
	// answers "can this process serve?", not "is this cluster fine".
	w.m.markReady(w.name)

	heartbeat := time.NewTicker(w.cfg.Heartbeat)
	defer heartbeat.Stop()
	debounce := time.NewTimer(w.cfg.Debounce)
	debounce.Stop()
	defer debounce.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.trigger:
			if !pending {
				pending = true
				debounce.Reset(w.cfg.Debounce)
			}
		case <-debounce.C:
			pending = false
			w.reconcile(ctx)
		case <-heartbeat.C:
			w.reconcile(ctx)
		}
	}
}

func (w *clusterWorker) reconcile(ctx context.Context) {
	start := time.Now()
	res, err := scan.Evaluate(ctx, w.client, w.opts)
	w.applyResult(&res, time.Since(start), time.Now(), err)
}

// applyResult folds one evaluation into the metrics, the issue tracker, the SLO
// tracker, and the shared alerter. A failed evaluation never reaches any
// tracker: an evaluation error is not "all clear", and treating it as one would
// resolve every tracked issue, then re-fire them all on the next success — and
// page the on-call for an API blip. The SLO tracker sits on the same side of
// that return for the same reason: an API error is neither "all healthy" nor
// "all broken", so it must not become a sample. The gap shows up as reduced
// window coverage, which is the honest representation.
//
// sloTr and sloN are always produced together by newSLOTracker (both nil, or
// both set), but the struct does not enforce that, so both are checked here
// rather than trusting the pairing: sloN.step on a nil sloN would panic.
func (w *clusterWorker) applyResult(res *scan.Result, dur time.Duration, now time.Time, err error) {
	w.m.update(w.name, res, dur, now, err)
	if err != nil {
		clusterLogf(w.name, "evaluation error: %v", err)
		return
	}
	d := w.tr.Observe(issueKeys(res), now)
	w.al.notify(w.roller, w.tr, now)
	// The object alert has already been enqueued above, LLM-free. Only now is
	// the model considered, and only for objects the throttle admits.
	w.ex.Consider(w.name, d, res.Health, flaggedWorkloads(res), res.ServiceIssues, now)
	if w.sloTr != nil && w.sloN != nil {
		c := res.Inventory.Census
		w.sloTr.Observe(c.Good, c.Total, now)
		v := w.sloTr.Verdict(now)
		w.m.updateSLO(w.name, true, w.sloTr.Target(), v.Fast, v.Slow)
		if n, ok := w.sloN.step(v, now); ok {
			logSLO(w.name, n, v)
			w.al.enqueue(n)
		}
	}
	w.m.updateIssues(w.name, w.tr, now)
	w.m.updateAlerts(w.al.stats())
	w.m.updateExplain(w.ex != nil, w.ex.Stats(now), w.ex.Latest())
	logDelta(w.name, res, d, len(w.tr.Active()), w.tr.FlapWindow())
}
