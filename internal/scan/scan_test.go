package scan

import (
	"context"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	intstr "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/secscan"
)

func TestEvaluate_HealthyClusterNoFlags(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	cli := fake.NewSimpleClientset(node)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Health.Verdict != "Healthy" {
		t.Errorf("want Healthy, got %q", res.Health.Verdict)
	}
	if got := len(res.Inventory.Workloads); got != 0 {
		t.Errorf("want no workloads, got %d", got)
	}
}

func TestEvaluate_FlagsCrashLoopingWorkload(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1",
		Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "web", Ready: false, RestartCount: 8,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}}
	cli := fake.NewSimpleClientset(node, dep, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "CrashLoopBackOff" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a CrashLoopBackOff finding, got %+v", res.Inventory.Workloads)
	}
}

func TestEvaluate_FlagsVolumeAttachError(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-0"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "db",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}},
		},
	}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "db-0.ev"},
		Reason:         "FailedAttachVolume",
		Type:           "Warning",
		Message:        `Multi-Attach error for volume "pvc-9"`,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "db-0"},
	}
	cli := fake.NewSimpleClientset(node, pod, ev)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "VolumeAttachError" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a VolumeAttachError finding, got %+v", res.Inventory.Workloads)
	}
}

func TestEvaluate_FlagsRestartLoop(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	now := time.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "flapper"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", RestartCount: 4,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-20 * time.Second))}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(now.Add(-25 * time.Second)),
				}},
			}},
		},
	}
	cli := fake.NewSimpleClientset(node, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "RestartLoop" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a RestartLoop finding, got %+v", res.Inventory.Workloads)
	}
}

func TestEvaluate_DiskUsageOffByDefault(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
	)
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.DiskUsage.Over) != 0 || len(res.DiskUsage.Nodes) != 0 {
		t.Errorf("disk usage must be empty when not enabled, got %+v", res.DiskUsage)
	}
}

func TestEvaluate_StaleHeartbeatDegrades(t *testing.T) {
	now := time.Now()
	rt := metav1.NewMicroTime(now.Add(-2 * time.Minute))
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-node-lease", Name: "w1"}, Spec: coordinationv1.LeaseSpec{RenewTime: &rt}},
	)
	res, err := Evaluate(context.Background(), client, Options{NodeHeartbeatThreshold: 40 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Health.Verdict != "Degraded" || res.Health.NodesStaleHeartbeat != 1 {
		t.Errorf("a Ready node with a stale lease must degrade the verdict: %+v", res.Health)
	}

	// Threshold 0 disables the check -> same cluster reads Healthy.
	off, _ := Evaluate(context.Background(), client, Options{})
	if off.Health.NodesStaleHeartbeat != 0 {
		t.Errorf("threshold 0 must disable the heartbeat check: %+v", off.Health)
	}
}

func TestEvaluate_ExpectedNodeAbsentDegrades(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
	)
	res, err := Evaluate(context.Background(), client, Options{ExpectedNodes: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Health.Verdict != "Degraded" || res.Health.NodesExpectedAbsent != 1 {
		t.Errorf("declared node b absent must degrade the verdict: %+v", res.Health)
	}

	off, _ := Evaluate(context.Background(), client, Options{})
	if off.Health.NodesExpectedAbsent != 0 {
		t.Errorf("no expected list must leave the count 0: %+v", off.Health)
	}
}

func TestEvaluate_LogsEnrichCrashFindings(t *testing.T) {
	crashPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop", Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "web", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	client := fake.NewSimpleClientset(crashPod)
	on, err := Evaluate(context.Background(), client, Options{Logs: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := findLogCause(on, "shop/web-1"); got == "" {
		t.Errorf("with --logs a crash finding should carry a LogCause, got none:\n%+v", on.Inventory.Workloads)
	}
	// Opt-out: no enrichment.
	off, _ := Evaluate(context.Background(), client, Options{})
	if got := findLogCause(off, "shop/web-1"); got != "" {
		t.Errorf("without --logs no LogCause, got %q", got)
	}
}

// TestEvaluate_LogsDedupPerContainer guards against enriching the same container
// twice. A container in CrashLoopBackOff whose last exit was OOMKilled fires BOTH
// the CrashLoop and OOMKilled detectors — two findings for one container. --logs
// must fetch+classify its previous logs once and enrich a single finding, so the
// report shows the "logs (previous container)" block once, not twice.
func TestEvaluate_LogsDedupPerContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop", Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:                 "web",
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
		}}},
	}
	client := fake.NewSimpleClientset(pod)
	on, err := Evaluate(context.Background(), client, Options{Logs: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := countLogCause(on, "shop/web-1"); n != 1 {
		t.Errorf("crashloop+OOM on one container should enrich exactly one finding, got %d", n)
	}
}

// findLogCause returns the first finding's LogCause for the given "ns/pod".
func findLogCause(r Result, pod string) string {
	for _, w := range r.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Pod == pod && f.LogCause != "" {
				return f.LogCause
			}
		}
	}
	return ""
}

// countLogCause counts findings carrying a LogCause for the given "ns/pod".
func countLogCause(r Result, pod string) int {
	n := 0
	for _, w := range r.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Pod == pod && f.LogCause != "" {
				n++
			}
		}
	}
	return n
}

func p32(i int32) *int32 { return &i }

func boolp(b bool) *bool { return &b }

func privPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", SecurityContext: &corev1.SecurityContext{Privileged: boolp(true)}}}},
	}
}

func nsCount(fs []secscan.Finding, ns string) int {
	n := 0
	for _, f := range fs {
		if f.Namespace == ns {
			n++
		}
	}
	return n
}

func TestEvaluate_SecurityOptInAndSystemExclusion(t *testing.T) {
	client := fake.NewSimpleClientset(privPod("default", "app"), privPod("kube-system", "cni"))

	// Flag off: no security findings at all.
	off, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(off.SecurityIssues) != 0 {
		t.Errorf("without Security, expected no findings, got %+v", off.SecurityIssues)
	}

	// All namespaces: kube-system excluded, default kept.
	all, err := Evaluate(context.Background(), client, Options{Security: true})
	if err != nil {
		t.Fatal(err)
	}
	if nsCount(all.SecurityIssues, "kube-system") != 0 {
		t.Errorf("kube-system must be excluded in all-namespaces mode, got %+v", all.SecurityIssues)
	}
	if nsCount(all.SecurityIssues, "default") == 0 {
		t.Errorf("default namespace privileged pod must be flagged, got %+v", all.SecurityIssues)
	}

	// Explicit -n kube-system: included.
	sys, err := Evaluate(context.Background(), client, Options{Security: true, Namespace: "kube-system"})
	if err != nil {
		t.Fatal(err)
	}
	if nsCount(sys.SecurityIssues, "kube-system") == 0 {
		t.Errorf("explicit -n kube-system must include it, got %+v", sys.SecurityIssues)
	}

	// Advisory: security findings never flip the verdict.
	if all.Health.Verdict != off.Health.Verdict {
		t.Errorf("security must not change the verdict (%q vs %q)", all.Health.Verdict, off.Health.Verdict)
	}
}

func TestEvaluate_FlagsProbeFailure(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1", Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "web",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "web-1.ev"},
		Reason:         "Unhealthy",
		Type:           "Warning",
		Message:        "Readiness probe failed: HTTP probe failed with statuscode: 503",
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "web-1", FieldPath: "spec.containers{web}"},
	}
	cli := fake.NewSimpleClientset(node, pod, ev)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "ProbeFailure" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a ProbeFailure finding, got %+v", res.Inventory.Workloads)
	}
}

func TestEvaluate_FlagsInitContainerFailure(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "orders-1", Labels: map[string]string{"app": "orders"}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "wait-for-db", RestartCount: 6,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
			}},
		},
	}
	cli := fake.NewSimpleClientset(node, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	var initFindings, crashFindings int
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			switch f.Issue {
			case "Init:CrashLoopBackOff":
				initFindings++
			case "CrashLoopBackOff":
				crashFindings++
			}
		}
	}
	if initFindings != 1 {
		t.Errorf("expected exactly 1 Init:CrashLoopBackOff finding, got %d (%+v)", initFindings, res.Inventory.Workloads)
	}
	if crashFindings != 0 {
		t.Errorf("main-container CrashLoopBackOff must not fire for an init-blocked pod, got %d", crashFindings)
	}
}

func TestEvaluate_FlagsPendingPVC(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	sc := "fast"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data-pvc"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "data-pvc.ev"},
		Reason:         "ProvisioningFailed",
		Type:           "Warning",
		Message:        `storageclass "fast" not found`,
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data-pvc"},
	}
	cli := fake.NewSimpleClientset(node, pvc, ev)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.PVCIssues) != 1 || res.PVCIssues[0].Name != "data-pvc" {
		t.Errorf("expected 1 PVCIssue for data-pvc, got %+v", res.PVCIssues)
	}
}

func TestEvaluate_FlagsFailedJob(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-migrate"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"},
		}},
	}
	cli := fake.NewSimpleClientset(node, job)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "JobFailed" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a JobFailed finding, got %+v", res.Inventory.Workloads)
	}
}

func TestEvaluate_FlagsFailedCreate(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(3)}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-7c9f",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", Controller: boolp(true)}}}}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "api-7c9f.ev"},
		Reason:         "FailedCreate",
		Type:           "Warning",
		Message:        `pods "api-7c9f-" is forbidden: exceeded quota: compute`,
		InvolvedObject: corev1.ObjectReference{Kind: "ReplicaSet", Namespace: "shop", Name: "api-7c9f"},
	}
	cli := fake.NewSimpleClientset(node, dep, rs, ev)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "FailedCreate" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a FailedCreate finding, got %+v", res.Inventory.Workloads)
	}
}

func TestEvaluate_AttributesRootCauseToNotReadyNode(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady"}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-1", Labels: map[string]string{"app": "api"}},
		Spec:   corev1.PodSpec{NodeName: "worker-2"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "api", Ready: false}}}}
	cli := fake.NewSimpleClientset(node, dep, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		if w.RootCause == "node worker-2 (NotReady)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a workload attributed to node worker-2, got %+v", res.Inventory.Workloads)
	}
}

func TestEvaluate_AttributesSharedRegistryFailure(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	depA := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "frontend"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	depB := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "search"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "frontend-1",
		Labels: map[string]string{"app": "frontend"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "frontend", Image: "ghcr.io/shop/frontend:2.4"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "frontend", Ready: false, Image: "ghcr.io/shop/frontend:2.4",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}
	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "search-1",
		Labels: map[string]string{"app": "search"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "search", Image: "ghcr.io/shop/search:1.9"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "search", Ready: false, Image: "ghcr.io/shop/search:1.9",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}
	cli := fake.NewSimpleClientset(node, depA, depB, podA, podB)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	attributed := 0
	for _, w := range res.Inventory.Workloads {
		if w.RootCause == "registry ghcr.io (2 workloads failing to pull)" {
			attributed++
		}
	}
	if attributed != 2 {
		t.Errorf("want both workloads attributed to registry ghcr.io, got %d: %+v", attributed, res.Inventory.Workloads)
	}
}

// TestEvaluate_NodeAttributionWinsOverRegistry guards the ordering of rootcause.Annotate
// (node) before rootcause.AnnotateRegistry in scan.Evaluate. It fails if someone swaps
// those two calls: the node-attributed workload would instead receive the registry string,
// and the remaining singleton group would still (incorrectly) get attributed.
func TestEvaluate_NodeAttributionWinsOverRegistry(t *testing.T) {
	// worker-2 is NotReady.
	notReadyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-2"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady"},
		}},
	}
	// Two Deployments both failing ImagePullBackOff from ghcr.io; ReplicaSets chain
	// pods back to their owning Deployment (required for inventory roll-up).
	depA := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	rsA := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-rs",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", Controller: boolp(true)}}}}
	depB := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "worker"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	rsB := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "worker-rs",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "worker", Controller: boolp(true)}}}}

	// podA is placed on the NotReady node.
	podA := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-1",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-rs", Controller: boolp(true)}}},
		Spec: corev1.PodSpec{
			NodeName:   "worker-2",
			Containers: []corev1.Container{{Name: "api", Image: "ghcr.io/shop/api:1.0"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "api", Ready: false, Image: "ghcr.io/shop/api:1.0",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}},
	}
	// podB is on a healthy (or unscheduled) node.
	podB := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "worker-1",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "worker-rs", Controller: boolp(true)}}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "worker", Image: "ghcr.io/shop/worker:2.0"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "worker", Ready: false, Image: "ghcr.io/shop/worker:2.0",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}},
	}
	cli := fake.NewSimpleClientset(notReadyNode, depA, rsA, depB, rsB, podA, podB)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}

	causeByName := map[string]string{}
	for _, w := range res.Inventory.Workloads {
		causeByName[w.Name] = w.RootCause
	}

	// api pod is on worker-2 (NotReady) → node attribution must win.
	if causeByName["api"] != "node worker-2 (NotReady)" {
		t.Errorf("api workload: want RootCause=%q, got %q", "node worker-2 (NotReady)", causeByName["api"])
	}
	// worker pod's registry group shrank to 1 after api was excluded → no registry attribution.
	if causeByName["worker"] != "" {
		t.Errorf("worker workload: want RootCause=%q (singleton group), got %q", "", causeByName["worker"])
	}
}

func TestEvaluate_AttributesRootCauseToBrokenPVC(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	sc := "fast"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "reports-data"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "reports-data.ev"},
		Reason:         "ProvisioningFailed",
		Type:           "Warning",
		Message:        `storageclass "fast" not found`,
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "reports-data"},
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "reports"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "reports-1",
		Labels: map[string]string{"app": "reports"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "reports", Image: "busybox:1.36"}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "reports-data"}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "reports", Ready: false,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}}}
	// SC "fast" exists so the structural MissingStorageClass path is bypassed;
	// the ProvisioningFailed event surfaces as the root cause instead.
	fastSC := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast"}}
	cli := fake.NewSimpleClientset(node, pvc, ev, dep, pod, fastSC)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		if w.RootCause == "PVC reports-data (ProvisioningFailed)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a workload attributed to PVC reports-data, got %+v", res.Inventory.Workloads)
	}
}

func TestEvaluate_CertsOffMakesNoSecretsCall(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	cli := fake.NewSimpleClientset(node)
	if _, err := Evaluate(context.Background(), cli, Options{}); err != nil {
		t.Fatal(err)
	}
	for _, a := range cli.Actions() {
		if a.GetResource().Resource == "secrets" {
			t.Fatalf("default scan must not touch secrets, saw action %+v", a)
		}
	}
}

func TestEvaluate_CertsOnAssessesTLSSecrets(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "bad-tls"},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("not a certificate")}}
	cli := fake.NewSimpleClientset(node, bad)
	res, err := Evaluate(context.Background(), cli, Options{Certs: true, CertWarnDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if res.Certificates == nil || res.Certificates.Checked != 1 || len(res.Certificates.Invalid) != 1 {
		t.Errorf("want Certificates with 1 checked / 1 invalid, got %+v", res.Certificates)
	}
}

func TestEvaluate_CertsForbiddenGraceful(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	cli := fake.NewSimpleClientset(node)
	cli.Fake.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", nil)
	})
	res, err := Evaluate(context.Background(), cli, Options{Certs: true, CertWarnDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if res.Certificates == nil || !res.Certificates.Forbidden {
		t.Errorf("forbidden secrets list must set Certificates.Forbidden, got %+v", res.Certificates)
	}
}

func TestEvaluate_StampsFindingConfidence(t *testing.T) {
	now := time.Now()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "cache-1", Labels: map[string]string{"app": "cache"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "cache", Ready: true, RestartCount: 5,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-20 * time.Second))}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(now.Add(-25 * time.Second))}}}}}}
	cli := fake.NewSimpleClientset(node, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "RestartLoop" {
				found = true
				if f.Confidence != "medium" {
					t.Errorf("RestartLoop confidence = %q, want medium", f.Confidence)
				}
			}
		}
	}
	if !found {
		t.Errorf("expected a RestartLoop finding, got %+v", res.Inventory.Workloads)
	}
}

func TestEvaluate_FlagsStuckTerminatingNamespace(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dt := metav1.NewTime(time.Now().Add(-3 * time.Hour))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "legacy-ns", DeletionTimestamp: &dt},
		Status: corev1.NamespaceStatus{Conditions: []corev1.NamespaceCondition{
			{Type: "NamespaceFinalizersRemaining", Status: corev1.ConditionTrue, Message: "finalizers remaining: kubernetes"}}}}
	cli := fake.NewSimpleClientset(node, ns)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range res.StuckTerminating {
		if is.Kind == "Namespace" && is.Name == "legacy-ns" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a stuck-terminating namespace, got %+v", res.StuckTerminating)
	}
}

func TestEvaluate_ForbiddenNamespacesStillScansPods(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "stuck", DeletionTimestamp: &dt,
		Finalizers: []string{"example.com/hook"}}}
	cli := fake.NewSimpleClientset(node, pod)
	cli.Fake.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", nil)
	})
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("a forbidden namespaces list must not fail the scan: %v", err)
	}
	found := false
	for _, is := range res.StuckTerminating {
		if is.Kind == "Pod" && is.Name == "stuck" {
			found = true
		}
	}
	if !found {
		t.Errorf("pod checks must still run when namespaces is forbidden, got %+v", res.StuckTerminating)
	}
}

func TestEvaluate_FlagsUnsatisfiablePDB(t *testing.T) {
	m := intstr.FromInt(3)
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"},
		Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: &m},
		Status:     policyv1.PodDisruptionBudgetStatus{ExpectedPods: 3, DesiredHealthy: 3, CurrentHealthy: 3, DisruptionsAllowed: 0},
	}
	cli := fake.NewSimpleClientset(pdb)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.PDBIssues) != 1 || res.PDBIssues[0].Category != "unsatisfiable" {
		t.Fatalf("expected one unsatisfiable PDB issue, got %+v", res.PDBIssues)
	}
}

func TestEvaluate_ForbiddenPDBsStillScans(t *testing.T) {
	cli := fake.NewSimpleClientset()
	cli.Fake.PrependReactor("list", "poddisruptionbudgets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "policy", Resource: "poddisruptionbudgets"}, "", nil)
	})
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("a forbidden PDB list must not error, got %v", err)
	}
	if len(res.PDBIssues) != 0 {
		t.Fatalf("forbidden PDB list must yield no issues, got %+v", res.PDBIssues)
	}
}

func TestEvaluate_FlagsStuckHPA(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-hpa"},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}, MaxReplicas: 5},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
			{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse, Reason: "FailedGetResourceMetric", Message: "no metrics"}}},
	}
	cli := fake.NewSimpleClientset(hpa)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.HPAIssues) != 1 || res.HPAIssues[0].Category != "metrics" {
		t.Fatalf("expected one metrics HPA issue, got %+v", res.HPAIssues)
	}
}

func TestEvaluate_ForbiddenHPAsStillScans(t *testing.T) {
	cli := fake.NewSimpleClientset()
	cli.Fake.PrependReactor("list", "horizontalpodautoscalers", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"}, "", nil)
	})
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("a forbidden HPA list must not error, got %v", err)
	}
	if len(res.HPAIssues) != 0 {
		t.Fatalf("forbidden HPA list must yield no issues, got %+v", res.HPAIssues)
	}
}

// downWebhookObjects builds a Fail validating webhook whose backend Service exists but
// has no ready endpoints, plus that Service and a not-ready EndpointSlice.
func downWebhookObjects() []runtime.Object {
	fail := admissionv1.Fail
	notReady := false
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "policy-webhook"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:          "validate.policy.io",
			FailurePolicy: &fail,
			ClientConfig:  admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{Namespace: "kube-system", Name: "policy-svc"}},
		}},
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "policy-svc"}}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "policy-svc-x", Labels: map[string]string{discoveryv1.LabelServiceName: "policy-svc"}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}}},
	}
	return []runtime.Object{vwc, svc, slice}
}

func TestEvaluate_FlagsDownWebhook(t *testing.T) {
	cli := fake.NewSimpleClientset(downWebhookObjects()...)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.WebhookIssues) != 1 || res.WebhookIssues[0].Problem != "no-endpoints" {
		t.Fatalf("expected one no-endpoints webhook issue, got %+v", res.WebhookIssues)
	}
}

func TestEvaluate_WebhookCheckSkippedWhenNamespaceScoped(t *testing.T) {
	cli := fake.NewSimpleClientset(downWebhookObjects()...)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "kube-system"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.WebhookIssues) != 0 {
		t.Fatalf("the webhook check must be skipped under --namespace, got %+v", res.WebhookIssues)
	}
}

func TestEvaluate_ForbiddenWebhooksStillScans(t *testing.T) {
	cli := fake.NewSimpleClientset()
	cli.Fake.PrependReactor("list", "validatingwebhookconfigurations", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "admissionregistration.k8s.io", Resource: "validatingwebhookconfigurations"}, "", nil)
	})
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("a forbidden webhook list must not error, got %v", err)
	}
	if len(res.WebhookIssues) != 0 {
		t.Fatalf("forbidden webhook list must yield no issues, got %+v", res.WebhookIssues)
	}
}

func TestEvaluate_ServiceNoEndpointsRootCause(t *testing.T) {
	// A selector-based Service with no matching pods and no endpoints → the
	// service issue's Detail is enriched with the no-pods cause.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"app": "web"}},
	}
	cli := fake.NewSimpleClientset(svc)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, is := range res.ServiceIssues {
		if is.Namespace == "shop" && is.Name == "web" {
			found = true
			if is.Detail != "no ready endpoints — the selector matches no pods" {
				t.Fatalf("detail = %q", is.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("expected a shop/web service issue, got %+v", res.ServiceIssues)
	}
}

func TestEvaluate_KubeletHealthOffByDefault(t *testing.T) {
	// Mirrors TestEvaluate_DiskUsageOffByDefault: the fake clientset's
	// RESTClient() is nil, so the nodes/proxy probe cannot be exercised through
	// it (the same reason disk-usage only tests its off path here). The probe's
	// classification is unit-tested directly in collect (TestClassifyKubeletHealthz);
	// this test pins the opt-out gate — without --kubelet-health, no node is probed.
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
	)
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.KubeletHealth.Probed != 0 || len(res.KubeletHealth.Unhealthy) != 0 {
		t.Errorf("kubelet health must be empty when not enabled, got %+v", res.KubeletHealth)
	}
}

func TestEvaluate_PVCMissingStorageClass_NoEvent(t *testing.T) {
	// A Pending PVC referencing a StorageClass that does not exist, with NO event,
	// is flagged structurally (proves the wiring passes StorageClasses + PVs and
	// that flagging no longer requires an event).
	sc := "fast-ssd"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &sc},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	cli := fake.NewSimpleClientset(pvc)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, is := range res.PVCIssues {
		if is.Namespace == "shop" && is.Name == "data" {
			found = true
			if is.Reason != "MissingStorageClass" || is.Detail != `references StorageClass "fast-ssd" which does not exist` {
				t.Fatalf("issue = %+v", is)
			}
		}
	}
	if !found {
		t.Fatalf("expected a shop/data PVC issue, got %+v", res.PVCIssues)
	}
}

func TestEvaluate_IngressRouteRootCause(t *testing.T) {
	// A broken ingress route whose backend Service selector matches no pods →
	// the route Detail is enriched with the no-pods cause.
	svcObj := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "web"}, Ports: []corev1.ServicePort{{Port: 80}}},
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-ing"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "web.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: "/",
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80},
					}},
				}},
			}},
		}}},
	}
	cli := fake.NewSimpleClientset(svcObj, ing)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, r := range res.IngressIssues {
		if r.Namespace == "shop" && r.Ingress == "web-ing" {
			found = true
			if r.Detail != "backend Service web:80 has no ready endpoints (likely 502/503) — the selector matches no pods" {
				t.Fatalf("detail = %q", r.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("expected a shop/web-ing route issue, got %+v", res.IngressIssues)
	}
}
