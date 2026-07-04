package rollout

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

var now = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

// flaggedDep builds a flagged (Ready<Desired) Deployment workload.
func flaggedDep(ns, name string) inventory.Workload {
	return inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment", Desired: 1, Ready: 0,
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff"}}}
}

// rs builds a ReplicaSet owned by `owner` at `revision`, created `age` before
// now, whose single container runs `image`.
func rs(ns, name, owner, revision, image string, age time.Duration) appsv1.ReplicaSet {
	r := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		Annotations:       map[string]string{"deployment.kubernetes.io/revision": revision},
		OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: owner}},
		CreationTimestamp: metav1.Time{Time: now.Add(-age)},
	}}
	r.Spec.Template.Spec.Containers = []corev1.Container{{Name: "c", Image: image}}
	return r
}

func TestAnnotate_RecentRolloutWithImageChange(t *testing.T) {
	wls := []inventory.Workload{flaggedDep("shop", "web")}
	rss := []appsv1.ReplicaSet{
		rs("shop", "web-1", "web", "1", "nginx:1.27", 30*24*time.Hour),
		rs("shop", "web-2", "web", "2", "nginx:bad", 4*24*time.Hour),
	}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil {
		t.Fatal("expected a Rollout annotation")
	}
	if got.Revision != "2" || got.OldImage != "nginx:1.27" || got.NewImage != "nginx:bad" {
		t.Errorf("unexpected rollout: %+v", got)
	}
	if got.Since == "" {
		t.Errorf("expected a Since age, got empty")
	}
}

func TestAnnotate_OldRolloutSkipped(t *testing.T) {
	wls := []inventory.Workload{flaggedDep("shop", "web")}
	rss := []appsv1.ReplicaSet{
		rs("shop", "web-1", "web", "1", "nginx:1.27", 60*24*time.Hour),
		rs("shop", "web-2", "web", "2", "nginx:bad", 30*24*time.Hour), // > 7d old
	}
	Annotate(wls, rss, now)
	if wls[0].Rollout != nil {
		t.Errorf("rollout older than the window should not annotate, got %+v", wls[0].Rollout)
	}
}

func TestAnnotate_ImageUnchanged(t *testing.T) {
	wls := []inventory.Workload{flaggedDep("shop", "web")}
	rss := []appsv1.ReplicaSet{
		rs("shop", "web-1", "web", "1", "nginx:1.27", 10*24*time.Hour),
		rs("shop", "web-2", "web", "2", "nginx:1.27", 2*24*time.Hour), // same image
	}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil || got.Revision != "2" {
		t.Fatalf("expected rollout revision 2, got %+v", got)
	}
	if got.OldImage != "" || got.NewImage != "" {
		t.Errorf("unchanged image should leave the delta empty, got %+v", got)
	}
}

func TestAnnotate_SingleRevisionNoDelta(t *testing.T) {
	wls := []inventory.Workload{flaggedDep("shop", "web")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1", "nginx:bad", 1*24*time.Hour)}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil || got.Revision != "1" {
		t.Fatalf("expected rollout revision 1, got %+v", got)
	}
	if got.OldImage != "" || got.NewImage != "" {
		t.Errorf("no prior revision -> no delta, got %+v", got)
	}
}

func TestAnnotate_SkipsNonDeploymentAndHealthy(t *testing.T) {
	ss := inventory.Workload{Namespace: "shop", Name: "ss", Kind: "StatefulSet", Desired: 1, Ready: 0}
	healthy := inventory.Workload{Namespace: "shop", Name: "ok", Kind: "Deployment", Desired: 1, Ready: 1}
	wls := []inventory.Workload{ss, healthy}
	rss := []appsv1.ReplicaSet{
		rs("shop", "ss-1", "ss", "1", "img", 1*24*time.Hour),
		rs("shop", "ok-1", "ok", "1", "img", 1*24*time.Hour),
	}
	Annotate(wls, rss, now)
	if wls[0].Rollout != nil || wls[1].Rollout != nil {
		t.Errorf("non-Deployment / healthy should not annotate: %+v %+v", wls[0].Rollout, wls[1].Rollout)
	}
}
