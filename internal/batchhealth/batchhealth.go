// Package batchhealth attaches a "JobFailed" finding to Job/CronJob workloads whose run
// failed. For a "Job" workload it inspects that Job; for a "CronJob" workload it inspects
// the newest owned Job. Pure and read-only: the caller supplies the assembled workloads
// plus the Jobs/CronJobs. Mirrors netpolicy/rollout.Annotate.
package batchhealth

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// cronFailedStatus is the header word a flagged CronJob carries.
//
// Assembly computes "Idle" from the active-job count, before any Job has been
// judged, and it is true — the schedule is alive and nothing is running right
// now. Printed above a JobFailed finding it reads as "nothing to see", so the
// word is rewritten here, on the one branch that has actually looked at the
// newest owned Job. The same fact is not derived twice: inventory.cronJobStatus
// would have to re-implement this walk to know it.
//
// Not "Failed", which is a standalone Job's word. A Job that failed is failed;
// a CronJob is not — it will fire again on schedule — and this phrasing matches
// the finding's own "the most recent scheduled run failed", so the header and
// the line beneath it agree.
//
// It moves no schemaVersion: Workload.Status is published as a bare string with
// no enum, so a new value is not a shape change.
const cronFailedStatus = "Last run failed"

// Annotate appends a "JobFailed" finding to each Job workload whose Job failed, and to
// each CronJob workload whose newest owned Job failed. CronJob→Jobs are derived from the
// Jobs' owner references, so the CronJob objects themselves are not needed. A flagged
// CronJob's Status is rewritten to cronFailedStatus; a Job's is left alone.
func Annotate(workloads []inventory.Workload, jobs []batchv1.Job) {
	byKey := make(map[string]*batchv1.Job, len(jobs))
	cronJobJobs := map[string][]*batchv1.Job{}
	for i := range jobs {
		j := &jobs[i]
		byKey[j.Namespace+"/"+j.Name] = j
		if name, ok := ownedByCronJob(*j); ok {
			cronJobJobs[j.Namespace+"/"+name] = append(cronJobJobs[j.Namespace+"/"+name], j)
		}
	}
	for i := range workloads {
		w := &workloads[i]
		wkey := w.Namespace + "/" + w.Name
		switch w.Kind {
		case "Job":
			if j := byKey[wkey]; j != nil {
				if f := jobFailedFinding(*j, wkey, false); f != nil {
					w.Findings = append(w.Findings, *f)
				}
			}
		case "CronJob":
			if latest := newestJob(cronJobJobs[wkey]); latest != nil {
				if f := jobFailedFinding(*latest, wkey, true); f != nil {
					w.Findings = append(w.Findings, *f)
					w.Status = cronFailedStatus
				}
			}
		}
	}
}

// ownedByCronJob returns the owning CronJob's name if the Job is controlled by one.
func ownedByCronJob(j batchv1.Job) (string, bool) {
	for _, o := range j.OwnerReferences {
		if o.Kind == "CronJob" && o.Controller != nil && *o.Controller {
			return o.Name, true
		}
	}
	return "", false
}

// newestJob returns the Job with the greatest CreationTimestamp, or nil.
func newestJob(jobs []*batchv1.Job) *batchv1.Job {
	var best *batchv1.Job
	for _, j := range jobs {
		if best == nil || j.CreationTimestamp.Time.After(best.CreationTimestamp.Time) {
			best = j
		}
	}
	return best
}

// jobFailedFinding returns a JobFailed finding if the Job has a Failed condition, else nil.
// wkey ("ns/name") identifies the workload; fromCronJob tailors the wording.
func jobFailedFinding(j batchv1.Job, wkey string, fromCronJob bool) *diagnose.Finding {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			// The condition is selected by type and status, so no matching
			// decision reads its message — this is the point at which the API's
			// free text becomes a kubeagent value. The reason is matched on, in
			// humanReason, so it is sanitized there instead: on the one arm that
			// echoes it rather than on the switch's input.
			msg := safetext.Line(c.Message)
			base, evidence, kind := "the Job failed", msg, "Job"
			if fromCronJob {
				base = "the most recent scheduled run failed"
				evidence = fmt.Sprintf("job %q: %s", j.Name, msg)
				kind = "CronJob"
			}
			reason := base
			if p := humanReason(c.Reason); p != "" {
				reason = base + " — " + p
			}
			return &diagnose.Finding{Pod: wkey, Kind: kind, Issue: "JobFailed", Reason: reason, Evidence: evidence}
		}
	}
	return nil
}

// humanReason maps a Job failure reason to a plain-language phrase.
//
// The switch is a matching decision, so it reads the raw value — a control
// character spliced into "BackoffLimitExceeded" must not make it stop matching.
// The default arm is the one that echoes the API's text into a finding's reason,
// so that arm sanitizes.
func humanReason(reason string) string {
	switch reason {
	case "BackoffLimitExceeded":
		return "exhausted its retries (BackoffLimitExceeded)"
	case "DeadlineExceeded":
		return "hit its deadline (DeadlineExceeded)"
	default:
		return safetext.Line(reason)
	}
}
