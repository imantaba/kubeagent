package collect

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/certhealth"
)

// The seven list functions the scan's phase-1 pool calls. Each wraps its error
// with the same text CollectInventory used, so an operator reading a failure
// sees the same sentence they always did.
func TestSingleListFunctionsWrapTheirErrors(t *testing.T) {
	cases := []struct {
		name     string
		resource string
		want     string
		call     func(context.Context, kubernetes.Interface) error
	}{
		{"Pods", "pods", "listing pods: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := Pods(ctx, c, "")
			return err
		}},
		{"Deployments", "deployments", "listing deployments: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := Deployments(ctx, c, "")
			return err
		}},
		{"ReplicaSets", "replicasets", "listing replicasets: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := ReplicaSets(ctx, c, "")
			return err
		}},
		{"StatefulSets", "statefulsets", "listing statefulsets: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := StatefulSets(ctx, c, "")
			return err
		}},
		{"DaemonSets", "daemonsets", "listing daemonsets: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := DaemonSets(ctx, c, "")
			return err
		}},
		{"Jobs", "jobs", "listing jobs: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := Jobs(ctx, c, "")
			return err
		}},
		{"CronJobs", "cronjobs", "listing cronjobs: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := CronJobs(ctx, c, "")
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("list", tc.resource, func(ktesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("boom")
			})
			err := tc.call(context.Background(), client)
			if err == nil {
				t.Fatalf("%s returned no error, want one", tc.name)
			}
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("%s error = %q, want prefix %q", tc.name, err.Error(), tc.want)
			}
		})
	}
}

// The namespace argument still scopes the list.
func TestSingleListFunctionsHonourTheNamespaceFilter(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "a"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "b"}},
	)
	pods, err := Pods(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Name != "a" {
		t.Errorf("Pods(ns=shop) = %d pods %v, want just shop/a", len(pods), pods)
	}
}

func TestCollectInventory_ListsControllersAndPods(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "d1"}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "rs1"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s1"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "ds1"}},
	)
	in, err := CollectInventory(context.Background(), client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(in.Pods) != 1 || len(in.Deployments) != 1 || len(in.ReplicaSets) != 1 ||
		len(in.StatefulSets) != 1 || len(in.DaemonSets) != 1 {
		t.Errorf("expected one of each kind, got %+v", in)
	}
}

func TestCollectInventory_ScopesToNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "d1"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "d2"}},
	)
	in, err := CollectInventory(context.Background(), client, "a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(in.Deployments) != 1 || in.Deployments[0].Namespace != "a" {
		t.Errorf("expected only namespace a, got %+v", in.Deployments)
	}
}

func TestNamespaces(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "legacy-ns"}}
	client := fake.NewSimpleClientset(ns)
	got, err := Namespaces(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "legacy-ns" {
		t.Fatalf("want the seeded namespace, got %+v", got)
	}
}

func TestNodes_ListsAllNodes(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}},
	)
	nodes, err := Nodes(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
}

func TestCollectInventory_ListsJobsAndCronJobs(t *testing.T) {
	client := fake.NewSimpleClientset(
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "j1"}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "cj1"}},
	)
	in, err := CollectInventory(context.Background(), client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(in.Jobs) != 1 || len(in.CronJobs) != 1 {
		t.Errorf("expected 1 job and 1 cronjob, got %d/%d", len(in.Jobs), len(in.CronJobs))
	}
}

func TestFactsFrom_WrapsEachPod(t *testing.T) {
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p2"}},
	}
	facts := FactsFrom(pods, nil)
	if len(facts) != 2 || facts[0].Pod == nil || facts[0].Pod.Name != "p1" {
		t.Fatalf("expected 2 facts wrapping each pod, got %+v", facts)
	}
}

func TestFactsFrom_CorrelatesEvents(t *testing.T) {
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p2"}},
	}
	events := []corev1.Event{
		{Reason: "FailedAttachVolume", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "a", Name: "p1"}},
		{Reason: "FailedAttachVolume", InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: "n1"}}, // non-pod -> ignored
	}
	facts := FactsFrom(pods, events)
	if len(facts[0].Events) != 1 {
		t.Errorf("p1 should have 1 correlated event, got %d", len(facts[0].Events))
	}
	if len(facts[1].Events) != 0 {
		t.Errorf("p2 should have no events, got %d", len(facts[1].Events))
	}
}

func TestParseNodeMetrics(t *testing.T) {
	data := []byte(`{"items":[
	  {"metadata":{"name":"n1"},"usage":{"cpu":"531m","memory":"27711Mi"}},
	  {"metadata":{"name":"n2"},"usage":{"cpu":"1046m","memory":"21927Mi"}}
	]}`)
	got, err := parseNodeMetrics(data)
	if err != nil {
		t.Fatalf("parseNodeMetrics: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(got))
	}
	if cpu := got["n1"][corev1.ResourceCPU]; cpu.MilliValue() != 531 {
		t.Errorf("n1 cpu = %d milli, want 531", cpu.MilliValue())
	}
	if mem := got["n2"][corev1.ResourceMemory]; mem.Value() != 21927*(1<<20) {
		t.Errorf("n2 mem = %d bytes", mem.Value())
	}
}

func TestParseNodeMetrics_Malformed(t *testing.T) {
	if _, err := parseNodeMetrics([]byte("not json")); err == nil {
		t.Error("expected error on malformed input")
	}
}

func TestAllPods_ListsAcrossNamespaces(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "p2"}},
	)
	pods, err := AllPods(context.Background(), client)
	if err != nil {
		t.Fatalf("AllPods: %v", err)
	}
	if len(pods) != 2 {
		t.Errorf("want 2 pods across namespaces, got %d", len(pods))
	}
}

func TestStorageClasses_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Provisioner: "p1"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Provisioner: "p2"},
	)
	scs, err := StorageClasses(context.Background(), client)
	if err != nil {
		t.Fatalf("StorageClasses: %v", err)
	}
	if len(scs) != 2 {
		t.Errorf("want 2 storageclasses, got %d", len(scs))
	}
}

func TestIngressClasses_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "traefik"}, Spec: networkingv1.IngressClassSpec{Controller: "traefik.io/ingress-controller"}},
	)
	ics, err := IngressClasses(context.Background(), client)
	if err != nil {
		t.Fatalf("IngressClasses: %v", err)
	}
	if len(ics) != 1 || ics[0].Spec.Controller != "traefik.io/ingress-controller" {
		t.Errorf("unexpected ingressclasses: %+v", ics)
	}
}

func TestSystemDaemonSets_OnlyKubeSystem(t *testing.T) {
	client := fake.NewSimpleClientset(
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "fluentd"}},
	)
	dss, err := SystemDaemonSets(context.Background(), client)
	if err != nil {
		t.Fatalf("SystemDaemonSets: %v", err)
	}
	if len(dss) != 1 || dss[0].Name != "cilium" {
		t.Errorf("want only kube-system/cilium, got %+v", dss)
	}
}

func TestServices_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s1"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "s2"}},
	)
	svcs, err := Services(context.Background(), client, "")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(svcs) != 2 {
		t.Errorf("want 2 services, got %d", len(svcs))
	}
}

func TestServices_NamespaceScoped(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s1"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "s2"}},
	)
	svcs, err := Services(context.Background(), client, "a")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(svcs) != 1 || svcs[0].Namespace != "a" {
		t.Errorf("want only namespace a, got %+v", svcs)
	}
}

func TestEndpointSlices_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s1-abc", Labels: map[string]string{discoveryv1.LabelServiceName: "s1"}}},
	)
	slices, err := EndpointSlices(context.Background(), client, "")
	if err != nil {
		t.Fatalf("EndpointSlices: %v", err)
	}
	if len(slices) != 1 || slices[0].Labels[discoveryv1.LabelServiceName] != "s1" {
		t.Errorf("unexpected slices: %+v", slices)
	}
}

func TestNetworkPolicies_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "deny-all"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "allow-web"}},
	)
	nps, err := NetworkPolicies(context.Background(), client, "")
	if err != nil {
		t.Fatalf("NetworkPolicies: %v", err)
	}
	if len(nps) != 2 {
		t.Errorf("want 2 network policies, got %d", len(nps))
	}
}

func TestNetworkPolicies_NamespaceScoped(t *testing.T) {
	client := fake.NewSimpleClientset(
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "deny-all"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "allow-web"}},
	)
	nps, err := NetworkPolicies(context.Background(), client, "a")
	if err != nil {
		t.Fatalf("NetworkPolicies: %v", err)
	}
	if len(nps) != 1 || nps[0].Namespace != "a" {
		t.Errorf("want only namespace a, got %+v", nps)
	}
}

func TestConfigMaps_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "c1"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "c2"}},
	)
	cms, err := ConfigMaps(context.Background(), client, "")
	if err != nil {
		t.Fatalf("ConfigMaps: %v", err)
	}
	if len(cms) != 2 {
		t.Errorf("want 2 configmaps, got %d", len(cms))
	}
}

func TestConfigMaps_NamespaceScoped(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "c1"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "c2"}},
	)
	cms, err := ConfigMaps(context.Background(), client, "a")
	if err != nil {
		t.Fatalf("ConfigMaps: %v", err)
	}
	if len(cms) != 1 || cms[0].Namespace != "a" {
		t.Errorf("want only namespace a, got %+v", cms)
	}
}

func TestParseNodeSummary_NodeFSAndPVCVolumes(t *testing.T) {
	data := []byte(`{
	  "node": {"fs": {"usedBytes": 170000000000, "capacityBytes": 200000000000}},
	  "pods": [
	    {"volume": [
	      {"usedBytes": 46000000000, "capacityBytes": 50000000000, "pvcRef": {"name": "data", "namespace": "shop"}},
	      {"usedBytes": 10, "capacityBytes": 20}
	    ]},
	    {"volume": [
	      {"usedBytes": 5, "capacityBytes": 10, "pvcRef": {"name": "cache", "namespace": "shop"}}
	    ]}
	  ]
	}`)
	s, ok, err := parseNodeSummary("n1", data)
	if err != nil || !ok {
		t.Fatalf("parse failed: ok=%v err=%v", ok, err)
	}
	if s.Node != "n1" || s.FSUsed != 170000000000 || s.FSCap != 200000000000 {
		t.Errorf("wrong node fs: %+v", s)
	}
	// Only volumes with a pvcRef are kept.
	if len(s.Volumes) != 2 {
		t.Fatalf("want 2 pvc volumes, got %d (%+v)", len(s.Volumes), s.Volumes)
	}
	if s.Volumes[0].Namespace != "shop" || s.Volumes[0].Name != "data" || s.Volumes[0].Cap != 50000000000 {
		t.Errorf("wrong first volume: %+v", s.Volumes[0])
	}
}

func TestParseNodeSummary_BadJSON(t *testing.T) {
	if _, ok, err := parseNodeSummary("n1", []byte("not json")); ok || err == nil {
		t.Errorf("want (false, err) on bad json, got ok=%v err=%v", ok, err)
	}
}

func TestPersistentVolumeClaimsAndVolumes_List(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data-0"}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-a"}},
	)
	pvcs, err := PersistentVolumeClaims(context.Background(), client, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pvcs) != 1 || pvcs[0].Name != "data-0" {
		t.Errorf("want 1 pvc data-0, got %+v", pvcs)
	}
	pvs, err := PersistentVolumes(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(pvs) != 1 || pvs[0].Name != "pv-a" {
		t.Errorf("want 1 pv pv-a, got %+v", pvs)
	}
}

func TestIngresses_List(t *testing.T) {
	client := fake.NewSimpleClientset(
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"}},
	)
	ings, err := Ingresses(context.Background(), client, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ings) != 1 || ings[0].Name != "web" {
		t.Errorf("want 1 ingress web, got %+v", ings)
	}
}

func TestNodeLeases_List(t *testing.T) {
	rt := metav1.NewMicroTime(time.Now())
	client := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-node-lease", Name: "node-1"},
		Spec:       coordinationv1.LeaseSpec{RenewTime: &rt},
	})
	got, err := NodeLeases(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "node-1" {
		t.Errorf("want 1 lease node-1, got %+v", got)
	}
}

func TestUnhealthyEvents(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "web.ev"},
		Reason:         "Unhealthy",
		Type:           "Warning",
		Message:        "Readiness probe failed: HTTP probe failed with statuscode: 503",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "web"},
	}
	client := fake.NewSimpleClientset(ev)
	got, err := UnhealthyEvents(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "Unhealthy" {
		t.Errorf("want 1 Unhealthy event, got %+v", got)
	}
}

// The second event is the point of this test. client-go's fake clientset
// ignores field selectors, so a lister that only sets one returns both events
// here; only the client-side repeat drops the unrelated one.
func TestFailedMountEvents_KeepsOnlyFailedMount(t *testing.T) {
	mount := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "api-0.mount"},
		Reason:         "FailedMount",
		Type:           "Warning",
		Message:        `MountVolume.SetUp failed for volume "settings" : configmap "settings" not found`,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "api-0"},
	}
	other := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "api-0.sched"},
		Reason:         "Scheduled",
		Type:           "Normal",
		Message:        "Successfully assigned shop/api-0 to n1",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "api-0"},
	}
	client := fake.NewSimpleClientset(mount, other)
	got, err := FailedMountEvents(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "FailedMount" {
		t.Errorf("want 1 FailedMount event, got %+v", got)
	}
}

func TestPVCEvents(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "data-pvc.ev"},
		Reason:         "ProvisioningFailed",
		Type:           "Warning",
		Message:        `storageclass "fast" not found`,
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data-pvc"},
	}
	client := fake.NewSimpleClientset(ev)
	got, err := PVCEvents(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "ProvisioningFailed" {
		t.Errorf("want 1 PVC event, got %+v", got)
	}
}

func TestFailedCreateEvents(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: "api-7c9f.ev"},
		Reason:         "FailedCreate",
		Type:           "Warning",
		Message:        `pods "api-7c9f-" is forbidden: exceeded quota: compute`,
		InvolvedObject: corev1.ObjectReference{Kind: "ReplicaSet", Namespace: "shop", Name: "api-7c9f"},
	}
	client := fake.NewSimpleClientset(ev)
	got, err := FailedCreateEvents(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "FailedCreate" {
		t.Fatalf("want one FailedCreate event, got %+v", got)
	}
}

func TestClassifyKubeletHealthz(t *testing.T) {
	cases := []struct {
		code                   int
		body                   string
		wantStatus, wantDetail string
	}{
		{200, "ok", "ok", ""},
		{500, "[+]ping ok\n[-]pleg failed\nhealthz check failed", "unhealthy", "[-]pleg failed"},
		{500, "healthz check failed", "unhealthy", ""},
		{403, "forbidden", "forbidden", ""},
		{401, "unauthorized", "forbidden", ""},
		{0, "", "unreachable", ""},
		{502, "bad gateway", "unreachable", ""},
		{503, "service unavailable", "unreachable", ""},
		{504, "gateway timeout", "unreachable", ""},
	}
	for _, c := range cases {
		p := classify("n", c.code, []byte(c.body))
		if p.Node != "n" || p.Status != c.wantStatus || p.Detail != c.wantDetail {
			t.Errorf("classify(%d, %q) = {%s, %q}, want {%s, %q}", c.code, c.body, p.Status, p.Detail, c.wantStatus, c.wantDetail)
		}
	}
}

func TestHealthzDetail(t *testing.T) {
	cases := []struct {
		name string
		body string
		max  int
		want string
	}{
		{"only failed line", "[-]pleg failed", 120, "[-]pleg failed"},
		{"failed line after others", "[+]ping ok\nhealthz check failed\n[-]pleg failed", 120, "[-]pleg failed"},
		{"no failed line, plain text", "healthz check failed", 120, ""},
		{"no failed line, json status body", `{"status":"failure","reason":"kubelet stopped"}`, 120, ""},
		{"empty body", "", 120, ""},
		{"failed line longer than max is truncated", "[-]" + strings.Repeat("x", 130), 120, "[-]" + strings.Repeat("x", 117) + "…"},
		{"forged [-] prefix split by a control character is not matched", "[\x00-]syncloop failed", 120, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := healthzDetail([]byte(c.body), c.max)
			if got != c.want {
				t.Errorf("healthzDetail(%q, %d) = %q, want %q", c.body, c.max, got, c.want)
			}
		})
	}
}

// TestHealthzDetail_Sanitizes pins R75: the "[-]" prefix match runs on the
// raw kubelet body — the forged-prefix case above proves that half — and the
// value healthzDetail returns is sanitized clean of control characters and
// escape bytes before it ever reaches the operator's terminal.
func TestHealthzDetail_Sanitizes(t *testing.T) {
	body := "[-]pleg failed\r\x1b[2K[-]forged different message"
	got := healthzDetail([]byte(body), 120)
	want := "[-]pleg failed [2K[-]forged different message"
	if got != want {
		t.Fatalf("healthzDetail(%q, 120) = %q, want %q", body, got, want)
	}
	for _, r := range got {
		if unicode.IsControl(r) {
			t.Errorf("healthzDetail(%q, 120) = %q contains control rune %U", body, got, r)
		}
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("healthzDetail(%q, 120) = %q contains an ANSI escape byte", body, got)
	}
}

// TestStatusCodeFrom pins R79(A): the fallback that recovers a /readyz
// probe's HTTP status code from the error client-go's StatusCode(&code)
// leaves at 0 when its serializer negotiator has no decoder for the
// response's content type.
func TestStatusCodeFrom(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"APIStatus error, code 500", &apierrors.StatusError{ErrStatus: metav1.Status{Code: 500}}, 500},
		{"APIStatus error, code 403", &apierrors.StatusError{ErrStatus: metav1.Status{Code: 403}}, 403},
		{"nil error", nil, 0},
		{"plain error, not APIStatus", errors.New("boom"), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := statusCodeFrom(c.err); got != c.want {
				t.Errorf("statusCodeFrom(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

func TestTLSSecrets(t *testing.T) {
	tls := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-tls"},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("PEM")}}
	client := fake.NewSimpleClientset(tls)
	got, err := TLSSecrets(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "shop-tls" {
		t.Fatalf("want the seeded TLS secret, got %+v", got)
	}
}

// TestTLSSecretsZeroesPrivateKeyOnACopy pins R105(B): TLSSecrets zeroes
// tls.key on the Secrets it returns, immediately after the List call and
// before returning, shortening the private key's residency in kubeagent
// from the whole scan down to nothing — kubeagent's own code never holds
// it. tls.crt is unchanged, the tracker's own stored object is unaffected
// (proven by a direct Get that bypasses TLSSecrets), and certhealth.Assess
// over the result is unaffected, since it only ever reads tls.crt.
func TestTLSSecretsZeroesPrivateKeyOnACopy(t *testing.T) {
	crt := []byte("not-a-real-cert-tls.crt-bytes")
	seed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop-tls"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": crt, "tls.key": []byte("private-key-bytes-must-not-survive")},
	}
	client := fake.NewSimpleClientset(seed)

	got, err := TLSSecrets(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 secret, got %d", len(got))
	}
	if len(got[0].Data["tls.key"]) != 0 {
		t.Errorf("tls.key = %q, want zeroed", got[0].Data["tls.key"])
	}
	if !bytes.Equal(got[0].Data["tls.crt"], crt) {
		t.Errorf("tls.crt = %q, want unchanged (%q)", got[0].Data["tls.crt"], crt)
	}

	// The tracker's own stored object must be untouched: fetch it directly,
	// bypassing TLSSecrets, and confirm tls.key is still intact there.
	stored, err := client.CoreV1().Secrets("shop").Get(context.Background(), "shop-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Data["tls.key"]) == 0 {
		t.Fatalf("the tracker's own copy had tls.key zeroed too — TLSSecrets must operate on a copy, not the tracker's object")
	}

	// Assess only ever reads tls.crt, so its output must be identical whether
	// tls.key is present or zeroed.
	before := certhealth.Assess([]corev1.Secret{*seed}, nil, 30, time.Time{})
	after := certhealth.Assess(got, nil, 30, time.Time{})
	if !reflect.DeepEqual(before, after) {
		t.Errorf("Assess differs after tls.key was zeroed: before=%+v after=%+v", before, after)
	}
}

func TestPodDisruptionBudgets(t *testing.T) {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"}}
	client := fake.NewSimpleClientset(pdb)
	got, err := PodDisruptionBudgets(context.Background(), client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "api" {
		t.Fatalf("expected the seeded PDB, got %+v", got)
	}
}

func TestHorizontalPodAutoscalers(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-hpa"}}
	client := fake.NewSimpleClientset(hpa)
	got, err := HorizontalPodAutoscalers(context.Background(), client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "api-hpa" {
		t.Fatalf("expected the seeded HPA, got %+v", got)
	}
}

func TestWebhookConfigurations(t *testing.T) {
	vwc := &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "vw"}}
	mwc := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "mw"}}
	client := fake.NewSimpleClientset(vwc, mwc)
	v, err := ValidatingWebhookConfigurations(context.Background(), client)
	if err != nil || len(v) != 1 || v[0].Name != "vw" {
		t.Fatalf("validating: got %+v err %v", v, err)
	}
	m, err := MutatingWebhookConfigurations(context.Background(), client)
	if err != nil || len(m) != 1 || m[0].Name != "mw" {
		t.Fatalf("mutating: got %+v err %v", m, err)
	}
}

func TestResourceQuotas(t *testing.T) {
	q1 := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "compute"}}
	q2 := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "objects"}}
	other := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "compute"}}
	client := fake.NewSimpleClientset(q1, q2, other)

	got, err := ResourceQuotas(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 quotas in shop, got %d", len(got))
	}

	all, err := ResourceQuotas(context.Background(), client, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 quotas across all namespaces, got %d", len(all))
	}
}

func TestParsePodMetricsSumsContainers(t *testing.T) {
	body := []byte(`{"items":[
	  {"metadata":{"namespace":"prod","name":"web-1"},
	   "containers":[{"name":"app","usage":{"cpu":"120m","memory":"200Mi"}},
	                 {"name":"sidecar","usage":{"cpu":"30m","memory":"56Mi"}}]},
	  {"metadata":{"namespace":"staging","name":"api-1"},
	   "containers":[{"name":"app","usage":{"cpu":"5m","memory":"32Mi"}}]}
	]}`)

	got, err := parsePodMetrics(body)
	if err != nil {
		t.Fatalf("parsePodMetrics: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 pods, got %d", len(got))
	}
	cpu := got["prod/web-1"][corev1.ResourceCPU]
	if cpu.MilliValue() != 150 {
		t.Errorf("want the two containers summed to 150m, got %s", cpu.String())
	}
	mem := got["prod/web-1"][corev1.ResourceMemory]
	if mem.Value() != 256*1024*1024 {
		t.Errorf("want 256Mi summed, got %s", mem.String())
	}
	if _, ok := got["staging/api-1"]; !ok {
		t.Error("want the second pod keyed namespace/name")
	}
}

func TestParsePodMetricsRejectsBadQuantity(t *testing.T) {
	body := []byte(`{"items":[{"metadata":{"namespace":"prod","name":"x"},
	  "containers":[{"name":"app","usage":{"cpu":"not-a-quantity"}}]}]}`)

	if _, err := parsePodMetrics(body); err == nil {
		t.Error("want an error for an unparseable quantity")
	}
}

func TestObjectEvents_ReturnsOnlyTheNamedObjectsEvents(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "payments", Name: "e1"},
			InvolvedObject: corev1.ObjectReference{Namespace: "payments", Name: "api-abc"},
			Reason:         "BackOff", Message: "back-off restarting failed container", Type: "Warning", Count: 5,
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "payments", Name: "e2"},
			InvolvedObject: corev1.ObjectReference{Namespace: "payments", Name: "other-pod"},
			Reason:         "Pulled", Message: "image pulled", Type: "Normal", Count: 1,
		},
	)

	got, err := ObjectEvents(context.Background(), client, "payments", "api-abc")
	if err != nil {
		t.Fatalf("ObjectEvents() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ObjectEvents() returned %d events, want 1 — the fake clientset ignores field "+
			"selectors, so the filter must also be applied client-side", len(got))
	}
	if got[0].Reason != "BackOff" {
		t.Errorf("Reason = %q, want %q", got[0].Reason, "BackOff")
	}
}

func TestObjectEvents_NoEventsIsNotAnError(t *testing.T) {
	got, err := ObjectEvents(context.Background(), fake.NewSimpleClientset(), "payments", "api-abc")
	if err != nil {
		t.Fatalf("ObjectEvents() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("ObjectEvents() = %v, want none", got)
	}
}

// A forbidden nodes/proxy read must be distinguishable from a node that simply
// has no stats. Before this, both came back as (zero, false, nil).
// NodeStats reads through client.CoreV1().RESTClient(), and the fake clientset's
// RESTClient() is hardcoded to return a nil *rest.RESTClient regardless of any
// reactor — calling it panics before a PrependReactor("get", "nodes") ever gets a
// chance to run (confirmed: fake.NewSimpleClientset() + PrependReactor panics with
// a nil pointer dereference inside k8s.io/client-go/rest.NewRequest, not a clean
// pass or a benign no-op). So this test uses a real *rest.RESTClient backed by an
// httptest server that always answers 403 Forbidden — a genuine HTTP round trip
// through the same code NodeStats calls in production, rather than the fake
// clientset's reactor chain.
func TestNodeStatsReturnsForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"forbidden"}`))
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("building a client for the fake forbidden server: %v", err)
	}
	_, ok, err := NodeStats(context.Background(), client, "node-1")
	if ok {
		t.Fatal("NodeStats reported success on a forbidden read")
	}
	if err == nil {
		t.Fatal("NodeStats swallowed a forbidden read; the caller cannot report a blind spot")
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("err = %v, want a Forbidden error", err)
	}
}

func TestPreviousLogsReturnsForbidden(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "pod-1", errors.New("no access"))
	})
	_, ok, err := PreviousLogs(context.Background(), client, "ns", "pod-1", "app")
	if ok {
		t.Fatal("PreviousLogs reported success on a forbidden read")
	}
	if err == nil {
		t.Fatal("PreviousLogs swallowed a forbidden read")
	}
}

func TestCapBody(t *testing.T) {
	small := []byte("ok")
	if got := capBody(small); string(got) != "ok" {
		t.Errorf("capBody shortened a small body to %q", got)
	}
	big := bytes.Repeat([]byte("a"), maxProxyBody+4096)
	if got := capBody(big); len(got) != maxProxyBody {
		t.Errorf("capBody returned %d bytes, want %d", len(got), maxProxyBody)
	}
	if got := capBody(nil); got != nil {
		t.Errorf("capBody(nil) = %v, want nil", got)
	}
}

// A proxied endpoint answering with far more than kubeagent will ever parse must
// not hand the whole body to a parser. client-go's Raw() has already read it
// all, so this bounds what the parsers see and what a later copy costs — not the
// transfer itself.
func TestCoreDNSMetricsCapsTheBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxProxyBody+64*1024))
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("building a client for the oversized-body server: %v", err)
	}
	body, code := CoreDNSMetrics(context.Background(), client, "kube-system", "coredns-1")
	if code != 200 {
		t.Errorf("code = %d, want 200", code)
	}
	if len(body) != maxProxyBody {
		t.Errorf("body = %d bytes, want it capped at %d", len(body), maxProxyBody)
	}
}
