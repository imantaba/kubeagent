package collect

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// kindGVRs maps every kind a policy rule may select to the resource to list and
// whether that resource is namespaced. The set is exactly the one
// internal/rbacprofile already grants read access to, so a policy rule can
// never need a permission the shipped ClusterRole does not carry — and
// `kubeagent rbac print` keeps describing what kubeagent actually reads.
//
// Secret is absent by design and must stay absent: a rule that could read a
// Secret and quote it as evidence would turn a policy file into an
// exfiltration channel through any report the operator forwards.
var kindGVRs = map[string]kindGVR{
	// core/v1
	"ConfigMap":             {gvr("", "v1", "configmaps"), true},
	"Namespace":             {gvr("", "v1", "namespaces"), false},
	"Node":                  {gvr("", "v1", "nodes"), false},
	"PersistentVolume":      {gvr("", "v1", "persistentvolumes"), false},
	"PersistentVolumeClaim": {gvr("", "v1", "persistentvolumeclaims"), true},
	"Pod":                   {gvr("", "v1", "pods"), true},
	"ResourceQuota":         {gvr("", "v1", "resourcequotas"), true},
	"Service":               {gvr("", "v1", "services"), true},
	// apps/v1
	"DaemonSet":   {gvr("apps", "v1", "daemonsets"), true},
	"Deployment":  {gvr("apps", "v1", "deployments"), true},
	"ReplicaSet":  {gvr("apps", "v1", "replicasets"), true},
	"StatefulSet": {gvr("apps", "v1", "statefulsets"), true},
	// batch/v1
	"CronJob": {gvr("batch", "v1", "cronjobs"), true},
	"Job":     {gvr("batch", "v1", "jobs"), true},
	// discovery.k8s.io/v1
	"EndpointSlice": {gvr("discovery.k8s.io", "v1", "endpointslices"), true},
	// networking.k8s.io/v1
	"Ingress":       {gvr("networking.k8s.io", "v1", "ingresses"), true},
	"IngressClass":  {gvr("networking.k8s.io", "v1", "ingressclasses"), false},
	"NetworkPolicy": {gvr("networking.k8s.io", "v1", "networkpolicies"), true},
	// storage.k8s.io/v1
	"StorageClass": {gvr("storage.k8s.io", "v1", "storageclasses"), false},
	// policy/v1
	"PodDisruptionBudget": {gvr("policy", "v1", "poddisruptionbudgets"), true},
	// autoscaling/v2
	"HorizontalPodAutoscaler": {gvr("autoscaling", "v2", "horizontalpodautoscalers"), true},
	// admissionregistration.k8s.io/v1
	"MutatingWebhookConfiguration":   {gvr("admissionregistration.k8s.io", "v1", "mutatingwebhookconfigurations"), false},
	"ValidatingWebhookConfiguration": {gvr("admissionregistration.k8s.io", "v1", "validatingwebhookconfigurations"), false},
}

type kindGVR struct {
	resource   schema.GroupVersionResource
	namespaced bool
}

func gvr(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

// KindGVR reports the resource to list for a kind, and whether the kind is one
// ByKind will read at all.
func KindGVR(kind string) (schema.GroupVersionResource, bool) {
	e, ok := kindGVRs[kind]
	if !ok {
		return schema.GroupVersionResource{}, false
	}
	return e.resource, true
}

// ByKind lists every object of one kind, optionally scoped to a namespace.
// Read-only: a List call and nothing else.
//
// A namespace is ignored for a cluster-scoped kind rather than producing an
// empty list, so a namespaced scan still evaluates rules about Nodes and
// StorageClasses.
//
// An error means the kind could not be read — refused, unreachable, or timed
// out. The caller must treat that as "not evaluated" and never as "no
// violations": the difference between the two is the whole value of the
// policy surface. The error's text is a boolean to the caller and is never
// rendered; an API error can carry a request URL, and a URL is a credential.
func ByKind(ctx context.Context, dyn dynamic.Interface, kind, namespace string) ([]*unstructured.Unstructured, error) {
	if kind == "Secret" {
		// Belt and braces: policy.Load refuses Secret at load time. This is the
		// second lock, so no future caller can reach a Secret through here.
		return nil, fmt.Errorf("kubeagent never reads Secret objects")
	}
	e, ok := kindGVRs[kind]
	if !ok {
		return nil, fmt.Errorf("kubeagent does not read the kind %q", kind)
	}

	var ri dynamic.ResourceInterface = dyn.Resource(e.resource)
	if e.namespaced && namespace != "" {
		ri = dyn.Resource(e.resource).Namespace(namespace)
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out, nil
}
