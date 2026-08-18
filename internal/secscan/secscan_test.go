package secscan

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func boolp(b bool) *bool    { return &b }
func int64p(i int64) *int64 { return &i }

// rsOwned builds a pod controlled by ReplicaSet rsName, in namespace ns.
func rsOwned(ns, podName, rsName string, ctrs ...corev1.Container) corev1.Pod {
	ctrl := true
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: podName,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rsName, Controller: &ctrl}},
		},
		Spec: corev1.PodSpec{Containers: ctrs},
	}
}

// rsForDeploy builds a ReplicaSet controlled by Deployment depName.
func rsForDeploy(ns, rsName, depName string) appsv1.ReplicaSet {
	ctrl := true
	return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: rsName,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: depName, Controller: &ctrl}},
	}}
}

// count returns how many findings have the given Check.
func count(fs []Finding, check string) int {
	n := 0
	for _, f := range fs {
		if f.Check == check {
			n++
		}
	}
	return n
}

func TestAssess_PrivilegedFoldsToDeployment(t *testing.T) {
	pod := rsOwned("payments", "api-xyz", "api-rs",
		corev1.Container{Name: "app", SecurityContext: &corev1.SecurityContext{Privileged: boolp(true)}})
	rs := []appsv1.ReplicaSet{rsForDeploy("payments", "api-rs", "api")}
	got := Assess([]corev1.Pod{pod}, nil, rs, nil)
	if count(got, "Privileged") != 1 {
		t.Fatalf("want one Privileged finding, got %+v", got)
	}
	f := got[0]
	if f.Profile != "baseline" || f.Kind != "Deployment" || f.Workload != "api" ||
		f.Container != "app" || f.Namespace != "payments" {
		t.Errorf("wrong attribution: %+v", f)
	}
}

func TestResolveWorkload_NodeOwnerIsThePodItself(t *testing.T) {
	ctrl := true
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "kube-system", Name: "etcd-worker-2",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Node", Name: "worker-2", Controller: &ctrl}},
		},
	}
	wl := resolveWorkload(pod, nil, nil)
	if wl.Kind != "Pod" || wl.Name != "etcd-worker-2" {
		t.Errorf("want a Node-owned pod to resolve to itself, got %+v", wl)
	}
}

func TestResolveWorkload_NoOwnerIsThePodItself(t *testing.T) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "standalone"}}
	wl := resolveWorkload(pod, nil, nil)
	if wl.Kind != "Pod" || wl.Name != "standalone" {
		t.Errorf("want an unowned pod to resolve to itself, got %+v", wl)
	}
}

func TestResolveWorkload_ReplicaSetStillFoldsToDeployment(t *testing.T) {
	ctrl := true
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "api-xyz",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-rs", Controller: &ctrl}},
		},
	}
	rsByKey := map[string]appsv1.ReplicaSet{"shop/api-rs": rsForDeploy("shop", "api-rs", "api")}
	wl := resolveWorkload(pod, rsByKey, nil)
	if wl.Kind != "Deployment" || wl.Name != "api" {
		t.Errorf("want a ReplicaSet-owned pod to still fold to its Deployment, got %+v", wl)
	}
}

// jobOwned builds a Job controlled by CronJob cronJobName, in namespace ns.
// If cronJobName is "", the Job is bare (no controller owner of its own).
func jobOwned(ns, jobName, cronJobName string) batchv1.Job {
	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: jobName}}
	if cronJobName != "" {
		ctrl := true
		job.OwnerReferences = []metav1.OwnerReference{{Kind: "CronJob", Name: cronJobName, Controller: &ctrl}}
	}
	return job
}

func TestResolveWorkload_JobOwnerFoldsToCronJob(t *testing.T) {
	ctrl := true
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "report-29780989-abcde",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "report-29780989", Controller: &ctrl}},
		},
	}
	jobsByKey := map[string]batchv1.Job{"shop/report-29780989": jobOwned("shop", "report-29780989", "report")}
	wl := resolveWorkload(pod, nil, jobsByKey)
	if wl.Kind != "CronJob" || wl.Name != "report" {
		t.Errorf("want a CronJob-owned Job's pod to fold to the CronJob, got %+v", wl)
	}
}

func TestResolveWorkload_BareJobStaysJob(t *testing.T) {
	ctrl := true
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "migrate-abcde",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "migrate", Controller: &ctrl}},
		},
	}
	jobsByKey := map[string]batchv1.Job{"shop/migrate": jobOwned("shop", "migrate", "")}
	wl := resolveWorkload(pod, nil, jobsByKey)
	if wl.Kind != "Job" || wl.Name != "migrate" {
		t.Errorf("want a bare Job's pod to stay Job/<name>, got %+v", wl)
	}
}

func TestResolveWorkload_JobAbsentFromSliceStaysJob(t *testing.T) {
	ctrl := true
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "migrate-abcde",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "migrate", Controller: &ctrl}},
		},
	}
	wl := resolveWorkload(pod, nil, nil) // migrate is absent from the (nil) jobs map
	if wl.Kind != "Job" || wl.Name != "migrate" {
		t.Errorf("want a Job absent from the slice to stay Job/<name>, got %+v", wl)
	}
}

func TestAssess_NotPrivileged(t *testing.T) {
	pod := rsOwned("shop", "web-xyz", "web-rs",
		corev1.Container{Name: "web", SecurityContext: &corev1.SecurityContext{Privileged: boolp(false)}})
	if count(Assess([]corev1.Pod{pod}, nil, nil, nil), "Privileged") != 0 {
		t.Error("a non-privileged container must not be flagged Privileged")
	}
}

func TestAssess_HostNamespaces(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "agent"},
		Spec:       corev1.PodSpec{HostNetwork: true, HostPID: true, Containers: []corev1.Container{{Name: "c"}}},
	}
	got := Assess([]corev1.Pod{pod}, nil, nil, nil)
	if count(got, "HostNamespaces") != 1 {
		t.Fatalf("want one HostNamespaces finding, got %+v", got)
	}
	// bare pod (no controller) -> Kind Pod, its own name; pod-level -> no container.
	f := got[0]
	if f.Kind != "Pod" || f.Workload != "agent" || f.Container != "" {
		t.Errorf("wrong attribution: %+v", f)
	}
}

func TestAssess_HostNamespaces_SingularSuffix(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "agent"},
		Spec:       corev1.PodSpec{HostPID: true, Containers: []corev1.Container{{Name: "c"}}},
	}
	got := Assess([]corev1.Pod{pod}, nil, nil, nil)
	if count(got, "HostNamespaces") != 1 {
		t.Fatalf("want one HostNamespaces finding, got %+v", got)
	}
	if got[0].Detail != "pod shares the host PID namespace" {
		t.Errorf("want singular suffix for one shared namespace, got %q", got[0].Detail)
	}
}

func TestAssess_HostNamespaces_PluralSuffix(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "agent"},
		Spec: corev1.PodSpec{
			HostNetwork: true, HostPID: true, HostIPC: true,
			Containers: []corev1.Container{{Name: "c"}},
		},
	}
	got := Assess([]corev1.Pod{pod}, nil, nil, nil)
	if count(got, "HostNamespaces") != 1 {
		t.Fatalf("want one HostNamespaces finding, got %+v", got)
	}
	if got[0].Detail != "pod shares the host network/PID/IPC namespaces" {
		t.Errorf("want plural suffix for three shared namespaces, got %q", got[0].Detail)
	}
}

func TestAssess_DedupsReplicas(t *testing.T) {
	c := corev1.Container{Name: "app", SecurityContext: &corev1.SecurityContext{Privileged: boolp(true)}}
	pods := []corev1.Pod{
		rsOwned("payments", "api-1", "api-rs", c),
		rsOwned("payments", "api-2", "api-rs", c),
	}
	rs := []appsv1.ReplicaSet{rsForDeploy("payments", "api-rs", "api")}
	if n := count(Assess(pods, nil, rs, nil), "Privileged"); n != 1 {
		t.Errorf("two replicas of one Deployment must collapse to one finding, got %d", n)
	}
}

func TestAssess_HostPath(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "node-agent"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c"}},
			Volumes: []corev1.Volume{{Name: "sock", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/docker.sock"}}}},
		},
	}
	got := Assess([]corev1.Pod{pod}, nil, nil, nil)
	if count(got, "HostPath") != 1 {
		t.Fatalf("want one HostPath finding, got %+v", got)
	}
}

func TestAssess_HostPath_ReadOnlyMount(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "node-agent"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "c",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "sock", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{{Name: "sock", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/docker.sock"}}}},
		},
	}
	got := Assess([]corev1.Pod{pod}, nil, nil, nil)
	if count(got, "HostPath") != 1 {
		t.Fatalf("want one HostPath finding, got %+v", got)
	}
	if got[0].Detail != "mounts hostPath /var/run/docker.sock (read-only host filesystem)" {
		t.Errorf("want read-only wording when every mount is read-only, got %q", got[0].Detail)
	}
}

func TestAssess_HostPath_MixedReadOnlyAndWritableMounts(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "node-agent"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "reader", VolumeMounts: []corev1.VolumeMount{{Name: "sock", ReadOnly: true}}},
				{Name: "writer", VolumeMounts: []corev1.VolumeMount{{Name: "sock", ReadOnly: false}}},
			},
			Volumes: []corev1.Volume{{Name: "sock", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/docker.sock"}}}},
		},
	}
	got := Assess([]corev1.Pod{pod}, nil, nil, nil)
	if count(got, "HostPath") != 1 {
		t.Fatalf("want one HostPath finding, got %+v", got)
	}
	if got[0].Detail != "mounts hostPath /var/run/docker.sock (writable host filesystem)" {
		t.Errorf("want writable wording when any mount is writable, got %q", got[0].Detail)
	}
}

func TestAssess_HostPath_NoContainerMountsIt(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "node-agent"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c"}}, // no VolumeMounts at all
			Volumes: []corev1.Volume{{Name: "sock", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/docker.sock"}}}},
		},
	}
	got := Assess([]corev1.Pod{pod}, nil, nil, nil)
	if count(got, "HostPath") != 1 {
		t.Fatalf("want one HostPath finding, got %+v", got)
	}
	if got[0].Detail != "mounts hostPath /var/run/docker.sock (writable host filesystem)" {
		t.Errorf("want writable wording (safe default) when nothing mounts the volume, got %q", got[0].Detail)
	}
}

func TestAssess_MultipleHostPaths(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "agent"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c"}},
			Volumes: []corev1.Volume{
				{Name: "sock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/docker.sock"}}},
				{Name: "cni", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/cni/net.d"}}},
			},
		},
	}
	if n := count(Assess([]corev1.Pod{pod}, nil, nil, nil), "HostPath"); n != 2 {
		t.Errorf("two distinct hostPath volumes must each be reported, got %d", n)
	}
}

func TestAssess_HostPort(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs",
		corev1.Container{Name: "web", Ports: []corev1.ContainerPort{{HostPort: 8080, ContainerPort: 8080}}})
	if count(Assess([]corev1.Pod{pod}, nil, nil, nil), "HostPort") != 1 {
		t.Errorf("want one HostPort finding")
	}
}

func TestAssess_AddedCapability(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{
		Name: "web",
		SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
			Add: []corev1.Capability{"NET_BIND_SERVICE", "SYS_ADMIN"}}},
	})
	got := Assess([]corev1.Pod{pod}, nil, nil, nil)
	if count(got, "AddedCapability") != 1 {
		t.Fatalf("want one AddedCapability finding, got %+v", got)
	}
	// NET_BIND_SERVICE alone is allowed by baseline.
	ok := rsOwned("shop", "ok-1", "ok-rs", corev1.Container{
		Name: "web",
		SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
			Add: []corev1.Capability{"NET_BIND_SERVICE"}}},
	})
	if count(Assess([]corev1.Pod{ok}, nil, nil, nil), "AddedCapability") != 0 {
		t.Errorf("NET_BIND_SERVICE alone must not be flagged")
	}
}

// hardened satisfies every workload check: non-root, no priv-esc, drops ALL.
func hardenedContainer(name string) corev1.Container {
	return corev1.Container{
		Name: name,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             boolp(true),
			AllowPrivilegeEscalation: boolp(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

func TestAssess_RunAsRoot_DefaultFlagged(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{Name: "web"}) // no securityContext
	if count(Assess([]corev1.Pod{pod}, nil, nil, nil), "RunAsRoot") != 1 {
		t.Error("a container with no runAsNonRoot must be flagged RunAsRoot")
	}
}

func TestAssess_RunAsRoot_PodLevelNonRootSatisfies(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{
		Name:            "web",
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: boolp(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
	})
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: boolp(true)} // inherited by the container
	if count(Assess([]corev1.Pod{pod}, nil, nil, nil), "RunAsRoot") != 0 {
		t.Error("pod-level runAsNonRoot must satisfy the container")
	}
}

func TestAssess_RunAsUserZeroFlagged(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{
		Name:            "web",
		SecurityContext: &corev1.SecurityContext{RunAsUser: int64p(0), AllowPrivilegeEscalation: boolp(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
	})
	if count(Assess([]corev1.Pod{pod}, nil, nil, nil), "RunAsRoot") != 1 {
		t.Error("runAsUser 0 must be flagged RunAsRoot")
	}
}

func TestAssess_AllowPrivilegeEscalationAndCaps(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{Name: "web"}) // nothing set
	got := Assess([]corev1.Pod{pod}, nil, nil, nil)
	if count(got, "AllowPrivilegeEscalation") != 1 {
		t.Error("no allowPrivilegeEscalation:false must be flagged")
	}
	if count(got, "CapabilitiesNotDropped") != 1 {
		t.Error("not dropping ALL must be flagged")
	}
}

func TestAssess_HardenedPodClean(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", hardenedContainer("web"))
	if got := Assess([]corev1.Pod{pod}, nil, nil, nil); len(got) != 0 {
		t.Errorf("a fully hardened pod must yield no findings, got %+v", got)
	}
}

func TestAssess_ExposedService(t *testing.T) {
	svcs := []corev1.Service{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "admin"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Port: 80}}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "internal"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{{Port: 80}}}},
	}
	got := Assess(nil, svcs, nil, nil)
	if count(got, "ExposedService") != 1 {
		t.Fatalf("want one ExposedService finding, got %+v", got)
	}
	f := got[0]
	if f.Kind != "Service" || f.Workload != "admin" || f.Profile != "kubeagent" {
		t.Errorf("wrong attribution: %+v", f)
	}
}

func TestAssess_ExposedService_ExternalNameSkipsExternalIPs(t *testing.T) {
	svcs := []corev1.Service{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "extname"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalIPs: []string{"1.2.3.4"}, ExternalName: "db.internal.example"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "clusterip"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ExternalIPs: []string{"1.2.3.4"}, Ports: []corev1.ServicePort{{Port: 80}}}},
	}
	got := Assess(nil, svcs, nil, nil)
	if count(got, "ExposedService") != 1 {
		t.Fatalf("want one ExposedService finding (ExternalName skipped), got %+v", got)
	}
	if got[0].Workload != "clusterip" {
		t.Errorf("want the ClusterIP service flagged, not the ExternalName one: %+v", got)
	}
}

func TestAssess_ExposedService_HeadlessWithExternalIPsAndNoPorts(t *testing.T) {
	// A headless Service is exempt from the API server's ports-required rule,
	// so this object is valid and reaches servicePorts' empty-ports branch.
	svcs := []corev1.Service{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "headless"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: corev1.ClusterIPNone,
				ExternalIPs: []string{"203.0.113.10"}}},
	}
	got := Assess(nil, svcs, nil, nil)
	if count(got, "ExposedService") != 1 {
		t.Fatalf("a headless Service with externalIPs is still externally reachable: %+v", got)
	}
	if got[0].Detail != "externalIPs set exposes no ports externally" {
		t.Errorf("want the no-ports detail, got %q", got[0].Detail)
	}
}

func TestAssess_ExposedService_NodePortAndExternalIPs(t *testing.T) {
	svcs := []corev1.Service{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "np"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 80}}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "eip"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ExternalIPs: []string{"1.2.3.4"}, Ports: []corev1.ServicePort{{Port: 80}}}},
	}
	if n := count(Assess(nil, svcs, nil, nil), "ExposedService"); n != 2 {
		t.Errorf("NodePort and externalIPs services must each be flagged, got %d", n)
	}
}
