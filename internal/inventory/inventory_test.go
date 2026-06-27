package inventory

import (
	"testing"
	"time"

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
		if got := humanAge(c.t, now); got != c.want {
			t.Errorf("%s: humanAge = %q, want %q", c.name, got, c.want)
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
	if workloadStatus(3, 3) != "Running" {
		t.Error("3/3 should be Running")
	}
	if workloadStatus(1, 2) != "Degraded" {
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
	if workloadStatus(0, 0) != "Scaled Down" {
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
