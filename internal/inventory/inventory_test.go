package inventory

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

func TestTermTime(t *testing.T) {
	if got := termTime(metav1.Time{}); got != "" {
		t.Errorf("zero time: got %q, want empty", got)
	}
	ts := metav1.Date(2026, 6, 22, 8, 14, 3, 0, time.UTC)
	if got := termTime(ts); got != "2026-06-22T08:14:03Z" {
		t.Errorf("got %q, want RFC3339 UTC", got)
	}
}

func TestHumanAge(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"days", now.Add(-36 * 24 * time.Hour), "36d"},
		{"hours", now.Add(-5 * time.Hour), "5h"},
		{"minutes", now.Add(-3 * time.Minute), "3m"},
		{"seconds", now.Add(-10 * time.Second), "10s"},
		{"future clamps to 0s", now.Add(time.Hour), "0s"},
	}
	for _, c := range cases {
		if got := HumanAge(c.t, now); got != c.want {
			t.Errorf("%s: HumanAge = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestControllerOwner(t *testing.T) {
	yes := true
	no := false
	refs := []metav1.OwnerReference{
		{Kind: "Node", Name: "n1", Controller: &no},
		{Kind: "ReplicaSet", Name: "rs1", Controller: &yes},
	}
	if o := controllerOwner(refs); o == nil || o.Kind != "ReplicaSet" {
		t.Errorf("expected the controller ref (ReplicaSet), got %+v", o)
	}
	if o := controllerOwner(nil); o != nil {
		t.Errorf("expected nil for no refs, got %+v", o)
	}
	noController := []metav1.OwnerReference{
		{Kind: "Node", Name: "n1", Controller: &no},
	}
	if o := controllerOwner(noController); o == nil || o.Kind != "Node" {
		t.Errorf("expected first ref when no controller is set, got %+v", o)
	}
}

func TestPodRestarts(t *testing.T) {
	t1 := metav1.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t2 := metav1.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC) // later
	p := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{RestartCount: 31, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: t1}}},
		{RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: t2}}},
	}}}
	n, last := podRestarts(p)
	if n != 32 {
		t.Errorf("total restarts = %d, want 32", n)
	}
	if termTime(last) != "2026-06-10T00:00:00Z" {
		t.Errorf("last restart = %q, want the later time", termTime(last))
	}
}

func TestPodReadyAndIsReady(t *testing.T) {
	p := corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Ready: true}, {Ready: false},
		}},
	}
	if got := podReady(p); got != "1/2" {
		t.Errorf("podReady = %q, want 1/2", got)
	}
	if podIsReady(p) {
		t.Error("podIsReady should be false when a container is not ready")
	}
	p.Status.ContainerStatuses[1].Ready = true
	if !podIsReady(p) {
		t.Error("podIsReady should be true when all containers are ready")
	}
}

func TestWorkloadStatusAndFlagged(t *testing.T) {
	if WorkloadStatus(3, 3) != "Running" {
		t.Error("3/3 should be Running")
	}
	if WorkloadStatus(1, 2) != "Degraded" {
		t.Error("1/2 should be Degraded")
	}
	healthy := Workload{Ready: 3, Desired: 3}
	if healthy.Flagged() {
		t.Error("healthy workload should not be flagged")
	}
	degraded := Workload{Ready: 1, Desired: 2}
	if !degraded.Flagged() {
		t.Error("degraded workload should be flagged")
	}
	withFinding := Workload{Ready: 1, Desired: 1, Findings: []diagnose.Finding{{Pod: "ns/p", Issue: "X"}}}
	if !withFinding.Flagged() {
		t.Error("a workload with a finding should be flagged even when ready==desired")
	}
	if WorkloadStatus(0, 0) != "Scaled Down" {
		t.Error("0/0 should be Scaled Down, not Degraded")
	}
}

// pod builds a one-container pod with the given restart count (recorded in the
// current container status) and image. It is NOT ready by default.
func pod(ns, name string, owners []metav1.OwnerReference, restarts int32, image string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: owners},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c", RestartCount: restarts, Ready: false}},
		},
	}
}

// readyPod is like pod but its single container is Ready.
func readyPod(ns, name string, owners []metav1.OwnerReference, image string) corev1.Pod {
	p := pod(ns, name, owners, 0, image)
	p.Status.ContainerStatuses[0].Ready = true
	return p
}

func p32(n int32) *int32 { return &n }

func ctrlRef(kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
}

func TestAssemble_DeploymentGroupsPodsAndAggregates(t *testing.T) {
	in := Inputs{
		Deployments: []appsv1.Deployment{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cattle-system", Name: "rancher"},
			Spec:       appsv1.DeploymentSpec{Replicas: p32(3)},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 3},
		}},
		ReplicaSets: []appsv1.ReplicaSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cattle-system", Name: "rancher-f7fb", OwnerReferences: ctrlRef("Deployment", "rancher")},
		}},
		Pods: []corev1.Pod{
			pod("cattle-system", "rancher-f7fb-64smq", ctrlRef("ReplicaSet", "rancher-f7fb"), 31, "rancher/rancher:v2.14.1"),
			pod("cattle-system", "rancher-f7fb-d2th5", ctrlRef("ReplicaSet", "rancher-f7fb"), 32, "rancher/rancher:v2.14.1"),
		},
	}
	ws := Assemble(in, nil)
	if len(ws) != 1 {
		t.Fatalf("expected 1 workload, got %d: %+v", len(ws), ws)
	}
	w := ws[0]
	if w.Kind != "Deployment" || w.Name != "rancher" {
		t.Errorf("kind/name = %s/%s, want Deployment/rancher", w.Kind, w.Name)
	}
	if w.Namespace != "cattle-system" {
		t.Errorf("namespace = %q, want cattle-system", w.Namespace)
	}
	if w.Desired != 3 || w.Ready != 3 || w.Status != "Running" {
		t.Errorf("got %d/%d %s, want 3/3 Running", w.Ready, w.Desired, w.Status)
	}
	if w.Restarts != 63 {
		t.Errorf("restarts = %d, want 63", w.Restarts)
	}
	if len(w.Pods) != 2 {
		t.Errorf("expected 2 pod rows, got %d", len(w.Pods))
	}
	if w.Image != "rancher/rancher:v2.14.1" {
		t.Errorf("image = %q", w.Image)
	}
}

func TestAssemble_AttachesFindingsAndSortsFlaggedFirst(t *testing.T) {
	in := Inputs{
		Deployments: []appsv1.Deployment{
			{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "healthy"}, Spec: appsv1.DeploymentSpec{Replicas: p32(1)}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "broken"}, Spec: appsv1.DeploymentSpec{Replicas: p32(2)}, Status: appsv1.DeploymentStatus{ReadyReplicas: 2}},
		},
		ReplicaSets: []appsv1.ReplicaSet{
			{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "healthy-rs", OwnerReferences: ctrlRef("Deployment", "healthy")}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "broken-rs", OwnerReferences: ctrlRef("Deployment", "broken")}},
		},
		Pods: []corev1.Pod{
			pod("a", "healthy-rs-1", ctrlRef("ReplicaSet", "healthy-rs"), 0, "img"),
			pod("a", "broken-rs-1", ctrlRef("ReplicaSet", "broken-rs"), 5, "img"),
		},
	}
	findings := []diagnose.Finding{{Pod: "a/broken-rs-1", Issue: "CrashLoopBackOff", Reason: "boom", Evidence: "x"}}
	ws := Assemble(in, findings)
	if len(ws) != 2 {
		t.Fatalf("expected 2 workloads, got %d", len(ws))
	}
	if ws[0].Name != "broken" || !ws[0].Flagged() {
		t.Errorf("flagged workload should sort first; got %+v", ws[0])
	}
	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Issue != "CrashLoopBackOff" {
		t.Errorf("finding not attached to broken: %+v", ws[0].Findings)
	}
	if ws[1].Name != "healthy" || ws[1].Flagged() {
		t.Errorf("healthy workload should sort last and be unflagged; got %+v", ws[1])
	}
}

func TestAssemble_BarePodBecomesItsOwnWorkload(t *testing.T) {
	in := Inputs{Pods: []corev1.Pod{
		readyPod("default", "lonely", nil, "img"), // no owner refs → bare pod
	}}
	ws := Assemble(in, nil)
	if len(ws) != 1 || ws[0].Kind != "Pod" || ws[0].Name != "lonely" {
		t.Fatalf("expected a bare-pod workload, got %+v", ws)
	}
	if ws[0].Desired != 1 || ws[0].Ready != 1 || ws[0].Status != "Running" {
		t.Errorf("bare pod health = %d/%d %s, want 1/1 Running", ws[0].Ready, ws[0].Desired, ws[0].Status)
	}
}

// terminalBarePod builds an owner-less pod that has run to completion in the
// given terminal phase (its single container is terminated and not ready).
func terminalBarePod(ns, name string, phase corev1.PodPhase, exitCode int32) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, // no owner → bare pod
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "c", Image: "img"}},
		},
		Status: corev1.PodStatus{
			Phase: phase,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "c", Ready: false,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}},
			}},
		},
	}
}

func TestAssemble_CompletedBarePodIsComplete(t *testing.T) {
	// A bare pod from `kubectl run` that ran once and exited 0 sits in
	// Succeeded/Completed. It is finished, not a degraded long-running workload,
	// so it must not be flagged (mirrors a completed Job).
	in := Inputs{Pods: []corev1.Pod{terminalBarePod("cattle-monitoring-system", "amtool-q-8056", corev1.PodSucceeded, 0)}}
	ws := Assemble(in, nil)
	if len(ws) != 1 || ws[0].Kind != "Pod" || ws[0].Name != "amtool-q-8056" {
		t.Fatalf("expected a bare-pod workload, got %+v", ws)
	}
	if ws[0].Status != "Complete" {
		t.Errorf("status = %q, want Complete", ws[0].Status)
	}
	if ws[0].Flagged() {
		t.Errorf("a completed bare pod must not be flagged; got %+v", ws[0])
	}
}

func TestAssemble_FailedBarePodIsFailedAndFlagged(t *testing.T) {
	in := Inputs{Pods: []corev1.Pod{terminalBarePod("batch", "oneshot-bad", corev1.PodFailed, 1)}}
	ws := Assemble(in, nil)
	if len(ws) != 1 || ws[0].Status != "Failed" {
		t.Fatalf("want a Failed bare-pod workload, got %+v", ws)
	}
	if !ws[0].Flagged() {
		t.Error("a failed bare pod should be flagged")
	}
}

func TestAssemble_PendingBarePodStillFlagged(t *testing.T) {
	// A non-terminal (Pending) bare pod is genuinely not running yet — it must
	// keep the ready<desired Degraded behavior and stay flagged.
	p := pod("default", "stuck", nil, 0, "img") // pod() builds a Running, not-ready pod
	p.Status.Phase = corev1.PodPending
	ws := Assemble(Inputs{Pods: []corev1.Pod{p}}, nil)
	if len(ws) != 1 || ws[0].Status != "Degraded" || !ws[0].Flagged() {
		t.Fatalf("a pending bare pod should stay Degraded and flagged, got %+v", ws)
	}
}

func TestAssemble_StatefulSetSeeding(t *testing.T) {
	in := Inputs{
		StatefulSets: []appsv1.StatefulSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "etcd"},
			Spec:       appsv1.StatefulSetSpec{Replicas: p32(3)},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 3},
		}},
		Pods: []corev1.Pod{pod("db", "etcd-0", ctrlRef("StatefulSet", "etcd"), 0, "etcd:3.5")},
	}
	ws := Assemble(in, nil)
	if len(ws) != 1 || ws[0].Kind != "StatefulSet" || ws[0].Name != "etcd" {
		t.Fatalf("got %+v", ws)
	}
	if ws[0].Desired != 3 || ws[0].Ready != 3 || ws[0].Status != "Running" {
		t.Errorf("got %d/%d %s, want 3/3 Running", ws[0].Ready, ws[0].Desired, ws[0].Status)
	}
}

func TestAssemble_DaemonSetSeeding(t *testing.T) {
	in := Inputs{
		DaemonSets: []appsv1.DaemonSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "node-exporter"},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 5, NumberReady: 4},
		}},
		Pods: []corev1.Pod{pod("kube-system", "node-exporter-abc", ctrlRef("DaemonSet", "node-exporter"), 0, "node-exporter:1")},
	}
	ws := Assemble(in, nil)
	if len(ws) != 1 || ws[0].Kind != "DaemonSet" {
		t.Fatalf("got %+v", ws)
	}
	if ws[0].Desired != 5 || ws[0].Ready != 4 || ws[0].Status != "Degraded" {
		t.Errorf("got %d/%d %s, want 4/5 Degraded", ws[0].Ready, ws[0].Desired, ws[0].Status)
	}
}

func TestAssemble_ReplicaSetWithoutDeploymentFallback(t *testing.T) {
	// The pod's ReplicaSet owner is not resolvable to a Deployment (no matching
	// ReplicaSet in Inputs), so it falls back to a ReplicaSet workload with
	// pod-derived counts.
	in := Inputs{Pods: []corev1.Pod{readyPod("a", "orphan-rs-1", ctrlRef("ReplicaSet", "orphan-rs"), "img")}}
	ws := Assemble(in, nil)
	if len(ws) != 1 || ws[0].Kind != "ReplicaSet" || ws[0].Name != "orphan-rs" {
		t.Fatalf("expected a ReplicaSet fallback workload, got %+v", ws)
	}
	if ws[0].Desired != 1 || ws[0].Ready != 1 {
		t.Errorf("derived counts = %d/%d, want 1/1", ws[0].Ready, ws[0].Desired)
	}
}

func TestHumanSince(t *testing.T) {
	now := time.Date(2026, 6, 22, 8, 14, 3, 0, time.UTC)
	if got := HumanSince("", now); got != "" {
		t.Errorf("empty -> %q, want \"\"", got)
	}
	if got := HumanSince("not-a-time", now); got != "" {
		t.Errorf("unparseable -> %q, want \"\"", got)
	}
	if got := HumanSince("2026-06-02T08:14:03Z", now); got != "20d ago" {
		t.Errorf("got %q, want \"20d ago\"", got)
	}
}

func TestJobStatus(t *testing.T) {
	failed := batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}}
	if jobStatus(failed) != "Failed" {
		t.Errorf("failed job: got %q", jobStatus(failed))
	}
	complete := batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
	if jobStatus(complete) != "Complete" {
		t.Errorf("complete job: got %q", jobStatus(complete))
	}
	running := batchv1.Job{Status: batchv1.JobStatus{Active: 2}}
	if jobStatus(running) != "Running" {
		t.Errorf("active job: got %q", jobStatus(running))
	}
	pending := batchv1.Job{}
	if jobStatus(pending) != "Pending" {
		t.Errorf("idle job: got %q", jobStatus(pending))
	}
}

func TestCronJobStatus(t *testing.T) {
	active := batchv1.CronJob{Status: batchv1.CronJobStatus{Active: []corev1.ObjectReference{{}, {}}}}
	if cronJobStatus(active) != "Active(2)" {
		t.Errorf("active cronjob: got %q", cronJobStatus(active))
	}
	idle := batchv1.CronJob{}
	if cronJobStatus(idle) != "Idle" {
		t.Errorf("idle cronjob: got %q", cronJobStatus(idle))
	}
}

func TestFlagged_FailedStatus(t *testing.T) {
	w := Workload{Kind: "Job", Ready: 0, Desired: 0, Status: "Failed"}
	if !w.Flagged() {
		t.Error("a Failed job should be flagged")
	}
	ok := Workload{Kind: "Job", Ready: 0, Desired: 0, Status: "Complete"}
	if ok.Flagged() {
		t.Error("a Complete job should not be flagged")
	}
}

func TestAssemble_StandaloneJob(t *testing.T) {
	in := Inputs{
		Jobs: []batchv1.Job{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "migrate"},
			Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
		}},
		Pods: []corev1.Pod{pod("batch", "migrate-xyz", ctrlRef("Job", "migrate"), 0, "migrate:1")},
	}
	ws := Assemble(in, nil)
	if len(ws) != 1 || ws[0].Kind != "Job" || ws[0].Name != "migrate" {
		t.Fatalf("got %+v", ws)
	}
	if ws[0].Status != "Complete" {
		t.Errorf("status = %q, want Complete", ws[0].Status)
	}
	if len(ws[0].Pods) != 1 {
		t.Errorf("expected 1 pod row, got %d", len(ws[0].Pods))
	}
}

func TestAssemble_CronJobRollsUpItsJobsPods(t *testing.T) {
	in := Inputs{
		CronJobs: []batchv1.CronJob{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "backup"},
			Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *"},
		}},
		Jobs: []batchv1.Job{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "backup-28000", OwnerReferences: ctrlRef("CronJob", "backup")},
		}},
		Pods: []corev1.Pod{pod("batch", "backup-28000-aaa", ctrlRef("Job", "backup-28000"), 0, "backup:1")},
	}
	ws := Assemble(in, nil)
	// Only the CronJob workload (the Job is not seeded separately; its pod rolls up).
	if len(ws) != 1 || ws[0].Kind != "CronJob" || ws[0].Name != "backup" {
		t.Fatalf("expected one CronJob workload, got %+v", ws)
	}
	if ws[0].Schedule != "0 2 * * *" {
		t.Errorf("schedule = %q", ws[0].Schedule)
	}
	if len(ws[0].Pods) != 1 || ws[0].Pods[0].Name != "backup-28000-aaa" {
		t.Errorf("expected the job's pod under the cronjob, got %+v", ws[0].Pods)
	}
}

func TestAssemble_CapsJobPods(t *testing.T) {
	in := Inputs{
		Jobs: []batchv1.Job{{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "noisy"}}},
		Pods: []corev1.Pod{
			pod("batch", "noisy-1", ctrlRef("Job", "noisy"), 0, "i"),
			pod("batch", "noisy-2", ctrlRef("Job", "noisy"), 0, "i"),
			pod("batch", "noisy-3", ctrlRef("Job", "noisy"), 0, "i"),
			pod("batch", "noisy-4", ctrlRef("Job", "noisy"), 0, "i"),
			pod("batch", "noisy-5", ctrlRef("Job", "noisy"), 0, "i"),
		},
	}
	ws := Assemble(in, nil)
	if len(ws) != 1 {
		t.Fatalf("got %d workloads", len(ws))
	}
	if len(ws[0].Pods) != 3 {
		t.Errorf("expected pods capped to 3, got %d", len(ws[0].Pods))
	}
	if ws[0].PodsOmitted != 2 {
		t.Errorf("PodsOmitted = %d, want 2", ws[0].PodsOmitted)
	}
}

func TestAssemble_OrphanedCronJobPodFallsBackToJob(t *testing.T) {
	// The Job is owned by a CronJob, but that CronJob object isn't in Inputs.
	// The pod must group under the Job, not a phantom CronJob workload.
	in := Inputs{
		Jobs: []batchv1.Job{{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "backup-28000", OwnerReferences: ctrlRef("CronJob", "gone")}}},
		Pods: []corev1.Pod{pod("batch", "backup-28000-aaa", ctrlRef("Job", "backup-28000"), 0, "i")},
	}
	ws := Assemble(in, nil)
	if len(ws) != 1 || ws[0].Kind != "Job" || ws[0].Name != "backup-28000" {
		t.Fatalf("expected a Job fallback workload, got %+v", ws)
	}
}

func TestSortWorkloads_KindTiebreaker(t *testing.T) {
	ws := []Workload{
		{Namespace: "a", Name: "dup", Kind: "Job", Status: "Complete"},
		{Namespace: "a", Name: "dup", Kind: "Deployment", Ready: 1, Desired: 1},
	}
	sortWorkloads(ws)
	if ws[0].Kind != "Deployment" || ws[1].Kind != "Job" {
		t.Errorf("expected Deployment before Job on name tie, got %s then %s", ws[0].Kind, ws[1].Kind)
	}
}

func TestAssemble_AggregatesLastRestart(t *testing.T) {
	p := readyPod("a", "p1", nil, "img")
	p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)},
	}
	ws := Assemble(Inputs{Pods: []corev1.Pod{p}}, nil)
	if len(ws) != 1 || ws[0].LastRestart != "2026-06-10T00:00:00Z" {
		t.Fatalf("workload LastRestart = %q, want 2026-06-10T00:00:00Z; ws=%+v", ws[0].LastRestart, ws)
	}
	if ws[0].Pods[0].LastRestart != "2026-06-10T00:00:00Z" {
		t.Errorf("pod row LastRestart = %q", ws[0].Pods[0].LastRestart)
	}
}

func TestPrioritize_DefaultShowsOnlyProblems(t *testing.T) {
	in := []Workload{
		{Namespace: "a", Name: "crash", Kind: "Deployment", Ready: 0, Desired: 1, Status: "Degraded"},                 // problem
		{Namespace: "a", Name: "healthy", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running"},                // hidden
		{Namespace: "a", Name: "restarted", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running", Restarts: 9}, // hidden (restart-only)
		{Namespace: "a", Name: "backup", Kind: "CronJob", Status: "Idle"},                                             // hidden (cron)
	}
	res := Prioritize(in, Opts{})
	if len(res.Workloads) != 1 || res.Workloads[0].Name != "crash" {
		t.Fatalf("expected only the problem, got %+v", res.Workloads)
	}
	if res.Workloads[0].Priority != 2 {
		t.Errorf("problem priority = %d, want 2", res.Workloads[0].Priority)
	}
	if res.HiddenRestarts != 1 || res.HiddenCron != 1 {
		t.Errorf("hidden counts = %d restarts / %d cron, want 1/1", res.HiddenRestarts, res.HiddenCron)
	}
}

func TestPrioritize_IncludeRestartsAndCron(t *testing.T) {
	in := []Workload{
		{Namespace: "a", Name: "restarted", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running", Restarts: 9},
		{Namespace: "a", Name: "backup", Kind: "CronJob", Status: "Idle"},
		{Namespace: "a", Name: "healthy", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running"},
	}
	res := Prioritize(in, Opts{IncludeRestarts: true, IncludeCron: true})
	// restart-only (3) and cron (4) shown; healthy-quiet still hidden.
	if len(res.Workloads) != 2 {
		t.Fatalf("expected 2 shown, got %+v", res.Workloads)
	}
	if res.Workloads[0].Name != "restarted" || res.Workloads[0].Priority != 3 {
		t.Errorf("restart-only should sort first at priority 3, got %+v", res.Workloads[0])
	}
	if res.Workloads[1].Name != "backup" || res.Workloads[1].Priority != 4 {
		t.Errorf("cron should be priority 4, got %+v", res.Workloads[1])
	}
	if res.HiddenRestarts != 0 || res.HiddenCron != 0 {
		t.Errorf("nothing should be hidden when both flags on, got %d/%d", res.HiddenRestarts, res.HiddenCron)
	}
}

func TestPrioritize_FailedCronGatedButFailedJobIsProblem(t *testing.T) {
	in := []Workload{
		{Namespace: "a", Name: "cj", Kind: "CronJob", Status: "Idle"},
		{Namespace: "a", Name: "job", Kind: "Job", Status: "Failed"}, // Flagged() via Status=="Failed"
	}
	res := Prioritize(in, Opts{})
	if len(res.Workloads) != 1 || res.Workloads[0].Name != "job" || res.Workloads[0].Priority != 2 {
		t.Fatalf("failed standalone Job should be the only P2 shown, got %+v", res.Workloads)
	}
	if res.HiddenCron != 1 {
		t.Errorf("the CronJob should be hidden, HiddenCron=%d", res.HiddenCron)
	}
}

func TestPrioritize_FailingCronJobShownByDefault(t *testing.T) {
	flagged := Workload{Namespace: "shop", Name: "nightly", Kind: "CronJob", Status: "Idle",
		Findings: []diagnose.Finding{{Issue: "JobFailed", Reason: "the most recent scheduled run failed"}}}
	healthy := Workload{Namespace: "shop", Name: "hourly", Kind: "CronJob", Status: "Idle"}
	res := Prioritize([]Workload{flagged, healthy}, Opts{}) // no IncludeCron
	shown := map[string]int{}
	for _, w := range res.Workloads {
		shown[w.Name] = w.Priority
	}
	if p, ok := shown["nightly"]; !ok || p != priorityProblem {
		t.Errorf("a flagged CronJob must be shown at priorityProblem(%d); shown=%+v", priorityProblem, shown)
	}
	if _, ok := shown["hourly"]; ok {
		t.Errorf("a healthy CronJob must stay hidden without --include-cron")
	}
	if res.HiddenCron != 1 {
		t.Errorf("HiddenCron = %d, want 1 (the healthy CronJob only)", res.HiddenCron)
	}
}

func TestPrioritize_SortsByPriorityThenNamespaceName(t *testing.T) {
	in := []Workload{
		{Namespace: "b", Name: "p2", Kind: "Deployment", Ready: 0, Desired: 1},
		{Namespace: "a", Name: "p1", Kind: "Deployment", Ready: 0, Desired: 1},
		{Namespace: "a", Name: "r", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running", Restarts: 1},
	}
	res := Prioritize(in, Opts{IncludeRestarts: true})
	got := []string{res.Workloads[0].Name, res.Workloads[1].Name, res.Workloads[2].Name}
	want := []string{"p1", "p2", "r"} // both problems (a/p1, b/p2) before the restart-only (a/r)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sort order = %v, want %v", got, want)
		}
	}
}

func TestPrioritize_CensusCountsHiddenHealthyWorkloads(t *testing.T) {
	in := []Workload{
		{Namespace: "a", Name: "crash", Kind: "Deployment", Ready: 0, Desired: 1, Status: "Degraded"},
		{Namespace: "a", Name: "healthy", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running"},
		{Namespace: "a", Name: "also-healthy", Kind: "StatefulSet", Ready: 2, Desired: 2, Status: "Running"},
	}
	res := Prioritize(in, Opts{})
	// Only "crash" is displayed, but all three are long-running workloads.
	if len(res.Workloads) != 1 {
		t.Fatalf("expected 1 displayed workload, got %+v", res.Workloads)
	}
	if res.Census.Total != 3 {
		t.Errorf("Census.Total = %d, want 3 (the healthy majority must be counted)", res.Census.Total)
	}
	if res.Census.Good != 2 {
		t.Errorf("Census.Good = %d, want 2", res.Census.Good)
	}
}

func TestPrioritize_CensusExcludesJobAndCronJob(t *testing.T) {
	in := []Workload{
		{Namespace: "a", Name: "web", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running"},
		{Namespace: "a", Name: "backup", Kind: "CronJob", Status: "Idle"},
		{Namespace: "a", Name: "migrate", Kind: "Job", Status: "Complete"},
		{Namespace: "a", Name: "failed-job", Kind: "Job", Status: "Failed"},
	}
	res := Prioritize(in, Opts{})
	// Neither kind is expected to be continuously up, so neither belongs in an
	// availability figure — not even the failed Job, whose findings never clear.
	if res.Census.Total != 1 {
		t.Errorf("Census.Total = %d, want 1 (only the Deployment)", res.Census.Total)
	}
	if res.Census.Good != 1 {
		t.Errorf("Census.Good = %d, want 1", res.Census.Good)
	}
}

func TestPrioritize_CensusCountsUnderReplicatedAsBad(t *testing.T) {
	// The numerator defect, pinned directly: no Findings, but Ready < Desired.
	// len(Findings)==0 would call this good; Flagged() correctly does not.
	in := []Workload{
		{Namespace: "a", Name: "web", Kind: "Deployment", Ready: 1, Desired: 3, Status: "Degraded"},
	}
	res := Prioritize(in, Opts{})
	if res.Census.Total != 1 {
		t.Fatalf("Census.Total = %d, want 1", res.Census.Total)
	}
	if res.Census.Good != 0 {
		t.Errorf("Census.Good = %d, want 0: an under-replicated workload is not available", res.Census.Good)
	}
}

func TestPrioritize_CensusCountsUnknownKinds(t *testing.T) {
	// Assemble's pod rollup emits an arbitrary owner kind for CRD-owned pods.
	// The exclusion list must let those through.
	in := []Workload{
		{Namespace: "a", Name: "canary", Kind: "Rollout", Ready: 2, Desired: 2, Status: "Running"},
	}
	res := Prioritize(in, Opts{})
	if res.Census.Total != 1 || res.Census.Good != 1 {
		t.Errorf("Census = %+v, want {Good:1 Total:1}: an unknown controller kind is long-running", res.Census)
	}
}

func TestPrioritize_CensusCountsFindingsAsBadEvenWhenFullyReady(t *testing.T) {
	// Ready >= Desired here, so only the Findings disjunct of Flagged() can mark
	// this workload bad. A predicate narrowed to a ready/desired comparison would
	// wrongly call it Good.
	in := []Workload{
		{Namespace: "a", Name: "web", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running",
			Findings: []diagnose.Finding{{Pod: "ns/p", Issue: "X"}}},
	}
	res := Prioritize(in, Opts{})
	if res.Census.Total != 1 {
		t.Fatalf("Census.Total = %d, want 1", res.Census.Total)
	}
	if res.Census.Good != 0 {
		t.Errorf("Census.Good = %d, want 0: a fully-ready workload with findings is not available", res.Census.Good)
	}
}

func TestPrioritize_CensusCountsFailedStatusAsBadEvenWhenFullyReady(t *testing.T) {
	// Ready >= Desired here and there are no Findings, so only the
	// Status == "Failed" disjunct of Flagged() can mark this workload bad. A
	// predicate narrowed to a ready/desired comparison would wrongly call it Good.
	in := []Workload{
		{Namespace: "a", Name: "web", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Failed"},
	}
	res := Prioritize(in, Opts{})
	if res.Census.Total != 1 {
		t.Fatalf("Census.Total = %d, want 1", res.Census.Total)
	}
	if res.Census.Good != 0 {
		t.Errorf("Census.Good = %d, want 0: a fully-ready workload with Status==Failed is not available", res.Census.Good)
	}
}

func TestPrioritize_CensusCountsScaledToZeroAsGood(t *testing.T) {
	// Ready(0) < Desired(0) is false and WorkloadStatus(0, 0) is "Scaled Down",
	// not "Failed", so Flagged() is false and this workload counts as Good
	// forever. That is the intended reading, not an oversight: an operator who
	// deliberately scaled a Deployment to zero replicas is not experiencing an
	// outage, so the SLI must not dent availability for it. Pinned here so the
	// decision cannot silently flip to "Scaled Down counts as bad" later.
	in := []Workload{
		{Namespace: "a", Name: "paused", Kind: "Deployment", Ready: 0, Desired: 0, Status: "Scaled Down"},
	}
	res := Prioritize(in, Opts{})
	if res.Census.Total != 1 {
		t.Fatalf("Census.Total = %d, want 1", res.Census.Total)
	}
	if res.Census.Good != 1 {
		t.Errorf("Census.Good = %d, want 1: a scaled-to-zero workload is intentional, not an outage", res.Census.Good)
	}
}

func TestPodOwnersResolvesEveryOwnershipShape(t *testing.T) {
	in := Inputs{
		Pods: []corev1.Pod{
			pod("prod", "api-abc-1", ctrlRef("ReplicaSet", "api-abc"), 0, "app:1.0"),
			pod("prod", "orphan-rs-1", ctrlRef("ReplicaSet", "unknown-rs"), 0, "app:1.0"),
			pod("prod", "cache-0", ctrlRef("StatefulSet", "cache"), 0, "app:1.0"),
			pod("prod", "nightly-1-x", ctrlRef("Job", "nightly-1"), 0, "app:1.0"),
			pod("prod", "oneoff-x", ctrlRef("Job", "oneoff"), 0, "app:1.0"),
			pod("prod", "detached-x", ctrlRef("Job", "detached"), 0, "app:1.0"),
			pod("prod", "bare", nil, 0, "app:1.0"),
		},
		ReplicaSets: []appsv1.ReplicaSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api-abc",
				OwnerReferences: ctrlRef("Deployment", "api")},
		}},
		Jobs: []batchv1.Job{
			{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "nightly-1",
				OwnerReferences: ctrlRef("CronJob", "nightly")}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "oneoff"}},
			// detached names a CronJob that is not in in.CronJobs, so its pod
			// must roll up to the Job, not to a CronJob nothing lists.
			{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "detached",
				OwnerReferences: ctrlRef("CronJob", "vanished")}},
		},
		CronJobs: []batchv1.CronJob{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "nightly"},
		}},
	}

	got := PodOwners(in)
	want := map[string]Owner{
		"prod/api-abc-1":   {Kind: "Deployment", Namespace: "prod", Name: "api"},
		"prod/orphan-rs-1": {Kind: "ReplicaSet", Namespace: "prod", Name: "unknown-rs"},
		"prod/cache-0":     {Kind: "StatefulSet", Namespace: "prod", Name: "cache"},
		"prod/nightly-1-x": {Kind: "CronJob", Namespace: "prod", Name: "nightly"},
		"prod/oneoff-x":    {Kind: "Job", Namespace: "prod", Name: "oneoff"},
		"prod/detached-x":  {Kind: "Job", Namespace: "prod", Name: "detached"},
		"prod/bare":        {Kind: "Pod", Namespace: "prod", Name: "bare"},
	}
	if len(got) != len(want) {
		t.Fatalf("PodOwners returned %d entries, want %d: %+v", len(got), len(want), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("PodOwners[%q] = %+v, want %+v", k, got[k], w)
		}
	}
}

func TestPodOwnersKeepsEveryPodOfAJob(t *testing.T) {
	// Assemble truncates a Job's pod list at jobPodCap so a report stays
	// readable. PodOwners must not: a baseline needs every pod's restarts.
	in := Inputs{Jobs: []batchv1.Job{{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "batch"}}}}
	for i := 0; i < jobPodCap+4; i++ {
		in.Pods = append(in.Pods, pod("prod", "batch-"+strconv.Itoa(i), ctrlRef("Job", "batch"), 0, "app:1.0"))
	}
	if got := len(PodOwners(in)); got != jobPodCap+4 {
		t.Errorf("PodOwners returned %d entries, want %d — every pod must be resolved", got, jobPodCap+4)
	}
}

func TestPodRowFor_BuildsEveryRowFieldAndAssembleRoutesThroughIt(t *testing.T) {
	// Deliberately far from any wall clock this test will run under: if
	// PodRowFor ignored its now parameter and called time.Now(), Age would be
	// hundreds of days rather than three, and the assertion below would say so.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	p := pod("shop", "cart-0", nil, 4, "registry.example.com/cart:1.2.3")
	p.CreationTimestamp = metav1.NewTime(now.Add(-72 * time.Hour))
	p.Spec.NodeName = "node-a"
	p.Status.PodIP = "192.0.2.10"
	p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{
			FinishedAt: metav1.NewTime(now.Add(-1 * time.Hour)),
		},
	}

	// Every field spelled out rather than derived from Assemble. Assemble
	// delegates to PodRowFor, so comparing the two would compare the function
	// against itself and pass however wrong its body became.
	want := PodRow{
		Name:  "cart-0",
		Phase: "Running",
		// The helper's container is neither waiting nor terminated, so the
		// display rule has nothing to say and falls through to the phase.
		State:       "Running",
		Ready:       "0/1", // the pod helper's one container status is Ready: false
		Restarts:    4,
		LastRestart: "2026-01-01T11:00:00Z", // now - 1h, RFC3339 UTC
		Node:        "node-a",
		IP:          "192.0.2.10",
		Age:         "3d", // now - 72h
		Image:       "registry.example.com/cart:1.2.3",
	}

	if got := PodRowFor(p, now); got != want {
		t.Errorf("PodRowFor() = %+v\nwant            = %+v", got, want)
	}

	// Assemble must still route through it: a later edit that re-inlines the row
	// literal would otherwise drift from PodRowFor silently. Age is the one field
	// that cannot match — Assemble stamps it from the wall clock, not this test's
	// fixed one.
	ws := Assemble(Inputs{Pods: []corev1.Pod{p}}, nil)
	if len(ws) != 1 || len(ws[0].Pods) != 1 {
		t.Fatalf("Assemble() = %+v, want one workload carrying one pod row", ws)
	}
	wantRow, viaAssemble := want, ws[0].Pods[0]
	wantRow.Age, viaAssemble.Age = "", ""
	if wantRow != viaAssemble {
		t.Errorf("Assemble's row = %+v\nwant           = %+v", viaAssemble, wantRow)
	}
}

// waiting sets a container status's waiting reason on the pod's main container,
// replacing whatever the pod helper left there.
func waiting(p corev1.Pod, reason string) corev1.Pod {
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: reason},
	}
	return p
}

// initStatus appends an init container and its status to a pod. The container
// list matters as well as the status list: a pod row's ready count is over
// spec.containers, and the init containers must not land there.
func initStatus(p corev1.Pod, name string, st corev1.ContainerState) corev1.Pod {
	p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Name: name})
	p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses,
		corev1.ContainerStatus{Name: name, State: st})
	return p
}

// TestPodRowFor_State covers the whole of the display rule, arm by arm, in the
// order the rule applies them. The phase is asserted alongside every case: the
// point of the field is that it sits *beside* status.phase rather than replacing
// it, so a case where the two agree proves as much as one where they differ.
func TestPodRowFor_State(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	base := func() corev1.Pod { return pod("shop", "cart-0", nil, 0, "registry.example.com/cart:1") }

	deleting := waiting(base(), "CrashLoopBackOff")
	del := metav1.NewTime(now.Add(-time.Minute))
	deleting.DeletionTimestamp = &del

	// An init container still running while a later one waits: the rule reads the
	// first init container that is *waiting with a reason* or *terminated
	// non-zero*, not simply the first init container.
	initSkipsRunning := initStatus(initStatus(base(), "warm",
		corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}),
		"migrate", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}})

	initTerminatedZero := initStatus(waiting(base(), "PodInitializing"), "migrate",
		corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}})

	tests := []struct {
		name      string
		p         corev1.Pod
		wantState string
		wantPhase string
	}{
		{
			name:      "a pod being deleted is Terminating whatever its containers say",
			p:         deleting,
			wantState: "Terminating",
			wantPhase: "Running",
		},
		{
			name: "an init container waiting with a reason wins over a main container's",
			p: initStatus(waiting(base(), "CrashLoopBackOff"), "migrate",
				corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}),
			wantState: "Init:ImagePullBackOff",
			wantPhase: "Running",
		},
		{
			name:      "a running init container is skipped, a later waiting one is not",
			p:         initSkipsRunning,
			wantState: "Init:ImagePullBackOff",
			wantPhase: "Running",
		},
		{
			name: "an init container that exited non-zero names its reason",
			p: initStatus(base(), "migrate",
				corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}),
			wantState: "Init:Error",
			wantPhase: "Running",
		},
		{
			name:      "an init container that exited zero is not a state at all",
			p:         initTerminatedZero,
			wantState: "PodInitializing",
			wantPhase: "Running",
		},
		{
			name:      "a main container waiting with a reason",
			p:         waiting(base(), "CrashLoopBackOff"),
			wantState: "CrashLoopBackOff",
			wantPhase: "Running",
		},
		{
			name: "a main container terminated with a reason, whatever its exit code",
			p: func() corev1.Pod {
				p := base()
				p.Status.Phase = corev1.PodSucceeded
				p.Status.ContainerStatuses[0].State = corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"},
				}
				return p
			}(),
			wantState: "Completed",
			wantPhase: "Succeeded",
		},
		{
			name:      "nothing to say — the state is the phase, which is today's behaviour",
			p:         base(),
			wantState: "Running",
			wantPhase: "Running",
		},
		{
			name: "a waiting container with no reason falls through rather than saying nothing",
			p: func() corev1.Pod {
				p := base()
				p.Status.Phase = corev1.PodPending
				p.Status.ContainerStatuses[0].State = corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{},
				}
				return p
			}(),
			wantState: "Pending",
			wantPhase: "Pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PodRowFor(tt.p, now)
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}
			if got.Phase != tt.wantPhase {
				t.Errorf("Phase = %q, want %q — the raw phase must be left alone", got.Phase, tt.wantPhase)
			}
		})
	}
}

// A container's waiting and terminated reasons are API text kubeagent does not
// validate: the API server writes whatever the kubelet reported. The state is
// built from them and is rendered into a report and carried in scan's JSON
// document, so PodRowFor is the point at which they become kubeagent values.
func TestPodRowFor_SanitizesTheStateReason(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	main := waiting(pod("shop", "cart-0", nil, 0, "img"), "Crash\x1b[2JLoop\x00\xffBack‮Off")
	if got := PodRowFor(main, now).State; !safe(got) {
		t.Errorf("unsanitized main-container reason reached the state: %q", got)
	}

	init := initStatus(pod("shop", "cart-1", nil, 0, "img"), "migrate",
		corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "Image\x1b[2JPull\x00\xffBack‮Off"}})
	got := PodRowFor(init, now).State
	if !safe(got) {
		t.Errorf("unsanitized init-container reason reached the state: %q", got)
	}
	if !strings.HasPrefix(got, "Init:") {
		t.Errorf("State = %q, want the Init: prefix kept", got)
	}
}

// safe reports whether s carries nothing safetext.Line removes. Deliberately not
// "s == safetext.Line(raw)": the point is what reaches the terminal, not which
// helper produced it.
func safe(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func TestWorkloadTraceFieldsOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(Workload{Namespace: "shop", Name: "api", Kind: "Deployment"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"rootCauseTrace", "rootCauseConfidence"} {
		if strings.Contains(string(b), key) {
			t.Errorf("workload with no trace encodes %q: %s", key, b)
		}
	}
}

func TestWorkloadTraceRoundTrips(t *testing.T) {
	want := Workload{
		Namespace: "shop", Name: "api", Kind: "Deployment",
		RootCauseTrace: []Hypothesis{{
			Cause:   "node worker-2 (NotReady)",
			Kind:    "node",
			Verdict: VerdictAttributed,
			Reason:  "pod api-a is scheduled on it",
		}},
		RootCauseConfidence: "high",
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Workload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.RootCauseTrace) != 1 || got.RootCauseTrace[0] != want.RootCauseTrace[0] {
		t.Errorf("trace = %+v, want %+v", got.RootCauseTrace, want.RootCauseTrace)
	}
	if got.RootCauseConfidence != "high" {
		t.Errorf("rootCauseConfidence = %q, want high", got.RootCauseConfidence)
	}
}
