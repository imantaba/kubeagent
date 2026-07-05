package watch

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/scan"
)

func TestChangeLogger_OnlyLogsOnChange(t *testing.T) {
	healthy := &scan.Result{Health: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 3, NodesTotal: 3}}
	degraded := &scan.Result{Health: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		Inventory: inventory.Result{Workloads: []inventory.Workload{{Namespace: "s", Name: "w", Ready: 0, Desired: 1}}}}

	var cl changeLogger
	if !cl.changed(healthy, nil) {
		t.Error("first observation should count as a change")
	}
	if cl.changed(healthy, nil) {
		t.Error("identical observation should NOT count as a change")
	}
	if !cl.changed(degraded, nil) {
		t.Error("verdict flip should count as a change")
	}
}

func TestSignature_DistinguishesFindingsAndErrors(t *testing.T) {
	a := &scan.Result{Health: clusterhealth.ClusterHealth{Verdict: "Healthy"}}
	if signature(a, nil) == signature(a, errDummy) {
		t.Error("error vs no-error must produce different signatures")
	}
}

var errDummy = errStr("x")

type errStr string

func (e errStr) Error() string { return string(e) }

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
