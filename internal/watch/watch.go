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
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/imantaba/kubeagent/internal/scan"
)

// Config configures the daemon.
type Config struct {
	Namespace              string
	MetricsAddr            string
	Heartbeat              time.Duration
	Debounce               time.Duration
	IncludeCron            bool
	IncludeRestarts        bool
	DiskUsage              bool
	DiskThreshold          float64
	NodeHeartbeatThreshold time.Duration
	ExpectedNodes          []string
	KubeletHealth          bool
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

	opts := scan.Options{Namespace: cfg.Namespace, IncludeCron: cfg.IncludeCron, IncludeRestarts: cfg.IncludeRestarts, DiskUsage: cfg.DiskUsage, DiskThreshold: cfg.DiskThreshold, NodeHeartbeatThreshold: cfg.NodeHeartbeatThreshold, ExpectedNodes: cfg.ExpectedNodes, KubeletHealth: cfg.KubeletHealth}
	var cl changeLogger
	reconcile := func() {
		start := time.Now()
		res, err := scan.Evaluate(ctx, client, opts)
		m.update(&res, time.Since(start), time.Now(), err)
		if cl.changed(&res, err) {
			log.Printf("kubeagent: %s", describe(&res, err))
		}
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

// changeLogger suppresses steady-state log spam: it logs only when the health
// picture changes vs the previous reconcile.
type changeLogger struct {
	prev   string
	inited bool
}

func (c *changeLogger) changed(res *scan.Result, err error) bool {
	sig := signature(res, err)
	if c.inited && sig == c.prev {
		return false
	}
	c.inited = true
	c.prev = sig
	return true
}

// signature is a compact, stable fingerprint of the evaluation outcome.
func signature(res *scan.Result, err error) string {
	if err != nil {
		return "err:" + err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%d/%d|", res.Health.Verdict, res.Health.NodesReady, res.Health.NodesTotal)
	var flagged []string
	for _, w := range res.Inventory.Workloads {
		if w.Flagged() {
			issues := make([]string, 0, len(w.Findings))
			for _, f := range w.Findings {
				issues = append(issues, f.Issue)
			}
			sort.Strings(issues)
			flagged = append(flagged, w.Namespace+"/"+w.Name+":"+strings.Join(issues, ","))
		}
	}
	sort.Strings(flagged)
	b.WriteString(strings.Join(flagged, ";"))
	return b.String()
}

// describe is a one-line human summary for the log.
func describe(res *scan.Result, err error) string {
	if err != nil {
		return "evaluation error: " + err.Error()
	}
	n := 0
	for _, w := range res.Inventory.Workloads {
		if w.Flagged() {
			n++
		}
	}
	return fmt.Sprintf("cluster %s (%d/%d nodes ready) — %d workload(s) flagged, %d service issue(s)",
		res.Health.Verdict, res.Health.NodesReady, res.Health.NodesTotal, n, len(res.ServiceIssues))
}
