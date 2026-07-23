package remediate

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func dep(ns, name string, issue string) inventory.Workload {
	// Ready 0 < Desired 1 => degraded, so a RolloutUndo is warranted.
	w := inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment", Desired: 1, Ready: 0}
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
	rss := []appsv1.ReplicaSet{rsWithImage("shop", "web-1", "web", "1", "nginx:1.27"), rsWithImage("shop", "web-2", "web", "2", "nginx:broken")}
	got := Plan(wls, rss, nil)
	if len(got) != 1 || got[0].Kind != "RolloutUndo" || got[0].Namespace != "shop" || got[0].Name != "web" {
		t.Fatalf("want one RolloutUndo for shop/web, got %+v", got)
	}
}

func TestPlan_SkipsWithoutImagePullFinding(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1"), rs("shop", "web-2", "web", "2")}
	if got := Plan(wls, rss, nil); len(got) != 0 {
		t.Fatalf("no finding -> no action, got %+v", got)
	}
}

func TestPlan_SkipsWithoutPriorRevision(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1")} // only one revision
	if got := Plan(wls, rss, nil); len(got) != 0 {
		t.Fatalf("no prior revision -> no action, got %+v", got)
	}
}

func TestPlan_SkipsProtectedNamespace(t *testing.T) {
	wls := []inventory.Workload{dep("kube-system", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{rsWithImage("kube-system", "web-1", "web", "1", "nginx:1.27"), rsWithImage("kube-system", "web-2", "web", "2", "nginx:broken")}
	if got := Plan(wls, rss, nil); len(got) != 0 {
		t.Fatalf("protected namespace -> no action, got %+v", got)
	}
}

func TestPlan_SkipsNonDeployment(t *testing.T) {
	w := inventory.Workload{Namespace: "shop", Name: "ss", Kind: "StatefulSet",
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff"}}}
	if got := Plan([]inventory.Workload{w}, nil, nil); len(got) != 0 {
		t.Fatalf("non-Deployment -> no action, got %+v", got)
	}
}

func TestPlan_ErrImagePullAlsoTriggers(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ErrImagePull")}
	rss := []appsv1.ReplicaSet{rsWithImage("shop", "web-1", "web", "1", "nginx:1.27"), rsWithImage("shop", "web-2", "web", "2", "nginx:broken")}
	got := Plan(wls, rss, nil)
	if len(got) != 1 || got[0].Kind != "RolloutUndo" {
		t.Fatalf("ErrImagePull should also propose RolloutUndo, got %+v", got)
	}
}

func TestPlan_SkipsAvailableDeployment(t *testing.T) {
	w := dep("shop", "web", "ImagePullBackOff")
	w.Ready, w.Desired = 1, 1 // previous revision still serving — not degraded
	rss := []appsv1.ReplicaSet{rsWithImage("shop", "web-1", "web", "1", "nginx:1.27"), rsWithImage("shop", "web-2", "web", "2", "nginx:broken")}
	if got := Plan([]inventory.Workload{w}, rss, nil); len(got) != 0 {
		t.Fatalf("available deployment (Ready==Desired) -> no rollback, got %+v", got)
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
	res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web", CurrentRevision: 2, TargetRevision: 1})
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

func node(name string, unschedulable, noExecute bool) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	n.Spec.Unschedulable = unschedulable
	if unschedulable { // the auto NoSchedule cordon taint is always present; must be ignored
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule})
	}
	if noExecute {
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoExecute})
	}
	return n
}

func TestPlan_ProposesUncordon(t *testing.T) {
	got := Plan(nil, nil, []corev1.Node{node("worker-1", true, false)})
	if len(got) != 1 || got[0].Kind != "Uncordon" || got[0].Name != "worker-1" || got[0].Target != "node/worker-1" {
		t.Fatalf("want one Uncordon for worker-1 with Target node/worker-1, got %+v", got)
	}
}

func TestPlan_SkipsSchedulableNode(t *testing.T) {
	if got := Plan(nil, nil, []corev1.Node{node("worker-1", false, false)}); len(got) != 0 {
		t.Fatalf("schedulable node -> no action, got %+v", got)
	}
}

func TestPlan_SkipsNoExecuteTaintedNode(t *testing.T) {
	if got := Plan(nil, nil, []corev1.Node{node("worker-1", true, true)}); len(got) != 0 {
		t.Fatalf("NoExecute-tainted cordoned node -> no action, got %+v", got)
	}
}

func TestPlan_EmitsBothRolloutUndoAndUncordon(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{rsWithImage("shop", "web-1", "web", "1", "nginx:1.27"), rsWithImage("shop", "web-2", "web", "2", "nginx:broken")}
	got := Plan(wls, rss, []corev1.Node{node("worker-1", true, false)})
	kinds := map[string]bool{}
	for _, a := range got {
		kinds[a.Kind] = true
	}
	if len(got) != 2 || !kinds["RolloutUndo"] || !kinds["Uncordon"] {
		t.Fatalf("want both RolloutUndo and Uncordon, got %+v", got)
	}
}

func TestApply_Uncordon(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
	n.Spec.Unschedulable = true
	cli := fake.NewSimpleClientset(n)
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if !res.Applied || res.Err != nil {
		t.Fatalf("expected applied, got %+v", res)
	}
	out, _ := cli.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if out.Spec.Unschedulable {
		t.Errorf("node should be schedulable after uncordon")
	}
}

func TestApply_UncordonSkipsWhenAlreadySchedulable(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}} // already schedulable
	cli := fake.NewSimpleClientset(n)
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if res.Applied || res.Err != nil {
		t.Fatalf("expected no-write skip, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatalf("must not write when already schedulable; saw update")
		}
	}
}

func TestApply_UncordonSkipsWhenNoExecuteTainted(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
	n.Spec.Unschedulable = true
	n.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoExecute}}
	cli := fake.NewSimpleClientset(n)
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if res.Applied || res.Err != nil {
		t.Fatalf("expected no-write skip for NoExecute-tainted node, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatalf("must not write a NoExecute-tainted node; saw update")
		}
	}
}

func TestPlan_RolloutUndoCarriesDiffAndRevisions(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{
		rsWithImage("shop", "web-1", "web", "1", "nginx:1.27"),
		rsWithImage("shop", "web-2", "web", "2", "nginx:broken"),
	}
	got := Plan(wls, rss, nil)
	if len(got) != 1 {
		t.Fatalf("want one action, got %+v", got)
	}
	a := got[0]
	if a.CurrentRevision != 2 || a.TargetRevision != 1 {
		t.Errorf("revisions: got cur=%d target=%d, want 2/1", a.CurrentRevision, a.TargetRevision)
	}
	want := []Change{
		{Field: "revision", From: "2", To: "1"},
		{Field: "image (c)", From: "nginx:broken", To: "nginx:1.27"},
	}
	if len(a.Changes) != 2 || a.Changes[0] != want[0] || a.Changes[1] != want[1] {
		t.Errorf("changes = %+v, want %+v", a.Changes, want)
	}
}

func TestPlan_SkipsSameTemplatePriorRevision(t *testing.T) {
	// rev 1 and rev 2 have IDENTICAL templates (same image): nothing to roll back to.
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{
		rsWithImage("shop", "web-1", "web", "1", "nginx:same"),
		rsWithImage("shop", "web-2", "web", "2", "nginx:same"),
	}
	if got := Plan(wls, rss, nil); len(got) != 0 {
		t.Fatalf("same-template prior revision -> no action (plan/apply alignment), got %+v", got)
	}
}

func TestPlan_TargetSkipsSameTemplateToDeeperRevision(t *testing.T) {
	// rev 3 (current, broken), rev 2 same template as 3, rev 1 differs -> target must be 1.
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{
		rsWithImage("shop", "web-1", "web", "1", "nginx:1.27"),
		rsWithImage("shop", "web-2", "web", "2", "nginx:broken"),
		rsWithImage("shop", "web-3", "web", "3", "nginx:broken"),
	}
	got := Plan(wls, rss, nil)
	if len(got) != 1 || got[0].TargetRevision != 1 {
		t.Fatalf("want target revision 1 (rev 2 template equals current), got %+v", got)
	}
}

func TestPlan_ReportsOtherTemplateFieldChanges(t *testing.T) {
	// Same image, but the target adds a command -> no image line; one "other fields" line.
	cur := rsWithImage("shop", "web-2", "web", "2", "nginx:1.27")
	prior := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	prior.Spec.Template.Spec.Containers[0].Command = []string{"/bin/serve"}
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	got := Plan(wls, []appsv1.ReplicaSet{prior, cur}, nil)
	if len(got) != 1 {
		t.Fatalf("want one action, got %+v", got)
	}
	a := got[0]
	if len(a.Changes) != 2 || a.Changes[0].Field != "revision" {
		t.Fatalf("changes = %+v, want revision line + other-fields line", a.Changes)
	}
	other := a.Changes[1]
	if other.Field != "1 other template field changed" || other.From != "" || other.To != "" {
		t.Errorf("other-fields line = %+v; must carry a count only, never contents", other)
	}
	for _, c := range a.Changes {
		if strings.Contains(c.Field+c.From+c.To, "/bin/serve") {
			t.Errorf("template content leaked into the diff: %+v", c)
		}
	}
}

func TestPlan_UncordonCarriesStaticChange(t *testing.T) {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec: corev1.NodeSpec{Unschedulable: true}}
	got := Plan(nil, nil, []corev1.Node{n})
	if len(got) != 1 {
		t.Fatalf("want one uncordon, got %+v", got)
	}
	want := Change{Field: "spec.unschedulable", From: "true", To: "false"}
	if len(got[0].Changes) != 1 || got[0].Changes[0] != want {
		t.Errorf("changes = %+v, want [%+v]", got[0].Changes, want)
	}
}

func TestApply_RefusesOnCurrentRevisionDrift(t *testing.T) {
	// Previewed cur=2 target=1, but a new rollout happened: deployment is now rev 3.
	cur := depObj("shop", "web", "nginx:still-broken", "3")
	r1 := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	r2 := rsWithImage("shop", "web-2", "web", "2", "nginx:broken")
	r3 := rsWithImage("shop", "web-3", "web", "3", "nginx:still-broken")
	cli := fake.NewSimpleClientset(cur, &r1, &r2, &r3)
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		CurrentRevision: 2, TargetRevision: 1,
	})
	if res.Applied || res.Err != nil {
		t.Fatalf("drift must refuse without error, got %+v", res)
	}
	if !strings.Contains(res.Detail, "state changed since preview") {
		t.Errorf("detail = %q, want the drift refusal", res.Detail)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" || act.GetVerb() == "patch" || act.GetVerb() == "delete" {
			t.Fatalf("drift refusal must make no write, saw %s", act.GetVerb())
		}
	}
}

func TestApply_RefusesOnTargetRevisionDrift(t *testing.T) {
	// Previewed target=1, but rev 1's RS is gone; pickTarget would land on rev 2.
	cur := depObj("shop", "web", "nginx:broken3", "3")
	r2 := rsWithImage("shop", "web-2", "web", "2", "nginx:1.27")
	r3 := rsWithImage("shop", "web-3", "web", "3", "nginx:broken3")
	cli := fake.NewSimpleClientset(cur, &r2, &r3)
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		CurrentRevision: 3, TargetRevision: 1,
	})
	if res.Applied || !strings.Contains(res.Detail, "state changed since preview") {
		t.Fatalf("target drift must refuse, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("target drift refusal must make no write")
		}
	}
}

func TestApply_MatchingPreviewApplies(t *testing.T) {
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		CurrentRevision: 2, TargetRevision: 1,
	})
	if !res.Applied || res.Err != nil {
		t.Fatalf("matching preview must apply, got %+v", res)
	}
}

func TestPlan_MultiContainerOnlyChangedImageListed(t *testing.T) {
	// rev 2 (current, broken): web=nginx:broken, sidecar=sidecar:v1
	// rev 1 (target, good):   web=nginx:1.27,   sidecar=sidecar:v1  (sidecar unchanged)
	// Only the "web" image change should appear; "sidecar" must not.
	rev2 := rsWithImage("shop", "web-2", "web", "2", "nginx:broken")
	rev2.Spec.Template.Spec.Containers[0].Name = "web"
	rev2.Spec.Template.Spec.Containers = append(
		rev2.Spec.Template.Spec.Containers,
		corev1.Container{Name: "sidecar", Image: "sidecar:v1"},
	)

	rev1 := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	rev1.Spec.Template.Spec.Containers[0].Name = "web"
	rev1.Spec.Template.Spec.Containers = append(
		rev1.Spec.Template.Spec.Containers,
		corev1.Container{Name: "sidecar", Image: "sidecar:v1"},
	)

	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	got := Plan(wls, []appsv1.ReplicaSet{rev1, rev2}, nil)
	if len(got) != 1 {
		t.Fatalf("want one RolloutUndo, got %+v", got)
	}
	a := got[0]

	// Changes[0] must be the revision line.
	if len(a.Changes) == 0 || a.Changes[0].Field != "revision" {
		t.Fatalf("changes[0] must be the revision line, got %+v", a.Changes)
	}

	// Exactly one image change must be present.
	var imageChanges []Change
	for _, c := range a.Changes {
		if strings.HasPrefix(c.Field, "image (") {
			imageChanges = append(imageChanges, c)
		}
	}
	if len(imageChanges) != 1 {
		t.Fatalf("want exactly one image change, got %+v", imageChanges)
	}

	want := Change{Field: "image (web)", From: "nginx:broken", To: "nginx:1.27"}
	if imageChanges[0] != want {
		t.Errorf("image change = %+v, want %+v", imageChanges[0], want)
	}

	// "sidecar" must not appear anywhere in the diff.
	for _, c := range a.Changes {
		if strings.Contains(c.Field+c.From+c.To, "sidecar") {
			t.Errorf("sidecar must not appear in changes: %+v", c)
		}
	}
}
