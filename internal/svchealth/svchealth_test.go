package svchealth

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func svc(ns, name string, t corev1.ServiceType, selector map[string]string, lbIngress int) corev1.Service {
	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Type: t, Selector: selector},
	}
	for i := 0; i < lbIngress; i++ {
		s.Status.LoadBalancer.Ingress = append(s.Status.LoadBalancer.Ingress, corev1.LoadBalancerIngress{IP: "1.2.3.4"})
	}
	return s
}

func slice(ns, svcName string, readyStates ...*bool) discoveryv1.EndpointSlice {
	es := discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: svcName + "-abc", Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
	}
	for _, r := range readyStates {
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: r}})
	}
	return es
}

func boolp(b bool) *bool { return &b }

func TestAssess_NoEndpoints(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	// a slice exists but all endpoints not-ready
	slices := []discoveryv1.EndpointSlice{slice("default", "web", boolp(false))}
	got := Assess(svcs, slices)
	if len(got) != 1 || got[0].Problem != "NoEndpoints" || got[0].Type != "ClusterIP" || got[0].Detail != "no ready endpoints" {
		t.Fatalf("want one NoEndpoints issue, got %+v", got)
	}
}

func TestAssess_HasReadyEndpoints_NoIssue(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	slices := []discoveryv1.EndpointSlice{slice("default", "web", boolp(true), nil)} // one ready, one nil(=ready)
	if got := Assess(svcs, slices); len(got) != 0 {
		t.Fatalf("ready endpoints should yield no issue, got %+v", got)
	}
}

func TestAssess_NilReadyCountsAsReady(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	slices := []discoveryv1.EndpointSlice{slice("default", "web", nil)} // nil Ready => ready
	if got := Assess(svcs, slices); len(got) != 0 {
		t.Fatalf("nil Ready should count as ready, got %+v", got)
	}
}

func TestAssess_ExternalNameAndSelectorlessSkipped(t *testing.T) {
	svcs := []corev1.Service{
		svc("default", "ext", corev1.ServiceTypeExternalName, nil, 0),
		svc("default", "manual", corev1.ServiceTypeClusterIP, nil, 0), // no selector
	}
	if got := Assess(svcs, nil); len(got) != 0 {
		t.Fatalf("ExternalName and selectorless services must be skipped, got %+v", got)
	}
}

func TestAssess_LoadBalancerNoAddress(t *testing.T) {
	s := svc("prod", "api-lb", corev1.ServiceTypeLoadBalancer, map[string]string{"app": "api"}, 0)
	s.CreationTimestamp = metav1.NewTime(time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC))
	svcs := []corev1.Service{s}
	slices := []discoveryv1.EndpointSlice{slice("prod", "api-lb", boolp(true))}
	got := Assess(svcs, slices)
	if len(got) != 1 || got[0].Problem != "NoExternalAddress" || got[0].Detail != "no external address" {
		t.Fatalf("want one NoExternalAddress issue, got %+v", got)
	}
	if got[0].Since != "2026-06-29T00:00:00Z" {
		t.Errorf("Since = %q, want 2026-06-29T00:00:00Z", got[0].Since)
	}
}

func TestAssess_LoadBalancerWithAddress_NoIssue(t *testing.T) {
	svcs := []corev1.Service{svc("prod", "api-lb", corev1.ServiceTypeLoadBalancer, map[string]string{"app": "api"}, 1)}
	slices := []discoveryv1.EndpointSlice{slice("prod", "api-lb", boolp(true))}
	if got := Assess(svcs, slices); len(got) != 0 {
		t.Fatalf("LB with an address and endpoints should have no issue, got %+v", got)
	}
}

func TestAssess_LoadBalancerNoAddressAndNoEndpoints_TwoIssues(t *testing.T) {
	svcs := []corev1.Service{svc("prod", "api-lb", corev1.ServiceTypeLoadBalancer, map[string]string{"app": "api"}, 0)}
	got := Assess(svcs, nil) // no slices => no endpoints
	if len(got) != 2 {
		t.Fatalf("want two issues (no address + no endpoints), got %+v", got)
	}
	// sorted by Problem: "NoEndpoints" < "NoExternalAddress"
	if got[0].Problem != "NoEndpoints" || got[1].Problem != "NoExternalAddress" {
		t.Errorf("issues should be sorted by problem, got %s then %s", got[0].Problem, got[1].Problem)
	}
}

func TestAssess_SortedByNamespaceName(t *testing.T) {
	svcs := []corev1.Service{
		svc("b", "z", corev1.ServiceTypeClusterIP, map[string]string{"a": "b"}, 0),
		svc("a", "y", corev1.ServiceTypeClusterIP, map[string]string{"a": "b"}, 0),
	}
	got := Assess(svcs, nil)
	if len(got) != 2 || got[0].Namespace != "a" || got[1].Namespace != "b" {
		t.Fatalf("want sorted by namespace, got %+v", got)
	}
}
