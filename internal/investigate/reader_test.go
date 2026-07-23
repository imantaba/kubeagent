package investigate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func call(name string, input map[string]string) toolCall {
	b, _ := json.Marshal(input)
	return toolCall{ID: "t1", Name: name, Input: b}
}

func TestReader_DescribePod_StructuredNoSecrets(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  "10.1.2.3",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "web", Ready: false, RestartCount: 5,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff", Message: "back-off restarting",
				}},
			}},
		},
	}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")

	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "pod", "namespace": "shop", "name": "web-abc",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "CrashLoopBackOff") || !strings.Contains(res.Content, "restarts=5") {
		t.Errorf("missing structured status: %q", res.Content)
	}
	if strings.Contains(res.Content, "10.1.2.3") {
		t.Errorf("pod IP must not leak into tool output: %q", res.Content)
	}
}

func TestReader_OutOfScope_IsError(t *testing.T) {
	r := Reader{client: fake.NewSimpleClientset()}
	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "pod", "namespace": "other", "name": "x",
	}), NewScope(nil))
	if !res.IsError || !strings.Contains(res.Content, "not in scope") {
		t.Errorf("out-of-scope call must return an error result, got %+v", res)
	}
}

func TestReader_UnknownKind_IsError(t *testing.T) {
	r := Reader{client: fake.NewSimpleClientset()}
	s := NewScope(nil)
	s.Add("secret", "shop", "creds")
	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "secret", "namespace": "shop", "name": "creds",
	}), s)
	if !res.IsError {
		t.Errorf("unknown/unsupported kind must return an error result, got %+v", res)
	}
}

func TestReader_GetEvents_ForInScopeObject(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "web-abc.1", Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{Name: "web-abc", Namespace: "shop"},
		Reason:         "BackOff", Message: "Back-off pulling image", Count: 3,
	}
	r := Reader{client: fake.NewSimpleClientset(ev)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_events", map[string]string{
		"namespace": "shop", "name": "web-abc",
	}), s)
	if res.IsError || !strings.Contains(res.Content, "BackOff") {
		t.Errorf("expected events, got %+v", res)
	}
}

func TestReader_GetRelated_OwnerAddsToScope(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc", Namespace: "shop",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-5f"}},
		},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-5f", Namespace: "shop"}}
	r := Reader{client: fake.NewSimpleClientset(pod, rs)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_related", map[string]string{
		"namespace": "shop", "name": "web-abc", "relation": "owner",
	}), s)
	if res.IsError || !strings.Contains(res.Content, "web-5f") {
		t.Fatalf("expected owner, got %+v", res)
	}
	if !s.Allowed("replicaset", "shop", "web-5f") {
		t.Error("resolved owner must be added to scope")
	}
}
