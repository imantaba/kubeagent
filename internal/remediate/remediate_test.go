package remediate

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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

func TestPlan_ErrImagePullAlsoTriggers(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ErrImagePull")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1"), rs("shop", "web-2", "web", "2")}
	got := Plan(wls, rss)
	if len(got) != 1 || got[0].Kind != "RolloutUndo" {
		t.Fatalf("ErrImagePull should also propose RolloutUndo, got %+v", got)
	}
}

func depObj(ns, name, image, curRev string) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		Annotations: map[string]string{"deployment.kubernetes.io/revision": curRev},
	}}
	d.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
	}
	return d
}

func rsWithImage(ns, name, owner, rev, image string) appsv1.ReplicaSet {
	r := rs(ns, name, owner, rev)
	r.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": owner, "pod-template-hash": "h" + rev}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
	}
	return r
}

func TestApply_RollsBackToPreviousTemplate(t *testing.T) {
	// current rev 2 = broken image; rev 1 = good image
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web"})
	if !res.Applied || res.Err != nil {
		t.Fatalf("expected applied, got %+v", res)
	}
	out, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if got := out.Spec.Template.Spec.Containers[0].Image; got != "nginx:1.27" {
		t.Errorf("image not rolled back: got %q", got)
	}
	if _, ok := out.Spec.Template.Labels["pod-template-hash"]; ok {
		t.Errorf("pod-template-hash must be cleared on the Deployment template")
	}
}

func TestApply_NoTargetWhenOnlyCurrentRevision(t *testing.T) {
	cur := depObj("shop", "web", "nginx:does-not-exist", "1")
	only := rsWithImage("shop", "web-1", "web", "1", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &only)
	res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web"})
	if res.Applied || res.Err != nil {
		t.Fatalf("expected a no-write skip, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatalf("no-target path must not write; saw update verb")
		}
	}
}

func TestApply_RejectsUnknownKindAndProtectedNs(t *testing.T) {
	cli := fake.NewSimpleClientset()
	if res := Apply(context.Background(), cli, Action{Kind: "Nuke", Namespace: "shop", Name: "web"}); res.Err == nil {
		t.Error("unknown kind must error")
	}
	if res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "kube-system", Name: "x"}); res.Err == nil {
		t.Error("protected namespace must error")
	}
}
