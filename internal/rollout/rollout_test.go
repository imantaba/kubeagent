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
	return rsContainers(ns, name, owner, revision, age, corev1.Container{Name: "c", Image: image})
}

// rsContainers builds a ReplicaSet owned by `owner` at `revision`, created `age`
// before now, with the given named containers.
func rsContainers(ns, name, owner, revision string, age time.Duration, containers ...corev1.Container) appsv1.ReplicaSet {
	r := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		Annotations:       map[string]string{"deployment.kubernetes.io/revision": revision},
		OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: owner}},
		CreationTimestamp: metav1.Time{Time: now.Add(-age)},
	}}
	r.Spec.Template.Spec.Containers = containers
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

func TestAnnotate_FirstRevisionNotAnnotated(t *testing.T) {
	wls := []inventory.Workload{flaggedDep("shop", "web")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1", "nginx:bad", 1*24*time.Hour)}
	Annotate(wls, rss, now)
	if got := wls[0].Rollout; got != nil {
		t.Errorf("revision 1 is the Deployment's creation, not a change; want no annotation, got %+v", got)
	}
}

// The gate is the revision number, not the presence of a prior ReplicaSet: a
// Deployment at revision 2 whose revision-1 ReplicaSet was garbage-collected
// did change, and there is no delta to print only because the old spec is gone.
func TestAnnotate_LaterRevisionAnnotatedWithoutAPriorReplicaSet(t *testing.T) {
	wls := []inventory.Workload{flaggedDep("shop", "web")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-2", "web", "2", "nginx:bad", 1*24*time.Hour)}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil || got.Revision != "2" {
		t.Fatalf("expected rollout revision 2, got %+v", got)
	}
	if got.OldImage != "" || got.NewImage != "" {
		t.Errorf("no prior revision -> no delta, got %+v", got)
	}
}

// R235 fixture (1): a multi-container Deployment where the failing finding
// names the second container. The delta must be reported for that container,
// not the template's first, and Rollout.Container must carry its name.
func TestAnnotate_MultiContainerDeltaForFailingContainer(t *testing.T) {
	wls := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0,
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff", Image: "example.com/shop/sidecar:bad"}},
	}}
	rss := []appsv1.ReplicaSet{
		rsContainers("shop", "web-1", "web", "1", 30*24*time.Hour,
			corev1.Container{Name: "app", Image: "example.com/shop/app:1.0"},
			corev1.Container{Name: "sidecar", Image: "example.com/shop/sidecar:ok"}),
		rsContainers("shop", "web-2", "web", "2", 4*24*time.Hour,
			corev1.Container{Name: "app", Image: "example.com/shop/app:1.0"},
			corev1.Container{Name: "sidecar", Image: "example.com/shop/sidecar:bad"}),
	}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil {
		t.Fatal("expected a Rollout annotation")
	}
	if got.OldImage != "example.com/shop/sidecar:ok" || got.NewImage != "example.com/shop/sidecar:bad" {
		t.Errorf("expected the delta for the failing container (sidecar), got OldImage=%q NewImage=%q", got.OldImage, got.NewImage)
	}
	if got.Container != "sidecar" {
		t.Errorf("Container = %q, want %q (not the template's first container)", got.Container, "sidecar")
	}
}

// R235 fixture (2): the single-container case, the guard against a rewrite.
// Even though the finding's image matches the (only, first) container, the
// matched container equals the template's first, so Container must stay
// empty — byte-identical to today's behavior.
func TestAnnotate_SingleContainerFindingImageMatchesFirstContainerNoSuffix(t *testing.T) {
	wls := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0,
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff", Image: "example.com/shop/web:bad"}},
	}}
	rss := []appsv1.ReplicaSet{
		rs("shop", "web-1", "web", "1", "example.com/shop/web:ok", 30*24*time.Hour),
		rs("shop", "web-2", "web", "2", "example.com/shop/web:bad", 4*24*time.Hour),
	}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil {
		t.Fatal("expected a Rollout annotation")
	}
	if got.OldImage != "example.com/shop/web:ok" || got.NewImage != "example.com/shop/web:bad" {
		t.Errorf("unexpected delta: %+v", got)
	}
	if got.Container != "" {
		t.Errorf("Container = %q, want empty — the single-container case must render byte-identically to today", got.Container)
	}
}

// R235 fixture (3): no finding carries an image — Annotate must fall back to
// today's firstImage behavior exactly, and Container must stay empty.
func TestAnnotate_NoFindingImageFallsBackToFirstImage(t *testing.T) {
	wls := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0,
		Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff"}}, // no Image set
	}}
	rss := []appsv1.ReplicaSet{
		rsContainers("shop", "web-1", "web", "1", 30*24*time.Hour,
			corev1.Container{Name: "app", Image: "example.com/shop/app:1.0"},
			corev1.Container{Name: "sidecar", Image: "example.com/shop/sidecar:ok"}),
		rsContainers("shop", "web-2", "web", "2", 4*24*time.Hour,
			corev1.Container{Name: "app", Image: "example.com/shop/app:2.0"},
			corev1.Container{Name: "sidecar", Image: "example.com/shop/sidecar:bad"}),
	}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil {
		t.Fatal("expected a Rollout annotation")
	}
	if got.OldImage != "example.com/shop/app:1.0" || got.NewImage != "example.com/shop/app:2.0" {
		t.Errorf("no finding image -> want the fallback to the template's first container, got %+v", got)
	}
	if got.Container != "" {
		t.Errorf("Container = %q, want empty in the fallback case", got.Container)
	}
}

// R235 fixture (4): a finding whose image matches no container in the
// template — same fallback as fixture (3), and no panic.
func TestAnnotate_FindingImageMatchesNoContainerFallsBack(t *testing.T) {
	wls := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0,
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff", Image: "example.net/other/unrelated:v1"}},
	}}
	rss := []appsv1.ReplicaSet{
		rs("shop", "web-1", "web", "1", "example.com/shop/web:ok", 30*24*time.Hour),
		rs("shop", "web-2", "web", "2", "example.com/shop/web:bad", 4*24*time.Hour),
	}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil {
		t.Fatal("expected a Rollout annotation")
	}
	if got.OldImage != "example.com/shop/web:ok" || got.NewImage != "example.com/shop/web:bad" {
		t.Errorf("finding image matches no container -> want the fallback to firstImage, got %+v", got)
	}
	if got.Container != "" {
		t.Errorf("Container = %q, want empty in the fallback case", got.Container)
	}
}

// R235 fixture (5), added in this round: the finding's image matches a
// container that exists in cur but has no same-named counterpart in prev —
// added by this revision, not changed by it. changedContainer must report no
// match so Annotate falls back to firstImage's unqualified delta (app's real
// v1 -> v2 change) instead of dropping the delta line entirely.
func TestAnnotate_FindingContainerAddedThisRevisionFallsBackToFirstImage(t *testing.T) {
	wls := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0,
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff", Image: "example.com/shop/sidecar:v1"}},
	}}
	rss := []appsv1.ReplicaSet{
		rsContainers("shop", "web-1", "web", "1", 30*24*time.Hour,
			corev1.Container{Name: "app", Image: "example.com/shop/app:v1"}),
		rsContainers("shop", "web-2", "web", "2", 4*24*time.Hour,
			corev1.Container{Name: "app", Image: "example.com/shop/app:v2"},
			corev1.Container{Name: "sidecar", Image: "example.com/shop/sidecar:v1"}),
	}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil {
		t.Fatal("expected a Rollout annotation")
	}
	if got.OldImage != "example.com/shop/app:v1" || got.NewImage != "example.com/shop/app:v2" {
		t.Errorf("finding's container has no counterpart in prev -> want the fallback to app's real delta, got %+v", got)
	}
	if got.Container != "" {
		t.Errorf("Container = %q, want empty — the fallback names no container", got.Container)
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
