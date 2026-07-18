package scan

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
