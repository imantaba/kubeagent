// Package ingresshealth flags Ingress routes whose backend Service is missing,
// has no ready endpoints, or does not expose the referenced port — the usual
// causes of a 502/503 from an ingress controller. Pure: the caller supplies the
// Ingresses, Services, and EndpointSlices. Read-only.
package ingresshealth

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imantaba/kubeagent/internal/svchealth"
)

// RouteIssue is one broken Ingress route: a rule (or the default backend) whose
// backend Service chain is broken.
type RouteIssue struct {
	Namespace string `json:"namespace"`
	Ingress   string `json:"ingress"`
	Host      string `json:"host,omitempty"`
	Path      string `json:"path,omitempty"`
	Service   string `json:"service"`
	Port      string `json:"port,omitempty"`
	Problem   string `json:"problem"` // "NoService" | "NoEndpoints" | "PortNotExposed"
	Detail    string `json:"detail"`
}

// Assess resolves each Ingress rule's backend Service (in the Ingress's own
// namespace) and flags broken routes. Only Service backends are checked;
// Resource backends are skipped.
func Assess(ingresses []networkingv1.Ingress, services []corev1.Service, slices []discoveryv1.EndpointSlice) []RouteIssue {
	byKey := make(map[string]corev1.Service, len(services))
	for _, s := range services {
		byKey[s.Namespace+"/"+s.Name] = s
	}
	var issues []RouteIssue
	for _, ing := range ingresses {
		if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil {
			if iss, ok := check(ing.Namespace, ing.Name, "", "", *b.Service, byKey, slices); ok {
				issues = append(issues, iss)
			}
		}
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				if p.Backend.Service == nil {
					continue // Resource backend — skip
				}
				if iss, ok := check(ing.Namespace, ing.Name, rule.Host, p.Path, *p.Backend.Service, byKey, slices); ok {
					issues = append(issues, iss)
				}
			}
		}
	}
	return issues
}

func check(ns, ingName, host, path string, be networkingv1.IngressServiceBackend, byKey map[string]corev1.Service, slices []discoveryv1.EndpointSlice) (RouteIssue, bool) {
	port := portString(be.Port)
	r := RouteIssue{Namespace: ns, Ingress: ingName, Host: host, Path: path, Service: be.Name, Port: port}
	svc, ok := byKey[ns+"/"+be.Name]
	if !ok {
		r.Problem = "NoService"
		r.Detail = fmt.Sprintf("backend Service %s not found", be.Name)
		return r, true
	}
	if svchealth.ReadyEndpoints(svc, slices) == 0 {
		r.Problem = "NoEndpoints"
		if port != "" {
			r.Detail = fmt.Sprintf("backend Service %s:%s has no ready endpoints (likely 502/503)", be.Name, port)
		} else {
			r.Detail = fmt.Sprintf("backend Service %s has no ready endpoints (likely 502/503)", be.Name)
		}
		return r, true
	}
	if !portMatches(be.Port, svc) {
		r.Problem = "PortNotExposed"
		r.Detail = fmt.Sprintf("backend Service %s does not expose port %s", be.Name, port)
		return r, true
	}
	return RouteIssue{}, false
}

// portString renders a backend port as its name, else its number, else "".
func portString(p networkingv1.ServiceBackendPort) string {
	if p.Name != "" {
		return p.Name
	}
	if p.Number != 0 {
		return strconv.Itoa(int(p.Number))
	}
	return ""
}

// portMatches reports whether the backend's named/numbered port exists on the
// Service. A backend that specifies no port is never a mismatch.
func portMatches(p networkingv1.ServiceBackendPort, svc corev1.Service) bool {
	if p.Name == "" && p.Number == 0 {
		return true
	}
	for _, sp := range svc.Spec.Ports {
		if p.Name != "" && sp.Name == p.Name {
			return true
		}
		if p.Number != 0 && sp.Port == p.Number {
			return true
		}
	}
	return false
}
