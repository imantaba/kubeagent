package capacity

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// node builds a Ready, schedulable, untainted node with the given allocatable.
// cpu is a quantity string like "4" or "500m"; mem like "16Gi".
func node(name, cpu, mem string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// notReady flips a node's Ready condition to False.
func notReady(n corev1.Node) corev1.Node {
	n.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}
	return n
}

// cordoned marks a node unschedulable.
func cordoned(n corev1.Node) corev1.Node {
	n.Spec.Unschedulable = true
	return n
}

// tainted adds a taint with the given effect.
func tainted(n corev1.Node, effect corev1.TaintEffect) corev1.Node {
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
		Key: "node-role.kubernetes.io/control-plane", Effect: effect,
	})
	return n
}

// pod builds a Running pod on nodeName with one container requesting cpu/mem.
// An empty quantity string means that request is absent.
func pod(namespace, name, nodeName, cpu, mem string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{container("app", cpu, mem, "", "")},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// container builds one container. Empty strings omit that request or limit.
func container(name, cpuReq, memReq, cpuLim, memLim string) corev1.Container {
	c := corev1.Container{Name: name}
	if cpuReq != "" || memReq != "" {
		c.Resources.Requests = corev1.ResourceList{}
		if cpuReq != "" {
			c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(cpuReq)
		}
		if memReq != "" {
			c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(memReq)
		}
	}
	if cpuLim != "" || memLim != "" {
		c.Resources.Limits = corev1.ResourceList{}
		if cpuLim != "" {
			c.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(cpuLim)
		}
		if memLim != "" {
			c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(memLim)
		}
	}
	return c
}

// ownedBy sets a controller ownerReference of the given kind and name.
func ownedBy(p corev1.Pod, kind, name string) corev1.Pod {
	yes := true
	p.OwnerReferences = []metav1.OwnerReference{
		{Kind: kind, Name: name, Controller: &yes},
	}
	return p
}

// replicaSet builds a ReplicaSet owned by a Deployment of the given name.
func replicaSet(namespace, name, deployment string) appsv1.ReplicaSet {
	yes := true
	return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name,
		OwnerReferences: []metav1.OwnerReference{
			{Kind: "Deployment", Name: deployment, Controller: &yes},
		},
	}}
}
