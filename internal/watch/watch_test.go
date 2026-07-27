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
	"net/url"
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
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/oncall"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/slo"
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

// testWorker builds a worker with no informers and no client — enough for the
// applyResult tests, which drive the fold directly rather than through a
// reconcile.
func testWorker(m *metrics, tr *watchstate.Tracker) *clusterWorker {
	return &clusterWorker{
		name:   defaultClusterName,
		m:      m,
		tr:     tr,
		roller: alertstate.New(alertstate.Options{Cluster: defaultClusterName}),
	}
}

// TestApplyResult_EvaluationErrorNeverReachesTheTracker pins the core invariant:
// a failed evaluation is not "all clear". If the error path reached Observe, one
// API blip would resolve every issue and re-fire them all on the next success —
// corrupting MTTR, inflating flap counts, and (once alerting lands) paging the
// on-call for a network hiccup.
func TestApplyResult_EvaluationErrorNeverReachesTheTracker(t *testing.T) {
	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

	w := testWorker(m, tr)
	captureLog(t, func() { w.applyResult(sampleResult(), time.Millisecond, at, nil) })
	before := len(tr.Active())
	if before == 0 {
		t.Fatal("fixture must produce active issues")
	}

	out := captureLog(t, func() {
		w.applyResult(&scan.Result{}, time.Millisecond, at.Add(time.Minute), errors.New("boom"))
	})
	if got := len(tr.Active()); got != before {
		t.Errorf("active issues %d -> %d; an evaluation error must resolve nothing", before, got)
	}
	if s := tr.Stats(); s.ResolvedTotal != 0 {
		t.Errorf("ResolvedTotal = %d, want 0", s.ResolvedTotal)
	}
	if !strings.Contains(out, "[local] evaluation error: boom") {
		t.Errorf("error must be logged, got %q", out)
	}
}

// TestApplyResult_EvaluationErrorIsRedactedInLogAndTheServedJSON pins the fix for
// the multi-cluster hub's credential leak: a watched cluster's API server URL
// comes from its kubeconfig, which can validly embed basic-auth userinfo or an
// auth-proxy token in the query string. client-go surfaces an unreachable
// server as a *url.Error whose Error() stringifies the full request URL, so
// both the daemon's log stream and the /issues roster must see the redacted
// (scheme://host-only) form, never the raw one — while still naming the host
// and the underlying cause so the operator can tell what actually went wrong.
func TestApplyResult_EvaluationErrorIsRedactedInLogAndTheServedJSON(t *testing.T) {
	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	w := testWorker(m, tr)

	// Fake, non-resolvable fixture values only: a .invalid host with obviously
	// fake userinfo and an obviously fake query-string token.
	const scheme, host = "https", "cluster.invalid:6443"
	userinfoErr := &url.Error{Op: "Get", URL: scheme + "://admin:s3cr3t@" + host + "/api", Err: errors.New("connection refused")}
	queryErr := &url.Error{Op: "Get", URL: scheme + "://" + host + "/api?access_token=t0psecret", Err: errors.New("net/http: TLS handshake timeout")}

	leaks := []string{"admin", "s3cr3t", "access_token", "t0psecret"}

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"userinfo", userinfoErr, "connection refused"},
		{"query string", queryErr, "TLS handshake timeout"},
	} {
		out := captureLog(t, func() {
			w.applyResult(&scan.Result{}, time.Millisecond, at, tc.err)
		})
		for _, leak := range leaks {
			if strings.Contains(out, leak) {
				t.Errorf("%s: log line leaked credential material %q: %q", tc.name, leak, out)
			}
		}
		if !strings.Contains(out, scheme+"://"+host) {
			t.Errorf("%s: log line must still name the host, got %q", tc.name, out)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: log line must stay diagnostic (%q), got %q", tc.name, tc.want, out)
		}

		body, err := m.issuesJSON()
		if err != nil {
			t.Fatalf("%s: issuesJSON: %v", tc.name, err)
		}
		for _, leak := range leaks {
			if strings.Contains(string(body), leak) {
				t.Errorf("%s: served /issues JSON leaked credential material %q: %s", tc.name, leak, body)
			}
		}
		if !strings.Contains(string(body), scheme+"://"+host) {
			t.Errorf("%s: served /issues JSON must still name the host: %s", tc.name, body)
		}
	}

	// A non-URL error (RBAC, TLS-without-a-URL, a plain scan error) must survive
	// intact: over-redacting it to "error" would make the feature useless for
	// the operator it exists to serve.
	plain := errors.New("etcd is unhealthy: at least one member is unreachable")
	out := captureLog(t, func() {
		w.applyResult(&scan.Result{}, time.Millisecond, at, plain)
	})
	if !strings.Contains(out, "etcd is unhealthy: at least one member is unreachable") {
		t.Errorf("non-url error must survive unredacted, got %q", out)
	}
}

func TestApplyResult_LogsTransitionsAndStaysQuietInSteadyState(t *testing.T) {
	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	w := testWorker(m, tr)

	first := captureLog(t, func() { w.applyResult(sampleResult(), time.Millisecond, at, nil) })
	if !strings.Contains(first, "[local] NEW Deployment/shop/web:CrashLoopBackOff") {
		t.Errorf("first sighting must log a NEW line, got %q", first)
	}
	if !strings.Contains(first, "issue(s) active,") {
		t.Errorf("summary line missing from %q", first)
	}

	steady := captureLog(t, func() {
		w.applyResult(sampleResult(), time.Millisecond, at.Add(time.Minute), nil)
	})
	if steady != "" {
		t.Errorf("an unchanged reconcile must log nothing, got %q", steady)
	}

	cleared := captureLog(t, func() {
		w.applyResult(&scan.Result{}, time.Millisecond, at.Add(2*time.Minute), nil)
	})
	if !strings.Contains(cleared, "[local] RESOLVED Deployment/shop/web:CrashLoopBackOff (fired for 2m0s)") {
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
		logDelta(defaultClusterName, res, watchstate.Delta{NewlyFlapping: []watchstate.Record{rec}}, 1, 30*time.Minute)
	})
	if !strings.Contains(out, "[local] FLAPPING Deployment/prod/api:CrashLoopBackOff (3 firings in 30m0s)") {
		t.Errorf("flap line missing from %q", out)
	}
	if !strings.Contains(out, "[local] cluster Degraded (2/3 nodes ready) — 1 issue(s) active, 0 new, 0 resolved") {
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
		runErr <- Run(ctx, []Target{{Name: "local", Client: client}}, Config{
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
	go func() { done <- Run(ctx, []Target{{Name: "local", Client: client}}, cfg) }()

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
	roller := alertstate.New(alertstate.Options{Cluster: defaultClusterName})
	al.notify(roller, tr, time.Now()) // must not panic
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
	al := &alerter{sink: sink}

	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	w := testWorker(m, tr)
	w.al = al

	captureLog(t, func() {
		w.applyResult(&scan.Result{}, time.Millisecond, at, errors.New("boom"))
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
	al := &alerter{sink: sink}

	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	w := testWorker(m, tr)
	w.al = al
	captureLog(t, func() { w.applyResult(sampleResult(), time.Millisecond, at, nil) })

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
	al := &alerter{sink: sink}

	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	sloTr, sloN := newSLOTracker(defaultClusterName, Config{SLOTarget: 0.999, Heartbeat: time.Minute, AlertRepeat: time.Hour})
	w := testWorker(m, tr)
	w.al = al
	w.sloTr, w.sloN = sloTr, sloN

	broken := sampleResult() // one broken workload, unchanged for the whole run

	now := sloBase
	captureLog(t, func() { w.applyResult(broken, time.Millisecond, now, nil) })
	for elapsed := time.Duration(0); elapsed < 6*time.Hour; elapsed += time.Minute {
		now = now.Add(time.Minute)
		captureLog(t, func() { w.applyResult(broken, time.Millisecond, now, nil) })
	}

	// hasSLOIdentity is shared by the wait loop and the final assertion below so
	// the two can never drift apart. That drift was the flake: applyResult logs
	// object-level notifications (al.notify) before the SLO one (al.enqueue), so
	// waiting on "any body has arrived" let the test cancel the context and call
	// sink.Close() while the SLO body was still sitting behind them in the
	// queue — Close only waits for the sender goroutine to exit, it does not
	// drain the queue first, and deliver() then built its request against the
	// already-cancelled context and dropped it. Waiting on the exact predicate
	// the assertion checks means the SLO body is guaranteed to have already
	// arrived by the time cancel/Close run.
	hasSLOIdentity := func(bs []string) bool {
		for _, b := range bs {
			if strings.Contains(b, `"kind":"SLO"`) && strings.Contains(b, `"name":"error-budget"`) {
				return true
			}
		}
		return false
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		arrived := hasSLOIdentity(bodies)
		mu.Unlock()
		if arrived {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	sink.Close()

	mu.Lock()
	defer mu.Unlock()
	if !hasSLOIdentity(bodies) {
		t.Errorf("no delivered notification carried the SLO/error-budget identity across %d bodies", len(bodies))
	}
}

// TestApplyResult_LogsTheBurnTransition proves applyResult itself calls
// logSLO on the firing edge, not just sloN.step to decide whether to. Every
// captureLog call in this file exists only to silence log output during a
// test, not to inspect it — deleting the logSLO(n, v) call in applyResult's
// SLO block fails no other test. This drives applyResult with the same
// broken-for-6h fixture as TestApplyResult_SLOBurnReachesTheSink above and
// asserts the captured log actually contains the NEW burn line logSLO builds.
//
// The fixture's single workload carries a Finding on every reconcile, so its
// Census reports good=0 for the whole run: Availability is exactly 0
// from the first broken sample onward, which pins BurnRate at exactly
// (1-0)/(1-0.999) = 1000 for both windows from the moment either window holds
// any data at all. That value does not drift as coverage keeps climbing
// toward 1 over the run, so it can be asserted as a literal here instead of
// read back off the tracker after the fact — unlike coverage, which does
// drift with exactly when the firing edge lands and is deliberately not
// asserted on.
func TestApplyResult_LogsTheBurnTransition(t *testing.T) {
	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	sloTr, sloN := newSLOTracker(defaultClusterName, Config{SLOTarget: 0.999, Heartbeat: time.Minute, AlertRepeat: time.Hour})
	w := testWorker(m, tr)
	w.sloTr, w.sloN = sloTr, sloN

	broken := sampleResult() // one broken workload, unchanged for the whole run

	now := sloBase
	out := captureLog(t, func() {
		w.applyResult(broken, time.Millisecond, now, nil)
		for elapsed := time.Duration(0); elapsed < 6*time.Hour; elapsed += time.Minute {
			now = now.Add(time.Minute)
			w.applyResult(broken, time.Millisecond, now, nil)
		}
	})

	if v := sloTr.Verdict(now); !v.Firing {
		t.Fatalf("test setup did not cross the burn thresholds by the end of the run (fast=%.1fx/%.0f%% slow=%.1fx/%.0f%%); fixture needs adjusting",
			v.Fast.BurnRate, v.Fast.Coverage*100, v.Slow.BurnRate, v.Slow.Coverage*100)
	}

	want := "kubeagent: [local] NEW SLO/error-budget:ErrorBudgetBurn (fast=1000.0x slow=1000.0x"
	if !strings.Contains(out, want) {
		t.Errorf("captured log did not contain the burn transition line starting %q; got:\n%s", want, out)
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

	err := Run(ctx, []Target{{Name: "local", Client: client}}, Config{
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

	runErr := Run(ctx, []Target{{Name: "local", Client: client}}, Config{
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

func TestApplyResult_HealthyClusterStillRecordsASample(t *testing.T) {
	// The defect this fixes: on a healthy cluster the display-filtered workload
	// list is empty, so the old census reported total==0, Observe recorded
	// nothing, and window coverage never left zero — the coverage gate could
	// never open and the daemon could never page.
	m := newMetrics([]string{defaultClusterName})
	tr := watchstate.New(watchstate.Options{})
	sloTr := slo.New(slo.Options{Target: 0.999, MaxSampleGap: 2 * time.Minute})
	sloN := newSLONotifier(defaultClusterName, time.Hour)
	w := testWorker(m, tr)
	w.sloTr, w.sloN = sloTr, sloN

	var res scan.Result
	res.Health.Verdict = "Healthy"
	res.Inventory.Census = inventory.Census{Good: 5, Total: 5}
	// Workloads deliberately left empty: that is what a healthy cluster looks like.

	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	w.applyResult(&res, 0, t0, nil)
	w.applyResult(&res, 0, t0.Add(30*time.Second), nil)

	got := sloTr.Report(slo.Fast, t0.Add(30*time.Second))
	if got.Coverage <= 0 {
		t.Errorf("Coverage = %v, want > 0: a healthy cluster must accumulate window coverage", got.Coverage)
	}
	if got.Availability != 1 {
		t.Errorf("Availability = %v, want 1 on an all-good census", got.Availability)
	}
}

func TestValidateExplainRejectsBadBudgetAndCooldown(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"zero budget", Config{Explain: true, ExplainBudget: 0, ExplainCooldown: time.Hour}, "budget"},
		{"negative budget", Config{Explain: true, ExplainBudget: -1, ExplainCooldown: time.Hour}, "budget"},
		{"negative cooldown", Config{Explain: true, ExplainBudget: 20, ExplainCooldown: -time.Second}, "cooldown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExplain(tc.cfg)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateExplainAcceptsZeroCooldownAndIsSkippedWhenOff(t *testing.T) {
	if err := validateExplain(Config{Explain: true, ExplainBudget: 1, ExplainCooldown: 0}); err != nil {
		t.Errorf("a zero cooldown is legal (budget is then the only limit): %v", err)
	}
	if err := validateExplain(Config{Explain: false, ExplainBudget: 0, ExplainCooldown: -1}); err != nil {
		t.Errorf("validation must not run when --explain is off: %v", err)
	}
}

// The explainer produces for the alert sink, so it must be fully stopped before
// the sink is closed. alert.Sink never closes its queue channel, so a late
// Enqueue does not panic — it does something quieter and worse: the
// notification lands in a buffer whose sender has already returned, and is
// never delivered and never counted as a drop. Asserting "no panic" would
// therefore prove nothing; the order itself is the assertion.
func TestRunTeardownOrderStopsTheExplainerBeforeTheSink(t *testing.T) {
	var mu sync.Mutex
	var steps []string
	teardownOrder = func(step string) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, step)
	}
	defer func() { teardownOrder = nil }()

	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run must tear down immediately

	cfg := Config{
		MetricsAddr:     "127.0.0.1:0",
		Heartbeat:       time.Hour,
		Debounce:        time.Hour,
		AlertURL:        "http://127.0.0.1:1/hook",
		AlertFormat:     "json",
		AlertRepeat:     time.Hour,
		Explain:         true,
		ExplainEndpoint: "http://127.0.0.1:1/v1",
		ExplainModel:    "test-model",
		ExplainBudget:   20,
		ExplainCooldown: time.Hour,
	}
	if err := Run(ctx, []Target{{Name: "local", Client: fake.NewSimpleClientset()}}, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), steps...)
	mu.Unlock()
	want := []string{"stopExplain", "explainerClose", "stopAlerts", "sinkClose"}
	if len(got) != len(want) {
		t.Fatalf("teardown steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("teardown steps = %v, want %v", got, want)
		}
	}
}

// TestNewExplainerRedactsTheEndpointCredential pins the credential-redaction
// rule (see redact.URL) for the watch enablement log line: an endpoint
// URL is treated as a bearer credential, so the log must carry no more of it
// than scheme://host.
func TestNewExplainerRedactsTheEndpointCredential(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	cfg := Config{
		Explain:         true,
		ExplainEndpoint: "http://127.0.0.1:1/v1/chat/completions?token=<PLACEHOLDER>",
		ExplainModel:    "test-model",
		ExplainBudget:   20,
		ExplainCooldown: time.Hour,
	}

	var ex *oncall.Explainer
	out := captureLog(t, func() {
		ex = newExplainer(ctx, cfg, nil)
	})
	cancel()
	ex.Close()

	if !strings.Contains(out, "backend=http://127.0.0.1:1,") {
		t.Errorf("log = %q, want the redacted backend http://127.0.0.1:1", out)
	}
	if strings.Contains(out, "/v1/chat/completions") {
		t.Errorf("log = %q, leaked the endpoint path", out)
	}
	if strings.Contains(out, "token=") || strings.Contains(out, "<PLACEHOLDER>") {
		t.Errorf("log = %q, leaked the credential", out)
	}
}

// TestNewExplainerLogsWhetherTheLocalKeyIsSet covers a silent misconfiguration:
// a local endpoint may legitimately need no API key, so the Helm chart marks the
// key's secretKeyRef optional — which means a mistyped Secret key produces no
// pod-start failure and no error, just unauthenticated model calls. The startup
// line has to say which case it is, without ever carrying the key itself.
func TestNewExplainerLogsWhetherTheLocalKeyIsSet(t *testing.T) {
	base := Config{
		Explain:         true,
		ExplainEndpoint: "http://127.0.0.1:1/v1",
		ExplainModel:    "test-model",
		ExplainBudget:   20,
		ExplainCooldown: time.Hour,
	}

	for _, tc := range []struct {
		name, key, want, reject string
	}{
		{"key set", "<PLACEHOLDER>", "api-key=set", "<PLACEHOLDER>"},
		{"no key", "", "api-key=absent", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cfg := base
			cfg.ExplainAPIKey = tc.key

			var ex *oncall.Explainer
			out := captureLog(t, func() { ex = newExplainer(ctx, cfg, nil) })
			cancel()
			ex.Close()

			if !strings.Contains(out, tc.want) {
				t.Errorf("log = %q, want it to report %s", out, tc.want)
			}
			if tc.reject != "" && strings.Contains(out, tc.reject) {
				t.Errorf("log = %q, leaked the API key", out)
			}
		})
	}
}

// TestNewExplainerSaysNothingAboutAKeyOnTheAnthropicPath guards against a
// misleading line: on the Anthropic path the key is read from the environment by
// the explain client, not carried in Config, so reporting it as absent here
// would be wrong.
func TestNewExplainerSaysNothingAboutAKeyOnTheAnthropicPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := Config{Explain: true, ExplainModel: "test-model", ExplainBudget: 20, ExplainCooldown: time.Hour}

	var ex *oncall.Explainer
	out := captureLog(t, func() { ex = newExplainer(ctx, cfg, nil) })
	cancel()
	ex.Close()

	if strings.Contains(out, "api-key=") {
		t.Errorf("log = %q, want no api-key field on the anthropic path", out)
	}
}

// freeLoopbackAddr reserves a loopback port and releases it, so the daemon can
// bind it. Racy in principle, fine in a test binary.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func waitForReady(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never reported ready within 10s", url)
}

func httpGetBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	return string(body)
}

// TestRun_OneBrokenClusterDoesNotStopTheOthers is the isolation guarantee. A
// remote cluster going away must degrade to a per-cluster reading, not take the
// daemon with it — and /readyz must still report ready, because a NotReady pod
// leaves its Service endpoints and Prometheus then stops scraping the clusters
// that ARE working.
func TestRun_OneBrokenClusterDoesNotStopTheOthers(t *testing.T) {
	// The bad cluster's List always errors, so its informers never sync and its
	// worker blocks in WaitForCacheSync for the full cacheSyncTimeout before its
	// first (failing) reconcile — that block is exactly what proves the daemon's
	// readiness does not wait on it either. Shrinking the bound here is what
	// keeps that real wait in milliseconds instead of the production 30s.
	//
	// This save/defer-restore has no lock: it relies on this test never running
	// with t.Parallel() (nor any other test in this package). Do not add
	// t.Parallel() here without giving cacheSyncTimeout a real guard first.
	origTimeout := cacheSyncTimeout
	cacheSyncTimeout = 200 * time.Millisecond
	defer func() { cacheSyncTimeout = origTimeout }()

	good := fake.NewSimpleClientset()
	bad := fake.NewSimpleClientset()
	bad.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, []Target{{Name: "good", Client: good}, {Name: "bad", Client: bad}}, Config{
			MetricsAddr: addr,
			Heartbeat:   time.Hour,
			Debounce:    10 * time.Millisecond,
		})
	}()

	// Ready means "every cluster finished a first attempt", so this returning 200
	// is itself the assertion that the broken cluster did not wedge readiness.
	waitForReady(t, "http://"+addr+"/readyz")

	body := httpGetBody(t, "http://"+addr+"/metrics")
	if !strings.Contains(body, `kubeagent_cluster_up{cluster="good"} 1`) {
		t.Errorf("working cluster must report up=1\n%s", body)
	}
	if !strings.Contains(body, `kubeagent_cluster_up{cluster="bad"} 0`) {
		t.Errorf("broken cluster must report up=0\n%s", body)
	}
	if !strings.Contains(body, "kubeagent_clusters_total 2") {
		t.Errorf("both clusters must be counted\n%s", body)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of cancellation")
	}
}

// TestRun_ExplainRunsAcrossMultipleClusters exercises --explain with more than
// one target, which no other test in this package did: the existing
// multi-target test, TestRun_OneBrokenClusterDoesNotStopTheOthers, never sets
// Explain, so its ex is nil and every oncall method short-circuits on its
// nil-receiver guard before touching any shared state. Here two clusterWorker
// goroutines really do call Consider/Stats/Latest on the one shared
// *oncall.Explainer concurrently — this is the path go test -race must clear
// (see internal/oncall's TestExplainerConcurrentConsiderStatsLatestIsRaceFree
// and TestThrottleConcurrentUseIsRaceFree for the isolated version of the same
// bug).
//
// Both clusters start with nothing broken so their first reconcile — the
// cold-start snapshot that the Explainer discards no matter which cluster's
// worker reaches it first — settles before either cluster has anything worth
// explaining. Only once both are past that point does the test create a
// crashing pod in each cluster, so the resulting explanations are genuine
// new-incident transitions, not the swallowed initial snapshot.
func TestRun_ExplainRunsAcrossMultipleClusters(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		io.WriteString(w, `{"choices":[{"message":{"content":"explanation text"}}]}`)
	}))
	defer srv.Close()

	east := fake.NewSimpleClientset()
	west := fake.NewSimpleClientset()

	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, []Target{{Name: "east", Client: east}, {Name: "west", Client: west}}, Config{
			MetricsAddr:     addr,
			Heartbeat:       time.Hour,
			Debounce:        10 * time.Millisecond,
			Explain:         true,
			ExplainEndpoint: srv.URL,
			ExplainModel:    "test-model",
			ExplainBudget:   1000,
			ExplainCooldown: 0,
		})
	}()

	waitForReady(t, "http://"+addr+"/readyz")
	// Let both clusters' cold-start reconcile land before introducing a real
	// incident in either.
	time.Sleep(100 * time.Millisecond)

	crashPod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop", Labels: map[string]string{"app": "web"}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "web", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}}},
		}
	}
	if _, err := east.CoreV1().Pods("shop").Create(context.Background(), crashPod(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the crash pod in east: %v", err)
	}
	if _, err := west.CoreV1().Pods("shop").Create(context.Background(), crashPod(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the crash pod in west: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := calls
		mu.Unlock()
		if got >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got < 2 {
		t.Fatalf("model calls = %d, want at least 2 (one explanation per cluster)", got)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of cancellation")
	}
}

// TestRun_RejectsADuplicateClusterNameBeforeStartingAnything pins that the
// target check runs with the other config validation, before the metrics server
// listens: once WaitForCacheSync is underway a reachable-but-unresponsive API
// server can hide a config error behind what looks like a cluster hang.
func TestRun_RejectsADuplicateClusterNameBeforeStartingAnything(t *testing.T) {
	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := fake.NewSimpleClientset()

	err := Run(ctx, []Target{{Name: "dup", Client: c}, {Name: "dup", Client: c}}, Config{
		MetricsAddr: addr,
		Heartbeat:   time.Hour,
		Debounce:    time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("Run = %v, want a duplicate-name error", err)
	}
	if _, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond); dialErr == nil {
		t.Error("the metrics server must not be listening after a rejected config")
	}
}
