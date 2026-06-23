package collect

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// CollectInventory lists pods and the Phase-B controller kinds (Deployments,
// ReplicaSets, StatefulSets, DaemonSets) in the given namespace (or all
// namespaces when empty). Read-only: List calls only.
func CollectInventory(ctx context.Context, client kubernetes.Interface, namespace string) (inventory.Inputs, error) {
	var in inventory.Inputs

	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing pods: %w", err)
	}
	in.Pods = pods.Items

	deploys, err := client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing deployments: %w", err)
	}
	in.Deployments = deploys.Items

	rs, err := client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing replicasets: %w", err)
	}
	in.ReplicaSets = rs.Items

	sts, err := client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing statefulsets: %w", err)
	}
	in.StatefulSets = sts.Items

	ds, err := client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing daemonsets: %w", err)
	}
	in.DaemonSets = ds.Items

	return in, nil
}

// Nodes lists all cluster nodes (read-only). Nodes are cluster-scoped, so this
// is not affected by the scan's namespace filter.
func Nodes(ctx context.Context, client kubernetes.Interface) ([]corev1.Node, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	return nodes.Items, nil
}

// FactsFrom wraps each pod in a diagnose.PodFacts for the detectors.
func FactsFrom(pods []corev1.Pod) []diagnose.PodFacts {
	facts := make([]diagnose.PodFacts, 0, len(pods))
	for i := range pods {
		pod := pods[i] // copy so &pod is stable per iteration
		facts = append(facts, diagnose.PodFacts{Pod: &pod})
	}
	return facts
}

