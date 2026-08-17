package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/controlplane"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// Pods lists pods in the given namespace (or all namespaces when empty).
// Read-only: a List call.
func Pods(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Pod, error) {
	list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	return list.Items, nil
}

// Deployments lists Deployments in the given namespace (or all namespaces when
// empty). Read-only: a List call.
func Deployments(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.Deployment, error) {
	list, err := client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}
	return list.Items, nil
}

// ReplicaSets lists ReplicaSets in the given namespace (or all namespaces when
// empty). Read-only: a List call.
func ReplicaSets(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.ReplicaSet, error) {
	list, err := client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing replicasets: %w", err)
	}
	return list.Items, nil
}

// StatefulSets lists StatefulSets in the given namespace (or all namespaces when
// empty). Read-only: a List call.
func StatefulSets(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.StatefulSet, error) {
	list, err := client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing statefulsets: %w", err)
	}
	return list.Items, nil
}

// DaemonSets lists DaemonSets in the given namespace (or all namespaces when
// empty). Read-only: a List call. SystemDaemonSets is a different thing: it
// lists kube-system only, regardless of the scan's namespace filter.
func DaemonSets(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.DaemonSet, error) {
	list, err := client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing daemonsets: %w", err)
	}
	return list.Items, nil
}

// Jobs lists Jobs in the given namespace (or all namespaces when empty).
// Read-only: a List call.
func Jobs(ctx context.Context, client kubernetes.Interface, namespace string) ([]batchv1.Job, error) {
	list, err := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	return list.Items, nil
}

// CronJobs lists CronJobs in the given namespace (or all namespaces when empty).
// Read-only: a List call.
func CronJobs(ctx context.Context, client kubernetes.Interface, namespace string) ([]batchv1.CronJob, error) {
	list, err := client.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing cronjobs: %w", err)
	}
	return list.Items, nil
}

// CollectInventory lists pods and the controller kinds (Deployments, ReplicaSets,
// StatefulSets, DaemonSets, Jobs, CronJobs) in the given namespace (or all
// namespaces when empty). Read-only: List calls only. It stops at the first
// failure and returns what it had, the same as it always did; the scan calls the
// seven functions directly so it can issue them together.
func CollectInventory(ctx context.Context, client kubernetes.Interface, namespace string) (inventory.Inputs, error) {
	var in inventory.Inputs
	var err error

	if in.Pods, err = Pods(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.Deployments, err = Deployments(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.ReplicaSets, err = ReplicaSets(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.StatefulSets, err = StatefulSets(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.DaemonSets, err = DaemonSets(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.Jobs, err = Jobs(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.CronJobs, err = CronJobs(ctx, client, namespace); err != nil {
		return in, err
	}
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

// NodeLeases lists node heartbeat Leases in kube-node-lease (one per node), read-only.
func NodeLeases(ctx context.Context, client kubernetes.Interface) ([]coordinationv1.Lease, error) {
	leases, err := client.CoordinationV1().Leases("kube-node-lease").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing node leases: %w", err)
	}
	return leases.Items, nil
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

// FailedCreateEvents lists the controller "FailedCreate" Warning events in the
// namespace ("" = all) — a Deployment's ReplicaSet, a StatefulSet, or a DaemonSet
// reporting that it cannot create pods (quota, LimitRange, admission webhook).
// Read-only; mirrors VolumeAttachEvents. Needs no permission beyond the event
// list scan already performs.
func FailedCreateEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=FailedCreate"})
	if err != nil {
		return nil, fmt.Errorf("listing FailedCreate events: %w", err)
	}
	return events.Items, nil
}

// FailedMountEvents lists the kubelet's "FailedMount" Warning events in the
// namespace ("" = all) — a volume the pod needs could not be mounted, most often
// a ConfigMap or Secret named in the pod spec that does not exist. Read-only;
// mirrors VolumeAttachEvents. Needs no permission beyond the event list scan
// already performs.
//
// The loop repeats the field selector client-side because client-go's fake
// clientset ignores field selectors: without it a test would see every event in
// the namespace and could not tell this read from one that was never wired up.
func FailedMountEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	list, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=FailedMount"})
	if err != nil {
		return nil, fmt.Errorf("listing volume-mount (FailedMount) events: %w", err)
	}
	out := make([]corev1.Event, 0, len(list.Items))
	for _, e := range list.Items {
		if e.Reason == "FailedMount" {
			out = append(out, e)
		}
	}
	return out, nil
}

// UnhealthyEvents lists the kubelet's probe-failure ("Unhealthy") Warning events
// in the namespace ("" = all). Read-only; mirrors VolumeAttachEvents. Needs no
// permission beyond the event list scan already performs.
func UnhealthyEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=Unhealthy"})
	if err != nil {
		return nil, fmt.Errorf("listing probe (Unhealthy) events: %w", err)
	}
	return events.Items, nil
}

// PVCEvents lists events involving PersistentVolumeClaims in the namespace (""=all).
// Read-only; pvchealth.Assess filters to the provisioning/binding failure reasons.
func PVCEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
	events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=PersistentVolumeClaim"})
	if err != nil {
		return nil, fmt.Errorf("listing PVC events: %w", err)
	}
	return events.Items, nil
}

// ObjectEvents lists the events attached to one object. The field selector is
// what a real API server uses to do the filtering server-side; the loop repeats
// it client-side because client-go's fake clientset ignores field selectors, so
// without it every test would see every event in the namespace.
func ObjectEvents(ctx context.Context, client kubernetes.Interface, namespace, name string) ([]corev1.Event, error) {
	list, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + name,
	})
	if err != nil {
		return nil, err
	}
	out := make([]corev1.Event, 0, len(list.Items))
	for _, e := range list.Items {
		if e.InvolvedObject.Name == name {
			out = append(out, e)
		}
	}
	return out, nil
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

// PodMetrics reads live per-pod usage from metrics-server via a raw GET on the
// metrics API, keyed "namespace/name". available is false (and err nil) when
// metrics-server is absent or forbidden, so a scan still succeeds without it —
// the same contract as NodeMetrics.
func PodMetrics(ctx context.Context, client kubernetes.Interface) (map[string]corev1.ResourceList, bool, error) {
	data, err := client.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/pods").DoRaw(ctx)
	if err != nil {
		return nil, false, nil // metrics-server absent/forbidden — non-fatal
	}
	usage, err := parsePodMetrics(data)
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

// Namespaces lists all namespaces (cluster-scoped; read-only) for the
// stuck-terminating check. Needs the base `namespaces` list grant; a forbidden
// list is handled gracefully by the caller (namespace checks are skipped).
func Namespaces(ctx context.Context, client kubernetes.Interface) ([]corev1.Namespace, error) {
	list, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	return list.Items, nil
}

// PodDisruptionBudgets lists PDBs in the namespace (empty = all), read-only. Used
// by the PDB-blocked-drains check. Needs the base policy/poddisruptionbudgets list
// grant; a forbidden/absent result simply omits the check.
func PodDisruptionBudgets(ctx context.Context, client kubernetes.Interface, namespace string) ([]policyv1.PodDisruptionBudget, error) {
	list, err := client.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing poddisruptionbudgets: %w", err)
	}
	return list.Items, nil
}

// ResourceQuotas lists ResourceQuotas in the namespace (empty = all namespaces),
// read-only. Used by the ResourceQuota near-exhaustion check. Needs the core-group
// resourcequotas list grant; a forbidden/absent result simply omits the check.
func ResourceQuotas(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.ResourceQuota, error) {
	list, err := client.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing resourcequotas: %w", err)
	}
	return list.Items, nil
}

// HorizontalPodAutoscalers lists HPAs in the namespace (empty = all), read-only.
// Used by the HPA-can't-scale check. Needs the base autoscaling/horizontalpodautoscalers
// list grant; a forbidden/absent result simply omits the check.
func HorizontalPodAutoscalers(ctx context.Context, client kubernetes.Interface, namespace string) ([]autoscalingv2.HorizontalPodAutoscaler, error) {
	list, err := client.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing horizontalpodautoscalers: %w", err)
	}
	return list.Items, nil
}

// ValidatingWebhookConfigurations lists all validating admission webhook configs
// (cluster-scoped; read-only). Used by the admission-webhook-failure check. Needs
// the base admissionregistration.k8s.io grant; a forbidden/absent result omits it.
func ValidatingWebhookConfigurations(ctx context.Context, client kubernetes.Interface) ([]admissionv1.ValidatingWebhookConfiguration, error) {
	list, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing validatingwebhookconfigurations: %w", err)
	}
	return list.Items, nil
}

// MutatingWebhookConfigurations lists all mutating admission webhook configs
// (cluster-scoped; read-only).
func MutatingWebhookConfigurations(ctx context.Context, client kubernetes.Interface) ([]admissionv1.MutatingWebhookConfiguration, error) {
	list, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing mutatingwebhookconfigurations: %w", err)
	}
	return list.Items, nil
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

// TLSSecrets lists the kubernetes.io/tls Secrets in the namespace ("" = all) —
// public certificate material for the opt-in --certs check. The type field
// selector narrows server-side; certhealth re-filters by type in-code (the fake
// clientset ignores field selectors). Requires the secrets add-on grant
// (deploy/rbac-certs.yaml); never called unless --certs is set.
func TLSSecrets(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Secret, error) {
	secrets, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=kubernetes.io/tls"})
	if err != nil {
		return nil, fmt.Errorf("listing TLS secrets: %w", err)
	}
	// list has no server-side field projection for Secrets, so the API
	// server already returned tls.key in the response body. Zero it here, on
	// a copy of the Data map — never on the object List returned — so the
	// private key's residency inside kubeagent ends at this line rather than
	// lasting for the whole scan. certhealth.Assess only ever reads tls.crt,
	// so this changes nothing it can observe.
	out := make([]corev1.Secret, len(secrets.Items))
	for i, s := range secrets.Items {
		if _, ok := s.Data["tls.key"]; ok {
			cp := make(map[string][]byte, len(s.Data))
			for k, v := range s.Data {
				if k == "tls.key" {
					continue
				}
				cp[k] = v
			}
			s.Data = cp
		}
		out[i] = s
	}
	return out, nil
}

// ConfigMaps lists ConfigMaps in the namespace (empty = all), read-only.
func ConfigMaps(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.ConfigMap, error) {
	cms, err := client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing configmaps: %w", err)
	}
	return cms.Items, nil
}

// NodeStats reads one node's kubelet summary API through the nodes/proxy
// subresource. It returns (zero, false, err) when the read is refused or the
// node is unreachable, so a scan can still succeed without it while naming what
// it could not see — a discarded error here would make a missing nodes/proxy
// grant not merely silent but unrepresentable.
func NodeStats(ctx context.Context, client kubernetes.Interface, node string) (diskusage.NodeSummary, bool, error) {
	data, err := client.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/nodes/%s/proxy/stats/summary", node)).DoRaw(ctx)
	if err != nil {
		return diskusage.NodeSummary{}, false, err
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

// PreviousLogs reads the tail of a container's previous run. It returns
// ("", false, err) when the read is refused, so --logs without a pods/log
// grant reports a blind spot instead of quietly finding no log cause. An empty
// log is ("", false, nil): nothing was refused, there was simply nothing there.
func PreviousLogs(ctx context.Context, client kubernetes.Interface, ns, pod, container string) (string, bool, error) {
	// 25 names the window internal/logscan.Classify's fallback cause reports
	// ("no signature in the last 25 lines"). Keep the two in sync if either
	// changes.
	tail := int64(25)
	raw, err := client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, Previous: true, TailLines: &tail,
	}).DoRaw(ctx)
	if err != nil {
		return "", false, err
	}
	if len(raw) == 0 {
		return "", false, nil
	}
	return string(raw), true, nil
}

// maxProxyBody bounds what kubeagent will parse from a proxied endpoint — a
// kubelet /healthz, a CoreDNS /metrics, an apiserver /readyz. 1 MiB is well past
// any real response and well short of a body worth parsing by mistake.
//
// This bounds the parsers and any later copy, NOT the transfer: client-go's
// Result.Raw() returns a body it has already read in full, with no cap, and
// gives no access to the underlying reader. Bounding the transfer would need a
// custom http.RoundTripper on the rest config — a separate change.
const maxProxyBody = 1 << 20

// capBody returns at most maxProxyBody bytes of b.
func capBody(b []byte) []byte {
	if len(b) > maxProxyBody {
		return b[:maxProxyBody]
	}
	return b
}

// KubeletHealthz probes a node's kubelet /healthz via the nodes/proxy subresource
// and classifies the result. Never returns an error (non-fatal, like NodeStats).
func KubeletHealthz(ctx context.Context, client kubernetes.Interface, node string) nodehealth.Probe {
	var code int
	body, _ := client.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/nodes/%s/proxy/healthz", node)).
		Do(ctx).StatusCode(&code).Raw()
	return classify(node, code, capBody(body))
}

// CoreDNSMetrics fetches a CoreDNS pod's :9153/metrics via the pods/proxy
// subresource, returning the raw body and HTTP status code. Never returns an error
// (non-fatal, like KubeletHealthz). Needs the pods/proxy get grant; a 401/403 is
// surfaced to the caller via the code.
func CoreDNSMetrics(ctx context.Context, client kubernetes.Interface, namespace, pod string) ([]byte, int) {
	var code int
	body, _ := client.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:9153/proxy/metrics", namespace, pod)).
		Do(ctx).StatusCode(&code).Raw()
	return capBody(body), code
}

// ControlPlaneReadyz probes the apiserver /readyz?verbose endpoint and classifies
// the result. Never returns an error (non-fatal, like KubeletHealthz). Needs the
// nonResourceURLs /readyz get grant; a 401/403 yields Status "forbidden".
//
// A real apiserver's /readyz failure body is text/plain; client-go's serializer
// negotiator has no decoder for that content type on a non-2xx response, and that
// negotiation failure returns from StatusCode(&code) without ever setting code, so
// the caller reads back 0 ("unreachable") no matter what status was actually
// written. statusCodeFrom recovers it from the error the same negotiation failure
// carries when code is left at 0.
func ControlPlaneReadyz(ctx context.Context, client kubernetes.Interface) controlplane.Probe {
	var code int
	body, err := client.CoreV1().RESTClient().Get().
		AbsPath("/readyz").Param("verbose", "true").
		Do(ctx).StatusCode(&code).Raw()
	if code == 0 {
		code = statusCodeFrom(err)
	}
	return controlplane.ParseReadyz(code, capBody(body))
}

// statusCodeFrom recovers the HTTP status code an error carries when it
// satisfies apierrors.APIStatus, and 0 otherwise (including a nil err).
func statusCodeFrom(err error) int {
	status, ok := err.(apierrors.APIStatus)
	if !ok {
		return 0
	}
	return int(status.Status().Code)
}

// classify maps a /healthz probe result to a Probe. 200 is ok; 401/403 is
// forbidden (grant missing); code 0 (no HTTP status — transport error) or a
// 502/503/504 gateway error is unreachable — the kubelet itself never
// answered, whether the proxy could not reach it or gave up waiting; any
// other status the kubelet returned is unhealthy.
func classify(node string, code int, body []byte) nodehealth.Probe {
	switch {
	case code == 200:
		return nodehealth.Probe{Node: node, Status: "ok"}
	case code == 401 || code == 403:
		return nodehealth.Probe{Node: node, Status: "forbidden"}
	case code == 0 || code == 502 || code == 503 || code == 504:
		return nodehealth.Probe{Node: node, Status: "unreachable"}
	default:
		return nodehealth.Probe{Node: node, Status: "unhealthy", Detail: healthzDetail(body, 120)}
	}
}

// healthzDetail returns the first failed-check line ("[-]…") from a kubelet
// /healthz body, trimmed, truncated to max runes and sanitized through
// safetext.Line, or "" when the body carries no such line — an unparsed body
// is not a diagnosis, and the row still degrades gracefully:
// printKubeletHealth omits the detail suffix entirely when Detail is empty.
// The "[-]" prefix test runs on the raw, unsanitized line: only the returned
// value passes through safetext.Line, so a control character spliced into a
// forged prefix cannot be sanitized away and then match.
func healthzDetail(body []byte, max int) string {
	for _, ln := range strings.Split(string(body), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "[-]") {
			return safetext.Line(truncateRunes(ln, max))
		}
	}
	return ""
}

func truncateRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
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

// parsePodMetrics decodes a metrics.k8s.io PodMetricsList body into per-pod
// resource quantities keyed "namespace/name". Unlike NodeMetricsList, usage is
// reported per container, so each pod's containers are summed.
func parsePodMetrics(data []byte) (map[string]corev1.ResourceList, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Containers []struct {
				Usage map[string]string `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parsing pod metrics: %w", err)
	}
	out := make(map[string]corev1.ResourceList, len(list.Items))
	for _, it := range list.Items {
		rl := corev1.ResourceList{}
		for _, c := range it.Containers {
			for k, v := range c.Usage {
				q, err := resource.ParseQuantity(v)
				if err != nil {
					return nil, fmt.Errorf("parsing usage %q for pod %s/%s: %w",
						v, it.Metadata.Namespace, it.Metadata.Name, err)
				}
				cur := rl[corev1.ResourceName(k)]
				cur.Add(q)
				rl[corev1.ResourceName(k)] = cur
			}
		}
		out[it.Metadata.Namespace+"/"+it.Metadata.Name] = rl
	}
	return out, nil
}
