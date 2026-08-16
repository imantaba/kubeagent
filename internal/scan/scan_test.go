package scan

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	intstr "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
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

// honourEventFieldSelectors makes a fake clientset filter event lists by the
// field selector the caller passed, the way a real API server does. client-go's
// fake ignores field selectors entirely, so by default every event lister sees
// every event in the namespace — which means a test cannot tell a read that
// asks for reason=X from one that was never wired up at all.
//
// The events live in this reactor rather than in the object tracker: the
// reactor is prepended, so it answers every event list before the tracker is
// consulted, and giving it the only copy keeps the two from disagreeing.
func honourEventFieldSelectors(cli *fake.Clientset, events []corev1.Event) {
	cli.PrependReactor("list", "events", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sel := action.(k8stesting.ListActionImpl).GetListRestrictions().Fields
		out := &corev1.EventList{}
		for _, e := range events {
			set := fields.Set{
				"reason":              e.Reason,
				"involvedObject.kind": e.InvolvedObject.Kind,
				"involvedObject.name": e.InvolvedObject.Name,
			}
			if sel == nil || sel.Matches(set) {
				out.Items = append(out.Items, e)
			}
		}
		return true, out, nil
	})
}

// TestEvaluate_FlagsVolumeMountError is the regression fixture for a detector
// that could not fire in any real run: VolumeMountDetector matches FailedMount
// events, and no read ever handed it one. Every surface reaches diagnosis
// through Evaluate, so this test at this layer is what proves the wiring — a
// unit test that hand-builds PodFacts cannot see the gap.
//
// The second, unrelated event is not decoration: client-go's fake clientset
// ignores field selectors, so it is what proves the client-side repeat in
// collect.FailedMountEvents actually filters.
//
// honourEventFieldSelectors is what makes this test mean anything at all.
// Without it the fake hands every event to every event lister, so a FailedMount
// event reaches diagnosis through the FailedAttachVolume read and the test is
// green while the product is broken — which is precisely how this defect
// survived.
func TestEvaluate_FlagsVolumeMountError(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-0"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "api",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}},
		},
	}
	mount := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "api-0.mount"},
		Reason:         "FailedMount",
		Type:           "Warning",
		Message:        `MountVolume.SetUp failed for volume "settings" : configmap "settings" not found`,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "api-0"},
	}
	other := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "api-0.sched"},
		Reason:         "Scheduled",
		Type:           "Normal",
		Message:        "Successfully assigned shop/api-0 to n1",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "api-0"},
	}
	cli := fake.NewSimpleClientset(node, pod)
	honourEventFieldSelectors(cli, []corev1.Event{*mount, *other})
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			got = append(got, f.Issue)
			if f.Issue == "VolumeMountError" && !strings.Contains(f.Evidence, `configmap "settings" not found`) {
				t.Errorf("evidence does not carry the kubelet message: %q", f.Evidence)
			}
		}
	}
	n := 0
	for _, issue := range got {
		if issue == "VolumeMountError" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want exactly 1 VolumeMountError finding, got %d (all findings: %v)", n, got)
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

// A refused Lease read must surface as a named blind spot and must not make
// kubeagent claim every Ready node's kubelet has stopped heartbeating — that
// would be reading a failed read as a fact about the cluster. Without the
// guard, a Ready node with no lease entry (because the whole list failed)
// would be misreported as "no kubelet lease" in Health.DownNodes, which is
// exactly the assertion internal/rootcause depends on staying empty here.
func TestEvaluate_LeasesForbiddenNoHeartbeatClaim(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	client := fake.NewSimpleClientset(node)
	client.Fake.PrependReactor("list", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "", nil)
	})

	res, err := Evaluate(context.Background(), client, Options{NodeHeartbeatThreshold: 40 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	var found *ReadFailure
	for i := range res.PartialReads {
		if res.PartialReads[i].Resource == "leases" {
			found = &res.PartialReads[i]
		}
	}
	if found == nil {
		t.Fatalf("PartialReads = %v, want an entry for leases", res.PartialReads)
	}
	if len(res.Health.DownNodes) != 0 {
		t.Errorf("Health.DownNodes = %+v, want none — a refused Lease read must not be read as every kubelet down", res.Health.DownNodes)
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

func TestEvaluate_ControlPlaneHealthOffByDefault(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
	)
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ControlPlane.Status != "" {
		t.Errorf("control plane must not be probed when disabled, got %+v", res.ControlPlane)
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

func TestEvaluate_ConfigErrorDetected(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-abc"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: `configmap "app-config" not found`}},
		}}},
	}
	cli := fake.NewSimpleClientset(pod)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "CreateContainerConfigError" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected a CreateContainerConfigError finding, got workloads %+v", res.Inventory.Workloads)
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

func TestEvaluate_FlagsStuckRollout(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"},
		Spec:       appsv1.DeploymentSpec{Replicas: p32(3)},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
				Message: `ReplicaSet "api-7f9c" has timed out progressing.`},
		}},
	}
	cli := fake.NewSimpleClientset(node, dep)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "RolloutStuck" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a RolloutStuck finding, got %+v", res.Inventory.Workloads)
	}
}

func TestEvaluate_DNSHealthOffByDefault(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
	)
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.DNS.Status != "" {
		t.Errorf("DNS must not be probed when disabled, got %+v", res.DNS)
	}
}

func TestEvaluate_FlagsSlowWebhook(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	url := "https://hook.example.com/validate"
	fail := admissionv1.Fail
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "slow-validator"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:           "policy.example.com",
			FailurePolicy:  &fail,
			ClientConfig:   admissionv1.WebhookClientConfig{URL: &url},
			TimeoutSeconds: p32(20),
		}},
	}
	cli := fake.NewSimpleClientset(node, vwc)
	res, err := Evaluate(context.Background(), cli, Options{}) // Namespace "" → webhook check runs; threshold 0 → 15
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range res.WebhookIssues {
		if is.Problem == "high-timeout" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a high-timeout webhook issue, got %+v", res.WebhookIssues)
	}
}

func TestEvaluate_FlagsNearFullQuota(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	rq := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "compute"},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{"pods": resource.MustParse("50")},
			Used: corev1.ResourceList{"pods": resource.MustParse("47")},
		},
	}
	cli := fake.NewSimpleClientset(node, rq)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop", QuotaThreshold: 0.90})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.QuotaIssues) != 1 || res.QuotaIssues[0].Severity != "near" || res.QuotaIssues[0].Resource != "pods" {
		t.Errorf("want one near pods quota issue, got %+v", res.QuotaIssues)
	}
}

func TestEvaluate_CensusCountsHealthyWorkloadsTheReportHides(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:   appsv1.DeploymentSpec{Replicas: p32(1)},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "web", Ready: true}}}}
	cli := fake.NewSimpleClientset(node, dep, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	// The report shows nothing — that is correct, and it is exactly why the SLI
	// must not count this list.
	if got := len(res.Inventory.Workloads); got != 0 {
		t.Fatalf("healthy cluster should display no workloads, got %+v", res.Inventory.Workloads)
	}
	if res.Inventory.Census.Total == 0 {
		t.Fatal("Census.Total = 0 on a cluster with a running Deployment: the SLI would record nothing and coverage would never leave zero")
	}
	if res.Inventory.Census.Good != res.Inventory.Census.Total {
		t.Errorf("Census = %+v, want Good == Total on a healthy cluster", res.Inventory.Census)
	}
}

func TestEvaluate_CensusDropsGoodWhenAWorkloadBreaks(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:   appsv1.DeploymentSpec{Replicas: p32(1)},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 0}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "web", Ready: false, RestartCount: 8,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}}
	cli := fake.NewSimpleClientset(node, dep, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Inventory.Census.Total == 0 {
		t.Fatal("Census.Total = 0, want the broken Deployment counted")
	}
	if res.Inventory.Census.Good != 0 {
		t.Errorf("Census.Good = %d, want 0: every workload here is broken — the Deployment by ReadyReplicas, the bare pod by its crash loop", res.Inventory.Census.Good)
	}
}

// TestEvaluate_CensusRealisticOwnershipCountsOneWorkload covers what the two
// tests above do not: an ordinary Deployment -> ReplicaSet -> Pod ownership
// chain, the shape almost every real cluster actually has. Both tests above
// give their pod no OwnerReferences and give the fake clientset no
// ReplicaSet, so the pod is assembled as its own bare Pod-kind workload
// alongside the Deployment (Census{Good:2, Total:2}) — that exercises the
// orphan-pod path, not this one. With a real ownership chain the pod rolls up
// into its Deployment instead of counting separately, so the census must be
// exactly one entry, not two. Unlike the relational assertions above
// (Good == Total, Good == 0), this uses an exact assertion: the point of this
// test is the count itself.
func TestEvaluate_CensusRealisticOwnershipCountsOneWorkload(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:   appsv1.DeploymentSpec{Replicas: p32(1)},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-7c9f",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", Controller: boolp(true)}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-7c9f-abcde",
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-7c9f", Controller: boolp(true)}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "web", Ready: true}}}}
	cli := fake.NewSimpleClientset(node, dep, rs, pod)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Inventory.Census.Total != 1 {
		t.Fatalf("Census.Total = %d, want 1: the pod should roll up into its Deployment via the ReplicaSet, not count as a separate workload", res.Inventory.Census.Total)
	}
	if res.Inventory.Census.Good != 1 {
		t.Errorf("Census.Good = %d, want 1", res.Inventory.Census.Good)
	}
}

func TestEvaluate_RecordsDeniedListsAsPartialReads(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("networkpolicies is forbidden: User cannot list resource")
	})

	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil (a denied optional list must degrade, not fail)", err)
	}

	var found *ReadFailure
	for i := range res.PartialReads {
		if res.PartialReads[i].Resource == "networkpolicies" {
			found = &res.PartialReads[i]
		}
	}
	if found == nil {
		t.Fatalf("PartialReads = %v, want an entry for networkpolicies", res.PartialReads)
	}
	if found.Reason == "" {
		t.Error("PartialReads entry has an empty Reason; a caller cannot tell why the read failed")
	}
}

func TestEvaluate_CleanClusterHasNoPartialReads(t *testing.T) {
	client := fake.NewSimpleClientset()

	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	if len(res.PartialReads) != 0 {
		t.Errorf("PartialReads = %v, want none on a cluster that answered every list", res.PartialReads)
	}
}

// Reason strings must contain "forbidden" or internal/htmlreport.safeReason
// degrades them to a generic phrase.
func TestBlindSpotReasonsAreClassifiable(t *testing.T) {
	for _, r := range []string{
		blindReason("get nodes/proxy"),
		blindReason("get pods/log"),
		blindReason("list secrets"),
		blindReason("get pods/proxy"),
		blindReason("get /readyz"),
	} {
		if !strings.Contains(r, "forbidden") {
			t.Errorf("reason %q lacks the substring \"forbidden\"; the HTML report will not classify it", r)
		}
	}
}

// A forbidden --certs read must surface as a named blind spot, not only as a
// flag inside the certificate report.
func TestForbiddenCertsRecordsABlindSpot(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("no access"))
	})
	res, err := Evaluate(context.Background(), client, Options{Certs: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range res.PartialReads {
		if p.Resource == "secrets" && strings.Contains(p.Reason, "forbidden") {
			found = true
		}
	}
	if !found {
		t.Errorf("PartialReads = %+v, want a forbidden entry for secrets", res.PartialReads)
	}
}

// A 401 on list secrets must be treated the same as a 403: certs' forbidden
// branch checked only IsForbidden, so an Unauthorized response fell through to
// the note() branch instead of being recorded as a named blind spot.
func TestUnauthorizedCertsRecordsABlindSpot(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewUnauthorized("no access")
	})
	res, err := Evaluate(context.Background(), client, Options{Certs: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Certificates == nil || !res.Certificates.Forbidden {
		t.Errorf("Certificates = %+v, want Forbidden=true on a 401 list secrets", res.Certificates)
	}
	found := false
	for _, p := range res.PartialReads {
		if p.Resource == "secrets" && strings.Contains(p.Reason, "forbidden") {
			found = true
		}
	}
	if !found {
		t.Errorf("PartialReads = %+v, want a forbidden entry for secrets on a 401", res.PartialReads)
	}
}

// nodeStatsFailingClient wraps a kubernetes.Interface so CoreV1().RESTClient()
// resolves to a real, working REST client instead of the fake clientset's own
// CoreV1().RESTClient(), which is hardcoded to return a nil *rest.RESTClient and
// panics the instant collect.NodeStats calls it — regardless of whether a reactor
// is registered. Every other typed client (Nodes, Pods, ...) still delegates to
// the wrapped fake, so List/Get calls keep seeing the fake's seeded objects and
// reactors; only the nodes/proxy read goes over a real HTTP round trip.
type nodeStatsFailingClient struct {
	kubernetes.Interface
	rest rest.Interface
}

func (c nodeStatsFailingClient) CoreV1() corev1client.CoreV1Interface {
	return nodeStatsFailingCoreV1{CoreV1Interface: c.Interface.CoreV1(), rest: c.rest}
}

type nodeStatsFailingCoreV1 struct {
	corev1client.CoreV1Interface
	rest rest.Interface
}

func (c nodeStatsFailingCoreV1) RESTClient() rest.Interface {
	return c.rest
}

// forbiddenNodeProxyServer starts an httptest server that answers every request
// with a genuine Forbidden Status object, so apierrors.IsForbidden(err) is true on
// the caller's end — the same as a real API server refusing a missing nodes/proxy
// grant.
func forbiddenNodeProxyServer(t *testing.T) rest.Interface {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"forbidden"}`))
	}))
	t.Cleanup(server.Close)
	real, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("building a client for the fake forbidden server: %v", err)
	}
	return real.CoreV1().RESTClient()
}

// One blind spot per feature, not one per node: a 200-node cluster must not
// print 200 identical lines.
func TestForbiddenDiskUsageRecordsOneBlindSpot(t *testing.T) {
	client := nodeStatsFailingClient{
		Interface: fake.NewSimpleClientset(
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}},
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-3"}},
		),
		rest: forbiddenNodeProxyServer(t),
	}
	res, err := Evaluate(context.Background(), client, Options{DiskUsage: true})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range res.PartialReads {
		if p.Resource == "nodes/proxy" {
			n++
		}
	}
	if n > 1 {
		t.Errorf("recorded %d nodes/proxy blind spots for 3 nodes, want at most 1", n)
	}
	if n == 0 {
		t.Error("recorded 0 nodes/proxy blind spots, want exactly 1 for a forbidden read")
	}
}

// readyzServer starts an httptest server that answers every request with the
// given status code and body, for driving controlplane.ParseReadyz's "ok" and
// "unhealthy" branches through a real HTTP round trip via ControlPlaneReadyz.
// Content-Type must be set explicitly: net/http sniffs a plain-text body and
// stamps "text/plain", and client-go's serializer negotiator has no decoder
// for that media type — on a non-2xx status that negotiation failure hits a
// client-go response path that returns without ever setting Result.statusCode,
// so the caller reads back code 0 ("unreachable") no matter what status this
// handler wrote.
func readyzServer(t *testing.T, code int, body string) rest.Interface {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	real, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("building a client for the fake readyz server: %v", err)
	}
	return real.CoreV1().RESTClient()
}

// unreachableServer returns a REST client pointed at a server that has already
// been closed, so a request against it fails at the transport layer with no
// HTTP response at all — the same shape ControlPlaneReadyz's code == 0 path
// (and nodehealth's identical kubelet /healthz path) classifies as
// "unreachable": the endpoint never answered, rather than refused the request.
func unreachableServer(t *testing.T) rest.Interface {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	real, err := kubernetes.NewForConfig(&rest.Config{Host: url})
	if err != nil {
		t.Fatalf("building a client for the closed server: %v", err)
	}
	return real.CoreV1().RESTClient()
}

// TestEvaluate_ReadyzBlindSpotsByStatus is R81's regression fixture: a probe
// of each of the four /readyz statuses produces exactly the expected
// blind-spot set — ok and unhealthy record none, forbidden and unreachable
// each record exactly one entry with distinct reasons, and the forbidden
// reason is unchanged from before R81.
func TestEvaluate_ReadyzBlindSpotsByStatus(t *testing.T) {
	cases := []struct {
		name       string
		rest       func(t *testing.T) rest.Interface
		wantBlind  bool
		wantReason string
	}{
		{"ok", func(t *testing.T) rest.Interface { return readyzServer(t, 200, "ok") }, false, ""},
		{"unhealthy", func(t *testing.T) rest.Interface { return readyzServer(t, 500, "[-]etcd failed\n") }, false, ""},
		{"forbidden", forbiddenNodeProxyServer, true, blindReason("get /readyz")},
		{"unreachable", unreachableServer, true, "kubeagent could not reach the apiserver /readyz endpoint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := nodeStatsFailingClient{Interface: fake.NewSimpleClientset(), rest: c.rest(t)}
			res, err := Evaluate(context.Background(), client, Options{ControlPlaneHealth: true})
			if err != nil {
				t.Fatal(err)
			}
			var found *ReadFailure
			for i := range res.PartialReads {
				if res.PartialReads[i].Resource == "/readyz" {
					found = &res.PartialReads[i]
				}
			}
			if c.wantBlind && found == nil {
				t.Fatalf("PartialReads = %+v, want a /readyz entry", res.PartialReads)
			}
			if !c.wantBlind && found != nil {
				t.Fatalf("PartialReads = %+v, want no /readyz entry", res.PartialReads)
			}
			if c.wantBlind && found.Reason != c.wantReason {
				t.Errorf("Reason = %q, want %q", found.Reason, c.wantReason)
			}
		})
	}
}

// TestEvaluate_ReadyzBlindSpotFlagOff proves the /readyz blind spot never
// fires when --health is off, regardless of what the endpoint would have
// answered — the probe never runs, so none of the four statuses is reachable.
func TestEvaluate_ReadyzBlindSpotFlagOff(t *testing.T) {
	cases := []struct {
		name string
		rest func(t *testing.T) rest.Interface
	}{
		{"ok", func(t *testing.T) rest.Interface { return readyzServer(t, 200, "ok") }},
		{"unhealthy", func(t *testing.T) rest.Interface { return readyzServer(t, 500, "[-]etcd failed\n") }},
		{"forbidden", forbiddenNodeProxyServer},
		{"unreachable", unreachableServer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := nodeStatsFailingClient{Interface: fake.NewSimpleClientset(), rest: c.rest(t)}
			res, err := Evaluate(context.Background(), client, Options{})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range res.PartialReads {
				if p.Resource == "/readyz" {
					t.Fatalf("PartialReads = %+v, want no /readyz entry with the flag off", res.PartialReads)
				}
			}
		})
	}
}

// podLogsFailingClient wraps a kubernetes.Interface so CoreV1().Pods(ns).GetLogs
// resolves to a real, working REST client instead of the fake clientset's own
// GetLogs, which always succeeds and never lets a test drive a real HTTP error
// status through DoRaw. Every other typed client (Nodes, Pods List/Get, ...)
// still delegates to the wrapped fake, so this only replaces the pods/log read.
type podLogsFailingClient struct {
	kubernetes.Interface
	rest rest.Interface
}

func (c podLogsFailingClient) CoreV1() corev1client.CoreV1Interface {
	return podLogsFailingCoreV1{CoreV1Interface: c.Interface.CoreV1(), rest: c.rest}
}

type podLogsFailingCoreV1 struct {
	corev1client.CoreV1Interface
	rest rest.Interface
}

func (c podLogsFailingCoreV1) Pods(namespace string) corev1client.PodInterface {
	return podLogsFailingPods{PodInterface: c.CoreV1Interface.Pods(namespace), rest: c.rest, ns: namespace}
}

type podLogsFailingPods struct {
	corev1client.PodInterface
	rest rest.Interface
	ns   string
}

func (c podLogsFailingPods) GetLogs(name string, opts *corev1.PodLogOptions) *rest.Request {
	return c.rest.Get().Namespace(c.ns).Name(name).Resource("pods").SubResource("log").VersionedParams(opts, scheme.ParameterCodec)
}

// previousLogNotFoundServer starts an httptest server that answers every
// request with a genuine 400 BadRequest Status object — the real API server's
// answer for a container that has simply never terminated (the normal case
// for ImagePullBackOff, CreateContainerConfigError, Pending, and a
// probe-failing container that has not yet restarted), never a 403/401.
func previousLogNotFoundServer(t *testing.T) rest.Interface {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","message":"previous terminated container \"web\" in pod \"web-1\" not found","code":400}`))
	}))
	t.Cleanup(server.Close)
	real, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("building a client for the fake bad-request server: %v", err)
	}
	return real.CoreV1().RESTClient()
}

// TestEvaluate_LogsPreviousContainerAbsentIsNotABlindSpot proves that --logs
// against a container that has never terminated — a routine 400, not a
// permission denial — records no pods/log blind spot.
func TestEvaluate_LogsPreviousContainerAbsentIsNotABlindSpot(t *testing.T) {
	crashPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop", Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "web", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	client := podLogsFailingClient{
		Interface: fake.NewSimpleClientset(crashPod),
		rest:      previousLogNotFoundServer(t),
	}
	res, err := Evaluate(context.Background(), client, Options{Logs: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.PartialReads {
		if p.Resource == "pods/log" {
			t.Errorf("PartialReads = %+v, want no pods/log entry: a container that has never terminated answers 400, not 403/401", res.PartialReads)
		}
	}
}

// TestEvaluate_NoteRedactsRefusalIdentity proves that a genuine 403 on a
// note()-covered list (networkpolicies) is reported in kubeagent's own words,
// never the API server's message — which, under the built-in RBAC authorizer,
// interpolates the requesting identity.
func TestEvaluate_NoteRedactsRefusalIdentity(t *testing.T) {
	identity := `User "system:serviceaccount:prod:kubeagent-sa" cannot list resource`
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", errors.New(identity))
	})

	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil (a denied optional list must degrade, not fail)", err)
	}

	var found *ReadFailure
	for i := range res.PartialReads {
		if res.PartialReads[i].Resource == "networkpolicies" {
			found = &res.PartialReads[i]
		}
	}
	if found == nil {
		t.Fatalf("PartialReads = %v, want an entry for networkpolicies", res.PartialReads)
	}
	if !strings.HasPrefix(found.Reason, "forbidden:") {
		t.Errorf("Reason = %q, want it to start with %q (kubeagent's own wording)", found.Reason, "forbidden:")
	}
	if strings.Contains(found.Reason, identity) {
		t.Errorf("Reason = %q, must not contain the API server's message, which names the requesting identity %q", found.Reason, identity)
	}
}

// slowKubeletHealthzServer answers /api/v1/nodes/<name>/proxy/healthz with a 500
// after a delay that is LONGEST for the first node and shortest for the last, so
// the probes finish in exactly the reverse of the order they were dispatched.
//
// It goes over real HTTP through the swapped RESTClient because that is the only
// way to get genuine overlap: k8stesting.Fake.Invokes holds a write lock across
// the whole reactor body, so a reactor that sleeps serialises the calls instead
// of overlapping them.
func slowKubeletHealthzServer(t *testing.T, nodes []string) (rest.Interface, func() []string) {
	t.Helper()

	index := map[string]int{}
	for i, n := range nodes {
		index[n] = i
	}

	var mu sync.Mutex
	var completed []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := nodeNameFromHealthzPath(r.URL.Path)
		i, ok := index[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		time.Sleep(time.Duration(len(nodes)-i) * 25 * time.Millisecond)

		mu.Lock()
		completed = append(completed, name)
		mu.Unlock()

		// Content-Type must be set: without it, client-go's content negotiation
		// on a non-2xx response fails before the status code is ever recorded on
		// the Result, and collect.KubeletHealthz sees code 0 ("unreachable")
		// instead of 500 ("unhealthy") — same reason forbiddenNodeProxyServer and
		// previousLogNotFoundServer above set it.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("[-]check-" + name + " failed\n"))
	}))
	t.Cleanup(server.Close)

	// QPS -1 for the same reason production uses it: client-go's default token
	// bucket would meter these probes and defeat the inversion under test.
	real, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: -1})
	if err != nil {
		t.Fatalf("building a client for the slow kubelet server: %v", err)
	}
	return real.CoreV1().RESTClient(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), completed...)
	}
}

// nodeNameFromHealthzPath pulls <name> out of /api/v1/nodes/<name>/proxy/healthz.
func nodeNameFromHealthzPath(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) < 4 || parts[2] != "nodes" {
		return ""
	}
	return parts[3]
}

// The phase-2 node fan-out reports in node order even when the probes finish in
// exactly the opposite order. nodehealth.Assess preserves probe order, so
// Unhealthy is a direct read-out of the order the pool wrote its slots in.
func TestKubeletHealthFanOutReportsInNodeOrderNotCompletionOrder(t *testing.T) {
	names := []string{"node-0", "node-1", "node-2", "node-3", "node-4"}
	objs := make([]runtime.Object, 0, len(names))
	for _, n := range names {
		objs = append(objs, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}})
	}

	restClient, _ := slowKubeletHealthzServer(t, names)
	client := nodeStatsFailingClient{Interface: fake.NewSimpleClientset(objs...), rest: restClient}

	t.Setenv("KUBEAGENT_SCAN_WORKERS", "8") // all five probes in flight at once
	res, err := Evaluate(context.Background(), client, Options{KubeletHealth: true})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, iss := range res.KubeletHealth.Unhealthy {
		got = append(got, iss.Node)
	}
	if !reflect.DeepEqual(got, names) {
		t.Errorf("KubeletHealth.Unhealthy = %v, want %v — the report must follow node order, not completion order", got, names)
	}
}

// PartialReads follows the fixed report order, not the order the refusals
// arrived. The reactors below are registered in the reverse of the expected
// order, so a report that followed registration or completion order comes out
// backwards and this test fails.
func TestPartialReadsFollowReportOrderNotCompletionOrder(t *testing.T) {
	client := fake.NewSimpleClientset()
	forbid := func(resource string) {
		client.Fake.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", nil)
		})
	}
	forbid("networkpolicies")
	forbid("persistentvolumes")
	forbid("namespaces")
	forbid("services")

	t.Setenv("KUBEAGENT_SCAN_WORKERS", "8")
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, p := range res.PartialReads {
		got = append(got, p.Resource)
	}
	want := []string{"services", "namespaces", "persistentvolumes", "networkpolicies"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PartialReads = %v, want %v", got, want)
	}
}

// Five event lists fail with a transport error, so five entries are recorded —
// note() deduplicates only through blind(), i.e. only for refusals. This pins the
// "five identical lines" behaviour that the report order deliberately keeps.
func TestFailingEventListsRecordOneEntryEach(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.Fake.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	t.Setenv("KUBEAGENT_SCAN_WORKERS", "8")
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.PartialReads) != 5 {
		t.Fatalf("PartialReads = %+v, want 5 entries (volume-attach, volume-mount, unhealthy, PVC, FailedCreate)", res.PartialReads)
	}
	for i, p := range res.PartialReads {
		if p.Resource != "events" {
			t.Errorf("PartialReads[%d].Resource = %q, want %q", i, p.Resource, "events")
		}
	}
}

// determinismFixture builds a cluster with a crash-looping pod, a Deployment, a
// Service with no endpoints, a PVC and two nodes. Every timestamp is 48 hours in
// the past so inventory.HumanAge renders whole days ("2d"): an age rendered in
// seconds would differ between two runs milliseconds apart and the comparison
// below would be testing the clock rather than the pool.
func determinismFixture() []runtime.Object {
	old := metav1.NewTime(time.Now().Add(-48 * time.Hour))
	return []runtime.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", CreationTimestamp: old},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b", CreationTimestamp: old},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-1", CreationTimestamp: old, Labels: map[string]string{"app": "api"}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				Name: "api", RestartCount: 7,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off restarting failed container"}},
			}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "worker-1", CreationTimestamp: old, Labels: map[string]string{"app": "worker"}},
			Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/2 nodes are available",
			}}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api", CreationTimestamp: old},
			Spec:   appsv1.DeploymentSpec{Replicas: ptrInt32(3)},
			Status: appsv1.DeploymentStatus{Replicas: 3, ReadyReplicas: 1}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api", CreationTimestamp: old},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "api"}, Ports: []corev1.ServicePort{{Port: 80}}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data", CreationTimestamp: old},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}},
	}
}

func ptrInt32(n int32) *int32 { return &n }

// Two runs at different worker counts must produce an identical Result. The
// third run repeats the second so that a fixture whose output drifts with the
// clock fails as a fixture bug rather than as a concurrency bug.
func TestEvaluateIsIndependentOfTheWorkerCount(t *testing.T) {
	// Built once and reused for every run below: fake.NewSimpleClientset deep-copies
	// on ingestion, so sharing objs across clientsets is safe, and calling
	// determinismFixture() fresh per run would give each run a different
	// time.Now()-derived CreationTimestamp — a raw field DeepEqual compares at
	// nanosecond precision — which would fail this test on clock drift alone,
	// never on the pool.
	objs := determinismFixture()
	run := func(workers string) Result {
		t.Setenv("KUBEAGENT_SCAN_WORKERS", workers)
		res, err := Evaluate(context.Background(), fake.NewSimpleClientset(objs...), Options{Security: true, IncludeRestarts: true})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	eight, eightAgain := run("8"), run("8")
	if !reflect.DeepEqual(eight, eightAgain) {
		t.Fatalf("two runs at the same worker count differed — the fixture is not stable, so the comparison below would be meaningless")
	}

	one := run("1")
	if !reflect.DeepEqual(one, eight) {
		t.Errorf("Evaluate at 1 worker differs from Evaluate at 8 workers:\n one   = %+v\n eight = %+v", one, eight)
	}
}

// Repeating the same scan many times must produce the same Result every time.
// The reactor delays each list by an amount derived from the resource name — a
// stable delay, no randomness — which is enough to vary which goroutine reaches
// the fake's lock first, and so to sample real orderings.
func TestEvaluateIsStableAcrossRepeatedRuns(t *testing.T) {
	// Same reasoning as TestEvaluateIsIndependentOfTheWorkerCount: build the
	// fixture once so every run shares the same CreationTimestamp values instead
	// of each newClient() call minting its own time.Now().
	objs := determinismFixture()
	newClient := func() *fake.Clientset {
		c := fake.NewSimpleClientset(objs...)
		c.Fake.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
			time.Sleep(time.Duration(len(action.GetResource().Resource)%5) * time.Millisecond)
			return false, nil, nil // fall through to the tracker
		})
		return c
	}

	t.Setenv("KUBEAGENT_SCAN_WORKERS", "8")
	first, err := Evaluate(context.Background(), newClient(), Options{Security: true, IncludeRestarts: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 20; i++ {
		got, err := Evaluate(context.Background(), newClient(), Options{Security: true, IncludeRestarts: true})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differed from run 0 — the output depends on the schedule", i)
		}
	}
}
