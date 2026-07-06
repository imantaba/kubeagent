package scan

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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

func p32(i int32) *int32 { return &i }
