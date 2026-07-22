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
// that has no existing finding and whose Deployment status shows a stuck rollout.
// It mutates the slice elements in place. Runs after createhealth.Annotate so a
// lingering FailedCreate event wins the "no existing finding" gate.
func Annotate(workloads []inventory.Workload, deployments []appsv1.Deployment) {
	byName := make(map[string]*appsv1.Deployment, len(deployments))
	for i := range deployments {
		d := &deployments[i]
		byName[d.Namespace+"/"+d.Name] = d
	}
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() || w.Kind != "Deployment" || len(w.Findings) > 0 {
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
