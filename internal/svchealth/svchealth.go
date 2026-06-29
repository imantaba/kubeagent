// Package svchealth flags Service-level problems a pod/workload scan misses: a
// selector-based Service with no ready backend endpoints, and a LoadBalancer
// Service with no external address. It is pure — the caller supplies the
// Services and EndpointSlices.
package svchealth

import (
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
)

// Issue is one Service-level problem.
type Issue struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Type      string `json:"type"`            // ClusterIP | NodePort | LoadBalancer
	Problem   string `json:"problem"`         // "NoEndpoints" | "NoExternalAddress"
	Detail    string `json:"detail"`          // human one-liner
	Since     string `json:"since,omitempty"` // RFC3339 service creationTimestamp (LB age)
}

// Assess flags Service problems. One Issue per (service, problem); a LoadBalancer
// with no address AND no endpoints yields two. Result is sorted by
// (Namespace, Name, Problem). ExternalName and selectorless Services are skipped.
func Assess(services []corev1.Service, slices []discoveryv1.EndpointSlice) []Issue {
	var out []Issue
	for _, s := range services {
		if s.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}
		if s.Spec.Type == corev1.ServiceTypeLoadBalancer && len(s.Status.LoadBalancer.Ingress) == 0 {
			lb := Issue{
				Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
				Problem: "NoExternalAddress", Detail: "no external address",
			}
			if !s.CreationTimestamp.IsZero() {
				lb.Since = s.CreationTimestamp.Time.UTC().Format(time.RFC3339)
			}
			out = append(out, lb)
		}
		if len(s.Spec.Selector) == 0 {
			continue
		}
		if readyEndpoints(s, slices) == 0 {
			out = append(out, Issue{
				Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
				Problem: "NoEndpoints", Detail: "no ready endpoints",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Problem < out[j].Problem
	})
	return out
}

// readyEndpoints counts ready backend addresses for a Service across its
// EndpointSlices (matched by namespace + the kubernetes.io/service-name label).
func readyEndpoints(svc corev1.Service, slices []discoveryv1.EndpointSlice) int {
	total := 0
	for _, sl := range slices {
		if sl.Namespace != svc.Namespace || sl.Labels[discoveryv1.LabelServiceName] != svc.Name {
			continue
		}
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				total += len(ep.Addresses)
			}
		}
	}
	return total
}
