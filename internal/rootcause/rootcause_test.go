package rootcause

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func wl(ns, name string, ready, desired int, nodes ...string) inventory.Workload {
	w := inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment", Ready: ready, Desired: desired, Status: "Degraded"}
	for i, n := range nodes {
		w.Pods = append(w.Pods, inventory.PodRow{Name: name + "-" + string(rune('a'+i)), Node: n})
	}
	return w
}

func TestAnnotate_AttributesPodOnNotReadyNode(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-2")}
	down := []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}}
	Annotate(ws, down)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Errorf("RootCause = %q, want node worker-2 (NotReady)", ws[0].RootCause)
	}
}

func TestAnnotate_StaleHeartbeatReason(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "web", 0, 1, "worker-1")}
	down := []clusterhealth.DownNode{{Name: "worker-1", Reason: "kubelet not heartbeating"}}
	Annotate(ws, down)
	if ws[0].RootCause != "node worker-1 (kubelet not heartbeating)" {
		t.Errorf("RootCause = %q", ws[0].RootCause)
	}
}

func TestAnnotate_HealthyNodeNoAttribution(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-9")} // not in down
	Annotate(ws, []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}})
	if ws[0].RootCause != "" {
		t.Errorf("workload on a healthy node must not be attributed, got %q", ws[0].RootCause)
	}
}

func TestAnnotate_NotFlaggedSkipped(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 2, 2, "worker-2")} // Ready==Desired, not flagged
	ws[0].Status = "Running"
	Annotate(ws, []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}})
	if ws[0].RootCause != "" {
		t.Errorf("a non-flagged workload must be skipped, got %q", ws[0].RootCause)
	}
}

func TestAnnotate_DeterministicPickSortedByNode(t *testing.T) {
	// Pods on two down nodes; the sorted-first node name wins.
	ws := []inventory.Workload{wl("shop", "api", 0, 3, "worker-5", "worker-2")}
	down := []clusterhealth.DownNode{{Name: "worker-5", Reason: "NotReady"}, {Name: "worker-2", Reason: "NotReady"}}
	Annotate(ws, down)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Errorf("want the sorted-first down node (worker-2), got %q", ws[0].RootCause)
	}
}

func TestAnnotate_EmptyDownNoop(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-2")}
	Annotate(ws, nil)
	if ws[0].RootCause != "" {
		t.Errorf("no down nodes => no attribution, got %q", ws[0].RootCause)
	}
}
