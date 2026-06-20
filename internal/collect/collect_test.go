package collect

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCluster_ReturnsFactsForAllPods(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "p2"}},
	)

	facts, err := Cluster(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 pod facts, got %d", len(facts))
	}
	if facts[0].Pod == nil || facts[0].Pod.Name == "" {
		t.Error("expected each fact to carry a non-empty Pod")
	}
}

func TestCluster_EmptyClusterReturnsNoFacts(t *testing.T) {
	facts, err := Cluster(context.Background(), fake.NewSimpleClientset())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("expected 0 facts, got %d", len(facts))
	}
}
