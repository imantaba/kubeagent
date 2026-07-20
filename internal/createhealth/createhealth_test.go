package createhealth

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func boolPtr(b bool) *bool { return &b }

// ownedRS builds a ReplicaSet controlled by the named Deployment.
func ownedRS(ns, name, deploy string) appsv1.ReplicaSet {
	return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: deploy, Controller: boolPtr(true)}},
	}}
}

// fcEvent builds a FailedCreate event on the given involved object.
func fcEvent(kind, ns, name, msg string, secs int64) corev1.Event {
	return corev1.Event{
		Reason:         "FailedCreate",
		Type:           "Warning",
		Message:        msg,
		InvolvedObject: corev1.ObjectReference{Kind: kind, Namespace: ns, Name: name},
		LastTimestamp:  metav1.Unix(secs, 0),
	}
}

const quotaMsg = `pods "api-7c9f-" is forbidden: exceeded quota: compute, requested: requests.cpu=2, used: requests.cpu=4, limited: requests.cpu=4`

func TestAnnotate_DeploymentViaReplicaSet(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 3, Ready: 0, Status: "Degraded"}}
	rss := []appsv1.ReplicaSet{ownedRS("shop", "api-7c9f", "api")}
	evs := []corev1.Event{fcEvent("ReplicaSet", "shop", "api-7c9f", quotaMsg, 100)}

	Annotate(ws, rss, evs)

	if len(ws[0].Findings) != 1 {
		t.Fatalf("want one finding, got %+v", ws[0].Findings)
	}
	f := ws[0].Findings[0]
	if f.Issue != "FailedCreate" {
		t.Errorf("Issue = %q, want FailedCreate", f.Issue)
	}
	if !strings.Contains(f.Reason, "ResourceQuota") {
		t.Errorf("Reason = %q, want it to mention ResourceQuota", f.Reason)
	}
	if f.Evidence != quotaMsg {
		t.Errorf("Evidence = %q, want the raw event message", f.Evidence)
	}
	if f.Pod != "shop/api" {
		t.Errorf("Pod = %q, want the workload identity shop/api", f.Pod)
	}
}

func TestAnnotate_StatefulSetDirect(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "db", Name: "pg", Kind: "StatefulSet", Desired: 2, Ready: 0, Status: "Degraded"}}
	msg := `admission webhook "policy.example.com" denied the request: label required`
	evs := []corev1.Event{fcEvent("StatefulSet", "db", "pg", msg, 100)}

	Annotate(ws, nil, evs)

	if len(ws[0].Findings) != 1 || !strings.Contains(ws[0].Findings[0].Reason, "admission webhook") {
		t.Fatalf("want an admission-webhook FailedCreate finding, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_DaemonSetLimitRange(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "sys", Name: "agent", Kind: "DaemonSet", Desired: 4, Ready: 1, Status: "Degraded"}}
	msg := `pods "agent-" is forbidden: maximum cpu usage per Container is 1, but limit is 2`
	evs := []corev1.Event{fcEvent("DaemonSet", "sys", "agent", msg, 100)}

	Annotate(ws, nil, evs)

	if len(ws[0].Findings) != 1 || !strings.Contains(ws[0].Findings[0].Reason, "LimitRange") {
		t.Fatalf("want a LimitRange FailedCreate finding, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_SkipsWorkloadWithExistingFinding(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 3, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff"}}}}
	rss := []appsv1.ReplicaSet{ownedRS("shop", "api-7c9f", "api")}
	evs := []corev1.Event{fcEvent("ReplicaSet", "shop", "api-7c9f", quotaMsg, 100)}

	Annotate(ws, rss, evs)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Issue != "CrashLoopBackOff" {
		t.Fatalf("must not annotate a workload that already has a finding, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_SkipsHealthyWorkload(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 3, Ready: 3, Status: "Running"}}
	rss := []appsv1.ReplicaSet{ownedRS("shop", "api-7c9f", "api")}
	evs := []corev1.Event{fcEvent("ReplicaSet", "shop", "api-7c9f", quotaMsg, 100)}

	Annotate(ws, rss, evs)

	if len(ws[0].Findings) != 0 {
		t.Fatalf("must not annotate a healthy workload, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_NewestEventWins(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 3, Ready: 0, Status: "Degraded"}}
	rss := []appsv1.ReplicaSet{ownedRS("shop", "api-7c9f", "api")}
	evs := []corev1.Event{
		fcEvent("ReplicaSet", "shop", "api-7c9f", "old: pod creation is failing", 100),
		fcEvent("ReplicaSet", "shop", "api-7c9f", quotaMsg, 200), // newer
	}

	Annotate(ws, rss, evs)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Evidence != quotaMsg {
		t.Fatalf("want the newest event's message, got %+v", ws[0].Findings)
	}
}

func TestClassifyCreateFailure(t *testing.T) {
	cases := map[string]string{
		quotaMsg:                                   "blocked by a ResourceQuota",
		`admission webhook "x" denied the request`: "rejected by an admission webhook",
		`maximum cpu usage per Container is 1`:      "violates a LimitRange",
		`is forbidden: some other policy`:           "forbidden by admission",
		`internal server error creating pod`:        "pod creation is failing",
	}
	for msg, want := range cases {
		if got := classifyCreateFailure(msg); got != want {
			t.Errorf("classifyCreateFailure(%q) = %q, want %q", msg, got, want)
		}
	}
}
