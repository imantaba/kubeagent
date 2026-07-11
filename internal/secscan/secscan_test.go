package secscan

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
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
	got := Assess([]corev1.Pod{pod}, nil, rs)
	if count(got, "Privileged") != 1 {
		t.Fatalf("want one Privileged finding, got %+v", got)
	}
	f := got[0]
	if f.Profile != "baseline" || f.Kind != "Deployment" || f.Workload != "api" ||
		f.Container != "app" || f.Namespace != "payments" {
		t.Errorf("wrong attribution: %+v", f)
	}
}

func TestAssess_NotPrivileged(t *testing.T) {
	pod := rsOwned("shop", "web-xyz", "web-rs",
		corev1.Container{Name: "web", SecurityContext: &corev1.SecurityContext{Privileged: boolp(false)}})
	if count(Assess([]corev1.Pod{pod}, nil, nil), "Privileged") != 0 {
		t.Error("a non-privileged container must not be flagged Privileged")
	}
}

func TestAssess_HostNamespaces(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "agent"},
		Spec:       corev1.PodSpec{HostNetwork: true, HostPID: true, Containers: []corev1.Container{{Name: "c"}}},
	}
	got := Assess([]corev1.Pod{pod}, nil, nil)
	if count(got, "HostNamespaces") != 1 {
		t.Fatalf("want one HostNamespaces finding, got %+v", got)
	}
	// bare pod (no controller) -> Kind Pod, its own name; pod-level -> no container.
	f := got[0]
	if f.Kind != "Pod" || f.Workload != "agent" || f.Container != "" {
		t.Errorf("wrong attribution: %+v", f)
	}
}

func TestAssess_DedupsReplicas(t *testing.T) {
	c := corev1.Container{Name: "app", SecurityContext: &corev1.SecurityContext{Privileged: boolp(true)}}
	pods := []corev1.Pod{
		rsOwned("payments", "api-1", "api-rs", c),
		rsOwned("payments", "api-2", "api-rs", c),
	}
	rs := []appsv1.ReplicaSet{rsForDeploy("payments", "api-rs", "api")}
	if n := count(Assess(pods, nil, rs), "Privileged"); n != 1 {
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
	got := Assess([]corev1.Pod{pod}, nil, nil)
	if count(got, "HostPath") != 1 {
		t.Fatalf("want one HostPath finding, got %+v", got)
	}
}

func TestAssess_HostPort(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs",
		corev1.Container{Name: "web", Ports: []corev1.ContainerPort{{HostPort: 8080, ContainerPort: 8080}}})
	if count(Assess([]corev1.Pod{pod}, nil, nil), "HostPort") != 1 {
		t.Errorf("want one HostPort finding")
	}
}

func TestAssess_AddedCapability(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{
		Name: "web",
		SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
			Add: []corev1.Capability{"NET_BIND_SERVICE", "SYS_ADMIN"}}},
	})
	got := Assess([]corev1.Pod{pod}, nil, nil)
	if count(got, "AddedCapability") != 1 {
		t.Fatalf("want one AddedCapability finding, got %+v", got)
	}
	// NET_BIND_SERVICE alone is allowed by baseline.
	ok := rsOwned("shop", "ok-1", "ok-rs", corev1.Container{
		Name: "web",
		SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
			Add: []corev1.Capability{"NET_BIND_SERVICE"}}},
	})
	if count(Assess([]corev1.Pod{ok}, nil, nil), "AddedCapability") != 0 {
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
	if count(Assess([]corev1.Pod{pod}, nil, nil), "RunAsRoot") != 1 {
		t.Error("a container with no runAsNonRoot must be flagged RunAsRoot")
	}
}

func TestAssess_RunAsRoot_PodLevelNonRootSatisfies(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{
		Name:            "web",
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: boolp(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
	})
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: boolp(true)} // inherited by the container
	if count(Assess([]corev1.Pod{pod}, nil, nil), "RunAsRoot") != 0 {
		t.Error("pod-level runAsNonRoot must satisfy the container")
	}
}

func TestAssess_RunAsUserZeroFlagged(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{
		Name:            "web",
		SecurityContext: &corev1.SecurityContext{RunAsUser: int64p(0), AllowPrivilegeEscalation: boolp(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
	})
	if count(Assess([]corev1.Pod{pod}, nil, nil), "RunAsRoot") != 1 {
		t.Error("runAsUser 0 must be flagged RunAsRoot")
	}
}

func TestAssess_AllowPrivilegeEscalationAndCaps(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{Name: "web"}) // nothing set
	got := Assess([]corev1.Pod{pod}, nil, nil)
	if count(got, "AllowPrivilegeEscalation") != 1 {
		t.Error("no allowPrivilegeEscalation:false must be flagged")
	}
	if count(got, "CapabilitiesNotDropped") != 1 {
		t.Error("not dropping ALL must be flagged")
	}
}

func TestAssess_HardenedPodClean(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", hardenedContainer("web"))
	if got := Assess([]corev1.Pod{pod}, nil, nil); len(got) != 0 {
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
	got := Assess(nil, svcs, nil)
	if count(got, "ExposedService") != 1 {
		t.Fatalf("want one ExposedService finding, got %+v", got)
	}
	f := got[0]
	if f.Kind != "Service" || f.Workload != "admin" || f.Profile != "kubeagent" {
		t.Errorf("wrong attribution: %+v", f)
	}
}
