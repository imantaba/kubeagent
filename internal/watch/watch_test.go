package watch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/alert"
	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

// captureLog redirects the standard logger for the duration of fn.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	fn()
	return buf.String()
}

// TestApplyResult_EvaluationErrorNeverReachesTheTracker pins the core invariant:
// a failed evaluation is not "all clear". If the error path reached Observe, one
// API blip would resolve every issue and re-fire them all on the next success —
// corrupting MTTR, inflating flap counts, and (once alerting lands) paging the
// on-call for a network hiccup.
func TestApplyResult_EvaluationErrorNeverReachesTheTracker(t *testing.T) {
	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

	captureLog(t, func() { applyResult(m, tr, nil, nil, nil, sampleResult(), time.Millisecond, at, nil) })
	before := len(tr.Active())
	if before == 0 {
		t.Fatal("fixture must produce active issues")
	}

	out := captureLog(t, func() {
		applyResult(m, tr, nil, nil, nil, &scan.Result{}, time.Millisecond, at.Add(time.Minute), errors.New("boom"))
	})
	if got := len(tr.Active()); got != before {
		t.Errorf("active issues %d -> %d; an evaluation error must resolve nothing", before, got)
	}
	if s := tr.Stats(); s.ResolvedTotal != 0 {
		t.Errorf("ResolvedTotal = %d, want 0", s.ResolvedTotal)
	}
	if !strings.Contains(out, "evaluation error: boom") {
		t.Errorf("error must be logged, got %q", out)
	}
}

func TestApplyResult_LogsTransitionsAndStaysQuietInSteadyState(t *testing.T) {
	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

	first := captureLog(t, func() { applyResult(m, tr, nil, nil, nil, sampleResult(), time.Millisecond, at, nil) })
	if !strings.Contains(first, "NEW Deployment/shop/web:CrashLoopBackOff") {
		t.Errorf("first sighting must log a NEW line, got %q", first)
	}
	if !strings.Contains(first, "issue(s) active,") {
		t.Errorf("summary line missing from %q", first)
	}

	steady := captureLog(t, func() { applyResult(m, tr, nil, nil, nil, sampleResult(), time.Millisecond, at.Add(time.Minute), nil) })
	if steady != "" {
		t.Errorf("an unchanged reconcile must log nothing, got %q", steady)
	}

	cleared := captureLog(t, func() {
		applyResult(m, tr, nil, nil, nil, &scan.Result{}, time.Millisecond, at.Add(2*time.Minute), nil)
	})
	if !strings.Contains(cleared, "RESOLVED Deployment/shop/web:CrashLoopBackOff (fired for 2m0s)") {
		t.Errorf("clearing must log a RESOLVED line with the firing duration, got %q", cleared)
	}
}

func TestLogDelta_ReportsFlapping(t *testing.T) {
	res := &scan.Result{Health: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3}}
	rec := watchstate.Record{
		Key:           watchstate.Key{Kind: "Deployment", Namespace: "prod", Name: "api", Issue: "CrashLoopBackOff"},
		RecentFirings: 3,
	}
	out := captureLog(t, func() {
		logDelta(res, watchstate.Delta{NewlyFlapping: []watchstate.Record{rec}}, 1, 30*time.Minute)
	})
	if !strings.Contains(out, "FLAPPING Deployment/prod/api:CrashLoopBackOff (3 firings in 30m0s)") {
		t.Errorf("flap line missing from %q", out)
	}
	if !strings.Contains(out, "cluster Degraded (2/3 nodes ready) — 1 issue(s) active, 0 new, 0 resolved") {
		t.Errorf("summary line missing from %q", out)
	}
}

// TestRun_GracefulShutdown verifies that Run() starts up correctly (informers
// sync, first reconcile completes, /readyz returns 200) and then shuts down
// cleanly (returns nil, server no longer reachable) when the context is cancelled.
func TestRun_GracefulShutdown(t *testing.T) {
	// Grab a free port so parallel test runs never collide.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	// Build a minimal fake cluster with one Ready node.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	client := fake.NewSimpleClientset(node)

	ctx, cancel := context.WithCancel(context.Background())

	// Run the daemon in the background; capture its return value.
	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, client, Config{
			MetricsAddr: addr,
			Heartbeat:   time.Hour, // prevent periodic reconcile noise during test
			Debounce:    50 * time.Millisecond,
		})
	}()

	// Poll /readyz until the daemon signals it is ready (informers synced,
	// initial reconcile done). Fail if this takes too long.
	readyz := "http://" + addr + "/readyz"
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("timed out waiting for /readyz to return 200")
		}
		resp, err := http.Get(readyz) //nolint:noctx
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Cancel the context — this should trigger a clean shutdown.
	cancel()

	// Run() must return within 3 seconds with a nil error.
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() returned non-nil error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return within 3s after context cancellation")
	}

	// Confirm the HTTP server is actually gone — a subsequent request must fail.
	_, connErr := http.Get(readyz) //nolint:noctx
	if connErr == nil {
		t.Error("expected connection refused after shutdown, but GET /readyz succeeded")
	}
}

// TestRun_RejectsBadAlertConfigBeforeStartingAnything pins that alert
// configuration is validated before anything else starts. A bad --alert-format
// or an alertmanager --alert-repeat above 4m must fail Run() immediately — not
// after WaitForCacheSync, which can block forever against a reachable-but-
// unresponsive API server and hide the operator's config mistake behind what
// looks like a cluster hang.
//
// The fake clientset's own List calls are instant, which would let even the
// buggy ordering slip under a timing deadline by accident. So every List is
// blocked here — standing in for an API server that accepts the connection but
// never answers — which is exactly the condition the fix must not be sensitive
// to. Validation that truly runs first never touches the client at all, so it
// returns regardless; validation that runs after WaitForCacheSync hangs with it.
func TestRun_RejectsBadAlertConfigBeforeStartingAnything(t *testing.T) {
	// Grab a free port so parallel test runs never collide (same convention as
	// TestRun_GracefulShutdown). Validation must fail before this is ever bound,
	// but reserving a real address keeps the test honest either way.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	client := fake.NewSimpleClientset()
	blockList := make(chan struct{})
	t.Cleanup(func() { close(blockList) })
	client.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		<-blockList // never returns during the test: simulates an unresponsive API server
		return false, nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const webhookURL = "https://example.invalid/hook"
	cfg := Config{
		MetricsAddr: addr,
		Heartbeat:   time.Hour,
		Debounce:    50 * time.Millisecond,
		AlertURL:    webhookURL,
		AlertFormat: "alertmanager",
		AlertRepeat: 10 * time.Minute, // exceeds the 4m alertmanager maximum
	}

	done := make(chan error, 1)
	go func() { done <- Run(ctx, client, cfg) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run() returned a nil error for an invalid alert configuration")
		}
		if !strings.Contains(err.Error(), "4m0s") {
			t.Errorf("error = %q, want it to mention the 4m maximum", err)
		}
		if strings.Contains(err.Error(), webhookURL) {
			t.Errorf("error = %q, must not echo the webhook URL (it is a credential)", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Run() did not return promptly for an invalid alert configuration — " +
			"validation is not failing fast, before the informers start (List calls are " +
			"blocked, standing in for an unresponsive API server)")
	}
}

// TestAlerter_NilIsDisabled pins that alerting is off by default: a nil *alerter
// is the "no webhook configured" case and must be inert, not a panic.
func TestAlerter_NilIsDisabled(t *testing.T) {
	var al *alerter
	tr := watchstate.New(watchstate.Options{})
	tr.Observe([]watchstate.Key{{Kind: "Deployment", Namespace: "shop", Name: "web", Issue: "Degraded"}}, time.Now())
	al.notify(tr, time.Now()) // must not panic
	if got := al.stats(); got != (alert.Stats{}) {
		t.Errorf("nil alerter stats = %+v, want the zero value", got)
	}
}

// TestApplyResult_EvaluationErrorSendsNoAlert extends the tracker invariant to the
// outbound path: one API blip must never page the on-call.
func TestApplyResult_EvaluationErrorSendsNoAlert(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink, err := alert.New(alert.Config{URL: srv.URL, Format: alert.FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("alert.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink.Start(ctx)
	al := &alerter{roller: alertstate.New(alertstate.Options{Repeat: time.Hour}), sink: sink}

	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

	captureLog(t, func() {
		applyResult(m, tr, al, nil, nil, &scan.Result{}, time.Millisecond, at, errors.New("boom"))
	})
	cancel()
	sink.Close()

	mu.Lock()
	defer mu.Unlock()
	if posts != 0 {
		t.Errorf("a failed evaluation produced %d alert POSTs, want 0", posts)
	}
}

// TestApplyResult_AlertsOnRealFindings is the happy path: a successful evaluation
// with findings reaches the receiver.
func TestApplyResult_AlertsOnRealFindings(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink, err := alert.New(alert.Config{URL: srv.URL, Format: alert.FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("alert.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink.Start(ctx)
	al := &alerter{roller: alertstate.New(alertstate.Options{Repeat: time.Hour}), sink: sink}

	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	captureLog(t, func() { applyResult(m, tr, al, nil, nil, sampleResult(), time.Millisecond, at, nil) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sink.Stats().FiringOK > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	sink.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("no alert reached the receiver")
	}
	if !strings.Contains(bodies[0], `"status":"firing"`) {
		t.Errorf("first body = %s, want a firing notification", bodies[0])
	}
}

// TestApplyResult_SLOBurnReachesTheSink proves the burn notification is
// actually delivered end-to-end through al.enqueue and the real sink, not just
// constructed and discarded. It drives applyResult with a workload that is
// broken on every sample for a full slow (6h) window, which crosses both burn
// thresholds and both coverage gates and fires the verdict.
//
// The object-level tracker is firing for the same broken workload throughout,
// so the roller (al.notify) is also delivering plenty of NEW/REPEAT
// notifications during this run — this test does not count deliveries, it
// looks for the one body carrying the SLO/error-budget identity specifically,
// which only sloNotifier ever manufactures. A regression that routed the burn
// notification through al.notify instead of al.enqueue (discarding it and
// re-rolling the object tracker instead) would still produce plenty of
// traffic at the receiver, so counting alone would not catch it — the
// object-kind noise would mask a missing SLO delivery.
func TestApplyResult_SLOBurnReachesTheSink(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink, err := alert.New(alert.Config{URL: srv.URL, Format: alert.FormatJSON, Repeat: time.Hour}, nil)
	if err != nil {
		t.Fatalf("alert.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink.Start(ctx)
	al := &alerter{roller: alertstate.New(alertstate.Options{Repeat: time.Hour}), sink: sink}

	m := newMetrics()
	tr := watchstate.New(watchstate.Options{})
	sloTr, sloN := newSLOTracker(Config{SLOTarget: 0.999, Heartbeat: time.Minute, AlertRepeat: time.Hour})

	broken := sampleResult() // one broken workload, unchanged for the whole run

	now := sloBase
	captureLog(t, func() { applyResult(m, tr, al, sloTr, sloN, broken, time.Millisecond, now, nil) })
	for elapsed := time.Duration(0); elapsed < 6*time.Hour; elapsed += time.Minute {
		now = now.Add(time.Minute)
		captureLog(t, func() { applyResult(m, tr, al, sloTr, sloN, broken, time.Millisecond, now, nil) })
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	sink.Close()

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, b := range bodies {
		if strings.Contains(b, `"kind":"SLO"`) && strings.Contains(b, `"name":"error-budget"`) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no delivered notification carried the SLO/error-budget identity across %d bodies", len(bodies))
	}
}

// TestRun_RejectsBadSLOTargetBeforeCacheSync pins that --slo-target is
// validated before anything else starts, exactly like the alert config check
// above. A bad target must fail Run() immediately, not after
// WaitForCacheSync, which can block forever against a reachable-but-
// unresponsive API server and hide the operator's config mistake behind what
// looks like a cluster hang.
func TestRun_RejectsBadSLOTargetBeforeCacheSync(t *testing.T) {
	client := fake.NewSimpleClientset()
	// Block List forever. A config error must surface anyway: if validation ran
	// after WaitForCacheSync, an unresponsive API server would hide it behind
	// what looks like a cluster hang.
	client.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		select {}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Run(ctx, client, Config{
		MetricsAddr: "127.0.0.1:0",
		Heartbeat:   time.Minute,
		Debounce:    time.Second,
		SLOTarget:   1.0,
	})
	if err == nil {
		t.Fatal("Run returned nil for --slo-target 100; want a startup error")
	}
	if !strings.Contains(err.Error(), "slo-target") {
		t.Errorf("error %q does not name the offending flag", err)
	}
}

// TestRun_ValidatesSLOTargetBeforeStartingTheMetricsServer guards the ordering
// itself, not just "Run fails fast": TestRun_RejectsBadSLOTargetBeforeCacheSync
// proves validation runs before the (potentially hanging) WaitForCacheSync, but
// that alone would still pass even if validateSLOTarget moved to anywhere
// earlier than WaitForCacheSync — including after the metrics server has
// already started listening. This test catches that narrower regression
// directly: on a bad --slo-target, the configured metrics port must never
// accept a connection, because Run must return before ever calling
// ListenAndServe.
func TestRun_ValidatesSLOTargetBeforeStartingTheMetricsServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	client := fake.NewSimpleClientset()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := Run(ctx, client, Config{
		MetricsAddr: addr,
		Heartbeat:   time.Minute,
		Debounce:    time.Second,
		SLOTarget:   1.0,
	})
	if runErr == nil {
		t.Fatal("Run returned nil for --slo-target 100; want a startup error")
	}

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 20*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatal("metrics server accepted a connection after a rejected --slo-target; validation must run before ListenAndServe")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
