// Package rollouthealth attaches a "RolloutStuck" finding to a flagged Deployment
// whose rollout has wedged — the new ReplicaSet's pods are not becoming available,
// so the Deployment's status carries a ReplicaFailure condition or a
// Progressing=False/ProgressDeadlineExceeded condition. Pure and read-only: the
// caller supplies the assembled+prioritized workloads and the Deployments (for
// their status conditions). Mirrors createhealth.Annotate.
package rollouthealth

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate appends a "RolloutStuck" finding to each flagged Deployment workload
// whose Deployment status shows a stuck rollout, and which no pod-level cause
// already explains — see restarting for the second half of that test. It mutates
// the slice elements in place. Runs after createhealth.Annotate so a lingering
// FailedCreate event wins the "no existing finding" gate.
func Annotate(workloads []inventory.Workload, deployments []appsv1.Deployment) {
	byName := make(map[string]*appsv1.Deployment, len(deployments))
	for i := range deployments {
		d := &deployments[i]
		byName[d.Namespace+"/"+d.Name] = d
	}
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() || w.Kind != "Deployment" || len(w.Findings) > 0 || restarting(w) {
			continue
		}
		dep, ok := byName[w.Namespace+"/"+w.Name]
		if !ok {
			continue
		}
		if ev, stuck := stuckCondition(dep); stuck {
			w.Findings = append(w.Findings, diagnose.Finding{
				Pod:      w.Namespace + "/" + w.Name,
				Issue:    "RolloutStuck",
				Reason:   "the Deployment's rollout cannot complete — the new pods are not becoming available",
				Evidence: ev,
			})
		}
	}
}

// restarting reports whether any pod in the workload has restarted enough times
// to be crash-looping rather than to have hit one bad moment.
//
// It sits beside the len(w.Findings) > 0 clause, not in place of it. That clause
// is this package's zero-redundancy rule — RolloutStuck says nothing a pod-level
// detector has already said — but it is evaluated against one momentary sample,
// and a crash-looping container is only in Waiting between restart attempts.
// Forty consecutive scans of one unchanged Deployment reported RolloutStuck 32
// times and CrashLoopBackOff 8, so the issue kind that gate verdicts, SARIF
// results, alert dedup keys and the watch daemon's /issues are keyed on depended
// on which millisecond the scan landed in. The restart count is durable across
// the whole cycle, so it answers the question the instant state cannot.
//
// The identical clause in internal/createhealth was replaced rather than
// extended, and deliberately: there a workload with a pod finding is still
// short of pods and the second cause is real, so suppressing it lost half the
// story. Here the pod finding IS the cause, and a second finding for the same
// thing is the redundancy this package exists to avoid.
//
// diagnose.RestartThreshold is read rather than re-typed so the number this
// package uses and the one RestartLoopDetector uses cannot drift apart.
func restarting(w *inventory.Workload) bool {
	for _, p := range w.Pods {
		if p.Restarts >= diagnose.RestartThreshold {
			return true
		}
	}
	return false
}

// stuckCondition returns the evidence string and true when the Deployment's
// status shows a wedged rollout. ReplicaFailure (the concrete pod-creation
// blocker) takes precedence over Progressing/ProgressDeadlineExceeded.
func stuckCondition(dep *appsv1.Deployment) (string, bool) {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return fmt.Sprintf("ReplicaFailure: %s", c.Message), true
		}
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
			return fmt.Sprintf("Progressing (ProgressDeadlineExceeded): %s", c.Message), true
		}
	}
	return "", false
}
