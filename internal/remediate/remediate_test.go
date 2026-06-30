package remediate

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func dep(ns, name string, issue string) inventory.Workload {
	w := inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment"}
	if issue != "" {
		w.Findings = []diagnose.Finding{{Pod: ns + "/" + name + "-x", Issue: issue}}
	}
	return w
}

func rs(ns, name, owner, revision string) appsv1.ReplicaSet {
	return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		Annotations:     map[string]string{"deployment.kubernetes.io/revision": revision},
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: owner}},
	}}
}

func TestPlan_ProposesRolloutUndo(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1"), rs("shop", "web-2", "web", "2")}
	got := Plan(wls, rss)
	if len(got) != 1 || got[0].Kind != "RolloutUndo" || got[0].Namespace != "shop" || got[0].Name != "web" {
		t.Fatalf("want one RolloutUndo for shop/web, got %+v", got)
	}
}

func TestPlan_SkipsWithoutImagePullFinding(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1"), rs("shop", "web-2", "web", "2")}
	if got := Plan(wls, rss); len(got) != 0 {
		t.Fatalf("no finding -> no action, got %+v", got)
	}
}

func TestPlan_SkipsWithoutPriorRevision(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1")} // only one revision
	if got := Plan(wls, rss); len(got) != 0 {
		t.Fatalf("no prior revision -> no action, got %+v", got)
	}
}

func TestPlan_SkipsProtectedNamespace(t *testing.T) {
	wls := []inventory.Workload{dep("kube-system", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{rs("kube-system", "web-1", "web", "1"), rs("kube-system", "web-2", "web", "2")}
	if got := Plan(wls, rss); len(got) != 0 {
		t.Fatalf("protected namespace -> no action, got %+v", got)
	}
}

func TestPlan_SkipsNonDeployment(t *testing.T) {
	w := inventory.Workload{Namespace: "shop", Name: "ss", Kind: "StatefulSet",
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff"}}}
	if got := Plan([]inventory.Workload{w}, nil); len(got) != 0 {
		t.Fatalf("non-Deployment -> no action, got %+v", got)
	}
}
