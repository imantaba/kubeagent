package clusterhealth

import (
	"reflect"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// The nodes, leases and workloads are built here rather than in internal/fuzzgen
// because this is their only caller. fuzzgen owns the shapes several targets
// share and the cursor primitives every target draws from.

// fuzzBase is the clock the heartbeat check is handed, and the instant lease
// renewals are drawn around. Fixed, because a fuzz target that reads the wall
// clock is not reproducible.
var fuzzBase = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// fuzzCluster draws the four inputs Assess takes. A node's name is a DNS
// subdomain the API server validates; its NodeReady condition reason and message
// are free text it does not, so those two come from hostile bytes.
func fuzzCluster(c *fuzzgen.Cursor) ([]corev1.Node, Heartbeat, []string, []inventory.Workload) {
	names := make([]string, 0, 3)
	var nodes []corev1.Node
	for i := 0; i < c.IntN(4); i++ {
		name := c.Name(20)
		names = append(names, name)
		n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
		n.Spec.Unschedulable = c.Bool()
		for k := 0; k < c.IntN(4); k++ {
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
				Type: corev1.NodeConditionType(c.Pick([]string{
					"Ready", "MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable",
				})),
				Status:  corev1.ConditionStatus(c.Pick([]string{"True", "False", "Unknown"})),
				Reason:  c.Hostile(48),
				Message: c.Hostile(160),
			})
		}
		nodes = append(nodes, n)
	}
	if len(names) == 0 {
		names = append(names, c.Name(20))
	}

	hb := Heartbeat{Now: fuzzBase}
	if c.Bool() {
		hb.Threshold = time.Duration(1+c.IntN(120)) * time.Second
	}
	for i := 0; i < c.IntN(4); i++ {
		l := coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: c.Pick(names)}}
		if c.Bool() {
			rt := metav1.NewMicroTime(c.Time(fuzzBase.Add(-time.Hour)).Time)
			l.Spec.RenewTime = &rt
		}
		hb.Leases = append(hb.Leases, l)
	}

	// The expected-node list reaches Assess from an operator flag, not from the
	// cluster, so it is drawn from the same DNS alphabet the API server would
	// have validated — a hostile value here would assert something about the
	// operator's own shell, not about kubeagent's ingress.
	var expected []string
	for i := 0; i < c.IntN(3); i++ {
		expected = append(expected, c.Pick(append([]string{"", "  "}, names...)))
	}

	var workloads []inventory.Workload
	for i := 0; i < c.IntN(4); i++ {
		workloads = append(workloads, inventory.Workload{
			Namespace: c.Pick([]string{systemNamespace, c.Name(20)}),
			Name:      c.Name(20),
			Kind:      c.Pick([]string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}),
			Desired:   c.IntN(6),
			Ready:     c.IntN(6),
			Status:    c.Pick([]string{"Healthy", "Degraded", "Progressing", "Last run failed"}),
		})
	}
	return nodes, hb, expected, workloads
}

// FuzzClusterAssess feeds hostile NodeReady condition text to the cluster
// verdict, whose node-issue lines are built from that reason and message.
func FuzzClusterAssess(f *testing.F) {
	f.Add([]byte("seed"))
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte("\x1b[2J\x1b[H"))
	f.Add([]byte("\xff\xfe\xfd"))
	f.Add([]byte("‮​"))

	f.Fuzz(func(t *testing.T, params []byte) {
		nodes, hb, expected, workloads := fuzzCluster(fuzzgen.New(params))
		got := Assess(nodes, hb, expected, workloads)

		if got.Verdict != "Healthy" && got.Verdict != "Degraded" {
			t.Errorf("Verdict = %q, want Healthy or Degraded", got.Verdict)
		}
		for _, iss := range got.NodeIssues {
			fuzzgen.AssertSafe(t, "nodeIssue", iss)
		}
		for _, iss := range got.SystemIssues {
			fuzzgen.AssertSafe(t, "systemIssue", iss)
		}
		for _, dn := range got.DownNodes {
			fuzzgen.AssertSafe(t, "downNode.name", dn.Name)
			fuzzgen.AssertSafe(t, "downNode.reason", dn.Reason)
			if dn.Reason != "NotReady" && dn.Reason != "kubelet not heartbeating" {
				t.Errorf("DownNode.Reason = %q, want one of this package's two literals", dn.Reason)
			}
		}

		again := Assess(nodes, hb, expected, workloads)
		if !reflect.DeepEqual(got, again) {
			t.Errorf("Assess is not deterministic")
		}
	})
}
