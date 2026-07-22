package svchealth

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
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
	got := Assess(svcs, slices, nil)
	if len(got) != 1 || got[0].Problem != "NoEndpoints" || got[0].Type != "ClusterIP" || got[0].Detail != "no ready endpoints" {
		t.Fatalf("want one NoEndpoints issue, got %+v", got)
	}
}

func TestAssess_HasReadyEndpoints_NoIssue(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	slices := []discoveryv1.EndpointSlice{slice("default", "web", boolp(true), nil)} // one ready, one nil(=ready)
	if got := Assess(svcs, slices, nil); len(got) != 0 {
		t.Fatalf("ready endpoints should yield no issue, got %+v", got)
	}
}

func TestAssess_NilReadyCountsAsReady(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	slices := []discoveryv1.EndpointSlice{slice("default", "web", nil)} // nil Ready => ready
	if got := Assess(svcs, slices, nil); len(got) != 0 {
		t.Fatalf("nil Ready should count as ready, got %+v", got)
	}
}

func TestAssess_ExternalNameAndSelectorlessSkipped(t *testing.T) {
	svcs := []corev1.Service{
		svc("default", "ext", corev1.ServiceTypeExternalName, nil, 0),
		svc("default", "manual", corev1.ServiceTypeClusterIP, nil, 0), // no selector
	}
	if got := Assess(svcs, nil, nil); len(got) != 0 {
		t.Fatalf("ExternalName and selectorless services must be skipped, got %+v", got)
	}
}

func TestAssess_LoadBalancerNoAddress(t *testing.T) {
	s := svc("prod", "api-lb", corev1.ServiceTypeLoadBalancer, map[string]string{"app": "api"}, 0)
	s.CreationTimestamp = metav1.NewTime(time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC))
	svcs := []corev1.Service{s}
	slices := []discoveryv1.EndpointSlice{slice("prod", "api-lb", boolp(true))}
	got := Assess(svcs, slices, nil)
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
	if got := Assess(svcs, slices, nil); len(got) != 0 {
		t.Fatalf("LB with an address and endpoints should have no issue, got %+v", got)
	}
}

func TestAssess_LoadBalancerNoAddressAndNoEndpoints_TwoIssues(t *testing.T) {
	svcs := []corev1.Service{svc("prod", "api-lb", corev1.ServiceTypeLoadBalancer, map[string]string{"app": "api"}, 0)}
	got := Assess(svcs, nil, nil) // no slices => no endpoints
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
	got := Assess(svcs, nil, nil)
	if len(got) != 2 || got[0].Namespace != "a" || got[1].Namespace != "b" {
		t.Fatalf("want sorted by namespace, got %+v", got)
	}
}

func int32p(i int32) *int32 { return &i }

func TestSelectorMatches(t *testing.T) {
	cases := []struct {
		name        string
		sel, labels map[string]string
		want        bool
	}{
		{"subset", map[string]string{"app": "web"}, map[string]string{"app": "web", "tier": "fe"}, true},
		{"missing key", map[string]string{"app": "web"}, map[string]string{"tier": "fe"}, false},
		{"value mismatch", map[string]string{"app": "web"}, map[string]string{"app": "api"}, false},
		{"empty selector", map[string]string{}, map[string]string{"app": "web"}, false},
		{"nil labels", map[string]string{"app": "web"}, nil, false},
	}
	for _, c := range cases {
		if got := selectorMatches(c.sel, c.labels); got != c.want {
			t.Errorf("%s: selectorMatches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBackendsFrom(t *testing.T) {
	deploys := []appsv1.Deployment{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "d1"},
			Spec: appsv1.DeploymentSpec{Replicas: int32p(3),
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "d"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "d2"}, // nil replicas → 1
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "d2"}}}}},
	}
	sts := []appsv1.StatefulSet{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s1"},
			Spec: appsv1.StatefulSetSpec{Replicas: int32p(0),
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "s"}}}}},
	}
	ds := []appsv1.DaemonSet{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "ds1"},
			Spec:   appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ds"}}}},
			Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 0}},
	}
	jobs := []batchv1.Job{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "j1"},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "j"}}}}},
	}
	cronjobs := []batchv1.CronJob{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "cj1"},
			Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "cj"}}}}}}},
	}

	got := BackendsFrom(deploys, sts, ds, jobs, cronjobs)
	if len(got) != 6 {
		t.Fatalf("want 6 backends, got %d: %+v", len(got), got)
	}
	by := map[string]Backend{}
	for _, b := range got {
		by[b.Kind+"/"+b.Namespace+"/"+labelVal(b.TemplateLabels)] = b
	}
	if b := by["Deployment/a/d"]; b.Desired != 3 || b.Ephemeral {
		t.Errorf("deploy d1: want Desired 3, not ephemeral, got %+v", b)
	}
	if b := by["Deployment/a/d2"]; b.Desired != 1 {
		t.Errorf("deploy d2 nil replicas: want Desired 1, got %+v", b)
	}
	if b := by["StatefulSet/a/s"]; b.Desired != 0 || b.Ephemeral {
		t.Errorf("sts: want Desired 0, not ephemeral, got %+v", b)
	}
	if b := by["DaemonSet/a/ds"]; b.Desired != 0 || b.Ephemeral {
		t.Errorf("ds: want Desired 0 from status, not ephemeral, got %+v", b)
	}
	if b := by["Job/a/j"]; !b.Ephemeral {
		t.Errorf("job: want Ephemeral true, got %+v", b)
	}
	if b := by["CronJob/a/cj"]; !b.Ephemeral {
		t.Errorf("cronjob: want Ephemeral true, got %+v", b)
	}
}

// labelVal returns the single label value (test helper for stable map keys).
func labelVal(m map[string]string) string {
	for _, v := range m {
		return v
	}
	return ""
}

// backend is a terse Backend literal for classification tests.
func backend(kind, ns string, desired int, ephemeral bool, labels map[string]string) Backend {
	return Backend{Kind: kind, Namespace: ns, TemplateLabels: labels, Desired: desired, Ephemeral: ephemeral}
}

func TestAssess_ExpectedBackings(t *testing.T) {
	sel := map[string]string{"app": "x"}
	cases := []struct {
		name        string
		be          Backend
		wantBacking string
		wantDetail  string
	}{
		{"cronjob", backend("CronJob", "default", 0, true, map[string]string{"app": "x"}), "CronJob", "no ready endpoints (backs CronJob — expected between runs)"},
		{"job", backend("Job", "default", 0, true, map[string]string{"app": "x"}), "Job", "no ready endpoints (backs Job — expected between runs)"},
		{"daemonset 0", backend("DaemonSet", "default", 0, false, map[string]string{"app": "x"}), "DaemonSet", "no ready endpoints (backs DaemonSet — 0 desired)"},
		{"deployment 0", backend("Deployment", "default", 0, false, map[string]string{"app": "x"}), "Deployment", "no ready endpoints (backs Deployment — scaled to 0)"},
		{"statefulset 0", backend("StatefulSet", "default", 0, false, map[string]string{"app": "x"}), "StatefulSet", "no ready endpoints (backs StatefulSet — scaled to 0)"},
	}
	for _, c := range cases {
		svcs := []corev1.Service{svc("default", "x", corev1.ServiceTypeClusterIP, sel, 0)}
		got := Assess(svcs, nil, []Backend{c.be})
		if len(got) != 1 {
			t.Fatalf("%s: want 1 issue, got %+v", c.name, got)
		}
		if !got[0].Expected || got[0].Backing != c.wantBacking || got[0].Detail != c.wantDetail {
			t.Errorf("%s: got Expected=%v Backing=%q Detail=%q", c.name, got[0].Expected, got[0].Backing, got[0].Detail)
		}
	}
}

func TestAssess_LiveDeploymentStaysPrimary(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	backends := []Backend{backend("Deployment", "default", 3, false, map[string]string{"app": "web"})}
	got := Assess(svcs, nil, backends)
	if len(got) != 1 || got[0].Expected || got[0].Detail != "no ready endpoints" || got[0].Backing != "" {
		t.Fatalf("a live (desired>0) Deployment with no endpoints must stay primary, got %+v", got)
	}
}

func TestAssess_RealOutageWinsOverCoincidentalJob(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	backends := []Backend{
		backend("Deployment", "default", 3, false, map[string]string{"app": "web"}),
		backend("CronJob", "default", 0, true, map[string]string{"app": "web"}),
	}
	got := Assess(svcs, nil, backends)
	if len(got) != 1 || got[0].Expected {
		t.Fatalf("a live Deployment match must keep the issue primary even if a job also matches, got %+v", got)
	}
}

func TestAssess_NoMatchingBackendStaysPrimary(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	backends := []Backend{backend("CronJob", "default", 0, true, map[string]string{"app": "other"})}
	got := Assess(svcs, nil, backends)
	if len(got) != 1 || got[0].Expected || got[0].Detail != "no ready endpoints" {
		t.Fatalf("a non-matching backend must leave the issue primary, got %+v", got)
	}
}

func TestIssue_JSONOmitsEmptyExpectedAndBacking(t *testing.T) {
	primary, _ := json.Marshal(Issue{Namespace: "n", Name: "x", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"})
	if strings.Contains(string(primary), "expected") || strings.Contains(string(primary), "backing") {
		t.Errorf("primary issue JSON must omit expected/backing: %s", primary)
	}
	expected, _ := json.Marshal(Issue{Namespace: "n", Name: "x", Problem: "NoEndpoints", Expected: true, Backing: "CronJob"})
	if !strings.Contains(string(expected), `"expected":true`) || !strings.Contains(string(expected), `"backing":"CronJob"`) {
		t.Errorf("expected issue JSON must carry expected/backing: %s", expected)
	}
}

func TestAssess_LiveDaemonSetStaysPrimary(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	backends := []Backend{backend("DaemonSet", "default", 3, false, map[string]string{"app": "web"})}
	got := Assess(svcs, nil, backends)
	if len(got) != 1 || got[0].Expected || got[0].Detail != "no ready endpoints" {
		t.Fatalf("DaemonSet desired>0 must stay primary, got %+v", got)
	}
}

func TestAssess_LiveStatefulSetStaysPrimary(t *testing.T) {
	svcs := []corev1.Service{svc("default", "db", corev1.ServiceTypeClusterIP, map[string]string{"app": "db"}, 0)}
	backends := []Backend{backend("StatefulSet", "default", 2, false, map[string]string{"app": "db"})}
	got := Assess(svcs, nil, backends)
	if len(got) != 1 || got[0].Expected || got[0].Detail != "no ready endpoints" {
		t.Fatalf("StatefulSet desired>0 must stay primary, got %+v", got)
	}
}

func TestAssess_BackendInDifferentNamespaceIgnored(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	backends := []Backend{backend("CronJob", "ns-other", 0, true, map[string]string{"app": "web"})}
	got := Assess(svcs, nil, backends)
	if len(got) != 1 || got[0].Expected {
		t.Fatalf("a backend in a different namespace must not classify the issue as expected, got %+v", got)
	}
}

func TestExpectedEmpty_Annotation(t *testing.T) {
	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg-ro",
			Annotations: map[string]string{ExpectedEmptyAnnotation: "true"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"role": "replica"}},
	}
	// No backend at all, and even with a LIVE backend the annotation must win (absolute override).
	live := []Backend{{Kind: "Deployment", Namespace: "db", TemplateLabels: map[string]string{"role": "replica"}, Desired: 3}}
	for _, backends := range [][]Backend{nil, live} {
		reason, ok := ExpectedEmpty(s, backends)
		if !ok {
			t.Fatalf("annotated Service must be expected-empty (backends=%v)", backends)
		}
		if !strings.Contains(reason, ExpectedEmptyAnnotation) {
			t.Errorf("reason %q should name the annotation", reason)
		}
	}
}

func TestExpectedEmpty_AnnotationSetsIssueExpected(t *testing.T) {
	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg-ro",
			Annotations: map[string]string{ExpectedEmptyAnnotation: "true"}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"role": "replica"}},
	}
	got := Assess([]corev1.Service{s}, nil, nil) // no slices -> 0 endpoints
	if len(got) != 1 || !got[0].Expected {
		t.Fatalf("annotated empty Service must yield one Expected issue, got %+v", got)
	}
}

func TestExpectedEmpty_NotAnnotatedNoBacking(t *testing.T) {
	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "pg-ro"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"role": "replica"}},
	}
	if _, ok := ExpectedEmpty(s, nil); ok {
		t.Error("a Service with no annotation and no backing must not be expected-empty")
	}
}

func TestExpectedEmpty_ScaledToZeroBacking(t *testing.T) {
	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: map[string]string{"app": "web"}},
	}
	backends := []Backend{{Kind: "Deployment", Namespace: "shop", TemplateLabels: map[string]string{"app": "web"}, Desired: 0}}
	reason, ok := ExpectedEmpty(s, backends)
	if !ok || !strings.Contains(reason, "scaled to 0") {
		t.Fatalf("scaled-to-0 Deployment backing should be expected with 'scaled to 0', got %q ok=%v", reason, ok)
	}
}

func pod(ns, name, node string, labels map[string]string, ready bool) corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
	p.Spec.NodeName = node
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	return p
}

func brokenNoEndpoints(ns, name string) Issue {
	return Issue{Namespace: ns, Name: name, Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"}
}

func TestAnnotateEndpointCause_NoPods(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "web")}
	services := []corev1.Service{svc("shop", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	AnnotateEndpointCause(issues, services, nil, nil)
	if issues[0].Detail != "no ready endpoints — the selector matches no pods" {
		t.Fatalf("detail = %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_NodeDownSingle(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "cache")}
	services := []corev1.Service{svc("shop", "cache", corev1.ServiceTypeClusterIP, map[string]string{"app": "cache"}, 0)}
	pods := []corev1.Pod{
		pod("shop", "cache-1", "worker-2", map[string]string{"app": "cache"}, false),
		pod("shop", "cache-2", "worker-2", map[string]string{"app": "cache"}, false),
	}
	down := []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}}
	AnnotateEndpointCause(issues, services, pods, down)
	if issues[0].Detail != "no ready endpoints — matching pods on down node worker-2 (NotReady)" {
		t.Fatalf("detail = %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_NodeDownMultiple(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "cache")}
	services := []corev1.Service{svc("shop", "cache", corev1.ServiceTypeClusterIP, map[string]string{"app": "cache"}, 0)}
	pods := []corev1.Pod{
		pod("shop", "cache-1", "worker-2", map[string]string{"app": "cache"}, false),
		pod("shop", "cache-2", "worker-3", map[string]string{"app": "cache"}, false),
	}
	down := []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}, {Name: "worker-3", Reason: "NotReady"}}
	AnnotateEndpointCause(issues, services, pods, down)
	if issues[0].Detail != "no ready endpoints — matching pods on 2 down nodes" {
		t.Fatalf("detail = %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_PodsNotReady(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "api")}
	services := []corev1.Service{svc("shop", "api", corev1.ServiceTypeClusterIP, map[string]string{"app": "api"}, 0)}
	pods := []corev1.Pod{
		pod("shop", "api-1", "worker-1", map[string]string{"app": "api"}, false),
		pod("shop", "api-2", "worker-1", map[string]string{"app": "api"}, false),
		pod("shop", "api-3", "worker-1", map[string]string{"app": "api"}, false),
	}
	AnnotateEndpointCause(issues, services, pods, nil) // worker-1 not down
	if issues[0].Detail != "no ready endpoints — 3 matching pods, 0 ready" {
		t.Fatalf("detail = %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_SinglePodNounSingular(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "api")}
	services := []corev1.Service{svc("shop", "api", corev1.ServiceTypeClusterIP, map[string]string{"app": "api"}, 0)}
	pods := []corev1.Pod{pod("shop", "api-1", "worker-1", map[string]string{"app": "api"}, false)}
	AnnotateEndpointCause(issues, services, pods, nil)
	if issues[0].Detail != "no ready endpoints — 1 matching pod, 0 ready" {
		t.Fatalf("detail = %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_LeavesExpectedAndReadyAndOtherProblems(t *testing.T) {
	services := []corev1.Service{
		svc("shop", "sched-zero", corev1.ServiceTypeClusterIP, map[string]string{"app": "sz"}, 0),
		svc("shop", "healthy", corev1.ServiceTypeClusterIP, map[string]string{"app": "h"}, 0),
	}
	pods := []corev1.Pod{pod("shop", "h-1", "worker-1", map[string]string{"app": "h"}, true)} // ready
	issues := []Issue{
		{Namespace: "shop", Name: "sched-zero", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints — declared via …", Expected: true},
		{Namespace: "shop", Name: "healthy", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"}, // has a ready matching pod → inconclusive, leave
		{Namespace: "shop", Name: "lb", Type: "LoadBalancer", Problem: "NoExternalAddress", Detail: "no external address"},
	}
	AnnotateEndpointCause(issues, services, pods, nil)
	if issues[0].Detail != "no ready endpoints — declared via …" {
		t.Errorf("expected-empty must be untouched, got %q", issues[0].Detail)
	}
	if issues[1].Detail != "no ready endpoints" {
		t.Errorf("a ready matching pod is inconclusive; leave detail, got %q", issues[1].Detail)
	}
	if issues[2].Detail != "no external address" {
		t.Errorf("NoExternalAddress must be untouched, got %q", issues[2].Detail)
	}
}

func TestAnnotateEndpointCause_UnscheduledPodNotNodeDown(t *testing.T) {
	// A matching pod with no nodeName (Pending) must fall through to pods-not-ready,
	// never node-down — even when down nodes exist.
	issues := []Issue{brokenNoEndpoints("shop", "api")}
	services := []corev1.Service{svc("shop", "api", corev1.ServiceTypeClusterIP, map[string]string{"app": "api"}, 0)}
	pods := []corev1.Pod{pod("shop", "api-1", "", map[string]string{"app": "api"}, false)} // empty nodeName
	down := []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}}
	AnnotateEndpointCause(issues, services, pods, down)
	if issues[0].Detail != "no ready endpoints — 1 matching pod, 0 ready" {
		t.Fatalf("an unscheduled pod must not be node-down, got %q", issues[0].Detail)
	}
}

func TestAnnotateEndpointCause_Idempotent(t *testing.T) {
	issues := []Issue{brokenNoEndpoints("shop", "web")}
	services := []corev1.Service{svc("shop", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	AnnotateEndpointCause(issues, services, nil, nil)
	first := issues[0].Detail
	AnnotateEndpointCause(issues, services, nil, nil)
	if issues[0].Detail != first {
		t.Fatalf("not idempotent: %q -> %q", first, issues[0].Detail)
	}
}
