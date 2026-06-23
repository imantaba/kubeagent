package clusterhealth

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// node builds a Node with the given Ready status, optional pressure conditions,
// and cordon flag.
func node(name string, ready bool, pressures []corev1.NodeConditionType, cordoned bool) corev1.Node {
	conds := []corev1.NodeCondition{{Type: corev1.NodeReady, Status: condStatus(ready)}}
	for _, p := range pressures {
		conds = append(conds, corev1.NodeCondition{Type: p, Status: corev1.ConditionTrue})
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status:     corev1.NodeStatus{Conditions: conds},
	}
}

func condStatus(b bool) corev1.ConditionStatus {
	if b {
		return corev1.ConditionTrue
	}
	return corev1.ConditionFalse
}

func TestNodeHealth(t *testing.T) {
	ready, issues := nodeHealth(node("n1", true, nil, false))
	if !ready || len(issues) != 0 {
		t.Errorf("healthy node: ready=%v issues=%v", ready, issues)
	}

	ready, issues = nodeHealth(node("n2", false, nil, false))
	if ready {
		t.Error("not-ready node reported ready")
	}
	if len(issues) != 1 || issues[0] != "NotReady" {
		t.Errorf("expected [NotReady], got %v", issues)
	}

	// Ready but under disk pressure: counts as ready, but is an issue.
	ready, issues = nodeHealth(node("n3", true, []corev1.NodeConditionType{corev1.NodeDiskPressure}, false))
	if !ready {
		t.Error("pressured-but-ready node should still be ready")
	}
	if len(issues) != 1 || issues[0] != "DiskPressure" {
		t.Errorf("expected [DiskPressure], got %v", issues)
	}

	ready, issues = nodeHealth(node("n4", true, nil, true))
	if !ready {
		t.Error("cordoned node should still be Ready")
	}
	if len(issues) != 1 || issues[0] != "SchedulingDisabled" {
		t.Errorf("expected [SchedulingDisabled], got %v", issues)
	}
}

func TestAssess_HealthyClusterAndSystem(t *testing.T) {
	nodes := []corev1.Node{node("a", true, nil, false), node("b", true, nil, false)}
	workloads := []inventory.Workload{
		{Namespace: "kube-system", Name: "coredns", Ready: 2, Desired: 2, Status: "Running"},
		{Namespace: "default", Name: "web", Ready: 1, Desired: 2, Status: "Degraded"}, // not kube-system → ignored
	}
	ch := Assess(nodes, workloads)
	if ch.Verdict != "Healthy" {
		t.Errorf("verdict = %q, want Healthy", ch.Verdict)
	}
	if ch.NodesTotal != 2 || ch.NodesReady != 2 {
		t.Errorf("nodes = %d/%d, want 2/2", ch.NodesReady, ch.NodesTotal)
	}
	if len(ch.NodeIssues) != 0 || len(ch.SystemIssues) != 0 {
		t.Errorf("expected no issues, got node=%v system=%v", ch.NodeIssues, ch.SystemIssues)
	}
}

func TestAssess_DegradedByNodeAndSystem(t *testing.T) {
	nodes := []corev1.Node{
		node("a", true, nil, false),
		node("b", false, nil, false), // NotReady
	}
	workloads := []inventory.Workload{
		{Namespace: "kube-system", Name: "coredns", Ready: 1, Desired: 2, Status: "Degraded"},
	}
	ch := Assess(nodes, workloads)
	if ch.Verdict != "Degraded" {
		t.Errorf("verdict = %q, want Degraded", ch.Verdict)
	}
	if ch.NodesReady != 1 || ch.NodesTotal != 2 {
		t.Errorf("nodes = %d/%d, want 1/2", ch.NodesReady, ch.NodesTotal)
	}
	if len(ch.NodeIssues) != 1 || ch.NodeIssues[0] != "b NotReady" {
		t.Errorf("node issues = %v, want [b NotReady]", ch.NodeIssues)
	}
	if len(ch.SystemIssues) != 1 || ch.SystemIssues[0] != "kube-system/coredns 1/2 Degraded" {
		t.Errorf("system issues = %v", ch.SystemIssues)
	}
}
