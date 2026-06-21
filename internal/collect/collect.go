package collect

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

// Cluster lists pods (in the given namespace, or all namespaces when namespace
// is empty) and wraps each in PodFacts. It is read-only: a single List call,
// never mutating anything.
func Cluster(ctx context.Context, client kubernetes.Interface, namespace string) ([]diagnose.PodFacts, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	facts := make([]diagnose.PodFacts, 0, len(pods.Items))
	for i := range pods.Items {
		pod := pods.Items[i] // copy so &pod is stable per iteration
		facts = append(facts, diagnose.PodFacts{Pod: &pod})
	}
	return facts, nil
}
