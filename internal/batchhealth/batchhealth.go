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
)

// Annotate appends a "JobFailed" finding to each Job workload whose Job failed, and to
// each CronJob workload whose newest owned Job failed. CronJob→Jobs are derived from the
// Jobs' owner references, so the CronJob objects themselves are not needed.
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
			base, evidence := "the Job failed", c.Message
			if fromCronJob {
				base = "the most recent scheduled run failed"
				evidence = fmt.Sprintf("job %q: %s", j.Name, c.Message)
			}
			reason := base
			if p := humanReason(c.Reason); p != "" {
				reason = base + " — " + p
			}
			return &diagnose.Finding{Pod: wkey, Issue: "JobFailed", Reason: reason, Evidence: evidence}
		}
	}
	return nil
}

// humanReason maps a Job failure reason to a plain-language phrase.
func humanReason(reason string) string {
	switch reason {
	case "BackoffLimitExceeded":
		return "exhausted its retries (BackoffLimitExceeded)"
	case "DeadlineExceeded":
		return "hit its deadline (DeadlineExceeded)"
	default:
		return reason
	}
}
