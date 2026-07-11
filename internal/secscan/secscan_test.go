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
