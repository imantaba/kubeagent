package clusterhealth

import (
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
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
	ch := Assess(nodes, Heartbeat{}, workloads)
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

func TestNamespaceScopeNote(t *testing.T) {
	if NamespaceScopeNote("") != "" {
		t.Error("all-namespaces should have no caveat")
	}
	if NamespaceScopeNote("kube-system") != "" {
		t.Error("-n kube-system should have no caveat")
	}
	if NamespaceScopeNote("cattle-system") == "" {
		t.Error("-n cattle-system should produce a caveat")
	}
}

func TestAssess_SystemJobFailedOmitsCount(t *testing.T) {
	nodes := []corev1.Node{node("a", true, nil, false)}
	workloads := []inventory.Workload{{Namespace: "kube-system", Name: "migrate", Kind: "Job", Status: "Failed"}}
	ch := Assess(nodes, Heartbeat{}, workloads)
	if ch.Verdict != "Degraded" {
		t.Fatalf("verdict = %q, want Degraded", ch.Verdict)
	}
	if len(ch.SystemIssues) != 1 || ch.SystemIssues[0] != "kube-system/migrate Failed" {
		t.Errorf("system issues = %v, want [kube-system/migrate Failed]", ch.SystemIssues)
	}
}

// notReadyNode builds a node whose NodeReady condition is False with the given
// reason and message.
func notReadyNode(name, reason, message string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: reason, Message: message},
		}},
	}
}

func TestNodeHealth_NotReadyIncludesReasonAndMessage(t *testing.T) {
	_, issues := nodeHealth(notReadyNode("n1", "KubeletNotReady", "container runtime network not ready: cni config uninitialized"))
	if len(issues) != 1 {
		t.Fatalf("want one issue, got %v", issues)
	}
	want := "NotReady: KubeletNotReady — container runtime network not ready: cni config uninitialized"
	if issues[0] != want {
		t.Errorf("want %q, got %q", want, issues[0])
	}
}

func TestNodeHealth_NotReadyTrimsLongMessage(t *testing.T) {
	long := "KubeletNotReady"
	msg := ""
	for i := 0; i < 200; i++ {
		msg += "x"
	}
	_, issues := nodeHealth(notReadyNode("n1", long, msg))
	if len(issues) != 1 {
		t.Fatalf("want one issue, got %v", issues)
	}
	if []rune(issues[0])[len([]rune(issues[0]))-1] != '…' {
		t.Errorf("expected a trailing ellipsis on a truncated message: %q", issues[0])
	}
	// "NotReady: KubeletNotReady — " prefix + 120 runes + "…"
	if n := len([]rune(issues[0])); n > 160 {
		t.Errorf("issue string too long (%d runes): %q", n, issues[0])
	}
}

func TestNodeHealth_NotReadyFallsBackWhenEmpty(t *testing.T) {
	_, issues := nodeHealth(notReadyNode("n1", "", ""))
	if len(issues) != 1 || issues[0] != "NotReady" {
		t.Errorf("want plain NotReady, got %v", issues)
	}
}

func TestNodeHealth_FirstLineOfMessageOnly(t *testing.T) {
	_, issues := nodeHealth(notReadyNode("n1", "KubeletNotReady", "first line\nsecond line"))
	if len(issues) != 1 || issues[0] != "NotReady: KubeletNotReady — first line" {
		t.Errorf("want only the first line of the message, got %v", issues)
	}
}

func TestAssess_NotReadyIssueCarriesNodeNameAndReason(t *testing.T) {
	ch := Assess([]corev1.Node{notReadyNode("worker-2", "KubeletNotReady", "kubelet stopped posting node status")}, Heartbeat{}, nil)
	if ch.Verdict != "Degraded" {
		t.Errorf("a NotReady node should still make the cluster Degraded, got %q", ch.Verdict)
	}
	if len(ch.NodeIssues) != 1 || ch.NodeIssues[0] != "worker-2 NotReady: KubeletNotReady — kubelet stopped posting node status" {
		t.Errorf("want the node name + enriched NotReady, got %v", ch.NodeIssues)
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
	ch := Assess(nodes, Heartbeat{}, workloads)
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

func hbReadyNode(name string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func hbLease(node string, renew time.Time) coordinationv1.Lease {
	rt := metav1.NewMicroTime(renew)
	return coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-node-lease", Name: node},
		Spec:       coordinationv1.LeaseSpec{RenewTime: &rt},
	}
}

func TestAssess_StaleHeartbeatDegrades(t *testing.T) {
	now := time.Now()
	hb := Heartbeat{Leases: []coordinationv1.Lease{hbLease("w1", now.Add(-90 * time.Second))}, Now: now, Threshold: 40 * time.Second}
	ch := Assess([]corev1.Node{hbReadyNode("w1")}, hb, nil)
	if ch.Verdict != "Degraded" || ch.NodesStaleHeartbeat != 1 {
		t.Fatalf("stale lease must degrade + count: %+v", ch)
	}
	if len(ch.NodeIssues) != 1 || !strings.Contains(ch.NodeIssues[0], "kubelet not heartbeating") {
		t.Errorf("want a heartbeat issue, got %+v", ch.NodeIssues)
	}
}

func TestAssess_FreshHeartbeatClean(t *testing.T) {
	now := time.Now()
	hb := Heartbeat{Leases: []coordinationv1.Lease{hbLease("w1", now.Add(-5 * time.Second))}, Now: now, Threshold: 40 * time.Second}
	ch := Assess([]corev1.Node{hbReadyNode("w1")}, hb, nil)
	if ch.Verdict != "Healthy" || ch.NodesStaleHeartbeat != 0 {
		t.Errorf("fresh lease must stay Healthy: %+v", ch)
	}
}

func TestAssess_MissingLeaseFlagged(t *testing.T) {
	now := time.Now()
	ch := Assess([]corev1.Node{hbReadyNode("w1")}, Heartbeat{Leases: nil, Now: now, Threshold: 40 * time.Second}, nil)
	if ch.NodesStaleHeartbeat != 1 || len(ch.NodeIssues) != 1 || !strings.Contains(ch.NodeIssues[0], "no kubelet lease") {
		t.Errorf("missing lease on a Ready node must flag: %+v", ch)
	}
}

func TestAssess_NotReadyNodeNoDuplicateHeartbeat(t *testing.T) {
	now := time.Now()
	notReady := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "w1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady"}}},
	}
	hb := Heartbeat{Leases: []coordinationv1.Lease{hbLease("w1", now.Add(-90 * time.Second))}, Now: now, Threshold: 40 * time.Second}
	ch := Assess([]corev1.Node{notReady}, hb, nil)
	if ch.NodesStaleHeartbeat != 0 {
		t.Errorf("NotReady node must not add a heartbeat issue: %+v", ch)
	}
	for _, iss := range ch.NodeIssues {
		if strings.Contains(iss, "heartbeating") {
			t.Errorf("no heartbeat issue expected on a NotReady node: %q", iss)
		}
	}
}

func TestAssess_HeartbeatThresholdDisabled(t *testing.T) {
	now := time.Now()
	ch := Assess([]corev1.Node{hbReadyNode("w1")}, Heartbeat{Leases: nil, Now: now, Threshold: 0}, nil)
	if ch.NodesStaleHeartbeat != 0 || ch.Verdict != "Healthy" {
		t.Errorf("threshold 0 disables the check: %+v", ch)
	}
}
