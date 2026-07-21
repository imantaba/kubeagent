package collect

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
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
		{500, "healthz check failed", "unhealthy", "healthz check failed"},
		{403, "forbidden", "forbidden", ""},
		{0, "", "unreachable", ""},
	}
	for _, c := range cases {
		p := classify("n", c.code, []byte(c.body))
		if p.Node != "n" || p.Status != c.wantStatus || p.Detail != c.wantDetail {
			t.Errorf("classify(%d, %q) = {%s, %q}, want {%s, %q}", c.code, c.body, p.Status, p.Detail, c.wantStatus, c.wantDetail)
		}
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
