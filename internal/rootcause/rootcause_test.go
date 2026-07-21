package rootcause

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/pvchealth"
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

// pullWL builds a flagged Deployment with an image-pull finding on the given image.
func pullWL(name, image, issue string) inventory.Workload {
	return inventory.Workload{Namespace: "shop", Name: name, Kind: "Deployment",
		Ready: 0, Desired: 1, Status: "Degraded", Image: image,
		Findings: []diagnose.Finding{{Pod: "shop/" + name, Issue: issue,
			Reason: "Bad image reference or registry authentication"}}}
}

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/org/app:v1":      "ghcr.io",
		"nginx:1.27":              "docker.io",
		"library/nginx":           "docker.io",
		"registry.local:5000/app": "registry.local:5000",
		"localhost/app":           "localhost",
		"nginx":                   "docker.io",
		"nginx@sha256:abc123":     "docker.io", // bare name with digest, no registry prefix
	}
	for image, want := range cases {
		if got := registryHost(image); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestAnnotateRegistry_GroupOfTwoAttributed(t *testing.T) {
	ws := []inventory.Workload{
		pullWL("frontend", "ghcr.io/shop/frontend:2.4", "ImagePullBackOff"),
		pullWL("search", "ghcr.io/shop/search:1.9", "ErrImagePull"),
	}
	AnnotateRegistry(ws)
	want := "registry ghcr.io (2 workloads failing to pull)"
	if ws[0].RootCause != want || ws[1].RootCause != want {
		t.Errorf("both should be attributed %q, got %q / %q", want, ws[0].RootCause, ws[1].RootCause)
	}
}

func TestAnnotateRegistry_SingleFailerNotAttributed(t *testing.T) {
	ws := []inventory.Workload{pullWL("api", "ghcr.io/shop/api:1.0", "ImagePullBackOff")}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "" {
		t.Errorf("a lone pull-failer must not be blamed on the registry, got %q", ws[0].RootCause)
	}
}

func TestAnnotateRegistry_NodeAttributionWinsAndShrinksGroup(t *testing.T) {
	nodeOwned := pullWL("api", "ghcr.io/shop/api:1.0", "ImagePullBackOff")
	nodeOwned.RootCause = "node worker-2 (NotReady)"
	ws := []inventory.Workload{nodeOwned, pullWL("web", "ghcr.io/shop/web:3.1", "ImagePullBackOff")}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Errorf("node attribution must be preserved, got %q", ws[0].RootCause)
	}
	if ws[1].RootCause != "" {
		t.Errorf("with the node-attributed workload excluded, the group is 1 -> no attribution, got %q", ws[1].RootCause)
	}
}

func TestAnnotateRegistry_NonPullFindingNotGrouped(t *testing.T) {
	crash := pullWL("worker", "ghcr.io/shop/worker:5", "CrashLoopBackOff")
	ws := []inventory.Workload{crash, pullWL("web", "ghcr.io/shop/web:3.1", "ImagePullBackOff")}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "" || ws[1].RootCause != "" {
		t.Errorf("a crash finding is not a pull failure; group is 1 -> none attributed, got %q / %q", ws[0].RootCause, ws[1].RootCause)
	}
}

func TestAnnotateRegistry_NotFlaggedSkipped(t *testing.T) {
	healthy := pullWL("web", "ghcr.io/shop/web:3.1", "ImagePullBackOff")
	healthy.Ready, healthy.Desired, healthy.Status = 1, 1, "Running"
	healthy.Findings = nil // healthy: no findings, not flagged
	ws := []inventory.Workload{healthy, pullWL("api", "ghcr.io/shop/api:1.0", "ImagePullBackOff")}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "" || ws[1].RootCause != "" {
		t.Errorf("unflagged workload must not count toward the group, got %q / %q", ws[0].RootCause, ws[1].RootCause)
	}
}

func TestAnnotateRegistry_TwoGroupsIndependent(t *testing.T) {
	ws := []inventory.Workload{
		pullWL("a", "ghcr.io/x/a:1", "ImagePullBackOff"),
		pullWL("b", "ghcr.io/x/b:1", "ImagePullBackOff"),
		pullWL("c", "quay.io/y/c:1", "ErrImagePull"),
		pullWL("d", "quay.io/y/d:1", "ImagePullBackOff"),
	}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "registry ghcr.io (2 workloads failing to pull)" ||
		ws[2].RootCause != "registry quay.io (2 workloads failing to pull)" {
		t.Errorf("each group gets its own host, got %q / %q", ws[0].RootCause, ws[2].RootCause)
	}
}

// pvcWL builds a flagged 0/1 Pending Deployment with one named pod (no findings —
// the realistic stuck-on-storage shape, flagged via Ready<Desired).
func pvcWL(ns, name, podName string) inventory.Workload {
	return inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment",
		Ready: 0, Desired: 1, Status: "Pending",
		Pods: []inventory.PodRow{{Name: podName, Phase: "Pending"}}}
}

func TestAnnotatePVC_MountedIssueAttributed(t *testing.T) {
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Phase: "Pending", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "PVC reports-data (ProvisioningFailed)" {
		t.Errorf("RootCause = %q, want PVC reports-data (ProvisioningFailed)", ws[0].RootCause)
	}
}

func TestAnnotatePVC_FailedBindingReason(t *testing.T) {
	ws := []inventory.Workload{pvcWL("db", "pg", "pg-0")}
	podPVCs := map[string][]string{"db/pg-0": {"pg-data"}}
	issues := []pvchealth.Issue{{Namespace: "db", Name: "pg-data", Phase: "Pending", Reason: "FailedBinding"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "PVC pg-data (FailedBinding)" {
		t.Errorf("RootCause = %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_HealthyMountsNotAttributed(t *testing.T) {
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"other-healthy-pvc"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "" {
		t.Errorf("workload mounting only healthy PVCs must not be attributed, got %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_ExistingRootCausePreserved(t *testing.T) {
	w := pvcWL("shop", "reports", "reports-1")
	w.RootCause = "node worker-2 (NotReady)"
	ws := []inventory.Workload{w}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Errorf("node attribution must win, got %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_NotFlaggedSkipped(t *testing.T) {
	w := pvcWL("shop", "reports", "reports-1")
	w.Ready, w.Desired, w.Status = 1, 1, "Running"
	ws := []inventory.Workload{w}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "" {
		t.Errorf("a healthy workload must be skipped, got %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_NamespaceIsolation(t *testing.T) {
	// Same PVC name broken in ANOTHER namespace must not match.
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "other", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "" {
		t.Errorf("an issue in a different namespace must not match, got %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_DeterministicSortedPick(t *testing.T) {
	// Pod mounts two broken PVCs; the sorted-first issue key wins.
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"zeta-data", "alpha-data"}}
	issues := []pvchealth.Issue{
		{Namespace: "shop", Name: "zeta-data", Reason: "ProvisioningFailed"},
		{Namespace: "shop", Name: "alpha-data", Reason: "FailedBinding"},
	}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "PVC alpha-data (FailedBinding)" {
		t.Errorf("sorted-first issue key must win, got %q", ws[0].RootCause)
	}
}

func TestAnnotatePVC_BeatsRegistryWhenRunFirst(t *testing.T) {
	// A workload that both mounts a broken PVC AND fails pulls alongside another
	// workload: running AnnotatePVC before AnnotateRegistry (the scan order) must
	// give it the PVC cause and shrink the registry group below threshold.
	stuck := pvcWL("shop", "reports", "reports-1")
	stuck.Image = "ghcr.io/shop/reports:1.0"
	stuck.Findings = []diagnose.Finding{{Pod: "shop/reports", Issue: "ImagePullBackOff", Reason: "Bad image reference or registry authentication"}}
	other := pullWL("web", "ghcr.io/shop/web:3.1", "ImagePullBackOff")
	ws := []inventory.Workload{stuck, other}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	AnnotateRegistry(ws)
	if ws[0].RootCause != "PVC reports-data (ProvisioningFailed)" {
		t.Errorf("PVC cause must win when run first, got %q", ws[0].RootCause)
	}
	if ws[1].RootCause != "" {
		t.Errorf("with the PVC-attributed workload excluded, the registry group is 1 -> no attribution, got %q", ws[1].RootCause)
	}
}

func TestAnnotatePVC_EmptyInputsNoop(t *testing.T) {
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	AnnotatePVC(ws, nil, nil)
	if ws[0].RootCause != "" {
		t.Errorf("empty inputs => no-op, got %q", ws[0].RootCause)
	}
}
