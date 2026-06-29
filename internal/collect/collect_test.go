package collect

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

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
	facts := FactsFrom(pods)
	if len(facts) != 2 || facts[0].Pod == nil || facts[0].Pod.Name != "p1" {
		t.Fatalf("expected 2 facts wrapping each pod, got %+v", facts)
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
