package rollouthealth

import (
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/createhealth"
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
	return degradedKind(ns, name, "Deployment")
}

// degradedKind builds a flagged workload of the given controller kind.
func degradedKind(ns, name, kind string) inventory.Workload {
	return inventory.Workload{Namespace: ns, Name: name, Kind: kind, Desired: 3, Ready: 2, Status: "Degraded"}
}

// rhNow is the injected clock every test in this file reads.
var rhNow = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// oldEnough and tooYoung sit either side of the grace period.
const (
	oldEnough = 20 * time.Minute
	tooYoung  = 60 * time.Second
)

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }

// ownedPod builds a pod controlled by the named workload, created ago before rhNow.
func ownedPod(ns, name, ownerKind, owner string, ready bool, ago time.Duration) corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			CreationTimestamp: metav1.NewTime(rhNow.Add(-ago)),
			OwnerReferences:   []metav1.OwnerReference{{Kind: ownerKind, Name: owner, Controller: boolPtr(true)}},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}},
	}
}

// statefulSet builds a StatefulSet wanting desired replicas, with the given
// status, created ago before rhNow.
func statefulSet(ns, name string, desired int32, s appsv1.StatefulSetStatus, ago time.Duration) appsv1.StatefulSet {
	return appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(rhNow.Add(-ago))},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(desired)},
		Status:     s,
	}
}

// daemonSet builds a DaemonSet with the given status, created ago before rhNow.
func daemonSet(ns, name string, s appsv1.DaemonSetStatus, ago time.Duration) appsv1.DaemonSet {
	return appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(rhNow.Add(-ago))},
		Status:     s,
	}
}

const deadlineMsg = `ReplicaSet "api-7f9c" has timed out progressing.`
const replicaFailMsg = `pods "api-7f9c-" is forbidden: exceeded quota: compute`

func TestAnnotate_ProgressDeadlineExceeded(t *testing.T) {
	ws := []inventory.Workload{degraded("shop", "api")}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg))}

	Annotate(ws, ds, nil, nil, nil, rhNow)

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

	Annotate(ws, ds, nil, nil, nil, rhNow)

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

	Annotate(ws, ds, nil, nil, nil, rhNow)

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
			Annotate(ws, []appsv1.Deployment{tc.d}, nil, nil, nil, rhNow)
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

	Annotate(ws, ds, nil, nil, nil, rhNow)

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

	Annotate(ws, ds, nil, nil, nil, rhNow)

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

	Annotate(ws, ds, nil, nil, nil, rhNow)

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

	Annotate(ws, ds, nil, nil, nil, rhNow)

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

	Annotate(ws, ds, nil, nil, nil, rhNow)

	if len(ws[0].Findings) != 0 {
		t.Errorf("want no RolloutStuck for a Terminated pod with a restart history, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_SkipsNonDeploymentAndUnflagged(t *testing.T) {
	// A flagged StatefulSet named like a stuck Deployment. The workload's kind
	// selects which object list is consulted, so a StatefulSet workload is never
	// answered from a Deployment's conditions even when the names collide.
	sts := inventory.Workload{Namespace: "db", Name: "pg", Kind: "StatefulSet", Desired: 3, Ready: 0, Status: "Degraded"}
	// An unflagged Deployment (Ready == Desired) must be skipped.
	healthy := inventory.Workload{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 3, Ready: 3, Status: "Running"}
	ws := []inventory.Workload{sts, healthy}
	ds := []appsv1.Deployment{
		deploy("db", "pg", cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg)),
		deploy("shop", "web", cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg)),
	}

	Annotate(ws, ds, nil, nil, nil, rhNow)

	if len(ws[0].Findings) != 0 {
		t.Errorf("StatefulSet: want no finding, got %+v", ws[0].Findings)
	}
	if len(ws[1].Findings) != 0 {
		t.Errorf("unflagged Deployment: want no finding, got %+v", ws[1].Findings)
	}
}

func TestAnnotate_NoMatchingDeployment(t *testing.T) {
	ws := []inventory.Workload{degraded("shop", "api")}
	Annotate(ws, nil, nil, nil, nil, rhNow) // no controller objects at all — must not panic, no finding
	if len(ws[0].Findings) != 0 {
		t.Errorf("want no finding when the Deployment is absent, got %+v", ws[0].Findings)
	}
}

// A StatefulSet carries no Progressing and no ReplicaFailure condition — it has
// no conditions at all — so stuckCondition has nothing to read and a wedged
// StatefulSet used to render `0/N Degraded` with no cause. The signal lives in
// the revision and replica counters instead.
func TestAnnotate_StatefulSetStuckUpdate(t *testing.T) {
	ws := []inventory.Workload{degradedKind("db", "pg", "StatefulSet")}
	sets := []appsv1.StatefulSet{statefulSet("db", "pg", 3, appsv1.StatefulSetStatus{
		Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 0,
		CurrentRevision: "pg-6d4b", UpdateRevision: "pg-7f9c",
	}, oldEnough)}
	pods := []corev1.Pod{ownedPod("db", "pg-0", "StatefulSet", "pg", false, oldEnough)}

	Annotate(ws, nil, sets, nil, pods, rhNow)

	if len(ws[0].Findings) != 1 {
		t.Fatalf("want one finding, got %+v", ws[0].Findings)
	}
	f := ws[0].Findings[0]
	if f.Issue != "RolloutStuck" {
		t.Errorf("Issue = %q, want the existing RolloutStuck kind", f.Issue)
	}
	if f.Reason != "the StatefulSet's rollout cannot complete — the new pods are not becoming available" {
		t.Errorf("Reason = %q, want the workload's own kind named", f.Reason)
	}
	if f.Evidence != "updatedReplicas 0/3, update revision pending" {
		t.Errorf("Evidence = %q", f.Evidence)
	}
	if f.Pod != "db/pg" {
		t.Errorf("Pod = %q, want db/pg", f.Pod)
	}
}

// No update is in flight, so the counters that matter are the ready ones: the
// StatefulSet cannot bring its replicas up.
func TestAnnotate_StatefulSetStuckScaleUp(t *testing.T) {
	ws := []inventory.Workload{degradedKind("db", "pg", "StatefulSet")}
	sets := []appsv1.StatefulSet{statefulSet("db", "pg", 3, appsv1.StatefulSetStatus{
		Replicas: 3, ReadyReplicas: 1, UpdatedReplicas: 3,
		CurrentRevision: "pg-6d4b", UpdateRevision: "pg-6d4b",
	}, oldEnough)}
	pods := []corev1.Pod{ownedPod("db", "pg-1", "StatefulSet", "pg", false, oldEnough)}

	Annotate(ws, nil, sets, nil, pods, rhNow)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Evidence != "readyReplicas 1/3, revision unchanged" {
		t.Fatalf("want the readyReplicas evidence, got %+v", ws[0].Findings)
	}
}

// "Not yet finished" and "wedged" look identical in a single sample, so the arm
// waits out the grace period before claiming anything.
func TestAnnotate_StatefulSetYoungPodIsNotYetStuck(t *testing.T) {
	ws := []inventory.Workload{degradedKind("db", "pg", "StatefulSet")}
	sets := []appsv1.StatefulSet{statefulSet("db", "pg", 3, appsv1.StatefulSetStatus{
		Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 0,
		CurrentRevision: "pg-6d4b", UpdateRevision: "pg-7f9c",
	}, oldEnough)}
	pods := []corev1.Pod{
		ownedPod("db", "pg-0", "StatefulSet", "pg", false, oldEnough),
		ownedPod("db", "pg-1", "StatefulSet", "pg", false, tooYoung), // still starting
	}

	Annotate(ws, nil, sets, nil, pods, rhNow)

	if len(ws[0].Findings) != 0 {
		t.Errorf("a rollout still inside the grace period is not stuck, got %+v", ws[0].Findings)
	}
}

// A ready pod's age is irrelevant — it is already where the rollout wants it.
func TestAnnotate_StatefulSetYoungReadyPodDoesNotHold(t *testing.T) {
	ws := []inventory.Workload{degradedKind("db", "pg", "StatefulSet")}
	sets := []appsv1.StatefulSet{statefulSet("db", "pg", 3, appsv1.StatefulSetStatus{
		Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 0,
		CurrentRevision: "pg-6d4b", UpdateRevision: "pg-7f9c",
	}, oldEnough)}
	pods := []corev1.Pod{
		ownedPod("db", "pg-0", "StatefulSet", "pg", false, oldEnough),
		ownedPod("db", "pg-1", "StatefulSet", "pg", true, tooYoung),
	}

	Annotate(ws, nil, sets, nil, pods, rhNow)

	if len(ws[0].Findings) != 1 {
		t.Errorf("want the finding — the young pod is ready, got %+v", ws[0].Findings)
	}
}

// A StatefulSet whose controller has created nothing yet has no young pod to
// hold the arm back, so its own age is what says whether it has had time.
func TestAnnotate_YoungStatefulSetWithNoPodsIsNotYetStuck(t *testing.T) {
	ws := []inventory.Workload{degradedKind("db", "pg", "StatefulSet")}
	sets := []appsv1.StatefulSet{statefulSet("db", "pg", 3, appsv1.StatefulSetStatus{
		Replicas: 0, ReadyReplicas: 0, UpdatedReplicas: 0,
		CurrentRevision: "pg-6d4b", UpdateRevision: "pg-6d4b",
	}, tooYoung)}

	Annotate(ws, nil, sets, nil, nil, rhNow)

	if len(ws[0].Findings) != 0 {
		t.Errorf("a StatefulSet created a minute ago is not a wedged rollout, got %+v", ws[0].Findings)
	}
}

// A pod owned by a different StatefulSet in the same namespace must not hold
// this one's arm back.
func TestAnnotate_StatefulSetIgnoresAnotherWorkloadsPod(t *testing.T) {
	ws := []inventory.Workload{degradedKind("db", "pg", "StatefulSet")}
	sets := []appsv1.StatefulSet{statefulSet("db", "pg", 3, appsv1.StatefulSetStatus{
		Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 0,
		CurrentRevision: "pg-6d4b", UpdateRevision: "pg-7f9c",
	}, oldEnough)}
	pods := []corev1.Pod{ownedPod("db", "cache-0", "StatefulSet", "cache", false, tooYoung)}

	Annotate(ws, nil, sets, nil, pods, rhNow)

	if len(ws[0].Findings) != 1 {
		t.Errorf("another workload's young pod must not suppress this one, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_DaemonSetStuckUpdate(t *testing.T) {
	ws := []inventory.Workload{degradedKind("sys", "agent", "DaemonSet")}
	sets := []appsv1.DaemonSet{daemonSet("sys", "agent", appsv1.DaemonSetStatus{
		DesiredNumberScheduled: 3, NumberReady: 1, UpdatedNumberScheduled: 1,
	}, oldEnough)}
	pods := []corev1.Pod{ownedPod("sys", "agent-aaa", "DaemonSet", "agent", false, oldEnough)}

	Annotate(ws, nil, nil, sets, pods, rhNow)

	if len(ws[0].Findings) != 1 {
		t.Fatalf("want one finding, got %+v", ws[0].Findings)
	}
	f := ws[0].Findings[0]
	if f.Issue != "RolloutStuck" {
		t.Errorf("Issue = %q, want the existing RolloutStuck kind", f.Issue)
	}
	if f.Reason != "the DaemonSet's rollout cannot complete — the new pods are not becoming available" {
		t.Errorf("Reason = %q, want the workload's own kind named", f.Reason)
	}
	if f.Evidence != "numberReady 1/3, updatedNumberScheduled 1/3" {
		t.Errorf("Evidence = %q", f.Evidence)
	}
}

// Every node has the new pod and none of them come ready — the update rolled
// out, the pods do not work.
func TestAnnotate_DaemonSetAllUpdatedNoneReady(t *testing.T) {
	ws := []inventory.Workload{degradedKind("sys", "agent", "DaemonSet")}
	sets := []appsv1.DaemonSet{daemonSet("sys", "agent", appsv1.DaemonSetStatus{
		DesiredNumberScheduled: 3, NumberReady: 0, UpdatedNumberScheduled: 3,
	}, oldEnough)}
	pods := []corev1.Pod{ownedPod("sys", "agent-aaa", "DaemonSet", "agent", false, oldEnough)}

	Annotate(ws, nil, nil, sets, pods, rhNow)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Evidence != "numberReady 0/3, all pods updated" {
		t.Fatalf("want the all-updated evidence, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_DaemonSetYoungPodIsNotYetStuck(t *testing.T) {
	ws := []inventory.Workload{degradedKind("sys", "agent", "DaemonSet")}
	sets := []appsv1.DaemonSet{daemonSet("sys", "agent", appsv1.DaemonSetStatus{
		DesiredNumberScheduled: 3, NumberReady: 1, UpdatedNumberScheduled: 1,
	}, oldEnough)}
	pods := []corev1.Pod{ownedPod("sys", "agent-aaa", "DaemonSet", "agent", false, tooYoung)}

	Annotate(ws, nil, nil, sets, pods, rhNow)

	if len(ws[0].Findings) != 0 {
		t.Errorf("a rollout still inside the grace period is not stuck, got %+v", ws[0].Findings)
	}
}

// A DaemonSet that wants no pods at all — every node excluded by a selector or
// a taint — is not a wedged rollout, and 0/0 is not a shortfall.
func TestAnnotate_DaemonSetWantingNothingIsNotStuck(t *testing.T) {
	ws := []inventory.Workload{degradedKind("sys", "agent", "DaemonSet")}
	sets := []appsv1.DaemonSet{daemonSet("sys", "agent", appsv1.DaemonSetStatus{
		DesiredNumberScheduled: 0, NumberReady: 0, UpdatedNumberScheduled: 0,
	}, oldEnough)}

	Annotate(ws, nil, nil, sets, nil, rhNow)

	if len(ws[0].Findings) != 0 {
		t.Errorf("want no finding for a DaemonSet scheduled nowhere, got %+v", ws[0].Findings)
	}
}

// The report a fleet or gate run forwards must not carry cluster identity. The
// two new arms read status counters only, so a pod, node or image name cannot
// reach the evidence — pinned here rather than left to review.
func TestAnnotate_EvidenceNamesNoPodNodeOrImage(t *testing.T) {
	ws := []inventory.Workload{
		degradedKind("db", "pg", "StatefulSet"),
		degradedKind("sys", "agent", "DaemonSet"),
	}
	sets := []appsv1.StatefulSet{statefulSet("db", "pg", 3, appsv1.StatefulSetStatus{
		Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 0,
		CurrentRevision: "pg-6d4b", UpdateRevision: "pg-7f9c",
	}, oldEnough)}
	dsets := []appsv1.DaemonSet{daemonSet("sys", "agent", appsv1.DaemonSetStatus{
		DesiredNumberScheduled: 3, NumberReady: 1, UpdatedNumberScheduled: 1,
	}, oldEnough)}
	pods := []corev1.Pod{
		ownedPod("db", "pg-0", "StatefulSet", "pg", false, oldEnough),
		ownedPod("sys", "agent-aaa", "DaemonSet", "agent", false, oldEnough),
	}
	pods[0].Spec.NodeName = "node-a"
	pods[1].Spec.NodeName = "node-b"

	Annotate(ws, nil, sets, dsets, pods, rhNow)

	for _, w := range ws {
		if len(w.Findings) != 1 {
			t.Fatalf("%s: want one finding, got %+v", w.Kind, w.Findings)
		}
		ev := w.Findings[0].Evidence
		for _, leak := range []string{"pg-0", "agent-aaa", "node-a", "node-b", "pg-6d4b", "pg-7f9c"} {
			if strings.Contains(ev, leak) {
				t.Errorf("%s: Evidence = %q, must not name %q", w.Kind, ev, leak)
			}
		}
	}
}

// The restart-count and existing-finding gates are the workload's, not the
// Deployment arm's: a crash-looping StatefulSet gets the same silence.
func TestAnnotate_StatefulSetWithARestartingPodIsSkipped(t *testing.T) {
	w := degradedKind("db", "pg", "StatefulSet")
	w.Pods = []inventory.PodRow{{Name: "pg-0", Restarts: diagnose.RestartThreshold}}
	ws := []inventory.Workload{w}
	sets := []appsv1.StatefulSet{statefulSet("db", "pg", 3, appsv1.StatefulSetStatus{
		Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 0,
		CurrentRevision: "pg-6d4b", UpdateRevision: "pg-7f9c",
	}, oldEnough)}
	pods := []corev1.Pod{ownedPod("db", "pg-0", "StatefulSet", "pg", false, oldEnough)}

	Annotate(ws, nil, sets, nil, pods, rhNow)

	if len(ws[0].Findings) != 0 {
		t.Errorf("want no finding when a pod is restarting repeatedly, got %+v", ws[0].Findings)
	}
}

// The ReplicaFailure arm is the long-horizon fallback, and no live test can
// reach it.
//
// A Deployment blocked from creating pods produces two signals from one cause:
// a FailedCreate event on its ReplicaSet, and a ReplicaFailure condition on the
// Deployment. createhealth.Annotate runs first and reads the events, so while
// the event is alive the workload already carries a FailedCreate finding and
// this package's existing-finding gate keeps RolloutStuck silent — which is the
// correct precedence, because FailedCreate names the cause specifically. A
// Kubernetes event expires after about an hour; the condition does not. Once
// the events have aged out the general answer is the only one left, and this
// arm is what supplies it.
//
// That is why the event slice here is empty rather than merely absent: it is
// the aged-out cluster, not a simplification. Every short live test — including
// the validation run that prompted this test — exercises the first hour and
// therefore always sees FailedCreate, so this arm has no live coverage at all.
func TestAnnotate_ReplicaFailureIsTheFallbackOnceEventsHaveAgedOut(t *testing.T) {
	const msg = `pods "api-7f9c-" is forbidden: exceeded quota: compute, requested: pods=1, used: pods=4, limited: pods=4`
	w := degraded("shop", "api")
	w.Desired, w.Ready = 1, 0 // the controller created nothing at all
	ws := []inventory.Workload{w}

	// The events have expired. createhealth reads events and nothing else, so
	// it attaches nothing and leaves the existing-finding gate open.
	createhealth.Annotate(ws, nil, nil)
	if len(ws[0].Findings) != 0 {
		t.Fatalf("createhealth must attach nothing with no events, got %+v", ws[0].Findings)
	}

	deps := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "FailedCreate", msg))}
	Annotate(ws, deps, nil, nil, nil, rhNow)

	if len(ws[0].Findings) != 1 {
		t.Fatalf("want exactly one finding, got %+v", ws[0].Findings)
	}
	f := ws[0].Findings[0]
	if f.Issue != "RolloutStuck" {
		t.Errorf("Issue = %q, want RolloutStuck", f.Issue)
	}
	if want := "ReplicaFailure: " + msg; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

// hostileText is what a Deployment's condition message carries when whatever
// wrote it was not the kube-controller-manager: invalid UTF-8, a
// screen-clearing ANSI escape, a right-to-left override and a NUL.
const hostileText = "timed\x1b[2J‮gnp\x00 out\xff"

// Both condition arms read a message the API server does not validate. A
// condition is selected by type, status and (for Progressing) reason, so no
// matching decision reads the message itself.
func TestAnnotate_SanitizesAConditionMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    appsv1.DeploymentCondition
	}{
		{"ReplicaFailure", cond(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "FailedCreate", hostileText)},
		{"ProgressDeadlineExceeded", cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", hostileText)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := []inventory.Workload{degraded("shop", "api")}
			Annotate(ws, []appsv1.Deployment{deploy("shop", "api", tc.c)}, nil, nil, nil, rhNow)
			if len(ws[0].Findings) != 1 {
				t.Fatalf("want one finding, got %+v", ws[0].Findings)
			}
			assertSanitized(t, "Evidence", ws[0].Findings[0].Evidence)
		})
	}
}

// assertSanitized fails unless s is what safetext.Line guarantees: valid UTF-8
// with no control characters and no Unicode formatting characters.
func assertSanitized(t *testing.T, where, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("%s is not valid UTF-8: %q", where, s)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Errorf("%s carries %U: %q", where, r, s)
		}
	}
}
