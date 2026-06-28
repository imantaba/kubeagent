package collect

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// CollectInventory lists pods and the controller kinds (Deployments, ReplicaSets,
// StatefulSets, DaemonSets, Jobs, CronJobs) in the given namespace (or all
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

	jobs, err := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing jobs: %w", err)
	}
	in.Jobs = jobs.Items

	cronjobs, err := client.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing cronjobs: %w", err)
	}
	in.CronJobs = cronjobs.Items

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
		pod := pods[i] // take this element's address for PodFacts
		facts = append(facts, diagnose.PodFacts{Pod: &pod})
	}
	return facts
}

// AllPods lists pods across all namespaces (read-only). Used for the cluster
// resource summary when the scan itself is namespace-scoped.
func AllPods(ctx context.Context, client kubernetes.Interface) ([]corev1.Pod, error) {
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing all pods: %w", err)
	}
	return pods.Items, nil
}

// NodeMetrics reads live per-node usage from metrics-server via a raw GET on the
// metrics API. available is false (and err nil) when metrics-server is absent or
// forbidden, so a scan still succeeds without it.
func NodeMetrics(ctx context.Context, client kubernetes.Interface) (map[string]corev1.ResourceList, bool, error) {
	data, err := client.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").DoRaw(ctx)
	if err != nil {
		return nil, false, nil // metrics-server absent/forbidden — non-fatal
	}
	usage, err := parseNodeMetrics(data)
	if err != nil {
		return nil, false, err
	}
	return usage, len(usage) > 0, nil
}

// StorageClasses lists all StorageClasses (cluster-scoped, read-only).
func StorageClasses(ctx context.Context, client kubernetes.Interface) ([]storagev1.StorageClass, error) {
	scs, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing storageclasses: %w", err)
	}
	return scs.Items, nil
}

// IngressClasses lists all IngressClasses (cluster-scoped, read-only).
func IngressClasses(ctx context.Context, client kubernetes.Interface) ([]networkingv1.IngressClass, error) {
	ics, err := client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ingressclasses: %w", err)
	}
	return ics.Items, nil
}

// SystemDaemonSets lists DaemonSets in kube-system (read-only) — used to detect
// the CNI regardless of the scan's namespace scope.
func SystemDaemonSets(ctx context.Context, client kubernetes.Interface) ([]appsv1.DaemonSet, error) {
	dss, err := client.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing kube-system daemonsets: %w", err)
	}
	return dss.Items, nil
}

// parseNodeMetrics decodes a metrics.k8s.io NodeMetricsList body into per-node
// resource quantities keyed by node name.
func parseNodeMetrics(data []byte) (map[string]corev1.ResourceList, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage map[string]string `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parsing node metrics: %w", err)
	}
	out := make(map[string]corev1.ResourceList, len(list.Items))
	for _, it := range list.Items {
		rl := corev1.ResourceList{}
		for k, v := range it.Usage {
			q, err := resource.ParseQuantity(v)
			if err != nil {
				return nil, fmt.Errorf("parsing usage %q for node %s: %w", v, it.Metadata.Name, err)
			}
			rl[corev1.ResourceName(k)] = q
		}
		out[it.Metadata.Name] = rl
	}
	return out, nil
}
