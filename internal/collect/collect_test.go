package collect

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCollectInventory_ListsControllersAndPods(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "d1"}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "rs1"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s1"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "ds1"}},
	)
	in, err := CollectInventory(context.Background(), client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(in.Pods) != 1 || len(in.Deployments) != 1 || len(in.ReplicaSets) != 1 ||
		len(in.StatefulSets) != 1 || len(in.DaemonSets) != 1 {
		t.Errorf("expected one of each kind, got %+v", in)
	}
}

func TestCollectInventory_ScopesToNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "d1"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "d2"}},
	)
	in, err := CollectInventory(context.Background(), client, "a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(in.Deployments) != 1 || in.Deployments[0].Namespace != "a" {
		t.Errorf("expected only namespace a, got %+v", in.Deployments)
	}
}

func TestFactsFrom_WrapsEachPod(t *testing.T) {
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p2"}},
	}
	facts := FactsFrom(pods)
	if len(facts) != 2 || facts[0].Pod == nil || facts[0].Pod.Name != "p1" {
		t.Fatalf("expected 2 facts wrapping each pod, got %+v", facts)
	}
}
