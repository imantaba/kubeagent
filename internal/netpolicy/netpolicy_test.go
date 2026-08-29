package netpolicy

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func np(ns, name string, sel map[string]string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: sel}},
	}
}

// degraded builds a flagged (ready<desired), finding-less Deployment with one pod.
func degraded(ns, name, podName string) inventory.Workload {
	return inventory.Workload{
		Namespace: ns, Name: name, Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
		Pods: []inventory.PodRow{{Name: podName, Phase: "Running", Ready: "0/1"}},
	}
}

func TestAnnotate_FlaggedNoFindingsSelected(t *testing.T) {
	ws := []inventory.Workload{degraded("default", "api", "api-1")}
	podLabels := map[string]map[string]string{"default/api-1": {"app": "api"}}
	pols := []networkingv1.NetworkPolicy{
		np("default", "deny-all", nil),                              // empty selector → matches all
		np("default", "allow-api", map[string]string{"app": "api"}), // matches app=api
	}
	Annotate(ws, podLabels, pols)
	got := ws[0].NetworkPolicies
	if len(got) != 2 || got[0] != "allow-api" || got[1] != "deny-all" {
		t.Fatalf("got %+v, want [allow-api deny-all] (sorted)", got)
	}
}

func TestAnnotate_SkipsWhenHasFinding(t *testing.T) {
	w := degraded("default", "api", "api-1")
	w.Findings = []diagnose.Finding{{Pod: "default/api-1", Issue: "OOMKilled"}}
	ws := []inventory.Workload{w}
	Annotate(ws, map[string]map[string]string{"default/api-1": {"app": "api"}}, []networkingv1.NetworkPolicy{np("default", "deny-all", nil)})
	if ws[0].NetworkPolicies != nil {
		t.Errorf("a workload with a detector finding must not get an NP hint, got %+v", ws[0].NetworkPolicies)
	}
}

func TestAnnotate_SkipsHealthy(t *testing.T) {
	healthy := inventory.Workload{
		Namespace: "default", Name: "web", Kind: "Deployment", Desired: 1, Ready: 1, Status: "Running",
		Pods: []inventory.PodRow{{Name: "web-1"}},
	}
	ws := []inventory.Workload{healthy}
	Annotate(ws, map[string]map[string]string{"default/web-1": {"app": "web"}}, []networkingv1.NetworkPolicy{np("default", "deny-all", nil)})
	if ws[0].NetworkPolicies != nil {
		t.Errorf("a healthy workload must not get an NP hint, got %+v", ws[0].NetworkPolicies)
	}
}

func TestAnnotate_LabelMismatchNoHint(t *testing.T) {
	ws := []inventory.Workload{degraded("default", "api", "api-1")}
	podLabels := map[string]map[string]string{"default/api-1": {"app": "api"}}
	pols := []networkingv1.NetworkPolicy{np("default", "for-web", map[string]string{"app": "web"})}
	Annotate(ws, podLabels, pols)
	if ws[0].NetworkPolicies != nil {
		t.Errorf("a non-matching policy must not be listed, got %+v", ws[0].NetworkPolicies)
	}
}

func TestAnnotate_CrossNamespaceNoHint(t *testing.T) {
	ws := []inventory.Workload{degraded("default", "api", "api-1")}
	podLabels := map[string]map[string]string{"default/api-1": {"app": "api"}}
	pols := []networkingv1.NetworkPolicy{np("other", "deny-all", nil)} // different namespace
	Annotate(ws, podLabels, pols)
	if ws[0].NetworkPolicies != nil {
		t.Errorf("a policy in another namespace must not match, got %+v", ws[0].NetworkPolicies)
	}
}

// TestAnnotate_ProbeFailureStillGetsHint pins the narrowing of the guard.
//
// The original guard attached the hint only to a flagged workload with ZERO
// detector findings. That suppressed it in the single commonest way a
// NetworkPolicy actually manifests: the policy blocks traffic to the pod, the
// readiness probe times out, a ProbeFailure finding is recorded, and the hint
// that would have explained the ProbeFailure is dropped because the
// ProbeFailure exists. The conjunction "probe failing AND a policy selects
// these pods" is specific and worth saying; that is why it is exempted while
// every other finding kind still suppresses.
func TestAnnotate_ProbeFailureStillGetsHint(t *testing.T) {
	w := degraded("default", "api", "api-1")
	w.Findings = []diagnose.Finding{{Pod: "default/api-1", Issue: "ProbeFailure"}}
	ws := []inventory.Workload{w}
	Annotate(ws, map[string]map[string]string{"default/api-1": {"app": "api"}},
		[]networkingv1.NetworkPolicy{np("default", "deny-all", nil)})
	if got := ws[0].NetworkPolicies; len(got) != 1 || got[0] != "deny-all" {
		t.Errorf("a probe-failing workload selected by a policy must get the hint, got %+v", got)
	}
}

// TestAnnotate_MixedFindingsStillSuppressed keeps the narrowing narrow: the
// exemption is for workloads whose findings are ALL probe failures. A workload
// that is also OOMKilled has a cause that explains it, so the policy hint is
// still noise there.
func TestAnnotate_MixedFindingsStillSuppressed(t *testing.T) {
	w := degraded("default", "api", "api-1")
	w.Findings = []diagnose.Finding{
		{Pod: "default/api-1", Issue: "ProbeFailure"},
		{Pod: "default/api-1", Issue: "OOMKilled"},
	}
	ws := []inventory.Workload{w}
	Annotate(ws, map[string]map[string]string{"default/api-1": {"app": "api"}},
		[]networkingv1.NetworkPolicy{np("default", "deny-all", nil)})
	if ws[0].NetworkPolicies != nil {
		t.Errorf("a workload with a non-probe finding must stay suppressed, got %+v", ws[0].NetworkPolicies)
	}
}
