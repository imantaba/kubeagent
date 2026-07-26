package oncall

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

type fakeClient struct {
	mu      sync.Mutex
	calls   int
	out     string
	err     error
	release chan struct{} // when non-nil, each call blocks until it receives
}

func (f *fakeClient) ExplainIncident(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	f.calls++
	rel := f.release
	out, err := f.out, f.err
	f.mu.Unlock()
	if rel != nil {
		<-rel
	}
	return out, err
}

func (f *fakeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type recorder struct {
	mu   sync.Mutex
	sent []alertstate.Notification
}

func (r *recorder) notify(n alertstate.Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, n)
}

func (r *recorder) all() []alertstate.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]alertstate.Notification(nil), r.sent...)
}

func newRecord(kind, ns, name, issue string, at time.Time) watchstate.Record {
	return watchstate.Record{
		Key:         watchstate.Key{Kind: kind, Namespace: ns, Name: name, Issue: issue},
		FirstSeen:   at,
		FiringSince: at,
		LastSeen:    at,
		Active:      true,
		Firings:     1,
	}
}

func flaggedWeb() []inventory.Workload {
	return []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment",
		Desired: 3, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff", Reason: "tag not found"}},
	}}
}

// harness builds a started explainer and returns it with its fake collaborators.
func harness(t *testing.T, cfg Config) (*Explainer, *fakeClient, *recorder, context.CancelFunc) {
	t.Helper()
	fc, ok := cfg.Client.(*fakeClient)
	if !ok {
		fc = &fakeClient{out: "because the tag is missing"}
		cfg.Client = fc
	}
	rec := &recorder{}
	cfg.Notify = rec.notify
	if cfg.Model == "" {
		cfg.Model = "test-model"
	}
	e := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	e.Start(ctx)
	return e, fc, rec, cancel
}

// waitFor polls until cond is true or the deadline passes. The worker is a real
// goroutine, so the tests need a settling point rather than a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The daemon's first reconcile sees every pre-existing issue as New. Explaining
// them would spend the whole budget re-explaining problems nobody just caused —
// the same reason a cold daemon must not page.
func TestColdStartExplainsNothing(t *testing.T) {
	e, fc, rec, cancel := harness(t, Config{Cooldown: 0, Budget: 20})
	defer func() { cancel(); e.Close() }()

	d := watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "ImagePullBackOff", t0),
		newRecord("Deployment", "shop", "cart", "CrashLoopBackOff", t0),
	}}
	e.Consider("", d, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	time.Sleep(100 * time.Millisecond)
	if fc.callCount() != 0 {
		t.Errorf("cold start made %d calls, want 0", fc.callCount())
	}
	if len(rec.all()) != 0 {
		t.Errorf("cold start sent %d notifications, want 0", len(rec.all()))
	}
}

func TestOneObjectWithTwoNewIssuesGetsOneCall(t *testing.T) {
	e, fc, rec, cancel := harness(t, Config{Cooldown: time.Hour, Budget: 20})
	defer func() { cancel(); e.Close() }()

	e.Consider("", watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0) // prime
	d := watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "Degraded", t0),
		newRecord("Deployment", "shop", "web", "ImagePullBackOff", t0),
	}}
	e.Consider("", d, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	waitFor(t, "one notification", func() bool { return len(rec.all()) == 1 })
	if fc.callCount() != 1 {
		t.Errorf("made %d calls for one object, want 1", fc.callCount())
	}
	n := rec.all()[0]
	if n.Reason != alertstate.ReasonExplanation {
		t.Errorf("reason = %q, want %q", n.Reason, alertstate.ReasonExplanation)
	}
	if n.Status != alertstate.StatusFiring {
		t.Errorf("status = %q, want %q", n.Status, alertstate.StatusFiring)
	}
	if n.Object != (alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"}) {
		t.Errorf("object = %v, want Deployment/shop/web", n.Object)
	}
	if n.Text == "" {
		t.Error("explanation notification must carry Text")
	}
	if len(n.Issues) != 2 || n.Issues[0] != "Degraded" || n.Issues[1] != "ImagePullBackOff" {
		t.Errorf("issues = %v, want both, sorted", n.Issues)
	}
	if n.FiringSince != t0 {
		t.Errorf("firingSince = %v, want the object's firing time %v", n.FiringSince, t0)
	}
}

func TestModelErrorCountsAsFailedAndSendsNothing(t *testing.T) {
	e, _, rec, cancel := harness(t, Config{
		Cooldown: time.Hour, Budget: 20, Client: &fakeClient{err: errors.New("boom")},
	})
	defer func() { cancel(); e.Close() }()

	e.Consider("", watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	e.Consider("", watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "ImagePullBackOff", t0),
	}}, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	waitFor(t, "the failure to be counted", func() bool { return e.Stats(t0).Failed == 1 })
	if got := len(rec.all()); got != 0 {
		t.Errorf("sent %d notifications after a model error, want 0", got)
	}
	if got := len(e.Latest()); got != 0 {
		t.Errorf("stored %d explanations after a model error, want 0", got)
	}
}

func TestEmptyModelOutputIsAFailure(t *testing.T) {
	e, _, rec, cancel := harness(t, Config{
		Cooldown: time.Hour, Budget: 20, Client: &fakeClient{out: "   \n "},
	})
	defer func() { cancel(); e.Close() }()

	e.Consider("", watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	e.Consider("", watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "ImagePullBackOff", t0),
	}}, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	waitFor(t, "the empty output to be counted as failed", func() bool { return e.Stats(t0).Failed == 1 })
	if got := len(rec.all()); got != 0 {
		t.Errorf("sent %d notifications for empty output, want 0", got)
	}
}

// Queue-full and policy-refused are different causes with different operator
// responses, so they must not share a counter.
func TestQueueFullCountsDroppedNotThrottled(t *testing.T) {
	release := make(chan struct{})
	fc := &fakeClient{out: "text", release: release}
	e, _, _, cancel := harness(t, Config{Cooldown: 0, Budget: 1000, Client: fc})
	defer func() { close(release); cancel(); e.Close() }()

	e.Consider("", watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)

	var recs []watchstate.Record
	for i := 0; i < 40; i++ {
		recs = append(recs, newRecord("Deployment", "shop", "w"+string(rune('a'+i)), "Degraded", t0))
	}
	e.Consider("", watchstate.Delta{New: recs}, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	s := e.Stats(t0)
	if s.Dropped == 0 {
		t.Errorf("40 objects against a queue of %d with a blocked worker must drop some; stats %+v", queueSize, s)
	}
	if s.Throttled != 0 {
		t.Errorf("budget was ample, so nothing may be throttled; stats %+v", s)
	}
}

func TestThrottledObjectsNeverReachTheModel(t *testing.T) {
	e, fc, _, cancel := harness(t, Config{Cooldown: time.Hour, Budget: 1})
	defer func() { cancel(); e.Close() }()

	e.Consider("", watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	e.Consider("", watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "Degraded", t0),
		newRecord("Deployment", "shop", "cart", "Degraded", t0),
	}}, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	// Stats().Allowed is stamped synchronously inside Consider, so waiting on it
	// says nothing about the worker. Wait on the model call itself, then hold
	// the window open: the assertion below is negative — a broken throttle would
	// show up as a second call, and only elapsed time can prove it never comes.
	waitFor(t, "the model call", func() bool { return fc.callCount() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if fc.callCount() != 1 {
		t.Errorf("made %d model calls with a budget of 1, want 1", fc.callCount())
	}
	if got := e.Stats(t0).Throttled; got != 1 {
		t.Errorf("throttled = %d, want 1", got)
	}
}

func TestLatestEvictsAtTheCap(t *testing.T) {
	e, _, _, cancel := harness(t, Config{Cooldown: 0, Budget: 100000})
	defer func() { cancel(); e.Close() }()

	e.Consider("", watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	// The queue holds queueSize jobs and Enqueue drops rather than blocks when
	// it is full, so firing maxLatest+25 objects at once loses most of them and
	// the store never reaches the cap. Pace the producer instead: wait for each
	// explanation to land before sending the next, until the store is full.
	for i := 0; i < maxLatest+25; i++ {
		e.Consider("", watchstate.Delta{New: []watchstate.Record{
			newRecord("Deployment", "shop", "w"+string(rune('a'+i%26))+string(rune('a'+i/26)), "Degraded", t0),
		}}, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0.Add(time.Duration(i)*time.Second))
		want := i + 1
		if want > maxLatest {
			want = maxLatest
		}
		waitFor(t, "the store to reach the next explanation", func() bool { return len(e.Latest()) >= want })
	}
	// The last 25 may still be in flight; hold the window open so an insert that
	// pushed past the cap would show up.
	time.Sleep(100 * time.Millisecond)
	if got := len(e.Latest()); got != maxLatest {
		t.Errorf("stored %d explanations, want exactly the cap of %d", got, maxLatest)
	}
}

func TestNilExplainerIsANoOp(t *testing.T) {
	var e *Explainer
	e.Start(context.Background())
	e.Consider("", watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "Degraded", t0),
	}}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	if got := e.Latest(); got != nil {
		t.Errorf("Latest on a nil explainer = %v, want nil", got)
	}
	if got := (e.Stats(t0)); got != (Stats{}) {
		t.Errorf("Stats on a nil explainer = %+v, want the zero value", got)
	}
	e.Close()
}

// TestConsiderKeysThrottleAndStorePerCluster pins the one place cluster
// identity cannot live at the boundary. The Explainer is shared across every
// cluster because the hourly budget is a cost control, and cost is a property
// of the process. Its cooldown map and its served store are keyed by object, so
// without the cluster in that key, shop/web in prod-eu and shop/web in prod-us
// share one cooldown slot and overwrite each other on /explanations.
func TestConsiderKeysThrottleAndStorePerCluster(t *testing.T) {
	e, fc, _, cancel := harness(t, Config{Cooldown: time.Hour, Budget: 10}) // long enough that a second call for the SAME key is refused
	defer func() { cancel(); e.Close() }()

	d := watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "ImagePullBackOff", t0),
	}}

	// The first Consider per cluster is that cluster's own cold-start prime
	// and explains nothing; each of prod-eu and prod-us reaches its own first
	// call independently, so both need their own throwaway before either of
	// their real transitions is admitted.
	e.Consider("prod-eu", watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	e.Consider("prod-us", watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	e.Consider("prod-eu", d, clusterhealth.ClusterHealth{}, nil, nil, t0)
	e.Consider("prod-us", d, clusterhealth.ClusterHealth{}, nil, nil, t0)

	waitFor(t, "both clusters' model calls", func() bool { return fc.callCount() == 2 })

	latest := e.Latest()
	if len(latest) != 2 {
		t.Fatalf("Latest() has %d entries, want 2 (one per cluster)", len(latest))
	}
	seen := map[string]bool{}
	for _, x := range latest {
		seen[x.Cluster] = true
	}
	if !seen["prod-eu"] || !seen["prod-us"] {
		t.Errorf("Latest() clusters = %v, want both prod-eu and prod-us", seen)
	}
}

// TestColdStartSkipIsPerCluster pins the fix for the process-wide cold-start
// bug: each cluster's first Consider call is ITS OWN initial snapshot and must
// be skipped independently, not just whichever cluster's worker calls Consider
// first. Before the fix, cluster B's first reconcile saw primed already true
// (set by cluster A) and got explained, burning the budget on B's startup
// backlog exactly as the skip exists to prevent.
func TestColdStartSkipIsPerCluster(t *testing.T) {
	e, fc, rec, cancel := harness(t, Config{Cooldown: 0, Budget: 20})
	defer func() { cancel(); e.Close() }()

	firstA := watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "ImagePullBackOff", t0),
	}}
	firstB := watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "cart", "CrashLoopBackOff", t0),
	}}

	// Cluster A's first reconcile is its own cold start: skipped.
	e.Consider("cluster-a", firstA, clusterhealth.ClusterHealth{}, nil, nil, t0)
	// Cluster B's first reconcile is ALSO its own cold start, independent of
	// A's — this is exactly what a process-wide bool gets wrong.
	e.Consider("cluster-b", firstB, clusterhealth.ClusterHealth{}, nil, nil, t0)

	time.Sleep(100 * time.Millisecond)
	if got := fc.callCount(); got != 0 {
		t.Fatalf("both clusters' cold starts made %d model calls, want 0", got)
	}
	if got := len(rec.all()); got != 0 {
		t.Fatalf("both clusters' cold starts sent %d notifications, want 0", got)
	}

	// Each cluster's SECOND reconcile is a genuine transition and must be
	// admitted normally.
	secondA := watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web2", "ImagePullBackOff", t0),
	}}
	secondB := watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "cart2", "CrashLoopBackOff", t0),
	}}
	e.Consider("cluster-a", secondA, clusterhealth.ClusterHealth{}, nil, nil, t0)
	e.Consider("cluster-b", secondB, clusterhealth.ClusterHealth{}, nil, nil, t0)

	waitFor(t, "both clusters' second reconciles to be explained", func() bool { return fc.callCount() == 2 })
	if got := len(rec.all()); got != 2 {
		t.Errorf("sent %d notifications, want 2 (one per cluster's second reconcile)", got)
	}
}

// TestExplainerConcurrentConsiderStatsLatestIsRaceFree pins that one shared
// *Explainer is safe for the concurrent use Task 4 subjects it to: every
// clusterWorker goroutine calls Consider, Stats and Latest on the SAME
// Explainer, because the hourly budget is a process-wide cost control, not a
// per-cluster one (internal/watch/cluster.go hands every worker the same
// pointer). Before the guard existed, this reliably failed under
// go test -race: primed and dropped were read/written with no synchronization,
// and Consider's calls into the shared Throttle raced the same way
// TestThrottleConcurrentUseIsRaceFree pins directly.
//
// internal/watch's TestRun_ExplainRunsAcrossMultipleClusters exercises the same
// scenario through the real daemon end-to-end; this test isolates it to the
// package that owns the fix.
func TestExplainerConcurrentConsiderStatsLatestIsRaceFree(t *testing.T) {
	e, _, _, cancel := harness(t, Config{Cooldown: 0, Budget: 1000000})
	defer func() { cancel(); e.Close() }()

	clusters := []string{"prod-eu", "prod-us", "prod-ap"}
	var wg sync.WaitGroup
	for _, cluster := range clusters {
		wg.Add(1)
		go func(cluster string) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				now := t0.Add(time.Duration(i) * time.Millisecond)
				d := watchstate.Delta{New: []watchstate.Record{
					newRecord("Deployment", "shop", fmt.Sprintf("w%d", i), "Degraded", now),
				}}
				e.Consider(cluster, d, clusterhealth.ClusterHealth{}, nil, nil, now)
				e.Stats(now)
				e.Latest()
			}
		}(cluster)
	}
	wg.Wait()
}

func TestCloseBeforeStartReturnsImmediately(t *testing.T) {
	e := New(Config{Client: &fakeClient{out: "x"}, Notify: func(alertstate.Notification) {}, Budget: 1})
	done := make(chan struct{})
	go func() { e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close on an unstarted explainer must not block")
	}
}
