package collect

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/diskusage"
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

// VolumeAttachEvents lists FailedAttachVolume Warning events in the namespace
// (empty = all), read-only. Attach failures are rare, so this field-selected
// List is cheap.
func VolumeAttachEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=FailedAttachVolume"})
	if err != nil {
		return nil, fmt.Errorf("listing volume-attach events: %w", err)
	}
	return events.Items, nil
}

// FactsFrom wraps each pod in a diagnose.PodFacts, attaching any of the given
// events that reference that pod (by InvolvedObject). Pods with no matching
// events get an empty slice, so status-only detectors are unaffected.
func FactsFrom(pods []corev1.Pod, events []corev1.Event) []diagnose.PodFacts {
	byPod := make(map[string][]corev1.Event)
	for _, e := range events {
		if e.InvolvedObject.Kind == "Pod" {
			key := e.InvolvedObject.Namespace + "/" + e.InvolvedObject.Name
			byPod[key] = append(byPod[key], e)
		}
	}
	facts := make([]diagnose.PodFacts, 0, len(pods))
	for i := range pods {
		pod := pods[i] // take this element's address for PodFacts
		facts = append(facts, diagnose.PodFacts{Pod: &pod, Events: byPod[pod.Namespace+"/"+pod.Name]})
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

// PersistentVolumeClaims lists PVCs in the namespace (all namespaces when
// empty), read-only.
func PersistentVolumeClaims(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.PersistentVolumeClaim, error) {
	pvcs, err := client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing persistentvolumeclaims: %w", err)
	}
	return pvcs.Items, nil
}

// PersistentVolumes lists all PVs (cluster-scoped, read-only).
func PersistentVolumes(ctx context.Context, client kubernetes.Interface) ([]corev1.PersistentVolume, error) {
	pvs, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing persistentvolumes: %w", err)
	}
	return pvs.Items, nil
}

// IngressClasses lists all IngressClasses (cluster-scoped, read-only).
func IngressClasses(ctx context.Context, client kubernetes.Interface) ([]networkingv1.IngressClass, error) {
	ics, err := client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ingressclasses: %w", err)
	}
	return ics.Items, nil
}

// Ingresses lists Ingresses in the namespace (empty = all), read-only.
func Ingresses(ctx context.Context, client kubernetes.Interface, namespace string) ([]networkingv1.Ingress, error) {
	ings, err := client.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ingresses: %w", err)
	}
	return ings.Items, nil
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

// Services lists Services in the namespace (empty = all), read-only.
func Services(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Service, error) {
	svcs, err := client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	return svcs.Items, nil
}

// EndpointSlices lists EndpointSlices in the namespace (empty = all), read-only.
func EndpointSlices(ctx context.Context, client kubernetes.Interface, namespace string) ([]discoveryv1.EndpointSlice, error) {
	slices, err := client.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing endpointslices: %w", err)
	}
	return slices.Items, nil
}

// NetworkPolicies lists NetworkPolicies in the namespace (empty = all), read-only.
func NetworkPolicies(ctx context.Context, client kubernetes.Interface, namespace string) ([]networkingv1.NetworkPolicy, error) {
	nps, err := client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing networkpolicies: %w", err)
	}
	return nps.Items, nil
}

// ConfigMaps lists ConfigMaps in the namespace (empty = all), read-only.
func ConfigMaps(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.ConfigMap, error) {
	cms, err := client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing configmaps: %w", err)
	}
	return cms.Items, nil
}

// NodeStats fetches one node's kubelet /stats/summary through the nodes/proxy
// subresource (read-only). A forbidden or unreachable node yields
// (zero, false, nil) so a scan still succeeds without it. Requires the
// nodes/proxy grant (opt-in; see deploy/rbac-diskusage.yaml).
func NodeStats(ctx context.Context, client kubernetes.Interface, node string) (diskusage.NodeSummary, bool, error) {
	data, err := client.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/nodes/%s/proxy/stats/summary", node)).DoRaw(ctx)
	if err != nil {
		return diskusage.NodeSummary{}, false, nil // forbidden/unreachable — non-fatal
	}
	return parseNodeSummary(node, data)
}

// parseNodeSummary decodes the kubelet Summary JSON we consume: the node root
// filesystem and each pod volume that carries a pvcRef.
func parseNodeSummary(node string, data []byte) (diskusage.NodeSummary, bool, error) {
	var raw struct {
		Node struct {
			Fs struct {
				UsedBytes     int64 `json:"usedBytes"`
				CapacityBytes int64 `json:"capacityBytes"`
			} `json:"fs"`
		} `json:"node"`
		Pods []struct {
			Volume []struct {
				UsedBytes     int64 `json:"usedBytes"`
				CapacityBytes int64 `json:"capacityBytes"`
				PVCRef        *struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"pvcRef"`
			} `json:"volume"`
		} `json:"pods"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return diskusage.NodeSummary{}, false, err
	}
	out := diskusage.NodeSummary{Node: node, FSUsed: raw.Node.Fs.UsedBytes, FSCap: raw.Node.Fs.CapacityBytes}
	for _, p := range raw.Pods {
		for _, v := range p.Volume {
			if v.PVCRef == nil {
				continue
			}
			out.Volumes = append(out.Volumes, diskusage.PVCVolume{
				Namespace: v.PVCRef.Namespace, Name: v.PVCRef.Name,
				Used: v.UsedBytes, Cap: v.CapacityBytes,
			})
		}
	}
	return out, true, nil
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
