package watch

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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

	captureLog(t, func() { applyResult(m, tr, sampleResult(), time.Millisecond, at, nil) })
	before := len(tr.Active())
	if before == 0 {
		t.Fatal("fixture must produce active issues")
	}

	out := captureLog(t, func() {
		applyResult(m, tr, &scan.Result{}, time.Millisecond, at.Add(time.Minute), errors.New("boom"))
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

	first := captureLog(t, func() { applyResult(m, tr, sampleResult(), time.Millisecond, at, nil) })
	if !strings.Contains(first, "NEW Deployment/shop/web:CrashLoopBackOff") {
		t.Errorf("first sighting must log a NEW line, got %q", first)
	}
	if !strings.Contains(first, "issue(s) active,") {
		t.Errorf("summary line missing from %q", first)
	}

	steady := captureLog(t, func() { applyResult(m, tr, sampleResult(), time.Millisecond, at.Add(time.Minute), nil) })
	if steady != "" {
		t.Errorf("an unchanged reconcile must log nothing, got %q", steady)
	}

	cleared := captureLog(t, func() {
		applyResult(m, tr, &scan.Result{}, time.Millisecond, at.Add(2*time.Minute), nil)
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
