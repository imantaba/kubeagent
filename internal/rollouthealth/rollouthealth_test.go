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

// A crash-looping container is only in Waiting between restart attempts; the
// rest of the cycle it is Terminated or Running and no pod-level detector
// matches. A gate reading that one momentary sample attached RolloutStuck in 32
// of 40 consecutive scans of the same unchanged Deployment, and CrashLoopBackOff
// in the other 8 — the reported issue kind depended on which millisecond the
// scan landed in. The restart count is durable across the whole cycle, so it
// answers the question the instant state cannot.
func TestAnnotate_SkipsWorkloadWithARestartingPod(t *testing.T) {
	w := degraded("shop", "api")
	w.Pods = []inventory.PodRow{{Name: "api-7f9c-aaa", Phase: "Running", Restarts: diagnose.RestartThreshold}}
	ws := []inventory.Workload{w}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 0 {
		t.Errorf("a workload whose pod is restarting repeatedly must not also be told its rollout is stuck, got %+v", ws[0].Findings)
	}
}

// The threshold is a floor, not a tripwire: a pod that has restarted once or
// twice is not crash-looping, and suppressing RolloutStuck for it would trade
// the instability for silence on a genuinely wedged rollout.
func TestAnnotate_FiresBelowTheRestartThreshold(t *testing.T) {
	w := degraded("shop", "api")
	w.Pods = []inventory.PodRow{{Name: "api-7f9c-aaa", Phase: "Running", Restarts: diagnose.RestartThreshold - 1}}
	ws := []inventory.Workload{w}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Issue != "RolloutStuck" {
		t.Errorf("want RolloutStuck below the restart threshold, got %+v", ws[0].Findings)
	}
}

// One restarting pod is enough. A Deployment whose other replicas are healthy is
// still a Deployment with a crash-looping pod, and the pod-level finding is the
// one that names the cause.
func TestAnnotate_OneRestartingPodAmongHealthyOnesSuppresses(t *testing.T) {
	w := degraded("shop", "api")
	w.Pods = []inventory.PodRow{
		{Name: "api-7f9c-aaa", Phase: "Running", Restarts: 0},
		{Name: "api-7f9c-bbb", Phase: "Running", Restarts: diagnose.RestartThreshold + 4},
	}
	ws := []inventory.Workload{w}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 0 {
		t.Errorf("want no finding when any pod is restarting repeatedly, got %+v", ws[0].Findings)
	}
}

// The regression fixture from the campaign: the pod is Terminated — the phase in
// which no pod-level detector matches — and carries a restart count above the
// threshold. This is the exact sample that produced 32 of the 40 unstable runs.
func TestAnnotate_TerminatedPodWithRestartsProducesNoRolloutStuck(t *testing.T) {
	w := degraded("shop", "api")
	w.Pods = []inventory.PodRow{{Name: "api-7f9c-aaa", Phase: "Terminated", Restarts: diagnose.RestartThreshold + 1}}
	ws := []inventory.Workload{w}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 0 {
		t.Errorf("want no RolloutStuck for a Terminated pod with a restart history, got %+v", ws[0].Findings)
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
