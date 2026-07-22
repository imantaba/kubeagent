package rollouthealth

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// cond builds a Deployment status condition.
func cond(t appsv1.DeploymentConditionType, s corev1.ConditionStatus, reason, msg string) appsv1.DeploymentCondition {
	return appsv1.DeploymentCondition{Type: t, Status: s, Reason: reason, Message: msg}
}

// deploy builds a Deployment with the given status conditions.
func deploy(ns, name string, conds ...appsv1.DeploymentCondition) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     appsv1.DeploymentStatus{Conditions: conds},
	}
}

// degraded builds a flagged (Ready < Desired) Deployment workload with no findings.
func degraded(ns, name string) inventory.Workload {
	return inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment", Desired: 3, Ready: 2, Status: "Degraded"}
}

const deadlineMsg = `ReplicaSet "api-7f9c" has timed out progressing.`
const replicaFailMsg = `pods "api-7f9c-" is forbidden: exceeded quota: compute`

func TestAnnotate_ProgressDeadlineExceeded(t *testing.T) {
	ws := []inventory.Workload{degraded("shop", "api")}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 1 {
		t.Fatalf("want one finding, got %+v", ws[0].Findings)
	}
	f := ws[0].Findings[0]
	if f.Issue != "RolloutStuck" {
		t.Errorf("Issue = %q, want RolloutStuck", f.Issue)
	}
	if f.Reason != "the Deployment's rollout cannot complete — the new pods are not becoming available" {
		t.Errorf("Reason = %q", f.Reason)
	}
	if !strings.HasPrefix(f.Evidence, "Progressing (ProgressDeadlineExceeded): ") || !strings.Contains(f.Evidence, deadlineMsg) {
		t.Errorf("Evidence = %q, want the Progressing-prefixed message", f.Evidence)
	}
	if f.Pod != "shop/api" {
		t.Errorf("Pod = %q, want shop/api", f.Pod)
	}
}

func TestAnnotate_ReplicaFailure(t *testing.T) {
	ws := []inventory.Workload{degraded("shop", "api")}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "FailedCreate", replicaFailMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Issue != "RolloutStuck" {
		t.Fatalf("want one RolloutStuck finding, got %+v", ws[0].Findings)
	}
	if ev := ws[0].Findings[0].Evidence; !strings.HasPrefix(ev, "ReplicaFailure: ") || !strings.Contains(ev, replicaFailMsg) {
		t.Errorf("Evidence = %q, want the ReplicaFailure-prefixed message", ev)
	}
}

func TestAnnotate_ReplicaFailureWinsOverProgressing(t *testing.T) {
	ws := []inventory.Workload{degraded("shop", "api")}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg),
		cond(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "FailedCreate", replicaFailMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 1 {
		t.Fatalf("want one finding, got %+v", ws[0].Findings)
	}
	if ev := ws[0].Findings[0].Evidence; !strings.HasPrefix(ev, "ReplicaFailure: ") {
		t.Errorf("Evidence = %q, want ReplicaFailure to win precedence", ev)
	}
}

// TestAnnotate_NotFlaggedCases covers Deployments that ARE flagged (degraded)
// but whose status condition is not a stuck-rollout signal — so no finding is
// added. It pins that the Deployment condition, not the workload status, is what
// gates the finding.
func TestAnnotate_NotFlaggedCases(t *testing.T) {
	cases := []struct {
		name string
		w    inventory.Workload
		d    appsv1.Deployment
	}{
		{"paused", degraded("shop", "api"),
			deploy("shop", "api", cond(appsv1.DeploymentProgressing, corev1.ConditionUnknown, "DeploymentPaused", "Deployment is paused"))},
		{"progressing within deadline", degraded("shop", "api"),
			deploy("shop", "api", cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated", "ReplicaSet is progressing"))},
		{"healthy available", degraded("shop", "api"),
			deploy("shop", "api", cond(appsv1.DeploymentAvailable, corev1.ConditionTrue, "MinimumReplicasAvailable", "ok"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := []inventory.Workload{tc.w}
			Annotate(ws, []appsv1.Deployment{tc.d})
			if len(ws[0].Findings) != 0 {
				t.Errorf("%s: want no finding, got %+v", tc.name, ws[0].Findings)
			}
		})
	}
}

func TestAnnotate_SkipsWorkloadWithExistingFinding(t *testing.T) {
	w := degraded("shop", "api")
	w.Findings = []diagnose.Finding{{Pod: "shop/api", Issue: "ImagePullBackOff"}}
	ws := []inventory.Workload{w}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Issue != "ImagePullBackOff" {
		t.Errorf("want the existing finding untouched, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_SkipsNonDeploymentAndUnflagged(t *testing.T) {
	// A flagged StatefulSet named like a stuck Deployment: the Kind gate must skip it
	// even though a same-named Deployment with the stuck condition is present.
	sts := inventory.Workload{Namespace: "db", Name: "pg", Kind: "StatefulSet", Desired: 3, Ready: 0, Status: "Degraded"}
	// An unflagged Deployment (Ready == Desired) must be skipped.
	healthy := inventory.Workload{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 3, Ready: 3, Status: "Running"}
	ws := []inventory.Workload{sts, healthy}
	ds := []appsv1.Deployment{
		deploy("db", "pg", cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg)),
		deploy("shop", "web", cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg)),
	}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 0 {
		t.Errorf("StatefulSet: want no finding, got %+v", ws[0].Findings)
	}
	if len(ws[1].Findings) != 0 {
		t.Errorf("unflagged Deployment: want no finding, got %+v", ws[1].Findings)
	}
}

func TestAnnotate_NoMatchingDeployment(t *testing.T) {
	ws := []inventory.Workload{degraded("shop", "api")}
	Annotate(ws, nil) // no Deployments at all — must not panic, no finding
	if len(ws[0].Findings) != 0 {
		t.Errorf("want no finding when the Deployment is absent, got %+v", ws[0].Findings)
	}
}
