package batchhealth

import (
	"testing"
	"unicode"
	"unicode/utf8"

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
	if f.Kind != "Job" {
		t.Errorf("Kind = %q, want Job", f.Kind)
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
	if f.Kind != "CronJob" {
		t.Errorf("Kind = %q, want CronJob", f.Kind)
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

// A flagged CronJob's header word says what happened. "Idle" is what assembly
// computes from the active-job count, and it is true — the schedule is alive
// and nothing is running right now — but printed above a JobFailed finding it
// reads as "nothing to see". The word is set here, where the newest owned Job
// has actually been judged, rather than in inventory.cronJobStatus, which runs
// before any Job has been looked at.
func TestAnnotate_CronJobStatusSaysTheLastRunFailed(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "nightly", Kind: "CronJob", Status: "Idle"}}
	j := failedJob("shop", "nightly-1", "BackoffLimitExceeded", "Job has reached the specified backoff limit")
	j.OwnerReferences = cronOwner("nightly")
	Annotate(ws, []batchv1.Job{j})
	if len(ws[0].Findings) != 1 {
		t.Fatalf("want 1 finding on the CronJob, got %d", len(ws[0].Findings))
	}
	if want := "Last run failed"; ws[0].Status != want {
		t.Errorf("Status = %q, want %q", ws[0].Status, want)
	}
}

// A CronJob mid-run keeps the word assembly computed. Only the branch that
// attaches a finding rewrites the status, so Active(1) and Idle both survive
// when nothing failed — the header never disagrees with the findings below it.
func TestAnnotate_CronJobStatusUntouchedWithoutAFailure(t *testing.T) {
	for _, status := range []string{"Idle", "Active(1)"} {
		ws := []inventory.Workload{{Namespace: "shop", Name: "nightly", Kind: "CronJob", Status: status}}
		ok := batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly-1", OwnerReferences: cronOwner("nightly"), CreationTimestamp: metav1.Unix(1000, 0)},
			Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
		}
		Annotate(ws, []batchv1.Job{ok})
		if ws[0].Status != status {
			t.Errorf("Status = %q, want it left as %q", ws[0].Status, status)
		}
	}
}

// A standalone Job keeps its own word. "Failed" is exactly right for a Job —
// it has no schedule to fire again — and rewriting it would flatten the
// difference this wording exists to preserve.
func TestAnnotate_JobStatusIsNotRewritten(t *testing.T) {
	ws := []inventory.Workload{{Namespace: "shop", Name: "db-migrate", Kind: "Job", Status: "Failed"}}
	Annotate(ws, []batchv1.Job{failedJob("shop", "db-migrate", "BackoffLimitExceeded", "Job has reached the specified backoff limit")})
	if len(ws[0].Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(ws[0].Findings))
	}
	if ws[0].Status != "Failed" {
		t.Errorf("Status = %q, want it left as Failed", ws[0].Status)
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

// hostileText is what a Job's condition carries when whatever wrote it was not
// the kube-controller-manager: invalid UTF-8, a screen-clearing ANSI escape, a
// right-to-left override and a NUL.
const hostileText = "exit\x1b[2J‮gnp\x00 code 1\xff"

// A condition's reason and message are both free text, and both reach a
// finding — the reason through humanReason's default arm, which returns an
// unrecognised reason verbatim.
func TestAnnotate_SanitizesAFailedConditionsReasonAndMessage(t *testing.T) {
	t.Run("Job", func(t *testing.T) {
		ws := []inventory.Workload{{Namespace: "shop", Name: "db-migrate", Kind: "Job"}}
		Annotate(ws, []batchv1.Job{failedJob("shop", "db-migrate", hostileText, hostileText)})
		if len(ws[0].Findings) != 1 {
			t.Fatalf("want 1 finding, got %d", len(ws[0].Findings))
		}
		assertSanitized(t, "Reason", ws[0].Findings[0].Reason)
		assertSanitized(t, "Evidence", ws[0].Findings[0].Evidence)
	})
	t.Run("CronJob", func(t *testing.T) {
		ws := []inventory.Workload{{Namespace: "shop", Name: "nightly", Kind: "CronJob"}}
		job := failedJob("shop", "nightly-1", hostileText, hostileText)
		job.OwnerReferences = cronOwner("nightly")
		job.CreationTimestamp = metav1.Unix(2000, 0)
		Annotate(ws, []batchv1.Job{job})
		if len(ws[0].Findings) != 1 {
			t.Fatalf("want 1 finding, got %d", len(ws[0].Findings))
		}
		assertSanitized(t, "Reason", ws[0].Findings[0].Reason)
		assertSanitized(t, "Evidence", ws[0].Findings[0].Evidence)
	})
}

// assertSanitized fails unless s is what safetext.Line guarantees: valid UTF-8
// with no control characters and no Unicode formatting characters.
func assertSanitized(t *testing.T, where, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("%s is not valid UTF-8: %q", where, s)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Errorf("%s carries %U: %q", where, r, s)
		}
	}
}
