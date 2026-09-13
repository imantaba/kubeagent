package investigate

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/hypothesis"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/pvchealth"
	"github.com/imantaba/kubeagent/internal/rootcause"
)

// annotatedWorkload is one flagged Deployment with one pod on worker-1,
// ready for the rootcause annotators to attach candidates to.
func annotatedWorkload() []inventory.Workload {
	return []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 1, Status: "Degraded",
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "CrashLoopBackOff", Container: "app"}},
		Pods:     []inventory.PodRow{{Name: "web-abc", Node: "worker-1"}},
	}}
}

// TestRootcauseNodeCauseFormatReachesTheRules pins the node cause text the
// annotator writes against what the node rule reads: the reason in
// parentheses picks the heartbeat table, and the object names the node.
func TestRootcauseNodeCauseFormatReachesTheRules(t *testing.T) {
	ws := annotatedWorkload()
	rootcause.Annotate(ws, []clusterhealth.DownNode{{Name: "worker-1", Reason: "kubelet not heartbeating"}})
	if len(ws[0].RootCauseTrace) != 1 || ws[0].RootCauseTrace[0].Cause != "node worker-1 (kubelet not heartbeating)" {
		t.Fatalf("annotator wrote %+v", ws[0].RootCauseTrace)
	}
	reads := hypothesis.Reads{
		Nodes: map[string]*corev1.Node{"worker-1": {ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}},
		PVCs: map[string]*corev1.PersistentVolumeClaim{}, Events: map[string][]corev1.Event{}, Failed: map[string]string{},
	}
	r := hypothesis.Decide(ws[0], reads)
	// A heartbeat reason with Ready True is unverified, not refuted: the
	// rule read the reason out of the parentheses.
	if !r.Decided || r.Outcome != hypothesis.Unverified || r.Evidence != "Ready condition is True, but the kubelet lease was not re-read" {
		t.Errorf("Decide = %+v", r)
	}
}

// TestRootcausePVCCauseFormatReachesTheRules pins the PVC cause text the
// annotator writes against what the grouping reads: the reason in
// parentheses selects the storage-class group.
func TestRootcausePVCCauseFormatReachesTheRules(t *testing.T) {
	ws := annotatedWorkload()
	rootcause.AnnotatePVC(ws, map[string][]string{"shop/web-abc": {"web-data"}},
		[]pvchealth.Issue{{Namespace: "shop", Name: "web-data", Phase: "Pending", Reason: "ProvisionerNotResponding"}})
	if len(ws[0].RootCauseTrace) != 1 || ws[0].RootCauseTrace[0].Cause != "PVC web-data (ProvisionerNotResponding)" {
		t.Fatalf("annotator wrote %+v", ws[0].RootCauseTrace)
	}
	class := "example-csi"
	reads := hypothesis.Reads{
		Nodes: map[string]*corev1.Node{},
		PVCs: map[string]*corev1.PersistentVolumeClaim{"shop/web-data": {ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-data"},
			Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}}},
		Events: map[string][]corev1.Event{}, Failed: map[string]string{},
	}
	r := hypothesis.Decide(ws[0], reads)
	if !r.Decided || r.Outcome != hypothesis.Confirmed || r.Cause != "PVC web-data (ProvisionerNotResponding)" {
		t.Fatalf("Decide = %+v", r)
	}
	if r.GroupKey != "storageclass/example-csi/ProvisionerNotResponding" || r.GroupText != "storage class example-csi (ProvisionerNotResponding)" {
		t.Errorf("the reason must be parsed out of the annotator's parentheses: key %q text %q", r.GroupKey, r.GroupText)
	}
}
