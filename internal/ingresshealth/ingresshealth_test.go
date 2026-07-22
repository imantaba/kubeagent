package ingresshealth

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/svchealth"
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
		eps = append(eps, discoveryv1.Endpoint{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &t}})
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
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "missing", 80)}, nil, nil, nil, nil, nil)
	if len(got) != 1 || got[0].Problem != "NoService" {
		t.Fatalf("want one NoService, got %+v", got)
	}
	if got[0].Namespace != "shop" || got[0].Ingress != "web" || got[0].Service != "missing" || got[0].Host != "x.io" || got[0].Path != "/api" {
		t.Errorf("wrong row: %+v", got[0])
	}
}

func TestAssess_NoEndpoints(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "api", 80)}, svcs, nil, nil, nil, nil) // no slices -> 0 ready
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
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "api", 8080)}, svcs, slices, nil, nil, nil) // ready, but 8080 not exposed
	if len(got) != 1 || got[0].Problem != "PortNotExposed" {
		t.Fatalf("want one PortNotExposed, got %+v", got)
	}
}

func TestAssess_HealthyRouteNoIssue(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	slices := []discoveryv1.EndpointSlice{sliceFor("shop", "api", 2)}
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "api", 80)}, svcs, slices, nil, nil, nil)
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
	if got := Assess([]networkingv1.Ingress{in}, []corev1.Service{s}, slices, nil, nil, nil); len(got) != 0 {
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
	got := Assess([]networkingv1.Ingress{in}, nil, nil, nil, nil, nil)
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
	if got := Assess([]networkingv1.Ingress{in}, nil, nil, nil, nil, nil); len(got) != 0 {
		t.Errorf("resource backend must be skipped, got %+v", got)
	}
}

// TestAssess_NoPortBackend_NotPortNotExposed guards the binding constraint that
// a backend specifying no port is never a PortNotExposed candidate, even when
// the Service is otherwise healthy. ing(...,0) yields a zero-value backend port.
func TestAssess_NoPortBackend_NotPortNotExposed(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	slices := []discoveryv1.EndpointSlice{sliceFor("shop", "api", 1)}
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/", "api", 0)}, svcs, slices, nil, nil, nil)
	if len(got) != 0 {
		t.Fatalf("no-port backend with a ready service must not be flagged, got %+v", got)
	}
}

func TestAssess_ExpectedEmpty_ScaledToZeroBackend(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "web", 80)} // helper builds a Service with port 80
	svcs[0].Spec.Selector = map[string]string{"app": "web"}
	backends := []svchealth.Backend{{Kind: "Deployment", Namespace: "shop", TemplateLabels: map[string]string{"app": "web"}, Desired: 0}}
	got := Assess([]networkingv1.Ingress{ing("shop", "site", "x.io", "/", "web", 80)}, svcs, nil, backends, nil, nil) // no slices -> 0 ready
	if len(got) != 1 || !got[0].Expected {
		t.Fatalf("route to a scaled-to-0 backend must be Expected, got %+v", got)
	}
	if !strings.Contains(got[0].Detail, "route parked") {
		t.Errorf("expected 'route parked' detail, got %q", got[0].Detail)
	}
}

func TestAssess_ExpectedEmpty_Annotated(t *testing.T) {
	svcs := []corev1.Service{svc("db", "pg-ro", 5432)}
	svcs[0].Spec.Selector = map[string]string{"role": "replica"}
	svcs[0].Annotations = map[string]string{svchealth.ExpectedEmptyAnnotation: "true"}
	got := Assess([]networkingv1.Ingress{ing("db", "pg", "pg.io", "/", "pg-ro", 5432)}, svcs, nil, nil, nil, nil)
	if len(got) != 1 || !got[0].Expected {
		t.Fatalf("route to an annotated Service must be Expected, got %+v", got)
	}
}

func TestAssess_GenuinelyBroken_NotExpected(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	svcs[0].Spec.Selector = map[string]string{"app": "api"}
	backends := []svchealth.Backend{{Kind: "Deployment", Namespace: "shop", TemplateLabels: map[string]string{"app": "api"}, Desired: 3}} // live, 0 ready
	got := Assess([]networkingv1.Ingress{ing("shop", "site", "x.io", "/", "api", 80)}, svcs, nil, backends, nil, nil)
	if len(got) != 1 || got[0].Expected {
		t.Fatalf("route to a live backend with 0 endpoints is a real issue, got %+v", got)
	}
	if !strings.Contains(got[0].Detail, "502/503") {
		t.Errorf("expected '502/503' detail, got %q", got[0].Detail)
	}
}

// svcSel builds a selector-bearing backend Service (the file's existing svc()
// helper has no selector).
func svcSel(ns, name string, selector map[string]string, port int32) corev1.Service {
	return corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Selector: selector, Ports: []corev1.ServicePort{{Port: port}}},
	}
}

func podLabeled(ns, name, node string, labels map[string]string, ready bool) corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
	p.Spec.NodeName = node
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	return p
}

// ingressTo builds a single-rule Ingress routing host/ to svcName:port.
func ingressTo(ns, name, host, svcName string, port int32) networkingv1.Ingress {
	return networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: "/",
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: svcName, Port: networkingv1.ServiceBackendPort{Number: port},
					}},
				}},
			}},
		}}},
	}
}

func firstDetail(t *testing.T, issues []RouteIssue) string {
	t.Helper()
	if len(issues) != 1 {
		t.Fatalf("want 1 route issue, got %d: %+v", len(issues), issues)
	}
	return issues[0].Detail
}

func TestAssess_BrokenRoute_NoPodsCause(t *testing.T) {
	ing := ingressTo("shop", "web-ing", "web.example.com", "web", 80)
	services := []corev1.Service{svcSel("shop", "web", map[string]string{"app": "web"}, 80)}
	got := firstDetail(t, Assess([]networkingv1.Ingress{ing}, services, nil, nil, nil, nil))
	if got != "backend Service web:80 has no ready endpoints (likely 502/503) — the selector matches no pods" {
		t.Fatalf("detail = %q", got)
	}
}

func TestAssess_BrokenRoute_NodeDownCause(t *testing.T) {
	ing := ingressTo("shop", "api-ing", "api.example.com", "api", 80)
	services := []corev1.Service{svcSel("shop", "api", map[string]string{"app": "api"}, 80)}
	pods := []corev1.Pod{podLabeled("shop", "api-1", "worker-2", map[string]string{"app": "api"}, false)}
	down := []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}}
	got := firstDetail(t, Assess([]networkingv1.Ingress{ing}, services, nil, nil, pods, down))
	if got != "backend Service api:80 has no ready endpoints (likely 502/503) — matching pods on down node worker-2 (NotReady)" {
		t.Fatalf("detail = %q", got)
	}
}

func TestAssess_BrokenRoute_PodsNotReadyCause(t *testing.T) {
	ing := ingressTo("shop", "api-ing", "api.example.com", "api", 80)
	services := []corev1.Service{svcSel("shop", "api", map[string]string{"app": "api"}, 80)}
	pods := []corev1.Pod{
		podLabeled("shop", "api-1", "worker-1", map[string]string{"app": "api"}, false),
		podLabeled("shop", "api-2", "worker-1", map[string]string{"app": "api"}, false),
		podLabeled("shop", "api-3", "worker-1", map[string]string{"app": "api"}, false),
	}
	got := firstDetail(t, Assess([]networkingv1.Ingress{ing}, services, nil, nil, pods, nil))
	if got != "backend Service api:80 has no ready endpoints (likely 502/503) — 3 matching pods, 0 ready" {
		t.Fatalf("detail = %q", got)
	}
}

func TestAssess_BrokenRoute_InconclusiveLeavesBase(t *testing.T) {
	ing := ingressTo("shop", "api-ing", "api.example.com", "api", 80)
	services := []corev1.Service{svcSel("shop", "api", map[string]string{"app": "api"}, 80)}
	pods := []corev1.Pod{podLabeled("shop", "api-1", "worker-1", map[string]string{"app": "api"}, true)} // ready
	got := firstDetail(t, Assess([]networkingv1.Ingress{ing}, services, nil, nil, pods, nil))
	if got != "backend Service api:80 has no ready endpoints (likely 502/503)" {
		t.Fatalf("detail = %q", got)
	}
}

func TestAssess_NoServiceRoute_NotEnriched(t *testing.T) {
	ing := ingressTo("shop", "ghost-ing", "ghost.example.com", "ghost", 80)
	got := firstDetail(t, Assess([]networkingv1.Ingress{ing}, nil, nil, nil, nil, nil))
	if got != "backend Service ghost not found" {
		t.Fatalf("NoService route must not be enriched, got %q", got)
	}
}

// TestAssess_ParkedRoute_NotEnrichedEvenWithPods pins that a parked route
// (scaled-to-zero backend → svchealth.ExpectedEmpty returns ok) returns
// Expected=true and a "route parked" Detail even when matching unready pods
// exist that would otherwise produce a "N matching pods, 0 ready" cause suffix
// on a genuinely broken route.
func TestAssess_ParkedRoute_NotEnrichedEvenWithPods(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "web", 80)}
	svcs[0].Spec.Selector = map[string]string{"app": "web"}
	backends := []svchealth.Backend{{Kind: "Deployment", Namespace: "shop", TemplateLabels: map[string]string{"app": "web"}, Desired: 0}}
	pods := []corev1.Pod{
		podLabeled("shop", "web-1", "worker-1", map[string]string{"app": "web"}, false),
		podLabeled("shop", "web-2", "worker-1", map[string]string{"app": "web"}, false),
	}
	got := Assess([]networkingv1.Ingress{ing("shop", "site", "x.io", "/", "web", 80)}, svcs, nil, backends, pods, nil)
	if len(got) != 1 || !got[0].Expected {
		t.Fatalf("parked route must be Expected even with matching pods, got %+v", got)
	}
	detail := got[0].Detail
	if !strings.Contains(detail, "route parked") {
		t.Errorf("expected 'route parked' in detail, got %q", detail)
	}
	if strings.Contains(detail, "0 ready") {
		t.Errorf("parked route must not be enriched with pod cause, got %q", detail)
	}
}
