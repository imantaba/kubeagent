package batchhealth

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

func failedJob(ns, name, reason, message string) batchv1.Job {
	return batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: message},
		}},
	}
}

func cronOwner(name string) []metav1.OwnerReference {
	ctrl := true
	return []metav1.OwnerReference{{Kind: "CronJob", Name: name, Controller: &ctrl}}
}

func TestAnnotate_FailedJob(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "db-migrate", Kind: "Job"}}
	jobs := []batchv1.Job{failedJob("shop", "db-migrate", "BackoffLimitExceeded", "Job has reached the specified backoff limit")}
	Annotate(ws, jobs)
	if len(ws[0].Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(ws[0].Findings))
	}
	f := ws[0].Findings[0]
	if f.Issue != "JobFailed" {
		t.Errorf("Issue = %q, want JobFailed", f.Issue)
	}
	if want := "the Job failed — exhausted its retries (BackoffLimitExceeded)"; f.Reason != want {
		t.Errorf("Reason = %q, want %q", f.Reason, want)
	}
	if f.Evidence != "Job has reached the specified backoff limit" {
		t.Errorf("Evidence = %q", f.Evidence)
	}
}

func TestAnnotate_CompleteJobNotFlagged(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "done", Kind: "Job"}}
	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "done"},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	Annotate(ws, []batchv1.Job{job})
	if len(ws[0].Findings) != 0 {
		t.Errorf("a Complete Job must not be flagged, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_CronJobNewestRunFailed(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "nightly", Kind: "CronJob"}}
	older := failedJob("shop", "nightly-1", "BackoffLimitExceeded", "old failure")
	older.OwnerReferences = cronOwner("nightly")
	older.CreationTimestamp = metav1.Unix(1000, 0)
	newer := failedJob("shop", "nightly-2", "DeadlineExceeded", "Job was active longer than specified deadline")
	newer.OwnerReferences = cronOwner("nightly")
	newer.CreationTimestamp = metav1.Unix(2000, 0)
	Annotate(ws, []batchv1.Job{older, newer})
	if len(ws[0].Findings) != 1 {
		t.Fatalf("want 1 finding on the CronJob, got %d", len(ws[0].Findings))
	}
	f := ws[0].Findings[0]
	if want := "the most recent scheduled run failed — hit its deadline (DeadlineExceeded)"; f.Reason != want {
		t.Errorf("Reason = %q, want %q", f.Reason, want)
	}
	if want := `job "nightly-2": Job was active longer than specified deadline`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

func TestAnnotate_CronJobNewestCompleteOlderFailed(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "nightly", Kind: "CronJob"}}
	older := failedJob("shop", "nightly-1", "BackoffLimitExceeded", "old failure")
	older.OwnerReferences = cronOwner("nightly")
	older.CreationTimestamp = metav1.Unix(1000, 0)
	newer := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly-2", OwnerReferences: cronOwner("nightly"), CreationTimestamp: metav1.Unix(2000, 0)},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	Annotate(ws, []batchv1.Job{older, newer})
	if len(ws[0].Findings) != 0 {
		t.Errorf("newest run Complete -> CronJob not flagged even if an older run failed, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_CronJobNewestRunning(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "nightly", Kind: "CronJob"}}
	running := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly-3", OwnerReferences: cronOwner("nightly"), CreationTimestamp: metav1.Unix(3000, 0)},
		Status:     batchv1.JobStatus{Active: 1},
	}
	Annotate(ws, []batchv1.Job{running})
	if len(ws[0].Findings) != 0 {
		t.Errorf("a Running latest run must not be flagged, got %+v", ws[0].Findings)
	}
}
