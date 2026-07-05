package watch

import (
	"testing"

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
