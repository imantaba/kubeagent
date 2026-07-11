package ingresshealth

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func svc(ns, name string, ports ...int32) corev1.Service {
	var sp []corev1.ServicePort
	for _, p := range ports {
		sp = append(sp, corev1.ServicePort{Port: p})
	}
	return corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{Ports: sp}}
}

// sliceFor builds an EndpointSlice with `ready` ready addresses for a service.
func sliceFor(ns, svcName string, ready int) discoveryv1.EndpointSlice {
	t := true
	var eps []discoveryv1.Endpoint
	for i := 0; i < ready; i++ {
		eps = append(eps, discoveryv1.Endpoint{Conditions: discoveryv1.EndpointConditions{Ready: &t}})
	}
	return discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: svcName + "-abc", Labels: map[string]string{"kubernetes.io/service-name": svcName}},
		Endpoints:  eps,
	}
}

// ing builds an Ingress with a single host/path rule to a service:port(number).
func ing(ns, name, host, path, svcName string, portNum int32) networkingv1.Ingress {
	return networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: path,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: svcName,
						Port: networkingv1.ServiceBackendPort{Number: portNum},
					}},
				}},
			}},
		}}},
	}
}

func TestAssess_NoService(t *testing.T) {
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "missing", 80)}, nil, nil)
	if len(got) != 1 || got[0].Problem != "NoService" {
		t.Fatalf("want one NoService, got %+v", got)
	}
	if got[0].Namespace != "shop" || got[0].Ingress != "web" || got[0].Service != "missing" || got[0].Host != "x.io" || got[0].Path != "/api" {
		t.Errorf("wrong row: %+v", got[0])
	}
}

func TestAssess_NoEndpoints(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "api", 80)}, svcs, nil) // no slices -> 0 ready
	if len(got) != 1 || got[0].Problem != "NoEndpoints" {
		t.Fatalf("want one NoEndpoints, got %+v", got)
	}
	if got[0].Port != "80" {
		t.Errorf("want port 80, got %q", got[0].Port)
	}
}

func TestAssess_PortNotExposed(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	slices := []discoveryv1.EndpointSlice{sliceFor("shop", "api", 1)}
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "api", 8080)}, svcs, slices) // ready, but 8080 not exposed
	if len(got) != 1 || got[0].Problem != "PortNotExposed" {
		t.Fatalf("want one PortNotExposed, got %+v", got)
	}
}

func TestAssess_HealthyRouteNoIssue(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	slices := []discoveryv1.EndpointSlice{sliceFor("shop", "api", 2)}
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "api", 80)}, svcs, slices)
	if len(got) != 0 {
		t.Fatalf("healthy route should yield no issue, got %+v", got)
	}
}

func TestAssess_NamedPortMatch(t *testing.T) {
	s := corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}}
	slices := []discoveryv1.EndpointSlice{sliceFor("shop", "api", 1)}
	in := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "x.io",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{Path: "/", Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api", Port: networkingv1.ServiceBackendPort{Name: "http"}}}}},
			}},
		}}},
	}
	if got := Assess([]networkingv1.Ingress{in}, []corev1.Service{s}, slices); len(got) != 0 {
		t.Errorf("named port 'http' should match, got %+v", got)
	}
}

func TestAssess_DefaultBackendChecked(t *testing.T) {
	in := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{Name: "fallback", Port: networkingv1.ServiceBackendPort{Number: 80}},
		}},
	}
	got := Assess([]networkingv1.Ingress{in}, nil, nil)
	if len(got) != 1 || got[0].Problem != "NoService" || got[0].Service != "fallback" || got[0].Host != "" || got[0].Path != "" {
		t.Fatalf("default backend should be checked, got %+v", got)
	}
}

func TestAssess_ResourceBackendSkipped(t *testing.T) {
	in := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{Path: "/", Backend: networkingv1.IngressBackend{Resource: &corev1.TypedLocalObjectReference{Kind: "StorageBucket", Name: "assets"}}}},
			}},
		}}},
	}
	if got := Assess([]networkingv1.Ingress{in}, nil, nil); len(got) != 0 {
		t.Errorf("resource backend must be skipped, got %+v", got)
	}
}
